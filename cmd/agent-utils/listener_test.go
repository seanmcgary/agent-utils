package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/settings"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/urfave/cli/v3"
)

// TestMain silences per-tick and per-shutdown logging. This file's tests
// drive drainAndClose and wrapTick directly, both of which log by design
// (see their comments), and that would otherwise bury a real assertion
// failure in the noise. Precedent: internal/listener/main_test.go.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
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

// TestListenerStatusReportsLiveForegroundListenerThroughPidfile covers:
// "status reports a live foreground listener through the pidfile." It
// writes a pidfile naming this test process's own pid, which is guaranteed
// alive for the duration of the test, exactly as a real `listener start`
// would for itself.
func TestListenerStatusReportsLiveForegroundListenerThroughPidfile(t *testing.T) {
	withHome(t)

	homeDir := os.Getenv("AGENT_UTILS_HOME")
	if homeDir == "" {
		t.Fatal("AGENT_UTILS_HOME not set by withHome")
	}
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

// testSleepCmd returns an unstarted long-sleep command, real enough that
// pidAlive's kill(pid, 0) has a genuine process to check and SIGTERM has a
// genuine process to end.
func testSleepCmd(t *testing.T) *exec.Cmd {
	t.Helper()
	return exec.Command("sleep", "30")
}

// TestListenerStopSignalsLiveForegroundPid covers stop's pidfile path: "stop
// ... signal the pidfile's process when one is live." It spawns this test
// binary as a child (via os.StartProcess re-exec is overkill; a real child
// process is used instead so pidAlive's kill(pid, 0) has something genuine
// to check) and asserts `stop` sends it a signal by observing the child
// exit.
func TestListenerStopSignalsLiveForegroundPid(t *testing.T) {
	withHome(t)

	// A short-lived real process this test can wait on: `sleep 30`, killed
	// early by `listener stop` sending SIGTERM to its pid. Using a real
	// child, not this test's own pid, means the test also proves stop does
	// not just report success without truly signaling anything.
	cmd := testSleepCmd(t)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	homeDir := os.Getenv("AGENT_UTILS_HOME")
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

// TestDrainAndCloseWaitsForInFlightTickBeforeClosingDB covers: "a test
// proves shutdown drains an in-flight tick before the database is closed."
// It drives drainAndClose directly -- the exact function runListener calls
// on every shutdown path -- with a controlled in-flight tick and a real
// database, and asserts the database is still open while the tick is
// gated, and only closes once the tick finishes.
func TestDrainAndCloseWaitsForInFlightTickBeforeClosingDB(t *testing.T) {
	withHome(t)

	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
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

	pidPath := filepath.Join(t.TempDir(), pidFileName)
	if err := os.WriteFile(pidPath, []byte("placeholder"), 0o600); err != nil {
		t.Fatalf("write pidfile stand-in: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- drainAndClose(cancelServer, cancelWorker, serverDone, workerDone, &tickWG, db, pidPath)
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
		t.Errorf("pidfile still exists after drainAndClose (stat err = %v)", statErr)
	}
}

// TestWrapTickRecoversPanicAndStillDrainsWaitGroup covers the recover
// decision recorded in wrapTick's comment: a panic inside deliver must not
// escape, and tickWG must still reach zero so drainAndClose's Wait does not
// hang forever waiting on a tick that already crashed.
func TestWrapTickRecoversPanicAndStillDrainsWaitGroup(t *testing.T) {
	var tickWG sync.WaitGroup
	tick := wrapTick(func(context.Context, string) {
		panic("boom")
	}, &tickWG)

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

	waited := make(chan struct{})
	go func() {
		tickWG.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("tickWG never reached zero after a recovered panic")
	}
}
