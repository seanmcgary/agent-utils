package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// loopYAML is the smallest configuration that passes validation.
const loopYAML = `name: %s
repo: o/r
checkout_base_dir: /tmp/checkout
worktree_dir: /tmp/worktrees
default_branch: master
%s
labels:
  trigger: t
  in_flight: i
  blocked: b
  review: r
  terminal: d
agent:
  model: sonnet
  worktree: none
  timeout: 1h
retry:
  max: 3
  backoff: [0s, 15m, 30m]
  breaker:
    orphan_threshold: 2
    cooldown: 30m
prompt: "plan"
resume_prompt: "resume"
`

func projectDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), ".agent-utils")
	if err := os.MkdirAll(config.ConfigsDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeConfig(t *testing.T, agentUtilsDir, name, extra string) {
	t.Helper()
	writeRawConfig(t, agentUtilsDir, name, fmt.Sprintf(loopYAML, name, extra))
}

func writeRawConfig(t *testing.T, agentUtilsDir, name, body string) {
	t.Helper()
	path := filepath.Join(config.ConfigsDir(agentUtilsDir), name+".yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverFindsAConfiguredLoop(t *testing.T) {
	dir := projectDir(t)
	writeConfig(t, dir, "planning", "")
	legacyFile(t, filepath.Join(dir, config.StateSubdir, "planning"), "planning",
		store.StatusSucceeded, 0)

	sources, problems := Discover(dir, projectA, "p")
	if len(problems) != 0 {
		t.Fatalf("problems: %+v", problems)
	}
	if len(sources) != 1 || sources[0].Loop != "planning" || sources[0].Repo != "o/r" {
		t.Fatalf("sources = %+v, want the planning loop", sources)
	}
}

// A loop whose configuration was deleted still has state on disk. Nothing else
// would ever find it, and its rows would sit unimported forever.
func TestDiscoverFindsStateWhoseConfigurationIsGone(t *testing.T) {
	dir := projectDir(t)
	legacyFile(t, filepath.Join(dir, config.StateSubdir, "retired"), "retired",
		store.StatusSucceeded, 0)

	sources, problems := Discover(dir, projectA, "p")
	if len(problems) != 0 {
		t.Fatalf("problems: %+v", problems)
	}
	if len(sources) != 1 || sources[0].Loop != "retired" {
		t.Fatalf("sources = %+v, want the retired loop's state", sources)
	}
}

// A configuration that does not load is reported, and the report says why. It
// is not fatal: state is per loop, so a broken sibling hides nothing another
// loop needs, and the broken loop cannot run until the file is fixed.
func TestDiscoverReportsABrokenConfiguration(t *testing.T) {
	dir := projectDir(t)
	writeRawConfig(t, dir, "planning", "name: planning\nrepo:\n  - not a string\n")

	_, problems := Discover(dir, projectA, "p")
	if len(problems) != 1 || problems[0].State != StateSkipped {
		t.Fatalf("problems = %+v, want one reported skip", problems)
	}
	if problems[0].Reason == "" {
		t.Error("a skipped configuration must say why")
	}
}

// A project with no configurations at all is a normal state.
func TestDiscoverToleratesAProjectWithNoLoops(t *testing.T) {
	dir := projectDir(t)
	sources, problems := Discover(dir, projectA, "p")
	if len(sources) != 0 || len(problems) != 0 {
		t.Fatalf("sources = %+v, problems = %+v; want both empty", sources, problems)
	}
}

// One file reached by two spellings is one source. Recording it twice would
// import every dispatch in it twice.
func TestDiscoverResolvesOnePathPerFile(t *testing.T) {
	dir := projectDir(t)
	stateDir := filepath.Join(dir, config.StateSubdir, "planning")
	legacyFile(t, stateDir, "planning", store.StatusSucceeded, 0)
	writeConfig(t, dir, "planning",
		"state_dir: "+filepath.Join(dir, config.StateSubdir, ".", "planning"))

	sources, problems := Discover(dir, projectA, "p")
	if len(problems) != 0 {
		t.Fatalf("problems: %+v", problems)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %+v, want one entry for one file", sources)
	}
}

// SourceFor is what the command line adds for its own loop, because --config
// takes a path that Discover's scan need not cover.
func TestSourceForFindsAnExplicitStateDirectory(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "elsewhere")
	legacyFile(t, stateDir, "planning", store.StatusSucceeded, 0)

	got, ok := SourceFor(stateDir, projectA, "p", "planning", "o/r")
	if !ok {
		t.Fatal("SourceFor did not find an existing state.db")
	}
	if got.Loop != "planning" || got.ProjectID != projectA {
		t.Errorf("source = %+v", got)
	}

	if _, ok := SourceFor(filepath.Join(t.TempDir(), "empty"), projectA, "p", "planning", "o/r"); ok {
		t.Error("SourceFor reported a state.db that does not exist")
	}
}
