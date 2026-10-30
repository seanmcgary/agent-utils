// Package ghapp authenticates as a GitHub App.
//
// An installation token expires an hour after it is minted and cannot be
// refreshed; a new one is minted by signing a JWT with the app's private key.
// A dispatch can run for a day, and the agent's gh and git each run as fresh
// processes, so tokens are cached ON DISK, per app and repository, and every
// process that needs one -- the daemon, a tick, the gh shim, the git
// credential helper -- reads the same file and mints only when it is close to
// expiry.
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
	"strconv"
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

// httpTimeout bounds each request New's client makes; see New.
const httpTimeout = 30 * time.Second

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
		// A mint runs under the repository's token-cache lock, and every gh
		// shim and credential helper call for that repository queues on it.
		// http.DefaultClient never times out, so one hung GitHub response
		// would stall all of them for good.
		ID: id, BaseURL: "https://api.github.com", HTTP: &http.Client{Timeout: httpTimeout}, Now: time.Now,
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

// tokenPath is keyed by app id as well as repository: after the operator
// switches apps, a token the old app minted must not be handed out as the
// new app's for the rest of its hour.
func (a *App) tokenPath(owner, name string) string {
	return filepath.Join(a.dir, "tokens", strconv.FormatInt(a.ID, 10), owner, name+".json")
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
	var out struct {
		ID int64 `json:"id"`
	}
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

// Verify asks GitHub for the app with a freshly signed JWT and returns its
// slug. Unlike Slug it never answers from the cache: a cached slug says
// nothing about whether the key in hand is still accepted, and checking a
// new or rotated key is the whole point. On success the cached slug is
// refreshed; the cached bot user id is kept only if the slug is unchanged.
func (a *App) Verify(ctx context.Context) (string, error) {
	var out struct {
		Slug string `json:"slug"`
	}
	if _, err := a.jwtCall(ctx, "GET", "/app", nil, &out); err != nil {
		return "", fmt.Errorf("read the GitHub App: %w", err)
	}
	if out.Slug == "" {
		return "", errors.New("read the GitHub App: GitHub returned no slug")
	}
	path := filepath.Join(a.dir, "tokens", fmt.Sprintf("bot-%d.json", a.ID))
	b, _ := readJSON[botFile](path)
	if b.Slug != out.Slug {
		b = botFile{Slug: out.Slug}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		_ = writeJSON(path, b)
	}
	return out.Slug, nil
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
		var out struct {
			Slug string `json:"slug"`
		}
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
		var out struct {
			ID int64 `json:"id"`
		}
		if _, err := a.call(ctx, "GET", "/users/"+url.PathEscape(login), "token "+tok, nil, &out); err != nil {
			return Identity{}, fmt.Errorf("read the app's bot user: %w", err)
		}
		if out.ID == 0 {
			// Without the id the noreply email is wrong, and every commit
			// made with it is attributed to no one.
			return Identity{}, errors.New("read the app's bot user: GitHub returned no id")
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
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write token cache: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write token cache: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write token cache: %w", err)
	}
	return nil
}
