# GitHub App Identity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a deployment act as a GitHub App (daemon API calls, agent `gh`, pushes, commit identity) and optionally add the origin issue's author as a `Co-authored-by:` trailer on every agent commit.

**Architecture:** A new `internal/ghapp` package mints repo-scoped installation tokens from the app's private key and caches them on disk under a file lock; a go-github transport picks the repository from each request's URL. A new `internal/identity` package prepares a per-dispatch directory (a `gh` shim, a `prepare-commit-msg` hook chained to the repository's own hooks) and the `GIT_CONFIG_*` environment that points git at it. The tick resolves the co-author and stores it on the dispatch row; the detached runner reads it and builds the agent's environment from it.

**Tech Stack:** Go 1.25, stdlib crypto (RS256 JWT), `github.com/google/go-github/v77`, `urfave/cli/v3`, SQLite via `modernc.org/sqlite`, git >= 2.31 (`rev-parse --path-format`).

**Spec:** `docs/superpowers/specs/2026-10-29-github-app-identity-design.md`

## Global Constraints

- No new module dependencies. JWT signing uses `crypto/rsa`, `crypto/sha256`, `encoding/base64`.
- With `github_app` unset and `commit.co_author_issue_author` false, the agent's environment, the git commands Go runs, and every GitHub client are exactly what they are today. A regression test pins this.
- Nothing is written to a repository's `.git/config` or `.git/hooks`. All per-dispatch git configuration travels in `GIT_CONFIG_COUNT` / `GIT_CONFIG_KEY_n` / `GIT_CONFIG_VALUE_n`.
- The installation token is never placed in the agent's environment. The agent reaches it only through `agent-utils internal gh-token` and `agent-utils internal git-credential`.
- Files holding a token or key: mode 0600; directories: 0700. Written atomically (temp file with random suffix, `O_EXCL|O_NOFOLLOW`, rename).
- Installation tokens are minted scoped to the single repository (`"repositories": [name]`) and refreshed when fewer than 10 minutes remain.
- Bot identity: name `<slug>[bot]`, email `<bot_user_id>+<slug>[bot]@users.noreply.github.com`.
- Co-author value: `<profile name or login> <public email or <id>+<login>@users.noreply.github.com>`. Skipped for `type: Bot` users and logins ending `[bot]`. A failed lookup never fails a dispatch.
- Never add `Co-Authored-By:` or any AI-attribution trailer to this repository's own commits.
- Go comment density and voice match the surrounding code: comments explain why, at length where the reason is not obvious.
- Run `make lint` and `go test ./...` before each commit. Tests use stdlib `testing` only.

## Review Focus

1. A repository cloned over SSH, on a host with no SSH key registered with GitHub: the agent's `git push` must go over HTTPS as the app. (Task 7, `TestAppEnvRewritesSSHRemote`)
2. A host with a global credential helper (osxkeychain, `store`) holding a personal token: git must return the app's credential, never the host's. (Task 7, `TestAppEnvCredentialHelperOverridesHostHelper`)
3. A repository with its own `pre-commit` (husky, lefthook) that fails: the commit must still be refused after `core.hooksPath` is overridden, and a passing one must still run. (Task 6, `TestHooksChainToRepositoryHooks`)
4. An issue author named `Seán O'Brien`, or a profile name containing a newline or `<`: the trailer must carry the apostrophe and accent intact and never break the hook script or the trailer line. (Task 4 `TestFormatCoAuthor`, Task 6 `TestCoAuthorWithApostrophe`)
5. `AGENT_UTILS_HOME` set to a non-default directory: the agent's environment does not carry it, so the `gh` shim and credential helper must still find the right settings and token cache. (Task 7, `TestHelperCommandCarriesHome`)

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/settings/settings.go` (modify) | `GitHubApp` and `Commit` blocks, three new `Fields` entries |
| `internal/lock/lock.go` (modify) | `AcquireWait`: block until the lock is free or ctx ends |
| `internal/ghapp/key.go` (create) | Load and check the private key file |
| `internal/ghapp/ghapp.go` (create) | `App`: JWT, installation lookup, token mint and cache, slug, bot identity |
| `internal/ghapp/transport.go` (create) | `http.RoundTripper` that authenticates each request for its repository; `WithRepo` |
| `internal/ghub/ghub.go` (modify) | `NewApp`, self-login override, `CoAuthor` |
| `internal/ghub/coauthor.go` (create) | `AuthorReader` interface, `FormatCoAuthor` |
| `internal/store/{store.go,types.go}` (modify) | `dispatches.co_author` column |
| `internal/identity/hooks.go` (create) | Hooks directory: chained stubs and the co-author `prepare-commit-msg` |
| `internal/identity/identity.go` (create) | `Prepare`, `GitEnv`, `gh` shim, `GIT_CONFIG_*` rendering |
| `cmd/agent-utils/internal_auth.go` (create) | `internal gh-token`, `internal git-credential` |
| `internal/worktree/worktree.go` (modify) | `Manager.Env` applied to every git command |
| `internal/runner/{args.go,runner.go}` (modify) | `Invocation.Env` appended to the agent's environment |
| `internal/loopcmd/{open.go,tick.go}` (modify) | Resolve app, build client, set `WT.Env`, resolve co-author, prepare identity |
| `internal/listener/{work.go,poll.go}` (modify) | App-mode clients |
| `cmd/agent-utils/{listener.go,project.go,config.go,config_app.go}` | Token-optional startup, webhook client, `config github-app` |
| `README.md`, `docs/configuration.md` | Docs |

---

### Task 1: Settings blocks

**Files:**
- Modify: `internal/settings/settings.go` (`Settings` struct ~line 82, `Save` header ~line 357, `Fields` ~line 440)
- Test: `internal/settings/settings_test.go`

**Interfaces:**
- Produces:
  ```go
  type GitHubApp struct {
      AppID          int64  `yaml:"app_id,omitempty"`
      PrivateKeyPath string `yaml:"private_key_path,omitempty"`
  }
  type Commit struct {
      CoAuthorIssueAuthor bool `yaml:"co_author_issue_author,omitempty"`
  }
  // on Settings:
  GitHubApp GitHubApp `yaml:"github_app,omitempty"`
  Commit    Commit    `yaml:"commit,omitempty"`
  ```
  Field keys `github_app.app_id`, `github_app.private_key_path`, `commit.co_author_issue_author`.

- [ ] **Step 1: Write the failing tests**

```go
func TestGitHubAppAndCommitRoundTrip(t *testing.T) {
	withHome(t)
	in := &Settings{
		GitHubApp: GitHubApp{AppID: 123456, PrivateKeyPath: "/keys/app.pem"},
		Commit:    Commit{CoAuthorIssueAuthor: true},
	}
	if err := Save(in); err != nil {
		t.Fatal(err)
	}
	out, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if out.GitHubApp != in.GitHubApp || out.Commit != in.Commit {
		t.Errorf("round trip = %+v, want %+v", *out, *in)
	}
}

func TestGitHubAppFields(t *testing.T) {
	var s Settings
	for _, kv := range [][2]string{
		{"github_app.app_id", "42"},
		{"github_app.private_key_path", "/abs/app.pem"},
		{"commit.co_author_issue_author", "true"},
	} {
		f, ok := FieldFor(kv[0])
		if !ok {
			t.Fatalf("no field %s", kv[0])
		}
		if err := f.Set(&s, kv[1]); err != nil {
			t.Fatalf("set %s: %v", kv[0], err)
		}
		if got := f.Get(&s); got != kv[1] {
			t.Errorf("%s = %q, want %q", kv[0], got, kv[1])
		}
	}
	bad := map[string]string{
		"github_app.app_id":             "abc",
		"github_app.private_key_path":   "relative/app.pem",
		"commit.co_author_issue_author": "maybe",
	}
	for k, v := range bad {
		f, _ := FieldFor(k)
		if err := f.Set(&s, v); err == nil {
			t.Errorf("%s=%q: want an error", k, v)
		}
	}
	if f, _ := FieldFor("github_app.app_id"); f.Set(&s, "0") == nil {
		t.Error("app_id 0 must be rejected; unset it instead")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/settings -run 'TestGitHubApp' -v`
Expected: FAIL, `unknown field GitHubApp`.

- [ ] **Step 3: Implement**

Add the two types above the `Settings` struct with doc comments (machine-wide because one app serves every project on a host; the key path is validated by `ghapp`, not here, because `Load` must stay a parse that never touches a second file). Add the two fields to `Settings`. Extend the `Save` header with: `# github_app: the GitHub App this machine acts as, and commit: how agent\n# commits are attributed.\n`.

Append to the `Fields()` slice:

```go
{
	Key: "github_app.app_id",
	Get: func(s *Settings) string {
		if s.GitHubApp.AppID == 0 {
			return ""
		}
		return strconv.FormatInt(s.GitHubApp.AppID, 10)
	},
	Set: func(s *Settings, v string) error {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return fmt.Errorf("github_app.app_id must be a positive number")
		}
		s.GitHubApp.AppID = id
		return nil
	},
	Unset: func(s *Settings) { s.GitHubApp.AppID = 0 },
},
{
	Key: "github_app.private_key_path",
	Get: func(s *Settings) string { return s.GitHubApp.PrivateKeyPath },
	Set: func(s *Settings, v string) error {
		if !filepath.IsAbs(v) {
			return fmt.Errorf("github_app.private_key_path must be absolute: the daemon's working directory is not yours")
		}
		s.GitHubApp.PrivateKeyPath = v
		return nil
	},
	Unset: func(s *Settings) { s.GitHubApp.PrivateKeyPath = "" },
},
{
	Key: "commit.co_author_issue_author",
	Get: func(s *Settings) string { return strconv.FormatBool(s.Commit.CoAuthorIssueAuthor) },
	Set: func(s *Settings, v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("commit.co_author_issue_author must be true or false: %w", err)
		}
		s.Commit.CoAuthorIssueAuthor = b
		return nil
	},
	Unset: func(s *Settings) { s.Commit.CoAuthorIssueAuthor = false },
},
```

`TestLoadWithNoFileReturnsZeroValue` compares with `Settings{}`; both new types are comparable, so it still compiles.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/settings -v`
Expected: PASS (all, including existing render/field-table tests; if a test enumerates `Fields()` keys, add the three keys to its expected list).

- [ ] **Step 5: Commit**

```bash
git add internal/settings
git commit -m "feat(settings): github_app and commit.co_author_issue_author"
```

---

### Task 2: Blocking lock

**Files:**
- Modify: `internal/lock/lock.go`
- Test: `internal/lock/lock_test.go`

**Interfaces:**
- Produces: `func AcquireWait(ctx context.Context, path string) (*Lock, error)`

- [ ] **Step 1: Write the failing test**

```go
func TestAcquireWaitBlocksUntilReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	held, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		l, err := AcquireWait(context.Background(), path)
		if err == nil {
			err = l.Release()
		}
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("AcquireWait returned while the lock was held")
	case <-time.After(150 * time.Millisecond):
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AcquireWait did not return after release")
	}
}

func TestAcquireWaitHonoursContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	held, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := AcquireWait(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/lock -run AcquireWait -v`
Expected: FAIL, `undefined: AcquireWait`.

- [ ] **Step 3: Implement**

```go
// lockPoll is how often AcquireWait retries a held lock. The token cache is
// the one caller, and its holder makes a single HTTPS round trip, so a short
// poll costs nothing and keeps a waiting gh call from noticeably stalling.
const lockPoll = 25 * time.Millisecond

// AcquireWait takes the lock at path, waiting while another process holds it,
// until ctx ends.
//
// It polls Acquire rather than calling flock without LOCK_NB, because a
// blocking flock cannot be interrupted by a context: a caller whose deadline
// passed would sit in the syscall until the holder let go.
func AcquireWait(ctx context.Context, path string) (*Lock, error) {
	for {
		l, err := Acquire(path)
		if !errors.Is(err, ErrHeld) {
			return l, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/lock -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/lock
git commit -m "feat(lock): AcquireWait"
```

---

### Task 3: `internal/ghapp`: key, JWT, tokens, bot identity, transport

**Files:**
- Create: `internal/ghapp/key.go`, `internal/ghapp/ghapp.go`, `internal/ghapp/transport.go`
- Test: `internal/ghapp/ghapp_test.go`, `internal/ghapp/testserver_test.go`

**Interfaces:**
- Consumes: `settings.Settings` (Task 1), `lock.AcquireWait` (Task 2), `home.Dir()`.
- Produces:
  ```go
  type Identity struct{ Login, Name, Email string }
  type App struct {
      ID      int64
      BaseURL string           // default "https://api.github.com"
      HTTP    *http.Client     // default http.DefaultClient
      Now     func() time.Time // default time.Now
      // unexported: key *rsa.PrivateKey, dir string
  }
  const RefreshBefore = 10 * time.Minute
  var ErrNotInstalled = errors.New("the GitHub App is not installed on this repository")
  func New(id int64, keyPath, cacheDir string) (*App, error)
  func FromSettings(s *settings.Settings) (*App, error) // nil, nil when app_id is 0
  func (a *App) JWT() (string, error)
  func (a *App) Token(ctx context.Context, repo string) (string, error)
  func (a *App) Slug(ctx context.Context) (string, error)
  func (a *App) Bot(ctx context.Context, repo string) (Identity, error)
  func (a *App) Client() *http.Client
  func WithRepo(ctx context.Context, repo string) context.Context
  ```

- [ ] **Step 1: Write the test server helper**

`internal/ghapp/testserver_test.go`:

```go
package ghapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeGitHub struct {
	srv       *httptest.Server
	pub       *rsa.PublicKey
	mints     atomic.Int32
	lookups   atomic.Int32
	installed map[string]int64 // "owner/name" -> installation id
	expiry    time.Duration
	mu        sync.Mutex
	scopes    [][]string // repositories requested per mint
}

func writeKey(t *testing.T, dir string) (string, *rsa.PrivateKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "app.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, k
}

// verifyJWT checks the RS256 signature and returns the claims.
func (f *fakeGitHub) verifyJWT(r *http.Request) (map[string]any, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil, false
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(f.pub, crypto.SHA256, sum[:], sig) != nil {
		return nil, false
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	_ = json.Unmarshal(raw, &claims)
	return claims, true
}

func newFakeGitHub(t *testing.T, pub *rsa.PublicKey) *fakeGitHub {
	f := &fakeGitHub{pub: pub, installed: map[string]int64{"acme/widgets": 42}, expiry: time.Hour}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{name}/installation", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.verifyJWT(r); !ok {
			http.Error(w, "bad jwt", 401)
			return
		}
		f.lookups.Add(1)
		id, ok := f.installed[r.PathValue("owner")+"/"+r.PathValue("name")]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, 404)
			return
		}
		fmt.Fprintf(w, `{"id":%d}`, id)
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.verifyJWT(r); !ok {
			http.Error(w, "bad jwt", 401)
			return
		}
		var body struct{ Repositories []string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.scopes = append(f.scopes, body.Repositories)
		f.mu.Unlock()
		n := f.mints.Add(1)
		// A real mint takes a round trip; widen the race window the lock closes.
		time.Sleep(20 * time.Millisecond)
		fmt.Fprintf(w, `{"token":"ghs_test%d","expires_at":%q}`, n,
			time.Now().Add(f.expiry).UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.verifyJWT(r); !ok {
			http.Error(w, "bad jwt", 401)
			return
		}
		fmt.Fprint(w, `{"slug":"acme-agent"}`)
	})
	mux.HandleFunc("GET /users/{login}", func(w http.ResponseWriter, r *http.Request) {
		// GitHub accepts "token" and "Bearer" for an installation token.
		if !strings.Contains(r.Header.Get("Authorization"), "ghs_") {
			http.Error(w, "want installation token", 401)
			return
		}
		if r.PathValue("login") != "acme-agent[bot]" {
			http.Error(w, "not found", 404)
			return
		}
		fmt.Fprint(w, `{"login":"acme-agent[bot]","id":9001,"type":"Bot"}`)
	})
	mux.HandleFunc("GET /repos/{owner}/{name}/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"number":1,"auth":%q}`, r.Header.Get("Authorization"))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newTestApp(t *testing.T) (*App, *fakeGitHub) {
	t.Helper()
	dir := t.TempDir()
	keyPath, k := writeKey(t, dir)
	f := newFakeGitHub(t, &k.PublicKey)
	a, err := New(7, keyPath, filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	a.BaseURL = f.srv.URL
	return a, f
}
```

- [ ] **Step 2: Write the failing tests**

`internal/ghapp/ghapp_test.go`:

```go
package ghapp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/settings"
)

func TestNewRejectsBadKeys(t *testing.T) {
	dir := t.TempDir()
	good, _ := writeKey(t, dir)

	if _, err := New(7, "relative.pem", dir); err == nil {
		t.Error("relative path accepted")
	}
	if err := os.Chmod(good, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(7, good, dir); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Errorf("0644 key: err = %v, want a mode error naming 0600", err)
	}
	junk := filepath.Join(dir, "junk.pem")
	os.WriteFile(junk, []byte("not a key"), 0o600)
	if _, err := New(7, junk, dir); err == nil {
		t.Error("non-PEM key accepted")
	}
	link := filepath.Join(dir, "link.pem")
	os.Chmod(good, 0o600)
	os.Symlink(good, link)
	if _, err := New(7, link, dir); err == nil {
		t.Error("symlinked key accepted")
	}
}

func TestFromSettings(t *testing.T) {
	if a, err := FromSettings(&settings.Settings{}); a != nil || err != nil {
		t.Errorf("unset: got %v, %v; want nil, nil", a, err)
	}
	_, err := FromSettings(&settings.Settings{GitHubApp: settings.GitHubApp{AppID: 7}})
	if err == nil || !strings.Contains(err.Error(), "private_key_path") {
		t.Errorf("missing key path: err = %v", err)
	}
}

func TestJWTClaims(t *testing.T) {
	a, f := newTestApp(t)
	now := time.Unix(1_800_000_000, 0)
	a.Now = func() time.Time { return now }
	tok, err := a.JWT()
	if err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	claims, ok := f.verifyJWT(r)
	if !ok {
		t.Fatal("signature did not verify")
	}
	if claims["iss"] != float64(7) || claims["iat"] != float64(now.Unix()-60) || claims["exp"] != float64(now.Add(9*time.Minute).Unix()) {
		t.Errorf("claims = %v", claims)
	}
}

func TestTokenIsCachedAndRepoScoped(t *testing.T) {
	a, f := newTestApp(t)
	ctx := context.Background()
	t1, err := a.Token(ctx, "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	t2, err := a.Token(ctx, "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	if t1 != t2 || f.mints.Load() != 1 || f.lookups.Load() != 1 {
		t.Errorf("tokens %q %q, mints %d, lookups %d; want one of each", t1, t2, f.mints.Load(), f.lookups.Load())
	}
	if got := f.scopes[0]; len(got) != 1 || got[0] != "widgets" {
		t.Errorf("mint scoped to %v, want [widgets]", got)
	}
	// A second process reads the file, not memory.
	b, _ := New(7, a.keyPath, a.dir)
	b.BaseURL = a.BaseURL
	if t3, _ := b.Token(ctx, "acme/widgets"); t3 != t1 || f.mints.Load() != 1 {
		t.Errorf("second App re-minted: %q, mints %d", t3, f.mints.Load())
	}
	info, err := os.Stat(filepath.Join(a.dir, "tokens", "acme", "widgets.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("cache file: %v, mode %v", err, info.Mode().Perm())
	}
}

func TestTokenRefreshesInsideThreshold(t *testing.T) {
	a, f := newTestApp(t)
	ctx := context.Background()
	start := time.Now()
	a.Now = func() time.Time { return start }
	if _, err := a.Token(ctx, "acme/widgets"); err != nil {
		t.Fatal(err)
	}
	a.Now = func() time.Time { return start.Add(time.Hour - RefreshBefore - time.Minute) }
	a.Token(ctx, "acme/widgets")
	if f.mints.Load() != 1 {
		t.Fatalf("refreshed early: %d mints", f.mints.Load())
	}
	a.Now = func() time.Time { return start.Add(time.Hour - RefreshBefore + time.Minute) }
	a.Token(ctx, "acme/widgets")
	if f.mints.Load() != 2 {
		t.Errorf("did not refresh inside threshold: %d mints", f.mints.Load())
	}
	if f.lookups.Load() != 1 {
		t.Errorf("installation looked up %d times; the cached id should be reused", f.lookups.Load())
	}
}

func TestConcurrentTokenMintsOnce(t *testing.T) {
	a, f := newTestApp(t)
	var wg sync.WaitGroup
	toks := make([]string, 8)
	for i := range toks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, _ := New(7, a.keyPath, a.dir)
			b.BaseURL = a.BaseURL
			toks[i], _ = b.Token(context.Background(), "acme/widgets")
		}()
	}
	wg.Wait()
	if f.mints.Load() != 1 {
		t.Errorf("%d mints, want 1", f.mints.Load())
	}
	for _, tok := range toks {
		if tok != toks[0] || tok == "" {
			t.Errorf("tokens differ: %v", toks)
		}
	}
}

func TestTokenNotInstalled(t *testing.T) {
	a, _ := newTestApp(t)
	_, err := a.Token(context.Background(), "acme/other")
	if !errors.Is(err, ErrNotInstalled) || !strings.Contains(err.Error(), "acme/other") {
		t.Errorf("err = %v, want ErrNotInstalled naming acme/other", err)
	}
}

func TestTokenRejectsBadRepo(t *testing.T) {
	a, _ := newTestApp(t)
	for _, r := range []string{"", "acme", "acme/../x", "../acme/x", "a/b/c", "acme/."} {
		if _, err := a.Token(context.Background(), r); err == nil {
			t.Errorf("repo %q accepted", r)
		}
	}
}

func TestBotIdentity(t *testing.T) {
	a, _ := newTestApp(t)
	id, err := a.Bot(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	want := Identity{Login: "acme-agent[bot]", Name: "acme-agent[bot]", Email: "9001+acme-agent[bot]@users.noreply.github.com"}
	if id != want {
		t.Errorf("Bot = %+v, want %+v", id, want)
	}
}

func TestTransportAuthenticatesPerRepo(t *testing.T) {
	a, _ := newTestApp(t)
	resp, err := a.Client().Get(a.BaseURL + "/repos/acme/widgets/issues/1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"auth":"token ghs_test1"`) {
		t.Errorf("body = %s", body)
	}
	if _, err := a.Client().Get(a.BaseURL + "/users/someone"); err == nil {
		t.Error("a non-repo path with no WithRepo context must fail, not go unauthenticated")
	}
	req, _ := http.NewRequestWithContext(WithRepo(context.Background(), "acme/widgets"), "GET", a.BaseURL+"/users/acme-agent[bot]", nil)
	if resp, err := a.Client().Do(req); err != nil || resp.StatusCode != 200 {
		t.Errorf("WithRepo request: %v %v", resp, err)
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/ghapp -v`
Expected: FAIL, `undefined: New`.

- [ ] **Step 4: Implement `key.go`**

```go
package ghapp

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// maxKeySize caps the read. A 4096-bit PEM key is about 3.2 KiB.
const maxKeySize = 16 * 1024

// loadKey reads and checks the app's private key.
//
// The checks are settings.readSecretFile's, for the same reason: this key
// mints a write token for every repository the app is installed on, so a
// group-readable copy, or one another account planted, must be refused
// rather than used.
func loadKey(path string) (*rsa.PrivateKey, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("github_app.private_key_path %q must be absolute", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("%s is a symlink, refusing to follow it", path)
		}
		return nil, fmt.Errorf("open GitHub App key: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat GitHub App key %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("%s is not owned by the current user", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s has mode %#o; chmod 0600 it before agent-utils will read it", path, perm)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxKeySize+1))
	if err != nil {
		return nil, fmt.Errorf("read GitHub App key %s: %w", path, err)
	}
	if len(raw) > maxKeySize {
		return nil, fmt.Errorf("%s is larger than %d bytes; it is not a GitHub App key", path, maxKeySize)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s is not a PEM file", path)
	}
	// GitHub issues PKCS#1 ("RSA PRIVATE KEY"); a key converted with openssl
	// is often PKCS#8. Accept both.
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s is not an RSA private key: %w", path, err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an RSA key; GitHub App keys are RSA", path)
	}
	return rk, nil
}
```

- [ ] **Step 5: Implement `ghapp.go`**

```go
// Package ghapp authenticates as a GitHub App.
//
// An installation token expires an hour after it is minted and cannot be
// refreshed; a new one is minted by signing a JWT with the app's private key.
// A dispatch can run for a day, and the agent's gh and git each run as fresh
// processes, so tokens are cached ON DISK, per repository, and every process
// that needs one -- the daemon, a tick, the gh shim, the git credential helper
// -- reads the same file and mints only when it is close to expiry.
package ghapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/settings"
)

// RefreshBefore is how long a cached token must still have to run before it
// is handed out. Ten minutes is far longer than any one gh or git command, so
// a token never expires mid-command.
const RefreshBefore = 10 * time.Minute

// ErrNotInstalled reports that the app has no installation covering a
// repository.
var ErrNotInstalled = errors.New("the GitHub App is not installed on this repository")

// Identity is a git author identity, and for the bot, its GitHub login.
type Identity struct {
	Login string
	Name  string
	Email string
}

// App is one GitHub App's credentials and token cache.
type App struct {
	ID      int64
	BaseURL string
	HTTP    *http.Client
	Now     func() time.Time

	key     *rsa.PrivateKey
	keyPath string
	dir     string
}

// New loads the key and returns an App caching under cacheDir.
func New(id int64, keyPath, cacheDir string) (*App, error) {
	if id <= 0 {
		return nil, fmt.Errorf("github_app.app_id must be positive")
	}
	k, err := loadKey(keyPath)
	if err != nil {
		return nil, err
	}
	return &App{
		ID: id, BaseURL: "https://api.github.com", HTTP: http.DefaultClient, Now: time.Now,
		key: k, keyPath: keyPath, dir: cacheDir,
	}, nil
}

// FromSettings returns the configured App, or nil when none is configured.
//
// A configured app whose key cannot be used is an error, never a fallback to
// GITHUB_TOKEN: an operator who set github_app expects every action to be the
// app's, and a silent fallback would attribute them to whoever owns the token.
func FromSettings(s *settings.Settings) (*App, error) {
	if s == nil || s.GitHubApp.AppID == 0 {
		return nil, nil
	}
	if s.GitHubApp.PrivateKeyPath == "" {
		return nil, fmt.Errorf("github_app.app_id is set but github_app.private_key_path is not")
	}
	dir, err := home.Dir()
	if err != nil {
		return nil, err
	}
	return New(s.GitHubApp.AppID, s.GitHubApp.PrivateKeyPath, dir)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// JWT returns an app JWT valid for nine minutes. iat is backdated a minute
// for clock skew, as GitHub documents; exp stays under GitHub's ten-minute cap.
func (a *App) JWT() (string, error) {
	now := a.Now()
	claims, err := json.Marshal(map[string]int64{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": a.ID,
	})
	if err != nil {
		return "", err
	}
	signing := b64([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign app JWT: %w", err)
	}
	return signing + "." + b64(sig), nil
}

// repoPart is what GitHub allows in an owner or repository name. It also
// keeps both out of path traversal when they become cache file names.
var repoPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func splitRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !repoPart.MatchString(owner) || !repoPart.MatchString(name) ||
		owner == "." || owner == ".." || name == "." || name == ".." {
		return "", "", fmt.Errorf("%q is not an owner/name repository", repo)
	}
	return owner, name, nil
}

type cachedToken struct {
	InstallationID int64     `json:"installation_id"`
	Token          string    `json:"token"`
	ExpiresAt      time.Time `json:"expires_at"`
}

func (a *App) tokenPath(owner, name string) string {
	return filepath.Join(a.dir, "tokens", owner, name+".json")
}

func (a *App) fresh(c cachedToken) bool {
	return c.Token != "" && c.ExpiresAt.Sub(a.Now()) > RefreshBefore
}

// Token returns an installation token scoped to repo alone.
//
// The fast path reads the cache file without the lock. A stale or missing
// entry takes the lock and READS AGAIN before minting: every process that
// found the same stale entry queues on the lock, and only the first one
// through should pay for a mint.
func (a *App) Token(ctx context.Context, repo string) (string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	path := a.tokenPath(owner, name)
	if c, ok := readJSON[cachedToken](path); ok && a.fresh(c) {
		return c.Token, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create token cache: %w", err)
	}
	l, err := lock.AcquireWait(ctx, path+".lock")
	if err != nil {
		return "", fmt.Errorf("lock token cache: %w", err)
	}
	defer l.Release()

	c, ok := readJSON[cachedToken](path)
	if ok && a.fresh(c) {
		return c.Token, nil
	}
	inst := c.InstallationID
	if inst == 0 {
		if inst, err = a.installation(ctx, owner, name); err != nil {
			return "", err
		}
	}
	tok, exp, err := a.mint(ctx, inst, name)
	if errors.Is(err, ErrNotInstalled) && c.InstallationID != 0 {
		// The cached installation id is stale: the app was uninstalled and
		// reinstalled. Look it up once more before giving up.
		if inst, err = a.installation(ctx, owner, name); err != nil {
			return "", err
		}
		tok, exp, err = a.mint(ctx, inst, name)
	}
	if err != nil {
		return "", err
	}
	if err := writeJSON(path, cachedToken{InstallationID: inst, Token: tok, ExpiresAt: exp}); err != nil {
		return "", err
	}
	return tok, nil
}

func (a *App) installation(ctx context.Context, owner, name string) (int64, error) {
	var out struct{ ID int64 `json:"id"` }
	status, err := a.jwtCall(ctx, "GET", "/repos/"+owner+"/"+name+"/installation", nil, &out)
	if status == http.StatusNotFound {
		return 0, fmt.Errorf("%s/%s: %w", owner, name, ErrNotInstalled)
	}
	if err != nil {
		return 0, fmt.Errorf("look up the app installation for %s/%s: %w", owner, name, err)
	}
	return out.ID, nil
}

func (a *App) mint(ctx context.Context, inst int64, name string) (string, time.Time, error) {
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	body := map[string][]string{"repositories": {name}}
	status, err := a.jwtCall(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", inst), body, &out)
	if status == http.StatusNotFound || status == http.StatusUnprocessableEntity {
		return "", time.Time{}, fmt.Errorf("installation %d does not cover %s: %w", inst, name, ErrNotInstalled)
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mint installation token: %w", err)
	}
	if out.Token == "" {
		return "", time.Time{}, errors.New("mint installation token: GitHub returned no token")
	}
	return out.Token, out.ExpiresAt, nil
}

// Slug returns the app's slug, from the cache when one is stored.
func (a *App) Slug(ctx context.Context) (string, error) {
	id, err := a.botCache(ctx, "")
	return strings.TrimSuffix(id.Login, "[bot]"), err
}

// Bot returns the app's bot identity. The bot's user id is read with an
// installation token for repo, because /users accepts no app JWT.
func (a *App) Bot(ctx context.Context, repo string) (Identity, error) {
	return a.botCache(ctx, repo)
}

type botFile struct {
	Slug   string `json:"slug"`
	UserID int64  `json:"user_id"`
}

// botCache fills tokens/bot-<app id>.json as far as it can. With repo empty
// only the slug is needed and fetched; with a repo the user id is fetched too.
// Neither changes for the life of an app, so the file is never refreshed.
func (a *App) botCache(ctx context.Context, repo string) (Identity, error) {
	path := filepath.Join(a.dir, "tokens", fmt.Sprintf("bot-%d.json", a.ID))
	b, _ := readJSON[botFile](path)
	if b.Slug == "" {
		var out struct{ Slug string `json:"slug"` }
		if _, err := a.jwtCall(ctx, "GET", "/app", nil, &out); err != nil {
			return Identity{}, fmt.Errorf("read the GitHub App: %w", err)
		}
		if out.Slug == "" {
			return Identity{}, errors.New("read the GitHub App: GitHub returned no slug")
		}
		b.Slug = out.Slug
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return Identity{}, err
		}
		_ = writeJSON(path, b)
	}
	login := b.Slug + "[bot]"
	if repo == "" {
		return Identity{Login: login, Name: login}, nil
	}
	if b.UserID == 0 {
		tok, err := a.Token(ctx, repo)
		if err != nil {
			return Identity{}, err
		}
		var out struct{ ID int64 `json:"id"` }
		if _, err := a.call(ctx, "GET", "/users/"+url.PathEscape(login), "token "+tok, nil, &out); err != nil {
			return Identity{}, fmt.Errorf("read the app's bot user: %w", err)
		}
		b.UserID = out.ID
		_ = writeJSON(path, b)
	}
	return Identity{
		Login: login,
		Name:  login,
		Email: fmt.Sprintf("%d+%s@users.noreply.github.com", b.UserID, login),
	}, nil
}

func (a *App) jwtCall(ctx context.Context, method, path string, in, out any) (int, error) {
	jwt, err := a.JWT()
	if err != nil {
		return 0, err
	}
	return a.call(ctx, method, path, "Bearer "+jwt, in, out)
}

// call makes one request. The status is returned alongside the error so a
// caller can turn a 404 into ErrNotInstalled; the response body is never put
// in the error, because a mint response carries a token.
func (a *App) call(ctx context.Context, method, path, auth string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(a.BaseURL, "/")+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func readJSON[T any](path string) (T, bool) {
	var v T
	raw, err := os.ReadFile(path)
	if err != nil {
		return v, false
	}
	return v, json.Unmarshal(raw, &v) == nil
}

// writeJSON writes v to path atomically at 0600. See settings.writeSecretFile
// for why the temp name is random and opened O_EXCL|O_NOFOLLOW.
func writeJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmp := path + ".tmp." + hex.EncodeToString(suffix[:])
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("write token cache: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write token cache: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write token cache: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write token cache: %w", err)
	}
	return nil
}
```

- [ ] **Step 6: Implement `transport.go`**

```go
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
```

- [ ] **Step 7: Run tests**

Run: `go test ./internal/ghapp -race -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/ghapp
git commit -m "feat(ghapp): mint and cache repo-scoped GitHub App installation tokens"
```

---

### Task 4: `ghub`: app client, self-login, co-author lookup

**Files:**
- Modify: `internal/ghub/ghub.go` (`GitHubClient` struct ~line 37, `New` ~58, `AuthenticatedLogin` ~349)
- Create: `internal/ghub/coauthor.go`
- Test: `internal/ghub/coauthor_test.go`

**Interfaces:**
- Consumes: `ghapp.App`, `ghapp.WithRepo` (Task 3).
- Produces:
  ```go
  func NewApp(app *ghapp.App) *GitHubClient
  type AuthorReader interface {
      CoAuthor(ctx context.Context, owner, repo string, number int) (string, error)
  }
  func (g *GitHubClient) CoAuthor(ctx context.Context, owner, repo string, number int) (string, error)
  func FormatCoAuthor(login string, id int64, name, email string) string
  ```

- [ ] **Step 1: Write the failing tests**

```go
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
```

```go
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
```

Also add, in the same file, a test that `AuthenticatedLogin` uses the override:

```go
func TestAuthenticatedLoginUsesSelfOverride(t *testing.T) {
	g := &GitHubClient{c: github.NewClient(nil), self: func(context.Context) (string, error) { return "acme-agent[bot]", nil }}
	got, err := g.AuthenticatedLogin(context.Background())
	if err != nil || got != "acme-agent[bot]" {
		t.Errorf("AuthenticatedLogin = %q, %v", got, err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/ghub -run 'CoAuthor|SelfOverride' -v`
Expected: FAIL, undefined `FormatCoAuthor`, `self`.

- [ ] **Step 3: Implement**

In `ghub.go`, add to `GitHubClient`:

```go
	// self, when set, answers AuthenticatedLogin instead of GET /user. An
	// installation token cannot call /user (403), so an app-mode client is
	// given the bot login here.
	self func(context.Context) (string, error)
```

In `AuthenticatedLogin`, after the cached-login check and before `g.c.Users.Get`:

```go
	if g.self != nil {
		login, err := g.self(ctx)
		if err != nil {
			return "", fmt.Errorf("get app login: %w", err)
		}
		if login == "" {
			return "", errors.New("the GitHub App has no login")
		}
		g.login = login
		return login, nil
	}
```

Add after `New`:

```go
// NewApp returns a client that authenticates each request as app's
// installation on the repository it addresses. See ghapp.Client.
//
// In app mode the agent and this daemon act as the same bot, so the
// self-filter in LatestReviewActivity finally filters the agent's own replies
// by login. store.LastTendAt remains the guard that holds in token mode.
func NewApp(app *ghapp.App) *GitHubClient {
	c := github.NewClient(app.Client())
	if u, err := url.Parse(strings.TrimSuffix(app.BaseURL, "/") + "/"); err == nil {
		c.BaseURL = u
	}
	return &GitHubClient{c: c, self: func(ctx context.Context) (string, error) {
		slug, err := app.Slug(ctx)
		if err != nil {
			return "", err
		}
		return slug + "[bot]", nil
	}}
}
```

Create `coauthor.go`:

```go
package ghub

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/seanmcgary/agent-utils/internal/ghapp"
)

// AuthorReader resolves the co-author an issue's commits should credit. It is
// narrow for the reason EpicReader is: a fake for any other pass need not
// grow it.
type AuthorReader interface {
	CoAuthor(ctx context.Context, owner, repo string, number int) (string, error)
}

// CoAuthor returns "Name <email>" for issue number's author, or "" when the
// author is a bot.
//
// The profile is read for the display name and PUBLIC email. A private email
// is not readable with an installation token or another user's PAT, and a
// failed profile read is not an error: GitHub credits a co-author through the
// noreply address just as well, so the trailer falls back to it.
func (g *GitHubClient) CoAuthor(ctx context.Context, owner, repo string, number int) (string, error) {
	gi, _, err := g.c.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return "", fmt.Errorf("get issue #%d author: %w", number, err)
	}
	u := gi.GetUser()
	login := u.GetLogin()
	if login == "" || u.GetType() == "Bot" || strings.HasSuffix(login, "[bot]") {
		return "", nil
	}
	var name, email string
	if p, _, err := g.c.Users.Get(ghapp.WithRepo(ctx, owner+"/"+repo), login); err == nil {
		name, email = p.GetName(), p.GetEmail()
	}
	return FormatCoAuthor(login, u.GetID(), name, email), nil
}

// FormatCoAuthor renders a Co-authored-by value. Control characters are
// dropped, so a profile name cannot end the trailer line and start another,
// and a name or email that would break the "Name <email>" shape falls back to
// the login or the noreply address.
func FormatCoAuthor(login string, id int64, name, email string) string {
	clean := func(s string) string {
		return strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, s))
	}
	name, email = clean(name), clean(email)
	if name == "" || strings.ContainsAny(name, "<>") {
		name = login
	}
	if email == "" || strings.ContainsAny(email, "<> ") || !strings.Contains(email, "@") {
		email = fmt.Sprintf("%d+%s@users.noreply.github.com", id, login)
	}
	return name + " <" + email + ">"
}
```

Add `"net/url"` to `ghub.go` imports and the `ghapp` import.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/ghub -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ghub
git commit -m "feat(ghub): GitHub App client and issue-author co-author lookup"
```

---

### Task 5: `dispatches.co_author`

**Files:**
- Modify: `internal/store/store.go` (CREATE TABLE ~line 84, `addedColumns` ~448, `CreateDispatch` ~1075, `dispatchColumns`/`scanDispatch` ~1199), `internal/store/types.go` (`Dispatch`)
- Test: `internal/store/coauthor_test.go`

**Interfaces:**
- Produces: `store.Dispatch.CoAuthor string`, persisted by `CreateDispatch`, read by `GetDispatch` and every other scanner.

- [ ] **Step 1: Write the failing test**

Use the package's existing test helper for opening a scoped store (find it with `grep -n 'func newTestStore\|func openTest' internal/store/*_test.go` and use that name in place of `newTestStore`):

```go
func TestDispatchCarriesCoAuthor(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateDispatch(Dispatch{
		Loop: "execution", Repo: "acme/widgets", Number: 5, Kind: KindStart,
		CoAuthor: "Seán O'Brien <7+sob@users.noreply.github.com>",
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.GetDispatch(id)
	if err != nil {
		t.Fatal(err)
	}
	if d.CoAuthor != "Seán O'Brien <7+sob@users.noreply.github.com>" {
		t.Errorf("CoAuthor = %q", d.CoAuthor)
	}
}
```

Add a migration case to the existing upgrade test, if one enumerates `addedColumns` against an old-schema database, so a pre-existing database gains the column.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store -run CoAuthor -v`
Expected: FAIL, unknown field `CoAuthor`.

- [ ] **Step 3: Implement**

- `types.go`, in `Dispatch` after `ReviewPending`:
  ```go
	// CoAuthor is the "Name <email>" the runner's commit hook credits, resolved
	// by the tick from the issue's author. The detached runner holds no GitHub
	// client, so this row is how the value reaches it. Empty means none: the
	// setting is off, the author is a bot, or the lookup failed.
	CoAuthor string
  ```
- `store.go` CREATE TABLE `dispatches`: after `review_pending`, add `,\n  co_author TEXT NOT NULL DEFAULT ''` with a one-line comment pointing at `Dispatch.CoAuthor`.
- `addedColumns`: `{"dispatches", "co_author", "TEXT NOT NULL DEFAULT ''"},`
- `CreateDispatch`: add `co_author` to the column list, one more `?` in the SELECT, and `d.CoAuthor` after `d.ReviewPending` in the arguments.
- `dispatchColumns`: append `, co_author`; `scanDispatch`: append `&d.CoAuthor`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/store -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "feat(store): dispatches.co_author"
```

---

### Task 6: `internal/identity`: co-author hooks

**Files:**
- Create: `internal/identity/hooks.go`, `internal/identity/identity.go` (shared helpers only in this task)
- Test: `internal/identity/hooks_test.go`, `internal/identity/gittest_test.go`

**Interfaces:**
- Produces:
  ```go
  const RepoEnv = "AGENT_UTILS_DISPATCH_REPO"
  type Plan struct {
      Dir      string          // per-dispatch directory; Prepare creates it, the caller removes it
      WorkDir  string          // the agent's working directory (a git checkout)
      Repo     string          // owner/name
      SelfPath string          // absolute path of this agent-utils binary
      Home     string          // AGENT_UTILS_HOME to bake into helper commands, "" when unset
      Bot      *ghapp.Identity // nil in token mode
      CoAuthor string          // "Name <email>", "" for none
  }
  func Prepare(ctx context.Context, p Plan) ([]string, error) // extra env for the agent
  func shellQuote(s string) string
  ```
  In this task `Prepare` handles only `CoAuthor`; Task 7 adds `Bot`.

- [ ] **Step 1: Write the git test helpers**

`gittest_test.go`:

```go
package identity

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isolatedGit returns an environment that hides the developer's own git
// config, so a test sees only what it sets up.
func isolatedGit(t *testing.T) []string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(cfg, []byte("[user]\n\tname = Host User\n\temail = host@example.com\n[init]\n\tdefaultBranch = main\n"), 0o600)
	return []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=" + cfg, "GIT_CONFIG_NOSYSTEM=1",
	}
}

func run(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo makes a repository with one commit and returns its path.
func newRepo(t *testing.T, env []string) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, env, "git", "init", "-q")
	os.WriteFile(filepath.Join(dir, "a"), []byte("a"), 0o644)
	run(t, dir, env, "git", "add", "a")
	run(t, dir, env, "git", "commit", "-qm", "init")
	return dir
}

func commit(t *testing.T, dir string, env []string, file, msg string) string {
	t.Helper()
	os.WriteFile(filepath.Join(dir, file), []byte(file), 0o644)
	run(t, dir, env, "git", "add", file)
	run(t, dir, env, "git", "commit", "-qm", msg)
	return run(t, dir, env, "git", "log", "-1", "--format=%B")
}

func trailers(msg string) []string {
	var out []string
	for _, l := range strings.Split(msg, "\n") {
		if strings.HasPrefix(l, "Co-authored-by:") {
			out = append(out, l)
		}
	}
	return out
}
```

- [ ] **Step 2: Write the failing tests**

`hooks_test.go`:

```go
package identity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const sean = "Sean McGary <sean@seanmcgary.com>"

func prepare(t *testing.T, base []string, workDir, coAuthor string) []string {
	t.Helper()
	extra, err := Prepare(context.Background(), Plan{
		Dir: filepath.Join(t.TempDir(), "run"), WorkDir: workDir, Repo: "acme/widgets",
		SelfPath: "/bin/false", CoAuthor: coAuthor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(append([]string{}, base...), extra...)
}

func TestCoAuthorTrailerAddedOnce(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	env := prepare(t, base, repo, sean)

	msg := commit(t, repo, env, "b", "feat: b")
	if got := trailers(msg); len(got) != 1 || got[0] != "Co-authored-by: "+sean {
		t.Fatalf("trailers = %q", got)
	}
	run(t, repo, env, "git", "commit", "-q", "--amend", "--no-edit")
	run(t, repo, env, "git", "commit", "-q", "--amend", "-m", run(t, repo, env, "git", "log", "-1", "--format=%B"))
	if got := trailers(run(t, repo, env, "git", "log", "-1", "--format=%B")); len(got) != 1 {
		t.Errorf("after amends, trailers = %q", got)
	}
}

func TestCoAuthorWithApostrophe(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	v := "Seán O'Brien <7+sob@users.noreply.github.com>"
	msg := commit(t, repo, prepare(t, base, repo, v), "b", "feat: b")
	if got := trailers(msg); len(got) != 1 || got[0] != "Co-authored-by: "+v {
		t.Errorf("trailers = %q", got)
	}
}

func TestCoAuthorFromGrandchildProcess(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	env := prepare(t, base, repo, sean)
	os.WriteFile(filepath.Join(repo, "c"), []byte("c"), 0o644)
	run(t, repo, env, "sh", "-c", `sh -c "git add c && git commit -qm 'feat: c'"`)
	if got := trailers(run(t, repo, env, "git", "log", "-1", "--format=%B")); len(got) != 1 {
		t.Errorf("trailers = %q", got)
	}
}

func TestHooksChainToRepositoryHooks(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	hooks := filepath.Join(repo, ".git", "hooks")
	marker := filepath.Join(t.TempDir(), "ran")
	os.WriteFile(filepath.Join(hooks, "prepare-commit-msg"),
		[]byte("#!/bin/sh\necho repo-hook >> \""+marker+"\"\necho 'Ticket: X-1' >> \"$1\"\n"), 0o755)
	os.WriteFile(filepath.Join(hooks, "pre-commit"),
		[]byte("#!/bin/sh\n[ -f BLOCK ] && exit 1\necho pre-commit >> \""+marker+"\"\n"), 0o755)
	env := prepare(t, base, repo, sean)

	msg := commit(t, repo, env, "b", "feat: b")
	if !strings.Contains(msg, "Ticket: X-1") || len(trailers(msg)) != 1 {
		t.Errorf("message = %q; want the repo hook's line and our trailer", msg)
	}
	ran, _ := os.ReadFile(marker)
	if string(ran) != "pre-commit\nrepo-hook\n" {
		t.Errorf("hooks ran = %q", ran)
	}

	os.WriteFile(filepath.Join(repo, "BLOCK"), nil, 0o644)
	run(t, repo, env, "git", "add", "BLOCK")
	cmd := exec.Command("git", "commit", "-qm", "blocked")
	cmd.Dir, cmd.Env = repo, env
	if err := cmd.Run(); err == nil {
		t.Error("a failing repository pre-commit must still refuse the commit")
	}
}

func TestHooksHonourRepositoryHooksPath(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	custom := filepath.Join(repo, ".husky")
	os.MkdirAll(custom, 0o755)
	marker := filepath.Join(t.TempDir(), "ran")
	os.WriteFile(filepath.Join(custom, "commit-msg"), []byte("#!/bin/sh\necho husky >> \""+marker+"\"\n"), 0o755)
	run(t, repo, base, "git", "config", "core.hooksPath", ".husky")
	commit(t, repo, prepare(t, base, repo, sean), "b", "feat: b")
	if ran, _ := os.ReadFile(marker); string(ran) != "husky\n" {
		t.Errorf("husky commit-msg ran = %q", ran)
	}
}

func TestNoCoAuthorMeansNoHooks(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	dir := filepath.Join(t.TempDir(), "run")
	extra, err := Prepare(context.Background(), Plan{Dir: dir, WorkDir: repo, Repo: "acme/widgets", SelfPath: "/bin/false"})
	if err != nil {
		t.Fatal(err)
	}
	if len(extra) != 0 {
		t.Errorf("env = %q, want none", extra)
	}
	if _, err := os.Stat(filepath.Join(dir, "hooks")); !os.IsNotExist(err) {
		t.Error("hooks directory written with no co-author")
	}
}

func TestConcurrentDispatchesInSiblingWorktrees(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	authors := []string{sean, "Ada Lovelace <1+ada@users.noreply.github.com>"}
	var wts []string
	for i := range authors {
		wt := filepath.Join(t.TempDir(), "wt")
		run(t, repo, base, "git", "worktree", "add", "-q", "-b", "issue-"+string(rune('a'+i)), wt)
		wts = append(wts, wt)
	}
	envs := make([][]string, len(authors))
	for i := range authors {
		envs[i] = prepare(t, base, wts[i], authors[i])
	}
	var wg sync.WaitGroup
	msgs := make([]string, len(authors))
	for i := range authors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range 5 {
				msgs[i] = commit(t, wts[i], envs[i], "f"+string(rune('0'+n)), "feat: step")
			}
		}()
	}
	wg.Wait()
	for i, wt := range wts {
		log := run(t, wt, base, "git", "log", "-5", "--format=%(trailers:key=Co-authored-by,valueonly)")
		for _, l := range strings.Split(log, "\n") {
			if l != "" && l != authors[i] {
				t.Errorf("worktree %d has trailer %q, want only %q", i, l, authors[i])
			}
		}
		if strings.Count(log, authors[i]) != 5 {
			t.Errorf("worktree %d: %d of 5 commits credited", i, strings.Count(log, authors[i]))
		}
	}
}
```

(Add `"os/exec"` to `hooks_test.go` imports. `t.Fatalf` from a goroutine in `commit` is acceptable in this test only because failure aborts the test either way; if `go vet` objects, switch the goroutine body to collect errors.)

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/identity -v`
Expected: FAIL, `undefined: Prepare`.

- [ ] **Step 4: Implement `identity.go` (co-author part)**

```go
// Package identity prepares the git and gh identity one agent dispatch runs
// under.
//
// Everything here is per dispatch and travels in the agent's environment or
// in a directory only that dispatch uses. Nothing is written to the
// repository's .git/config or .git/hooks: every worktree of a repository
// shares those, so two agents working issues by different authors would
// overwrite each other's settings.
package identity

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/ghapp"
)

// RepoEnv names the repository the token helpers serve. It is the only way
// they learn it; see cmd/agent-utils internal gh-token.
const RepoEnv = "AGENT_UTILS_DISPATCH_REPO"

// Plan is what Prepare needs to know about one dispatch.
type Plan struct {
	Dir      string
	WorkDir  string
	Repo     string
	SelfPath string
	Home     string
	Bot      *ghapp.Identity
	CoAuthor string
}

// Prepare writes the dispatch's directory and returns the environment to add
// to the agent's. With no app and no co-author it writes nothing and returns
// nothing, so the agent runs exactly as it did before this package existed.
func Prepare(ctx context.Context, p Plan) ([]string, error) {
	var kvs []gitKV
	var env []string
	if p.CoAuthor != "" {
		if err := os.MkdirAll(p.Dir, 0o700); err != nil {
			return nil, fmt.Errorf("create dispatch directory: %w", err)
		}
		hooksDir, err := writeHooks(ctx, p)
		if err != nil {
			return nil, err
		}
		kvs = append(kvs, gitKV{"core.hooksPath", hooksDir})
	}
	if len(kvs) == 0 {
		return env, nil
	}
	return append(env, renderGitConfig(kvs)...), nil
}

type gitKV struct{ key, value string }

// renderGitConfig turns kvs into GIT_CONFIG_COUNT and its numbered pairs.
// git applies these above every config file, including the repository's, and
// a repeated key adds a value rather than replacing it, which is what lets
// one key carry two insteadOf rewrites.
func renderGitConfig(kvs []gitKV) []string {
	out := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(kvs))}
	for i, kv := range kvs {
		out = append(out,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv.key),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv.value))
	}
	return out
}

// shellQuote quotes s for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
```

- [ ] **Step 5: Implement `hooks.go`**

```go
package identity

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// clientHooks is every client-side hook git runs from core.hooksPath. Setting
// core.hooksPath replaces the repository's hook directory outright, so each
// one gets a stub that runs the repository's own; without them husky,
// lefthook and plain .git/hooks checks would silently stop running.
var clientHooks = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch",
	"pre-commit", "pre-merge-commit", "commit-msg", "post-commit",
	"pre-rebase", "post-checkout", "post-merge", "pre-push",
	"pre-auto-gc", "post-rewrite", "reference-transaction",
	"post-index-change", "sendemail-validate",
}

// writeHooks writes <Dir>/hooks and returns its path.
//
// The repository's hook directory is resolved here, in the runner, before
// the agent's environment overrides core.hooksPath -- git rev-parse
// --git-path hooks answers with the repository's own core.hooksPath when it
// has one, else <common dir>/hooks.
//
// The co-author is written to a file the hook reads, not into the script, so
// a name with a quote in it is data and never shell.
func writeHooks(ctx context.Context, p Plan) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	cmd.Dir = p.WorkDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("find the repository's hooks in %s: %w", p.WorkDir, err)
	}
	orig := strings.TrimSpace(string(out))

	dir := filepath.Join(p.Dir, "hooks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create hooks directory: %w", err)
	}
	valueFile := filepath.Join(p.Dir, "co-author")
	if err := os.WriteFile(valueFile, []byte(p.CoAuthor), 0o600); err != nil {
		return "", fmt.Errorf("write co-author: %w", err)
	}

	for _, name := range clientHooks {
		stub := "#!/bin/sh\n" +
			"h=" + shellQuote(filepath.Join(orig, name)) + "\n" +
			"[ -x \"$h\" ] && exec \"$h\" \"$@\"\n" +
			"exit 0\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0o700); err != nil {
			return "", fmt.Errorf("write %s hook: %w", name, err)
		}
	}

	// The repository's prepare-commit-msg runs first and can refuse the
	// commit. addIfDifferent keeps the trailer to one copy across --amend,
	// rebase rewords and squashes.
	pcm := "#!/bin/sh\n" +
		"h=" + shellQuote(filepath.Join(orig, "prepare-commit-msg")) + "\n" +
		"if [ -x \"$h\" ]; then \"$h\" \"$@\" || exit $?; fi\n" +
		"v=$(cat " + shellQuote(valueFile) + ") || exit 1\n" +
		"exec git interpret-trailers --in-place --if-exists addIfDifferent --trailer \"Co-authored-by: $v\" \"$1\"\n"
	if err := os.WriteFile(filepath.Join(dir, "prepare-commit-msg"), []byte(pcm), 0o700); err != nil {
		return "", fmt.Errorf("write prepare-commit-msg hook: %w", err)
	}
	return dir, nil
}
```

- [ ] **Step 6: Run tests**

Run: `go test ./internal/identity -race -v`
Expected: PASS. (`TestHooksChainToRepositoryHooks` expects `pre-commit\nrepo-hook\n` because git runs `pre-commit` before `prepare-commit-msg`.)

- [ ] **Step 7: Commit**

```bash
git add internal/identity
git commit -m "feat(identity): co-author prepare-commit-msg hook chained to repository hooks"
```

---

### Task 7: `internal/identity`: app identity, gh shim, credential helper env

**Files:**
- Modify: `internal/identity/identity.go`
- Test: `internal/identity/app_test.go`

**Interfaces:**
- Consumes: `Plan`, `renderGitConfig`, `shellQuote` (Task 6), `ghapp.Identity` (Task 3).
- Produces:
  ```go
  func GitEnv(selfPath, home, repo string, bot ghapp.Identity) []string // for Go-side git (worktree.Manager.Env)
  func helperCommand(selfPath, home string) string
  ```

- [ ] **Step 1: Write the failing tests**

```go
package identity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/ghapp"
)

var bot = ghapp.Identity{Login: "acme-agent[bot]", Name: "acme-agent[bot]", Email: "9001+acme-agent[bot]@users.noreply.github.com"}

// fakeSelf writes a stand-in agent-utils binary that answers the two helper
// subcommands with a fixed token, and records the environment it saw.
func fakeSelf(t *testing.T) (path, seen string) {
	t.Helper()
	dir := t.TempDir()
	seen = filepath.Join(dir, "seen")
	path = filepath.Join(dir, "agent-utils")
	script := "#!/bin/sh\n" +
		"echo \"home=$AGENT_UTILS_HOME repo=$" + RepoEnv + "\" >> " + shellQuote(seen) + "\n" +
		"case \"$2\" in\n" +
		"gh-token) echo ghs_app ;;\n" +
		"git-credential) [ \"$3\" = get ] && printf 'username=x-access-token\\npassword=ghs_app\\n' ;;\n" +
		"esac\n"
	os.WriteFile(path, []byte(script), 0o755)
	return path, seen
}

func appEnv(t *testing.T, base []string, workDir, self, home, coAuthor string) []string {
	t.Helper()
	extra, err := Prepare(context.Background(), Plan{
		Dir: filepath.Join(t.TempDir(), "run"), WorkDir: workDir, Repo: "acme/widgets",
		SelfPath: self, Home: home, Bot: &bot, CoAuthor: coAuthor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(append([]string{}, base...), extra...)
}

func TestAppEnvSetsBotIdentity(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	self, _ := fakeSelf(t)
	env := appEnv(t, base, repo, self, "", "")
	commit(t, repo, env, "b", "feat: b")
	got := run(t, repo, env, "git", "log", "-1", "--format=%an <%ae>|%cn <%ce>")
	want := bot.Name + " <" + bot.Email + ">"
	if got != want+"|"+want {
		t.Errorf("author|committer = %q, want bot for both", got)
	}
}

func TestAppEnvAndCoAuthorTogether(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	self, _ := fakeSelf(t)
	env := appEnv(t, base, repo, self, "", sean)
	msg := commit(t, repo, env, "b", "feat: b")
	if got := trailers(msg); len(got) != 1 || got[0] != "Co-authored-by: "+sean {
		t.Errorf("trailers = %q", got)
	}
	if got := run(t, repo, env, "git", "log", "-1", "--format=%ae"); got != bot.Email {
		t.Errorf("author = %q", got)
	}
}

func TestAppEnvRewritesSSHRemote(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	run(t, repo, base, "git", "remote", "add", "origin", "git@github.com:acme/widgets.git")
	self, _ := fakeSelf(t)
	env := appEnv(t, base, repo, self, "", "")
	if got := run(t, repo, env, "git", "remote", "get-url", "--push", "origin"); got != "https://github.com/acme/widgets.git" {
		t.Errorf("push url = %q", got)
	}
	run(t, repo, base, "git", "remote", "set-url", "origin", "ssh://git@github.com/acme/widgets.git")
	if got := run(t, repo, env, "git", "remote", "get-url", "--push", "origin"); got != "https://github.com/acme/widgets.git" {
		t.Errorf("ssh:// push url = %q", got)
	}
}

func TestAppEnvCredentialHelperOverridesHostHelper(t *testing.T) {
	base := isolatedGit(t)
	// A host helper that would hand out a personal token.
	host := filepath.Join(t.TempDir(), "host-helper")
	os.WriteFile(host, []byte("#!/bin/sh\nprintf 'username=me\\npassword=ghp_personal\\n'\n"), 0o755)
	cfgPath := strings.TrimPrefix(base[2], "GIT_CONFIG_GLOBAL=")
	f, _ := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("[credential]\n\thelper = !" + host + "\n")
	f.Close()
	repo := newRepo(t, base)
	self, _ := fakeSelf(t)
	env := appEnv(t, base, repo, self, "", "")

	cmd := exec.Command("git", "credential", "fill")
	cmd.Dir, cmd.Env = repo, env
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "password=ghs_app") || strings.Contains(string(out), "ghp_personal") {
		t.Errorf("credential fill = %q; want the app token only", out)
	}
}

func TestGhShimPassesFreshToken(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	self, _ := fakeSelf(t)
	// A stand-in gh on PATH that prints the token it was given.
	ghDir := t.TempDir()
	os.WriteFile(filepath.Join(ghDir, "gh"), []byte("#!/bin/sh\necho \"GH_TOKEN=$GH_TOKEN args=$*\"\n"), 0o755)
	base[0] = "PATH=" + ghDir + ":" + os.Getenv("PATH")
	t.Setenv("PATH", ghDir+":"+os.Getenv("PATH")) // Prepare resolves gh from its own PATH
	env := appEnv(t, base, repo, self, "", "")
	if got := run(t, repo, env, "sh", "-c", "gh issue list"); got != "GH_TOKEN=ghs_app args=issue list" {
		t.Errorf("gh shim output = %q", got)
	}
	if strings.Contains(strings.Join(env, "\n"), "ghs_app") {
		t.Error("the token itself is in the agent's environment")
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "GH_CONFIG_DIR=") && strings.Contains(kv, os.Getenv("HOME")) {
			t.Errorf("GH_CONFIG_DIR points at the host user's config: %s", kv)
		}
	}
}

func TestHelperCommandCarriesHome(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	self, seen := fakeSelf(t)
	env := appEnv(t, base, repo, self, "/srv/agent-utils home", "")
	cmd := exec.Command("git", "credential", "fill")
	cmd.Dir, cmd.Env = repo, env
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
	if _, err := cmd.Output(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(seen)
	if !strings.Contains(string(got), "home=/srv/agent-utils home repo=acme/widgets") {
		t.Errorf("helper saw %q", got)
	}
}

func TestGitEnvForDaemon(t *testing.T) {
	env := GitEnv("/usr/local/bin/agent-utils", "", "acme/widgets", bot)
	joined := strings.Join(env, "\n")
	for _, want := range []string{RepoEnv + "=acme/widgets", "=user.email", "=" + bot.Email, "insteadOf"} {
		if !strings.Contains(joined, want) {
			t.Errorf("GitEnv missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "core.hooksPath") {
		t.Error("the daemon's git must not get the co-author hook")
	}
}
```

(Add `"os/exec"` to imports.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/identity -run 'App|Gh|Helper|GitEnv' -v`
Expected: FAIL, bot identity absent, `undefined: GitEnv`.

- [ ] **Step 3: Implement**

Add to `identity.go`:

```go
// helperCommand is the shell prefix that runs this binary's token helpers.
// AGENT_UTILS_HOME is written into it rather than passed in the agent's
// environment: agentEnv withholds that variable from the agent on purpose
// (see runner.Spawn), but the helpers must open the same settings and token
// cache as the daemon that spawned it.
func helperCommand(selfPath, home string) string {
	cmd := shellQuote(selfPath)
	if home != "" {
		cmd = "AGENT_UTILS_HOME=" + shellQuote(home) + " " + cmd
	}
	return cmd
}

// appGitConfig points git at the app: its credential, HTTPS for SSH-style
// remotes, and the bot as the default author and committer.
//
// The empty helper value comes FIRST and is load-bearing: it clears every
// helper inherited from the host's config files for github.com, so an
// osxkeychain or store helper holding a personal token is never asked.
//
// The identity is user.name/user.email, deliberately not GIT_AUTHOR_* in the
// environment: those would override the author of an existing commit on
// --amend and during a rebase, rewriting a human's authorship as the bot's.
func appGitConfig(selfPath, home string, bot ghapp.Identity) []gitKV {
	return []gitKV{
		{"credential.https://github.com.helper", ""},
		{"credential.https://github.com.helper", "!" + helperCommand(selfPath, home) + " internal git-credential"},
		{"url.https://github.com/.insteadOf", "git@github.com:"},
		{"url.https://github.com/.insteadOf", "ssh://git@github.com/"},
		{"user.name", bot.Name},
		{"user.email", bot.Email},
	}
}

// GitEnv is the environment Go-side git runs with in app mode: fetches and
// the automatic rebase's push. It carries no hooks; those commands make no
// new commits.
func GitEnv(selfPath, home, repo string, bot ghapp.Identity) []string {
	return append([]string{RepoEnv + "=" + repo},
		renderGitConfig(appGitConfig(selfPath, home, bot))...)
}

// writeGhShim writes <Dir>/bin/gh. It fetches a token on EVERY invocation:
// a dispatch outlives a token, and the helper returns the cached one unless
// it is within ghapp.RefreshBefore of expiring, so the call is cheap.
func writeGhShim(p Plan) (binDir string, err error) {
	realGh, err := exec.LookPath("gh")
	if err != nil {
		return "", nil // no gh on this host; nothing to wrap
	}
	binDir = filepath.Join(p.Dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", err
	}
	shim := "#!/bin/sh\n" +
		"GH_TOKEN=$(" + helperCommand(p.SelfPath, p.Home) + " internal gh-token) || exit 1\n" +
		"export GH_TOKEN\n" +
		"exec " + shellQuote(realGh) + " \"$@\"\n"
	return binDir, os.WriteFile(filepath.Join(binDir, "gh"), []byte(shim), 0o700)
}
```

Replace `Prepare`'s body:

```go
func Prepare(ctx context.Context, p Plan) ([]string, error) {
	if p.Bot == nil && p.CoAuthor == "" {
		return nil, nil
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create dispatch directory: %w", err)
	}
	var kvs []gitKV
	var env []string
	if p.Bot != nil {
		kvs = append(kvs, appGitConfig(p.SelfPath, p.Home, *p.Bot)...)
		ghConfig := filepath.Join(p.Dir, "gh")
		if err := os.MkdirAll(ghConfig, 0o700); err != nil {
			return nil, err
		}
		binDir, err := writeGhShim(p)
		if err != nil {
			return nil, fmt.Errorf("write gh shim: %w", err)
		}
		env = append(env,
			RepoEnv+"="+p.Repo,
			// An empty config dir, so a gh that somehow bypasses the shim
			// finds no stored login rather than the host user's.
			"GH_CONFIG_DIR="+ghConfig,
			"GH_PROMPT_DISABLED=1",
		)
		if binDir != "" {
			// Appended after agentEnv's PATH; exec keeps the last value of a
			// repeated key.
			env = append(env, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		}
	}
	if p.CoAuthor != "" {
		hooksDir, err := writeHooks(ctx, p)
		if err != nil {
			return nil, err
		}
		kvs = append(kvs, gitKV{"core.hooksPath", hooksDir})
	}
	return append(env, renderGitConfig(kvs)...), nil
}
```

Add `"os/exec"` and `"path/filepath"` to `identity.go` imports.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/identity -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/identity
git commit -m "feat(identity): app credential helper, gh shim and bot identity for the agent"
```

---

### Task 8: `internal gh-token` and `internal git-credential`

**Files:**
- Create: `cmd/agent-utils/internal_auth.go`
- Modify: `cmd/agent-utils/main.go` (`internalCommand()` ~line 1056: add two subcommands)
- Test: `cmd/agent-utils/internal_auth_test.go`

**Interfaces:**
- Consumes: `settings.Load`, `ghapp.FromSettings`, `ghapp.App.Token`, `identity.RepoEnv`.
- Produces: `func ghTokenCommand() *cli.Command`, `func gitCredentialCommand() *cli.Command`, and the testable core `func credentialResponse(in io.Reader, out io.Writer, op string, token func() (string, error)) error`.

- [ ] **Step 1: Write the failing tests**

```go
package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestCredentialResponse(t *testing.T) {
	tok := func() (string, error) { return "ghs_x", nil }
	var out bytes.Buffer
	if err := credentialResponse(strings.NewReader("protocol=https\nhost=github.com\n\n"), &out, "get", tok); err != nil {
		t.Fatal(err)
	}
	if out.String() != "username=x-access-token\npassword=ghs_x\n" {
		t.Errorf("get = %q", out.String())
	}
	for _, in := range []string{"protocol=https\nhost=gitlab.com\n\n", "protocol=http\nhost=github.com\n\n"} {
		out.Reset()
		credentialResponse(strings.NewReader(in), &out, "get", tok)
		if out.Len() != 0 {
			t.Errorf("%q answered with %q; only https github.com gets the token", in, out.String())
		}
	}
	out.Reset()
	if err := credentialResponse(strings.NewReader("protocol=https\nhost=github.com\n\n"), &out, "store", tok); err != nil || out.Len() != 0 {
		t.Errorf("store: %q, %v; want silence", out.String(), err)
	}
	failing := func() (string, error) { return "", errors.New("mint failed") }
	if err := credentialResponse(strings.NewReader("protocol=https\nhost=github.com\n\n"), &out, "get", failing); err == nil {
		t.Error("a failed mint must exit non-zero")
	}
}

func TestDispatchRepoRequired(t *testing.T) {
	t.Setenv(identity.RepoEnv, "")
	if _, err := dispatchRepo(); err == nil {
		t.Error("no repository in the environment must be an error")
	}
	t.Setenv(identity.RepoEnv, "acme/widgets")
	if r, err := dispatchRepo(); err != nil || r != "acme/widgets" {
		t.Errorf("dispatchRepo = %q, %v", r, err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/agent-utils -run 'CredentialResponse|DispatchRepo' -v`
Expected: FAIL, undefined.

- [ ] **Step 3: Implement**

```go
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/ghapp"
	"github.com/seanmcgary/agent-utils/internal/identity"
	"github.com/seanmcgary/agent-utils/internal/settings"
	"github.com/urfave/cli/v3"
)

// dispatchRepo is the repository the helpers serve. It comes from the
// dispatch's environment and never from a flag, so the shim and the
// credential helper cannot be pointed at another repository the app is
// installed on by an argument the agent writes. This is a speed bump, not a
// boundary: see the Security section of the README.
func dispatchRepo() (string, error) {
	r := strings.TrimSpace(os.Getenv(identity.RepoEnv))
	if r == "" {
		return "", fmt.Errorf("%s is not set; this command runs inside an agent dispatch", identity.RepoEnv)
	}
	return r, nil
}

func appToken(ctx context.Context) (string, error) {
	repo, err := dispatchRepo()
	if err != nil {
		return "", err
	}
	s, err := settings.Load()
	if err != nil {
		return "", err
	}
	app, err := ghapp.FromSettings(s)
	if err != nil {
		return "", err
	}
	if app == nil {
		return "", errors.New("no github_app is configured")
	}
	return app.Token(ctx, repo)
}

func ghTokenCommand() *cli.Command {
	return &cli.Command{
		Name:  "gh-token",
		Usage: "print an installation token for the dispatch's repository",
		Action: func(ctx context.Context, c *cli.Command) error {
			tok, err := appToken(ctx)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(c.Root().Writer, tok)
			return err
		},
	}
}

func gitCredentialCommand() *cli.Command {
	return &cli.Command{
		Name:      "git-credential",
		Usage:     "git credential helper for the dispatch's repository",
		Arguments: []cli.Argument{&cli.StringArg{Name: "operation"}},
		Action: func(ctx context.Context, c *cli.Command) error {
			return credentialResponse(os.Stdin, c.Root().Writer, c.StringArg("operation"),
				func() (string, error) { return appToken(ctx) })
		},
	}
}

// credentialResponse implements git's credential helper protocol. Only "get"
// for https://github.com answers; "store" and "erase" are ignored, because
// the cache is ghapp's and a token git saw rejected is one minutes from
// expiry that the next get replaces anyway.
func credentialResponse(in io.Reader, out io.Writer, op string, token func() (string, error)) error {
	if op != "get" {
		_, _ = io.Copy(io.Discard, in)
		return nil
	}
	attrs := map[string]string{}
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			attrs[k] = v
		}
	}
	if attrs["protocol"] != "https" || attrs["host"] != "github.com" {
		return nil
	}
	tok, err := token()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "username=x-access-token\npassword=%s\n", tok)
	return err
}
```

In `internalCommand()`'s `Commands` slice, after the `run-agent` entry, add `ghTokenCommand(), gitCredentialCommand(),`.

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/agent-utils -run 'CredentialResponse|DispatchRepo' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/agent-utils/internal_auth.go cmd/agent-utils/internal_auth_test.go cmd/agent-utils/main.go
git commit -m "feat(cli): internal gh-token and git-credential helpers"
```

---

### Task 9: Wire the tick and the runner

**Files:**
- Modify: `internal/worktree/worktree.go` (`Manager` ~line 18, `runCtx` ~455, `gitOutput` ~489)
- Modify: `internal/runner/args.go` (`Invocation`), `internal/runner/runner.go` (`Supervise` ~line 159)
- Modify: `internal/loopcmd/open.go` (`Options`, `Open` ~line 145-230), `internal/loopcmd/tick.go` (`Deps` ~66, `dispatch` ~694, `RunAgent` ~970-1040)
- Test: `internal/worktree/env_test.go`, `internal/runner/runner_test.go`, `internal/loopcmd/coauthor_test.go`

**Interfaces:**
- Consumes: Tasks 3-7.
- Produces:
  - `worktree.Manager.Env []string`
  - `runner.Invocation.Env []string`
  - `loopcmd.Options.App *ghapp.App`, `loopcmd.Options.Authors ghub.AuthorReader`
  - `loopcmd.Deps.App *ghapp.App`, `Deps.Authors ghub.AuthorReader`, `Deps.CoAuthor bool`
  - `func resolveCoAuthor(ctx context.Context, cfg *config.Config, deps Deps, number int) string`

- [ ] **Step 1: Write the failing tests**

`internal/worktree/env_test.go`:

```go
func TestManagerEnvReachesGit(t *testing.T) {
	// Reuse this package's existing fixture for a primary checkout with an
	// origin (grep worktree_test.go for the helper that builds one).
	m, path := newManagerWithIssueWorktree(t)
	m.Env = []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=acme-agent[bot]"}
	got, err := m.gitStdout(context.Background(), path, "config", "user.name")
	if err != nil || strings.TrimSpace(got) != "acme-agent[bot]" {
		t.Errorf("user.name = %q, %v", got, err)
	}
	out, err := m.gitOutput(path, "config", "user.name")
	if err != nil || strings.TrimSpace(out) != "acme-agent[bot]" {
		t.Errorf("gitOutput path: user.name = %q, %v", out, err)
	}
}
```

`internal/runner/runner_test.go`: find the existing test that runs `Supervise` against a fake `claude` on PATH that dumps its environment (grep for `agentEnv` in the tests). Add:

```go
func TestSuperviseAppendsInvocationEnv(t *testing.T) {
	// Same setup as the existing env-dump test, with:
	inv.Env = []string{"GIT_CONFIG_COUNT=0", "AGENT_UTILS_DISPATCH_REPO=acme/widgets"}
	// ... run Supervise ...
	// Assert the dumped environment contains both, and still lacks GITHUB_TOKEN.
}

func TestSuperviseEnvUnchangedWithoutIdentity(t *testing.T) {
	// With inv.Env nil, the dumped environment equals agentEnv() + claudeEnv(cfg)
	// exactly -- the default-mode regression pin from the Global Constraints.
}
```

Write these against the real helpers in that file; the assertion bodies are the two comments above.

`internal/loopcmd/coauthor_test.go`:

```go
type fakeAuthors struct {
	v   string
	err error
	n   int
}

func (f *fakeAuthors) CoAuthor(context.Context, string, string, int) (string, error) {
	f.n++
	return f.v, f.err
}

func TestResolveCoAuthor(t *testing.T) {
	cfg := &config.Config{Repo: "acme/widgets"}
	a := &fakeAuthors{v: "Sean McGary <sean@seanmcgary.com>"}

	if got := resolveCoAuthor(context.Background(), cfg, Deps{Authors: a}, 5); got != "" || a.n != 0 {
		t.Errorf("setting off: %q, %d calls", got, a.n)
	}
	if got := resolveCoAuthor(context.Background(), cfg, Deps{Authors: a, CoAuthor: true}, 5); got != a.v {
		t.Errorf("setting on: %q", got)
	}
	a.err = errors.New("network")
	if got := resolveCoAuthor(context.Background(), cfg, Deps{Authors: a, CoAuthor: true}, 5); got != "" {
		t.Errorf("lookup failure must yield no co-author, got %q", got)
	}
}
```

Then extend the existing `dispatch` test in `tick_test.go` (the one asserting the created dispatch row's fields) to set `deps.CoAuthor = true`, `deps.Authors = &fakeAuthors{v: "X <x@y.z>"}` and assert the stored row's `CoAuthor == "X <x@y.z>"`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/worktree ./internal/runner ./internal/loopcmd -run 'ManagerEnv|InvocationEnv|EnvUnchanged|CoAuthor' -v`
Expected: FAIL, unknown fields.

- [ ] **Step 3: Implement worktree**

Add to `Manager`:

```go
	// Env is added to every git command this Manager runs. In app mode it is
	// identity.GitEnv: the app's credential helper and bot identity, so
	// fetches and the automatic rebase's push act as the app.
	Env []string
```

In `runCtx` and `gitOutput`, after `cmd.Dir = dir`:

```go
	if len(m.Env) > 0 {
		cmd.Env = append(os.Environ(), m.Env...)
	}
```

- [ ] **Step 4: Implement runner**

`args.go`, add to `Invocation`:

```go
	// Env is added to the agent's environment after agentEnv and the
	// harness's own, so it wins any repeated key. It carries the dispatch's
	// git and gh identity; see internal/identity.
	Env []string
```

`runner.go` `Supervise`: replace `cmd.Env = append(agentEnv(), extraEnv...)` with:

```go
	cmd.Env = append(append(agentEnv(), extraEnv...), inv.Env...)
```

- [ ] **Step 5: Implement loopcmd.Open**

In `Options` add:

```go
	// Authors resolves co-authors. Nil falls back to GH when GH implements
	// it; the daemon passes it explicitly, as it does Epic, because the
	// DeliveryCache wrapper does not.
	Authors ghub.AuthorReader
```

In `Deps` add:

```go
	// App is the GitHub App this machine acts as, or nil in token mode.
	App *ghapp.App
	// Authors and CoAuthor drive commit.co_author_issue_author: the tick
	// resolves the issue author through Authors when CoAuthor is set.
	Authors  ghub.AuthorReader
	CoAuthor bool
```

In `Open`, replace the token block (the lines from `token := opts.Token` through `gh = ghub.New(token)` and its `}`) with:

```go
	s, err := settings.Load()
	if err != nil {
		return nil, Deps{}, nil, err
	}
	// A configured app is used wherever a token would have been, and it is
	// an error -- never a fallback to GITHUB_TOKEN -- for it to be unusable.
	app, err := ghapp.FromSettings(s)
	if err != nil {
		return nil, Deps{}, nil, err
	}

	// The token must come from the environment, never a flag. [keep the
	// existing comment's remaining text verbatim]
	token := opts.Token
	if token == "" && app == nil && opts.RequireGitHub && opts.GH == nil {
		return nil, Deps{}, nil, fmt.Errorf("GITHUB_TOKEN is not set and no github_app is configured")
	}

	gh := opts.GH
	if gh == nil {
		if app != nil {
			gh = ghub.NewApp(app)
		} else {
			gh = ghub.New(token)
		}
	}
	authors := opts.Authors
	if authors == nil {
		if ar, ok := gh.(ghub.AuthorReader); ok {
			authors = ar
		}
	}
```

After `wt := worktree.New(...)`:

```go
	if app != nil {
		bot, err := app.Bot(context.Background(), cfg.Repo)
		if err != nil {
			db.Close()
			return nil, Deps{}, nil, fmt.Errorf("resolve the GitHub App's bot identity for %s: %w", cfg.Repo, err)
		}
		wt.Env = identity.GitEnv(self, os.Getenv(home.EnvVar), cfg.Repo, bot)
	}
```

In the `deps := Deps{...}` literal add `App: app, Authors: authors, CoAuthor: s.Commit.CoAuthorIssueAuthor,`.

Grep for tests that construct the error string `"GITHUB_TOKEN is not set"` from `Open` and update them to the new text.

- [ ] **Step 6: Implement the tick side**

In `tick.go` add:

```go
// resolveCoAuthor returns the co-author for issue number's commits, or "".
//
// It runs in the tick because the detached runner has no GitHub client; the
// value reaches the runner on the dispatch row. A lookup failure is a
// warning, never a failed dispatch: the commit is still correct without the
// trailer, and a flaky profile read must not stall an issue.
func resolveCoAuthor(ctx context.Context, cfg *config.Config, deps Deps, number int) string {
	if !deps.CoAuthor || deps.Authors == nil {
		return ""
	}
	owner, repo, ok := strings.Cut(cfg.Repo, "/")
	if !ok {
		return ""
	}
	v, err := deps.Authors.CoAuthor(ctx, owner, repo, number)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s#%d: no co-author: %v\n", cfg.Repo, number, err)
		return ""
	}
	return v
}
```

In `dispatch`, in the `store.Dispatch{...}` passed to `CreateDispatch`, add:

```go
		// The issue's author for the commit hook. A tend's Number is its
		// issue too, so a fix pushed during a tend credits the same person.
		CoAuthor: resolveCoAuthor(ctx, cfg, deps, d.Issue),
```

In `RunAgent`, after the `prompt, err := runner.RenderPrompt(...)` error block and before `return runner.Supervise(...)`:

```go
	idEnv, idDir, err := prepareIdentity(ctx, cfg, deps, d, workDir)
	if err != nil {
		_ = runner.Finish(cfg, deps.Store, d, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1, APIError: err.Error(),
		}, deps.Now())
		return err
	}
	if idDir != "" {
		defer os.RemoveAll(idDir)
	}
```

and pass `Env: idEnv,` in the `runner.Invocation{...}` literal. Add:

```go
// prepareIdentity builds the dispatch's identity directory and environment.
// It returns no directory, and no environment, when neither the app nor the
// co-author setting applies.
func prepareIdentity(ctx context.Context, cfg *config.Config, deps Deps, d store.Dispatch, workDir string) ([]string, string, error) {
	if deps.App == nil && d.CoAuthor == "" {
		return nil, "", nil
	}
	dir, err := home.Dir()
	if err != nil {
		return nil, "", err
	}
	p := identity.Plan{
		Dir:      filepath.Join(dir, "run", strconv.FormatInt(d.ID, 10)),
		WorkDir:  workDir,
		Repo:     cfg.Repo,
		SelfPath: deps.SelfPath,
		Home:     os.Getenv(home.EnvVar),
		CoAuthor: d.CoAuthor,
	}
	// A leftover directory from a runner that died is this dispatch id's
	// own, and stale; start clean.
	_ = os.RemoveAll(p.Dir)
	if deps.App != nil {
		bot, err := deps.App.Bot(ctx, cfg.Repo)
		if err != nil {
			return nil, "", fmt.Errorf("resolve the GitHub App's bot identity: %w", err)
		}
		p.Bot = &bot
	}
	env, err := identity.Prepare(ctx, p)
	if err != nil {
		_ = os.RemoveAll(p.Dir)
		return nil, "", fmt.Errorf("prepare dispatch identity: %w", err)
	}
	return env, p.Dir, nil
}
```

Add the needed imports (`ghapp`, `identity`, `home`, `settings`, `strconv`, `strings`, `path/filepath`) to each file.

- [ ] **Step 7: Run tests**

Run: `go test ./internal/worktree ./internal/runner ./internal/loopcmd -race`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/worktree internal/runner internal/loopcmd
git commit -m "feat(loopcmd): run dispatches as the GitHub App and credit the issue author"
```

---

### Task 10: Listener, project and CLI wiring

**Files:**
- Modify: `internal/listener/work.go` (`Worker` ~198, `access` ~680-715, the four `loopcmd.Options{...}` literals at ~746, ~1161, ~1250, ~1393)
- Modify: `internal/listener/poll.go` (~line 60)
- Modify: `cmd/agent-utils/listener.go` (`ensureToken` ~443 and its caller; worker construction)
- Modify: `cmd/agent-utils/project.go` (~592, ~713, ~997, ~1044)
- Test: `internal/listener/work_test.go`, `cmd/agent-utils/listener_test.go`, `cmd/agent-utils/project_test.go`

**Interfaces:**
- Consumes: `ghub.NewApp`, `ghub.AuthorReader`, `ghapp.FromSettings`, `loopcmd.Options.Authors`.
- Produces: `listener.Worker.App *ghapp.App`; `func configuredApp() (*ghapp.App, error)` in `cmd/agent-utils/listener.go`; `registerWebhookDeps.App bool`.

- [ ] **Step 1: Write the failing tests**

In `internal/listener/work_test.go`:

```go
func TestAccessUsesAppWithoutToken(t *testing.T) {
	w := NewWorker(nil)
	w.Token = func() (string, error) { return "", errors.New("no env file") }
	w.App = testApp(t) // a *ghapp.App built against an httptest server; see below
	acc, err := w.access()
	if err != nil {
		t.Fatalf("app mode must not need a token: %v", err)
	}
	if acc.gh == nil || acc.epic == nil || acc.authors == nil {
		t.Errorf("access = %+v; want gh, epic and authors set", acc)
	}
}
```

`testApp` writes a key with the same code as `ghapp`'s `writeKey` test helper (copy it; it is ten lines) and calls `ghapp.New(7, keyPath, t.TempDir())`. No request is made by `access`.

In `cmd/agent-utils/listener_test.go`, add a case to the existing `ensureToken` tests: with `AGENT_UTILS_HOME` pointing at a temp dir whose `config.yaml` has a valid `github_app` block and key, and no env file, `ensureToken(nil, io.Discard, false)` returns nil.

In `cmd/agent-utils/project_test.go`, add a case to the `registerWebhookRun` tests: `deps.Token == ""` with `deps.App = true` (see Step 3) proceeds past the token check.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/listener ./cmd/agent-utils -run 'AppWithoutToken|ensureToken|registerWebhook' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

`work.go`:
- `Worker`: add
  ```go
	// App, when set, authenticates every client this worker builds as the
	// GitHub App; Token is then never read.
	App *ghapp.App
  ```
- `access` struct: add `authors ghub.AuthorReader`.
- `access()`:
  ```go
  func (w *Worker) access() (*access, error) {
  	var c ghub.Client
  	var tok string
  	if w.App != nil {
  		c = ghub.NewApp(w.App)
  	} else {
  		var err error
  		if tok, err = w.Token(); err != nil {
  			return nil, err
  		}
  		c = w.NewClient(tok)
  	}
  	acc := &access{token: tok, gh: ghub.NewDeliveryCache(c)}
  	if er, ok := c.(ghub.EpicReader); ok {
  		acc.epic = er
  	}
  	if ar, ok := c.(ghub.AuthorReader); ok {
  		acc.authors = ar
  	}
  	return acc, nil
  }
  ```
- In each `loopcmd.Options{...}` literal that sets `GH: acc.gh`, add `Authors: acc.authors,`.

`poll.go` (~60):

```go
	var src PollSource
	if w.App != nil {
		src = ghub.NewApp(w.App)
	} else {
		token, err := w.Token()
		if err != nil {
			slog.Error("cannot read the github token to poll", "err", err)
			return
		}
		src = w.NewPollSource(token)
	}
```

`cmd/agent-utils/listener.go`: add a helper and use it.

```go
// configuredApp returns the machine's GitHub App, or nil in token mode.
func configuredApp() (*ghapp.App, error) {
	s, err := settings.Load()
	if err != nil {
		return nil, err
	}
	return ghapp.FromSettings(s)
}
```

At the top of `ensureToken`:

```go
	// A GitHub App replaces the token entirely; nothing to ask for.
	if app, err := configuredApp(); err != nil {
		return err
	} else if app != nil {
		return nil
	}
```

Where `listener run`/`listener poll` build the worker with `listener.NewWorker(db)`, add:

```go
	if w.App, err = configuredApp(); err != nil {
		return err
	}
```

`cmd/agent-utils/project.go`: the two `registerWebhookRun` call sites build `Hooks: ghub.New(token)` and pass `Token: token`. Replace with:

```go
	app, err := configuredApp()
	if err != nil {
		return err
	}
	token := os.Getenv("GITHUB_TOKEN")
	hooks := ghub.New(token)
	if app != nil {
		hooks = ghub.NewApp(app)
	}
	return registerWebhookRun(ctx, repos, registerWebhookDeps{
		// ...unchanged fields...
		Hooks: hooks,
		Token: token,
		App:   app != nil,
	})
```

Add `App bool` to `registerWebhookDeps` with a comment (a GitHub App authenticates the hook calls; it needs the "Webhooks: read and write" repository permission). Change both `if deps.Token == "" {` checks (~713, ~1044) to `if deps.Token == "" && !deps.App {` and the message to `"GITHUB_TOKEN is not set and no github_app is configured"`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/listener ./cmd/agent-utils -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/listener cmd/agent-utils/listener.go cmd/agent-utils/project.go cmd/agent-utils/*_test.go
git commit -m "feat(listener): authenticate the daemon as the GitHub App when one is configured"
```

---

### Task 11: `agent-utils config github-app`

**Files:**
- Create: `cmd/agent-utils/config_app.go`
- Modify: `cmd/agent-utils/config.go` (add `githubAppCommand()` to the `config` command's `Commands`)
- Test: `cmd/agent-utils/config_app_test.go`

**Interfaces:**
- Consumes: `ghapp.New`, `App.Slug`, `settings.Load/Save`.
- Produces: `func githubAppCommand() *cli.Command`, testable core `func configureGitHubApp(ctx context.Context, out io.Writer, id int64, keyPath string, verify func(*ghapp.App) (string, error)) error`.

- [ ] **Step 1: Write the failing test**

```go
func TestConfigureGitHubApp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_UTILS_HOME", dir)
	keyPath := writeTestKey(t, dir) // same ten lines as ghapp's writeKey
	var out bytes.Buffer
	verify := func(*ghapp.App) (string, error) { return "acme-agent", nil }
	if err := configureGitHubApp(context.Background(), &out, 7, keyPath, verify); err != nil {
		t.Fatal(err)
	}
	s, _ := settings.Load()
	if s.GitHubApp.AppID != 7 || s.GitHubApp.PrivateKeyPath != keyPath {
		t.Errorf("saved %+v", s.GitHubApp)
	}
	if !strings.Contains(out.String(), "acme-agent[bot]") {
		t.Errorf("output %q does not name the bot", out.String())
	}

	failing := func(*ghapp.App) (string, error) { return "", errors.New("401 Unauthorized") }
	if err := configureGitHubApp(context.Background(), &out, 8, keyPath, failing); err == nil {
		t.Error("a key GitHub rejects must not be saved")
	}
	if s, _ := settings.Load(); s.GitHubApp.AppID != 7 {
		t.Error("failed verification overwrote the saved app")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/agent-utils -run ConfigureGitHubApp -v`
Expected: FAIL, undefined.

- [ ] **Step 3: Implement**

```go
package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/seanmcgary/agent-utils/internal/ghapp"
	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/settings"
	"github.com/urfave/cli/v3"
)

func githubAppCommand() *cli.Command {
	return &cli.Command{
		Name:  "github-app",
		Usage: "act as a GitHub App instead of a personal token",
		Description: "Stores the app id and private key path in the machine-wide settings, " +
			"after checking that GitHub accepts the key. Every daemon call, agent gh call, " +
			"push and commit then acts as the app's bot.",
		Flags: []cli.Flag{
			&cli.Int64Flag{Name: "app-id", Usage: "the GitHub App's id"},
			&cli.StringFlag{Name: "private-key", Usage: "path to the app's private key (.pem), mode 0600"},
			&cli.BoolFlag{Name: "unset", Usage: "stop acting as the app"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			out := c.Root().Writer
			if c.Bool("unset") {
				s, err := settings.Load()
				if err != nil {
					return err
				}
				s.GitHubApp = settings.GitHubApp{}
				if err := settings.Save(s); err != nil {
					return err
				}
				_, _ = fmt.Fprintln(out, "GitHub App removed; GITHUB_TOKEN is used again.")
				return nil
			}
			if c.Int64("app-id") <= 0 || c.String("private-key") == "" {
				return fmt.Errorf("--app-id and --private-key are required")
			}
			keyPath, err := filepath.Abs(c.String("private-key"))
			if err != nil {
				return err
			}
			return configureGitHubApp(ctx, out, c.Int64("app-id"), keyPath,
				func(a *ghapp.App) (string, error) { return a.Slug(ctx) })
		},
	}
}

// configureGitHubApp saves the app only after GitHub has accepted a JWT
// signed with its key, so a wrong id or key fails here rather than on the
// first tick of a detached daemon.
func configureGitHubApp(ctx context.Context, out io.Writer, id int64, keyPath string, verify func(*ghapp.App) (string, error)) error {
	dir, err := home.EnsureDir()
	if err != nil {
		return err
	}
	app, err := ghapp.New(id, keyPath, dir)
	if err != nil {
		return err
	}
	slug, err := verify(app)
	if err != nil {
		return fmt.Errorf("GitHub rejected app %d with this key: %w", id, err)
	}
	s, err := settings.Load()
	if err != nil {
		return err
	}
	s.GitHubApp = settings.GitHubApp{AppID: id, PrivateKeyPath: keyPath}
	if err := settings.Save(s); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Acting as GitHub App %s; commits and comments will be by %s[bot].\n", slug, slug)
	return nil
}
```

Register `githubAppCommand()` in `config.go`'s `Commands` beside `webhook`.

- [ ] **Step 4: Run tests**

Run: `go test ./cmd/agent-utils -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/agent-utils/config_app.go cmd/agent-utils/config_app_test.go cmd/agent-utils/config.go
git commit -m "feat(cli): config github-app"
```

---

### Task 12: Docs

**Files:**
- Modify: `docs/configuration.md` (machine-wide settings section ~line 90-130)
- Modify: `README.md` (Security ~line 540-580; Webhooks/Cron token mentions ~453, ~631, ~830, ~1008)
- Test: `internal/config/docs_test.go` (existing; it may check documented keys)

- [ ] **Step 1: Write `docs/configuration.md` section "Running as a GitHub App"**

Cover, in this repository's documentation voice:
- Creating the app: repository permissions Contents (read/write), Issues (read/write), Pull requests (read/write), Metadata (read), and Webhooks (read/write) only if `project init` registers repository webhooks with it. No user permissions. Install it on the loop repositories only.
- `agent-utils config github-app --app-id N --private-key /abs/app.pem`, `--unset`; the key must be 0600 and owned by the daemon's user.
- What changes: daemon calls, agent `gh`, pushes (SSH remotes are rewritten to HTTPS for the agent and the automatic rebase), commits authored and committed by `<slug>[bot]`. `GITHUB_TOKEN` is no longer required; when both are present the app wins.
- Token mechanics: repo-scoped installation tokens in `~/.agent-utils/tokens/`, refreshed 10 minutes before expiry; the agent's environment never holds one.
- `commit.co_author_issue_author`: `agent-utils config set commit.co_author_issue_author true`; the trailer format; public email vs noreply; bots skipped; works in token mode too; implemented as a hook, so the agent cannot drop it by ignoring a prompt.
- The known gap: tools that run git with a scrubbed environment get the host's identity and no trailer.

- [ ] **Step 2: Update README**

- Security section: replace the first bullet's "`GITHUB_TOKEN` is removed" framing with both modes. Token mode: unchanged text. App mode: the agent can mint a token for its repository through the helpers, and since the key is readable by the same uid it can mint for any repository the app is installed on; install the app narrowly and run the daemon as a dedicated user for a real boundary.
- Each "needs `GITHUB_TOKEN`" sentence gains "(or a GitHub App, see docs/configuration.md#running-as-a-github-app)".

- [ ] **Step 3: Run the docs tests and lint**

Run: `go test ./internal/config -run Docs -v && make lint`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add README.md docs/configuration.md
git commit -m "docs: running as a GitHub App, issue-author co-authoring"
```

---

### Task 13: Full verification

- [ ] **Step 1: Run everything**

Run: `make lint && go test -race ./...`
Expected: PASS.

- [ ] **Step 2: Default-mode check**

Run: `grep -rn 'os.Getenv("GITHUB_TOKEN")' cmd internal | grep -v _test`
Expected: only the existing call sites, each now reached only when no app is configured (`Open` ignores the token when `app != nil`; `project.go` and the listener branch on `app`).

- [ ] **Step 3: Manual smoke test against a real app (operator-run)**

On a scratch repository with the app installed: `agent-utils config github-app ...`, `agent-utils config set commit.co_author_issue_author true`, label an issue opened by a human, and confirm on GitHub that the issue comments, labels, pushed commits and pull request are by `<slug>[bot]` and each commit carries `Co-authored-by:` for the issue's author.
