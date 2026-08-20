package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/project"
	"github.com/seanmcgary/agent-utils/internal/registry"
	"github.com/seanmcgary/agent-utils/internal/wizard"
	"github.com/urfave/cli/v3"
)

// failWizard fails the test the instant it is called. It is the tripwire
// project_test.go's fakeHookAdmin/Confirm pattern already establishes for
// "must not prompt": a test asserting the wizard is skipped proves nothing if
// it merely fails to check that RunWizard was invoked.
func failWizard(t *testing.T) func(agentUtilsDir, rootDir string) (string, error) {
	t.Helper()
	return func(string, string) (string, error) {
		t.Fatal("RunWizard must not be called")
		return "", nil
	}
}

// validLoopConfig returns a *config.Config that config.Load accepts, the
// same shape internal/wizard/write_test.go's validConfig builds, so a fake
// RunWizard can exercise the real wizard.Write path without a scripted
// Prompter.
func validLoopConfig(name string) *config.Config {
	return &config.Config{
		Name:            name,
		Repo:            "acme/example",
		CheckoutBaseDir: "/tmp/example",
		WorktreeDir:     "/tmp/worktrees",
		DefaultBranch:   "main",
		Labels: config.Labels{
			Trigger: "status:trigger", InFlight: "status:in-flight",
			Blocked: "status:blocked", Review: "status:review",
			Veto: []string{"blocked:*"},
		},
		Agent: config.Agent{
			Model: "opus", Effort: "high", PermissionMode: "acceptEdits",
			Worktree: config.WorktreePerIssue, MaxBudgetUSD: 25,
			Timeout: config.Duration(3 * time.Hour),
		},
		Retry: config.Retry{
			Max:     1,
			Backoff: []config.Duration{config.Duration(0)},
			Breaker: config.Breaker{
				OrphanThreshold: 2,
				Cooldown:        config.Duration(30 * time.Minute),
			},
		},
		Prompt:       "do the thing for #{{.Issue.Number}}",
		ResumePrompt: "resume the thing for #{{.Issue.Number}}",
	}
}

func agentUtilsDirFor(root string) string { return filepath.Join(root, config.DirName) }

// TestProjectInitNoLoopCreatesOneRegisteredProject covers: "init --no-loop in
// an empty directory creates .agent-utils/configs/, writes a descriptor with
// a uuid, and registers exactly one project."
func TestProjectInitNoLoopCreatesOneRegisteredProject(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	var out bytes.Buffer

	err := projectInitRun(projectInitDeps{
		Dir: dir, NoLoop: true, Interactive: true,
		RunWizard: failWizard(t), Out: &out,
	})
	if err != nil {
		t.Fatalf("projectInitRun: %v", err)
	}

	agentUtilsDir := agentUtilsDirFor(dir)
	if info, statErr := os.Stat(filepath.Join(agentUtilsDir, config.ConfigsSubdir)); statErr != nil || !info.IsDir() {
		t.Fatalf("configs/ was not created: %v", statErr)
	}

	cfg, err := project.Load(agentUtilsDir)
	if err != nil {
		t.Fatalf("project.Load: %v", err)
	}
	if _, err := uuid.Parse(cfg.ID); err != nil {
		t.Errorf("descriptor ID = %q, want a uuid: %v", cfg.ID, err)
	}

	projects, err := registry.List()
	if err != nil {
		t.Fatalf("registry.List: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("registered %d projects, want exactly 1", len(projects))
	}
}

// TestProjectInitModes covers: "The descriptor is mode 0600 and both
// directories are 0700."
func TestProjectInitModes(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	var out bytes.Buffer

	if err := projectInitRun(projectInitDeps{
		Dir: dir, NoLoop: true, Interactive: true,
		RunWizard: failWizard(t), Out: &out,
	}); err != nil {
		t.Fatalf("projectInitRun: %v", err)
	}

	agentUtilsDir := agentUtilsDirFor(dir)
	configsDir := filepath.Join(agentUtilsDir, config.ConfigsSubdir)

	checkMode := func(path string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("mode of %s = %v, want %v", path, got, want)
		}
	}
	checkMode(agentUtilsDir, 0o700)
	checkMode(configsDir, 0o700)
	checkMode(project.Path(agentUtilsDir), 0o600)
}

// TestProjectInitTwiceKeepsSameIDAndRegistersOnce covers: "Running init twice
// keeps the same id and registers once."
func TestProjectInitTwiceKeepsSameIDAndRegistersOnce(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	var out1, out2 bytes.Buffer

	if err := projectInitRun(projectInitDeps{
		Dir: dir, NoLoop: true, Interactive: true, RunWizard: failWizard(t), Out: &out1,
	}); err != nil {
		t.Fatalf("first init: %v", err)
	}
	first, err := project.Load(agentUtilsDirFor(dir))
	if err != nil {
		t.Fatalf("project.Load: %v", err)
	}

	if err := projectInitRun(projectInitDeps{
		Dir: dir, NoLoop: true, Interactive: true, RunWizard: failWizard(t), Out: &out2,
	}); err != nil {
		t.Fatalf("second init: %v", err)
	}
	second, err := project.Load(agentUtilsDirFor(dir))
	if err != nil {
		t.Fatalf("project.Load: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("id changed across two inits: %q -> %q", first.ID, second.ID)
	}

	projects, err := registry.List()
	if err != nil {
		t.Fatalf("registry.List: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("registered %d projects after two inits, want exactly 1", len(projects))
	}
}

// TestProjectInitPositionalNameUsedAndTakenNameSuffixed covers: "init with a
// positional name uses it; a taken name gets a suffix and says so."
func TestProjectInitPositionalNameUsedAndTakenNameSuffixed(t *testing.T) {
	withHome(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	var outA, outB bytes.Buffer

	if err := projectInitRun(projectInitDeps{
		Dir: dirA, Name: "web", NoLoop: true, Interactive: true, RunWizard: failWizard(t), Out: &outA,
	}); err != nil {
		t.Fatalf("first init: %v", err)
	}
	cfgA, err := project.Load(agentUtilsDirFor(dirA))
	if err != nil {
		t.Fatalf("project.Load: %v", err)
	}
	if cfgA.Name != "web" {
		t.Fatalf("Name = %q, want the positional name %q", cfgA.Name, "web")
	}

	if err := projectInitRun(projectInitDeps{
		Dir: dirB, Name: "web", NoLoop: true, Interactive: true, RunWizard: failWizard(t), Out: &outB,
	}); err != nil {
		t.Fatalf("second init: %v", err)
	}
	cfgB, err := project.Load(agentUtilsDirFor(dirB))
	if err != nil {
		t.Fatalf("project.Load: %v", err)
	}
	if cfgB.Name != "web-2" {
		t.Fatalf("Name = %q, want web-2 when web is taken", cfgB.Name)
	}
	if !strings.Contains(outB.String(), "web") || !strings.Contains(outB.String(), "already taken") {
		t.Errorf("output = %q, want it to say the name was already taken", outB.String())
	}
}

// TestProjectInitMachineWideDirRefusesAndWritesNothing covers: "init in the
// machine-wide directory exits non-zero and writes nothing."
func TestProjectInitMachineWideDirRefusesAndWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_UTILS_HOME", home)
	var out bytes.Buffer

	err := projectInitRun(projectInitDeps{
		Dir: home, NoLoop: true, Interactive: true, RunWizard: failWizard(t), Out: &out,
	})
	if err == nil {
		t.Fatal("init in the machine-wide directory: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "machine-wide") {
		t.Errorf("error = %q, want it to name the machine-wide directory", err.Error())
	}
	if _, statErr := os.Stat(filepath.Join(home, config.DirName)); !os.IsNotExist(statErr) {
		t.Errorf("%s/.agent-utils exists after a refused init, want nothing written", home)
	}
}

// TestProjectInitWizardWritesLoopFileConfigLoadAccepts covers: "init with the
// wizard writes a loop file that config.Load accepts."
func TestProjectInitWizardWritesLoopFileConfigLoadAccepts(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	var out bytes.Buffer

	err := projectInitRun(projectInitDeps{
		Dir: dir, NoLoop: false, Interactive: true,
		RunWizard: func(agentUtilsDir, rootDir string) (string, error) {
			return wizard.Write(agentUtilsDir, validLoopConfig("planning"))
		},
		Out: &out,
	})
	if err != nil {
		t.Fatalf("projectInitRun: %v", err)
	}

	loopPath := filepath.Join(agentUtilsDirFor(dir), config.ConfigsSubdir, "planning.yaml")
	if _, err := config.Load(loopPath); err != nil {
		t.Fatalf("config.Load(%s): %v", loopPath, err)
	}
	if !strings.Contains(out.String(), loopPath) {
		t.Errorf("output = %q, want it to name the loop file written", out.String())
	}
}

// TestProjectInitNonInteractiveSkipsWizardAndNamesLoopNew covers: "A
// non-interactive init skips the wizard, still creates the project, and
// names `project loop new`."
func TestProjectInitNonInteractiveSkipsWizardAndNamesLoopNew(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	var out bytes.Buffer

	err := projectInitRun(projectInitDeps{
		Dir: dir, NoLoop: false, Interactive: false, RunWizard: failWizard(t), Out: &out,
	})
	if err != nil {
		t.Fatalf("projectInitRun: %v", err)
	}
	if _, loadErr := project.Load(agentUtilsDirFor(dir)); loadErr != nil {
		t.Errorf("project was not created despite the skipped wizard: %v", loadErr)
	}
	if !strings.Contains(out.String(), "project loop new") {
		t.Errorf("output = %q, want it to name `project loop new`", out.String())
	}
}

// TestProjectLoopNewNonInteractiveErrorsAndPromptsNothing covers: "A
// non-interactive loop new exits non-zero and prompts nothing."
func TestProjectLoopNewNonInteractiveErrorsAndPromptsNothing(t *testing.T) {
	var out bytes.Buffer
	err := projectLoopNewRun(projectLoopNewDeps{
		AgentUtilsDir: t.TempDir(), RootDir: t.TempDir(),
		Interactive: false, RunWizard: failWizard(t), Out: &out,
	})
	if err == nil {
		t.Fatal("loop new in a non-interactive run: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("error = %q, want it to name the non-interactive rule", err.Error())
	}
}

// TestProjectLoopNewWritesLoopFile is a thin smoke test of the success path:
// an interactive run calls RunWizard and reports what it wrote.
func TestProjectLoopNewWritesLoopFile(t *testing.T) {
	var out bytes.Buffer
	called := false
	err := projectLoopNewRun(projectLoopNewDeps{
		AgentUtilsDir: t.TempDir(), RootDir: t.TempDir(),
		Interactive: true,
		RunWizard: func(agentUtilsDir, rootDir string) (string, error) {
			called = true
			return filepath.Join(agentUtilsDir, "configs", "execution.yaml"), nil
		},
		Out: &out,
	})
	if err != nil {
		t.Fatalf("projectLoopNewRun: %v", err)
	}
	if !called {
		t.Error("RunWizard was not called on an interactive run")
	}
	if !strings.Contains(out.String(), "execution.yaml") {
		t.Errorf("output = %q, want it to name the loop file written", out.String())
	}
}

// TestProjectInitCLIPositionalNameNotFlag covers the hazard the brief calls
// out directly: the project name is a POSITIONAL argument, not --name, since
// `project` already declares --name as the project selector and a child
// flag of the same name would shadow it. Running the real command tree (not
// calling projectInitRun directly) is what actually proves the positional
// argument, not just the internal function signature.
func TestProjectInitCLIPositionalNameNotFlag(t *testing.T) {
	withHome(t)
	dir := t.TempDir()

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
		[]string{"agent-utils", "project", "init", "myname", "--dir", dir, "--no-loop"})
	os.Stdout = old
	outW.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, outR); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	if runErr != nil {
		t.Fatalf("project init myname --dir %s --no-loop: %v", dir, runErr)
	}

	cfg, err := project.Load(agentUtilsDirFor(dir))
	if err != nil {
		t.Fatalf("project.Load: %v", err)
	}
	if cfg.Name != "myname" {
		t.Errorf("Name = %q, want the positional argument %q", cfg.Name, "myname")
	}
}

// TestProjectInitMachineWideAgentUtilsDirRefusesAndWritesNothing covers the
// CRITICAL review finding directly: the actual dangerous invocation is `cd ~
// && agent-utils project init`, where Dir is the machine-wide directory's
// PARENT and the descriptor would land at <machine-wide>/.agent-utils,
// beside registry.json and state.db. The guard must compare the COMPUTED
// .agent-utils path against home.Dir(), not the root directory against it —
// those are never equal for this exact invocation, which is why the sibling
// TestProjectInitMachineWideDirRefusesAndWritesNothing (Dir set EQUAL to
// AGENT_UTILS_HOME) alone did not catch this: that is the one arrangement a
// root-only comparison happens to get right.
func TestProjectInitMachineWideAgentUtilsDirRefusesAndWritesNothing(t *testing.T) {
	parent := t.TempDir()
	machineWide := filepath.Join(parent, ".agent-utils")
	t.Setenv("AGENT_UTILS_HOME", machineWide)
	var out bytes.Buffer

	err := projectInitRun(projectInitDeps{
		Dir: parent, NoLoop: true, Interactive: true, RunWizard: failWizard(t), Out: &out,
	})
	if err == nil {
		t.Fatal("init whose computed .agent-utils dir IS the machine-wide directory: want an error, got nil")
	}
	if _, statErr := os.Stat(filepath.Join(machineWide, project.FileName)); !os.IsNotExist(statErr) {
		t.Errorf("%s exists after a refused init, want no descriptor written into the machine-wide directory", filepath.Join(machineWide, project.FileName))
	}
}

// TestProjectInitDefaultNameTakenReportsRename covers IMPORTANT 2: the
// rename report openProject used to print (internal/loopcmd/resolve.go's
// RenamedFrom, derived from the directory basename) must still fire when NO
// positional name is given and project.EnsureNamed has to uniquify the
// directory-derived name. Before the internal/project.EnsureNamed refactor,
// mintProjectDescriptor's name == "" branch hard-returned "" for
// renamedFrom, so this path silently produced "web-2" with no explanation.
func TestProjectInitDefaultNameTakenReportsRename(t *testing.T) {
	withHome(t)

	// Two directories cannot share one basename on a real filesystem, so the
	// collision project.EnsureNamed actually sees -- two projects whose
	// directory basename slugs to the same name -- is built with two distinct
	// parents both containing a directory named "web".
	rootA := filepath.Join(t.TempDir(), "web")
	rootB := filepath.Join(t.TempDir(), "web")
	if err := os.MkdirAll(rootA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rootB, 0o755); err != nil {
		t.Fatal(err)
	}

	var outA, outB bytes.Buffer
	if err := projectInitRun(projectInitDeps{
		Dir: rootA, NoLoop: true, Interactive: true, RunWizard: failWizard(t), Out: &outA,
	}); err != nil {
		t.Fatalf("first init: %v", err)
	}
	cfgA, err := project.Load(agentUtilsDirFor(rootA))
	if err != nil {
		t.Fatalf("project.Load: %v", err)
	}
	if cfgA.Name != "web" {
		t.Fatalf("Name = %q, want %q", cfgA.Name, "web")
	}

	if err := projectInitRun(projectInitDeps{
		Dir: rootB, NoLoop: true, Interactive: true, RunWizard: failWizard(t), Out: &outB,
	}); err != nil {
		t.Fatalf("second init: %v", err)
	}
	cfgB, err := project.Load(agentUtilsDirFor(rootB))
	if err != nil {
		t.Fatalf("project.Load: %v", err)
	}
	if cfgB.Name != "web-2" {
		t.Fatalf("Name = %q, want web-2 when the directory-derived name web is taken", cfgB.Name)
	}
	if !strings.Contains(outB.String(), "already taken") {
		t.Errorf("output = %q, want it to say the directory-derived name was already taken, "+
			"the way openProject's RenamedFrom reporting used to", outB.String())
	}
}

// TestProjectInitIgnoredPositionalNameIsReported covers MINOR 5: a
// positional name passed to an already-initialised project is silently
// dropped by mintProjectDescriptor (the project already has an identity),
// but the operator typed it and deserves to be told it did nothing.
func TestProjectInitIgnoredPositionalNameIsReported(t *testing.T) {
	withHome(t)
	dir := t.TempDir()
	var first, second bytes.Buffer

	if err := projectInitRun(projectInitDeps{
		Dir: dir, Name: "original", NoLoop: true, Interactive: true, RunWizard: failWizard(t), Out: &first,
	}); err != nil {
		t.Fatalf("first init: %v", err)
	}

	if err := projectInitRun(projectInitDeps{
		Dir: dir, Name: "newname", NoLoop: true, Interactive: true, RunWizard: failWizard(t), Out: &second,
	}); err != nil {
		t.Fatalf("second init: %v", err)
	}
	if !strings.Contains(second.String(), "newname") || !strings.Contains(second.String(), "ignored") {
		t.Errorf("output = %q, want it to say the requested name %q was ignored", second.String(), "newname")
	}

	cfg, err := project.Load(agentUtilsDirFor(dir))
	if err != nil {
		t.Fatalf("project.Load: %v", err)
	}
	if cfg.Name != "original" {
		t.Errorf("Name = %q, want the original identity to be kept", cfg.Name)
	}
}

// clonedRepo builds what a `git clone` of an already-onboarded repository
// leaves on a new host: a COMMITTED project descriptor (name and id already
// minted, on some other machine) and, when loopName is non-empty, a committed
// loop configuration beside it. Nothing here touches the registry — that is
// exactly what `project init` has to do on the new host.
func clonedRepo(t *testing.T, name, id, loopName string) string {
	t.Helper()
	root := t.TempDir()
	agentUtilsDir := agentUtilsDirFor(root)
	if err := os.MkdirAll(config.ConfigsDir(agentUtilsDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := project.Save(agentUtilsDir, &project.Config{Name: name, ID: id}); err != nil {
		t.Fatal(err)
	}
	if loopName != "" {
		if _, err := wizard.Write(agentUtilsDir, validLoopConfig(loopName)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestProjectInitClonedRepoRegistersAndSkipsWizard covers the whole clone
// case: a repo whose .agent-utils/config.yaml AND .agent-utils/configs/*.yaml
// are already committed needs nothing minted and nothing asked — its only
// missing piece is the entry in this host's registry. init used to offer the
// wizard anyway, and wizard.Write refuses to overwrite, so the operator
// answered every question and only then hit the failure.
func TestProjectInitClonedRepoRegistersAndSkipsWizard(t *testing.T) {
	withHome(t)
	const id = "3f8c1d2e-0000-4000-8000-000000000001"
	root := clonedRepo(t, "lawndominator", id, "planning")
	var out bytes.Buffer

	err := projectInitRun(projectInitDeps{
		Dir: root, NoLoop: false, Interactive: true,
		RunWizard: failWizard(t), Out: &out,
	})
	if err != nil {
		t.Fatalf("projectInitRun on a cloned repo: %v", err)
	}

	projects, err := registry.List()
	if err != nil {
		t.Fatalf("registry.List: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("registered %d projects, want exactly 1", len(projects))
	}
	if projects[0].ID != id || projects[0].Name != "lawndominator" {
		t.Errorf("registered %+v, want the committed identity %q/%q", projects[0], "lawndominator", id)
	}

	// The operator has to be able to see that init recognised the existing
	// setup rather than silently doing nothing: name, id, path, loop count.
	for _, want := range []string{"lawndominator", id, agentUtilsDirFor(root), "1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, want it to report %q", out.String(), want)
		}
	}
}

// TestProjectInitClonedRepoTwiceStaysASuccess covers the ordinary re-run: the
// same project, same id, initialised again on the same host is a no-op
// success, not a name collision with itself.
func TestProjectInitClonedRepoTwiceStaysASuccess(t *testing.T) {
	withHome(t)
	const id = "3f8c1d2e-0000-4000-8000-000000000002"
	root := clonedRepo(t, "lawndominator", id, "planning")

	for i := 0; i < 2; i++ {
		var out bytes.Buffer
		if err := projectInitRun(projectInitDeps{
			Dir: root, NoLoop: false, Interactive: true,
			RunWizard: failWizard(t), Out: &out,
		}); err != nil {
			t.Fatalf("init %d: %v", i+1, err)
		}
	}

	projects, err := registry.List()
	if err != nil {
		t.Fatalf("registry.List: %v", err)
	}
	if len(projects) != 1 || projects[0].ID != id {
		t.Fatalf("registry = %+v, want exactly one entry for %q", projects, id)
	}
}

// TestProjectInitDescriptorWithoutLoopsStillOffersWizard guards the skip from
// swallowing a half-finished setup: a descriptor with no loop configuration at
// all is a project someone started and never finished, not a clone, and the
// wizard is still what it needs.
func TestProjectInitDescriptorWithoutLoopsStillOffersWizard(t *testing.T) {
	withHome(t)
	root := clonedRepo(t, "halfdone", "3f8c1d2e-0000-4000-8000-000000000003", "")
	var out bytes.Buffer

	called := false
	err := projectInitRun(projectInitDeps{
		Dir: root, NoLoop: false, Interactive: true,
		RunWizard: func(agentUtilsDir, rootDir string) (string, error) {
			called = true
			return wizard.Write(agentUtilsDir, validLoopConfig("planning"))
		},
		Out: &out,
	})
	if err != nil {
		t.Fatalf("projectInitRun: %v", err)
	}
	if !called {
		t.Error("RunWizard was not called for a project with a descriptor but no loop configurations")
	}
}

// TestProjectInitBrokenLoopFileStillSkipsWizard covers the deliberate choice
// about config.Entry.Err: a loop file that fails to load still proves the repo
// was set up. Walking the operator through 24 questions the wizard cannot even
// write the answers to (wizard.Write refuses to overwrite) instead of telling
// them to fix the file they already have would be the same failure this whole
// change removes.
func TestProjectInitBrokenLoopFileStillSkipsWizard(t *testing.T) {
	withHome(t)
	root := clonedRepo(t, "broken", "3f8c1d2e-0000-4000-8000-000000000004", "")
	loopPath := filepath.Join(agentUtilsDirFor(root), config.ConfigsSubdir, "planning.yaml")
	if err := os.WriteFile(loopPath, []byte("name: planning\nnot_a_field: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(loopPath); err == nil {
		t.Fatal("the fixture loop file must fail to load, or this test proves nothing")
	}

	var out bytes.Buffer
	if err := projectInitRun(projectInitDeps{
		Dir: root, NoLoop: false, Interactive: true,
		RunWizard: failWizard(t), Out: &out,
	}); err != nil {
		t.Fatalf("projectInitRun: %v", err)
	}
}

// TestProjectInitRefusesNameTakenByADifferentProject covers the collision a
// COMMITTED descriptor used to walk straight past: project.EnsureNamed only
// uniquifies a name it mints, so cloning a repo whose committed name matches a
// project already on this host wrote a second registry entry with the same
// name — and from then on every `project --name <that name>` command silently
// acted on whichever of the two ticked last.
func TestProjectInitRefusesNameTakenByADifferentProject(t *testing.T) {
	withHome(t)
	existing := t.TempDir()
	var first bytes.Buffer
	if err := projectInitRun(projectInitDeps{
		Dir: existing, Name: "lawndominator", NoLoop: true, Interactive: true,
		RunWizard: failWizard(t), Out: &first,
	}); err != nil {
		t.Fatalf("first init: %v", err)
	}

	clone := clonedRepo(t, "lawndominator", "3f8c1d2e-0000-4000-8000-000000000005", "planning")
	var out bytes.Buffer
	err := projectInitRun(projectInitDeps{
		Dir: clone, NoLoop: false, Interactive: true,
		RunWizard: failWizard(t), Out: &out,
	})
	if err == nil {
		t.Fatal("init of a clone whose committed name is taken: want an error, got nil")
	}
	// Both paths, so the operator can tell the two projects apart, and the
	// descriptor to edit.
	for _, want := range []string{agentUtilsDirFor(existing), project.Path(agentUtilsDirFor(clone)), "name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err.Error(), want)
		}
	}

	projects, listErr := registry.List()
	if listErr != nil {
		t.Fatalf("registry.List: %v", listErr)
	}
	if len(projects) != 1 {
		t.Fatalf("registry holds %d projects after a refused init, want the original 1 only: %+v",
			len(projects), projects)
	}
}
