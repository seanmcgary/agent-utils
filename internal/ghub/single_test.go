package ghub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v77/github"
)

// insiderPR is one pull request whose head lives in this repository and whose
// author is a repository member: the shape ListOpenPullRequests marks trusted.
func insiderPR() *github.PullRequest {
	return &github.PullRequest{
		Number: github.Ptr(108),
		Head: &github.PullRequestBranch{
			Ref:  github.Ptr("feat/thing"),
			Repo: &github.Repository{FullName: github.Ptr("o/r")},
		},
		Base:              &github.PullRequestBranch{Ref: github.Ptr("master")},
		Body:              github.Ptr("Closes #51"),
		Draft:             github.Ptr(false),
		AuthorAssociation: github.Ptr("OWNER"),
	}
}

// A delivery names one issue, so the tick that answers it fetches one issue.
// The list endpoints are what burned the token budget: a delivery about an
// unlabelled test issue read every open issue, every open pull request and a
// commit comparison per review issue.
func TestIssueFetchesOneIssue(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/51", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&github.Issue{
			Number: github.Ptr(51),
			Title:  github.Ptr("a real issue"),
			Labels: []*github.Label{{Name: github.Ptr("trigger")}},
		})
	})
	mux.HandleFunc("/repos/o/r/issues", func(http.ResponseWriter, *http.Request) {
		t.Fatal("the list endpoint must not be called for a single fetch")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).Issue(context.Background(), "o", "r", 51)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if got.Number != 51 || got.Title != "a real issue" {
		t.Fatalf("Issue = %+v", got)
	}
	if !got.HasLabel("trigger") {
		t.Errorf("labels not carried through: %v", got.Labels)
	}
}

// GitHub's issues endpoint answers a pull request number with a pull request.
// ConvertIssues drops those from a list; a single fetch has nothing to drop it
// from, so the discrimination has to be reported instead -- and reported
// DISTINCTLY, because the caller resolves such a number to the issue the pull
// request closes rather than treating it as a missing issue.
func TestIssueReportsAPullRequestDistinctly(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/108", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&github.Issue{
			Number:           github.Ptr(108),
			Title:            github.Ptr("actually a pull request"),
			PullRequestLinks: &github.PullRequestLinks{URL: github.Ptr("https://example/pr/108")},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := newTestClient(t, srv).Issue(context.Background(), "o", "r", 108)
	if !errors.Is(err, ErrNotAnIssue) {
		t.Fatalf("Issue error = %v, want errors.Is(err, ErrNotAnIssue)", err)
	}
}

// The single fetch must decide trust exactly as the list path does. Tending
// checks a head branch out and runs an agent inside it, so a single-fetch path
// that skipped the trust computation would let an untrusted head be tended --
// a privilege escalation reachable from one webhook delivery.
func TestSingleFetchPullRequestCarriesTheSameTrustAsTheListPath(t *testing.T) {
	for _, c := range []struct {
		name    string
		mutate  func(*github.PullRequest)
		trusted bool
	}{
		{"insider branch in this repository", func(*github.PullRequest) {}, true},
		{"fork head", func(pr *github.PullRequest) {
			pr.Head.Repo.FullName = github.Ptr("attacker/r")
		}, false},
		{"outside contributor", func(pr *github.PullRequest) {
			// Deprecated only for Events API payloads. This is the pull
			// requests endpoint, which is exactly where the deprecation notice
			// says to read it, and it is the field trust is decided from.
			pr.AuthorAssociation = github.Ptr("NONE") //nolint:staticcheck // SA1019: valid on the pulls endpoint
		}, false},
		{"head ref git would read as an option", func(pr *github.PullRequest) {
			pr.Head.Ref = github.Ptr("--upload-pack=evil")
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			one := insiderPR()
			c.mutate(one)

			mux := http.NewServeMux()
			mux.HandleFunc("/repos/o/r/pulls/108", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(one)
			})
			mux.HandleFunc("/repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]*github.PullRequest{one})
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			gh := newTestClient(t, srv)

			single, err := gh.PullRequest(context.Background(), "o", "r", 108)
			if err != nil {
				t.Fatalf("PullRequest: %v", err)
			}
			listed, err := gh.ListOpenPullRequests(context.Background(), "o", "r")
			if err != nil {
				t.Fatalf("ListOpenPullRequests: %v", err)
			}
			if len(listed) != 1 {
				t.Fatalf("listed %d pull requests, want 1", len(listed))
			}
			if single != listed[0] {
				t.Fatalf("single fetch = %+v\nlist path   = %+v\nthe two must agree field for field", single, listed[0])
			}
			if single.Trusted != c.trusted {
				t.Fatalf("Trusted = %v, want %v", single.Trusted, c.trusted)
			}
		})
	}
}
