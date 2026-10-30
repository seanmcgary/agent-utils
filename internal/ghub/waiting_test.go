package ghub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-github/v77/github"
)

// The reverse edge crosses repositories: a closed issue in one repository
// lists a dependent in another, and the dependent's repository must survive
// the conversion -- it is the only thing that says where the release is
// written.
func TestBlockingCarriesTheDependentsRepositoryAndReason(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/svc/mirror/issues/697/dependencies/blocking", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]*github.Issue{{
			Number:      github.Ptr(945),
			State:       github.Ptr("open"),
			StateReason: github.Ptr("reopened"),
			Repository:  &github.Repository{FullName: github.Ptr("o/r")},
			Labels:      []*github.Label{{Name: github.Ptr("status:awaiting-mirror")}},
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).Blocking(context.Background(), "svc", "mirror", 697)
	if err != nil {
		t.Fatalf("Blocking: %v", err)
	}
	if len(got) != 1 || got[0].Number != 945 || got[0].Repo != "o/r" || got[0].StateReason != "reopened" {
		t.Fatalf("Blocking = %+v", got)
	}
	if !got[0].HasLabel("status:awaiting-mirror") {
		t.Errorf("labels not carried: %v", got[0].Labels)
	}
}

func graphQLServer(t *testing.T, path, answer string, seen *map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		b, _ := io.ReadAll(r.Body)
		if seen != nil {
			_ = json.Unmarshal(b, seen)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	})
	return httptest.NewServer(mux)
}

func TestClosingPullRequestsReadsMergeCommits(t *testing.T) {
	var req map[string]any
	srv := graphQLServer(t, "/graphql", `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{
		"pageInfo":{"hasNextPage":false},
		"nodes":[
		  {"number":698,"merged":true,"mergeCommit":{"oid":"abc"},"repository":{"nameWithOwner":"svc/mirror"}},
		  {"number":699,"merged":false,"mergeCommit":null,"repository":{"nameWithOwner":"svc/mirror"}}
		]}}}}}`, &req)
	defer srv.Close()

	got, err := newTestClient(t, srv).ClosingPullRequests(context.Background(), "svc", "mirror", 697)
	if err != nil {
		t.Fatalf("ClosingPullRequests: %v", err)
	}
	if len(got) != 2 || !got[0].Merged || got[0].MergeCommit != "abc" || got[0].Repo != "svc/mirror" {
		t.Fatalf("got %+v", got)
	}
	if got[1].Merged || got[1].MergeCommit != "" {
		t.Errorf("unmerged pull request = %+v", got[1])
	}
	vars, _ := req["variables"].(map[string]any)
	if vars["owner"] != "svc" || vars["name"] != "mirror" || vars["number"] != float64(697) {
		t.Errorf("variables = %v", vars)
	}
	if q, _ := req["query"].(string); !strings.Contains(q, "includeClosedPrs: true") {
		t.Errorf("query does not include closed pull requests: %s", q)
	}
}

// GraphQL reports failure as 200 with an errors array. Reading that as "no
// closing pull requests" would release without waiting for any check.
func TestClosingPullRequestsRefusesAGraphQLError(t *testing.T) {
	srv := graphQLServer(t, "/graphql", `{"data":null,"errors":[{"message":"rate limited"}]}`, nil)
	defer srv.Close()
	if _, err := newTestClient(t, srv).ClosingPullRequests(context.Background(), "svc", "mirror", 697); err == nil ||
		!strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v, want the GraphQL error", err)
	}
}

// A truncated list could omit the merged pull request.
func TestClosingPullRequestsRefusesATruncatedList(t *testing.T) {
	srv := graphQLServer(t, "/graphql", `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{
		"pageInfo":{"hasNextPage":true},"nodes":[]}}}}}`, nil)
	defer srv.Close()
	if _, err := newTestClient(t, srv).ClosingPullRequests(context.Background(), "svc", "mirror", 697); err == nil {
		t.Fatal("want an error for a truncated list")
	}
}

func TestGraphQLURLForEnterpriseServer(t *testing.T) {
	ghe, _ := url.Parse("https://ghe.example.com/api/v3/")
	if got := graphQLURL(ghe); got != "https://ghe.example.com/api/graphql" {
		t.Errorf("GHES graphql = %q", got)
	}
	dotcom, _ := url.Parse("https://api.github.com/")
	if got := graphQLURL(dotcom); got != "graphql" {
		t.Errorf("github.com graphql = %q", got)
	}
}

func TestWorkflowRunsSendsTheQuery(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/svc/mirror/actions/workflows/mirror.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("branch") != "main" || q.Get("status") != "success" || q.Get("head_sha") != "abc" {
			t.Errorf("query = %v", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&github.WorkflowRuns{
			TotalCount: github.Ptr(1),
			WorkflowRuns: []*github.WorkflowRun{{
				ID: github.Ptr(int64(7)), HeadSHA: github.Ptr("abc"), Status: github.Ptr("completed"),
				Conclusion: github.Ptr("success"), HTMLURL: github.Ptr("https://x/runs/7"),
			}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).WorkflowRuns(context.Background(), "svc", "mirror",
		RunQuery{Workflow: "mirror.yml", Branch: "main", Status: "success", HeadSHA: "abc"})
	if err != nil {
		t.Fatalf("WorkflowRuns: %v", err)
	}
	if len(got) != 1 || got[0].ID != 7 || got[0].Conclusion != "success" || got[0].HTMLURL != "https://x/runs/7" {
		t.Fatalf("got %+v", got)
	}
}
