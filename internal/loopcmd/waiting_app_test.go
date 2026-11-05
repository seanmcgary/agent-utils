package loopcmd

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/ghapp"
	"github.com/seanmcgary/agent-utils/internal/ghub"
)

// appGitHub fakes the parts of GitHub the waiting pass touches, as seen by a
// GitHub App installed on both o/r and svc/mirror. Like the real API, it
// hides a dependency whose repository the token cannot read: blocked_by
// leaves it out, and issueDependenciesSummary still counts it.
type appGitHub struct {
	mu sync.Mutex
	// visible maps each minted token to the repositories it can read.
	visible map[string]map[string]bool
	mints   int
	// labels is o/r#945's label set.
	labels map[string]bool
}

func newAppGitHub(t *testing.T) (*appGitHub, *httptest.Server) {
	t.Helper()
	g := &appGitHub{
		visible: map[string]map[string]bool{},
		labels:  map[string]bool{"cve-bump": true, waitingLabel: true},
	}
	installed := map[string]bool{"o/r": true, "svc/mirror": true}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{name}/installation", func(w http.ResponseWriter, r *http.Request) {
		if !installed[r.PathValue("owner")+"/"+r.PathValue("name")] {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"id":42}`)
	})
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Repositories []string `json:"repositories"`
		}
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		g.mints++
		tok := fmt.Sprintf("ghs_%d", g.mints)
		seen := map[string]bool{}
		if len(body.Repositories) == 0 {
			for repo := range installed {
				seen[repo] = true
			}
		}
		for _, name := range body.Repositories {
			for repo := range installed {
				if strings.HasSuffix(repo, "/"+name) {
					seen[repo] = true
				}
			}
		}
		g.visible[tok] = seen
		fmt.Fprintf(w, `{"token":%q,"expires_at":"2099-01-01T00:00:00Z"}`, tok)
	})

	// sees reports whether the request's token can read repo; a request that
	// cannot gets the 404 GitHub gives.
	sees := func(w http.ResponseWriter, r *http.Request, repo string) bool {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "token ")
		g.mu.Lock()
		ok := g.visible[tok][repo]
		g.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
		return ok
	}

	mux.HandleFunc("GET /repos/o/r/issues/945", func(w http.ResponseWriter, r *http.Request) {
		if !sees(w, r, "o/r") {
			return
		}
		g.mu.Lock()
		var ls []map[string]string
		for l := range g.labels {
			ls = append(ls, map[string]string{"name": l})
		}
		g.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 945, "state": "open", "labels": ls,
			"repository_url": "https://api.github.com/repos/o/r",
		})
	})
	mux.HandleFunc("GET /repos/o/r/issues/945/dependencies/blocked_by", func(w http.ResponseWriter, r *http.Request) {
		if !sees(w, r, "o/r") {
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "token ")
		g.mu.Lock()
		cross := g.visible[tok]["svc/mirror"]
		g.mu.Unlock()
		if !cross {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprint(w, `[{"number":697,"state":"closed","state_reason":"completed",
			"repository":{"full_name":"svc/mirror"}}]`)
	})
	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		repo := fmt.Sprintf("%v/%v", q.Variables["owner"], q.Variables["name"])
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "token ")
		g.mu.Lock()
		ok := g.visible[tok][repo]
		g.mu.Unlock()
		if !ok {
			fmt.Fprintf(w, `{"data":{"repository":null},"errors":[{"message":"Could not resolve to a Repository with the name '%s'."}]}`, repo)
			return
		}
		switch {
		case strings.Contains(q.Query, "issueDependenciesSummary") && repo == "o/r":
			fmt.Fprint(w, `{"data":{"repository":{"issue":{"issueDependenciesSummary":{"totalBlockedBy":1}}}}}`)
		case strings.Contains(q.Query, "closedByPullRequestsReferences") && repo == "svc/mirror":
			fmt.Fprintf(w, `{"data":{"repository":{"issue":{"closedByPullRequestsReferences":{
				"pageInfo":{"hasNextPage":false},
				"nodes":[{"number":698,"merged":true,"mergeCommit":{"oid":%q},"repository":{"nameWithOwner":"svc/mirror"}}]}}}}}`, mergeSHA)
		default:
			http.Error(w, "unexpected query against "+repo, http.StatusBadRequest)
		}
	})
	mux.HandleFunc("GET /repos/svc/mirror", func(w http.ResponseWriter, r *http.Request) {
		if sees(w, r, "svc/mirror") {
			fmt.Fprint(w, `{"full_name":"svc/mirror","default_branch":"main"}`)
		}
	})
	mux.HandleFunc("GET /repos/svc/mirror/actions/workflows/mirror.yml/runs", func(w http.ResponseWriter, r *http.Request) {
		if sees(w, r, "svc/mirror") {
			fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"id":1,"head_sha":%q,"head_branch":"main",
				"event":"push","status":"completed","conclusion":"success"}]}`, mergeSHA)
		}
	})
	mux.HandleFunc("POST /repos/o/r/issues/945/labels", func(w http.ResponseWriter, r *http.Request) {
		if !sees(w, r, "o/r") {
			return
		}
		var add []string
		_ = json.NewDecoder(r.Body).Decode(&add)
		g.mu.Lock()
		for _, l := range add {
			g.labels[l] = true
		}
		g.mu.Unlock()
		fmt.Fprint(w, `[]`)
	})
	mux.HandleFunc("DELETE /repos/o/r/issues/945/labels/{label}", func(w http.ResponseWriter, r *http.Request) {
		if !sees(w, r, "o/r") {
			return
		}
		g.mu.Lock()
		delete(g.labels, r.PathValue("label"))
		g.mu.Unlock()
		fmt.Fprint(w, `[]`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusNotImplemented)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return g, srv
}

func newWaitApp(t *testing.T, base string, repoScoped bool) *ghapp.App {
	t.Helper()
	dir := t.TempDir()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "app.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	if err := os.WriteFile(keyPath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := ghapp.New(7, keyPath, filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	app.BaseURL = base
	app.RepoScoped = repoScoped
	return app
}

// exchange-infra#1014 end to end, through the real ghapp transport and the
// real GitHub client: a waiting issue in o/r is blocked by svc/mirror#697,
// which closed as completed with a green mirror.yml run on its merge commit.
// mirror.yml lives in svc/mirror, not o/r. With an installation-wide token
// the pass sees the blocker and releases the issue.
func TestWaitReleasesACrossRepositoryBlockerThroughTheApp(t *testing.T) {
	g, srv := newAppGitHub(t)
	cfg, deps, _ := waitFixture(t, "mirror.yml")
	deps.Wait = ghub.NewApp(newWaitApp(t, srv.URL, false))

	sum, err := WaitSweep(context.Background(), cfg, deps, []int{945})
	if err != nil {
		t.Fatalf("WaitSweep: %v", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if sum.Released != 1 || !g.labels[cfg.Labels.Trigger] || g.labels[waitingLabel] {
		t.Fatalf("released = %d, labels = %v; want the trigger added and %q removed",
			sum.Released, g.labels, waitingLabel)
	}
	if g.mints != 1 {
		t.Errorf("mints = %d; o/r and svc/mirror share an installation and should share one token", g.mints)
	}
}

// The same issue with github_app.token_scope: repo. The o/r token cannot see
// svc/mirror, so blocked_by is empty while GitHub counts one blocker. The
// issue stays waiting, and the log names the hidden blocker.
func TestWaitHoldsACrossRepositoryBlockerWithRepoScopedTokens(t *testing.T) {
	logs := captureTickLogs(t)
	g, srv := newAppGitHub(t)
	cfg, deps, _ := waitFixture(t, "mirror.yml")
	deps.Wait = ghub.NewApp(newWaitApp(t, srv.URL, true))

	sum, err := WaitSweep(context.Background(), cfg, deps, []int{945})
	if err != nil {
		t.Fatalf("WaitSweep: %v", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if sum.Released != 0 || !g.labels[waitingLabel] || g.labels[cfg.Labels.Trigger] {
		t.Fatalf("released = %d, labels = %v; want the issue left waiting", sum.Released, g.labels)
	}
	out := logs.String()
	if !strings.Contains(out, "blocker this token cannot see") || strings.Contains(out, "declares no blocker") {
		t.Errorf("logs = %s; want the hidden-blocker WARN and not \"declares no blocker\"", out)
	}
}
