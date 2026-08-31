package loopcmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/migrate"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/runner"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/seanmcgary/agent-utils/internal/worktree"
)

// MigrationPolicy decides what an unimported legacy database means to a command.
//
// A command that WRITES must not proceed against state it could not import: a
// tick would re-dispatch every open issue and start a second agent in a worktree
// that already holds one. A command that only READS must not fail because some
// other loop's old file is broken; it says so and carries on.
type MigrationPolicy bool

const (
	FailOnUnimported MigrationPolicy = false
	WarnOnUnimported MigrationPolicy = true
)

// ProjectRef is the project a command acts for. The runner is given one
// explicitly, because it resolves no project of its own.
type ProjectRef struct {
	ID   string
	Name string
	// Dir is the project's .agent-utils directory. It is empty when the runner
	// was pointed at a configuration outside any such directory.
	Dir string
}

// Options configures Open.
type Options struct {
	// Token authenticates the GitHub client.
	//
	// The caller reads it, because the daemon reads it from a file on each tick
	// and must not change its own environment to pass it.
	//
	// NOTHING may ever fill this field from a cli.Flag. A flag value shows up in
	// `ps` output and in the shell history of anyone who typed it. The command
	// reads the environment; the daemon reads ~/.agent-utils/env.
	Token           string
	RequireGitHub   bool
	MigrationPolicy MigrationPolicy
	// GH is the GitHub client this tick must use. Nil means "build one from
	// Token", which is what every CLI command does.
	//
	// The webhook daemon passes one instead: a delivery fans out across every
	// loop watching the repository, and each loop's tick starts by fetching the
	// SAME issue, so the daemon builds one authenticated, memoising client for
	// the delivery and hands it to each Open. The token is machine-wide, so one
	// client per delivery is legitimate; the memo is not, beyond that delivery.
	// listener.access and ghub.DeliveryCache both state why the lifetime must
	// stay with the caller: a client cached any longer would decide from stale
	// LABELS.
	GH ghub.Client
	// Epic reads sub-issues and issue dependencies. Nil means "build one from
	// Token", like GH.
	//
	// It is a SEPARATE field from GH rather than an assertion on it, because
	// the daemon's GH is a *ghub.DeliveryCache: that type memoises single-issue
	// fetches for one delivery and implements ghub.Client only. Asserting
	// GH.(ghub.EpicReader) therefore succeeds for every CLI command and fails
	// for every webhook delivery -- which is the epic sweep's main driver, so
	// the feature would be dead exactly where it matters and nowhere a test
	// injecting its own reader would notice.
	Epic ghub.EpicReader
}

// Open resolves a project's loop configuration and builds everything one tick
// needs: the state directory, the GitHub client, the worktree manager and the
// rest of Deps. It is the single path the CLI and the webhook daemon both call
// to reach a tick, so the two run byte-identical code.
//
// The caller MUST defer the returned cleanup function. It closes the shared
// state database; the daemon calls Open once per delivery, so skipping cleanup
// leaks one database handle per delivery.
func Open(ref ProjectRef, configPath string, opts Options) (*config.Config, Deps, func(), error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, Deps{}, nil, err
	}

	// Resolve the state directory before anything uses it. When state_dir is not
	// set this derives <project>/.agent-utils/state/<name>. The database no
	// longer lives there; the tick lock and the logs still do.
	stateDir, err := cfg.ResolveStateDir(configPath)
	if err != nil {
		return nil, Deps{}, nil, err
	}
	cfg.StateDir = stateDir

	// Resolve the two working directories against the PROJECT, for the same
	// reason. Both are used raw downstream: checkout_base_dir becomes the
	// agent's cmd.Dir, and worktree_dir is where every worktree is created. A
	// relative value would therefore mean whatever directory the reading
	// process was started in -- and the listener daemon's launchd plist sets
	// WorkingDirectory to the machine-wide ~/.agent-utils, so the daemon would
	// run the agent inside the directory that holds the registry and the state
	// database instead of inside the repository. Open is the one path every
	// context reaches a tick through, so resolving here fixes all of them.
	checkoutDir, worktreeDir, err := cfg.ResolveWorkDirs(ref.Dir, configPath)
	if err != nil {
		return nil, Deps{}, nil, err
	}
	cfg.CheckoutBaseDir = checkoutDir
	cfg.WorktreeDir = worktreeDir

	// 0700: the state directory holds agent transcripts, which quote everything
	// the agent read and ran.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, Deps{}, nil, fmt.Errorf("create state directory: %w", err)
	}

	// The token must come from the environment, never a flag. A flag value
	// shows up in `ps` output and in the shell history of anyone who typed it.
	// The detached runner never calls the GitHub API, so it must neither require
	// nor carry the token. Requiring it there would put a repository-write
	// credential in the environment of a process whose child is the agent.
	token := opts.Token
	if token == "" && opts.RequireGitHub && opts.GH == nil {
		return nil, Deps{}, nil, fmt.Errorf("GITHUB_TOKEN is not set")
	}

	// A caller-supplied client is used as it is. Building a second one here
	// would silently undo the sharing the daemon set up and put the per-loop
	// fetches back.
	gh := opts.GH
	if gh == nil {
		gh = ghub.New(token)
	}

	// Falls back to the same client when the caller supplied none. The
	// concrete *ghub.GitHubClient satisfies both interfaces; only the
	// DeliveryCache wrapper does not, and the daemon passes Epic explicitly.
	epicReader := opts.Epic
	if epicReader == nil {
		if er, ok := gh.(ghub.EpicReader); ok {
			epicReader = er
		}
	}

	if _, err := home.EnsureDir(); err != nil {
		return nil, Deps{}, nil, err
	}
	dbPath, err := home.StateDBPath()
	if err != nil {
		return nil, Deps{}, nil, err
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, Deps{}, nil, err
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
		sources = migrate.Add(sources, own)
	}
	if err := migrate.EnsureProject(db, sources, problems); err != nil {
		if opts.MigrationPolicy == FailOnUnimported {
			db.Close()
			return nil, Deps{}, nil, err
		}
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
	}

	self, err := os.Executable()
	if err != nil {
		db.Close()
		return nil, Deps{}, nil, fmt.Errorf("locate this executable: %w", err)
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		db.Close()
		return nil, Deps{}, nil, fmt.Errorf("resolve config path: %w", err)
	}

	wt := worktree.New(cfg.CheckoutBaseDir, cfg.WorktreeDir, cfg.Name, cfg.DefaultBranch)

	deps := Deps{
		Store:      db.Project(ref.ID),
		ProjectID:  ref.ID,
		GH:         gh,
		Epic:       epicReader,
		WT:         wt,
		SelfPath:   self,
		ConfigPath: abs,
		Now:        time.Now,
		Spawn:      runner.Spawn,
		IsAlive:    proc.IsAlive,
		Fetch:      wt.Fetch,
		Behind:     wt.BehindLocal,
		// The shipped binary's only wiring of the automatic rebase. A Deps
		// built without it rebases nothing and dispatches an agent for every
		// tend, silently -- which is correct for a test and dead on arrival
		// here.
		Git: wt,
	}
	return cfg, deps, func() { db.Close() }, nil
}

// RunTick takes the loop's lock and runs one tick.
//
// It returns an error wrapping lock.ErrHeld when another tick already holds the
// lock. The caller decides what that means: the command exits quietly, and the
// daemon drops the delivery, because the running tick reads the same GitHub
// state a moment later than the dropped one would have.
func RunTick(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error) {
	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		return Summary{}, err
	}
	defer l.Release()

	return Tick(ctx, cfg, deps)
}
