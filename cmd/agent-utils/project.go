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
			loopName := c.String("name")
			repos := collectRepos(entries, loopName)
			if len(repos) == 0 {
				return noReposErr(loopName)
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

// noReposErr explains why register-webhook found nothing to do.
//
// Two different causes read very differently to an operator: a typo'd
// --name should be told to check the flag it passed, but with no --name at
// all, every loop's entry failing to load (each already reported on stderr
// by collectRepos) is the only way to reach here, and pointing at a flag
// never given would send them looking in the wrong place.
func noReposErr(loopName string) error {
	if loopName != "" {
		return fmt.Errorf("no repository named by loop %q; check --name", loopName)
	}
	return errors.New("no repositories to register: every loop configuration failed to load")
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

// missingWebhookFields names whichever of webhook.url and webhook.secret is
// actually empty. Reachable independently of each other: `config set
// webhook.enabled true` plus `config set webhook.url <url>` sets a URL
// without ever minting a secret, so naming both when only one is empty would
// misdirect the operator.
func missingWebhookFields(s *settings.Settings) []string {
	var missing []string
	if strings.TrimSpace(s.Webhook.URL) == "" {
		missing = append(missing, "webhook.url")
	}
	if strings.TrimSpace(s.Webhook.Secret) == "" {
		missing = append(missing, "webhook.secret")
	}
	return missing
}

// missingWebhookFieldsErr turns missingWebhookFields' result into an error
// with correct subject-verb agreement, so "webhook.url is not set" reads
// naturally alone and "webhook.url and webhook.secret are not set" does too
// when both are empty.
func missingWebhookFieldsErr(missing []string) error {
	verb := "is"
	if len(missing) > 1 {
		verb = "are"
	}
	return fmt.Errorf(
		"%s %s not set; run `agent-utils config webhook --enable --url <url>` first",
		strings.Join(missing, " and "), verb)
}

// registerWebhookRun validates, confirms, and then registers repos.
//
// Every early return here happens before Hooks.ListHooks/CreateHook/EditHook
// is ever called, which is what the acceptance criteria on a missing
// webhook.url, a missing token, and a declined or impossible confirmation are
// checking: this command grants GitHub the right to trigger agent dispatch,
// so nothing gets called until the operator (or --yes) has agreed to that.
func registerWebhookRun(ctx context.Context, repos []string, deps registerWebhookDeps) error {
	if missing := missingWebhookFields(deps.Settings); len(missing) > 0 {
		return missingWebhookFieldsErr(missing)
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
// A found hook is always edited, never conditionally skipped, and the
// strongest reason is the secret: GitHub returns Config.Secret obfuscated
// (see ghub.Hook), so nothing in a listed hook can ever be compared against
// the stored secret to detect that it has been rotated. After `config
// webhook --rotate-secret` (which tells the operator to re-run this
// command), a comparison-gated skip would silently decline to push the new
// secret to a repository whose hook otherwise looks unchanged — every
// later delivery would then be signed with the old secret and rejected by
// the listener, while this command reported success. Unconditional EditHook
// also happens to re-subscribe an already-registered repository when
// ghub.HookEvents grows between releases, and repairs a hook GitHub flipped
// Active=false on after a run of failed deliveries, but the secret is why
// this is not optional.
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
			// A failure writing this line means the operator cannot see which
			// repositories already succeeded, so continuing on to the next
			// repository would register more hooks whose outcome is now
			// invisible to them. Treat it the same as a real failure.
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
