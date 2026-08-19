package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
