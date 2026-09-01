package loopcmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// The two-agents-in-one-branch guard has to point BOTH ways.
//
// The tend dispatcher already refuses an issue any loop of the project holds:
// its guards read store.RunningDispatchesForRepo, which spans loops. The loop
// side had no reciprocal check -- engine.Decide reads RunningDispatches, which
// is scoped to the loop, and a tend's rows are filed under the dispatcher's
// reserved name, so a loop could never see one.
//
// What that allowed: an issue carries the tend label, a tend dispatch starts
// rebasing and force-pushing its branch, and a loop whose veto list does not
// happen to cover that label ticks, sees no live dispatch OF ITS OWN, and starts
// an agent on the same branch. Both worktrees are detached, so git refuses
// nothing, and the two race on `git push --force-with-lease`.
func TestTickDoesNotStartAnAgentWhileTheTendDispatcherHoldsTheIssue(t *testing.T) {
	loopCfg := tickConfig(t)
	tendCfg := sweepConfig(t)
	// One project, one repository, two dispatchers. The store is shared because
	// that is the whole subject: the rows the loop cannot see through its own
	// name are right there under the dispatcher's.
	gh := &fakeGH{issues: []ghub.Issue{
		// Both labels at once, which is the ordinary state of an issue whose
		// pull request is under review while its loop still has work queued.
		{Number: 51, Labels: []string{"trigger", "review"}},
	}}
	spawned := 0
	deps := newDeps(t, loopCfg, gh, &spawned)

	// The tend dispatcher's live row, written under its reserved name exactly
	// as a real dispatch writes it.
	if _, err := deps.Store.CreateDispatch(store.Dispatch{
		Loop: tendCfg.Name, Repo: tendCfg.Repo, Number: 51, PRNumber: 108,
		Kind: store.KindTend, SessionID: "tend-1", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}

	if _, err := Tick(context.Background(), loopCfg, deps); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if spawned != 0 {
		t.Fatalf("spawned = %d, want 0: a loop started an agent on a branch the tend dispatcher is force-pushing", spawned)
	}
	running, _ := deps.Store.RunningDispatches(loopCfg.Name, loopCfg.Repo)
	if len(running) != 0 {
		t.Fatalf("the loop's running dispatches = %+v, want none", running)
	}

	// The control. With the tend finished, the same fixture dispatches.
	rows, _ := deps.Store.RunningDispatchesForRepo(tendCfg.Repo)
	for _, d := range rows {
		if err := deps.Store.FinishDispatch(d.ID, store.DispatchResult{
			Status: store.StatusSucceeded,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Tick(context.Background(), loopCfg, deps); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if spawned != 1 {
		t.Fatalf("spawned = %d after the tend finished, want 1", spawned)
	}
}

// The same property through the delivery fast path, which builds its own
// engine.State and would otherwise be a second place for the guard to be
// missing from.
func TestTickIssueDoesNotStartAnAgentWhileTheTendDispatcherHoldsTheIssue(t *testing.T) {
	loopCfg := tickConfig(t)
	tendCfg := sweepConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 51, Labels: []string{"trigger", "review"}}}}
	spawned := 0
	deps := newDeps(t, loopCfg, gh, &spawned)

	if _, err := deps.Store.CreateDispatch(store.Dispatch{
		Loop: tendCfg.Name, Repo: tendCfg.Repo, Number: 51, PRNumber: 108,
		Kind: store.KindTend, SessionID: "tend-1", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}

	logs := captureTickLogs(t)
	if _, err := TickIssue(context.Background(), loopCfg, deps, 51); err != nil {
		t.Fatalf("TickIssue: %v", err)
	}
	if spawned != 0 {
		t.Fatalf("spawned = %d, want 0", spawned)
	}
	// The reason matters as much as the outcome, and it is what separates the
	// two layers of this guard. A skip line naming the tend dispatcher says the
	// DECISION declined; without it the loop would still not have spawned --
	// store.CreateDispatch refuses the row -- but only after deciding to
	// dispatch, having already stamped labels and burned the decision, and with
	// nothing in the log an operator could read as an explanation.
	if out := logs.String(); !strings.Contains(out, "tend dispatcher") {
		t.Errorf("no log line naming the tend dispatcher as the reason:\n%s", out)
	}
}

// The delivery fast path must not pay for a git fetch on a delivery it will not
// act on.
//
// `git fetch origin --prune` on the primary checkout used to run before the tend
// label was tested, so EVERY issue and pull request delivery in a tending
// project paid for one -- and a transient network failure logged "tend delivery
// failed" for deliveries tending would never have touched. The label test's own
// comment claims most deliveries land there and that this is what keeps the fast
// path cheap, which was true of the GitHub reads below it and not of the fetch
// above it.
func TestTendIssueDoesNotFetchForAnIssueItWillNotTend(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{
		// No tend label: nothing here is tendable.
		{Number: 51, Labels: []string{"trigger"}},
	}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	fetches := 0
	deps.Fetch = func(context.Context) error {
		fetches++
		return nil
	}

	if _, err := TendIssue(context.Background(), cfg, deps, 51); err != nil {
		t.Fatalf("TendIssue: %v", err)
	}
	if fetches != 0 {
		t.Errorf("fetches = %d, want 0 for a delivery about an issue that carries no tend label", fetches)
	}

	// The control: an issue that IS tendable still fetches, because the
	// comparisons below read remote tracking refs and a stale answer there
	// reports a branch as current after its base has moved.
	gh.issues = []ghub.Issue{{Number: 51, Labels: []string{"review"}}}
	gh.prs = []ghub.PullRequest{reviewPRFixture(51, 108, "master")}
	gh.behind = map[int]int{108: 3}
	if _, err := TendIssue(context.Background(), cfg, deps, 51); err != nil {
		t.Fatalf("TendIssue: %v", err)
	}
	if fetches != 1 {
		t.Errorf("fetches = %d after a tendable delivery, want 1", fetches)
	}
}
