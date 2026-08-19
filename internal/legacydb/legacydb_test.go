package legacydb

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/store"
)

// oldDDL is the schema the per-loop databases were written with, before the
// canonical database existed. It is copied here rather than imported, because
// internal/store no longer has it and this package's whole job is to read files
// nothing writes any more.
const oldDDL = `
CREATE TABLE IF NOT EXISTS issues (
  loop            TEXT NOT NULL,
  repo            TEXT NOT NULL,
  number          INTEGER NOT NULL,
  session_id      TEXT NOT NULL DEFAULT '',
  worktree_path   TEXT NOT NULL DEFAULT '',
  retry_count     INTEGER NOT NULL DEFAULT 0,
  last_retry_tick INTEGER NOT NULL DEFAULT 0,
  needs_retry     INTEGER NOT NULL DEFAULT 0,
  session_started INTEGER NOT NULL DEFAULT 0,
  parked          INTEGER NOT NULL DEFAULT 0,
  updated_at      TIMESTAMP NOT NULL,
  PRIMARY KEY (loop, repo, number)
);

CREATE TABLE IF NOT EXISTS dispatches (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  loop         TEXT NOT NULL,
  repo         TEXT NOT NULL,
  number       INTEGER NOT NULL,
  kind         TEXT NOT NULL,
  session_id   TEXT NOT NULL DEFAULT '',
  pid          INTEGER NOT NULL DEFAULT 0,
  pid_start_at TIMESTAMP,
  status       TEXT NOT NULL,
  started_at   TIMESTAMP NOT NULL,
  finished_at  TIMESTAMP,
  exit_code    INTEGER NOT NULL DEFAULT 0,
  cost_usd     REAL NOT NULL DEFAULT 0,
  duration_ms  INTEGER NOT NULL DEFAULT 0,
  api_error    TEXT NOT NULL DEFAULT '',
  log_path     TEXT NOT NULL DEFAULT '',
  pr_number    INTEGER NOT NULL DEFAULT 0,
  title        TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS pr_links (
  loop      TEXT NOT NULL,
  repo      TEXT NOT NULL,
  number    INTEGER NOT NULL,
  pr_number INTEGER NOT NULL,
  head_ref  TEXT NOT NULL,
  base_ref  TEXT NOT NULL,
  behind_by INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (loop, repo, number)
);

CREATE TABLE IF NOT EXISTS ticks (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  loop            TEXT NOT NULL,
  started_at      TIMESTAMP NOT NULL,
  breaker_tripped INTEGER NOT NULL DEFAULT 0,
  summary_json    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS cooldowns (
  loop  TEXT PRIMARY KEY,
  until TIMESTAMP NOT NULL
);
`

// earlyDDL is the shape an early release wrote: no dispatches.pr_number, no
// dispatches.title, no pr_links.behind_by. Those columns were added later, and
// a file that predates them must still be readable.
const earlyDDL = `
CREATE TABLE IF NOT EXISTS dispatches (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  loop         TEXT NOT NULL,
  repo         TEXT NOT NULL,
  number       INTEGER NOT NULL,
  kind         TEXT NOT NULL,
  session_id   TEXT NOT NULL DEFAULT '',
  pid          INTEGER NOT NULL DEFAULT 0,
  pid_start_at TIMESTAMP,
  status       TEXT NOT NULL,
  started_at   TIMESTAMP NOT NULL,
  finished_at  TIMESTAMP,
  exit_code    INTEGER NOT NULL DEFAULT 0,
  cost_usd     REAL NOT NULL DEFAULT 0,
  duration_ms  INTEGER NOT NULL DEFAULT 0,
  api_error    TEXT NOT NULL DEFAULT '',
  log_path     TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS pr_links (
  loop      TEXT NOT NULL,
  repo      TEXT NOT NULL,
  number    INTEGER NOT NULL,
  pr_number INTEGER NOT NULL,
  head_ref  TEXT NOT NULL,
  base_ref  TEXT NOT NULL,
  PRIMARY KEY (loop, repo, number)
);
`

// rawOpen opens a legacy file the way the old binary did, so a test can write
// to it. Nothing in the package under test may use this handle.
func rawOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite",
		"file:"+path+"?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// seed creates a legacy file with the given DDL and returns its path plus a
// writable handle on it.
func seed(t *testing.T, ddl string) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	db := rawOpen(t, path)
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("apply the old schema: %v", err)
	}
	return path, db
}

// read opens path with the package under test and reads one loop from it.
func read(t *testing.T, path, loop string) Data {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	defer db.Close()
	data, err := db.Read(loop)
	if err != nil {
		t.Fatalf("Read(%s): %v", loop, err)
	}
	return data
}

// countRows reports the row count of every legacy table, read through a plain
// handle that has nothing to do with the package under test.
func countRows(t *testing.T, path string) map[string]int {
	t.Helper()
	db := rawOpen(t, path)
	out := make(map[string]int)
	for _, table := range []string{"issues", "dispatches", "pr_links", "ticks", "cooldowns"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

func TestReadRoundTripsEveryTableOfAnOldDatabase(t *testing.T) {
	path, db := seed(t, oldDDL)
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := db.Exec(`
		INSERT INTO issues (loop, repo, number, session_id, worktree_path, retry_count,
		                    last_retry_tick, needs_retry, session_started, parked, updated_at)
		VALUES ('planning', 'o/r', 42, 'sess-1', '/tmp/wt/42', 2, 7, 1, 1, 0, ?)`,
		now); err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO dispatches (id, loop, repo, number, kind, session_id, pid, pid_start_at,
		                        status, started_at, finished_at, exit_code, cost_usd,
		                        duration_ms, api_error, log_path, pr_number, title)
		VALUES (9, 'planning', 'o/r', 42, 'start', 'sess-1', 4242, ?, 'succeeded', ?, ?,
		        0, 1.25, 9000, '', '/tmp/log/9.log', 77, 'Add the thing')`,
		now, now, now); err != nil {
		t.Fatalf("insert dispatch: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO pr_links (loop, repo, number, pr_number, head_ref, base_ref, behind_by)
		VALUES ('planning', 'o/r', 42, 77, 'feat/x', 'main', 3)`); err != nil {
		t.Fatalf("insert pr link: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO ticks (loop, started_at, breaker_tripped, summary_json)
		VALUES ('planning', ?, 1, '{"n":1}')`, now); err != nil {
		t.Fatalf("insert tick: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO cooldowns (loop, until) VALUES ('planning', ?)`,
		now); err != nil {
		t.Fatalf("insert cooldown: %v", err)
	}

	data := read(t, path, "planning")

	if len(data.Issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(data.Issues))
	}
	issue := data.Issues[0]
	want := store.IssueState{
		Loop: "planning", Repo: "o/r", Number: 42, SessionID: "sess-1",
		WorktreePath: "/tmp/wt/42", RetryCount: 2, LastRetryTick: 7,
		NeedsRetry: true, SessionStarted: true, Parked: false, UpdatedAt: issue.UpdatedAt,
	}
	if issue != want {
		t.Errorf("issue = %+v, want %+v", issue, want)
	}
	if !issue.UpdatedAt.Equal(now) {
		t.Errorf("issue.UpdatedAt = %v, want %v", issue.UpdatedAt, now)
	}

	if len(data.Dispatches) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(data.Dispatches))
	}
	disp := data.Dispatches[0]
	if disp.ID != 9 {
		t.Errorf("dispatch ID = %d, want the legacy identifier 9", disp.ID)
	}
	if disp.Repo != "o/r" || disp.Number != 42 || disp.Kind != store.KindStart {
		t.Errorf("dispatch key = %+v", disp)
	}
	if disp.SessionID != "sess-1" || disp.PID != 4242 || disp.Status != store.StatusSucceeded {
		t.Errorf("dispatch run = %+v", disp)
	}
	if disp.ExitCode != 0 || disp.CostUSD != 1.25 || disp.DurationMS != 9000 {
		t.Errorf("dispatch outcome = %+v", disp)
	}
	if disp.LogPath != "/tmp/log/9.log" || disp.PRNumber != 77 || disp.Title != "Add the thing" {
		t.Errorf("dispatch detail = %+v", disp)
	}
	if !disp.StartedAt.Equal(now) || !disp.FinishedAt.Equal(now) || !disp.PIDStartAt.Equal(now) {
		t.Errorf("dispatch times = %v %v %v, want %v",
			disp.StartedAt, disp.FinishedAt, disp.PIDStartAt, now)
	}

	if len(data.PRLinks) != 1 {
		t.Fatalf("pr links = %d, want 1", len(data.PRLinks))
	}
	wantLink := store.PRLink{
		Loop: "planning", Repo: "o/r", Number: 42, PRNumber: 77,
		HeadRef: "feat/x", BaseRef: "main", BehindBy: 3,
	}
	if data.PRLinks[0] != wantLink {
		t.Errorf("pr link = %+v, want %+v", data.PRLinks[0], wantLink)
	}

	if len(data.Ticks) != 1 {
		t.Fatalf("ticks = %d, want 1", len(data.Ticks))
	}
	if !data.Ticks[0].BreakerTripped || data.Ticks[0].SummaryJSON != `{"n":1}` {
		t.Errorf("tick = %+v", data.Ticks[0])
	}
	if !data.Ticks[0].StartedAt.Equal(now) {
		t.Errorf("tick.StartedAt = %v, want %v", data.Ticks[0].StartedAt, now)
	}

	if data.Cooldown == nil {
		t.Fatal("cooldown = nil, want the seeded row")
	}
	if data.Cooldown.Loop != "planning" || !data.Cooldown.Until.Equal(now) {
		t.Errorf("cooldown = %+v, want planning until %v", *data.Cooldown, now)
	}

	if got := data.Rows(); got != 5 {
		t.Errorf("Rows() = %d, want 5", got)
	}
}

func TestReadZeroesTheColumnsAnEarlyFileLacks(t *testing.T) {
	path, db := seed(t, earlyDDL)
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := db.Exec(`
		INSERT INTO dispatches (id, loop, repo, number, kind, session_id, pid, status,
		                        started_at, exit_code, cost_usd, duration_ms, api_error, log_path)
		VALUES (1, 'planning', 'o/r', 5, 'start', 'sess', 111, 'running', ?, 0, 0, 0, '', '')`,
		now); err != nil {
		t.Fatalf("insert dispatch: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO pr_links (loop, repo, number, pr_number, head_ref, base_ref)
		VALUES ('planning', 'o/r', 5, 12, 'feat/y', 'main')`); err != nil {
		t.Fatalf("insert pr link: %v", err)
	}

	data := read(t, path, "planning")

	if len(data.Dispatches) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(data.Dispatches))
	}
	if data.Dispatches[0].Title != "" || data.Dispatches[0].PRNumber != 0 {
		t.Errorf("missing columns must read as zero values, got title %q pr %d",
			data.Dispatches[0].Title, data.Dispatches[0].PRNumber)
	}
	if data.Dispatches[0].ID != 1 || data.Dispatches[0].Status != store.StatusRunning {
		t.Errorf("the columns that DO exist must still be read: %+v", data.Dispatches[0])
	}
	if len(data.PRLinks) != 1 {
		t.Fatalf("pr links = %d, want 1", len(data.PRLinks))
	}
	if data.PRLinks[0].BehindBy != 0 || data.PRLinks[0].PRNumber != 12 {
		t.Errorf("pr link = %+v, want behind_by 0 and pr 12", data.PRLinks[0])
	}
	// issues, ticks and cooldowns are absent from this file entirely.
	if len(data.Issues) != 0 || len(data.Ticks) != 0 || data.Cooldown != nil {
		t.Errorf("absent tables must read as empty, got %+v", data)
	}
}

func TestReadOfAFileWithNoTablesIsEmptyAndNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}

	data := read(t, path, "planning")

	if data.Rows() != 0 {
		t.Errorf("Rows() = %d, want 0", data.Rows())
	}
	if data.Issues != nil || data.Dispatches != nil || data.PRLinks != nil ||
		data.Ticks != nil || data.Cooldown != nil {
		t.Errorf("empty file must read as an empty Data, got %+v", data)
	}
}

func TestReadReturnsOnlyTheRequestedLoop(t *testing.T) {
	path, db := seed(t, oldDDL)
	now := time.Now().UTC().Truncate(time.Second)

	// Two loops sharing one state_dir, which the configuration allows. Each is
	// claimed by a different project, so mixing them would hand one project the
	// other's state.
	for _, loop := range []string{"planning", "execution"} {
		if _, err := db.Exec(`
			INSERT INTO issues (loop, repo, number, updated_at) VALUES (?, 'o/r', 1, ?)`,
			loop, now); err != nil {
			t.Fatalf("insert issue for %s: %v", loop, err)
		}
		if _, err := db.Exec(`
			INSERT INTO dispatches (loop, repo, number, kind, status, started_at)
			VALUES (?, 'o/r', 1, 'start', 'succeeded', ?)`, loop, now); err != nil {
			t.Fatalf("insert dispatch for %s: %v", loop, err)
		}
		if _, err := db.Exec(`
			INSERT INTO pr_links (loop, repo, number, pr_number, head_ref, base_ref)
			VALUES (?, 'o/r', 1, 2, 'h', 'main')`, loop); err != nil {
			t.Fatalf("insert pr link for %s: %v", loop, err)
		}
		if _, err := db.Exec(`
			INSERT INTO ticks (loop, started_at) VALUES (?, ?)`, loop, now); err != nil {
			t.Fatalf("insert tick for %s: %v", loop, err)
		}
		if _, err := db.Exec(`INSERT INTO cooldowns (loop, until) VALUES (?, ?)`,
			loop, now); err != nil {
			t.Fatalf("insert cooldown for %s: %v", loop, err)
		}
	}

	data := read(t, path, "planning")

	if data.Rows() != 5 {
		t.Fatalf("Rows() = %d, want the five planning rows only", data.Rows())
	}
	for _, got := range []string{
		data.Issues[0].Loop, data.Dispatches[0].Loop, data.PRLinks[0].Loop,
		data.Ticks[0].Loop, data.Cooldown.Loop,
	} {
		if got != "planning" {
			t.Errorf("read an %q row while asking for planning", got)
		}
	}
}

func TestHasLiveRunnerNeedsARunningRowAndALiveProcess(t *testing.T) {
	running := Data{Dispatches: []store.Dispatch{
		{ID: 7, PID: 4242, Status: store.StatusRunning},
	}}
	finished := Data{Dispatches: []store.Dispatch{
		{ID: 7, PID: 4242, Status: store.StatusSucceeded},
	}}

	alive := func(pid int, dispatchID int64) bool {
		// The live runner carries the LEGACY identifier on its command line.
		return pid == 4242 && dispatchID == 7
	}
	dead := func(int, int64) bool { return false }

	if !running.HasLiveRunner(alive) {
		t.Error("a running row with a live process must hold the source open")
	}
	if finished.HasLiveRunner(alive) {
		t.Error("a succeeded row must not hold the source open")
	}
	if running.HasLiveRunner(dead) {
		t.Error("a running row left by a crashed runner must not pin the source open")
	}
}

func TestReadLeavesTheSourceRowCountsUnchanged(t *testing.T) {
	path, db := seed(t, oldDDL)
	now := time.Now().UTC().Truncate(time.Second)

	if _, err := db.Exec(`
		INSERT INTO issues (loop, repo, number, updated_at) VALUES ('planning', 'o/r', 1, ?)`,
		now); err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO dispatches (loop, repo, number, kind, status, started_at)
		VALUES ('planning', 'o/r', 1, 'start', 'running', ?)`, now); err != nil {
		t.Fatalf("insert dispatch: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO cooldowns (loop, until) VALUES ('planning', ?)`,
		now); err != nil {
		t.Fatalf("insert cooldown: %v", err)
	}

	before := countRows(t, path)
	if got := read(t, path, "planning").Rows(); got != 3 {
		t.Fatalf("Rows() = %d, want 3", got)
	}
	after := countRows(t, path)

	for table, want := range before {
		if after[table] != want {
			t.Errorf("%s holds %d rows after a read, want %d -- the reader wrote to the source",
				table, after[table], want)
		}
	}
}
