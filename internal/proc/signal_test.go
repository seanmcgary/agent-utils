package proc

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Not a real test: the fixture. The test binary re-executes itself, so the
// child is a Go process whose argv this package controls exactly. It returns
// at once unless the parent set the marker, so a normal run skips it.
func TestSignalHelperProcess(t *testing.T) {
	if os.Getenv("AGENT_UTILS_SIGNAL_HELPER") != "1" {
		return
	}
	time.Sleep(60 * time.Second)
}

// startFakeRunner starts a long-lived helper process carrying
// "--dispatch <dispatchID>" on its command line, and returns it alongside a
// waitExit function that reports whether it exited within a deadline.
//
// Only one goroutine ever calls cmd.Wait(): a second concurrent Wait on one
// os.Process is a race, so waitExit and t.Cleanup share a single sync.Once
// around the one call, however many times either side asks for it.
func startFakeRunner(t *testing.T, dispatchID string) (cmd *exec.Cmd, waitExit func(time.Duration) bool) {
	t.Helper()
	cmd = exec.Command(os.Args[0],
		"-test.run=^TestSignalHelperProcess$", "--", DispatchFlag, dispatchID)
	cmd.Env = append(os.Environ(), "AGENT_UTILS_SIGNAL_HELPER=1")
	// Setpgid makes the fake runner the leader of its own process group, the
	// same shape the real agent child has (runner.go:154) -- SignalGroup
	// signals the GROUP led by a pid, which is only meaningful if that pid
	// leads one.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake runner: %v", err)
	}

	done := make(chan struct{})
	var once sync.Once
	reap := func() {
		once.Do(func() {
			_ = cmd.Wait()
			close(done)
		})
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		reap()
	})
	waitVisible(t, cmd.Process.Pid) // poll CommandLine until ps sees it

	waitExit = func(timeout time.Duration) bool {
		go reap()
		select {
		case <-done:
			return true
		case <-time.After(timeout):
			return false
		}
	}
	return cmd, waitExit
}

// waitVisible polls CommandLine until ps reports the process, so a test does
// not race the fork/exec of the fake runner it just started.
func waitVisible(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cmdline, err := CommandLine(pid); err == nil && cmdline != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d never became visible to ps", pid)
}

func TestSignalRefusesNonPositivePids(t *testing.T) {
	for _, pid := range []int{0, -1, -1000} {
		if err := Signal(pid, 7, syscall.SIGTERM); err == nil {
			t.Errorf("Signal(%d, ...) = nil error, want an error", pid)
		}
	}
}

func TestSignalGroupRefusesNonPositivePidsAndPidOne(t *testing.T) {
	for _, pid := range []int{0, -1, -1000, 1} {
		if err := SignalGroup(pid, syscall.SIGTERM); err == nil {
			t.Errorf("SignalGroup(%d, ...) = nil error, want an error", pid)
		}
	}
}

func TestSignalOnALiveNonRunnerRefusesAndLeavesItAlive(t *testing.T) {
	cmd, _ := startFakeRunner(t, "7")
	pid := cmd.Process.Pid

	// The process IS "--dispatch 7"; ask about a different dispatch.
	err := Signal(pid, 70, syscall.SIGTERM)
	if !errors.Is(err, ErrNotRunner) {
		t.Fatalf("Signal for the wrong dispatch = %v, want ErrNotRunner", err)
	}
	// Nothing has signalled the process, so a plain liveness probe (no Wait
	// involved) is sufficient here -- it was never asked to exit.
	if syscall.Kill(pid, 0) != nil {
		t.Fatalf("Signal for the wrong dispatch killed the process")
	}
}

func TestSignalKillsTheFakeRunner(t *testing.T) {
	cmd, waitExit := startFakeRunner(t, "7")
	pid := cmd.Process.Pid

	if err := Signal(pid, 7, syscall.SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if !waitExit(5 * time.Second) {
		t.Fatalf("pid %d still alive after SIGTERM", pid)
	}
}

func TestSignalGroupKillsTheProcessGroup(t *testing.T) {
	cmd, waitExit := startFakeRunner(t, "7")
	pid := cmd.Process.Pid

	if err := SignalGroup(pid, syscall.SIGTERM); err != nil {
		t.Fatalf("SignalGroup: %v", err)
	}
	if !waitExit(5 * time.Second) {
		t.Fatalf("pid %d still alive after SignalGroup", pid)
	}
}

func TestVerifyRunner(t *testing.T) {
	cmd, _ := startFakeRunner(t, "7")
	pid := cmd.Process.Pid

	if err := VerifyRunner(pid, 7); err != nil {
		t.Fatalf("VerifyRunner(pid, 7) = %v, want nil", err)
	}
	if err := VerifyRunner(pid, 70); !errors.Is(err, ErrNotRunner) {
		t.Fatalf("VerifyRunner(pid, 70) = %v, want ErrNotRunner (whole-token match)", err)
	}
}

func TestVerifyRunnerFailsClosedOnAPsError(t *testing.T) {
	// A pid this improbably high is not a live process on any test machine,
	// so CommandLine fails; VerifyRunner must refuse rather than assume the
	// process is ours (the opposite bias from IsAlive, which fails open).
	const implausiblePid = 1 << 30
	if err := VerifyRunner(implausiblePid, 7); !errors.Is(err, ErrNotRunner) {
		t.Fatalf("VerifyRunner on an implausible pid = %v, want ErrNotRunner", err)
	}
}

// A sanity check that the dispatch id round-trips through the fixture as a
// string, matching how the real runner's argv looks.
func TestFakeRunnerCarriesTheDispatchFlag(t *testing.T) {
	cmd, _ := startFakeRunner(t, strconv.Itoa(42))
	pid := cmd.Process.Pid
	if err := VerifyRunner(pid, 42); err != nil {
		t.Fatalf("VerifyRunner: %v", err)
	}
}
