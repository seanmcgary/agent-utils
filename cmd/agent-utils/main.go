// Command agent-utils holds utilities for agent workflows.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/loopcmd"
	"github.com/seanmcgary/agent-utils/internal/migrate"
	"github.com/seanmcgary/agent-utils/internal/registry"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/seanmcgary/agent-utils/internal/version"
	"github.com/urfave/cli/v3"
	"golang.org/x/term"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cmd := &cli.Command{
		Name:    "agent-utils",
		Usage:   "utilities for agent workflows",
		Version: version.GetVersion(),
		Commands: []*cli.Command{
			// Top level spans the machine.
			listCommand(),
			sessionsCommand(),
			logsCommand(),
			forgetCommand(),
			migrateCommand(),
			versionCommand(),
			configCommand(),
			listenerCommand(),
			// Everything project-scoped lives under `project`.
			projectCommand(),
			internalCommand(),
		},
	}
	if err := cmd.Run(context.Background(), os.Args); err != nil {
		// Plain text to stderr, NOT slog. The default handler here is a JSON
		// one, and several of this program's errors are deliberately
		// multi-line: the retry.backoff_ticks migration message, the "run
		// `agent-utils project init`" guidance, the launchd writable-path
		// refusal, and the missing env file all print a command for the
		// operator to run. Through a JSON handler those arrive as one line
		// with literal \n escapes, which makes the remediation unreadable
		// exactly when it is needed. Structured logging is for the tick's own
		// records; a fatal is for a human at a terminal.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func versionCommand() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "print the version and the commit this binary was built from",
		Action: func(_ context.Context, _ *cli.Command) error {
			fmt.Printf("agent-utils %s (%s)\n", version.GetVersion(), version.GetCommit())
			return nil
		},
	}
}

// projectSelectorFlag names the project a command acts on, so it works from any
// directory. Omitted, the project in the current directory is used.
func projectSelectorFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    "name",
		Aliases: []string{"project"},
		Usage:   "project to act on; omit to use the project in the current directory",
	}
}

// selectedProject reads the project selector off the `project` command itself.
//
// It cannot use c.String("name"): the loop subcommands define their OWN --name
// for the loop, and urfave/cli lets a child shadow a parent's flag of the same
// name. Reading the flag from the command that declares it is what keeps
// `project --name web loop tick --name planning` unambiguous.
func selectedProject(c *cli.Command) string {
	for _, cmd := range c.Lineage() {
		if cmd.Name == "project" {
			return cmd.String("name")
		}
	}
	return ""
}

// openProject resolves the project to act on.
//
// It used to also report when the call had minted a fresh descriptor, back
// when ResolveProject could onboard a directory implicitly. `agent-utils
// project init` (project.go) owns that reporting now: ResolveProject never
// creates a descriptor, so there is nothing here left to report.
func openProject(c *cli.Command) (*loopcmd.Project, error) {
	return loopcmd.ResolveProject(selectedProject(c))
}

// refOf names the project a command acts for.
func refOf(p *loopcmd.Project) loopcmd.ProjectRef {
	return loopcmd.ProjectRef{ID: p.Config.ID, Name: p.Config.Name, Dir: p.Dir}
}

func configFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name: "config",
		Usage: "path to a loop configuration file. Omit it to select one by name " +
			"from " + config.DirName + "/" + config.ConfigsSubdir,
	}
}

// nameFlag selects a configuration by name from the local .agent-utils
// directory, as an alternative to giving a path with --config.
func nameFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name: "name",
		Usage: "name of a loop configuration in " + config.DirName + "/" +
			config.ConfigsSubdir + " (without the .yaml extension)",
	}
}

// resolveConfigPath decides which configuration file a command should use.
//
// Precedence:
//  1. --config, an explicit path. cron should use this: it depends on no
//     directory layout and never prompts.
//  2. --name, resolved against the local .agent-utils directory.
//  3. The only configuration present, when there is exactly one.
//  4. An interactive choice, but ONLY when stdin is a terminal. A prompt in a
//     cron job would hang forever, so a non-interactive run gets an error
//     listing the names instead.
//
// resolveLoopConfig decides which loop configuration a command should use,
// within an already-resolved project.
//
// Precedence: an explicit --config path, then --name, then the only loop
// present, then an interactive choice. The prompt appears only on a terminal;
// a cron run gets an error listing the names instead.
func resolveLoopConfig(c *cli.Command, dir string) (string, error) {
	path, name := c.String("config"), c.String("name")

	// Both set is ambiguous. Silently preferring one would hide a mistake in a
	// cron entry until it had been running against the wrong loop for a while.
	if path != "" && name != "" {
		return "", errors.New("--config and --name are alternatives; pass only one")
	}
	if path != "" {
		return path, nil
	}

	if name != "" {
		return config.Resolve(dir, name)
	}

	entries, err := config.List(dir)
	if err != nil {
		return "", err
	}
	if len(entries) == 1 {
		return entries[0].Path, nil
	}
	if !isInteractive() {
		// Suggest a config that actually loads. Pointing the operator at a
		// broken one just moves the confusion one command along.
		example := entries[0].Name
		for _, e := range entries {
			if e.Err == nil {
				example = e.Name
				break
			}
		}
		// FullName already includes the root command name.
		return "", fmt.Errorf("%w: %s\n\nName one, for example:\n  %s --name %s",
			config.ErrAmbiguous, strings.Join(config.Names(entries), ", "),
			c.FullName(), example)
	}
	return promptForConfig(entries)
}

// isInteractive reports whether stdin is a terminal. This is what keeps a
// prompt out of a cron job.
//
// It asks the terminal driver rather than checking os.ModeCharDevice. That mode
// bit is set for /dev/null, which is exactly what cron attaches to stdin, so the
// simpler check calls a cron run interactive and prompts into a void.
func isInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// promptForConfig asks which configuration to use. Prompts go to stderr so a
// piped stdout stays machine readable.
func promptForConfig(entries []config.Entry) (string, error) {
	fmt.Fprintln(os.Stderr, "Select a loop configuration:")
	for i, e := range entries {
		suffix := e.Repo
		if e.Err != nil {
			suffix = "INVALID: " + e.Err.Error()
		}
		fmt.Fprintf(os.Stderr, "  %d) %-20s %s\n", i+1, e.Name, suffix)
	}
	fmt.Fprintf(os.Stderr, "Enter a number or a name: ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read selection: %w", err)
	}
	choice := strings.TrimSpace(line)
	if choice == "" {
		return "", errors.New("no configuration selected")
	}

	if n, err := strconv.Atoi(choice); err == nil {
		if n < 1 || n > len(entries) {
			return "", fmt.Errorf("selection %d is out of range 1-%d", n, len(entries))
		}
		return entries[n-1].Path, nil
	}
	for _, e := range entries {
		if e.Name == choice {
			return e.Path, nil
		}
	}
	return "", fmt.Errorf("%w %q", config.ErrNotFound, choice)
}

// projectStatusCommand reports every project this tool has been used against.
// It reads only local state, so it needs no token and works offline.
// listCommand reports every project registered on this machine. It reads only
// local state, so it needs no token and works offline.
// projectListCommand prints one project's loop configurations.
func projectListCommand() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list this project's loop configurations",
		Action: func(_ context.Context, c *cli.Command) error {
			p, err := openProject(c)
			if err != nil {
				return err
			}
			entries, err := config.List(p.Dir)
			if err != nil {
				return err
			}
			fmt.Print(loopcmd.RenderConfigs(p, entries))
			return nil
		},
	}
}

// projectStatusCommand describes one project: its identity, its configurations
// and the state of each loop.
func projectStatusCommand() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "describe this project: identity, configurations and loop state",
		Action: func(_ context.Context, c *cli.Command) error {
			p, err := openProject(c)
			if err != nil {
				return err
			}
			detail, err := loopcmd.Describe(p)
			if err != nil {
				return err
			}
			fmt.Print(loopcmd.RenderProjectDetail(detail))
			return nil
		},
	}
}

// projectSessionsCommand groups everything about claude sessions. A session
// spans the resumes of one issue, so it is a different unit from a dispatch.
func projectSessionsCommand() *cli.Command {
	return &cli.Command{
		Name:  "sessions",
		Usage: "inspect the claude sessions this project has created",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list every session with its issue, runs, cost and state",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "name", Usage: "restrict to one loop"},
				},
				Action: func(_ context.Context, c *cli.Command) error {
					p, err := openProject(c)
					if err != nil {
						return err
					}
					sessions, err := loopcmd.Sessions(p, c.String("name"))
					if err != nil {
						return err
					}
					fmt.Print(loopcmd.RenderSessions(p, sessions))
					return nil
				},
			},
		},
	}
}

// sessionsCommand reports the claude sessions on this machine.
//
// It is a separate command from projectSessionsCommand rather than the same one
// registered twice: the top level is the machine-wide scope, so this one takes a
// --project selector the project-scoped twin has no use for, and it prints a
// table with a PROJECT column through a different renderer. Sharing a
// constructor would mean one command whose flags and output both depend on where
// it was registered.
func sessionsCommand() *cli.Command {
	return &cli.Command{
		Name:  "sessions",
		Usage: "inspect the claude sessions on this machine",
		Commands: []*cli.Command{
			{
				Name:  "list",
				Usage: "list every session with its project, issue, runs, cost and state",
				// The loop selector is spelled --loop here and --name under
				// `project sessions list`. At the top level the project
				// selector and the loop selector have to coexist on one
				// command, and two flags cannot both be called name. The
				// per-project twin has no --project flag to collide with,
				// because `project --name` sits on the parent -- see the
				// selectedProject comment above, which explains the shadowing
				// that forced that older spelling. Deliberately no alias: the
				// two commands are different surfaces, not one surface with two
				// spellings.
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "project",
						Usage: "restrict to one project, by name, id or path"},
					&cli.StringFlag{Name: "loop",
						Usage: "restrict to loops with this name"},
					&cli.BoolFlag{Name: "running",
						Usage: "restrict to sessions whose agent is still alive"},
					&cli.BoolFlag{Name: "orphaned",
						Usage: "restrict to sessions marked running whose process is gone"},
				},
				Action: func(_ context.Context, c *cli.Command) error {
					filter := loopcmd.SessionFilter{
						Project:  c.String("project"),
						Loop:     c.String("loop"),
						Running:  c.Bool("running"),
						Orphaned: c.Bool("orphaned"),
					}
					sessions, err := loopcmd.AllSessions(filter)
					if err != nil {
						return err
					}
					fmt.Print(loopcmd.RenderAllSessions(sessions, filter))
					return nil
				},
			},
			sessionsKillCommand(),
			sessionsResumeCommand(),
		},
	}
}

// selectorFlags are the three mutually exclusive targets, plus the two
// flags that narrow --issue and --all, shared by `sessions kill` and
// `sessions resume`. Every flag carries a Usage string, matching the
// convention `sessions list`'s flags set above.
func selectorFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "project",
			Usage: "restrict to one project, by name, id or path"},
		&cli.StringFlag{Name: "loop",
			Usage: "restrict --issue and --all to loops with this name"},
		&cli.StringFlag{Name: "session",
			Usage: "act on this session id"},
		&cli.IntFlag{Name: "issue",
			Usage: "act on this issue number (needs a project, and a loop if the number is ambiguous)"},
		&cli.BoolFlag{Name: "all",
			Usage: "act on every matching target; destructive, so it requires --yes outside a terminal"},
		&cli.BoolFlag{Name: "yes",
			Usage: "skip the confirmation prompt --all would otherwise print"},
	}
}

// selectorFrom reads the shared selector flags off c. --loop is spelled the
// same as `sessions list`'s own --loop (see that command's flag comment for
// why the top level does not alias --name here too).
func selectorFrom(c *cli.Command) loopcmd.Selector {
	return loopcmd.Selector{
		Project: c.String("project"),
		Loop:    c.String("loop"),
		Session: c.String("session"),
		Issue:   c.Int("issue"),
		All:     c.Bool("all"),
	}
}

// sessionsKillCommand stops a running session: it holds the issue (so the
// next tick does not dispatch it again) and signals the runner. See
// sessionsKillRun for the guard order.
func sessionsKillCommand() *cli.Command {
	flags := selectorFlags()
	flags = append(flags,
		&cli.BoolFlag{Name: "force",
			Usage: "SIGKILL the agent's process group and the runner, instead of waiting for a graceful exit"},
		&cli.DurationFlag{Name: "timeout", Value: loopcmd.DefaultKillTimeout,
			Usage: "how long to wait for the runner to exit after SIGTERM before reporting it still alive"},
	)
	return &cli.Command{
		Name:  "kill",
		Usage: "stop a running session: hold its issue and signal the runner",
		Flags: flags,
		Action: func(_ context.Context, c *cli.Command) error {
			args := killArgs{
				Selector: selectorFrom(c),
				Yes:      c.Bool("yes"),
				Force:    c.Bool("force"),
				Timeout:  c.Duration("timeout"),
			}
			if isInteractive() {
				args.Confirm = confirmSessionAction
			}
			return sessionsKillRun(args)
		},
	}
}

// sessionsResumeCommand clears the stopped flag a kill (or an invalid
// override label) left on an issue.
func sessionsResumeCommand() *cli.Command {
	return &cli.Command{
		Name:  "resume",
		Usage: "clear the stopped flag on an issue, so the loop may dispatch it again",
		Flags: selectorFlags(),
		Action: func(_ context.Context, c *cli.Command) error {
			args := killArgs{
				Selector: selectorFrom(c),
				Yes:      c.Bool("yes"),
			}
			if isInteractive() {
				args.Confirm = confirmSessionAction
			}
			return sessionsResumeRun(args)
		},
	}
}

// killArgs bundles sessions kill/resume's inputs so the ordered guard
// sequence in sessionsKillRun/sessionsResumeRun -- Selector.Validate, then
// the destructive --all gate -- can be driven directly by a test, with a
// canned Confirm and no real terminal.
type killArgs struct {
	Selector loopcmd.Selector
	Yes      bool
	// Force and Timeout are read by kill only; resume ignores them.
	Force   bool
	Timeout time.Duration
	// Confirm asks the operator to approve a destructive --all. It is a
	// function FIELD, not a direct call, which is what makes the gate
	// testable without a tty -- the same seam registerWebhookRun uses
	// (project.go:519). It is set only when isInteractive() is true; a
	// non-interactive run leaves it nil, which the gate below treats as "no
	// way to ask".
	Confirm func(desc string) (bool, error)
}

// confirmDestructiveAll applies the --all gate as ONE branch: anything other
// than a bare, unconfirmed --all proceeds; a non-interactive run (Confirm ==
// nil) refuses and names --yes; otherwise the operator is asked, and a
// decline is reported back as (false, nil) -- not an error -- so the caller
// prints nothing and exits clean.
func confirmDestructiveAll(args killArgs) (bool, error) {
	if !args.Selector.All || args.Yes {
		return true, nil
	}
	if args.Confirm == nil {
		return false, errors.New(
			"refusing --all without confirmation in a non-interactive run; pass --yes")
	}
	return args.Confirm(args.Selector.Describe())
}

// allFailed reports whether every result in rs failed. sessionsKillRun and
// sessionsResumeRun use it to decide their own exit status: a partial
// failure already printed, per target, exactly what went wrong, so the
// command still exits 0 -- only a total loss is worth a nonzero exit.
func allFailed(rs []loopcmd.Result) bool {
	if len(rs) == 0 {
		return false
	}
	for _, r := range rs {
		if r.Action != loopcmd.ActionFailed {
			return false
		}
	}
	return true
}

// sessionsKillRun applies, in order: Selector.Validate (a bad selector must
// fail before anything opens the database), the destructive --all gate, and
// then loopcmd.Kill. It returns an error only when every target failed.
func sessionsKillRun(args killArgs) error {
	if err := args.Selector.Validate(); err != nil {
		return err
	}
	proceed, err := confirmDestructiveAll(args)
	if err != nil {
		return err
	}
	if !proceed {
		return nil
	}
	results, err := loopcmd.Kill(loopcmd.KillOptions{
		Selector: args.Selector, Force: args.Force, Timeout: args.Timeout,
	})
	if err != nil {
		return err
	}
	fmt.Print(loopcmd.RenderResults("kill", results))
	if allFailed(results) {
		return fmt.Errorf("every target failed to kill")
	}
	return nil
}

// sessionsResumeRun mirrors sessionsKillRun for `sessions resume`.
func sessionsResumeRun(args killArgs) error {
	if err := args.Selector.Validate(); err != nil {
		return err
	}
	proceed, err := confirmDestructiveAll(args)
	if err != nil {
		return err
	}
	if !proceed {
		return nil
	}
	results, err := loopcmd.Resume(args.Selector)
	if err != nil {
		return err
	}
	fmt.Print(loopcmd.RenderResults("resume", results))
	if allFailed(results) {
		return fmt.Errorf("every target failed to resume")
	}
	return nil
}

// confirmSessionAction prompts before a destructive --all kill or resume. It
// follows confirmRegisterWebhook's shape: prompt to stderr, read one line
// from stdin, accept only "y"/"yes".
func confirmSessionAction(desc string) (bool, error) {
	fmt.Fprintf(os.Stderr, "This will act on %s.\n", desc)
	fmt.Fprint(os.Stderr, "Continue? [y/N] ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	choice := strings.ToLower(strings.TrimSpace(line))
	return choice == "y" || choice == "yes", nil
}

// projectCommand groups everything scoped to one project. Naming it explicitly
// is what makes the top level unambiguously machine-wide.
func projectCommand() *cli.Command {
	return &cli.Command{
		Name:  "project",
		Usage: "act on one project: its loops, configurations and logs",
		Flags: []cli.Flag{projectSelectorFlag()},
		Commands: []*cli.Command{
			projectInitCommand(),
			projectStatusCommand(),
			projectListCommand(),
			projectSessionsCommand(),
			logsCommand(),
			loopCommand(),
			registerWebhookCommand(),
			deregisterWebhookCommand(),
		},
	}
}

// forgetCommand removes a project from the registry. It touches nothing the
// project owns, so it is safe to run against a directory that has moved.
func forgetCommand() *cli.Command {
	return &cli.Command{
		Name:      "forget",
		Usage:     "remove a project from the registry without touching its files",
		Arguments: []cli.Argument{&cli.StringArg{Name: "project"}},
		Action: func(_ context.Context, c *cli.Command) error {
			selector := c.StringArg("project")
			if selector == "" {
				return errors.New("usage: agent-utils forget <project name, id or path>")
			}
			if err := registry.ForgetSelector(selector); err != nil {
				return err
			}
			fmt.Printf("forgot %s\n", selector)
			return nil
		},
	}
}

func logsCommand() *cli.Command {
	return &cli.Command{
		Name:  "logs",
		Usage: "show the log of a dispatched agent, live or after the fact",
		Flags: []cli.Flag{
			configFlag(),
			nameFlag(),
			&cli.StringFlag{Name: "session",
				Usage: "show the dispatches that used this claude session id"},
			&cli.IntFlag{Name: "issue", Usage: "show the newest dispatch for this issue"},
			&cli.IntFlag{Name: "dispatch", Usage: "show this dispatch id exactly"},
			&cli.BoolFlag{Name: "follow", Aliases: []string{"f"},
				Usage: "keep streaming while the agent is alive"},
			&cli.BoolFlag{Name: "list", Aliases: []string{"l"},
				Usage: "list recent dispatches and their ids instead of showing a log"},
			&cli.BoolFlag{Name: "raw", Usage: "print the stream-json verbatim"},
			&cli.BoolFlag{Name: "thinking", Usage: "include the agent's thinking blocks"},
			&cli.BoolFlag{Name: "stderr", Usage: "show the agent's standard error instead"},
			&cli.BoolFlag{Name: "runner", Usage: "show the runner's own log instead"},
			&cli.BoolFlag{Name: "path", Usage: "print the log file path and exit"},
			&cli.IntFlag{Name: "limit", Value: 20, Usage: "how many dispatches --list shows"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			p, err := openProject(c)
			if err != nil {
				return err
			}

			// A session identifier names its own loop, so --session alone is
			// enough; asking for --name too would demand what the id already
			// determines.
			var path string
			if sess := c.String("session"); sess != "" && c.String("name") == "" && c.String("config") == "" {
				_, path, err = loopcmd.FindSession(p, sess)
			} else {
				path, err = resolveLoopConfig(c, p.Dir)
			}
			if err != nil {
				return err
			}
			// Reading logs needs no GitHub access.
			cfg, deps, cleanup, err := loopcmd.Open(refOf(p), path, loopcmd.Options{
				Token:           os.Getenv("GITHUB_TOKEN"),
				RequireGitHub:   false,
				MigrationPolicy: loopcmd.WarnOnUnimported,
			})
			if err != nil {
				return err
			}
			defer cleanup()

			if c.Bool("list") {
				ds, err := deps.Store.RecentDispatches(
					cfg.Name, cfg.Repo, c.Int("issue"), c.Int("limit"))
				if err != nil {
					return err
				}
				fmt.Print(loopcmd.RenderDispatchList(ds))
				return nil
			}

			stream := loopcmd.StreamAgent
			switch {
			case c.Bool("stderr"):
				stream = loopcmd.StreamStderr
			case c.Bool("runner"):
				stream = loopcmd.StreamRunner
			}

			opts := loopcmd.LogOptions{
				Session:  c.String("session"),
				Issue:    c.Int("issue"),
				Dispatch: int64(c.Int("dispatch")),
				Stream:   stream,
				Follow:   c.Bool("follow"),
				Raw:      c.Bool("raw"),
				Thinking: c.Bool("thinking"),
				Harness:  cfg.Agent.Harness,
			}

			d, err := loopcmd.SelectDispatch(deps.Store, cfg, opts)
			if err != nil {
				return err
			}
			logPath := loopcmd.LogPathFor(cfg, d, stream)
			if c.Bool("path") {
				fmt.Println(logPath)
				return nil
			}

			fmt.Fprintf(os.Stderr, "dispatch %d  issue #%d  %s  %s\n%s\n\n",
				d.ID, d.Number, d.Kind, d.Status, logPath)
			return loopcmd.Tail(ctx, os.Stdout, logPath, d, opts)
		},
	}
}

// listCommand reports every project registered on this machine. It reads only
// local state, so it needs no token and works offline.
func listCommand() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list every project on this machine and the state of its loops",
		Action: func(_ context.Context, _ *cli.Command) error {
			projects, err := loopcmd.Projects()
			if err != nil {
				return err
			}
			fmt.Print(loopcmd.RenderProjects(projects))
			return nil
		},
	}
}

// migrateCommand imports the state of the old per-loop layout, for the whole
// machine, and prints what it did.
//
// It sweeps every registered project, so it belongs at the top level rather than
// under `project`. It exists for an operator who wants the report, not because
// anything waits on it: every command migrates the project it touches, so the
// import happens whether or not this is ever run.
func migrateCommand() *cli.Command {
	return &cli.Command{
		Name: "migrate",
		Usage: "import state left by the old per-loop databases; not required, " +
			"a project is migrated the first time a command touches it",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "dry-run",
				Usage: "report what would be imported and write nothing"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			if _, err := home.EnsureDir(); err != nil {
				return err
			}
			dbPath, err := home.StateDBPath()
			if err != nil {
				return err
			}
			db, err := store.Open(dbPath)
			if err != nil {
				return err
			}
			defer db.Close()

			dryRun := c.Bool("dry-run")
			report, err := migrate.Sweep(db, migrate.Options{DryRun: dryRun})
			if err != nil {
				return fmt.Errorf("sweep the machine for unimported state: %w", err)
			}
			fmt.Print(loopcmd.RenderMigrateReport(report, dryRun))

			// Err names every failure already. Rebuilding that message here would
			// let the two drift apart, and the write path prints the same one.
			return report.Err()
		},
	}
}

func loopCommand() *cli.Command {
	return &cli.Command{
		Name:  "loop",
		Usage: "run and inspect an issue-driven agent loop",
		Commands: []*cli.Command{
			projectLoopNewCommand(),
			{
				Name:  "tick",
				Usage: "run one reconcile and dispatch pass, then exit",
				Flags: []cli.Flag{configFlag(), nameFlag()},
				Action: func(ctx context.Context, c *cli.Command) error {
					p, err := openProject(c)
					if err != nil {
						return err
					}
					path, err := resolveLoopConfig(c, p.Dir)
					if err != nil {
						return err
					}
					cfg, deps, cleanup, err := loopcmd.Open(refOf(p), path, loopcmd.Options{
						Token:           os.Getenv("GITHUB_TOKEN"),
						RequireGitHub:   true,
						MigrationPolicy: loopcmd.FailOnUnimported,
					})
					if err != nil {
						return err
					}
					defer cleanup()

					_, err = loopcmd.RunTick(ctx, cfg, deps)
					if errors.Is(err, lock.ErrHeld) {
						slog.Info("another tick is running; exiting", "loop", cfg.Name)
						return nil
					}
					return err
				},
			},
			{
				Name:  "status",
				Usage: "print the reconciled view without changing anything",
				Flags: []cli.Flag{configFlag(), nameFlag()},
				Action: func(ctx context.Context, c *cli.Command) error {
					p, err := openProject(c)
					if err != nil {
						return err
					}
					path, err := resolveLoopConfig(c, p.Dir)
					if err != nil {
						return err
					}
					cfg, deps, cleanup, err := loopcmd.Open(refOf(p), path, loopcmd.Options{
						Token:           os.Getenv("GITHUB_TOKEN"),
						RequireGitHub:   true,
						MigrationPolicy: loopcmd.FailOnUnimported,
					})
					if err != nil {
						return err
					}
					defer cleanup()

					out, err := loopcmd.Status(ctx, cfg, deps)
					if err != nil {
						return err
					}
					fmt.Print(out)
					return nil
				},
			},
			{
				Name:  "reset",
				Usage: "drop the stored session and worktree for one issue",
				Flags: []cli.Flag{
					configFlag(),
					nameFlag(),
					&cli.IntFlag{Name: "issue", Usage: "issue number", Required: true},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					p, err := openProject(c)
					if err != nil {
						return err
					}
					path, err := resolveLoopConfig(c, p.Dir)
					if err != nil {
						return err
					}
					cfg, deps, cleanup, err := loopcmd.Open(refOf(p), path, loopcmd.Options{
						Token:           os.Getenv("GITHUB_TOKEN"),
						RequireGitHub:   false,
						MigrationPolicy: loopcmd.FailOnUnimported,
					})
					if err != nil {
						return err
					}
					defer cleanup()

					// Take the same lock a tick takes. Without it a reset can
					// delete a worktree while a tick is dispatching into it.
					l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
					if errors.Is(err, lock.ErrHeld) {
						return fmt.Errorf("a tick is running for loop %q; try again", cfg.Name)
					}
					if err != nil {
						return err
					}
					defer l.Release()

					return loopcmd.Reset(cfg, deps.Store, deps.WT, c.Int("issue"), deps.IsAlive)
				},
			},
		},
	}
}

func internalCommand() *cli.Command {
	return &cli.Command{
		Name:   "internal",
		Usage:  "internal commands; not for direct use",
		Hidden: true,
		Commands: []*cli.Command{
			{
				Name:  "run-agent",
				Usage: "run one dispatch and record its outcome",
				Flags: []cli.Flag{
					// The runner is spawned with an explicit path and must never
					// prompt or scan, so this one stays required.
					&cli.StringFlag{Name: "config", Required: true},
					&cli.IntFlag{Name: "dispatch", Usage: "dispatch id", Required: true},
					// The runner resolves no project, so it is told which one owns
					// the rows it writes.
					&cli.StringFlag{Name: "project", Usage: "project id", Required: true},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					// Install the signal handler so an operator's `sessions kill`
					// (which SIGTERMs this process) cancels the context Supervise
					// runs the agent under, instead of leaving the agent behind
					// with no supervisor left to record its outcome or sweep its
					// process group.
					ctx, cancel := runAgentContext(ctx)
					defer cancel()

					configPath := c.String("config")
					ref := loopcmd.ProjectRef{
						ID: c.String("project"),
						// Derived, not passed: the runner must not depend on a
						// lookup it cannot perform. An empty result simply means
						// the loop's own state directory is the only source.
						Dir: config.DirFromPath(configPath),
					}
					cfg, deps, cleanup, err := loopcmd.Open(ref, configPath, loopcmd.Options{
						Token:           os.Getenv("GITHUB_TOKEN"),
						RequireGitHub:   false,
						MigrationPolicy: loopcmd.FailOnUnimported,
					})
					if err != nil {
						return err
					}
					defer cleanup()
					return loopcmd.RunAgent(ctx, cfg, deps, int64(c.Int("dispatch")))
				},
			},
		},
	}
}
