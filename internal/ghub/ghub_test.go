package ghub

import (
	"testing"

	"github.com/google/go-github/v77/github"
)

func TestConvertIssuesDropsPullRequests(t *testing.T) {
	in := []*github.Issue{
		{
			Number: github.Ptr(1),
			Title:  github.Ptr("a real issue"),
			Labels: []*github.Label{{Name: github.Ptr("status:ready-for-spec")}},
		},
		{
			Number:           github.Ptr(2),
			Title:            github.Ptr("actually a pull request"),
			PullRequestLinks: &github.PullRequestLinks{URL: github.Ptr("https://example/pr/2")},
		},
	}

	got := ConvertIssues(in)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1; pull requests must be dropped", len(got))
	}
	if got[0].Number != 1 {
		t.Errorf("Number = %d, want 1", got[0].Number)
	}
	if len(got[0].Labels) != 1 || got[0].Labels[0] != "status:ready-for-spec" {
		t.Errorf("Labels = %v", got[0].Labels)
	}
}

func TestConvertIssuesTolerantOfNilFields(t *testing.T) {
	in := []*github.Issue{{Number: github.Ptr(7)}}
	got := ConvertIssues(in)
	if len(got) != 1 || got[0].Number != 7 {
		t.Fatalf("got = %+v", got)
	}
	if got[0].Labels == nil {
		t.Error("Labels must be a non-nil empty slice")
	}
}

func TestHasLabel(t *testing.T) {
	i := Issue{Labels: []string{"Status:Ready-For-Spec", "blocked:design"}}
	if !i.HasLabel("status:ready-for-spec") {
		t.Error("HasLabel must be case insensitive")
	}
	if !i.HasAnyLabel([]string{"nope", "blocked:design"}) {
		t.Error("HasAnyLabel must match the second entry")
	}
	if i.HasAnyLabel(nil) {
		t.Error("HasAnyLabel(nil) must be false")
	}
	if i.HasLabel("missing") {
		t.Error("HasLabel must be false for an absent label")
	}
}
