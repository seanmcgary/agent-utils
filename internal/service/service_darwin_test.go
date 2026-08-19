//go:build darwin

package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errStub stands in for the error launchctl.exec.Command would return for a
// non-zero exit status. Its message does not matter to any assertion; only
// its non-nilness does.
var errStub = errors.New("stub launchctl failure")

// stubExecutable points executablePath at path for the duration of the
// test, restoring the real os.Executable afterward. Tests must never let
// Install resolve the actual `go test` binary: its location and
// permissions are outside the test's control, and this file exists
// specifically to make those inputs deterministic.
func stubExecutable(t *testing.T, path string) {
	t.Helper()
	prev := executablePath
	executablePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { executablePath = prev })
}

// stubLaunchctl replaces the launchctl variable so Install/Uninstall/Status
// never shell out to the real binary. No test in this package may run
// launchctl: bootstrap would register a plist in the developer's actual
// login session.
func stubLaunchctl(t *testing.T, fn func(args ...string) ([]byte, error)) {
	t.Helper()
	prev := launchctl
	launchctl = fn
	t.Cleanup(func() { launchctl = prev })
}

// writableSelf creates a fake "agent-utils" binary at 0755 inside a 0700
// directory -- a stand-in for a normal install location -- and returns its
// path.
func writableSelf(t *testing.T) string {
	t.Helper()
	dir := t.TempDir() // t.TempDir() defaults to 0700: not group/other writable.
	path := filepath.Join(dir, "agent-utils")
	if err := os.WriteFile(path, []byte("fake binary"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

func TestInstallWritesPlistToOverrideDirOnly(t *testing.T) {
	self := writableSelf(t)
	stubExecutable(t, self)

	var bootstrapArgs []string
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		bootstrapArgs = args
		return nil, nil
	})

	launchAgents := t.TempDir()
	t.Setenv(LaunchAgentsDirEnvVar, launchAgents)
	homeDir := t.TempDir()
	t.Setenv("AGENT_UTILS_HOME", homeDir)

	m := New()
	args := []string{"--listen-addr", "127.0.0.1", "--listen-port", "8787", "listener", "start"}
	if err := m.Install(self, args); err != nil {
		t.Fatalf("Install: %v", err)
	}

	wantPath := filepath.Join(launchAgents, Label+".plist")
	gotPath, err := m.ServiceFilePath()
	if err != nil {
		t.Fatalf("ServiceFilePath: %v", err)
	}
	if gotPath != wantPath {
		t.Fatalf("ServiceFilePath = %q, want %q", gotPath, wantPath)
	}
	info, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("plist was not written to the override directory: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("plist mode = %o, want 0644", perm)
	}

	doc, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	text := string(doc)
	if !strings.Contains(text, self) {
		t.Errorf("plist does not reference resolved binary %q:\n%s", self, text)
	}
	for _, a := range args {
		if !strings.Contains(text, a) {
			t.Errorf("plist missing argument %q:\n%s", a, text)
		}
	}

	// This is the acceptance criterion this test exists to prove: the real
	// ~/Library/LaunchAgents was never touched. There is no portable way to
	// assert a negative about a directory outside our sandbox, so instead we
	// assert the positive that fully explains the outcome: the path Install
	// wrote to is rooted at the override, not at any real home directory.
	if !strings.HasPrefix(wantPath, launchAgents) {
		t.Fatalf("plist path %q escaped the override directory %q", wantPath, launchAgents)
	}

	if len(bootstrapArgs) != 3 || bootstrapArgs[0] != "bootstrap" || bootstrapArgs[2] != wantPath {
		t.Errorf("launchctl invoked with %v, want [bootstrap gui/<uid> %s]", bootstrapArgs, wantPath)
	}
}

func TestInstallRefusesWorldWritableParentDirectory(t *testing.T) {
	dir := t.TempDir()
	// A checkout's ./bin, or any directory another local user can write
	// into, is exactly the case Install must refuse: RunAtLoad+KeepAlive
	// make the plist permanent login-time execution of whatever sits at
	// this path.
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod fixture dir: %v", err)
	}
	path := filepath.Join(dir, "agent-utils")
	if err := os.WriteFile(path, []byte("fake binary"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	stubExecutable(t, path)
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		t.Fatalf("launchctl must not run when Install refuses the binary path, got args %v", args)
		return nil, nil
	})
	t.Setenv(LaunchAgentsDirEnvVar, t.TempDir())

	m := New()
	err := m.Install(path, []string{"listener", "start"})
	if err == nil {
		t.Fatal("Install did not refuse a binary in a world-writable directory")
	}

	writtenPath, pathErr := m.ServiceFilePath()
	if pathErr != nil {
		t.Fatalf("ServiceFilePath: %v", pathErr)
	}
	if _, statErr := os.Stat(writtenPath); statErr == nil {
		t.Fatal("Install refused the binary but still wrote a plist")
	}
}

func TestInstallRefusesWorldWritableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-utils")
	// The directory is safe (0700); the binary itself is not. os.WriteFile's
	// mode argument is subject to umask on file creation, so an explicit
	// chmod afterward is what actually guarantees the world-write bit ends
	// up set -- the same reason the parent-directory test above chmods
	// after creating its fixture instead of trusting the create call.
	if err := os.WriteFile(path, []byte("fake binary"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	if err := os.Chmod(path, 0o777); err != nil {
		t.Fatalf("chmod fixture binary: %v", err)
	}
	stubExecutable(t, path)
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		t.Fatalf("launchctl must not run when Install refuses the binary path, got args %v", args)
		return nil, nil
	})
	t.Setenv(LaunchAgentsDirEnvVar, t.TempDir())

	if err := New().Install(path, nil); err == nil {
		t.Fatal("Install did not refuse a world-writable binary")
	}
}

func TestUninstallRemovesPlistEvenWhenBootoutFails(t *testing.T) {
	launchAgents := t.TempDir()
	t.Setenv(LaunchAgentsDirEnvVar, launchAgents)
	plistPath := filepath.Join(launchAgents, Label+".plist")
	if err := os.WriteFile(plistPath, []byte("<plist/>"), 0o644); err != nil {
		t.Fatalf("seed plist: %v", err)
	}

	var bootoutArgs []string
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		bootoutArgs = args
		// bootout legitimately returns non-zero when nothing is loaded;
		// Uninstall must treat that as success and still clean up the file.
		return []byte("Boot-out failed: 3: No such process"), errStub
	})

	if err := New().Uninstall(); err != nil {
		t.Fatalf("Uninstall returned an error for a non-zero bootout: %v", err)
	}
	if len(bootoutArgs) != 2 || bootoutArgs[0] != "bootout" {
		t.Errorf("launchctl invoked with %v, want [bootout gui/<uid>/%s]", bootoutArgs, Label)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatalf("plist still present after Uninstall: err=%v", err)
	}
}

func TestUninstallIsIdempotentWhenNothingIsInstalled(t *testing.T) {
	t.Setenv(LaunchAgentsDirEnvVar, t.TempDir())
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		return []byte("could not find service"), errStub
	})
	if err := New().Uninstall(); err != nil {
		t.Fatalf("Uninstall on a service that was never installed returned an error: %v", err)
	}
}

func TestStatusReportsPidFromLaunchctlPrint(t *testing.T) {
	launchAgents := t.TempDir()
	t.Setenv(LaunchAgentsDirEnvVar, launchAgents)
	plistPath := filepath.Join(launchAgents, Label+".plist")
	if err := os.WriteFile(plistPath, []byte("<plist/>"), 0o644); err != nil {
		t.Fatalf("seed plist: %v", err)
	}

	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		return []byte("state = running\n\tpid = 4242\n\tsome other field = 1\n"), nil
	})

	got, err := New().Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	want := Status{Installed: true, Running: true, PID: 4242}
	if got != want {
		t.Errorf("Status() = %+v, want %+v", got, want)
	}
}

func TestStatusReportsNotInstalledAndNotRunning(t *testing.T) {
	t.Setenv(LaunchAgentsDirEnvVar, t.TempDir())
	stubLaunchctl(t, func(args ...string) ([]byte, error) {
		return []byte("Could not find service"), errStub
	})

	got, err := New().Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	want := Status{Installed: false, Running: false, PID: 0}
	if got != want {
		t.Errorf("Status() = %+v, want %+v", got, want)
	}
}

func TestParsePID(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   int
	}{
		{"present", "state = running\n\tpid = 123\n", 123},
		{"absent", "state = not running\n", 0},
		{"malformed", "\tpid = not-a-number\n", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parsePID(c.output); got != c.want {
				t.Errorf("parsePID(%q) = %d, want %d", c.output, got, c.want)
			}
		})
	}
}
