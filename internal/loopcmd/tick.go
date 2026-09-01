// Package loopcmd holds the tick orchestration and the operator commands.
package loopcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/runner"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/seanmcgary/agent-utils/internal/worktree"
)

// resolveProviders returns the pi provider serving each issue's next dispatch,
// keyed by issue number.
//
// It is built here rather than inside the engine because resolution shells out
// and engine.Decide is pure. Only a pi dispatch has a provider worth
// comparing: claude reaches one vendor one way, so there is nothing a provider
// comparison could say about it, and resolving would spend a subprocess to
// learn "".
//
// One resolution per distinct MODEL, not per issue. A loop of thirty issues
// almost always runs one model, and `pi --list-models` is a process spawn.
func resolveProviders(ctx context.Context, cfg *config.Config, issues []ghub.Issue) map[int]string {
	out := make(map[int]string, len(issues))
	byModel := map[string]string{}
	for _, iss := range issues {
		// A label this loop cannot parse is not this function's problem: Decide
		// turns it into a KindStop, and an unresolved provider changes nothing
		// about that.
		ov, err := config.ParseOverrides(iss.Labels)
		if err != nil {
			continue
		}
		if engine.EffectiveHarness(cfg, ov) != config.HarnessPi {
			continue
		}
		model := runner.Effective(cfg, ov).Model
		if model == "" {
			continue
		}
		provider, seen := byModel[model]
		if !seen {
			provider = runner.ResolveProvider(ctx, model)
			byModel[model] = provider
		}
		if provider != "" {
			out[iss.Number] = provider
		}
	}
	return out
}

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
	//
	// It takes a context because it runs "git fetch origin --prune" over the
	// NETWORK, and the daemon's periodic tend check calls it on the single
	// wake goroutine while holding the loop lock: unbounded, one unreachable
	// remote stops every retry of every loop until the daemon is restarted.
	// The command-line tick may legitimately pass context.Background(); the
	// daemon must not. Open wires it to Manager.FetchCtx.
	Fetch func(ctx context.Context) error
	// Behind counts the commits baseRef has that headRef does not, using only
	// the local checkout, and reports known=false for a ref that does not
	// resolve. It is the gate of the periodic tend check.
	//
	// It is a seam because WT is a concrete *worktree.Manager that a test
	// cannot substitute, and because the answer depends on a git checkout no
	// unit test has. Open wires it to Manager.BehindLocalCtx.
	//
	// It takes a context for the reason Fetch does: the periodic check calls
	// it once per stored link on the wake goroutine, and a git command that
	// never returns there stalls the whole daemon.
	Behind func(ctx context.Context, headRef, baseRef string) (behind int, known bool, err error)
	// Git is the git the automatic rebase drives. A nil Git disables that path
	// entirely and every tend decision falls through to the agent, which is
	// what keeps a Deps built by hand -- every test that predates this field --
	// working unchanged. Open wires it to WT.
	Git RebaseGit
	// Force carries `loop tick --force` into engine.Decide, where it suspends
	// the breaker cooldown, every retry backoff window, and the breaker's own
	// trip for that one call. See engine.State.Force.
	//
	// Only the command-line sweep sets it. TickIssue and the tend sweep leave
	// it false, so no webhook delivery and no daemon wake can force a tick:
	// the override is an operator standing at a terminal, and a gate a
	// delivery could clear on its own would not be a gate.
	Force bool
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
	Rebased int `json:"rebased"`
	// Backoff counts a KindTend decision that met a conflict already known to
	// have defeated the agent, within its backoff window, and dispatched
	// nothing. Separate from Rebased and Tended so an operator auditing the
	// summary can tell a declined repeat from a completed one.
	Backoff  int `json:"backoff"`
	Promoted int `json:"promoted"`
	Parked   int `json:"parked"`
	// Stopped counts KindStop decisions applied this tick: an operator's
	// `sessions kill`, or an invalid label override. Either way nothing was
	// dispatched and nothing was written to GitHub.
	Stopped        int  `json:"stopped"`
	Live           int  `json:"live"`
	Orphans        int  `json:"orphans"`
	BreakerTripped bool `json:"breaker_tripped"`
	// Forced records that this tick ran with --force, so the recorded tick
	// says why it acted inside a cooldown the previous one honoured.
	Forced bool `json:"forced"`
}

// Tick runs one FULL reconcile and dispatch pass over every open issue.
//
// This is the sweep, and it stays: it is what catches the work no webhook
// event names. GitHub sends no delivery when a retry deadline passes on an
// issue nobody touched, and the daemon's merge, push, and periodic triggers
// all cover only a machine that runs it -- a machine with no daemon gets
// none of the three, so this is the only thing that ever notices a pull
// request behind its base there. `project loop tick` under cron runs this;
// the daemon runs TickIssue for the fast path. Both may run at once -- the
// per-loop lock in RunTick and TickIssue makes an overlapping pass harmless.
func Tick(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error) {
	var sum Summary
	now := deps.Now()

	// Still fetched, and a failure no longer suppresses anything. The fetch
	// existed for two readers: the branch comparisons a tend decided from, and
	// the worktree a dispatch is created in. The comparisons left with tending,
	// so all that is left is keeping the primary checkout current for the
	// worktrees this tick may create -- and a stale checkout there costs a
	// worktree branched from an older origin ref, not a wrong decision. It must
	// not abandon the tick either: reaping dead runners, retrying and parking
	// have nothing to do with git, and abandoning the pass would leave a dead
	// runner's issue with no failure flag at all.
	if deps.Fetch != nil {
		if err := deps.Fetch(ctx); err != nil {
			slog.Error("fetch primary checkout; carrying on with a possibly stale one",
				"loop", cfg.Name, "err", err)
		}
	}

	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	issues, err := deps.GH.ListOpenIssues(ctx, owner, repo)
	if err != nil {
		return sum, err
	}

	// No pull requests, no comparisons, no review activity. A loop does not
	// tend: the project's tend dispatcher owns all of that, and it runs its own
	// pass over the same repository (see loopcmd.TendTick). What is left here is
	// the loop's own issues, which is what a loop was always about.
	snap := engine.Snapshot{Issues: issues}

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
	st := engine.State{
		Issues: states, Running: live, CooldownUntil: time.Time{}, Force: deps.Force,
		Providers: resolveProviders(ctx, cfg, snap.Issues),
	}
	sum.Live = len(live)
	sum.Forced = deps.Force

	if st.CooldownUntil, err = deps.Store.CooldownUntil(cfg.Name); err != nil {
		return sum, err
	}
	// Log the override where it is actually exercised. An unforced tick inside
	// this window would have halted with no decisions and no explanation of
	// which deadline stopped it, so a forced one names the deadline it stepped
	// over -- the operator's receipt for the dispatches that follow.
	if deps.Force && !st.CooldownUntil.IsZero() && now.Before(st.CooldownUntil) {
		slog.Warn("forcing this tick past the circuit breaker cooldown",
			"loop", cfg.Name, "cooldown_until", st.CooldownUntil)
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

	// The same per-pass ceiling TendSweep applies, and for the same reason:
	// this is what ONE trigger may do to the remote and to the token budget.
	// TendSweep bounded itself from the start; the full tick did not need to,
	// because a tend decision required a pull request to be BEHIND its base and
	// a cron sweep found at most a handful. The review-activity trigger removes
	// that natural bound -- every review-labelled pull request carrying an
	// unanswered trusted comment is now a candidate, and on the FIRST tick after
	// this feature is installed every one of them qualifies at once, because no
	// pull request has a finished tend row yet. Without a cap that upgrade
	// dispatches an agent per open review pull request in one pass.
	//
	// Only TEND decisions are capped. A start, a resume, a retry, a park and a
	// stop are all bounded by the labels a human applied, and dropping one would
	// strand the issue rather than delay a rebase.
	tends := 0
	var deferred []int
	for _, d := range plan.Decisions {
		if d.Kind == engine.KindTend {
			if tends >= maxTendPerSweep {
				deferred = append(deferred, d.Issue)
				continue
			}
			tends++
		}
		if err := act(ctx, cfg, deps, d, now, &sum); err != nil {
			// One failed decision must not abandon the rest of the tick.
			slog.Error("decision failed", "loop", cfg.Name, "kind", d.Kind,
				"issue", d.Issue, "err", err)
		}
	}
	if len(deferred) > 0 {
		// Never silent, and the issues are NAMED rather than counted: a capped
		// pass that said nothing would read as "every stale pull request was
		// tended", which is the opposite of the truth. TendSweep's own cap
		// warning states the same rule.
		slog.Warn("tick hit the per-pass tend cap; the rest wait for the next tick",
			"loop", cfg.Name, "tended", sum.Tended, "rebased", sum.Rebased,
			"deferred", deferred)
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

		// Clear the git locks the dead runner left in its worktree. The retry
		// lands in that SAME worktree (EnsureIssue returns an existing one
		// untouched), so an index.lock left by a SIGKILL mid-operation fails
		// every git command the next agent runs -- and it would burn the whole
		// retry budget on debris no agent can clear.
		//
		// Safe precisely here and nowhere else: this runner has just been
		// proven dead and the caller holds the loop lock, so no process can be
		// holding those files. Logged and continued on failure -- the row and
		// the retry flag below are the reap's real job, and abandoning them
		// over debris would strand the issue holding an in-flight label with
		// no agent and no failure recorded.
		clearStaleLocks(cfg, deps, d)

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

// clearStaleLocks removes the git lock files a dead dispatch left behind.
//
// A tend runs in the PULL REQUEST's worktree and every other kind in the
// issue's, so the path is chosen by kind: clearing the wrong one would leave
// the debris that is actually in the way and delete a file in a worktree this
// dispatch never touched.
//
// A loop configured with no per-issue worktree has nothing to clear. Its
// agents run in the primary checkout, which is shared with every other loop
// and with whatever the operator is doing in it, so a lock there may well be
// held by a live git process.
func clearStaleLocks(cfg *config.Config, deps Deps, d store.Dispatch) {
	if cfg.Agent.Worktree != config.WorktreePerIssue || deps.WT == nil {
		return
	}
	path := deps.WT.PathForIssue(d.Number)
	if d.Kind == store.KindTend {
		path = deps.WT.PathForPR(d.PRNumber)
	}
	cleared, err := worktree.ClearStaleLocks(path)
	if err != nil {
		slog.Warn("could not clear stale git locks after a dead runner",
			"loop", cfg.Name, "dispatch", d.ID, "worktree", path, "err", err)
		return
	}
	for _, p := range cleared {
		slog.Info("cleared a stale git lock left by a dead runner",
			"loop", cfg.Name, "dispatch", d.ID, "lock", p)
	}
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
		outcome, pendingConflict, rebaseErr := gitRebase(ctx, cfg, deps, d, now)
		switch {
		case rebaseErr != nil:
			// Logged, not returned: a git failure must not abandon the rest of
			// the sweep, and the agent is the fallback this whole path is
			// built around.
			slog.Warn("automatic rebase failed; falling back to the tend agent",
				"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", rebaseErr)
		case outcome == doneRebased:
			// Count the rebase first, so the summary still reports it. Then
			// fall through to the dispatch ONLY when review feedback is still
			// unanswered: the rebase settled the staleness half of the
			// decision, not the review half.
			sum.Rebased++
			if !d.ReviewPending {
				return nil
			}
		case outcome == doneNoRebase:
			// Settled by declining to act. No agent, and nothing counted. The
			// branch this pass reasoned about is gone -- see gitRebase's doc
			// comment -- so an agent sent at it now would work from a stale
			// premise whatever the feedback says.
			return nil
		case outcome == doneBackedOff:
			// gitRebase already declined to back off a ReviewPending decision
			// -- that check is INSIDE the backoff, not here -- so a
			// doneBackedOff reaching this point always has no feedback to
			// answer. No special case belongs here: adding one back would
			// double-dispatch the agent gitRebase already counted as backed
			// off.
			sum.Backoff++
			return nil
		}
		// The conflict sighting is committed only once the agent is really
		// dispatched. dispatch can still fail after this point -- a worktree it
		// cannot build, a spawn that will not start -- and no agent runs then,
		// so counting it would let a repeating worktree or spawn failure walk a
		// pull request to the 24h tier without the agent having seen the
		// conflict once. That is the same failure the failed-abort path inside
		// gitRebase refuses to cause, reached by a different route, so it gets
		// the same answer: seen_count counts agent dispatches that HAPPENED.
		err := dispatch(ctx, cfg, deps, d, now, store.KindTend)
		if err == nil {
			recordConflictDispatch(cfg, deps, d, pendingConflict)
		}
		return count(&sum.Tended, err)
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
		// The stamps are the EFFECTIVE configuration, not the override alone.
		// dispatches.model and dispatches.harness record only the label and are
		// empty whenever the loop default was used -- an ambiguity the
		// retirement rule cannot carry, because "empty" there would mean both
		// "claude, by default" and "not recorded".
		harness := runner.Effective(cfg, d.Overrides).Harness
		if harness == "" {
			harness = config.HarnessClaude
		}
		if err := deps.Store.BeginDispatch(cfg.Name, cfg.Repo, d.Issue, sessionID,
			harness, d.Provider, isRetry, now); err != nil {
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
		// Not an override, and so not read from d.Overrides: no label names a
		// provider. It is the resolved value the engine compared, carried here
		// so a park comment can name the account that actually failed.
		Provider: d.Provider,
		// ReviewPending travels here, not on pr_links: see
		// store.Dispatch.ReviewPending for why.
		ReviewPending: d.ReviewPending,
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

const retryCapComment = `🔁 **Orphan retry cap reached (%d/%d)** — %d consecutive agent dispatches for this issue failed to complete.%s

To proceed: re-add the ` + "`%s`" + ` label once the underlying issue has cleared, and this resumes normally. Changing the harness, or the model to one on a different provider, clears the cap on its own.`

// capFallback is the wording for a park with no recorded reason. It was once
// the whole comment, and an operator reading it learned nothing actionable --
// the 402 that caused the park was sitting in the dispatch row the entire
// time. It is the fallback now, never the default.
const capFallback = " This usually indicates a sustained platform-side issue rather than a problem with the issue itself. Parking here rather than retrying indefinitely."

// urlPattern matches an absolute URL. Deliberately greedy to the next space: a
// partially redacted URL is worse than none, because the surviving prefix
// still identifies the resource.
var urlPattern = regexp.MustCompile(`https?://\S+`)

// capCause renders the sentence between the cap line and the instruction: what
// actually went wrong, and what was running when it did.
func capCause(d store.Dispatch) string {
	reason := failureSentence(d.APIError)
	if reason == "" {
		return capFallback
	}
	where := ""
	if parts := nonEmpty(d.Harness, d.Model, d.Provider); len(parts) > 0 {
		where = " under " + strings.Join(parts, ", ")
	}
	return fmt.Sprintf(
		"\n\nThe last failure reported:\n\n> %s\n\nThat ran%s. Parking here rather than retrying indefinitely.",
		reason, where)
}

// nonEmpty drops the empty strings, so a claude dispatch -- which records no
// provider -- does not render an empty slot in the parenthetical.
func nonEmpty(vals ...string) []string {
	var out []string
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// failureSentence renders a dispatch's api_error for a PUBLIC GitHub comment,
// or "" when nothing was recorded.
//
// api_error is often a bare status followed by the provider's JSON body --
// `402: {"message":"...","code":402,...}`. The message field is the sentence a
// human needs and the rest is envelope. Extracting it is best-effort: anything
// that does not parse is redacted and truncated as it stands, which still
// beats saying nothing.
func failureSentence(apiError string) string {
	s := strings.TrimSpace(apiError)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "{"); i >= 0 {
		var body struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(s[i:]), &body); err == nil && body.Message != "" {
			prefix := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s[:i]), ":"))
			if prefix != "" {
				return redactForComment(prefix + ": " + body.Message)
			}
			return redactForComment(body.Message)
		}
	}
	return redactForComment(s)
}

// redactForComment strips what must not be published and caps the length.
//
// A provider is free to put anything in an error string, and OpenRouter's 402
// puts a key-management URL in it:
// https://openrouter.ai/workspaces/default/keys/<id>. That names the key and is
// credential-adjacent, so every URL goes. The unredacted text stays on the
// dispatch row and in the run log, where `agent-utils project logs` can still
// reach it.
func redactForComment(s string) string {
	// A marker rather than nothing: providers put URLs mid-sentence ("To
	// increase, visit <url> and adjust the limit"), and deleting one outright
	// leaves a sentence that reads as though a word is missing.
	s = urlPattern.ReplaceAllString(s, "[link redacted]")
	s = strings.Join(strings.Fields(s), " ")
	const max = 300
	if r := []rune(s); len(r) > max {
		// Cut on a rune boundary. Slicing the byte string would split a
		// multi-byte rune and put mojibake in a GitHub comment.
		s = strings.TrimSpace(string(r[:max])) + "…"
	}
	return s
}

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

	// Best-effort. The park is the point of this function, so a read that fails
	// must not stop the comment or the label edits: the cause is omitted and
	// the fallback wording stands.
	var last store.Dispatch
	if runs, err := deps.Store.RecentDispatches(cfg.Name, cfg.Repo, d.Issue, 5); err == nil {
		for _, r := range runs {
			if r.Status == store.StatusFailed {
				last = r
				break
			}
		}
	}

	body := fmt.Sprintf(retryCapComment,
		cfg.Retry.Max, cfg.Retry.Max, cfg.Retry.Max, capCause(last), cfg.Labels.Trigger)
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

	// Two prompts, and a tend uses NEITHER branch: the tend dispatcher's
	// configuration carries the project's tend.prompt in cfg.Prompt, because it
	// is the only prompt that dispatcher has. So a KindTend dispatch takes the
	// default here and starts a fresh session, which is exactly what it must do
	// -- a tend never inherits and never resumes, so "-r" would target a session
	// claude never created and fail in under a second, every time.
	tmpl := cfg.Prompt
	resume := false
	if d.Kind == store.KindResume {
		tmpl, resume = cfg.ResumePrompt, true
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
			// From the DISPATCH row, not the pr_links row: the tick's
			// Decision never reaches this detached process, and pr_links was
			// rejected as a transport -- see store.Dispatch.ReviewPending.
			ReviewPending: d.ReviewPending,
		},
		// A tend gets NO labels, and that is deliberate rather than an
		// oversight: the tend dispatcher's configuration fills all four label
		// roles with the eligibility label purely so the shared validator has
		// something to check, and rendering those into a prompt would tell the
		// agent that its trigger, in-flight, blocked and terminal labels are
		// all the same string. project.Load refuses a tend prompt that
		// references .Labels, so nothing can read what is withheld here.
		Labels: func() runner.PromptLabels {
			if d.Kind == store.KindTend {
				return runner.PromptLabels{}
			}
			return runner.PromptLabels{
				Trigger:  cfg.Labels.Trigger,
				InFlight: cfg.Labels.InFlight,
				Blocked:  cfg.Labels.Blocked,
				Terminal: cfg.Labels.Terminal,
			}
		}(),
		// The eligibility label, and only for a tend. A loop's prompt has its
		// own four labels and no business reading a project-level one.
		Tend: runner.PromptTend{Label: cfg.Tend.Label},
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
