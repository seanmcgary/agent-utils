package store

import (
	"database/sql"
	"path/filepath"
	"sync"
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
	_, ok, err := db.EarliestRetryAfterAt(time.Now().UTC(), nil)
	if err != nil {
		t.Fatalf("EarliestRetryAfterAt: %v", err)
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

	due, ok, err := db.EarliestRetryAfterAt(time.Now().UTC(), nil)
	if err != nil {
		t.Fatalf("EarliestRetryAfterAt: %v", err)
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
	if _, ok, err := db.EarliestRetryAfterAt(time.Now().UTC(), nil); err != nil || ok {
		t.Fatalf("ok = %v (err %v), want false while the loop is in cooldown", ok, err)
	}

	// An expired cooldown must not hide the row: the loop decides again.
	if err := s.SetCooldown("planning", time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}
	due, ok, err := db.EarliestRetryAfterAt(time.Now().UTC(), nil)
	if err != nil {
		t.Fatalf("EarliestRetryAfterAt: %v", err)
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

	due, ok, err := db.EarliestRetryAfterAt(time.Now().UTC(), nil)
	if err != nil {
		t.Fatalf("EarliestRetryAfterAt: %v", err)
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

// The cooldown boundary is judged against the supplied clock, so the daemon can
// freeze it against its own Now seam.
func TestEarliestRetryAfterAtUsesTheSuppliedClock(t *testing.T) {
	db := openDB(t)
	s := db.Project(testProject)
	seedRetryRow(t, s, 1, retryNow, true, false)
	if err := s.SetCooldown("planning", retryNow.Add(time.Hour)); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}

	if _, ok, err := db.EarliestRetryAfterAt(retryNow, nil); err != nil || ok {
		t.Fatalf("ok = %v (err %v), want false inside the cooldown", ok, err)
	}
	due, ok, err := db.EarliestRetryAfterAt(retryNow.Add(2*time.Hour), nil)
	if err != nil {
		t.Fatalf("EarliestRetryAfterAt: %v", err)
	}
	if !ok || due.Number != 1 {
		t.Errorf("due = %+v, ok = %v, want issue 1 once the clock is past the cooldown", due, ok)
	}
}

// One row is returned per call, so a caller that cannot act on the earliest one
// has to be able to ask for the next. Without this the daemon, which cannot
// tick a loop it is unable to route, would be handed the same unservable row on
// every wake and would never reach any other loop's due deadline.
func TestEarliestRetryAfterAtStepsOverTheSkippedLoops(t *testing.T) {
	db := openDB(t)
	s := db.Project(testProject)
	put := func(loop string, number int, at time.Time) {
		t.Helper()
		if err := s.PutIssueState(IssueState{
			Loop: loop, Repo: "o/r", Number: number,
			NeedsRetry: true, RetryAfter: at, UpdatedAt: retryNow,
		}); err != nil {
			t.Fatalf("PutIssueState: %v", err)
		}
	}
	// Two rows of the earliest loop: the skip is by LOOP, not by row, so one
	// stuck loop with a thousand pending issues still costs one skip.
	put("planning", 1, retryNow)
	put("planning", 2, retryNow.Add(time.Minute))
	put("review", 3, retryNow.Add(time.Hour))

	skip := []LoopKey{{ProjectID: testProject, Loop: "planning"}}
	due, ok, err := db.EarliestRetryAfterAt(retryNow.Add(2*time.Hour), skip)
	if err != nil {
		t.Fatalf("EarliestRetryAfterAt: %v", err)
	}
	if !ok || due.Loop != "review" || due.Number != 3 {
		t.Errorf("due = %+v, ok = %v, want review's deadline past both planning rows", due, ok)
	}

	// The skipped rows are stepped over, not deleted: with no skip the
	// earliest is still planning.
	if due, ok, err = db.EarliestRetryAfterAt(retryNow.Add(2*time.Hour), nil); err != nil || !ok || due.Loop != "planning" {
		t.Errorf("due = %+v, ok = %v (err %v), want planning still pending", due, ok, err)
	}

	// A skip that covers every loop is "nothing to serve", not an error.
	skip = append(skip, LoopKey{ProjectID: testProject, Loop: "review"})
	if _, ok, err = db.EarliestRetryAfterAt(retryNow.Add(2*time.Hour), skip); err != nil || ok {
		t.Errorf("ok = %v (err %v), want false when every pending loop is skipped", ok, err)
	}

	// The skip is keyed by project as well as loop. Two projects may run
	// loops of the same name, and one project's stuck loop must not hide the
	// other's.
	other := db.Project(otherProject)
	if err = other.PutIssueState(IssueState{
		Loop: "planning", Repo: "o/r", Number: 4,
		NeedsRetry: true, RetryAfter: retryNow, UpdatedAt: retryNow,
	}); err != nil {
		t.Fatalf("PutIssueState: %v", err)
	}
	due, ok, err = db.EarliestRetryAfterAt(retryNow.Add(2*time.Hour), skip)
	if err != nil {
		t.Fatalf("EarliestRetryAfterAt: %v", err)
	}
	if !ok || due.ProjectID != otherProject {
		t.Errorf("due = %+v, ok = %v, want the other project's loop of the same name", due, ok)
	}
}

// MarkNeedsRetry reads retry_count and then writes it back. Several processes
// write this file -- every tick and every detached runner -- so the transaction
// takes the write lock as it begins. A deferred transaction would try to
// upgrade a read snapshot at the write, and SQLite answers that with
// SQLITE_BUSY_SNAPSHOT WITHOUT invoking the busy handler, so busy_timeout would
// not cover it: the failure flag would be lost and the issue stranded holding
// the in-flight label.
func TestConcurrentMarkNeedsRetryNeverLosesAFailureFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	const writers = 6

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(number int) {
			defer wg.Done()
			// A separate handle per goroutine, the way a separate process has one.
			db, err := Open(path)
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			defer db.Close()
			if err := db.Project(testProject).MarkNeedsRetry(
				"planning", "o/r", number, retryNow, retryBackoff()); err != nil {
				t.Errorf("MarkNeedsRetry for issue %d: %v", number, err)
			}
		}(i + 1)
	}
	wg.Wait()

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	states, err := db.Project(testProject).IssueStates("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != writers {
		t.Fatalf("len = %d, want %d rows; a failure flag was lost", len(states), writers)
	}
	for number, st := range states {
		if !st.NeedsRetry || !st.RetryAfter.Equal(retryNow) {
			t.Errorf("issue %d = %+v, want needs_retry with a deadline of %v",
				number, st, retryNow)
		}
	}
}

// The dispatch path writes this row from a tick that holds the loop's flock; a
// detached runner process finishing a failed dispatch writes it through
// MarkNeedsRetry and holds nothing. So the columns the failure path owns must
// not be re-written from a value the dispatch read earlier.
//
// A retry spends the budget recorded on the ROW, not one carried across that
// gap, and it leaves the deadline alone -- MarkNeedsRetry is the only writer of
// a real one, and a deadline stamped before the agent runs would be overwritten
// by the failure that follows, collapsing the escalating list to one entry.
func TestBeginDispatchSpendsTheBudgetRecordedOnTheRow(t *testing.T) {
	s := openTemp(t)

	// Two failures: retry_count is still 0 (MarkNeedsRetry does not spend it),
	// so drive it up the way a real retry does, then have the "other process"
	// record one more failure before this dispatch writes.
	if err := s.BeginDispatch("planning", "o/r", 1, "s1", "claude", "", true, retryNow); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkNeedsRetry("planning", "o/r", 1, retryNow, retryBackoff()); err != nil {
		t.Fatal(err)
	}

	if err := s.BeginDispatch("planning", "o/r", 1, "s2", "claude", "", true, retryNow); err != nil {
		t.Fatalf("BeginDispatch: %v", err)
	}

	got, err := s.IssueState("planning", "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.RetryCount != 2 {
		t.Errorf("RetryCount = %d, want 2: the budget must be spent from the row", got.RetryCount)
	}
	if got.RetryAfter.IsZero() {
		t.Error("RetryAfter = 0; a retry must leave MarkNeedsRetry's deadline alone")
	}
	if got.NeedsRetry {
		t.Error("NeedsRetry = true; the dispatch it belongs to is now running")
	}
	if got.SessionID != "s2" {
		t.Errorf("SessionID = %q, want the session this dispatch runs under", got.SessionID)
	}
}

// A human trigger begins a new episode, so this one branch does reset the
// budget and drop the deadline left from the previous one.
func TestBeginDispatchOnAHumanTriggerResetsTheBudget(t *testing.T) {
	s := openTemp(t)
	if err := s.BeginDispatch("planning", "o/r", 1, "s1", "claude", "", true, retryNow); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkNeedsRetry("planning", "o/r", 1, retryNow, retryBackoff()); err != nil {
		t.Fatal(err)
	}

	if err := s.BeginDispatch("planning", "o/r", 1, "s2", "claude", "", false, retryNow); err != nil {
		t.Fatalf("BeginDispatch: %v", err)
	}

	got, err := s.IssueState("planning", "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.RetryCount != 0 || !got.RetryAfter.IsZero() || got.NeedsRetry {
		t.Errorf("state = %+v, want the budget, the deadline and the flag all reset", got)
	}
}

// The worktree is created between the dispatch's two writes -- git, so seconds,
// not microseconds -- and that is the widest window a concurrent failure can
// land in. This write must therefore touch nothing but the path.
func TestSetWorktreePathLeavesTheFailureColumnsAlone(t *testing.T) {
	s := openTemp(t)
	if err := s.BeginDispatch("planning", "o/r", 1, "s1", "claude", "", true, retryNow); err != nil {
		t.Fatal(err)
	}
	// The runner process records its failure while git is still working.
	if err := s.MarkNeedsRetry("planning", "o/r", 1, retryNow, retryBackoff()); err != nil {
		t.Fatal(err)
	}

	if err := s.SetWorktreePath("planning", "o/r", 1, "/wt/1", retryNow); err != nil {
		t.Fatalf("SetWorktreePath: %v", err)
	}

	got, err := s.IssueState("planning", "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.WorktreePath != "/wt/1" {
		t.Errorf("WorktreePath = %q, want /wt/1", got.WorktreePath)
	}
	if !got.NeedsRetry {
		t.Error("NeedsRetry = false; the failure recorded during the worktree step was clobbered")
	}
	if got.RetryAfter.IsZero() {
		t.Error("RetryAfter = 0; the failure's deadline was clobbered")
	}
	if got.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want the 1 the dispatch spent", got.RetryCount)
	}
}

// ClearRetryAfter retires a deadline the engine can never reach without
// destroying the failure it belongs to: nothing re-derives that flag, and the
// issue would be stranded holding an in-flight label with no agent.
func TestClearRetryAfterKeepsTheFlag(t *testing.T) {
	s := openTemp(t)
	if err := s.MarkNeedsRetry("planning", "o/r", 1, retryNow, retryBackoff()); err != nil {
		t.Fatal(err)
	}

	if err := s.ClearRetryAfter("planning", "o/r", 1); err != nil {
		t.Fatalf("ClearRetryAfter: %v", err)
	}

	got, err := s.IssueState("planning", "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !got.RetryAfter.IsZero() {
		t.Errorf("RetryAfter = %v, want zero", got.RetryAfter)
	}
	if !got.NeedsRetry {
		t.Error("NeedsRetry = false; the failure must survive so reopening the issue retries")
	}
}
