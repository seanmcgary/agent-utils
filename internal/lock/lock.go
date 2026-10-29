// Package lock provides a per-loop advisory file lock. It stops two cron ticks
// from dispatching the same issue.
package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrHeld reports that another process holds the lock.
var ErrHeld = errors.New("lock is held by another process")

// Lock is an acquired advisory lock.
type Lock struct {
	f *os.File
}

// Acquire takes an exclusive non-blocking lock on path.
// It returns ErrHeld when another process already holds it.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrHeld
		}
		return nil, fmt.Errorf("flock: %w", err)
	}
	return &Lock{f: f}, nil
}

// lockPoll is how often AcquireWait retries a held lock. The token cache is
// the one caller, and its holder makes a single HTTPS round trip, so a short
// poll costs nothing and keeps a waiting gh call from noticeably stalling.
const lockPoll = 25 * time.Millisecond

// AcquireWait takes the lock at path, waiting while another process holds it,
// until ctx ends.
//
// It polls Acquire rather than calling flock without LOCK_NB, because a
// blocking flock cannot be interrupted by a context: a caller whose deadline
// passed would sit in the syscall until the holder let go.
func AcquireWait(ctx context.Context, path string) (*Lock, error) {
	for {
		l, err := Acquire(path)
		if !errors.Is(err, ErrHeld) {
			return l, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// Release unlocks and closes the lock file.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	if err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN); err != nil {
		l.f.Close()
		return fmt.Errorf("unlock: %w", err)
	}
	return l.f.Close()
}
