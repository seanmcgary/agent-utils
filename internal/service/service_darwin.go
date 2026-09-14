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

// Label and LaunchAgentsDirEnvVar live in service.go, not here: they carry
// no launchd-specific behavior, and plist_test.go (which has no build tag,
// so it type-checks on every platform CI runs) references both.

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

// Install writes the plist and registers it with launchd.
//
// The path this method actually installs comes from resolveSelf, not from
// binary: see resolveSelf's comment for why a caller-supplied path is not
// trusted as the SOURCE of the installed path. binary is still checked, not
// silently discarded -- a caller passing a different path is confused about
// what this method does, and failing loudly here is cheaper than silently
// installing something other than what it asked for. Pass "" to skip the
// check (e.g. a caller that only ever wants "whatever is currently running,
// no matter what").
func (m darwinManager) Install(binary string, args []string) error {
	self, err := resolveSelf()
	if err != nil {
		return err
	}
	if binary != "" {
		want, evalErr := filepath.EvalSymlinks(binary)
		if evalErr != nil || want != self {
			return fmt.Errorf("refusing to install %s: this program can only install the binary it is running as (%s)", binary, self)
		}
	}

	// home.EnsureDir, not home.Dir: WorkingDirectory below must exist by
	// the time launchd spawns the process, or the spawn fails outright. With
	// KeepAlive true that is a throttled respawn loop with no
	// StandardErrorPath yet to explain why -- the daemon would silently
	// never start. home.EnsureDir also keeps StandardOutPath,
	// StandardErrorPath, and WorkingDirectory agreeing with wherever the
	// daemon itself resolves ~/.agent-utils/env from, including under
	// AGENT_UTILS_HOME in a test; os.UserHomeDir would not honor that
	// override and would point the log files somewhere other than where the
	// running daemon actually is.
	homeDir, err := home.EnsureDir()
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

	// bootout first, tolerating failure, is what makes Manager.Install's
	// documented idempotence true on darwin. `launchctl bootstrap` against a
	// label already bootstrapped in the gui/<uid> domain does not replace
	// the registration -- it fails outright, exit 5, "Bootstrap failed: 5:
	// Input/output error". And an old agent is essentially always loaded:
	// RunAtLoad brings it back at every login, so the ordinary case for a
	// reinstall is a currently-running one, not a fresh machine. Without
	// this bootout, `listener install` run a second time -- exactly the
	// upgrade path the README documents -- would fail. bootout returning
	// non-zero here means the agent was not loaded, the normal first-install
	// case, so that failure is logged and swallowed the same way Uninstall
	// treats it below, not surfaced as an error.
	//
	// This runs after the plist write, not before: bootout only needs the
	// label, not the file, so ordering against the write doesn't affect
	// whether it can run. Putting it after keeps both launchctl calls
	// adjacent at the bottom of this method, so the bootstrap they set up
	// reads as one uninterrupted sequence rather than being split around the
	// file write in between.
	if out, err := launchctl("bootout", gui()+"/"+Label); err != nil {
		slog.Info("launchctl bootout reported non-zero, continuing", "label", Label, "output", strings.TrimSpace(string(out)))
	}

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
	// idempotent, since `listener uninstall` may run against a plist a user
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
