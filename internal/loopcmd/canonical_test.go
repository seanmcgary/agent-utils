package loopcmd

import (
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/store"
)

const (
	projectA = "11111111-1111-1111-1111-111111111111"
	projectB = "22222222-2222-2222-2222-222222222222"
)

// A session is one conversation across every dispatch that used it. The rows
// arrive newest first, so the first row seen for a session is the one whose
// status, kind and title the summary reports.
func TestSessionsFromSummarisesNewestFirst(t *testing.T) {
	now := time.Now().UTC()
	ds := []store.Dispatch{
		{
			ID: 3, ProjectID: projectA, Loop: "planning", Repo: "o/r", Number: 42,
			Kind: store.KindResume, SessionID: "sess-1", Status: store.StatusSucceeded,
			StartedAt: now, CostUSD: 2, Title: "newest title",
		},
		{
			ID: 2, ProjectID: projectA, Loop: "planning", Repo: "o/r", Number: 42,
			Kind: store.KindStart, SessionID: "sess-1", Status: store.StatusFailed,
			StartedAt: now.Add(-time.Hour), CostUSD: 1, Title: "older title",
		},
	}

	got := sessionsFrom(ds, "")
	if len(got) != 1 {
		t.Fatalf("sessions = %d, want the two dispatches grouped into one", len(got))
	}
	s := got[0]
	if s.Dispatches != 2 {
		t.Errorf("Dispatches = %d, want 2", s.Dispatches)
	}
	if s.Cost != 3 {
		t.Errorf("Cost = %v, want the sum 3", s.Cost)
	}
	if s.LastStatus != store.StatusSucceeded || s.LastKind != store.KindResume {
		t.Errorf("the newest dispatch must set the state: %+v", s)
	}
	if s.Title != "newest title" {
		t.Errorf("Title = %q, want the newest", s.Title)
	}
	if !s.First.Equal(now.Add(-time.Hour)) {
		t.Errorf("First = %v, want the oldest run", s.First)
	}
}

// A dispatch with no session identifier is not a session. The loop filter is
// what `sessions list --name` passes.
func TestSessionsFromFiltersByLoop(t *testing.T) {
	now := time.Now().UTC()
	ds := []store.Dispatch{
		{ProjectID: projectA, Loop: "planning", SessionID: "a", StartedAt: now},
		{ProjectID: projectA, Loop: "execution", SessionID: "b", StartedAt: now},
		{ProjectID: projectA, Loop: "planning", SessionID: "", StartedAt: now},
	}

	got := sessionsFrom(ds, "planning")
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("sessions = %+v, want only the planning session", got)
	}
	if all := sessionsFrom(ds, ""); len(all) != 2 {
		t.Errorf("unfiltered sessions = %d, want the two that have an identifier", len(all))
	}
}

// A running dispatch whose process is gone is an orphan, and the report has to
// say so rather than call it live.
func TestSessionsFromReportsAnOrphan(t *testing.T) {
	ds := []store.Dispatch{{
		ID: 1, ProjectID: projectA, Loop: "planning", SessionID: "a",
		Status: store.StatusRunning, PID: 0, StartedAt: time.Now().UTC(),
	}}

	got := sessionsFrom(ds, "")
	if len(got) != 1 {
		t.Fatalf("sessions = %d, want 1", len(got))
	}
	// pid 0 can never be alive, so the row is an orphan.
	if got[0].Live || !got[0].Orphaned {
		t.Errorf("state = %+v, want orphaned", got[0])
	}
}

// The snapshot is the join between a configuration file and the database. Its
// key is the project AND the loop: a loop name alone exists in more than one
// project, and keying on it would add two projects' numbers together.
func TestSnapshotKeepsProjectsApart(t *testing.T) {
	db := openCanonicalForTest(t)

	for _, p := range []string{projectA, projectB} {
		s := db.Project(p)
		if _, err := s.RecordTick("planning", false, "{}"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateDispatch(store.Dispatch{
			Loop: "planning", Repo: "o/r", Number: 1, Kind: store.KindStart,
		}); err != nil {
			t.Fatal(err)
		}
	}

	snap, err := readSnapshot(db)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{projectA, projectB} {
		k := store.LoopKey{ProjectID: p, Loop: "planning"}
		if got := snap.loops[k].Ticks; got != 1 {
			t.Errorf("project %s has %d ticks, want its own 1", p, got)
		}
		if got := snap.orphans[loopRepo{LoopKey: k, Repo: "o/r"}]; got != 1 {
			t.Errorf("project %s has %d orphans, want its own 1", p, got)
		}
	}
}

// A loop that was pointed at a new repository still holds the old one's
// dispatches. The summary reports the repository the loop watches today.
func TestSnapshotSeparatesRepositories(t *testing.T) {
	db := openCanonicalForTest(t)
	s := db.Project(projectA)

	for _, repo := range []string{"o/old", "o/new"} {
		id, err := s.CreateDispatch(store.Dispatch{
			Loop: "planning", Repo: repo, Number: 1, Kind: store.KindStart,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishDispatch(id, store.DispatchResult{
			Status: store.StatusSucceeded, CostUSD: 5,
		}); err != nil {
			t.Fatal(err)
		}
	}

	snap, err := readSnapshot(db)
	if err != nil {
		t.Fatal(err)
	}
	st := snap.loops[store.LoopKey{ProjectID: projectA, Loop: "planning"}]
	if st.CostByRepo["o/new"] != 5 {
		t.Errorf("cost for the current repository = %v, want 5", st.CostByRepo["o/new"])
	}
	if st.Cost != 10 {
		t.Errorf("total cost = %v, want both repositories' 10", st.Cost)
	}
}

func openCanonicalForTest(t *testing.T) *store.DB {
	t.Helper()
	t.Setenv("AGENT_UTILS_HOME", t.TempDir())
	db, err := openCanonical()
	if err != nil {
		t.Fatalf("openCanonical: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
