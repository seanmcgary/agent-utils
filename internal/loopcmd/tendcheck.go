package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/lock"
)

// TendCheckResult is what one candidate pass found.
type TendCheckResult struct {
	// Stale is the number of pull requests confirmed behind their base.
	Stale int
	// Confirmed reports that the pass called GitHub. A pass that gated locally
	// and found nothing reports false, which is the case the caller's log line
	// is worth staying quiet about.
	Confirmed bool
}

// TendCheck answers one question: has any of this loop's pull requests fallen
// behind its base?
//
// It exists so the daemon can ask that question on a timer without paying for
// it. The GitHub equivalent costs two list calls plus one comparison per pull
// request, per loop, per project, on every interval. This reads refs the loop's
// own fetch already updated, so the common case -- nothing is behind -- costs
// no request at all.
//
// Three properties are load-bearing. Do not break them.
//
//  1. The local step is a GATE, never a decision. It decides only whether to
//     spend the API calls. A pr_links row can be stale -- a pull request that
//     merged, an issue that lost its review label -- so a dispatch made on the
//     local answer alone would rebase work that is already done.
//  2. Zero GitHub calls when nothing is behind and force is false. This is the
//     whole reason the pass can run every fifteen minutes.
//  3. TendSweep stays the only code that decides what to dispatch. This
//     function reports a count; it dispatches nothing and writes no issue
//     state.
//
// force runs the confirm step whether or not anything looks behind. The caller
// sets it on the first pass after the daemon starts and every few hours after
// that, because the gate can only skip the calls when it has rows to trust: a
// loop with no rows would otherwise stay silent forever, and a row that drifted
// would stay wrong forever.
func TendCheck(ctx context.Context, cfg *config.Config, deps Deps, force bool) (TendCheckResult, error) {
	var out TendCheckResult
	if !cfg.TendPR {
		return out, nil
	}

	// A seam that a hand-built Deps may leave nil, the way Deps.Fetch is
	// nil-guarded in tendDispatch. Without this the daemon panics on the Serve
	// goroutine and takes every project down with it.
	if deps.Behind == nil {
		return out, nil
	}

	// The loop lock, for the same reason every other Fetch in this package is
	// under one. This pass fetches -- which moves the refs a concurrent rebase
	// resolves -- and deletes pr_links rows, which races PutPRLink; tendSnapshot
	// records that this package keeps a single writer under the lock
	// deliberately.
	//
	// It does NOT wait. A held lock means a tick is already running for this
	// loop, and that tick does this pass's work as part of its own: the sweep it
	// performs is the thing this pass would have armed. Waiting would pin the
	// caller's goroutine behind an agent dispatch.
	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if errors.Is(err, lock.ErrHeld) {
		slog.Info("another tick holds the loop lock; skipping this tend check",
			"loop", cfg.Name)
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer l.Release()

	// Before every comparison below, because each of them reads a remote
	// tracking ref this updates. A stale answer is worse than no answer: it
	// reports a branch as current after the base moved.
	if deps.Fetch != nil {
		if err := deps.Fetch(); err != nil {
			return out, fmt.Errorf("fetch primary checkout: %w", err)
		}
	}

	links, err := deps.Store.PRLinks(cfg.Name, cfg.Repo)
	if err != nil {
		return out, err
	}

	behind := make(map[int]bool, len(links))
	for number, link := range links {
		n, known, err := deps.Behind(link.HeadRef, link.BaseRef)
		if err != nil {
			// One unusable row must not abandon the pass, for the reason
			// tendSnapshot gives: anyone able to open a pull request could
			// otherwise stop every rebase this loop would do.
			slog.Warn("local compare failed; skipping this pull request",
				"loop", cfg.Name, "issue", number, "pr", link.PRNumber, "err", err)
			continue
		}
		// An unknown ref is a branch the prune removed, which is a pull request
		// whose branch is gone. It is not a candidate and not an error.
		if !known || n <= 0 {
			continue
		}
		behind[number] = true
	}

	if len(behind) == 0 && !force {
		return out, nil
	}

	owner, repo := cfg.RepoOwner(), cfg.RepoName()
	prs, err := deps.GH.ListOpenPullRequests(ctx, owner, repo)
	if err != nil {
		return out, err
	}
	issues, err := deps.GH.ListOpenIssues(ctx, owner, repo)
	if err != nil {
		return out, err
	}
	out.Confirmed = true

	open := make(map[int]bool, len(prs))
	for _, pr := range prs {
		open[pr.Number] = true
	}

	// The rows for pull requests that are no longer open. Nothing deleted one
	// before this pass existed, so a database accumulates them -- and the gate
	// above would count a merged branch as behind its base on every interval,
	// forever.
	for number, link := range links {
		if open[link.PRNumber] {
			continue
		}
		if err := deps.Store.DeletePRLink(cfg.Name, cfg.Repo, number); err != nil {
			// Named for the failure, like every other Error line in this package
			// ("decision failed", "retire dead dispatch").
			slog.Error("could not delete a pr link whose pull request is closed",
				"loop", cfg.Name, "issue", number, "pr", link.PRNumber, "err", err)
			continue
		}
		// Info, not Debug: nothing in this program logs at Debug, no handler is
		// configured for it, and a row disappearing from the database is a state
		// change an operator should be able to find afterwards.
		slog.Info("dropped a pr link whose pull request is closed",
			"loop", cfg.Name, "issue", number, "pr", link.PRNumber)
	}

	// The count comes from what GitHub just returned, NOT from the rows the gate
	// read. The rows are how this pass decides whether to look; they are not
	// what it reports. A forced pass on a loop with no rows at all -- the first
	// pass after the daemon starts -- would otherwise confirm nothing and report
	// zero, which is the one case force exists to cover.
	for _, iss := range issues {
		if !iss.HasLabel(cfg.Labels.Review) {
			continue
		}
		// LinkPR, not a lookup by a row's pr_number, and that is the point. It
		// links only a TRUSTED pull request -- same repository, an owner, member
		// or collaborator, both refs safe (internal/ghub/ghub.go:107-118) -- that
		// names this issue in a closing reference. A stored row carries none of
		// that, so trusting it would let a fork's branch reach the rebase path
		// this feature adds.
		pr, ok := engine.LinkPR(iss.Number, prs)
		if !ok {
			continue
		}
		// The same boundary TendSweep enforces: a pull request targeting
		// release/1.0 is behind for reasons this loop's default branch knows
		// nothing about, and rebasing it would be a tend nobody asked for.
		if pr.BaseRef != cfg.DefaultBranch {
			continue
		}
		n, known, err := deps.Behind(pr.HeadRef, pr.BaseRef)
		if err != nil {
			slog.Warn("local compare failed; skipping this pull request",
				"loop", cfg.Name, "issue", iss.Number, "pr", pr.Number, "err", err)
			continue
		}
		if !known || n <= 0 {
			continue
		}
		out.Stale++
	}
	return out, nil
}
