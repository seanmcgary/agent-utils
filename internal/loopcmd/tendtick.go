package loopcmd

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// RunTendTick takes the tend dispatcher's lock and runs one full pass.
//
// It is the counterpart of RunTick for the loops: `agent-utils project loop
// tick --name tend` under cron reaches this, and it is what makes tending
// visible to an operator as a thing they can run by hand rather than something
// that only ever happens inside a daemon.
func RunTendTick(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error) {
	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		return Summary{}, err
	}
	defer l.Release()

	return TendTick(ctx, cfg, deps)
}

// TendTick is the tend dispatcher's full sweep: every open issue carrying the
// project's tend label, judged on BOTH staleness and unanswered review
// activity.
//
// It is the pass that catches what no delivery names. A webhook can be missing,
// broken, or simply not registered on a machine with no daemon at all, and on
// such a machine this -- run from cron -- is the only thing that ever notices a
// pull request behind its base or a review comment nobody answered. It is also
// the safety net under TendIssue, which is the fast path for the same review
// activity.
//
// It is deliberately NOT what a merge arms. TendSweep is, and the difference is
// its whole subject: a merge says the default branch moved, and a pass answering
// a merge must act only on pull requests behind THAT branch. This pass answers a
// clock, so it may read review activity too -- see TendSweep's property 1 for
// why putting that read there instead would break the thing that keeps a merge
// from dispatching agents at pull requests that are perfectly current.
func TendTick(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error) {
	var sum Summary
	now := deps.Now()

	// Before the comparisons, which read remote tracking refs. A stale answer
	// here is worse than none: it reports a branch as current after the base
	// moved. Unlike a loop tick, this pass has nothing to do BUT tend, so a
	// failed fetch stops it rather than degrading it.
	if deps.Fetch != nil {
		if err := deps.Fetch(ctx); err != nil {
			return sum, err
		}
	}

	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	issues, err := deps.GH.ListOpenIssues(ctx, owner, repo)
	if err != nil {
		return sum, err
	}
	prs, err := deps.GH.ListOpenPullRequests(ctx, owner, repo)
	if err != nil {
		return sum, err
	}

	// One query for every pull request this pass might tend, not one per
	// candidate: LastTendByPR groups by pull request.
	//
	// lastTendOK is what makes a failed read fail CLOSED, and it is
	// load-bearing. An unset lastTend entry reads as the zero time, and any
	// review activity at all is After(zero) -- so leaving the map empty and
	// still reading review activity would mark EVERY tendable pull request in
	// the repository as review-pending and answer one broken store read with a
	// burst of agent dispatches. A failed read therefore suppresses the review
	// trigger for this pass entirely: pull requests are judged on staleness
	// alone, which is what this pass did before the trigger existed. It also
	// spends no GitHub call on an answer that could not be used.
	lastTend := map[int]time.Time{}
	lastTendOK := true
	if m, err := deps.Store.LastTendByPR(cfg.Name, cfg.Repo); err != nil {
		lastTendOK = false
		slog.Warn("read last tend times; judging every pull request on staleness alone",
			"loop", cfg.Name, "err", err)
	} else {
		lastTend = m
	}

	snap := engine.Snapshot{
		Issues: issues, PRs: prs,
		BehindBy: map[int]int{}, ReviewedAt: map[int]time.Time{},
	}
	var links []store.PRLink
	for _, iss := range issues {
		if !iss.HasLabel(cfg.Tend.Label) {
			continue
		}
		pr, ok := engine.LinkPR(iss.Number, prs)
		if !ok {
			continue
		}
		behind, err := deps.GH.BehindBy(ctx, owner, repo, pr.BaseRef, pr.HeadRef)
		if err != nil {
			// One unusable pull request must not abandon the whole pass. If
			// this returned early, anyone able to open a pull request could
			// stop every rebase this project would otherwise do.
			slog.Warn("compare failed; skipping this pull request",
				"loop", cfg.Name, "issue", iss.Number, "pr", pr.Number, "err", err)
			continue
		}
		snap.BehindBy[pr.Number] = behind
		links = append(links, tendLink(cfg, iss.Number, pr, behind))

		// Asked only for a candidate already accepted above -- the tend label
		// and a LinkPR-trusted pull request -- so a pass with no candidates
		// costs nothing. See tendsweep.go and tendcheck.go for why this read is
		// deliberately absent from both of those.
		if lastTendOK {
			if activity, err := deps.GH.LatestReviewActivity(ctx, owner, repo, pr.Number); err != nil {
				slog.Warn("read review activity; judging this pull request on staleness alone",
					"loop", cfg.Name, "issue", iss.Number, "pr", pr.Number, "err", err)
			} else if !activity.IsZero() {
				snap.ReviewedAt[pr.Number] = activity
			}
		}
	}

	for _, l := range links {
		if err := deps.Store.PutPRLink(l); err != nil {
			slog.Error("store pr link", "loop", cfg.Name, "issue", l.Number, "err", err)
		}
	}

	st, err := tendState(cfg, deps, lastTend, now, &sum)
	if err != nil {
		return sum, err
	}

	plan := engine.DecideTend(cfg, snap, st)
	tendAct(ctx, cfg, deps, plan, now, &sum)

	body, _ := json.Marshal(sum)
	if _, err := deps.Store.RecordTick(cfg.Name, false, string(body)); err != nil {
		return sum, err
	}
	slog.Info("tend tick complete", "loop", cfg.Name, "summary", string(body))
	return sum, nil
}

// TendIssue decides tending for ONE issue: the one a webhook delivery named.
//
// It is the tend dispatcher's twin of TickIssue, and it exists for the same
// reason: a delivery says something changed about one issue, not "reconcile the
// repository". It is also the FAST PATH for the review-activity trigger --
// pull_request_review and pull_request_review_comment are both in
// ghub.HookEvents, so a review lands here within seconds of being written,
// where TendTick's cron sweep would find it only on its next run.
//
// The lock is taken here rather than in a wrapper, exactly as TickIssue takes
// its own: there is no caller that wants the scoped pass without it, deliveries
// arrive seconds apart, and two agents in one worktree is the hazard the lock
// exists for.
func TendIssue(ctx context.Context, cfg *config.Config, deps Deps, number int) (Summary, error) {
	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		return Summary{}, err
	}
	defer l.Release()

	return tendIssue(ctx, cfg, deps, number)
}

func tendIssue(ctx context.Context, cfg *config.Config, deps Deps, number int) (Summary, error) {
	var sum Summary
	now := deps.Now()

	if deps.Fetch != nil {
		if err := deps.Fetch(ctx); err != nil {
			return sum, err
		}
	}

	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	// subject resolves a delivery that named a PULL REQUEST to the issue it
	// closes, and hands back the pull request number it came from -- which is
	// how a review comment on PR #40 reaches issue #12's tend without a
	// listing. It is shared with tickIssue rather than copied: the trust rules
	// it applies to a pull request body are the part that must not drift.
	iss, eventPR, ok, err := subject(ctx, cfg, deps, owner, repo, number)
	if err != nil {
		return sum, err
	}
	if !ok {
		return sum, nil
	}
	if !iss.HasLabel(cfg.Tend.Label) {
		// Not tendable. No pull request fetch, no comparison, no review read:
		// most deliveries land here, and this is what keeps the fast path cheap.
		return sum, nil
	}

	snap := engine.Snapshot{
		Issues:   []ghub.Issue{iss},
		BehindBy: map[int]int{}, ReviewedAt: map[int]time.Time{},
	}
	lastTend := map[int]time.Time{}
	if pr, found := reviewPR(ctx, cfg, deps, owner, repo, iss, eventPR); found {
		snap.PRs = []ghub.PullRequest{pr}
		behind, err := deps.GH.BehindBy(ctx, owner, repo, pr.BaseRef, pr.HeadRef)
		if err != nil {
			// One unusable pull request must not abandon the pass. Anyone able
			// to open a pull request could otherwise stop this dispatcher
			// answering deliveries for the issue it closes.
			slog.Warn("compare failed; skipping this pull request",
				"loop", cfg.Name, "issue", iss.Number, "pr", pr.Number, "err", err)
		} else {
			snap.BehindBy[pr.Number] = behind
			if err := deps.Store.PutPRLink(tendLink(cfg, iss.Number, pr, behind)); err != nil {
				slog.Error("store pr link", "loop", cfg.Name, "issue", iss.Number, "err", err)
			}
		}

		// The last-tend read comes FIRST, and a failure skips the review read
		// entirely. That ordering is the fail-closed guard, not a preference:
		// an unset lastTend entry reads as the zero time, and any review
		// activity at all is After(zero), so reading activity without a
		// last-tend answer would mark the pull request review-pending on the
		// strength of a store read that failed -- dispatching an agent
		// precisely because the pass could not tell whether one had already
		// answered. It also spends no GitHub call on an answer that could not
		// be used.
		last, lastErr := deps.Store.LastTendAt(cfg.Name, cfg.Repo, pr.Number)
		if lastErr != nil {
			slog.Warn("read last tend time; judging this pull request on staleness alone",
				"loop", cfg.Name, "issue", iss.Number, "pr", pr.Number, "err", lastErr)
		} else {
			if !last.IsZero() {
				lastTend[pr.Number] = last
			}
			if activity, err := deps.GH.LatestReviewActivity(ctx, owner, repo, pr.Number); err != nil {
				slog.Warn("read review activity; judging this pull request on staleness alone",
					"loop", cfg.Name, "issue", iss.Number, "pr", pr.Number, "err", err)
			} else if !activity.IsZero() {
				snap.ReviewedAt[pr.Number] = activity
			}
		}
	}

	st, err := tendState(cfg, deps, lastTend, now, &sum)
	if err != nil {
		return sum, err
	}

	plan := engine.DecideTend(cfg, snap, st)
	// The boundary that bounds a delivery's blast radius, and the counterpart
	// of TickIssue's own check. A one-issue snapshot cannot produce a decision
	// for another issue today, but this must not depend on an invariant living
	// in another package.
	mine := plan.Decisions[:0]
	for _, d := range plan.Decisions {
		if d.Issue != iss.Number {
			slog.Warn("dropping a tend decision for an issue this delivery did not name",
				"loop", cfg.Name, "delivered", iss.Number, "issue", d.Issue)
			continue
		}
		mine = append(mine, d)
	}
	plan.Decisions = mine
	tendAct(ctx, cfg, deps, plan, now, &sum)

	body, _ := json.Marshal(sum)
	if _, err := deps.Store.RecordTick(cfg.Name, false, string(body)); err != nil {
		return sum, err
	}
	if reason := plan.NoDecisionReason(iss.Number); reason != "" {
		slog.Info("tend delivery complete", "loop", cfg.Name, "issue", iss.Number,
			"summary", string(body), "reason", reason)
	} else {
		slog.Info("tend delivery complete", "loop", cfg.Name, "issue", iss.Number,
			"summary", string(body))
	}
	return sum, nil
}
