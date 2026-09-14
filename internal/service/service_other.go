//go:build !darwin

// This file backs Manager everywhere except darwin. --daemon integration
// with an OS service manager is launchd-only for now (systemd is meant to
// follow); on every other platform the foreground `listener start` command
// is the only supported mode, and this stub says so instead of pretending
// to support a service it cannot register.
package service

type otherManager struct{}

// newManager returns the unsupported stub. Called from service.New.
func newManager() Manager {
	return otherManager{}
}

func (otherManager) Install(string, []string) error   { return ErrUnsupported }
func (otherManager) Uninstall() error                 { return ErrUnsupported }
func (otherManager) Status() (Status, error)          { return Status{}, ErrUnsupported }
func (otherManager) ServiceFilePath() (string, error) { return "", ErrUnsupported }
