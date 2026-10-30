package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/ghapp"
	"github.com/seanmcgary/agent-utils/internal/identity"
	"github.com/seanmcgary/agent-utils/internal/settings"
	"github.com/urfave/cli/v3"
)

// dispatchRepo is the repository the helpers serve. It comes from the
// dispatch's environment and never from a flag, so the shim and the
// credential helper cannot be pointed at another repository the app is
// installed on by an argument the agent writes. This is a speed bump, not a
// boundary: see the Security section of the README.
func dispatchRepo() (string, error) {
	r := strings.TrimSpace(os.Getenv(identity.RepoEnv))
	if r == "" {
		return "", fmt.Errorf("%s is not set; this command runs inside an agent dispatch", identity.RepoEnv)
	}
	return r, nil
}

func appToken(ctx context.Context) (string, error) {
	repo, err := dispatchRepo()
	if err != nil {
		return "", err
	}
	s, err := settings.Load()
	if err != nil {
		return "", err
	}
	app, err := ghapp.FromSettings(s)
	if err != nil {
		return "", err
	}
	if app == nil {
		return "", errors.New("no github_app is configured")
	}
	return app.Token(ctx, repo)
}

func ghTokenCommand() *cli.Command {
	return &cli.Command{
		Name:  "gh-token",
		Usage: "print an installation token for the dispatch's repository",
		Action: func(ctx context.Context, c *cli.Command) error {
			tok, err := appToken(ctx)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(c.Root().Writer, tok)
			return err
		},
	}
}

func gitCredentialCommand() *cli.Command {
	return &cli.Command{
		Name:      "git-credential",
		Usage:     "git credential helper for the dispatch's repository",
		Arguments: []cli.Argument{&cli.StringArg{Name: "operation"}},
		Action: func(ctx context.Context, c *cli.Command) error {
			return credentialResponse(os.Stdin, c.Root().Writer, c.StringArg("operation"),
				func() (string, error) { return appToken(ctx) })
		},
	}
}

// credentialResponse implements git's credential helper protocol. Only "get"
// for https://github.com answers; "store" and "erase" are ignored, because
// the cache is ghapp's and a token git saw rejected is one minutes from
// expiry that the next get replaces anyway.
func credentialResponse(in io.Reader, out io.Writer, op string, token func() (string, error)) error {
	if op != "get" {
		_, _ = io.Copy(io.Discard, in)
		return nil
	}
	attrs := map[string]string{}
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			attrs[k] = v
		}
	}
	if attrs["protocol"] != "https" || attrs["host"] != "github.com" {
		return nil
	}
	tok, err := token()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "username=x-access-token\npassword=%s\n", tok)
	return err
}
