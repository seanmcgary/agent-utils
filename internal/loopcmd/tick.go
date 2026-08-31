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
	Store *store.Store
	// ProjectID owns every row this loop writes. The detached runner is given it
	// explicitly, because it resolves no project of its own.
	ProjectID string
	GH        ghub.Client
	// Epic reads sub-issues and issue dependencies for the epic sweep. It is
	// narrow on purpose -- see ghub.EpicReader -- so a test of any OTHER pass
	// does not have to grow three methods it never calls.
	Epic       ghub.EpicReader
	WT         *worktree.Manager
	SelfPath   string
	ConfigPath string
	Now        func() time.Time
	Spawn      func(selfPath string, dispatchID int64, projectID, configPath, runnerLog string) (int, error)
	// IsAlive reports whether a dispatch's runner process is still running.
	// It is a seam so a test can control liveness; production passes proc.IsAlive.
	IsAlive func(pid int, dispatchID int64) bool
	// Fetch updates the primary checkout. It is a seam so a test can skip git.
	Fetch func() error
	// Behind counts the commits baseRef has that headRef does not, using only
	// the local checkout, and reports known=false for a ref that does not
	// resolve. It is the gate of the periodic tend check.
	//
	// It is a seam because WT is a concrete *worktree.Manager that a test
	// cannot substitute, and because the answer depends on a git checkout no
	// unit test has. Open wires it to Manager.BehindLocal.
	Behind func(headRef, baseRef string) (behind int, known bool, err error)
	// Git is the git the automatic rebase drives. A nil Git disables that path
	// entirely and every tend decision falls through to the agent, which is
	// what keeps a Deps built by hand -- every test that predates this field --
	// working unchanged. Open wires it to WT.
	Git RebaseGit
}

// count increments n only when the action succeeded, so the recorded summary
// never claims a dispatch that did not happen.
func count(n *int, err error) error {
	if err == nil {
		*n++
	}
	return err
}

// pidGracePeriod is how long a dispatch row may carry a non-positive pid
// before the tick treats it as dead. It covers the window between the row
// insert and the pid write, so a crash in that window cannot cause a duplicate
// dispatch.
//
// Non-positive, not zero: a spawn can record a placeholder that is not 0.
// runner.Spawn returned -1 for every successful spawn until it was fixed
// (os.Process.Release invalidates the handle), and rows written by that build
// still exist on disk. proc.IsAlive answers false for any pid <= 0, so a
// condition testing only 0 sends a live agent's row straight to the reaper.
const pidGracePeriod = 90 * time.Second

// isLive reports whether d's process should be treated as still running.
//
// A row whose process has not registered its pid yet is NOT an orphan. The
// tick writes the pid just after the spawn, so a row younger than
// pidGracePeriod carrying a non-positive pid is a live agent in that window,
// not a dead one. Any pid <= 0 counts as unregistered: isAlive rejects all
// of them, and a spawn can leave a placeholder other than 0 (see
// pidGracePeriod). Under cron this window was minutes from the next tick;
// under the webhook daemon deliveries arrive seconds apart, and reaping here
// retried an issue whose agent was still working.
//
// This is the ONE liveness rule reapDead and loopcmd.CleanupClosedPR both
// use. Reset calls isAlive directly instead, which is correct for Reset --
// an operator invoking it is not racing a spawn that just happened -- and
// wrong for the delivery path, where a dispatch row can be seconds old.
// Copying Reset's rule there would delete a worktree out from under an agent
// that had just started.
func isLive(d store.Dispatch, isAlive func(pid int, dispatchID int64) bool, now time.Time) bool {
	if d.PID <= 0 && now.Sub(d.StartedAt) < pidGracePeriod {
		return true
	}
	return isAlive(d.PID, d.RunnerID())
}

// Summary reports what one tick did.
type Summary struct {
	Started int `json:"started"`
	Resumed int `json:"resumed"`
	Retried int `json:"retried"`
	Tended  int `json:"tended"`
	// Rebased counts the pull requests git replayed with no agent. It is
	// separate from Tended so a sweep's line says which of the two happened:
	// how many rebases cost nothing, and how many needed an agent.
	Rebased  int `json:"rebased"`
	Promoted int `json:"promoted"`
	Parked   int `json:"parked"`
	// Stopped counts KindStop decisions applied this tick: an operator's
	// `sessions kill`, or an invalid label override. Either way nothing was
	// dispatched and nothing was written to GitHub.
	Stopped        int  `json:"stopped"`
	Live           int  `json:"live"`
	Orphans        int  `json:"orphans"`
	BreakerTripped bool `json:"breaker_tripped"`
}

// Tick runs one FULL reconcile and dispatch pass over every open issue.
//
// This is the sweep, and it stays: it is what catches the work no webhook
// event names. GitHub sends no delivery when a pull request falls behind
// because someone pushed to master (that is a push event, which this daemon
// does not subscribe to), and none when a retry deadline passes on an issue
// nobody touched. `project loop tick` under cron runs this; the daemon runs
// TickIssue for the fast path. Both may run at once -- the per-loop lock in
// RunTick and TickIssue makes an overlapping pass harmless.
func Tick(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error) {
	var sum Summary
	now := deps.Now()

	// A failed fetch makes branch comparisons stale, so it suppresses TENDING.
	// It must not abandon the tick: reaping dead runners, retrying, and parking
	// have nothing to do with git, and abandoning the pass would leave a dead
	// runner's issue with no failure flag at all.
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
				// stop the loop for every issue it watches.
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
	live, err := reapDead(cfg, deps, running, states, now, &sum)
	if err != nil {
		return sum, err
	}
	st := engine.State{Issues: states, Running: live, CooldownUntil: time.Time{}}
	sum.Live = len(live)

	if st.CooldownUntil, err = deps.Store.CooldownUntil(cfg.Name); err != nil {
		return sum, err
	}

	plan := engine.Decide(cfg, snap, st, now)
	sum.BreakerTripped = plan.BreakerTripped

	// Run against the same snapshot Decide just read, so "the engine cannot
	// reach this row" is judged from exactly the issue set the engine saw.
	clearUnreachableDeadlines(cfg, deps, snap, states)

	if plan.BreakerTripped {
		if err := deps.Store.SetCooldown(cfg.Name, plan.CooldownUntil); err != nil {
			return sum, err
		}
		slog.Warn("circuit breaker tripped; skipping all dispatch",
			"loop", cfg.Name, "cooldown_until", plan.CooldownUntil)
	}

	for _, d := range plan.Decisions {
		if err := act(ctx, cfg, deps, d, now, &sum); err != nil {
			// One failed decision must not abandon the rest of the tick.
			slog.Error("decision failed", "loop", cfg.Name, "kind", d.Kind,
				"issue", d.Issue, "err", err)
		}
	}

	// The backstop. A webhook delivery can be missed -- the daemon down, the
	// proxy down, a delivery dropped -- and a missed close leaves a sub-issue
	// waiting forever with nothing to show that anything is wrong. This finds
	// it. A failure is logged and does not fail the tick: the tick's own work
	// is dispatch, and a sweep that could not read GitHub says nothing about
	// that.
	//
	// It runs AFTER the dispatch pass above, so an issue promoted here is
	// dispatched by the NEXT tick, not this one. That is deliberate: dispatch
	// decides from a snapshot read at the top of this function, and promoting
	// into that snapshot would mean deciding from a repository state that no
	// single read ever saw. One tick of latency on the backstop path costs
	// nothing -- the webhook path has none.
	//
	// EpicSweepAll takes NO lock. RunTick already holds it; see that function.
	if epicSum, err := EpicSweepAll(ctx, cfg, deps); err != nil {
		slog.Warn("epic sweep failed", "loop", cfg.Name, "err", err)
	} else {
		sum.Promoted += epicSum.Promoted
	}

	body, _ := json.Marshal(sum)
	if _, err := deps.Store.RecordTick(cfg.Name, plan.BreakerTripped, string(body)); err != nil {
		return sum, err
	}
	slog.Info("tick complete", "loop", cfg.Name, "summary", string(body))
	return sum, nil
}

// reapDead splits running rows into the live ones it returns and the dead ones
// it retires, marking each dead row's issue for retry and reflecting that write
// back into states.
//
// It is shared by the full tick and the issue-scoped one rather than copied.
// The caller chooses the ROWS -- the whole loop's for a sweep, one issue's for
// a delivery -- and nothing else about reaping may differ between them: a
// second copy that drifted on the pid grace period would put a second agent
// into a worktree that already holds one.
func reapDead(
	cfg *config.Config,
	deps Deps,
	running []store.Dispatch,
	states map[int]store.IssueState,
	now time.Time,
	sum *Summary,
) ([]store.Dispatch, error) {
	var live []store.Dispatch
	for _, d := range running {
		if isLive(d, deps.IsAlive, now) {
			live = append(live, d)
			continue
		}

		// The runner died without recording an outcome. Retire the row AND write
		// the durable failure flag. The flag is what the next decision reads: a
		// tick that declines to act (backoff or breaker) must not lose the fact.
		if err := deps.Store.FinishDispatch(d.ID, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1, APIError: "runner process died",
		}); err != nil {
			return nil, fmt.Errorf("retire dead dispatch %d: %w", d.ID, err)
		}
		if d.Kind != store.KindTend {
			if err := deps.Store.MarkNeedsRetry(
				cfg.Name, cfg.Repo, d.Number, now, runner.RetryBackoff(cfg)); err != nil {
				return nil, fmt.Errorf("mark issue %d for retry: %w", d.Number, err)
			}
			// Reflect the write in the snapshot this tick decides from. The row
			// is read back rather than patched by hand: MarkNeedsRetry stamps a
			// retry deadline as well as the flag, and a tick deciding from a
			// snapshot without it would dispatch the retry immediately and skip
			// the first backoff entry entirely.
			sIssue, err := deps.Store.IssueState(cfg.Name, cfg.Repo, d.Number)
			if err != nil {
				return nil, fmt.Errorf("re-read issue %d after marking it for retry: %w",
					d.Number, err)
			}
			states[d.Number] = sIssue
		}
		sum.Orphans++
	}
	return live, nil
}

// clearUnreachableDeadlines drops the retry DEADLINE from every stamped row
// whose issue this tick's snapshot did not offer to engine.Decide.
//
// Decide iterates snap.Issues and skips a vetoed one before it ever looks at
// NeedsRetry, so two ordinary operator actions put a row permanently outside
// its reach: closing (or transferring) the issue, which drops it from
// ListOpenIssues, and adding a veto label. KindClearRetry is the only thing
// that retires such a flag and it requires the issue to be in that list, so
// before the webhook daemon existed the row simply sat there, inert. It is no
// longer inert: store.EarliestRetryAfterAt selects on retry_after alone, hands
// the daemon the same past deadline every MinWakeInterval, and each one costs a
// FULL tick -- token read, database open, git fetch, ListOpenIssues,
// ListOpenPullRequests, a BehindBy per review issue -- forever, per stranded
// issue, with a repository-write token. Nothing about that tick changes the
// row, so the next wake finds it again.
//
// Only the deadline is cleared; see store.ClearRetryAfter for why the flag
// stays. A row the engine cannot reach is exactly the case where destroying
// the failure would be unrecoverable, and exactly the case where reopening the
// issue or removing the label must resume the retry at once.
//
// A failed clear is logged and the sweep continues: one unwritable row must not
// abandon the rest of the tick, and the next tick tries again.
func clearUnreachableDeadlines(
	cfg *config.Config,
	deps Deps,
	snap engine.Snapshot,
	states map[int]store.IssueState,
) {
	reachable := make(map[int]bool, len(snap.Issues))
	for _, iss := range snap.Issues {
		if iss.HasAnyLabel(cfg.Labels.Veto) {
			continue
		}
		reachable[iss.Number] = true
	}

	for number, state := range states {
		if state.RetryAfter.IsZero() || !state.NeedsRetry || state.Parked || reachable[number] {
			continue
		}
		if err := deps.Store.ClearRetryAfter(cfg.Name, cfg.Repo, number); err != nil {
			slog.Error("clear a retry deadline the loop can no longer reach",
				"loop", cfg.Name, "issue", number, "err", err)
			continue
		}
		slog.Info("cleared a retry deadline for an issue this loop can no longer see",
			"loop", cfg.Name, "issue", number,
			"reason", "closed, transferred, or carrying a veto label")
	}
}

func act(
	ctx context.Context,
	cfg *config.Config,
	deps Deps,
	d engine.Decision,
	now time.Time,
	sum *Summary,
) error {
	switch d.Kind {
	case engine.KindStop:
		// Not a dispatch, and not a GitHub write: parkRetryExhausted stays
		// the only one this program performs. The write is LOCAL only --
		// MarkStopped -- so an operator kill or an invalid label is visible
		// in `loop status` and `sessions list` without ever touching the
		// issue on GitHub.
		slog.Info("stopping issue", "loop", cfg.Name, "issue", d.Issue, "reason", d.Reason)
		return count(&sum.Stopped, deps.Store.MarkStopped(cfg.Name, cfg.Repo, d.Issue, d.Reason, now))
	case engine.KindClearRetry:
		// Not a dispatch. Clear a failure flag that no retry can act on, so the
		// issue is not stranded outside the loop forever.
		slog.Info("clearing stale retry flag", "loop", cfg.Name, "issue", d.Issue,
			"reason", d.Reason)
		return deps.Store.ClearNeedsRetry(cfg.Name, cfg.Repo, d.Issue)
	case engine.KindParkRetryExhausted:
		return count(&sum.Parked, parkRetryExhausted(ctx, cfg, deps, d))
	case engine.KindTend:
		// git first, the agent second. A rebase that replays cleanly needs no
		// conversation, and this is the common case: the agent exists for the
		// conflicts. gitRebase reports whether it settled the decision --
		// including the case where it settled it by declining to act, which is
		// what a refused lease means.
		switch outcome, err := gitRebase(ctx, cfg, deps, d); {
		case err != nil:
			// Logged, not returned: a git failure must not abandon the rest of
			// the sweep, and the agent is the fallback this whole path is
			// built around.
			slog.Warn("automatic rebase failed; falling back to the tend agent",
				"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", err)
		case outcome == doneRebased:
			sum.Rebased++
			return nil
		case outcome == doneNoRebase:
			// Settled by declining to act. No agent, and nothing counted.
			return nil
		}
		return count(&sum.Tended, dispatch(ctx, cfg, deps, d, now, store.KindTend))
	case engine.KindResume:
		return count(&sum.Resumed, dispatch(ctx, cfg, deps, d, now, store.KindResume))
	case engine.KindRetryResume:
		return count(&sum.Retried, dispatch(ctx, cfg, deps, d, now, store.KindResume))
	case engine.KindRetryStart:
		// The previous attempt never created a usable session, so resuming would
		// fail every time. Start instead, with a NEW identifier: the decision
		// carries no session id precisely so dispatch mints a fresh one.
		return count(&sum.Retried, dispatch(ctx, cfg, deps, d, now, store.KindStart))
	case engine.KindStart:
		return count(&sum.Started, dispatch(ctx, cfg, deps, d, now, store.KindStart))
	default:
		return fmt.Errorf("unknown decision kind %q", d.Kind)
	}
}

func dispatch(
	ctx context.Context,
	cfg *config.Config,
	deps Deps,
	d engine.Decision,
	now time.Time,
	kind string,
) error {
	// A tend is included here, and used to be excluded. It inherits the issue's
	// session when the engine offered one, so the rebase agent arrives knowing
	// the branch it is rebasing. The earlier rule -- always a fresh identifier,
	// on the grounds that a rebase is idempotent and needs no memory -- gave
	// every tend a throwaway conversation and a cold read of the diff. The
	// engine still offers nothing when the issue has no STARTED session, and
	// that case lands on the same fresh identifier as before.
	sessionID := d.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}

	logPath := runner.LogPath(cfg.StateDir, cfg.Name, d.Issue, kind, now)

	// Persist the issue state BEFORE spawning. If the process dies between the
	// spawn and this write, the next tick would otherwise see "no session" and
	// start a second agent with a fresh session against a live worktree.
	//
	// BeginDispatch, not a read-modify-write through PutIssueState: this tick
	// holds the loop's flock, but a detached runner process finishing a failed
	// dispatch does not, and it writes this same row through MarkNeedsRetry.
	// State read here and written back after the worktree and the spawn would
	// silently drop a failure recorded in that gap -- flag, deadline and retry
	// budget together. See store.BeginDispatch.
	//
	// RetryCount is load-bearing: MarkNeedsRetry indexes the backoff list with
	// it on the NEXT failure, which is why it is spent in SQL rather than
	// incremented from a value read before the gap. LastRetryTick is no longer
	// stamped: the wait is wall-clock now, so a tick number names nothing a
	// decision can use. The column stays in the table because dropping one
	// costs a rebuild and buys nothing.
	isRetry := d.Kind == engine.KindRetryStart || d.Kind == engine.KindRetryResume
	if kind != store.KindTend {
		if err := deps.Store.BeginDispatch(cfg.Name, cfg.Repo, d.Issue, sessionID, isRetry, now); err != nil {
			return err
		}
	}

	// Create the dispatch row BEFORE the worktree, so a worktree failure is
	// recorded as a failed dispatch rather than vanishing into a log line.
	dispatchID, err := deps.Store.CreateDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: d.Issue, Kind: kind,
		SessionID: sessionID, LogPath: logPath, PRNumber: d.PR, Title: d.Title,
		// A tend carries the issue's Overrides like every other dispatch: it
		// inherits the issue's session, and that session only means something
		// to the harness that minted it. See spec section 6.7.
		Model:   d.Overrides.Model,
		Harness: d.Overrides.Harness,
		Effort:  d.Overrides.Effort,
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
	}

	// Record the working directory in both worktree modes, not only per_issue.
	// Recording it only in one left the path empty in "none" mode, so status
	// reported no working directory for a live agent.
	if kind != store.KindTend {
		if err := deps.Store.SetWorktreePath(cfg.Name, cfg.Repo, d.Issue, workDir, now); err != nil {
			return err
		}
	}

	runnerLog := runner.RunnerLogPath(cfg.StateDir, cfg.Name, dispatchID)
	pid, err := deps.Spawn(deps.SelfPath, dispatchID, deps.ProjectID, deps.ConfigPath, runnerLog)
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
	// The deadline goes with the flag. A parked issue that kept one would leave
	// a permanent past deadline in the row for the daemon's wake query to find.
	state.RetryAfter = time.Time{}
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

// tendResumes reports whether a tend dispatch borrowed the issue's session and
// must therefore continue it rather than create it.
//
// It compares identifiers instead of trusting the kind, because a tend carries
// its own throwaway session whenever the issue had no started one, and "-r"
// against a session claude never created fails every time. This is the same
// gate engine.tendDecisions applies when it decides what to offer; the two
// agree by both reading SessionStarted.
func tendResumes(d store.Dispatch, state store.IssueState) bool {
	return d.Kind == store.KindTend &&
		state.SessionStarted &&
		state.SessionID != "" &&
		state.SessionID == d.SessionID
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
		// A tend that inherited the issue's session must RESUME it. Passing an
		// existing identifier to --session-id is refused outright ("Session ID
		// <uuid> is already in use"), so getting this wrong fails the dispatch
		// in under a second rather than degrading quietly.
		//
		// The answer is re-derived from the store rather than carried on the
		// row: this runs in a detached process that holds only the dispatch,
		// and the alternative -- a second tend kind, or a resume column -- puts
		// the fact in two places for reapDead, the liveness map, worktree
		// selection and cleanup to disagree about.
		st, err := deps.Store.IssueState(cfg.Name, cfg.Repo, d.Number)
		if err != nil {
			return err
		}
		resume = tendResumes(d, st)
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
		_ = runner.Finish(cfg, deps.Store, d, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1, APIError: err.Error(),
		}, deps.Now())
		return err
	}

	// Overrides come from THE ROW, not from a snapshot the tick took: this
	// detached process never saw the tick's decision, and the labels it was
	// parsed from may since have changed on GitHub.
	return runner.Supervise(ctx, cfg, deps.Store, d,
		runner.Invocation{
			SessionID: d.SessionID, Prompt: prompt, Resume: resume,
			Overrides: config.Overrides{Model: d.Model, Harness: d.Harness, Effort: d.Effort},
		},
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
		if isAlive(d.PID, d.RunnerID()) {
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
