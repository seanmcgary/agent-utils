package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// testProject is a stand-in for a real project UUID. Every scoped read and
// write carries one, and an empty string is reserved for rows that predate the
// project key.
const testProject = "11111111-1111-1111-1111-111111111111"

func openTemp(t *testing.T) *Store {
	t.Helper()
	return openTempAt(t, filepath.Join(t.TempDir(), "state.db"))
}

func openTempAt(t *testing.T, path string) *Store {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db.Project(testProject)
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

	db1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s1 := db1.Project(testProject)
	if err := s1.PutIssueState(IssueState{
		Loop: "planning", Repo: "o/r", Number: 1, SessionID: "keep", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	db1.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	got, _ := db2.Project(testProject).IssueStates("planning", "o/r")
	if got[1].SessionID != "keep" {
		t.Errorf("session did not persist across reopen: %+v", got)
	}
}

// A database created by an older build must keep working. CREATE TABLE IF NOT
// EXISTS does nothing to an existing file, so every column added after the
// first release has to be added explicitly.
func TestOpenMigratesAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// Build a database with the pre-migration shape.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`
		CREATE TABLE issues (
		  loop TEXT NOT NULL, repo TEXT NOT NULL, number INTEGER NOT NULL,
		  session_id TEXT NOT NULL DEFAULT '', worktree_path TEXT NOT NULL DEFAULT '',
		  retry_count INTEGER NOT NULL DEFAULT 0, last_retry_tick INTEGER NOT NULL DEFAULT 0,
		  updated_at TIMESTAMP NOT NULL, PRIMARY KEY (loop, repo, number));
		CREATE TABLE dispatches (
		  id INTEGER PRIMARY KEY AUTOINCREMENT, loop TEXT NOT NULL, repo TEXT NOT NULL,
		  number INTEGER NOT NULL, kind TEXT NOT NULL, session_id TEXT NOT NULL DEFAULT '',
		  pid INTEGER NOT NULL DEFAULT 0, pid_start_at TIMESTAMP, status TEXT NOT NULL,
		  started_at TIMESTAMP NOT NULL, finished_at TIMESTAMP,
		  exit_code INTEGER NOT NULL DEFAULT 0, cost_usd REAL NOT NULL DEFAULT 0,
		  duration_ms INTEGER NOT NULL DEFAULT 0, api_error TEXT NOT NULL DEFAULT '',
		  log_path TEXT NOT NULL DEFAULT '');
		CREATE TABLE pr_links (
		  loop TEXT NOT NULL, repo TEXT NOT NULL, number INTEGER NOT NULL,
		  pr_number INTEGER NOT NULL, head_ref TEXT NOT NULL, base_ref TEXT NOT NULL,
		  PRIMARY KEY (loop, repo, number));
		INSERT INTO issues (loop, repo, number, session_id, updated_at)
		  VALUES ('planning', 'o/r', 1, 'keep-me', CURRENT_TIMESTAMP);`)
	if err != nil {
		t.Fatal(err)
	}
	old.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open must migrate an older database: %v", err)
	}
	defer db.Close()

	// A row that predates the project key is carried over with an empty
	// project_id. The importer stamps it later; nothing is dropped here.
	unclaimed := db.Project("")
	states, err := unclaimed.IssueStates("planning", "o/r")
	if err != nil {
		t.Fatalf("IssueStates after migration: %v", err)
	}
	if states[1].SessionID != "keep-me" {
		t.Errorf("existing row lost: %+v", states[1])
	}

	// Every query naming a new column must work for a real project too.
	s := db.Project(testProject)
	if _, err := s.RunningDispatches("planning", "o/r"); err != nil {
		t.Errorf("RunningDispatches after migration: %v", err)
	}
	if _, err := s.PRLinks("planning", "o/r"); err != nil {
		t.Errorf("PRLinks after migration: %v", err)
	}
	if _, err := s.CooldownUntil("planning"); err != nil {
		t.Errorf("CooldownUntil after migration: %v", err)
	}
	if got, err := s.IssueStates("planning", "o/r"); err != nil || len(got) != 0 {
		t.Errorf("a project must not see unclaimed rows: %v, %v", got, err)
	}
}
