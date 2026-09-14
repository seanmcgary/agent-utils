package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
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

// isolateService points service.New at scratch directories instead of the
// operator's real ~/Library/LaunchAgents (darwin) and /etc/systemd/system
// (linux).
//
// Without this, any test that reaches internal/service falls back to the REAL
// directory whenever the override is unset. A developer who has ever run
// `listener install` on their own machine would have `go test ./cmd/...`
// silently tear down their real service. Both override variables exist
// precisely so a test can opt out; see internal/service/selfinstall_test.go
// and service_linux_test.go for the same pattern one layer down.
//
// Note this isolates the DIRECTORY, not the command. A real `launchctl print`
// or `systemctl show` still runs for a `status` query -- both are read-only
// and neither can find anything under a scratch directory. Use fakeService
// below wherever a test needs the Manager itself under control.
func isolateService(t *testing.T) {
	t.Helper()
	t.Setenv(service.LaunchAgentsDirEnvVar, t.TempDir())
	t.Setenv(service.SystemdUnitDirEnvVar, t.TempDir())
}

// fakeManager records what the CLI asks of a Manager and answers with what
// the test tells it to.
type fakeManager struct {
	status        service.Status
	statusErr     error
	installBinary string
	installArgs   []string
	installErr    error
	uninstalled   bool
	uninstallErr  error
}

func (f *fakeManager) Install(binary string, args []string) error {
	f.installBinary = binary
	f.installArgs = args
	return f.installErr
}
func (f *fakeManager) Uninstall() error                 { f.uninstalled = true; return f.uninstallErr }
func (f *fakeManager) Status() (service.Status, error)  { return f.status, f.statusErr }
func (f *fakeManager) ServiceFilePath() (string, error) { return "/scratch/" + service.UnitName, nil }

// fakeService substitutes m for the real platform Manager, so a cmd-level
// test can assert what the CLI asked for without running launchctl,
// systemctl, or sudo.
func fakeService(t *testing.T, m *fakeManager) {
	t.Helper()
	prev := service.New
	service.New = func() service.Manager { return m }
	t.Cleanup(func() { service.New = prev })
}

// runListenerCLINoExit runs the listener command tree with cli.OsExiter
// replaced, and reports the exit code the library asked for.
//
// urfave/cli v3.11.0 does NOT return an error for an unknown subcommand: it
// prints "No help topic for '<verb>'" and calls cli.OsExiter(3), which by
// default is os.Exit. A test that asserts on a returned error therefore kills
// the whole test binary mid-suite with no failure attribution. Measured
// against this exact tree shape.
func runListenerCLINoExit(t *testing.T, args ...string) (stdout string, exitCode int) {
	t.Helper()
	prev := cli.OsExiter
	cli.OsExiter = func(code int) { exitCode = code }
	t.Cleanup(func() { cli.OsExiter = prev })
	out, _ := runListenerCLI(t, args...)
	return out, exitCode
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

// helpCommands returns the subcommand names listed in the COMMANDS: block of
// a urfave/cli help screen. Each row is indented and starts with the name.
func helpCommands(t *testing.T, out string) []string {
	t.Helper()
	var names []string
	inBlock := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "COMMANDS:") {
			inBlock = true
			continue
		}
		if inBlock {
			if strings.TrimSpace(line) == "" {
				break
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			names = append(names, strings.TrimSuffix(fields[0], ","))
		}
	}
	return names
}

// TestListenerHelpListsExactlyTheFourSubcommands pins the command surface.
// `start` and `stop` are gone: a registered service is started and stopped
// with systemctl or launchctl, and a foreground `listener run` is stopped
// with Ctrl-C. This test fails if either verb comes back, and -- unlike the
// substring check it replaces -- it fails if the subcommands disappear.
func TestListenerHelpListsExactlyTheFourSubcommands(t *testing.T) {
	out, err := runListenerCLI(t, "listener", "--help")
	if err != nil {
		t.Fatalf("listener --help: %v", err)
	}
	got := helpCommands(t, out)
	want := []string{"run", "install", "uninstall", "status"}
	if len(got) != len(want) {
		t.Fatalf("listener --help lists %v, want exactly %v\n%s", got, want, out)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("subcommand %d = %q, want %q", i, got[i], w)
		}
	}
}

// TestListenerRejectsTheRemovedVerbs proves the removal is real rather than
// a rename that left an alias behind. It goes through runListenerCLINoExit
// because the library exits the process rather than returning an error; see
// that helper's comment.
func TestListenerRejectsTheRemovedVerbs(t *testing.T) {
	for _, verb := range []string{"start", "stop"} {
		t.Run(verb, func(t *testing.T) {
			withHome(t)
			isolateService(t)
			out, code := runListenerCLINoExit(t, "listener", verb)
			if code == 0 {
				t.Fatalf("listener %s exited 0; it should be an unknown command\n%s", verb, out)
			}
		})
	}
}

// TestListenerRunDisabledWebhookFailsAndNamesEnableCommand covers: "listener
// run with webhook.enabled false exits non-zero and names the config
// webhook command."
func TestListenerRunDisabledWebhookFailsAndNamesEnableCommand(t *testing.T) {
	withHome(t)

	// No settings file at all: settings.Load returns the zero value, whose
	// Webhook.Enabled is false. That is the common case this guards
	// against -- a machine that has never run `config webhook --enable`.
	_, err := runListenerCLI(t, "listener", "run")
	if err == nil {
		t.Fatal("listener run with webhook disabled: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "config webhook --enable") {
		t.Errorf("error = %q, want it to name `agent-utils config webhook --enable`", err.Error())
	}
}

// TestListenerRunEmptySecretFails covers: "listener run with an empty
// secret exits non-zero."
func TestListenerRunEmptySecretFails(t *testing.T) {
	withHome(t)

	if err := settings.Save(&settings.Settings{
		Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: ""},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err := runListenerCLI(t, "listener", "run")
	if err == nil {
		t.Fatal("listener run with an empty secret: want an error, got nil")
	}
}

// TestListenerRunInvalidPortOverrideFails proves the CLI override keeps
// the same 1..65535 rule `config set webhook.listen_port` enforces, with no
// exception for the listener command: --listen-port 0 must stay rejected
// here even though a positive Port is otherwise accepted.
func TestListenerRunInvalidPortOverrideFails(t *testing.T) {
	withHome(t)

	if err := settings.Save(&settings.Settings{
		Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: "supersecretvalue"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err := runListenerCLI(t, "listener", "run", "--listen-port", "0")
	if err == nil {
		t.Fatal("listener run --listen-port 0: want an error, got nil")
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
// `listener run` must fail fast at the lock, before it can write a
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

	err = runListener(context.Background(), io.Discard, "127.0.0.1", 18080,
		func() (string, error) { return "supersecretvalue", nil })
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
	isolateService(t)

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

// TestListenerStatusReportsAndRemovesAStalePidfile proves two things about
// the same fixture. First, status's liveness comes from the lock, not from
// kill(pid, 0): it writes a pidfile naming this TEST process's own pid
// (genuinely alive) but does NOT hold the lock, simulating a listener that
// was killed -9 and left its pidfile behind. A pid-based check would wrongly
// report this alive.
//
// Second, status now REMOVES that pidfile. The listener's old `stop`
// subcommand used to own the cleanup and no longer exists, so without this,
// status would report the same dead pid forever.
func TestListenerStatusReportsAndRemovesAStalePidfile(t *testing.T) {
	withHome(t)
	isolateService(t)

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
	if !strings.Contains(out, "stale") {
		t.Errorf("status output = %q, want it to say the pidfile was stale", out)
	}
	if _, statErr := os.Stat(pidPath); !os.IsNotExist(statErr) {
		t.Errorf("listener status left the stale pidfile at %s", pidPath)
	}
}

// TestListenerStatusLabelsTheBackendNeutrally pins the rename from
// "launchd:" to "service:". The backend is launchd on darwin and systemd on
// linux, and the label must not name one of them.
func TestListenerStatusLabelsTheBackendNeutrally(t *testing.T) {
	withHome(t)
	isolateService(t)
	fakeService(t, &fakeManager{status: service.Status{Installed: true, Running: true, PID: 7}})

	out, err := runListenerCLI(t, "listener", "status")
	if err != nil {
		t.Fatalf("listener status: %v", err)
	}
	if !strings.Contains(out, "service: installed=true running=true pid=7") {
		t.Errorf("listener status does not report the Manager's answer:\n%s", out)
	}
	if strings.Contains(out, "launchd:") {
		t.Errorf("listener status still names launchd:\n%s", out)
	}
}

// TestListenerUninstallWithNothingInstalledSaysSo covers the idempotent path
// an operator hits on a machine that never ran `listener install`.
func TestListenerUninstallWithNothingInstalledSaysSo(t *testing.T) {
	withHome(t)
	isolateService(t)
	fake := &fakeManager{status: service.Status{Installed: false}}
	fakeService(t, fake)

	out, err := runListenerCLI(t, "listener", "uninstall")
	if err != nil {
		t.Fatalf("listener uninstall: %v", err)
	}
	if !strings.Contains(out, "no listener service is installed") {
		t.Errorf("listener uninstall did not report an empty machine:\n%s", out)
	}
	if fake.uninstalled {
		t.Error("listener uninstall called Uninstall with nothing installed")
	}
}

// TestListenerUninstallOnAnUnsupportedPlatformSaysSo covers the other reason
// there is nothing to remove.
func TestListenerUninstallOnAnUnsupportedPlatformSaysSo(t *testing.T) {
	withHome(t)
	isolateService(t)
	fakeService(t, &fakeManager{statusErr: service.ErrUnsupported})

	out, err := runListenerCLI(t, "listener", "uninstall")
	if err != nil {
		t.Fatalf("listener uninstall: %v", err)
	}
	if !strings.Contains(out, "no listener service is installed") {
		t.Errorf("listener uninstall did not report an unsupported platform:\n%s", out)
	}
}

// TestListenerUninstallSurfacesARealStatusFailure is the fail-closed case.
// A Status error that is NOT ErrUnsupported means this machine's
// registration could not be READ -- a permission problem, an I/O error --
// and a root-owned, boot-persistent unit may well still be installed.
// Telling the operator "nothing is installed" and exiting 0 would be a lie
// at the one moment it matters.
func TestListenerUninstallSurfacesARealStatusFailure(t *testing.T) {
	withHome(t)
	isolateService(t)
	fakeService(t, &fakeManager{statusErr: errors.New("permission denied")})

	if _, err := runListenerCLI(t, "listener", "uninstall"); err == nil {
		t.Fatal("listener uninstall reported success for an unreadable registration")
	}
}

// TestListenerUninstallRemovesAnInstalledService covers the ordinary path.
func TestListenerUninstallRemovesAnInstalledService(t *testing.T) {
	withHome(t)
	isolateService(t)
	fake := &fakeManager{status: service.Status{Installed: true}}
	fakeService(t, fake)

	out, err := runListenerCLI(t, "listener", "uninstall")
	if err != nil {
		t.Fatalf("listener uninstall: %v", err)
	}
	if !fake.uninstalled {
		t.Error("listener uninstall did not call Uninstall")
	}
	if !strings.Contains(out, "uninstalled") {
		t.Errorf("listener uninstall said nothing about what it did:\n%s", out)
	}
}

// TestListenerInstallRegistersListenerRunNeverInstall is the test for the
// one hazard installService's own comment names: the registered command must
// be `listener run`. Registering `listener install` would make the service
// reinstall itself at every start.
func TestListenerInstallRegistersListenerRunNeverInstall(t *testing.T) {
	withHome(t)
	isolateService(t)
	if err := settings.Save(&settings.Settings{
		Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: "a-secret"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Setenv("GITHUB_TOKEN", "")
	if _, err := listener.SetToken("ghp_testtoken"); err != nil {
		t.Fatalf("SetToken: %v", err)
	}

	fake := &fakeManager{}
	fakeService(t, fake)

	if _, err := runListenerCLI(t, "listener", "install", "--listen-port", "8788"); err != nil {
		t.Fatalf("listener install: %v", err)
	}
	want := []string{"listener", "run", "--listen-port", "8788"}
	if strings.Join(fake.installArgs, " ") != strings.Join(want, " ") {
		t.Errorf("installed args = %v, want %v", fake.installArgs, want)
	}
	// removedDaemonFlag is split rather than a literal so this line does not
	// itself match the repo-wide grep for the BoolFlag task 4 deleted.
	removedDaemonFlag := "--" + "daemon"
	for _, a := range fake.installArgs {
		if a == "install" || a == removedDaemonFlag {
			t.Errorf("installed args name a service-management verb: %v", fake.installArgs)
		}
	}
}

// TestListenerInstallDisabledWebhookFailsBeforeTouchingTheServiceManager
// pins the check order. `install` must make exactly the checks `run` makes,
// and make them first: a service registered against a disabled webhook, an
// empty secret, or an unreadable token comes up looking healthy and then
// fails every single delivery, with nothing at a terminal to say why.
func TestListenerInstallDisabledWebhookFailsBeforeTouchingTheServiceManager(t *testing.T) {
	withHome(t)
	isolateService(t)
	fake := &fakeManager{}
	fakeService(t, fake)

	// No settings file at all: settings.Load returns the zero value, whose
	// Webhook.Enabled is false.
	_, err := runListenerCLI(t, "listener", "install")
	if err == nil {
		t.Fatal("listener install with the webhook disabled: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "config webhook --enable") {
		t.Errorf("error %q does not name the command that fixes it", err)
	}
	if fake.installArgs != nil {
		t.Error("listener install reached the service manager before its checks")
	}
}

// TestListenerInstallEmptySecretFails is the second of the four checks.
func TestListenerInstallEmptySecretFails(t *testing.T) {
	withHome(t)
	isolateService(t)
	fake := &fakeManager{}
	fakeService(t, fake)

	if err := settings.Save(&settings.Settings{
		Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: ""},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, err := runListenerCLI(t, "listener", "install")
	if err == nil {
		t.Fatal("listener install with an empty secret: want an error, got nil")
	}
	if fake.installArgs != nil {
		t.Error("listener install reached the service manager before its checks")
	}
}

// TestContainsWritableRefusalMatchesOnlyTheSharedConstant pins ruling 1 from
// task-4: containsWritableRefusal matches on service.WritableRefusalPhrase,
// the exported constant refuseIfWritableByOthers (internal/service/service.go)
// builds its refusal from, rather than on a literal copy of the phrase kept
// on this side. That is what makes a wording drift between the two
// impossible to express rather than merely detectable, so this test builds a
// synthetic error containing the constant instead of driving a real Install
// against a writable path -- doing that for real would write a genuine
// launchd plist or systemd unit and run a real launchctl/systemctl, which
// global test policy forbids.
func TestContainsWritableRefusalMatchesOnlyTheSharedConstant(t *testing.T) {
	writable := fmt.Errorf("refusing to install: /some/path is %s", service.WritableRefusalPhrase)
	if !containsWritableRefusal(writable) {
		t.Errorf("containsWritableRefusal(%v) = false, want true", writable)
	}
	unrelated := errors.New("permission denied")
	if containsWritableRefusal(unrelated) {
		t.Errorf("containsWritableRefusal(%v) = true, want false", unrelated)
	}

	explainedWritable := explainInstallErr(writable)
	if !strings.Contains(explainedWritable.Error(), "~/bin") {
		t.Errorf("explainInstallErr(%v) = %v, want it to add the ~/bin operator guidance",
			writable, explainedWritable)
	}
	explainedUnrelated := explainInstallErr(unrelated)
	if strings.Contains(explainedUnrelated.Error(), "~/bin") {
		t.Errorf("explainInstallErr(%v) = %v, want no operator guidance for an unrelated error",
			unrelated, explainedUnrelated)
	}
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
	// hazard lives. cancelShuttingDown/cancelServer/cancelWorker are still
	// real CancelFuncs so drainAndClose's calls to them are exercised, even
	// though nothing downstream is listening on their contexts.
	_, cancelShuttingDown := context.WithCancel(context.Background())
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
		done <- drainAndClose(cancelShuttingDown, cancelServer, cancelWorker, serverDone, workerDone, srv, &tickWG, db, lk, pidPath)
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
	_, cancelShuttingDown := context.WithCancel(context.Background())
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

	if err := drainAndClose(cancelShuttingDown, cancelServer, cancelWorker, serverDone, workerDone, srv, &tickWG, db, lk, pidPath); err != nil {
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
		Secret: func() (string, error) { return "test-secret", nil },
		Port:   port,
		Tick:   func(context.Context, listener.Delivery) {},
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
	tick := wrapTick(tickCtx, func(ctx context.Context, _ listener.Delivery) {
		seen <- ctx
	})

	handlerCtx, cancelHandler := context.WithCancel(context.Background())
	cancelHandler() // simulate cancelServer() firing before Tick runs

	tick(handlerCtx, listener.Delivery{Repo: "owner/repo", Number: 7})

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
	tick := wrapTick(context.Background(), func(context.Context, listener.Delivery) {
		panic("boom")
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		tick(context.Background(), listener.Delivery{Repo: "owner/repo", Number: 7})
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
	shuttingDown, cancelShuttingDown := context.WithCancel(context.Background())
	defer cancelShuttingDown()
	instrumentRetries(w, &tickWG, shuttingDown)

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

// TestInstrumentRetriesDrainCompletesAfterAStoppedTimer covers the CRITICAL
// regression a prior review round found: work.go stops an armed retry timer
// before it fires along three paths (schedule re-arming an existing key,
// clear on a success or a lock-shed tick, and stopAll at shutdown). If
// tickWG's counter is incremented when the timer is ARMED rather than when
// it FIRES, every one of those paths leaves the counter permanently
// stuck above zero with no matching Done -- and since nothing bounds
// drainAndClose's tickWG.Wait() with a timeout, that hangs the whole
// shutdown forever: the database is never closed, the pidfile is never
// removed, and the listener lock is never released, so every later
// `listener run` refuses with "already running" until the process is
// SIGKILLed. This test stops an armed timer and asserts the drain still
// completes -- the case the previous round's suite did not cover.
func TestInstrumentRetriesDrainCompletesAfterAStoppedTimer(t *testing.T) {
	withHome(t)
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	w := listener.NewWorker(db)
	var tickWG sync.WaitGroup
	shuttingDown, cancelShuttingDown := context.WithCancel(context.Background())
	defer cancelShuttingDown()
	instrumentRetries(w, &tickWG, shuttingDown)

	fired := false
	timer := w.After(time.Hour, func() {
		fired = true
	})
	if !timer.Stop() {
		t.Fatal("Stop reported the timer had already fired or was already stopped; " +
			"this test needs a timer it can genuinely stop before it fires")
	}

	waited := make(chan struct{})
	go func() {
		tickWG.Wait()
		close(waited)
	}()

	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("tickWG.Wait() did not return after an armed timer was stopped before firing; " +
			"drainAndClose would hang forever, holding the database open and the listener lock held")
	}

	if fired {
		t.Fatal("the stopped timer's callback ran; Stop() should have prevented that")
	}
}

// TestInstrumentRetriesSkipsFiringDuringShutdown covers the IMPORTANT fix:
// a retry timer that fires after shuttingDown has been cancelled must not
// run the wrapped tick body at all -- it must not start a coding agent on
// the way out -- while still accounting for itself in tickWG so the drain
// does not hang waiting for a decision that has already been made.
func TestInstrumentRetriesSkipsFiringDuringShutdown(t *testing.T) {
	withHome(t)
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	w := listener.NewWorker(db)
	var tickWG sync.WaitGroup
	shuttingDown, cancelShuttingDown := context.WithCancel(context.Background())
	instrumentRetries(w, &tickWG, shuttingDown)

	// Shutdown is already underway before the timer ever fires -- exactly
	// the case an HTTP-delivered retry can reach, since tickCtx (what
	// work.go's own ctx.Err() check inside schedule would see for such a
	// retry) is not cancelled until after the drain completes.
	cancelShuttingDown()

	ran := false
	timer := w.After(time.Millisecond, func() {
		ran = true
	})
	defer timer.Stop()

	waited := make(chan struct{})
	go func() {
		tickWG.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("tickWG never reached zero after a shutdown-skipped retry")
	}

	if ran {
		t.Fatal("the wrapped callback ran the retry tick after shuttingDown was already cancelled")
	}
}

// routingTableFor is the shape of the fixtures below: a routing table built
// by hand, so these tests exercise the FORMAT without a registry, a project
// directory or a terminal. What the scan itself finds is covered one layer
// down, in internal/listener/route_test.go.
func routingTableFor(targets []listener.Target, skips []listener.Skip) string {
	return routingTable(listener.Routes{Targets: targets, Skips: skips})
}

// loopsUnder returns the indented loop lines that follow the line naming
// repo, which is what "grouped by repository" has to mean to be worth
// printing: a flat list that happens to contain the right strings would
// satisfy a Contains assertion while telling an operator nothing.
func loopsUnder(table, repo string) []string {
	var out []string
	found := false
	for _, line := range strings.Split(table, "\n") {
		if strings.TrimSpace(line) == repo && strings.HasPrefix(line, "  ") {
			found = true
			continue
		}
		if !found {
			continue
		}
		if !strings.HasPrefix(line, "    ") {
			break
		}
		out = append(out, strings.TrimSpace(line))
	}
	return out
}

func TestRoutingTableGroupsLoopsUnderTheirRepository(t *testing.T) {
	table := routingTableFor([]listener.Target{
		{ProjectName: "lawndominator", LoopName: "planning", Repo: "mcgarylabs/lawndominator-monorepo"},
		{ProjectName: "lawndominator", LoopName: "execution", Repo: "mcgarylabs/lawndominator-monorepo"},
		{ProjectName: "widgets", LoopName: "planning", Repo: "acme/widgets"},
	}, nil)

	if !strings.Contains(table, "2 repositories") {
		t.Errorf("table does not count the repositories it will route:\n%s", table)
	}
	got := loopsUnder(table, "mcgarylabs/lawndominator-monorepo")
	want := []string{"lawndominator/execution", "lawndominator/planning"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("loops under the monorepo = %q, want %q:\n%s", got, want, table)
	}
	if got := loopsUnder(table, "acme/widgets"); len(got) != 1 || got[0] != "widgets/planning" {
		t.Errorf("loops under acme/widgets = %q, want [widgets/planning]:\n%s", got, table)
	}
}

// One repository, singular. A daemon routing exactly one repository is the
// common case, and "1 repositories" is the kind of thing an operator reads as
// "this output is not being looked after".
func TestRoutingTableCountsOneRepositoryInTheSingular(t *testing.T) {
	table := routingTableFor([]listener.Target{
		{ProjectName: "widgets", LoopName: "planning", Repo: "acme/widgets"},
	}, nil)

	if !strings.Contains(table, "1 repository") || strings.Contains(table, "1 repositories") {
		t.Errorf("table = %q, want the count in the singular", table)
	}
}

// The case this banner exists for. A daemon that routes nothing verifies
// signatures and accepts deliveries exactly like a healthy one, so the
// startup output is the only place the difference can show.
func TestRoutingTableZeroCaseSaysWhyItIsAProblemAndNamesTheCauses(t *testing.T) {
	table := routingTableFor(nil, nil)

	for _, want := range []string{
		"NOT LISTENING",            // hard to miss in a terminal
		"accept",                   // what it will still do with a delivery
		"agent-utils project init", // cause one: nothing registered here
		"repo:",                    // cause two: no loop declares a repository
	} {
		if !strings.Contains(table, want) {
			t.Errorf("the zero case does not mention %q:\n%s", want, table)
		}
	}
}

// A skip is why an operator's repository is missing from the table above, so
// the two have to appear together -- an early return on "no targets" that
// dropped the skips would hide the very thing that explains the zero case.
func TestRoutingTableZeroCaseStillReportsWhatWasSkipped(t *testing.T) {
	table := routingTableFor(nil, []listener.Skip{
		{Project: "alpha", Dir: "/gone/.agent-utils", Reason: "the project directory no longer exists"},
	})

	if !strings.Contains(table, "NOT LISTENING") {
		t.Errorf("the zero-case warning disappeared once there was a skip:\n%s", table)
	}
	if !strings.Contains(table, "alpha") || !strings.Contains(table, "/gone/.agent-utils") {
		t.Errorf("the skip is not reported next to the zero case:\n%s", table)
	}
}

func TestRoutingTableNamesTheFileAndErrorOfASkippedLoop(t *testing.T) {
	table := routingTableFor([]listener.Target{
		{ProjectName: "widgets", LoopName: "planning", Repo: "acme/widgets"},
	}, []listener.Skip{
		{Project: "alpha", Dir: "/gone/.agent-utils", Reason: "the project directory no longer exists"},
		{Project: "beta", Dir: "/here/.agent-utils", File: "broken.yaml",
			Reason: "cannot load config: field this_key_does_not_exist not found"},
	})

	for _, want := range []string{
		"broken.yaml",
		"this_key_does_not_exist",
		"alpha",
		"/gone/.agent-utils",
		"beta",
	} {
		if !strings.Contains(table, want) {
			t.Errorf("the skipped entries do not mention %q:\n%s", want, table)
		}
	}
	// The skips must not be mistaken for routing: they come after the table
	// of repositories, not inside it.
	if strings.Index(table, "acme/widgets") > strings.Index(table, "broken.yaml") {
		t.Errorf("the skips are printed before the routing table:\n%s", table)
	}
}

// A config that does not parse yields a MULTI-line error: gopkg.in/yaml.v3
// reports "yaml: unmarshal errors:\n  line 2: field ... not found". Printed
// raw, its second line lands in the skip list at its own indentation and
// reads as a separate skipped entry -- which is what this looked like the
// first time the banner ran against a real broken config.
func TestRoutingTableKeepsEachSkipOnOneLine(t *testing.T) {
	table := routingTableFor([]listener.Target{
		{ProjectName: "widgets", LoopName: "planning", Repo: "acme/widgets"},
	}, []listener.Skip{
		{Project: "beta", Dir: "/here/.agent-utils", File: "broken.yaml",
			Reason: "cannot load config: yaml: unmarshal errors:\n  line 2: field this_key_does_not_exist not found"},
	})

	var skipLines []string
	for _, line := range strings.Split(strings.TrimSpace(table), "\n") {
		if strings.Contains(line, "broken.yaml") || strings.Contains(line, "this_key_does_not_exist") {
			skipLines = append(skipLines, line)
		}
	}
	if len(skipLines) != 1 {
		t.Fatalf("the skip spans %d lines, want one:\n%s", len(skipLines), table)
	}
	if !strings.Contains(skipLines[0], "line 2: field this_key_does_not_exist not found") {
		t.Errorf("flattening the skip lost the detail of the error: %q", skipLines[0])
	}
}
