package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIssueStateRoundTrip(t *testing.T) {
	s := openTemp(t)
	want := IssueState{
		Loop:          "planning",
		Repo:          "o/r",
		Number:        42,
		SessionID:     "sess-1",
		WorktreePath:  "/tmp/wt/issue-42",
		RetryCount:    2,
		LastRetryTick: 7,
		UpdatedAt:     time.Now().UTC().Truncate(time.Second),
	}
	if err := s.PutIssueState(want); err != nil {
		t.Fatalf("PutIssueState: %v", err)
	}

	got, err := s.IssueStates("planning", "o/r")
	if err != nil {
		t.Fatalf("IssueStates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[42].SessionID != "sess-1" || got[42].RetryCount != 2 || got[42].LastRetryTick != 7 {
		t.Errorf("round trip mismatch: %+v", got[42])
	}
}

func TestPutIssueStateIsUpsert(t *testing.T) {
	s := openTemp(t)
	base := IssueState{Loop: "planning", Repo: "o/r", Number: 1, SessionID: "a", UpdatedAt: time.Now()}
	if err := s.PutIssueState(base); err != nil {
		t.Fatal(err)
	}
	base.SessionID = "b"
	base.RetryCount = 3
	if err := s.PutIssueState(base); err != nil {
		t.Fatal(err)
	}
	got, _ := s.IssueStates("planning", "o/r")
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 after upsert", len(got))
	}
	if got[1].SessionID != "b" || got[1].RetryCount != 3 {
		t.Errorf("upsert did not overwrite: %+v", got[1])
	}
}

func TestDispatchLifecycle(t *testing.T) {
	s := openTemp(t)
	id, err := s.CreateDispatch(Dispatch{
		Loop: "planning", Repo: "o/r", Number: 5,
		Kind: KindStart, SessionID: "sess-5", LogPath: "/tmp/l.jsonl",
	})
	if err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}

	running, err := s.RunningDispatches("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || running[0].Status != StatusRunning {
		t.Fatalf("running = %+v", running)
	}

	if err := s.SetDispatchProcess(id, 4242, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDispatch(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 4242 {
		t.Errorf("PID = %d, want 4242", got.PID)
	}

	err = s.FinishDispatch(id, DispatchResult{
		Status: StatusSucceeded, ExitCode: 0, CostUSD: 1.25, DurationMS: 900,
	})
	if err != nil {
		t.Fatalf("FinishDispatch: %v", err)
	}

	running, _ = s.RunningDispatches("planning", "o/r")
	if len(running) != 0 {
		t.Errorf("running = %d, want 0 after finish", len(running))
	}
	got, _ = s.GetDispatch(id)
	if got.CostUSD != 1.25 || got.Status != StatusSucceeded {
		t.Errorf("finished dispatch = %+v", got)
	}
}

func TestPRLinkRoundTrip(t *testing.T) {
	s := openTemp(t)
	if err := s.PutPRLink(PRLink{
		Loop: "exec", Repo: "o/r", Number: 9, PRNumber: 31,
		HeadRef: "feat/x", BaseRef: "master",
	}); err != nil {
		t.Fatal(err)
	}
	links, err := s.PRLinks("exec", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if links[9].PRNumber != 31 || links[9].HeadRef != "feat/x" {
		t.Errorf("links = %+v", links)
	}
}

func TestTickCountAndCooldown(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < 3; i++ {
		if _, err := s.RecordTick("planning", false, "{}"); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.TickCount("planning")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("TickCount = %d, want 3", n)
	}

	until := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	if err := s.SetCooldown("planning", until); err != nil {
		t.Fatal(err)
	}
	got, err := s.CooldownUntil("planning")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(until) {
		t.Errorf("CooldownUntil = %v, want %v", got, until)
	}
}

func TestReopenPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.PutIssueState(IssueState{
		Loop: "planning", Repo: "o/r", Number: 1, SessionID: "keep", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, _ := s2.IssueStates("planning", "o/r")
	if got[1].SessionID != "keep" {
		t.Errorf("session did not persist across reopen: %+v", got)
	}
}
