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
	if !st.Force && !st.CooldownUntil.IsZero() && now.Before(st.CooldownUntil) {
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

		// Resolved by the caller, because Decide is pure. An absent entry means
		// unresolved, and unresolved never counts as a provider change.
		provider := st.Providers[iss.Number]

		// FAILURE PATH. NeedsRetry is durable state written when a dispatch died
		// or exited non-zero. It covers both a dead runner and a clean non-zero
		// exit, so a failing dispatch can never redispatch without a cap.
		if state.NeedsRetry {
			// The reference loops define an orphan as "carries the in-flight
			// label AND has no live agent". Honour that: an agent that finished
			// its work and moved the label on must not be woken by a retry.
			//
			// The TRIGGER label is the second way to still have work. A dispatch
			// that dies at startup -- a bad prompt template, a session the
			// harness cannot resume -- never takes ownership, so it never
			// applies the in-flight label and the trigger label it was
			// dispatched for is still sitting there. That is a failure with
			// work left, not an agent that moved on, and it must reach
			// retryDecision so the cap can eventually park it.
			//
			// Conflating the two made a startup failure UNCOUNTABLE: the flag
			// was cleared, the next tick saw a triggered issue with a clean
			// slate, dispatched, died in about a second, and cleared again.
			// RetryCount never advanced, so the cap never engaged and the loop
			// could not escalate to the human.
			if !iss.HasLabel(cfg.Labels.InFlight) && !iss.HasLabel(cfg.Labels.Trigger) {
				// Nothing to retry: the agent moved the label on before the
				// failure was recorded.
				//
				// The flag MUST be cleared here. Nothing else clears it, so
				// leaving it set strands the issue permanently: every later tick
				// takes this branch, the trigger check below is never reached,
				// and re-applying the trigger label does nothing.
				decided[iss.Number] = true
				decisions = append(decisions, Decision{
					Kind:   KindClearRetry,
					Issue:  iss.Number,
					Reason: "failure recorded after the agent moved the issue on",
				})
				continue
			}
			d, eligible, skip := retryDecision(cfg, iss.Number, state, ov, provider, now, st.Force)
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
		// the trigger label, so the check below already skips it.
		//
		// Re-applying that label reaches this branch only once needs_retry is
		// clear, which the park itself does. While the flag is still set the
		// failure path above owns the issue, and it un-parks only when the
		// CONFIGURATION changed -- see configRetired. That asymmetry is
		// deliberate: a configuration that failed its whole budget will fail
		// the same way again, and a label edit on its own is not new evidence.
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
		if resumable(cfg, state, ov) {
			decisions = append(decisions, Decision{
				Kind:      KindResume,
				Issue:     iss.Number,
				Title:     iss.Title,
				SessionID: state.SessionID,
				Reason:    "trigger label present and a started session exists",
				Overrides: ov,
				Provider:  provider,
			})
			continue
		}
		// A session belonging to ANOTHER harness carries no identifier into the
		// start: the new harness must mint its own. Reusing the old id would
		// hand claude an id it refuses and pi an id it would quietly reuse.
		sessionID, reason := state.SessionID, "trigger label present and no started session exists"
		if state.SessionStarted && state.SessionID != "" {
			sessionID = ""
			reason = fmt.Sprintf(
				"starting a new session: the existing one was created by %s and this dispatch runs %s",
				state.SessionHarness, EffectiveHarness(cfg, ov))
		}
		decisions = append(decisions, Decision{
			Kind:      KindStart,
			Issue:     iss.Number,
			Title:     iss.Title,
			SessionID: sessionID,
			Reason:    reason,
			Overrides: ov,
			Provider:  provider,
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
	if !st.Force && eligibleRetries >= cfg.Retry.Breaker.OrphanThreshold {
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
func retryDecision(cfg *config.Config, number int, state store.IssueState,
	ov config.Overrides, provider string, now time.Time, force bool) (*Decision, bool, string) {
	// ABOVE the cap and above the backoff window, because it retires both. The
	// window was sized to let the old configuration's platform recover, and
	// that platform is not the one about to be used.
	//
	// KindStart, not KindRetryStart: loopcmd dispatches a start with
	// isRetry=false, and store.BeginDispatch then clears parked, retry_count
	// and retry_after in one statement. That reset IS the retirement; there is
	// no separate unpark. The second result is false so a retirement never
	// counts toward the circuit breaker -- a human reconfiguring the loop is
	// not evidence of a platform fault, and counting it would drop every other
	// issue's dispatch for the whole cooldown.
	if reason := configRetired(cfg, state, ov, provider); reason != "" {
		return &Decision{
			Kind:      KindStart,
			Issue:     number,
			SessionID: "",
			Reason:    reason,
			Provider:  provider,
		}, false, ""
	}

	if state.RetryCount >= cfg.Retry.Max {
		return &Decision{
			Kind:   KindParkRetryExhausted,
			Issue:  number,
			Reason: fmt.Sprintf("retry cap reached (%d/%d)", state.RetryCount, cfg.Retry.Max),
		}, false, ""
	}

	if !force && !state.RetryAfter.IsZero() && now.Before(state.RetryAfter) {
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
	if resumable(cfg, state, ov) {
		return &Decision{
			Kind:      KindRetryResume,
			Issue:     number,
			SessionID: state.SessionID,
			Reason:    fmt.Sprintf("retry %d/%d, resuming the existing session", state.RetryCount+1, cfg.Retry.Max),
			Provider:  provider,
		}, true, ""
	}
	// A session the running harness cannot see is no better than no session:
	// resuming it would fail identically every attempt and spend the whole
	// budget reaching a park that blamed the platform.
	why := "the previous attempt never started one"
	if state.SessionStarted && state.SessionID != "" {
		why = fmt.Sprintf("the existing one was created by %s and this retry runs %s",
			state.SessionHarness, EffectiveHarness(cfg, ov))
	}
	return &Decision{
		Kind:      KindRetryStart,
		Issue:     number,
		SessionID: "",
		Reason:    fmt.Sprintf("retry %d/%d with a new session; %s", state.RetryCount+1, cfg.Retry.Max, why),
		Provider:  provider,
	}, true, ""
}

// resumable reports whether state's session may be handed to a resume by the
// harness this dispatch will run under.
//
// A session id is only meaningful to the harness that minted it: each keeps its
// own store. Handing one across fails in opposite directions -- claude exits
// non-zero on an id it has never seen, and pi creates a fresh session under
// that id and carries on, so the conversation is silently gone. Neither is a
// resume, so the engine starts clean instead and lets the dispatch mint a new
// identifier.
//
// An UNKNOWN recorded harness (empty) is not a mismatch. Rows written before
// the column existed all have one, and treating unknown as "different" would
// restart every in-flight session the moment this version was installed.
func resumable(cfg *config.Config, state store.IssueState, ov config.Overrides) bool {
	if state.SessionID == "" || !state.SessionStarted {
		return false
	}
	if state.SessionHarness == "" {
		return true
	}
	return state.SessionHarness == EffectiveHarness(cfg, ov)
}

// configRetired reports whether the issue's accumulated retry failures still
// describe the configuration the next dispatch will run under. It returns the
// reason when they do not, and "" when they still do.
//
// A retry cap is evidence about ONE configuration. Three OpenRouter 402s say
// nothing about whether claude/opus can build the issue, and holding the cap
// across that change makes the change unusable: the park removes the trigger
// label, so the operator's only move is to re-apply it, and the failure path
// above the trigger branch meets the cap again and re-parks before the new
// configuration has run once.
//
// It compares against what was last ATTEMPTED (DispatchHarness,
// DispatchProvider), never against what last succeeded in creating a session.
// A dispatch that dies before the harness emits a session identifier -- which
// is exactly what a misconfigured harness does -- never updates
// SessionHarness, so a rule written against that field would read "changed" on
// every tick and redispatch forever with no human in the loop. BeginDispatch
// stamps these before the agent runs, so one change buys one retirement.
//
// Both comparisons treat an empty recorded value as UNKNOWN rather than as a
// change, which is the reading resumable applies and for the same reason: rows
// predating those columns would otherwise all retire at once on upgrade.
//
// provider is the provider serving the model this dispatch would use, resolved
// by the caller because Decide is pure. It is empty for claude, which reaches
// one vendor one way, and empty whenever resolution failed; either way the
// provider comparison is skipped and the cap stands.
func configRetired(cfg *config.Config, state store.IssueState, ov config.Overrides, provider string) string {
	if h := EffectiveHarness(cfg, ov); state.DispatchHarness != "" && state.DispatchHarness != h {
		return fmt.Sprintf(
			"retiring the retry history: it belongs to %s and this dispatch runs %s",
			state.DispatchHarness, h)
	}
	// A model change WITHIN one provider is not a retirement. Swapping one
	// OpenRouter model for another while OpenRouter is out of credits changes
	// nothing the failures were about, so the cap must still hold. Crossing to
	// another provider is a different account with its own balance, and the
	// failures stop being evidence.
	if state.DispatchProvider != "" && provider != "" && state.DispatchProvider != provider {
		return fmt.Sprintf(
			"retiring the retry history: it belongs to provider %s and this dispatch runs %s",
			state.DispatchProvider, provider)
	}
	return ""
}

// EffectiveHarness is the harness a dispatch will actually run under: the
// issue's override when it carries one, and the loop's configured harness
// otherwise. ov is already parsed and validated by ParseOverrides, so its
// Harness is either empty or a known harness name.
//
// Exported because loopcmd stamps the same value on the issue row through
// BeginDispatch. Two copies of "which harness will actually run" is how the
// stamp and the comparison drift apart.
func EffectiveHarness(cfg *config.Config, ov config.Overrides) string {
	if ov.Harness != "" {
		return ov.Harness
	}
	if cfg.Agent.Harness != "" {
		return cfg.Agent.Harness
	}
	// config.Load defaults this, so an empty value only reaches here from a
	// hand-built Config in a test. Name the same default Load applies.
	return config.HarnessClaude
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
// A tend decision carries no resolved Provider. It makes no retry decision and
// can never retire a cap, and loopcmd skips BeginDispatch entirely for a tend,
// so there is nothing for the value to reach.
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
		// A tend inherits the issue's session, so it must also inherit the
		// issue's overrides: the session belongs to the harness that minted it,
		// and a tend running the loop default would be handed an identifier
		// that harness has never seen.
		//
		// An override the loop cannot parse skips the tend rather than stopping
		// the issue. A stale rebase is not the issue's own work, and the
		// trigger path already stops an issue whose labels are invalid where
		// that work would happen.
		ov, ovErr := config.ParseOverrides(iss.Labels)
		if ovErr != nil {
			skips[iss.Number] = ovErr.Error()
			continue
		}
		// Inherit the issue's session, so the rebase agent carries the context
		// of the work it is rebasing rather than meeting the branch cold.
		//
		// resumable is the same gate the trigger and retry paths apply, and for
		// the same reasons: an identifier the running harness never created
		// cannot be resumed, and "-r" against one fails identically every run.
		// An empty identifier here tells dispatch to mint a fresh one, which is
		// what a pull request whose issue has no started session still gets.
		sessionID := ""
		reason := fmt.Sprintf("%s is %d commits behind",
			describeLink(iss.Number, pr), snap.BehindBy[pr.Number])
		if s := states[iss.Number]; resumable(cfg, s, ov) {
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
			Overrides: ov,
		})
	}
	return out
}
