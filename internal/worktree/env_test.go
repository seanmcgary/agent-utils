package worktree

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Manager.Env must reach both git paths: the bounded one the automatic rebase
// and fetch use, and the unbounded one worktree creation uses. In app mode it
// carries the credential helper and bot identity, so a path that dropped it
// would fetch or push as whoever the machine's own git config names.
func TestManagerEnvReachesGit(t *testing.T) {
	repo := initRepo(t)
	m := New(repo, filepath.Join(t.TempDir(), "worktrees"), "planning", "master")
	path, err := m.EnsureIssue(7)
	if err != nil {
		t.Fatalf("EnsureIssue: %v", err)
	}

	m.Env = []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=acme-agent[bot]"}
	got, err := m.gitStdout(context.Background(), path, "config", "user.name")
	if err != nil || strings.TrimSpace(got) != "acme-agent[bot]" {
		t.Errorf("gitStdout path: user.name = %q, %v", got, err)
	}
	out, err := m.gitOutput(path, "config", "user.name")
	if err != nil || strings.TrimSpace(out) != "acme-agent[bot]" {
		t.Errorf("gitOutput path: user.name = %q, %v", out, err)
	}
}

// With no Env, git sees the repository's own configuration, as it always has.
func TestManagerWithoutEnvUsesTheRepositoryConfig(t *testing.T) {
	repo := initRepo(t)
	m := New(repo, filepath.Join(t.TempDir(), "worktrees"), "planning", "master")

	got, err := m.gitStdout(context.Background(), repo, "config", "user.name")
	if err != nil || strings.TrimSpace(got) != "t" {
		t.Errorf("user.name = %q, %v; want the fixture's own %q", got, err, "t")
	}
}
