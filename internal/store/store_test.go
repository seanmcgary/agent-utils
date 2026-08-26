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

// writeOldSchema builds a database file with the pre-migration shape, so a
// test can assert that Open brings it forward. Extracted so more than one
// test can start from the same pre-migration fixture.
func writeOldSchema(t *testing.T, path string) {
	t.Helper()
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
}

// A database created by an older build must keep working. CREATE TABLE IF NOT
// EXISTS does nothing to an existing file, so every column added after the
// first release has to be added explicitly.
func TestOpenMigratesAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	writeOldSchema(t, path)

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

// MarkStopped and ClearStopped round-trip the flag and its reason.
//
// ClearStopped also clears needs_retry and retry_after, but leaves parked
// alone. A killed runner's dispatch is recorded FAILED, and finish marks the
// issue for retry (runner.go:320), so a resumed issue would otherwise carry a
// failure it did not earn.
func TestMarkStoppedAndClearStopped(t *testing.T) {
	s := openTemp(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.MarkStopped("planning", "o/r", 1, "harness:gpt is not valid", now); err != nil {
		t.Fatalf("MarkStopped: %v", err)
	}
	got, err := s.IssueState("planning", "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Stopped || got.StoppedReason != "harness:gpt is not valid" {
		t.Fatalf("after MarkStopped = %+v", got)
	}

	// Simulate the retry a killed dispatch earns, so ClearStopped's extra
	// clearing has something to prove it clears.
	if err := s.MarkNeedsRetry("planning", "o/r", 1, now, []time.Duration{time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutIssueState(IssueState{
		Loop: "planning", Repo: "o/r", Number: 1, Parked: true, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	cleared, err := s.ClearStopped("planning", "o/r", 1, now)
	if err != nil {
		t.Fatalf("ClearStopped: %v", err)
	}
	if !cleared {
		t.Fatal("ClearStopped must report true for an issue that was actually stopped")
	}
	got, err = s.IssueState("planning", "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stopped || got.StoppedReason != "" {
		t.Fatalf("after ClearStopped, still stopped: %+v", got)
	}
	if got.NeedsRetry || !got.RetryAfter.IsZero() {
		t.Fatalf("ClearStopped must clear needs_retry and retry_after: %+v", got)
	}
	if !got.Parked {
		t.Fatalf("ClearStopped must NOT clear parked: %+v", got)
	}
}

// ClearStopped must not touch an issue that is not actually stopped: an
// --issue/--session resume target is resolved from config or the registry,
// not from the stopped table, so it can legitimately name an issue that
// merely sits in retry backoff. Without the `stopped = 1` predicate this
// would silently discard that backoff.
func TestClearStoppedIsANoOpForAnIssueThatIsNotStopped(t *testing.T) {
	s := openTemp(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.MarkNeedsRetry("planning", "o/r", 1, now, []time.Duration{time.Minute}); err != nil {
		t.Fatal(err)
	}

	cleared, err := s.ClearStopped("planning", "o/r", 1, now)
	if err != nil {
		t.Fatalf("ClearStopped: %v", err)
	}
	if cleared {
		t.Fatal("ClearStopped must report false for an issue that was never stopped")
	}

	got, err := s.IssueState("planning", "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NeedsRetry || got.RetryAfter.IsZero() {
		t.Fatalf("ClearStopped must leave retry state alone for an issue that was not stopped: %+v", got)
	}
}

// BeginDispatch, MarkSucceeded, and PutIssueState must all leave stopped
// alone. PutIssueState is the subtle one: parkRetryExhausted reads a whole
// state and writes it back (tick.go:499), and if stopped were in its
// conflict set, a state read before a kill and written after it would carry
// stopped = 0 and silently un-stop the issue.
func TestStoppedSurvivesBeginDispatchMarkSucceededAndPutIssueState(t *testing.T) {
	s := openTemp(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.MarkStopped("planning", "o/r", 1, "operator kill", now); err != nil {
		t.Fatal(err)
	}

	if err := s.BeginDispatch("planning", "o/r", 1, "sess-1", false, now); err != nil {
		t.Fatal(err)
	}
	got, _ := s.IssueState("planning", "o/r", 1)
	if !got.Stopped {
		t.Fatalf("BeginDispatch cleared stopped: %+v", got)
	}

	if err := s.MarkSucceeded("planning", "o/r", 1); err != nil {
		t.Fatal(err)
	}
	got, _ = s.IssueState("planning", "o/r", 1)
	if !got.Stopped {
		t.Fatalf("MarkSucceeded cleared stopped: %+v", got)
	}

	// The park path's exact shape: read a state, then write it back stale.
	stale, err := s.IssueState("planning", "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutIssueState(stale); err != nil {
		t.Fatal(err)
	}
	got, _ = s.IssueState("planning", "o/r", 1)
	if !got.Stopped {
		t.Fatalf("PutIssueState cleared stopped: %+v", got)
	}
}

// Store.StoppedIssues is scoped to one project; DB.StoppedIssues spans every
// project. Two projects each hold an issue 7 in a loop with the same name, so
// a read that forgot the project would merge them.
func TestStoppedIssuesScoping(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	const projA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const projB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	sA := db.Project(projA)
	sB := db.Project(projB)
	now := time.Now().UTC().Truncate(time.Second)

	if err := sA.MarkStopped("loopname", "o/r", 7, "reason A", now); err != nil {
		t.Fatal(err)
	}
	if err := sB.MarkStopped("loopname", "o/r", 7, "reason B", now); err != nil {
		t.Fatal(err)
	}

	scoped, err := sA.StoppedIssues("loopname", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 1 || scoped[0].StoppedReason != "reason A" {
		t.Fatalf("Store.StoppedIssues scoped = %+v", scoped)
	}

	all, err := db.StoppedIssues()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("DB.StoppedIssues = %d entries, want 2: %+v", len(all), all)
	}
	byProject := map[string]StoppedIssue{}
	for _, si := range all {
		byProject[si.ProjectID] = si
	}
	if byProject[projA].Reason != "reason A" || byProject[projB].Reason != "reason B" {
		t.Fatalf("DB.StoppedIssues merged projects: %+v", all)
	}
}

// CreateDispatch round-trips the three override columns; SetDispatchAgentPID
// round-trips the agent's own pid, separate from the runner's pid.
func TestCreateDispatchOverridesAndAgentPID(t *testing.T) {
	s := openTemp(t)
	id, err := s.CreateDispatch(Dispatch{
		Loop: "planning", Repo: "o/r", Number: 3, Kind: KindStart,
		Model: "claude-opus-5", Harness: "pi", Effort: "high",
	})
	if err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}
	got, err := s.GetDispatch(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "claude-opus-5" || got.Harness != "pi" || got.Effort != "high" {
		t.Fatalf("override columns did not round-trip: %+v", got)
	}
	if got.AgentPID != 0 {
		t.Fatalf("AgentPID should start at 0: %+v", got)
	}

	if err := s.SetDispatchAgentPID(id, 9999); err != nil {
		t.Fatalf("SetDispatchAgentPID: %v", err)
	}
	got, err = s.GetDispatch(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentPID != 9999 {
		t.Fatalf("AgentPID = %d, want 9999", got.AgentPID)
	}
}

// A database created by an older build gains all six new columns, and a
// stopped issue's stopped/stopped_reason survive the key rebuild that
// happens along the way. A string check that only greps for "stopped" in the
// rebuild column list is satisfied by "stopped_reason" alone and would stay
// green even if "stopped," itself were dropped from the rebuild -- the exact
// silently-un-stop-every-issue bug this test guards against. Asserting on the
// actual data survives that mutation; asserting on the column list does not.
func TestOpenMigratesTheStoppedAndOverrideColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	writeOldSchema(t, path)
	writeOldSchemaStoppedIssue(t, path)

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open must migrate an older database: %v", err)
	}
	defer db.Close()

	for _, c := range []struct{ table, column string }{
		{"issues", "stopped"},
		{"issues", "stopped_reason"},
		{"dispatches", "agent_pid"},
		{"dispatches", "model"},
		{"dispatches", "harness"},
		{"dispatches", "effort"},
	} {
		has, err := hasColumn(db.db, c.table, c.column)
		if err != nil {
			t.Fatalf("hasColumn(%s.%s): %v", c.table, c.column, err)
		}
		if !has {
			t.Errorf("column %s.%s missing after migration", c.table, c.column)
		}
	}

	unclaimed := db.Project("")
	states, err := unclaimed.IssueStates("planning", "o/r")
	if err != nil {
		t.Fatalf("IssueStates after migration: %v", err)
	}
	got, ok := states[2]
	if !ok {
		t.Fatalf("stopped issue #2 lost in migration: %+v", states)
	}
	if !got.Stopped {
		t.Errorf("Stopped did not survive migration: %+v", got)
	}
	if got.StoppedReason != "pre-existing stop" {
		t.Errorf("StoppedReason = %q, want %q", got.StoppedReason, "pre-existing stop")
	}
}

// writeOldSchemaStoppedIssue inserts a second issue row directly, with the
// stopped flag and reason set, into the pre-migration schema written by
// writeOldSchema -- which predates the stopped/stopped_reason columns
// entirely, so this uses ALTER TABLE to add them the way an intermediate
// build would have, then sets them on a fresh row.
func writeOldSchemaStoppedIssue(t *testing.T, path string) {
	t.Helper()
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	_, err = old.Exec(`
		ALTER TABLE issues ADD COLUMN stopped INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE issues ADD COLUMN stopped_reason TEXT NOT NULL DEFAULT '';
		INSERT INTO issues (loop, repo, number, session_id, stopped, stopped_reason, updated_at)
		  VALUES ('planning', 'o/r', 2, '', 1, 'pre-existing stop', CURRENT_TIMESTAMP);`)
	if err != nil {
		t.Fatal(err)
	}
}
