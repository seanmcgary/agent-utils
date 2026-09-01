package loopcmd

import (
	"context"
	"database/sql"
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
	_ "modernc.org/sqlite"
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
	dirty           bool
	rebaseErr       error
	pushErr         error
	abortErr        error
	ensureErr       error
	headSHAErr      error
	headSHAOverride string

	// conflictedPaths and conflictedPathsErr choose the backoff gate's
	// answer. An empty slice with no error is the default, matching what a
	// worktree with no readable conflict looks like: the backoff gate falls
	// through and the ordinary conflict path (abort, dispatch) runs.
	conflictedPaths    []string
	conflictedPathsErr error
	// liveConflictedPaths is the worktree's CURRENT conflicted state, as
	// opposed to conflictedPaths, which is the fixture. Rebase sets it on a
	// failure and AbortRebase clears it; see those two methods.
	liveConflictedPaths []string
	// conflictedPathsCalls counts how many times ConflictedPaths was read, so
	// a test can assert it ran on the detached cleanup context and only once
	// per conflict.
	conflictedPathsCalls int

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
//
// headSHAOverride lets a test simulate a HEAD that moved between two act
// calls -- an agent, or a human, pushing a fix -- without a second real
// checkout: the fingerprint backoff must treat it as a new conflict.
func (g *fakeGit) HeadSHA(context.Context, string) (string, error) {
	if g.headSHAErr != nil {
		return "", g.headSHAErr
	}
	if g.headSHAOverride != "" {
		return g.headSHAOverride, nil
	}
	if g.ensured {
		return "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", nil
	}
	return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
}

// Rebase re-creates the conflicted state on every failing attempt, the way a
// real rebase does. conflictedPaths is the test's FIXTURE -- what this branch
// conflicts on -- and liveConflictedPaths is the worktree's current state,
// which only exists between a failed rebase and its abort. Modelling the two
// separately is what lets one fixture drive several act calls, as a repeat
// conflict across sweeps really does.
func (g *fakeGit) Rebase(context.Context, string, string) error {
	g.rebases++
	if g.rebaseErr != nil {
		g.liveConflictedPaths = g.conflictedPaths
	}
	return g.rebaseErr
}

func (g *fakeGit) AbortRebase(ctx context.Context, _ string) error {
	g.aborts++
	// A real abort discards the conflicted state. Clearing it here is what
	// makes "the paths are read BEFORE the abort" a tested property rather
	// than a comment.
	g.liveConflictedPaths = nil
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

// ConflictedPaths models the two ordering properties the real thing depends
// on, so that getting either wrong fails a test rather than passing silently.
//
//   - It fails on a DEAD context, like every other worktree helper: they use
//     exec.CommandContext, so an expired context fails the command without
//     running git. gitRebase must therefore read the paths on the detached
//     cleanup context, never on the caller's, whose commonest way of reaching
//     the conflict branch is expiring.
//   - AbortRebase CLEARS the paths, like a real "git rebase --abort". The read
//     must happen BEFORE the abort, and a read moved after it sees an empty
//     list, which the gate reads as "no conflict" and never backs off.
func (g *fakeGit) ConflictedPaths(ctx context.Context, _ string) ([]string, error) {
	g.conflictedPathsCalls++
	if ctx.Err() != nil {
		g.cleanupSawDeadCtx = true
		return nil, ctx.Err()
	}
	if g.conflictedPathsErr != nil {
		return nil, g.conflictedPathsErr
	}
	return g.liveConflictedPaths, nil
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
	outcome, _, err := gitRebase(ctx, cfg, deps, tendDecision(), time.Now())
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

	outcome, _, err := gitRebase(context.Background(), cfg, deps, tendDecision(), time.Now())
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

	outcome, _, err := gitRebase(context.Background(), cfg, deps, tendDecision(), time.Now())
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

// --- Task 9: the repeat-conflict backoff ---

// A first conflict at a fingerprint dispatches the agent and writes a row
// with seen_count = 1 and a one-hour deadline.
func TestConflictBackoffFirstSightingDispatchesAndWritesOneHour(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	git.conflictedPaths = []string{"a.go"}
	now := time.Now()
	var sum Summary

	if err := act(context.Background(), cfg, deps, tendDecision(), now, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Tended != 1 {
		t.Fatalf("Tended = %d, want 1: a conflict never seen before must dispatch", sum.Tended)
	}
	if sum.Backoff != 0 {
		t.Errorf("Backoff = %d, want 0", sum.Backoff)
	}

	row, ok, err := deps.Store.TendConflict(rebaseLoop, "o/r", 12)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("TendConflict reported no row after a first-sighting dispatch")
	}
	if row.SeenCount != 1 {
		t.Errorf("SeenCount = %d, want 1", row.SeenCount)
	}
	if want := now.Add(time.Hour); row.RetryAfter.Sub(want).Abs() > time.Second {
		t.Errorf("RetryAfter = %v, want close to %v", row.RetryAfter, want)
	}
}

// The same fingerprint within the backoff window returns doneBackedOff,
// dispatches nothing, and leaves seen_count and retry_after UNCHANGED: a
// backed-off pass must not advance the count or move the deadline, or the
// agent would never be dispatched again.
func TestConflictBackoffWithinWindowDispatchesNothingAndWritesNothing(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	git.conflictedPaths = []string{"a.go"}
	now := time.Now()

	var sum1 Summary
	if err := act(context.Background(), cfg, deps, tendDecision(), now, &sum1); err != nil {
		t.Fatal(err)
	}
	before, ok, err := deps.Store.TendConflict(rebaseLoop, "o/r", 12)
	if err != nil || !ok {
		t.Fatalf("TendConflict after first dispatch: ok=%v err=%v", ok, err)
	}

	var sum2 Summary
	if err := act(context.Background(), cfg, deps, tendDecision(), now.Add(time.Minute), &sum2); err != nil {
		t.Fatal(err)
	}
	if sum2.Tended != 0 {
		t.Errorf("Tended = %d, want 0: the backoff window has not elapsed", sum2.Tended)
	}
	if sum2.Backoff != 1 {
		t.Errorf("Backoff = %d, want 1", sum2.Backoff)
	}

	after, ok, err := deps.Store.TendConflict(rebaseLoop, "o/r", 12)
	if err != nil || !ok {
		t.Fatalf("TendConflict after backed-off pass: ok=%v err=%v", ok, err)
	}
	if after.SeenCount != before.SeenCount {
		t.Errorf("SeenCount changed from %d to %d; a backed-off pass must write nothing",
			before.SeenCount, after.SeenCount)
	}
	if !after.RetryAfter.Equal(before.RetryAfter) {
		t.Errorf("RetryAfter changed from %v to %v; a backed-off pass must write nothing",
			before.RetryAfter, after.RetryAfter)
	}
}

// The same fingerprint AFTER retry_after has passed dispatches again and
// writes seen_count = 2 with a six-hour deadline.
func TestConflictBackoffAfterDeadlineDispatchesAgainAndAdvancesToSixHours(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	git.conflictedPaths = []string{"a.go"}
	now := time.Now()

	var sum1 Summary
	if err := act(context.Background(), cfg, deps, tendDecision(), now, &sum1); err != nil {
		t.Fatal(err)
	}

	later := now.Add(2 * time.Hour)
	var sum2 Summary
	if err := act(context.Background(), cfg, deps, tendDecision(), later, &sum2); err != nil {
		t.Fatal(err)
	}
	if sum2.Tended != 1 {
		t.Fatalf("Tended = %d, want 1: the deadline has passed", sum2.Tended)
	}

	row, ok, err := deps.Store.TendConflict(rebaseLoop, "o/r", 12)
	if err != nil || !ok {
		t.Fatalf("TendConflict: ok=%v err=%v", ok, err)
	}
	if row.SeenCount != 2 {
		t.Errorf("SeenCount = %d, want 2", row.SeenCount)
	}
	if want := later.Add(6 * time.Hour); row.RetryAfter.Sub(want).Abs() > time.Second {
		t.Errorf("RetryAfter = %v, want close to %v", row.RetryAfter, want)
	}
}

// A moved HEAD produces a new fingerprint and writes seen_count = 1, even
// though a row already exists: an agent or a human changed the branch, so the
// conflict this pass meets is not the one already tried.
func TestConflictBackoffMovedHeadStartsANewFingerprint(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	git.conflictedPaths = []string{"a.go"}
	now := time.Now()

	var sum1 Summary
	if err := act(context.Background(), cfg, deps, tendDecision(), now, &sum1); err != nil {
		t.Fatal(err)
	}

	// Still inside the backoff window, but the head has moved.
	git.headSHAOverride = "cccccccccccccccccccccccccccccccccccccccc"
	var sum2 Summary
	if err := act(context.Background(), cfg, deps, tendDecision(), now.Add(time.Minute), &sum2); err != nil {
		t.Fatal(err)
	}
	if sum2.Tended != 1 {
		t.Fatalf("Tended = %d, want 1: a moved head is a new conflict", sum2.Tended)
	}

	row, ok, err := deps.Store.TendConflict(rebaseLoop, "o/r", 12)
	if err != nil || !ok {
		t.Fatalf("TendConflict: ok=%v err=%v", ok, err)
	}
	if row.SeenCount != 1 {
		t.Errorf("SeenCount = %d, want 1: a new fingerprint resets the count", row.SeenCount)
	}
}

// A clean rebase deletes the conflict row: a branch that replayed cleanly has
// no conflict left to remember.
func TestConflictBackoffCleanRebaseDeletesTheRow(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	git.conflictedPaths = []string{"a.go"}
	now := time.Now()

	var sum1 Summary
	if err := act(context.Background(), cfg, deps, tendDecision(), now, &sum1); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := deps.Store.TendConflict(rebaseLoop, "o/r", 12); err != nil || !ok {
		t.Fatalf("TendConflict after the first conflict: ok=%v err=%v", ok, err)
	}

	git.rebaseErr = nil
	var sum2 Summary
	if err := act(context.Background(), cfg, deps, tendDecision(), now.Add(time.Minute), &sum2); err != nil {
		t.Fatal(err)
	}
	if sum2.Rebased != 1 {
		t.Fatalf("Rebased = %d, want 1", sum2.Rebased)
	}
	if _, ok, err := deps.Store.TendConflict(rebaseLoop, "o/r", 12); err != nil || ok {
		t.Fatalf("TendConflict after a clean rebase: ok=%v err=%v, want no row", ok, err)
	}
}

// A rebase failure that leaves no conflicted path is not a conflict this gate
// understands. Refusing to dispatch on it would be a silent stall, so it
// dispatches like an ordinary conflict.
func TestConflictBackoffNoConflictedPathsDispatches(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	// git.conflictedPaths left nil: the default, matching a worktree with no
	// readable conflicted paths.
	var sum Summary

	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1: no conflicted paths must not be treated as a backoff", sum.Tended)
	}
}

// An unreadable conflict row must dispatch the agent: the gate fails OPEN, so
// state it could not read never strands a pull request.
func TestConflictBackoffUnreadableRowDispatches(t *testing.T) {
	cfg := rebaseConfig(t)
	dbPath := filepath.Join(t.TempDir(), "s.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	git := &fakeGit{
		rebaseErr:       errors.New("CONFLICT (content): Merge conflict in a.go"),
		conflictedPaths: []string{"a.go"},
	}
	deps := Deps{
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
	}

	// Break the read this gate depends on without touching anything else the
	// pass writes: dropping the table makes TendConflict's SELECT fail while
	// dispatches, pr_links, and every other table stay intact.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("DROP TABLE tend_conflicts"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	var sum Summary
	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1: an unreadable row must fail open", sum.Tended)
	}
}

// A ReviewPending decision is never backed off, even inside the window, and
// because it therefore reaches the agent, it still advances seen_count like
// any other dispatch: the check belongs INSIDE the backoff, not as an
// override in act.
func TestConflictBackoffReviewPendingDispatchesAndAdvancesCount(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	git.conflictedPaths = []string{"a.go"}
	now := time.Now()

	var sum1 Summary
	if err := act(context.Background(), cfg, deps, tendDecision(), now, &sum1); err != nil {
		t.Fatal(err)
	}

	d := tendDecision()
	d.ReviewPending = true
	var sum2 Summary
	if err := act(context.Background(), cfg, deps, d, now.Add(time.Minute), &sum2); err != nil {
		t.Fatal(err)
	}
	if sum2.Tended != 1 {
		t.Fatalf("Tended = %d, want 1: a review-pending decision must never be backed off", sum2.Tended)
	}
	if sum2.Backoff != 0 {
		t.Errorf("Backoff = %d, want 0", sum2.Backoff)
	}

	row, ok, err := deps.Store.TendConflict(rebaseLoop, "o/r", 12)
	if err != nil || !ok {
		t.Fatalf("TendConflict: ok=%v err=%v", ok, err)
	}
	if row.SeenCount != 2 {
		t.Errorf("SeenCount = %d, want 2: a review-pending dispatch still counts as a sighting", row.SeenCount)
	}
}

// --- Task 6: review activity falls through a clean rebase ---

// A clean rebase on a ReviewPending decision must count the rebase AND fall
// through to dispatch the agent: the rebase settles only the staleness half
// of the decision, and the feedback is still unanswered.
func TestCleanRebaseOnAReviewPendingDecisionCountsAndDispatches(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	d := tendDecision()
	d.ReviewPending = true
	var sum Summary

	if err := act(context.Background(), cfg, deps, d, time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Rebased != 1 {
		t.Errorf("Rebased = %d, want 1: the clean replay still happened", sum.Rebased)
	}
	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1: unanswered review feedback must still reach the agent", sum.Tended)
	}
	if git.pushes != 1 {
		t.Errorf("pushes = %d, want 1", git.pushes)
	}

	running, err := deps.Store.RunningDispatches(rebaseLoop, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || !running[0].ReviewPending {
		t.Fatalf("running dispatch = %+v, want one row with ReviewPending set", running)
	}
}

// Without ReviewPending, a clean rebase settles the decision outright: no
// agent runs, matching the behaviour before this trigger existed.
func TestCleanRebaseWithoutReviewPendingDispatchesNoAgent(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, _ := rebaseDeps(t, cfg)
	var sum Summary

	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Rebased != 1 || sum.Tended != 0 {
		t.Errorf("Rebased = %d, Tended = %d, want 1 and 0", sum.Rebased, sum.Tended)
	}
}

// An unreadable conflicted-path list dispatches the agent. The gate fails
// OPEN, and that direction is the whole point: a gate whose job is to DECLINE
// to spend money must never be able to strand a pull request on a read it
// could not make. A rebase that failed with no readable conflict is also not a
// conflict this gate understands.
func TestUnreadableConflictedPathsDispatchesTheAgent(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	git.conflictedPaths = []string{"a.go"}
	git.conflictedPathsErr = errors.New("git diff exploded")
	now := time.Now()

	var sum Summary
	if err := act(context.Background(), cfg, deps, tendDecision(), now, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1: an unreadable path list must fail open", sum.Tended)
	}
	if sum.Backoff != 0 {
		t.Errorf("Backoff = %d, want 0", sum.Backoff)
	}
	// No fingerprint could be computed, so nothing may be recorded: a row
	// written from an unreadable conflict would back off a conflict nobody
	// ever identified.
	if _, ok, err := deps.Store.TendConflict(rebaseLoop, "o/r", 12); err != nil || ok {
		t.Errorf("TendConflict: ok=%v err=%v, want no row", ok, err)
	}
}

// A failed abort dispatches no agent, so it must record no sighting either.
//
// seen_count counts agent DISPATCHES that met a fingerprint. The abort sits
// between the gate and the dispatch, and a failed abort destroys the worktree
// and returns doneNoRebase, so committing the row before it would let repeated
// abort failures at a stable head walk a pull request to the 24h tier without
// the agent having seen the conflict once.
func TestFailedAbortRecordsNoConflictSighting(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	git.conflictedPaths = []string{"a.go"}
	git.abortErr = errors.New("abort failed")
	now := time.Now()

	var sum Summary
	if err := act(context.Background(), cfg, deps, tendDecision(), now, &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Tended != 0 {
		t.Errorf("Tended = %d, want 0: a failed abort dispatches nobody", sum.Tended)
	}
	if git.removes != 1 {
		t.Errorf("removes = %d, want 1: a worktree left mid-rebase must be destroyed", git.removes)
	}
	if _, ok, err := deps.Store.TendConflict(rebaseLoop, "o/r", 12); err != nil || ok {
		t.Errorf("TendConflict: ok=%v err=%v, want no row: no agent was dispatched", ok, err)
	}
}

// The conflicted paths are read on the DETACHED cleanup context, not the
// caller's.
//
// The commonest way to reach the conflict branch at all is the caller's
// context expiring -- that is why the abort was moved to a detached context in
// the first place. The worktree helpers use exec.CommandContext, so a read on
// an expired context fails without running git, and the backoff would be
// silently unreachable in exactly the case it exists for.
func TestConflictedPathsAreReadOnTheDetachedContext(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	git.conflictedPaths = []string{"a.go"}
	now := time.Now()

	dead, cancel := context.WithCancel(context.Background())
	cancel()

	var sum Summary
	if err := act(dead, cfg, deps, tendDecision(), now, &sum); err != nil {
		t.Fatal(err)
	}
	if git.cleanupSawDeadCtx {
		t.Error("a cleanup command was handed an expired context")
	}
	row, ok, err := deps.Store.TendConflict(rebaseLoop, "o/r", 12)
	if err != nil || !ok {
		t.Fatalf("TendConflict: ok=%v err=%v, want a row: the gate must run on a dead caller context", ok, err)
	}
	if row.SeenCount != 1 {
		t.Errorf("SeenCount = %d, want 1", row.SeenCount)
	}
}

// The tiers escalate 1h, 6h, 24h and then CLAMP at 24h.
//
// Nothing else drives count past 2, so without this the whole 24h tier and the
// clamp are unexecuted -- and an out-of-range index there is a panic in the
// unattended daemon, on the path that exists precisely because a conflict keeps
// repeating.
func TestConflictBackoffTiersEscalateThenClampAtTwentyFourHours(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	git.conflictedPaths = []string{"a.go"}

	now := time.Now()
	// One dispatch per tier, each after the previous deadline has passed. The
	// fingerprint is stable across all of them: same head, same paths.
	for i, want := range []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour, 24 * time.Hour} {
		var sum Summary
		if err := act(context.Background(), cfg, deps, tendDecision(), now, &sum); err != nil {
			t.Fatal(err)
		}
		if sum.Tended != 1 {
			t.Fatalf("sighting %d: Tended = %d, want 1", i+1, sum.Tended)
		}
		row, ok, err := deps.Store.TendConflict(rebaseLoop, "o/r", 12)
		if err != nil || !ok {
			t.Fatalf("sighting %d: TendConflict ok=%v err=%v", i+1, ok, err)
		}
		if row.SeenCount != i+1 {
			t.Errorf("sighting %d: SeenCount = %d, want %d", i+1, row.SeenCount, i+1)
		}
		if got := row.RetryAfter.Sub(now); (got - want).Abs() > time.Second {
			t.Errorf("sighting %d: wait = %v, want %v", i+1, got, want)
		}
		// Step past the deadline this sighting just wrote.
		now = row.RetryAfter.Add(time.Minute)
	}
}

// A dispatch that fails records no sighting.
//
// seen_count counts agent dispatches that HAPPENED. dispatch can fail after the
// backoff gate has already decided -- a worktree it cannot build, a spawn that
// will not start -- and no agent runs then. Counting it would let a repeating
// worktree or spawn failure walk a pull request to the 24h tier without the
// agent ever having seen the conflict, which is the same failure the
// failed-abort path refuses to cause, reached by a different route.
func TestFailedDispatchRecordsNoConflictSighting(t *testing.T) {
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	git.conflictedPaths = []string{"a.go"}
	deps.Spawn = func(string, int64, string, string, string) (int, error) {
		return 0, errors.New("spawn refused")
	}

	var sum Summary
	err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum)
	if err == nil {
		t.Fatal("act returned no error for a failed spawn")
	}
	if sum.Tended != 0 {
		t.Errorf("Tended = %d, want 0: the spawn failed", sum.Tended)
	}
	if _, ok, err := deps.Store.TendConflict(rebaseLoop, "o/r", 12); err != nil || ok {
		t.Errorf("TendConflict: ok=%v err=%v, want no row: no agent was dispatched", ok, err)
	}
}
