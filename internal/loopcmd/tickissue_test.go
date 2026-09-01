package loopcmd

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// captureTickLogs routes this package's structured logging into a buffer for
// one test. TestMain discards it by default, which is right for every test
// that is not judging a log line.
func captureTickLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// breakerWarning is the message warnBreakerNotEvaluated logs. The summary line
// every tick writes contains the word "breaker" too (breaker_tripped), so a
// test matching on that alone would pass on the wrong line.
const breakerWarning = "dispatching a retry without evaluating the circuit breaker"

// This is the bug the user hit, verbatim: they opened an unlabelled test issue
// and the daemon dispatched an agent for a completely unrelated issue that was
// already eligible. A delivery says "something about THIS issue changed"; it is
// not an instruction to reconcile the repository.
func TestTickIssueDecidesOnlyTheIssueTheDeliveryNames(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{
		// The unlabelled issue the human just opened: nothing to do.
		{Number: 7},
		// Eligible, and nobody touched it. A delivery about #7 must leave it
		// alone; the cron sweep is what picks it up.
		{Number: 51, Labels: []string{"trigger"}},
	}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TickIssue(context.Background(), cfg, deps, 7)
	if err != nil {
		t.Fatalf("TickIssue: %v", err)
	}
	if spawned != 0 {
		t.Fatalf("spawned = %d, want 0: a delivery about #7 dispatched an agent for another issue", spawned)
	}
	if sum.Started != 0 {
		t.Errorf("Started = %d, want 0", sum.Started)
	}
	running, _ := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if len(running) != 0 {
		t.Fatalf("running dispatches = %+v, want none", running)
	}

	// The token burn is the other half of the bug and is invisible in the
	// dispatch count: the full reconcile read every open issue and every open
	// pull request for a delivery that named one issue.
	if gh.listedIssues != 0 || gh.listedPRs != 0 {
		t.Errorf("list calls: issues=%d prs=%d, want 0 and 0 for a scoped tick",
			gh.listedIssues, gh.listedPRs)
	}
	if len(gh.fetchedIssues) != 1 || gh.fetchedIssues[0] != 7 {
		t.Errorf("fetched issues = %v, want exactly [7]", gh.fetchedIssues)
	}

	// The control: the same fixture, delivered for the eligible issue, does
	// dispatch. Without this the assertion above would also pass if TickIssue
	// simply never dispatched anything.
	if _, err := TickIssue(context.Background(), cfg, deps, 51); err != nil {
		t.Fatalf("TickIssue(51): %v", err)
	}
	if spawned != 1 {
		t.Fatalf("spawned = %d after a delivery naming #51, want 1", spawned)
	}
	running, _ = deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if len(running) != 1 || running[0].Number != 51 {
		t.Fatalf("running dispatches = %+v, want one for issue 51", running)
	}
}

// PRs are also technically issues: they share the number space, and three of
// the five subscribed events name a pull request. Every row this loop keeps is
// keyed by ISSUE number, so a pull_request event has to act on the issue its
// pull request closes.
func TestTickIssueOnAPullRequestActsOnTheLinkedIssue(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 51, Labels: []string{"trigger"}}},
		prs: []ghub.PullRequest{{
			Number: 108, HeadRef: "feat/thing", BaseRef: "master",
			Body: "Closes #51", Trusted: true,
		}},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	if _, err := TickIssue(context.Background(), cfg, deps, 108); err != nil {
		t.Fatalf("TickIssue: %v", err)
	}
	if spawned != 1 {
		t.Fatalf("spawned = %d, want 1", spawned)
	}
	running, _ := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if len(running) != 1 || running[0].Number != 51 {
		t.Fatalf("running dispatches = %+v, want one keyed by issue 51, not by the pull request number", running)
	}
	// The resolution is what is under test, so the path it took is asserted
	// too: the delivered number is looked up as an issue first (GitHub's
	// issues endpoint answers a pull request there), then the linked issue.
	// Without this the assertions above would also pass for a full reconcile,
	// which dispatches #51 for reasons of its own.
	if gh.listedIssues != 0 || gh.listedPRs != 0 {
		t.Errorf("list calls: issues=%d prs=%d, want 0 and 0", gh.listedIssues, gh.listedPRs)
	}
	if len(gh.fetchedIssues) != 2 || gh.fetchedIssues[0] != 108 || gh.fetchedIssues[1] != 51 {
		t.Errorf("fetched issues = %v, want [108 51]: the delivered number, then the issue it closes", gh.fetchedIssues)
	}
}

// A pull request that closes no issue names no state this loop keeps. Falling
// back to treating 108 as issue 108 would decide a row that belongs to a
// different thing entirely.
func TestTickIssueOnAPullRequestClosingNoIssueIsANoOp(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 108, Labels: []string{"trigger"}}},
		prs: []ghub.PullRequest{{
			Number: 108, HeadRef: "feat/thing", BaseRef: "master",
			Body: "refs #51, but closes nothing", Trusted: true,
		}},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	if _, err := TickIssue(context.Background(), cfg, deps, 108); err != nil {
		t.Fatalf("TickIssue: %v", err)
	}
	if spawned != 0 {
		t.Fatalf("spawned = %d, want 0 for a pull request that closes no issue", spawned)
	}
	running, _ := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if len(running) != 0 {
		t.Fatalf("running dispatches = %+v, want none", running)
	}
}

// The scoped path takes the same per-loop lock the full tick takes, for the
// same reason: two agents in one worktree. Deliveries arrive seconds apart and
// a cron sweep may be running at the same moment, so this is the ordinary
// case, not an edge one.
func TestTickIssueTakesTheLoopLock(t *testing.T) {
	cfg := tickConfig(t)
	spawned := 0
	deps := newDeps(t, cfg, &noCallGH{t: t}, &spawned)

	held, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	_, err = TickIssue(context.Background(), cfg, deps, 7)
	if !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("TickIssue error = %v, want errors.Is(err, lock.ErrHeld)", err)
	}
	if spawned != 0 {
		t.Fatalf("spawned = %d, want 0: a held lock must dispatch nothing", spawned)
	}
}

// Reaping is scoped too. A delivery about #7 must not retire the dispatch rows
// of every other issue in the loop: that would flag them for retry and, on the
// next sweep, start a second agent for work that is still running.
func TestTickIssueReapsOnlyTheDeliveredIssuesDeadRunner(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{
		{Number: 7, Labels: []string{"in-flight"}},
		{Number: 51, Labels: []string{"in-flight"}},
	}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.IsAlive = func(int, int64) bool { return false }

	for _, n := range []int{7, 51} {
		id, _ := deps.Store.CreateDispatch(store.Dispatch{
			Loop: cfg.Name, Repo: cfg.Repo, Number: n, Kind: store.KindStart, SessionID: "s",
		})
		_ = deps.Store.SetDispatchProcess(id, 999999, time.Now().Add(-time.Hour))
		_ = deps.Store.PutIssueState(store.IssueState{
			Loop: cfg.Name, Repo: cfg.Repo, Number: n, SessionID: "s",
			SessionStarted: true, UpdatedAt: time.Now(),
		})
	}

	sum, err := TickIssue(context.Background(), cfg, deps, 7)
	if err != nil {
		t.Fatalf("TickIssue: %v", err)
	}
	if sum.Orphans != 1 {
		t.Errorf("Orphans = %d, want 1: only the delivered issue's row may be reaped", sum.Orphans)
	}

	// The delivered issue is reaped and retried, which adds a row of its own;
	// what matters is that the UNRELATED issue's live row was left exactly as
	// it was.
	running, _ := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	others := 0
	for _, d := range running {
		if d.Number == 51 {
			others++
		}
	}
	if others != 1 {
		t.Fatalf("running dispatches for issue 51 = %d, want its row left alone: %+v", others, running)
	}
	states, _ := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	// The failure was recorded and then spent by the retry this same pass
	// dispatched, which is what RetryCount reports; NeedsRetry is cleared by
	// the dispatch, exactly as in a full tick.
	if states[7].RetryCount != 1 {
		t.Errorf("RetryCount for the delivered issue = %d, want 1", states[7].RetryCount)
	}
	if states[51].NeedsRetry || states[51].RetryCount != 0 {
		t.Errorf("an unrelated issue was flagged for retry by a delivery about another issue: %+v", states[51])
	}
}

// A scoped tick still records a tick, so the counter and the summary keep
// meaning something: `loop status` reads them, and a daemon that ticked
// hundreds of times while recording none would look idle.
func TestTickIssueRecordsATick(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 7, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	if _, err := TickIssue(context.Background(), cfg, deps, 7); err != nil {
		t.Fatalf("TickIssue: %v", err)
	}
	n, err := deps.Store.TickCount(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("TickCount = %d, want 1", n)
	}
}

// The known regression, made loud instead of silent: engine.Decide's breaker
// counts retries within ONE call, and a scoped call holds at most one issue, so
// it can never reach a threshold above 1. Until the breaker counts over a
// rolling window, the operator has to be able to see that from the log.
func TestTickIssueWarnsThatTheBreakerCannotTrip(t *testing.T) {
	buf := captureTickLogs(t)
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 7, Labels: []string{"in-flight"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	_ = deps.Store.PutIssueState(store.IssueState{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 7, SessionID: "s",
		SessionStarted: true, NeedsRetry: true, UpdatedAt: time.Now(),
	})

	sum, err := TickIssue(context.Background(), cfg, deps, 7)
	if err != nil {
		t.Fatalf("TickIssue: %v", err)
	}
	if sum.Retried != 1 {
		t.Fatalf("Retried = %d, want 1; the warning is only meaningful on a retry", sum.Retried)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, breakerWarning) {
		t.Fatalf("a scoped retry must warn that the circuit breaker was not evaluated:\n%s", out)
	}
}

// A tick that never warns for an ordinary decision, so the warning above keeps
// its meaning instead of appearing on every delivery.
func TestTickIssueDoesNotWarnAboutTheBreakerWithoutARetry(t *testing.T) {
	buf := captureTickLogs(t)
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 7, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	if _, err := TickIssue(context.Background(), cfg, deps, 7); err != nil {
		t.Fatalf("TickIssue: %v", err)
	}
	if strings.Contains(buf.String(), breakerWarning) {
		t.Fatalf("the breaker warning fired for a tick with no retry:\n%s", buf.String())
	}
}

// Tending from a delivery costs one pull request fetch and one comparison, not
// a listing of every open pull request in the repository.
func TestTickIssueTendsFromASinglePullRequestFetch(t *testing.T) {
	cfg := tickConfig(t)
	cfg.Tend.Enabled = true
	gh := &fakeGH{
		issues: []ghub.Issue{
			{Number: 51, Labels: []string{"review"}},
			// Also awaiting review and also behind. A delivery about #51 must
			// not tend it.
			{Number: 60, Labels: []string{"review"}},
		},
		prs: []ghub.PullRequest{
			{Number: 108, HeadRef: "feat/51", BaseRef: "master", Body: "Closes #51", Trusted: true},
			{Number: 109, HeadRef: "feat/60", BaseRef: "master", Body: "Closes #60", Trusted: true},
		},
		behind: map[int]int{108: 16, 109: 16},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	// The delivery names the pull request, as a pull_request event does.
	sum, err := TickIssue(context.Background(), cfg, deps, 108)
	if err != nil {
		t.Fatalf("TickIssue: %v", err)
	}
	if sum.Tended != 1 {
		t.Fatalf("Tended = %d, want 1", sum.Tended)
	}
	if gh.listedPRs != 0 {
		t.Errorf("ListOpenPullRequests called %d times; a delivery must fetch only its own pull request", gh.listedPRs)
	}
	running, _ := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if len(running) != 1 || running[0].Number != 51 || running[0].PRNumber != 108 {
		t.Fatalf("running dispatches = %+v, want one tend of PR 108 for issue 51", running)
	}
}

// An issues event on a review-labelled issue has no pull request number in it,
// so the scoped tick uses the link a previous tick stored. This is what keeps
// "the human commented on the issue" able to tend, without listing every open
// pull request.
func TestTickIssueTendsThroughTheStoredPullRequestLink(t *testing.T) {
	cfg := tickConfig(t)
	cfg.Tend.Enabled = true
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 51, Labels: []string{"review"}}},
		prs: []ghub.PullRequest{{
			Number: 108, HeadRef: "feat/51", BaseRef: "master", Body: "Closes #51", Trusted: true,
		}},
		behind: map[int]int{108: 16},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	_ = deps.Store.PutPRLink(store.PRLink{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 51,
		PRNumber: 108, HeadRef: "feat/51", BaseRef: "master",
	})

	sum, err := TickIssue(context.Background(), cfg, deps, 51)
	if err != nil {
		t.Fatalf("TickIssue: %v", err)
	}
	if sum.Tended != 1 {
		t.Fatalf("Tended = %d, want 1", sum.Tended)
	}
	if gh.listedPRs != 0 {
		t.Errorf("ListOpenPullRequests called %d times, want 0", gh.listedPRs)
	}
	if len(gh.fetchedPRs) != 1 || gh.fetchedPRs[0] != 108 {
		t.Errorf("fetched pull requests = %v, want exactly [108]", gh.fetchedPRs)
	}
}

// Trust is re-decided on the fetched pull request, not inherited from the
// stored link. Tending checks the head branch out and runs an agent in it, so
// a pull request whose head has since moved to a fork must stop being tended.
func TestTickIssueDoesNotTendAnUntrustedPullRequest(t *testing.T) {
	cfg := tickConfig(t)
	cfg.Tend.Enabled = true
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 51, Labels: []string{"review"}}},
		prs: []ghub.PullRequest{{
			Number: 108, HeadRef: "feat/51", BaseRef: "master", Body: "Closes #51",
			HeadRepo: "attacker/r", Trusted: false,
		}},
		behind: map[int]int{108: 16},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TickIssue(context.Background(), cfg, deps, 108)
	if err != nil {
		t.Fatalf("TickIssue: %v", err)
	}
	if sum.Tended != 0 || spawned != 0 {
		t.Fatalf("Tended = %d, spawned = %d, want 0 and 0 for an untrusted head", sum.Tended, spawned)
	}
	// The trust check has to happen in this pass, not only inside the engine:
	// an untrusted pull request must not even be recorded as this issue's link,
	// or a later tick would read it back as the branch to check out.
	links, _ := deps.Store.PRLinks(cfg.Name, cfg.Repo)
	if _, ok := links[51]; ok {
		t.Errorf("an untrusted pull request was stored as issue 51's link: %+v", links)
	}
}

// secondLoopConfig returns a second loop of the same repository, with its own
// name and its own state directory.
//
// It exists because the saving under test is only visible with more than one
// loop watching one repository: that is the shape the webhook daemon fans a
// delivery out into, and the shape that made one event cost N identical
// fetches of one issue.
func secondLoopConfig(t *testing.T, name string) *config.Config {
	t.Helper()
	cfg := tickConfig(t)
	cfg.Name = name
	return cfg
}

// The fan-out: one delivery, two loops watching the repository, ONE fetch of
// the issue it named. Each loop keeps its own state and spends its own budget,
// but they were all asking GitHub the same question at the same instant.
func TestOneDeliveryFetchesTheIssueOnceAcrossEveryLoop(t *testing.T) {
	planning := tickConfig(t)
	execution := secondLoopConfig(t, "execution")
	gh := &fakeGH{issues: []ghub.Issue{{Number: 51, Labels: []string{"trigger"}}}}
	// One cache for one delivery -- exactly what listener.Deliver builds and
	// drops around its loop over the targets.
	shared := ghub.NewDeliveryCache(gh)

	spawned := 0
	for _, cfg := range []*config.Config{planning, execution} {
		deps := newDeps(t, cfg, shared, &spawned)
		if _, err := TickIssue(context.Background(), cfg, deps, 51); err != nil {
			t.Fatalf("TickIssue(%s): %v", cfg.Name, err)
		}
	}

	if len(gh.fetchedIssues) != 1 || gh.fetchedIssues[0] != 51 {
		t.Errorf("fetched issues = %v, want exactly [51] for one delivery to two loops", gh.fetchedIssues)
	}
	// The control. Without it the assertion above would also pass if the
	// second loop had simply decided nothing at all.
	if spawned != 2 {
		t.Errorf("spawned = %d, want one agent per loop: the shared fetch must not skip a loop", spawned)
	}
}

// The staleness guard, and the reason the cache's lifetime is one delivery.
//
// The daemon decides from LABELS. This is the exact sequence the daemon exists
// to answer: a delivery about an issue with nothing to do, then the delivery
// raised BECAUSE the trigger label was added. A cache that outlived the first
// delivery would answer the second with the labels from before the label that
// caused it, and no agent would ever start.
func TestASecondDeliveryFetchesTheIssueAgainAndSeesTheNewLabels(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 51}}}
	spawned := 0
	deps := newDeps(t, cfg, ghub.NewDeliveryCache(gh), &spawned)

	if _, err := TickIssue(context.Background(), cfg, deps, 51); err != nil {
		t.Fatalf("TickIssue: %v", err)
	}
	if spawned != 0 {
		t.Fatalf("spawned = %d, want 0 for an issue carrying no trigger label", spawned)
	}

	// The label that raises the second delivery.
	gh.issues = []ghub.Issue{{Number: 51, Labels: []string{"trigger"}}}
	deps.GH = ghub.NewDeliveryCache(gh)
	if _, err := TickIssue(context.Background(), cfg, deps, 51); err != nil {
		t.Fatalf("TickIssue (second delivery): %v", err)
	}

	if len(gh.fetchedIssues) != 2 {
		t.Errorf("fetched issues = %v, want one fetch per delivery", gh.fetchedIssues)
	}
	if spawned != 1 {
		t.Errorf("spawned = %d, want 1: the second delivery must decide from the labels that raised it", spawned)
	}
}

// A delivery about a pull request costs the same two fetches however many
// loops watch the repository: the delivered number (answered as a pull
// request), then the issue it closes. engine.ClosesIssue reads the body those
// fetches returned, so the resolution is settled once for the delivery.
func TestOneDeliveryResolvesAPullRequestToItsIssueOnce(t *testing.T) {
	planning := tickConfig(t)
	execution := secondLoopConfig(t, "execution")
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 51, Labels: []string{"trigger"}}},
		prs: []ghub.PullRequest{{
			Number: 108, HeadRef: "feat/51", BaseRef: "master",
			Body: "Closes #51", Trusted: true,
		}},
	}
	shared := ghub.NewDeliveryCache(gh)

	spawned := 0
	var stores []*store.Store
	for _, cfg := range []*config.Config{planning, execution} {
		deps := newDeps(t, cfg, shared, &spawned)
		stores = append(stores, deps.Store)
		if _, err := TickIssue(context.Background(), cfg, deps, 108); err != nil {
			t.Fatalf("TickIssue(%s): %v", cfg.Name, err)
		}
	}

	if len(gh.fetchedPRs) != 1 || gh.fetchedPRs[0] != 108 {
		t.Errorf("fetched pull requests = %v, want exactly [108]", gh.fetchedPRs)
	}
	if len(gh.fetchedIssues) != 2 || gh.fetchedIssues[0] != 108 || gh.fetchedIssues[1] != 51 {
		t.Errorf("fetched issues = %v, want [108 51]: the delivered number once, then the issue it closes once",
			gh.fetchedIssues)
	}
	// The control: both loops really did resolve the pull request to issue 51.
	if spawned != 2 {
		t.Fatalf("spawned = %d, want one agent per loop", spawned)
	}
	for i, s := range stores {
		running, _ := s.RunningDispatches([]string{"planning", "execution"}[i], "o/r")
		if len(running) != 1 || running[0].Number != 51 {
			t.Errorf("loop %d running = %+v, want one dispatch keyed by issue 51", i, running)
		}
	}
}

// Trust is still decided per the existing path when the fetch is shared.
// Tending checks the head branch out and runs an agent inside it, so the
// Trusted flag convertPR set at the API boundary is what every loop of the
// delivery must read -- neither dropped by the sharing nor recomputed beside
// it.
func TestASharedFetchStillDecidesTrustForATend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trusted bool
		want    int
	}{
		{"trusted head", true, 2},
		{"fork head", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			planning := tickConfig(t)
			planning.Tend.Enabled = true
			execution := secondLoopConfig(t, "execution")
			execution.Tend.Enabled = true
			// Each loop's own project would name it as the tend host; these
			// two are separate projects watching one repository, which is the
			// case this test is about.
			execution.Tend.Loop = execution.Name
			gh := &fakeGH{
				issues: []ghub.Issue{{Number: 51, Labels: []string{"review"}}},
				prs: []ghub.PullRequest{{
					Number: 108, HeadRef: "feat/51", BaseRef: "master",
					Body: "Closes #51", HeadRepo: "o/r", Trusted: tc.trusted,
				}},
				behind: map[int]int{108: 16},
			}
			shared := ghub.NewDeliveryCache(gh)

			spawned, tended := 0, 0
			for _, cfg := range []*config.Config{planning, execution} {
				deps := newDeps(t, cfg, shared, &spawned)
				sum, err := TickIssue(context.Background(), cfg, deps, 108)
				if err != nil {
					t.Fatalf("TickIssue(%s): %v", cfg.Name, err)
				}
				tended += sum.Tended
			}

			if tended != tc.want || spawned != tc.want {
				t.Errorf("tended = %d, spawned = %d, want %d of each", tended, spawned, tc.want)
			}
			if len(gh.fetchedPRs) != 1 {
				t.Errorf("fetched pull requests = %v, want one fetch for the delivery", gh.fetchedPRs)
			}
		})
	}
}
