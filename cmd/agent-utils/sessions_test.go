package main

import (
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/loopcmd"
)

// Both guards below -- Selector.Validate and the destructive --all gate --
// must fire before either *Run function ever opens the database. Pointing
// AGENT_UTILS_HOME at a directory that does not exist proves it: if either
// guard let control through to loopcmd.Kill/Resume, the resulting registry or
// state-database error would NOT contain the text these tests look for.

// TestSessionsKillAllWithoutYesErrorsNamingYes covers: "--all without --yes
// and without a tty errors naming --yes."
func TestSessionsKillAllWithoutYesErrorsNamingYes(t *testing.T) {
	t.Setenv("AGENT_UTILS_HOME", "/nonexistent/agent-utils-home")
	err := sessionsKillRun(killArgs{Selector: loopcmd.Selector{All: true}})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("sessionsKillRun(--all, no --yes, no Confirm) = %v, want an error naming --yes", err)
	}
}

func TestSessionsResumeAllWithoutYesErrorsNamingYes(t *testing.T) {
	t.Setenv("AGENT_UTILS_HOME", "/nonexistent/agent-utils-home")
	err := sessionsResumeRun(killArgs{Selector: loopcmd.Selector{All: true}})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("sessionsResumeRun(--all, no --yes, no Confirm) = %v, want an error naming --yes", err)
	}
}

// TestSessionsKillBadSelectorErrorsBeforeAnyRead covers: "a bad selector
// errors" -- and does so before Kill ever resolves a target, which the
// unreachable AGENT_UTILS_HOME proves.
func TestSessionsKillBadSelectorErrorsBeforeAnyRead(t *testing.T) {
	t.Setenv("AGENT_UTILS_HOME", "/nonexistent/agent-utils-home")
	err := sessionsKillRun(killArgs{Selector: loopcmd.Selector{}})
	if err == nil {
		t.Fatal("sessionsKillRun with no selector set = nil, want an error")
	}
}

func TestSessionsResumeBadSelectorErrorsBeforeAnyRead(t *testing.T) {
	t.Setenv("AGENT_UTILS_HOME", "/nonexistent/agent-utils-home")
	// Session and Issue together are mutually exclusive: Selector.Validate
	// must reject this before resolve() ever runs.
	err := sessionsResumeRun(killArgs{Selector: loopcmd.Selector{Session: "abc", Issue: 7}})
	if err == nil {
		t.Fatal("sessionsResumeRun with two selectors set = nil, want an error")
	}
}

// TestSessionsKillInteractiveDeclineActsOnNothing covers: "an interactive
// decline returns nil and acts on nothing." A declined Confirm must return
// before loopcmd.Kill ever resolves a target -- again proven by the
// unreachable AGENT_UTILS_HOME: if Kill ran, resolving --all would try to
// read the registry and the canonical database under that path and fail with
// an unrelated error, not nil.
func TestSessionsKillInteractiveDeclineActsOnNothing(t *testing.T) {
	t.Setenv("AGENT_UTILS_HOME", "/nonexistent/agent-utils-home")
	args := killArgs{
		Selector: loopcmd.Selector{All: true},
		Timeout:  5 * time.Second,
		Confirm:  func(string) (bool, error) { return false, nil },
	}
	if err := sessionsKillRun(args); err != nil {
		t.Fatalf("sessionsKillRun with a declined confirmation = %v, want nil", err)
	}
}

func TestSessionsResumeInteractiveDeclineActsOnNothing(t *testing.T) {
	t.Setenv("AGENT_UTILS_HOME", "/nonexistent/agent-utils-home")
	args := killArgs{
		Selector: loopcmd.Selector{All: true},
		Confirm:  func(string) (bool, error) { return false, nil },
	}
	if err := sessionsResumeRun(args); err != nil {
		t.Fatalf("sessionsResumeRun with a declined confirmation = %v, want nil", err)
	}
}

// TestSessionsKillAllWithYesSkipsConfirm covers the non-destructive path of
// the --all gate: --yes proceeds with no Confirm function at all, so control
// reaches loopcmd.Kill, which then fails resolving state under the
// unreachable AGENT_UTILS_HOME -- proving --yes actually let it through,
// rather than the gate silently swallowing it.
func TestSessionsKillAllWithYesSkipsConfirm(t *testing.T) {
	t.Setenv("AGENT_UTILS_HOME", "/nonexistent/agent-utils-home")
	err := sessionsKillRun(killArgs{Selector: loopcmd.Selector{All: true}, Yes: true})
	if err == nil {
		t.Fatal("sessionsKillRun(--all --yes) under an unreachable home = nil, want an error from opening state")
	}
	if strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v, must not be the --yes gate error once --yes is set", err)
	}
}
