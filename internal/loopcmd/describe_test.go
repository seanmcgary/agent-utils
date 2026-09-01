package loopcmd

import (
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/project"
	"github.com/seanmcgary/agent-utils/internal/store"
)

func at(hour, min int) time.Time {
	return time.Date(2026, 8, 31, hour, min, 0, 0, time.UTC)
}

// The report exists to answer "what happened to this session" in one command.
// The header identifies the session and the runs account for it, newest last
// so the story reads in the order it happened.
func TestRenderSessionDetailShowsTheHeaderAndEveryRun(t *testing.T) {
	out := RenderSessionDetail(SessionDetail{
		Session: Session{
			ID: "369ff362", Project: "lawndominator", Loop: "execution",
			Issue: 183, Title: "feat: add to shed UI refresh",
			Harness: "pi", Model: "deepseek/deepseek-v4-flash-0731",
		},
		PR: 186,
		Runs: []SessionRun{
			{Dispatch: store.Dispatch{ID: 110, Kind: store.KindStart, Status: store.StatusSucceeded,
				CostUSD: 0.43, DurationMS: 8880000, StartedAt: at(3, 21)}},
			{Dispatch: store.Dispatch{ID: 118, Kind: store.KindTend, Status: store.StatusFailed,
				APIError:  "No conversation found with session ID: 369ff362",
				StartedAt: at(14, 51)}},
		},
	})

	for _, want := range []string{
		"369ff362", "lawndominator", "execution", "#183",
		"feat: add to shed UI refresh", "pi", "deepseek/deepseek-v4-flash-0731", "186",
		"110", "start", "succeeded", "$0.43",
		"118", "tend", "failed", "No conversation found with session ID: 369ff362",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q:\n%s", want, out)
		}
	}
}

// The runs read oldest first: this is a history, and a failure means more when
// you can see the run it followed.
func TestRenderSessionDetailOrdersRunsOldestFirst(t *testing.T) {
	out := RenderSessionDetail(SessionDetail{
		Session: Session{ID: "s", Loop: "execution", Issue: 1},
		Runs: []SessionRun{
			{Dispatch: store.Dispatch{ID: 110, Kind: store.KindStart, Status: store.StatusSucceeded, StartedAt: at(3, 21)}},
			{Dispatch: store.Dispatch{ID: 118, Kind: store.KindTend, Status: store.StatusFailed, StartedAt: at(14, 51)}},
		},
	})
	if strings.Index(out, "110") > strings.Index(out, "118") {
		t.Errorf("runs must read oldest first:\n%s", out)
	}
}

// The line that would have saved the investigation: a session failing the same
// way every run is a stuck loop, not three unrelated accidents.
func TestRenderSessionDetailCallsOutARepeatedFailure(t *testing.T) {
	same := "No conversation found with session ID: s"
	out := RenderSessionDetail(SessionDetail{
		Session: Session{ID: "s", Loop: "execution", Issue: 183},
		Runs: []SessionRun{
			{Dispatch: store.Dispatch{ID: 110, Kind: store.KindStart, Status: store.StatusSucceeded, StartedAt: at(3, 21)}},
			{Dispatch: store.Dispatch{ID: 118, Kind: store.KindTend, Status: store.StatusFailed,
				APIError: same, StartedAt: at(14, 51)}},
			{Dispatch: store.Dispatch{ID: 123, Kind: store.KindTend, Status: store.StatusFailed,
				APIError: same, StartedAt: at(15, 34)}},
		},
	})
	if !strings.Contains(out, "2 of 3 runs failed") {
		t.Errorf("the failure count must be summarised:\n%s", out)
	}
	if !strings.Contains(out, "same error") {
		t.Errorf("a repeated identical failure must be called out:\n%s", out)
	}
}

// Distinct failures must NOT be reported as one recurring problem: that would
// send an operator looking for a single cause that does not exist.
func TestRenderSessionDetailDoesNotClaimDistinctFailuresAreTheSame(t *testing.T) {
	out := RenderSessionDetail(SessionDetail{
		Session: Session{ID: "s", Loop: "execution", Issue: 1},
		Runs: []SessionRun{
			{Dispatch: store.Dispatch{ID: 1, Kind: store.KindStart, Status: store.StatusFailed,
				APIError: "529", StartedAt: at(3, 21)}},
			{Dispatch: store.Dispatch{ID: 2, Kind: store.KindStart, Status: store.StatusFailed,
				APIError: "rebase conflict", StartedAt: at(4, 21)}},
		},
	})
	if strings.Contains(out, "same error") {
		t.Errorf("two different errors must not be reported as one:\n%s", out)
	}
}

// A session whose runs all succeeded needs no failure summary at all.
func TestRenderSessionDetailStaysQuietWhenNothingFailed(t *testing.T) {
	out := RenderSessionDetail(SessionDetail{
		Session: Session{ID: "s", Loop: "planning", Issue: 1},
		Runs: []SessionRun{
			{Dispatch: store.Dispatch{ID: 1, Kind: store.KindStart, Status: store.StatusSucceeded, StartedAt: at(3, 21)}},
		},
	})
	if strings.Contains(out, "failed") {
		t.Errorf("a clean session must not mention failure:\n%s", out)
	}
}

// A dispatch that failed before anything could describe it still has an exit
// code, and the report must say so rather than leaving the run unexplained.
func TestRenderSessionDetailReportsAFailureWithNoRecordedReason(t *testing.T) {
	out := RenderSessionDetail(SessionDetail{
		Session: Session{ID: "s", Loop: "execution", Issue: 1},
		Runs: []SessionRun{
			{Dispatch: store.Dispatch{ID: 9, Kind: store.KindTend, Status: store.StatusFailed,
				ExitCode: 1, StartedAt: at(3, 21)}},
		},
	})
	if !strings.Contains(out, "exit 1") {
		t.Errorf("a reasonless failure must still report its exit code:\n%s", out)
	}
}

// A running dispatch is not a failure and not a result: it is still going.
func TestRenderSessionDetailShowsARunningDispatch(t *testing.T) {
	out := RenderSessionDetail(SessionDetail{
		Session: Session{ID: "s", Loop: "execution", Issue: 1},
		Runs: []SessionRun{
			{Dispatch: store.Dispatch{ID: 9, Kind: store.KindStart, Status: store.StatusRunning, StartedAt: at(3, 21)}},
		},
	})
	if !strings.Contains(out, "running") {
		t.Errorf("a live run must be shown as running:\n%s", out)
	}
}

// The report names the command that shows a run's full log, so the next step
// after reading it needs no guessing.
func TestRenderSessionDetailPointsAtTheLogs(t *testing.T) {
	out := RenderSessionDetail(SessionDetail{
		Session: Session{ID: "sess-a", Loop: "execution", Issue: 1},
		Runs: []SessionRun{
			{Dispatch: store.Dispatch{ID: 9, Kind: store.KindTend, Status: store.StatusFailed, StartedAt: at(3, 21)}},
		},
	})
	if !strings.Contains(out, "logs") {
		t.Errorf("the report must name the command for a full log:\n%s", out)
	}
}

// The header must describe the run that CREATED the session, not the newest
// one. Session.Model/Harness carry the newest dispatch's settings, and a tend
// carrying no overrides resolves to the loop's defaults -- so a session whose
// work ran under pi reported "claude (opus)", which is the opposite of the
// fact an operator opens this report to learn.
func TestRenderSessionDetailReportsTheCreatingRunsHarness(t *testing.T) {
	out := RenderSessionDetail(SessionDetail{
		Session: Session{ID: "s", Loop: "execution", Issue: 183,
			Harness: "claude", Model: "opus"},
		Runs: []SessionRun{
			{Dispatch: store.Dispatch{ID: 110, Kind: store.KindStart,
				Status: store.StatusSucceeded, StartedAt: at(3, 21)},
				Harness: "pi", Model: "deepseek/deepseek-v4-flash-0731"},
			{Dispatch: store.Dispatch{ID: 118, Kind: store.KindTend,
				Status: store.StatusFailed, StartedAt: at(14, 51)},
				Harness: "claude", Model: "opus"},
		},
	})
	if !strings.Contains(out, "pi (deepseek/deepseek-v4-flash-0731)") {
		t.Errorf("the header must name the harness that created the session:\n%s", out)
	}
}

// A later run under a different harness is the diagnosis, not a detail: a
// session id means nothing to a harness that did not mint it, so this is the
// shape of a loop that cannot resume its own work.
func TestRenderSessionDetailFlagsARunUnderADifferentHarness(t *testing.T) {
	out := RenderSessionDetail(SessionDetail{
		Session: Session{ID: "s", Loop: "execution", Issue: 183},
		Runs: []SessionRun{
			{Dispatch: store.Dispatch{ID: 110, Kind: store.KindStart,
				Status: store.StatusSucceeded, StartedAt: at(3, 21)}, Harness: "pi"},
			{Dispatch: store.Dispatch{ID: 118, Kind: store.KindTend,
				Status: store.StatusFailed, StartedAt: at(14, 51)}, Harness: "claude"},
		},
	})
	if !strings.Contains(out, "harness") || !strings.Contains(out, "claude") {
		t.Errorf("a harness change across runs must be called out:\n%s", out)
	}
}

// Runs that all ran the same way need no such note.
func TestRenderSessionDetailStaysQuietWhenTheHarnessNeverChanged(t *testing.T) {
	out := RenderSessionDetail(SessionDetail{
		Session: Session{ID: "s", Loop: "execution", Issue: 1},
		Runs: []SessionRun{
			{Dispatch: store.Dispatch{ID: 1, Kind: store.KindStart,
				Status: store.StatusSucceeded, StartedAt: at(3, 21)}, Harness: "pi"},
			{Dispatch: store.Dispatch{ID: 2, Kind: store.KindTend,
				Status: store.StatusSucceeded, StartedAt: at(4, 21)}, Harness: "pi"},
		},
	})
	if strings.Contains(out, "ran under") {
		t.Errorf("an unchanged harness must not be reported as a change:\n%s", out)
	}
}

// --- the collection layer, over a real registry + canonical store ---

// The report must gather EVERY run of the session, not the newest one the way
// `logs --session` selects. That difference is the whole point: a session with
// one good run and two identical failures reads as healthy in every other view.
func TestDescribeSessionCollectsEveryRunAndThePullRequest(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	dir := registerResolveProject(t, "proj-a", "demo-a", "execution")
	// registerResolveProject writes the loop config and the registry entry.
	// Resolving a *Project also needs the project descriptor.
	if err := project.Save(dir, &project.Config{Name: "demo-a", ID: "proj-a"}); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(mustHomeDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	st := db.Project("proj-a")
	// Each row is finished before the next is created. These are three runs of
	// one issue OVER TIME, which is the situation the report describes, and
	// store.CreateDispatch refuses to open a second live dispatch across the
	// tend/loop divide -- so leaving them all running would be asking the store
	// to record the very state it exists to prevent.
	var prev int64
	for _, d := range []store.Dispatch{
		{Loop: "execution", Repo: "o/r", Number: 9, Kind: store.KindStart, SessionID: "sess-1"},
		{Loop: "execution", Repo: "o/r", Number: 9, Kind: store.KindTend,
			SessionID: "sess-1", PRNumber: 186},
		{Loop: "execution", Repo: "o/r", Number: 9, Kind: store.KindStart, SessionID: "other"},
	} {
		if prev != 0 {
			if err := st.FinishDispatch(prev, store.DispatchResult{
				Status: store.StatusSucceeded,
			}); err != nil {
				t.Fatal(err)
			}
		}
		id, err := st.CreateDispatch(d)
		if err != nil {
			t.Fatal(err)
		}
		prev = id
	}
	db.Close()

	p, err := ResolveProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	sd, err := DescribeSession(p, "sess-1")
	if err != nil {
		t.Fatalf("DescribeSession: %v", err)
	}
	if len(sd.Runs) != 2 {
		t.Errorf("Runs = %d, want the session's two runs and not the third", len(sd.Runs))
	}
	if sd.PR != 186 {
		t.Errorf("PR = %d, want 186 from the tend run", sd.PR)
	}
	if sd.Session.Project != "demo-a" {
		t.Errorf("Project = %q, want the project name in the header", sd.Session.Project)
	}
}

// The top-level report finds the session's project itself. An operator reading
// the machine-wide `sessions list` has an id and no idea which project owns
// it, and being made to supply one would defeat the table they read it from.
func TestDescribeSessionAnywhereFindsTheOwningProject(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	dirA := registerResolveProject(t, "proj-a", "demo-a", "execution")
	dirB := registerResolveProject(t, "proj-b", "demo-b", "execution")
	for _, d := range []struct{ dir, name, id string }{
		{dirA, "demo-a", "proj-a"}, {dirB, "demo-b", "proj-b"},
	} {
		if err := project.Save(d.dir, &project.Config{Name: d.name, ID: d.id}); err != nil {
			t.Fatal(err)
		}
	}

	db, err := store.Open(mustHomeDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Project("proj-b").CreateDispatch(store.Dispatch{
		Loop: "execution", Repo: "o/r", Number: 9, Kind: store.KindStart, SessionID: "sess-b",
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	sd, err := DescribeSessionAnywhere("", "sess-b")
	if err != nil {
		t.Fatalf("DescribeSessionAnywhere: %v", err)
	}
	if sd.Session.Project != "demo-b" {
		t.Errorf("Project = %q, want demo-b: the session belongs to the second project",
			sd.Session.Project)
	}
	if len(sd.Runs) != 1 {
		t.Errorf("Runs = %d, want 1", len(sd.Runs))
	}
}

// An id that exists nowhere on the machine must say so, not name one project
// it happened to look in first.
func TestDescribeSessionAnywhereReportsAnUnknownSession(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	dir := registerResolveProject(t, "proj-a", "demo-a", "execution")
	if err := project.Save(dir, &project.Config{Name: "demo-a", ID: "proj-a"}); err != nil {
		t.Fatal(err)
	}

	if _, err := DescribeSessionAnywhere("", "nope"); err == nil ||
		!strings.Contains(err.Error(), "nope") {
		t.Errorf("err = %v, want it to name the missing session", err)
	}
}

// A session id that does not exist must say so plainly. An operator reaches
// this command with an id copied from somewhere, and a typo is the likeliest
// reason it finds nothing.
func TestDescribeSessionReportsAnUnknownSessionClearly(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	dir := registerResolveProject(t, "proj-a", "demo-a", "execution")
	// registerResolveProject writes the loop config and the registry entry.
	// Resolving a *Project also needs the project descriptor.
	if err := project.Save(dir, &project.Config{Name: "demo-a", ID: "proj-a"}); err != nil {
		t.Fatal(err)
	}

	p, err := ResolveProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DescribeSession(p, "nope"); err == nil ||
		!strings.Contains(err.Error(), "nope") {
		t.Errorf("err = %v, want it to name the missing session", err)
	}
}

// A session with no dispatches is a real state -- a row written before the
// first run finished -- and must read as empty rather than as a broken table.
func TestRenderSessionDetailExplainsASessionWithNoRuns(t *testing.T) {
	out := RenderSessionDetail(SessionDetail{
		Session: Session{ID: "s", Loop: "planning", Issue: 1},
	})
	if !strings.Contains(out, "no runs") {
		t.Errorf("an empty session must say so:\n%s", out)
	}
}
