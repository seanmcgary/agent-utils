package loopcmd

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/registry"
	"github.com/seanmcgary/agent-utils/internal/store"
)

const (
	projectA = "11111111-1111-1111-1111-111111111111"
	projectB = "22222222-2222-2222-2222-222222222222"
	projectC = "33333333-3333-3333-3333-333333333333"
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

// Two projects can hold the same session identifier: nothing in the schema
// makes one unique across projects, and a copied worktree or an imported
// legacy source reproduces one. Keying on the identifier alone would merge
// them, and the machine-wide report would show one project's runs and cost
// under the other project's name.
func TestSessionsFromKeepsTwoProjectsApart(t *testing.T) {
	now := time.Now().UTC()
	ds := []store.Dispatch{
		{
			ID: 2, ProjectID: projectA, Loop: "planning", SessionID: "shared",
			Status: store.StatusSucceeded, StartedAt: now, CostUSD: 1,
		},
		{
			ID: 1, ProjectID: projectB, Loop: "planning", SessionID: "shared",
			Status: store.StatusSucceeded, StartedAt: now.Add(-time.Hour), CostUSD: 2,
		},
	}

	got := sessionsFrom(ds, "")
	if len(got) != 2 {
		t.Fatalf("sessions = %d, want 2", len(got))
	}
	if got[0].ProjectID != projectA {
		t.Errorf("got[0].ProjectID = %q, want %q", got[0].ProjectID, projectA)
	}
	if got[1].ProjectID != projectB {
		t.Errorf("got[1].ProjectID = %q, want %q", got[1].ProjectID, projectB)
	}
	for _, s := range got {
		if s.ID != "shared" {
			t.Errorf("ID = %q, want %q", s.ID, "shared")
		}
		if s.Dispatches != 1 {
			t.Errorf("project %s has Dispatches = %d, want its own 1", s.ProjectID, s.Dispatches)
		}
	}
	if got[0].Cost != 1 {
		t.Errorf("got[0].Cost = %v, want 1", got[0].Cost)
	}
	if got[1].Cost != 2 {
		t.Errorf("got[1].Cost = %v, want 2", got[1].Cost)
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

// The state flags are a selection, not a chain of independent filters. Neither
// flag means every state, and both flags together mean the union: a naive AND
// would answer `sessions list --running --orphaned` with nothing at all, since
// no session is both live and orphaned.
func TestKeepStateSelectsTheRequestedStates(t *testing.T) {
	liveSession := Session{Live: true}
	orphanSession := Session{Orphaned: true}
	doneSession := Session{}

	cases := []struct {
		name         string
		running      bool
		orphaned     bool
		wantLive     bool
		wantOrphan   bool
		wantFinished bool
	}{
		{"neither flag keeps every state", false, false, true, true, true},
		{"running alone keeps the live session", true, false, true, false, false},
		{"orphaned alone keeps the orphan", false, true, false, true, false},
		{"both flags keep their union", true, true, true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keepState(liveSession, tc.running, tc.orphaned); got != tc.wantLive {
				t.Errorf("keepState(live, %v, %v) = %v, want %v",
					tc.running, tc.orphaned, got, tc.wantLive)
			}
			if got := keepState(orphanSession, tc.running, tc.orphaned); got != tc.wantOrphan {
				t.Errorf("keepState(orphaned, %v, %v) = %v, want %v",
					tc.running, tc.orphaned, got, tc.wantOrphan)
			}
			if got := keepState(doneSession, tc.running, tc.orphaned); got != tc.wantFinished {
				t.Errorf("keepState(finished, %v, %v) = %v, want %v",
					tc.running, tc.orphaned, got, tc.wantFinished)
			}
		})
	}
}

// A session the registry cannot name still belongs in the machine-wide report.
// Two different things can go unnamed and they must not read the same: a
// forgotten project keeps its identifier, so a short id is enough to tell its
// rows apart and to look it up, while a pre-project row has no identifier at
// all and no project selector can ever reach it. Printing an empty column for
// either one would make the rows look like a rendering bug.
//
// The last case is why this tests the NAME and not the map key. A project
// registered before it had a descriptor is in the registry with an empty name,
// which RenderProjects already handles with its own (unnamed) fallback. Keyed
// on map presence, such a project would take the naming branch and render a
// blank column.
func TestNameProjectsMarksForgottenAndUnclaimedProjects(t *testing.T) {
	sessions := []Session{
		{ProjectID: projectA},
		{ProjectID: projectB},
		{ProjectID: "abc"},
		{ProjectID: ""},
		{ProjectID: projectC},
	}

	nameProjects(sessions, map[string]string{projectA: "lawndominator", projectC: ""})

	want := []string{"lawndominator", projectB[:8], "abc", "(unclaimed)", projectC[:8]}
	for i, w := range want {
		if got := sessions[i].Project; got != w {
			t.Errorf("sessions[%d].Project = %q, want %q", i, got, w)
		}
	}
}

// This is the only test that proves --project filters on the project id the
// selector resolved to and not on the raw selector. Filtering on the selector
// would match no dispatch row at all, because a dispatch records the id.
func TestAllSessionsRestrictsToOneProject(t *testing.T) {
	isolate(t)

	for _, p := range []struct{ id, name string }{{projectA, "alpha"}, {projectB, "beta"}} {
		dir := filepath.Join(t.TempDir(), config.DirName)
		if err := registry.Register(dir, p.id, p.name); err != nil {
			t.Fatalf("registry.Register(%s): %v", p.name, err)
		}
	}

	db, err := openCanonical()
	if err != nil {
		t.Fatalf("openCanonical: %v", err)
	}
	for _, p := range []struct{ id, session string }{{projectA, "sess-a"}, {projectB, "sess-b"}} {
		if _, err := db.Project(p.id).CreateDispatch(store.Dispatch{
			Loop: "planning", Repo: "o/r", Number: 1,
			Kind: store.KindStart, SessionID: p.session,
		}); err != nil {
			t.Fatalf("CreateDispatch(%s): %v", p.id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := AllSessions(SessionFilter{Project: "alpha"})
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("sessions = %d, want only the one project's 1", len(got))
	}
	if got[0].ID != "sess-a" {
		t.Errorf("ID = %q, want %q", got[0].ID, "sess-a")
	}
	if got[0].ProjectID != projectA {
		t.Errorf("ProjectID = %q, want %q", got[0].ProjectID, projectA)
	}
	if got[0].Project != "alpha" {
		t.Errorf("Project = %q, want %q", got[0].Project, "alpha")
	}

	all, err := AllSessions(SessionFilter{})
	if err != nil {
		t.Fatalf("AllSessions unfiltered: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("unfiltered sessions = %d, want both projects' 2", len(all))
	}
}

// A registry entry is allowed to hold an empty id, and a project scoped to one
// would report no sessions for a project that has many. Refusing is the only
// answer that does not lie about the state.
func TestAllSessionsRejectsAProjectWithNoIdentifier(t *testing.T) {
	isolate(t)

	dir := filepath.Join(t.TempDir(), config.DirName)
	if err := registry.Register(dir, "", "nameless"); err != nil {
		t.Fatalf("registry.Register: %v", err)
	}

	_, err := AllSessions(SessionFilter{Project: "nameless"})
	if err == nil {
		t.Fatal("AllSessions for a project with no id: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "project init") {
		t.Errorf("error = %q, want it to name `agent-utils project init`", err.Error())
	}
}

// The renderer picks its empty-list text from this, and the two texts point at
// different actions: nothing has run yet sends the operator to `agent-utils
// list`, while nothing matched sends it back to its own flags. Any field going
// unread here would print the wrong advice.
func TestSessionFilterKnowsWhenItNarrowsTheReport(t *testing.T) {
	if (SessionFilter{}).filtered() {
		t.Error("empty SessionFilter.filtered() = true, want false")
	}
	cases := map[string]SessionFilter{
		"project":  {Project: "alpha"},
		"loop":     {Loop: "planning"},
		"running":  {Running: true},
		"orphaned": {Orphaned: true},
	}
	for name, f := range cases {
		if !f.filtered() {
			t.Errorf("SessionFilter{%s}.filtered() = false, want true", name)
		}
	}
}
