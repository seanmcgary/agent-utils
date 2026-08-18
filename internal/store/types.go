package store

import "time"

// Dispatch kinds.
const (
	KindStart  = "start"
	KindResume = "resume"
	KindTend   = "tend"
)

// Dispatch statuses.
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// IssueState is the durable per-issue record.
type IssueState struct {
	Loop         string
	Repo         string
	Number       int
	SessionID    string
	WorktreePath string
	RetryCount   int
	// LastRetryTick is the tick counter value when the last retry was dispatched.
	LastRetryTick int64
	// NeedsRetry is durable failure state. A dispatch that dies or exits non-zero
	// sets it. Only a retry, a park, or a success clears it. It must NOT be
	// derived from the dispatches table: a tick that declines to act (backoff or
	// circuit breaker) would then lose the fact and strand the issue forever.
	NeedsRetry bool
	// SessionStarted records that claude actually created the session. Until it
	// is true, a retry must start rather than resume, because "-r" against a
	// session that was never created fails every time.
	SessionStarted bool
	// Parked records that the loop gave up after the retry cap.
	Parked    bool
	UpdatedAt time.Time
}

// Dispatch is one agent run.
type Dispatch struct {
	ID         int64
	Loop       string
	Repo       string
	Number     int
	Kind       string
	SessionID  string
	PID        int
	PIDStartAt time.Time
	Status     string
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int
	CostUSD    float64
	DurationMS int64
	APIError   string
	LogPath    string
	PRNumber   int
	// Title is the issue title at dispatch time. The detached runner never sees
	// the tick's GitHub snapshot, so a prompt using {{.Issue.Title}} would
	// otherwise render an empty string.
	Title string
}

// DispatchResult is the outcome recorded when a dispatch ends.
type DispatchResult struct {
	Status     string
	ExitCode   int
	CostUSD    float64
	DurationMS int64
	APIError   string
	// SessionStarted reports that the run produced a session identifier, so the
	// session exists on disk and a later retry must resume rather than restart.
	SessionStarted bool
}

// PRLink maps an issue to the pull request that closes it.
type PRLink struct {
	Loop     string
	Repo     string
	Number   int
	PRNumber int
	HeadRef  string
	BaseRef  string
	// BehindBy is how many commits the head lacks from the base. The tend prompt
	// renders it, so it must survive into the detached runner, which never sees
	// the tick's snapshot.
	BehindBy int
}
