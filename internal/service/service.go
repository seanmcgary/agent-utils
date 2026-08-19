// Package service manages this program's registration as a long-running OS
// service, so the webhook listener survives logout and machine restart
// without a user having to remember to start it back up by hand.
//
// It is named for the concept it implements, not the OS mechanism behind it
// today: launchd on darwin now, systemd meant to follow later, matching
// internal/proc, which shells out to ps but is named for what it reports
// rather than the tool itself.
package service

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
//
// Declared here rather than in service_darwin.go: it carries no launchd
// build-tagged behavior of its own, and a plist_test.go that references it
// (or Label) needs to type-check on every GOOS the test suite runs under,
// not just darwin. Both constants living behind //go:build darwin once
// broke `go vet ./...` on ubuntu-latest, since a test file with no build tag
// referenced a symbol that only existed on darwin.
const LaunchAgentsDirEnvVar = "AGENT_UTILS_LAUNCH_AGENTS_DIR"

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
	// executable's own resolved path, not trusted as the source of it: the
	// darwin implementation refuses to install anything other than the
	// binary it is currently running as, since a service definition with
	// RunAtLoad+KeepAlive is permanent login-time execution of whatever
	// path it names. Pass "" to skip the check. It is idempotent: installing
	// over an existing registration replaces it.
	Install(binary string, args []string) error
	// Uninstall removes the service registration. It is idempotent:
	// uninstalling a service that is not installed is not an error, since
	// `listener stop` may run against a registration a user already
	// removed by hand.
	Uninstall() error
	// Status reports whether the service is installed and, if so, running.
	Status() (Status, error)
	// ServiceFilePath returns the path of the on-disk service definition,
	// whether or not it currently exists.
	ServiceFilePath() (string, error)
}

// New returns the Manager for the current platform: the launchd-backed
// implementation on darwin, and an unsupported stub everywhere else. The
// stub exists so `listener start` (no --daemon) keeps working on every
// platform even though `--daemon` is macOS-only.
func New() Manager {
	return newManager()
}
