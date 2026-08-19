package migrate

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/store"

	_ "modernc.org/sqlite"
)

const (
	projectA = "11111111-1111-1111-1111-111111111111"
	projectB = "22222222-2222-2222-2222-222222222222"
)

// oldDDL is the schema every per-loop database carried before the canonical one.
// The tests build real files with it, because a migration that is only tested
// against fixtures it invented is not tested at all.
const oldDDL = `
CREATE TABLE issues (
  loop TEXT NOT NULL, repo TEXT NOT NULL, number INTEGER NOT NULL,
  session_id TEXT NOT NULL DEFAULT '', worktree_path TEXT NOT NULL DEFAULT '',
  retry_count INTEGER NOT NULL DEFAULT 0, last_retry_tick INTEGER NOT NULL DEFAULT 0,
  needs_retry INTEGER NOT NULL DEFAULT 0, session_started INTEGER NOT NULL DEFAULT 0,
  parked INTEGER NOT NULL DEFAULT 0, updated_at TIMESTAMP NOT NULL,
  PRIMARY KEY (loop, repo, number));
CREATE TABLE dispatches (
  id INTEGER PRIMARY KEY AUTOINCREMENT, loop TEXT NOT NULL, repo TEXT NOT NULL,
  number INTEGER NOT NULL, kind TEXT NOT NULL, session_id TEXT NOT NULL DEFAULT '',
  pid INTEGER NOT NULL DEFAULT 0, pid_start_at TIMESTAMP, status TEXT NOT NULL,
  started_at TIMESTAMP NOT NULL, finished_at TIMESTAMP,
  exit_code INTEGER NOT NULL DEFAULT 0, cost_usd REAL NOT NULL DEFAULT 0,
  duration_ms INTEGER NOT NULL DEFAULT 0, api_error TEXT NOT NULL DEFAULT '',
  log_path TEXT NOT NULL DEFAULT '', pr_number INTEGER NOT NULL DEFAULT 0,
  title TEXT NOT NULL DEFAULT '');
CREATE TABLE pr_links (
  loop TEXT NOT NULL, repo TEXT NOT NULL, number INTEGER NOT NULL,
  pr_number INTEGER NOT NULL, head_ref TEXT NOT NULL, base_ref TEXT NOT NULL,
  behind_by INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (loop, repo, number));
CREATE TABLE ticks (
  id INTEGER PRIMARY KEY AUTOINCREMENT, loop TEXT NOT NULL,
  started_at TIMESTAMP NOT NULL, breaker_tripped INTEGER NOT NULL DEFAULT 0,
  summary_json TEXT NOT NULL DEFAULT '');
CREATE TABLE cooldowns (loop TEXT PRIMARY KEY, until TIMESTAMP NOT NULL);
`

// legacyFile writes a per-loop database holding one issue, one dispatch, one pr
// link, two ticks and a cooldown, and returns its path.
func legacyFile(t *testing.T, dir, loop, status string, pid int) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, StateDBFile)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(oldDDL); err != nil {
		// The DDL only runs once per file; a second loop in the same file
		// reuses the tables.
		if _, retry := db.Exec("SELECT 1 FROM issues LIMIT 1"); retry != nil {
			t.Fatalf("create the legacy schema: %v", err)
		}
	}

	now := time.Now().UTC()
	if _, err := db.Exec(`
		INSERT INTO issues (loop, repo, number, session_id, worktree_path, updated_at)
		VALUES (?, 'o/r', 42, ?, '/tmp/wt/42', ?)`, loop, "sess-"+loop, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO dispatches (loop, repo, number, kind, session_id, pid, status,
		                        started_at, cost_usd, title)
		VALUES (?, 'o/r', 42, 'start', ?, ?, ?, ?, 1.25, 'do the thing')`,
		loop, "sess-"+loop, pid, status, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO pr_links (loop, repo, number, pr_number, head_ref, base_ref, behind_by)
		VALUES (?, 'o/r', 42, 9, 'issue-42', 'master', 3)`, loop); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.Exec(
			`INSERT INTO ticks (loop, started_at, summary_json) VALUES (?, ?, '{}')`,
			loop, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO cooldowns (loop, until) VALUES (?, ?)`,
		loop, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return path
}

func canonical(t *testing.T) *store.DB {
	t.Helper()
	t.Setenv(home.EnvVar, t.TempDir())
	path, err := home.StateDBPath()
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func sourceAt(path, projectID, loop string) Source {
	return Source{Path: path, ProjectID: projectID, ProjectName: "p", Loop: loop, Repo: "o/r"}
}

func dead(int, int64) bool  { return false }
func alive(int, int64) bool { return true }

func rowCount(t *testing.T, path, table string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&n); err != nil {
		t.Fatalf("count %s in %s: %v", table, path, err)
	}
	return n
}

func TestRunImportsEveryRowOfASource(t *testing.T) {
	db := canonical(t)
	path := legacyFile(t, filepath.Join(t.TempDir(), "state", "planning"), "planning",
		store.StatusSucceeded, 0)

	report, err := Run(db, []Source{sourceAt(path, projectA, "planning")},
		Options{IsAlive: dead})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if failed := report.Failed(); len(failed) != 0 {
		t.Fatalf("failed: %+v", failed)
	}
	if report.Rows() != 6 {
		t.Errorf("wrote %d rows, want 6", report.Rows())
	}

	s := db.Project(projectA)
	states, err := s.IssueStates("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if states[42].SessionID != "sess-planning" {
		t.Errorf("issue not imported: %+v", states[42])
	}
	ds, err := s.DispatchesForLoop("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Title != "do the thing" {
		t.Errorf("dispatch not imported: %+v", ds)
	}
	n, err := s.TickCount("planning")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("TickCount = %d, want 2", n)
	}

	// The source is a backup now, and it is left exactly as it was.
	if got := rowCount(t, path, "dispatches"); got != 1 {
		t.Errorf("the source was modified: %d dispatch rows", got)
	}
	marker := filepath.Join(filepath.Dir(path), MarkerFile)
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("a sealed source has no %s: %v", MarkerFile, err)
	}
}

// Two projects that run a loop of the same name against the same repository must
// not collide. Before the canonical database they were separate files, so this
// is the case the project key exists for.
func TestRunKeepsTwoProjectsApart(t *testing.T) {
	db := canonical(t)
	pathA := legacyFile(t, filepath.Join(t.TempDir(), "a", "planning"), "planning",
		store.StatusSucceeded, 0)
	pathB := legacyFile(t, filepath.Join(t.TempDir(), "b", "planning"), "planning",
		store.StatusSucceeded, 0)

	if _, err := Run(db, []Source{
		sourceAt(pathA, projectA, "planning"),
		sourceAt(pathB, projectB, "planning"),
	}, Options{IsAlive: dead}); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{projectA, projectB} {
		ds, err := db.Project(p).DispatchesForLoop("planning", "o/r")
		if err != nil {
			t.Fatal(err)
		}
		if len(ds) != 1 {
			t.Errorf("project %s sees %d dispatches, want its own 1", p, len(ds))
		}
	}
}

// Two loops may share one state_dir, so one file holds both. Each loop is a
// separate source and must import only its own rows.
func TestRunSplitsTwoLoopsSharingOneFile(t *testing.T) {
	db := canonical(t)
	dir := filepath.Join(t.TempDir(), "shared")
	path := legacyFile(t, dir, "planning", store.StatusSucceeded, 0)
	legacyFile(t, dir, "execution", store.StatusSucceeded, 0)

	if _, err := Run(db, []Source{
		sourceAt(path, projectA, "planning"),
		sourceAt(path, projectA, "execution"),
	}, Options{IsAlive: dead}); err != nil {
		t.Fatal(err)
	}

	s := db.Project(projectA)
	for _, loop := range []string{"planning", "execution"} {
		ds, err := s.DispatchesForLoop(loop, "o/r")
		if err != nil {
			t.Fatal(err)
		}
		if len(ds) != 1 {
			t.Errorf("loop %s has %d dispatches, want 1", loop, len(ds))
		}
	}
}

// The same file and loop cannot belong to two projects. The second is refused
// rather than quietly taking the first one's history.
func TestRunRefusesASecondClaimOnOneSource(t *testing.T) {
	db := canonical(t)
	path := legacyFile(t, filepath.Join(t.TempDir(), "shared"), "planning",
		store.StatusSucceeded, 0)

	if _, err := Run(db, []Source{sourceAt(path, projectA, "planning")},
		Options{IsAlive: dead}); err != nil {
		t.Fatal(err)
	}

	report, err := Run(db, []Source{sourceAt(path, projectB, "planning")},
		Options{IsAlive: dead})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failed()) != 1 {
		t.Fatalf("a second claim was accepted: %+v", report.Results)
	}
	if err := EnsureProject(db, []Source{sourceAt(path, projectB, "planning")}, nil); err == nil {
		t.Error("the write path must refuse a source it could not import")
	}
}

// A second pass over a sealed source must change nothing. Every command reaches
// this code, so a pass that duplicated rows would grow the database forever.
func TestRunIsIdempotent(t *testing.T) {
	db := canonical(t)
	path := legacyFile(t, filepath.Join(t.TempDir(), "state", "planning"), "planning",
		store.StatusSucceeded, 0)
	src := []Source{sourceAt(path, projectA, "planning")}

	if _, err := Run(db, src, Options{IsAlive: dead}); err != nil {
		t.Fatal(err)
	}
	report, err := Run(db, src, Options{IsAlive: dead})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 0 {
		t.Errorf("a sealed source was read again: %+v", report.Results)
	}

	ds, err := db.Project(projectA).DispatchesForLoop("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Errorf("dispatches = %d, want 1; the import duplicated rows", len(ds))
	}
}

// A sealed machine must not take the machine-wide lock on every command. The
// lock file is removed and never recreated, which is what proves the fast path
// ran.
func TestASealedMachineTakesNoLock(t *testing.T) {
	db := canonical(t)
	path := legacyFile(t, filepath.Join(t.TempDir(), "state", "planning"), "planning",
		store.StatusSucceeded, 0)
	src := []Source{sourceAt(path, projectA, "planning")}

	if _, err := Run(db, src, Options{IsAlive: dead}); err != nil {
		t.Fatal(err)
	}
	dir, err := home.Dir()
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, LockFile)
	if err := os.Remove(lockPath); err != nil {
		t.Fatalf("the first run did not take the lock: %v", err)
	}

	if _, err := Run(db, src, Options{IsAlive: dead}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath); err == nil {
		t.Error("a sealed machine took the migration lock")
	}
}

// A source whose runner is still alive is not final. It stays open, keeps its
// marker file away, and is read again until the runner is gone.
func TestALiveRunnerKeepsASourceOpen(t *testing.T) {
	db := canonical(t)
	dir := filepath.Join(t.TempDir(), "state", "planning")
	path := legacyFile(t, dir, "planning", store.StatusRunning, 4242)
	src := []Source{sourceAt(path, projectA, "planning")}

	if _, err := Run(db, src, Options{IsAlive: alive}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerFile)); err == nil {
		t.Error("an open source was marked as migrated")
	}

	// The old runner finished, in its own file, exactly as it would in life.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(
		`UPDATE dispatches SET status = ?, cost_usd = 4.5, finished_at = ? WHERE id = 1`,
		store.StatusSucceeded, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	if _, err := Run(db, src, Options{IsAlive: dead}); err != nil {
		t.Fatal(err)
	}
	ds, err := db.Project(projectA).DispatchesForLoop("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(ds))
	}
	if ds[0].Status != store.StatusSucceeded || ds[0].CostUSD != 4.5 {
		t.Errorf("the outcome recorded by the old runner was lost: %+v", ds[0])
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerFile)); err != nil {
		t.Errorf("the source was not sealed once its runner was gone: %v", err)
	}
}

// An imported dispatch keeps the identifier its live runner carries. Matching on
// the new identifier would report it dead, and the tick would start a second
// agent in a worktree that already holds one.
func TestAnImportedDispatchKeepsItsRunnerIdentity(t *testing.T) {
	db := canonical(t)
	path := legacyFile(t, filepath.Join(t.TempDir(), "state", "planning"), "planning",
		store.StatusRunning, 4242)

	// Give the canonical database a dispatch of its own first, so the imported
	// row is certain to be renumbered.
	if _, err := db.Project(projectB).CreateDispatch(store.Dispatch{
		Loop: "other", Repo: "o/r", Number: 1, Kind: store.KindStart,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(db, []Source{sourceAt(path, projectA, "planning")},
		Options{IsAlive: alive}); err != nil {
		t.Fatal(err)
	}
	ds, err := db.Project(projectA).RunningDispatches("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Fatalf("running dispatches = %d, want 1", len(ds))
	}
	if ds[0].ID == 1 {
		t.Fatal("the imported row was not renumbered; the test proves nothing")
	}
	if ds[0].RunnerID() != 1 {
		t.Errorf("RunnerID = %d, want the identifier its runner carries (1)", ds[0].RunnerID())
	}
}

// A dry run reports and writes nothing.
func TestDryRunWritesNothing(t *testing.T) {
	db := canonical(t)
	dir := filepath.Join(t.TempDir(), "state", "planning")
	path := legacyFile(t, dir, "planning", store.StatusSucceeded, 0)

	report, err := Run(db, []Source{sourceAt(path, projectA, "planning")},
		Options{IsAlive: dead, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Rows() == 0 {
		t.Error("a dry run must report the rows it would write")
	}

	ds, err := db.Project(projectA).DispatchesForLoop("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 0 {
		t.Errorf("a dry run wrote %d dispatches", len(ds))
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerFile)); err == nil {
		t.Error("a dry run wrote a marker file")
	}
	// The source is untouched, and the work is still pending.
	pending, err := Pending(db, []Source{sourceAt(path, projectA, "planning")})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Error("a dry run recorded the source as done")
	}
}

// An empty or unrelated file is not a reason to block a tick: there is nothing
// in it to lose.
func TestASourceWithNoTablesIsSealedNotFailed(t *testing.T) {
	db := canonical(t)
	dir := filepath.Join(t.TempDir(), "state", "planning")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, StateDBFile)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := Run(db, []Source{sourceAt(path, projectA, "planning")},
		Options{IsAlive: dead})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failed()) != 0 {
		t.Fatalf("an empty legacy file blocked the import: %+v", report.Failed())
	}
	if err := report.Err(); err != nil {
		t.Errorf("Err = %v, want nil", err)
	}
}

// The write path must fail loudly when a source it can see cannot be read.
// Carrying on against a database missing this loop's rows would re-dispatch
// every open issue and start a second agent in a worktree that already holds one.
func TestEnsureProjectFailsOnAnUnreadableSource(t *testing.T) {
	db := canonical(t)
	dir := filepath.Join(t.TempDir(), "state", "planning")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, StateDBFile)
	if err := os.WriteFile(path, []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := EnsureProject(db, []Source{sourceAt(path, projectA, "planning")}, nil)
	if err == nil {
		t.Fatal("EnsureProject accepted a source it could not read")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the error must name the source it could not import: %v", err)
	}
}

// A loop whose configuration does not load is not fatal. State is per loop, so a
// broken sibling hides nothing the loop being run needs, and blocking its ticks
// on someone else's YAML would be a new failure this change has no reason to add.
func TestEnsureProjectToleratesADiscoverySkip(t *testing.T) {
	db := canonical(t)
	problems := []Result{{
		Source: sourceAt("", projectA, "broken"),
		State:  StateSkipped,
		Reason: "broken.yaml does not load",
	}}
	if err := EnsureProject(db, nil, problems); err != nil {
		t.Errorf("a discovery skip blocked the write path: %v", err)
	}
}

// A loop whose state_dir IS the home directory has already written into the
// canonical file. Its rows arrive without a project and are stamped, not copied.
//
// The home path is reached through a symlink here on purpose. Sources are
// recorded with symlinks resolved, so a comparison against an unresolved path
// takes the wrong branch: the importer reads nothing, seals the source, and every
// one of those rows stays invisible to every scoped query. The report says
// "imported" while the loop has silently lost its whole history.
func TestCanonicalSourceIsStampedThroughASymlinkedHome(t *testing.T) {
	real := filepath.Join(t.TempDir(), "real-home")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "linked-home")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this filesystem has no symlinks: %v", err)
	}
	t.Setenv(home.EnvVar, link)

	path, err := home.StateDBPath()
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// A row the schema upgrade carried over belongs to no project yet.
	if err := db.Project("").PutIssueState(store.IssueState{
		Loop: "planning", Repo: "o/r", Number: 42, SessionID: "keep-me",
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	src, ok := SourceFor(filepath.Dir(path), projectA, "p", "planning", "o/r")
	if !ok {
		t.Fatal("SourceFor did not find the canonical database")
	}
	if !IsCanonical(src.Path) {
		t.Fatal("the canonical database was not recognised as itself")
	}

	report, err := Run(db, []Source{src}, Options{IsAlive: dead})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failed()) != 0 {
		t.Fatalf("failed: %+v", report.Failed())
	}
	if report.Rows() != 1 {
		t.Errorf("stamped %d rows, want the one row that was already here", report.Rows())
	}

	got, err := db.Project(projectA).IssueState("planning", "o/r", 42)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "keep-me" {
		t.Errorf("the row was never claimed by its project: %+v", got)
	}

	// The note beside a legacy file must never appear beside the canonical one:
	// it says the file is a backup that nothing reads.
	if _, err := os.Stat(filepath.Join(real, MarkerFile)); err == nil {
		t.Error("a MIGRATED note was left beside the canonical database")
	}
}

// A runner that finishes between the read and the liveness check must not be
// sealed away with a stale running row: the reaper would rewrite a successful
// run as failed and re-dispatch the issue.
func TestASourceIsRereadBeforeItIsSealed(t *testing.T) {
	db := canonical(t)
	dir := filepath.Join(t.TempDir(), "state", "planning")
	path := legacyFile(t, dir, "planning", store.StatusRunning, 4242)

	// The runner records its outcome while the importer is between its read and
	// its liveness check, and is gone by the time the check runs.
	finishOnce := false
	isAlive := func(int, int64) bool {
		if !finishOnce {
			finishOnce = true
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.Exec(
				`UPDATE dispatches SET status = ?, cost_usd = 7, finished_at = ? WHERE id = 1`,
				store.StatusSucceeded, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
		}
		return false
	}

	if _, err := Run(db, []Source{sourceAt(path, projectA, "planning")},
		Options{IsAlive: isAlive}); err != nil {
		t.Fatal(err)
	}

	ds, err := db.Project(projectA).DispatchesForLoop("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(ds))
	}
	if ds[0].Status != store.StatusSucceeded || ds[0].CostUSD != 7 {
		t.Errorf("the outcome written between the read and the check was lost: %+v", ds[0])
	}
}
