package loopcmd

import (
	"context"
	"log/slog"
	"path/filepath"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/lock"
)

// CleanupClosedPR removes the worktree of a pull request that just closed --
// merged or not -- and the worktree of the issue it closes, once neither is
// in use.
//
// # Every close, not only a merge
//
// The operator chose this deliberately, with the risk stated: an unmerged
// close often means the work continues and a replacement gets pushed, but
// leaving every closed pull request's checkout on disk forever was the
// larger problem. A worktree of this repository's own monorepo is
// ~866MB (node_modules). The live-dispatch guard below is what keeps this
// from touching work in progress; it does NOT protect uncommitted, unpushed
// work sitting in an otherwise idle worktree -- that loss is the accepted
// risk, which is why removal is logged (see removeWorktree).
//
// # The liveness guard
//
// isLive is reapDead's rule, not Reset's. Reset calls IsAlive directly,
// which is correct there -- an operator invoking Reset is not racing a spawn
// that just happened. This runs on the delivery path, where a dispatch row
// can be seconds old, so Reset's rule would delete a worktree out from under
// an agent whose pid has not been written yet.
//
// A live row for EITHER the issue or the pull request cancels the WHOLE
// cleanup, not just the checkout its own row names: the two checkouts are
// one piece of work, and an issue agent mid-push while its pull request's
// tend worktree is deleted (or the reverse) is the same hazard as deleting
// out from under a live agent directly.
//
// # No API call spent here
//
// prNumber's pull request is fetched through deps.GH, which on the delivery
// path is a *ghub.DeliveryCache: the issue pass that ran moments earlier in
// this same delivery (see loopcmd.TickIssue -> subject) already fetched this
// exact number, so this read is served from that memo.
func CleanupClosedPR(ctx context.Context, cfg *config.Config, deps Deps, prNumber int) error {
	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		return err
	}
	defer l.Release()

	owner, repo := cfg.RepoOwner(), cfg.RepoName()
	pr, err := deps.GH.PullRequest(ctx, owner, repo, prNumber)
	if err != nil {
		return err
	}
	issueNumber, hasIssue := engine.ClosesIssue(pr)

	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		return err
	}
	now := deps.Now()
	for _, d := range running {
		relevant := d.PRNumber == prNumber || (hasIssue && d.Number == issueNumber)
		if !relevant {
			continue
		}
		if isLive(d, deps.IsAlive, now) {
			slog.Info("skipping worktree cleanup: a live dispatch is using it",
				"loop", cfg.Name, "pr", prNumber, "issue", d.Number, "dispatch", d.ID)
			return nil
		}
	}

	removeWorktree(cfg, deps, deps.WT.PathForPR(prNumber))
	if hasIssue {
		removeWorktree(cfg, deps, deps.WT.PathForIssue(issueNumber))
	}
	return nil
}

// removeWorktree deletes path. It does not block on path carrying
// uncommitted changes -- the operator chose to reclaim the disk regardless
// -- but it logs a warning naming the worktree first, so the loss is visible
// after the fact rather than silent.
func removeWorktree(cfg *config.Config, deps Deps, path string) {
	dirty, err := deps.WT.Dirty(path)
	if err != nil {
		slog.Error("check worktree for uncommitted changes before removing it",
			"loop", cfg.Name, "path", path, "err", err)
	} else if dirty {
		slog.Warn("removing a worktree that had uncommitted changes",
			"loop", cfg.Name, "path", path)
	}
	if err := deps.WT.Remove(path); err != nil {
		slog.Error("remove worktree", "loop", cfg.Name, "path", path, "err", err)
	}
}
