package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/listener"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

// configTokenCommand stores the GitHub token the listener and the cron entry
// read from ~/.agent-utils/env.
//
// It exists because the alternative was telling an operator to run
// `install -m 600 /dev/null ~/.agent-utils/env` and an `echo ... >>` by hand.
// Every part of that except the token itself is something this program knows,
// and the hand-run form gets the mode wrong the moment somebody drops the
// `-m 600` -- at which point the daemon refuses to start and the operator is
// debugging file permissions instead of running a loop.
func configTokenCommand() *cli.Command {
	return &cli.Command{
		Name:  "token",
		Usage: "store the GitHub token the listener and cron read from `~/.agent-utils/env`",
		Description: "Uses $GITHUB_TOKEN if it is set, otherwise offers the token `gh auth token` " +
			"reports as the default, otherwise prompts with echo disabled. Reads one piped in " +
			"(`echo $TOKEN | agent-utils config token`) for a scripted machine build.\n" +
			"Any other line already in the file is left untouched.",
		Action: func(_ context.Context, c *cli.Command) error {
			// No flag and no argument, deliberately. main.go's rule: "The
			// token must come from the environment, never a flag. A flag
			// value shows up in `ps` output and in the shell history of
			// anyone who typed it." An argument is worse on both counts, so
			// one passed by an operator who assumed it would work is
			// rejected rather than quietly ignored while the prompt asks for
			// a value they believe they already gave.
			if c.Args().Len() > 0 {
				return errors.New(
					"`config token` takes no arguments: a token on the command line shows up in " +
						"`ps` output and in your shell history. Run it with no arguments and paste " +
						"the token at the prompt, or pipe it in: echo $TOKEN | agent-utils config token")
			}
			return storeToken(os.Stdin, os.Stderr)
		},
	}
}

// storeToken obtains a token -- from the environment, from `gh`, from a
// prompt, or from a pipe, in the order readToken documents -- and writes it
// to the env file, reporting where it went on out.
//
// in and out are parameters rather than os.Stdin/os.Stderr directly so this
// is testable without a pty, and so `listener start` can reuse it.
func storeToken(in io.Reader, out io.Writer) error {
	token, err := readToken(in, out)
	if err != nil {
		return err
	}
	path, err := listener.SetToken(token)
	if err != nil {
		return err
	}
	// The path and the mode, never the token: this line lands in a terminal
	// scrollback, a screen share, or a CI log.
	_, _ = fmt.Fprintf(out, "Wrote GITHUB_TOKEN to %s (mode 0600).\n", path)
	return nil
}

// githubTokenEnv is both the variable discovery reads out of this process's
// environment and the name written into the env file, deliberately the same
// one: an operator who exported GITHUB_TOKEN for `gh`, `git` or a script has
// already told this machine what the token is, and asking them to paste it
// again is a question with a known answer.
const githubTokenEnv = "GITHUB_TOKEN"

// environmentToken returns the token exported in this process's environment,
// and whether there was one at all.
//
// Trimmed and tested for emptiness rather than os.LookupEnv: `GITHUB_TOKEN=`
// is how a shell profile or a wrapper script UNSETS one for a command, and
// treating that as a discovered credential would send an empty value to
// SetToken and fail with "the GITHUB_TOKEN value is empty" instead of just
// asking.
func environmentToken() (string, bool) {
	token := strings.TrimSpace(os.Getenv(githubTokenEnv))
	return token, token != ""
}

// ghAuthTokenTimeout bounds the `gh auth token` call. gh can block -- on a
// keychain prompt, on a wedged credential helper, on a hung network call to a
// GitHub Enterprise host -- and discovery runs BEFORE this command has asked
// the operator anything, so a gh that never returns would leave `config
// token` hanging with no prompt and no explanation of what it was waiting for.
//
// A variable, not a constant, so the test for the hang does not have to spend
// the real timeout to prove it fires.
var ghAuthTokenTimeout = 5 * time.Second

// ghAuthToken is a variable, not a direct call at the use site, so no test
// ever shells out to a real gh: whether the machine running the suite happens
// to have gh installed, or logged in, must not change what the tests assert.
var ghAuthToken = ghAuthTokenFromCLI

// ghAuthTokenFromCLI asks the `gh` CLI for the token it is already holding,
// and returns "" when there is none to be had.
//
// Every failure is the same answer -- no token -- and none of them is an
// agent-utils error: gh missing from PATH, gh present but not logged in
// (exit 1), a timeout, an empty stdout. The operator asked to store a token,
// not to run gh, so each of these falls through to the plain prompt rather
// than aborting the command with somebody else's problem.
func ghAuthTokenFromCLI() string {
	ctx, cancel := context.WithTimeout(context.Background(), ghAuthTokenTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "gh", "auth", "token")

	// WaitDelay, or the timeout above only half works. Cancelling kills gh
	// itself, but anything gh spawned -- a credential helper, an ssh or a
	// keychain prompt -- inherits the stdout pipe and keeps it open, and
	// Output() blocks reading that pipe until every holder is gone. Measured:
	// with the context timeout alone, a `gh` that hangs for 30s still took
	// the full 30s to return. WaitDelay bounds the wait after cancellation,
	// closes the pipe, and lets this fall through to the prompt.
	cmd.WaitDelay = time.Second

	// Output, which captures the child's stderr into the error, rather than
	// leaving cmd.Stderr nil for it to inherit this process's: `gh auth
	// token` prints "not logged in to any hosts" there, and that line
	// appearing mid-command reads as agent-utils failing at something the
	// operator never asked it to do. The captured copy is discarded with the
	// error below.
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	// Trimmed: gh emits a trailing newline, and storing that as part of the
	// credential would write a token that is simply wrong in a way that only
	// surfaces much later, as a 401 from GitHub.
	return strings.TrimSpace(string(out))
}

// fingerprint renders a token as its type prefix and its last four
// characters: enough for an operator to tell WHICH credential is being
// offered, not enough for anyone to use it.
//
// A default cannot be shown as plain text. The prompt disables echo precisely
// so that a repository-write credential never lands in terminal scrollback,
// and printing the value as a default would put it there anyway -- in the
// scrollback, in a screen share, in a recorded session, in a CI log -- where
// it outlives the command by as long as the token does.
func fingerprint(token string) string {
	const revealed = 4

	r := []rune(token)
	if len(r) < revealed*2 {
		// Four characters of a very short value is a large fraction of it,
		// so reveal nothing at all rather than most of it.
		return "…"
	}

	// A GitHub token carries its type before its last underscore (ghp_, gho_,
	// github_pat_). That prefix is a published format marker rather than a
	// secret, and it is what tells a classic PAT from a fine-grained one at a
	// glance -- shown only when what follows it is still long enough that the
	// revealed characters stay a small part of the whole.
	prefix := ""
	if i := strings.LastIndex(token, "_"); i >= 0 && len([]rune(token[i+1:])) >= revealed*2 {
		prefix = token[:i+1]
	}
	return prefix + "…" + string(r[len(r)-revealed:])
}

// isTerminalInput and readSecret are variables, not direct term calls at the
// use site, so the prompt path is reachable from a test at all:
// term.ReadPassword fails with ENOTTY on anything that is not a tty, and a
// test that swapped the process's stdin for a pty would be the only one in
// this repo needing one. Everything the prompt DECIDES -- whether to offer a
// discovered token as the default, and what the prompt line reveals about it
// -- stays on this side of the seam, where it is tested.
var isTerminalInput = func(in io.Reader) bool {
	f, ok := in.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// readSecret reads one line from a terminal with echo disabled.
//
// term.ReadPassword, not a plain read: an echoed token is left sitting in the
// terminal's scrollback, visible to anyone who scrolls back, and captured
// whole by a screen share or a recorded session. It is a repository-write
// credential, so that exposure outlives the command by as long as the token
// does.
var readSecret = func(in io.Reader) (string, error) {
	f, ok := in.(*os.File)
	if !ok {
		// Only reachable if a caller reads a secret from something
		// isTerminalInput never claimed was a terminal.
		return "", errors.New("cannot disable echo on input that is not a terminal")
	}
	raw, err := term.ReadPassword(int(f.Fd()))
	return string(raw), err
}

// readToken obtains a token, looking for one the operator already has before
// asking for one they would only have to fetch and paste:
//
//	at a terminal: $GITHUB_TOKEN, else `gh auth token` as the prompt default,
//	               else a plain prompt;
//	on a pipe:     the piped line, else $GITHUB_TOKEN, else a refusal.
//
// A piped value beats the environment because it is an explicit instruction,
// while the environment is only somewhere to LOOK when nothing was given:
// `echo "$OTHER" | agent-utils config token` on a machine that also exports
// GITHUB_TOKEN must store what was piped, not what was exported.
//
// `gh` is consulted only on the terminal path. Its token is a strong hint
// rather than an instruction -- the operator may want a different one -- so
// it is offered as a default that Enter accepts, and a default needs someone
// there to accept it. Under cron or launchd, where the refusal below fires,
// there is nobody.
//
// Prompts go to out (stderr at every call site) by the same convention as the
// rest of this program, so a piped stdout stays machine readable.
func readToken(in io.Reader, out io.Writer) (string, error) {
	if isTerminalInput(in) {
		if token, ok := environmentToken(); ok {
			reportEnvironmentToken(out, token)
			return token, nil
		}

		// The fingerprint, never the value: see fingerprint. Naming the
		// source matters as much as the digits -- an operator who did not
		// know gh had a token needs to see which credential Enter is about
		// to store.
		prompt := "GitHub token (not echoed): "
		suggested := ghAuthToken()
		if suggested != "" {
			prompt = fmt.Sprintf(
				"GitHub token [%s from `gh auth token`, press Enter to accept] (not echoed): ",
				fingerprint(suggested))
		}

		_, _ = fmt.Fprint(out, prompt)
		raw, err := readSecret(in)
		// The newline the operator typed was swallowed with the echo, so
		// print one; without it the next output starts on the prompt line.
		_, _ = fmt.Fprintln(out)
		if err != nil {
			return "", fmt.Errorf("read the token: %w", err)
		}
		if strings.TrimSpace(raw) == "" && suggested != "" {
			// Enter on its own accepts the default. Empty with no default
			// falls through to SetToken, which rejects it by name.
			return suggested, nil
		}
		return raw, nil
	}

	// Only the first line. `echo` appends a newline and a here-doc or a file
	// redirect may append more; treating the rest as part of the credential
	// would store a token that is simply wrong, and the failure would surface
	// much later as a 401 from GitHub.
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read the token from stdin: %w", err)
	}
	if strings.TrimSpace(line) == "" {
		// Nothing was piped, so fall back to the environment: a provisioning
		// script that runs `GITHUB_TOKEN=... agent-utils config token` with
		// no stdin at all has still said what the token is, and refusing it
		// for want of a pipe would be refusing an answer already given.
		if token, ok := environmentToken(); ok {
			reportEnvironmentToken(out, token)
			return token, nil
		}

		// Otherwise this is cron, launchd, or a CI step: stdin is /dev/null,
		// so there is nobody to prompt. Failing here, naming the command, is
		// what keeps a service from hanging forever on a question no one
		// will ever see.
		return "", errors.New(
			"stdin is not a terminal and nothing was piped to it, so there is nobody to prompt. " +
				"Run `agent-utils config token` from a terminal, set GITHUB_TOKEN in the " +
				"environment, or pipe the token in: echo $TOKEN | agent-utils config token")
	}
	return line, nil
}

// reportEnvironmentToken says which credential was taken from the
// environment, and how to store a different one instead -- the operator gave
// no answer here, so they are told what was decided for them, by fingerprint
// rather than by value.
func reportEnvironmentToken(out io.Writer, token string) {
	_, _ = fmt.Fprintf(out,
		"Using the GitHub token from $%s (%s); pipe a different one in to override.\n",
		githubTokenEnv, fingerprint(token))
}
