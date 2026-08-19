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
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/loopcmd"
	"github.com/seanmcgary/agent-utils/internal/migrate"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/project"
	"github.com/seanmcgary/agent-utils/internal/registry"
	"github.com/seanmcgary/agent-utils/internal/runner"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/seanmcgary/agent-utils/internal/version"
	"github.com/seanmcgary/agent-utils/internal/worktree"
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
			logsCommand(),
			forgetCommand(),
			migrateCommand(),
			versionCommand(),
			// Everything project-scoped lives under `project`.
			projectCommand(),
			internalCommand(),
		},
	}
	if err := cmd.Run(context.Background(), os.Args); err != nil {
		slog.Error("fatal", "err", err)
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

// openProject resolves the project and reports it when this call onboarded it.
func openProject(c *cli.Command) (*loopcmd.Project, error) {
	p, err := loopcmd.ResolveProject(selectedProject(c))
	if err != nil {
		return nil, err
	}
	if p.Created {
		fmt.Fprintf(os.Stderr, "Registered project %q (%s)\n", p.Config.Name, p.Dir)
		if p.RenamedFrom != "" {
			fmt.Fprintf(os.Stderr,
				"The name %q was already taken by another project, so this one is %q.\n"+
					"Change it by editing %s\n",
				p.RenamedFrom, p.Config.Name, project.Path(p.Dir))
		}
	}
	return p, nil
}

// refOf names the project a command acts for.
func refOf(p *loopcmd.Project) projectRef {
	return projectRef{ID: p.Config.ID, Name: p.Config.Name, Dir: p.Dir}
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

// sessionsCommand groups everything about claude sessions. A session spans the
// resumes of one issue, so it is a different unit from a dispatch.
func sessionsCommand() *cli.Command {
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

// projectCommand groups everything scoped to one project. Naming it explicitly
// is what makes the top level unambiguously machine-wide.
func projectCommand() *cli.Command {
	return &cli.Command{
		Name:  "project",
		Usage: "act on one project: its loops, configurations and logs",
		Flags: []cli.Flag{projectSelectorFlag()},
		Commands: []*cli.Command{
			projectStatusCommand(),
			projectListCommand(),
			sessionsCommand(),
			logsCommand(),
			loopCommand(),
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
			cfg, deps, cleanup, err := setup(refOf(p), path, false)
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
			fmt.Print(renderMigrateReport(report, dryRun))

			// Err names every failure already. Rebuilding that message here would
			// let the two drift apart, and the write path prints the same one.
			return report.Err()
		},
	}
}

// renderMigrateReport formats a migration report for a terminal.
//
// It is separate from the command so the wording can be checked without a home
// directory, a registry or a legacy file on disk.
func renderMigrateReport(report migrate.Report, dryRun bool) string {
	var b strings.Builder

	if dryRun {
		// Opening the canonical database applies the schema upgrade, and the
		// report cannot be produced without opening it. Say so rather than let
		// an operator believe --dry-run touched nothing at all.
		fmt.Fprintf(&b, "Dry run: no state was imported and no legacy file was touched.\n")
		fmt.Fprintf(&b, "Opening the canonical database still brought its schema up to date;\n")
		fmt.Fprintf(&b, "that part cannot be avoided.\n\n")
	}

	if len(report.Results) == 0 {
		fmt.Fprintf(&b, "Nothing left to import. Every registered project's state is already\n")
		fmt.Fprintf(&b, "in the canonical database.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "%-20s %-16s %-10s %-7s %s\n",
		"PROJECT", "LOOP", "STATE", "ROWS", "SOURCE")
	for _, res := range report.Results {
		fmt.Fprintf(&b, "%-20s %-16s %-10s %-7d %s\n",
			orDash(res.Source.ProjectName), orDash(res.Source.Loop),
			res.State, res.Rows, orDash(res.Source.Path))
	}

	verb := "imported"
	if dryRun {
		verb = "would be imported"
	}
	fmt.Fprintf(&b, "\n%d source(s); %d row(s) %s.\n",
		len(report.Results), report.Rows(), verb)

	// A reason does not fit the table, so it goes under it, one paragraph per
	// source, the way a loop's error does in `project status`.
	for _, res := range report.Results {
		if res.Reason == "" {
			continue
		}
		fmt.Fprintf(&b, "\n%s (loop %s): %s\n",
			orDash(res.Source.Path), orDash(res.Source.Loop), res.Reason)
	}
	return b.String()
}

// orDash keeps a column filled. A discovery failure has no path and no loop, and
// an empty cell in the middle of a table reads as a rendering bug.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func loopCommand() *cli.Command {
	return &cli.Command{
		Name:  "loop",
		Usage: "run and inspect an issue-driven agent loop",
		Commands: []*cli.Command{
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
					cfg, deps, cleanup, err := setup(refOf(p), path, true)
					if err != nil {
						return err
					}
					defer cleanup()

					l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
					if errors.Is(err, lock.ErrHeld) {
						slog.Info("another tick is running; exiting", "loop", cfg.Name)
						return nil
					}
					if err != nil {
						return err
					}
					defer l.Release()

					_, err = loopcmd.Tick(ctx, cfg, deps)
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
					cfg, deps, cleanup, err := setup(refOf(p), path, true)
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
					cfg, deps, cleanup, err := setup(refOf(p), path, false)
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
					configPath := c.String("config")
					ref := projectRef{
						ID: c.String("project"),
						// Derived, not passed: the runner must not depend on a
						// lookup it cannot perform. An empty result simply means
						// the loop's own state directory is the only source.
						Dir: config.DirFromPath(configPath),
					}
					cfg, deps, cleanup, err := setup(ref, configPath, false)
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

// projectRef is the project a command acts for. The runner is given one
// explicitly, because it resolves no project of its own.
type projectRef struct {
	ID   string
	Name string
	// Dir is the project's .agent-utils directory. It is empty when the runner
	// was pointed at a configuration outside any such directory.
	Dir string
}

func setup(ref projectRef, configPath string, needsGitHub bool) (*config.Config, loopcmd.Deps, func(), error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, loopcmd.Deps{}, nil, err
	}

	// Resolve the state directory before anything uses it. When state_dir is not
	// set this derives <project>/.agent-utils/state/<name>. The database no
	// longer lives there; the tick lock and the logs still do.
	stateDir, err := cfg.ResolveStateDir(configPath)
	if err != nil {
		return nil, loopcmd.Deps{}, nil, err
	}
	cfg.StateDir = stateDir

	// 0700: the state directory holds agent transcripts, which quote everything
	// the agent read and ran.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, loopcmd.Deps{}, nil, fmt.Errorf("create state directory: %w", err)
	}

	// The token must come from the environment, never a flag. A flag value
	// shows up in `ps` output and in the shell history of anyone who typed it.
	// The detached runner never calls the GitHub API, so it must neither require
	// nor carry the token. Requiring it there would put a repository-write
	// credential in the environment of a process whose child is the agent.
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" && needsGitHub {
		return nil, loopcmd.Deps{}, nil, fmt.Errorf("GITHUB_TOKEN is not set")
	}

	if _, err := home.EnsureDir(); err != nil {
		return nil, loopcmd.Deps{}, nil, err
	}
	dbPath, err := home.StateDBPath()
	if err != nil {
		return nil, loopcmd.Deps{}, nil, err
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, loopcmd.Deps{}, nil, err
	}

	// This is the WRITE path, so an unimported source is fatal. A tick against a
	// database missing this loop's rows would re-dispatch every open issue and
	// start a second agent in a worktree that already holds one.
	//
	// The loop's own state directory is always included. --config takes an
	// arbitrary path, so this loop is not always inside the directory Discover
	// scans.
	var (
		sources  []migrate.Source
		problems []migrate.Result
	)
	if ref.Dir != "" {
		sources, problems = migrate.Discover(ref.Dir, ref.ID, ref.Name)
	}
	if own, ok := migrate.SourceFor(cfg.StateDir, ref.ID, ref.Name, cfg.Name, cfg.Repo); ok {
		sources = append(sources, own)
	}
	if err := migrate.EnsureProject(db, sources, problems); err != nil {
		db.Close()
		return nil, loopcmd.Deps{}, nil, err
	}

	self, err := os.Executable()
	if err != nil {
		db.Close()
		return nil, loopcmd.Deps{}, nil, fmt.Errorf("locate this executable: %w", err)
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		db.Close()
		return nil, loopcmd.Deps{}, nil, fmt.Errorf("resolve config path: %w", err)
	}

	wt := worktree.New(cfg.CheckoutBaseDir, cfg.WorktreeDir, cfg.Name, cfg.DefaultBranch)

	deps := loopcmd.Deps{
		Store:      db.Project(ref.ID),
		ProjectID:  ref.ID,
		GH:         ghub.New(token),
		WT:         wt,
		SelfPath:   self,
		ConfigPath: abs,
		Now:        time.Now,
		Spawn:      runner.Spawn,
		IsAlive:    proc.IsAlive,
		Fetch:      wt.Fetch,
	}
	return cfg, deps, func() { db.Close() }, nil
}
