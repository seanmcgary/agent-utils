package loopcmd

import (
	"regexp"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/metrics"
	"github.com/seanmcgary/agent-utils/internal/runner"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// Metric names emitted by this package. They are constants so a test asserts
// on the same spelling the code sends, and so the full list sits in one place
// beside docs/configuration.md's table of them.
//
// Later phases add per-issue funnel metrics derived from label transitions and
// post-merge look-backs. They belong in this list, and their tags should come
// from the same builders below, so a dashboard can join a funnel stage to the
// dispatches that produced it on project, repo and loop.
const (
	mDispatchStarted     = "dispatch.started"
	mDispatchFinished    = "dispatch.finished"
	mDispatchCost        = "dispatch.cost_usd"
	mDispatchDuration    = "dispatch.duration_ms"
	mDispatchAPIError    = "dispatch.api_error"
	mDispatchConcurrent  = "dispatch.concurrent"
	mDispatchRetry       = "dispatch.retry"
	mDispatchSpawnFailed = "dispatch.spawn_failed"
	mRetryCapReached     = "retry_cap.reached"
	mBreakerTripped      = "breaker.tripped"
	mResumeFailed        = "resume.failed"
	mWorktreeFailed      = "worktree.setup_failed"
	mRebaseConflict      = "rebase.conflict"
	mRebaseAgentFree     = "rebase.agent_free"
	mRebaseAgent         = "rebase.agent"
	mPark                = "park"
	mTendDispatch        = "tend.dispatch"
	mPRBehindBy          = "pr.behind_by"
	mTickDuration        = "tick.duration_ms"
)

// Values of the "pass" tag on tick.duration_ms: which entry point ran.
const (
	passSweep     = "sweep"
	passIssue     = "issue"
	passTendSweep = "tend_sweep"
	passTendIssue = "tend_issue"
	passTendBase  = "tend_base"
)

// loopTags are the tags every metric about one loop carries.
//
// project is the project's NAME, not its id. An id is a UUID, which reads as
// noise on a dashboard; the name is unique on a machine and is what an
// operator types. It is empty for a Deps built by hand, and metrics.Tag drops
// an empty tag rather than sending "project:".
//
// No issue or pull request number is ever a tag. Each one is a new time
// series for as long as the agent retains it, and a busy repository would mint
// thousands; the log lines carry the numbers for anyone who needs to find one.
func loopTags(cfg *config.Config, deps Deps) []string {
	return []string{
		metrics.Tag("project", deps.ProjectName),
		metrics.Tag("repo", cfg.Repo),
		metrics.Tag("loop", cfg.Name),
	}
}

// kindTag maps a dispatch kind onto the tag value. store.KindStart is spelled
// "fresh" because "start" reads as an event, and a dashboard grouping by kind
// wants the three nouns fresh, resume and tend.
func kindTag(kind string) string {
	if kind == store.KindStart {
		return "fresh"
	}
	return kind
}

// runTags are loopTags plus what the agent ran as: kind, harness, model and
// effort.
//
// The EFFECTIVE values, resolved the way the runner resolves them, not the
// override alone. A dispatch row records only the label, so its model is empty
// whenever the loop's default was used, and grouping by that would put every
// default-model dispatch in one unnamed bucket. An effective value that is
// still empty -- no model configured anywhere -- is left off.
func runTags(cfg *config.Config, deps Deps, kind string, ov config.Overrides) []string {
	eff := runner.Effective(cfg, ov)
	harness := eff.Harness
	if harness == "" {
		harness = config.HarnessClaude
	}
	return append(loopTags(cfg, deps),
		metrics.Tag("kind", kindTag(kind)),
		metrics.Tag("harness", harness),
		metrics.Tag("model", eff.Model),
		metrics.Tag("effort", eff.Effort),
	)
}

// dispatchTags are runTags for a stored dispatch row.
func dispatchTags(cfg *config.Config, deps Deps, d store.Dispatch) []string {
	return runTags(cfg, deps, d.Kind,
		config.Overrides{Model: d.Model, Harness: d.Harness, Effort: d.Effort})
}

// exitClass buckets an exit code. The raw code is not a tag: a signal, a
// harness's own codes and -1 for "no process ever ran" would each be a series,
// and what a dashboard needs is the split between "the agent exited cleanly",
// "the agent exited with an error" and "this program recorded a failure with
// no exit code at all" (a dead runner, a worktree, a template).
func exitClass(code int) string {
	switch {
	case code == 0:
		return "zero"
	case code < 0:
		return "none"
	default:
		return "nonzero"
	}
}

// statusCode matches an HTTP status a provider error usually leads with --
// `402: {"message": ...}` -- as a whole number, so a 402 buried in an id does
// not count.
var statusCode = regexp.MustCompile(`\b(401|402|403|429|500|502|503|504|529)\b`)

// errorClass reduces a dispatch's api_error to a coarse class, for the
// error_class tag on dispatch.api_error.
//
// The text itself is never a tag. It is free-form provider output, it can
// carry a URL or an account identifier (see redactForComment), and every
// distinct string would be a new series. A class is a fixed vocabulary.
//
// Checked in order, most specific first. The program's own messages come
// before the provider ones because they are exact and the provider ones are
// heuristics: "claude refused the session id as already in use" must not be
// read as a rate limit because some later word matched.
func errorClass(apiError string) string {
	s := strings.ToLower(apiError)
	switch {
	case s == "":
		return ""
	case strings.Contains(s, "runner process died"):
		return "runner_died"
	case s == killedByOperatorReason || s == "reset by operator":
		return "operator"
	case strings.Contains(s, "already in use"):
		return "session_in_use"
	case strings.Contains(s, "terminated background work"):
		return "background_abandoned"
	case strings.Contains(s, "no conversation found"):
		return "session_missing"
	case strings.Contains(s, "start claude") || strings.Contains(s, "executable file not found"):
		return "harness_start"
	}
	code := statusCode.FindString(s)
	switch {
	case code == "429" || strings.Contains(s, "rate limit") || strings.Contains(s, "rate_limit"):
		return "rate_limit"
	case code == "402" || strings.Contains(s, "credit") || strings.Contains(s, "billing") ||
		strings.Contains(s, "quota") || strings.Contains(s, "insufficient"):
		return "billing"
	case code == "401" || code == "403" || strings.Contains(s, "unauthorized") ||
		strings.Contains(s, "authentication") || strings.Contains(s, "invalid api key"):
		return "auth"
	case code == "529" || strings.Contains(s, "overloaded"):
		return "overloaded"
	case strings.Contains(s, "deadline exceeded") || strings.Contains(s, "timed out") ||
		strings.Contains(s, "timeout") || strings.Contains(s, "signal: killed") ||
		strings.Contains(s, "signal: terminated"):
		return "timeout"
	case code != "" || strings.Contains(s, "internal server error") ||
		strings.Contains(s, "bad gateway") || strings.Contains(s, "service unavailable"):
		return "server"
	}
	return "other"
}

// emitFinished records a dispatch that has left the running state.
//
// d must already carry its outcome: the row as FinishDispatch left it, or, for
// a reaped runner, a copy with the result fields filled in. dur is zero when
// the caller has no honest duration to give -- a runner found dead by a later
// tick ran for an unknown part of the time since it started -- and then no
// duration is sent rather than a wrong one.
//
// Three callers, deliberately. RunAgent covers every outcome a live runner
// records, reapDead covers the one a runner cannot record because it is dead,
// and `sessions kill` covers a runner it had to kill before it could record
// anything (see productionKiller). Between them every dispatch that emitted dispatch.started emits
// exactly one dispatch.finished, which is the property that makes
// started-minus-finished a usable in-flight count.
func emitFinished(cfg *config.Config, deps Deps, d store.Dispatch, dur time.Duration) {
	m := metrics.Default()
	tags := dispatchTags(cfg, deps, d)
	m.Count(mDispatchFinished, 1, append(tags,
		metrics.Tag("status", d.Status),
		metrics.Tag("exit_code", exitClass(d.ExitCode)))...)
	m.Distribution(mDispatchCost, d.CostUSD, tags...)
	if dur > 0 {
		m.Distribution(mDispatchDuration, float64(dur.Milliseconds()), tags...)
	}
	if d.Status != store.StatusFailed {
		return
	}
	if class := errorClass(d.APIError); class != "" {
		m.Count(mDispatchAPIError, 1, append(tags, metrics.Tag("error_class", class))...)
	}
	// A failed resume is its own counter as well as a filter on
	// dispatch.finished. It is the failure that most often means the session
	// itself is the problem -- gone, held by another process, minted by the
	// other harness -- rather than the work, and an alert wants to name it
	// without knowing the tag vocabulary.
	if d.Kind == store.KindResume {
		m.Count(mResumeFailed, 1, append(tags, metrics.Tag("error_class", errorClass(d.APIError)))...)
	}
}

// tendTrigger names why a tend decision fired, from what the decision itself
// carries: the pull request was behind its base, it carried unanswered review
// activity, or both.
func tendTrigger(d engine.Decision) string {
	behind := d.BehindBy > 0
	switch {
	case behind && d.ReviewPending:
		return "behind_review"
	case d.ReviewPending:
		return "review"
	case behind:
		return "behind"
	}
	return "unknown"
}

// timeTick records how long one pass took. Call it deferred, with the time
// the pass began, so an early return is timed too.
//
// The wall clock, never deps.Now: tests pin deps.Now to a fixed instant, and a
// duration measured against it would always be zero.
func timeTick(cfg *config.Config, deps Deps, pass string, began time.Time) {
	metrics.Default().Distribution(mTickDuration, float64(time.Since(began).Milliseconds()),
		append(loopTags(cfg, deps), metrics.Tag("pass", pass))...)
}
