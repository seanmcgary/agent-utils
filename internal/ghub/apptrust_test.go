package ghub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/google/go-github/v77/github"
	"github.com/seanmcgary/agent-utils/internal/ghapp"
)

// appBot is the bot identity the app-mode tests configure.
var appBot = ghapp.Identity{Login: "exchange-agent-app[bot]", ID: 5586999}

// authoredPR is insiderPR with the author and association replaced.
func authoredPR(number int, login string, id int64, assoc string) *github.PullRequest {
	pr := insiderPR()
	pr.Number = github.Ptr(number)
	pr.User = &github.User{Login: github.Ptr(login), ID: github.Ptr(id)}
	pr.AuthorAssociation = github.Ptr(assoc) //nolint:staticcheck // SA1019: valid on the pulls endpoint
	return pr
}

// permServer answers the pulls endpoints with prs and the collaborator
// permission endpoint from perms: a login missing from perms gets a 404, and
// the value "error" gets a 500. lookups counts permission requests.
func permServer(t *testing.T, prs []*github.PullRequest, perms map[string]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var lookups atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(prs)
	})
	mux.HandleFunc("GET /repos/o/r/pulls/{n}", func(w http.ResponseWriter, r *http.Request) {
		for _, pr := range prs {
			if r.PathValue("n") == strconv.Itoa(pr.GetNumber()) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(pr)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("GET /repos/o/r/collaborators/{login}/permission", func(w http.ResponseWriter, r *http.Request) {
		lookups.Add(1)
		p, ok := perms[r.PathValue("login")]
		switch {
		case !ok:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		case p == "error":
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(&github.RepositoryPermissionLevel{Permission: github.Ptr(p)})
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &lookups
}

// appClient is newTestClient in app mode: it knows the app's bot.
func appClient(t *testing.T, srv *httptest.Server) *GitHubClient {
	gh := newTestClient(t, srv)
	gh.bot = func(context.Context, string) (ghapp.Identity, error) { return appBot, nil }
	return gh
}

// With an installation token GitHub reports the app's own bot as NONE and a
// private org member as CONTRIBUTOR, so association alone trusts neither. The
// single fetch and the list path must still agree on every case.
func TestAppModeTrust(t *testing.T) {
	perms := map[string]string{
		"writer":     "write",
		"maintainer": "maintain",
		"admin":      "admin",
		"reader":     "read",
		"flaky":      "error",
		// A fork author with admin rights: the fork check must still win.
		"forker": "admin",
		// Never consulted for the app's bot; present to prove it.
		appBot.Login: "read",
	}
	for _, c := range []struct {
		name    string
		pr      *github.PullRequest
		trusted bool
	}{
		{"the app's bot by login", authoredPR(1, appBot.Login, 1, "NONE"), true},
		{"the app's bot by user id after a rename", authoredPR(2, "renamed-app[bot]", appBot.ID, "NONE"), true},
		{"another bot", authoredPR(3, "dependabot[bot]", 49699333, "NONE"), false},
		{"CONTRIBUTOR with write", authoredPR(4, "writer", 10, "CONTRIBUTOR"), true},
		{"CONTRIBUTOR with maintain", authoredPR(5, "maintainer", 11, "CONTRIBUTOR"), true},
		{"NONE with admin", authoredPR(6, "admin", 12, "NONE"), true},
		{"CONTRIBUTOR with read", authoredPR(7, "reader", 13, "CONTRIBUTOR"), false},
		{"CONTRIBUTOR the endpoint 404s for", authoredPR(8, "stranger", 14, "CONTRIBUTOR"), false},
		{"CONTRIBUTOR whose lookup errors", authoredPR(9, "flaky", 15, "CONTRIBUTOR"), false},
		{"an association the fallback does not cover", authoredPR(10, "writer", 10, "FIRST_TIME_CONTRIBUTOR"), false},
		{"MEMBER needs no lookup", authoredPR(11, "member", 16, "MEMBER"), true},
		{"fork whose author has admin", func() *github.PullRequest {
			pr := authoredPR(12, "forker", 17, "CONTRIBUTOR")
			pr.Head.Repo.FullName = github.Ptr("forker/r")
			return pr
		}(), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := permServer(t, []*github.PullRequest{c.pr}, perms)
			gh := appClient(t, srv)

			single, err := gh.PullRequest(context.Background(), "o", "r", c.pr.GetNumber())
			if err != nil {
				t.Fatalf("PullRequest: %v", err)
			}
			listed, err := appClient(t, srv).ListOpenPullRequests(context.Background(), "o", "r")
			if err != nil {
				t.Fatalf("ListOpenPullRequests: %v", err)
			}
			if len(listed) != 1 || single != listed[0] {
				t.Fatalf("single fetch = %+v\nlist path   = %+v\nthe two must agree", single, listed)
			}
			if single.Trusted != c.trusted {
				t.Fatalf("Trusted = %v, want %v", single.Trusted, c.trusted)
			}
		})
	}
}

// The app's bot and a fork are decided without a permission lookup.
func TestAppModeTrustSkipsLookupsItDoesNotNeed(t *testing.T) {
	fork := authoredPR(2, "forker", 17, "CONTRIBUTOR")
	fork.Head.Repo.FullName = github.Ptr("forker/r")
	srv, lookups := permServer(t, []*github.PullRequest{
		authoredPR(1, appBot.Login, appBot.ID, "NONE"),
		fork,
	}, map[string]string{"forker": "admin"})

	if _, err := appClient(t, srv).ListOpenPullRequests(context.Background(), "o", "r"); err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}
	if n := lookups.Load(); n != 0 {
		t.Errorf("made %d permission lookups, want 0", n)
	}
}

// A listing can hold many pull requests by one author; one client looks the
// author up once, and the single-fetch path reuses that answer.
func TestAppModePermissionLookupIsCachedPerLogin(t *testing.T) {
	srv, lookups := permServer(t, []*github.PullRequest{
		authoredPR(1, "writer", 10, "CONTRIBUTOR"),
		authoredPR(2, "writer", 10, "CONTRIBUTOR"),
		authoredPR(3, "Writer", 10, "CONTRIBUTOR"),
		authoredPR(4, "reader", 13, "CONTRIBUTOR"),
		authoredPR(5, "reader", 13, "CONTRIBUTOR"),
	}, map[string]string{"writer": "write", "Writer": "write", "reader": "read"})
	gh := appClient(t, srv)

	listed, err := gh.ListOpenPullRequests(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}
	for _, pr := range listed {
		if want := pr.Number <= 3; pr.Trusted != want {
			t.Errorf("#%d Trusted = %v, want %v", pr.Number, pr.Trusted, want)
		}
	}
	if _, err := gh.PullRequest(context.Background(), "o", "r", 2); err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
	if n := lookups.Load(); n != 2 {
		t.Errorf("made %d permission lookups, want 2 (one per login)", n)
	}
}

// A bot identity that cannot be resolved does not stop a human with write
// access from being trusted, and leaves the bot itself untrusted.
func TestAppModeTrustWhenTheBotCannotBeResolved(t *testing.T) {
	srv, _ := permServer(t, []*github.PullRequest{
		authoredPR(1, appBot.Login, appBot.ID, "NONE"),
		authoredPR(2, "writer", 10, "CONTRIBUTOR"),
	}, map[string]string{"writer": "write"})
	gh := newTestClient(t, srv)
	gh.bot = func(context.Context, string) (ghapp.Identity, error) {
		return ghapp.Identity{}, errors.New("installation token mint failed")
	}

	listed, err := gh.ListOpenPullRequests(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}
	if listed[0].Trusted {
		t.Error("the bot was trusted although its identity could not be resolved")
	}
	if !listed[1].Trusted {
		t.Error("a CONTRIBUTOR with write access was not trusted")
	}
}

// Token mode decides from author_association alone, as it always has, and
// never calls the permission endpoint.
func TestTokenModeTrustIsUnchanged(t *testing.T) {
	prs := []*github.PullRequest{
		authoredPR(1, "owner", 1, "OWNER"),
		authoredPR(2, "member", 2, "MEMBER"),
		authoredPR(3, "collab", 3, "COLLABORATOR"),
		authoredPR(4, "writer", 4, "CONTRIBUTOR"),
		authoredPR(5, appBot.Login, appBot.ID, "NONE"),
		authoredPR(6, "dependabot[bot]", 49699333, "NONE"),
	}
	srv, lookups := permServer(t, prs, map[string]string{"writer": "write", appBot.Login: "admin"})

	listed, err := newTestClient(t, srv).ListOpenPullRequests(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("ListOpenPullRequests: %v", err)
	}
	for _, pr := range listed {
		if want := pr.Number <= 3; pr.Trusted != want {
			t.Errorf("#%d (%s) Trusted = %v, want %v", pr.Number, pr.AuthorAssociation, pr.Trusted, want)
		}
	}
	if n := lookups.Load(); n != 0 {
		t.Errorf("token mode made %d permission lookups, want 0", n)
	}
}
