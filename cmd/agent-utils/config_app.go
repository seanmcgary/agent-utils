package main

import (
	"context"
	"fmt"
	"io"
	"path/filepath"

	"github.com/seanmcgary/agent-utils/internal/ghapp"
	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/settings"
	"github.com/urfave/cli/v3"
)

func githubAppCommand() *cli.Command {
	return &cli.Command{
		Name:  "github-app",
		Usage: "act as a GitHub App instead of a personal token",
		Description: "Stores the app id and private key path in the machine-wide settings, " +
			"after checking that GitHub accepts the key. Every daemon call, agent gh call, " +
			"push and commit then acts as the app's bot.",
		Flags: []cli.Flag{
			&cli.Int64Flag{Name: "app-id", Usage: "the GitHub App's id"},
			&cli.StringFlag{Name: "private-key", Usage: "path to the app's private key (.pem), mode 0600"},
			&cli.BoolFlag{Name: "unset", Usage: "stop acting as the app"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			out := c.Root().Writer
			if c.Bool("unset") {
				s, err := settings.Load()
				if err != nil {
					return err
				}
				s.GitHubApp = settings.GitHubApp{}
				if err := settings.Save(s); err != nil {
					return err
				}
				_, _ = fmt.Fprintln(out, "GitHub App removed; GITHUB_TOKEN is used again.")
				return nil
			}
			if c.Int64("app-id") <= 0 || c.String("private-key") == "" {
				return fmt.Errorf("--app-id and --private-key are required")
			}
			keyPath, err := filepath.Abs(c.String("private-key"))
			if err != nil {
				return err
			}
			return configureGitHubApp(ctx, out, c.Int64("app-id"), keyPath,
				func(a *ghapp.App) (string, error) { return a.Slug(ctx) })
		},
	}
}

// configureGitHubApp saves the app only after GitHub has accepted a JWT
// signed with its key, so a wrong id or key fails here rather than on the
// first tick of a detached daemon.
func configureGitHubApp(ctx context.Context, out io.Writer, id int64, keyPath string, verify func(*ghapp.App) (string, error)) error {
	dir, err := home.EnsureDir()
	if err != nil {
		return err
	}
	app, err := ghapp.New(id, keyPath, dir)
	if err != nil {
		return err
	}
	slug, err := verify(app)
	if err != nil {
		return fmt.Errorf("GitHub rejected app %d with this key: %w", id, err)
	}
	s, err := settings.Load()
	if err != nil {
		return err
	}
	s.GitHubApp = settings.GitHubApp{AppID: id, PrivateKeyPath: keyPath}
	if err := settings.Save(s); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Acting as GitHub App %s; commits and comments will be by %s[bot].\n", slug, slug)
	return nil
}
