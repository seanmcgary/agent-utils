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
	appHits   atomic.Int32
	installed map[string]int64 // "owner/name" -> installation id
	expiry    time.Duration
	mu        sync.Mutex
	scopes    [][]string // repositories requested per mint
	redirect  string     // target of GET /repos/{owner}/{name}/redirect
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
			http.Error(w, "bad jwt", http.StatusUnauthorized)
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
			http.Error(w, "bad jwt", http.StatusUnauthorized)
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
		f.appHits.Add(1)
		if _, ok := f.verifyJWT(r); !ok {
			http.Error(w, "bad jwt", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"slug":"acme-agent"}`)
	})
	mux.HandleFunc("GET /users/{login}", func(w http.ResponseWriter, r *http.Request) {
		// GitHub accepts "token" and "Bearer" for an installation token.
		if !strings.Contains(r.Header.Get("Authorization"), "ghs_") {
			http.Error(w, "want installation token", http.StatusUnauthorized)
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
	mux.HandleFunc("GET /repos/{owner}/{name}/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, f.redirect, http.StatusFound)
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
