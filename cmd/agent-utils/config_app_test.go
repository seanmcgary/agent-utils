package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/ghapp"
	"github.com/seanmcgary/agent-utils/internal/settings"
)

func writeTestKey(t *testing.T, dir string) string {
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
	return path
}

func TestConfigureGitHubApp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_UTILS_HOME", dir)
	keyPath := writeTestKey(t, dir)
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

func TestConfigShowSaysTheAppWinsOverGitHubToken(t *testing.T) {
	withHome(t)
	t.Setenv(githubTokenEnv, "ghp_env")
	if err := settings.Save(&settings.Settings{GitHubApp: settings.GitHubApp{AppID: 7, PrivateKeyPath: "/k.pem"}}); err != nil {
		t.Fatal(err)
	}
	out, _, err := runConfigCLI(t, "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "GitHub App") || !strings.Contains(out, githubTokenEnv+" is set but ignored") {
		t.Errorf("show output = %q", out)
	}

	t.Setenv(githubTokenEnv, "")
	out, _, _ = runConfigCLI(t, "config", "show")
	if !strings.Contains(out, "GitHub App") || strings.Contains(out, "ignored") {
		t.Errorf("show output without token = %q", out)
	}

	if err := settings.Save(&settings.Settings{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(githubTokenEnv, "ghp_env")
	out, _, _ = runConfigCLI(t, "config", "show")
	if strings.Contains(out, "acts as the GitHub App") || strings.Contains(out, "ignored") {
		t.Errorf("no app configured, yet show output = %q", out)
	}
}

func TestConfigureGitHubAppDoesNotSaveWhenVerifyIsRejectedDespiteCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_UTILS_HOME", dir)
	keyPath := writeTestKey(t, dir)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad jwt", http.StatusUnauthorized)
	}))
	defer srv.Close()
	// A slug cached by an earlier successful run must not stand in for GitHub.
	tokens := filepath.Join(dir, "tokens")
	if err := os.MkdirAll(tokens, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tokens, "bot-7.json"), []byte(`{"slug":"acme-agent"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	verify := func(a *ghapp.App) (string, error) {
		a.BaseURL = srv.URL
		return a.Verify(context.Background())
	}
	if err := configureGitHubApp(context.Background(), io.Discard, 7, keyPath, verify); err == nil {
		t.Fatal("a key GitHub rejects must not be saved")
	}
	if s, _ := settings.Load(); s.GitHubApp.AppID != 0 {
		t.Errorf("saved %+v", s.GitHubApp)
	}
}
