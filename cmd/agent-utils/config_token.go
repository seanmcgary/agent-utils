package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

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
		Description: "Prompts for the token with echo disabled, or reads it from a pipe " +
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

// storeToken reads a token from in and writes it to the env file, reporting
// where it went on out.
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

// readToken obtains a token from in: from a terminal with echo disabled, or
// from a non-terminal stdin (a pipe) for a scripted machine build.
//
// Prompts go to out (stderr at every call site) by the same convention as the
// rest of this program, so a piped stdout stays machine readable.
func readToken(in io.Reader, out io.Writer) (string, error) {
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		_, _ = fmt.Fprint(out, "GitHub token (not echoed): ")
		// term.ReadPassword, not a plain read: an echoed token is left
		// sitting in the terminal's scrollback, visible to anyone who
		// scrolls back, and captured whole by a screen share or a recorded
		// session. It is a repository-write credential, so that exposure
		// outlives the command by as long as the token does.
		raw, err := term.ReadPassword(int(f.Fd()))
		// The newline the operator typed was swallowed with the echo, so
		// print one; without it the next output starts on the prompt line.
		_, _ = fmt.Fprintln(out)
		if err != nil {
			return "", fmt.Errorf("read the token: %w", err)
		}
		return string(raw), nil
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
		// Reached under cron, launchd, or a CI step: stdin is /dev/null, so
		// there is nobody to prompt. Failing here, naming the command, is
		// what keeps a service from hanging forever on a question no one
		// will ever see.
		return "", errors.New(
			"stdin is not a terminal and nothing was piped to it, so there is nobody to prompt. " +
				"Run `agent-utils config token` from a terminal, or pipe the token in: " +
				"echo $TOKEN | agent-utils config token")
	}
	return line, nil
}
