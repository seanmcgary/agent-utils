package engine

import (
	"time"

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
	// TickCount is how many ticks this loop has recorded, NOT including this
	// one: the tick reads it before it records itself. The backoff arithmetic
	// relies on that, because LastRetryTick is stamped from the same value.
	TickCount int64
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
}

// Plan is the full output of one decision pass.
type Plan struct {
	Decisions      []Decision
	BreakerTripped bool
	CooldownUntil  time.Time
}
