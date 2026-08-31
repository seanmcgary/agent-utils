package main

import (
	"context"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// runLogsSession parses argv against the logs command's own flags and
// arguments and returns what logsSession makes of them. It parses the REAL
// command so the argument declaration is under test too: `logs -f <session>`
// only works if the bare value is declared as an argument rather than left to
// be swallowed as an unknown one.
func runLogsSession(t *testing.T, argv ...string) (string, error) {
	t.Helper()
	var got string
	var gotErr error
	cmd := logsCommand()
	cmd.Action = func(_ context.Context, c *cli.Command) error {
		got, gotErr = logsSession(c)
		return nil
	}
	root := &cli.Command{Name: "agent-utils", Commands: []*cli.Command{cmd}}
	if err := root.Run(context.Background(), append([]string{"agent-utils"}, argv...)); err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	return got, gotErr
}

// The identifier an operator copies out of `sessions list` is what they type
// after -f, with no flag in front of it.
func TestLogsSessionAcceptsABareArgument(t *testing.T) {
	got, err := runLogsSession(t, "logs", "-f", "sess-a")
	if err != nil {
		t.Fatalf("logsSession = %v", err)
	}
	if got != "sess-a" {
		t.Errorf("session = %q, want sess-a from the bare argument", got)
	}
}

// The flag stays: both session tables print it, and scripts already use it.
func TestLogsSessionStillAcceptsTheFlag(t *testing.T) {
	got, err := runLogsSession(t, "logs", "--session", "sess-a")
	if err != nil {
		t.Fatalf("logsSession = %v", err)
	}
	if got != "sess-a" {
		t.Errorf("session = %q, want sess-a from the flag", got)
	}
}

// Two spellings that disagree have no right answer, so the operator is asked
// rather than served whichever one a precedence rule happened to pick.
func TestLogsSessionRefusesTwoDifferentSessions(t *testing.T) {
	_, err := runLogsSession(t, "logs", "--session", "sess-a", "sess-b")
	if err == nil || !strings.Contains(err.Error(), "sess-b") {
		t.Fatalf("logsSession = %v, want an error naming the second value", err)
	}

	// The same value twice is not a conflict.
	got, err := runLogsSession(t, "logs", "--session", "sess-a", "sess-a")
	if err != nil || got != "sess-a" {
		t.Fatalf("logsSession = %q, %v; want sess-a and no error", got, err)
	}
}

// No session at all is the dispatch- or issue-selected case, which the rest of
// the command handles.
func TestLogsSessionIsEmptyWhenNeitherIsGiven(t *testing.T) {
	got, err := runLogsSession(t, "logs", "--issue", "42")
	if err != nil || got != "" {
		t.Fatalf("logsSession = %q, %v; want empty and no error", got, err)
	}
}
