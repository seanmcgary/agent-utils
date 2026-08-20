package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/listener"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/service"
	"github.com/seanmcgary/agent-utils/internal/settings"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/urfave/cli/v3"
)

// pidFileName is the listener's pidfile, at <home>/listener.pid. It records
// which pid and address a running listener bound, for `status` to report
// and for `stop` to signal.
//
// It is NOT the source of truth for whether a listener is alive -- see
// lockFileName. internal/proc.IsAlive cannot be reused to find this process
// either way: it matches a runner's command line against "--dispatch <id>",
// and the listener carries no such argument.
const pidFileName = "listener.pid"

// lockFileName is the listener's single-instance and liveness lock, at
// <home>/listener.lock, held for the whole lifetime of a running listener
// (see runListener) and released by drainAndClose.
//
// This, not kill(pid, 0) against the pid recorded in the pidfile, is what
// `stop` and `status` trust to answer "is a listener actually running":
// pids are recycled by the OS, so a listener that was killed -9 or panicked
// -- leaving its pidfile behind, since only drainAndClose's own clean
// shutdown removes it -- can have its old pid handed to an unrelated
// process this same user later starts. kill(pid, 0) would then report that
// unrelated process alive, and `stop` would SIGTERM it. flock, by contrast,
// is released by the kernel the instant the holding process's file
// descriptor closes, on a clean exit OR a crash OR a kill -9, so
// lock.Acquire succeeding is proof, not a guess, that nothing currently
// holds it. It doubles as this command's single-instance guard: a second
// `listener start` fails fast at lock.Acquire, before it can overwrite a
// live listener's pidfile out from under it.
const lockFileName = "listener.lock"

// listenerCommand groups the webhook daemon's lifecycle: run it, stop it,
// and ask what it is doing. It is top level, not project-scoped, because one
// listener serves every project on the machine.
func listenerCommand() *cli.Command {
	return &cli.Command{
		Name:  "listener",
		Usage: "run the webhook listener that dispatches loops on GitHub deliveries",
		Commands: []*cli.Command{
			listenerStartCommand(),
			listenerStopCommand(),
			listenerStatusCommand(),
		},
	}
}

func listenerStartCommand() *cli.Command {
	return &cli.Command{
		Name:  "start",
		Usage: "run the listener in the foreground, or install it as a launchd agent with --daemon",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "daemon",
				Usage: "install and start the listener as a launchd user agent instead of running here"},
			listenPortFlag(),
			listenAddrFlag(),
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			st, err := settings.Load()
			if err != nil {
				return err
			}
			if !st.Webhook.Enabled {
				return errors.New(
					"the webhook daemon is disabled; run `agent-utils config webhook --enable` first")
			}

			// --listen-port/--listen-addr validate through the exact same
			// settings.FieldFor(...).Set path `config set` and `config
			// webhook` use (setField, in config.go), and are applied to an
			// in-memory copy only -- they are never saved to config.yaml.
			// An override that reached service.Install's plist unvalidated
			// would be rendered into a file launchd executes at every
			// login; going through the shared validator is what keeps
			// `--listen-port 0` rejected here exactly as it is in `config
			// set webhook.listen_port` and in listener.New itself.
			var overrideArgs []string
			if c.IsSet("listen-addr") {
				v := c.String("listen-addr")
				if err := setField(st, "webhook.listen_addr", v); err != nil {
					return err
				}
				overrideArgs = append(overrideArgs, "--listen-addr", v)
			}
			if c.IsSet("listen-port") {
				v := c.Int("listen-port")
				if err := setField(st, "webhook.listen_port", strconv.Itoa(v)); err != nil {
					return err
				}
				overrideArgs = append(overrideArgs, "--listen-port", strconv.Itoa(v))
			}

			// Refuse to run an unauthenticated listener. listener.New
			// refuses an empty secret too, but that check happens only
			// after a database is opened and a pidfile written below; this
			// one fails before any of that, with a message that names what
			// is actually wrong rather than New's generic one.
			if st.Webhook.Secret == "" {
				return errors.New(
					"webhook.secret is empty; refusing to start an unauthenticated listener")
			}

			// Checked up front, once, before opening the database or
			// binding a socket: without this a daemon started against a
			// 0644 (or missing) env file comes up looking healthy and then
			// fails every single tick, since Worker reads the token fresh
			// on every delivery (see internal/listener/env.go's Token).
			if err := ensureToken(os.Stdin, os.Stderr, isInteractive()); err != nil {
				return err
			}

			if c.Bool("daemon") {
				return installDaemon(overrideArgs)
			}

			def := st.WithDefaults()
			// os.Stdout, and only on this path: --daemon returned above
			// without ever running a server, so it has no routing table to
			// report and would be printing one for a process that is not
			// the one serving.
			return runListener(ctx, os.Stdout, def.Webhook.ListenAddr, def.Webhook.ListenPort, currentSecret)
		},
	}
}

// ensureToken proves the GitHub token is readable before the listener starts,
// and offers to write the env file when it is simply not there yet.
//
// The prompt is offered for an ABSENT file only. A wrong mode, a symlink, or
// a file owned by somebody else all still fail with the error Token
// produced: those are conditions an operator has to look at -- something put
// a credential file into that state -- and silently overwriting the file
// would destroy the evidence while answering nothing.
//
// interactive is a parameter rather than an isInteractive() call inside, so
// this is testable without a pty. It must be false whenever stdin is not a
// terminal: under launchd or cron stdin is /dev/null, and a prompt there
// would hang the service forever on a question nobody will ever see -- the
// same rule resolveLoopConfig documents.
func ensureToken(in io.Reader, out io.Writer, interactive bool) error {
	_, err := listener.Token()
	if err == nil {
		return nil
	}
	if !interactive || !errors.Is(err, listener.ErrEnvFileMissing) {
		return fmt.Errorf("github token: %w", err)
	}

	// The first sentence reports what discovery already turned up, so the
	// operator is not told there is nothing a line before the token they
	// exported gets used. Only the environment is named here: a `gh` token
	// announces itself in the prompt that follows immediately ("[ghp_…AB12
	// from `gh auth token`...]"), and running `gh auth token` a second time
	// just to phrase this sentence would double a subprocess that is already
	// allowed to take five seconds.
	//
	// The second sentence stays as it was: it is what explains why this file
	// exists at all, rather than the token being asked for once and kept in
	// memory.
	found := "No GitHub token is stored yet."
	if _, ok := environmentToken(); ok {
		found = "No GitHub token is stored yet, but $" + githubTokenEnv + " is set in this environment."
	}
	_, _ = fmt.Fprintln(out, found+" The listener reads one from "+
		"~/.agent-utils/env on every delivery.")
	if err := storeToken(in, out); err != nil {
		return err
	}

	// Read it back rather than trusting the write: the point of checking here
	// at all is that the daemon fails now, at a terminal, instead of on every
	// tick once it is detached.
	if _, err := listener.Token(); err != nil {
		return fmt.Errorf("github token: %w", err)
	}
	return nil
}

// installDaemon registers this program as a launchd user agent, invoked at
// login and kept alive, running `listener start` (never `--daemon` again --
// that would just reinstall itself in a loop) plus any validated listen
// override.
func installDaemon(overrideArgs []string) error {
	// self is passed to Install for its own sake (a caller that names a
	// binary other than the one it is actually running as is confused about
	// what this does, and failing loudly is cheaper than silently ignoring
	// it), but Install resolves the SOURCE of the installed path itself, via
	// os.Executable() plus filepath.EvalSymlinks; see resolveSelf in
	// internal/service/service_darwin.go. That is what keeps a service
	// definition with RunAtLoad+KeepAlive -- permanent login-time execution
	// -- from ever being pointed at a path this process merely claims to be
	// running from.
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this executable: %w", err)
	}

	args := append([]string{"listener", "start"}, overrideArgs...)

	mgr := service.New()
	if err := mgr.Install(self, args); err != nil {
		return explainInstallErr(err)
	}
	path, err := mgr.ServiceFilePath()
	if err != nil {
		return err
	}
	fmt.Println(path)
	return nil
}

// explainInstallErr adds operator-facing context to an Install failure
// caused by a group- or world-writable install path (see
// refuseIfWritableByOthers in internal/service/service_darwin.go). That
// refusal is the correct security default -- a plist with RunAtLoad and
// KeepAlive is permanent login-time execution of whatever path it names, and
// a writable parent directory would let another local account replace the
// binary launchd runs at every login -- but the bare error from
// os.Lstat-driven mode checks reads like a bug report, not an explanation.
// The common case it is guarding against, an Intel-Mac Homebrew install
// under /usr/local (commonly drwxrwxr-x, group admin), is named explicitly
// so the operator understands why and knows the fix.
func explainInstallErr(err error) error {
	if err == nil {
		return nil
	}
	if !containsWritableRefusal(err) {
		return fmt.Errorf("install as a launchd agent: %w", err)
	}
	return fmt.Errorf(
		"install as a launchd agent: %w\n\n"+
			"agent-utils refuses to install itself from a group- or world-writable\n"+
			"location. This is a common outcome for a Homebrew install under\n"+
			"/usr/local on an Intel Mac, where the directory is typically\n"+
			"drwxrwxr-x owned by group \"admin\". A launchd agent with\n"+
			"RunAtLoad+KeepAlive is permanent login-time execution of whatever path\n"+
			"it names, so a writable parent directory would let another local\n"+
			"account replace the binary launchd runs at every login. Move the\n"+
			"binary to a location only you can write to (for example ~/bin) and\n"+
			"run `listener start --daemon` again", err)
}

// containsWritableRefusal reports whether err is (or wraps) the specific
// refusal refuseIfWritableByOthers produces. It matches on text rather than
// a sentinel because that function returns a plain fmt.Errorf with no
// exported error value to compare against; see the cross-reference comment
// left next to that error in internal/service/service_darwin.go, which
// exists so a wording change there cannot silently break this match without
// a reader noticing.
func containsWritableRefusal(err error) bool {
	return strings.Contains(err.Error(), "writable by group or other")
}

// currentSecret reads the webhook secret from the settings file, fresh.
//
// It is the Secret seam listener.Server calls on EVERY delivery, not a value
// captured at start. `config webhook --rotate-secret` writes a new secret and
// tells the operator to re-run register-webhook so GitHub signs with it; a
// daemon still holding the old value would then reject every delivery with a
// 401, and the log would fill with signature failures indistinguishable from
// an attack. listener.Token does the same for the GitHub token and documents
// the same reason, and the README promises that behaviour a paragraph before
// it describes rotation.
func currentSecret() (string, error) {
	st, err := settings.Load()
	if err != nil {
		return "", err
	}
	return st.Webhook.Secret, nil
}

// runListener runs the listener in the foreground until it receives SIGINT
// or SIGTERM, then shuts down in the order drainAndClose documents.
//
// ctx is accepted for symmetry with the rest of this program's Actions, but
// shutdown itself is driven by the OS signal, not by ctx: a foreground
// `listener start` must keep serving after its own CLI Action would
// otherwise be considered "done", which is exactly what a long-running
// daemon is.
func runListener(_ context.Context, out io.Writer, addr string, port int, secret func() (string, error)) error {
	dir, err := home.EnsureDir()
	if err != nil {
		return err
	}

	// Acquired first, before anything else opens or writes: see
	// lockFileName. A second `listener start` must fail HERE, before it can
	// touch the pidfile or the database a live listener already owns.
	lockPath := filepath.Join(dir, lockFileName)
	lk, err := lock.Acquire(lockPath)
	if errors.Is(err, lock.ErrHeld) {
		return errors.New(
			"a listener is already running (its lock is held); " +
				"run `agent-utils listener status` to check, or `listener stop` to stop it first")
	}
	if err != nil {
		return fmt.Errorf("acquire listener lock: %w", err)
	}
	releaseLock := func() {
		if err := lk.Release(); err != nil {
			slog.Warn("release listener lock", "path", lockPath, "err", err)
		}
	}

	dbPath, err := home.StateDBPath()
	if err != nil {
		releaseLock()
		return err
	}
	db, err := store.Open(dbPath)
	if err != nil {
		releaseLock()
		return err
	}
	closeDB := func() {
		if err := db.Close(); err != nil {
			slog.Warn("close state database", "err", err)
		}
	}

	pidPath := filepath.Join(dir, pidFileName)

	w := listener.NewWorker(db)

	// shuttingDown is cancelled at the very start of drainAndClose, before
	// anything else. instrumentRetries checks it so a retry timer firing
	// during shutdown does not start a fresh coding agent; see its doc
	// comment for why work.go's own ctx.Err() check inside schedule cannot
	// be trusted to catch this for every retry origin.
	shuttingDown, cancelShuttingDown := context.WithCancel(context.Background())
	// A safety net for every early-return path below, before drainAndClose
	// ever runs: drainAndClose itself calls cancelShuttingDown explicitly,
	// as its very first action, on the real shutdown path -- this defer
	// only matters if runListener returns before reaching that call.
	// CancelFuncs are idempotent, so the explicit call and this defer
	// calling it again later cost nothing.
	defer cancelShuttingDown()

	var tickWG sync.WaitGroup
	// instrumentRetries wraps Worker.After so a retry-fired tick is
	// accounted for and panic-safe exactly like an HTTP-delivered one; see
	// its own comment for why that gap existed and why fixing it belongs
	// here rather than inside internal/listener.
	instrumentRetries(w, &tickWG, shuttingDown)

	// tickCtx is a context of its own, deliberately NOT serverCtx (below):
	// Handler(ctx) threads whatever ctx ListenAndServe was given into every
	// Tick call, and cancelServer() -- drainAndClose step 1 -- cancels
	// exactly that ctx to stop accepting new deliveries. If Tick's own work
	// ran under that same ctx, step 1 would cancel every tick already IN
	// FLIGHT too, and drainAndClose's later Wait would then only be timing
	// how long they take to unwind under a dead context, not waiting for
	// them to actually finish -- precisely the "cancelling mid-tick" half
	// of the hazard drainAndClose's own comment names. wrapTick below
	// discards the ctx Server passes it and substitutes this one, which
	// nothing cancels until after the drain (see the defer below), by
	// which point the process is exiting anyway.
	tickCtx, cancelTickCtx := context.WithCancel(context.Background())
	defer cancelTickCtx()

	srv, err := listener.New(&listener.Server{
		Addr:   addr,
		Port:   port,
		Secret: secret,
		Tick:   wrapTick(tickCtx, w.Deliver),
	})
	if err != nil {
		closeDB()
		releaseLock()
		return err
	}

	// Write the pidfile before Serve starts, not after: `status` and `stop`
	// must be able to find this process from the moment it starts
	// listening, and there is no point at which this process is "half
	// running" that they should observe instead. Safe to do unconditionally
	// here: the lock acquired above already proves no other listener could
	// be holding this pidfile's identity.
	if err := writePidfile(pidPath, os.Getpid(), addr, port); err != nil {
		closeDB()
		releaseLock()
		return fmt.Errorf("write pidfile: %w", err)
	}

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	serverCtx, cancelServer := context.WithCancel(context.Background())
	workerCtx, cancelWorker := context.WithCancel(context.Background())

	serverDone := make(chan error, 1)
	serverStopped := make(chan struct{})
	go func() {
		serverDone <- srv.ListenAndServe(serverCtx)
		close(serverStopped)
	}()

	workerDone := make(chan struct{})
	go func() {
		w.Serve(workerCtx)
		close(workerDone)
	}()

	// The routing table goes to the operator's terminal, not to slog: it is
	// a multi-line table, and the JSON handler a daemon runs under would
	// turn it into one unreadable escaped string. The slog line below stays
	// as the machine-readable record of the same event.
	//
	// Printed here, after ListenAndServe has been started and at the same
	// moment "listener started" is recorded, so a daemon that cannot bind
	// still reports that failure as its outcome rather than this table
	// becoming the last thing an operator reads.
	printRoutingTable(out)
	slog.Info("listener started", "addr", addr, "port", port, "pid", os.Getpid())

	// Whichever happens first -- an operator or launchd sending a signal, or
	// the server exiting on its own (a bind failure surfacing late, or an
	// unexpected internal error) -- both funnel into the same ordered
	// shutdown below. serverStopped is a pure trigger; drainAndClose reads
	// the actual result off serverDone itself, exactly once, regardless of
	// which branch fired.
	select {
	case <-sigCtx.Done():
		slog.Info("listener received a shutdown signal")
	case <-serverStopped:
		slog.Warn("listener's http server exited on its own")
	}

	return drainAndClose(cancelShuttingDown, cancelServer, cancelWorker, serverDone, workerDone, srv, &tickWG, db, lk, pidPath)
}

// printRoutingTable writes what this daemon will actually route to out.
//
// A listener that routes NOTHING -- no project registered on this host, every
// loop config broken -- verifies signatures and returns 200 exactly like a
// healthy one, and then does nothing with the delivery. Before this, the only
// evidence either way was a per-delivery log line nobody reads until they
// already suspect something is wrong.
//
// A write failure is logged rather than returned: this runs after the socket
// is bound and serving, and a closed stdout is no reason to take down a daemon
// that is otherwise working.
func printRoutingTable(out io.Writer) {
	routes, err := listener.Scan()
	if err != nil {
		// Only registry.List failing gets here (see listener.Scan), and it
		// fails the same way on every delivery: nothing will route at all
		// until it is fixed. Not fatal, because the registry can be repaired
		// under a running daemon and the next delivery will pick it up.
		slog.Error("cannot enumerate the routing table", "err", err)
		writeBanner(out, fmt.Sprintf(
			"NOT LISTENING FOR ANYTHING: the project registry could not be read, so no\n"+
				"delivery can be routed to any loop: %v\n", err))
		return
	}
	writeBanner(out, routingTable(routes))
}

// writeBanner writes one already-formatted block, discarding a write error
// for the reason printRoutingTable documents.
func writeBanner(out io.Writer, s string) {
	_, _ = io.WriteString(out, s)
}

// oneLine folds a multi-line reason onto one line.
//
// A config that does not parse produces exactly that: gopkg.in/yaml.v3
// reports "yaml: unmarshal errors:\n  line 2: field ... not found". Printed
// raw into the indented list below, its continuation lines land at their own
// indentation and read as further skipped entries -- which is what the first
// run of this banner against a real broken config looked like.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// routingTable renders the repositories this daemon will accept deliveries
// for, grouped by repository because that is the key GitHub delivers against,
// and then whatever the scan had to skip.
//
// It reports only what is configured HERE. Whether GitHub actually has a
// webhook registered for one of these repositories is a different question,
// and answering it would need an API call per repository and a token on the
// startup path; `agent-utils project register-webhook` is where that belongs.
func routingTable(routes listener.Routes) string {
	var b strings.Builder

	groups := routes.ByRepo()
	if len(groups) == 0 {
		// Deliberately loud, and deliberately not phrased as "0
		// repositories": this is the one startup state that looks identical
		// to a healthy daemon from the outside, and both of its causes are
		// things the operator can fix in a minute if they are told which one
		// it is.
		fmt.Fprintf(&b, "NOT LISTENING FOR ANYTHING: no loop on this machine watches any repository.\n")
		fmt.Fprintf(&b, "This listener will still verify and accept GitHub deliveries, and then do\n")
		fmt.Fprintf(&b, "nothing with them. Either:\n")
		fmt.Fprintf(&b, "  * no project is registered on this host -- run `agent-utils project init`\n")
		fmt.Fprintf(&b, "    in each project, and `agent-utils list` to see what is registered; or\n")
		fmt.Fprintf(&b, "  * no loop configuration in %s/%s declares a `repo:`.\n",
			config.DirName, config.ConfigsSubdir)
	} else {
		fmt.Fprintf(&b, "listening for deliveries on %d repositor%s (configured locally; GitHub is not asked):\n",
			len(groups), map[bool]string{true: "y", false: "ies"}[len(groups) == 1])
		for _, g := range groups {
			fmt.Fprintf(&b, "  %s\n", g.Repo)
			for _, t := range g.Targets {
				// project/loop, the pair every other command names a loop
				// by: a loop name alone is ambiguous across projects.
				fmt.Fprintf(&b, "    %s/%s\n", t.ProjectName, t.LoopName)
			}
		}
	}

	if len(routes.Skips) > 0 {
		// Printed even when the table above is empty: a skip is usually the
		// REASON a repository an operator expected is missing, and the two
		// only mean something together.
		fmt.Fprintf(&b, "\nskipped, and therefore not routed:\n")
		for _, s := range routes.Skips {
			if s.File != "" {
				fmt.Fprintf(&b, "  %s/%s: %s\n", s.Project, s.File, oneLine(s.Reason))
				continue
			}
			fmt.Fprintf(&b, "  %s (%s): %s\n", s.Project, s.Dir, oneLine(s.Reason))
		}
	}
	return b.String()
}

// drainAndClose runs the shutdown sequence, in this exact order, and why:
//
//  0. Cancel shuttingDown FIRST, before anything else -- even before
//     serverCtx. This is the signal instrumentRetries checks so a retry
//     timer firing anywhere during the steps below bails out instead of
//     starting a fresh coding agent on the way out; see instrumentRetries'
//     own doc comment for why this has to be a signal of its own rather
//     than reusing serverCtx or workerCtx.
//
//  1. Stop accepting deliveries. Cancelling serverCtx makes
//     Server.ListenAndServe run http.Server.Shutdown, which stops the
//     listener socket and waits (up to 10s) for any still-active HTTP
//     request to finish, before ListenAndServe returns.
//
//  2. Only now cancel the daemon context so the worker's retry timers stop
//     (Worker.Serve's stopAll). Doing this earlier would let a retry fire
//     concurrently with the database close in step 4.
//
//  3. Wait for every tick already started to finish. Two accountings, for
//     two different origins: srv.Drain() waits on Server's own WaitGroup,
//     which the HTTP pool goroutine increments before it starts (see
//     Server.Drain's comment); tickWG.Wait() waits on this command's own
//     WaitGroup, which instrumentRetries increments (at FIRE time; see its
//     comment for why not at arm time) around every retry that actually
//     runs. Neither alone is complete -- a retry-fired tick never passes
//     through Server's pool, and an HTTP-delivered tick never passes
//     through Worker.After -- so both run.
//
//     A tick started by Worker.Wake is the one origin deliberately CANCELLED
//     rather than drained: Serve calls tickOne synchronously under workerCtx,
//     which step 2 has just cancelled. That is a choice, not an oversight.
//     Step 2 still WAITS for Serve to return, so the process never exits out
//     from under such a tick; what it does not do is let it finish its GitHub
//     calls, which have no client waiting on them and can run for minutes.
//     The cost is bounded and self-correcting: the worst outcome is a park
//     whose durable state was written but whose comment and label edit were
//     not, and the next tick re-derives that from the issue's own labels. An
//     unbounded shutdown is not similarly recoverable -- launchd SIGKILLs a
//     daemon that takes too long, which is strictly worse than a cancelled
//     GitHub call.
//
//  4. Only now close the database. A tickOne in flight when the database
//     closes underneath it leaves the dispatches row it started stuck in
//     "running", which the next tick has to reap as an orphan (see
//     internal/loopcmd/tick.go's orphan sweep) rather than simply finishing.
//
//  5. Remove the pidfile, but only if it still names THIS process. A second
//     `listener start` cannot get far enough to overwrite it now that the
//     lock (step 6) serializes starts, but re-checking here costs nothing
//     and means this function is never the one that deletes a pidfile some
//     other process is relying on.
//
//  6. Release the lock last of all. Everything this process owns --
//     socket, timers, in-flight ticks, database handle, pidfile -- is
//     already gone by the time `stop`/`status` could observe the lock as
//     free, so there is no window where the lock says "not running" while
//     any of that is still true.
func drainAndClose(
	cancelShuttingDown, cancelServer, cancelWorker context.CancelFunc,
	serverDone <-chan error, workerDone <-chan struct{},
	srv *listener.Server, tickWG *sync.WaitGroup,
	db *store.DB, lk *lock.Lock, pidPath string,
) error {
	cancelShuttingDown()

	cancelServer()
	serveErr := <-serverDone

	cancelWorker()
	<-workerDone

	srv.Drain()
	tickWG.Wait()

	dbErr := db.Close()

	if pf, err := readPidfile(pidPath); err == nil {
		if pf.PID == os.Getpid() {
			if rmErr := os.Remove(pidPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				slog.Warn("remove pidfile", "path", pidPath, "err", rmErr)
			}
		} else {
			slog.Warn("pidfile no longer names this process; leaving it alone",
				"path", pidPath, "pid", pf.PID, "this_pid", os.Getpid())
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		slog.Warn("read pidfile before removing it", "path", pidPath, "err", err)
	}

	if err := lk.Release(); err != nil {
		slog.Warn("release listener lock", "err", err)
	}

	if serveErr != nil {
		return fmt.Errorf("listener: %w", serveErr)
	}
	return dbErr
}

// wrapTick adapts deliver (production always passes Worker.Deliver) into the
// Tick callback listener.Server.New requires.
//
// It ignores the ctx Server passes it and calls deliver with tickCtx
// instead; see runListener's comment on tickCtx for why substituting it is
// required, not merely defensive: Handler(ctx) threads ListenAndServe's own
// ctx into every Tick call, and that same ctx is what drainAndClose cancels
// FIRST, to stop accepting new deliveries, before it waits for in-flight
// ones to finish. Passing it straight through to deliver would cancel every
// tick already running the instant shutdown begins, and the later wait
// would then only be timing an unwind, not honoring a genuine "let it
// finish" guarantee.
//
// It also recovers a panic. Neither internal/listener's HTTP pool goroutine
// nor its retry callback (see instrumentRetries) recovers one, and an
// unrecovered panic in ANY Go goroutine kills the whole process -- there is
// no per-goroutine isolation. This command is the process owner, so the
// decision belongs here.
func wrapTick(tickCtx context.Context, deliver func(ctx context.Context, repo string)) func(context.Context, string) {
	return func(_ context.Context, repo string) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("webhook tick panicked; recovered to keep the listener alive",
					"repo", repo, "panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
			}
		}()
		deliver(tickCtx, repo)
	}
}

// instrumentRetries wraps w.After -- an exported seam on Worker specifically
// so a caller can do this (internal/listener/work.go) -- so a retry-fired
// tick is accounted for by tickWG, protected by recover, and stopped from
// starting a fresh coding agent once shutdown has begun, exactly like an
// HTTP-delivered one.
//
// Without this, work.go's schedule runs tickOne directly inside the
// time.AfterFunc callback After arms, entirely outside anything this
// command tracks. Two consequences, both real: first, Worker.Serve's
// stopAll only stops ARMED timers (see its own comment), so a timer that
// had already fired and begun running tickOne by the time shutdown starts
// is invisible to drainAndClose, which returns and lets the process exit
// out from under it -- exactly how a dispatches row ends up inserted
// `running` with no pid ever registered, which internal/loopcmd/tick.go
// does not treat as an orphan and so never reaps. Second, a panic inside
// that same callback is unrecovered and kills the whole process, same as an
// unrecovered panic anywhere else.
//
// Add(1) and Done are both at FIRE time -- the first and (deferred) last
// things the wrapped callback does -- NOT at arm time. An earlier version of
// this wrapper called Add(1) when After armed the timer, on the theory that
// time.AfterFunc gives the same happens-before guarantee "go func(){}()"
// does. That reasoning holds for WHETHER the callback eventually runs, but
// not for whether it runs AT ALL: three paths in work.go stop an armed timer
// before it ever fires -- schedule re-arming an existing key (a new
// delivery or a fresh failure for a loop that already has a retry pending),
// clear on a successful or lock-shed tick, and stopAll at shutdown -- and
// Stop() succeeding means the callback that would have called Done() never
// runs at all. Each of those paths left tickWG's counter permanently
// incremented with no matching Done, which hangs drainAndClose's
// tickWG.Wait() forever: the database is never closed, the pidfile is never
// removed, and the listener lock (see lockFileName) is never released, so
// the hung process keeps holding it and every later `listener start`
// refuses with "already running" until something SIGKILLs the hung
// process. Moving Add/Done to fire time closes that: a stopped timer's
// callback never runs, so it never touches tickWG at all, and there is
// nothing to leak. The cost is a retry that fires in the microseconds
// between Wait() observing zero and the database closing going
// unaccounted; what makes that harmless is the shuttingDown gate below, not
// stopAll. stopAll runs when Worker.Serve returns -- drainAndClose step 2,
// BEFORE the drain -- so a tick still draining afterwards can schedule a NEW
// timer that stopAll never saw. Such a timer fires with shuttingDown already
// cancelled and bails out without running a tick.
//
// shuttingDown is a second, independent problem this wrapper also has to
// solve. work.go's schedule already checks its own captured ctx.Err()
// before running tickOne, meant to catch "the daemon is shutting down."
// That check works for a retry armed from Worker.Wake, which captures
// workerCtx (cancelled by drainAndClose's step 2, before the drain). It
// does NOT work for a retry armed from an HTTP-delivered tick, which
// captures tickCtx (see wrapTick) -- deliberately not cancelled until AFTER
// drainAndClose returns, so an in-flight tick is allowed to finish rather
// than being cut off. That same property means work.go's own check can
// never see tickCtx as cancelled while shutdown is actually happening, so
// without shuttingDown, a retry timer armed from an HTTP delivery and
// firing during shutdown would run a full tickOne -- starting a coding
// agent -- while the rest of the process is being torn down around it.
// shuttingDown is cancelled at the very start of drainAndClose (before
// cancelServer), specifically to give this wrapper a signal that is
// trustworthy regardless of which ctx the retry happened to capture.
//
// work.go's own comment on schedule says "a shared bound belongs in the
// command that owns both" -- Server's semaphore and Worker's retry timers.
// This command is that owner. The bound itself is NOT shared today, and this
// wrapper does not make it so: it acquires no semaphore. A retry-fired tick
// runs outside Server's MaxInFlight, bounded only by the number of loops on
// this machine (one timer per loop at a time, stopped before another is
// armed). What this wrapper does own is accounting, panic recovery, and the
// shutdown gate.
func instrumentRetries(w *listener.Worker, tickWG *sync.WaitGroup, shuttingDown context.Context) {
	inner := w.After
	w.After = func(d time.Duration, f func()) *time.Timer {
		return inner(d, func() {
			// Brackets the ENTIRE callback -- whether it goes on to run f or
			// bails below on shuttingDown -- which is what step 3 of
			// drainAndClose is actually waiting to know: not "did a tick
			// run," but "has every timer that fired finished deciding what
			// to do."
			tickWG.Add(1)
			defer tickWG.Done()

			if shuttingDown.Err() != nil {
				// See this function's doc comment: work.go's own ctx.Err()
				// check inside schedule cannot see this for an
				// HTTP-delivered retry, because that ctx (tickCtx) is not
				// cancelled until after this process has already finished
				// shutting down. This is the diagnosable-but-unavoidable
				// gap in a panic's log context too: a repo/loop identity
				// would help here, but After's signature (f func()) carries
				// none, and widening it would mean changing work.go's
				// schedule and every fake in work_test.go for a minor
				// logging improvement; the recovered panic below still logs
				// a full stack trace, which is what actually locates the
				// failure in code.
				slog.Info("skipping a retry tick: the listener is shutting down")
				return
			}

			defer func() {
				if r := recover(); r != nil {
					slog.Error("retry tick panicked; recovered to keep the listener alive",
						"panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
				}
			}()
			f()
		})
	}
}

// pidfileContent is the pidfile's on-disk content: not only the pid, but the
// address `status` reports without needing to probe the socket itself.
type pidfileContent struct {
	PID  int    `json:"pid"`
	Addr string `json:"addr"`
	Port int    `json:"port"`
}

// writePidfile records this process's identity and bound address.
//
// Mode 0600, not because this file holds a secret, but because it is a
// trust anchor `stop` signals blindly: any other local account able to
// overwrite it could redirect a later SIGTERM at a process of their
// choosing.
func writePidfile(path string, pid int, addr string, port int) error {
	body, err := json.Marshal(pidfileContent{PID: pid, Addr: addr, Port: port})
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

// readPidfile reads back what writePidfile wrote. A missing file reports
// os.ErrNotExist via errors.Is, which callers use to distinguish "no
// listener has ever run" from a pidfile that exists but is unreadable.
func readPidfile(path string) (pidfileContent, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return pidfileContent{}, err
	}
	var pf pidfileContent
	if err := json.Unmarshal(raw, &pf); err != nil {
		return pidfileContent{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return pf, nil
}

// listenerLive reports whether a listener currently holds lockFileName,
// which is the only thing this command trusts as proof of that -- see
// lockFileName's own comment for why kill(pid, 0) against the pidfile's pid
// is not safe to use for this. A lock acquired successfully here is
// released again immediately; this call is a probe, not a claim.
//
// Two caveats, both accepted rather than silently ignored:
//
// It skips lock.Acquire entirely, and returns "not live" straight away,
// when lockPath has never existed. Without this, a read-only `status` or
// `stop` on a machine that has never started a listener would still create
// <home> and an empty lock file as a side effect (lock.Acquire's
// os.MkdirAll and os.OpenFile with O_CREATE) -- the common case for a fresh
// checkout or a CI box, hit every time those commands run.
//
// Once the lock file does exist, this still takes a real, briefly-held
// LOCK_EX to answer the question, and internal/lock exposes no
// non-exclusive "would this succeed" query. That means this probe can race
// a concurrent `listener start`: if this probe's Acquire lands in the
// narrow window before start's own, start's Acquire observes ErrHeld and
// fails with "already running" even though nothing was actually running --
// only this probe, for a moment. Adding a non-exclusive query to
// internal/lock is out of this task's scope for what is a narrow,
// self-correcting race (a retried `listener start` succeeds immediately
// after), so it is documented here rather than fixed.
func listenerLive(lockPath string) (bool, error) {
	if _, err := os.Stat(lockPath); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	lk, err := lock.Acquire(lockPath)
	if errors.Is(err, lock.ErrHeld) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if err := lk.Release(); err != nil {
		slog.Warn("release probe lock", "path", lockPath, "err", err)
	}
	return false, nil
}

func listenerStopCommand() *cli.Command {
	return &cli.Command{
		Name:  "stop",
		Usage: "stop the listener, whether it runs as a launchd agent, in the foreground, or both",
		Action: func(_ context.Context, _ *cli.Command) error {
			acted := false

			mgr := service.New()
			if status, err := mgr.Status(); err == nil && status.Installed {
				if err := mgr.Uninstall(); err != nil {
					return fmt.Errorf("uninstall the launchd agent: %w", err)
				}
				fmt.Println("uninstalled the launchd agent")
				acted = true
			}
			// A Status error here means either an unsupported platform
			// (internal/service's non-darwin stub) or a launchd this
			// process cannot query. Either way there is nothing installed
			// for this branch to remove, and stop must still work for a
			// foreground listener -- see the lock/pidfile handling below,
			// which is the only way to stop a listener off macOS.

			dir, err := home.Dir()
			if err != nil {
				return err
			}
			pidPath := filepath.Join(dir, pidFileName)
			lockPath := filepath.Join(dir, lockFileName)

			live, err := listenerLive(lockPath)
			if err != nil {
				return fmt.Errorf("check whether a listener is running: %w", err)
			}
			if live {
				pf, err := readPidfile(pidPath)
				if err != nil {
					return fmt.Errorf(
						"a listener is running (its lock is held) but its pidfile is unreadable: %w", err)
				}
				// The pid comes out of a JSON file on disk, and it is
				// handed straight to kill(2), which reads non-positive
				// values as broadcasts: 0 signals the CALLER's whole
				// process group, -1 every process this user owns, and any
				// other negative value a process group of its own. Nothing
				// upstream guarantees the number is a pid at all -- a
				// truncated write, a hand-edited file, or another local
				// account that got write access to it (the file is 0600 for
				// this reason, see writePidfile) is enough.
				if pf.PID <= 0 {
					return fmt.Errorf(
						"the listener pidfile at %s records pid %d, which is not a process; "+
							"delete it and stop the listener by hand", pidPath, pf.PID)
				}
				if err := syscall.Kill(pf.PID, syscall.SIGTERM); err != nil {
					return fmt.Errorf("signal listener pid %d: %w", pf.PID, err)
				}
				fmt.Printf("sent SIGTERM to listener pid %d\n", pf.PID)
				acted = true
			} else if _, statErr := os.Stat(pidPath); statErr == nil {
				// The lock is free, so whatever the pidfile says is stale:
				// left by a process that died without running its own
				// shutdown (a kill -9, a crash). Clean it up so `status`
				// does not keep reporting a dead pid forever; a listener's
				// own drainAndClose already removes this on an ordinary
				// shutdown, so this only ever fires on the unclean path.
				if rmErr := os.Remove(pidPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
					slog.Warn("remove stale pidfile", "path", pidPath, "err", rmErr)
				}
			}

			if !acted {
				fmt.Println("no listener is installed or running")
			}
			return nil
		},
	}
}

func listenerStatusCommand() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "report whether the listener is installed as a launchd agent and whether it is running",
		Action: func(_ context.Context, _ *cli.Command) error {
			mgr := service.New()
			status, statusErr := mgr.Status()
			if statusErr != nil {
				// Unsupported platform or an unreadable launchd state; not
				// fatal to this command, since the lock/pidfile below may
				// still have something to report -- see listenerStopCommand.
				fmt.Printf("launchd: unavailable (%v)\n", statusErr)
			} else {
				fmt.Printf("launchd: installed=%t running=%t", status.Installed, status.Running)
				if status.Running {
					fmt.Printf(" pid=%d", status.PID)
				}
				fmt.Println()
			}

			dir, err := home.Dir()
			if err != nil {
				return err
			}
			pidPath := filepath.Join(dir, pidFileName)
			lockPath := filepath.Join(dir, lockFileName)

			live, err := listenerLive(lockPath)
			if err != nil {
				fmt.Printf("pidfile: cannot determine liveness (%v)\n", err)
				live = false
			}

			pf, err := readPidfile(pidPath)
			switch {
			case errors.Is(err, os.ErrNotExist):
				fmt.Println("pidfile: none")
			case err != nil:
				fmt.Printf("pidfile: unreadable (%v)\n", err)
			default:
				fmt.Printf("pidfile: pid=%d alive=%t addr=%s:%d\n",
					pf.PID, live, pf.Addr, pf.Port)
			}
			return nil
		},
	}
}
