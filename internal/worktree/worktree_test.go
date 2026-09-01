package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitOut runs git in dir and returns its trimmed stdout. Tests use it to read
// the fixture's own state, so a failure is the test's bug and fails it.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return strings.TrimSpace(string(out))
}

// commitOnBranch adds an empty commit to a branch of a BARE repository, which
// stands in for another writer pushing while a tend pass runs. It uses
// plumbing because a bare repository has no working tree to commit from.
func commitOnBranch(t *testing.T, repo, branch, message string) {
	t.Helper()
	tree := gitOut(t, repo, "rev-parse", branch+"^{tree}")
	commit := gitOut(t, repo, "commit-tree", tree, "-p", branch, "-m", message)
	gitOut(t, repo, "update-ref", "refs/heads/"+branch, commit)
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir, _ := initRepoWithOrigin(t)
	return dir
}

func initRepoWithOrigin(t *testing.T) (repo, origin string) {
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
	// A rebase makes commits of its own, and it reads the identity from config
	// rather than from the environment the fixture sets per command. Signing is
	// disabled for the same reason: the developer's global config must not
	// decide whether these tests pass.
	run("config", "user.name", "t")
	run("config", "user.email", "t@e")
	run("config", "commit.gpgsign", "false")
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
	return dir, bare
}

// newTestManagerWithOrigin builds a repository with a master branch two
// commits ahead of origin/feature, and a feature branch with a commit of its
// own that master lacks -- so ahead and behind differ and an implementation
// that swaps the two directions cannot pass.
func newTestManagerWithOrigin(t *testing.T) *Manager {
	t.Helper()
	m, _ := newTestManagerWithRemote(t)
	return m
}

// newTestManagerWithRemote is newTestManagerWithOrigin, returning the bare
// origin as well so a test can inspect the pushed refs and can simulate
// another writer moving the branch.
func newTestManagerWithRemote(t *testing.T) (*Manager, string) {
	t.Helper()
	dir, origin := initRepoWithOrigin(t)
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

	return New(dir, filepath.Join(t.TempDir(), "worktrees"), "planning", "master"), origin
}

// newTestManagerWithConflict builds a repository whose feature branch and
// master edit the SAME line of the same file, so replaying feature onto master
// must stop. It is the fixture for the path that hands the pull request to an
// agent.
func newTestManagerWithConflict(t *testing.T) (*Manager, string) {
	t.Helper()
	dir, origin := initRepoWithOrigin(t)
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
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("checkout", "-b", "feature")
	write("the feature's line\n")
	run("add", ".")
	run("commit", "-m", "feature edits the shared line")
	run("push", "-u", "origin", "feature")

	run("checkout", "master")
	write("master's line\n")
	run("add", ".")
	run("commit", "-m", "master edits the shared line")
	run("push", "origin", "master")

	return New(dir, filepath.Join(t.TempDir(), "worktrees"), "planning", "master"), origin
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

// A clean replay pushes, and the remote then holds the rebased commit.
func TestRebaseAndPushWithLease(t *testing.T) {
	m, origin := newTestManagerWithRemote(t)
	path, err := m.EnsurePR(9, "feature")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := m.HeadSHA(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Rebase(context.Background(), path, "master"); err != nil {
		t.Fatal(err)
	}
	if err := m.PushWithLease(context.Background(), path, "feature", lease); err != nil {
		t.Fatal(err)
	}
	if gitOut(t, origin, "rev-parse", "feature") == lease {
		t.Error("the remote branch did not move")
	}
	if got := gitOut(t, origin, "rev-parse", "feature"); got != gitOut(t, path, "rev-parse", "HEAD") {
		t.Errorf("origin/feature = %s, want the rebased head", got)
	}
}

// The lease is the guard. A remote that moved after the fetch refuses the
// push, and the branch keeps the other writer's commit.
func TestPushWithLeaseRefusesWhenTheRemoteMoved(t *testing.T) {
	m, origin := newTestManagerWithRemote(t)
	path, err := m.EnsurePR(9, "feature")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := m.HeadSHA(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Rebase(context.Background(), path, "master"); err != nil {
		t.Fatal(err)
	}

	// Somebody else pushes to the branch while this pass runs.
	commitOnBranch(t, origin, "feature", "someone else's work")
	before := gitOut(t, origin, "rev-parse", "feature")

	if err := m.PushWithLease(context.Background(), path, "feature", lease); err == nil {
		t.Fatal("the push must be refused when the remote moved")
	}
	if got := gitOut(t, origin, "rev-parse", "feature"); got != before {
		t.Error("the refused push still changed the branch")
	}
}

// The lease is a rev expression git resolves locally, so the dangerous shapes
// are the ones that RESOLVE: an abbreviated id, an uppercase id, and a ref
// name. Each was measured to push and move the branch with the guard removed,
// because each resolves to the tip the lease is supposed to be pinning. The
// empty and trailing-newline cases are here for completeness -- git refuses
// those itself -- and the subtests assert the branch did not move, not merely
// that an error came back.
func TestPushWithLeaseRefusesAMalformedLease(t *testing.T) {
	m, origin := newTestManagerWithRemote(t)
	path, err := m.EnsurePR(9, "feature")
	if err != nil {
		t.Fatal(err)
	}
	full, err := m.HeadSHA(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Rebase(context.Background(), path, "master"); err != nil {
		t.Fatal(err)
	}
	before := gitOut(t, origin, "rev-parse", "feature")

	for _, c := range []struct{ name, lease string }{
		{"empty", ""},
		{"abbreviated", full[:8]},
		{"a ref name", "refs/heads/feature"},
		{"uppercase", strings.ToUpper(full)},
		{"padded", full + "\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := m.PushWithLease(context.Background(), path, "feature", c.lease); err == nil {
				t.Error("the push must be refused")
			}
			if got := gitOut(t, origin, "rev-parse", "feature"); got != before {
				t.Error("the branch moved")
				// Put the fixture back, so one case that pushes does not turn
				// every later case into a false failure.
				gitOut(t, origin, "update-ref", "refs/heads/feature", before)
			}
		})
	}
}

// An unsafe ref never reaches git, on either the push or the rebase.
func TestRebaseAndPushRejectAnUnsafeRef(t *testing.T) {
	m, _ := newTestManagerWithRemote(t)
	path, err := m.EnsurePR(9, "feature")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := m.HeadSHA(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Rebase(context.Background(), path, "-oops"); err == nil {
		t.Error("Rebase must refuse an unsafe base ref")
	}
	if err := m.PushWithLease(context.Background(), path, "--delete", lease); err == nil {
		t.Error("PushWithLease must refuse an unsafe head ref")
	}
}

// A conflict leaves no rebase in progress. A worktree stuck mid-rebase would
// fail every later pass for that pull request.
func TestAbortRebaseLeavesACleanWorktree(t *testing.T) {
	m, _ := newTestManagerWithConflict(t)
	path, err := m.EnsurePR(9, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Rebase(context.Background(), path, "master"); err == nil {
		t.Fatal("this fixture must conflict")
	}
	if err := m.AbortRebase(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if out := gitOut(t, path, "status", "--porcelain"); out != "" {
		t.Errorf("worktree is not clean after the abort: %q", out)
	}
	dirty, err := m.DirtyCtx(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Error("the aborted worktree still reports dirty")
	}
}

// A conflicted rebase reports the file it conflicted on, before the abort
// clears it. The backoff this feeds needs it read in exactly that window.
func TestConflictedPathsReportsAConflictedRebase(t *testing.T) {
	m, _ := newTestManagerWithConflict(t)
	path, err := m.EnsurePR(9, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Rebase(context.Background(), path, "master"); err == nil {
		t.Fatal("this fixture must conflict")
	}
	got, err := m.ConflictedPaths(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "f.txt" {
		t.Errorf("ConflictedPaths = %v, want [f.txt]", got)
	}
}

// A clean worktree reports no conflicted paths and no error, so the caller
// can tell "nothing to report" from "the read failed".
func TestConflictedPathsOnACleanWorktree(t *testing.T) {
	m, _ := newTestManagerWithRemote(t)
	path, err := m.EnsurePR(9, "feature")
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.ConflictedPaths(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("ConflictedPaths on a clean worktree = %v, want empty", got)
	}
}

// The caller aborts unconditionally after any Rebase failure, because it
// cannot tell a conflict from a command that never ran. Nothing to abort must
// therefore not be an error.
func TestAbortRebaseWithNoRebaseInProgress(t *testing.T) {
	m, _ := newTestManagerWithRemote(t)
	path, err := m.EnsurePR(9, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.AbortRebase(context.Background(), path); err != nil {
		t.Errorf("AbortRebase with no rebase in progress: %v", err)
	}
}

// A deadline that has passed must stop the command rather than hang the
// daemon's loop lock.
func TestRebaseRespectsTheContext(t *testing.T) {
	m, _ := newTestManagerWithRemote(t)
	path, err := m.EnsurePR(9, "feature")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Rebase(ctx, path, "master"); err == nil {
		t.Error("a cancelled context must fail the rebase")
	}
	if _, err := m.HeadSHA(ctx, path); err == nil {
		t.Error("a cancelled context must fail HeadSHA")
	}
	if _, err := m.EnsurePRCtx(ctx, 9, "feature"); err == nil {
		t.Error("a cancelled context must fail EnsurePRCtx")
	}
}

// HeadSHA is the source of the lease, so it must return a bare object id and
// nothing else -- no trailing newline, and none of the advice git prints on
// stderr while succeeding.
func TestHeadSHAReturnsABareObjectID(t *testing.T) {
	m, _ := newTestManagerWithRemote(t)
	path, err := m.EnsurePR(9, "feature")
	if err != nil {
		t.Fatal(err)
	}
	// git writes this hint to stderr on every command that resolves a ref.
	gitOut(t, path, "config", "advice.detachedHead", "true")

	sha, err := m.HeadSHA(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if want := gitOut(t, path, "rev-parse", "HEAD"); sha != want {
		t.Errorf("HeadSHA = %q, want %q", sha, want)
	}
	if !leaseSHA.MatchString(sha) {
		t.Errorf("HeadSHA = %q, which PushWithLease would refuse", sha)
	}
}

// A dead context must not look like a clean worktree.
//
// The probes for the rebase state directory fail on an expired context -- the
// same expired context that would have killed the rebase and left that
// directory behind. Returning nil there would tell the caller the abort
// succeeded, and its "abort failed, destroy the worktree" path would never run
// in the one situation it exists for.
func TestAbortRebaseFailsOnADeadContext(t *testing.T) {
	m, _ := newTestManagerWithConflict(t)
	path, err := m.EnsurePR(9, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Rebase(context.Background(), path, "master"); err == nil {
		t.Fatal("this fixture must conflict")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.AbortRebase(ctx, path); err == nil {
		t.Error("AbortRebase must not report success on a cancelled context")
	}

	// The rebase really was still in progress, so the case above is the one
	// described and not a worktree that happened to be clean already.
	if err := m.AbortRebase(context.Background(), path); err != nil {
		t.Fatalf("abort with a live context: %v", err)
	}
	if out := gitOut(t, path, "status", "--porcelain"); out != "" {
		t.Errorf("worktree is not clean after the abort: %q", out)
	}
}
