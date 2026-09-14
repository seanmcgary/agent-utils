//go:build linux

package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/home"
)

// call records one invocation of the runCommand seam.
type call struct {
	stdin string
	name  string
	args  []string
}

// line renders a call the way the assertions below compare them.
func (c call) line() string {
	return strings.TrimSpace(c.name + " " + strings.Join(c.args, " "))
}

// errStubLinux stands in for the error exec.Command returns for a non-zero
// exit status. Only its non-nilness matters to any assertion here.
var errStubLinux = errors.New("stub command failure")

// stubRunCommand replaces the runCommand variable so no test in this package
// ever runs a real systemctl, tee, chmod, rm, or sudo. This is not a
// convenience: `systemctl enable --now` would register the listener on the
// developer's own machine, `sudo` would block the suite on a password
// prompt, and `tee` would write a file.
func stubRunCommand(t *testing.T, fn func(stdin []byte, name string, args ...string) ([]byte, error)) *[]call {
	t.Helper()
	var calls []call
	prev := runCommand
	runCommand = func(stdin []byte, name string, args ...string) ([]byte, error) {
		calls = append(calls, call{stdin: string(stdin), name: name, args: args})
		if fn == nil {
			return nil, nil
		}
		return fn(stdin, name, args...)
	}
	t.Cleanup(func() { runCommand = prev })
	return &calls
}

// isolate points the unit directory and the agent-utils home directory at
// scratch directories and returns the unit directory.
//
// Setting SystemdUnitDirEnvVar also drops the sudo prefix from privileged(),
// by design -- see that function. So every test that calls isolate asserts
// on UNPRIVILEGED command names. TestInstallUsesSudoAndTheDefaultUnitPath is
// the one test that does not call isolate, and it is what proves the
// privileged form.
func isolate(t *testing.T) string {
	t.Helper()
	unitDir := t.TempDir()
	t.Setenv(SystemdUnitDirEnvVar, unitDir)
	t.Setenv(home.EnvVar, t.TempDir())
	return unitDir
}

func TestServiceFilePathUsesTheOverrideDirectory(t *testing.T) {
	unitDir := isolate(t)
	got, err := New().ServiceFilePath()
	if err != nil {
		t.Fatalf("ServiceFilePath: %v", err)
	}
	if want := filepath.Join(unitDir, UnitName); got != want {
		t.Fatalf("ServiceFilePath = %q, want %q", got, want)
	}
}

// TestInstallUsesSudoAndTheDefaultUnitPath is the only test here that leaves
// SystemdUnitDirEnvVar unset, and it covers the two things that only the
// unset case can show: the default unit directory, and the sudo prefix.
// Nothing is actually written or run, because runCommand is stubbed.
func TestInstallUsesSudoAndTheDefaultUnitPath(t *testing.T) {
	t.Setenv(SystemdUnitDirEnvVar, "")
	t.Setenv(home.EnvVar, t.TempDir())
	self := privateSelf(t)
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
	calls := stubRunCommand(t, nil)

	if err := New().Install(self, []string{"listener", "run"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(*calls) == 0 {
		t.Fatal("Install ran no commands")
	}
	wantPath := "/etc/systemd/system/" + UnitName
	first := (*calls)[0]
	if first.name != "sudo" {
		t.Errorf("first command ran as %q, want sudo", first.name)
	}
	if first.args[0] != "tee" || first.args[len(first.args)-1] != wantPath {
		t.Errorf("first command = %v, want tee writing %s", first.args, wantPath)
	}
	for _, c := range *calls {
		if c.name != "sudo" {
			t.Errorf("command %q did not go through sudo", c.line())
		}
	}
}

func TestInstallTeesThenChmodsThenReloadsThenEnables(t *testing.T) {
	unitDir := isolate(t)
	self := privateSelf(t)
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
	calls := stubRunCommand(t, nil)

	if err := New().Install(self, []string{"listener", "run", "--listen-port", "8788"}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	unitPath := filepath.Join(unitDir, UnitName)
	want := []string{
		"tee " + unitPath,
		"chmod 0644 " + unitPath,
		"systemctl daemon-reload",
		"systemctl enable --now " + UnitName,
	}
	if len(*calls) != len(want) {
		t.Fatalf("Install ran %d commands, want %d: %+v", len(*calls), len(want), *calls)
	}
	for i, w := range want {
		if got := (*calls)[i].line(); got != w {
			t.Errorf("command %d = %q, want %q", i, got, w)
		}
	}
}

// TestInstallSendsTheUnitOnStdinAndNeverStagesAFile is the core security
// assertion of this task. The rendered unit goes from memory to root's
// stdin. It is never written to a file that a non-root process could
// rewrite between the write and the privileged read.
func TestInstallSendsTheUnitOnStdinAndNeverStagesAFile(t *testing.T) {
	isolate(t)
	self := privateSelf(t)
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
	calls := stubRunCommand(t, nil)

	args := []string{"listener", "run", "--listen-addr", "127.0.0.1"}
	if err := New().Install(self, args); err != nil {
		t.Fatalf("Install: %v", err)
	}

	doc := (*calls)[0].stdin
	if doc == "" {
		t.Fatal("Install did not send the unit on stdin")
	}
	if !strings.Contains(doc, self) {
		t.Errorf("unit does not name the resolved binary %q:\n%s", self, doc)
	}
	for _, a := range args {
		if !strings.Contains(doc, a) {
			t.Errorf("unit is missing argument %q:\n%s", a, doc)
		}
	}
	if !strings.Contains(doc, "[Install]") {
		t.Errorf("unit is not a complete unit file:\n%s", doc)
	}
}

// TestInstallCarriesAgentUtilsHomeIntoTheUnit covers the override case:
// without the Environment line, a machine with a moved home directory would
// install a service that reads a directory its operator never looks at.
func TestInstallCarriesAgentUtilsHomeIntoTheUnit(t *testing.T) {
	isolate(t)
	moved := t.TempDir()
	t.Setenv(home.EnvVar, moved)
	self := privateSelf(t)
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
	calls := stubRunCommand(t, nil)

	if err := New().Install(self, []string{"listener", "run"}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	doc := (*calls)[0].stdin
	if !strings.Contains(doc, `Environment="`+home.EnvVar+`=`+moved+`"`) {
		t.Errorf("unit does not carry %s:\n%s", home.EnvVar, doc)
	}
	if !strings.Contains(doc, "WorkingDirectory="+moved) {
		t.Errorf("unit's WorkingDirectory is not the overridden home:\n%s", doc)
	}
}

// TestInstallStopsAtTheFirstFailure proves Install does not enable a unit it
// failed to place.
func TestInstallStopsAtTheFirstFailure(t *testing.T) {
	isolate(t)
	self := privateSelf(t)
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
	calls := stubRunCommand(t, func(stdin []byte, name string, args ...string) ([]byte, error) {
		return []byte("tee: permission denied"), errStubLinux
	})

	err := New().Install(self, []string{"listener", "run"})
	if err == nil {
		t.Fatal("Install did not report the failure")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error %q does not carry the command's own output", err)
	}
	if len(*calls) != 1 {
		t.Errorf("Install ran %d commands after a failed placement, want 1: %+v", len(*calls), *calls)
	}
}

// TestInstallRefusesToRunAsRoot is the guard against the worst outcome in
// this change: a unit that runs the agent dispatcher as root at every boot.
// The refusal is on the effective user identifier alone, NOT on SUDO_USER
// being set -- `su -`, a root login shell, `doas`, and
// `sudo env -u SUDO_USER ...` all reach Install with euid 0 and no
// SUDO_USER.
func TestInstallRefusesToRunAsRoot(t *testing.T) {
	isolate(t)
	self := privateSelf(t)
	stubExecutable(t, self)
	calls := stubRunCommand(t, nil)

	for _, sudoUser := range []string{"sean", ""} {
		t.Run("SUDO_USER="+sudoUser, func(t *testing.T) {
			t.Setenv("SUDO_USER", sudoUser)
			stubGeteuid(t, 0)

			err := New().Install(self, []string{"listener", "run"})
			if err == nil {
				t.Fatal("Install accepted an effective user identifier of 0")
			}
			if !strings.Contains(err.Error(), "as yourself") {
				t.Errorf("refusal %q does not tell the operator what to do instead", err)
			}
			if len(*calls) != 0 {
				t.Errorf("Install ran %d commands before refusing: %+v", len(*calls), *calls)
			}
		})
	}
}

func TestInstallRefusesAWorldWritableBinaryDirectory(t *testing.T) {
	isolate(t)
	loose := filepath.Join(privateDir(t), "loose")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(loose, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	self := filepath.Join(loose, "agent-utils")
	if err := os.WriteFile(self, []byte("fake binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
	calls := stubRunCommand(t, nil)

	err := New().Install(self, []string{"listener", "run"})
	if err == nil {
		t.Fatal("Install accepted a world-writable binary directory")
	}
	if !strings.Contains(err.Error(), "writable by group or other") {
		t.Errorf("refusal %q is not the writable-path refusal", err)
	}
	if len(*calls) != 0 {
		t.Errorf("Install ran %d commands before refusing: %+v", len(*calls), *calls)
	}
}

// TestInstallRefusesAMismatchedBinaryArgument mirrors the darwin contract:
// a caller naming a path other than the running binary is confused, and
// failing loudly is cheaper than silently installing something else.
func TestInstallRefusesAMismatchedBinaryArgument(t *testing.T) {
	isolate(t)
	self := privateSelf(t)
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
	calls := stubRunCommand(t, nil)

	other := privateSelf(t)
	if err := New().Install(other, []string{"listener", "run"}); err == nil {
		t.Fatal("Install accepted a binary argument that is not the running binary")
	}
	if len(*calls) != 0 {
		t.Errorf("Install ran %d commands before refusing: %+v", len(*calls), *calls)
	}
}

// TestInstallRefusesAnUnsafeArgumentAndRunsNothing proves renderUnit's
// refusal reaches all the way out, and that it happens before any command.
func TestInstallRefusesAnUnsafeArgumentAndRunsNothing(t *testing.T) {
	isolate(t)
	self := privateSelf(t)
	stubExecutable(t, self)
	stubGeteuid(t, 1000)
	calls := stubRunCommand(t, nil)

	err := New().Install(self, []string{"listener", "run", "--listen-addr", "%h"})
	if err == nil {
		t.Fatal("Install accepted an argument containing a systemd specifier")
	}
	if !strings.Contains(err.Error(), "render") {
		t.Errorf("error %q does not name the render step", err)
	}
	if len(*calls) != 0 {
		t.Errorf("Install ran %d commands before refusing: %+v", len(*calls), *calls)
	}
}

func TestUninstallWithNoUnitFileRunsNothing(t *testing.T) {
	isolate(t)
	calls := stubRunCommand(t, nil)

	if err := New().Uninstall(); err != nil {
		t.Fatalf("Uninstall on a machine with no unit: %v", err)
	}
	if len(*calls) != 0 {
		t.Errorf("Uninstall ran %d commands with no unit installed: %+v", len(*calls), *calls)
	}
}

// seedUnit writes a unit file into the override directory so Uninstall and
// Status have something to find.
func seedUnit(t *testing.T, unitDir string) string {
	t.Helper()
	path := filepath.Join(unitDir, UnitName)
	if err := os.WriteFile(path, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("seed unit file: %v", err)
	}
	return path
}

func TestUninstallDisablesRemovesThenReloads(t *testing.T) {
	unitDir := isolate(t)
	unitPath := seedUnit(t, unitDir)
	calls := stubRunCommand(t, nil)

	if err := New().Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	want := []string{
		"systemctl disable --now " + UnitName,
		"rm -f " + unitPath,
		"systemctl daemon-reload",
	}
	if len(*calls) != len(want) {
		t.Fatalf("Uninstall ran %d commands, want %d: %+v", len(*calls), len(want), *calls)
	}
	for i, w := range want {
		if got := (*calls)[i].line(); got != w {
			t.Errorf("command %d = %q, want %q", i, got, w)
		}
	}
}

// TestUninstallContinuesAfterAFailedDisable pins idempotence: the unit may
// already be disabled, and `listener uninstall` must still remove the file.
func TestUninstallContinuesAfterAFailedDisable(t *testing.T) {
	unitDir := isolate(t)
	seedUnit(t, unitDir)
	calls := stubRunCommand(t, func(stdin []byte, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "disable" {
			return []byte("Failed to disable unit: Unit file does not exist."), errStubLinux
		}
		return nil, nil
	})

	if err := New().Uninstall(); err != nil {
		t.Fatalf("Uninstall did not tolerate a failed disable: %v", err)
	}
	if len(*calls) != 3 {
		t.Errorf("Uninstall ran %d commands, want 3: %+v", len(*calls), *calls)
	}
}

// TestUninstallFailsOnAFailedRemoval proves the tolerance above is scoped to
// the disable step. A unit file that could not be removed is still installed,
// and saying otherwise would be a lie.
func TestUninstallFailsOnAFailedRemoval(t *testing.T) {
	unitDir := isolate(t)
	seedUnit(t, unitDir)
	stubRunCommand(t, func(stdin []byte, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "-f" {
			return []byte("rm: permission denied"), errStubLinux
		}
		return nil, nil
	})

	if err := New().Uninstall(); err == nil {
		t.Fatal("Uninstall did not report a failed removal")
	}
}

func TestStatusReportsActiveUnitWithItsPID(t *testing.T) {
	unitDir := isolate(t)
	seedUnit(t, unitDir)
	calls := stubRunCommand(t, func(stdin []byte, name string, args ...string) ([]byte, error) {
		return []byte("ActiveState=active\nMainPID=4242\n"), nil
	})

	st, err := New().Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Installed || !st.Running || st.PID != 4242 {
		t.Errorf("Status = %+v, want installed, running, pid 4242", st)
	}
	// Status must never prompt for a password: an operator asking a question
	// should not be asked for their credentials to get an answer. Under the
	// override privileged() would be unprivileged anyway, so assert the
	// stronger property -- Status does not call privileged at all.
	if len(*calls) != 1 || (*calls)[0].name != "systemctl" {
		t.Errorf("Status ran %+v, want a single unprivileged systemctl", *calls)
	}
}

func TestStatusReportsInstalledButInactive(t *testing.T) {
	unitDir := isolate(t)
	seedUnit(t, unitDir)
	stubRunCommand(t, func(stdin []byte, name string, args ...string) ([]byte, error) {
		return []byte("ActiveState=inactive\nMainPID=0\n"), nil
	})

	st, err := New().Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Installed || st.Running || st.PID != 0 {
		t.Errorf("Status = %+v, want installed, not running, pid 0", st)
	}
}

func TestStatusReportsNotInstalledWithNoUnitFile(t *testing.T) {
	isolate(t)
	stubRunCommand(t, func(stdin []byte, name string, args ...string) ([]byte, error) {
		return []byte("ActiveState=inactive\nMainPID=0\n"), nil
	})

	st, err := New().Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Installed || st.Running {
		t.Errorf("Status = %+v, want not installed and not running", st)
	}
}

// TestStatusTreatsAFailedSystemctlAsInstalledButNotRunning mirrors the
// darwin implementation's handling of a failed `launchctl print`. The unit
// file is seeded first, so the assertion distinguishes "stat succeeded,
// systemctl failed" from "returned the zero Status".
func TestStatusTreatsAFailedSystemctlAsInstalledButNotRunning(t *testing.T) {
	unitDir := isolate(t)
	seedUnit(t, unitDir)
	stubRunCommand(t, func(stdin []byte, name string, args ...string) ([]byte, error) {
		return nil, errStubLinux
	})

	st, err := New().Status()
	if err != nil {
		t.Fatalf("Status reported an error for a failed systemctl: %v", err)
	}
	if !st.Installed {
		t.Error("Status lost the unit file it had already found")
	}
	if st.Running || st.PID != 0 {
		t.Errorf("Status = %+v, want not running", st)
	}
}

// TestParseShowPropertiesSplitsOnTheFirstEqualsSign covers the case the
// function's own comment calls out: a property value may itself contain an
// equals sign.
func TestParseShowPropertiesSplitsOnTheFirstEqualsSign(t *testing.T) {
	props := parseShowProperties("ActiveState=active\nEnvironment=HOME=/home/sean\nMainPID=7\n")
	if got := props["Environment"]; got != "HOME=/home/sean" {
		t.Errorf("Environment = %q, want %q", got, "HOME=/home/sean")
	}
	if got := props["ActiveState"]; got != "active" {
		t.Errorf("ActiveState = %q, want %q", got, "active")
	}
	if got := props["MainPID"]; got != "7" {
		t.Errorf("MainPID = %q, want %q", got, "7")
	}
}

// TestParseShowPropertiesIgnoresALineWithNoEqualsSign guards the parser
// against a blank trailing line and against any banner systemctl might emit.
func TestParseShowPropertiesIgnoresALineWithNoEqualsSign(t *testing.T) {
	props := parseShowProperties("\nActiveState=active\nnonsense\n\n")
	if len(props) != 1 {
		t.Errorf("parsed %d properties, want 1: %v", len(props), props)
	}
}
