//go:build linux

// This file implements Manager with systemd. CI runs on ubuntu-latest, so it
// is compiled, vetted, linted, and tested there natively -- the opposite of
// service_darwin.go, which only ever type-checks under the Makefile's
// GOOS=darwin vet line.
package service

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/home"
)

// unitDescription is the unit's human-readable name, shown by `systemctl
// status` and by `systemctl list-units`.
const unitDescription = "agent-utils webhook listener"

// restartSeconds is the delay systemd waits before restarting the listener.
// See renderUnit for why a delay is required rather than optional.
const restartSeconds = 5

// geteuid reports this process's effective user identifier. It is a
// variable, not a direct os.Geteuid call, so a test can exercise Install's
// refusal to run as root without the suite having to run as root.
//
// Declared here, not in service.go: only Install below calls it, and
// golangci-lint's unused check flagged both this variable and its test
// helper stubGeteuid as unused under GOOS=darwin, since darwin has no caller
// for either. Living beside the one call site that exists makes `make check`
// clean on a darwin development machine as well as in CI.
var geteuid = os.Geteuid

// runCommand runs a command with stdin and returns its combined output. It
// is a variable, not a direct exec.Command call at each use site, for the
// same reason service_darwin.go's launchctl is: without it, this package's
// tests would run `systemctl enable --now` against the developer's own
// machine and would block on a real sudo password prompt.
//
// The command name is resolved through PATH, as launchctl's is. That is a
// real dependency on the operator's PATH and, for the sudo'd commands, on
// sudo's own secure_path -- and it is not a new exposure: this program
// already executes git, gh, and the agent harness by name on every dispatch,
// so a PATH an attacker controls has already lost the machine.
var runCommand = func(stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd.CombinedOutput()
}

// systemdUnitDir resolves the directory the unit file lives in.
func systemdUnitDir() string {
	if dir := strings.TrimSpace(os.Getenv(SystemdUnitDirEnvVar)); dir != "" {
		return dir
	}
	return "/etc/systemd/system"
}

// privileged runs name with args as root, through sudo.
//
// Only the steps that write to /etc or talk to the system manager go through
// here. The rest of Install runs as the operator, deliberately: a program
// running under sudo for its whole life resolves HOME to /root and would
// install a service naming root's state directory rather than the operator's.
// Install refuses that case outright; see its first check.
//
// sudo is invoked without -n, so it prompts on a terminal when it needs to.
// That prompt is the point: `listener install` is an interactive command an
// operator runs by hand, and asking them for their own password to write a
// root-owned unit is the expected cost.
//
// The sudo prefix is dropped when SystemdUnitDirEnvVar is set. That variable
// steers the DESTINATION of a root-owned write and a root-owned delete, so
// honoring it while still escalating would turn an environment variable into
// "create or delete a root-owned file at a path of my choosing" -- which
// AGENT_UTILS_SYSTEMD_DIR=/etc/cron.d makes concrete. The variable exists
// only so the test suite does not touch /etc/systemd/system, and a test does
// not need root. Setting it outside a test does not redirect a privileged
// install; it makes the install fail, which is the correct outcome.
func privileged(stdin []byte, name string, args ...string) ([]byte, error) {
	if strings.TrimSpace(os.Getenv(SystemdUnitDirEnvVar)) != "" {
		return runCommand(stdin, name, args...)
	}
	return runCommand(stdin, "sudo", append([]string{name}, args...)...)
}

// firstLineOnly returns just the first line of a command's (already
// trimmed) output, for use in an error message.
//
// Every privileged() call site in Install and Uninstall except the tee one
// embeds the command's whole trimmed output, because chmod, systemctl, and
// rm each fail with one short line that already IS the diagnosis. tee is
// different: runCommand uses exec.Cmd.CombinedOutput, and tee copies its
// entire stdin to stdout regardless of whether the write to its destination
// argument succeeds. So a permission failure on the placement -- the
// riskiest command in this file, since it is the one that lands root-owned
// content on disk -- produces output whose first line names the real
// problem and whose remaining lines are the entire rendered unit file. Using
// only the first line in that one error keeps the message pointing at the
// failure instead of at the document tee was asked to write.
func firstLineOnly(output []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(output)), "\n")
	return line
}

type linuxManager struct{}

// newManager returns the systemd-backed Manager. Called from service.New.
func newManager() Manager {
	return linuxManager{}
}

func (linuxManager) ServiceFilePath() (string, error) {
	return filepath.Join(systemdUnitDir(), UnitName), nil
}

// Install renders the unit, places it under /etc/systemd/system as root,
// and enables it.
//
// The path this method installs comes from resolveSelf, not from binary; see
// resolveSelf's comment in service.go for why a caller-supplied path is not
// trusted as the SOURCE of the installed path. The reason binds harder here
// than on darwin: this unit is started by root at every boot.
func (m linuxManager) Install(binary string, args []string) error {
	// Refused first, before anything reads a path.
	//
	// The test is the effective user identifier alone, NOT euid 0 together
	// with a set SUDO_USER. `su -`, a root login shell, `doas`, and
	// `sudo env -u SUDO_USER agent-utils listener install` all arrive here
	// with euid 0 and no SUDO_USER, and every one of them would otherwise
	// produce a unit with User=root, Group=root, HOME=/root and
	// Restart=always -- the program that dispatches coding agents with
	// permission prompts disabled, running as root, permanently, from the
	// most ordinary operator mistake in this whole command.
	//
	// There is no case in which running the whole program as root is
	// correct: it calls sudo itself for exactly the three steps that need
	// it, and every directory it resolves must be the operator's.
	if geteuid() == 0 {
		return errors.New(
			"run `agent-utils listener install` as yourself, not as root: " +
				"it calls sudo itself for the steps that need root, and as root it would " +
				"install a service that runs as root and reads root's home directory")
	}

	self, err := resolveSelf()
	if err != nil {
		return err
	}
	if binary != "" {
		want, evalErr := filepath.EvalSymlinks(binary)
		if evalErr != nil || want != self {
			return fmt.Errorf(
				"refusing to install %s: this program can only install the binary it is running as (%s)",
				binary, self)
		}
	}

	// home.EnsureDir, not home.Dir: WorkingDirectory must exist by the time
	// systemd starts the service, or the start fails. With Restart=always
	// that is a restart loop whose only explanation is in the journal, which
	// an operator has no reason to read yet. EnsureDir also honors
	// AGENT_UTILS_HOME, so the unit points wherever the operator's own
	// commands point.
	homeDir, err := home.EnsureDir()
	if err != nil {
		return fmt.Errorf("locate agent-utils home directory: %w", err)
	}

	acct, err := user.Current()
	if err != nil {
		return fmt.Errorf("locate the current user: %w", err)
	}
	// The numeric gid is the fallback, not an error. A gid with no
	// /etc/group entry is ordinary in a minimal container, and systemd's
	// Group= accepts a number, so there is nothing here worth failing an
	// install over.
	group := acct.Gid
	if grp, lookupErr := user.LookupGroupId(acct.Gid); lookupErr == nil {
		group = grp.Name
	} else {
		slog.Warn("no group name for this gid, using the number",
			"gid", acct.Gid, "err", lookupErr)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locate home directory: %w", err)
	}

	// HOME is set explicitly rather than relied on. systemd does set HOME
	// for a User= unit, but this program resolves ~/.agent-utils from HOME
	// on every path that touches state, and a unit that states it is a unit
	// a reader can verify without knowing which systemd version is running.
	env := []string{"HOME=" + userHome}
	// AGENT_UTILS_HOME is carried in only when the operator has set it.
	// Without this, a machine with a moved home directory would install a
	// service reading a directory its operator never looks at.
	if v := strings.TrimSpace(os.Getenv(home.EnvVar)); v != "" {
		env = append(env, home.EnvVar+"="+v)
	}

	doc, err := renderUnit(systemdUnit{
		Description:      unitDescription,
		ExecStart:        append([]string{self}, args...),
		User:             acct.Username,
		Group:            group,
		WorkingDirectory: homeDir,
		Environment:      env,
		RestartSec:       restartSeconds,
	})
	if err != nil {
		return fmt.Errorf("render the systemd unit: %w", err)
	}

	path, err := m.ServiceFilePath()
	if err != nil {
		return err
	}

	slog.Info("installing systemd unit", "unit", UnitName, "path", path, "binary", self)

	// The rendered unit goes from memory straight to root's stdin. It is
	// never staged in a file first.
	//
	// Staging would open a window: between this process closing the staged
	// file and root reading it, anything running as the operator -- which is
	// precisely this program's threat model, since it dispatches agents with
	// permission prompts disabled on untrusted text -- could rewrite those
	// bytes or replace the path with a symlink. Root would then place
	// content that renderUnit never produced, and every guarantee renderUnit
	// makes would be bypassed without renderUnit being wrong about anything.
	//
	// There is no shell here to worry about, either: runCommand is
	// exec.Command with an explicit argv, so `tee` receives the destination
	// as one argument with no quoting, redirect, or word splitting anywhere.
	if out, err := privileged(doc, "tee", path); err != nil {
		return fmt.Errorf("write %s: %w: %s", path, err, firstLineOnly(out))
	}
	// tee creates the file with root's umask applied to 0666, so the mode is
	// normalized rather than assumed. 0644 because systemd only reads it,
	// and it carries no secret by design. Ownership needs no command: tee
	// ran as root, so the file is already root's.
	if out, err := privileged(nil, "chmod", "0644", path); err != nil {
		return fmt.Errorf("chmod %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	if out, err := privileged(nil, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// enable --now does both halves: enable writes the multi-user.target
	// link that starts it at boot, --now starts it in this boot as well.
	if out, err := privileged(nil, "systemctl", "enable", "--now", UnitName); err != nil {
		return fmt.Errorf("systemctl enable --now %s: %w: %s", UnitName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Uninstall stops and disables the unit, then removes it.
func (m linuxManager) Uninstall() error {
	path, err := m.ServiceFilePath()
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			// Nothing is registered, so there is nothing to remove.
			// Returning here rather than running the commands anyway is what
			// keeps `listener uninstall` from asking for a sudo password on
			// a machine that never had the service installed.
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, statErr)
	}

	// A non-zero exit here means the unit is already disabled, or was never
	// loaded. Uninstall must be idempotent -- an operator may have disabled
	// it by hand -- so this is logged and the removal continues. The removal
	// itself is NOT tolerant: a unit file still on disk is still installed.
	if out, err := privileged(nil, "systemctl", "disable", "--now", UnitName); err != nil {
		slog.Warn("systemctl disable reported non-zero, continuing",
			"unit", UnitName, "output", strings.TrimSpace(string(out)), "err", err)
	}
	if out, err := privileged(nil, "rm", "-f", path); err != nil {
		return fmt.Errorf("remove %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	if out, err := privileged(nil, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Status reports whether the unit is on disk and, via `systemctl show`,
// whether systemd currently runs it.
func (m linuxManager) Status() (Status, error) {
	path, err := m.ServiceFilePath()
	if err != nil {
		return Status{}, err
	}
	installed := true
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			installed = false
		} else {
			return Status{}, fmt.Errorf("stat %s: %w", path, statErr)
		}
	}

	// runCommand, not privileged: `systemctl show` reads, and an operator
	// asking what the service is doing must not be asked for a password to
	// find out. --property twice, rather than `systemctl status`, because
	// show prints one Property=value per line and is the only form of this
	// command with a documented machine-readable output.
	out, err := runCommand(nil, "systemctl", "show", UnitName,
		"--property=ActiveState", "--property=MainPID")
	if err != nil {
		// No systemd, or a systemctl this process cannot run. Either way
		// there is no live pid to report, and a status query is not the
		// place to fail. service_darwin.go treats a failed `launchctl
		// print` the same way. Note this keeps `installed`, which was
		// answered by a stat that already succeeded.
		return Status{Installed: installed}, nil
	}
	props := parseShowProperties(string(out))
	pid, convErr := strconv.Atoi(strings.TrimSpace(props["MainPID"]))
	if convErr != nil || pid < 0 {
		pid = 0
	}
	return Status{
		Installed: installed,
		Running:   props["ActiveState"] == "active",
		PID:       pid,
	}, nil
}

// parseShowProperties reads `systemctl show --property=...` output into a
// map. Each line is Property=value, and a value may itself contain an equals
// sign -- Environment= is the one this program would notice -- so the split
// is on the FIRST one only. A line with no equals sign is skipped.
func parseShowProperties(output string) map[string]string {
	props := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		props[key] = value
	}
	return props
}
