package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/epic"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/lock"
)

// maxPromotePerSweep is how many issues one sweep may promote.
//
// It is higher than maxTendPerSweep, which is 10, because the two cap different
// things. A tend decision is an agent process in a git worktree with permission
// prompts disabled; a promotion is one label write. The cost of this batch is
// 25 API calls, and the cost of tending's is 10 agents.
//
// It bounds the whole PASS, not one epic. EpicSweepAll walks every open issue
// carrying the epic label, and anyone with triage can apply that label: a cap
// applied per epic would let N epics authorise 25 x N writes on one tick, which
// is the unbounded repository-wide fan-out this design exists to avoid. The
// budget is therefore threaded through, not re-read.
//
// It is a constant rather than a configuration field for the reason
// maxTendPerSweep is: no operator has needed a different value. What is left
// over is logged and NAMED, never dropped silently, and the next sweep takes
// the next batch.
const maxPromotePerSweep = 25

// maxBlockerReadsPerSweep bounds the READS one pass may make, as
// maxPromotePerSweep bounds its writes.
//
// The write budget cannot do this job: it decrements only on a successful
// promotion, so a pass over epics whose children are all blocked or all
// already promoted -- the steady state -- spends no write budget and would
// read without limit. The epic label costs one triage permission, and
// ListOpenIssues paginates to completion, so the read fan-out is otherwise
// proportional to what any triager cares to label.
//
// 300 at the README's 15-minute cron is ~1200 reads/hour against a
// 5000/hour token shared with every other loop on the machine.
const maxBlockerReadsPerSweep = 300

// EpicSweep promotes the sub-issues that the closure of one issue unblocked.
//
// closed is the issue a delivery reported closed. The sweep starts there, walks
// up to its parent, and considers that parent's children. It dispatches NO
// agent and spends no tokens: its whole output is label writes.
//
// # Why this may act on many issues when TickIssue may not
//
// Worker.RunIssue records that a full reconcile per delivery was removed
// because it burned a token budget on every open issue of every project
// watching the repository. This pass acts on many issues again, so it must not
// become that. Four things keep it apart, and the first is the one that
// matters:
//
//  1. It dispatches no agent. The removed pass was expensive because it started
//     agents. This one writes labels.
//  2. It runs for ONE event -- an issue closing -- and only when that issue's
//     parent carries the epic label. Opening an issue, moving a label and
//     commenting arm nothing.
//  3. Its fan-out is the epic's own children, not the repository.
//  4. It is capped at maxPromotePerSweep.
//
// # Where the lock is taken
//
// This function TAKES the loop lock, before calling sweepEpic. Only the
// Parent read above happens outside it -- everything sweepEpic does,
// including SubIssues and one BlockedBy call per candidate child, runs WHILE
// the lock is held. The hold is therefore O(children) network round-trips,
// not O(1), and Worker.issuePass drops a delivery that finds the lock held,
// with no retry -- so every one of those round-trips is a window in which a
// labelled issue can be dropped and never picked up.
//
// That cost is accepted rather than hoisting the reads out, because sweepEpic
// is SHARED with the cron path: EpicSweepAll's caller (RunTick) already holds
// this lock across the whole of Tick, reads included. Splitting sweepEpic so
// the webhook path reads before locking and the cron path reads after would
// buy the webhook path a shorter hold at the cost of two code paths that read
// GitHub state in different places relative to the lock -- which is exactly
// the kind of divergence the "both share sweepEpic" design exists to rule
// out. One hold shape, paid by both callers, is the simpler and safer trade.
//
// EpicSweepAll does NOT take it, because its caller already holds it. See that
// function.
//
// # Why a GitHub-only write needs the LOOP lock at all
//
// Nothing local is written here, so the lock is not protecting a file or a
// database row. It is protecting an ORDERING. The label this pass adds is the
// trigger label, and the next tick to see it dispatches an agent. A tick
// running concurrently with this pass would read the issue list either side of
// the write and, in the "before" case, decide nothing for an issue that is
// about to become dispatchable -- harmless -- or, worse, race a park that is
// removing the same label. Taking the lock makes the promotion and the
// dispatch decision serial, which is the property the rest of this package
// already relies on.
func EpicSweep(ctx context.Context, cfg *config.Config, deps Deps, closed int) (Summary, error) {
	var sum Summary
	if deps.Epic == nil {
		return sum, errors.New("epic sweep: no EpicReader; Deps.Epic is nil")
	}
	if !isEntryLoop(cfg, deps) {
		return sum, nil
	}

	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	// One call, and for almost every delivery it is the only one. Most issues
	// in most repositories have no parent, so this is the fast exit.
	parent, err := deps.Epic.Parent(ctx, owner, repo, closed)
	if err != nil {
		if errors.Is(err, ghub.ErrNoParent) {
			return sum, nil
		}
		// NOT treated as "no parent". A failure says nothing about whether the
		// issue belongs to an epic, and both readings are wrong to assume.
		return sum, fmt.Errorf("epic sweep: read parent of #%d: %w", closed, err)
	}
	if !parent.HasLabel(epic.Label) {
		return sum, nil
	}
	// A parent in ANOTHER repository is not this loop's epic. Its children are
	// read from, and written to, this loop's owner/repo, so a foreign parent
	// would expand whichever LOCAL issue shares its number. GitHub permits a
	// cross-repository parent, so this is reachable, not theoretical.
	if !parent.InRepo(owner, repo) {
		slog.Info("epic sweep skipped: the parent lives in another repository",
			"loop", cfg.Name, "issue", closed, "parent", parent.Number, "parent_repo", parent.Repo)
		return sum, nil
	}

	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		return sum, fmt.Errorf("epic sweep: lock loop %s: %w", cfg.Name, err)
	}
	defer l.Release()

	budget := maxPromotePerSweep
	readBudget := maxBlockerReadsPerSweep
	return sweepEpic(ctx, cfg, deps, parent.Number, &budget, &readBudget)
}

// EpicSweepAll runs the sweep for every open epic of the repository.
//
// It is the CRON path. A webhook delivery can be missed -- the daemon can be
// down, the proxy can be down, GitHub can drop one -- and a missed close is a
// sub-issue that waits forever with no sign that anything is wrong. This pass
// finds it on the next tick. The daemon is the fast path; this is the backstop.
//
// It enters at the epic instead of at a closed child. Every step after the
// entry is shared with EpicSweep, so the two cannot decide differently.
//
// # It does NOT take the loop lock
//
// Its only caller is Tick, and Tick's production caller, RunTick in open.go,
// already holds that lock. flock is per open-file-description, so acquiring
// it again in the same process returns ErrHeld -- which would make this
// backstop silently promote nothing, forever, which is precisely the failure
// it exists to prevent. This is the same reason Tick itself takes no lock and
// TendSweep does: TendSweep's caller does not hold one.
func EpicSweepAll(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error) {
	var sum Summary
	if deps.Epic == nil {
		return sum, errors.New("epic sweep: no EpicReader; Deps.Epic is nil")
	}
	if !isEntryLoop(cfg, deps) {
		return sum, nil
	}

	owner, repo := cfg.RepoOwner(), cfg.RepoName()
	issues, err := deps.GH.ListOpenIssues(ctx, owner, repo)
	if err != nil {
		return sum, fmt.Errorf("epic sweep: list open issues: %w", err)
	}

	// One budget for the whole pass. See maxPromotePerSweep: a per-epic cap
	// would let the number of epics multiply the write authority, and applying
	// the epic label costs an attacker one triage permission.
	budget := maxPromotePerSweep
	// See maxBlockerReadsPerSweep: the write budget above is not decremented
	// in the steady state (nothing promotable), so it cannot bound the reads a
	// pass makes. This one can.
	readBudget := maxBlockerReadsPerSweep
	var swept int
	for _, iss := range issues {
		if !iss.HasLabel(epic.Label) {
			continue
		}
		// ListOpenIssues returns this repository's issues, so InRepo is
		// redundant here today. It is checked anyway, because the write below
		// is by number and the guard must not depend on which listing fed it.
		//
		// It does couple this path to ConvertIssues carrying Repo for the LIST
		// endpoint, not only the three epic ones. GitHub populates
		// repository_url on GET /repos/{o}/{r}/issues, so this holds -- and
		// TestListOpenIssuesCarriesTheRepository in internal/ghub/epic_test.go
		// pins it, because if it ever stopped holding, this backstop would
		// promote nothing and say nothing, which is the failure this whole
		// design is built to avoid.
		if !iss.InRepo(owner, repo) {
			continue
		}
		if budget <= 0 {
			slog.Warn("epic sweep hit its per-pass cap; the remaining epics wait for the next tick",
				"loop", cfg.Name, "promoted", sum.Promoted, "epics_swept", swept)
			break
		}
		if readBudget <= 0 {
			slog.Warn("epic sweep hit its blocker-read cap; the remaining epics wait for the next tick",
				"loop", cfg.Name, "promoted", sum.Promoted, "epics_swept", swept)
			break
		}
		swept++
		one, err := sweepEpic(ctx, cfg, deps, iss.Number, &budget, &readBudget)
		sum.Promoted += one.Promoted
		if err != nil {
			// One unreadable epic must not abandon the rest. If this returned,
			// anyone able to label an issue `epic` could stall every promotion
			// the loop would otherwise make.
			//
			// ErrHeld is not a failure, and must not be logged as one: the spec
			// states it is a skip. It cannot arise on this path today -- the
			// caller holds the lock and this function takes none -- but a bare
			// Warn here would be wrong the moment that changes.
			if errors.Is(err, lock.ErrHeld) {
				slog.Info("epic sweep skipped: another tick holds the loop lock",
					"loop", cfg.Name, "epic", iss.Number)
				continue
			}
			slog.Warn("epic sweep failed for one epic; continuing",
				"loop", cfg.Name, "epic", iss.Number, "err", err)
			continue
		}
	}
	return sum, nil
}

// sweepEpic considers the children of one epic and promotes what it may.
// Both drivers call it, so neither can decide differently from the other.
//
// The CALLER holds the loop lock. sweepEpic takes none: EpicSweep acquires it
// and Tick's caller already holds it, so acquiring here would deadlock the cron
// path against its own caller.
//
// budget is the number of promotions the whole PASS may still make. It is a
// pointer because EpicSweepAll spends one budget across many epics.
//
// readBudget is the number of blocker reads (BlockedBy calls) the whole PASS
// may still make, for the same reason and by the same mechanism -- see
// maxBlockerReadsPerSweep. A child whose blockers could not be read because
// the budget was already spent is held exactly as one that failed to read for
// any other reason: BlockersUnknown, never promoted.
func sweepEpic(
	ctx context.Context, cfg *config.Config, deps Deps, parent int, budget, readBudget *int,
) (Summary, error) {
	var sum Summary
	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	kids, err := deps.Epic.SubIssues(ctx, owner, repo, parent)
	if err != nil {
		return sum, fmt.Errorf("epic sweep: read sub-issues of #%d: %w", parent, err)
	}

	rule := epic.Rule{Veto: cfg.Labels.Veto, Owner: owner, Repo: repo, Trigger: cfg.Labels.Trigger}

	children := make([]epic.Child, 0, len(kids))
	for _, kid := range kids {
		// A child in ANOTHER repository is skipped before anything else. The
		// promotion below writes a label by NUMBER against this loop's
		// owner/repo, so a foreign child's number would label whichever LOCAL
		// issue happens to carry it -- an unrelated issue, moved into the
		// pipeline by a relation someone created in a repository this operator
		// may not control. GitHub permits a cross-repository sub-issue, so this
		// is reachable. An issue whose repository the response did not name is
		// also skipped: InRepo answers false for "unknown", which is the safe
		// direction when the alternative is labelling the wrong issue.
		if !kid.InRepo(owner, repo) {
			slog.Info("skipping a sub-issue outside this repository",
				"loop", cfg.Name, "epic", parent, "issue", kid.Number, "issue_repo", kid.Repo)
			continue
		}
		// A number GitHub could not have. The handler validates the identically
		// sourced value on the way in; this is the same check on the way out,
		// because this one is a WRITE target.
		if kid.Number <= 0 {
			slog.Warn("skipping a sub-issue with an impossible number",
				"loop", cfg.Name, "epic", parent, "number", kid.Number)
			continue
		}
		// The filter is an OPTIMIZATION, not the rule: it saves a call for a
		// child that cannot be promoted whatever its blockers say. Promote
		// tests the same conditions again and is the only place the decision is
		// made. See epic.NeedsBlockers.
		if !epic.NeedsBlockers(kid, rule) {
			continue
		}
		c := epic.Child{Issue: kid}
		if *readBudget <= 0 {
			// The read budget for the whole PASS is spent. Fail closed: a child
			// whose blockers were never read is UNKNOWN, not "no blockers", so
			// it is held for a later sweep rather than promoted on a guess. See
			// maxBlockerReadsPerSweep.
			slog.Warn("blocker-read cap reached; holding this sub-issue unread",
				"loop", cfg.Name, "epic", parent, "issue", kid.Number)
			c.BlockersUnknown = true
			children = append(children, c)
			continue
		}
		*readBudget--
		c.Blockers, err = deps.Epic.BlockedBy(ctx, owner, repo, kid.Number)
		if err != nil {
			// Held, not dropped and not promoted. An unreadable blocker list
			// says nothing, and the alternative reading promotes an issue whose
			// blockers may still be open. One unusable child must not abandon
			// the sweep, for the reason Tick gives about one unusable pull
			// request: anyone able to open an issue could otherwise stall the
			// loop.
			slog.Warn("cannot read blockers; holding this sub-issue",
				"loop", cfg.Name, "epic", parent, "issue", kid.Number, "err", err)
			c.BlockersUnknown = true
		}
		children = append(children, c)
	}

	promote := epic.Promote(children, rule)
	if len(promote) == 0 {
		return sum, nil
	}

	var deferred []int
	if len(promote) > *budget {
		deferred = promote[*budget:]
		promote = promote[:*budget]
	}

	for _, n := range promote {
		if err := deps.Epic.EditLabels(ctx, owner, repo, n,
			[]string{cfg.Labels.Trigger}, nil); err != nil {
			// One failed write must not abandon the rest. The next close
			// delivery, or the next cron tick, promotes this one: nothing about
			// the issue changed, so the rule still selects it.
			slog.Error("cannot promote sub-issue",
				"loop", cfg.Name, "epic", parent, "issue", n, "err", err)
			continue
		}
		sum.Promoted++
		*budget--
		// One line per promotion, naming the label. This is a GitHub write made
		// with no human and no agent in the loop, and the log is the only
		// record of it that lives on this machine.
		slog.Info("promoted an unblocked sub-issue",
			"loop", cfg.Name, "epic", parent, "issue", n, "label", cfg.Labels.Trigger)
	}

	if len(deferred) > 0 {
		// Never silent. A capped sweep that said nothing would read as "every
		// unblocked sub-issue was promoted", which is the opposite of the
		// truth. NAMED, not counted, so an operator can see which work waits --
		// but TRUNCATED, because the length is the child count of an epic
		// anyone with triage can grow. handler.go's safeLabels sets the shape:
		// at most a few, and a count of what did not fit.
		slog.Warn("epic sweep hit its cap; the rest wait for the next sweep",
			"loop", cfg.Name, "epic", parent, "promoted", sum.Promoted,
			"deferred", loggedNumbers(deferred), "deferred_total", len(deferred))
	}
	return sum, nil
}

// maxLoggedNumbers bounds an issue-number list carried into a log line.
const maxLoggedNumbers = 10

// loggedNumbers returns at most maxLoggedNumbers of ns.
//
// The caller logs len(ns) beside it, so nothing is hidden by the truncation --
// only moved from the line to a count, which is what handler.go's safeLabels
// does for a label list of attacker-controlled length.
func loggedNumbers(ns []int) []int {
	if len(ns) <= maxLoggedNumbers {
		return ns
	}
	return ns[:maxLoggedNumbers]
}

// isEntryLoop reports whether cfg is the one loop allowed to promote.
//
// A derivation that cannot name exactly one entry loop returns false, and the
// reason is logged. It is logged at WARN and not returned as an error because
// it is a permanent misconfiguration rather than a failed pass: returning an
// error would schedule retries of something no retry can fix, and every retry
// would log the same line again.
func isEntryLoop(cfg *config.Config, deps Deps) bool {
	dir := config.DirFromPath(deps.ConfigPath)
	if dir == "" {
		slog.Warn("epic sweep skipped: cannot locate the project directory",
			"loop", cfg.Name, "path", deps.ConfigPath)
		return false
	}
	name, err := config.EntryLoop(dir, cfg.Repo)
	if err != nil {
		slog.Warn("epic sweep skipped: cannot name the pipeline's entry loop",
			"loop", cfg.Name, "repo", cfg.Repo, "err", err)
		return false
	}
	return name == cfg.Name
}
