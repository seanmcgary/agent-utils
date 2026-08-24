package loopcmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// cleanupConfig is tickConfig with a real git repository wired in as the
// checkout, so deps.WT.EnsureIssue and deps.WT.EnsurePR can build real
// worktrees for CleanupClosedPR to remove.
func cleanupConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := tickConfig(t)
	dir := t.TempDir()
	cfg.CheckoutBaseDir = filepath.Join(dir, "checkout")
	cfg.WorktreeDir = filepath.Join(dir, "worktrees")
	cfg.DefaultBranch = "master"

	run := func(wd string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = wd
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(cfg.CheckoutBaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	run(cfg.CheckoutBaseDir, "init", "-b", "master")
	if err := os.WriteFile(filepath.Join(cfg.CheckoutBaseDir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(cfg.CheckoutBaseDir, "add", ".")
	run(cfg.CheckoutBaseDir, "commit", "-m", "initial")

	bare := t.TempDir()
	cmd := exec.Command("git", "init", "--bare", bare)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	run(cfg.CheckoutBaseDir, "remote", "add", "origin", bare)
	run(cfg.CheckoutBaseDir, "push", "-u", "origin", "master")

	// A second branch, pushed to origin, is what EnsurePR checks out for the
	// pull request's worktree.
	run(cfg.CheckoutBaseDir, "checkout", "-b", "pr-branch")
	run(cfg.CheckoutBaseDir, "push", "-u", "origin", "pr-branch")
	run(cfg.CheckoutBaseDir, "checkout", "master")

	return cfg
}

// closingPRFixture is a trusted pull request closing issue, with a head
// branch that actually exists in cleanupConfig's fixture repository, so
// EnsurePR can build a real worktree from it.
func closingPRFixture(pr, issue int) ghub.PullRequest {
	return ghub.PullRequest{
		Number:  pr,
		Body:    fmt.Sprintf("Closes #%d", issue),
		HeadRef: "pr-branch",
		BaseRef: "master",
		Trusted: true,
	}
}

// The pull request closes no issue: only the pr- worktree exists, and only
// it is removed.
func TestCleanupClosedPRRemovesThePRWorktreeWhenThePullRequestClosesNoIssue(t *testing.T) {
	cfg := cleanupConfig(t)
	gh := &fakeGH{prs: []ghub.PullRequest{{
		Number: 11, Body: "no closing reference here", HeadRef: "pr-branch",
		BaseRef: "master", Trusted: true,
	}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	prPath, err := deps.WT.EnsurePR(11, "pr-branch")
	if err != nil {
		t.Fatal(err)
	}

	if err := CleanupClosedPR(context.Background(), cfg, deps, 11); err != nil {
		t.Fatalf("CleanupClosedPR: %v", err)
	}
	if _, err := os.Stat(prPath); !os.IsNotExist(err) {
		t.Errorf("pr worktree still exists: %v", err)
	}
}

// A merged close removes both worktrees. CleanupClosedPR does not
// distinguish merged from unmerged -- the operator's decision was "on any
// close" -- but the merged case is the common one and gets its own test.
func TestCleanupClosedPRRemovesBothWorktreesOnAMergedClose(t *testing.T) {
	cfg := cleanupConfig(t)
	gh := &fakeGH{prs: []ghub.PullRequest{closingPRFixture(11, 1)}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	issuePath, err := deps.WT.EnsureIssue(1)
	if err != nil {
		t.Fatal(err)
	}
	prPath, err := deps.WT.EnsurePR(11, "pr-branch")
	if err != nil {
		t.Fatal(err)
	}

	if err := CleanupClosedPR(context.Background(), cfg, deps, 11); err != nil {
		t.Fatalf("CleanupClosedPR: %v", err)
	}
	if _, err := os.Stat(issuePath); !os.IsNotExist(err) {
		t.Errorf("issue worktree still exists: %v", err)
	}
	if _, err := os.Stat(prPath); !os.IsNotExist(err) {
		t.Errorf("pr worktree still exists: %v", err)
	}
}

// An unmerged close removes both worktrees too. The operator declined the
// alternative (remove only pr-<N> on an unmerged close) in favour of
// reclaiming the disk; CleanupClosedPR is never told whether the pull
// request merged, so its behavior here must be identical to the merged case.
func TestCleanupClosedPRRemovesBothWorktreesOnAnUnmergedClose(t *testing.T) {
	cfg := cleanupConfig(t)
	gh := &fakeGH{prs: []ghub.PullRequest{closingPRFixture(11, 1)}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	issuePath, err := deps.WT.EnsureIssue(1)
	if err != nil {
		t.Fatal(err)
	}
	prPath, err := deps.WT.EnsurePR(11, "pr-branch")
	if err != nil {
		t.Fatal(err)
	}

	if err := CleanupClosedPR(context.Background(), cfg, deps, 11); err != nil {
		t.Fatalf("CleanupClosedPR: %v", err)
	}
	if _, err := os.Stat(issuePath); !os.IsNotExist(err) {
		t.Errorf("issue worktree still exists: %v", err)
	}
	if _, err := os.Stat(prPath); !os.IsNotExist(err) {
		t.Errorf("pr worktree still exists: %v", err)
	}
}

// A live dispatch for the issue this pull request closes cancels the WHOLE
// cleanup, not just the issue worktree: the two checkouts are one piece of
// work.
func TestCleanupClosedPRRemovesNeitherWorktreeWhenTheIssueHasALiveDispatch(t *testing.T) {
	cfg := cleanupConfig(t)
	gh := &fakeGH{prs: []ghub.PullRequest{closingPRFixture(11, 1)}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	// newDeps defaults IsAlive to true; this dispatch is "live".

	issuePath, err := deps.WT.EnsureIssue(1)
	if err != nil {
		t.Fatal(err)
	}
	prPath, err := deps.WT.EnsurePR(11, "pr-branch")
	if err != nil {
		t.Fatal(err)
	}
	liveDispatch(t, cfg, deps, store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, Kind: store.KindStart, SessionID: "s1",
	})

	if err := CleanupClosedPR(context.Background(), cfg, deps, 11); err != nil {
		t.Fatalf("CleanupClosedPR: %v", err)
	}
	if _, err := os.Stat(issuePath); err != nil {
		t.Errorf("issue worktree was removed: %v", err)
	}
	if _, err := os.Stat(prPath); err != nil {
		t.Errorf("pr worktree was removed even though the issue has a live dispatch: %v", err)
	}
}

// A row younger than pidGracePeriod carrying pid 0 must be treated as live,
// exactly as reapDead treats it -- not as a dead row IsAlive happens to
// answer false for. If cleanup used Reset's bare IsAlive rule instead, it
// would delete a worktree out from under an agent whose pid had not been
// written yet.
func TestCleanupClosedPRRemovesNeitherWorktreeWhenARowIsYoungerThanPidGracePeriod(t *testing.T) {
	cfg := cleanupConfig(t)
	gh := &fakeGH{prs: []ghub.PullRequest{closingPRFixture(11, 1)}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	// IsAlive answers false: only the grace period can keep this row live.
	deps.IsAlive = func(int, int64) bool { return false }

	issuePath, err := deps.WT.EnsureIssue(1)
	if err != nil {
		t.Fatal(err)
	}
	prPath, err := deps.WT.EnsurePR(11, "pr-branch")
	if err != nil {
		t.Fatal(err)
	}
	// Created moments ago via CreateDispatch, which stamps StartedAt with the
	// real wall clock; deps.Now defaults to time.Now in newDeps, so this row
	// is well inside pidGracePeriod with pid still 0.
	if _, err := deps.Store.CreateDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, Kind: store.KindStart, SessionID: "s1",
	}); err != nil {
		t.Fatal(err)
	}

	if err := CleanupClosedPR(context.Background(), cfg, deps, 11); err != nil {
		t.Fatalf("CleanupClosedPR: %v", err)
	}
	if _, err := os.Stat(issuePath); err != nil {
		t.Errorf("issue worktree was removed: %v", err)
	}
	if _, err := os.Stat(prPath); err != nil {
		t.Errorf("pr worktree was removed even though its row is inside pidGracePeriod: %v", err)
	}
}

// Removal is not blocked by uncommitted changes -- the operator chose to
// reclaim the disk regardless -- but it must be logged so the loss is
// visible afterwards. This test proves the removal side; the warning is
// exercised by inspection (removeWorktree calls deps.WT.Dirty before every
// removal).
func TestCleanupClosedPRRemovesADirtyWorktree(t *testing.T) {
	cfg := cleanupConfig(t)
	gh := &fakeGH{prs: []ghub.PullRequest{closingPRFixture(11, 1)}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	issuePath, err := deps.WT.EnsureIssue(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(issuePath, "uncommitted.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err := deps.WT.Dirty(issuePath)
	if err != nil {
		t.Fatal(err)
	}
	if !dirty {
		t.Fatal("fixture is not actually dirty; the test proves nothing")
	}

	if err := CleanupClosedPR(context.Background(), cfg, deps, 11); err != nil {
		t.Fatalf("CleanupClosedPR: %v", err)
	}
	if _, err := os.Stat(issuePath); !os.IsNotExist(err) {
		t.Errorf("dirty issue worktree was not removed: %v", err)
	}
}

// A dead dispatch row -- outside pidGracePeriod and not alive -- must not
// block cleanup, the way a live one does.
func TestCleanupClosedPRIgnoresADeadDispatchRow(t *testing.T) {
	cfg := cleanupConfig(t)
	gh := &fakeGH{prs: []ghub.PullRequest{closingPRFixture(11, 1)}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.IsAlive = func(int, int64) bool { return false }
	// A fixed, ancient clock puts every row outside pidGracePeriod.
	deps.Now = func() time.Time { return time.Now().Add(time.Hour) }

	issuePath, err := deps.WT.EnsureIssue(1)
	if err != nil {
		t.Fatal(err)
	}
	prPath, err := deps.WT.EnsurePR(11, "pr-branch")
	if err != nil {
		t.Fatal(err)
	}
	liveDispatch(t, cfg, deps, store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, Kind: store.KindStart, SessionID: "s1",
	})

	if err := CleanupClosedPR(context.Background(), cfg, deps, 11); err != nil {
		t.Fatalf("CleanupClosedPR: %v", err)
	}
	if _, err := os.Stat(issuePath); !os.IsNotExist(err) {
		t.Errorf("issue worktree still exists: %v", err)
	}
	if _, err := os.Stat(prPath); !os.IsNotExist(err) {
		t.Errorf("pr worktree still exists: %v", err)
	}
}
