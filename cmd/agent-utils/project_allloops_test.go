package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/project"
	"github.com/seanmcgary/agent-utils/internal/registry"
	"github.com/seanmcgary/agent-utils/internal/wizard"
	"github.com/urfave/cli/v3"
)

// failScaffold is failWizard's counterpart: a test asserting --all-loops was
// NOT taken proves nothing unless the seam fails when called.
func failScaffold(t *testing.T) func(agentUtilsDir, rootDir string) ([]scaffoldedLoop, error) {
	t.Helper()
	return func(string, string) ([]scaffoldedLoop, error) {
		t.Fatal("ScaffoldLoops must not be called")
		return nil, nil
	}
}

// TestProjectInitAllLoopsWritesEveryTemplate drives the REAL scaffoldLoops
// against a real git checkout, so it covers the whole path end to end: Detect
// reads the origin remote, wizard.Scaffold builds four configurations, and
// wizard.Write reloads each one through config.Load.
func TestProjectInitAllLoopsWritesEveryTemplate(t *testing.T) {
	withHome(t)
	dir := gitRepoWithOrigin(t, "git@github.com:acme/widgets.git")
	var out bytes.Buffer

	err := projectInitRun(projectInitDeps{
		Dir: dir, AllLoops: true, Interactive: true,
		RunWizard: failWizard(t), ScaffoldLoops: scaffoldLoops, Out: &out,
	})
	if err != nil {
		t.Fatalf("projectInitRun: %v", err)
	}

	entries, err := config.List(agentUtilsDirFor(dir))
	if err != nil {
		t.Fatalf("config.List: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("wrote %d loop configurations, want 4", len(entries))
	}

	byName := map[string]*config.Config{}
	for _, e := range entries {
		if e.Err != nil {
			t.Fatalf("loop configuration %s does not load: %v", e.Path, e.Err)
		}
		loaded, loadErr := config.Load(e.Path)
		if loadErr != nil {
			t.Fatalf("load %s: %v", e.Path, loadErr)
		}
		byName[loaded.Name] = loaded
	}

	// The table this whole change exists to deliver.
	want := map[string]struct{ model, effort string }{
		"planning":                {"opus", "high"},
		"execution":               {"sonnet", "medium"},
		"pr-review":               {"opus", "medium"},
		"exec-pr-review-findings": {"sonnet", "medium"},
	}
	for name, w := range want {
		cfg, ok := byName[name]
		if !ok {
			t.Errorf("no loop configuration named %q was written", name)
			continue
		}
		if cfg.Agent.Model != w.model || cfg.Agent.Effort != w.effort {
			t.Errorf("%s: agent = %s/%s, want %s/%s", name, cfg.Agent.Model, cfg.Agent.Effort, w.model, w.effort)
		}
		if cfg.Repo != "acme/widgets" {
			t.Errorf("%s: repo = %q, want the detected %q", name, cfg.Repo, "acme/widgets")
		}
		if cfg.DefaultBranch != "master" {
			t.Errorf("%s: default_branch = %q, want %q", name, cfg.DefaultBranch, "master")
		}
	}

	// Every loop named, and the bypassPermissions warning the operator gets
	// instead of a prompt.
	for name := range want {
		if !strings.Contains(out.String(), name+".yaml") {
			t.Errorf("output does not name %s.yaml:\n%s", name, out.String())
		}
	}
	if !strings.Contains(out.String(), "bypassPermissions") {
		t.Errorf("output does not warn about bypassPermissions:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "loop tick --name planning") {
		t.Errorf("output does not name the next command:\n%s", out.String())
	}
}

// --all-loops is the whole point BECAUSE it needs no terminal: it must work
// where the wizard cannot, which is the case the interactive gate would
// otherwise swallow.
func TestProjectInitAllLoopsRunsWithoutATerminal(t *testing.T) {
	withHome(t)
	dir := gitRepoWithOrigin(t, "https://github.com/acme/widgets.git")
	var out bytes.Buffer

	err := projectInitRun(projectInitDeps{
		Dir: dir, AllLoops: true, Interactive: false,
		RunWizard: failWizard(t), ScaffoldLoops: scaffoldLoops, Out: &out,
	})
	if err != nil {
		t.Fatalf("projectInitRun: %v", err)
	}
	entries, err := config.List(agentUtilsDirFor(dir))
	if err != nil {
		t.Fatalf("config.List: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("wrote %d loop configurations in a non-interactive run, want 4", len(entries))
	}
	if strings.Contains(out.String(), "not a terminal") {
		t.Errorf("--all-loops took the non-interactive skip:\n%s", out.String())
	}
}

// The two flags ask for opposite things, so the combination is an operator
// error rather than a precedence rule to be guessed at -- and it is refused
// BEFORE anything is written, so a mistyped command leaves no half-made
// project behind.
func TestProjectInitAllLoopsWithNoLoopIsRefusedAndWritesNothing(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	var out bytes.Buffer

	err := projectInitRun(projectInitDeps{
		Dir: dir, AllLoops: true, NoLoop: true, Interactive: true,
		RunWizard: failWizard(t), ScaffoldLoops: failScaffold(t), Out: &out,
	})
	if err == nil {
		t.Fatal("--all-loops with --no-loop: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "--all-loops") || !strings.Contains(err.Error(), "--no-loop") {
		t.Errorf("error does not name both flags: %v", err)
	}
	if _, statErr := os.Stat(agentUtilsDirFor(dir)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the refused run created %s", agentUtilsDirFor(dir))
	}
	if projects, listErr := registry.List(); listErr == nil && len(projects) != 0 {
		t.Errorf("the refused run registered %d projects", len(projects))
	}
}

// A project that already has loops returns early with its existing-loops
// report, exactly as it does for the wizard: wizard.Write refuses to
// overwrite, so scaffolding here could only ever half-succeed.
func TestProjectInitAllLoopsNoOpsWhenLoopsExist(t *testing.T) {
	withHome(t)
	root := clonedRepo(t, "already", "3f8c1d2e-0000-4000-8000-00000000a11d", "planning")
	var out bytes.Buffer

	if err := projectInitRun(projectInitDeps{
		Dir: root, AllLoops: true, Interactive: true,
		RunWizard: failWizard(t), ScaffoldLoops: failScaffold(t), Out: &out,
	}); err != nil {
		t.Fatalf("projectInitRun: %v", err)
	}
	if !strings.Contains(out.String(), "nothing else to do") {
		t.Errorf("output = %q, want the existing-loops report", out.String())
	}
}

// Without an origin remote there is no repo to write and no prompt to ask
// for one, so the run fails with a message that names the way out rather
// than writing four loops pointing at nothing.
func TestProjectInitAllLoopsWithoutAnOriginRemoteErrors(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	var out bytes.Buffer

	err := projectInitRun(projectInitDeps{
		Dir: dir, AllLoops: true, Interactive: true,
		RunWizard: failWizard(t), ScaffoldLoops: scaffoldLoops, Out: &out,
	})
	if err == nil {
		t.Fatal("--all-loops outside a git checkout: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "origin remote") {
		t.Errorf("error does not name the missing origin remote: %v", err)
	}
	// The project itself is still minted: steps 1-4 of init do not depend on
	// the loops, and re-running init (or `loop new`) must find it there.
	if _, loadErr := project.Load(agentUtilsDirFor(dir)); loadErr != nil {
		t.Errorf("the project descriptor was not written: %v", loadErr)
	}
}

// A scaffold that fails part way keeps what it already wrote and says which
// loop broke. wizard.Write reloads each file before returning, so the files
// named as written are known good; rolling them back would throw away the
// only evidence of where it stopped.
func TestProjectInitAllLoopsReportsWhatItWroteBeforeFailing(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	var out bytes.Buffer

	err := projectInitRun(projectInitDeps{
		Dir: dir, AllLoops: true, Interactive: true,
		RunWizard: failWizard(t),
		ScaffoldLoops: func(agentUtilsDir, rootDir string) ([]scaffoldedLoop, error) {
			path, writeErr := wizard.Write(agentUtilsDir, validLoopConfig("planning"))
			if writeErr != nil {
				return nil, writeErr
			}
			return []scaffoldedLoop{{
				Path: path, Model: "opus", Effort: "high", PermissionMode: "acceptEdits",
			}}, errors.New("scaffold execution: disk on fire")
		},
		Out: &out,
	})
	if err == nil {
		t.Fatal("a failing scaffold: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "disk on fire") {
		t.Errorf("error does not carry the scaffold's own message: %v", err)
	}
	if !strings.Contains(out.String(), "planning.yaml") {
		t.Errorf("output does not name the loop written before the failure:\n%s", out.String())
	}
	// acceptEdits only: the warning is driven by what was actually written,
	// not printed unconditionally.
	if strings.Contains(out.String(), "bypassPermissions") {
		t.Errorf("warned about bypassPermissions for a loop that does not use it:\n%s", out.String())
	}
}

// The flag has to exist on the real command tree, not just in
// projectInitDeps -- the same thing TestProjectInitCLIPositionalNameNotFlag
// proves for the positional name.
func TestProjectInitAllLoopsCLIFlag(t *testing.T) {
	withHome(t)
	dir := gitRepoWithOrigin(t, "git@github.com:acme/widgets.git")

	root := &cli.Command{
		Name:     "agent-utils",
		Commands: []*cli.Command{projectCommand()},
	}

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer outR.Close()
	old := os.Stdout
	os.Stdout = outW
	runErr := root.Run(context.Background(),
		[]string{"agent-utils", "project", "init", "--dir", dir, "--all-loops"})
	os.Stdout = old
	outW.Close()
	var buf bytes.Buffer
	if _, copyErr := io.Copy(&buf, outR); copyErr != nil {
		t.Fatalf("read captured stdout: %v", copyErr)
	}
	if runErr != nil {
		t.Fatalf("project init --dir %s --all-loops: %v", dir, runErr)
	}

	entries, err := config.List(agentUtilsDirFor(dir))
	if err != nil {
		t.Fatalf("config.List: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("wrote %d loop configurations, want 4", len(entries))
	}
}

// gitRepoWithOrigin makes a real git work tree with an origin remote, which
// is what wizard.Detect reads. origin/HEAD is deliberately NOT set: Detect
// then reports no default branch, which is the case that proves Scaffold
// takes master from the template rather than from git.
func gitRepoWithOrigin(t *testing.T, remote string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", dir}, args...)
		if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("remote", "add", "origin", remote)
	return dir
}
