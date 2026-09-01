package store

import (
	"testing"
	"time"
)

func TestPutTendConflictRoundTripsEveryField(t *testing.T) {
	s := openTemp(t)
	want := TendConflict{
		Loop:        "execution",
		Repo:        "o/r",
		PRNumber:    9,
		Fingerprint: "abc123",
		SeenCount:   2,
		FirstSeenAt: time.Now().Add(-time.Hour).UTC().Truncate(time.Second),
		LastSeenAt:  time.Now().UTC().Truncate(time.Second),
		RetryAfter:  time.Now().Add(6 * time.Hour).UTC().Truncate(time.Second),
	}
	if err := s.PutTendConflict(want); err != nil {
		t.Fatalf("PutTendConflict: %v", err)
	}

	got, ok, err := s.TendConflict("execution", "o/r", 9)
	if err != nil {
		t.Fatalf("TendConflict: %v", err)
	}
	if !ok {
		t.Fatal("TendConflict reported no row after PutTendConflict")
	}
	if got.Fingerprint != want.Fingerprint {
		t.Errorf("Fingerprint = %q, want %q", got.Fingerprint, want.Fingerprint)
	}
	if got.SeenCount != want.SeenCount {
		t.Errorf("SeenCount = %d, want %d", got.SeenCount, want.SeenCount)
	}
	if !got.FirstSeenAt.Equal(want.FirstSeenAt) {
		t.Errorf("FirstSeenAt = %v, want %v", got.FirstSeenAt, want.FirstSeenAt)
	}
	if !got.LastSeenAt.Equal(want.LastSeenAt) {
		t.Errorf("LastSeenAt = %v, want %v", got.LastSeenAt, want.LastSeenAt)
	}
	if !got.RetryAfter.Equal(want.RetryAfter) {
		t.Errorf("RetryAfter = %v, want %v", got.RetryAfter, want.RetryAfter)
	}
}

// One row per pull request, not per fingerprint: a new fingerprint replaces
// the row rather than accumulating a second one.
func TestPutTendConflictReplacesOnANewFingerprint(t *testing.T) {
	s := openTemp(t)
	if err := s.PutTendConflict(TendConflict{
		Loop: "execution", Repo: "o/r", PRNumber: 9, Fingerprint: "first",
		SeenCount: 1, FirstSeenAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
		RetryAfter: time.Now().Add(time.Hour).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	newRetry := time.Now().Add(6 * time.Hour).UTC().Truncate(time.Second)
	if err := s.PutTendConflict(TendConflict{
		Loop: "execution", Repo: "o/r", PRNumber: 9, Fingerprint: "second",
		SeenCount: 1, FirstSeenAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
		RetryAfter: newRetry,
	}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.TendConflict("execution", "o/r", 9)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("TendConflict reported no row")
	}
	if got.Fingerprint != "second" {
		t.Errorf("Fingerprint = %q, want %q: the second put must replace the first", got.Fingerprint, "second")
	}
	if !got.RetryAfter.Equal(newRetry) {
		t.Errorf("RetryAfter = %v, want %v", got.RetryAfter, newRetry)
	}
}

func TestTendConflictReportsFalseForAnAbsentRow(t *testing.T) {
	s := openTemp(t)
	got, ok, err := s.TendConflict("execution", "o/r", 9)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("TendConflict reported a row for a pull request with none: %+v", got)
	}
}

func TestDeleteTendConflictIsIdempotent(t *testing.T) {
	s := openTemp(t)
	if err := s.PutTendConflict(TendConflict{
		Loop: "execution", Repo: "o/r", PRNumber: 9, Fingerprint: "x",
		SeenCount: 1, FirstSeenAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTendConflict("execution", "o/r", 9); err != nil {
		t.Fatalf("DeleteTendConflict: %v", err)
	}
	if _, ok, err := s.TendConflict("execution", "o/r", 9); err != nil || ok {
		t.Fatalf("row still present after delete: ok=%v err=%v", ok, err)
	}
	// A second delete of an absent row is not an error, the same as
	// DeletePRLink: two passes may agree the same row is gone.
	if err := s.DeleteTendConflict("execution", "o/r", 9); err != nil {
		t.Errorf("a second delete must be a no-op: %v", err)
	}
}

func TestTendConflictIsScopedByProject(t *testing.T) {
	db, _ := openTempDB(t)
	a := db.Project("11111111-1111-1111-1111-111111111111")
	b := db.Project("22222222-2222-2222-2222-222222222222")

	if err := a.PutTendConflict(TendConflict{
		Loop: "execution", Repo: "o/r", PRNumber: 9, Fingerprint: "x",
		SeenCount: 1, FirstSeenAt: time.Now().UTC(), LastSeenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := b.TendConflict("execution", "o/r", 9); err != nil || ok {
		t.Fatalf("project b sees project a's row: ok=%v err=%v", ok, err)
	}
}
