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

// newTestManagerWithOrigin builds a repository with a master branch two
// commits ahead of origin/feature, and a feature branch with a commit of its
// own that master lacks -- so ahead and behind differ and an implementation
// that swaps the two directions cannot pass.
func newTestManagerWithOrigin(t *testing.T) *Manager {
	t.Helper()
	dir := initRepo(t)
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

	run("checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "feature work")
	run("push", "-u", "origin", "feature")

	run("checkout", "master")
	for i := 0; i < 2; i++ {
		name := filepath.Join(dir, "m.txt")
		if err := os.WriteFile(name, []byte{byte('a' + i)}, 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", ".")
		run("commit", "-m", "master work")
	}
	run("push", "origin", "master")

	return New(dir, filepath.Join(t.TempDir(), "worktrees"), "planning", "master")
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

// The periodic tend pass counts what the base has and the head does not, with
// no GitHub call. A branch the prune removed is not an error and not behind:
// it is a pull request whose branch is gone, and the pass must skip it.
func TestBehindLocal(t *testing.T) {
	m := newTestManagerWithOrigin(t) // origin/master and origin/feature exist

	behind, known, err := m.BehindLocal("feature", "master")
	if err != nil {
		t.Fatal(err)
	}
	if !known {
		t.Fatal("known = false, want true for a branch that resolves")
	}
	if behind != 2 {
		t.Errorf("behind = %d, want 2", behind)
	}

	_, known, err = m.BehindLocal("branch-that-was-pruned", "master")
	if err != nil {
		t.Fatalf("a missing ref must not be an error: %v", err)
	}
	if known {
		t.Error("known = true for a ref that does not resolve")
	}
}

// An unsafe ref never reaches git.
func TestBehindLocalRejectsAnUnsafeRef(t *testing.T) {
	m := newTestManagerWithOrigin(t)
	if _, _, err := m.BehindLocal("-oops", "master"); err == nil {
		t.Error("an unsafe ref must be refused")
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
