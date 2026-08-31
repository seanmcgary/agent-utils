package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mustGitDir resolves a worktree's real git directory, following the .git file
// indirection when there is one.
func mustGitDir(t *testing.T, path string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--absolute-git-dir")
	cmd.Dir = path
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse --absolute-git-dir: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// A crash during a git operation leaves index.lock behind, and every later git
// command in that worktree fails with "Unable to create '.../index.lock'". The
// process that held it is gone, so the file is pure debris.
//
// A worktree's .git is a FILE pointing into the main repository, so the lock is
// not where a naive join puts it. Resolving that indirection is the whole
// difficulty, and this fixture is a real linked worktree.
func TestClearStaleLocksRemovesIndexLockInAWorktree(t *testing.T) {
	repo := initRepo(t)
	wtDir := filepath.Join(t.TempDir(), "worktrees")
	m := New(repo, wtDir, "planning", "master")
	path, err := m.EnsureIssue(7)
	if err != nil {
		t.Fatal(err)
	}

	lock := filepath.Join(mustGitDir(t, path), "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	cleared, err := ClearStaleLocks(path)
	if err != nil {
		t.Fatalf("ClearStaleLocks: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("index.lock is still present: %v", err)
	}
	if len(cleared) != 1 {
		t.Errorf("cleared = %v, want the one lock it removed", cleared)
	}
}

// HEAD.lock blocks the ref update a checkout or a rebase ends with, so a crash
// mid-operation can leave this one instead of index.lock.
func TestClearStaleLocksRemovesHeadLock(t *testing.T) {
	repo := initRepo(t)
	wtDir := filepath.Join(t.TempDir(), "worktrees")
	m := New(repo, wtDir, "planning", "master")
	path, err := m.EnsureIssue(7)
	if err != nil {
		t.Fatal(err)
	}

	lock := filepath.Join(mustGitDir(t, path), "HEAD.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ClearStaleLocks(path); err != nil {
		t.Fatalf("ClearStaleLocks: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("HEAD.lock is still present: %v", err)
	}
}

// A plain checkout has a .git DIRECTORY. The primary checkout is one, so this
// is not a hypothetical shape.
func TestClearStaleLocksHandlesAPlainRepository(t *testing.T) {
	repo := initRepo(t)
	lock := filepath.Join(repo, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ClearStaleLocks(repo); err != nil {
		t.Fatalf("ClearStaleLocks: %v", err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("index.lock is still present: %v", err)
	}
}

// The agent's WORK is not debris. An in-progress rebase and uncommitted edits
// are what the retry agent is meant to find and resolve, so deleting them would
// silently discard the crashed run's output.
func TestClearStaleLocksLeavesRealStateAlone(t *testing.T) {
	repo := initRepo(t)
	wtDir := filepath.Join(t.TempDir(), "worktrees")
	m := New(repo, wtDir, "planning", "master")
	path, err := m.EnsureIssue(7)
	if err != nil {
		t.Fatal(err)
	}
	gitDir := mustGitDir(t, path)

	rebaseDir := filepath.Join(gitDir, "rebase-merge")
	if err := os.MkdirAll(rebaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	edit := filepath.Join(path, "f.txt")
	if err := os.WriteFile(edit, []byte("uncommitted work"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "index.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := ClearStaleLocks(path); err != nil {
		t.Fatalf("ClearStaleLocks: %v", err)
	}
	if _, err := os.Stat(rebaseDir); err != nil {
		t.Errorf("rebase state was destroyed: %v", err)
	}
	if got, err := os.ReadFile(edit); err != nil || string(got) != "uncommitted work" {
		t.Errorf("uncommitted work was destroyed: %q %v", got, err)
	}
}

// Nothing to clear is the ordinary case and must be silent, not an error.
func TestClearStaleLocksIsQuietWhenThereIsNothingToClear(t *testing.T) {
	repo := initRepo(t)
	cleared, err := ClearStaleLocks(repo)
	if err != nil {
		t.Fatalf("ClearStaleLocks: %v", err)
	}
	if len(cleared) != 0 {
		t.Errorf("cleared = %v, want nothing", cleared)
	}
}

// A worktree removed since the dispatch died is not an error either: the reap
// still has a row to retire, and failing here would abandon it.
func TestClearStaleLocksAcceptsAMissingWorktree(t *testing.T) {
	cleared, err := ClearStaleLocks(filepath.Join(t.TempDir(), "gone"))
	if err != nil {
		t.Errorf("a missing worktree must not be an error: %v", err)
	}
	if len(cleared) != 0 {
		t.Errorf("cleared = %v, want nothing", cleared)
	}
}

// A path that is not a repository at all must not be treated as one. The
// caller hands over whatever the dispatch row's worktree column said, and a
// stale row can name a directory that has since become something else.
func TestClearStaleLocksAcceptsADirectoryThatIsNotARepository(t *testing.T) {
	if _, err := ClearStaleLocks(t.TempDir()); err != nil {
		t.Errorf("a non-repository must not be an error: %v", err)
	}
}
