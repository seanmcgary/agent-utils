package loopcmd

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/ghub"
	_ "modernc.org/sqlite"
)

// The review trigger fails CLOSED: a failed LatestReviewActivity read leaves
// the pull request's entry unset, so tendDecisions judges it on staleness
// alone. A current pull request with no other reason to tend must therefore
// still dispatch nothing, even though the read failed -- treating a failed
// read as "everything is pending" would answer one broken call with a burst
// of dispatches, the opposite of the goal.
func TestTickDoesNotTendACurrentPullRequestWhenReviewReadFails(t *testing.T) {
	cfg := tickConfig(t)
	cfg.TendPR = true
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 51, Labels: []string{"review"}}},
		prs: []ghub.PullRequest{
			{Number: 108, HeadRef: "feat/51", BaseRef: "master", Body: "Closes #51", Trusted: true},
		},
		behind:            map[int]int{108: 0},
		reviewActivityErr: map[int]error{108: context.DeadlineExceeded},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if sum.Tended != 0 {
		t.Errorf("Tended = %d, want 0: a failed review read must not manufacture a reason to dispatch", sum.Tended)
	}
}

// Same property through the delivery fast path, TickIssue, which reads
// LatestReviewActivity beside its own BehindBy call.
func TestTickIssueDoesNotTendACurrentPullRequestWhenReviewReadFails(t *testing.T) {
	cfg := tickConfig(t)
	cfg.TendPR = true
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 51, Labels: []string{"review"}}},
		prs: []ghub.PullRequest{
			{Number: 108, HeadRef: "feat/51", BaseRef: "master", Body: "Closes #51", Trusted: true},
		},
		behind:            map[int]int{108: 0},
		reviewActivityErr: map[int]error{108: context.DeadlineExceeded},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TickIssue(context.Background(), cfg, deps, 108)
	if err != nil {
		t.Fatalf("TickIssue: %v", err)
	}
	if sum.Tended != 0 {
		t.Errorf("Tended = %d, want 0: a failed review read must not manufacture a reason to dispatch", sum.Tended)
	}
}

// The positive case beside the failure one: review activity newer than the
// last tend produces a tend dispatch for a pull request that is otherwise
// current, and the dispatch row carries ReviewPending.
func TestTickTendsACurrentPullRequestWithNewReviewActivity(t *testing.T) {
	cfg := tickConfig(t)
	cfg.TendPR = true
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 51, Labels: []string{"review"}}},
		prs: []ghub.PullRequest{
			{Number: 108, HeadRef: "feat/51", BaseRef: "master", Body: "Closes #51", Trusted: true},
		},
		behind:         map[int]int{108: 0},
		reviewActivity: map[int]time.Time{108: time.Now()},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if sum.Tended != 1 {
		t.Fatalf("Tended = %d, want 1: review activity with no prior tend is pending", sum.Tended)
	}

	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || !running[0].ReviewPending {
		t.Fatalf("running dispatch = %+v, want one row with ReviewPending set", running)
	}
}

// An unreadable last-tend time suppresses the review trigger for the whole
// pass, and the proof is that no review read is even attempted.
//
// This is the failure direction that matters most in the whole feature. An
// unset lastTend entry reads as the zero time, and ANY review activity is
// After(zero) -- so a pass that lost its last-tend answer but still read
// review activity would mark every review-labelled pull request in the
// repository as review-pending and answer one broken store read with a burst
// of agent dispatches, at roughly $0.75 each. Judging on staleness alone is
// exactly the behaviour this loop had before the trigger existed, so failing
// closed costs nothing that was ever promised.
func TestTickSkipsTheReviewReadWhenTheLastTendReadFails(t *testing.T) {
	cfg := tickConfig(t)
	cfg.TendPR = true
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 51, Labels: []string{"review"}}},
		prs: []ghub.PullRequest{
			{Number: 108, HeadRef: "feat/51", BaseRef: "master", Body: "Closes #51", Trusted: true},
		},
		behind: map[int]int{108: 0},
		// The read would report activity if it ran. It must not run.
		reviewActivity: map[int]time.Time{108: time.Now()},
	}
	spawned := 0
	deps, dbPath := newDepsAt(t, cfg, gh, &spawned)

	// Break the last-tend read, and only that read.
	dropDispatches(t, dbPath)

	// The tick itself fails afterwards, when it reads running dispatches from
	// the same table. That is expected and is not what this test asserts: the
	// property is what happened BEFORE that, while the pass still had a
	// choice about spending a GitHub call.
	_, _ = Tick(context.Background(), cfg, deps)

	if gh.reviewActivityCalls != 0 {
		t.Errorf("LatestReviewActivity was called %d times, want 0: an unknown last-tend time must suppress the review trigger, not be read around",
			gh.reviewActivityCalls)
	}
	if spawned != 0 {
		t.Errorf("spawned = %d, want 0", spawned)
	}
}

// dropDispatches removes the table LastTendAt and LastTendByPR select from, so
// a test can fail exactly that read.
//
// Deps.Store is a concrete type with no seam to inject a failure through, and
// introducing an interface for two tests would be a wider change than the
// property is worth. The pass under test fails later, when it reads running
// dispatches from the same table; the tests assert what happened BEFORE that,
// while the pass still had a choice about spending a GitHub call.
func dropDispatches(t *testing.T, dbPath string) {
	t.Helper()
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec("DROP TABLE dispatches"); err != nil {
		t.Fatal(err)
	}
}

// The delivery fast path fails closed the same way the full tick does, and for
// the same reason: an unset lastTend entry reads as the zero time, so any
// review activity is After(zero). TickIssue orders the store read FIRST and
// skips the GitHub read when it fails; without that ordering a failed store
// read would manufacture a reason to dispatch an agent.
func TestTickIssueSkipsTheReviewReadWhenTheLastTendReadFails(t *testing.T) {
	cfg := tickConfig(t)
	cfg.TendPR = true
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 51, Labels: []string{"review"}}},
		prs: []ghub.PullRequest{
			{Number: 108, HeadRef: "feat/51", BaseRef: "master", Body: "Closes #51", Trusted: true},
		},
		behind: map[int]int{108: 0},
		// The read would report activity if it ran. It must not run.
		reviewActivity: map[int]time.Time{108: time.Now()},
	}
	spawned := 0
	deps, dbPath := newDepsAt(t, cfg, gh, &spawned)

	dropDispatches(t, dbPath)

	_, _ = TickIssue(context.Background(), cfg, deps, 108)

	if gh.reviewActivityCalls != 0 {
		t.Errorf("LatestReviewActivity was called %d times, want 0: an unknown last-tend time must suppress the review trigger",
			gh.reviewActivityCalls)
	}
	if spawned != 0 {
		t.Errorf("spawned = %d, want 0", spawned)
	}
}
