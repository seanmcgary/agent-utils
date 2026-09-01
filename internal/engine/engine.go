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

	// Every kind blocks its issue, tends included. A loop no longer dispatches
	// tends, so a KindTend row under a loop's own name can only be one written
	// before tending became its own dispatcher, still draining -- and it used to
	// be admitted here unless it held the issue's session, on the grounds that a
	// tend with a session of its own "shares nothing". That was the wrong test:
	// what a tend and a loop agent share is the BRANCH, which both force-push,
	// and no session identifier says anything about that. The rows drain either
	// way, and until they do the conservative answer is the correct one.
	liveIssues := make(map[int]bool, len(st.Running))
	for _, d := range st.Running {
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
	// issue skipped by one stage and then picked up by a later one does not
	// carry a reason contradicting its own decision.
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
			decided[iss.Number] = true
			skips[iss.Number] = "a dispatch is already live for this issue"
			continue
		}
		if st.Tended[iss.Number] {
			// The other half of the same guard, from outside this loop's scope.
			// The tend dispatcher declines an issue any loop of the project is
			// working (TendState.LiveIssues); this is a loop declining an issue
			// the dispatcher is working. Without it the guard was one-directional
			// and a loop whose veto list did not happen to cover the tend label
			// would start an agent on a branch a tend was rebasing and
			// force-pushing.
			//
			// Its own skip reason, not the one above: "a dispatch is already
			// live" sends an operator looking through this loop's sessions for a
			// row that is filed under the dispatcher's name.
			decided[iss.Number] = true
			skips[iss.Number] = "the project's tend dispatcher holds a live dispatch for this issue"
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
		// The tend dispatcher applies the SAME rule from its own side, reading
		// every loop's stopped issues project-wide (engine.TendState.Stopped),
		// because a stopped issue with a behind pull request would otherwise
		// get a tend agent force-pushing the branch of the session the operator
		// just killed.
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
		//
		// store.KindStart, not store.KindTend: this is the trigger path, which
		// only ever starts or resumes the issue's own session, never a tend's.
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

	// No tend decisions. A loop does not tend: tending is the project's own
	// dispatcher, and DecideTend is what makes its decisions, from
	// project-wide state this per-loop pass does not hold.
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
//
// kind matters here for the same reason it matters to runner.Effective: a
// loop whose tend.harness differs from its agent.harness must START the
// tend's session, never resume the issue's. pi does not refuse an identifier
// it has never seen -- it creates a fresh session under it and carries on --
// so a resume across harnesses loses the conversation silently, the same
// failure the doc comment above already describes for two DIFFERENT
// harnesses generally. A tend inheriting the issue's session while running a
// cheaper tend.harness is exactly that case, reached through the new
// configuration layer instead of a harness: label.
//
// The unknown-harness carve-out above is NOT closed by kind, and that is a
// deliberate limit rather than an oversight. A row written before
// session_harness existed reports "", so a tend running a differing
// tend.harness still resumes it. Closing it would mean guessing that an
// unknown harness differs from this one, which restarts every in-flight
// session on upgrade -- the failure the carve-out exists to prevent, and a
// worse one than a single mis-resumed tend. MarkSessionStarted records the
// resolved harness on every session from here on, so the exposure shrinks to
// rows that predate that column and disappears as they age out.
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

// EffectiveHarness is the harness a dispatch will actually run under,
// resolved in the same order runner.Effective resolves Settings.Harness: the
// issue's label override when it carries one, then the configuration's
// agent.harness, then the claude default. ov is already parsed and validated
// by ParseOverrides, so its Harness is either empty or a known harness name.
//
// There is no dispatch KIND here any more. It used to take one, because a loop
// hosted the project's tends and could run them on a different harness from its
// own -- so "which harness will actually run" depended on whether this dispatch
// was a tend. The tend dispatcher has its own configuration now, whose
// agent.harness IS the tend harness, so the answer depends only on the
// configuration and the label.
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
