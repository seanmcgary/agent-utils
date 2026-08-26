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
		return Plan{
			CooldownUntil: st.CooldownUntil,
			Halted: fmt.Sprintf("the circuit breaker is in cooldown until %s",
				st.CooldownUntil.Format(time.RFC3339)),
		}
	}

	liveIssues := make(map[int]bool, len(st.Running))
	liveTendPRs := make(map[int]bool, len(st.Running))
	for _, d := range st.Running {
		if d.Kind == store.KindTend {
			liveTendPRs[d.PRNumber] = true
			// A tend that inherited the issue's session HOLDS that session for
			// as long as it runs, so it blocks the issue as well as its pull
			// request. Two claude processes resuming one session id is the same
			// hazard as two agents in one branch, and it is reachable: a human
			// re-applying the trigger label to an issue still awaiting review
			// produces exactly that pair. A tend carrying its own throwaway
			// session shares nothing and blocks nothing, which is why this
			// compares identifiers rather than testing the kind alone.
			if s := st.Issues[d.Number]; s.SessionID != "" && s.SessionID == d.SessionID {
				liveIssues[d.Number] = true
			}
			continue
		}
		liveIssues[d.Number] = true
	}

	issues := append([]ghub.Issue(nil), snap.Issues...)
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })

	var decisions []Decision
	var parks []Decision
	// stops holds every KindStop decision. It is kept OUT of decisions,
	// exactly like parks, because the breaker branch below drops every
	// entry of decisions and rewrites its skip reason (engine.go:159-168).
	// A stop is the refusal to dispatch, not a dispatch, so it must survive
	// a tripped breaker with its own reason intact.
	var stops []Decision
	decided := make(map[int]bool)
	// skips records why each examined issue got no decision. Entries are
	// removed at the end for every issue that turned out to have one, so an
	// issue skipped by the trigger check and then picked up by tendDecisions
	// does not carry a reason contradicting its own decision.
	skips := make(map[int]string)
	eligibleRetries := 0

	for _, iss := range issues {
		if iss.HasAnyLabel(cfg.Labels.Veto) {
			skips[iss.Number] = "a veto label is present"
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
			skips[iss.Number] = "a dispatch is already live for this issue"
			continue
		}

		state := st.Issues[iss.Number]

		// An operator-stopped issue must never dispatch again until
		// `agent-utils sessions resume` clears the flag. This check sits
		// ABOVE the NeedsRetry branch: a killed dispatch always records a
		// failure, so a stopped issue almost always also carries the retry
		// flag, and if the retry path won here the loop would redispatch the
		// very issue it was told to stop.
		//
		// decided MUST be set here. tendDecisions skips only decided issues
		// (engine.go:259), and a stopped issue awaiting review with a behind
		// pull request would otherwise get a tend agent force-pushing the
		// branch of the session the operator just killed.
		if state.Stopped {
			decided[iss.Number] = true
			skips[iss.Number] = stoppedSkipReason(state.StoppedReason)
			continue
		}

		// Parse ONCE, here, above the retry path. retryDecision receives no
		// labels, so a parse below it could never reach a retry decision and
		// every retry would silently fall back to the configured model.
		//
		// The result is not ACTED on here. An invalid label must stop only a
		// DISPATCH; it must never block KindClearRetry or
		// KindParkRetryExhausted, which are repair actions.
		ov, ovErr := config.ParseOverrides(iss.Labels)

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
			d, eligible, skip := retryDecision(cfg, iss.Number, state, now)
			// Convert to a stop BEFORE counting toward the breaker. A retry
			// that becomes a stop never dispatches, so counting it would let
			// a label push the circuit breaker over its threshold and drop
			// every other issue's dispatches for the whole cooldown. The
			// park kind is exempt: the retry cap is a fact about the issue,
			// not its labels, and must not be blocked by an invalid one.
			if d != nil && d.Kind != KindParkRetryExhausted && ovErr != nil {
				decided[iss.Number] = true
				stops = append(stops, Decision{
					Kind:   KindStop,
					Issue:  iss.Number,
					Reason: ovErr.Error(),
				})
				continue
			}
			if eligible {
				eligibleRetries++
			}
			if d != nil {
				decided[iss.Number] = true
				if d.Kind == KindParkRetryExhausted {
					parks = append(parks, *d)
				} else {
					d.Overrides = ov
					decisions = append(decisions, *d)
				}
			} else {
				skips[iss.Number] = skip
			}
			continue
		}

		// A parked issue needs no separate guard here. parkRetryExhausted removes
		// the trigger label, so the check below already skips it, and a human who
		// re-applies that label deliberately un-parks the issue.
		if !iss.HasLabel(cfg.Labels.Trigger) {
			// Not final: tendDecisions may still act on this issue below, and
			// it replaces this reason with its own when it does not.
			skips[iss.Number] = "no trigger label is present"
			continue
		}

		decided[iss.Number] = true
		if ovErr != nil {
			stops = append(stops, Decision{
				Kind:   KindStop,
				Issue:  iss.Number,
				Reason: ovErr.Error(),
			})
			continue
		}
		if state.SessionID != "" && state.SessionStarted {
			decisions = append(decisions, Decision{
				Kind:      KindResume,
				Issue:     iss.Number,
				Title:     iss.Title,
				SessionID: state.SessionID,
				Reason:    "trigger label present and a started session exists",
				Overrides: ov,
			})
			continue
		}
		decisions = append(decisions, Decision{
			Kind:      KindStart,
			Issue:     iss.Number,
			Title:     iss.Title,
			SessionID: state.SessionID,
			Reason:    "trigger label present and no started session exists",
			Overrides: ov,
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
		// Every dispatch decided above is dropped here, so each one becomes a
		// skip. Without this an issue the engine WAS going to act on reports
		// no reason at all, which is the same all-zeros line this change
		// exists to explain.
		for _, d := range decisions {
			skips[d.Issue] = "the circuit breaker tripped this tick and every dispatch was dropped"
		}
		return finish(Plan{
			Decisions:      append(stops, parks...),
			Skips:          skips,
			BreakerTripped: true,
			CooldownUntil:  now.Add(cfg.Retry.Breaker.Cooldown.Std()),
		})
	}

	decisions = append(decisions, parks...)
	decisions = append(decisions, stops...)

	if cfg.TendPR {
		decisions = append(decisions, tendDecisions(cfg, issues, snap, st.Issues, liveTendPRs, decided, skips)...)
	}

	return finish(Plan{Decisions: decisions, Skips: skips})
}

// finish drops the skip reason of every issue that ended up with a decision.
// The reasons are recorded as the pass goes, before it is known whether a
// later stage acts on the same issue, so this is what keeps "why nothing
// happened" from being reported for an issue where something did.
func finish(p Plan) Plan {
	for _, d := range p.Decisions {
		delete(p.Skips, d.Issue)
	}
	return p
}

// retryDecision returns the action for one failed issue. The second result
// reports whether the failure cleared its backoff window, which is what the
// circuit breaker counts. The third explains a nil decision, so the caller
// never has to re-derive it from the same state.
//
// The window is a wall-clock deadline stored on the issue, not a count of
// ticks. A tick used to be a fixed cron interval, but the webhook daemon can
// tick a loop at any moment, so a tick count no longer names a stable wait.
// MarkNeedsRetry stamps the deadline where the failure is recorded.
func retryDecision(cfg *config.Config, number int, state store.IssueState, now time.Time) (*Decision, bool, string) {
	if state.RetryCount >= cfg.Retry.Max {
		return &Decision{
			Kind:   KindParkRetryExhausted,
			Issue:  number,
			Reason: fmt.Sprintf("retry cap reached (%d/%d)", state.RetryCount, cfg.Retry.Max),
		}, false, ""
	}

	if !state.RetryAfter.IsZero() && now.Before(state.RetryAfter) {
		// Still inside the backoff window. Take no action and post no comment.
		// NeedsRetry stays set in the store, so the next tick sees it again.
		return nil, false, fmt.Sprintf(
			"waiting for the retry backoff window to expire at %s",
			state.RetryAfter.Format(time.RFC3339))
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
		}, true, ""
	}
	return &Decision{
		Kind:      KindRetryStart,
		Issue:     number,
		SessionID: "",
		Reason:    fmt.Sprintf("retry %d/%d with a new session; the previous attempt never started one", state.RetryCount+1, cfg.Retry.Max),
	}, true, ""
}

// stoppedSkipReason renders the skip reason for a stopped issue. An empty
// StoppedReason (a hand-edited database, or a row from before this field
// existed) must not render as a sentence starting with a semicolon.
func stoppedSkipReason(reason string) string {
	if reason == "" {
		return "clear it with `agent-utils sessions resume`"
	}
	return fmt.Sprintf("%s; clear it with `agent-utils sessions resume`", reason)
}

// tendDecisions selects stale pull requests for issues awaiting review.
//
// It refines skips for the issues it examines and declines to act on. An issue
// awaiting review reached this point via the trigger check, whose reason ("no
// trigger label") is true but useless for a review issue: what the operator
// needs is that the pull request is current, or already being tended, or not
// linked at all.
//
// states is the issue state of THIS loop only, which is what makes the
// inherited session the right one without a lookup. An issue is worked by
// several loops in turn -- planning, then execution -- and each keeps its own
// session under its own name. Decide is called per loop, tending is gated on
// that loop's tend_pr, and states came from that loop's rows, so the session a
// tend inherits is necessarily the one belonging to the loop that owns the
// pull request.
func tendDecisions(
	cfg *config.Config,
	issues []ghub.Issue,
	snap Snapshot,
	states map[int]store.IssueState,
	liveTendPRs map[int]bool,
	decided map[int]bool,
	skips map[int]string,
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
			// Not a review issue at all: the trigger check already said why
			// this one produced nothing, and tending has nothing to add.
			continue
		}
		pr, ok := LinkPR(iss.Number, snap.PRs)
		if !ok {
			skips[iss.Number] = "the issue is awaiting review and no trusted pull request is linked"
			continue
		}
		if liveTendPRs[pr.Number] {
			skips[iss.Number] = "a tend dispatch is already live for the linked pull request"
			continue
		}
		if snap.BehindBy[pr.Number] <= 0 {
			// A current pull request produces nothing. Silence is correct.
			skips[iss.Number] = "the linked pull request is already up to date with its base"
			continue
		}
		// Inherit the issue's session, so the rebase agent carries the context
		// of the work it is rebasing rather than meeting the branch cold.
		//
		// SessionStarted is the same gate retryDecision applies, and for the
		// same reason: an identifier claude never created cannot be resumed,
		// and "-r" against one fails identically every run. An empty
		// identifier here tells dispatch to mint a fresh one, which is what a
		// pull request whose issue has no started session still gets.
		sessionID := ""
		reason := fmt.Sprintf("%s is %d commits behind",
			describeLink(iss.Number, pr), snap.BehindBy[pr.Number])
		if s := states[iss.Number]; s.SessionStarted && s.SessionID != "" {
			sessionID = s.SessionID
			reason += ", resuming the issue's session"
		}
		out = append(out, Decision{
			Kind:      KindTend,
			Issue:     iss.Number,
			PR:        pr.Number,
			HeadRef:   pr.HeadRef,
			BaseRef:   pr.BaseRef,
			SessionID: sessionID,
			Reason:    reason,
		})
	}
	return out
}
