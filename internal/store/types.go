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
	// LastRetryTick is dead. Nothing writes it since retries moved to a
	// wall-clock deadline; the column and this field survive so a legacy import
	// round-trips, and dropping a column costs a table rebuild.
	LastRetryTick int64
	// NeedsRetry is durable failure state. A dispatch that dies or exits non-zero
	// sets it. Only a retry, a park, or a success clears it. It must NOT be
	// derived from the dispatches table: a tick that declines to act (backoff or
	// circuit breaker) would then lose the fact and strand the issue forever.
	NeedsRetry bool
	// SessionStarted records that the harness actually created the session.
	// Until it is true, a retry must start rather than resume, because "-r"
	// against a session that was never created fails every time.
	SessionStarted bool
	// SessionHarness is the harness that created the session, and it is what
	// says whether SessionID means anything to the harness about to run.
	//
	// Each harness keeps its own session store, so an id minted by one is
	// meaningless to the other -- and the two fail in OPPOSITE directions.
	// claude exits non-zero ("No conversation found with session ID: <uuid>");
	// pi creates a fresh session under that id and carries on, so the
	// conversation is silently gone. Neither is a resume, and the engine turns
	// a mismatch into a clean start rather than either outcome.
	//
	// Empty means "unknown": the row predates this column, or the session was
	// created before it was recorded. An unknown harness never counts as a
	// mismatch -- guessing would restart every in-flight session on upgrade.
	SessionHarness string
	// Parked records that the loop gave up after the retry cap.
	Parked bool
	// RetryAfter is the deadline before which no retry for this issue may run.
	// The zero value means no deadline, so a pending retry runs at once.
	RetryAfter time.Time
	// Stopped records that an operator killed this issue's session, or that
	// Decide refused to dispatch it because of an invalid label. It is
	// deliberately NOT Parked: Parked means the retry budget ran out, which
	// is a fact about the issue's failure history, while Stopped is a
	// refusal to dispatch at all, cleared only by `sessions resume`. Only
	// MarkStopped and ClearStopped write it; PutIssueState reads it back but
	// must never write it, or a stale read-modify-write (parkRetryExhausted)
	// would silently un-stop the issue.
	Stopped bool
	// StoppedReason is why Stopped is set: the operator's own text, or a
	// label parse error. It is shown in `sessions list` and `loop status`
	// so an operator who did not kill the session can learn why it stopped.
	StoppedReason string
	UpdatedAt     time.Time
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
	// AgentPID is the agent child's own process identifier, recorded after
	// Supervise starts it. It is distinct from PID, the RUNNER's process --
	// the runner is spawned Setsid and the agent child Setpgid into its own
	// group, so a signal to one does not reach the other, and killing an
	// agent needs its own pid. Supervise clears it back to 0 once cmd.Wait
	// returns, because the runner OUTLIVES its agent child; a row whose
	// runner then dies or is killed before recording anything still carries
	// whatever value was last written, so a caller must verify the runner is
	// still alive before trusting a non-zero value here.
	AgentPID int
	// Model, Harness, and Effort are the label overrides in effect for this
	// dispatch, empty when none applied. An empty column means "no
	// override", never "the empty model" -- runner.Effective is the one
	// place that resolves these against the configured value.
	Model   string
	Harness string
	Effort  string
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

// Webhook is the record of one repository's GitHub webhook registration.
//
// It is written only after GitHub confirms the create or the edit, so a row
// here is evidence that a hook exists, not that one was asked for.
type Webhook struct {
	// ProjectID is the owning project's UUID, the first half of the key.
	ProjectID string
	// Repo is "owner/name", the second half.
	Repo string
	// HookID is GitHub's identifier for the hook. It is what deregistration
	// deletes by: matching on URL instead cannot find the hook a project
	// registered before webhook.url was changed.
	HookID int64
	// URL is the delivery target the hook carried when it was recorded. After
	// a webhook.url change it is the only local record of where the live hook
	// still points, which is what makes an orphan diagnosable.
	URL          string
	RegisteredAt time.Time
}

// StoppedIssue is one stopped issue, reported machine-wide by DB.StoppedIssues.
// It names the project because a key of loop and number alone collides
// across projects -- the same reason sessionKey carries the project
// (internal/loopcmd/sessions.go:231).
type StoppedIssue struct {
	ProjectID string
	Loop      string
	Repo      string
	Number    int
	Reason    string
}
