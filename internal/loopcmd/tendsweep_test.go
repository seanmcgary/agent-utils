package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/project"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// sweepConfig is the TEND DISPATCHER: tickConfig's infrastructure, under the
// reserved name, carrying the project's tend policy. That is the shape
// config.LoadTend builds, and it is the only configuration the tend passes are
// ever handed.
func sweepConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := tickConfig(t)
	cfg.Name = project.Reserved
	cfg.Tend = project.Tend{
		Enabled: true, Label: "review",
		Model: "sonnet", Prompt: "rebase #{{.Issue.Number}}",
	}
	return cfg
}

// reviewPRFixture is a trusted pull request that closes issue n. Trusted is
// load-bearing: engine.LinkPR skips !pr.Trusted, so a fixture without it links
// nothing and every assertion below reads zero for the wrong reason.
func reviewPRFixture(issue, pr int, base string) ghub.PullRequest {
	return ghub.PullRequest{
		Number:  pr,
		Body:    fmt.Sprintf("Closes #%d", issue),
		HeadRef: fmt.Sprintf("issue-%d", issue),
		BaseRef: base,
		Trusted: true,
	}
}

// The sweep dispatches a rebase for a review issue whose pull request is
// behind, and does nothing for an issue that merely carries the trigger label.
// A FULL tick would start an agent for that one. This pass must not: it
// answers a merge, and a merge calls for a rebase and nothing else.
func TestTendSweepDispatchesOnlyTendDecisions(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{
			{Number: 1, Labels: []string{"review"}},
			{Number: 2, Labels: []string{"trigger"}},
		},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "master")},
		behind: map[int]int{11: 3},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TendSweep(context.Background(), cfg, deps, "master")
	if err != nil {
		t.Fatalf("TendSweep: %v", err)
	}

	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1", sum.Tended)
	}
	if sum.Started != 0 {
		t.Errorf("Started = %d, want 0: a sweep must not start an agent for a triggered issue", sum.Started)
	}
	if spawned != 1 {
		t.Errorf("spawned = %d, want 1", spawned)
	}
}

// A merge into master says nothing about a pull request targeting another
// branch. Rebasing that branch would be a tend agent dispatched for an
// unrelated event -- the shape of the incident Worker.Deliver records.
func TestTendSweepIgnoresAPullRequestTargetingAnotherBranch(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "release/1.0")},
		behind: map[int]int{11: 5},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TendSweep(context.Background(), cfg, deps, "master")
	if err != nil {
		t.Fatalf("TendSweep: %v", err)
	}
	if sum.Tended != 0 || spawned != 0 {
		t.Errorf("Tended = %d, spawned = %d, want 0 and 0", sum.Tended, spawned)
	}
	// The skip happens before the comparison, so it costs no API call either.
	if len(gh.compared) != 0 {
		t.Errorf("compared %d pull requests, want 0", len(gh.compared))
	}
}

// A pull request level with its base produces nothing. Silence is correct.
func TestTendSweepIgnoresAnUpToDatePullRequest(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "master")},
		behind: map[int]int{11: 0},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TendSweep(context.Background(), cfg, deps, "master")
	if err != nil {
		t.Fatalf("TendSweep: %v", err)
	}
	if sum.Tended != 0 {
		t.Errorf("Tended = %d, want 0", sum.Tended)
	}
}

// There is no "tending is off" case here any more. TendSweep is only ever
// handed the tend dispatcher's configuration, and config.LoadTend refuses to
// build one for a project that has not switched tending on -- so the pass that
// used to cost an issue listing before discovering it had nothing to do is
// never reached at all.

// One merge must not spawn an unbounded number of agents. Each dispatch is a
// detached process with permission prompts disabled, in its own worktree.
func TestTendSweepCapsDispatchesPerSweep(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{behind: map[int]int{}}
	for i := 1; i <= maxTendPerSweep+3; i++ {
		gh.issues = append(gh.issues, ghub.Issue{Number: i, Labels: []string{"review"}})
		gh.prs = append(gh.prs, reviewPRFixture(i, 100+i, "master"))
		gh.behind[100+i] = 2
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TendSweep(context.Background(), cfg, deps, "master")
	if err != nil {
		t.Fatalf("TendSweep: %v", err)
	}
	if sum.Tended != maxTendPerSweep {
		t.Errorf("Tended = %d, want %d", sum.Tended, maxTendPerSweep)
	}
	if spawned != maxTendPerSweep {
		t.Errorf("spawned = %d, want %d", spawned, maxTendPerSweep)
	}
}

// A dead TEND row is retired, or its pull request is never tended again. A
// dead row of any OTHER kind is left alone: retiring it would flag an issue
// this pass never examined for retry, the hazard tickIssue describes.
//
// behind is 0 so no NEW tend row is created. Reaping happens before Decide
// either way, so the property under test is unaffected -- and with a non-zero
// behind the fresh tend row would be indistinguishable from an unreaped one.
func TestTendSweepRetiresDeadTendRowsOnly(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{
			{Number: 1, Labels: []string{"review"}},
			{Number: 2, Labels: []string{"in-flight"}},
		},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "master")},
		behind: map[int]int{11: 0},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.IsAlive = func(int, int64) bool { return false }

	liveDispatch(t, cfg, deps, store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, Kind: store.KindTend, PRNumber: 11, SessionID: "t1",
	})
	// A LOOP's row, under a loop's name. That is the shape the guard is about:
	// the sweep reads every loop's running dispatches so it can see the agents
	// it must not collide with, and it must retire none of them.
	liveDispatch(t, cfg, deps, store.Dispatch{
		Loop: "execution", Repo: cfg.Repo, Number: 2, Kind: store.KindStart, SessionID: "s1",
	})

	if _, err := TendSweep(context.Background(), cfg, deps, "master"); err != nil {
		t.Fatalf("TendSweep: %v", err)
	}

	running, err := deps.Store.RunningDispatchesForRepo(cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	var sawTend, sawStart bool
	for _, d := range running {
		switch d.Kind {
		case store.KindTend:
			sawTend = true
		case store.KindStart:
			sawStart = true
		}
	}
	if sawTend {
		t.Error("the dead tend row was not retired")
	}
	if !sawStart {
		t.Error("a dead start row was retired by a tend sweep; only tend rows may be")
	}
	st, err := deps.Store.IssueState("execution", cfg.Repo, 2)
	if err != nil {
		t.Fatal(err)
	}
	if st.NeedsRetry {
		t.Error("issue 2 was flagged for retry by a pass that never examined it")
	}
	// The symmetric half, and the one that pins reapDead's KindTend guard: a
	// retired TEND row must write no issue state either. Nothing clears that
	// flag except a retry, and a tend row's issue has none to run, so writing
	// it would strand the issue outside the loop.
	st1, err := deps.Store.IssueState(cfg.Name, cfg.Repo, 1)
	if err != nil {
		t.Fatal(err)
	}
	if st1.NeedsRetry {
		t.Error("retiring a dead tend row flagged its issue for retry")
	}
}

// A stale checkout is the one the tend agent would rebase in. The sweep stops
// rather than dispatching an agent into it.
func TestTendSweepStopsWhenTheFetchFails(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "master")},
		behind: map[int]int{11: 3},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.Fetch = func(context.Context) error { return errors.New("network down") }

	sum, err := TendSweep(context.Background(), cfg, deps, "master")
	if err == nil {
		t.Fatal("want an error when the fetch failed, got nil")
	}
	if sum.Tended != 0 || spawned != 0 {
		t.Errorf("Tended = %d, spawned = %d, want 0 and 0", sum.Tended, spawned)
	}
}

// One unusable pull request must not stop the pass. If it did, anyone able to
// open a pull request could stop every rebase this loop would otherwise do.
func TestTendSweepContinuesPastAFailedComparison(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{
			{Number: 1, Labels: []string{"review"}},
			{Number: 2, Labels: []string{"review"}},
		},
		prs: []ghub.PullRequest{
			reviewPRFixture(1, 11, "master"),
			reviewPRFixture(2, 12, "master"),
		},
		behind:    map[int]int{11: 3, 12: 3},
		behindErr: map[int]error{11: errors.New("no common ancestor")},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TendSweep(context.Background(), cfg, deps, "master")
	if err != nil {
		t.Fatalf("TendSweep: %v", err)
	}
	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1: the second pull request must still be tended", sum.Tended)
	}
}

// There are no breaker tests here any more, and their removal is the same
// change as the rest: the circuit breaker counts RETRY decisions, the tend
// dispatcher makes none, and DecideTend has no breaker to trip or obey. What
// the two deleted tests pinned -- that a sweep neither writes a cooldown nor
// dispatches around one -- is now true by construction rather than by
// suppression, because there is no retry policy in the dispatcher's
// configuration at all (see config.LoadTend).

// seedStartedSession gives an issue the session state an execution agent would
// have left behind: an identifier claude actually created.
func seedStartedSession(t *testing.T, deps Deps, cfg *config.Config, number int, id string) {
	t.Helper()
	if err := deps.Store.PutIssueState(store.IssueState{
		Loop: cfg.Name, Repo: cfg.Repo, Number: number,
		SessionID: id, SessionStarted: true,
	}); err != nil {
		t.Fatal(err)
	}
}

// The whole point of the change: a rebase agent resumes the session that built
// the branch instead of meeting it cold.
// A tend runs in its OWN session, minted for the dispatch, even when the issue
// has a perfectly good one. See the session comment in engine.DecideTend:
// the clean-rebase path is Go now, so the agent only meets conflicts and review
// threads, and both are described by the branch rather than by the conversation
// that wrote it. Inheriting also blocked the issue for as long as the tend ran.
func TestTendDispatchDoesNotInheritTheIssuesSession(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "master")},
		behind: map[int]int{11: 3},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	seedStartedSession(t, deps, cfg, 1, "sess-1")

	if _, err := TendSweep(context.Background(), cfg, deps, "master"); err != nil {
		t.Fatalf("TendSweep: %v", err)
	}

	running, _ := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if len(running) != 1 {
		t.Fatalf("running dispatches = %d, want 1", len(running))
	}
	if got := running[0].SessionID; got == "sess-1" {
		t.Errorf("dispatch session = %q, want a fresh one: a tend does not inherit", got)
	}
	if running[0].SessionID == "" {
		t.Error("dispatch session is empty: the tend must still get an identifier of its own")
	}
}

// A tend must never write the issue's session row. It borrows the session; it
// does not own it, and a tend that stamped one would let a rebase decide how
// the issue's own next dispatch behaves.
func TestTendDispatchLeavesTheIssueSessionRowAlone(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "master")},
		behind: map[int]int{11: 3},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	seedStartedSession(t, deps, cfg, 1, "sess-1")

	if _, err := TendSweep(context.Background(), cfg, deps, "master"); err != nil {
		t.Fatalf("TendSweep: %v", err)
	}

	states, _ := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if got := states[1].SessionID; got != "sess-1" {
		t.Errorf("issue session = %q, want it untouched at %q", got, "sess-1")
	}
	if states[1].RetryCount != 0 {
		t.Errorf("RetryCount = %d, want 0: a tend must not spend the issue's retry budget",
			states[1].RetryCount)
	}
}

// An issue whose session never started still gets a rebase, on a fresh
// identifier. Passing the unstarted one would make "-r" fail every time.
func TestTendDispatchMintsASessionWhenNoneStarted(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "master")},
		behind: map[int]int{11: 3},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	if _, err := TendSweep(context.Background(), cfg, deps, "master"); err != nil {
		t.Fatalf("TendSweep: %v", err)
	}

	running, _ := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if len(running) != 1 {
		t.Fatalf("running dispatches = %d, want 1", len(running))
	}
	if running[0].SessionID == "" {
		t.Error("a tend with no session to inherit must still carry an identifier")
	}
}
