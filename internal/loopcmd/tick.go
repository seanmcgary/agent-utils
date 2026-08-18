// Package loopcmd holds the tick orchestration and the operator commands.
package loopcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/runner"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/seanmcgary/agent-utils/internal/worktree"
)

// Deps holds everything a tick needs. Each field is replaceable in a test.
type Deps struct {
	Store      *store.Store
	GH         ghub.Client
	WT         *worktree.Manager
	SelfPath   string
	ConfigPath string
	Now        func() time.Time
	Spawn      func(selfPath string, dispatchID int64, configPath, runnerLog string) (int, error)
	// IsAlive reports whether a dispatch's runner process is still running.
	// It is a seam so a test can control liveness; production passes proc.IsAlive.
	IsAlive func(pid int, dispatchID int64) bool
	// Fetch updates the primary checkout. It is a seam so a test can skip git.
	Fetch func() error
}

// count increments n only when the action succeeded, so the recorded summary
// never claims a dispatch that did not happen.
func count(n *int, err error) error {
	if err == nil {
		*n++
	}
	return err
}

// pidGracePeriod is how long a dispatch row may carry pid 0 before the tick
// treats it as dead. It covers the window between the row insert and the pid
// write, so a crash in that window cannot cause a duplicate dispatch.
const pidGracePeriod = 90 * time.Second

// Summary reports what one tick did.
type Summary struct {
	Started        int  `json:"started"`
	Resumed        int  `json:"resumed"`
	Retried        int  `json:"retried"`
	Tended         int  `json:"tended"`
	Parked         int  `json:"parked"`
	Live           int  `json:"live"`
	Orphans        int  `json:"orphans"`
	BreakerTripped bool `json:"breaker_tripped"`
}

// Tick runs one reconcile and dispatch pass.
func Tick(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error) {
	var sum Summary
	now := deps.Now()

	// A failed fetch makes branch comparisons stale, so it suppresses TENDING.
	// It must not abandon the tick: reaping dead runners, retrying, and parking
	// have nothing to do with git, and skipping RecordTick would freeze the tick
	// counter that every backoff window is measured in.
	fetchOK := true
	if deps.Fetch != nil {
		if err := deps.Fetch(); err != nil {
			fetchOK = false
			slog.Error("fetch primary checkout; skipping tend this tick",
				"loop", cfg.Name, "err", err)
		}
	}

	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	issues, err := deps.GH.ListOpenIssues(ctx, owner, repo)
	if err != nil {
		return sum, err
	}

	snap := engine.Snapshot{Issues: issues, BehindBy: map[int]int{}}
	if cfg.TendPR && fetchOK {
		prs, err := deps.GH.ListOpenPullRequests(ctx, owner, repo)
		if err != nil {
			return sum, err
		}
		snap.PRs = prs
		for _, iss := range issues {
			if !iss.HasLabel(cfg.Labels.Review) {
				continue
			}
			pr, ok := engine.LinkPR(iss.Number, prs)
			if !ok {
				continue
			}
			behind, err := deps.GH.BehindBy(ctx, owner, repo, pr.BaseRef, pr.HeadRef)
			if err != nil {
				// One unusable pull request must not abandon the whole tick. If
				// this returned early, anyone able to open a pull request could
				// stop the loop, and the tick counter would freeze with it,
				// which also freezes every backoff window.
				slog.Warn("compare failed; skipping this pull request",
					"loop", cfg.Name, "issue", iss.Number, "pr", pr.Number, "err", err)
				continue
			}
			snap.BehindBy[pr.Number] = behind
			if err := deps.Store.PutPRLink(store.PRLink{
				Loop: cfg.Name, Repo: cfg.Repo, Number: iss.Number,
				PRNumber: pr.Number, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef,
			}); err != nil {
				slog.Error("store pr link", "loop", cfg.Name, "issue", iss.Number, "err", err)
			}
		}
	}

	states, err := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if err != nil {
		return sum, err
	}
	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		return sum, err
	}

	// Split running rows into live and dead by asking the operating system.
	// This is the fact the LLM orchestrator could not obtain, and it is why the
	// retry policy here needs no marker comments.
	st := engine.State{Issues: states, CooldownUntil: time.Time{}}
	for _, d := range running {
		// A row whose process has not registered its pid yet is NOT an orphan.
		// The tick writes the pid just after the spawn, so a young row with
		// pid 0 is a live agent in that window, not a dead one.
		if d.PID == 0 && now.Sub(d.StartedAt) < pidGracePeriod {
			st.Running = append(st.Running, d)
			continue
		}
		if deps.IsAlive(d.PID, d.ID) {
			st.Running = append(st.Running, d)
			continue
		}

		// The runner died without recording an outcome. Retire the row AND write
		// the durable failure flag. The flag is what the next decision reads: a
		// tick that declines to act (backoff or breaker) must not lose the fact.
		if err := deps.Store.FinishDispatch(d.ID, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1, APIError: "runner process died",
		}); err != nil {
			return sum, fmt.Errorf("retire dead dispatch %d: %w", d.ID, err)
		}
		if d.Kind != store.KindTend {
			if err := deps.Store.MarkNeedsRetry(cfg.Name, cfg.Repo, d.Number); err != nil {
				return sum, fmt.Errorf("mark issue %d for retry: %w", d.Number, err)
			}
			// Reflect the write in the snapshot this tick decides from.
			sIssue := states[d.Number]
			sIssue.Number = d.Number
			sIssue.NeedsRetry = true
			states[d.Number] = sIssue
		}
		sum.Orphans++
	}
	sum.Live = len(st.Running)
	st.Issues = states

	if st.CooldownUntil, err = deps.Store.CooldownUntil(cfg.Name); err != nil {
		return sum, err
	}
	if st.TickCount, err = deps.Store.TickCount(cfg.Name); err != nil {
		return sum, err
	}

	plan := engine.Decide(cfg, snap, st, now)
	sum.BreakerTripped = plan.BreakerTripped

	if plan.BreakerTripped {
		if err := deps.Store.SetCooldown(cfg.Name, plan.CooldownUntil); err != nil {
			return sum, err
		}
		slog.Warn("circuit breaker tripped; skipping all dispatch",
			"loop", cfg.Name, "cooldown_until", plan.CooldownUntil)
	}

	for _, d := range plan.Decisions {
		if err := act(ctx, cfg, deps, d, st, now, &sum); err != nil {
			// One failed decision must not abandon the rest of the tick.
			slog.Error("decision failed", "loop", cfg.Name, "kind", d.Kind,
				"issue", d.Issue, "err", err)
		}
	}

	body, _ := json.Marshal(sum)
	if _, err := deps.Store.RecordTick(cfg.Name, plan.BreakerTripped, string(body)); err != nil {
		return sum, err
	}
	slog.Info("tick complete", "loop", cfg.Name, "summary", string(body))
	return sum, nil
}

func act(
	ctx context.Context,
	cfg *config.Config,
	deps Deps,
	d engine.Decision,
	st engine.State,
	now time.Time,
	sum *Summary,
) error {
	switch d.Kind {
	case engine.KindClearRetry:
		// Not a dispatch. Clear a failure flag that no retry can act on, so the
		// issue is not stranded outside the loop forever.
		slog.Info("clearing stale retry flag", "loop", cfg.Name, "issue", d.Issue,
			"reason", d.Reason)
		return deps.Store.ClearNeedsRetry(cfg.Name, cfg.Repo, d.Issue)
	case engine.KindParkRetryExhausted:
		return count(&sum.Parked, parkRetryExhausted(ctx, cfg, deps, d))
	case engine.KindTend:
		return count(&sum.Tended, dispatch(ctx, cfg, deps, d, st, now, store.KindTend))
	case engine.KindResume:
		return count(&sum.Resumed, dispatch(ctx, cfg, deps, d, st, now, store.KindResume))
	case engine.KindRetryResume:
		return count(&sum.Retried, dispatch(ctx, cfg, deps, d, st, now, store.KindResume))
	case engine.KindRetryStart:
		// The previous attempt never created a usable session, so resuming would
		// fail every time. Start instead, with a NEW identifier: the decision
		// carries no session id precisely so dispatch mints a fresh one.
		return count(&sum.Retried, dispatch(ctx, cfg, deps, d, st, now, store.KindStart))
	case engine.KindStart:
		return count(&sum.Started, dispatch(ctx, cfg, deps, d, st, now, store.KindStart))
	default:
		return fmt.Errorf("unknown decision kind %q", d.Kind)
	}
}

func dispatch(
	ctx context.Context,
	cfg *config.Config,
	deps Deps,
	d engine.Decision,
	st engine.State,
	now time.Time,
	kind string,
) error {
	state, err := deps.Store.IssueState(cfg.Name, cfg.Repo, d.Issue)
	if err != nil {
		// Persisting a zero value read from a failed query would wipe the
		// session identifier and the retry counter of a live issue.
		return fmt.Errorf("read issue state for #%d: %w", d.Issue, err)
	}

	sessionID := d.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	// A tend run keeps no memory between runs, because a rebase is idempotent.
	if kind == store.KindTend {
		sessionID = uuid.NewString()
	}

	logPath := runner.LogPath(cfg.StateDir, cfg.Name, d.Issue, kind, now)

	// Persist the issue state BEFORE spawning. If the process dies between the
	// spawn and this write, the next tick would otherwise see "no session" and
	// start a second agent with a fresh session against a live worktree.
	if kind != store.KindTend {
		state.SessionID = sessionID
		state.UpdatedAt = now
		state.NeedsRetry = false
		state.Parked = false
		switch d.Kind {
		case engine.KindRetryStart, engine.KindRetryResume:
			state.RetryCount++
			state.LastRetryTick = st.TickCount
		default:
			// A human trigger begins a new episode, so the budget starts over.
			state.RetryCount = 0
		}
		if err := deps.Store.PutIssueState(state); err != nil {
			return err
		}
	}

	// Create the dispatch row BEFORE the worktree, so a worktree failure is
	// recorded as a failed dispatch rather than vanishing into a log line.
	dispatchID, err := deps.Store.CreateDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: d.Issue, Kind: kind,
		SessionID: sessionID, LogPath: logPath, PRNumber: d.PR, Title: d.Title,
	})
	if err != nil {
		return err
	}

	workDir := cfg.CheckoutBaseDir
	if cfg.Agent.Worktree == config.WorktreePerIssue {
		var wtErr error
		if kind == store.KindTend {
			workDir, wtErr = deps.WT.EnsurePR(d.PR, d.HeadRef)
		} else {
			workDir, wtErr = deps.WT.EnsureIssue(d.Issue)
		}
		if wtErr != nil {
			// Record the failed dispatch, but do NOT set the retry flag. No agent
			// ran, so the issue carries no in-flight label, and a retry flag on an
			// issue that is not in flight can never be acted on. The trigger label
			// is still present, so the ordinary path tries again next tick.
			_ = deps.Store.FinishDispatch(dispatchID, store.DispatchResult{
				Status: store.StatusFailed, ExitCode: -1, APIError: wtErr.Error(),
			})
			return wtErr
		}
		if kind != store.KindTend {
			state.WorktreePath = workDir
			_ = deps.Store.PutIssueState(state)
		}
	}

	runnerLog := runner.RunnerLogPath(cfg.StateDir, cfg.Name, dispatchID)
	pid, err := deps.Spawn(deps.SelfPath, dispatchID, deps.ConfigPath, runnerLog)
	if err != nil {
		// As above: no agent ran, so no retry flag. Setting one here would strand
		// the issue, because nothing can act on a retry flag without in-flight.
		_ = deps.Store.FinishDispatch(dispatchID, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1, APIError: err.Error(),
		})
		return err
	}
	if err := deps.Store.SetDispatchProcess(dispatchID, pid, now); err != nil {
		return err
	}

	slog.Info("dispatched", "loop", cfg.Name, "kind", kind, "issue", d.Issue,
		"pr", d.PR, "dispatch", dispatchID, "pid", pid, "session", sessionID,
		"reason", d.Reason)
	return nil
}

const retryCapComment = `🔁 **Orphan retry cap reached (%d/%d)** — %d consecutive agent dispatches for this issue failed to complete. This usually indicates a sustained platform-side issue rather than a problem with the issue itself. Parking here rather than retrying indefinitely.

To proceed: re-add the ` + "`%s`" + ` label once the underlying issue has cleared, and this resumes normally.`

// parkRetryExhausted is the ONE GitHub write this program performs. Every other
// comment and label change belongs to the dispatched agent. The exception
// exists because the failing action is the dispatch itself, so an agent sent to
// report the failure would fail the same way.
func parkRetryExhausted(ctx context.Context, cfg *config.Config, deps Deps, d engine.Decision) error {
	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	state, err := deps.Store.IssueState(cfg.Name, cfg.Repo, d.Issue)
	if err != nil {
		return err
	}
	if state.Parked {
		// Already parked. Re-posting would put one identical comment on the issue
		// per tick, forever, if a previous label edit failed after the comment.
		return nil
	}

	// Write the durable park state FIRST. It is idempotent, and recording it
	// before the GitHub calls means a failure there cannot produce a comment
	// storm on the next tick.
	state.Parked = true
	state.NeedsRetry = false
	state.UpdatedAt = deps.Now()
	if err := deps.Store.PutIssueState(state); err != nil {
		return err
	}

	body := fmt.Sprintf(retryCapComment,
		cfg.Retry.Max, cfg.Retry.Max, cfg.Retry.Max, cfg.Labels.Trigger)
	if err := deps.GH.PostComment(ctx, owner, repo, d.Issue, body); err != nil {
		return err
	}
	// Remove the trigger label as well as the in-flight label. Without this the
	// issue still looks queued, so the next tick resumes it and the park stops
	// nothing at all.
	if err := deps.GH.EditLabels(ctx, owner, repo, d.Issue,
		[]string{cfg.Labels.Blocked},
		[]string{cfg.Labels.InFlight, cfg.Labels.Trigger}); err != nil {
		return err
	}

	slog.Warn("parked at retry cap", "loop", cfg.Name, "issue", d.Issue)
	return nil
}

// RunAgent executes one dispatch. The detached runner process calls it.
func RunAgent(ctx context.Context, cfg *config.Config, deps Deps, dispatchID int64) error {
	d, err := deps.Store.GetDispatch(dispatchID)
	if err != nil {
		return err
	}
	if d.Status != store.StatusRunning {
		// The tick already reaped this dispatch. Two supervisors for one row
		// would both record an outcome and both mutate the worktree.
		return fmt.Errorf("dispatch %d is %s, not running", dispatchID, d.Status)
	}
	// Self-register. The pid this process reports is by definition the live one,
	// which closes the window where the row carries pid 0.
	if err := deps.Store.SetDispatchProcess(dispatchID, os.Getpid(), time.Now()); err != nil {
		return err
	}

	tmpl := cfg.Prompt
	resume := false
	switch d.Kind {
	case store.KindResume:
		tmpl, resume = cfg.ResumePrompt, true
	case store.KindTend:
		tmpl = cfg.TendPrompt
	}

	links, err := deps.Store.PRLinks(cfg.Name, cfg.Repo)
	if err != nil {
		return fmt.Errorf("read pr links: %w", err)
	}
	pr := links[d.Number]

	workDir := cfg.CheckoutBaseDir
	if cfg.Agent.Worktree == config.WorktreePerIssue {
		if d.Kind == store.KindTend {
			workDir = deps.WT.PathForPR(d.PRNumber)
		} else {
			workDir = deps.WT.PathForIssue(d.Number)
		}
	}

	prompt, err := runner.RenderPrompt(tmpl, runner.PromptData{
		Repo:      cfg.Repo,
		Loop:      cfg.Name,
		SessionID: d.SessionID,
		Worktree:  workDir,
		Issue:     runner.PromptIssue{Number: d.Number, Title: d.Title},
		PR: runner.PromptPR{
			Number:  d.PRNumber,
			HeadRef: pr.HeadRef,
			BaseRef: pr.BaseRef,
			// The tend prompt renders BehindBy and acts on it: it tells the agent
			// to make no push when the branch is current. Leaving it zero told
			// every tend agent the opposite of why it was dispatched.
			BehindBy: pr.BehindBy,
		},
		Labels: runner.PromptLabels{
			Trigger:  cfg.Labels.Trigger,
			InFlight: cfg.Labels.InFlight,
			Blocked:  cfg.Labels.Blocked,
			Review:   cfg.Labels.Review,
			Terminal: cfg.Labels.Terminal,
		},
	})
	if err != nil {
		// Route this through the same accounting as any other failed dispatch.
		// Calling FinishDispatch alone would skip MarkNeedsRetry, and the issue
		// would keep its trigger label and redispatch every tick with no cap:
		// one detached process per tick, forever, on a single template typo.
		_ = runner.Finish(deps.Store, d, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1, APIError: err.Error(),
		})
		return err
	}

	return runner.Supervise(ctx, cfg, deps.Store, d,
		runner.Invocation{SessionID: d.SessionID, Prompt: prompt, Resume: resume},
		workDir, d.LogPath)
}

// Reset drops the stored session and worktree for one issue, so the next tick
// starts it clean.
func Reset(cfg *config.Config, s *store.Store, wt *worktree.Manager, number int,
	isAlive func(pid int, dispatchID int64) bool) error {
	running, err := s.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		return err
	}
	for _, d := range running {
		if d.Number != number {
			continue
		}
		if isAlive(d.PID, d.ID) {
			// Removing the worktree now would delete files an agent is editing.
			return fmt.Errorf(
				"issue #%d has a live dispatch (pid %d); stop it before resetting",
				number, d.PID)
		}
		// The runner is gone. Retire the row so the next tick is coherent.
		if err := s.FinishDispatch(d.ID, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1, APIError: "reset by operator",
		}); err != nil && !errors.Is(err, store.ErrDispatchNotRunning) {
			return err
		}
	}
	if err := wt.Remove(wt.PathForIssue(number)); err != nil {
		return err
	}
	return s.DeleteIssueState(cfg.Name, cfg.Repo, number)
}
