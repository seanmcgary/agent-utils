package main

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

// A SIGTERM to this process cancels the context runAgentContext returns.
// runAgentContext installs signal.NotifyContext, which is the wiring under
// test: a handler that is never installed fails silently, and stays invisible
// until an operator's kill orphans an agent instead of stopping it.
func TestRunAgentContextCancelsOnSIGTERM(t *testing.T) {
	ctx, cancel := runAgentContext(context.Background())
	defer cancel()

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM to self: %v", err)
	}

	select {
	case <-ctx.Done():
		// signal.NotifyContext installs its own handler, replacing the default
		// terminate action, so the test process surviving the signal at all
		// is part of what is under test here.
	case <-time.After(5 * time.Second):
		t.Fatal("context was not cancelled within 5s of SIGTERM")
	}
}
