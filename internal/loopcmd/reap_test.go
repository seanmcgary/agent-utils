package loopcmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// reapFixture is a loop whose agents run in per-issue worktrees, with one
// dispatch row and its issue state already written. The worktree directory is
// created directly rather than through git: the reap only has to find the lock
// files, and a real clone per test would pay seconds for nothing.
func reapFixture(t *testing.T, kind string, number, pr int) (*config.Config, Deps, string) {
	t.Helper()
	cfg := tickConfig(t)
	cfg.Agent.Worktree = config.WorktreePerIssue
	spawned := 0
	deps := newDeps(t, cfg, &fakeGH{}, &spawned)
	deps.IsAlive = func(int, int64) bool { return false }

	path := deps.WT.PathForIssue(number)
	if kind == store.KindTend {
		path = deps.WT.PathForPR(pr)
	}
	gitDir := filepath.Join(path, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "index.lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	id, err := deps.Store.CreateDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: number, Kind: kind,
		SessionID: "s", PRNumber: pr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.SetDispatchProcess(id, 4242, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.PutIssueState(store.IssueState{
		Loop: cfg.Name, Repo: cfg.Repo, Number: number, SessionID: "s",
		SessionStarted: true, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return cfg, deps, filepath.Join(gitDir, "index.lock")
}

// runReap drives reapDead over whatever the store currently has running.
func runReap(t *testing.T, cfg *config.Config, deps Deps) Summary {
	t.Helper()
	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	states, err := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	var sum Summary
	if _, err := reapDead(cfg, deps, running, states, time.Now(), &sum); err != nil {
		t.Fatalf("reapDead: %v", err)
	}
	return sum
}

// A machine crash kills the agent mid-git-operation, leaving index.lock in its
// worktree. The retry lands in that same worktree, so without this every git
// command it runs fails and the retry burns to the cap on debris no agent can
// clear.
func TestReapClearsStaleGitLocksInTheDispatchsWorktree(t *testing.T) {
	cfg, deps, lock := reapFixture(t, store.KindStart, 1, 0)

	if sum := runReap(t, cfg, deps); sum.Orphans != 1 {
		t.Fatalf("Orphans = %d, want 1", sum.Orphans)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("index.lock survived the reap: %v", err)
	}
}

// A tend runs in the PULL REQUEST's worktree, not the issue's. Clearing the
// issue's would leave the tend's own debris in place and delete nothing that
// was in the way.
func TestReapClearsLocksInATendsPullRequestWorktree(t *testing.T) {
	cfg, deps, lock := reapFixture(t, store.KindTend, 1, 20)

	runReap(t, cfg, deps)
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("index.lock survived the reap of a tend: %v", err)
	}
}

// A LIVE agent's worktree must never be touched: git is using those locks to
// make its own operations safe, and removing one corrupts the run in progress.
func TestReapLeavesALiveAgentsLocksAlone(t *testing.T) {
	cfg, deps, lock := reapFixture(t, store.KindStart, 1, 0)
	deps.IsAlive = func(int, int64) bool { return true }

	if sum := runReap(t, cfg, deps); sum.Orphans != 0 {
		t.Fatalf("Orphans = %d, want 0: the agent is alive", sum.Orphans)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("a live agent's index.lock was removed: %v", err)
	}
}

// A loop that runs agents in the primary checkout has no per-dispatch worktree
// to clear, and clearing the checkout's own locks could hit a live git process
// that has nothing to do with this loop.
func TestReapClearsNothingWhenTheLoopUsesNoWorktrees(t *testing.T) {
	cfg, deps, _ := reapFixture(t, store.KindStart, 1, 0)
	cfg.Agent.Worktree = config.WorktreeNone
	lock := filepath.Join(cfg.CheckoutBaseDir, ".git", "index.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	runReap(t, cfg, deps)
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("the primary checkout's index.lock was removed: %v", err)
	}
}

// The reap's real job is the row and the retry flag. Debris that cannot be
// cleared -- a permission problem, a path that is no longer a repository --
// must not abandon that, or the issue is stranded holding an in-flight label
// with no agent and no failure recorded.
func TestReapStillRetiresTheRowWhenTheWorktreeIsGone(t *testing.T) {
	cfg, deps, lock := reapFixture(t, store.KindStart, 1, 0)
	if err := os.RemoveAll(filepath.Dir(filepath.Dir(lock))); err != nil {
		t.Fatal(err)
	}

	if sum := runReap(t, cfg, deps); sum.Orphans != 1 {
		t.Fatalf("Orphans = %d, want 1 even with the worktree gone", sum.Orphans)
	}
	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 0 {
		t.Errorf("running = %d, want the dead row retired", len(running))
	}
	states, err := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if !states[1].NeedsRetry {
		t.Error("the issue was not queued for retry")
	}
}
