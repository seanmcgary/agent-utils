package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
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

// stubPi writes a fake pi onto PATH. It records its own argv to a file (so the
// test can prove the binary and header were used), then prints a pi JSON stream
// and exits with the given code.
func stubPi(t *testing.T, exitCode int, body string) string {
	t.Helper()
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "pi-argv.log")
	script := "#!/bin/sh\n"
	script += "echo \"$@\" > " + argvLog + "\n"
	script += "cat <<'EOF'\n" + body + "\nEOF\nexit " + strconv.Itoa(exitCode) + "\n"
	p := filepath.Join(dir, "pi")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvLog
}

func TestSupervisePiUsesPiExecutable(t *testing.T) {
	piStream := `{"type":"session","id":"abc"}` + "\n" +
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stopReason":"stop","usage":{"cost":{"total":0.1}}}}` + "\n"
	argvLog := stubPi(t, 0, piStream)

	s := newStore(t)
	id, err := s.CreateDispatch(store.Dispatch{
		Loop: "planning", Repo: "o/r", Number: 4,
		Kind: store.KindStart, SessionID: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDispatch(id)

	cfg := &config.Config{Agent: config.Agent{
		Harness: config.HarnessPi, Model: "anthropic/claude-x",
		Timeout: config.Duration(60e9),
	}}
	_ = Supervise(context.Background(), cfg, s, d,
		Invocation{SessionID: "abc", Prompt: "go"}, t.TempDir(),
		filepath.Join(t.TempDir(), "run.jsonl"))

	got, _ := s.GetDispatch(id)
	if got.Status != store.StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", got.Status)
	}
	if got.SessionID != "abc" {
		t.Errorf("SessionID = %q, want abc", got.SessionID)
	}
	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimSpace(string(argv))
	if !strings.HasPrefix(trimmed, "-p --mode json --session-id abc") {
		t.Errorf("pi argv = %q, want it to begin with -p --mode json --session-id", trimmed)
	}
}

// Background tasks must be off by default.
//
// claude backgrounds a subagent unless told otherwise, and "claude -p" waits a
// bounded time for background work, then KILLS it and exits zero. A dispatch
// that fanned out to subagents was recorded as succeeded with its work
// abandoned, and the loop retired the issue on that lie.
func TestClaudeEnvDisablesBackgroundTasksByDefault(t *testing.T) {
	env := claudeEnv(&config.Config{})

	if !slices.Contains(env, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1") {
		t.Errorf("background tasks must be disabled unless the operator opts in; got %v", env)
	}
	// agent.timeout is the outer bound for a dispatch. claude's own ten-minute
	// background ceiling is a second, shorter, invisible deadline that silently
	// preempts it.
	if !slices.Contains(env, "CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0") {
		t.Errorf("the background wait ceiling must be lifted; got %v", env)
	}
}

func TestClaudeEnvHonoursTheOptIn(t *testing.T) {
	on := true
	env := claudeEnv(&config.Config{Agent: config.Agent{BackgroundTasks: &on}})

	if slices.Contains(env, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1") {
		t.Errorf("background_tasks: true must reach the agent; got %v", env)
	}
	if !slices.Contains(env, "CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0") {
		t.Errorf("the ceiling stays lifted even when backgrounding is allowed; got %v", env)
	}
}

// The notice is one line in a stream that arrives in arbitrary chunks, so a
// match that depends on where the pipe broke would fail in production and pass
// in a test that writes the line whole.
func TestSentinelWriterFindsAMarkerSplitAcrossWrites(t *testing.T) {
	var sink bytes.Buffer
	w := newSentinelWriter(&sink, windDownNotice)

	half := len(windDownNotice) / 2
	for _, chunk := range []string{
		"tool noise\n",
		windDownNotice[:half],
		windDownNotice[half:] + " 600s; terminating.\n",
	} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if !w.Seen() {
		t.Error("Seen() = false; the marker spanned two writes and must still match")
	}
	// The stream must reach the log byte for byte: it is the operator's only
	// record of what the agent's stderr said.
	if got := sink.String(); !strings.Contains(got, windDownNotice+" 600s; terminating.") {
		t.Errorf("stderr was not passed through intact: %q", got)
	}
}

func TestSentinelWriterStaysQuietOnOrdinaryStderr(t *testing.T) {
	var sink bytes.Buffer
	w := newSentinelWriter(&sink, windDownNotice)

	for i := 0; i < 50; i++ {
		if _, err := w.Write([]byte("Background tasks are fine here\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if w.Seen() {
		t.Error("Seen() = true on stderr that never carried the notice")
	}
}

// The regression this whole change exists for.
//
// claude exits ZERO after killing background work it gave up waiting on, and
// the stream still carries an ordinary success result. Believed, that exit code
// marks the issue succeeded and the loop never returns to it: a real run lost
// half a phase of committed-to work this way, with the transcript's last word
// being the agent describing tasks it had handed to subagents and never saw
// land. The dispatch must fail so the next tick RESUMES the session.
func TestSuperviseFailsWhenClaudeAbandonsBackgroundWork(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		`echo '{"type":"result","subtype":"success","session_id":"bg","total_cost_usd":36.47,"duration_ms":26385,"is_error":false,"result":"Once that lands I will commit phase C."}'` + "\n" +
		"echo 'Background tasks still running after 600s; terminating." +
		" Set CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0 to wait indefinitely.' >&2\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := newStore(t)
	// Follow the tick's own order: BeginDispatch writes the issue row before
	// anything is spawned, and MarkSessionStarted updates that row rather than
	// creating one.
	if err := s.BeginDispatch("execution", "o/r", 68, "bg", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	id, _ := s.CreateDispatch(store.Dispatch{
		Loop: "execution", Repo: "o/r", Number: 68, Kind: store.KindStart, SessionID: "bg",
	})
	d, _ := s.GetDispatch(id)

	cfg := &config.Config{
		Agent: config.Agent{Model: "opus", Timeout: config.Duration(60e9)},
		Retry: config.Retry{Max: 3, Backoff: []config.Duration{config.Duration(60e9)}},
	}
	_ = Supervise(context.Background(), cfg, s, d,
		Invocation{SessionID: "bg", Prompt: "go"}, t.TempDir(),
		filepath.Join(t.TempDir(), "run.jsonl"))

	got, _ := s.GetDispatch(id)
	if got.Status != store.StatusFailed {
		t.Fatalf("Status = %q, want failed: claude exited zero with its subagents killed", got.Status)
	}
	if !strings.Contains(got.APIError, "background") {
		t.Errorf("APIError = %q, want it to name the abandoned background work", got.APIError)
	}
	// The issue must come back, and it must come back as a RESUME: the session
	// exists, and claude refuses a reused --session-id outright.
	st, err := s.IssueState("execution", "o/r", 68)
	if err != nil {
		t.Fatal(err)
	}
	if !st.NeedsRetry {
		t.Error("NeedsRetry = false; the abandoned work would never be picked up again")
	}
	if !st.SessionStarted {
		t.Error("SessionStarted = false; the retry must resume, not restart")
	}
}

// A dispatch that claude refuses because the session id is already in use must
// record the session as STARTED, so the next tick resumes instead of colliding
// with itself again.
//
// This is the koinos issue-73 wedge. Once an issue reached this state it could
// not leave: the dispatch failed at no cost, engine.Decide saw "no session" and
// chose START again, and the identical failure repeated on every tick. The
// refusal is proof the session exists, so it is treated as evidence rather than
// as an opaque non-zero exit.
func TestSuperviseTreatsASessionInUseRefusalAsProofTheSessionExists(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"echo 'Error: Session ID b3b1a9e5-fe9a-4b69-b681-5ed247fe01ff is already in use.' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := newStore(t)
	if err := s.BeginDispatch("execution", "o/r", 73, "b3b1a9e5", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	id, _ := s.CreateDispatch(store.Dispatch{
		Loop: "execution", Repo: "o/r", Number: 73, Kind: store.KindStart, SessionID: "b3b1a9e5",
	})
	d, _ := s.GetDispatch(id)

	cfg := &config.Config{
		Agent: config.Agent{Model: "opus", Timeout: config.Duration(60e9)},
		Retry: config.Retry{Max: 3, Backoff: []config.Duration{config.Duration(60e9)}},
	}
	_ = Supervise(context.Background(), cfg, s, d,
		Invocation{SessionID: "b3b1a9e5", Prompt: "go"}, t.TempDir(),
		filepath.Join(t.TempDir(), "run.jsonl"))

	got, _ := s.GetDispatch(id)
	if got.Status != store.StatusFailed {
		t.Errorf("Status = %q, want failed", got.Status)
	}
	// The whole point: without this the next tick starts a THIRD time against
	// the same id and fails identically, forever.
	st, err := s.IssueState("execution", "o/r", 73)
	if err != nil {
		t.Fatal(err)
	}
	if !st.SessionStarted {
		t.Error("SessionStarted = false; the loop would restart against the same " +
			"session id and wedge again")
	}
	// Failing is what spends the retry budget, so a session that genuinely
	// cannot be resumed eventually parks instead of looping at no cost.
	if !st.NeedsRetry {
		t.Error("NeedsRetry = false; the failure must schedule the resume")
	}
}
