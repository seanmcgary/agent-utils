package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// retryNow is a whole second in UTC. retry_after holds Unix seconds, so a
// deadline with a fractional part would not survive the round trip and the
// literal expectations below would fail for a reason that is not the arithmetic.
var retryNow = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

// retryBackoff is the list every literal expectation in this file is written
// against: 0s, 15m, 30m.
func retryBackoff() []time.Duration {
	return []time.Duration{0, 15 * time.Minute, 30 * time.Minute}
}

// A database written before retry_after existed must gain the column on the
// next open. Without the addedColumns entry every query naming the column fails
// on an older file, and the loop stops reading its own state.
func TestOpenAddsRetryAfterToAnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	// The shape this file had immediately before retry_after: already
	// project-keyed, so the key rebuild does not run and only addColumns can
	// supply the column.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`
		CREATE TABLE issues (
		  project_id TEXT NOT NULL DEFAULT '', loop TEXT NOT NULL, repo TEXT NOT NULL,
		  number INTEGER NOT NULL, session_id TEXT NOT NULL DEFAULT '',
		  worktree_path TEXT NOT NULL DEFAULT '', retry_count INTEGER NOT NULL DEFAULT 0,
		  last_retry_tick INTEGER NOT NULL DEFAULT 0, needs_retry INTEGER NOT NULL DEFAULT 0,
		  session_started INTEGER NOT NULL DEFAULT 0, parked INTEGER NOT NULL DEFAULT 0,
		  updated_at TIMESTAMP NOT NULL,
		  PRIMARY KEY (project_id, loop, repo, number));
		INSERT INTO issues (project_id, loop, repo, number, session_id, updated_at)
		  VALUES ('` + testProject + `', 'planning', 'o/r', 1, 'keep-me', CURRENT_TIMESTAMP);`,
	); err != nil {
		t.Fatal(err)
	}
	old.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open must add retry_after to an older database: %v", err)
	}
	defer db.Close()

	st, err := db.Project(testProject).IssueState("planning", "o/r", 1)
	if err != nil {
		t.Fatalf("IssueState after the upgrade: %v", err)
	}
	if st.SessionID != "keep-me" {
		t.Errorf("existing row lost: %+v", st)
	}
	if !st.RetryAfter.IsZero() {
		t.Errorf("RetryAfter = %v, want the zero time for a pre-existing row", st.RetryAfter)
	}
}

func TestRetryAfterRoundTrip(t *testing.T) {
	s := openTemp(t)
	want := retryNow.Add(45 * time.Minute)
	if err := s.PutIssueState(IssueState{
		Loop: "planning", Repo: "o/r", Number: 42,
		RetryAfter: want, UpdatedAt: retryNow,
	}); err != nil {
		t.Fatalf("PutIssueState: %v", err)
	}

	got, err := s.IssueState("planning", "o/r", 42)
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}
	if !got.RetryAfter.Equal(want) {
		t.Errorf("RetryAfter = %v, want %v", got.RetryAfter, want)
	}
}

// The arithmetic is pinned with literal times rather than a formula recomputed
// from the same fields the implementation reads: a test that indexes the list
// with the stored retry_count passes against an off-by-one.
func TestMarkNeedsRetryStampsTheEscalatingDeadline(t *testing.T) {
	cases := []struct {
		name       string
		retryCount int
		backoff    []time.Duration
		want       time.Time
	}{
		{"first failure", 0, retryBackoff(), retryNow},
		{"second failure", 1, retryBackoff(), retryNow.Add(15 * time.Minute)},
		{"third failure", 2, retryBackoff(), retryNow.Add(30 * time.Minute)},
		// A retry_count past the end of the list must clamp, not panic.
		{"past the end of the list", 5, retryBackoff(), retryNow.Add(30 * time.Minute)},
		// retry.max: 0 is legal, so the list may be absent entirely.
		{"no backoff configured", 0, nil, time.Time{}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openTemp(t)
			if err := s.PutIssueState(IssueState{
				Loop: "planning", Repo: "o/r", Number: 1,
				RetryCount: c.retryCount, UpdatedAt: retryNow,
			}); err != nil {
				t.Fatalf("PutIssueState: %v", err)
			}
			if err := s.MarkNeedsRetry("planning", "o/r", 1, retryNow, c.backoff); err != nil {
				t.Fatalf("MarkNeedsRetry: %v", err)
			}

			got, err := s.IssueState("planning", "o/r", 1)
			if err != nil {
				t.Fatalf("IssueState: %v", err)
			}
			if !got.NeedsRetry {
				t.Error("NeedsRetry = false, want true")
			}
			if !got.RetryAfter.Equal(c.want) {
				t.Errorf("RetryAfter = %v, want %v", got.RetryAfter, c.want)
			}
			if got.RetryCount != c.retryCount {
				t.Errorf("RetryCount = %d, want %d; recording a failure must not renumber it",
					got.RetryCount, c.retryCount)
			}
		})
	}
}

// A failure recorded for an issue with no row at all must still stamp a
// deadline, indexed at retry_count 0.
func TestMarkNeedsRetryCreatesTheRow(t *testing.T) {
	s := openTemp(t)
	if err := s.MarkNeedsRetry("planning", "o/r", 7, retryNow, retryBackoff()); err != nil {
		t.Fatalf("MarkNeedsRetry: %v", err)
	}
	got, err := s.IssueState("planning", "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NeedsRetry || !got.RetryAfter.Equal(retryNow) {
		t.Errorf("state = %+v, want needs_retry with a deadline of %v", got, retryNow)
	}
}

func TestClearNeedsRetryZeroesTheDeadline(t *testing.T) {
	s := openTemp(t)
	if err := s.MarkNeedsRetry("planning", "o/r", 1, retryNow, retryBackoff()); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearNeedsRetry("planning", "o/r", 1); err != nil {
		t.Fatalf("ClearNeedsRetry: %v", err)
	}
	got, err := s.IssueState("planning", "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.NeedsRetry || !got.RetryAfter.IsZero() {
		t.Errorf("state = %+v, want the flag and the deadline both cleared", got)
	}
}

func TestMarkSucceededZeroesTheDeadline(t *testing.T) {
	s := openTemp(t)
	if err := s.MarkNeedsRetry("planning", "o/r", 1, retryNow, retryBackoff()); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSucceeded("planning", "o/r", 1); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}
	got, err := s.IssueState("planning", "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.NeedsRetry || !got.RetryAfter.IsZero() {
		t.Errorf("state = %+v, want the flag and the deadline both cleared", got)
	}
}

// seedRetryRow writes one issue row with an explicit deadline and flags.
func seedRetryRow(t *testing.T, s *Store, number int, at time.Time, needsRetry, parked bool) {
	t.Helper()
	if err := s.PutIssueState(IssueState{
		Loop: "planning", Repo: "o/r", Number: number,
		NeedsRetry: needsRetry, Parked: parked, RetryAfter: at,
		UpdatedAt: retryNow,
	}); err != nil {
		t.Fatalf("PutIssueState: %v", err)
	}
}

func TestEarliestRetryAfterOnAnEmptyTable(t *testing.T) {
	db := openDB(t)
	_, ok, err := db.EarliestRetryAfter()
	if err != nil {
		t.Fatalf("EarliestRetryAfter: %v", err)
	}
	if ok {
		t.Error("ok = true on an empty table, want false")
	}
}

// Every excluded row would hand the daemon a deadline permanently in the past
// to spin on, so each exclusion is proved on its own.
func TestEarliestRetryAfterSkipsRowsNoRetryCanActOn(t *testing.T) {
	db := openDB(t)
	s := db.Project(testProject)

	seedRetryRow(t, s, 1, retryNow, true, true)                   // parked
	seedRetryRow(t, s, 2, retryNow, false, false)                 // flag cleared
	seedRetryRow(t, s, 3, time.Time{}, true, false)               // no deadline
	seedRetryRow(t, s, 4, retryNow.Add(time.Hour), true, false)   // live, later
	seedRetryRow(t, s, 5, retryNow.Add(time.Minute), true, false) // live, sooner

	due, ok, err := db.EarliestRetryAfter()
	if err != nil {
		t.Fatalf("EarliestRetryAfter: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want the live row")
	}
	if due.Number != 5 {
		t.Errorf("Number = %d, want 5 (the smallest live deadline)", due.Number)
	}
	if !due.At.Equal(retryNow.Add(time.Minute)) {
		t.Errorf("At = %v, want %v", due.At, retryNow.Add(time.Minute))
	}
	if due.ProjectID != testProject || due.Loop != "planning" || due.Repo != "o/r" {
		t.Errorf("key = %+v, want the seeded project, loop and repo", due)
	}
}

// A loop whose breaker is in cooldown decides nothing at all, so its
// needs_retry row keeps a past deadline for the whole cooldown. Returning it
// would make the daemon re-tick that loop every wake interval, each pass
// reading the GitHub API with a repository-write token.
func TestEarliestRetryAfterSkipsALoopInCooldown(t *testing.T) {
	db := openDB(t)
	s := db.Project(testProject)
	seedRetryRow(t, s, 1, retryNow, true, false)

	if err := s.SetCooldown("planning", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}
	if _, ok, err := db.EarliestRetryAfter(); err != nil || ok {
		t.Fatalf("ok = %v (err %v), want false while the loop is in cooldown", ok, err)
	}

	// An expired cooldown must not hide the row: the loop decides again.
	if err := s.SetCooldown("planning", time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}
	due, ok, err := db.EarliestRetryAfter()
	if err != nil {
		t.Fatalf("EarliestRetryAfter: %v", err)
	}
	if !ok || due.Number != 1 {
		t.Errorf("due = %+v, ok = %v, want issue 1 once the cooldown has passed", due, ok)
	}
}

// One file holds every project. A deadline must be returned with the identifier
// of the project that owns it, and a second project's later deadline must not
// displace it.
func TestEarliestRetryAfterIsReportedPerProject(t *testing.T) {
	db := openDB(t)
	a, b := db.Project(testProject), db.Project(otherProject)

	seedRetryRow(t, a, 1, retryNow, true, false)
	seedRetryRow(t, b, 1, retryNow.Add(time.Hour), true, false)

	due, ok, err := db.EarliestRetryAfter()
	if err != nil {
		t.Fatalf("EarliestRetryAfter: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want project A's deadline")
	}
	if due.ProjectID != testProject {
		t.Errorf("ProjectID = %q, want project A (%q)", due.ProjectID, testProject)
	}
	if !due.At.Equal(retryNow) {
		t.Errorf("At = %v, want project A's earlier deadline %v", due.At, retryNow)
	}
}
