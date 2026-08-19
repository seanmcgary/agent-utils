package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/listener"
	"github.com/seanmcgary/agent-utils/internal/service"
	"github.com/seanmcgary/agent-utils/internal/settings"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/urfave/cli/v3"
)

// pidFileName is the listener's pidfile, at <home>/listener.pid.
//
// It is required, not optional. internal/proc.IsAlive cannot be reused to
// find this process: it matches a runner's command line against
// "--dispatch <id>", and the listener carries no such argument. The pidfile
// is therefore the only thing `stop` and `status` have to find and identify
// a foreground listener, which is the only mode this program supports off
// macOS (service.New's non-darwin stub refuses --daemon outright).
const pidFileName = "listener.pid"

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
			if _, err := listener.Token(); err != nil {
				return fmt.Errorf("github token: %w", err)
			}

			if c.Bool("daemon") {
				return installDaemon(overrideArgs)
			}

			def := st.WithDefaults()
			return runListener(ctx, def.Webhook.ListenAddr, def.Webhook.ListenPort, def.Webhook.Secret)
		},
	}
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
// exported error value to compare against, and adding one is out of scope
// here: internal/service is landed and reviewed.
func containsWritableRefusal(err error) bool {
	return strings.Contains(err.Error(), "writable by group or other")
}

// runListener runs the listener in the foreground until it receives SIGINT
// or SIGTERM, then shuts down in the order drainAndClose documents.
//
// ctx is accepted for symmetry with the rest of this program's Actions, but
// shutdown itself is driven by the OS signal, not by ctx: a foreground
// `listener start` must keep serving after its own CLI Action would
// otherwise be considered "done", which is exactly what a long-running
// daemon is.
func runListener(_ context.Context, addr string, port int, secret string) error {
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

	dir, err := home.Dir()
	if err != nil {
		_ = db.Close()
		return err
	}
	pidPath := filepath.Join(dir, pidFileName)

	w := listener.NewWorker(db)

	var tickWG sync.WaitGroup
	srv, err := listener.New(&listener.Server{
		Addr:   addr,
		Port:   port,
		Secret: secret,
		Tick:   wrapTick(w.Deliver, &tickWG),
	})
	if err != nil {
		_ = db.Close()
		return err
	}

	// Write the pidfile before Serve starts, not after: `status` and `stop`
	// must be able to find this process from the moment it starts
	// listening, and there is no point at which this process is "half
	// running" that they should observe instead.
	if err := writePidfile(pidPath, os.Getpid(), addr, port); err != nil {
		_ = db.Close()
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

	return drainAndClose(cancelServer, cancelWorker, serverDone, workerDone, &tickWG, db, pidPath)
}

// drainAndClose runs the shutdown sequence, in this exact order, and why:
//
//  1. Stop accepting deliveries first. Cancelling serverCtx makes
//     Server.ListenAndServe run http.Server.Shutdown, which stops the
//     listener socket and waits (up to 10s) for any still-active HTTP
//     request to finish, before ListenAndServe returns.
//  2. Only now cancel the daemon context so the worker's retry timers stop
//     (Worker.Serve's stopAll). Doing this earlier would let a retry fire
//     concurrently with the database close in step 4.
//  3. Wait for every tick already handed to the HTTP handler's bounded pool
//     to finish. Server keeps its own semaphore private (it is an
//     unexported field), so tickWG -- incremented and decremented by
//     wrapTick, around every call this process makes into Worker.Deliver --
//     is this command's own accounting of the same in-flight set.
//  4. Only now close the database. A tickOne in flight when the database
//     closes underneath it leaves the dispatches row it started stuck in
//     "running", which the next tick has to reap as an orphan (see
//     internal/loopcmd/tick.go's orphan sweep) rather than simply finishing.
//  5. Remove the pidfile last. `stop` and `status` must never observe a
//     live pid for a process that has already given up its listening
//     socket and its database handle.
func drainAndClose(
	cancelServer, cancelWorker context.CancelFunc,
	serverDone <-chan error, workerDone <-chan struct{},
	tickWG *sync.WaitGroup, db *store.DB, pidPath string,
) error {
	cancelServer()
	serveErr := <-serverDone

	cancelWorker()
	<-workerDone

	tickWG.Wait()

	dbErr := db.Close()

	if err := os.Remove(pidPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("remove pidfile", "path", pidPath, "err", err)
	}

	if serveErr != nil {
		return fmt.Errorf("listener: %w", serveErr)
	}
	return dbErr
}

// wrapTick adapts deliver (production always passes Worker.Deliver) into the
// Tick callback listener.Server.New requires, adding two things Server's own
// pool goroutine does not have.
//
// The first is tickWG: see drainAndClose step 3.
//
// The second is recover. Neither internal/listener's HTTP pool goroutine
// (handler.go's handleWebhook) nor its retry callback (work.go's schedule)
// recovers a panic, and an unrecovered panic in ANY goroutine kills the
// whole Go process -- there is no per-goroutine isolation. This command is
// the process owner, so the decision belongs here: recover is added at this
// one point in the call chain, the only one reachable without editing
// internal/listener, which is landed, reviewed, and out of this task's
// scope. It does not close the gap in the retry callback, which still runs
// unrecovered inside internal/listener's own time.AfterFunc; a delivery that
// panics is contained, a retry that panics is not. See the E6 report.
func wrapTick(deliver func(ctx context.Context, repo string), tickWG *sync.WaitGroup) func(context.Context, string) {
	return func(ctx context.Context, repo string) {
		tickWG.Add(1)
		defer tickWG.Done()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("webhook tick panicked; recovered to keep the listener alive",
					"repo", repo, "panic", fmt.Sprintf("%v", r), "stack", string(debug.Stack()))
			}
		}()
		deliver(ctx, repo)
	}
}

// pidfile is the pidfile's on-disk content: not only the pid, but the
// address status reports without needing to probe the socket itself.
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

// pidAlive reports whether pid names a process this user can signal.
//
// Unlike internal/proc.IsAlive, it does not also match a command line: that
// package's second check exists because a dispatch runner's pid could be
// reused by an unrelated process and IsAlive has a specific "--dispatch <id>"
// argument to key on. The listener carries no equivalent argument, so
// kill(pid, 0) -- "does this pid exist and can I signal it" -- is all that
// is available here. The pidfile itself is the identity check: it is
// written only by this process, at 0600, at the moment it starts serving.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
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
			// foreground listener -- see the pidfile handling below, which
			// is the only way to stop a listener off macOS.

			dir, err := home.Dir()
			if err != nil {
				return err
			}
			pidPath := filepath.Join(dir, pidFileName)
			pf, err := readPidfile(pidPath)
			switch {
			case err == nil && pidAlive(pf.PID):
				if err := syscall.Kill(pf.PID, syscall.SIGTERM); err != nil {
					return fmt.Errorf("signal listener pid %d: %w", pf.PID, err)
				}
				fmt.Printf("sent SIGTERM to listener pid %d\n", pf.PID)
				acted = true
			case err == nil:
				// A stale pidfile, left by a process that died without
				// running its own shutdown (a kill -9, a crash). Clean it
				// up so `status` does not keep reporting a dead pid
				// forever; the live process's own drainAndClose already
				// removes it on an ordinary shutdown, so this only ever
				// fires on the unclean path.
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
				// fatal to this command, since the pidfile below may still
				// have something to report -- see listenerStopCommand.
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
			pf, err := readPidfile(pidPath)
			switch {
			case errors.Is(err, os.ErrNotExist):
				fmt.Println("pidfile: none")
			case err != nil:
				fmt.Printf("pidfile: unreadable (%v)\n", err)
			default:
				fmt.Printf("pidfile: pid=%d alive=%t addr=%s:%d\n",
					pf.PID, pidAlive(pf.PID), pf.Addr, pf.Port)
			}
			return nil
		},
	}
}
