package loopcmd

import (
	"context"
	"log/slog"
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

// rebaseOutcome is what gitRebase settled. It is three values rather than a
// bool because two different outcomes both mean "dispatch no agent" while only
// one of them rebased anything -- a bool made the caller count a REFUSED push
// as a completed rebase in the tick summary an operator audits.
type rebaseOutcome int

const (
	// notDone: the caller must dispatch the tend agent.
	notDone rebaseOutcome = iota
	// doneRebased: git replayed the branch and pushed it.
	doneRebased
	// doneNoRebase: this pass settled the decision by declining to act. No
	// agent, and nothing to count.
	doneNoRebase
)

// gitRebase replays a pull request's branch on its base with git alone, and
// reports whether it settled the decision.
//
// A tend agent exists for the rebases that need judgment. Most do not: the
// base moved, the branch replays cleanly, and the result is a force-push that
// no conversation improves. This function does that case for nothing, and
// hands the rest to the agent unchanged.
//
// Two outcomes mean "dispatch no agent", and the second is the one worth
// reading twice:
//
//   - doneRebased: the rebase replayed and the push landed.
//   - doneNoRebase: the push was REFUSED because the remote moved, or a failed
//     abort forced the worktree to be destroyed. The branch this pass reasoned
//     about is gone, so an agent sent at it now would work from the same stale
//     premise. The next tick reads the new state and decides again.
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
// A veto label, a stopped session, and a parked issue stop the rebase as well
// as the agent. They are applied by engine.Decide, which is upstream of act:
// tendDecisions drops a vetoed issue outright, and a stopped issue is marked
// decided so no tend decision is produced for it. No such issue reaches this
// function at all, so nothing here can rebase one.
//
// That is worth naming because the design intent differs. The operator wanted
// a clean replay to run regardless -- it spends no token and writes no label,
// so a paused issue would still get a current branch. Delivering that means a
// path around the veto and stop filters, which changes what a veto label
// MEANS, and it is tracked as its own decision rather than smuggled in here.
// Until then, this comment describes what the code does.
func gitRebase(ctx context.Context, cfg *config.Config, deps Deps, d engine.Decision) (rebaseOutcome, error) {
	// A loop with no per-issue worktree has no pull-request checkout to rebase
	// in, and this pass will not create one: the agent path already handles
	// that mode.
	if cfg.Agent.Worktree != config.WorktreePerIssue || deps.Git == nil {
		return notDone, nil
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
		return notDone, err
	}
	if dirty {
		slog.Info("tend worktree holds uncommitted or unpushed work; leaving this rebase to the agent",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR)
		return notDone, nil
	}

	path, err := deps.Git.EnsurePRCtx(ctx, d.PR, d.HeadRef)
	if err != nil {
		return notDone, err
	}

	// The lease. It is read AFTER EnsurePRCtx, which fetches the head ref and
	// checks it out, so it is the commit the remote had a moment ago -- which
	// is exactly what the push must be pinned to. PushWithLease refuses an id
	// that is not a full object hash, so a polluted read fails loudly here
	// rather than silently degrading the guard.
	lease, err := deps.Git.HeadSHA(ctx, path)
	if err != nil {
		return notDone, err
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
			return doneNoRebase, nil
		}
		slog.Info("rebase did not replay cleanly; dispatching the tend agent",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", err)
		return notDone, nil
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
		return doneNoRebase, nil
	}

	if err := recordRebase(cfg, deps, d); err != nil {
		// The rebase HAPPENED. A failed record must not report it as undone,
		// or the caller would send an agent at an already-current branch.
		slog.Error("could not record an automatic rebase", "loop", cfg.Name,
			"issue", d.Issue, "pr", d.PR, "err", err)
	}
	slog.Info("rebased a pull request with git; no agent was dispatched",
		"loop", cfg.Name, "issue", d.Issue, "pr", d.PR,
		"head", truncate(d.HeadRef, 120), "base", truncate(d.BaseRef, 120))
	return doneRebased, nil
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
