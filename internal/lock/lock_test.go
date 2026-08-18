package lock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquireAndRelease(t *testing.T) {
	p := filepath.Join(t.TempDir(), "loop.lock")

	l, err := Acquire(p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	l2, err := Acquire(p)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	l2.Release()
}

func TestSecondAcquireReportsHeld(t *testing.T) {
	p := filepath.Join(t.TempDir(), "loop.lock")

	l, err := Acquire(p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()

	_, err = Acquire(p)
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("second Acquire error = %v, want ErrHeld", err)
	}
}
