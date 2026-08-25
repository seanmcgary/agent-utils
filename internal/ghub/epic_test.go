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

func TestParentReturnsTheEpic(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/73/parent", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&github.Issue{
			Number: github.Ptr(69),
			Title:  github.Ptr("epic(mobile): the ios app"),
			State:  github.Ptr("open"),
			Labels: []*github.Label{{Name: github.Ptr("epic")}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).Parent(context.Background(), "o", "r", 73)
	if err != nil {
		t.Fatalf("Parent: %v", err)
	}
	if got.Number != 69 {
		t.Errorf("Number = %d, want 69", got.Number)
	}
	if !got.HasLabel("epic") {
		t.Errorf("labels not carried through: %v", got.Labels)
	}
}

// A 404 is the ORDINARY answer for an issue with no parent, which is most
// issues in most repositories. It must be a sentinel a caller can branch on,
// not an error that stops a sweep.
func TestParentReportsNoParentAsSentinel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/12/parent", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := newTestClient(t, srv).Parent(context.Background(), "o", "r", 12)
	if !errors.Is(err, ErrNoParent) {
		t.Fatalf("Parent error = %v, want it to wrap ErrNoParent", err)
	}
}

func TestSubIssuesCarriesStateAndLabels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/69/sub_issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]*github.Issue{
			{Number: github.Ptr(71), State: github.Ptr("closed")},
			{
				Number: github.Ptr(74),
				State:  github.Ptr("open"),
				Labels: []*github.Label{{Name: github.Ptr("status:plan-ready-for-review")}},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).SubIssues(context.Background(), "o", "r", 69)
	if err != nil {
		t.Fatalf("SubIssues: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("SubIssues returned %d, want 2", len(got))
	}
	if got[0].IsOpen() {
		t.Errorf("71 read as open; want closed")
	}
	if !got[1].HasLabel("status:plan-ready-for-review") {
		t.Errorf("74 lost its labels: %v", got[1].Labels)
	}
}

// A sub-issue may live in ANOTHER repository. Its number means nothing in this
// one, and the sweep writes by number, so the repository must survive the read.
func TestSubIssuesCarriesAForeignChildsRepository(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/69/sub_issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
		  {"number":73,"state":"open","labels":[],
		   "repository_url":"https://api.github.com/repos/o/r"},
		  {"number":74,"state":"open","labels":[],
		   "repository_url":"https://api.github.com/repos/other/repo"}
		]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).SubIssues(context.Background(), "o", "r", 69)
	if err != nil {
		t.Fatalf("SubIssues: %v", err)
	}
	if !got[0].InRepo("o", "r") {
		t.Errorf("73 Repo = %q, want it to read as local", got[0].Repo)
	}
	if got[1].InRepo("o", "r") {
		t.Errorf("74 Repo = %q read as local; it lives in other/repo", got[1].Repo)
	}
}

// A server that always reports a next page must not spin a webhook handler
// forever.
func TestPagedIssuesRefusesAPaginationThatDoesNotAdvance(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/69/sub_issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// rel="next" pointing at page 1, forever.
		w.Header().Set("Link", `<`+srv.URL+`/repos/o/r/issues/69/sub_issues?page=1>; rel="next"`)
		_, _ = w.Write([]byte(`[{"number":71,"state":"closed","labels":[]}]`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	if _, err := newTestClient(t, srv).SubIssues(context.Background(), "o", "r", 69); err == nil {
		t.Fatal("want an error when pagination does not advance, got nil")
	}
}

// The state of a blocker is what decides a promotion, and a blocker may live in
// another repository. Its state is in THIS response, so the sweep never needs a
// second call -- pin that the field survives the conversion.
func TestBlockedByCarriesStateOfAForeignBlocker(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/74/dependencies/blocked_by",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
			  {"number":73,"state":"closed","labels":[],
			   "repository":{"full_name":"o/r"}},
			  {"number":9,"state":"open","labels":[],
			   "repository":{"full_name":"other/repo"}}
			]`))
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).BlockedBy(context.Background(), "o", "r", 74)
	if err != nil {
		t.Fatalf("BlockedBy: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("BlockedBy returned %d, want 2", len(got))
	}
	if got[0].IsOpen() {
		t.Errorf("73 read as open, want closed")
	}
	if !got[1].IsOpen() {
		t.Errorf("the foreign blocker read as closed, want open")
	}
}

func TestBlockedByReturnsEmptyForNoDependencies(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/78/dependencies/blocked_by",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).BlockedBy(context.Background(), "o", "r", 78)
	if err != nil {
		t.Fatalf("BlockedBy: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("BlockedBy returned %d, want 0", len(got))
	}
}

// Both list endpoints are paginated. A 30-child epic read one page deep would
// silently promote nothing for its tail, which is a wrong answer that looks
// like a correct one.
func TestSubIssuesFollowsPagination(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/69/sub_issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[{"number":72,"state":"open","labels":[]}]`))
			return
		}
		w.Header().Set("Link", `<`+srv.URL+`/repos/o/r/issues/69/sub_issues?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`[{"number":71,"state":"closed","labels":[]}]`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).SubIssues(context.Background(), "o", "r", 69)
	if err != nil {
		t.Fatalf("SubIssues: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("SubIssues returned %d, want both pages (2)", len(got))
	}
}

// EpicSweepAll gates every epic on InRepo, so the LIST endpoint has to carry
// the repository too -- not only the three epic endpoints. GitHub populates
// repository_url there, and this pins it: if it ever stopped, the cron backstop
// would promote nothing and log nothing, which is the silent failure this
// design exists to avoid.
func TestListOpenIssuesCarriesTheRepository(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
		  {"number":69,"state":"open","labels":[{"name":"epic"}],
		   "repository_url":"https://api.github.com/repos/o/r"}
		]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).ListOpenIssues(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListOpenIssues returned %d, want 1", len(got))
	}
	if !got[0].InRepo("o", "r") {
		t.Errorf("Repo = %q; the epic sweep's cron path gates every epic on this",
			got[0].Repo)
	}
}
