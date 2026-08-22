package store

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const otherProject = "22222222-2222-2222-2222-222222222222"

// Two projects can legitimately run a loop with the same name, against the same
// repository, on the same issue numbers. One file holds both, so every scoped
// read has to filter by project or one project reads the other's session.
func TestTwoProjectsWithTheSameLoopStaySeparate(t *testing.T) {
	db := openDB(t)
	a, b := db.Project(testProject), db.Project(otherProject)

	if err := a.PutIssueState(IssueState{
		Loop: "planning", Repo: "o/r", Number: 7, SessionID: "session-a",
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("PutIssueState: %v", err)
	}
	if err := b.PutIssueState(IssueState{
		Loop: "planning", Repo: "o/r", Number: 7, SessionID: "session-b",
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("PutIssueState: %v", err)
	}

	got, err := a.IssueState("planning", "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "session-a" {
		t.Errorf("project A read %q, want session-a", got.SessionID)
	}
	got, err = b.IssueState("planning", "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "session-b" {
		t.Errorf("project B read %q, want session-b", got.SessionID)
	}

	// A delete in one project must leave the other's row alone.
	if err := a.DeleteIssueState("planning", "o/r", 7); err != nil {
		t.Fatal(err)
	}
	states, err := b.IssueStates("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Errorf("project B lost its row when project A deleted its own: %v", states)
	}
}

// A session identifier is unique, but a scoped caller must not be able to reach
// another project's dispatch by naming one.
func TestScopedReadsCannotCrossProjects(t *testing.T) {
	db := openDB(t)
	a, b := db.Project(testProject), db.Project(otherProject)

	id, err := a.CreateDispatch(Dispatch{
		Loop: "planning", Repo: "o/r", Number: 1, Kind: KindStart, SessionID: "shared-id",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := b.GetDispatch(id); err == nil {
		t.Error("GetDispatch returned another project's dispatch")
	}
	ds, err := b.DispatchesBySession("shared-id")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 0 {
		t.Errorf("DispatchesBySession returned %d rows from another project", len(ds))
	}
	// The owning project still sees it.
	if _, err := a.GetDispatch(id); err != nil {
		t.Errorf("the owning project cannot read its own dispatch: %v", err)
	}
	// A finish from the wrong project must not land either.
	if err := b.FinishDispatch(id, DispatchResult{Status: StatusSucceeded}); err == nil {
		t.Error("FinishDispatch succeeded across projects")
	}
}

// The machine-wide read is what the cross-project commands use instead of
// opening one database per loop.
func TestRunningDispatchesSpansEveryProject(t *testing.T) {
	db := openDB(t)
	for _, p := range []string{testProject, otherProject} {
		if _, err := db.Project(p).CreateDispatch(Dispatch{
			Loop: "planning", Repo: "o/r", Number: 1, Kind: KindStart,
		}); err != nil {
			t.Fatal(err)
		}
	}

	running, err := db.RunningDispatches()
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 2 {
		t.Fatalf("len = %d, want both projects' rows", len(running))
	}
	seen := map[string]bool{}
	for _, d := range running {
		seen[d.ProjectID] = true
	}
	if !seen[testProject] || !seen[otherProject] {
		t.Errorf("RunningDispatches missed a project: %v", seen)
	}
}

// The machine-wide sessions report cannot be built from a scoped read: it has
// to name every project's sessions in one table, and it relies on the newest
// dispatch appearing first so the summary keeps its ordering.
func TestDispatchesSpansEveryProjectNewestFirst(t *testing.T) {
	db := openDB(t)
	var ids []int64
	for _, p := range []string{testProject, otherProject} {
		id, err := db.Project(p).CreateDispatch(Dispatch{
			Loop: "planning", Repo: "o/r", Number: 1, Kind: KindStart,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	all, err := db.Dispatches()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("len = %d, want both projects' rows", len(all))
	}
	seen := map[string]bool{}
	for _, d := range all {
		seen[d.ProjectID] = true
	}
	if !seen[testProject] || !seen[otherProject] {
		t.Errorf("Dispatches missed a project: %v", seen)
	}
	if all[0].ID != ids[1] {
		t.Errorf("all[0].ID = %d, want %d", all[0].ID, ids[1])
	}
	if all[1].ID != ids[0] {
		t.Errorf("all[1].ID = %d, want %d", all[1].ID, ids[0])
	}
}

// A loop that has ticked but never dispatched must still be reported. A join
// would drop it, and reporting "0 ticks" for a live loop is worse than silence.
func TestLoopStatesIncludesALoopThatNeverDispatched(t *testing.T) {
	db := openDB(t)
	s := db.Project(testProject)
	if _, err := s.RecordTick("planning", false, "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordTick("planning", false, "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Project(otherProject).CreateDispatch(Dispatch{
		Loop: "execution", Repo: "o/r", Number: 2, Kind: KindStart,
	}); err != nil {
		t.Fatal(err)
	}

	states, err := db.LoopStates()
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[LoopKey]LoopState{}
	for _, st := range states {
		byKey[LoopKey{ProjectID: st.ProjectID, Loop: st.Loop}] = st
	}
	planning := byKey[LoopKey{ProjectID: testProject, Loop: "planning"}]
	if planning.Ticks != 2 {
		t.Errorf("Ticks = %d, want 2", planning.Ticks)
	}
	if planning.LastTick.IsZero() {
		t.Error("LastTick is zero for a loop that ticked twice")
	}
	if _, ok := byKey[LoopKey{ProjectID: otherProject, Loop: "execution"}]; !ok {
		t.Error("a loop with dispatches and no ticks is missing")
	}
}

// Every tick and every runner on the machine opens this one file, so two
// processes can reach the schema upgrade at the same time. The second must find
// the work done, not redo it.
func TestConcurrentOpensAgreeOnTheSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, err := Open(path)
			if err != nil {
				t.Errorf("Open: %v", err)
				return
			}
			defer db.Close()
			if err := db.Project(testProject).PutIssueState(IssueState{
				Loop: "planning", Repo: "o/r", Number: 1, UpdatedAt: time.Now().UTC(),
			}); err != nil {
				t.Errorf("PutIssueState: %v", err)
			}
		}()
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
	if len(states) != 1 {
		t.Errorf("len = %d, want 1 row after four concurrent opens", len(states))
	}
}

// An imported dispatch was renumbered here, but its live runner still carries
// the identifier from the file it started with.
func TestRunnerIDPrefersTheLegacyIdentifier(t *testing.T) {
	imported := Dispatch{ID: 91, LegacyID: 4, LegacySource: "/old/state.db"}
	if got := imported.RunnerID(); got != 4 {
		t.Errorf("RunnerID = %d, want the legacy identifier 4", got)
	}
	fresh := Dispatch{ID: 91}
	if got := fresh.RunnerID(); got != 91 {
		t.Errorf("RunnerID = %d, want the row identifier 91", got)
	}
}

// The upgrade must survive a database that already carries the project key.
func TestUpgradeIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	for i := 0; i < 3; i++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := db.Project(testProject).PutIssueState(IssueState{
			Loop: "planning", Repo: "o/r", Number: 1, UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var n int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM issues`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("issues holds %d rows after three opens, want 1", n)
	}
}

func openDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
