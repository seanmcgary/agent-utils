package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/lock"
)

const openTestYAML = `
name: planning
repo: o/r
checkout_base_dir: %s
worktree_dir: %s
state_dir: %s
labels:
  trigger: trigger
  in_flight: in-flight
  blocked: blocked
  review: review
default_branch: master
agent:
  model: opus
  worktree: none
  timeout: 1h
retry:
  max: 0
  breaker:
    orphan_threshold: 1
    cooldown: 1m
prompt: "p"
resume_prompt: "r"
`

// writeOpenConfig writes a loop configuration that config.Load accepts, rooted
// in its own temp directory, so Open's filesystem writes (state dir, database)
// stay isolated between tests.
func writeOpenConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := fmt.Sprintf(openTestYAML, dir, filepath.Join(dir, "wt"), filepath.Join(dir, "state"))
	p := filepath.Join(dir, "loop.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpen_RequireGitHubMissingTokenFails(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	path := writeOpenConfig(t)

	_, _, _, err := Open(ProjectRef{}, path, Options{RequireGitHub: true})
	if err == nil {
		t.Fatal("Open with RequireGitHub true and no token: want error, got nil")
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN is not set") {
		t.Fatalf("err = %v, want it to mention GITHUB_TOKEN is not set", err)
	}
}

func TestOpen_NoGitHubRequiredSucceedsWithEmptyToken(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	path := writeOpenConfig(t)

	cfg, deps, cleanup, err := Open(ProjectRef{}, path, Options{RequireGitHub: false})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cleanup()

	if cfg.Name != "planning" {
		t.Errorf("cfg.Name = %q, want %q", cfg.Name, "planning")
	}
	if deps.Store == nil {
		t.Fatal("deps.Store is nil")
	}
	if deps.GH == nil {
		t.Fatal("deps.GH is nil")
	}
}

func TestOpen_CleanupClosesTheDatabase(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	path := writeOpenConfig(t)

	_, deps, cleanup, err := Open(ProjectRef{}, path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	cleanup()

	if _, err := deps.Store.IssueStates("planning", "o/r"); err == nil {
		t.Fatal("query after cleanup: want an error because the database is closed, got nil")
	}
}

// noCallGH fails the test the moment any GitHub method is called. It proves a
// held lock stops RunTick before Tick ever reaches the GitHub client.
type noCallGH struct{ t *testing.T }

func (f *noCallGH) ListOpenIssues(context.Context, string, string) ([]ghub.Issue, error) {
	f.t.Fatal("ListOpenIssues called; RunTick should have returned before Tick started")
	return nil, nil
}

func (f *noCallGH) Issue(context.Context, string, string, int) (ghub.Issue, error) {
	f.t.Fatal("Issue called; a held lock must return before any GitHub call")
	return ghub.Issue{}, nil
}

func (f *noCallGH) PullRequest(context.Context, string, string, int) (ghub.PullRequest, error) {
	f.t.Fatal("PullRequest called; a held lock must return before any GitHub call")
	return ghub.PullRequest{}, nil
}

func (f *noCallGH) ListOpenPullRequests(context.Context, string, string) ([]ghub.PullRequest, error) {
	f.t.Fatal("ListOpenPullRequests called; RunTick should have returned before Tick started")
	return nil, nil
}

func (f *noCallGH) BehindBy(context.Context, string, string, string, string) (int, error) {
	f.t.Fatal("BehindBy called; RunTick should have returned before Tick started")
	return 0, nil
}

func (f *noCallGH) PostComment(context.Context, string, string, int, string) error {
	f.t.Fatal("PostComment called; RunTick should have returned before Tick started")
	return nil
}

func (f *noCallGH) EditLabels(context.Context, string, string, int, []string, []string) error {
	f.t.Fatal("EditLabels called; RunTick should have returned before Tick started")
	return nil
}

// TestRunTick_HeldLockReportsErrHeldAndDispatchesNothing proves the errorlint
// requirement directly: a caller must match this error with errors.Is, not ==,
// and a held lock must short-circuit before any dispatch work starts.
func TestRunTick_HeldLockReportsErrHeldAndDispatchesNothing(t *testing.T) {
	cfg := tickConfig(t)
	spawned := 0
	deps := newDeps(t, cfg, &noCallGH{t: t}, &spawned)

	held, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	_, err = RunTick(context.Background(), cfg, deps)
	if !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("RunTick error = %v, want errors.Is(err, lock.ErrHeld)", err)
	}
	if spawned != 0 {
		t.Fatalf("spawned = %d, want 0: a held lock must dispatch nothing", spawned)
	}
}

// writeProjectConfig lays out a real project -- <root>/.agent-utils/configs/loop.yaml
// -- whose checkout_base_dir and worktree_dir are RELATIVE, and returns the
// project root, the .agent-utils directory, and the configuration's path.
func writeProjectConfig(t *testing.T) (root, agentUtilsDir, configPath string) {
	t.Helper()
	root = t.TempDir()
	agentUtilsDir = filepath.Join(root, config.DirName)
	configs := filepath.Join(agentUtilsDir, "configs")
	if err := os.MkdirAll(configs, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(openTestYAML, ".", "build/worktrees", filepath.Join(root, "state"))
	configPath = filepath.Join(configs, "loop.yaml")
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, agentUtilsDir, configPath
}

// This is the bug. checkout_base_dir becomes the agent's working directory --
// Tick hands it to runner.Spawn, which sets it as cmd.Dir -- and worktree_dir
// is where every worktree is created. A relative value used raw resolves
// against whichever process happens to be running: the shell's directory for a
// `--name <project>` run started anywhere else, and ~/.agent-utils for the
// listener daemon, whose launchd plist sets WorkingDirectory there.
//
// So the same project, opened from two directories, must produce one absolute
// path: the one under the project root. Run from a directory that is NOT the
// project, and compare against a run from inside it.
func TestOpen_RelativeDirsResolveToTheProjectRootFromAnyWorkingDirectory(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	root, agentUtilsDir, configPath := writeProjectConfig(t)
	ref := ProjectRef{ID: "p1", Name: "example", Dir: agentUtilsDir}

	open := func() (checkout, worktrees string) {
		t.Helper()
		cfg, _, cleanup, err := Open(ref, configPath, Options{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer cleanup()
		return cfg.CheckoutBaseDir, cfg.WorktreeDir
	}

	// A `--name` run from an unrelated directory, e.g. the user's shell
	// elsewhere or the daemon's ~/.agent-utils.
	t.Chdir(t.TempDir())
	awayCheckout, awayWorktrees := open()

	// The same project, run from inside it -- the case that works today only
	// because the working directory happens to be right.
	t.Chdir(root)
	homeCheckout, homeWorktrees := open()

	if awayCheckout != homeCheckout {
		t.Errorf("checkout_base_dir depends on the working directory: %q away, %q inside the project",
			awayCheckout, homeCheckout)
	}
	if awayWorktrees != homeWorktrees {
		t.Errorf("worktree_dir depends on the working directory: %q away, %q inside the project",
			awayWorktrees, homeWorktrees)
	}
	if awayCheckout != root {
		t.Errorf("checkout_base_dir = %q, want the project root %q", awayCheckout, root)
	}
	if want := filepath.Join(root, "build", "worktrees"); awayWorktrees != want {
		t.Errorf("worktree_dir = %q, want %q", awayWorktrees, want)
	}
}

// A --config pointing outside any .agent-utils directory leaves the runner
// with no project root, so a relative path there has nothing to resolve
// against. It must say so rather than silently adopting the process's own
// working directory.
func TestOpen_RelativeDirWithNoProjectRootIsAnError(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	dir := t.TempDir()
	body := fmt.Sprintf(openTestYAML, ".", filepath.Join(dir, "wt"), filepath.Join(dir, "state"))
	path := filepath.Join(dir, "loop.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := Open(ProjectRef{}, path, Options{})
	if err == nil {
		t.Fatal("want an error for a relative checkout_base_dir with no project root, got nil")
	}
	if !strings.Contains(err.Error(), "checkout_base_dir") {
		t.Errorf("err = %v, want it to name checkout_base_dir", err)
	}
}

// The daemon's client is a *ghub.DeliveryCache, which implements ghub.Client
// and NOT ghub.EpicReader. Open must still produce a usable Deps.Epic, or the
// epic sweep is dead on the webhook path -- its primary driver -- while every
// test that injects its own reader stays green.
func TestOpenCarriesTheEpicReaderPastTheDeliveryCache(t *testing.T) {
	// The premise. If DeliveryCache ever grows the three methods, the explicit
	// Options.Epic field can go -- but until then, asserting on GH is a trap
	// that fails only in production.
	if _, ok := any((*ghub.DeliveryCache)(nil)).(ghub.EpicReader); ok {
		t.Fatal("DeliveryCache now implements EpicReader; this test's premise is stale")
	}

	t.Setenv(home.EnvVar, t.TempDir())
	path := writeOpenConfig(t)

	real := ghub.New("")
	_, deps, cleanup, err := Open(ProjectRef{}, path, Options{
		// Exactly what listener.access builds: the cache for GH, the
		// un-wrapped client for Epic.
		GH:   ghub.NewDeliveryCache(real),
		Epic: real,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cleanup()

	if deps.Epic == nil {
		t.Fatal("deps.Epic is nil; the epic sweep is dead on the webhook path")
	}
}

// The CLI path supplies neither, and must still get a reader.
func TestOpenBuildsAnEpicReaderWhenTheCallerSuppliesNone(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	path := writeOpenConfig(t)

	_, deps, cleanup, err := Open(ProjectRef{}, path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cleanup()

	if deps.Epic == nil {
		t.Fatal("deps.Epic is nil on the CLI path")
	}
}
