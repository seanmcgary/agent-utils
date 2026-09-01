package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"

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

// TendCheck answers one question: has any of this project's pull requests
// fallen behind its base?
//
// It runs on the tend dispatcher's own configuration, so the rows it reads and
// the lock it takes are the dispatcher's, not any loop's. It is called once per
// PROJECT per interval rather than once per loop, which is a real saving as
// well as a correction: before tending had a dispatcher, every loop of every
// project was opened on every interval so that all but one of them could
// discover it did not tend.
//
// It exists so the daemon can ask that question on a timer without paying for
// it. The GitHub equivalent costs two listings plus one comparison per pull
// request, per project, on every interval -- and the listings are
// PAGINATED at 100, so a busy repository pays two requests per page rather
// than two requests. This reads refs the dispatcher's own fetch already updated, so
// the common case -- nothing is behind -- costs no request at all.
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
// No call to LatestReviewActivity belongs here either, for the same reason
// property 2 exists: this function's whole contract is zero GitHub calls when
// nothing is behind, and review activity is invisible to a local checkout --
// there is no local ref that records who commented on a pull request. Adding
// it here would either cost a request per candidate on every interval (paid
// even when nothing is behind, breaking property 2) or require a second,
// independent read of the same GitHub state tickIssue and Tick already cover.
// It deletes a tend_conflicts row below, which is a store call, not a GitHub
// call, and does not touch this contract.
//
// force runs the confirm step whether or not anything looks behind. The caller
// sets it on the first pass after the daemon starts and every few hours after
// that, because the gate can only skip the calls when it has rows to trust: a
// loop with no rows would otherwise stay silent forever, and a row that drifted
// would stay wrong forever.
func TendCheck(ctx context.Context, cfg *config.Config, deps Deps, force bool) (TendCheckResult, error) {
	var out TendCheckResult

	// A seam that a hand-built Deps may leave nil, the way Deps.Fetch is
	// nil-guarded in tendDispatch. Without this the daemon panics on the Serve
	// goroutine and takes every project down with it.
	if deps.Behind == nil {
		return out, nil
	}

	// The tend dispatcher's lock, for the same reason every other Fetch in this
	// package is under one. This pass fetches -- which moves the refs a
	// concurrent rebase resolves -- and deletes pr_links rows, which races
	// PutPRLink; tendSnapshot records that this package keeps a single writer
	// under the lock deliberately.
	//
	// It does NOT wait. A held lock means a tend pass is already running, and
	// that pass does this one's work as part of its own: the sweep it performs
	// is the thing this pass would have armed. Waiting would pin the caller's
	// goroutine behind an agent dispatch.
	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if errors.Is(err, lock.ErrHeld) {
		slog.Info("another tend pass holds the dispatcher's lock; skipping this tend check",
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
		if err := deps.Fetch(ctx); err != nil {
			return out, fmt.Errorf("fetch primary checkout: %w", err)
		}
	}

	links, err := deps.Store.PRLinks(cfg.Name, cfg.Repo)
	if err != nil {
		return out, err
	}

	// A sorted slice of the keys, not a range over the map, and both passes
	// below use it. Go randomises map iteration, so a loop with three failing
	// compares -- or three rows to delete -- would print the same three lines
	// in a different order on every interval, and an operator diffing two
	// passes could not tell a changed state from a reshuffled one.
	// internal/config.Load records the same rule for the same reason.
	numbers := make([]int, 0, len(links))
	for number := range links {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)

	// A count, not a set. The keys came from a map, so they are unique
	// already, and nothing reads them back: the only question asked below is
	// whether anything at all looked behind.
	behind := 0
	for _, number := range numbers {
		link := links[number]
		n, known, err := deps.Behind(ctx, link.HeadRef, link.BaseRef)
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
		behind++
	}

	if behind == 0 && !force {
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
	for _, number := range numbers {
		link := links[number]
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
		// The conflict row follows the pull request out. It is a separate
		// table and a separate call -- not part of the pr_links delete above
		// -- so its own failure must not abandon the pr_links delete that
		// already succeeded; log and continue exactly as that one does.
		if err := deps.Store.DeleteTendConflict(cfg.Name, cfg.Repo, link.PRNumber); err != nil {
			slog.Error("could not delete a tend conflict row whose pull request is closed",
				"loop", cfg.Name, "issue", number, "pr", link.PRNumber, "err", err)
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
		if !iss.HasLabel(cfg.Tend.Label) {
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
		// release/1.0 is behind for reasons the project's default branch knows
		// nothing about, and rebasing it would be a tend nobody asked for.
		if pr.BaseRef != cfg.DefaultBranch {
			continue
		}
		n, known, err := deps.Behind(ctx, pr.HeadRef, pr.BaseRef)
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
