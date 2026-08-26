package proc

import (
	"errors"
	"fmt"
	"syscall"
)

// ErrNotRunner reports that a pid could not be confirmed as the runner for a
// dispatch, so no signal may be sent to it.
var ErrNotRunner = errors.New("not this dispatch's runner")

// VerifyRunner reports an error unless pid is CONFIRMED to be the runner for
// dispatchID.
//
// It exists rather than a reuse of IsAlive because the two want OPPOSITE
// biases. IsAlive fails SAFE by reporting alive when ps errors (proc.go:42):
// for liveness that is right, since a transient error must not cause a
// duplicate dispatch. For signalling it is inverted -- a ps that fails means
// the process was never confirmed to be ours, and the safe answer is to
// refuse, not to send a signal to whatever pid happens to be there.
func VerifyRunner(pid int, dispatchID int64) error {
	if pid <= 0 {
		return fmt.Errorf("%w: pid %d is not positive", ErrNotRunner, pid)
	}
	cmdline, err := CommandLine(pid)
	if err != nil {
		// Fail CLOSED, the opposite of IsAlive: an unreadable command line is
		// not evidence the process is ours.
		return fmt.Errorf("%w: %v", ErrNotRunner, err)
	}
	if !matchesDispatch(cmdline, dispatchID) {
		return ErrNotRunner
	}
	return nil
}

// Signal sends sig to pid after VerifyRunner passes. The caller must not
// signal a pid it has not confirmed belongs to this dispatch: process
// identifiers are reused, and an unconfirmed pid could be an unrelated
// process the operating system has since handed to someone else.
func Signal(pid int, dispatchID int64, sig syscall.Signal) error {
	if err := VerifyRunner(pid, dispatchID); err != nil {
		return err
	}
	if err := syscall.Kill(pid, sig); err != nil {
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	return nil
}

// SignalGroup sends sig to the process group led by pid.
//
// It takes a POSITIVE pid and negates it internally, so no call site is one
// typo from -1. It rejects any identifier of 1 or less, not merely 0 or less:
// it negates its argument, and kill(2) reads -1 as "every process this user
// owns" -- a pid of 1 is positive, is a live identifier in any container, and
// would produce exactly that broadcast.
//
// It carries no runner check: the agent process has no --dispatch argument
// for VerifyRunner to match against, so the CALLER must establish the pid is
// current before calling this (the kill command does, via the runner's
// verified agent_pid).
func SignalGroup(pid int, sig syscall.Signal) error {
	if pid <= 1 {
		return fmt.Errorf("refusing to signal process group for pid %d", pid)
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		return fmt.Errorf("signal process group %d: %w", pid, err)
	}
	return nil
}
