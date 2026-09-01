package loopcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// maxTendPerSweep is how many pull requests one sweep may act on.
//
// act applies no ceiling of its own: it calls gitRebase and then, when that
// did not settle the decision, dispatch, once per decision. A repository with
// forty stale review pull requests would answer one trigger with forty of
// them.
//
// Most of a sweep is now agent-free, so the cost this bounds is no longer only
// the detached agent process with permission prompts disabled. It is what a
// single trigger may do to the REMOTE: forty force-pushes, each preceded by a
// fetch and a rebase, on one merge or one push or one tick of the periodic
// check. The agent dispatches are still the expensive tail of that, and they
// are still counted against the same ceiling.
//
// The cap is a constant rather than a configuration field because no operator
// has needed a different value yet; promote it if one does. What is left over
// is logged, never dropped silently, and the next sweep takes the next batch.
const maxTendPerSweep = 10

// TendSweep rebases the stale pull requests of one project, and does nothing
// else.
//
// base is the branch that moved. It exists so the pass can enforce the thing
// that makes it safe rather than assert it: a merge into master says nothing
// about a pull request targeting release/1.0, and rebasing that branch would be
// a tend agent dispatched for an unrelated event -- the shape of the incident
// Worker.Deliver records.
//
// # Why this is not the reconcile that was removed
//
// Worker.RunIssue records that a full reconcile per delivery was removed: it
// burned a token budget on every open issue of every project watching the
// repository, and one unlabelled test issue dispatched a tend agent for an
// unrelated issue whose pull request was 16 commits behind. This pass acts on
// many issues again, so it must not become that. Four things keep it apart:
//
//  1. Three things arm it, and all three name the same subject: the project's
//     default branch moving. A pull request merged into it, a push to it, and
//     the periodic tend check finding a pull request behind it. Opening an
//     issue, moving a label and commenting arm no sweep, and neither does a
//     merge or a push to any other branch. Adding a FOURTH trigger is fine
//     only while it keeps that property -- the reconcile that was removed
//     failed exactly here, by running for events that said nothing about any
//     branch.
//  2. It keeps TEND decisions only. Every other kind is dropped below, before
//     anything is dispatched.
//  3. It only considers pull requests targeting the branch that actually moved.
//  4. It dispatches at most maxTendPerSweep of them.
//
// This is also why review activity is deliberately NOT read here, unlike
// tickIssue and Tick. Property 1 states the sweep's subject: everything that
// arms it names the loop's default branch moving. Review activity is not
// that subject -- a merge to master must not dispatch agents at pull requests
// that are current and merely carry comments -- so adding the read here would
// break property 1 the same way a fifth trigger with no branch of its own
// would. Review activity does not need this sweep anyway:
// pull_request_review and pull_request_review_comment are both in
// ghub.HookEvents, so a review already produces its own delivery that reaches
// tickIssue directly, and Tick's cron sweep is that fast path's safety net.
// Keeping the read out of here also keeps this sweep's GitHub cost where it
// is -- one BehindBy per review issue, not two.
//
// Decisions come from engine.DecideTend, the same function the full tend tick
// calls. A scoped copy of the live-dispatch, stopped and link rules would be a
// second implementation free to drift; see tickIssue, which states the same
// rule for the loops.
//
// # Where the lock is taken
//
// The GitHub reads happen BEFORE the lock and the dispatch happens under it.
// TickIssue holds the loop lock for one issue fetch; a sweep that held it for a
// paginated issue listing, a paginated pull request listing and a comparison
// per review issue would hold it for tens of seconds. That matters because of
// what the holder does to everyone else: Worker.issuePass drops a delivery that
// finds the lock held, with no retry, on the reasoning that "the tick already
// holding the lock reads the same GitHub state a moment later than this one
// would have". That reasoning is true of a TickIssue holder and FALSE of this
// one, which decides no issue but the ones it tends. Every second this pass
// holds the lock is a second in which a labelled issue can be dropped and never
// picked up. So it holds the lock only for the fetch and the dispatch.
//
// BehindBy is CompareCommits, a GitHub API call, so the comparisons do not
// depend on Fetch having run and lose nothing by preceding it.
func TendSweep(ctx context.Context, cfg *config.Config, deps Deps, base string) (Summary, error) {
	snap, links, err := tendSnapshot(ctx, cfg, deps, base)
	if err != nil {
		return Summary{}, err
	}

	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		return Summary{}, err
	}
	defer l.Release()

	return tendDispatch(ctx, cfg, deps, snap, links, base)
}

// tendSnapshot reads GitHub and returns what engine.Decide needs, plus the
// pull request links derived from it. It takes no lock, touches no git, and
// writes nothing: the links are RETURNED rather than stored, because every
// other pr_links write in this package happens under the loop lock and this
// function deliberately runs outside it. Storing here would leave a
// tens-of-seconds window in which a concurrent TickIssue, holding the lock,
// upserts the same row -- and PutPRLink is last-writer-wins on behind_by and
// head_ref, so the loser's values would be ones read at a different moment.
func tendSnapshot(ctx context.Context, cfg *config.Config, deps Deps, base string) (engine.Snapshot, []store.PRLink, error) {
	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	issues, err := deps.GH.ListOpenIssues(ctx, owner, repo)
	if err != nil {
		return engine.Snapshot{}, nil, err
	}
	prs, err := deps.GH.ListOpenPullRequests(ctx, owner, repo)
	if err != nil {
		return engine.Snapshot{}, nil, err
	}

	var links []store.PRLink
	snap := engine.Snapshot{Issues: issues, PRs: prs, BehindBy: map[int]int{}}
	for _, iss := range issues {
		if !iss.HasLabel(cfg.Tend.Label) {
			continue
		}
		pr, ok := engine.LinkPR(iss.Number, prs)
		if !ok {
			continue
		}
		// The branch that moved is the only reason this pass exists. A pull
		// request targeting anything else is behind for reasons the branch
		// that moved knows nothing about. Skipping here also saves the
		// comparison.
		if pr.BaseRef != base {
			continue
		}
		behind, err := deps.GH.BehindBy(ctx, owner, repo, pr.BaseRef, pr.HeadRef)
		if err != nil {
			// One unusable pull request must not abandon the sweep. If this
			// returned early, anyone able to open a pull request could stop
			// every rebase this loop would otherwise do.
			slog.Warn("compare failed; skipping this pull request",
				"loop", cfg.Name, "issue", iss.Number, "pr", pr.Number, "err", err)
			continue
		}
		snap.BehindBy[pr.Number] = behind
		links = append(links, store.PRLink{
			Loop: cfg.Name, Repo: cfg.Repo, Number: iss.Number,
			PRNumber: pr.Number, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef,
			// The real count, unlike Tick and tickIssue, which both upsert this
			// row with BehindBy unset. RunAgent renders it into the tend
			// prompt, so a zero tells the agent the opposite of why it was
			// dispatched. Best-effort only: the next full tick zeroes it again,
			// and fixing that means teaching the other two writers to set it.
			BehindBy: behind,
		})
	}
	return snap, links, nil
}

// tendDispatch decides and acts. The caller holds the loop lock.
func tendDispatch(
	ctx context.Context, cfg *config.Config, deps Deps,
	snap engine.Snapshot, links []store.PRLink, base string,
) (Summary, error) {
	var sum Summary
	now := deps.Now()

	// Under the lock, and after the reads: the fetch prepares the checkout the
	// tend agent rebases in, so a failure here means there is nothing safe to
	// dispatch INTO. Unlike Tick, which suppresses tending and still reaps and
	// retries, this pass has only tending to do, so it stops.
	if deps.Fetch != nil {
		if err := deps.Fetch(ctx); err != nil {
			return sum, fmt.Errorf("fetch primary checkout: %w", err)
		}
	}

	// Under the lock, so this pass cannot interleave its pr_links writes with a
	// TickIssue holding it. One failed row must not abandon the sweep: the link
	// is what the tend prompt renders, not what the decision reads.
	for _, l := range links {
		if err := deps.Store.PutPRLink(l); err != nil {
			slog.Error("store pr link", "loop", cfg.Name, "issue", l.Number, "err", err)
		}
	}

	// No lastTend map, so no pull request is ever review-pending here. That is
	// property 1 above, expressed in code rather than asserted: this sweep's
	// subject is the default branch moving, and review activity is not that
	// subject.
	st, err := tendState(cfg, deps, nil, now, &sum)
	if err != nil {
		return sum, err
	}

	plan := engine.DecideTend(cfg, snap, st)
	logTendSkips(cfg, plan)

	// Issue order, so a capped sweep takes the low-numbered batch every time
	// and the next sweep takes the next one. DecideTend already walks issues in
	// order, so this pins the batch identity rather than establishing it -- and
	// pinning it here is what keeps the cap below from depending on an ordering
	// guarantee that lives in another package.
	tends := plan.Decisions
	sort.Slice(tends, func(i, j int) bool { return tends[i].Issue < tends[j].Issue })

	var deferred []int
	if len(tends) > maxTendPerSweep {
		for _, d := range tends[maxTendPerSweep:] {
			deferred = append(deferred, d.Issue)
		}
		tends = tends[:maxTendPerSweep]
	}

	for _, d := range tends {
		if err := act(ctx, cfg, deps, d, now, &sum); err != nil {
			// One failed decision must not abandon the rest of the sweep.
			slog.Error("decision failed", "loop", cfg.Name, "kind", d.Kind,
				"issue", d.Issue, "err", err)
		}
	}
	if len(deferred) > 0 {
		// Never silent. A capped sweep that said nothing would read as "every
		// stale pull request was rebased", which is the opposite of the truth.
		// The deferred issues are NAMED, not counted: this program's log lines
		// carry "issue" and "pr" everywhere else, and a bare count leaves an
		// operator no way to tell which work is waiting. sum.Tended rather than
		// len(tends), because act failures are swallowed above and the intended
		// count would overstate what ran. The rebases are reported alongside,
		// because most of a sweep's work now costs no agent at all and a line
		// naming only the dispatches reads as "nothing happened".
		slog.Warn("tend sweep hit its per-sweep cap; the rest wait for the next sweep",
			"loop", cfg.Name, "dispatched", sum.Tended, "rebased", sum.Rebased,
			"backoff", sum.Backoff, "deferred", deferred)
	}

	// Recorded like any other tick, so the counter and the last-tick time keep
	// meaning something in `project loop status`. Never breaker-tripped: the
	// breaker counts retry decisions within one call and this pass makes none,
	// so a pass that will not act on that evidence must not report it either.
	body, _ := json.Marshal(sum)
	if _, err := deps.Store.RecordTick(cfg.Name, false, string(body)); err != nil {
		return sum, err
	}
	slog.Info("tend sweep complete", "loop", cfg.Name, "base", base, "summary", string(body))
	return sum, nil
}
