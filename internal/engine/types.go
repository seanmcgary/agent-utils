package engine

import (
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// Kind is the type of a decision.
type Kind string

// Decision kinds.
const (
	// KindStart begins a new session for an issue.
	KindStart Kind = "start"
	// KindResume continues the stored session for an issue.
	KindResume Kind = "resume"
	// KindRetryStart redispatches a failed issue with a NEW session, because the
	// previous attempt never created one.
	KindRetryStart Kind = "retry_start"
	// KindRetryResume redispatches a failed issue into its existing session.
	KindRetryResume Kind = "retry_resume"
	// KindTend rebases a stale pull request.
	KindTend Kind = "tend"
	// KindParkRetryExhausted is the one decision that writes to GitHub.
	KindParkRetryExhausted Kind = "park_retry_exhausted"
	// KindClearRetry clears a failure flag that no retry can ever act on,
	// because the issue is not in flight. Without it such an issue is stranded
	// permanently and no human action recovers it.
	KindClearRetry Kind = "clear_retry"
	// KindStop refuses to dispatch an issue: an operator stopped it, or an
	// issue label's override is invalid. It is a refusal, not a dispatch, so
	// it must survive a tripped circuit breaker, which drops every entry of
	// Decisions and rewrites its skip reason (engine.go:159-168).
	KindStop Kind = "stop"
)

// Snapshot is the GitHub view for one tick.
type Snapshot struct {
	Issues []ghub.Issue
	PRs    []ghub.PullRequest
	// BehindBy maps a pull request number to how many commits it lacks.
	BehindBy map[int]int
}

// State is the stored view for one tick.
type State struct {
	Issues map[int]store.IssueState
	// Running holds every dispatch row still marked running whose process is
	// confirmed alive. The caller performs the liveness check, so Decide stays
	// pure.
	Running []store.Dispatch
	// CooldownUntil is the time before which the loop must not dispatch.
	CooldownUntil time.Time
}

// Decision is one action the tick must perform.
type Decision struct {
	Kind      Kind
	Issue     int
	Title     string
	PR        int
	SessionID string
	HeadRef   string
	BaseRef   string
	Reason    string
	// Overrides carries the per-issue label overrides for a dispatch that
	// starts, resumes, or retries a session. The runner is a detached
	// process that never sees the tick's GitHub snapshot, so this is how an
	// override reaches it: on the row, the same way title and behind_by
	// already travel.
	Overrides config.Overrides
}

// Plan is the full output of one decision pass.
type Plan struct {
	Decisions []Decision
	// Skips explains, per issue number, why an issue Decide examined produced
	// no decision. It exists because a scoped tick's summary is all zeros for
	// half a dozen quite different reasons -- a veto label, a live agent, an
	// unexpired backoff window, no trigger label -- and an operator reading
	// "nothing happened" cannot tell which. The reason has to come from HERE
	// rather than be re-derived by the caller: a second copy of these rules
	// would be free to disagree with the one that actually decided.
	//
	// An issue that DID get a decision is absent from the map, so a caller can
	// log the entry whenever there is one.
	Skips map[int]string
	// Halted explains why the pass returned without examining any issue at
	// all. Only the breaker cooldown does that, and it happens before any
	// issue is looked at, so it cannot be a per-issue skip.
	Halted         string
	BreakerTripped bool
	CooldownUntil  time.Time
}

// NoDecisionReason returns why issue got no decision this pass, or "" when it
// got one. It is the single accessor a caller needs, so no caller has to know
// that a whole-pass halt and a per-issue skip are recorded differently.
func (p Plan) NoDecisionReason(issue int) string {
	if p.Halted != "" {
		return p.Halted
	}
	return p.Skips[issue]
}
