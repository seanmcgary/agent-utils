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

// A closure is a fact about {project, repo, number}, so it marks EVERY session
// that ever worked the issue -- not only the newest one, which is what
// applyStopped does.
func TestApplyClosedMarksEverySessionOfTheIssue(t *testing.T) {
	sessions := []Session{
		{ID: "old", ProjectID: projectA, Repo: "o/r", Issue: 7, Last: time.Now().Add(-time.Hour)},
		{ID: "new", ProjectID: projectA, Repo: "o/r", Issue: 7, Last: time.Now()},
		{ID: "other-issue", ProjectID: projectA, Repo: "o/r", Issue: 8},
	}
	applyClosed(sessions, closedSet([]store.Closure{
		{ProjectID: projectA, Repo: "o/r", Number: 7},
	}, ""))

	if !sessions[0].Closed || !sessions[1].Closed {
		t.Errorf("both sessions of the closed issue must be marked: %+v", sessions[:2])
	}
	if sessions[2].Closed {
		t.Errorf("a different issue must not be marked: %+v", sessions[2])
	}
}

// The same number in another repository, or another project, is another issue.
func TestApplyClosedIsKeyedByProjectAndRepo(t *testing.T) {
	sessions := []Session{
		{ID: "same", ProjectID: projectA, Repo: "o/r", Issue: 7},
		{ID: "other-repo", ProjectID: projectA, Repo: "o/other", Issue: 7},
		{ID: "other-project", ProjectID: projectB, Repo: "o/r", Issue: 7},
	}
	applyClosed(sessions, closedSet([]store.Closure{
		{ProjectID: projectA, Repo: "o/r", Number: 7},
	}, ""))

	if !sessions[0].Closed {
		t.Error("the closed issue's session must be marked")
	}
	if sessions[1].Closed || sessions[2].Closed {
		t.Errorf("only the matching repo and project may be marked: %+v", sessions[1:])
	}
}

// An issue nothing has reported on stays visible. The closures table is written
// only by the listener, so a machine that has never run the daemon must see the
// report it saw before.
func TestApplyClosedLeavesUnknownIssuesVisible(t *testing.T) {
	sessions := []Session{{ID: "a", ProjectID: projectA, Repo: "o/r", Issue: 7}}
	applyClosed(sessions, closedSet(nil, ""))
	if sessions[0].Closed {
		t.Error("an issue with no closure row must not be marked closed")
	}
}

// closedSet narrows to one project, for the per-project report that has no
// project column to tell two projects' rows apart.
func TestClosedSetNarrowsToOneProject(t *testing.T) {
	set := closedSet([]store.Closure{
		{ProjectID: projectA, Repo: "o/r", Number: 1},
		{ProjectID: projectB, Repo: "o/r", Number: 2},
	}, projectA)

	if !set[closedKey{ProjectID: projectA, Repo: "o/r", Number: 1}] {
		t.Error("the named project's closure must survive")
	}
	if set[closedKey{ProjectID: projectB, Repo: "o/r", Number: 2}] {
		t.Error("another project's closure must be dropped")
	}
}

func closedAndOpen() []Session {
	return []Session{
		{ID: "sess-open", Project: "alpha", Loop: "planning", Issue: 1,
			Dispatches: 1, LastStatus: store.StatusSucceeded},
		{ID: "sess-closed", Project: "alpha", Loop: "planning", Issue: 2,
			Dispatches: 1, LastStatus: store.StatusSucceeded, Closed: true},
	}
}

func TestRenderSessionsHidesClosedUntilAsked(t *testing.T) {
	p := &Project{Config: &projectConfigStub, Root: "/p", Dir: "/p/.agent-utils"}

	def := RenderSessions(p, closedAndOpen(), false)
	if strings.Contains(def, "sess-closed") {
		t.Errorf("a closed issue's session must be hidden by default:\n%s", def)
	}
	if !strings.Contains(def, "sess-open") {
		t.Errorf("an open issue's session must still be listed:\n%s", def)
	}
	if !strings.Contains(def, "1 closed session hidden (--all to show)") {
		t.Errorf("the footer must say what it hid, and how to see it:\n%s", def)
	}

	all := RenderSessions(p, closedAndOpen(), true)
	if !strings.Contains(all, "sess-closed") {
		t.Errorf("--all must show the closed issue's session:\n%s", all)
	}
	if !strings.Contains(all, "CLOSED") {
		t.Errorf("--all must mark the row CLOSED:\n%s", all)
	}
	if strings.Contains(all, "hidden") {
		t.Errorf("--all hides nothing, so it must not report a hidden count:\n%s", all)
	}
}

func TestRenderAllSessionsHidesClosedUntilAsked(t *testing.T) {
	def := RenderAllSessions(closedAndOpen(), SessionFilter{})
	if strings.Contains(def, "sess-closed") {
		t.Errorf("a closed issue's session must be hidden by default:\n%s", def)
	}
	if !strings.Contains(def, "1 closed session hidden (--all to show)") {
		t.Errorf("the footer must say what it hid:\n%s", def)
	}

	all := RenderAllSessions(closedAndOpen(), SessionFilter{All: true})
	if !strings.Contains(all, "sess-closed") || !strings.Contains(all, "CLOSED") {
		t.Errorf("--all must show the closed session, marked CLOSED:\n%s", all)
	}
}

// A live agent on a closed issue still reads as running: the state column
// reports the AGENT, and a runaway agent is the thing an operator most needs to
// see.
func TestClosedRanksBelowTheAgentStates(t *testing.T) {
	out := RenderAllSessions([]Session{
		{ID: "sess-live", Project: "alpha", Loop: "planning", Issue: 1,
			LastStatus: store.StatusRunning, Live: true, Closed: true},
		{ID: "sess-orphan", Project: "alpha", Loop: "planning", Issue: 2,
			LastStatus: store.StatusRunning, Orphaned: true, Closed: true},
	}, SessionFilter{All: true})

	if !strings.Contains(out, "running") {
		t.Errorf("a live agent on a closed issue must read as running:\n%s", out)
	}
	if !strings.Contains(out, "ORPHANED") {
		t.Errorf("an orphan on a closed issue must read as ORPHANED:\n%s", out)
	}
}

// Every session hidden is not the same state as no session at all, and the two
// are fixed by different things: one by --all, the other by starting a loop.
func TestAnAllClosedReportSaysSoRatherThanReadingEmpty(t *testing.T) {
	only := []Session{{ID: "sess-closed", Project: "alpha", Loop: "planning",
		Issue: 2, LastStatus: store.StatusSucceeded, Closed: true}}

	machine := RenderAllSessions(only, SessionFilter{})
	if !strings.Contains(machine, "No open sessions.") ||
		!strings.Contains(machine, "--all to show") {
		t.Errorf("an all-closed machine report must explain itself:\n%s", machine)
	}
	if strings.Contains(machine, "No sessions yet") {
		t.Errorf("hidden work is not an empty machine:\n%s", machine)
	}

	p := &Project{Config: &projectConfigStub, Root: "/p", Dir: "/p/.agent-utils"}
	perProject := RenderSessions(p, only, false)
	if !strings.Contains(perProject, "No open sessions.") ||
		!strings.Contains(perProject, "--all to show") {
		t.Errorf("an all-closed project report must explain itself:\n%s", perProject)
	}
	if strings.Contains(perProject, "No sessions yet") {
		t.Errorf("hidden work is not an empty project:\n%s", perProject)
	}
}

// The whole path, against a real database: a closure written by the store
// reaches the session the report renders.
func TestAllSessionsMarksClosedFromTheDatabase(t *testing.T) {
	isolate(t)

	dir := filepath.Join(t.TempDir(), config.DirName)
	if err := registry.Register(dir, projectA, "alpha"); err != nil {
		t.Fatalf("registry.Register: %v", err)
	}

	db, err := openCanonical()
	if err != nil {
		t.Fatalf("openCanonical: %v", err)
	}
	s := db.Project(projectA)
	for _, n := range []int{1, 2} {
		if _, err := s.CreateDispatch(store.Dispatch{
			Loop: "planning", Repo: "o/r", Number: n, Kind: store.KindStart,
			SessionID: sessionIDFor(n), StartedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("CreateDispatch: %v", err)
		}
	}
	if err := s.MarkClosed("o/r", 2, time.Now()); err != nil {
		t.Fatalf("MarkClosed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := AllSessions(SessionFilter{})
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("sessions = %d, want 2 -- AllSessions marks, it does not hide", len(got))
	}
	byIssue := map[int]Session{}
	for _, sess := range got {
		byIssue[sess.Issue] = sess
	}
	if byIssue[1].Closed {
		t.Error("the open issue's session must not be marked closed")
	}
	if !byIssue[2].Closed {
		t.Error("the closed issue's session must be marked closed")
	}
	if byIssue[2].Repo != "o/r" {
		t.Errorf("Repo = %q, want the dispatch row's o/r", byIssue[2].Repo)
	}
}

func sessionIDFor(n int) string {
	return string(rune('a'+n)) + "-sess"
}

// A live session survives its issue's closure.
//
// The closure marks every session that ever worked the issue, so an agent that
// is running RIGHT NOW on an issue somebody closed under it gets marked too.
// Hiding that row hides the one session an operator can still act on -- it is
// burning tokens, and `sessions kill` needs its identifier -- so the default
// report keeps it and the hidden count does not count it.
func TestVisibleKeepsALiveSessionOnAClosedIssue(t *testing.T) {
	sessions := []Session{
		{ID: "sess-live", Issue: 9, Closed: true, Live: true},
		{ID: "sess-done", Issue: 9, Closed: true},
	}

	shown, hidden := visible(sessions, false)
	if len(shown) != 1 || shown[0].ID != "sess-live" {
		t.Fatalf("the live session must survive the closure: %+v", shown)
	}
	if hidden != 1 {
		t.Errorf("only the finished session is hidden, so the count is 1, got %d", hidden)
	}
}

// The renderers show it, and show it as running rather than CLOSED.
func TestRenderSessionsShowsALiveSessionOnAClosedIssue(t *testing.T) {
	p := &Project{Config: &projectConfigStub, Root: "/p", Dir: "/p/.agent-utils"}
	live := []Session{{ID: "sess-live", Loop: "execution", Issue: 9,
		Dispatches: 2, LastStatus: store.StatusRunning, Closed: true, Live: true}}

	for name, got := range map[string]string{
		"RenderSessions":    RenderSessions(p, live, false),
		"RenderAllSessions": RenderAllSessions(live, SessionFilter{}),
	} {
		if !strings.Contains(got, "sess-live") {
			t.Errorf("%s must list the running session:\n%s", name, got)
		}
		if !strings.Contains(got, "running") {
			t.Errorf("%s must mark it running, not CLOSED:\n%s", name, got)
		}
		if strings.Contains(got, "hidden") {
			t.Errorf("%s hid nothing, so it must report no hidden count:\n%s", name, got)
		}
	}
}
