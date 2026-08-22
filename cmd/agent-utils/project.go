package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/project"
	"github.com/seanmcgary/agent-utils/internal/registry"
	"github.com/seanmcgary/agent-utils/internal/settings"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/seanmcgary/agent-utils/internal/wizard"
	"github.com/urfave/cli/v3"
)

// projectInitCommand creates a project explicitly and, unless --no-loop,
// walks through writing its first loop configuration.
//
// The project name is a POSITIONAL argument, following forget's precedent
// (cli.StringArg, read with c.StringArg("name")), and MUST NOT be a --name
// flag: `project` already declares --name as the PROJECT SELECTOR
// (projectSelectorFlag), and urfave/cli lets a child flag of the same name
// shadow a parent's — the exact hazard selectedProject's doc comment warns
// about. A --name here would silently mean two different things depending on
// where in the command line it appeared.
func projectInitCommand() *cli.Command {
	return &cli.Command{
		Name:      "init",
		Usage:     "create a project explicitly and, unless --no-loop, set up its first loop",
		Arguments: []cli.Argument{&cli.StringArg{Name: "name"}},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "dir", Usage: "project directory; omit to use the working directory"},
			&cli.BoolFlag{Name: "no-loop", Usage: "create the project only; skip the loop configuration wizard"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			dir := c.String("dir")
			if dir == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				dir = wd
			}
			return projectInitRun(projectInitDeps{
				Dir:         dir,
				Name:        c.StringArg("name"),
				NoLoop:      c.Bool("no-loop"),
				Interactive: isInteractive(),
				RunWizard:   runLoopWizard,
				Out:         os.Stdout,
			})
		},
	}
}

// projectInitDeps bundles project init's already-resolved inputs, the way
// registerWebhookDeps does for register-webhook: the resolution, minting,
// registration and reporting sequence in projectInitRun can then be driven by
// a test against a temporary $AGENT_UTILS_HOME and a scripted RunWizard
// function, none of which needs a real terminal. Only the Action above wires
// the real ones in.
type projectInitDeps struct {
	// Dir is the project's root directory: --dir, or the working directory.
	Dir string
	// Name is the positional project name. Empty lets mintProjectDescriptor
	// (via project.EnsureNamed) name the project after Dir's base name instead.
	Name        string
	NoLoop      bool
	Interactive bool
	// RunWizard runs the loop-configuration wizard and writes the result. It
	// is called only when NoLoop is false and Interactive is true, and takes
	// (agentUtilsDir, rootDir): Write needs the former, Detect needs the
	// latter.
	RunWizard func(agentUtilsDir, rootDir string) (string, error)
	Out       io.Writer
}

// projectInitRun resolves the target directory, mints or loads the project's
// descriptor, registers it, and — unless told not to — runs the loop wizard.
//
// Re-running it on an already-initialised project is deliberately NOT an
// error: it reports the existing identity and re-registers (a no-op update;
// see registry.Register). It offers the wizard only when the project has no
// loop configuration yet, so `project init` doubles as the entry point for
// finishing a half-set-up project someone forgot already existed, while a
// repository cloned with its loops already committed just gets registered.
func projectInitRun(deps projectInitDeps) error {
	rootDir, err := filepath.Abs(deps.Dir)
	if err != nil {
		return err
	}
	agentUtilsDir := filepath.Join(rootDir, config.DirName)

	// The machine-wide directory (internal/home.Dir()) is an ordinary-looking
	// directory under $HOME with nothing marking it as special. This refusal
	// is the entire reason `project init` exists as an explicit step: without
	// it, `cd ~ && agent-utils project init` would happily write a project
	// descriptor into <machine-wide>/.agent-utils, beside registry.json and
	// state.db, and register it as a "project".
	//
	// Both agentUtilsDir AND rootDir are checked against machineWide.
	// agentUtilsDir is the one that matters for the invocation above --
	// there rootDir is home.Dir()'s PARENT, never equal to it, so a
	// comparison against rootDir alone passes it straight through; the
	// sibling internal/config.FindDir guard makes the identical choice, by
	// comparing its walk CANDIDATE rather than the walk directory
	// (discover.go's isDir(candidate) check). rootDir is still checked too,
	// for the direct-hit case ($AGENT_UTILS_HOME itself, or --dir pointed at
	// it), which agentUtilsDir's comparison alone would miss. Symlinks are
	// resolved on both sides (home.Resolve) for the same reason FindDir
	// does: macOS resolves /var to /private/var, and a raw string compare
	// would fail open. An unresolvable machine-wide directory (home.Dir
	// erroring) degrades to skipping the guard: there is nothing to protect
	// against.
	if machineWide, homeErr := home.Dir(); homeErr == nil {
		mw := home.Resolve(machineWide)
		if home.Resolve(agentUtilsDir) == mw || home.Resolve(rootDir) == mw {
			return fmt.Errorf(
				"refusing to initialise a project in %s: that is agent-utils' machine-wide "+
					"directory (it holds the registry and the canonical state database); "+
					"run `project init` from the directory you want to turn into a project instead",
				rootDir)
		}
	}

	configsDir := config.ConfigsDir(agentUtilsDir)
	// 0700 on both: .agent-utils/ is the project's entire local state and
	// configs/ holds its loop files, matching internal/home.EnsureDir's mode
	// for the same reason — these directories are project-private. Created
	// before minting the descriptor so an interrupted run leaves an empty
	// configs/ rather than a descriptor with nowhere to put a loop file.
	if err := os.MkdirAll(configsDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", configsDir, err)
	}

	cfg, created, renamedFrom, err := mintProjectDescriptor(agentUtilsDir, rootDir, deps.Name)
	if err != nil {
		return err
	}

	// project.EnsureNamed uniquifies a name only on the descriptor it MINTS,
	// so a descriptor that arrived with the repository -- the clone-to-a-new-
	// host case -- walks straight past that check. Registering it anyway put
	// two projects with the same name in the registry, and from that moment
	// every `agent-utils project --name <that name>` command silently acted on
	// whichever of the two ticked last: a status read against the wrong
	// repository, or a `loop reset` that threw away the wrong project's
	// session. Refusing here happens BEFORE registry.Register, so a rejected
	// clone leaves the registry exactly as it was. Comparing by id keeps
	// re-running init on the SAME project the no-op success it has always been.
	if !created {
		if other, ok := conflictingProject(cfg.Name, cfg.ID); ok {
			return fmt.Errorf(
				"refusing to register %s: the name %q already belongs to a different project (%s, id %s)\n"+
					"project names are unique across this machine; edit the name: field in %s and run init again",
				agentUtilsDir, cfg.Name, other.AgentUtilsDir, other.ID, project.Path(agentUtilsDir))
		}
	}

	if err := registry.Register(agentUtilsDir, cfg.ID, cfg.Name); err != nil {
		// Best effort, matching ResolveProject's own comment: the registry is
		// an index, and losing this update costs the project a line in
		// `agent-utils list`, never its descriptor.
		fmt.Fprintf(os.Stderr, "warning: could not update the registry: %v\n", err)
	}

	// This reporting used to live in openProject (which printed to STDERR,
	// alongside its other unsolicited-but-informational messages), reached
	// the first time any project command ran in an un-onboarded directory.
	// It moves here because `project init` is now the explicit place a
	// project is created or re-identified; a sibling unit (F5) removes the
	// old implicit branch. It prints to STDOUT here instead: unlike
	// openProject's callers, whose real output is something else (a status
	// table, a log stream) that this must not interleave with, `project
	// init`'s entire job IS reporting what it did -- there is no other
	// output for this to compete with.
	if created {
		if err := reportf(deps.Out, "Created project %q (%s)\n", cfg.Name, agentUtilsDir); err != nil {
			return err
		}
		if renamedFrom != "" {
			if err := reportf(deps.Out,
				"The name %q was already taken by another project, so this one is %q.\n"+
					"Change it by editing %s\n",
				renamedFrom, cfg.Name, project.Path(agentUtilsDir)); err != nil {
				return err
			}
		}
	} else {
		if err := reportf(deps.Out, "Project %q already exists (%s)\n", cfg.Name, agentUtilsDir); err != nil {
			return err
		}
		if deps.Name != "" && deps.Name != cfg.Name {
			// The project already has an identity, so mintProjectDescriptor
			// never even looked at deps.Name -- but the operator typed it,
			// and `Project "original" already exists` alone gives no hint
			// that the name they asked for did nothing.
			if err := reportf(deps.Out,
				"The name %q was ignored: this project already has an identity; "+
					"rename it by editing %s\n",
				deps.Name, project.Path(agentUtilsDir)); err != nil {
				return err
			}
		}
	}

	// A repository cloned to a new host arrives with its loop configurations
	// already committed, and its only missing piece is this host's registry
	// entry -- which the Register call above has now written. Offering the
	// wizard here was actively harmful: wizard.Write refuses to overwrite an
	// existing loop file, so an operator answered all two dozen questions and
	// only then watched the command fail with nothing to show for it.
	//
	// An entry whose Err is non-nil counts as an existing loop on purpose. A
	// loop file that fails to load still proves the repository was set up; the
	// fix is to repair that file, not to be walked through the wizard that
	// cannot write over it anyway.
	if loops := existingLoopCount(agentUtilsDir); loops > 0 {
		return reportf(deps.Out,
			"Registered %q (%s) at %s with %d existing loop configuration%s; nothing else to do.\n",
			cfg.Name, cfg.ID, agentUtilsDir, loops, plural(loops))
	}

	if deps.NoLoop {
		return reportf(deps.Out,
			"Skipped the loop configuration wizard (--no-loop). "+
				"Run `agent-utils project loop new` to add one.\n")
	}
	if !deps.Interactive {
		// A prompt in a cron job would hang forever — the same rule
		// resolveLoopConfig already documents. init still does the
		// non-prompting half of its job (steps 1-4) and only skips the part
		// that needs a human.
		return reportf(deps.Out,
			"Skipped the loop configuration wizard: stdin is not a terminal. "+
				"Run `agent-utils project loop new` from a terminal to add one.\n")
	}

	loopPath, err := deps.RunWizard(agentUtilsDir, rootDir)
	if err != nil {
		return err
	}
	loopName := strings.TrimSuffix(filepath.Base(loopPath), filepath.Ext(loopPath))
	if err := reportf(deps.Out, "Wrote loop configuration %s\n", loopPath); err != nil {
		return err
	}
	return reportf(deps.Out, "Next: agent-utils project --name %s loop tick --name %s\n", cfg.Name, loopName)
}

// conflictingProject returns a registered project that answers to name but is
// a DIFFERENT project than id, which is the collision that makes a name
// useless as a selector.
//
// A registry that cannot be read reports no conflict: the registry is an index
// (see registry.Register's own contract), and failing init because the index
// was unreadable would refuse a legitimate project over a check that is a
// guard, not the operation.
func conflictingProject(name, id string) (registry.Project, bool) {
	projects, err := registry.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not check the registry for a name collision: %v\n", err)
		return registry.Project{}, false
	}
	for _, p := range projects {
		if p.ID != id && strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return registry.Project{}, false
}

// existingLoopCount reports how many loop configurations the project already
// has, treating config.ErrNoConfigs as none rather than as a failure: init
// creates configs/ moments earlier, and an empty one is the normal state of a
// project being set up for the first time.
//
// Any other read failure also counts as none. The alternative -- failing init
// -- would refuse to record a project in the registry over a directory listing
// this command never actually needed, and the wizard that follows reports a
// real write problem far more clearly.
func existingLoopCount(agentUtilsDir string) int {
	entries, err := config.List(agentUtilsDir)
	if err != nil {
		if !errors.Is(err, config.ErrNoConfigs) {
			fmt.Fprintf(os.Stderr, "warning: could not list loop configurations: %v\n", err)
		}
		return 0
	}
	return len(entries)
}

// plural returns the "s" that makes a count read naturally.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// reportf writes one status line to out and wraps a write failure with
// enough context to say what could not be reported. Matches the treatment
// registerWebhooks already gives its own out writes: a caller that cannot
// see this line cannot tell what init or loop new actually did, so a failed
// write here is not swallowed the way the direct os.Stderr warnings
// elsewhere in this file are.
func reportf(out io.Writer, format string, args ...any) error {
	if _, err := fmt.Fprintf(out, format, args...); err != nil {
		return fmt.Errorf("write status: %w", err)
	}
	return nil
}

// mintProjectDescriptor creates the project's descriptor if it does not
// already exist, using name as the base identity when it is non-empty and
// falling back to the directory's own (slugged) basename otherwise.
//
// It calls project.EnsureNamed so both paths -- an explicit positional name
// and the directory-derived fallback -- report a rename via renamedFrom.
// Before this function existed, only the explicit-name path had a uniquify
// loop; the directory-derived path had none, so a taken directory-derived
// name could never say so.
func mintProjectDescriptor(agentUtilsDir, rootDir, name string) (*project.Config, bool, string, error) {
	base := name
	if base == "" {
		base = filepath.Base(rootDir)
	}
	return project.EnsureNamed(agentUtilsDir, project.Slug(base), registry.NameTaken)
}

// runLoopWizard runs the interactive setup wizard and writes the resulting
// loop configuration.
//
// Detect is given rootDir (the project's own directory, for its git-derived
// defaults) and Write is given agentUtilsDir (Write joins it with
// config.ConfigsSubdir itself) — the two are not interchangeable.
//
// Prompts go to os.Stderr, matching promptForConfig and
// confirmRegisterWebhook's existing convention in this file: a piped stdout
// stays machine readable even during an interactive wizard session.
func runLoopWizard(agentUtilsDir, rootDir string) (string, error) {
	cfg, err := wizard.Run(wizard.NewTerminalPrompter(os.Stdin, os.Stderr), wizard.Detect(rootDir))
	if err != nil {
		return "", err
	}
	return wizard.Write(agentUtilsDir, cfg)
}

// projectLoopNewCommand adds another loop configuration to an already
// initialised project via the setup wizard. It is the entry point for a
// second (or third...) loop, once `project init` has already created the
// project.
func projectLoopNewCommand() *cli.Command {
	return &cli.Command{
		Name:  "new",
		Usage: "add another loop configuration to this project via the setup wizard",
		Action: func(_ context.Context, c *cli.Command) error {
			p, err := openProject(c)
			if err != nil {
				return err
			}
			return projectLoopNewRun(projectLoopNewDeps{
				AgentUtilsDir: p.Dir,
				RootDir:       p.Root,
				Interactive:   isInteractive(),
				RunWizard:     runLoopWizard,
				Out:           os.Stdout,
			})
		},
	}
}

// projectLoopNewDeps mirrors projectInitDeps' shape for the same reason:
// projectLoopNewRun must be testable against a scripted RunWizard function
// without a real terminal.
type projectLoopNewDeps struct {
	AgentUtilsDir string
	RootDir       string
	Interactive   bool
	RunWizard     func(agentUtilsDir, rootDir string) (string, error)
	Out           io.Writer
}

// projectLoopNewRun runs the wizard and reports what it wrote.
//
// Unlike project init, `loop new` has no non-wizard work to fall back to —
// prompting is its entire job — so a non-interactive run is an outright
// error rather than a skip-and-continue. The message names the same rule
// resolveLoopConfig already documents: a prompt in a cron job would hang
// forever.
func projectLoopNewRun(deps projectLoopNewDeps) error {
	if !deps.Interactive {
		return errors.New(
			"refusing to prompt for a loop configuration in a non-interactive run: " +
				"run `agent-utils project loop new` from a terminal")
	}
	path, err := deps.RunWizard(deps.AgentUtilsDir, deps.RootDir)
	if err != nil {
		return err
	}
	return reportf(deps.Out, "Wrote loop configuration %s\n", path)
}

// registerWebhookCommand registers, with GitHub, the webhook endpoint that
// lets the daemon dispatch an agent for this project's loops.
//
// --name here selects a LOOP, and it shadows project's own --name (which
// selects the PROJECT): urfave/cli lets a child command declare a flag with
// the same name as its parent's, and the child's own value wins for
// c.String("name") read from the child. projectSessionsCommand does exactly
// this for the same reason, and selectedProject's doc comment states the
// general rule this command follows: resolve the project by walking the lineage
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

			records, closeRecords, err := openWebhookRecords(p.Config.ID)
			if err != nil {
				return err
			}
			defer closeRecords()

			token := os.Getenv("GITHUB_TOKEN")
			return registerWebhookRun(ctx, repos, registerWebhookDeps{
				Records: records,
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
	Hooks ghub.HookAdmin
	// Records is where the registration is written once GitHub confirms it.
	// Before it existed, registration left NOTHING on this machine naming the
	// hook it had just created.
	Records     webhookRecords
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

	return registerWebhooks(ctx, deps.Hooks, deps.Records, repos, deps.Settings, deps.Out)
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
func registerWebhooks(ctx context.Context, hooks ghub.HookAdmin, records webhookRecords, repos []string, s *settings.Settings, out io.Writer) error {
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
		// The RECORDED id is matched first, and only then the URL.
		//
		// Matching on URL alone is what orphaned hooks: change webhook.url,
		// re-run this command, and the hook already registered no longer
		// matches, so a SECOND hook is created while the first keeps
		// delivering to the dead endpoint -- with the record then overwritten
		// to name the new one, leaving nothing on this machine able to find
		// the old one again. Editing the recorded hook instead moves it to the
		// new URL, so a URL change repoints one hook rather than growing a
		// second. The URL match still runs for a repository registered before
		// the record existed, and for one another project registered.
		rec, hasRecord, err := records.Webhook(repo)
		if err != nil {
			return err
		}
		var found *ghub.Hook
		for i := range existing {
			if hasRecord && existing[i].ID == rec.HookID {
				found = &existing[i]
				break
			}
			if found == nil && existing[i].URL == s.Webhook.URL {
				found = &existing[i]
				// Keep looking rather than break: a recorded id later in the
				// list wins over a URL match, because it is the hook THIS
				// project registered.
			}
		}

		if found != nil {
			if err := hooks.EditHook(ctx, owner, name, found.ID, spec); err != nil {
				return err
			}
			if err := recordWebhook(records, repo, found.ID, s.Webhook.URL); err != nil {
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
		if err := recordWebhook(records, repo, id, s.Webhook.URL); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(out, "created %s (hook %d)\n", repo, id); err != nil {
			return fmt.Errorf("report creation for %s: %w", repo, err)
		}
	}
	return nil
}

// recordWebhook stores what was just registered, AFTER GitHub confirmed it.
//
// The order matters in both directions. Recording first would leave a row
// naming a hook a failed create never made, and the next deregistration would
// try to delete it. Not recording at all is what this whole feature fixes: a
// hook nothing on this machine names is an orphan the moment webhook.url
// changes.
//
// A failed write is returned rather than warned about, and the message says the
// hook IS registered: the operator has granted GitHub dispatch rights and has
// no local record of it, which is exactly the state they need to know about.
func recordWebhook(records webhookRecords, repo string, id int64, url string) error {
	err := records.PutWebhook(store.Webhook{
		Repo: repo, HookID: id, URL: url, RegisteredAt: time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf(
			"hook %d is registered on %s at GitHub, but recording it here failed: %w\n"+
				"re-run `agent-utils project register-webhook` once the state database is writable",
			id, repo, err)
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

// webhookRecords is the slice of the canonical state database the webhook
// commands need. It is an interface, not *store.Store, for the same reason
// ghub.HookAdmin is one: registration and deregistration can then be driven by
// a test with no $AGENT_UTILS_HOME, no sqlite file and no network.
type webhookRecords interface {
	// PutWebhook records a registration GitHub has already confirmed.
	PutWebhook(w store.Webhook) error
	// Webhook returns this project's record for a repository, if it has one.
	Webhook(repo string) (store.Webhook, bool, error)
	// DeleteWebhook forgets a registration, once GitHub confirms it is gone.
	DeleteWebhook(repo string) error
	// OtherHolders returns rows in OTHER projects that record the same hook.
	// Deleting a hook two projects share stops deliveries for both, so this is
	// what deregistration checks before it decides.
	OtherHolders(repo string, hookID int64) ([]store.Webhook, error)
}

// projectWebhooks adapts the canonical database to webhookRecords.
//
// The embedded *store.Store answers everything scoped to this project. Only
// OtherHolders crosses the project boundary, and it is written here rather than
// on Store deliberately: Store's contract is that a scoped caller can neither
// see nor touch another project's rows, so the machine-wide read lives on DB
// and this adapter is the one place that filters this project back out of it.
type projectWebhooks struct {
	*store.Store
	db        *store.DB
	projectID string
}

func (p projectWebhooks) OtherHolders(repo string, hookID int64) ([]store.Webhook, error) {
	all, err := p.db.WebhooksForHook(repo, hookID)
	if err != nil {
		return nil, err
	}
	var others []store.Webhook
	for _, w := range all {
		if w.ProjectID != p.projectID {
			others = append(others, w)
		}
	}
	return others, nil
}

// openWebhookRecords opens the canonical state database for one project and
// returns a closer the caller must defer.
//
// It opens the database directly rather than through loopcmd's migrating
// opener: the legacy per-loop files this program imports never held a webhook
// table, so there is nothing here for a migration to carry across, and a
// registration must not be blocked by an unrelated import failing.
func openWebhookRecords(projectID string) (webhookRecords, func(), error) {
	if _, err := home.EnsureDir(); err != nil {
		return nil, nil, err
	}
	path, err := home.StateDBPath()
	if err != nil {
		return nil, nil, err
	}
	db, err := store.Open(path)
	if err != nil {
		return nil, nil, err
	}
	records := projectWebhooks{Store: db.Project(projectID), db: db, projectID: projectID}
	return records, func() { _ = db.Close() }, nil
}

// deregisterWebhookCommand removes, at GitHub, the webhook this project
// registered, and forgets the record of it.
//
// --name here selects a LOOP, and it shadows project's own --name (which
// selects the PROJECT), exactly as registerWebhookCommand's doc comment
// explains: urfave/cli lets a child command declare a flag with the same name
// as its parent's, and the child's own value wins for c.String("name") read
// from the child.
func deregisterWebhookCommand() *cli.Command {
	return &cli.Command{
		Name:  "deregister-webhook",
		Usage: "delete this project's GitHub webhooks and forget the recorded registrations",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "name", Usage: "restrict to one loop; omit to deregister every loop's repository"},
			&cli.BoolFlag{Name: "yes", Usage: "skip the confirmation prompt"},
			&cli.BoolFlag{Name: "force", Usage: "delete a hook another project also records, stopping its deliveries too"},
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

			records, closeRecords, err := openWebhookRecords(p.Config.ID)
			if err != nil {
				return err
			}
			defer closeRecords()

			token := os.Getenv("GITHUB_TOKEN")
			return deregisterWebhookRun(ctx, repos, deregisterWebhookDeps{
				Records: records,
				// ghub.New never talks to the network by itself; the token
				// check inside deregisterWebhookRun runs before any of
				// ListHooks or DeleteHook is called on this client.
				Hooks:       ghub.New(token),
				Settings:    s,
				Token:       token,
				Yes:         c.Bool("yes"),
				Force:       c.Bool("force"),
				Interactive: isInteractive(),
				Confirm:     confirmDeregisterWebhook,
				Out:         os.Stdout,
			})
		},
	}
}

// deregisterWebhookDeps mirrors registerWebhookDeps for the same reason: the
// validation, confirmation, GitHub-call and record-clearing sequence has to be
// drivable by a test with no real project, terminal, token or network.
type deregisterWebhookDeps struct {
	Records  webhookRecords
	Hooks    ghub.HookAdmin
	Settings *settings.Settings
	Token    string
	Yes      bool
	// Force deletes a hook another project also records. Without it that case
	// is a refusal, because the delete would silently stop their deliveries.
	Force       bool
	Interactive bool
	// Confirm asks the operator to approve, and is called only when Yes is
	// false and Interactive is true.
	Confirm func(repos []string) (bool, error)
	Out     io.Writer
}

// deregisterWebhookRun validates, confirms, and then deletes repos' webhooks.
//
// It deliberately does NOT require webhook.url or webhook.secret the way
// register-webhook does. Deleting needs neither: the hook is addressed by its
// recorded id, and no secret is written. Demanding them would refuse to clean
// up after an operator who has already unset the webhook configuration, which
// is precisely when an orphaned hook is left delivering to a dead endpoint.
func deregisterWebhookRun(ctx context.Context, repos []string, deps deregisterWebhookDeps) error {
	if deps.Token == "" {
		return errors.New("GITHUB_TOKEN is not set")
	}

	if !deps.Yes {
		// The same rule register-webhook and resolveLoopConfig already state: a
		// prompt in a cron job would hang forever. This command deletes state
		// at GitHub, so the refusal has to come before any call.
		if !deps.Interactive {
			return fmt.Errorf(
				"refusing to delete a webhook without confirmation in a non-interactive run: %s\n"+
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

	for _, repo := range repos {
		if err := deregisterOne(ctx, repo, deps); err != nil {
			return err
		}
	}
	return nil
}

// deregisterOne removes one repository's webhook and forgets the record of it.
func deregisterOne(ctx context.Context, repo string, deps deregisterWebhookDeps) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		// config.Load already validates repo is owner/name form; reaching this
		// means Entry.Repo was built some other way. Fail loudly rather than
		// calling GitHub with a nonsense owner. Mirrors registerWebhooks.
		return fmt.Errorf("repo %q is not in owner/name form", repo)
	}

	hookID, recorded, found, err := resolveHookToDelete(ctx, owner, name, repo, deps)
	if err != nil {
		return err
	}
	if !found {
		return reportf(deps.Out,
			"%s: nothing to deregister (no recorded registration, and no hook at GitHub delivering to %s)\n",
			repo, deliveryTarget(deps.Settings))
	}

	if err := checkSharedHook(repo, hookID, deps); err != nil {
		return err
	}

	err = deps.Hooks.DeleteHook(ctx, owner, name, hookID)
	switch {
	case err == nil:
		// The record is cleared only now, after GitHub confirmed: clearing it
		// on a failed delete would lose the id while the hook kept delivering,
		// leaving nothing on this machine able to name it again.
		if recorded {
			if err := deps.Records.DeleteWebhook(repo); err != nil {
				return err
			}
		}
		if recorded {
			return reportf(deps.Out, "deleted %s (hook %d)\n", repo, hookID)
		}
		return reportf(deps.Out,
			"deleted %s (hook %d, found by matching webhook.url: no registration was recorded for it)\n",
			repo, hookID)

	case errors.Is(err, ghub.ErrHookNotFound):
		// The operator deleted it in GitHub's UI and is now tidying up. Failing
		// here would leave a recorded row that nothing on this machine can ever
		// clear. The scope reading of a 404 is named in the message rather than
		// acted on, because GitHub reports both causes identically and only the
		// operator can tell them apart.
		if recorded {
			if err := deps.Records.DeleteWebhook(repo); err != nil {
				return err
			}
		}
		return reportf(deps.Out,
			"%s: hook %d was already gone at GitHub; removed the local record\n"+
				"  (if deliveries continue, the token may lack the admin:repo_hook scope rather than the hook being absent)\n",
			repo, hookID)

	default:
		return err
	}
}

// resolveHookToDelete finds which hook to delete, and reports whether the
// answer came from this project's record.
//
// The recorded id is preferred and no listing happens at all when there is one:
// that is the entire point of recording it. After `config set webhook.url`, the
// live hook still points at the PREVIOUS endpoint, so a URL match would fail to
// find exactly the orphan the operator came here to remove.
//
// The URL fallback exists only for a repository registered before the record
// existed. It is reported as such by the caller so the operator can tell which
// path ran.
func resolveHookToDelete(ctx context.Context, owner, name, repo string, deps deregisterWebhookDeps) (int64, bool, bool, error) {
	rec, ok, err := deps.Records.Webhook(repo)
	if err != nil {
		return 0, false, false, err
	}
	if ok {
		return rec.HookID, true, true, nil
	}

	url := strings.TrimSpace(deps.Settings.Webhook.URL)
	if url == "" {
		// Nothing recorded and nothing to match against. Saying so is more use
		// than an error: there is no action left that this command could take.
		return 0, false, false, nil
	}
	existing, err := deps.Hooks.ListHooks(ctx, owner, name)
	if err != nil {
		return 0, false, false, err
	}
	for _, h := range existing {
		if h.URL == url {
			return h.ID, false, true, nil
		}
	}
	return 0, false, false, nil
}

// deliveryTarget names webhook.url for a report, or says it is unset.
func deliveryTarget(s *settings.Settings) string {
	if url := strings.TrimSpace(s.Webhook.URL); url != "" {
		return url
	}
	return "webhook.url, which is not set"
}

// checkSharedHook refuses to delete a hook another project also records.
//
// Two projects can watch one repository through one webhook.url: registering
// from the first creates the hook, registering from the second FINDS it by URL
// and edits it, and both then record the same id. Deleting it on behalf of one
// silently stops deliveries for the other, and nothing in that project's own
// state would say why it went quiet.
//
// Refusing and naming the candidates rather than guessing mirrors the ambiguity
// rule registry.Find already follows. --force overrides, and says plainly who
// just lost delivery.
func checkSharedHook(repo string, hookID int64, deps deregisterWebhookDeps) error {
	holders, err := deps.Records.OtherHolders(repo, hookID)
	if err != nil {
		return err
	}
	if len(holders) == 0 {
		return nil
	}
	if !deps.Force {
		return sharedHookErr(repo, hookID, holders)
	}
	// Their rows are deliberately left alone: this project's view cannot write
	// another's, and a stale row is self-healing -- running deregister-webhook
	// there gets a 404 from GitHub and clears it.
	return reportf(deps.Out,
		"warning: hook %d on %s is also recorded by %d other project(s); --force deletes it anyway, "+
			"and they stop receiving deliveries:%s\n"+
			"  run `agent-utils project --name <project> deregister-webhook` in each to clear their records\n",
		hookID, repo, len(holders), describeHolders(holders))
}

// sharedHookErr names every other project that records the hook, with its path,
// because the operator cannot decide what to do about a project they cannot
// identify. It follows registry.ambiguousNameErr's shape for that reason.
func sharedHookErr(repo string, hookID int64, holders []store.Webhook) error {
	return fmt.Errorf(
		"refusing to delete hook %d on %s: %d other project(s) record the same hook, "+
			"and deleting it stops their webhook deliveries too:%s\n"+
			"pass --force to delete it anyway",
		hookID, repo, len(holders), describeHolders(holders))
}

// describeHolders renders one indented line per project that records a hook.
//
// A project missing from the registry is reported by its identifier alone. The
// registry is an index, never a source of truth (see its package comment), so a
// project absent from it must not silently disappear from a refusal whose whole
// job is to list who is affected.
func describeHolders(holders []store.Webhook) string {
	known := map[string]registry.Project{}
	if projects, err := registry.List(); err == nil {
		for _, p := range projects {
			known[p.ID] = p
		}
	}
	var b strings.Builder
	for _, h := range holders {
		if p, ok := known[h.ProjectID]; ok {
			fmt.Fprintf(&b, "\n  %s (%s) at %s", p.Name, h.ProjectID, p.Root)
			continue
		}
		fmt.Fprintf(&b, "\n  %s (not in the registry)", h.ProjectID)
	}
	return b.String()
}

// confirmDeregisterWebhook asks the operator to approve deleting the webhooks
// for repos. It is called only when isInteractive() is true; see
// deregisterWebhookRun.
//
// The prompt goes to stderr, matching confirmRegisterWebhook, so a piped stdout
// stays machine readable.
func confirmDeregisterWebhook(repos []string) (bool, error) {
	fmt.Fprintln(os.Stderr, "This deletes the GitHub webhook on:")
	for _, r := range repos {
		fmt.Fprintf(os.Stderr, "  %s\n", r)
	}
	fmt.Fprintln(os.Stderr, "Deliveries stop immediately; the loops keep running only under cron.")
	fmt.Fprint(os.Stderr, "Continue? [y/N] ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	choice := strings.ToLower(strings.TrimSpace(line))
	return choice == "y" || choice == "yes", nil
}
