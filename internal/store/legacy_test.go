package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func legacyKey(project, path string) LegacyKey {
	return LegacyKey{Path: path, ProjectID: project, Loop: "planning", Repo: "o/r"}
}

func sampleData(sessionID string) LegacyData {
	now := time.Now().UTC().Truncate(time.Second)
	return LegacyData{
		Issues: []IssueState{{
			Loop: "planning", Repo: "o/r", Number: 42, SessionID: sessionID,
			WorktreePath: "/tmp/wt/issue-42", RetryCount: 1, SessionStarted: true,
			UpdatedAt: now,
		}},
		Dispatches: []Dispatch{{
			ID: 3, Loop: "planning", Repo: "o/r", Number: 42, Kind: KindStart,
			SessionID: sessionID, PID: 4242, Status: StatusRunning, StartedAt: now,
			LogPath: "/logs/start-42.jsonl", Title: "do the thing", CostUSD: 1.5,
		}},
		PRLinks: []PRLink{{
			Loop: "planning", Repo: "o/r", Number: 42, PRNumber: 9,
			HeadRef: "issue-42", BaseRef: "master", BehindBy: 2,
		}},
		Ticks: []Tick{
			{Loop: "planning", StartedAt: now, SummaryJSON: "{}"},
			{Loop: "planning", StartedAt: now, SummaryJSON: "{}"},
		},
		Cooldown: &Cooldown{Loop: "planning", Until: now.Add(time.Hour)},
	}
}

func TestImportLegacyCopiesEveryTable(t *testing.T) {
	db := openDB(t)
	k := legacyKey(testProject, "/old/planning/state.db")

	rows, err := db.ImportLegacy(k, sampleData("sess-1"), true)
	if err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}
	if rows != 6 {
		t.Errorf("wrote %d rows, want 6", rows)
	}

	s := db.Project(testProject)
	states, err := s.IssueStates("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if states[42].SessionID != "sess-1" || !states[42].SessionStarted {
		t.Errorf("issue did not survive the import: %+v", states[42])
	}
	ds, err := s.RunningDispatches("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Fatalf("running dispatches = %d, want 1", len(ds))
	}
	// The row is renumbered here, so the identifier the live runner carries has
	// to come from legacy_id.
	if ds[0].RunnerID() != 3 {
		t.Errorf("RunnerID = %d, want the legacy identifier 3", ds[0].RunnerID())
	}
	if ds[0].LegacySource != k.Path {
		t.Errorf("LegacySource = %q, want %q", ds[0].LegacySource, k.Path)
	}
	if ds[0].Title != "do the thing" {
		t.Errorf("Title lost in the import: %q", ds[0].Title)
	}
	links, err := s.PRLinks("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if links[42].PRNumber != 9 || links[42].BehindBy != 2 {
		t.Errorf("pr link lost in the import: %+v", links[42])
	}
	n, err := s.TickCount("planning")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("TickCount = %d, want 2", n)
	}
	until, err := s.CooldownUntil("planning")
	if err != nil {
		t.Fatal(err)
	}
	if until.IsZero() {
		t.Error("cooldown lost in the import")
	}
}

// The import must be safe to repeat. A sealed source is never read again, and a
// second pass over the same rows must not duplicate them.
func TestImportLegacyIsIdempotent(t *testing.T) {
	db := openDB(t)
	k := legacyKey(testProject, "/old/planning/state.db")
	data := sampleData("sess-1")

	if _, err := db.ImportLegacy(k, data, true); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ImportLegacy(k, data, true)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if rows != 0 {
		t.Errorf("a sealed source wrote %d rows on a second pass, want 0", rows)
	}

	s := db.Project(testProject)
	ds, err := s.DispatchesForLoop("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Errorf("dispatches = %d, want 1; the import duplicated rows", len(ds))
	}
	n, err := s.TickCount("planning")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("TickCount = %d, want 2; the import duplicated ticks", n)
	}
}

// Two projects can hold the same loop name, the same repository and the same
// issue numbers. Their imports must not touch each other.
func TestImportLegacyKeepsTwoProjectsApart(t *testing.T) {
	db := openDB(t)
	a := legacyKey(testProject, "/a/planning/state.db")
	b := legacyKey(otherProject, "/b/planning/state.db")

	if _, err := db.ImportLegacy(a, sampleData("sess-a"), true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ImportLegacy(b, sampleData("sess-b"), true); err != nil {
		t.Fatal(err)
	}

	got, err := db.Project(testProject).IssueState("planning", "o/r", 42)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "sess-a" {
		t.Errorf("project A reads %q, want sess-a", got.SessionID)
	}
	got, err = db.Project(otherProject).IssueState("planning", "o/r", 42)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "sess-b" {
		t.Errorf("project B reads %q, want sess-b", got.SessionID)
	}
}

// A refresh carries the outcome a runner from the old binary recorded in the
// source after this database had already copied the row.
func TestRefreshAppliesAnOutcomeToARunningRow(t *testing.T) {
	db := openDB(t)
	k := legacyKey(testProject, "/old/planning/state.db")
	data := sampleData("sess-1")

	if _, err := db.ImportLegacy(k, data, false); err != nil {
		t.Fatal(err)
	}

	// The old runner finished, in its own file.
	finished := data
	finished.Dispatches = []Dispatch{data.Dispatches[0]}
	finished.Dispatches[0].Status = StatusSucceeded
	finished.Dispatches[0].ExitCode = 0
	finished.Dispatches[0].CostUSD = 4.25
	finished.Dispatches[0].FinishedAt = time.Now().UTC()

	if _, err := db.ImportLegacy(k, finished, true); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	ds, err := db.Project(testProject).DispatchesForLoop("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(ds))
	}
	if ds[0].Status != StatusSucceeded || ds[0].CostUSD != 4.25 {
		t.Errorf("the refresh did not carry the outcome over: %+v", ds[0])
	}
}

// The tick reaps an imported dispatch whose process is gone before the source is
// read again. That verdict is a guess; the source holds what the runner recorded,
// so the refresh must be allowed to correct it.
func TestRefreshCorrectsTheReapersGuess(t *testing.T) {
	db := openDB(t)
	k := legacyKey(testProject, "/old/planning/state.db")
	data := sampleData("sess-1")

	if _, err := db.ImportLegacy(k, data, false); err != nil {
		t.Fatal(err)
	}
	s := db.Project(testProject)
	ds, err := s.RunningDispatches("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDispatch(ds[0].ID, DispatchResult{
		Status: StatusFailed, ExitCode: -1, APIError: reaperVerdict,
	}); err != nil {
		t.Fatal(err)
	}

	finished := data
	finished.Dispatches = []Dispatch{data.Dispatches[0]}
	finished.Dispatches[0].Status = StatusSucceeded
	finished.Dispatches[0].CostUSD = 2

	if _, err := db.ImportLegacy(k, finished, true); err != nil {
		t.Fatal(err)
	}
	got, err := s.DispatchesForLoop("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Status != StatusSucceeded {
		t.Errorf("status = %q, want the source's outcome to win over the reaper's guess",
			got[0].Status)
	}
}

// An outcome this binary recorded is a fact, not a guess. A refresh must not
// overwrite it.
func TestRefreshLeavesAFinishedRowAlone(t *testing.T) {
	db := openDB(t)
	k := legacyKey(testProject, "/old/planning/state.db")
	data := sampleData("sess-1")

	if _, err := db.ImportLegacy(k, data, false); err != nil {
		t.Fatal(err)
	}
	s := db.Project(testProject)
	ds, _ := s.RunningDispatches("planning", "o/r")
	if err := s.FinishDispatch(ds[0].ID, DispatchResult{
		Status: StatusSucceeded, CostUSD: 9,
	}); err != nil {
		t.Fatal(err)
	}

	stale := data
	stale.Dispatches = []Dispatch{data.Dispatches[0]}
	stale.Dispatches[0].Status = StatusFailed
	stale.Dispatches[0].CostUSD = 0

	if _, err := db.ImportLegacy(k, stale, true); err != nil {
		t.Fatal(err)
	}
	got, _ := s.DispatchesForLoop("planning", "o/r")
	if got[0].Status != StatusSucceeded || got[0].CostUSD != 9 {
		t.Errorf("a refresh overwrote an outcome recorded here: %+v", got[0])
	}
}

// The source's issue row is frozen at import time for every column the old
// runner does not write. A refresh must not drag those stale values over what
// this binary wrote afterwards.
func TestRefreshDoesNotClobberNewerIssueState(t *testing.T) {
	db := openDB(t)
	k := legacyKey(testProject, "/old/planning/state.db")
	data := sampleData("sess-old")
	data.Issues[0].UpdatedAt = time.Now().UTC().Add(-time.Hour)

	if _, err := db.ImportLegacy(k, data, false); err != nil {
		t.Fatal(err)
	}

	// This binary resumed the issue and recorded a new session.
	s := db.Project(testProject)
	if err := s.PutIssueState(IssueState{
		Loop: "planning", Repo: "o/r", Number: 42, SessionID: "sess-new",
		WorktreePath: "/tmp/wt/new", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	// The source still holds the older snapshot.
	if _, err := db.ImportLegacy(k, data, true); err != nil {
		t.Fatal(err)
	}
	got, err := s.IssueState("planning", "o/r", 42)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "sess-new" || got.WorktreePath != "/tmp/wt/new" {
		t.Errorf("a stale source overwrote newer issue state: %+v", got)
	}
}

// A refresh whose source row IS newer carries the four flags the old runner
// writes, and nothing else.
func TestRefreshAppliesNewerRetryFlags(t *testing.T) {
	db := openDB(t)
	k := legacyKey(testProject, "/old/planning/state.db")
	data := sampleData("sess-1")
	if _, err := db.ImportLegacy(k, data, false); err != nil {
		t.Fatal(err)
	}

	newer := data
	newer.Issues = []IssueState{data.Issues[0]}
	newer.Issues[0].NeedsRetry = true
	newer.Issues[0].SessionID = "must-not-be-copied"
	newer.Issues[0].UpdatedAt = time.Now().UTC().Add(time.Minute)

	if _, err := db.ImportLegacy(k, newer, true); err != nil {
		t.Fatal(err)
	}
	got, err := db.Project(testProject).IssueState("planning", "o/r", 42)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NeedsRetry {
		t.Error("the retry flag the old runner wrote was not carried over")
	}
	if got.SessionID != "sess-1" {
		t.Errorf("SessionID = %q; a refresh must copy only the runner's four flags",
			got.SessionID)
	}
}

// Two projects that both claim one file's loop cannot both be right. The second
// is refused rather than silently taking the first one's history.
func TestImportRefusesASourceClaimedByAnotherProject(t *testing.T) {
	db := openDB(t)
	path := "/shared/planning/state.db"

	if _, err := db.ImportLegacy(legacyKey(testProject, path), sampleData("a"), true); err != nil {
		t.Fatal(err)
	}
	_, err := db.ImportLegacy(legacyKey(otherProject, path), sampleData("b"), true)
	if !errors.Is(err, ErrSourceClaimed) {
		t.Errorf("err = %v, want ErrSourceClaimed", err)
	}
}

// A loop whose state_dir is the home directory wrote into this very file. Its
// rows are already here without an owner, so they are stamped, not copied.
func TestImportStampsRowsWhenTheSourceIsThisDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Rows carried over by the schema upgrade belong to no project yet.
	unclaimed := db.Project("")
	if err := unclaimed.PutIssueState(IssueState{
		Loop: "planning", Repo: "o/r", Number: 1, SessionID: "keep", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := unclaimed.PutIssueState(IssueState{
		Loop: "execution", Repo: "o/r", Number: 2, SessionID: "other", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := db.ImportLegacy(legacyKey(testProject, path), LegacyData{}, true)
	if err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}
	if rows != 1 {
		t.Errorf("stamped %d rows, want only the planning row", rows)
	}

	got, err := db.Project(testProject).IssueState("planning", "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "keep" {
		t.Errorf("the planning row was not claimed: %+v", got)
	}
	// The other loop in the same directory keeps waiting for its own project.
	left, err := unclaimed.IssueStates("execution", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Errorf("the execution row was claimed by the wrong loop: %v", left)
	}
}

// An open source is read again. Its recorded state is what the migration uses to
// decide that.
func TestLegacySourceRecordsTheState(t *testing.T) {
	db := openDB(t)
	k := legacyKey(testProject, "/old/planning/state.db")

	if _, err := db.ImportLegacy(k, sampleData("s"), false); err != nil {
		t.Fatal(err)
	}
	row, err := db.LegacySource(k)
	if err != nil {
		t.Fatal(err)
	}
	if !row.ExistsInRecord || row.State != SourceOpen {
		t.Fatalf("state = %q, exists = %v; want an open record", row.State, row.ExistsInRecord)
	}

	if _, err := db.ImportLegacy(k, sampleData("s"), true); err != nil {
		t.Fatal(err)
	}
	row, err = db.LegacySource(k)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != SourceSealed {
		t.Errorf("state = %q, want sealed", row.State)
	}
	if row.FirstImported.After(row.LastImported) {
		t.Error("FirstImported must not move on a refresh")
	}
}
