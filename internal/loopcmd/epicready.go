package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/epic"
	"github.com/seanmcgary/agent-utils/internal/lock"
)

// EpicReady sweeps one epic because an operator asked for it by hand, and then
// takes the request back off the issue.
//
// # Why a closure cannot cover this
//
// EpicSweep enters at a CLOSED sub-issue: it walks up to the parent and
// considers that parent's children. Everything after the entry is shared, so
// one sweep is one sweep -- but the entry itself is the problem. An epic whose
// children have never closed produces no such delivery, so its FIRST promotion
// has nothing to arm it, and an epic whose first child is unblocked from the
// start waits forever with no sign that anything is wrong.
//
// Applying epic.Label cannot arm it either, even though that delivery does
// arrive. An issue is labelled an epic BEFORE its sub-issues and dependencies
// are attached -- the label is what an operator applies to start building the
// graph -- so a sweep armed there walks an epic with no children.
// epic.ReadyLabel is applied last, when the graph is entered, which is the
// first moment there is anything to decide.
//
// # The re-read is the authority, not the delivery
//
// The delivery says a label was applied at some past moment. This reads the
// issue and requires BOTH labels to be present now: epic.Label, because that is
// the only switch and without it anyone able to apply one label could sweep an
// arbitrary issue as though it were an epic; and epic.ReadyLabel, so a delivery
// that arrives after the operator took the button back off does not sweep. One
// read answers both.
//
// # Order: sweep, then consume
//
// The label is removed AFTER the sweep, and a failed removal is logged rather
// than returned. Removing first would spend the button on a sweep that might
// fail, leaving an operator with nothing pressed and nothing promoted. A
// removal that fails the other way leaves a label an operator can clear by
// hand, with every promotion already written.
func EpicReady(ctx context.Context, cfg *config.Config, deps Deps, number int) (Summary, error) {
	var sum Summary
	if deps.Epic == nil {
		return sum, errors.New("epic ready: no EpicReader; Deps.Epic is nil")
	}
	// Before the read, so a loop that may not promote spends no API call. The
	// downstream loops see this delivery too -- every loop watching the
	// repository is a target -- and a read per loop per press is the fan-out
	// this guard exists to keep at one.
	if !isEntryLoop(cfg, deps) {
		return sum, nil
	}

	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	iss, err := deps.GH.Issue(ctx, owner, repo, number)
	if err != nil {
		// NOT treated as "not an epic". A failed read says nothing about what
		// the issue carries, and both readings are wrong to assume -- the same
		// reading EpicSweep gives a failed parent lookup.
		return sum, fmt.Errorf("epic ready: read #%d: %w", number, err)
	}
	if !iss.HasLabel(epic.Label) {
		// Info, not Warn: applying epic.ReadyLabel to an ordinary issue is a
		// typo, not a misconfiguration, and it is the operator's to see rather
		// than the machine's to complain about.
		slog.Info("epic ready ignored: the issue is not an epic",
			"loop", cfg.Name, "issue", number, "label", epic.Label)
		return sum, nil
	}
	if !iss.HasLabel(epic.ReadyLabel) {
		slog.Info("epic ready ignored: the label is already gone",
			"loop", cfg.Name, "issue", number, "label", epic.ReadyLabel)
		return sum, nil
	}
	// An epic in ANOTHER repository is not this loop's, for the reason
	// EpicSweep gives about a foreign parent: the sweep below writes labels by
	// NUMBER against this loop's owner/repo, so a foreign epic would expand
	// whichever LOCAL issue shares its number.
	if !iss.InRepo(owner, repo) {
		slog.Info("epic ready skipped: the epic lives in another repository",
			"loop", cfg.Name, "issue", number, "issue_repo", iss.Repo)
		return sum, nil
	}

	// Taken here, and held across sweepEpic's reads, exactly as EpicSweep takes
	// it. See that function for why a GitHub-only write needs the loop lock at
	// all: the label this promotes with is the trigger label, and the next tick
	// to see it dispatches an agent.
	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		return sum, fmt.Errorf("epic ready: lock loop %s: %w", cfg.Name, err)
	}
	defer l.Release()

	// One epic, so the pass budget is the epic's budget. The caps still apply:
	// an epic's child count is something anyone with triage can grow.
	budget := maxPromotePerSweep
	readBudget := maxBlockerReadsPerSweep
	sum, err = sweepEpic(ctx, cfg, deps, number, &budget, &readBudget)
	if err != nil {
		// Returned WITHOUT consuming the label. The button stays pressed, so
		// the operator can see that the request was not answered -- and any
		// later delivery for this epic finds it still set.
		return sum, err
	}

	if err := deps.Epic.EditLabels(ctx, owner, repo, number,
		nil, []string{epic.ReadyLabel}); err != nil {
		// Logged, not returned. The promotions above are already written to
		// GitHub, and reporting the pass as failed would invite a retry that
		// promotes nothing (every child now carries the trigger label) and
		// fails on the same write again.
		slog.Error("cannot remove the epic-ready label; it must be cleared by hand",
			"loop", cfg.Name, "issue", number, "label", epic.ReadyLabel,
			"promoted", sum.Promoted, "err", err)
		return sum, nil
	}
	slog.Info("swept an epic on request", "loop", cfg.Name, "issue", number,
		"promoted", sum.Promoted, "label", epic.ReadyLabel)
	return sum, nil
}
