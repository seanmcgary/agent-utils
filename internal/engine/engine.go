// Package engine decides what a tick must do. Decide is pure.
package engine

import (
	"fmt"
	"sort"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// Decide returns the actions for one tick. It is pure: it reads only its
// arguments, it performs no input or output, and it reads no clock. The caller
// supplies now.
func Decide(cfg *config.Config, snap Snapshot, st State, now time.Time) Plan {
	if !st.CooldownUntil.IsZero() && now.Before(st.CooldownUntil) {
		return Plan{CooldownUntil: st.CooldownUntil}
	}

	liveIssues := make(map[int]bool, len(st.Running))
	liveTendPRs := make(map[int]bool, len(st.Running))
	for _, d := range st.Running {
		if d.Kind == store.KindTend {
			liveTendPRs[d.PRNumber] = true
			continue
		}
		liveIssues[d.Number] = true
	}

	issues := append([]ghub.Issue(nil), snap.Issues...)
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })

	var decisions []Decision
	var parks []Decision
	decided := make(map[int]bool)
	eligibleRetries := 0

	for _, iss := range issues {
		if iss.HasAnyLabel(cfg.Labels.Veto) {
			continue
		}
		if liveIssues[iss.Number] {
			// A live dispatch is the guard against double dispatch. Labels are
			// not, because the agent owns them and may not have flipped yet.
			//
			// Mark the issue decided so tendDecisions skips it too. An agent
			// working the branch and a tend agent force-pushing it are the same
			// hazard as two dispatches, and the agent flips its own labels
			// asynchronously, so the review label can still be present here.
			decided[iss.Number] = true
			continue
		}

		state := st.Issues[iss.Number]

		// FAILURE PATH. NeedsRetry is durable state written when a dispatch died
		// or exited non-zero. It covers both a dead runner and a clean non-zero
		// exit, so a failing dispatch can never redispatch without a cap.
		if state.NeedsRetry {
			// The reference loops define an orphan as "carries the in-flight
			// label AND has no live agent". Honour that: an agent that finished
			// its work and moved the label on must not be woken by a retry.
			if !iss.HasLabel(cfg.Labels.InFlight) {
				// No in-flight run to retry: either the agent moved the label on
				// before the failure was recorded, or the failure happened before
				// any agent took ownership.
				//
				// The flag MUST be cleared here. Nothing else clears it, so
				// leaving it set strands the issue permanently: every later tick
				// takes this branch, the trigger check below is never reached,
				// and re-applying the trigger label does nothing.
				decided[iss.Number] = true
				decisions = append(decisions, Decision{
					Kind:   KindClearRetry,
					Issue:  iss.Number,
					Reason: "failure recorded while the issue was not in flight",
				})
				continue
			}
			d, eligible := retryDecision(cfg, iss.Number, state, now)
			if eligible {
				eligibleRetries++
			}
			if d != nil {
				decided[iss.Number] = true
				if d.Kind == KindParkRetryExhausted {
					parks = append(parks, *d)
				} else {
					decisions = append(decisions, *d)
				}
			}
			continue
		}

		// A parked issue needs no separate guard here. parkRetryExhausted removes
		// the trigger label, so the check below already skips it, and a human who
		// re-applies that label deliberately un-parks the issue.
		if !iss.HasLabel(cfg.Labels.Trigger) {
			continue
		}

		decided[iss.Number] = true
		if state.SessionID != "" && state.SessionStarted {
			decisions = append(decisions, Decision{
				Kind:      KindResume,
				Issue:     iss.Number,
				Title:     iss.Title,
				SessionID: state.SessionID,
				Reason:    "trigger label present and a started session exists",
			})
			continue
		}
		decisions = append(decisions, Decision{
			Kind:      KindStart,
			Issue:     iss.Number,
			Title:     iss.Title,
			SessionID: state.SessionID,
			Reason:    "trigger label present and no started session exists",
		})
	}

	// The breaker treats several failures in one tick as a platform problem
	// rather than several unrelated crashes. It drops every DISPATCH decision.
	// Parks survive: the reference loop states that a cap-reached comment already
	// due is still posted during a breaker tick.
	//
	// KNOWN GAP: eligibleRetries counts retries within THIS call, so a call
	// scoped to one issue can never reach a threshold above 1. Every webhook
	// delivery is such a call (loopcmd.TickIssue), which means the breaker no
	// longer sees the platform-wide failure it was written to catch -- only the
	// cron sweep still can. The chosen fix is to count failures over a rolling
	// time window instead of within one call; that needs new database state and
	// is a separate change. Until then loopcmd.warnBreakerNotEvaluated logs
	// every scoped retry that was dispatched without this check, so the gap is
	// visible in the operator's log rather than silent.
	if eligibleRetries >= cfg.Retry.Breaker.OrphanThreshold {
		return Plan{
			Decisions:      parks,
			BreakerTripped: true,
			CooldownUntil:  now.Add(cfg.Retry.Breaker.Cooldown.Std()),
		}
	}

	decisions = append(decisions, parks...)

	if cfg.TendPR {
		decisions = append(decisions, tendDecisions(cfg, issues, snap, liveTendPRs, decided)...)
	}

	return Plan{Decisions: decisions}
}

// retryDecision returns the action for one failed issue. The second result
// reports whether the failure cleared its backoff window, which is what the
// circuit breaker counts.
//
// The window is a wall-clock deadline stored on the issue, not a count of
// ticks. A tick used to be a fixed cron interval, but the webhook daemon can
// tick a loop at any moment, so a tick count no longer names a stable wait.
// MarkNeedsRetry stamps the deadline where the failure is recorded.
func retryDecision(cfg *config.Config, number int, state store.IssueState, now time.Time) (*Decision, bool) {
	if state.RetryCount >= cfg.Retry.Max {
		return &Decision{
			Kind:   KindParkRetryExhausted,
			Issue:  number,
			Reason: fmt.Sprintf("retry cap reached (%d/%d)", state.RetryCount, cfg.Retry.Max),
		}, false
	}

	if !state.RetryAfter.IsZero() && now.Before(state.RetryAfter) {
		// Still inside the backoff window. Take no action and post no comment.
		// NeedsRetry stays set in the store, so the next tick sees it again.
		return nil, false
	}

	// Resume only when claude actually created the session. Otherwise "-r" would
	// target a session that never existed and fail identically every retry.
	//
	// A retry that must START carries NO session identifier. claude refuses to
	// reuse one ("Session ID <uuid> is already in use"), so passing the old id
	// would make every retry fail in under a second and then park the issue with
	// a comment blaming the platform.
	if state.SessionStarted && state.SessionID != "" {
		return &Decision{
			Kind:      KindRetryResume,
			Issue:     number,
			SessionID: state.SessionID,
			Reason:    fmt.Sprintf("retry %d/%d, resuming the existing session", state.RetryCount+1, cfg.Retry.Max),
		}, true
	}
	return &Decision{
		Kind:      KindRetryStart,
		Issue:     number,
		SessionID: "",
		Reason:    fmt.Sprintf("retry %d/%d with a new session; the previous attempt never started one", state.RetryCount+1, cfg.Retry.Max),
	}, true
}

// tendDecisions selects stale pull requests for issues awaiting review.
func tendDecisions(
	cfg *config.Config,
	issues []ghub.Issue,
	snap Snapshot,
	liveTendPRs map[int]bool,
	decided map[int]bool,
) []Decision {
	var out []Decision
	for _, iss := range issues {
		if iss.HasAnyLabel(cfg.Labels.Veto) {
			continue
		}
		if decided[iss.Number] {
			// The issue already has a decision this tick. Two agents in one
			// branch is worse than a late rebase.
			continue
		}
		if !iss.HasLabel(cfg.Labels.Review) {
			continue
		}
		pr, ok := LinkPR(iss.Number, snap.PRs)
		if !ok {
			continue
		}
		if liveTendPRs[pr.Number] {
			continue
		}
		if snap.BehindBy[pr.Number] <= 0 {
			// A current pull request produces nothing. Silence is correct.
			continue
		}
		out = append(out, Decision{
			Kind:    KindTend,
			Issue:   iss.Number,
			PR:      pr.Number,
			HeadRef: pr.HeadRef,
			BaseRef: pr.BaseRef,
			Reason: fmt.Sprintf("%s is %d commits behind",
				describeLink(iss.Number, pr), snap.BehindBy[pr.Number]),
		})
	}
	return out
}
