package store

import (
	"path/filepath"
	"testing"
	"time"
)

// openTempDB returns the unscoped handle beside the scoped view, for the
// machine-wide reads a Store cannot answer.
func openTempDB(t *testing.T) (*DB, *Store) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, db.Project(testProject)
}

func TestMarkClosedRoundTrips(t *testing.T) {
	db, s := openTempDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.MarkClosed("o/r", 42, now); err != nil {
		t.Fatalf("MarkClosed: %v", err)
	}

	got, err := db.Closures()
	if err != nil {
		t.Fatalf("Closures: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].ProjectID != testProject || got[0].Repo != "o/r" || got[0].Number != 42 {
		t.Errorf("closure = %+v, want this project's o/r#42", got[0])
	}
	if !got[0].ClosedAt.Equal(now) {
		t.Errorf("ClosedAt = %v, want %v", got[0].ClosedAt, now)
	}
}

// A redelivered close must not move the timestamp: the first time the issue
// closed is the fact worth keeping, and the reconcile re-marks issues the
// listener already marked.
func TestMarkClosedKeepsTheFirstTimestamp(t *testing.T) {
	db, s := openTempDB(t)
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := s.MarkClosed("o/r", 7, first); err != nil {
		t.Fatalf("MarkClosed: %v", err)
	}
	if err := s.MarkClosed("o/r", 7, time.Now().UTC()); err != nil {
		t.Fatalf("MarkClosed again: %v", err)
	}

	got, err := db.Closures()
	if err != nil {
		t.Fatalf("Closures: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 -- the second mark must update, not insert", len(got))
	}
	if !got[0].ClosedAt.Equal(first) {
		t.Errorf("ClosedAt = %v, want the original %v", got[0].ClosedAt, first)
	}
}

// A reopen removes the row rather than writing a false one, so the issue reads
// exactly like one nothing has ever reported on.
func TestClearClosedRemovesTheRow(t *testing.T) {
	db, s := openTempDB(t)
	if err := s.MarkClosed("o/r", 3, time.Now()); err != nil {
		t.Fatalf("MarkClosed: %v", err)
	}
	if err := s.ClearClosed("o/r", 3); err != nil {
		t.Fatalf("ClearClosed: %v", err)
	}
	got, err := db.Closures()
	if err != nil {
		t.Fatalf("Closures: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("closures = %+v, want none after a reopen", got)
	}
}

// Clearing an issue that was never closed is a no-op, not an error: a reopen
// delivery can arrive for an issue this machine never saw close.
func TestClearClosedOnAnUnknownIssueSucceeds(t *testing.T) {
	_, s := openTempDB(t)
	if err := s.ClearClosed("o/r", 99); err != nil {
		t.Fatalf("ClearClosed on an unknown issue: %v", err)
	}
}

// Closure is keyed by project as well as repo. Two projects watching one
// repository must not close each other's issues.
func TestClosuresAreScopedToTheProject(t *testing.T) {
	db, s := openTempDB(t)
	other := db.Project("22222222-2222-2222-2222-222222222222")
	if err := s.MarkClosed("o/r", 5, time.Now()); err != nil {
		t.Fatalf("MarkClosed: %v", err)
	}
	if err := other.ClearClosed("o/r", 5); err != nil {
		t.Fatalf("ClearClosed: %v", err)
	}

	got, err := db.Closures()
	if err != nil {
		t.Fatalf("Closures: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 -- another project's clear must not reach this row", len(got))
	}
}

func TestBelievedOpenExcludesClosedIssues(t *testing.T) {
	db, s := openTempDB(t)
	for _, n := range []int{1, 2} {
		if _, err := s.CreateDispatch(Dispatch{
			Loop: "planning", Repo: "o/r", Number: n, Kind: KindStart,
		}); err != nil {
			t.Fatalf("CreateDispatch: %v", err)
		}
	}
	if err := s.MarkClosed("o/r", 1, time.Now()); err != nil {
		t.Fatalf("MarkClosed: %v", err)
	}

	got, err := db.BelievedOpen()
	if err != nil {
		t.Fatalf("BelievedOpen: %v", err)
	}
	if len(got) != 1 || got[0].Number != 2 {
		t.Fatalf("BelievedOpen = %+v, want only o/r#2", got)
	}
	if got[0].ProjectID != testProject || got[0].Repo != "o/r" {
		t.Errorf("ref = %+v, want this project's o/r", got[0])
	}
}

// Several dispatches for one issue are one candidate: the reconcile asks GitHub
// per issue, not per run.
func TestBelievedOpenIsDistinct(t *testing.T) {
	db, s := openTempDB(t)
	for i := 0; i < 3; i++ {
		if _, err := s.CreateDispatch(Dispatch{
			Loop: "planning", Repo: "o/r", Number: 4, Kind: KindResume,
		}); err != nil {
			t.Fatalf("CreateDispatch: %v", err)
		}
	}
	got, err := db.BelievedOpen()
	if err != nil {
		t.Fatalf("BelievedOpen: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 row for three dispatches of one issue", len(got))
	}
}

// Two loops watching one issue are ONE candidate: the reconcile asks GitHub
// about a number, and the answer is the same for both.
func TestBelievedOpenCollapsesTwoLoopsOnOneIssue(t *testing.T) {
	db, s := openTempDB(t)
	for _, loop := range []string{"planning", "execution"} {
		if _, err := s.CreateDispatch(Dispatch{
			Loop: loop, Repo: "o/r", Number: 11, Kind: KindStart,
		}); err != nil {
			t.Fatalf("CreateDispatch: %v", err)
		}
	}
	got, err := db.BelievedOpen()
	if err != nil {
		t.Fatalf("BelievedOpen: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("BelievedOpen = %+v, want one row for one issue", got)
	}
}

// A dispatch that names no issue is not a candidate. Asking GitHub about issue
// 0 is a guaranteed 404, and a tend dispatch can carry one.
func TestBelievedOpenSkipsRowsThatNameNoIssue(t *testing.T) {
	db, s := openTempDB(t)
	if _, err := s.CreateDispatch(Dispatch{
		Loop: "planning", Repo: "o/r", Number: 0, Kind: KindTend,
	}); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}
	if _, err := s.CreateDispatch(Dispatch{
		Loop: "planning", Repo: "", Number: 9, Kind: KindStart,
	}); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}

	got, err := db.BelievedOpen()
	if err != nil {
		t.Fatalf("BelievedOpen: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("BelievedOpen = %+v, want none", got)
	}
}

// A database written before the closures table existed must gain it on open,
// like every other table added after the first release.
func TestOpenAddsTheClosuresTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	writeOldSchema(t, path)

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open must migrate an older database: %v", err)
	}
	defer db.Close()

	s := db.Project(testProject)
	if err := s.MarkClosed("o/r", 1, time.Now()); err != nil {
		t.Fatalf("MarkClosed on a migrated database: %v", err)
	}
	got, err := db.Closures()
	if err != nil {
		t.Fatalf("Closures: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
}
