package loopcmd

import (
	"context"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/ghub"
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
