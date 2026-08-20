//go:build !darwin

// This file backs Manager everywhere except darwin. --daemon integration
// with an OS service manager is launchd-only for now (systemd is meant to
// follow); on every other platform the foreground `listener start` command
// is the only supported mode, and this stub says so instead of pretending
// to support a service it cannot register.
package service

import "errors"

// errUnsupported is returned by every method: --daemon needs an OS service
// manager, and this platform does not have one wired up yet.
var errUnsupported = errors.New("service management (--daemon) is supported on macOS only; run `listener start` in the foreground instead")

type otherManager struct{}

// newManager returns the unsupported stub. Called from service.New.
func newManager() Manager {
	return otherManager{}
}

func (otherManager) Install(string, []string) error   { return errUnsupported }
func (otherManager) Uninstall() error                 { return errUnsupported }
func (otherManager) Status() (Status, error)          { return Status{}, errUnsupported }
func (otherManager) ServiceFilePath() (string, error) { return "", errUnsupported }
