package listener

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/registry"
)

// minimalConfig returns a valid loop configuration body for name and repo.
// route.go only reads Name and Repo off the loaded config, but config.Load
// validates the whole file, so a fixture that skipped the other required
// fields would fail to load before Targets ever got a chance to look at
// Repo.
func minimalConfig(name, repo string) string {
	return "" +
		"name: " + name + "\n" +
		"repo: " + repo + "\n" +
		"checkout_base_dir: /tmp/checkout\n" +
		"worktree_dir: /tmp/worktrees\n" +
		"default_branch: master\n" +
		"labels:\n" +
		"  trigger: status:ready\n" +
		"  in_flight: status:in-flight\n" +
		"  blocked: status:blocked\n" +
		"  review: status:review\n" +
		"agent:\n" +
		"  model: opus\n" +
		"  worktree: per_issue\n" +
		"  timeout: 1h\n" +
		"retry:\n" +
		"  breaker:\n" +
		"    orphan_threshold: 1\n" +
		"    cooldown: 5m\n" +
		"prompt: do the thing\n" +
		"resume_prompt: resume\n"
}

// setHome points $AGENT_UTILS_HOME at a fresh temporary directory, so the
// registry and every project directory this test creates are isolated from
// the real machine.
func setHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_UTILS_HOME", dir)
	return dir
}

// newProject creates and registers a project's .agent-utils directory. It
// does NOT create a configs subdirectory -- callers that need loops call
// writeLoop, and a caller that does not is exercising the no-configs-dir
// case on purpose.
func newProject(t *testing.T, home, name string) (id, dir string) {
	t.Helper()
	root := filepath.Join(home, "projects", name)
	dir = filepath.Join(root, config.DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id = "id-" + name
	if err := registry.Register(dir, id, name); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return id, dir
}

// writeLoop writes one loop configuration file into a project's configs/
// subdirectory, creating it if needed.
func writeLoop(t *testing.T, agentUtilsDir, fileName, body string) {
	t.Helper()
	configs := config.ConfigsDir(agentUtilsDir)
	if err := os.MkdirAll(configs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configs, fileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTargetsReturnsBothLoopsForASharedRepo(t *testing.T) {
	home := setHome(t)
	idA, dirA := newProject(t, home, "alpha")
	idB, dirB := newProject(t, home, "beta")
	writeLoop(t, dirA, "planning.yaml", minimalConfig("planning", "acme/widgets"))
	writeLoop(t, dirB, "planning.yaml", minimalConfig("planning", "acme/widgets"))

	targets, err := Targets("acme/widgets")
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("len(targets) = %d, want 2: %+v", len(targets), targets)
	}
	seen := map[string]bool{}
	for _, tg := range targets {
		seen[tg.ProjectID] = true
	}
	if !seen[idA] || !seen[idB] {
		t.Errorf("targets = %+v, want one loop from each of %s and %s", targets, idA, idB)
	}
}

func TestTargetsSkipsADeletedProjectDirectory(t *testing.T) {
	home := setHome(t)
	_, dirA := newProject(t, home, "alpha")
	idB, dirB := newProject(t, home, "beta")
	writeLoop(t, dirA, "planning.yaml", minimalConfig("planning", "acme/widgets"))
	writeLoop(t, dirB, "planning.yaml", minimalConfig("planning", "acme/widgets"))

	// alpha's directory is gone, but it stays in the registry -- exactly the
	// state registry.Project.Exists documents: moved or deleted, not pruned.
	if err := os.RemoveAll(dirA); err != nil {
		t.Fatal(err)
	}

	targets, err := Targets("acme/widgets")
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 1 || targets[0].ProjectID != idB {
		t.Fatalf("targets = %+v, want exactly beta's loop", targets)
	}
}

func TestTargetsSkipsAnUnparsableLoopFile(t *testing.T) {
	home := setHome(t)
	_, dir := newProject(t, home, "alpha")
	writeLoop(t, dir, "broken.yaml", "name: broken\nthis_key_does_not_exist: true\n")
	writeLoop(t, dir, "good.yaml", minimalConfig("planning", "acme/widgets"))

	targets, err := Targets("acme/widgets")
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 1 || targets[0].LoopName != "planning" {
		t.Fatalf("targets = %+v, want exactly the one parsable loop", targets)
	}
}

func TestTargetsSkipsAProjectWithNoConfigsDir(t *testing.T) {
	home := setHome(t)
	newProject(t, home, "empty") // never gets writeLoop, so no configs/ dir exists
	idGood, dirGood := newProject(t, home, "good")
	writeLoop(t, dirGood, "planning.yaml", minimalConfig("planning", "acme/widgets"))

	targets, err := Targets("acme/widgets")
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 1 || targets[0].ProjectID != idGood {
		t.Fatalf("targets = %+v, want exactly good's loop", targets)
	}
}

func TestTargetsMatchIgnoresCase(t *testing.T) {
	home := setHome(t)
	_, dir := newProject(t, home, "alpha")
	writeLoop(t, dir, "planning.yaml", minimalConfig("planning", "Acme/Widgets"))

	targets, err := Targets("acme/widgets")
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want a case-insensitive match", targets)
	}
}

// A corrupt registry is a real failure the caller must see: routing nothing
// silently would turn every delivery into a no-op with no recorded outcome
// anywhere. Every OTHER failure mode in this file is per-project and gets
// logged and skipped instead; this is the one exception.
func TestTargetsReturnsAnErrorWhenTheRegistryCannotBeRead(t *testing.T) {
	home := setHome(t)
	if err := os.WriteFile(filepath.Join(home, registry.FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Targets("acme/widgets"); err == nil {
		t.Fatal("Targets returned a nil error for a corrupt registry file")
	}
}

func TestTargetForReturnsExactlyOneLoopAndOkFalseForUnknown(t *testing.T) {
	home := setHome(t)
	idA, dirA := newProject(t, home, "alpha")
	writeLoop(t, dirA, "planning.yaml", minimalConfig("planning", "acme/widgets"))

	target, ok, err := TargetFor(idA, "planning")
	if err != nil {
		t.Fatalf("TargetFor: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true for a known project/loop pair")
	}
	if target.LoopName != "planning" || target.ProjectID != idA {
		t.Errorf("target = %+v, want alpha's planning loop", target)
	}

	if _, ok, err := TargetFor(idA, "no-such-loop"); err != nil || ok {
		t.Errorf("TargetFor(known project, unknown loop) = ok=%v err=%v, want ok=false, err=nil", ok, err)
	}
	if _, ok, err := TargetFor("no-such-project", "planning"); err != nil || ok {
		t.Errorf("TargetFor(unknown project, known loop) = ok=%v err=%v, want ok=false, err=nil", ok, err)
	}
}

// A retry deadline must wake exactly the one loop it belongs to. If
// TargetFor ever matched by loop name alone, project A's deadline would
// wake project B's identically-named loop too, spending B's token budget on
// A's issue.
func TestTargetForDoesNotReturnAnotherProjectsSameNamedLoop(t *testing.T) {
	home := setHome(t)
	idA, dirA := newProject(t, home, "alpha")
	_, dirB := newProject(t, home, "beta")
	writeLoop(t, dirA, "planning.yaml", minimalConfig("planning", "acme/widgets"))
	writeLoop(t, dirB, "planning.yaml", minimalConfig("planning", "acme/widgets"))

	target, ok, err := TargetFor(idA, "planning")
	if err != nil {
		t.Fatalf("TargetFor: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if target.ProjectID != idA || target.Dir != dirA {
		t.Errorf("target = %+v, want alpha's loop, not beta's same-named one", target)
	}
}
