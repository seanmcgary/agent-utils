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
