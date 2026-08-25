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

func TestConvertIssuesCarriesState(t *testing.T) {
	got := ConvertIssues([]*github.Issue{
		{Number: github.Ptr(1), State: github.Ptr("open")},
		{Number: github.Ptr(2), State: github.Ptr("closed")},
	})
	if len(got) != 2 {
		t.Fatalf("ConvertIssues returned %d issues, want 2", len(got))
	}
	if !got[0].IsOpen() {
		t.Errorf("issue 1 State = %q, want it to read as open", got[0].State)
	}
	if got[1].IsOpen() {
		t.Errorf("issue 2 State = %q, want it to read as closed", got[1].State)
	}
}

// The repository an issue lives in decides whether the sweep may write to it by
// number. A sub-issue may live in ANOTHER repository, and its number means
// nothing here.
func TestConvertIssuesCarriesTheRepository(t *testing.T) {
	got := ConvertIssues([]*github.Issue{
		{Number: github.Ptr(1), RepositoryURL: github.Ptr("https://api.github.com/repos/o/r")},
		{Number: github.Ptr(2), Repository: &github.Repository{FullName: github.Ptr("other/repo")}},
		{Number: github.Ptr(3)},
	})
	if len(got) != 3 {
		t.Fatalf("ConvertIssues returned %d issues, want 3", len(got))
	}
	if got[0].Repo != "o/r" {
		t.Errorf("Repo from repository_url = %q, want o/r", got[0].Repo)
	}
	if got[1].Repo != "other/repo" {
		t.Errorf("Repo from repository object = %q, want other/repo", got[1].Repo)
	}
	// An issue that names no repository must NOT be assumed local. InRepo is
	// what the sweep gates its write on, and the safe answer to "unknown" is no.
	if got[2].Repo != "" {
		t.Errorf("Repo = %q for an issue naming none, want empty", got[2].Repo)
	}
	if got[2].InRepo("o", "r") {
		t.Error("an issue naming no repository must not read as local")
	}
}

func TestInRepoFoldsCase(t *testing.T) {
	i := Issue{Repo: "McGaryLabs/Koinos"}
	if !i.InRepo("mcgarylabs", "koinos") {
		t.Error("InRepo must fold case, as HasLabel does")
	}
	if i.InRepo("mcgarylabs", "other") {
		t.Error("InRepo matched the wrong repository")
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
