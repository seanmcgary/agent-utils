package loopcmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/project"
	"github.com/seanmcgary/agent-utils/internal/registry"
)

// chdir points the process cwd at dir for the duration of the test and
// restores it on cleanup. ResolveProject's directory-based path reads
// os.Getwd() directly (that is what lets it work "from any directory" the
// way git finds .git), so exercising it needs a real cwd change rather than
// a parameter.
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(old); err != nil {
			t.Fatal(err)
		}
	})
}

// isolate points the machine-wide directory and $HOME at fresh temp
// directories, so a test's registry.Register/List calls cannot see another
// test's projects and FindDir's walk-up cannot climb into the operator's
// real home directory.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_UTILS_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENT_UTILS_DIR", "")
}

// TestResolveProjectNoDescriptorNamesInitAndWritesNothing covers F5's first
// acceptance line: a directory with .agent-utils/ but no descriptor must
// report an error naming `project init`, and must mint nothing. Before this
// change, ResolveProject called project.Ensure here and silently onboarded
// the directory -- that is the exact bug this task fixes.
func TestResolveProjectNoDescriptorNamesInitAndWritesNothing(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	agentUtilsDir := filepath.Join(root, config.DirName)
	if err := os.MkdirAll(agentUtilsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	chdir(t, root)

	_, err := ResolveProject("")
	if err == nil {
		t.Fatal("ResolveProject in a .agent-utils dir with no descriptor: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "project init") {
		t.Errorf("error = %q, want it to name `agent-utils project init`", err.Error())
	}

	if _, statErr := os.Stat(project.Path(agentUtilsDir)); !os.IsNotExist(statErr) {
		t.Errorf("%s exists after a failed resolve, want no descriptor written", project.Path(agentUtilsDir))
	}
	projects, listErr := registry.List()
	if listErr != nil {
		t.Fatalf("registry.List: %v", listErr)
	}
	if len(projects) != 0 {
		t.Errorf("registered %d projects after a failed resolve, want 0", len(projects))
	}
}

// TestResolveProjectNoAgentUtilsDirNamesInit covers F5's second acceptance
// line: a directory with no .agent-utils/ anywhere in its parents must
// report an error naming `project init`, matching config.ErrNoDir's shape.
func TestResolveProjectNoAgentUtilsDirNamesInit(t *testing.T) {
	isolate(t)
	chdir(t, t.TempDir())

	_, err := ResolveProject("")
	if err == nil {
		t.Fatal("ResolveProject with no .agent-utils anywhere: want an error, got nil")
	}
	if !errors.Is(err, config.ErrNoDir) {
		t.Errorf("err = %v, want it to wrap config.ErrNoDir", err)
	}
	// Assert on text only noProjectErr itself can produce, not a bare
	// "project init" substring: config.ErrNoDir's own message used to name
	// `agent-utils project init` too (removed in MINOR 2's fix), which meant
	// this test passed identically whether or not ResolveProject wrapped the
	// error at all -- it pinned errors.Is but not the requirement it is
	// named for.
	if !strings.Contains(err.Error(), "This command acts on the project you are in") {
		t.Errorf("error = %q, want noProjectErr's own message", err.Error())
	}
	if !strings.Contains(err.Error(), "agent-utils project --name") {
		t.Errorf("error = %q, want the registry-selector alternative noProjectErr names", err.Error())
	}
	if strings.Contains(err.Error(), "mkdir -p") {
		t.Errorf("error = %q, must not contain the old `mkdir -p` shape", err.Error())
	}
}

// TestResolveProjectInitialisedProjectResolvesAndReregisters covers F5's
// third acceptance line: an already-initialised project (a descriptor minted
// ahead of time, the way `project init` does it, not through ResolveProject
// itself) resolves cleanly and is re-registered, so a project that moves is
// still found.
func TestResolveProjectInitialisedProjectResolvesAndReregisters(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	agentUtilsDir := filepath.Join(root, config.DirName)
	if err := os.MkdirAll(agentUtilsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &project.Config{Name: "widgets", ID: "fixed-id-123"}
	if err := project.Save(agentUtilsDir, cfg); err != nil {
		t.Fatal(err)
	}
	chdir(t, root)

	p, err := ResolveProject("")
	if err != nil {
		t.Fatalf("ResolveProject: %v", err)
	}
	if p.Config.Name != "widgets" || p.Config.ID != "fixed-id-123" {
		t.Errorf("resolved config = %+v, want the descriptor already on disk", p.Config)
	}
	// Compared resolved: t.TempDir() on Darwin returns a /var/... path that
	// is itself a symlink to /private/var/..., and os.Getwd() after chdir
	// returns the resolved spelling, so a raw string compare would fail for
	// a reason that has nothing to do with ResolveProject.
	if home.Resolve(p.Dir) != home.Resolve(agentUtilsDir) {
		t.Errorf("Dir = %q, want %q", p.Dir, agentUtilsDir)
	}

	projects, listErr := registry.List()
	if listErr != nil {
		t.Fatalf("registry.List: %v", listErr)
	}
	if len(projects) != 1 {
		t.Fatalf("registered %d projects, want exactly 1 (re-registered on resolve)", len(projects))
	}
	if projects[0].ID != "fixed-id-123" {
		t.Errorf("registered project id = %q, want %q", projects[0].ID, "fixed-id-123")
	}
}
