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
