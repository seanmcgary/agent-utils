//go:build darwin

// This file implements Manager with launchd. CI runs on ubuntu-latest, so it
// is never compiled there; `GOOS=darwin go build ./internal/service/...` and
// `GOOS=darwin go vet ./internal/service/...` are the only things that ever
// type-check it, and both are required gates for any change here.
package service

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/home"
)

// Label is the launchd service identifier. It doubles as the plist's
// filename stem and as the last path component of the `launchctl
// bootout`/`print` targets, so it must stay stable across releases: changing
// it would silently orphan every previously installed agent, which would
// keep running forever under the old label with no way for a later
// Uninstall to find it.
const Label = "com.seanmcgary.agent-utils.listener"

// LaunchAgentsDirEnvVar overrides the LaunchAgents directory. A test needs
// this the same way internal/home needs AGENT_UTILS_HOME: without an
// override, `go test` would write a plist into the developer's real
// ~/Library/LaunchAgents, and `launchctl bootstrap` would then load it into
// their actual login session.
const LaunchAgentsDirEnvVar = "AGENT_UTILS_LAUNCH_AGENTS_DIR"

// launchAgentsDir resolves the directory the plist lives in.
func launchAgentsDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(LaunchAgentsDirEnvVar)); dir != "" {
		return dir, nil
	}
	// os.UserHomeDir, not internal/home.Dir: LaunchAgents lives under the
	// OS-level user home (~/Library/...), a different directory than
	// AGENT_UTILS_HOME, which only ever names this program's own state
	// directory (~/.agent-utils by default).
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(userHome, "Library", "LaunchAgents"), nil
}

// executablePath resolves the path of the running process's own binary. It
// is a variable, not a direct os.Executable() call at each use site, so a
// test can point Install at a path inside a scratch directory without the
// test binary itself needing to live there -- see resolveSelf for why the
// caller-supplied binary argument is not used for this instead.
var executablePath = os.Executable

// launchctl runs `launchctl <args...>` and returns its combined output. It
// is a variable, not a direct exec.Command call at each use site, so a test
// can prove Install and Uninstall write and remove the right files without
// ever invoking the real launchd -- `launchctl bootstrap` would otherwise
// register a plist in the developer's actual login session every time this
// package's tests ran.
var launchctl = func(args ...string) ([]byte, error) {
	return exec.Command("launchctl", args...).CombinedOutput()
}

type darwinManager struct{}

// newManager returns the launchd-backed Manager. Called from service.New.
func newManager() Manager {
	return darwinManager{}
}

func (darwinManager) ServiceFilePath() (string, error) {
	dir, err := launchAgentsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, Label+".plist"), nil
}

// resolveSelf returns the absolute, symlink-resolved path to the running
// binary and refuses one that a user other than its owner could overwrite.
//
// It resolves via os.Executable() (through the executablePath variable)
// rather than trusting the binary argument Install receives: RunAtLoad and
// KeepAlive are both true in the plist this produces, so whatever path ends
// up in ProgramArguments[0] is permanent login-time execution. Accepting an
// arbitrary caller-supplied path there would let exactly the kind of
// injection this package's plist rendering guards against back in through a
// different door. The only binary this method should ever install is the
// one currently running it, and os.Executable() is the one source that
// actually answers that question rather than asserting it.
//
// The writability check matters for the same reason: this program dispatches
// agents that run with permission prompts disabled on untrusted text (see
// README, "Security") and can write anything the user can. A plist that
// pointed into a checkout's ./bin -- or any other path with a writable
// parent -- would hand a prompt-injected agent a way to overwrite the binary
// launchd runs at every login, i.e. persistence across reboots.
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
		// 0o022 is the group-write and other-write bits. Anything else in
		// the mode (owner permissions, the sticky bit, setuid/setgid) does
		// not let a second user modify this path.
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("refusing to install: %s is writable by group or other", path)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return nil
		}
		path = parent
	}
}

// Install writes the plist and registers it with launchd.
//
// binary is accepted to satisfy the Manager interface, but the path this
// method actually installs comes from resolveSelf, not from binary -- see
// resolveSelf's comment for why a caller-supplied path is not trusted here.
func (m darwinManager) Install(_ string, args []string) error {
	self, err := resolveSelf()
	if err != nil {
		return err
	}

	// home.Dir, not os.UserHomeDir: StandardOutPath, StandardErrorPath, and
	// WorkingDirectory must agree with wherever the daemon itself resolves
	// ~/.agent-utils/env from, including under AGENT_UTILS_HOME in a test.
	// Two different notions of "home" here would point the log files
	// somewhere other than where the running daemon actually is.
	homeDir, err := home.Dir()
	if err != nil {
		return fmt.Errorf("locate agent-utils home directory: %w", err)
	}

	p := launchdPlist{
		Label:             Label,
		ProgramArguments:  append([]string{self}, args...),
		RunAtLoad:         true,
		KeepAlive:         true,
		StandardOutPath:   filepath.Join(homeDir, "listener.stdout.log"),
		StandardErrorPath: filepath.Join(homeDir, "listener.stderr.log"),
		WorkingDirectory:  homeDir,
	}
	doc, err := renderPlist(p)
	if err != nil {
		return fmt.Errorf("render plist: %w", err)
	}

	dir, err := launchAgentsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	path, err := m.ServiceFilePath()
	if err != nil {
		return err
	}
	// 0644: launchd (running as the same user, not a privileged daemon on
	// the gui/<uid> domain) needs only to read this file. There is no
	// secret in it to protect -- the plist carries no EnvironmentVariables
	// key and no token, by design -- so world-readable costs nothing.
	if err := os.WriteFile(path, doc, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	slog.Info("installing launch agent", "label", Label, "path", path, "binary", self)

	out, err := launchctl("bootstrap", gui(), path)
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Uninstall boots the agent out of launchd, then removes the plist.
func (m darwinManager) Uninstall() error {
	// bootout returns a non-zero status when the service is not currently
	// loaded. Treat that as success, not failure: Uninstall must be
	// idempotent, since `listener stop` may run against a plist a user
	// already removed by hand, or against a machine that rebooted without
	// the agent ever having bootstrapped successfully.
	out, err := launchctl("bootout", gui()+"/"+Label)
	if err != nil {
		slog.Info("launchctl bootout reported non-zero, continuing", "label", Label, "output", strings.TrimSpace(string(out)))
	}

	path, err := m.ServiceFilePath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// Status reports whether the plist is on disk and, via `launchctl print`,
// whether launchd currently has it running.
func (m darwinManager) Status() (Status, error) {
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

	out, err := launchctl("print", gui()+"/"+Label)
	if err != nil {
		// Not loaded, or loaded but not running (launchctl print exits
		// non-zero when the service is unknown to launchd). Either way
		// there is no live pid to report.
		return Status{Installed: installed, Running: false}, nil
	}
	pid := parsePID(string(out))
	return Status{Installed: installed, Running: pid > 0, PID: pid}, nil
}

// gui returns this user's launchd GUI domain target, gui/<uid>, the form
// every launchctl subcommand in this file addresses.
func gui() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

// parsePID reads the pid out of `launchctl print`'s output. The command has
// no documented machine-readable format (no -json, unlike launchctl list),
// so this scans for the "pid = <n>" line its plain-text output carries.
func parsePID(output string) int {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, "pid = ")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}
