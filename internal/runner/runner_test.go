package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
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
	// The runner outlives its agent child: it still calls finish, and
	// MarkNeedsRetry on a failure, after cmd.Wait returns. agent_pid must be
	// cleared back to 0 in that window, or a `--force` kill racing it would
	// read a pid the kernel may have reissued to an unrelated process and
	// SIGKILL that process's group (kill.go:291).
	if got.AgentPID != 0 {
		t.Errorf("AgentPID = %d, want 0 after Supervise returns", got.AgentPID)
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

// stubClaudeWithStderr writes body to stdout and errText to stderr, then exits
// with exitCode. It is stubClaude for the failures that say what went wrong on
// stderr rather than in a result line.
func stubClaudeWithStderr(t *testing.T, exitCode int, body, errText string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOF'\n" + body + "\nEOF\n" +
		"cat >&2 <<'EOF'\n" + errText + "\nEOF\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	p := filepath.Join(dir, "claude")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The failure this whole change exists for: a dispatch dies before writing a
// result line, and the only account of why is on stderr. Recording
// "exit status 1" throws it away, and the operator has no way back to it
// short of reading the log file by hand.
func TestSuperviseRecordsTheStderrTailWhenThereIsNoResultLine(t *testing.T) {
	stubClaudeWithStderr(t, 1, "", "No conversation found with session ID: abc")

	s := newStore(t)
	id, _ := s.CreateDispatch(store.Dispatch{
		Loop: "execution", Repo: "o/r", Number: 3, Kind: store.KindTend, SessionID: "abc",
	})
	d, _ := s.GetDispatch(id)

	cfg := &config.Config{Agent: config.Agent{Model: "opus", Timeout: config.Duration(60e9)}}
	_ = Supervise(context.Background(), cfg, s, d,
		Invocation{SessionID: "abc", Prompt: "go"}, t.TempDir(),
		filepath.Join(t.TempDir(), "run.jsonl"))

	got, _ := s.GetDispatch(id)
	if got.Status != store.StatusFailed {
		t.Fatalf("Status = %q, want failed", got.Status)
	}
	if got.APIError != "No conversation found with session ID: abc" {
		t.Errorf("APIError = %q, want the stderr tail", got.APIError)
	}
}

// A result line that names the failure outranks the stderr tail: the harness
// said it deliberately, where stderr is whatever happened to be printed last.
func TestSuperviseKeepsTheResultLineErrorOverTheStderrTail(t *testing.T) {
	stubClaudeWithStderr(t, 1,
		`{"type":"result","subtype":"error_during_execution","is_error":true,`+
			`"errors":["No conversation found with session ID: abc"]}`,
		"npm warn deprecated something@1.0.0")

	s := newStore(t)
	id, _ := s.CreateDispatch(store.Dispatch{
		Loop: "execution", Repo: "o/r", Number: 4, Kind: store.KindTend, SessionID: "abc",
	})
	d, _ := s.GetDispatch(id)

	cfg := &config.Config{Agent: config.Agent{Model: "opus", Timeout: config.Duration(60e9)}}
	_ = Supervise(context.Background(), cfg, s, d,
		Invocation{SessionID: "abc", Prompt: "go"}, t.TempDir(),
		filepath.Join(t.TempDir(), "run.jsonl"))

	got, _ := s.GetDispatch(id)
	if got.APIError != "No conversation found with session ID: abc" {
		t.Errorf("APIError = %q, want the result line's errors[]", got.APIError)
	}
}

// With nothing on stderr and no result line, the exit status is all there is.
// It must still be recorded: a blank reason reads as "no failure".
func TestSuperviseFallsBackToTheExitStatusWhenStderrIsEmpty(t *testing.T) {
	stubClaudeWithStderr(t, 1, "", "")

	s := newStore(t)
	id, _ := s.CreateDispatch(store.Dispatch{
		Loop: "execution", Repo: "o/r", Number: 5, Kind: store.KindStart, SessionID: "abc",
	})
	d, _ := s.GetDispatch(id)

	cfg := &config.Config{Agent: config.Agent{Model: "opus", Timeout: config.Duration(60e9)}}
	_ = Supervise(context.Background(), cfg, s, d,
		Invocation{SessionID: "abc", Prompt: "go"}, t.TempDir(),
		filepath.Join(t.TempDir(), "run.jsonl"))

	got, _ := s.GetDispatch(id)
	if got.APIError == "" {
		t.Error("APIError is empty; the exit status must still be recorded")
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

// stubLiveClaude writes a fake claude onto PATH that prints one stream-json
// line, records its own pid to pidFile, and then sleeps for a long time. It is
// `exec`ed as the last step so the recorded pid stays the process group
// leader's pid -- the same one cmd.Cancel and the SIGKILL sweep signal.
func stubLiveClaude(t *testing.T, pidFile string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"cat <<'EOF'\n" +
		`{"type":"result","subtype":"success","session_id":"abc","total_cost_usd":0.1,"duration_ms":10,"is_error":false,"result":"ok"}` +
		"\nEOF\n" +
		"echo $$ > " + pidFile + "\n" +
		"exec sleep 60\n"
	p := filepath.Join(dir, "claude")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestSuperviseUnderCancellationKillsTheAgentAndRecordsAnOutcome proves the
// SIGTERM path end to end: cancelling the context Supervise runs under, while
// a live agent is running, must make Supervise return, retire the dispatch
// row out of "running", and leave no agent process behind. That last
// assertion is the one proving the SIGKILL sweep (runner.go:211-216) actually
// reaches the agent's process group -- a killed-but-unreaped child still
// answers kill(pid, 0), so this polls a deadline rather than checking once.
func TestSuperviseUnderCancellationKillsTheAgentAndRecordsAnOutcome(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "agent.pid")
	stubLiveClaude(t, pidFile)

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
	cfg := &config.Config{Agent: config.Agent{Model: "opus"}}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Supervise(ctx, cfg, s, d,
			Invocation{SessionID: "abc", Prompt: "go"}, t.TempDir(), logPath)
	}()

	// Wait for the agent to record its own pid, proving it is actually
	// running before cancelling.
	var agentPID int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile)
		if err == nil && len(strings.TrimSpace(string(b))) > 0 {
			agentPID, err = strconv.Atoi(strings.TrimSpace(string(b)))
			if err == nil && agentPID > 0 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if agentPID <= 0 {
		t.Fatalf("stub agent never recorded a pid in %s", pidFile)
	}

	// Supervise must have recorded the agent's own pid via
	// SetDispatchAgentPID (runner.go:212-214) immediately after starting it,
	// while the agent is still alive. Without this, agent_pid stays 0
	// forever, and kill.go:291's `if w.Dispatch.AgentPID > 0` silently skips
	// the agent group kill under --force -- degrading it to a runner-only
	// kill that leaves the agent orphaned in a worktree the loop believes is
	// free. (agent_pid is read here, before cancellation: Supervise clears it
	// back to 0 once cmd.Wait returns, so a live process is the only window
	// in which it is expected to be set.)
	if beforeCancel, err := s.GetDispatch(id); err != nil {
		t.Fatal(err)
	} else if beforeCancel.AgentPID != agentPID {
		t.Errorf("AgentPID = %d, want the stub's own pid %d while it is still running", beforeCancel.AgentPID, agentPID)
	}

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Supervise returned nil error for a cancelled run; want a recorded failure")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Supervise did not return within 15s of cancellation")
	}

	got, err := s.GetDispatch(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == store.StatusRunning {
		t.Fatalf("dispatch status = %q, want it retired out of running", got.Status)
	}

	// The stub process must actually be gone -- proof the SIGKILL sweep
	// reached its process group, not merely that Supervise gave up waiting.
	deadline = time.Now().Add(10 * time.Second)
	for {
		if err := syscall.Kill(agentPID, 0); err != nil {
			break // ESRCH: the process is gone.
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent pid %d is still alive 10s after cancellation", agentPID)
		}
		time.Sleep(50 * time.Millisecond)
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

// The last stderr line is what a harness prints on its way out, and for a
// failure with no result line it is the only sentence naming the cause. It is
// captured as the stream is written, because the file it lands in can be
// megabytes of tool noise.
func TestTailWriterKeepsTheLastNonEmptyLine(t *testing.T) {
	var sink bytes.Buffer
	w := newTailWriter(&sink)

	for _, chunk := range []string{
		"npm warn something\n",
		"No conversation found with session ID: abc\n",
		"\n \n",
	} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if got := w.Last(); got != "No conversation found with session ID: abc" {
		t.Errorf("Last() = %q, want the last non-blank line", got)
	}
	if got := sink.String(); !strings.Contains(got, "npm warn something") {
		t.Errorf("stderr was not passed through intact: %q", got)
	}
}

// A line split across writes is the normal case, not the exotic one: which
// bytes land in which Write is decided by the pipe.
func TestTailWriterJoinsALineSplitAcrossWrites(t *testing.T) {
	var sink bytes.Buffer
	w := newTailWriter(&sink)

	for _, chunk := range []string{"No conversation ", "found with session ID: abc\n"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if got := w.Last(); got != "No conversation found with session ID: abc" {
		t.Errorf("Last() = %q, want the joined line", got)
	}
}

// A harness that exits without a trailing newline still said something.
func TestTailWriterKeepsAnUnterminatedFinalLine(t *testing.T) {
	var sink bytes.Buffer
	w := newTailWriter(&sink)

	if _, err := w.Write([]byte("first\nfatal: no such session")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := w.Last(); got != "fatal: no such session" {
		t.Errorf("Last() = %q, want the unterminated final line", got)
	}
}

// Empty stderr must read as empty rather than as a blank reason, so the
// caller can fall back to the exit status.
func TestTailWriterReportsNothingForEmptyStderr(t *testing.T) {
	var sink bytes.Buffer
	w := newTailWriter(&sink)

	if _, err := w.Write([]byte("   \n\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := w.Last(); got != "" {
		t.Errorf("Last() = %q, want empty", got)
	}
}

// A single enormous line must not grow the buffer without bound: this runs for
// the whole life of an agent that can print megabytes of tool output.
func TestTailWriterBoundsWhatItRetains(t *testing.T) {
	var sink bytes.Buffer
	w := newTailWriter(&sink)

	if _, err := w.Write([]byte(strings.Repeat("x", 64*1024))); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := len(w.Last()); got > tailLimit {
		t.Errorf("Last() kept %d bytes, want at most %d", got, tailLimit)
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

// A SUCCESSFUL run whose stderr merely mentions the phrase must not be
// rewritten to failed.
//
// stderr carries megabytes of tool output, and a dev server saying "port 3000
// is already in use" is not claude refusing a session. Before the pattern was
// tightened and gated on a non-zero exit, that run's finished work was thrown
// away and dispatched again.
func TestSuperviseKeepsASuccessfulRunThatMerelyMentionsSomethingInUse(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"echo 'warning: port 3000 is already in use, retrying' >&2\n" +
		`echo '{"type":"result","subtype":"success","session_id":"ok1","total_cost_usd":2.5,"is_error":false,"result":"done"}'` + "\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	s := newStore(t)
	if err := s.BeginDispatch("execution", "o/r", 80, "ok1", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	id, _ := s.CreateDispatch(store.Dispatch{
		Loop: "execution", Repo: "o/r", Number: 80, Kind: store.KindStart, SessionID: "ok1",
	})
	d, _ := s.GetDispatch(id)

	cfg := &config.Config{Agent: config.Agent{Model: "opus", Timeout: config.Duration(60e9)}}
	if err := Supervise(context.Background(), cfg, s, d,
		Invocation{SessionID: "ok1", Prompt: "go"}, t.TempDir(),
		filepath.Join(t.TempDir(), "run.jsonl")); err != nil {
		t.Fatalf("Supervise: %v", err)
	}

	got, _ := s.GetDispatch(id)
	if got.Status != store.StatusSucceeded {
		t.Errorf("Status = %q, want succeeded: finished work must not be discarded "+
			"because tool output mentioned something being in use", got.Status)
	}
	if got.CostUSD != 2.5 {
		t.Errorf("CostUSD = %v, want 2.5", got.CostUSD)
	}
}

// The pattern must match claude's actual line, not the bare phrase.
func TestSessionInUsePatternMatchesOnlyClaudesRefusal(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"claude's refusal", "Error: Session ID b3b1a9e5-fe9a-4b69-b681-5ed247fe01ff is already in use.", true},
		{"a dev server", "warning: port 3000 is already in use, retrying", false},
		{"a lock file", "the file is already in use by another process", false},
		{"the bare phrase", "is already in use", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionInUse.MatchString(tc.line); got != tc.want {
				t.Errorf("MatchString(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// The refusal arrives in whatever chunks the pipe hands over, so a match split
// across two writes must still be found.
func TestPatternWriterFindsAMatchSplitAcrossWrites(t *testing.T) {
	var sink bytes.Buffer
	w := newPatternWriter(&sink, sessionInUse)

	for _, chunk := range []string{
		"tool noise\n",
		"Error: Session ID b3b1a9e5 ",
		"is already in use.\n",
	} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if !w.Seen() {
		t.Error("Seen() = false; the refusal spanned two writes and must still match")
	}
	if got := sink.String(); !strings.Contains(got, "Error: Session ID b3b1a9e5 is already in use.") {
		t.Errorf("stderr was not passed through intact: %q", got)
	}
}
