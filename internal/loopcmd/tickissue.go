package loopcmd

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// TickIssue decides ONE issue: the one a webhook delivery named.
//
// A delivery says "something about this issue changed, figure out what and
// dispatch the correct executor to handle it." It is not an instruction to
// reconcile the repository. The full reconcile that used to answer a delivery
// read every open issue, every open pull request and a commit comparison per
// review issue, then decided all of them -- so opening one unlabelled test
// issue dispatched a tend agent for an unrelated issue whose pull request
// happened to be 16 commits behind. That is a token budget spent on work
// nobody asked for, on every delivery, per project watching the repository.
//
// Everything else about a tick is unchanged: the same lock, the same
// engine.Decide, the same dispatch and the same state writes. Only the SCOPE
// of what is fetched and decided is narrower. Tick remains the sweep that
// catches what no event names; see its comment.
//
// The lock is taken here rather than in a RunTickIssue wrapper (as RunTick
// wraps Tick) because there is no caller that wants the scoped pass WITHOUT
// it: deliveries arrive seconds apart, a cron sweep may be running in the same
// worktree, and two agents in one worktree is the hazard the lock exists for.
// A caller matches lock.ErrHeld with errors.Is and drops the delivery, exactly
// as it does for RunTick.
func TickIssue(ctx context.Context, cfg *config.Config, deps Deps, number int) (Summary, error) {
	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		return Summary{}, err
	}
	defer l.Release()

	return tickIssue(ctx, cfg, deps, number)
}

func tickIssue(ctx context.Context, cfg *config.Config, deps Deps, number int) (Summary, error) {
	var sum Summary
	now := deps.Now()

	// Same reasoning as Tick: a failed fetch makes branch comparisons stale, so
	// it suppresses TENDING only. Reaping and retrying have nothing to do with
	// git, and abandoning the pass would leave a dead runner's issue with no
	// failure flag at all.
	fetchOK := true
	if deps.Fetch != nil {
		if err := deps.Fetch(); err != nil {
			fetchOK = false
			slog.Error("fetch primary checkout; skipping tend this delivery",
				"loop", cfg.Name, "issue", number, "err", err)
		}
	}

	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	iss, eventPR, ok, err := subject(ctx, cfg, deps, owner, repo, number)
	if err != nil {
		return sum, err
	}
	if !ok {
		// Already logged by subject. Nothing this loop keeps state for was
		// named, so there is nothing to record either.
		return sum, nil
	}

	snap := engine.Snapshot{Issues: []ghub.Issue{iss}, BehindBy: map[int]int{}}
	if cfg.TendPR && fetchOK && iss.HasLabel(cfg.Labels.Review) {
		if pr, found := reviewPR(ctx, cfg, deps, owner, repo, iss, eventPR); found {
			snap.PRs = []ghub.PullRequest{pr}
			behind, err := deps.GH.BehindBy(ctx, owner, repo, pr.BaseRef, pr.HeadRef)
			if err != nil {
				// As in Tick: one unusable pull request must not abandon the
				// pass. Anyone able to open a pull request could otherwise stop
				// this loop answering deliveries for the issue it closes.
				slog.Warn("compare failed; skipping this pull request",
					"loop", cfg.Name, "issue", iss.Number, "pr", pr.Number, "err", err)
			} else {
				snap.BehindBy[pr.Number] = behind
				if err := deps.Store.PutPRLink(store.PRLink{
					Loop: cfg.Name, Repo: cfg.Repo, Number: iss.Number,
					PRNumber: pr.Number, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef,
				}); err != nil {
					slog.Error("store pr link", "loop", cfg.Name, "issue", iss.Number, "err", err)
				}
			}
		}
	}

	// One row, not the loop's whole issue map. engine.Decide reads st.Issues
	// only for the issues in the snapshot, so a wider read would be pure cost.
	state, err := deps.Store.IssueState(cfg.Name, cfg.Repo, iss.Number)
	if err != nil {
		return sum, err
	}
	states := map[int]store.IssueState{iss.Number: state}

	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		return sum, err
	}
	// Reaping is scoped to THIS issue. Reaping the whole loop's rows on a
	// delivery would retire live agents' rows for issues nobody touched, flag
	// those issues for retry, and let the next pass start a second agent in a
	// worktree that already holds one. The rows are filtered by issue number,
	// which keeps a tend row too: a tend dispatch is keyed by the issue it
	// serves and carries the pull request number alongside.
	mine := make([]store.Dispatch, 0, len(running))
	for _, d := range running {
		if d.Number == iss.Number {
			mine = append(mine, d)
		}
	}
	live, err := reapDead(cfg, deps, mine, states, now, &sum)
	if err != nil {
		return sum, err
	}
	// Live counts this issue's agents, not the loop's. The summary is scoped
	// because the pass is; `loop status` reads the store, not this number.
	sum.Live = len(live)

	st := engine.State{Issues: states, Running: live}
	if st.CooldownUntil, err = deps.Store.CooldownUntil(cfg.Name); err != nil {
		return sum, err
	}

	// The SAME engine.Decide a full tick calls. The decision logic is the
	// tested part of this program and must stay single-sourced: a scoped copy
	// would be a second implementation of the retry, veto and double-dispatch
	// rules, drifting silently.
	plan := engine.Decide(cfg, snap, st, now)
	sum.BreakerTripped = plan.BreakerTripped

	// clearUnreachableDeadlines is deliberately NOT called here. It is a sweep
	// over every stamped row the engine can no longer reach, and this pass
	// looked at one issue, so it has no evidence about any other row. Tick
	// still runs it under cron.

	if plan.BreakerTripped {
		if err := deps.Store.SetCooldown(cfg.Name, plan.CooldownUntil); err != nil {
			return sum, err
		}
		slog.Warn("circuit breaker tripped; skipping all dispatch",
			"loop", cfg.Name, "cooldown_until", plan.CooldownUntil)
	}

	warnBreakerNotEvaluated(cfg, plan, iss.Number)

	for _, d := range plan.Decisions {
		// Belt and braces. A one-issue snapshot cannot produce a decision for
		// another issue today, but this is the boundary that bounds the blast
		// radius of a delivery, and it must not depend on an invariant that
		// lives in another package.
		if d.Issue != iss.Number {
			slog.Warn("dropping a decision for an issue this delivery did not name",
				"loop", cfg.Name, "delivered", iss.Number, "issue", d.Issue, "kind", d.Kind)
			continue
		}
		if err := act(ctx, cfg, deps, d, now, &sum); err != nil {
			// One failed decision must not abandon the rest of the pass.
			slog.Error("decision failed", "loop", cfg.Name, "kind", d.Kind,
				"issue", d.Issue, "err", err)
		}
	}

	// Recorded like any other tick, so the counter and the last-tick time keep
	// meaning something. A daemon answering deliveries all day while recording
	// no ticks would read as an idle loop in `project loop status`.
	body, _ := json.Marshal(sum)
	if _, err := deps.Store.RecordTick(cfg.Name, plan.BreakerTripped, string(body)); err != nil {
		return sum, err
	}
	slog.Info("issue tick complete", "loop", cfg.Name, "issue", iss.Number,
		"summary", string(body))
	return sum, nil
}

// subject resolves the number a delivery carried to the issue this loop keys
// its state by, and reports the pull request the delivery named when it named
// one.
//
// Pull requests share the issue number space, and three of the five subscribed
// events (plus issue_comment on a pull request) name one. Sessions, retries,
// the in-flight label and dispatch rows are all keyed by ISSUE number, so a
// delivery about a pull request has to be resolved to the issue that pull
// request closes before anything can be decided.
//
// ok is false, with no error, when the delivery names nothing this loop keeps
// state for. That is not a failure and must not be retried: there is no such
// issue to act on, now or later.
func subject(
	ctx context.Context,
	cfg *config.Config,
	deps Deps,
	owner, repo string,
	number int,
) (ghub.Issue, int, bool, error) {
	iss, err := deps.GH.Issue(ctx, owner, repo, number)
	if err == nil {
		return iss, 0, true, nil
	}
	if !errors.Is(err, ghub.ErrNotAnIssue) {
		return ghub.Issue{}, 0, false, err
	}

	pr, err := deps.GH.PullRequest(ctx, owner, repo, number)
	if err != nil {
		return ghub.Issue{}, 0, false, err
	}
	linked, found := engine.ClosesIssue(pr)
	if !found {
		// Deliberately NOT a fallback to "decide issue <number>". The two
		// share a number space but a pull request is not a tracked issue in
		// this model, and deciding the issue that happens to carry the pull
		// request's number would act on a completely unrelated thing.
		slog.Info("pull request closes no issue; nothing to decide",
			"loop", cfg.Name, "pr", pr.Number)
		return ghub.Issue{}, 0, false, nil
	}

	iss, err = deps.GH.Issue(ctx, owner, repo, linked)
	if err != nil {
		if errors.Is(err, ghub.ErrNotAnIssue) {
			// The body names another pull request ("Closes #<pr>"). Nothing
			// keyed by that number exists either, and chasing it further would
			// be an unbounded walk over attacker-writable text.
			slog.Warn("pull request names another pull request as closed; nothing to decide",
				"loop", cfg.Name, "pr", pr.Number, "names", linked)
			return ghub.Issue{}, 0, false, nil
		}
		return ghub.Issue{}, 0, false, err
	}
	slog.Info("resolved a pull request to the issue it closes",
		"loop", cfg.Name, "pr", pr.Number, "issue", iss.Number)
	return iss, pr.Number, true, nil
}

// reviewPR returns the pull request to consider tending for iss, fetched
// singly.
//
// The pull request is identified either by the delivery itself (a
// pull_request* event names it) or by the link a previous tick stored for this
// issue. Neither is a guess that gets acted on unchecked: engine.LinkPR is
// applied to the fetched pull request exactly as it is in a full tick, so the
// closing reference and the TRUST decision are re-judged against what GitHub
// says right now. Tending checks the head branch out and runs an agent inside
// it, so a head that has since moved to a fork must stop being tended even
// though the stored link still points at it.
//
// When neither source names one, this delivery does not tend. The cron sweep
// still lists open pull requests and finds a new one; paying for that listing
// on every delivery is what this change exists to stop.
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
			// Not fatal: the rest of the pass still decides this issue.
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

// warnBreakerNotEvaluated records the one behaviour a scoped pass cannot
// reproduce.
//
// engine.Decide's circuit breaker counts eligibleRetries -- issues needing a
// retry within the SAME call -- and treats several at once as a platform
// problem rather than several unrelated crashes. A call scoped to one issue
// holds at most one such retry, so the breaker cannot trip on a delivery for
// any threshold above 1. The user has chosen to keep the breaker and count it
// over a rolling time window instead; that needs new database state and is a
// separate change.
//
// Until then the gap is logged rather than left silent, and only when it could
// have mattered -- a pass that dispatched a retry. Warning on every delivery
// would bury the line it is meant to be.
func warnBreakerNotEvaluated(cfg *config.Config, plan engine.Plan, number int) {
	if cfg.Retry.Breaker.OrphanThreshold <= 1 {
		// The threshold is reachable from a single issue, so nothing is
		// missing.
		return
	}
	for _, d := range plan.Decisions {
		if d.Kind != engine.KindRetryStart && d.Kind != engine.KindRetryResume {
			continue
		}
		slog.Warn("dispatching a retry without evaluating the circuit breaker",
			"loop", cfg.Name, "issue", number,
			"threshold", cfg.Retry.Breaker.OrphanThreshold,
			"reason", "the breaker counts retries within one decision, and a delivery decides one issue")
		return
	}
}
