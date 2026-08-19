package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/listener"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/service"
	"github.com/seanmcgary/agent-utils/internal/settings"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/urfave/cli/v3"
)

// TestMain silences per-tick and per-shutdown logging. This file's tests
// drive drainAndClose, wrapTick and instrumentRetries directly, all of which
// log by design (see their comments), and that would otherwise bury a real
// assertion failure in the noise. Precedent: internal/listener/main_test.go.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// isolateLaunchd points service.New at a scratch LaunchAgents directory
// instead of the operator's real ~/Library/LaunchAgents.
//
// Without this, any test that runs `listener stop` or `listener status` on
// darwin calls service.New().Status() (and stop calls Uninstall()), which
// internal/service/service_darwin.go resolves through launchAgentsDir --
// and that function falls back to the REAL LaunchAgents directory whenever
// LaunchAgentsDirEnvVar is unset. A developer who has ever run `listener
// start --daemon` on their own machine would have `go test ./cmd/...`
// silently run `launchctl bootout` against their real daemon and delete its
// real plist. service.LaunchAgentsDirEnvVar exists precisely so a test can
// opt out of that; see internal/service/service_darwin_test.go for the same
// pattern one layer down.
func isolateLaunchd(t *testing.T) {
	t.Helper()
	t.Setenv(service.LaunchAgentsDirEnvVar, t.TempDir())
}

// runListenerCLI runs the listener command tree against args and returns
// what it printed to stdout, mirroring project_init_test.go's
// TestProjectInitCLIPositionalNameNotFlag: a bare root built from just
// listenerCommand() so a test cannot accidentally exercise `project` or
// `config` and touch state this test does not control.
func runListenerCLI(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	root := &cli.Command{
		Name:     "agent-utils",
		Commands: []*cli.Command{listenerCommand()},
	}

	outR, outW, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatalf("stdout pipe: %v", pipeErr)
	}
	old := os.Stdout
	os.Stdout = outW
	runErr := root.Run(context.Background(), append([]string{"agent-utils"}, args...))
	os.Stdout = old
	outW.Close()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, outR); err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return buf.String(), runErr
}

// TestListenerHelpListsThreeSubcommands covers the acceptance bullet:
// "listener --help lists three subcommands."
func TestListenerHelpListsThreeSubcommands(t *testing.T) {
	out, err := runListenerCLI(t, "listener", "--help")
	if err != nil {
		t.Fatalf("listener --help: %v", err)
	}
	for _, name := range []string{"start", "stop", "status"} {
		if !strings.Contains(out, name) {
			t.Errorf("listener --help output = %q, want it to list subcommand %q", out, name)
		}
	}
}

// TestListenerStartDisabledWebhookFailsAndNamesEnableCommand covers: "listener
// start with webhook.enabled false exits non-zero and names the config
// webhook command."
func TestListenerStartDisabledWebhookFailsAndNamesEnableCommand(t *testing.T) {
	withHome(t)

	// No settings file at all: settings.Load returns the zero value, whose
	// Webhook.Enabled is false. That is the common case this guards
	// against -- a machine that has never run `config webhook --enable`.
	_, err := runListenerCLI(t, "listener", "start")
	if err == nil {
		t.Fatal("listener start with webhook disabled: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "config webhook --enable") {
		t.Errorf("error = %q, want it to name `agent-utils config webhook --enable`", err.Error())
	}
}

// TestListenerStartEmptySecretFails covers: "listener start with an empty
// secret exits non-zero."
func TestListenerStartEmptySecretFails(t *testing.T) {
	withHome(t)

	if err := settings.Save(&settings.Settings{
		Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: ""},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err := runListenerCLI(t, "listener", "start")
	if err == nil {
		t.Fatal("listener start with an empty secret: want an error, got nil")
	}
}

// TestListenerStartInvalidPortOverrideFails proves the CLI override keeps
// the same 1..65535 rule `config set webhook.listen_port` enforces, with no
// exception for the listener command: --listen-port 0 must stay rejected
// here even though a positive Port is otherwise accepted.
func TestListenerStartInvalidPortOverrideFails(t *testing.T) {
	withHome(t)

	if err := settings.Save(&settings.Settings{
		Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: "supersecretvalue"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err := runListenerCLI(t, "listener", "start", "--listen-port", "0")
	if err == nil {
		t.Fatal("listener start --listen-port 0: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "between 1 and 65535") {
		t.Errorf("error = %q, want the same range message settings.Fields uses", err.Error())
	}

	// The rejected override must never have been written to config.yaml:
	// setField validates against an in-memory copy and this command never
	// calls settings.Save at all.
	s, loadErr := settings.Load()
	if loadErr != nil {
		t.Fatalf("Load: %v", loadErr)
	}
	if s.Webhook.ListenPort != 0 {
		t.Errorf("stored ListenPort = %d, want 0 (untouched)", s.Webhook.ListenPort)
	}
}

// TestRunListenerRefusesWhenAlreadyRunning covers the CRITICAL fix: a second
// `listener start` must fail fast at the lock, before it can write a
// pidfile a live listener already owns. Simulating "already running" by
// holding lockFileName directly -- rather than actually starting a second
// listener -- keeps this test fast and deterministic: runListener returns
// at the lock check, well before it would ever bind a socket.
func TestRunListenerRefusesWhenAlreadyRunning(t *testing.T) {
	withHome(t)
	homeDir := os.Getenv("AGENT_UTILS_HOME")

	held, err := lock.Acquire(filepath.Join(homeDir, lockFileName))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	err = runListener(context.Background(), "127.0.0.1", 18080, "supersecretvalue")
	if err == nil {
		t.Fatal("runListener while the lock is held: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error = %q, want it to say a listener is already running", err.Error())
	}

	if _, statErr := os.Stat(filepath.Join(homeDir, pidFileName)); !os.IsNotExist(statErr) {
		t.Errorf("pidfile exists after a refused start (stat err = %v)", statErr)
	}
}

// TestListenerStatusReportsLiveForegroundListenerThroughPidfile covers:
// "status reports a live foreground listener through the pidfile." It holds
// lockFileName itself -- exactly what a real runListener does for its whole
// lifetime -- and writes a pidfile alongside it, since `alive` now comes
// from the lock, not from probing the pid.
func TestListenerStatusReportsLiveForegroundListenerThroughPidfile(t *testing.T) {
	withHome(t)
	isolateLaunchd(t)

	homeDir := os.Getenv("AGENT_UTILS_HOME")
	if homeDir == "" {
		t.Fatal("AGENT_UTILS_HOME not set by withHome")
	}

	held, err := lock.Acquire(filepath.Join(homeDir, lockFileName))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	pidPath := filepath.Join(homeDir, pidFileName)
	if err := writePidfile(pidPath, os.Getpid(), "127.0.0.1", 8787); err != nil {
		t.Fatalf("writePidfile: %v", err)
	}

	out, err := runListenerCLI(t, "listener", "status")
	if err != nil {
		t.Fatalf("listener status: %v", err)
	}
	if !strings.Contains(out, "alive=true") {
		t.Errorf("status output = %q, want it to report the pidfile's process as alive", out)
	}
	if !strings.Contains(out, "127.0.0.1:8787") {
		t.Errorf("status output = %q, want it to report the bound address", out)
	}
}

// TestListenerStatusReportsStaleePidfileAsNotAlive proves status's liveness
// comes from the lock, not from kill(pid, 0): it writes a pidfile naming
// this TEST process's own pid (genuinely alive) but does NOT hold the lock,
// simulating a listener that was killed -9 and left its pidfile behind. A
// pid-based check would wrongly report this alive.
func TestListenerStatusReportsStaleePidfileAsNotAlive(t *testing.T) {
	withHome(t)
	isolateLaunchd(t)

	homeDir := os.Getenv("AGENT_UTILS_HOME")
	pidPath := filepath.Join(homeDir, pidFileName)
	if err := writePidfile(pidPath, os.Getpid(), "127.0.0.1", 8787); err != nil {
		t.Fatalf("writePidfile: %v", err)
	}

	out, err := runListenerCLI(t, "listener", "status")
	if err != nil {
		t.Fatalf("listener status: %v", err)
	}
	if !strings.Contains(out, "alive=false") {
		t.Errorf("status output = %q, want alive=false: the lock is not held, "+
			"so this pidfile is stale regardless of whether its pid happens to be alive", out)
	}
}

// TestListenerStopSignalsLiveForegroundPid covers stop's pidfile path: "stop
// ... signal the pidfile's process when one is live." It spawns this test
// binary as a child (a real process, not this test's own pid) and holds the
// lock itself, matching what a real runListener does, so stop's liveness
// check has something genuine to key on.
func TestListenerStopSignalsLiveForegroundPid(t *testing.T) {
	withHome(t)
	isolateLaunchd(t)

	homeDir := os.Getenv("AGENT_UTILS_HOME")

	held, err := lock.Acquire(filepath.Join(homeDir, lockFileName))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	// A short-lived real process this test can wait on: `sleep 30`, killed
	// early by `listener stop` sending SIGTERM to its pid. Using a real
	// child, not this test's own pid, means the test also proves stop does
	// not just report success without truly signaling anything.
	cmd := testSleepCmd(t)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	pidPath := filepath.Join(homeDir, pidFileName)
	if err := writePidfile(pidPath, cmd.Process.Pid, "127.0.0.1", 8787); err != nil {
		t.Fatalf("writePidfile: %v", err)
	}

	out, err := runListenerCLI(t, "listener", "stop")
	if err != nil {
		t.Fatalf("listener stop: %v", err)
	}
	if !strings.Contains(out, "sent SIGTERM") {
		t.Errorf("stop output = %q, want it to report signaling the pid", out)
	}

	waitErrCh := make(chan error, 1)
	go func() { waitErrCh <- cmd.Wait() }()
	select {
	case <-waitErrCh:
		// The child exited, killed by the SIGTERM `stop` sent it.
	case <-time.After(5 * time.Second):
		t.Fatal("child process did not exit after `listener stop`")
	}
}

// TestListenerStopRemovesStalePidfileWithoutSignalingAnyone covers the other
// half of CRITICAL 1's fix: with the lock free, a pidfile naming this TEST
// process's own (genuinely alive) pid must NOT be signaled -- liveness comes
// from the lock, and the lock says no listener is running.
func TestListenerStopRemovesStalePidfileWithoutSignalingAnyone(t *testing.T) {
	withHome(t)
	isolateLaunchd(t)

	homeDir := os.Getenv("AGENT_UTILS_HOME")
	pidPath := filepath.Join(homeDir, pidFileName)
	if err := writePidfile(pidPath, os.Getpid(), "127.0.0.1", 8787); err != nil {
		t.Fatalf("writePidfile: %v", err)
	}

	out, err := runListenerCLI(t, "listener", "stop")
	if err != nil {
		t.Fatalf("listener stop: %v", err)
	}
	if strings.Contains(out, "SIGTERM") {
		t.Errorf("stop output = %q, must not claim to signal anything: the lock was free", out)
	}
	if _, statErr := os.Stat(pidPath); !os.IsNotExist(statErr) {
		t.Errorf("stale pidfile still exists after stop (stat err = %v)", statErr)
	}
}

// testSleepCmd returns an unstarted long-sleep command, real enough that
// SIGTERM has a genuine process to end.
func testSleepCmd(t *testing.T) *exec.Cmd {
	t.Helper()
	return exec.Command("sleep", "30")
}

// TestDrainAndCloseWaitsForInFlightTickBeforeClosingDB covers: "a test
// proves shutdown drains an in-flight tick before the database is closed."
// It drives drainAndClose directly -- the exact function runListener calls
// on every shutdown path -- with a controlled in-flight tick tracked by
// tickWG (standing in for a retry-fired tick; srv.Drain() is exercised too,
// but has nothing in flight here, so it returns immediately) and a real
// database, and asserts the database is still open while the tick is
// gated, and only closes once the tick finishes.
func TestDrainAndCloseWaitsForInFlightTickBeforeClosingDB(t *testing.T) {
	withHome(t)
	homeDir := os.Getenv("AGENT_UTILS_HOME")

	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	srv, err := listenerServerForTest(t)
	if err != nil {
		t.Fatalf("build test server: %v", err)
	}

	lk, err := lock.Acquire(filepath.Join(homeDir, lockFileName))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	var tickWG sync.WaitGroup
	release := make(chan struct{})
	dbWasOpenDuringTick := false

	tickWG.Add(1)
	go func() {
		defer tickWG.Done()
		<-release
		// If drainAndClose had already closed the database by the time
		// this in-flight tick is allowed to finish, LoopStates would fail.
		if _, err := db.LoopStates(); err == nil {
			dbWasOpenDuringTick = true
		}
	}()

	// serverDone and workerDone stand in for a server and a worker that
	// have already stopped -- this test is only about the ordering AFTER
	// both of those have exited, which is exactly where the in-flight-tick
	// hazard lives. cancelServer/cancelWorker are still real CancelFuncs so
	// drainAndClose's calls to them are exercised, even though nothing
	// downstream is listening on their contexts.
	_, cancelServer := context.WithCancel(context.Background())
	_, cancelWorker := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	serverDone <- nil
	workerDone := make(chan struct{})
	close(workerDone)

	pidPath := filepath.Join(homeDir, pidFileName)
	if err := writePidfile(pidPath, os.Getpid(), "127.0.0.1", 8787); err != nil {
		t.Fatalf("write pidfile stand-in: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- drainAndClose(cancelServer, cancelWorker, serverDone, workerDone, srv, &tickWG, db, lk, pidPath)
	}()

	// drainAndClose must still be blocked in tickWG.Wait(): it has not
	// returned, and the database must still be usable.
	select {
	case <-done:
		t.Fatal("drainAndClose returned before the in-flight tick finished")
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := db.LoopStates(); err != nil {
		t.Fatalf("database unusable while a tick is still in flight: %v", err)
	}

	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("drainAndClose: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drainAndClose did not return after the in-flight tick finished")
	}

	if !dbWasOpenDuringTick {
		t.Fatal("database was already closed while the in-flight tick was still running")
	}
	if _, err := db.LoopStates(); err == nil {
		t.Error("db.LoopStates succeeded after drainAndClose; want the database closed")
	}
	if _, statErr := os.Stat(pidPath); !os.IsNotExist(statErr) {
		t.Errorf("pidfile still exists after drainAndClose")
	}

	// The lock must be released too: a fresh Acquire against the same path
	// must now succeed.
	relocked, err := lock.Acquire(filepath.Join(homeDir, lockFileName))
	if err != nil {
		t.Fatalf("lock not released by drainAndClose: %v", err)
	}
	_ = relocked.Release()
}

// TestDrainAndCloseLeavesAnotherProcessesPidfileAlone covers the IMPORTANT
// fix: pidfile removal is conditional on the pidfile still naming THIS
// process. It writes a pidfile naming a different pid before calling
// drainAndClose, and asserts that file survives.
func TestDrainAndCloseLeavesAnotherProcessesPidfileAlone(t *testing.T) {
	withHome(t)
	homeDir := os.Getenv("AGENT_UTILS_HOME")

	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	srv, err := listenerServerForTest(t)
	if err != nil {
		t.Fatalf("build test server: %v", err)
	}
	lk, err := lock.Acquire(filepath.Join(homeDir, lockFileName))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	var tickWG sync.WaitGroup
	_, cancelServer := context.WithCancel(context.Background())
	_, cancelWorker := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	serverDone <- nil
	workerDone := make(chan struct{})
	close(workerDone)

	pidPath := filepath.Join(homeDir, pidFileName)
	// A pid that is not this process's own -- os.Getpid()+1 is never equal
	// to os.Getpid(), which is all this assertion depends on.
	if err := writePidfile(pidPath, os.Getpid()+1, "127.0.0.1", 8787); err != nil {
		t.Fatalf("write pidfile stand-in: %v", err)
	}

	if err := drainAndClose(cancelServer, cancelWorker, serverDone, workerDone, srv, &tickWG, db, lk, pidPath); err != nil {
		t.Fatalf("drainAndClose: %v", err)
	}

	if _, statErr := os.Stat(pidPath); statErr != nil {
		t.Errorf("pidfile naming another pid was removed (stat err = %v), want it left alone", statErr)
	}
}

// listenerServerForTest builds a minimal, never-served *listener.Server
// purely so drainAndClose has something to call Drain() on: with nothing
// ever dispatched through its Handler, Drain returns immediately, and this
// test's assertions are about tickWG's ordering, not Server's own.
func listenerServerForTest(t *testing.T) (*listener.Server, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		return nil, err
	}
	return listener.New(&listener.Server{
		Secret: "test-secret",
		Port:   port,
		Tick:   func(context.Context, string) {},
	})
}

// TestWrapTickUsesTickCtxNotHandlerCtx covers the IMPORTANT fix: wrapTick
// must call deliver with the tickCtx it was built with, never with whatever
// ctx Server happens to pass it, and cancelling that latter ctx (standing in
// for drainAndClose's cancelServer()) must not cancel tickCtx.
func TestWrapTickUsesTickCtxNotHandlerCtx(t *testing.T) {
	tickCtx, cancelTickCtx := context.WithCancel(context.Background())
	defer cancelTickCtx()

	seen := make(chan context.Context, 1)
	tick := wrapTick(tickCtx, func(ctx context.Context, _ string) {
		seen <- ctx
	})

	handlerCtx, cancelHandler := context.WithCancel(context.Background())
	cancelHandler() // simulate cancelServer() firing before Tick runs

	tick(handlerCtx, "owner/repo")

	select {
	case got := <-seen:
		if got != tickCtx {
			t.Fatal("wrapTick called deliver with a ctx other than tickCtx")
		}
		if got.Err() != nil {
			t.Fatalf("tickCtx.Err() = %v, want nil: a cancelled handler ctx must not cancel the tick's own context", got.Err())
		}
	default:
		t.Fatal("deliver was never called")
	}
}

// TestWrapTickRecoversPanic covers the recover decision: a panic inside
// deliver must not escape wrapTick.
func TestWrapTickRecoversPanic(t *testing.T) {
	tick := wrapTick(context.Background(), func(context.Context, string) {
		panic("boom")
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		tick(context.Background(), "owner/repo")
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("tick did not return; the panic escaped wrapTick's recover")
	}
}

// TestInstrumentRetriesRecoversPanicAndTracksWaitGroup covers the IMPORTANT
// fix: a retry-fired tick (which never passes through Tick/wrapTick, since
// work.go's schedule calls tickOne directly from the After callback) must
// still be recovered on panic and still be accounted for by tickWG, so
// drainAndClose's drain step actually covers it.
func TestInstrumentRetriesRecoversPanicAndTracksWaitGroup(t *testing.T) {
	withHome(t)
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	w := listener.NewWorker(db)
	var tickWG sync.WaitGroup
	instrumentRetries(w, &tickWG)

	fired := make(chan struct{})
	timer := w.After(time.Millisecond, func() {
		defer close(fired)
		panic("boom")
	})
	defer timer.Stop()

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("wrapped After's callback never ran")
	}

	waited := make(chan struct{})
	go func() {
		tickWG.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("tickWG never reached zero after a recovered panic in a retry-fired tick; " +
			"either Add/Done are unbalanced or the panic escaped and crashed the test binary")
	}
}
