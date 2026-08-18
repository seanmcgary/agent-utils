package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "master")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "initial")

	// EnsureIssue starts worktrees from origin/<default_branch>, so the fixture
	// needs a real origin to resolve that ref.
	bare := t.TempDir()
	cmd := exec.Command("git", "init", "--bare", bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	run("remote", "add", "origin", bare)
	run("push", "-u", "origin", "master")
	return dir
}

func TestEnsureIssueCreatesWorktreeAndIsIdempotent(t *testing.T) {
	repo := initRepo(t)
	wtDir := filepath.Join(t.TempDir(), "worktrees")
	m := New(repo, wtDir, "planning", "master")

	path, err := m.EnsureIssue(42)
	if err != nil {
		t.Fatalf("EnsureIssue: %v", err)
	}
	want := filepath.Join(wtDir, "planning", "issue-42")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if _, err := os.Stat(filepath.Join(path, "f.txt")); err != nil {
		t.Errorf("worktree does not contain the repository file: %v", err)
	}

	again, err := m.EnsureIssue(42)
	if err != nil {
		t.Fatalf("second EnsureIssue: %v", err)
	}
	if again != path {
		t.Errorf("second call returned %q, want %q", again, path)
	}
}

func TestRemoveDeletesWorktree(t *testing.T) {
	repo := initRepo(t)
	wtDir := filepath.Join(t.TempDir(), "worktrees")
	m := New(repo, wtDir, "planning", "master")

	path, err := m.EnsureIssue(7)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("worktree still exists after Remove")
	}
}

func TestPathHelpers(t *testing.T) {
	m := New("/repo", "/wt", "exec", "main")
	if got := m.PathForIssue(3); got != "/wt/exec/issue-3" {
		t.Errorf("PathForIssue = %q", got)
	}
	if got := m.PathForPR(9); got != "/wt/exec/pr-9" {
		t.Errorf("PathForPR = %q", got)
	}
}
