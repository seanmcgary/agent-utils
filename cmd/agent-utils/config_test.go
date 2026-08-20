package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/settings"
	"github.com/urfave/cli/v3"
)

// withHome points $AGENT_UTILS_HOME at a fresh temp directory, so a test
// cannot see another test's settings file and cannot touch the operator's
// real ~/.agent-utils/config.yaml. internal/settings.Path resolves through
// internal/home, which honours this exact variable.
func withHome(t *testing.T) {
	t.Helper()
	t.Setenv("AGENT_UTILS_HOME", t.TempDir())
}

// runConfigCLI runs the config command tree against args and returns what it
// printed to stdout and stderr. It builds a bare root rather than the real
// main() tree so a test cannot accidentally exercise `project` or `loop` and
// touch a real project directory.
//
// Both streams are captured, not just stdout: config webhook's operator
// messages (the --rotate-secret re-register line, warnings) go to stderr by
// the same convention as the rest of this program's prompts, and a test that
// only reads stdout cannot tell a message from a typo in it from a message
// that was never printed at all.
func runConfigCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := &cli.Command{
		Name:     "agent-utils",
		Commands: []*cli.Command{configCommand()},
	}

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	runErr := root.Run(context.Background(), append([]string{"agent-utils"}, args...))
	os.Stdout, os.Stderr = oldOut, oldErr
	outW.Close()
	errW.Close()

	var outBuf, errBuf bytes.Buffer
	if _, err := io.Copy(&outBuf, outR); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	if _, err := io.Copy(&errBuf, errR); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return outBuf.String(), errBuf.String(), runErr
}

// TestConfigWebhookEnableWithoutURLFailsAndWritesNothing covers B2's bullet:
// "webhook --enable with no URL and none stored must fail and write NOTHING —
// a half-configured file is worse than a rejected command."
func TestConfigWebhookEnableWithoutURLFailsAndWritesNothing(t *testing.T) {
	withHome(t)

	if _, _, err := runConfigCLI(t, "config", "webhook", "--enable"); err == nil {
		t.Fatal("webhook --enable with no url and none stored: want an error, got nil")
	}

	s, err := settings.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if *s != (settings.Settings{}) {
		t.Errorf("Load = %+v, want the zero value: the failed --enable must not have written anything", *s)
	}

	// Load() alone cannot distinguish "wrote nothing" from "wrote a
	// zero-valued file": a missing file and an empty-but-present one both
	// read back as the zero value. Assert the file itself was never created.
	path, err := settings.Path()
	if err != nil {
		t.Fatalf("settings.Path: %v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("settings file exists at %s after a failed --enable, want no file (stat err = %v)", path, statErr)
	}
}

// TestConfigWebhookEnableWithURLWritesSecretAndDefaults covers: "webhook
// --enable --url https://x/y writes a 64-character secret and the defaults."
func TestConfigWebhookEnableWithURLWritesSecretAndDefaults(t *testing.T) {
	withHome(t)

	if _, _, err := runConfigCLI(t, "config", "webhook", "--enable", "--url", "https://x/y"); err != nil {
		t.Fatalf("webhook --enable --url: %v", err)
	}

	s, err := settings.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !s.Webhook.Enabled {
		t.Error("Webhook.Enabled = false, want true")
	}
	if len(s.Webhook.Secret) != 64 {
		t.Errorf("secret length = %d, want 64 (GenerateSecret's 32 bytes hex-encoded)", len(s.Webhook.Secret))
	}

	// config show/webhook must never call WithDefaults (see settings.go's
	// WithDefaults comment), so an unset listen_addr/listen_port stays zero in
	// the stored file; the "defaults" the task acceptance names are what
	// WithDefaults fills in for the listener, checked here explicitly.
	withDefaults := s.WithDefaults()
	if withDefaults.Webhook.ListenAddr != settings.DefaultListenAddr {
		t.Errorf("WithDefaults ListenAddr = %q, want %q", withDefaults.Webhook.ListenAddr, settings.DefaultListenAddr)
	}
	if withDefaults.Webhook.ListenPort != settings.DefaultListenPort {
		t.Errorf("WithDefaults ListenPort = %d, want %d", withDefaults.Webhook.ListenPort, settings.DefaultListenPort)
	}
}

// TestConfigWebhookEnableAndDisableTogetherFails covers: "--enable and
// --disable together is an error."
func TestConfigWebhookEnableAndDisableTogetherFails(t *testing.T) {
	withHome(t)

	if _, _, err := runConfigCLI(t, "config", "webhook", "--enable", "--disable", "--url", "https://x/y"); err == nil {
		t.Fatal("webhook --enable --disable: want an error, got nil")
	}
}

// TestConfigWebhookRotateSecretMintsNewSecretAndSaysReregister covers:
// "--rotate-secret mints a new secret and prints a line telling the operator
// to run `agent-utils project register-webhook` again for every repository."
func TestConfigWebhookRotateSecretMintsNewSecretAndSaysReregister(t *testing.T) {
	withHome(t)

	if _, _, err := runConfigCLI(t, "config", "webhook", "--enable", "--url", "https://x/y"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	before, err := settings.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	_, stderr, err := runConfigCLI(t, "config", "webhook", "--rotate-secret")
	if err != nil {
		t.Fatalf("rotate-secret: %v", err)
	}
	after, err := settings.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if after.Webhook.Secret == before.Webhook.Secret {
		t.Error("--rotate-secret did not change the stored secret")
	}

	// The re-register line goes to stderr (config.go), not stdout, so it must
	// be asserted against stderr specifically — a prior version of this test
	// checked only the stored secret and left this message unverified, which
	// meant deleting the Fprintln would not have failed any test.
	if !strings.Contains(stderr, "register-webhook") {
		t.Errorf("stderr = %q, want it to direct the operator to re-run register-webhook", stderr)
	}
}

// TestConfigWebhookRotateSecretDoesNotPrintBeforeSaveSucceeds covers MINOR 2
// from the B2+D2 review: `--rotate-secret --enable` with no stored URL and
// no --url fails at the --enable check, after the secret has already been
// minted in memory but before Save is ever called. The re-register message
// must not appear in that case — it would tell the operator to re-register a
// secret that was never written to disk.
func TestConfigWebhookRotateSecretDoesNotPrintBeforeSaveSucceeds(t *testing.T) {
	withHome(t)

	_, stderr, err := runConfigCLI(t, "config", "webhook", "--rotate-secret", "--enable")
	if err == nil {
		t.Fatal("rotate-secret --enable with no url: want an error, got nil")
	}
	if strings.Contains(stderr, "register-webhook") {
		t.Errorf("stderr = %q, must not print the re-register message: nothing was saved", stderr)
	}

	s, loadErr := settings.Load()
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if *s != (settings.Settings{}) {
		t.Errorf("Load = %+v, want the zero value: the failed command must not have written anything", *s)
	}
}

// TestConfigShowRedactsSecretUnlessRevealed covers: "show on a populated file
// prints ***redacted***; show --reveal prints the secret."
func TestConfigShowRedactsSecretUnlessRevealed(t *testing.T) {
	withHome(t)

	want := &settings.Settings{Webhook: settings.Webhook{
		Enabled: true,
		URL:     "https://x/y",
		Secret:  "supersecretvalue",
	}}
	if err := settings.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, _, err := runConfigCLI(t, "config", "show")
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if !strings.Contains(out, settings.Redacted) {
		t.Errorf("show output = %q, want it to contain %q", out, settings.Redacted)
	}
	if strings.Contains(out, want.Webhook.Secret) {
		t.Errorf("show output leaked the secret: %q", out)
	}

	out, _, err = runConfigCLI(t, "config", "show", "--reveal")
	if err != nil {
		t.Fatalf("show --reveal: %v", err)
	}
	if !strings.Contains(out, want.Webhook.Secret) {
		t.Errorf("show --reveal output = %q, want it to contain the secret", out)
	}
}

// TestConfigGetRedactsSecretUnlessRevealed exercises `get`, the other read
// path B1's Field.Secret governs; show's coverage does not reach it.
func TestConfigGetRedactsSecretUnlessRevealed(t *testing.T) {
	withHome(t)

	want := &settings.Settings{Webhook: settings.Webhook{Secret: "supersecretvalue"}}
	if err := settings.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, _, err := runConfigCLI(t, "config", "get", "webhook.secret")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(out, settings.Redacted) {
		t.Errorf("get output = %q, want %q", out, settings.Redacted)
	}

	out, _, err = runConfigCLI(t, "config", "get", "webhook.secret", "--reveal")
	if err != nil {
		t.Fatalf("get --reveal: %v", err)
	}
	if !strings.Contains(out, want.Webhook.Secret) {
		t.Errorf("get --reveal output = %q, want it to contain the secret", out)
	}
}

// TestConfigSetUnknownKeyNamesKnownKeys covers: "set webhook.nope x exits
// non-zero and names the known keys."
func TestConfigSetUnknownKeyNamesKnownKeys(t *testing.T) {
	withHome(t)

	_, _, err := runConfigCLI(t, "config", "set", "webhook.nope", "x")
	if err == nil {
		t.Fatal("set webhook.nope: want an error, got nil")
	}
	for _, f := range settings.Fields() {
		if !strings.Contains(err.Error(), f.Key) {
			t.Errorf("error %q does not name known key %q", err.Error(), f.Key)
		}
	}
}

// TestConfigSetSecretDoesNotPanic covers the reviewed hazard: settings.Field's
// Set is nil for webhook.secret by design (a hand-typed secret must be
// impossible), and internal/settings' own tests call Set unguarded. Without a
// nil check here, `config set webhook.secret x` panics instead of erroring.
func TestConfigSetSecretDoesNotPanic(t *testing.T) {
	withHome(t)

	_, _, err := runConfigCLI(t, "config", "set", "webhook.secret", "x")
	if err == nil {
		t.Fatal("set webhook.secret: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "rotate-secret") {
		t.Errorf("error = %q, want it to direct the operator to --rotate-secret", err.Error())
	}
}

// TestConfigSetThenUnsetRoundTrips is a thin smoke test for the two simplest
// paths: set writes a valid value, unset clears it back to the zero value.
func TestConfigSetThenUnsetRoundTrips(t *testing.T) {
	withHome(t)

	if _, _, err := runConfigCLI(t, "config", "set", "webhook.listen_port", "9999"); err != nil {
		t.Fatalf("set: %v", err)
	}
	s, err := settings.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Webhook.ListenPort != 9999 {
		t.Fatalf("ListenPort = %d, want 9999", s.Webhook.ListenPort)
	}

	if _, _, err := runConfigCLI(t, "config", "unset", "webhook.listen_port"); err != nil {
		t.Fatalf("unset: %v", err)
	}
	s, err = settings.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Webhook.ListenPort != 0 {
		t.Fatalf("ListenPort after unset = %d, want 0", s.Webhook.ListenPort)
	}
}
