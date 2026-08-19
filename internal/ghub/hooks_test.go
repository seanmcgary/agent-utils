package ghub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-github/v77/github"
)

// newTestClient returns a GitHubClient whose underlying github.Client points
// at the given httptest server, so RepositoriesService calls hit the fake
// endpoints instead of the real GitHub API.
func newTestClient(t *testing.T, srv *httptest.Server) *GitHubClient {
	t.Helper()
	c := github.NewClient(nil)
	// BaseURL must end with a trailing slash, or go-github's relative-URL
	// resolution drops the last path segment of the fake server's address.
	base, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("parse base url: %v", err)
	}
	c.BaseURL = base
	return &GitHubClient{c: c}
}

func TestListHooksFollowsSecondPage(t *testing.T) {
	mux := http.NewServeMux()
	calls := 0
	mux.HandleFunc("/repos/o/r/hooks", func(w http.ResponseWriter, r *http.Request) {
		calls++
		page := r.URL.Query().Get("page")
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case "", "1":
			// Link header advertises a second page, which is what
			// go-github's pagination follows to set resp.NextPage.
			w.Header().Set("Link", fmt.Sprintf(`<%s?page=2>; rel="next"`, "/repos/o/r/hooks"))
			_ = json.NewEncoder(w).Encode([]*github.Hook{
				{ID: github.Ptr(int64(1)), Config: &github.HookConfig{URL: github.Ptr("https://one.example/hook")}, Events: []string{"issues"}, Active: github.Ptr(true)},
			})
		case "2":
			_ = json.NewEncoder(w).Encode([]*github.Hook{
				{ID: github.Ptr(int64(2)), Config: &github.HookConfig{URL: github.Ptr("https://two.example/hook")}, Events: []string{"pull_request"}, Active: github.Ptr(false)},
			})
		default:
			t.Fatalf("unexpected page %q", page)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := newTestClient(t, srv)
	hooks, err := g.ListHooks(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("ListHooks: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2; ListHooks must follow the second page", calls)
	}
	if len(hooks) != 2 {
		t.Fatalf("len(hooks) = %d, want 2", len(hooks))
	}
	if hooks[0].ID != 1 || hooks[0].URL != "https://one.example/hook" {
		t.Errorf("hooks[0] = %+v", hooks[0])
	}
	if hooks[1].ID != 2 || hooks[1].URL != "https://two.example/hook" {
		t.Errorf("hooks[1] = %+v", hooks[1])
	}
	if !hooks[0].Active || hooks[1].Active {
		t.Errorf("Active not carried through: %+v %+v", hooks[0], hooks[1])
	}
	if len(hooks[0].Events) != 1 || hooks[0].Events[0] != "issues" {
		t.Errorf("Events not carried through: %+v", hooks[0])
	}
}

func TestListHooks404NamesScope(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/hooks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(&github.ErrorResponse{Message: "Not Found"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := newTestClient(t, srv)
	_, err := g.ListHooks(context.Background(), "o", "r")
	if err == nil {
		t.Fatal("ListHooks: want error on 404, got nil")
	}
	if !strings.Contains(err.Error(), "admin:repo_hook") {
		t.Errorf("error = %q, want it to name admin:repo_hook (a 404 here means the token lacks that scope, not that the repo is missing)", err.Error())
	}
}

func TestCreateHookSendsConfig(t *testing.T) {
	mux := http.NewServeMux()
	var gotBody map[string]any
	mux.HandleFunc("/repos/o/r/hooks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&github.Hook{ID: github.Ptr(int64(42))})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := newTestClient(t, srv)
	id, err := g.CreateHook(context.Background(), "o", "r", HookSpec{
		URL:    "https://example.com/hook",
		Secret: "shh",
		Events: []string{"issues", "pull_request"},
	})
	if err != nil {
		t.Fatalf("CreateHook: %v", err)
	}
	if id != 42 {
		t.Errorf("id = %d, want 42", id)
	}

	cfg, ok := gotBody["config"].(map[string]any)
	if !ok {
		t.Fatalf("config missing or wrong type in request body: %+v", gotBody)
	}
	if cfg["content_type"] != "json" {
		t.Errorf("content_type = %v, want json", cfg["content_type"])
	}
	if cfg["insecure_ssl"] != "0" {
		t.Errorf("insecure_ssl = %v, want \"0\"", cfg["insecure_ssl"])
	}
	if cfg["secret"] != "shh" {
		t.Errorf("secret = %v, want shh", cfg["secret"])
	}
	if cfg["url"] != "https://example.com/hook" {
		t.Errorf("url = %v, want https://example.com/hook", cfg["url"])
	}
}

func TestEditHook(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/hooks/7", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Fatalf("method = %s, want PATCH", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&github.Hook{ID: github.Ptr(int64(7))})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	g := newTestClient(t, srv)
	err := g.EditHook(context.Background(), "o", "r", 7, HookSpec{
		URL:    "https://example.com/hook",
		Secret: "shh",
		Events: []string{"issues"},
	})
	if err != nil {
		t.Fatalf("EditHook: %v", err)
	}
}

func TestIsHookEvent(t *testing.T) {
	for _, e := range HookEvents {
		if !IsHookEvent(e) {
			t.Errorf("IsHookEvent(%q) = false, want true", e)
		}
	}
	if IsHookEvent("push") {
		t.Error(`IsHookEvent("push") = true, want false`)
	}
}
