package loopcmd

import (
	"context"
	"log/slog"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// The tend dispatcher.
//
// Tending is not a loop and has no loop file. It is a project-level policy --
// keep this repository's open pull requests rebased, and answer review activity
// on them -- with a dispatcher of its own: its own name, its own agent, its own
// session per dispatch, its own worktrees, and its own rows in every table.
// config.LoadTend builds the *config.Config it runs on, from the project
// descriptor plus the repository facts its loops agree on.
//
// Three passes reach it, and they are the same three that always did. Nothing
// here is armed by anything else, and the constraint is load-bearing rather
// than incidental -- see TendSweep for what happens when it is relaxed.
//
//   - TendSweep, armed by the default branch MOVING: a merge into it, a push to
//     it, or the periodic check finding a pull request behind it.
//   - TendIssue, the delivery fast path: one issue named by a webhook, which is
//     how review activity reaches tending within seconds.
//   - TendTick, the cron sweep that catches what no delivery names, and the
//     safety net under the fast path.
//
// # Why the guards are project-wide
//
// A tend force-pushes a branch some LOOP's agent wrote. The two must never be
// in that branch at once, and the dispatcher cannot see a loop's rows through
// its own name -- its own scope holds only its own tends. So both guards read
// the whole project: store.RunningDispatchesForRepo for live agents and
// store.StoppedIssuesForRepo for issues an operator stopped. Reading only the
// reserved name's rows would see an empty project and force-push under a live
// agent every time.

// tendState reads the project-wide guards one tend pass decides from, and
// reaps the dispatcher's OWN dead rows on the way through.
//
// Only the dispatcher's rows are reaped, and that asymmetry is deliberate. A
// dead row belonging to a LOOP is that loop's to retire: retiring it here would
// finish a dispatch this pass holds no lock for, and the loop's own tick would
// then find the row already gone and never flag its issue for retry. So a
// loop's dead rows are simply treated as LIVE by this pass, which is the
// conservative direction -- the cost is a rebase declined, and the alternative
// cost is a second agent in a worktree that already holds one.
//
// lastTend is supplied by the caller because the three passes learn it
// differently: a sweep never reads it at all, the cron tick reads the whole map
// in one query, and the delivery path reads one pull request's entry.
func tendState(
	cfg *config.Config, deps Deps, lastTend map[int]time.Time, now time.Time, sum *Summary,
) (engine.TendState, error) {
	running, err := deps.Store.RunningDispatchesForRepo(cfg.Repo)
	if err != nil {
		return engine.TendState{}, err
	}

	mine := make([]store.Dispatch, 0, len(running))
	live := make([]store.Dispatch, 0, len(running))
	for _, d := range running {
		if d.Loop == cfg.Name {
			mine = append(mine, d)
			continue
		}
		live = append(live, d)
	}
	// reapDead writes no issue state for a tend row -- it guards MarkNeedsRetry
	// with `d.Kind != store.KindTend` -- so the map it is handed stays empty
	// and is never read back. It is passed rather than nil because reapDead
	// assigns into it on the path a tend row can never take.
	liveMine, err := reapDead(cfg, deps, mine, map[int]store.IssueState{}, now, sum)
	if err != nil {
		return engine.TendState{}, err
	}
	live = append(live, liveMine...)
	sum.Live = len(live)

	liveIssues, liveTendPRs := engine.TendLiveness(live)

	stopped, err := deps.Store.StoppedIssuesForRepo(cfg.Repo)
	if err != nil {
		return engine.TendState{}, err
	}

	return engine.TendState{
		LiveIssues:  liveIssues,
		LiveTendPRs: liveTendPRs,
		Stopped:     stopped,
		LastTend:    lastTend,
	}, nil
}

// tendAct performs one pass's decisions and logs the ones it skipped.
//
// A failed decision is logged and the pass continues, for the reason every
// other multi-decision pass in this package gives: one unusable pull request
// must not abandon the rest, or anyone able to open a pull request could stop
// every rebase this project would otherwise do.
func tendAct(
	ctx context.Context, cfg *config.Config, deps Deps,
	plan engine.TendPlan, now time.Time, sum *Summary,
) {
	for _, d := range plan.Decisions {
		if err := act(ctx, cfg, deps, d, now, sum); err != nil {
			slog.Error("decision failed", "loop", cfg.Name, "kind", d.Kind,
				"issue", d.Issue, "pr", d.PR, "err", err)
		}
	}
}

// tendLink is the pr_links row a tend pass records for one decision candidate.
//
// The link is what the tend PROMPT renders -- head ref, base ref and how far
// behind the branch is -- not what the decision reads, so a failed write is
// logged and the pass carries on. BehindBy is the real count: RunAgent renders
// it into the prompt to tell the agent why it was dispatched, and a zero there
// tells it the opposite.
func tendLink(cfg *config.Config, issue int, pr ghub.PullRequest, behind int) store.PRLink {
	return store.PRLink{
		Loop: cfg.Name, Repo: cfg.Repo, Number: issue,
		PRNumber: pr.Number, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef,
		BehindBy: behind,
	}
}

// reviewPR returns the pull request to consider tending for iss, fetched
// singly.
//
// The pull request is identified either by the delivery itself (a
// pull_request* event names it) or by the link a previous pass stored for this
// issue. Neither is a guess that gets acted on unchecked: engine.LinkPR is
// applied to the fetched pull request exactly as it is in a full pass, so the
// closing reference and the TRUST decision are re-judged against what GitHub
// says right now. Tending checks the head branch out and runs an agent inside
// it, so a head that has since moved to a fork must stop being tended even
// though the stored link still points at it.
//
// When neither source names one, this delivery does not tend. The cron sweep
// still lists open pull requests and finds a new one; paying for that listing
// on every delivery is what TendIssue exists to avoid.
func reviewPR(
	ctx context.Context,
	cfg *config.Config,
	deps Deps,
	owner, repo string,
	iss ghub.Issue,
	eventPR int,
) (ghub.PullRequest, bool) {
	number := eventPR
	if number == 0 {
		links, err := deps.Store.PRLinks(cfg.Name, cfg.Repo)
		if err != nil {
			slog.Error("read pr links", "loop", cfg.Name, "issue", iss.Number, "err", err)
			return ghub.PullRequest{}, false
		}
		number = links[iss.Number].PRNumber
	}
	if number <= 0 {
		return ghub.PullRequest{}, false
	}

	pr, err := deps.GH.PullRequest(ctx, owner, repo, number)
	if err != nil {
		slog.Warn("cannot read the pull request for this issue; skipping tend",
			"loop", cfg.Name, "issue", iss.Number, "pr", number, "err", err)
		return ghub.PullRequest{}, false
	}
	linked, ok := engine.LinkPR(iss.Number, []ghub.PullRequest{pr})
	if !ok {
		return ghub.PullRequest{}, false
	}
	return linked, true
}
