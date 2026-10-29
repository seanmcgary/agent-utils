package ghapp

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
)

type repoKey struct{}

// WithRepo names the repository whose installation authenticates a request
// whose URL names none, such as GET /users/{login}. Every other request the
// daemon makes is under /repos/{owner}/{name}/ and needs no hint.
func WithRepo(ctx context.Context, repo string) context.Context {
	return context.WithValue(ctx, repoKey{}, repo)
}

// repoInPath matches the repository in an API path. The optional prefix is
// GitHub Enterprise Server's.
var repoInPath = regexp.MustCompile(`^(?:/api/v3)?/repos/([^/]+)/([^/]+)(?:/|$)`)

type transport struct {
	app  *App
	base http.RoundTripper
}

// RoundTrip authenticates req with an installation token for the repository
// it addresses. A request that addresses none and carries no WithRepo hint is
// refused rather than sent unauthenticated: an anonymous call would succeed
// against public data and fail confusingly, at a 60-an-hour rate limit, on
// anything else.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	repo := ""
	if m := repoInPath.FindStringSubmatch(req.URL.Path); m != nil {
		repo = m[1] + "/" + m[2]
	} else if r, ok := req.Context().Value(repoKey{}).(string); ok {
		repo = r
	}
	if repo == "" {
		return nil, fmt.Errorf("ghapp: %s %s names no repository; wrap the context with ghapp.WithRepo", req.Method, req.URL.Path)
	}
	tok, err := t.app.Token(req.Context(), repo)
	if err != nil {
		return nil, err
	}
	r2 := req.Clone(req.Context())
	r2.Header.Set("Authorization", "token "+tok)
	return t.base.RoundTrip(r2)
}

// Client returns an HTTP client that authenticates every request as the app's
// installation on the repository the request addresses.
func (a *App) Client() *http.Client {
	base := http.DefaultTransport
	if a.HTTP != nil && a.HTTP.Transport != nil {
		base = a.HTTP.Transport
	}
	return &http.Client{Transport: &transport{app: a, base: base}}
}
