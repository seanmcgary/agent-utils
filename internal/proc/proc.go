// Package proc reports whether a dispatch runner process is still alive.
package proc

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// DispatchFlag is the argument the runner carries. Liveness matches on it, so
// a reused process identifier cannot be mistaken for a live runner.
const DispatchFlag = "--dispatch"

// CommandLine returns the full command line of pid.
func CommandLine(pid int) (string, error) {
	// -ww stops procps on Linux from truncating the argument list at 80 columns
	// when stdout is not a terminal. Without it the --dispatch token can fall off
	// the end and every live runner is reported dead.
	out, err := exec.Command("ps", "-ww", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", fmt.Errorf("read command line of %d: %w", pid, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// IsAlive reports whether pid is a live runner for dispatchID.
//
// Two checks run. The first asks the kernel whether the process exists. The
// second confirms the process is the expected runner, because the operating
// system reuses process identifiers and an unrelated program could hold this
// one.
func IsAlive(pid int, dispatchID int64) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	cmdline, err := CommandLine(pid)
	if err != nil {
		// The kernel already confirmed the process exists. A failed ps (EAGAIN
		// under load, for example) is not evidence of death. Fail SAFE: report
		// alive, so a transient error cannot cause a duplicate dispatch.
		return true
	}
	// Match on whole tokens. A substring match would make "--dispatch 7" match a
	// live runner for dispatch 70, which would strand dispatch 7 forever.
	return matchesDispatch(cmdline, dispatchID)
}

// matchesDispatch reports whether a command line is the runner for dispatchID.
// It compares whole tokens, never a substring.
func matchesDispatch(cmdline string, dispatchID int64) bool {
	want := strconv.FormatInt(dispatchID, 10)
	fields := strings.Fields(cmdline)
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == DispatchFlag && fields[i+1] == want {
			return true
		}
	}
	return false
}
