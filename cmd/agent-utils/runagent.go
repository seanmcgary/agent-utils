package main

import (
	"context"
	"os/signal"
	"syscall"
)

// runAgentContext wraps ctx so that SIGINT or SIGTERM cancels it.
//
// It is a NAMED function, not two lines inlined into the run-agent action, so
// the wiring can be tested -- a handler that is never installed fails
// silently and stays invisible until an operator's `kill` orphans the agent
// instead of stopping it. `sessions kill` sends SIGTERM to the runner; this is
// what lets Supervise notice, cancel the agent's context, and let the
// existing SIGTERM-then-SIGKILL sweep (runner.go:157, :215) reach the agent's
// process group before this process exits.
func runAgentContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
}
