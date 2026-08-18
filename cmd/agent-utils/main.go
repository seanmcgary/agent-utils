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
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/loopcmd"
	"github.com/seanmcgary/agent-utils/internal/proc"
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
			loopCommand(),
			projectStatusCommand(),
			listCommand(),
			forgetCommand(),
			versionCommand(),
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
func resolveConfigPath(c *cli.Command) (string, error) {
	path, name := c.String("config"), c.String("name")

	// Both set is ambiguous. Silently preferring one would hide a mistake in a
	// cron entry until it had been running against the wrong loop for a while.
	if path != "" && name != "" {
		return "", errors.New("--config and --name are alternatives; pass only one")
	}
	if path != "" {
		return path, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir, err := config.FindDir(cwd)
	if err != nil {
		return "", fmt.Errorf("%w\n\nCreate one with:\n  mkdir -p %s/%s\n\nOr pass a file directly with --config",
			err, config.DirName, config.ConfigsSubdir)
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
func projectStatusCommand() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "list every onboarded project and the state of its loops",
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

// forgetCommand removes a project from the registry. It touches nothing the
// project owns, so it is safe to run against a directory that has moved.
func forgetCommand() *cli.Command {
	return &cli.Command{
		Name:      "forget",
		Usage:     "remove a project from the registry without touching its files",
		Arguments: []cli.Argument{&cli.StringArg{Name: "path"}},
		Action: func(_ context.Context, c *cli.Command) error {
			root := c.StringArg("path")
			if root == "" {
				return errors.New("usage: agent-utils forget <project path>")
			}
			dir := root
			if filepath.Base(dir) != config.DirName {
				dir = filepath.Join(root, config.DirName)
			}
			if err := registry.Forget(dir); err != nil {
				return err
			}
			fmt.Printf("forgot %s\n", dir)
			return nil
		},
	}
}

// listCommand prints the configurations in the local .agent-utils directory.
func listCommand() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list the loop configurations in the local " + config.DirName + " directory",
		Action: func(_ context.Context, _ *cli.Command) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			dir, err := config.FindDir(cwd)
			if err != nil {
				return fmt.Errorf("%w\n\nCreate one with:\n  mkdir -p %s/%s",
					err, config.DirName, config.ConfigsSubdir)
			}

			entries, err := config.List(dir)
			if err != nil {
				return err
			}

			fmt.Printf("%s\n\n", config.ConfigsDir(dir))
			fmt.Printf("%-20s %-40s %s\n", "NAME", "REPO", "STATUS")
			for _, e := range entries {
				status := "ok"
				repo := e.Repo
				if e.Err != nil {
					status = "INVALID"
					repo = "-"
				}
				fmt.Printf("%-20s %-40s %s\n", e.Name, repo, status)
			}
			// Print the reason for each broken file after the table, so the
			// table stays readable and the error is still visible.
			for _, e := range entries {
				if e.Err != nil {
					fmt.Printf("\n%s: %v\n", e.Name, e.Err)
				}
			}
			return nil
		},
	}
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
					path, err := resolveConfigPath(c)
					if err != nil {
						return err
					}
					cfg, deps, cleanup, err := setup(path, true)
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
					path, err := resolveConfigPath(c)
					if err != nil {
						return err
					}
					cfg, deps, cleanup, err := setup(path, true)
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
					path, err := resolveConfigPath(c)
					if err != nil {
						return err
					}
					cfg, deps, cleanup, err := setup(path, false)
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
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					cfg, deps, cleanup, err := setup(c.String("config"), false)
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

func setup(configPath string, needsGitHub bool) (*config.Config, loopcmd.Deps, func(), error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, loopcmd.Deps{}, nil, err
	}

	// Resolve the state directory before anything uses it. When state_dir is
	// not set this derives <project>/.agent-utils/state/<name>, which is what
	// keeps two projects from sharing one database.
	stateDir, err := cfg.ResolveStateDir(configPath)
	if err != nil {
		return nil, loopcmd.Deps{}, nil, err
	}
	cfg.StateDir = stateDir

	// Record the project so `agent-utils status` can find it later. This is an
	// index, not state anything depends on, so a failure is logged and the
	// command carries on.
	if dir := config.DirFromPath(configPath); dir != "" {
		if err := registry.Register(dir); err != nil {
			slog.Warn("could not record project in the registry", "dir", dir, "err", err)
		}
	}
	// 0700: the state directory holds the sqlite database, which carries
	// session identifiers and transcript logs.
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

	s, err := store.Open(filepath.Join(cfg.StateDir, "state.db"))
	if err != nil {
		return nil, loopcmd.Deps{}, nil, err
	}

	self, err := os.Executable()
	if err != nil {
		s.Close()
		return nil, loopcmd.Deps{}, nil, fmt.Errorf("locate this executable: %w", err)
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		s.Close()
		return nil, loopcmd.Deps{}, nil, fmt.Errorf("resolve config path: %w", err)
	}

	wt := worktree.New(cfg.CheckoutBaseDir, cfg.WorktreeDir, cfg.Name, cfg.DefaultBranch)

	deps := loopcmd.Deps{
		Store:      s,
		GH:         ghub.New(token),
		WT:         wt,
		SelfPath:   self,
		ConfigPath: abs,
		Now:        time.Now,
		Spawn:      runner.Spawn,
		IsAlive:    proc.IsAlive,
		Fetch:      wt.Fetch,
	}
	return cfg, deps, func() { s.Close() }, nil
}
