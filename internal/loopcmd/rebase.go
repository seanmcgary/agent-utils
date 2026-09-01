package loopcmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// RebaseGit is the git the automatic rebase needs, and nothing else.
//
// It is an interface, not the concrete *worktree.Manager, for one reason: the
// rebase path is the only code in this program that force-pushes, and a test
// of its branching must be able to make a push fail without a remote to fail
// against. *worktree.Manager satisfies it, and Open wires it in.
type RebaseGit interface {
	PathForPR(number int) string
	EnsurePRCtx(ctx context.Context, number int, headRef string) (string, error)
	DirtyCtx(ctx context.Context, path string) (bool, error)
	HeadSHA(ctx context.Context, path string) (string, error)
	Rebase(ctx context.Context, path, baseRef string) error
	AbortRebase(ctx context.Context, path string) error
	RemoveCtx(ctx context.Context, path string) error
	PushWithLease(ctx context.Context, path, headRef, lease string) error
	// ConflictedPaths returns the paths a rebase left in conflict, sorted. It
	// is here, on the interface, for the same reason every other method is:
	// the repeat-conflict backoff branches on it, and a test of that branch
	// must be able to drive a conflict without a real git worktree.
	ConflictedPaths(ctx context.Context, path string) ([]string, error)
}

// rebaseBudget bounds one whole automatic rebase, from the dirty check to the
// push.
//
// worktree.gitTimeout bounds one git COMMAND, not the operation: EnsurePRCtx
// alone runs up to three of them, so the per-command bound leaves a worst case
// of six minutes before this function has even read the lease, and around
// fourteen across every command it can run. act holds the loop lock for all of
// it, so nothing else about this loop -- no webhook delivery, no cron tick --
// proceeds meanwhile. This is the bound on the whole thing, and it is what a
// caller passing context.Background() would have left absent.
//
// A deadline reached mid-rebase kills the rebase. The cleanup that follows
// runs on its own detached context so it is not killed too; see cleanupBudget.
const rebaseBudget = 5 * time.Minute

// cleanupBudget bounds the abort, and the removal behind it, on a context
// detached from the caller's.
//
// It is deliberately small. This runs after something already went wrong, and
// the two commands it covers are local -- no fetch, no network -- so a
// generous budget would buy nothing and would hold the loop lock past a
// shutdown that has already been asked for.
const cleanupBudget = 30 * time.Second

// rebaseOutcome is what gitRebase settled. It now names FOUR outcomes, three
// of which mean "dispatch no agent" -- doneNoRebase, doneBackedOff, and (when
// the decision carries no unanswered review activity) doneRebased -- while
// only doneRebased itself rebased anything. A bool would have made the caller
// count a REFUSED push, or a declined repeat, as a completed rebase in the
// tick summary an operator audits.
type rebaseOutcome int

const (
	// notDone: the caller must dispatch the tend agent.
	notDone rebaseOutcome = iota
	// doneRebased: git replayed the branch and pushed it.
	doneRebased
	// doneNoRebase: this pass settled the decision by declining to act. No
	// agent, and nothing to count.
	doneNoRebase
	// doneBackedOff: the rebase conflicted, the conflict is one that already
	// defeated the agent at this fingerprint, and the deadline has not
	// passed. No agent, and nothing written to tend_conflicts -- see the
	// backoff gate below for why a declining pass must write nothing.
	doneBackedOff
)

// conflictBackoff[n-1] is the wait after the nth agent dispatch at one
// fingerprint. Index n-1, not n: a pull request with no row has never had an
// agent sent at this conflict, and the agent is the right answer to a
// conflict it has not seen, so the first sighting dispatches with no wait and
// consults no entry here.
//
// A package variable, not a configuration field, for the same reason
// maxTendPerSweep is: no operator has needed a different value, and every
// value the schedule could take is small enough that changing it is a code
// change and a release, not a per-loop knob to keep straight.
var conflictBackoff = []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour}

// conflictFingerprint identifies one rebase conflict by its head commit and
// its conflicted paths, so a later pass can recognise a REPEAT of the same
// conflict rather than a new one.
//
// The BASE commit is deliberately EXCLUDED. Including it looks safer and
// defeats the whole feature: a tend sweep is armed by the base branch moving,
// so the base differs on every sweep by construction, and a fingerprint
// carrying it would be new every time and would suppress nothing. Finding 5
// is exactly this shape -- one pull request met the same CLAUDE.md conflict
// on four sweeps in five hours, and every one of those sweeps had a new base.
//
// headSHA is the value gitRebase already reads as the push lease: read after
// EnsurePRCtx checked out FETCH_HEAD, so it is the remote head of the branch,
// and read before Rebase, so a mid-rebase HEAD never pollutes it. The head
// IS included, and it is what lets the backoff release: a head that moved is
// an agent, or a human, that changed the branch, so whatever conflict this
// pass meets next is not the one already tried.
//
// paths must already be sorted -- ConflictedPaths guarantees this -- and are
// joined on NUL, the one byte a path cannot hold, matching how they were read
// with "-z" in the first place.
func conflictFingerprint(headSHA string, paths []string) string {
	sum := sha256.Sum256([]byte(headSHA + "\x00" + strings.Join(paths, "\x00")))
	return hex.EncodeToString(sum[:])
}

// gitRebase replays a pull request's branch on its base with git alone, and
// reports whether it settled the decision.
//
// A tend agent exists for the rebases that need judgment. Most do not: the
// base moved, the branch replays cleanly, and the result is a force-push that
// no conversation improves. This function does that case for nothing, and
// hands the rest to the agent unchanged.
//
// Three outcomes mean "dispatch no agent", and the second and third are the
// ones worth reading twice:
//
//   - doneRebased: the rebase replayed and the push landed.
//   - doneNoRebase: the push was REFUSED because the remote moved, or a failed
//     abort forced the worktree to be destroyed. The branch this pass reasoned
//     about is gone, so an agent sent at it now would work from the same stale
//     premise. The next tick reads the new state and decides again.
//   - doneBackedOff: the rebase conflicted, and the conflict is one that
//     already defeated the agent within its backoff window. See
//     checkConflictBackoff.
//
// now is the caller's own clock, passed rather than read here through
// deps.Now(): act already holds one now for the whole pass and hands it to
// dispatch and reapDead too, and a second read here would let a test that
// pins the tick's clock see two different times in one pass.
//
// # Guards
//
// Two, and only two:
//
//  1. --force-with-lease, pinned to the commit this pass fetched. Git refuses
//     the push when the remote moved, so a commit somebody else pushed is
//     never overwritten. This is enforced by git, not by this program.
//  2. No live dispatch for the issue or the pull request. engine.Decide
//     already suppresses a tend decision while an agent works that issue, so a
//     decision reaching this function has passed it. A rebase under a running
//     agent is the same hazard as two agents.
//
// One more was considered and deliberately REJECTED by the operator, and does
// not belong here: commit authorship is not inspected. A branch carrying a
// human's commits is rebased like any other, because the lease already refuses
// the push that would lose work.
//
// # What stops a rebase that this function never sees
//
// A stopped session and a live agent stop the rebase as well as the agent.
// They are applied by engine.DecideTend, which is upstream of act: a stopped
// issue and one already holding an agent anywhere in the project are both
// skipped there, so no such issue reaches this function at all and nothing
// here can rebase one.
//
// That is worth naming because the design intent differs. The operator wanted
// a clean replay to run regardless -- it spends no token and writes no label,
// so a paused issue would still get a current branch. Delivering that means a
// path around the veto and stop filters, which changes what a veto label
// MEANS, and it is tracked as its own decision rather than smuggled in here.
// Until then, this comment describes what the code does.
func gitRebase(ctx context.Context, cfg *config.Config, deps Deps, d engine.Decision, now time.Time) (rebaseOutcome, *store.TendConflict, error) {
	// A loop with no per-issue worktree has no pull-request checkout to rebase
	// in, and this pass will not create one: the agent path already handles
	// that mode.
	//
	// The repeat-conflict backoff is inert for such a loop, and that is a real
	// limitation rather than an oversight. The fingerprint is built from the
	// conflicted paths, which only exist in a worktree this pass rebased, so a
	// loop running agent.worktree: none still hands the same conflict to the
	// agent on every tick. Fixing it means fingerprinting a conflict this pass
	// never produced, which is a different feature; docs/configuration.md says
	// so where it documents the schedule.
	if cfg.Agent.Worktree != config.WorktreePerIssue || deps.Git == nil {
		return notDone, nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, rebaseBudget)
	defer cancel()

	// The dirty check comes FIRST, before the worktree is refreshed, and that
	// ordering is the whole guard. EnsurePRCtx runs "checkout --detach
	// FETCH_HEAD" on an existing worktree, which orphans any local commits an
	// agent left behind -- so a check made afterwards asks about a tree the
	// refresh already flattened, and DirtyCtx's ahead-of-upstream test returns
	// false for a detached worktree by construction. Checking the stale path
	// before the refresh is what lets a crashed agent's unpushed work stop
	// this pass.
	dirty, err := deps.Git.DirtyCtx(ctx, deps.Git.PathForPR(d.PR))
	if err != nil {
		return notDone, nil, err
	}
	if dirty {
		slog.Info("tend worktree holds uncommitted or unpushed work; leaving this rebase to the agent",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR)
		return notDone, nil, nil
	}

	path, err := deps.Git.EnsurePRCtx(ctx, d.PR, d.HeadRef)
	if err != nil {
		return notDone, nil, err
	}

	// The lease. It is read AFTER EnsurePRCtx, which fetches the head ref and
	// checks it out, so it is the commit the remote had a moment ago -- which
	// is exactly what the push must be pinned to. PushWithLease refuses an id
	// that is not a full object hash, so a polluted read fails loudly here
	// rather than silently degrading the guard.
	lease, err := deps.Git.HeadSHA(ctx, path)
	if err != nil {
		return notDone, nil, err
	}

	if err := deps.Git.Rebase(ctx, path, d.BaseRef); err != nil {
		// The cleanup runs on a context DETACHED from the one that may have
		// just failed. The commonest way to reach this line is the rebase
		// budget expiring or the daemon shutting down, and the worktree
		// helpers use exec.CommandContext: an already-dead context makes a git
		// command fail without running git at all. Reusing ctx here would make
		// the abort fail by construction in exactly the case the abort exists
		// for, and then make the removal below fail the same way -- and a
		// half-removed worktree is WORSE than a stuck one. "worktree remove"
		// would fail, os.RemoveAll would succeed because it takes no context,
		// and "worktree prune" would fail, leaving the directory gone and its
		// registration behind. Every later "worktree add" at that path then
		// fails permanently with "missing but already registered worktree",
		// and nothing in this program prunes worktrees -- Manager.Fetch prunes
		// remote refs. One expired deadline would kill that pull request's
		// tend path for good, agent escalation included, until a human ran
		// "git worktree prune".
		//
		// With a live context the abort usually SUCCEEDS, which repairs the
		// worktree rather than destroying it.
		cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), cleanupBudget)
		defer cancelCleanup()

		// The repeat-conflict backoff gate. It runs BEFORE the abort below,
		// on the same detached cleanupCtx, because the abort clears the
		// conflicted paths this gate needs to fingerprint the conflict -- see
		// worktree.Manager.ConflictedPaths and conflictFingerprint.
		backedOff, pendingConflict := checkConflictBackoff(cleanupCtx, cfg, deps, d, path, lease, now)

		// Unconditional, and its own error is logged rather than returned: a
		// worktree left mid-rebase fails every later pass for this pull
		// request, and the rebase failure below is the one worth reporting.
		if abortErr := deps.Git.AbortRebase(cleanupCtx, path); abortErr != nil {
			// The abort failed on a context that was still alive, so the
			// worktree really may still hold .git/rebase-merge -- and an agent
			// started in it can force-push a half-replayed tree. Worktrees are
			// stable across ticks, so the broken state would persist.
			//
			// Destroy it and dispatch nobody. The next pass builds it fresh.
			slog.Error("could not abort a failed rebase; removing the worktree and dispatching nothing",
				"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", abortErr)
			if rmErr := deps.Git.RemoveCtx(cleanupCtx, path); rmErr != nil {
				slog.Error("could not remove a worktree left mid-rebase",
					"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", rmErr)
			}
			// pendingConflict is deliberately DROPPED here. No agent is
			// dispatched on this path, and seen_count counts agent
			// dispatches; committing it would let repeated abort failures
			// escalate a pull request to the 24h tier without the agent ever
			// having seen the conflict once.
			return doneNoRebase, nil, nil
		}
		if backedOff {
			return doneBackedOff, nil, nil
		}
		// The abort succeeded and the agent is the answer to this conflict, so
		// the prepared row is handed back for the caller to commit. It is NOT
		// committed here: dispatch can still fail -- a worktree it cannot
		// build, a spawn that will not start -- and no agent runs then. See
		// act's KindTend case.
		slog.Info("rebase did not replay cleanly; dispatching the tend agent",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", err)
		return notDone, pendingConflict, nil
	}

	if err := deps.Git.PushWithLease(ctx, path, d.HeadRef, lease); err != nil {
		// The lease did its job, or the remote is unreachable. Either way this
		// pass acts no further and dispatches no agent; see the doc comment.
		//
		// The branch name is truncated because it reaches this line from a
		// webhook payload by way of a database row, and worktree.SafeRef
		// bounds its CHARSET but not its LENGTH.
		slog.Warn("force-with-lease push refused; leaving this branch alone",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR,
			"head", truncate(d.HeadRef, 120), "err", err)
		// done, but NOT rebased. The caller must not count this: the summary
		// is the surface an operator audits unattended force-pushes with, and
		// nothing was pushed.
		return doneNoRebase, nil, nil
	}

	if err := recordRebase(cfg, deps, d); err != nil {
		// The rebase HAPPENED. A failed record must not report it as undone,
		// or the caller would send an agent at an already-current branch.
		slog.Error("could not record an automatic rebase", "loop", cfg.Name,
			"issue", d.Issue, "pr", d.PR, "err", err)
	}
	// A branch that replayed cleanly has no conflict left to remember.
	// Handled explicitly and logged, not returned: errcheck forbids a bare
	// "_ =", but the rebase HAPPENED regardless, and reporting it as undone
	// would send an agent at an already-current branch.
	if err := deps.Store.DeleteTendConflict(cfg.Name, cfg.Repo, d.PR); err != nil {
		slog.Error("could not clear the tend-conflict backoff row after a clean rebase",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", err)
	}
	slog.Info("rebased a pull request with git; no agent was dispatched",
		"loop", cfg.Name, "issue", d.Issue, "pr", d.PR,
		"head", truncate(d.HeadRef, 120), "base", truncate(d.BaseRef, 120))
	return doneRebased, nil, nil
}

// checkConflictBackoff is the repeat-conflict backoff gate. It reports
// whether this pass must decline to dispatch the agent at the conflict d's
// rebase just met.
//
// It WRITES nothing. It returns the row that should be recorded if the agent
// really is dispatched, and recordConflictDispatch commits it once the caller
// has committed to dispatching -- see the comment on the return below for the
// failed-abort case that forces the split.
//
// The gate fails OPEN: a read it could not make -- the conflicted-path list
// or the stored row -- dispatches the agent, because a gate that declines to
// spend money must never be able to strand a pull request on state it could
// not read. This is the opposite direction from the review trigger in
// engine.DecideTend, which fails CLOSED, because the two gates guard
// opposite defaults: the review trigger decides whether to ACT at all, so an
// unreadable input must not manufacture a reason to spend; this gate decides
// whether to REFUSE an action already decided, so an unreadable input must
// not manufacture a reason to withhold it.
//
// ctx is the DETACHED cleanupCtx gitRebase built for the abort, not the ctx
// that may have just expired: ConflictedPaths runs exec.CommandContext, and a
// dead context fails the command without running git at all.
func checkConflictBackoff(
	ctx context.Context,
	cfg *config.Config,
	deps Deps,
	d engine.Decision,
	path, headSHA string,
	now time.Time,
) (backedOff bool, pending *store.TendConflict) {
	paths, err := deps.Git.ConflictedPaths(ctx, path)
	if err != nil {
		// A rebase failure that leaves no readable conflicted-path list is
		// not a conflict this gate understands; fail open rather than stall
		// the pull request on a read it could not make.
		slog.Warn("could not read conflicted paths; skipping the repeat-conflict backoff",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", err)
		return false, nil
	}
	if len(paths) == 0 {
		// A rebase can fail for reasons that leave no conflicted path -- a
		// dead context, a bad ref -- and refusing to dispatch on THAT would
		// be a silent stall, not a backoff.
		return false, nil
	}

	fp := conflictFingerprint(headSHA, paths)
	row, ok, err := deps.Store.TendConflict(cfg.Name, cfg.Repo, d.PR)
	if err != nil {
		slog.Warn("could not read the tend-conflict backoff row; dispatching the agent",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", err)
		return false, nil
	}

	// d.ReviewPending is checked HERE, inside the backoff, rather than by
	// letting act override doneBackedOff afterward. The backoff's evidence is
	// a repeated rebase conflict and says nothing about whether a reviewer's
	// comment has been answered, so a review-pending decision must never be
	// backed off -- and because it therefore reaches the agent, THIS pass IS
	// a dispatch at this fingerprint and must advance seen_count like any
	// other. Letting act override the outcome instead would dispatch the
	// agent without ever advancing the count, so a stuck conflict with a
	// talkative reviewer would be bounded by nothing.
	if ok && row.Fingerprint == fp && now.Before(row.RetryAfter) && !d.ReviewPending {
		// Backed off. Nothing is written: a sweep is armed by every merge and
		// every push, so a pass that merely observes this conflict happens
		// many times an hour, and a write here would push retry_after
		// forward faster than it arrives -- the agent would never be
		// dispatched again. seen_count counts agent DISPATCHES that met this
		// fingerprint, never passes that only looked.
		slog.Info("repeat rebase conflict backed off",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR,
			"seen_count", row.SeenCount, "retry_after", row.RetryAfter,
			"conflicted_paths", len(paths),
			"paths", truncate(strings.Join(paths, ", "), 200))
		return true, nil
	}

	// The agent is about to be dispatched at this fingerprint. count is 1 on
	// a first sighting or a changed fingerprint -- a pull request with no
	// matching row has never had an agent sent at THIS conflict, and the
	// agent is the right answer to a conflict it has not seen.
	count := 1
	if ok && row.Fingerprint == fp {
		count = row.SeenCount + 1
	}
	idx := count
	if idx > len(conflictBackoff) {
		idx = len(conflictBackoff)
	}
	retryAfter := now.Add(conflictBackoff[idx-1])
	newRow := store.TendConflict{
		Loop: cfg.Name, Repo: cfg.Repo, PRNumber: d.PR,
		Fingerprint: fp, SeenCount: count, LastSeenAt: now, RetryAfter: retryAfter,
	}
	if ok && row.Fingerprint == fp {
		newRow.FirstSeenAt = row.FirstSeenAt
	} else {
		newRow.FirstSeenAt = now
	}
	// The row is RETURNED, not written here. Between this point and the
	// dispatch sits the abort, and a failed abort destroys the worktree and
	// returns doneNoRebase -- no agent. Writing here would advance seen_count
	// for a dispatch that never happened, and repeated abort failures at a
	// stable head would walk a pull request to the 24h tier without the agent
	// ever having seen the conflict once. seen_count means agent dispatches,
	// so only the caller, which knows whether it is really about to dispatch,
	// may commit it.
	return false, &newRow
}

// A row this function writes is deleted when git replays the branch cleanly,
// when TendCheck drops a closed pull request's pr_links row, and when the
// closed-pull-request cleanup runs. It is NOT deleted when the AGENT resolves
// the conflict and pushes: the pull request is then current, so no tend
// decision is produced, and gitRebase never runs again to notice. The row
// survives until the pull request closes. That is harmless -- the agent's push
// moved the head, so the fingerprint no longer matches and the backoff cannot
// suppress anything -- but it is state nothing prunes, so do not read the
// deletion sites as "the row always follows the conflict out".
//
// recordConflictDispatch commits the row checkConflictBackoff prepared, at the
// point the caller has committed to dispatching the agent.
//
// A failed write is logged, not returned: the agent still runs, and losing one
// backoff round is better than abandoning the pass.
func recordConflictDispatch(cfg *config.Config, deps Deps, d engine.Decision, row *store.TendConflict) {
	if row == nil {
		return
	}
	if err := deps.Store.PutTendConflict(*row); err != nil {
		slog.Error("could not write the tend-conflict backoff row",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", err)
	}
}

// recordRebase writes the row that gives a force-push a cause.
//
// Without it the only evidence is a force-push in the pull request's timeline
// and a log line in a daemon an operator may not be watching.
//
// The session identifier is empty on purpose. There is no conversation.
// sessionsFrom skips a dispatch with no session, so a rebase never appears in
// `sessions list` and never distorts a session's run count or cost, while
// `project logs --list` shows it like any other dispatch.
func recordRebase(cfg *config.Config, deps Deps, d engine.Decision) error {
	return deps.Store.RecordFinishedDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: d.Issue, Kind: store.KindRebase,
		PRNumber: d.PR, Title: d.Title,
	})
}
