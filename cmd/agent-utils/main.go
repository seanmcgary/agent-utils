// Command agent-utils holds utilities for agent workflows.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/loopcmd"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/runner"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/seanmcgary/agent-utils/internal/worktree"
	"github.com/urfave/cli/v3"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cmd := &cli.Command{
		Name:  "agent-utils",
		Usage: "utilities for agent workflows",
		Commands: []*cli.Command{
			loopCommand(),
			internalCommand(),
		},
	}
	if err := cmd.Run(context.Background(), os.Args); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func configFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:     "config",
		Usage:    "path to the loop configuration file",
		Required: true,
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
				Flags: []cli.Flag{configFlag()},
				Action: func(ctx context.Context, c *cli.Command) error {
					cfg, deps, cleanup, err := setup(c.String("config"), true)
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
				Flags: []cli.Flag{configFlag()},
				Action: func(ctx context.Context, c *cli.Command) error {
					cfg, deps, cleanup, err := setup(c.String("config"), true)
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
					&cli.IntFlag{Name: "issue", Usage: "issue number", Required: true},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					cfg, deps, cleanup, err := setup(c.String("config"), false)
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
					configFlag(),
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
