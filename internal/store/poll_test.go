package store

import (
	"testing"
	"time"
)

func TestPollSnapshotRoundTripsAndUpserts(t *testing.T) {
	db, _ := openTempDB(t)
	at := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

	first := []PollSubject{
		{Repo: "o/r", Number: 51, State: "open", Labels: "status:ready", UpdatedAt: at},
		{Repo: "o/r", Number: 52, IsPR: true, State: "open", UpdatedAt: at},
	}
	if err := db.SavePollSubjects("o/r", first); err != nil {
		t.Fatalf("SavePollSubjects: %v", err)
	}

	later := at.Add(time.Hour)
	if err := db.SavePollSubjects("o/r", []PollSubject{
		{Repo: "o/r", Number: 51, State: "closed", Labels: "status:ready", UpdatedAt: later},
	}); err != nil {
		t.Fatalf("SavePollSubjects (update): %v", err)
	}

	got, err := db.PollSnapshot("o/r")
	if err != nil {
		t.Fatalf("PollSnapshot: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[51].State != "closed" || !got[51].UpdatedAt.Equal(later) {
		t.Errorf("51 = %+v, want the UPDATED row", got[51])
	}
	if !got[52].IsPR {
		t.Errorf("52 = %+v, want IsPR", got[52])
	}
}

// Two projects may spell one repository differently in their yaml, and
// ByRepo already folds case for that reason. A snapshot that did not would
// diff each spelling against its own empty history and deliver everything
// twice -- which, on a fresh poll, means dispatching an agent per issue.
func TestPollSnapshotFoldsRepositoryCase(t *testing.T) {
	db, _ := openTempDB(t)
	if err := db.SavePollSubjects("O/R", []PollSubject{
		{Repo: "O/R", Number: 7, State: "open", UpdatedAt: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("SavePollSubjects: %v", err)
	}

	got, err := db.PollSnapshot("o/r")
	if err != nil {
		t.Fatalf("PollSnapshot: %v", err)
	}
	if _, ok := got[7]; !ok {
		t.Fatalf("snapshot for o/r = %+v, want the row written as O/R", got)
	}
}

// "No cursor" and "a cursor at the zero time" mean opposite things: the first
// seeds and delivers nothing, the second delivers the whole history. They
// cannot be told apart by value, so the read reports presence separately.
func TestPollCursorReportsAbsence(t *testing.T) {
	db, _ := openTempDB(t)

	_, ok, err := db.PollCursor("o/r")
	if err != nil {
		t.Fatalf("PollCursor: %v", err)
	}
	if ok {
		t.Fatal("a repository never polled must report ok=false")
	}

	want := PollCursor{
		Repo:     "o/r",
		Since:    time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC),
		HeadSHA:  "deadbeef",
		SeededAt: time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC),
	}
	if err := db.SavePollCursor(want); err != nil {
		t.Fatalf("SavePollCursor: %v", err)
	}

	got, ok, err := db.PollCursor("O/R")
	if err != nil {
		t.Fatalf("PollCursor: %v", err)
	}
	if !ok {
		t.Fatal("ok = false after SavePollCursor")
	}
	if got.HeadSHA != want.HeadSHA || !got.Since.Equal(want.Since) || !got.SeededAt.Equal(want.SeededAt) {
		t.Errorf("cursor = %+v, want %+v", got, want)
	}
}

// SavePollCursor's ON CONFLICT clause deliberately omits seeded_at, so a
// repository's ORIGINAL seeding time survives every later write. seeded_at is
// the durable half of the absence-vs-zero-time distinction that keeps a fresh
// poll from dispatching an agent for every issue in a repository's history:
// an operator needs to be able to tell when a repository entered the poll,
// and that answer must not drift every time the cursor advances.
func TestSavePollCursorKeepsTheOriginalSeededAt(t *testing.T) {
	db, _ := openTempDB(t)

	original := PollCursor{
		Repo:     "o/r",
		Since:    time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC),
		HeadSHA:  "deadbeef",
		SeededAt: time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC),
	}
	if err := db.SavePollCursor(original); err != nil {
		t.Fatalf("SavePollCursor (initial): %v", err)
	}

	later := PollCursor{
		Repo:     "o/r",
		Since:    original.Since.Add(24 * time.Hour),
		HeadSHA:  "cafef00d",
		SeededAt: original.SeededAt.Add(24 * time.Hour),
	}
	if err := db.SavePollCursor(later); err != nil {
		t.Fatalf("SavePollCursor (later): %v", err)
	}

	got, ok, err := db.PollCursor("o/r")
	if err != nil {
		t.Fatalf("PollCursor: %v", err)
	}
	if !ok {
		t.Fatal("ok = false after SavePollCursor")
	}
	if !got.Since.Equal(later.Since) || got.HeadSHA != later.HeadSHA {
		t.Errorf("cursor = %+v, want Since/HeadSHA updated to %+v", got, later)
	}
	if !got.SeededAt.Equal(original.SeededAt) {
		t.Errorf("SeededAt = %v, want the ORIGINAL %v to survive", got.SeededAt, original.SeededAt)
	}
}
