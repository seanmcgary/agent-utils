package settings

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// every test points $AGENT_UTILS_HOME at its own temp directory, so tests
// cannot see each other's files and cannot touch the real machine's
// ~/.agent-utils.
func withHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_UTILS_HOME", dir)
	return dir
}

func TestLoadWithNoFileReturnsZeroValue(t *testing.T) {
	withHome(t)

	s, err := Load()
	if err != nil {
		t.Fatalf("Load on a fresh home must not error: %v", err)
	}
	if s == nil {
		t.Fatal("Load returned a nil *Settings")
	}
	if *s != (Settings{}) {
		t.Errorf("Load = %+v, want the zero value", *s)
	}
}

func TestSaveThenLoadRoundTripsEveryField(t *testing.T) {
	withHome(t)

	want := &Settings{Webhook: Webhook{
		Enabled:    true,
		URL:        "https://example.com/hooks/agent-utils",
		ListenAddr: "0.0.0.0",
		ListenPort: 9999,
		Secret:     "supersecretvalue",
	}}
	if err := Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if *got != *want {
		t.Errorf("round trip = %+v, want %+v", *got, *want)
	}
}

func TestSavedFileModeIsExactly0600(t *testing.T) {
	home := withHome(t)

	if err := Save(&Settings{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(filepath.Join(home, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %#o, want 0600", perm)
	}
}

// A leftover 0644 temp file from a previous crash must not leak its mode into
// the file Save produces: os.WriteFile(path+".tmp", ...), which registry.go
// uses, ignores the mode argument when the target already exists, so a stale
// world-readable "config.yaml.tmp" would silently publish the HMAC secret.
// Save must use a fresh, randomly-named temp file instead.
func TestPreexistingWorldReadableTmpFileDoesNotLeakItsMode(t *testing.T) {
	home := withHome(t)

	stale := filepath.Join(home, FileName+".tmp")
	if err := os.WriteFile(stale, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Save(&Settings{Webhook: Webhook{Secret: "topsecret"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(filepath.Join(home, FileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %#o, want 0600 despite the stale 0644 tmp file", perm)
	}
}

func TestLoadOnAProjectDescriptorReturnsTheNamedError(t *testing.T) {
	home := withHome(t)

	descriptor := "id: 11111111-1111-1111-1111-111111111111\nname: some-project\n"
	if err := os.WriteFile(filepath.Join(home, FileName), []byte(descriptor), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load()
	if !errors.Is(err, ErrProjectDescriptor) {
		t.Fatalf("Load err = %v, want ErrProjectDescriptor", err)
	}
}

func TestSaveOnAProjectDescriptorRefusesAndLeavesBytesUntouched(t *testing.T) {
	home := withHome(t)

	descriptor := "id: 11111111-1111-1111-1111-111111111111\nname: some-project\n"
	path := filepath.Join(home, FileName)
	if err := os.WriteFile(path, []byte(descriptor), 0o600); err != nil {
		t.Fatal(err)
	}

	err := Save(&Settings{Webhook: Webhook{Secret: "x"}})
	if !errors.Is(err, ErrProjectDescriptor) {
		t.Fatalf("Save err = %v, want ErrProjectDescriptor", err)
	}

	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != descriptor {
		t.Errorf("Save modified the file despite refusing it: %q", raw)
	}
}

func TestLoadOn0644FileGivesTheModeError(t *testing.T) {
	home := withHome(t)

	if err := os.WriteFile(filepath.Join(home, FileName), []byte("webhook:\n  enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load()
	if err == nil {
		t.Fatal("Load on a 0644 file must return an error")
	}
	if !strings.Contains(err.Error(), "0644") && !strings.Contains(err.Error(), "mode") {
		t.Errorf("err = %v, want it to mention the mode", err)
	}
}

func TestLoadWithAnUnknownKeyErrors(t *testing.T) {
	home := withHome(t)

	if err := os.WriteFile(filepath.Join(home, FileName), []byte("webhook:\n  bogus: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("Load with an unknown key must error")
	}
}

func TestGenerateSecretReturns64HexCharsAndDiffersEachCall(t *testing.T) {
	a, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if len(a) != 64 {
		t.Errorf("len = %d, want 64", len(a))
	}
	for _, r := range a {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("secret contains non-hex rune %q: %s", r, a)
		}
	}

	b, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	if a == b {
		t.Error("two calls to GenerateSecret produced the same value")
	}
}

func TestWithDefaultsFillsUnsetFieldsOnly(t *testing.T) {
	var zero Settings
	got := zero.WithDefaults()
	if got.Webhook.ListenAddr != "127.0.0.1" {
		t.Errorf("ListenAddr = %q, want 127.0.0.1", got.Webhook.ListenAddr)
	}
	if got.Webhook.ListenPort != 8787 {
		t.Errorf("ListenPort = %d, want 8787", got.Webhook.ListenPort)
	}

	custom := Settings{Webhook: Webhook{ListenAddr: "10.0.0.1", ListenPort: 1234}}
	got = custom.WithDefaults()
	if got.Webhook.ListenAddr != "10.0.0.1" || got.Webhook.ListenPort != 1234 {
		t.Errorf("WithDefaults overwrote a set value: %+v", got)
	}
}

func TestWebhookURLValidation(t *testing.T) {
	field, ok := FieldFor("webhook.url")
	if !ok {
		t.Fatal(`FieldFor("webhook.url") not found`)
	}

	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"https accepted", "https://h/p", false},
		{"http rejected for non-loopback host", "http://example.com/p", true},
		{"http allowed for loopback", "http://127.0.0.1:8787/p", false},
		{"not a url", "notaurl", true},
		{"schemeless value", "example.com/p", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &Settings{}
			err := field.Set(s, c.value)
			if c.wantErr && err == nil {
				t.Errorf("Set(%q) = nil error, want an error", c.value)
			}
			if !c.wantErr && err != nil {
				t.Errorf("Set(%q) = %v, want no error", c.value, err)
			}
		})
	}
}

func TestWebhookListenPortValidation(t *testing.T) {
	field, ok := FieldFor("webhook.listen_port")
	if !ok {
		t.Fatal(`FieldFor("webhook.listen_port") not found`)
	}

	cases := []struct {
		value   string
		wantErr bool
	}{
		{"0", true},
		{"70000", true},
		{"abc", true},
		{"8787", false},
	}
	for _, c := range cases {
		t.Run(c.value, func(t *testing.T) {
			s := &Settings{}
			err := field.Set(s, c.value)
			if c.wantErr && err == nil {
				t.Errorf("Set(%q) = nil error, want an error", c.value)
			}
			if !c.wantErr && err != nil {
				t.Errorf("Set(%q) = %v, want no error", c.value, err)
			}
		})
	}
}

func TestWebhookSecretHasNoSetter(t *testing.T) {
	field, ok := FieldFor("webhook.secret")
	if !ok {
		t.Fatal(`FieldFor("webhook.secret") not found`)
	}
	if field.Set != nil {
		t.Error("webhook.secret must have a nil Set: it is only reachable through --rotate-secret")
	}
	if !field.Secret {
		t.Error("webhook.secret must be marked Secret so Render redacts it")
	}
}

func TestRenderRedactsTheSecretUnlessRevealed(t *testing.T) {
	s := &Settings{Webhook: Webhook{Secret: "topsecretvalue"}}

	redacted, err := Render(s, false)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(redacted, Redacted) {
		t.Errorf("redacted render does not contain %q:\n%s", Redacted, redacted)
	}
	if strings.Contains(redacted, "topsecretvalue") {
		t.Errorf("redacted render leaks the secret:\n%s", redacted)
	}

	revealed, err := Render(s, true)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(revealed, "topsecretvalue") {
		t.Errorf("revealed render is missing the secret:\n%s", revealed)
	}
}

// An empty file used to return the zero value before the mode was ever
// checked, so a 0644 config.yaml stayed unreported until something wrote a
// secret into it -- at which point it was already world-readable.
func TestLoadOnAnEmpty0644FileStillGivesTheModeError(t *testing.T) {
	home := withHome(t)
	if err := os.WriteFile(filepath.Join(home, FileName), []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("Load on an empty 0644 file must return an error")
	}
}

// This file holds the sole authenticator for an endpoint that starts a coding
// agent. A symlink at its path would have Load read, and later Save replace,
// a file this process does not own; the token file is defended the same way
// (internal/listener/env.go).
func TestLoadRefusesToFollowASymlink(t *testing.T) {
	home := withHome(t)

	target := filepath.Join(t.TempDir(), "elsewhere.yaml")
	if err := os.WriteFile(target, []byte("webhook:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, FileName)); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("Load followed a symlink at the settings path")
	}
}

// A FIFO is not a settings file. Without the regular-file check an open of one
// reads EOF and Load would report an empty configuration -- silently, and with
// an unauthenticated listener as the consequence.
func TestLoadRefusesANonRegularFile(t *testing.T) {
	home := withHome(t)
	path := filepath.Join(home, FileName)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo is unavailable here: %v", err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a FIFO at the settings path")
	}
}

// captureStderr swaps os.Stderr for a pipe, runs fn, and returns what fn
// wrote. The warning under test is written to the real os.Stderr at call time,
// which is the only thing an operator at a terminal sees.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var buf strings.Builder
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	fn()

	os.Stderr = orig
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out := <-done
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Widening the bind address is the one reach-granting step in this program
// that is not gated by a confirmation, an acknowledgement or a refusal, and it
// is the largest: the endpoint behind it starts a coding agent with permission
// prompts disabled. It is ungated on purpose -- a reverse proxy on another
// host is legitimate -- so the operator must at least be told.
func TestSettingANonLoopbackListenAddrWarns(t *testing.T) {
	f, ok := FieldFor("webhook.listen_addr")
	if !ok {
		t.Fatal("webhook.listen_addr is not a settable field")
	}

	var s Settings
	out := captureStderr(t, func() {
		if err := f.Set(&s, "0.0.0.0"); err != nil {
			t.Errorf("Set: %v", err)
		}
	})
	if !strings.Contains(out, "0.0.0.0") || !strings.Contains(strings.ToLower(out), "warning") {
		t.Errorf("stderr = %q, want a warning naming the address", out)
	}
	if s.Webhook.ListenAddr != "0.0.0.0" {
		t.Errorf("ListenAddr = %q; the warning must not block the change", s.Webhook.ListenAddr)
	}

	// Loopback is the default and must stay silent, or the warning becomes
	// noise an operator learns to skip past.
	for _, addr := range []string{"127.0.0.1", "localhost", "::1"} {
		out := captureStderr(t, func() {
			if err := f.Set(&s, addr); err != nil {
				t.Errorf("Set(%q): %v", addr, err)
			}
		})
		if out != "" {
			t.Errorf("Set(%q) wrote %q to stderr, want nothing", addr, out)
		}
	}
}
