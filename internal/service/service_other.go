//go:build !darwin && !linux

// This file backs Manager on every platform with no service-manager
// implementation. Registration is launchd on darwin and systemd on linux; on
// every other platform `listener run` in the foreground is the only supported
// mode, and this stub says so instead of pretending to support a service it
// cannot register.
//
// Nothing compiles this file except the Makefile's `GOOS=freebsd go vet`
// line, and nothing runs its test. That is deliberate and is the honest
// state: the release targets build linux and darwin only.
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
