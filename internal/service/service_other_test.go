//go:build !darwin && !linux

package service

import (
	"errors"
	"testing"
)

// TestOtherManagerReportsUnsupported pins the fail-closed behavior of every
// Manager method on a platform with no service-manager backend: each must
// return ErrUnsupported rather than silently doing nothing or panicking.
//
// Nothing in CI RUNS this test. CI is ubuntu-latest, and the Makefile's vet
// target type-checks this file under GOOS=freebsd -- vet analyzes test files,
// so a compile error here fails `make check`, but no assertion below is ever
// executed. That is the honest state of a stub for platforms this project
// ships no binary for (see the Makefile's release targets: linux and darwin
// only). Keep the assertions cheap and the file compiling.
func TestOtherManagerReportsUnsupported(t *testing.T) {
	m := New()

	if err := m.Install("agent-utils", []string{"listener", "start"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Install error = %v, want ErrUnsupported", err)
	}

	if err := m.Uninstall(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Uninstall error = %v, want ErrUnsupported", err)
	}

	if _, err := m.Status(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Status error = %v, want ErrUnsupported", err)
	}

	if _, err := m.ServiceFilePath(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("ServiceFilePath error = %v, want ErrUnsupported", err)
	}
}
