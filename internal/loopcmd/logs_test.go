package loopcmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/store"
)

func render(t *testing.T, opts LogOptions, lines ...string) string {
	t.Helper()
	var b bytes.Buffer
	r := newRenderer(&b, opts)
	for _, l := range lines {
		r.line(l)
	}
	return b.String()
}

func TestRendererShowsWhatMattersAndHidesNoise(t *testing.T) {
	out := render(t, LogOptions{},
		`{"type":"system","subtype":"init","model":"opus","cwd":"/w","permissionMode":"bypassPermissions"}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":10}`,
		`{"type":"rate_limit_event"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Opening the PR."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
		`{"type":"result","subtype":"success","num_turns":3,"total_cost_usd":1.25,"duration_ms":5000,"result":"done"}`,
	)

	for _, want := range []string{"session start", "opus", "Opening the PR.", "Bash", "go test", "ok", "cost=$1.2500", "turns=3"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q:\n%s", want, out)
		}
	}
	// Token counters and rate-limit events are pure noise in a transcript.
	for _, unwanted := range []string{"thinking_tokens", "rate_limit", "estimated_tokens"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output should not contain %q:\n%s", unwanted, out)
		}
	}
}

func TestRendererHidesThinkingUnlessAsked(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"secret reasoning"}]}}`

	if out := render(t, LogOptions{}, line); strings.Contains(out, "secret reasoning") {
		t.Errorf("thinking must be hidden by default:\n%s", out)
	}
	if out := render(t, LogOptions{Thinking: true}, line); !strings.Contains(out, "secret reasoning") {
		t.Errorf("--thinking must show it:\n%s", out)
	}
}

func TestRendererMarksAFailedToolResult(t *testing.T) {
	out := render(t, LogOptions{},
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"boom","is_error":true}]}}`)
	if !strings.Contains(out, "←!") {
		t.Errorf("a failed tool result must be marked:\n%s", out)
	}
}

// A malformed or non-JSON line must still be shown. Dropping it would hide the
// very output someone is reading the log to find.
func TestRendererPassesThroughUnparseableLines(t *testing.T) {
	out := render(t, LogOptions{}, "panic: something went wrong", `{"broken`)
	if !strings.Contains(out, "panic: something went wrong") || !strings.Contains(out, `{"broken`) {
		t.Errorf("unparseable lines must pass through:\n%s", out)
	}
}

func TestRendererReportsAnAPIError(t *testing.T) {
	out := render(t, LogOptions{},
		`{"type":"result","subtype":"error","is_error":true,"api_error_status":"529"}`)
	if !strings.Contains(out, "529") {
		t.Errorf("an api error must be surfaced:\n%s", out)
	}
}

func TestTailReadsAWholeFileWithoutFollow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jsonl")
	body := `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var b bytes.Buffer
	// PID 0 is never alive, so this would hang if Follow were honoured wrongly.
	err := Tail(context.Background(), &b, path, store.Dispatch{ID: 1, PID: 0}, LogOptions{})
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if !strings.Contains(b.String(), "hello") {
		t.Errorf("got %q", b.String())
	}
}

// Following a dispatch whose process is gone must return, not hang.
func TestTailWithFollowStopsWhenTheProcessIsGone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.jsonl")
	if err := os.WriteFile(path, []byte("plain line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	var b bytes.Buffer
	go func() {
		done <- Tail(context.Background(), &b, path,
			store.Dispatch{ID: 999, PID: 0}, LogOptions{Follow: true})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Tail: %v", err)
		}
	case <-timeAfterSeconds(5):
		t.Fatal("Tail with Follow hung on a dead dispatch")
	}
	if !strings.Contains(b.String(), "plain line") {
		t.Errorf("got %q", b.String())
	}
}

func TestTailReportsAMissingLogClearly(t *testing.T) {
	err := Tail(context.Background(), &bytes.Buffer{},
		filepath.Join(t.TempDir(), "nope.jsonl"), store.Dispatch{}, LogOptions{})
	if err == nil || !strings.Contains(err.Error(), "no log at") {
		t.Fatalf("err = %v, want a clear missing-log message", err)
	}
}

func TestRenderDispatchListFlagsADeadRunner(t *testing.T) {
	out := RenderDispatchList([]store.Dispatch{
		{ID: 7, Number: 42, Kind: store.KindStart, Status: store.StatusRunning, PID: 0},
	})
	if !strings.Contains(out, "DEAD") {
		t.Errorf("a running row with a dead process must show DEAD:\n%s", out)
	}
	if RenderDispatchList(nil) == "" {
		t.Error("an empty list must still say something")
	}
}

func timeAfterSeconds(n int) <-chan time.Time {
	return time.After(time.Duration(n) * time.Second)
}
