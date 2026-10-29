package ghub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-github/v77/github"
)

func TestFormatCoAuthor(t *testing.T) {
	cases := []struct {
		login       string
		id          int64
		name, email string
		want        string
	}{
		{"seanmcgary", 12, "Sean McGary", "sean@seanmcgary.com", "Sean McGary <sean@seanmcgary.com>"},
		{"seanmcgary", 12, "", "", "seanmcgary <12+seanmcgary@users.noreply.github.com>"},
		{"seanmcgary", 12, "Sean McGary", "", "Sean McGary <12+seanmcgary@users.noreply.github.com>"},
		{"sob", 7, "Seán O'Brien", "", "Seán O'Brien <7+sob@users.noreply.github.com>"},
		// Control characters are dropped; the name still holds "<", so it
		// falls back to the login.
		{"x", 1, "Evil\nCo-authored-by: Mallory <m@x>", "", "x <1+x@users.noreply.github.com>"},
		{"x", 1, "A <b>", "a@b.c", "x <a@b.c>"},
		{"x", 1, "  Spaced  ", "not-an-email", "Spaced <1+x@users.noreply.github.com>"},
		{"x", 1, "N", "a b@c.d", "N <1+x@users.noreply.github.com>"},
	}
	for _, c := range cases {
		if got := FormatCoAuthor(c.login, c.id, c.name, c.email); got != c.want {
			t.Errorf("FormatCoAuthor(%q, %q, %q) = %q, want %q", c.login, c.name, c.email, got, c.want)
		}
	}
}

func fakeAPI(t *testing.T, issueUser, profile string) *GitHubClient {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/acme/widgets/issues/5", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"number":5,"user":%s}`, issueUser)
	})
	mux.HandleFunc("GET /users/{login}", func(w http.ResponseWriter, r *http.Request) {
		if profile == "" {
			http.Error(w, "boom", 500)
			return
		}
		fmt.Fprint(w, profile)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := github.NewClient(nil)
	c.BaseURL, _ = url.Parse(srv.URL + "/")
	return &GitHubClient{c: c}
}

func TestCoAuthor(t *testing.T) {
	ctx := context.Background()
	g := fakeAPI(t, `{"login":"seanmcgary","id":12,"type":"User"}`,
		`{"login":"seanmcgary","id":12,"name":"Sean McGary","email":"sean@seanmcgary.com"}`)
	if got, err := g.CoAuthor(ctx, "acme", "widgets", 5); err != nil || got != "Sean McGary <sean@seanmcgary.com>" {
		t.Errorf("CoAuthor = %q, %v", got, err)
	}
	g = fakeAPI(t, `{"login":"seanmcgary","id":12,"type":"User"}`, "")
	if got, err := g.CoAuthor(ctx, "acme", "widgets", 5); err != nil || got != "seanmcgary <12+seanmcgary@users.noreply.github.com>" {
		t.Errorf("profile failure: CoAuthor = %q, %v; want login+noreply", got, err)
	}
	g = fakeAPI(t, `{"login":"acme-agent[bot]","id":9001,"type":"Bot"}`, `{}`)
	if got, err := g.CoAuthor(ctx, "acme", "widgets", 5); err != nil || got != "" {
		t.Errorf("bot author: CoAuthor = %q, %v; want empty", got, err)
	}
}

func TestAuthenticatedLoginUsesSelfOverride(t *testing.T) {
	g := &GitHubClient{c: github.NewClient(nil), self: func(context.Context) (string, error) { return "acme-agent[bot]", nil }}
	got, err := g.AuthenticatedLogin(context.Background())
	if err != nil || got != "acme-agent[bot]" {
		t.Errorf("AuthenticatedLogin = %q, %v", got, err)
	}
}
