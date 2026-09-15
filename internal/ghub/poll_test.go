package ghub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-github/v77/github"
)

// A poll must see pull requests as well as issues: they share the endpoint
// because they share a number space, and three of the delivery flags the
// poller derives turn on which kind a subject is. ConvertIssues drops them,
// which is why this path does not use it.
func TestListSubjectsUpdatedSinceKeepsPullRequests(t *testing.T) {
	var gotQuery url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]*github.Issue{
			{
				Number:    github.Ptr(51),
				State:     github.Ptr("open"),
				Labels:    []*github.Label{{Name: github.Ptr("status:ready")}},
				UpdatedAt: &github.Timestamp{Time: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)},
			},
			{
				Number:           github.Ptr(52),
				State:            github.Ptr("closed"),
				UpdatedAt:        &github.Timestamp{Time: time.Date(2026, 9, 15, 11, 0, 0, 0, time.UTC)},
				PullRequestLinks: &github.PullRequestLinks{URL: github.Ptr("https://api.github.com/repos/o/r/pulls/52")},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	since := time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)
	got, err := newTestClient(t, srv).ListSubjectsUpdatedSince(context.Background(), "o", "r", since)
	if err != nil {
		t.Fatalf("ListSubjectsUpdatedSince: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (the pull request must not be dropped): %+v", len(got), got)
	}
	if got[0].Number != 51 || got[0].IsPullRequest {
		t.Errorf("subject[0] = %+v, want issue 51", got[0])
	}
	if got[1].Number != 52 || !got[1].IsPullRequest {
		t.Errorf("subject[1] = %+v, want pull request 52", got[1])
	}
	if got[0].State != "open" || got[1].State != "closed" {
		t.Errorf("states = %q, %q; want open, closed", got[0].State, got[1].State)
	}
	if len(got[0].Labels) != 1 || got[0].Labels[0] != "status:ready" {
		t.Errorf("labels = %v, want [status:ready]", got[0].Labels)
	}

	// The query is load-bearing: state=all is what makes a close visible at
	// all, and direction=asc is what lets the caller advance its cursor to the
	// last subject it fully processed after a mid-page failure.
	for key, want := range map[string]string{
		"state": "all", "sort": "updated", "direction": "asc",
		"since": "2026-09-15T09:00:00Z",
	} {
		if gotQuery.Get(key) != want {
			t.Errorf("query %s = %q, want %q", key, gotQuery.Get(key), want)
		}
	}
}

// A merge arms the tend sweep. Without this field the poller cannot tell a
// merged pull request from an abandoned one, and a merge would arm nothing.
func TestPullRequestCarriesMerged(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls/108", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		pr := insiderPR()
		pr.State = github.Ptr("closed")
		pr.Merged = github.Ptr(true)
		_ = json.NewEncoder(w).Encode(pr)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).PullRequest(context.Background(), "o", "r", 108)
	if err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
	if !got.Merged {
		t.Error("Merged = false, want true")
	}
	if got.BaseRef != "master" {
		t.Errorf("BaseRef = %q, want master", got.BaseRef)
	}
}

// A direct push to the default branch produces no pull request event at all,
// so the poller watches the branch itself. BehindBy answers a different
// question -- how far one ref trails another -- and cannot report identity.
func TestBranchHeadReturnsTheCommitSHA(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/commits/master", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&github.RepositoryCommit{SHA: github.Ptr("deadbeef")})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).BranchHead(context.Background(), "o", "r", "master")
	if err != nil {
		t.Fatalf("BranchHead: %v", err)
	}
	if got != "deadbeef" {
		t.Errorf("BranchHead = %q, want deadbeef", got)
	}
}
