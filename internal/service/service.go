// Package service manages this program's registration as a long-running OS
// service, so the webhook listener survives logout and machine restart
// without a user having to remember to start it back up by hand.
//
// It is named for the concept it implements, not the OS mechanism behind it
// today: launchd on darwin and systemd on linux, matching internal/proc,
// which shells out to ps but is named for what it reports rather than the
// tool itself.
package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Label is the launchd service identifier. It doubles as the plist's
// filename stem and as the last path component of the `launchctl
// bootout`/`print` targets, so it must stay stable across releases: changing
// it would silently orphan every previously installed agent, which would
// keep running forever under the old label with no way for a later
// Uninstall to find it.
//
// UnitName is the systemd equivalent.
const Label = "com.seanmcgary.agent-utils.listener"

// LaunchAgentsDirEnvVar overrides the LaunchAgents directory. A test needs
// this the same way internal/home needs AGENT_UTILS_HOME: without an
// override, `go test` would write a plist into the developer's real
// ~/Library/LaunchAgents, and `launchctl bootstrap` would then load it into
// their actual login session.
//
// Declared here rather than in service_darwin.go: it carries no launchd
// build-tagged behavior of its own, and a plist_test.go that references it
// (or Label) needs to type-check on every GOOS the test suite runs under,
// not just darwin. Both constants living behind //go:build darwin once
// broke `go vet ./...` on ubuntu-latest, since a test file with no build tag
// referenced a symbol that only existed on darwin.
const LaunchAgentsDirEnvVar = "AGENT_UTILS_LAUNCH_AGENTS_DIR"

// UnitName is the systemd unit's filename. It doubles as the unit
// identifier every `systemctl` subcommand in service_linux.go addresses, so
// it must stay stable across releases for the same reason Label must:
// changing it orphans every unit a previous version installed, leaving a
// service that keeps starting at every boot with no way for a later
// Uninstall to find it.
const UnitName = "agent-utils-listener.service"

// SystemdUnitDirEnvVar overrides the directory the systemd unit is written
// to. It exists for the same reason LaunchAgentsDirEnvVar does: without an
// override, `go test` would try to write into the real /etc/systemd/system
// and `systemctl enable` would register the result on the developer's own
// machine.
//
// It carries one guarantee LaunchAgentsDirEnvVar does not need. The launchd
// plist is written by an ordinary os.WriteFile running as the operator, so
// that override can never reach a path the operator could not already
// write. The systemd unit is placed by root. So service_linux.go's
// privileged() drops the sudo prefix whenever THIS variable is set: an
// environment variable must never be able to direct a root-privileged write
// or delete at a path of its choosing. Setting it outside a test therefore
// does not redirect a privileged install; it makes the install fail.
//
// Declared here rather than in service_linux.go so a test file with no
// build tag can reference it on every GOOS the suite runs under. Label and
// LaunchAgentsDirEnvVar living behind //go:build darwin once broke
// `go vet ./...` on ubuntu-latest for exactly that reason.
const SystemdUnitDirEnvVar = "AGENT_UTILS_SYSTEMD_DIR"

// WritableRefusalPhrase is the sentence fragment refuseIfWritableByOthers
// puts in its error, and the exact string cmd/agent-utils/listener.go's
// containsWritableRefusal matches on to decide whether to append
// operator-facing context to the failure.
//
// It is one exported constant rather than a literal repeated on both sides
// because the two live in different packages and nothing but this constant
// would make a reword on one side fail on the other. A pair of tests that
// each pin their own copy of the phrase both keep passing after a drift;
// a shared constant makes the drift impossible to express.
const WritableRefusalPhrase = "writable by group or other"

// Status reports whether the service is registered with the OS and, if so,
// whether it is currently running.
type Status struct {
	Installed bool
	Running   bool
	PID       int
}

// Manager installs, removes, and reports on this program as an OS-managed
// background service.
type Manager interface {
	// Install registers the running executable to run as the service,
	// invoked with args. binary is verified against the running
	// executable's own resolved path, not trusted as the source of it: each
	// platform implementation refuses to install anything other than the
	// binary it is currently running as, since a service definition with
	// RunAtLoad+KeepAlive (launchd) or Restart=always (systemd) is permanent
	// execution of whatever path it names, without the operator present.
	// Pass "" to skip the check. It is idempotent: installing over an
	// existing registration replaces it.
	Install(binary string, args []string) error
	// Uninstall removes the service registration. It is idempotent:
	// uninstalling a service that is not installed is not an error, since
	// `listener uninstall` may run against a registration a user already
	// removed by hand.
	Uninstall() error
	// Status reports whether the service is installed and, if so, running.
	Status() (Status, error)
	// ServiceFilePath returns the path of the on-disk service definition,
	// whether or not it currently exists.
	ServiceFilePath() (string, error)
}

// executablePath resolves the path of the running process's own binary. It
// is a variable, not a direct os.Executable() call at each use site, so a
// test can point Install at a path inside a scratch directory without the
// test binary itself needing to live there -- see resolveSelf for why this,
// not Install's binary argument, is the SOURCE of the path that gets
// installed.
var executablePath = os.Executable

// resolveSelf returns the absolute, symlink-resolved path to the running
// binary and refuses one that a user other than its owner could overwrite.
//
// It resolves via os.Executable() (through the executablePath variable),
// not via Install's binary argument: RunAtLoad and KeepAlive are both true
// in the plist this produces, so whatever path ends up in
// ProgramArguments[0] is permanent login-time execution. Treating an
// arbitrary caller-supplied string as the SOURCE of that path -- rather than
// merely checking it -- would let exactly the kind of injection this
// package's plist rendering guards against back in through a different
// door. The only binary this method should ever install is the one
// currently running it, and os.Executable() is the one source that actually
// answers that question rather than asserting it. Install still verifies
// its binary argument against this result and fails loudly on a mismatch,
// rather than silently ignoring what the caller asked for.
//
// The writability check matters for the same reason: this program dispatches
// agents that run with permission prompts disabled on untrusted text (see
// README, "Security") and can write anything the user can. A plist that
// pointed into a checkout's ./bin -- or any other path with a writable
// parent -- would hand a prompt-injected agent a way to overwrite the binary
// launchd runs at every login, i.e. persistence across reboots.
//
// This binds harder for a systemd system unit than for a launchd user
// agent. A launchd agent runs as the operator, so a writable binary path
// buys an attacker persistence as that operator. A systemd system unit
// is started by root at every boot, so the same writable path is both
// persistence AND a route into the operator's account from any local
// account that can write that directory.
func resolveSelf() (string, error) {
	self, err := executablePath()
	if err != nil {
		return "", fmt.Errorf("locate this executable: %w", err)
	}
	real, err := filepath.EvalSymlinks(self)
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	if err := refuseIfWritableByOthers(real); err != nil {
		return "", err
	}
	return real, nil
}

// refuseIfWritableByOthers walks real and every parent directory up to the
// filesystem root, refusing if any is group- or world-writable. A writable
// parent is as dangerous as a writable file: an attacker who cannot modify
// the binary in place can still rename a replacement over it, or remove and
// recreate the path, because that only requires write access to the
// directory entry, not the file itself.
func refuseIfWritableByOthers(real string) error {
	path := real
	for {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("stat %s: %w", path, err)
		}
		// 0o022 is the group-write and other-write bits. Masking to just
		// those and ignoring the rest of the mode (owner permissions,
		// setuid/setgid) is deliberate, not an oversight about the sticky
		// bit: on a directory, sticky (mode 1000) is exactly what stops
		// another user renaming or unlinking an entry they don't own, but
		// it only protects entries that already exist. It does nothing to
		// stop another user CREATING a new entry at a path this plist will
		// later name -- e.g. a binary that does not exist yet at install
		// time, in a writable directory that is later populated. So a
		// sticky, world-writable directory (mode 1777, like /tmp) is still
		// refused here, correctly.
		// cmd/agent-utils/listener.go's explainInstallErr matches
		// WritableRefusalPhrase, not a literal it carries its own copy of,
		// to decide whether to append operator-facing context (the
		// Intel-Mac Homebrew /usr/local case) to this error. The wording
		// lives on the constant and travels with it, so rewording the
		// phrase here cannot desync the two sides the way two independent
		// literals could.
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("refusing to install: %s is %s", path, WritableRefusalPhrase)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

// ErrUnsupported is returned by every Manager method on a platform with no
// service-manager backend. It is exported, and callers compare against it
// with errors.Is, because "this platform cannot register a service" and
// "this machine's service registration could not be read" need different
// answers: cmd/agent-utils/listener.go's `uninstall` reports the first as
// "nothing is installed" and must surface the second as the error it is.
var ErrUnsupported = errors.New(
	"service management (`listener install` / `listener uninstall`) is supported on macOS and Linux only; " +
		"run `agent-utils listener run` in the foreground instead")

// New returns the Manager for the current platform: launchd on darwin,
// systemd on linux, and an unsupported stub everywhere else. The stub exists
// so `listener run` keeps working on every platform even though `listener
// install` needs a service manager this program knows how to drive.
//
// It is a variable, not a plain function, so cmd/agent-utils's tests can
// substitute a fake Manager. Without that seam, every `listener status` and
// `listener uninstall` test in that package runs a real `launchctl print` or
// a real `systemctl show` against the developer's own machine.
var New = func() Manager {
	return newManager()
}
