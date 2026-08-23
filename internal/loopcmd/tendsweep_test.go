package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// sweepConfig is tickConfig with tending on. tickConfig leaves TendPR false,
// and TendSweep must produce nothing for a loop that does not tend.
func sweepConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := tickConfig(t)
	cfg.TendPR = true
	cfg.TendPrompt = "rebase #{{.Issue.Number}}"
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

// A loop that does not tend produces nothing, whoever calls, and costs no API
// call. The caller checks this too; TendSweep is exported, so it checks itself.
func TestTendSweepDoesNothingWhenTendPRIsOff(t *testing.T) {
	cfg := sweepConfig(t)
	cfg.TendPR = false
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "master")},
		behind: map[int]int{11: 3},
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
	if gh.listedIssues != 0 {
		t.Errorf("listedIssues = %d, want 0: a loop that does not tend must cost no API call", gh.listedIssues)
	}
}

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
	liveDispatch(t, cfg, deps, store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 2, Kind: store.KindStart, SessionID: "s1",
	})

	if _, err := TendSweep(context.Background(), cfg, deps, "master"); err != nil {
		t.Fatalf("TendSweep: %v", err)
	}

	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
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
	st, err := deps.Store.IssueState(cfg.Name, cfg.Repo, 2)
	if err != nil {
		t.Fatal(err)
	}
	if st.NeedsRetry {
		t.Error("issue 2 was flagged for retry by a pass that never examined it")
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
	deps.Fetch = func() error { return errors.New("network down") }

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

// The breaker counts retry decisions, and this pass discards every one of
// them. A pass that will not act on that evidence must not stop the passes
// that would.
func TestTendSweepWritesNoCooldown(t *testing.T) {
	cfg := sweepConfig(t)
	cfg.Retry.Breaker.OrphanThreshold = 1
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "master")},
		behind: map[int]int{11: 3},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	// One issue already needing a retry is enough to trip a threshold of 1.
	if err := deps.Store.MarkNeedsRetry(cfg.Name, cfg.Repo, 1, deps.Now(), nil); err != nil {
		t.Fatal(err)
	}

	if _, err := TendSweep(context.Background(), cfg, deps, "master"); err != nil {
		t.Fatalf("TendSweep: %v", err)
	}

	until, err := deps.Store.CooldownUntil(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !until.IsZero() {
		t.Errorf("CooldownUntil = %v, want zero: a tend sweep must not trip the breaker", until)
	}
}
