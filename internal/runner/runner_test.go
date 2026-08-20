package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// stubClaude writes a fake claude onto PATH. It prints a stream-json stream and
// exits with the given code.
func stubClaude(t *testing.T, exitCode int, body string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOF'\n" + body + "\nEOF\nexit " + strconv.Itoa(exitCode) + "\n"
	p := filepath.Join(dir, "claude")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// testProject stands in for a real project UUID. Every row the store writes is
// keyed by one.
const testProject = "11111111-1111-1111-1111-111111111111"

func newStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db.Project(testProject)
}

func TestSuperviseRecordsSuccess(t *testing.T) {
	stubClaude(t, 0, `{"type":"result","subtype":"success","session_id":"abc","total_cost_usd":0.5,"duration_ms":1200,"is_error":false,"result":"ok"}`)

	s := newStore(t)
	id, err := s.CreateDispatch(store.Dispatch{
		Loop: "planning", Repo: "o/r", Number: 1,
		Kind: store.KindStart, SessionID: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDispatch(id)

	logPath := filepath.Join(t.TempDir(), "run.jsonl")
	cfg := &config.Config{Agent: config.Agent{Model: "opus", Timeout: config.Duration(60e9)}}

	err = Supervise(context.Background(), cfg, s, d,
		Invocation{SessionID: "abc", Prompt: "go"}, t.TempDir(), logPath)
	if err != nil {
		t.Fatalf("Supervise: %v", err)
	}

	got, _ := s.GetDispatch(id)
	if got.Status != store.StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", got.Status)
	}
	if got.CostUSD != 0.5 {
		t.Errorf("CostUSD = %v, want 0.5", got.CostUSD)
	}
	if got.DurationMS != 1200 {
		t.Errorf("DurationMS = %d, want 1200", got.DurationMS)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("log file was not written: %v", err)
	}
}

func TestSuperviseRecordsFailureOnNonZeroExit(t *testing.T) {
	stubClaude(t, 1, `{"type":"result","subtype":"error","is_error":true,"api_error_status":"529"}`)

	s := newStore(t)
	id, _ := s.CreateDispatch(store.Dispatch{
		Loop: "planning", Repo: "o/r", Number: 2, Kind: store.KindStart, SessionID: "x",
	})
	d, _ := s.GetDispatch(id)

	cfg := &config.Config{Agent: config.Agent{Model: "opus", Timeout: config.Duration(60e9)}}
	_ = Supervise(context.Background(), cfg, s, d,
		Invocation{SessionID: "x", Prompt: "go"}, t.TempDir(),
		filepath.Join(t.TempDir(), "run.jsonl"))

	got, _ := s.GetDispatch(id)
	if got.Status != store.StatusFailed {
		t.Errorf("Status = %q, want failed", got.Status)
	}
	if got.APIError != "529" {
		t.Errorf("APIError = %q, want 529", got.APIError)
	}
}

func TestSuperviseRecordsFailureWhenStreamHasNoResult(t *testing.T) {
	stubClaude(t, 0, `{"type":"assistant"}`)

	s := newStore(t)
	id, _ := s.CreateDispatch(store.Dispatch{
		Loop: "planning", Repo: "o/r", Number: 3, Kind: store.KindStart, SessionID: "y",
	})
	d, _ := s.GetDispatch(id)

	cfg := &config.Config{Agent: config.Agent{Model: "opus", Timeout: config.Duration(60e9)}}
	_ = Supervise(context.Background(), cfg, s, d,
		Invocation{SessionID: "y", Prompt: "go"}, t.TempDir(),
		filepath.Join(t.TempDir(), "run.jsonl"))

	got, _ := s.GetDispatch(id)
	if got.Status != store.StatusFailed {
		t.Errorf("Status = %q, want failed when no result line is present", got.Status)
	}
}

// The detached runner is the second MarkNeedsRetry call site, and the one with
// neither a configuration nor a clock in scope before this change. A failure
// recorded here must carry the configured wait, indexed by the retry count the
// row already holds, or every agent that dies under the runner retries with no
// backoff at all.
func TestFinishStampsTheRetryDeadlineFromTheConfiguration(t *testing.T) {
	s := newStore(t)
	id, _ := s.CreateDispatch(store.Dispatch{
		Loop: "planning", Repo: "o/r", Number: 9, Kind: store.KindStart, SessionID: "z",
	})
	d, _ := s.GetDispatch(id)
	if err := s.PutIssueState(store.IssueState{
		Loop: "planning", Repo: "o/r", Number: 9, RetryCount: 1,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Retry: config.Retry{
		Max: 3,
		Backoff: []config.Duration{
			0,
			config.Duration(15 * time.Minute),
			config.Duration(30 * time.Minute),
		},
	}}
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	// Finish returns the failure it recorded; the record is what is under test.
	_ = Finish(cfg, s, d, store.DispatchResult{
		Status: store.StatusFailed, ExitCode: 1, APIError: "boom",
	}, now)

	st, err := s.IssueState("planning", "o/r", 9)
	if err != nil {
		t.Fatal(err)
	}
	if !st.NeedsRetry {
		t.Error("NeedsRetry = false, want true")
	}
	want := now.Add(15 * time.Minute)
	if !st.RetryAfter.Equal(want) {
		t.Errorf("RetryAfter = %v, want %v (Backoff[1], indexed by retry_count 1)",
			st.RetryAfter, want)
	}
}

// The agent runs with permission prompts disabled on third-party issue text, so
// it must not inherit the repository-write credential. This is asserted rather
// than assumed because both hops (the detached runner and the agent) had to be
// filtered separately: a same-user process can read its parent's environment.
func TestAgentEnvExcludesCredentials(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_secret_value")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "should-not-appear")
	t.Setenv("HOME", "/home/tester")

	env := agentEnv()

	for _, kv := range env {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			t.Error("GITHUB_TOKEN must not reach the agent")
		}
		if strings.HasPrefix(kv, "AWS_SECRET_ACCESS_KEY=") {
			t.Error("the environment must be an allowlist, not a denylist")
		}
	}
	var sawHome bool
	for _, kv := range env {
		if kv == "HOME=/home/tester" {
			sawHome = true
		}
	}
	if !sawHome {
		t.Error("HOME must be preserved; the agent needs it to run git and gh")
	}
}

// Spawn must return the child's REAL process identifier.
//
// os.Process.Release invalidates the handle and sets Pid to -1, so reading
// cmd.Process.Pid after the Release call returned -1 on every successful
// spawn. The tick wrote that -1 into the dispatch row, and a later tick read
// it as a dead runner and retried an issue whose agent was still working --
// a second agent in a worktree that already held one.
func TestSpawnReturnsTheRealPidNotTheReleasedHandle(t *testing.T) {
	// /bin/echo takes the runner's arguments, prints them and exits. Nothing
	// about the child matters here except that the kernel gave it a pid.
	pid, err := Spawn("/bin/echo", 7, testProject, "/tmp/loop.yaml",
		filepath.Join(t.TempDir(), "runner-7.log"))
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("Spawn returned pid %d; a spawned process always has a positive pid", pid)
	}
}
