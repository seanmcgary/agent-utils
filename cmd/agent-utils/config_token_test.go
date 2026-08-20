package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/listener"
)

// Every test here drives storeToken/readToken with an injected reader and
// writer rather than os.Stdin. A test that swapped the process's real stdin
// for a pty would be the only test in this repo needing one, and a test that
// left os.Stdin alone would block forever the moment the prompt path was
// reached under `go test` with a terminal attached.

// Piping the token in is the scripted-machine-build path: `echo $TOKEN |
// agent-utils config token`, with no terminal anywhere.
func TestStoreTokenReadsPipedStdin(t *testing.T) {
	withHome(t)
	var out bytes.Buffer

	if err := storeToken(strings.NewReader("ghp_piped\n"), &out); err != nil {
		t.Fatalf("storeToken: %v", err)
	}

	got, err := listener.Token()
	if err != nil {
		t.Fatalf("listener.Token: %v", err)
	}
	if got != "ghp_piped" {
		t.Errorf("listener.Token() = %q, want %q", got, "ghp_piped")
	}

	// What it printed must locate the file and state the mode, and must not
	// contain the credential: this line lands in a terminal scrollback, a
	// screen share, or a CI log.
	msg := out.String()
	if strings.Contains(msg, "ghp_piped") {
		t.Errorf("printed the token: %q", msg)
	}
	home := os.Getenv("AGENT_UTILS_HOME")
	if !strings.Contains(msg, filepath.Join(home, "env")) {
		t.Errorf("output = %q, want it to name the file it wrote", msg)
	}
	if !strings.Contains(msg, "0600") {
		t.Errorf("output = %q, want it to state the mode", msg)
	}
}

// No terminal and nothing piped is cron, launchd, or a CI step. Prompting
// there would hang forever, which is the rule resolveLoopConfig already
// documents, so it must fail immediately and name the command the operator
// should run at a terminal instead.
func TestStoreTokenRefusesWithNoTerminalAndNoInput(t *testing.T) {
	withHome(t)
	// Explicitly empty, not merely unset in the test process: discovery uses
	// $GITHUB_TOKEN when nothing was piped, so a developer with one exported
	// in their shell would otherwise see this refusal turn into a success and
	// get a different result from `go test` than CI does.
	withoutGithubToken(t)
	var out bytes.Buffer

	err := storeToken(strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("storeToken with no terminal and no input: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "config token") {
		t.Errorf("error = %v, want it to name `agent-utils config token`", err)
	}

	home := os.Getenv("AGENT_UTILS_HOME")
	if _, statErr := os.Stat(filepath.Join(home, "env")); statErr == nil {
		t.Error("an env file was written despite the refusal")
	}
}

// A refused value must not leave a half-written file behind either.
func TestStoreTokenRejectsAnUnwritableValue(t *testing.T) {
	withHome(t)
	var out bytes.Buffer

	if err := storeToken(strings.NewReader("ghp_a'b\n"), &out); err == nil {
		t.Fatal("storeToken with a quote in the token: want an error, got nil")
	}
	home := os.Getenv("AGENT_UTILS_HOME")
	if _, statErr := os.Stat(filepath.Join(home, "env")); statErr == nil {
		t.Error("an env file was written despite the refusal")
	}
}

// Only the first line is taken. `echo` adds a newline, and a here-doc or a
// file redirect may add more; treating the rest as part of the credential
// would store a token that is simply wrong.
func TestStoreTokenTakesOnlyTheFirstPipedLine(t *testing.T) {
	withHome(t)
	var out bytes.Buffer

	if err := storeToken(strings.NewReader("ghp_piped\ntrailing junk\n"), &out); err != nil {
		t.Fatalf("storeToken: %v", err)
	}
	got, err := listener.Token()
	if err != nil {
		t.Fatalf("listener.Token: %v", err)
	}
	if got != "ghp_piped" {
		t.Errorf("listener.Token() = %q, want %q", got, "ghp_piped")
	}
}

// The command has to be reachable as `agent-utils config token`; the tests
// above exercise the helper, not the wiring.
func TestConfigTokenIsRegistered(t *testing.T) {
	for _, c := range configCommand().Commands {
		if c.Name == "token" {
			if len(c.Flags) != 0 {
				t.Errorf("config token has flags %v; the token must never be passed as one, "+
					"because a flag value shows up in ps output and in shell history", c.Flags)
			}
			return
		}
	}
	t.Fatal("no `token` subcommand under `config`")
}

// The three tests below cover `listener start`'s token check. It calls
// listener.Token() up front so a bad file fails fast, rather than on every
// tick; the only change is that an ABSENT file, at a terminal, becomes a
// question instead of an error.

func TestEnsureTokenPromptsAndContinuesWhenTheFileIsAbsent(t *testing.T) {
	withHome(t)
	var out bytes.Buffer

	if err := ensureToken(strings.NewReader("ghp_typed\n"), &out, true); err != nil {
		t.Fatalf("ensureToken: %v", err)
	}

	got, err := listener.Token()
	if err != nil {
		t.Fatalf("listener.Token after ensureToken: %v", err)
	}
	if got != "ghp_typed" {
		t.Errorf("listener.Token() = %q, want %q", got, "ghp_typed")
	}
}

// Under launchd or cron there is no terminal, and prompting would hang
// forever. The existing error stands, unchanged.
func TestEnsureTokenKeepsTheErrorWithNoTerminal(t *testing.T) {
	withHome(t)
	var out bytes.Buffer

	err := ensureToken(strings.NewReader("ghp_typed\n"), &out, false)
	if err == nil {
		t.Fatal("ensureToken with no terminal and no env file: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "install -m 600") {
		t.Errorf("error = %v, want the existing create-the-file guidance", err)
	}
	home := os.Getenv("AGENT_UTILS_HOME")
	if _, statErr := os.Stat(filepath.Join(home, "env")); statErr == nil {
		t.Error("an env file was written without a terminal to ask at")
	}
}

// A wrong mode, a symlink or a bad owner are conditions the operator has to
// look at, not ones a prompt can answer: overwriting the file would destroy
// the evidence of whatever put it in that state. Only the absent case is
// interactive.
func TestEnsureTokenDoesNotPromptForAWrongMode(t *testing.T) {
	withHome(t)
	home := os.Getenv("AGENT_UTILS_HOME")
	envPath := filepath.Join(home, "env")
	if err := os.WriteFile(envPath, []byte("export GITHUB_TOKEN=ghp_old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	err := ensureToken(strings.NewReader("ghp_typed\n"), &out, true)
	if err == nil {
		t.Fatal("ensureToken on a 0644 env file: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "mode") {
		t.Errorf("error = %v, want it to name the mode", err)
	}
	body, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "ghp_old") {
		t.Errorf("env file = %q, want it left exactly as it was found", body)
	}
}

// The tests below cover token DISCOVERY: the step in front of the prompt that
// looks for a credential the operator already has, so `config token` and
// `listener start`'s inline offer stop asking for something the machine can
// already answer.
//
// Two things are stubbed, and only two. isTerminalInput/readSecret stand in
// for the tty (term.ReadPassword fails with ENOTTY on anything else, so the
// prompt path is otherwise unreachable from a test), and ghAuthToken stands
// in for the `gh` binary, so no test here depends on whether the machine
// running it happens to have gh installed or logged in.

// fakeTerminal records what the stubbed prompt was asked for, so a test can
// assert that a discovered token meant the operator was never prompted at
// all -- the difference between "found it" and "asked anyway".
type fakeTerminal struct {
	typed   string
	prompts int
}

func stubTerminal(t *testing.T, typed string) *fakeTerminal {
	t.Helper()
	ft := &fakeTerminal{typed: typed}
	origTerminal, origSecret := isTerminalInput, readSecret
	t.Cleanup(func() { isTerminalInput, readSecret = origTerminal, origSecret })
	isTerminalInput = func(io.Reader) bool { return true }
	readSecret = func(io.Reader) (string, error) {
		ft.prompts++
		return ft.typed, nil
	}
	return ft
}

// stubGh replaces the `gh auth token` lookup and counts the calls: a test
// that wants "gh was consulted and had nothing" has to tell that apart from
// "gh was never consulted", which is exactly the bug of forgetting to wire
// discovery in at all.
func stubGh(t *testing.T, token string) *int {
	t.Helper()
	calls := 0
	orig := ghAuthToken
	t.Cleanup(func() { ghAuthToken = orig })
	ghAuthToken = func() string {
		calls++
		return token
	}
	return &calls
}

// withoutGithubToken makes a test independent of the shell that started it.
// A developer with GITHUB_TOKEN exported would otherwise get different
// behaviour from `go test` than CI does.
func withoutGithubToken(t *testing.T) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "")
}

// A token already in the environment is one the operator has plainly got, so
// asking them to paste it again is a question with a known answer. It is used
// without prompting, even sitting at a terminal.
func TestStoreTokenUsesTheEnvironmentTokenWithoutPrompting(t *testing.T) {
	withHome(t)
	t.Setenv("GITHUB_TOKEN", "ghp_fromtheenvironmentAB12")
	term := stubTerminal(t, "ghp_typedatprompt")
	stubGh(t, "ghp_fromghcliCD34")
	var out bytes.Buffer

	if err := storeToken(strings.NewReader(""), &out); err != nil {
		t.Fatalf("storeToken: %v", err)
	}

	if term.prompts != 0 {
		t.Errorf("prompted %d times despite $GITHUB_TOKEN being set", term.prompts)
	}
	got, err := listener.Token()
	if err != nil {
		t.Fatalf("listener.Token: %v", err)
	}
	if got != "ghp_fromtheenvironmentAB12" {
		t.Errorf("listener.Token() = %q, want the value from $GITHUB_TOKEN", got)
	}

	msg := out.String()
	if strings.Contains(msg, "ghp_fromtheenvironmentAB12") || strings.Contains(msg, "ghp_fromghcliCD34") {
		t.Errorf("printed a token: %q", msg)
	}
	if !strings.Contains(msg, "GITHUB_TOKEN") {
		t.Errorf("output = %q, want it to say the token came from the environment", msg)
	}
	if !strings.Contains(msg, "…AB12") {
		t.Errorf("output = %q, want a masked fingerprint so the operator can tell which credential it is", msg)
	}
}

// The scripted machine build, with no stdin at all: `GITHUB_TOKEN=... agent-utils
// config token` in a provisioning script has no terminal and nothing piped,
// and must not hit the "there is nobody to prompt" refusal when the answer is
// sitting in its own environment.
func TestStoreTokenUsesTheEnvironmentTokenWithNoStdinAtAll(t *testing.T) {
	withHome(t)
	t.Setenv("GITHUB_TOKEN", "ghp_fromtheenvironmentAB12")
	stubGh(t, "")
	var out bytes.Buffer

	if err := storeToken(strings.NewReader(""), &out); err != nil {
		t.Fatalf("storeToken with no terminal and no piped input: %v", err)
	}
	got, err := listener.Token()
	if err != nil {
		t.Fatalf("listener.Token: %v", err)
	}
	if got != "ghp_fromtheenvironmentAB12" {
		t.Errorf("listener.Token() = %q, want the value from $GITHUB_TOKEN", got)
	}
}

// A piped value is an explicit instruction; the environment is only a place
// to LOOK when nothing was given. `echo "$OTHER" | agent-utils config token`
// on a machine that also exports GITHUB_TOKEN must store what was piped.
func TestStoreTokenPrefersPipedStdinOverTheEnvironment(t *testing.T) {
	withHome(t)
	t.Setenv("GITHUB_TOKEN", "ghp_fromtheenvironmentAB12")
	var out bytes.Buffer

	if err := storeToken(strings.NewReader("ghp_piped\n"), &out); err != nil {
		t.Fatalf("storeToken: %v", err)
	}
	got, err := listener.Token()
	if err != nil {
		t.Fatalf("listener.Token: %v", err)
	}
	if got != "ghp_piped" {
		t.Errorf("listener.Token() = %q, want the piped value to win", got)
	}
}

// `gh` holding a token is a strong hint, not an instruction -- the operator
// may want a different one -- so it is offered as the prompt's default and
// Enter accepts it.
func TestReadTokenOffersTheGhTokenAsTheDefault(t *testing.T) {
	withHome(t)
	withoutGithubToken(t)
	stubGh(t, "ghp_fromghcliAB12")
	stubTerminal(t, "") // the operator pressed Enter
	var out bytes.Buffer

	if err := storeToken(strings.NewReader(""), &out); err != nil {
		t.Fatalf("storeToken: %v", err)
	}
	got, err := listener.Token()
	if err != nil {
		t.Fatalf("listener.Token: %v", err)
	}
	if got != "ghp_fromghcliAB12" {
		t.Errorf("listener.Token() = %q, want the token gh reported", got)
	}

	msg := out.String()
	if strings.Contains(msg, "ghp_fromghcliAB12") {
		t.Errorf("printed the token: %q", msg)
	}
	if !strings.Contains(msg, "gh auth token") {
		t.Errorf("prompt = %q, want it to name where the default came from", msg)
	}
	if !strings.Contains(msg, "…AB12") {
		t.Errorf("prompt = %q, want a masked fingerprint of the default", msg)
	}
}

// Typing over the default replaces it. The default is a convenience, never a
// value the operator cannot get rid of.
func TestReadTokenPrefersATypedTokenOverTheGhDefault(t *testing.T) {
	withHome(t)
	withoutGithubToken(t)
	stubGh(t, "ghp_fromghcliAB12")
	stubTerminal(t, "ghp_typedatprompt")
	var out bytes.Buffer

	if err := storeToken(strings.NewReader(""), &out); err != nil {
		t.Fatalf("storeToken: %v", err)
	}
	got, err := listener.Token()
	if err != nil {
		t.Fatalf("listener.Token: %v", err)
	}
	if got != "ghp_typedatprompt" {
		t.Errorf("listener.Token() = %q, want the typed value to override the default", got)
	}
	msg := out.String()
	if !strings.Contains(msg, "…AB12") {
		t.Errorf("prompt = %q, want the gh default to have been offered at all", msg)
	}
	if strings.Contains(msg, "ghp_fromghcliAB12") || strings.Contains(msg, "ghp_typedatprompt") {
		t.Errorf("printed a token: %q", msg)
	}
}

// gh absent, or gh present but not logged in: both arrive here as "no token",
// and the prompt is the plain one it has always been -- with no mention of a
// default the operator cannot accept, and nothing of gh's own stderr in it.
func TestReadTokenPromptsPlainlyWhenGhHasNoToken(t *testing.T) {
	withHome(t)
	withoutGithubToken(t)
	calls := stubGh(t, "")
	stubTerminal(t, "ghp_typedatprompt")
	var out bytes.Buffer

	if err := storeToken(strings.NewReader(""), &out); err != nil {
		t.Fatalf("storeToken: %v", err)
	}
	if *calls == 0 {
		t.Error("gh was never consulted; discovery has to try it before falling back to the prompt")
	}
	got, err := listener.Token()
	if err != nil {
		t.Fatalf("listener.Token: %v", err)
	}
	if got != "ghp_typedatprompt" {
		t.Errorf("listener.Token() = %q, want the typed value", got)
	}
	if msg := out.String(); !strings.HasPrefix(msg, "GitHub token (not echoed): ") {
		t.Errorf("prompt = %q, want the plain prompt with no default offered", msg)
	}
}

// `listener start`'s inline offer keeps the sentence explaining why the file
// matters, and its first sentence now reports what discovery found rather
// than always claiming there is nothing.
func TestEnsureTokenReportsAnEnvironmentTokenInItsOffer(t *testing.T) {
	withHome(t)
	t.Setenv("GITHUB_TOKEN", "ghp_fromtheenvironmentAB12")
	term := stubTerminal(t, "ghp_typedatprompt")
	var out bytes.Buffer

	if err := ensureToken(strings.NewReader(""), &out, true); err != nil {
		t.Fatalf("ensureToken: %v", err)
	}
	if term.prompts != 0 {
		t.Errorf("prompted %d times despite $GITHUB_TOKEN being set", term.prompts)
	}
	msg := out.String()
	if !strings.Contains(msg, "GITHUB_TOKEN") {
		t.Errorf("output = %q, want the offer to reflect what discovery found", msg)
	}
	if !strings.Contains(msg, "~/.agent-utils/env on every delivery") {
		t.Errorf("output = %q, want the sentence explaining why the file matters kept", msg)
	}
	if strings.Contains(msg, "ghp_fromtheenvironmentAB12") {
		t.Errorf("printed the token: %q", msg)
	}
}

// The three tests below drive the real `gh` lookup with a stub binary on
// PATH. They are what keeps the seam honest: every rule about the subprocess
// -- trimming, timing out, and never surfacing gh's stderr as ours -- lives
// on the far side of it and would otherwise be tested nowhere.

func fakeGh(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// PREPENDED to the real PATH, not replacing it. exec.LookPath takes the
	// first match, so the stub still wins over a real gh -- but a PATH
	// holding only this directory leaves the stub's own shell unable to find
	// `sleep`, so it exits 127 at once and a test meant to prove the timeout
	// fires proves nothing instead. That version of this test passed with the
	// timeout deleted.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// `gh auth token` emits a trailing newline. Storing it unchanged would write
// a token that is simply wrong, and the failure would surface much later as a
// 401 from GitHub.
func TestGhAuthTokenFromCLITrimsTheOutput(t *testing.T) {
	fakeGh(t, `echo ghp_fromghcli`)

	if got := ghAuthTokenFromCLI(); got != "ghp_fromghcli" {
		t.Errorf("ghAuthTokenFromCLI() = %q, want the trimmed token", got)
	}
}

// gh not logged in exits non-zero and explains itself on stderr. That is gh's
// problem, not agent-utils': it must read as "no token found", and gh's
// stderr must not reach the operator's terminal as though this program had
// failed.
func TestGhAuthTokenFromCLIReportsNothingWhenGhFails(t *testing.T) {
	fakeGh(t, `echo "gh: not logged in to any hosts" >&2; exit 1`)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	got := ghAuthTokenFromCLI()
	os.Stderr = orig
	w.Close()
	leaked, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if got != "" {
		t.Errorf("ghAuthTokenFromCLI() = %q, want %q for a gh that exits non-zero", got, "")
	}
	if len(leaked) != 0 {
		t.Errorf("gh's stderr reached this process's stderr: %q", leaked)
	}
}

// A gh that never returns -- a hung keychain prompt, a wedged credential
// helper -- must not take `config token` down with it. Without the timeout
// the command hangs with no prompt and no explanation.
func TestGhAuthTokenFromCLIGivesUpOnAHang(t *testing.T) {
	fakeGh(t, `sleep 30`)
	orig := ghAuthTokenTimeout
	t.Cleanup(func() { ghAuthTokenTimeout = orig })
	ghAuthTokenTimeout = 200 * time.Millisecond

	start := time.Now()
	got := ghAuthTokenFromCLI()

	if got != "" {
		t.Errorf("ghAuthTokenFromCLI() = %q, want %q when gh hangs", got, "")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s for a hung gh; the timeout did not fire", elapsed)
	}
}

// gh not installed at all is the common case on a fresh machine, and reaches
// the same answer by a different route: exec fails before anything runs.
func TestGhAuthTokenFromCLIReportsNothingWhenGhIsAbsent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	if got := ghAuthTokenFromCLI(); got != "" {
		t.Errorf("ghAuthTokenFromCLI() = %q, want %q when gh is not on PATH", got, "")
	}
}
