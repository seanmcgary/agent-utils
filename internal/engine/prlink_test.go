package engine

import (
	"testing"

	"github.com/seanmcgary/agent-utils/internal/ghub"
)

func TestLinkPR(t *testing.T) {
	prs := []ghub.PullRequest{
		{Number: 10, Body: "Closes #5", HeadRef: "feat/five", BaseRef: "master", Trusted: true},
		{Number: 11, Body: "unrelated work", Trusted: true},
		{Number: 12, Body: "fixes #123", Trusted: true},
		{Number: 13, Body: "Resolves #7\n\nmore text", Trusted: true},
	}

	cases := []struct {
		issue   int
		wantPR  int
		wantHit bool
	}{
		{5, 10, true},
		{7, 13, true},
		{123, 12, true},
		{12, 0, false}, // "#123" must not match issue 12
		{999, 0, false},
	}

	for _, c := range cases {
		got, ok := LinkPR(c.issue, prs)
		if ok != c.wantHit {
			t.Errorf("LinkPR(%d) ok = %v, want %v", c.issue, ok, c.wantHit)
			continue
		}
		if ok && got.Number != c.wantPR {
			t.Errorf("LinkPR(%d) = PR %d, want PR %d", c.issue, got.Number, c.wantPR)
		}
	}
}

func TestLinkPRIgnoresEmptyBody(t *testing.T) {
	if _, ok := LinkPR(1, []ghub.PullRequest{{Number: 2, Trusted: true}}); ok {
		t.Error("an empty body must not link")
	}
}

// A fork pull request can claim "Closes #N" for any issue. Linking it would make
// the tend path check an untrusted branch out and run an agent inside it.
func TestLinkPRIgnoresUntrustedPullRequest(t *testing.T) {
	prs := []ghub.PullRequest{{Number: 9, Body: "Closes #1", Trusted: false}}
	if _, ok := LinkPR(1, prs); ok {
		t.Error("an untrusted pull request must never link")
	}
}

func TestLinkPRPrefersLowestNumber(t *testing.T) {
	prs := []ghub.PullRequest{
		{Number: 30, Body: "Closes #4", Trusted: true},
		{Number: 12, Body: "Closes #4", Trusted: true},
	}
	got, ok := LinkPR(4, prs)
	if !ok || got.Number != 12 {
		t.Errorf("got PR %d (ok=%v), want the lowest trusted match, PR 12", got.Number, ok)
	}
}

// A pull_request event names a PR, but every row this program writes --
// sessions, retries, the in-flight label, dispatch rows -- is keyed by ISSUE
// number. Resolving the PR to the issue it closes is what lets a delivery
// about a PR act on the state the loop actually keeps.
func TestClosesIssue(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
		ok   bool
	}{
		{"closes", "Closes #51", 51, true},
		{"fixes lowercase", "some prose\n\nfixes #7\n", 7, true},
		{"resolved past tense", "Resolved #12", 12, true},
		{"no keyword", "see #51 for context", 0, false},
		{"empty body", "", 0, false},
		{"multi digit is not truncated", "Closes #123", 123, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ClosesIssue(ghub.PullRequest{Number: 108, Body: c.body})
			if ok != c.ok || got != c.want {
				t.Fatalf("ClosesIssue(%q) = (%d, %v), want (%d, %v)", c.body, got, ok, c.want, c.ok)
			}
		})
	}
}

// Determinism matters for the same reason it does in LinkPR: a body naming
// two issues must always resolve to the same one, or one delivery would
// dispatch an agent for a different issue than the next identical delivery.
func TestClosesIssuePrefersLowestNumber(t *testing.T) {
	got, ok := ClosesIssue(ghub.PullRequest{Body: "Closes #51 and fixes #12"})
	if !ok || got != 12 {
		t.Fatalf("ClosesIssue = (%d, %v), want (12, true)", got, ok)
	}
}
