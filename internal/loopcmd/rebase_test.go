package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/seanmcgary/agent-utils/internal/worktree"
)

// rebaseLoop is the loop every fixture below belongs to. It is a constant
// because the dispatch rows the record assertions read are keyed by it.
const rebaseLoop = "execution"

// fakeGit is a RebaseGit that touches no repository. It exists because the one
// property worth testing here -- which branch of gitRebase runs -- is decided
// by whether git succeeds, and making a real force-push FAIL requires a remote
// to fail against.
type fakeGit struct {
	// dirty, rebaseErr and pushErr choose the branch under test.
	dirty      bool
	rebaseErr  error
	pushErr    error
	abortErr   error
	ensureErr  error
	headSHAErr error

	// abortNeedsLiveCtx makes the abort fail on a dead context and succeed on
	// a live one, which is how the real thing behaves: the worktree helpers
	// use exec.CommandContext, and an expired context fails the command
	// without running git. It is the difference between the cleanup repairing
	// the worktree and the cleanup being unable to run at all.
	abortNeedsLiveCtx bool

	// cleanupSawDeadCtx records that a cleanup command was handed a context
	// that had already expired. The removal path must never be reached that
	// way: "worktree remove" fails, os.RemoveAll succeeds because it takes no
	// context, and "worktree prune" fails, which strands the registration and
	// kills every later worktree add at that path.
	cleanupSawDeadCtx bool

	// The counters. Each is the evidence for a property the code must hold:
	// pushes for "nothing was pushed after a conflict", aborts for "the
	// worktree is never left mid-rebase", removes for "a failed abort destroys
	// it".
	rebases int
	aborts  int
	pushes  int
	removes int

	// ensured records that the worktree was refreshed, and dirtyBefore records
	// whether the dirty check ran before that refresh. EnsurePRCtx detaches
	// HEAD, which flattens exactly the state the dirty check looks for, so the
	// ORDER is the guard, not the presence of the check.
	ensured     bool
	dirtyBefore bool

	// lease is the value handed to PushWithLease. It must be the head read
	// after the refresh, not before.
	lease string
}

func (g *fakeGit) PathForPR(number int) string { return "/wt/pr" }

func (g *fakeGit) EnsurePRCtx(context.Context, int, string) (string, error) {
	if g.ensureErr != nil {
		return "", g.ensureErr
	}
	g.ensured = true
	return "/wt/pr", nil
}

func (g *fakeGit) DirtyCtx(context.Context, string) (bool, error) {
	if !g.ensured {
		g.dirtyBefore = true
	}
	return g.dirty, nil
}

// HeadSHA answers a different id before and after the refresh, so a lease read
// too early is visible in the pushed value rather than passing silently.
func (g *fakeGit) HeadSHA(context.Context, string) (string, error) {
	if g.headSHAErr != nil {
		return "", g.headSHAErr
	}
	if g.ensured {
		return "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil
	}
	return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
}

func (g *fakeGit) Rebase(context.Context, string, string) error {
	g.rebases++
	return g.rebaseErr
}

func (g *fakeGit) AbortRebase(ctx context.Context, _ string) error {
	g.aborts++
	if ctx.Err() != nil {
		g.cleanupSawDeadCtx = true
		return ctx.Err()
	}
	if g.abortNeedsLiveCtx {
		return nil
	}
	return g.abortErr
}

func (g *fakeGit) RemoveCtx(ctx context.Context, _ string) error {
	g.removes++
	if ctx.Err() != nil {
		g.cleanupSawDeadCtx = true
		return ctx.Err()
	}
	return nil
}

// PushWithLease mirrors the real lease check. Without it a regression in the
// lease read would show up only in an equality assertion in one test, while
// production refuses the push outright.
func (g *fakeGit) PushWithLease(_ context.Context, _, _, lease string) error {
	g.pushes++
	g.lease = lease
	if !fakeLeaseSHA.MatchString(lease) {
		return fmt.Errorf("refusing to push with lease %q, which is not a full object id", lease)
	}
	return g.pushErr
}

// fakeLeaseSHA is worktree.leaseSHA, which is unexported. A full lowercase
// object id in either hash size, and nothing else.
var fakeLeaseSHA = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

// rebaseCheckout builds a real checkout with an "origin" carrying the head and
// base branches. It is real git because the ESCALATION path is not faked: when
// gitRebase declines, act dispatches the tend agent, and that goes through
// worktree.Manager.EnsurePR, which needs a repository to add a worktree to.
func rebaseCheckout(t *testing.T, root string) string {
	t.Helper()
	origin := filepath.Join(root, "origin.git")
	checkout := filepath.Join(root, "checkout")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(root, "init", "--bare", "--initial-branch=master", origin)
	run(root, "clone", origin, checkout)
	if err := os.WriteFile(filepath.Join(checkout, "a.go"), []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(checkout, "add", "a.go")
	run(checkout, "commit", "-m", "first")
	run(checkout, "push", "origin", "master")
	run(checkout, "push", "origin", "master:feat/x")
	run(checkout, "fetch", "origin")
	return checkout
}

// rebaseConfig is a loop that tends in a per-issue worktree, which is the only
// mode the automatic rebase runs in.
func rebaseConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	return &config.Config{
		Name:            rebaseLoop,
		Repo:            "o/r",
		DefaultBranch:   "master",
		TendPR:          true,
		TendPrompt:      "rebase #{{.Issue.Number}}",
		CheckoutBaseDir: rebaseCheckout(t, dir),
		WorktreeDir:     filepath.Join(dir, "wt"),
		StateDir:        filepath.Join(dir, "state"),
		Labels: config.Labels{
			Trigger: "status:todo", InFlight: "status:doing",
			Blocked: "status:blocked", Review: "status:review", Terminal: "status:done",
		},
		Agent: config.Agent{
			Model: "opus", Worktree: config.WorktreePerIssue,
			Timeout: config.Duration(time.Hour),
		},
		Prompt:       "plan #{{.Issue.Number}}",
		ResumePrompt: "resume #{{.Issue.Number}}",
	}
}

func tendDecision() engine.Decision {
	return engine.Decision{
		Kind: engine.KindTend, Issue: 7, PR: 12, Title: "a title",
		HeadRef: "feat/x", BaseRef: "master",
	}
}

// rebaseDeps builds a Deps whose Git is the fake and whose Store is a real
// temporary database, so the record assertions read what was actually written.
// Spawn counts the agent dispatches the fallback path makes.
func rebaseDeps(t *testing.T, cfg *config.Config) (Deps, *fakeGit) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	git := &fakeGit{}
	return Deps{
		Store:      db.Project(testProject),
		ProjectID:  testProject,
		GH:         &countingGH{},
		WT:         worktree.New(cfg.CheckoutBaseDir, cfg.WorktreeDir, cfg.Name, cfg.DefaultBranch),
		SelfPath:   "/bin/true",
		ConfigPath: "/tmp/loop.yaml",
		Now:        time.Now,
		Spawn: func(string, int64, string, string, string) (int, error) {
			return 4242, nil
		},
		IsAlive: func(int, int64) bool { return true },
		Git:     git,
	}, git
}

// The point of the feature: a clean replay costs no agent and no tokens.
func TestGitRebaseCleanReplayDispatchesNoAgent(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	var sum Summary

	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Rebased != 1 {
		t.Errorf("Rebased = %d, want 1", sum.Rebased)
	}
	if sum.Tended != 0 {
		t.Errorf("Tended = %d, want 0: no agent may be dispatched for a clean replay", sum.Tended)
	}
	if git.pushes != 1 {
		t.Errorf("pushes = %d, want 1", git.pushes)
	}
}

// The dirty check must ask about the worktree as the last pass left it.
// EnsurePRCtx detaches HEAD onto FETCH_HEAD, which flattens a crashed agent's
// unpushed commits, so a check made after the refresh always answers "clean".
func TestGitRebaseChecksForDirtyWorkBeforeRefreshingTheWorktree(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	var sum Summary

	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if !git.dirtyBefore {
		t.Error("the dirty check ran after the worktree refresh, which flattens the work it looks for")
	}
}

// The lease is the one guard on the force-push, and it must name the commit
// this pass fetched -- not the one the worktree held before the refresh.
func TestGitRebasePushesWithTheLeaseReadAfterTheRefresh(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	var sum Summary

	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if want := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"; git.lease != want {
		t.Errorf("lease = %q, want %q: the lease must be the head the refresh fetched", git.lease, want)
	}
}

// A conflict is what an agent is for. The abort must run first, so the next
// pass does not meet a worktree stuck mid-rebase.
func TestGitRebaseConflictAbortsAndDispatchesTheAgent(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	var sum Summary

	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if git.aborts != 1 {
		t.Errorf("aborts = %d, want 1", git.aborts)
	}
	if git.pushes != 0 {
		t.Errorf("pushes = %d, want 0 after a conflict", git.pushes)
	}
	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1: a conflict escalates to the agent", sum.Tended)
	}
	if sum.Rebased != 0 {
		t.Errorf("Rebased = %d, want 0", sum.Rebased)
	}
}

// The worst state this path can produce: a worktree still holding
// .git/rebase-merge. An agent started in it can force-push a half-replayed
// tree, so the worktree is destroyed and NOBODY is dispatched.
func TestGitRebaseFailedAbortDestroysTheWorktreeAndDispatchesNobody(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	git.abortErr = errors.New("context canceled")
	var sum Summary

	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if git.removes != 1 {
		t.Errorf("removes = %d, want 1: a worktree that could not be aborted must be destroyed", git.removes)
	}
	if sum.Tended != 0 {
		t.Errorf("Tended = %d, want 0: no agent may start in a worktree stuck mid-rebase", sum.Tended)
	}
	if sum.Rebased != 0 {
		t.Errorf("Rebased = %d, want 0", sum.Rebased)
	}
}

// A refused push means somebody else moved the branch while this pass ran.
// Sending an agent at a branch under active work is the more dangerous answer,
// so this pass does nothing and lets the next one read the new state.
func TestGitRebaseRefusedPushDispatchesNoAgent(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.pushErr = errors.New("stale info")
	var sum Summary

	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Tended != 0 || sum.Rebased != 0 {
		t.Errorf("Tended = %d, Rebased = %d; want 0 and 0", sum.Tended, sum.Rebased)
	}

	// A refused push is not a rebase, and it must not be recorded as one: the
	// summary and the dispatch log are what an operator audits unattended
	// force-pushes with.
	ds, err := deps.Store.RecentDispatches(rebaseLoop, "o/r", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 0 {
		t.Errorf("dispatches = %d, want 0: nothing was pushed", len(ds))
	}
}

// A dirty worktree holds work a rebase would destroy. The agent is the right
// actor for it.
func TestGitRebaseDirtyWorktreeDispatchesTheAgent(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.dirty = true
	var sum Summary

	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if git.rebases != 0 {
		t.Errorf("rebases = %d, want 0 in a dirty worktree", git.rebases)
	}
	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1", sum.Tended)
	}
}

// A loop with worktree: none has no pull-request worktree to rebase in.
func TestGitRebaseWithoutAPerIssueWorktreeDispatchesTheAgent(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	cfg.Agent.Worktree = config.WorktreeNone
	var sum Summary

	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if git.rebases != 0 {
		t.Errorf("rebases = %d, want 0", git.rebases)
	}
	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1", sum.Tended)
	}
}

// A nil Git is what every Deps built before this feature has. It must fall
// through to the agent rather than panic.
func TestGitRebaseWithoutGitDispatchesTheAgent(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, _ := rebaseDeps(t, cfg)
	deps.Git = nil
	var sum Summary

	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1", sum.Tended)
	}
}

// The record. A force-push with no local cause is what an operator would find
// otherwise. The row must NOT appear as a session: it has no conversation.
func TestACleanRebaseWritesADispatchRowWithNoSession(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, _ := rebaseDeps(t, cfg)
	var sum Summary
	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}

	ds, err := deps.Store.RecentDispatches(rebaseLoop, "o/r", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(ds))
	}
	if ds[0].Kind != store.KindRebase {
		t.Errorf("Kind = %q, want %q", ds[0].Kind, store.KindRebase)
	}
	if ds[0].SessionID != "" {
		t.Errorf("SessionID = %q, want empty", ds[0].SessionID)
	}
	if ds[0].Status != store.StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", ds[0].Status)
	}
	if got := sessionsFrom(ds, ""); len(got) != 0 {
		t.Errorf("sessions = %d, want 0: a rebase is not a session", len(got))
	}
}

// A rebase row must not freeze its issue. Every guard that partitions running
// dispatch rows treats an unknown kind as a live agent, so the row has to land
// finished.
func TestARebaseRowIsNotLive(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, _ := rebaseDeps(t, cfg)
	var sum Summary
	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}

	running, err := deps.Store.RunningDispatches(rebaseLoop, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 0 {
		t.Errorf("running dispatches = %d, want 0: a rebase row must never read as a live agent", len(running))
	}
}

// countingGH is shared with the tend-check tests; this assertion keeps the
// rebase path honest about it. The automatic rebase talks to git only.
func TestGitRebaseMakesNoGitHubCall(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, _ := rebaseDeps(t, cfg)
	gh := deps.GH.(*countingGH)
	var sum Summary

	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if gh.calls != 0 {
		t.Errorf("GitHub calls = %d, want 0", gh.calls)
	}
}

var _ ghub.Client = (*countingGH)(nil)

// The case the destroy path exists for, and the one that used to break it.
// The commonest way to reach a failed rebase is the budget expiring or the
// daemon shutting down, and the worktree helpers fail a command outright on a
// dead context. Reusing that context would make the abort fail by
// construction and then half-remove the worktree: directory gone, registration
// stranded, every later "worktree add" at that path failing forever.
func TestGitRebaseCleansUpOnAContextTheCallerAlreadyCancelled(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = context.DeadlineExceeded
	git.abortNeedsLiveCtx = true

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// gitRebase directly: act's agent fallback would need a live context of
	// its own, and the property under test is entirely inside gitRebase.
	outcome, err := gitRebase(ctx, cfg, deps, tendDecision())
	if err != nil {
		t.Fatalf("gitRebase: %v", err)
	}
	if git.cleanupSawDeadCtx {
		t.Error("the cleanup ran on the cancelled context; it must be detached from the caller's")
	}
	if git.aborts != 1 {
		t.Errorf("aborts = %d, want 1", git.aborts)
	}
	// A live context lets the abort succeed, which REPAIRS the worktree.
	// Destroying it is the fallback, not the outcome.
	if git.removes != 0 {
		t.Errorf("removes = %d, want 0: an abort that succeeds must not destroy the worktree", git.removes)
	}
	if outcome != notDone {
		t.Errorf("outcome = %v, want notDone: a recovered worktree escalates to the agent", outcome)
	}
}

// A worktree refresh that fails leaves nothing to rebase in. It is reported
// and the agent takes over, rather than the sweep abandoning the decision.
func TestGitRebaseWorktreeRefreshFailureFallsBackToTheAgent(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.ensureErr = errors.New("fetch origin feat/x: exit status 128")

	outcome, err := gitRebase(context.Background(), cfg, deps, tendDecision())
	if err == nil {
		t.Fatal("err = nil; a failed worktree refresh must be reported")
	}
	if outcome != notDone {
		t.Errorf("outcome = %v, want notDone", outcome)
	}
	if git.rebases != 0 || git.pushes != 0 {
		t.Errorf("rebases = %d, pushes = %d; want 0 and 0", git.rebases, git.pushes)
	}

	var sum Summary
	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatalf("act: %v", err)
	}
	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1: act must log the git failure and dispatch the agent", sum.Tended)
	}
}

// An unreadable head means no lease, and no lease means no force-push. The
// pass must stop before the rebase rather than push unguarded.
func TestGitRebaseUnreadableHeadNeverPushes(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.headSHAErr = errors.New(`rev-parse HEAD returned "", which is not an object id`)

	outcome, err := gitRebase(context.Background(), cfg, deps, tendDecision())
	if err == nil {
		t.Fatal("err = nil; an unreadable head must be reported")
	}
	if outcome != notDone {
		t.Errorf("outcome = %v, want notDone", outcome)
	}
	if git.rebases != 0 {
		t.Errorf("rebases = %d, want 0: the lease is read before the replay", git.rebases)
	}
	if git.pushes != 0 {
		t.Errorf("pushes = %d, want 0: nothing may be force-pushed without a lease", git.pushes)
	}
}
