//go:build !darwin

package service

import (
	"strings"
	"testing"
)

// TestOtherManagerReportsUnsupported pins the fail-closed behavior of every
// Manager method on a non-darwin platform: --daemon is launchd-only for now,
// and each method must say so with a macOS-specific error rather than
// silently doing nothing or panicking. This is the test that keeps that
// contract honest now that CI (ubuntu-latest) actually compiles this file.
func TestOtherManagerReportsUnsupported(t *testing.T) {
	m := New()

	if err := m.Install("agent-utils", []string{"listener", "start"}); err == nil {
		t.Error("Install did not report unsupported")
	} else if !strings.Contains(err.Error(), "macOS") {
		t.Errorf("Install error %q does not name macOS", err)
	}

	if err := m.Uninstall(); err == nil {
		t.Error("Uninstall did not report unsupported")
	} else if !strings.Contains(err.Error(), "macOS") {
		t.Errorf("Uninstall error %q does not name macOS", err)
	}

	if _, err := m.Status(); err == nil {
		t.Error("Status did not report unsupported")
	} else if !strings.Contains(err.Error(), "macOS") {
		t.Errorf("Status error %q does not name macOS", err)
	}

	if _, err := m.ServiceFilePath(); err == nil {
		t.Error("ServiceFilePath did not report unsupported")
	} else if !strings.Contains(err.Error(), "macOS") {
		t.Errorf("ServiceFilePath error %q does not name macOS", err)
	}
}
