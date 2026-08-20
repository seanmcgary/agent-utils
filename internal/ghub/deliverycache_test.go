package ghub

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// countingGH is a Client that answers from a fixture and counts what it was
// asked for. The saving a DeliveryCache exists for is invisible in the values
// returned and visible only in these counters.
type countingGH struct {
	issues []Issue
	prs    []PullRequest

	fetchedIssues []int
	fetchedPRs    []int
	listedIssues  int
	listedPRs     int
	compares      int
	comments      int
	edits         int
}

// Issue answers ErrNotAnIssue for a number the fixture holds as a pull
// request, exactly as GitHub's issues endpoint does.
func (f *countingGH) Issue(_ context.Context, _, _ string, number int) (Issue, error) {
	f.fetchedIssues = append(f.fetchedIssues, number)
	for _, pr := range f.prs {
		if pr.Number == number {
			return Issue{}, fmt.Errorf("o/r#%d: %w", number, ErrNotAnIssue)
		}
	}
	for _, iss := range f.issues {
		if iss.Number == number {
			return iss, nil
		}
	}
	return Issue{}, fmt.Errorf("issue #%d not found", number)
}

func (f *countingGH) PullRequest(_ context.Context, _, _ string, number int) (PullRequest, error) {
	f.fetchedPRs = append(f.fetchedPRs, number)
	for _, pr := range f.prs {
		if pr.Number == number {
			return pr, nil
		}
	}
	return PullRequest{}, fmt.Errorf("pull request #%d not found", number)
}

func (f *countingGH) ListOpenIssues(context.Context, string, string) ([]Issue, error) {
	f.listedIssues++
	return f.issues, nil
}

func (f *countingGH) ListOpenPullRequests(context.Context, string, string) ([]PullRequest, error) {
	f.listedPRs++
	return f.prs, nil
}

func (f *countingGH) BehindBy(context.Context, string, string, string, string) (int, error) {
	f.compares++
	return 16, nil
}

func (f *countingGH) PostComment(context.Context, string, string, int, string) error {
	f.comments++
	return nil
}

func (f *countingGH) EditLabels(context.Context, string, string, int, []string, []string) error {
	f.edits++
	return nil
}

// One delivery fans out across every loop watching the repository, and each
// loop asks for the SAME issue. Two loops meant two identical fetches; ten
// loops meant ten.
func TestDeliveryCacheFetchesAnIssueOnce(t *testing.T) {
	gh := &countingGH{issues: []Issue{{Number: 51, Labels: []string{"trigger"}}}}
	c := NewDeliveryCache(gh)

	first, err := c.Issue(context.Background(), "o", "r", 51)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	second, err := c.Issue(context.Background(), "o", "r", 51)
	if err != nil {
		t.Fatalf("Issue (second): %v", err)
	}

	if len(gh.fetchedIssues) != 1 {
		t.Errorf("fetched issues = %v, want exactly one fetch for one delivery", gh.fetchedIssues)
	}
	if first.Number != 51 || second.Number != 51 || !second.HasLabel("trigger") {
		t.Errorf("the memoised answer differs from the fetched one: %+v then %+v", first, second)
	}
}

// A cache is one delivery's, so a second cache must fetch again. This is the
// staleness guard: the daemon decides from an issue's LABELS, and a value
// carried between deliveries would decide from the state before the label
// that triggered the second one.
func TestASecondDeliveryCacheFetchesAgain(t *testing.T) {
	gh := &countingGH{issues: []Issue{{Number: 51}}}

	if _, err := NewDeliveryCache(gh).Issue(context.Background(), "o", "r", 51); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := NewDeliveryCache(gh).Issue(context.Background(), "o", "r", 51); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if len(gh.fetchedIssues) != 2 {
		t.Errorf("fetched issues = %v, want one fetch per cache", gh.fetchedIssues)
	}
}

// ErrNotAnIssue is an ANSWER, not a transport failure: it is how the delivered
// number is recognised as a pull request. Every loop of the delivery walks
// that same path, so the answer is memoised with the same sentinel intact --
// a caller matches it with errors.Is.
func TestDeliveryCacheMemoisesTheNotAnIssueAnswer(t *testing.T) {
	gh := &countingGH{prs: []PullRequest{{Number: 108}}}
	c := NewDeliveryCache(gh)

	for i := range 2 {
		if _, err := c.Issue(context.Background(), "o", "r", 108); !errors.Is(err, ErrNotAnIssue) {
			t.Fatalf("Issue call %d error = %v, want errors.Is(err, ErrNotAnIssue)", i, err)
		}
	}
	if len(gh.fetchedIssues) != 1 {
		t.Errorf("fetched issues = %v, want exactly one fetch", gh.fetchedIssues)
	}
}

// The pull request is fetched once too, and the trust convertPR decided at the
// API boundary is what every loop reads. Tending checks the head branch out
// and runs an agent in it, so a cache that dropped or re-derived Trusted would
// be a code execution bug, not a caching one.
func TestDeliveryCacheFetchesAPullRequestOnceAndKeepsItsTrust(t *testing.T) {
	gh := &countingGH{prs: []PullRequest{{
		Number: 108, HeadRef: "feat/51", BaseRef: "master",
		Body: "Closes #51", Trusted: true, HeadRepo: "o/r",
	}}}
	c := NewDeliveryCache(gh)

	first, err := c.PullRequest(context.Background(), "o", "r", 108)
	if err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
	second, err := c.PullRequest(context.Background(), "o", "r", 108)
	if err != nil {
		t.Fatalf("PullRequest (second): %v", err)
	}

	if len(gh.fetchedPRs) != 1 {
		t.Errorf("fetched pull requests = %v, want exactly one fetch", gh.fetchedPRs)
	}
	if !first.Trusted || !second.Trusted || second.HeadRepo != "o/r" || second.Body != "Closes #51" {
		t.Errorf("the memoised pull request differs from the fetched one: %+v then %+v", first, second)
	}
}

// The cache is keyed by the number it was asked for. Answering issue 60 from
// issue 51's fetch would decide one issue from another's labels.
func TestDeliveryCacheKeepsNumbersApart(t *testing.T) {
	gh := &countingGH{issues: []Issue{
		{Number: 51, Labels: []string{"trigger"}},
		{Number: 60, Labels: []string{"review"}},
	}}
	c := NewDeliveryCache(gh)

	a, err := c.Issue(context.Background(), "o", "r", 51)
	if err != nil {
		t.Fatalf("Issue(51): %v", err)
	}
	b, err := c.Issue(context.Background(), "o", "r", 60)
	if err != nil {
		t.Fatalf("Issue(60): %v", err)
	}

	if a.Number != 51 || !a.HasLabel("trigger") {
		t.Errorf("issue 51 = %+v", a)
	}
	if b.Number != 60 || !b.HasLabel("review") {
		t.Errorf("issue 60 = %+v", b)
	}
	if len(gh.fetchedIssues) != 2 {
		t.Errorf("fetched issues = %v, want one fetch per number", gh.fetchedIssues)
	}
}

// A write this program makes to an issue's labels invalidates what it holds
// for that issue. parkRetryExhausted removes the trigger and in-flight labels,
// and a later loop of the same delivery deciding from the labels as they were
// BEFORE that write would dispatch the agent the park exists to stop.
func TestDeliveryCacheRefetchesAnIssueAfterItsLabelsAreEdited(t *testing.T) {
	gh := &countingGH{issues: []Issue{{Number: 51, Labels: []string{"trigger"}}}}
	c := NewDeliveryCache(gh)

	if _, err := c.Issue(context.Background(), "o", "r", 51); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := c.EditLabels(context.Background(), "o", "r", 51, nil, []string{"trigger"}); err != nil {
		t.Fatalf("EditLabels: %v", err)
	}
	if _, err := c.Issue(context.Background(), "o", "r", 51); err != nil {
		t.Fatalf("Issue (after the edit): %v", err)
	}

	if gh.edits != 1 {
		t.Errorf("EditLabels reached the client %d times, want 1: a cache must not swallow a write", gh.edits)
	}
	if len(gh.fetchedIssues) != 2 {
		t.Errorf("fetched issues = %v, want a refetch after this program edited the labels", gh.fetchedIssues)
	}
}

// Everything else is passed straight through. The listings belong to the cron
// sweep, which reconciles a whole repository and must read it as it is now;
// the comparison and the comment are not repeated reads of one number.
func TestDeliveryCachePassesEverythingElseThrough(t *testing.T) {
	gh := &countingGH{issues: []Issue{{Number: 51}}, prs: []PullRequest{{Number: 108}}}
	c := NewDeliveryCache(gh)
	ctx := context.Background()

	for range 2 {
		if _, err := c.ListOpenIssues(ctx, "o", "r"); err != nil {
			t.Fatalf("ListOpenIssues: %v", err)
		}
		if _, err := c.ListOpenPullRequests(ctx, "o", "r"); err != nil {
			t.Fatalf("ListOpenPullRequests: %v", err)
		}
		if _, err := c.BehindBy(ctx, "o", "r", "master", "feat/51"); err != nil {
			t.Fatalf("BehindBy: %v", err)
		}
		if err := c.PostComment(ctx, "o", "r", 51, "body"); err != nil {
			t.Fatalf("PostComment: %v", err)
		}
	}

	if gh.listedIssues != 2 || gh.listedPRs != 2 || gh.compares != 2 || gh.comments != 2 {
		t.Errorf("listedIssues=%d listedPRs=%d compares=%d comments=%d, want 2 of each",
			gh.listedIssues, gh.listedPRs, gh.compares, gh.comments)
	}
}
