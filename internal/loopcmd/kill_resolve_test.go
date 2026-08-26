package loopcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/registry"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// --- fixtures for the resolution layer: resolve, resolveBySession,
// resolveByIssue, resolveAll, targetFor, matchDispatch, Kill, Resume ---
//
// None of resolve/resolveBySession/resolveByIssue/resolveAll/targetFor/
// matchDispatch/Kill/Resume had a test that called them directly (spec
// finding A5): every existing test drove either a lower layer (killer.one)
// or a higher one (the resumeAll helper, now resumeTarget via runByLoop).
// These build a real registry + configs-directory + canonical-store fixture,
// the shape internal/listener/work_test.go:178 uses for the analogous
// targetFor resolution.

// registerResolveProject creates a project directory with one loop config
// under configs/, registers it in the registry, and returns the
// .agent-utils directory.
func registerResolveProject(t *testing.T, projectID, name, loop string) (agentUtilsDir string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".agent-utils")
	configs := filepath.Join(dir, "configs")
	if err := os.MkdirAll(configs, 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state", loop)
	body := fmt.Sprintf(killTestYAML, loop, root, filepath.Join(root, "wt", loop), state)
	if err := os.WriteFile(filepath.Join(configs, loop+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(dir, projectID, name); err != nil {
		t.Fatal(err)
	}
	return dir
}

// mustHomeDBPath resolves the canonical state database path under the
// AGENT_UTILS_HOME this test set, matching what openCanonical/Open both use.
func mustHomeDBPath(t *testing.T) string {
	t.Helper()
	if _, err := home.EnsureDir(); err != nil {
		t.Fatal(err)
	}
	p, err := home.StateDBPath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveBySessionFindsTheRightProjectLoopIssue(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	dir := registerResolveProject(t, "proj-a", "demo-a", "planning")

	db, err := store.Open(mustHomeDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	st := db.Project("proj-a")
	if _, err := st.CreateDispatch(store.Dispatch{
		Loop: "planning", Repo: "o/r", Number: 9, Kind: store.KindStart, SessionID: "sess-1",
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	targets, err := resolve(Selector{Session: "sess-1"}, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want exactly one", targets)
	}
	got := targets[0]
	if got.ProjectID != "proj-a" || got.Loop != "planning" || got.Issue != 9 || got.Session != "sess-1" {
		t.Errorf("target = %+v, want project proj-a, loop planning, issue 9, session sess-1", got)
	}
	if got.ConfigPath == "" || got.Dir != dir {
		t.Errorf("target = %+v, want a resolved config path and dir %s", got, dir)
	}
}

func TestResolveAllNarrowedByProject(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	registerResolveProject(t, "proj-a", "demo-a", "planning")
	registerResolveProject(t, "proj-b", "demo-b", "planning")

	db, err := store.Open(mustHomeDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct {
		id     string
		number int
	}{{"proj-a", 1}, {"proj-b", 2}} {
		st := db.Project(p.id)
		if _, err := st.CreateDispatch(store.Dispatch{
			Loop: "planning", Repo: "o/r", Number: p.number, Kind: store.KindStart,
		}); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	targets, err := resolve(Selector{All: true, Project: "demo-a"}, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want exactly one target narrowed to demo-a", targets)
	}
	if targets[0].ProjectID != "proj-a" || targets[0].Issue != 1 {
		t.Errorf("target = %+v, want project-a's issue 1 only", targets[0])
	}
}

// TestKillOnUnresolvableConfigReportsAFailedResultNotAFatalError proves
// targetFor's contract: a target whose ConfigPath could not be resolved (a
// project the registry no longer knows, or a loop config.Resolve cannot
// find) becomes one failed Result per such target through runByLoop, rather
// than a fatal error that would abandon every other target in the batch.
func TestKillOnUnresolvableConfigReportsAFailedResultNotAFatalError(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	goodDir := registerResolveProject(t, "proj-a", "demo-a", "planning")
	goodPath, err := config.Resolve(goodDir, "planning")
	if err != nil {
		t.Fatal(err)
	}

	targets := []Target{
		{ProjectID: "ghost", Loop: "planning", Issue: 1, ConfigPath: ""},
		{ProjectID: "proj-a", Project: "demo-a", Dir: goodDir, Loop: "planning", Issue: 2, ConfigPath: goodPath},
	}

	var ran []int
	results := runByLoop(targets, func(cfg *config.Config, st *store.Store, t Target) Result {
		ran = append(ran, t.Issue)
		return Result{Target: t, Action: ActionSignalled}
	})

	if len(results) != 2 {
		t.Fatalf("results = %+v, want one per target", results)
	}
	var unresolved, resolved Result
	for _, r := range results {
		if r.Target.Issue == 1 {
			unresolved = r
		} else {
			resolved = r
		}
	}
	if unresolved.Action != ActionFailed || unresolved.Err == nil {
		t.Errorf("unresolved target result = %+v, want a failed Result naming the problem", unresolved)
	}
	if resolved.Action != ActionSignalled {
		t.Errorf("resolved target result = %+v, want it to have run", resolved)
	}
	if len(ran) != 1 || ran[0] != 2 {
		t.Errorf("ran = %v, want only the resolvable target's fn to run", ran)
	}
}

// TestResolveByIssueIsNotAmbiguousWhenTheIssueExistsInOnlyOneLoop is spec B9:
// a project with two loop configs must not be treated as ambiguous for an
// issue that only actually exists in one of them. The old resolveByIssue
// enumerated config.List's loop configs rather than rows, so any project
// with 2+ loop configs was rejected as ambiguous regardless of where the
// issue lived -- contradicting spec section 4.1.1 and README.md's promise
// that --loop is needed only when the number matches more than one loop.
func TestResolveByIssueIsNotAmbiguousWhenTheIssueExistsInOnlyOneLoop(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	root := t.TempDir()
	dir := filepath.Join(root, ".agent-utils")
	configs := filepath.Join(dir, "configs")
	if err := os.MkdirAll(configs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, loop := range []string{"planning", "building"} {
		state := filepath.Join(dir, "state", loop)
		body := fmt.Sprintf(killTestYAML, loop, root, filepath.Join(root, "wt", loop), state)
		if err := os.WriteFile(filepath.Join(configs, loop+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.Register(dir, "proj-a", "demo-a"); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(mustHomeDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	st := db.Project("proj-a")
	// Issue 7 exists only in the "planning" loop; "building" has no row for
	// it at all.
	if _, err := st.CreateDispatch(store.Dispatch{
		Loop: "planning", Repo: "o/r", Number: 7, Kind: store.KindStart,
	}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	targets, err := resolve(Selector{Issue: 7, Project: "demo-a"}, false)
	if err != nil {
		t.Fatalf("resolve: %v, want it to resolve unambiguously to the planning loop", err)
	}
	if len(targets) != 1 || targets[0].Loop != "planning" {
		t.Fatalf("targets = %+v, want exactly one target in the planning loop", targets)
	}
}

// TestKillWithNoRunningDispatchStillMarksTheIssueStopped is spec B3: a
// runner that has already crashed leaves no running dispatch row, but the
// trigger label may still be set on GitHub. Kill must set the stopped flag
// in that case too -- not just report ActionAlreadyGone and move on -- or a
// tick racing the crash reads the trigger label with no live dispatch and
// starts a second agent, the exact race the write-before-signal ordering in
// killer.one exists to close for the ordinary path.
func TestKillWithNoRunningDispatchStillMarksTheIssueStopped(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	registerResolveProject(t, "proj-a", "demo-a", "planning")

	// No dispatch row at all: the ordinary "the runner already crashed" case.
	results, err := Kill(KillOptions{Selector: Selector{Issue: 7, Project: "demo-a"}})
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if len(results) != 1 || results[0].Action != ActionAlreadyGone {
		t.Fatalf("results = %+v, want a single ActionAlreadyGone", results)
	}

	db, err := store.Open(mustHomeDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st := db.Project("proj-a")
	got, err := st.IssueState("planning", "o/r", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Stopped {
		t.Error("issue must be marked stopped even though no running dispatch was found")
	}
}
