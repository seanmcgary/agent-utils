// Package service manages this program's registration as a long-running OS
// service, so the webhook listener survives logout and machine restart
// without a user having to remember to start it back up by hand.
//
// It is named for the concept it implements, not the OS mechanism behind it
// today: launchd on darwin now, systemd meant to follow later, matching
// internal/proc, which shells out to ps but is named for what it reports
// rather than the tool itself.
package service

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
	// Install registers binary, invoked with args, to run as the service.
	// It is idempotent: installing over an existing registration replaces
	// it.
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
