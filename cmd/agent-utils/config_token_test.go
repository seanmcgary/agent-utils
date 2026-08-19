package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
