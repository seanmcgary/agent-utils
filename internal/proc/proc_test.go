package proc

import (
	"os"
	"os/exec"
	"testing"
)

func TestIsAliveFalseForDeadProcess(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if IsAlive(cmd.Process.Pid, 1) {
		t.Error("IsAlive = true for an exited process")
	}
}

func TestIsAliveRejectsPrefixCollision(t *testing.T) {
	// A runner for dispatch 70 must not make dispatch 7 look alive.
	line := "/usr/local/bin/agent-utils internal run-agent --dispatch 70 --config /x.yaml"
	if matchesDispatch(line, 7) {
		t.Error("substring collision: dispatch 7 matched a dispatch 70 runner")
	}
	if !matchesDispatch(line, 70) {
		t.Error("dispatch 70 must match its own runner")
	}
}

func TestIsAliveFalseForUnrelatedProcess(t *testing.T) {
	// This test process is alive, but its arguments do not contain the
	// dispatch marker, so it must not be mistaken for a dispatch runner.
	if IsAlive(os.Getpid(), 987654) {
		t.Error("IsAlive = true for a live but unrelated process")
	}
}

func TestIsAliveFalseForImpossiblePID(t *testing.T) {
	if IsAlive(-1, 1) {
		t.Error("IsAlive = true for pid -1")
	}
	if IsAlive(0, 1) {
		t.Error("IsAlive = true for pid 0")
	}
}

func TestCommandLineReturnsOwnName(t *testing.T) {
	out, err := CommandLine(os.Getpid())
	if err != nil {
		t.Fatalf("CommandLine: %v", err)
	}
	if out == "" {
		t.Error("CommandLine returned an empty string for this process")
	}
}
