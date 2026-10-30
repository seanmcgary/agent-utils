package lock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
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

func TestAcquireWaitBlocksUntilReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	held, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		l, err := AcquireWait(context.Background(), path)
		if err == nil {
			err = l.Release()
		}
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("AcquireWait returned while the lock was held")
	case <-time.After(150 * time.Millisecond):
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AcquireWait did not return after release")
	}
}

func TestAcquireWaitHonoursContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	held, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := AcquireWait(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}
