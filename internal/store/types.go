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

// Legacy source states.
const (
	// SourceOpen means a runner from the old binary may still write this file,
	// so it must be read again before it can be trusted as final.
	SourceOpen = "open"
	// SourceSealed means nothing will write the file again. It is never reopened.
	SourceSealed = "sealed"
)

// IssueState is the durable per-issue record.
type IssueState struct {
	// ProjectID is the owning project's UUID. It is the first part of every key
	// in this database, and it is what keeps two projects apart in one file.
	ProjectID    string
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
	Parked bool
	// RetryAfter is the deadline before which no retry for this issue may run.
	// The zero value means no deadline, so a pending retry runs at once.
	RetryAfter time.Time
	UpdatedAt  time.Time
}

// Dispatch is one agent run.
type Dispatch struct {
	ID         int64
	ProjectID  string
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
	// LegacySource is the per-loop database this row was imported from, empty
	// for a row created here.
	LegacySource string
	// LegacyID is the identifier this row had in that file.
	LegacyID int64
}

// RunnerID is the dispatch identifier the runner process actually carries.
//
// An imported dispatch was renumbered by this database, but its live runner
// still carries the identifier from the file it was started with, both on its
// command line and in its log file name. Liveness checks and runner log paths
// must use this, never ID: matching on ID would report every imported in-flight
// dispatch as dead, and the tick would start a second agent in a worktree that
// already holds one.
func (d Dispatch) RunnerID() int64 {
	if d.LegacyID != 0 {
		return d.LegacyID
	}
	return d.ID
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
	ProjectID string
	Loop      string
	Repo      string
	Number    int
	PRNumber  int
	HeadRef   string
	BaseRef   string
	// BehindBy is how many commits the head lacks from the base. The tend prompt
	// renders it, so it must survive into the detached runner, which never sees
	// the tick's snapshot.
	BehindBy int
}

// Tick is one recorded reconcile pass. The importer carries these across, so
// a migrated loop keeps its history and its tick counter.
type Tick struct {
	ProjectID      string
	Loop           string
	StartedAt      time.Time
	BreakerTripped bool
	SummaryJSON    string
}

// Cooldown is the time before which a loop must not dispatch.
type Cooldown struct {
	ProjectID string
	Loop      string
	Until     time.Time
}

// LoopKey identifies one loop of one project.
type LoopKey struct {
	ProjectID string
	Loop      string
}

// RetryDue is the earliest pending retry deadline on this machine. It names the
// project as well as the loop, because one file holds every project and the
// daemon has to tick the right one.
type RetryDue struct {
	ProjectID string
	Loop      string
	Repo      string
	Number    int
	At        time.Time
}

// LoopState is one loop's totals, read machine-wide in a single pass.
type LoopState struct {
	ProjectID string
	Loop      string
	Ticks     int64
	LastTick  time.Time
	// Cost is every dispatch this loop ever recorded, whatever repository it
	// watched at the time.
	Cost float64
	// CostByRepo splits that by repository. A loop whose repo was changed in its
	// configuration still holds the old repository's dispatches, and a report of
	// what it costs today must not add the two together.
	CostByRepo map[string]float64
}
