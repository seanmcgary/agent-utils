package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/settings"
	"github.com/urfave/cli/v3"
)

// registerWebhookCommand registers, with GitHub, the webhook endpoint that
// lets the daemon dispatch an agent for this project's loops.
//
// --name here selects a LOOP, and it shadows project's own --name (which
// selects the PROJECT): urfave/cli lets a child command declare a flag with
// the same name as its parent's, and the child's own value wins for
// c.String("name") read from the child. sessionsCommand does exactly this
// for the same reason, and selectedProject's doc comment states the general
// rule this command follows: resolve the project by walking the lineage
// (openProject/selectedProject), and read the loop selector directly off
// this command with c.String("name").
func registerWebhookCommand() *cli.Command {
	return &cli.Command{
		Name:  "register-webhook",
		Usage: "register this project's repositories with GitHub as webhook delivery targets",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "name", Usage: "restrict to one loop; omit to register every loop's repository"},
			&cli.BoolFlag{Name: "yes", Usage: "skip the confirmation prompt"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			p, err := openProject(c)
			if err != nil {
				return err
			}
			entries, err := config.List(p.Dir)
			if err != nil {
				return err
			}
			// c.String("name") is THIS command's own --name (the loop), not
			// project's; see the doc comment above.
			repos := collectRepos(entries, c.String("name"))
			if len(repos) == 0 {
				return errors.New("no repositories to register; check --name")
			}

			s, err := settings.Load()
			if err != nil {
				return err
			}

			token := os.Getenv("GITHUB_TOKEN")
			return registerWebhookRun(ctx, repos, registerWebhookDeps{
				// ghub.New never talks to the network by itself; the token check
				// inside registerWebhookRun runs before any of ListHooks,
				// CreateHook or EditHook is ever called on this client.
				Hooks:       ghub.New(token),
				Settings:    s,
				Token:       token,
				Yes:         c.Bool("yes"),
				Interactive: isInteractive(),
				Confirm:     confirmRegisterWebhook,
				Out:         os.Stdout,
			})
		},
	}
}

// collectRepos gathers the distinct repositories this project's loops watch.
//
// A loop whose entry has a non-nil Err is skipped and reported on stderr,
// rather than aborting the whole command: one broken configuration file must
// not block registering the webhook for every other loop's repository.
func collectRepos(entries []config.Entry, loopName string) []string {
	seen := map[string]bool{}
	var repos []string
	for _, e := range entries {
		if loopName != "" && e.Name != loopName {
			continue
		}
		if e.Err != nil {
			fmt.Fprintf(os.Stderr, "skipping loop %q: %v\n", e.Name, e.Err)
			continue
		}
		if seen[e.Repo] {
			continue
		}
		seen[e.Repo] = true
		repos = append(repos, e.Repo)
	}
	return repos
}

// registerWebhookDeps bundles register-webhook's already-resolved inputs, so
// the validation, confirmation and GitHub-call sequence in registerWebhookRun
// can be driven by a test against a fake ghub.HookAdmin, a synthetic
// settings.Settings and a canned Confirm function — none of which require a
// real project, a real terminal or a real GITHUB_TOKEN. Only the Action above
// wires the real ones in.
type registerWebhookDeps struct {
	Hooks       ghub.HookAdmin
	Settings    *settings.Settings
	Token       string
	Yes         bool
	Interactive bool
	// Confirm asks the operator to approve, and is called only when Yes is
	// false and Interactive is true.
	Confirm func(repos []string) (bool, error)
	Out     io.Writer
}

// registerWebhookRun validates, confirms, and then registers repos.
//
// Every early return here happens before Hooks.ListHooks/CreateHook/EditHook
// is ever called, which is what the acceptance criteria on a missing
// webhook.url, a missing token, and a declined or impossible confirmation are
// checking: this command grants GitHub the right to trigger agent dispatch,
// so nothing gets called until the operator (or --yes) has agreed to that.
func registerWebhookRun(ctx context.Context, repos []string, deps registerWebhookDeps) error {
	if strings.TrimSpace(deps.Settings.Webhook.URL) == "" || strings.TrimSpace(deps.Settings.Webhook.Secret) == "" {
		return errors.New(
			"webhook.url and webhook.secret are not set; run " +
				"`agent-utils config webhook --enable --url <url>` first")
	}
	if deps.Token == "" {
		return errors.New("GITHUB_TOKEN is not set")
	}

	if !deps.Yes {
		// A prompt in a cron job would hang forever; that rule is already
		// written into resolveLoopConfig, and this command carries the same
		// obligation because it is at least as capable of running unattended.
		if !deps.Interactive {
			return fmt.Errorf(
				"refusing to register a webhook without confirmation in a non-interactive run: %s\n"+
					"pass --yes to proceed", strings.Join(repos, ", "))
		}
		ok, err := deps.Confirm(repos)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("aborted")
		}
	}

	if !deps.Settings.Webhook.Enabled {
		// Registering before enabling is a reasonable order (the operator may
		// be setting up several repositories before flipping the daemon on),
		// so this warns rather than refuses.
		fmt.Fprintln(os.Stderr,
			"warning: webhook.enabled is false; the listener will refuse to start "+
				"until `agent-utils config webhook --enable` is run")
	}

	return registerWebhooks(ctx, deps.Hooks, repos, deps.Settings, deps.Out)
}

// registerWebhooks does the actual GitHub work: for each repository it edits
// the existing hook whose Config.URL equals webhook.url, or creates one.
//
// A found hook is always edited, never left alone, even when its Events and
// Active already look correct: ghub.HookEvents can grow between releases, and
// GitHub itself can flip Active to false after a run of failed deliveries.
// Always sending the current, full HookEvents/Active=true is what makes
// re-running this command after HookEvents grows re-subscribe an
// already-registered repository, rather than leaving it silently behind.
func registerWebhooks(ctx context.Context, hooks ghub.HookAdmin, repos []string, s *settings.Settings, out io.Writer) error {
	spec := ghub.HookSpec{URL: s.Webhook.URL, Secret: s.Webhook.Secret, Events: ghub.HookEvents}
	for _, repo := range repos {
		owner, name, ok := strings.Cut(repo, "/")
		if !ok {
			// config.Load already validates repo is owner/name form (see
			// internal/config's validate), so reaching this means Entry.Repo was
			// built some other way; fail loudly instead of calling ListHooks
			// with a nonsense owner.
			return fmt.Errorf("repo %q is not in owner/name form", repo)
		}

		existing, err := hooks.ListHooks(ctx, owner, name)
		if err != nil {
			return err
		}
		var found *ghub.Hook
		for i := range existing {
			if existing[i].URL == s.Webhook.URL {
				found = &existing[i]
				break
			}
		}

		if found != nil {
			if err := hooks.EditHook(ctx, owner, name, found.ID, spec); err != nil {
				return err
			}
			// The hook is already registered with GitHub at this point; a
			// failure to print the confirmation line is not worth aborting the
			// rest of the repositories over, but errcheck still requires the
			// return value be looked at, so report it the same way a later
			// repository's real failure would be.
			if _, err := fmt.Fprintf(out, "updated %s (hook %d)\n", repo, found.ID); err != nil {
				return fmt.Errorf("report update for %s: %w", repo, err)
			}
			continue
		}

		id, err := hooks.CreateHook(ctx, owner, name, spec)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "created %s (hook %d)\n", repo, id); err != nil {
			return fmt.Errorf("report creation for %s: %w", repo, err)
		}
	}
	return nil
}

// confirmRegisterWebhook asks the operator to approve registering a webhook
// for repos. It is called only when isInteractive() is true; see
// registerWebhookRun.
//
// The prompt goes to stderr, matching promptForConfig, so a piped stdout
// stays machine readable even in the rare case someone scripts an
// interactive session.
func confirmRegisterWebhook(repos []string) (bool, error) {
	fmt.Fprintln(os.Stderr, "This grants GitHub the right to trigger agent dispatch on:")
	for _, r := range repos {
		fmt.Fprintf(os.Stderr, "  %s\n", r)
	}
	fmt.Fprint(os.Stderr, "Continue? [y/N] ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	choice := strings.ToLower(strings.TrimSpace(line))
	return choice == "y" || choice == "yes", nil
}
