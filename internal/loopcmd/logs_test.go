package loopcmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/project"
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

// The result line's errors[] is where a refused resume says so. Rendering the
// line without it printed "result: error_during_execution turns=0" and left
// the operator to open the log file by hand.
func TestRendererReportsTheResultLineErrors(t *testing.T) {
	out := render(t, LogOptions{},
		`{"type":"result","subtype":"error_during_execution","is_error":true,`+
			`"errors":["No conversation found with session ID: abc"]}`)
	if !strings.Contains(out, "No conversation found with session ID: abc") {
		t.Errorf("the result line's errors must be surfaced:\n%s", out)
	}
}

func TestRendererReportsEveryResultLineError(t *testing.T) {
	out := render(t, LogOptions{},
		`{"type":"result","subtype":"error","is_error":true,"errors":["first","second"]}`)
	for _, want := range []string{"first", "second"} {
		if !strings.Contains(out, want) {
			t.Errorf("error %q was dropped:\n%s", want, out)
		}
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

func TestRenderSessionsAggregatesAndFlags(t *testing.T) {
	p := &Project{Config: &projectConfigStub, Root: "/p", Dir: "/p/.agent-utils"}
	out := RenderSessions(p, []Session{
		{ID: "sess-a", Loop: "planning", Issue: 42, Title: "Add zone lookup",
			Dispatches: 3, Cost: 5.05, LastStatus: store.StatusSucceeded},
		{ID: "sess-b", Loop: "planning", Issue: 57, Title: "Timezone bug",
			Dispatches: 1, Cost: 2.40, LastStatus: store.StatusRunning, Orphaned: true},
		{ID: "sess-c", Loop: "execution", Issue: 58, Dispatches: 1,
			LastStatus: store.StatusRunning, Live: true},
	}, false)

	for _, want := range []string{"sess-a", "Add zone lookup", "$5.05", "ORPHANED", "running", "--session"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q:\n%s", want, out)
		}
	}
}

func TestRenderSessionsExplainsAnEmptyList(t *testing.T) {
	p := &Project{Config: &projectConfigStub, Root: "/p", Dir: "/p/.agent-utils"}
	if out := RenderSessions(p, nil, false); !strings.Contains(out, "No sessions yet") {
		t.Errorf("an empty list must explain itself:\n%s", out)
	}
}

func TestRenderAllSessionsShowsTheProjectColumnAndFlagsAnOrphan(t *testing.T) {
	out := RenderAllSessions([]Session{
		{ID: "sess-a", Project: "weather", Loop: "planning", Issue: 42,
			Title: "Add zone lookup", Dispatches: 3, Cost: 5.05,
			LastStatus: store.StatusSucceeded},
		{ID: "sess-b", Project: "atlas", Loop: "planning", Issue: 57,
			Title: "Timezone bug", Dispatches: 1, Cost: 2.40,
			LastStatus: store.StatusRunning, Orphaned: true},
		// LastStatus is deliberately NOT "running": the Live branch has to be
		// the only thing that can produce that word, or deleting the branch
		// leaves the output identical and the assertion below proves nothing.
		{ID: "sess-c", Project: "(unclaimed)", Loop: "execution", Issue: 58,
			Dispatches: 1, LastStatus: store.StatusSucceeded, Live: true},
	}, SessionFilter{})

	for _, want := range []string{"PROJECT", "weather", "atlas", "(unclaimed)",
		"sess-a", "Add zone lookup", "$5.05", "ORPHANED", "running", "--session"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q:\n%s", want, out)
		}
	}
	// The follow command must name the project: top-level logs resolves the
	// project from the working directory, which is not where this table is read.
	if !strings.Contains(out, "project --name") {
		t.Errorf("output should contain %q:\n%s", "project --name", out)
	}
	// Header and rows must stay pinned to each other. Both are written from
	// separate format strings, so widening one and not the other misaligns
	// every row of the table while every Contains assertion above still
	// passes.
	lines := strings.Split(strings.TrimLeft(out, "\n"), "\n")
	header, row := lines[0], lines[1]
	for _, col := range []struct{ head, cell string }{
		{"PROJECT", "weather"},
		{"SESSION", "sess-a"},
		{"LOOP", "planning"},
		{"TITLE", "Add zone lookup"},
	} {
		if got, want := strings.Index(row, col.cell), strings.Index(header, col.head); got != want {
			t.Errorf("column %s starts at %d in the row and %d in the header:\n%s",
				col.head, got, want, out)
		}
	}
}

func TestRenderAllSessionsExplainsAnEmptyList(t *testing.T) {
	out := RenderAllSessions(nil, SessionFilter{})
	if !strings.Contains(out, "No sessions yet") {
		t.Errorf("an unfiltered empty list must explain itself:\n%s", out)
	}
	if !strings.Contains(out, "agent-utils list") {
		t.Errorf("an unfiltered empty list must point at %q:\n%s", "agent-utils list", out)
	}

	filtered := RenderAllSessions(nil, SessionFilter{Loop: "planning"})
	if !strings.Contains(filtered, "No sessions matched") {
		t.Errorf("a filtered empty list must say the filter excluded everything:\n%s", filtered)
	}
	if strings.Contains(filtered, "No sessions yet") {
		t.Errorf("a filtered empty list must not claim nothing has run:\n%s", filtered)
	}
}

func TestRenderProjectDetailShowsIdentityAndKeepsTheTableOnOneLine(t *testing.T) {
	p := &Project{Config: &projectConfigStub, Root: "/p", Dir: "/p/.agent-utils"}
	out := RenderProjectDetail(&ProjectDetail{
		Project: p,
		Entries: []config.Entry{{Name: "planning"}, {Name: "broken", Err: errMultiLine}},
		Loops: []LoopSummary{
			{Name: "planning", Repo: "o/r", Ticks: 9, Live: 1, Cost: 7.75},
			{Name: "broken", Err: errMultiLine},
		},
	})

	for _, want := range []string{"proj-stub", "id ", "configs", "planning", "o/r"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q:\n%s", want, out)
		}
	}
	// A multi-line config error must not break the table; it belongs below it.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "broken ") && strings.Contains(line, "second line") {
			t.Errorf("a multi-line error leaked into the table row: %q", line)
		}
	}
	if !strings.Contains(out, "second line") {
		t.Error("the full error must still be printed under the table")
	}
}

var projectConfigStub = project.Config{Name: "proj-stub", ID: "id-1234"}

var errMultiLine = errors.New("parse failed:\n  second line of the error")

// TestRenderProjectDetailReportsRecordedWebhooks covers surfacing the recorded
// registration: an operator debugging a loop that stopped reacting to GitHub
// needs to see whether a webhook is recorded for the repository at all, and
// which hook id it is, without opening the state database by hand.
func TestRenderProjectDetailReportsRecordedWebhooks(t *testing.T) {
	p := &Project{Config: &projectConfigStub, Root: "/p", Dir: "/p/.agent-utils"}
	out := RenderProjectDetail(&ProjectDetail{
		Project: p,
		Entries: []config.Entry{{Name: "planning", Repo: "o/r"}, {Name: "other", Repo: "o/quiet"}},
		Loops: []LoopSummary{
			{Name: "planning", Repo: "o/r"},
			{Name: "other", Repo: "o/quiet"},
		},
		Webhooks: []WebhookStatus{
			{Repo: "o/r", Recorded: true, HookID: 123, URL: "https://hooks.example/webhook"},
			{Repo: "o/quiet"},
		},
	})

	for _, want := range []string{"o/r", "123", "https://hooks.example/webhook"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should report the recorded hook %q:\n%s", want, out)
		}
	}
	// A repository with no record must say so rather than be omitted: a silent
	// gap reads as "fine", which is the state the operator is trying to rule out.
	var quiet string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "o/quiet") {
			quiet = line
		}
	}
	if quiet == "" || !strings.Contains(quiet, "not recorded") {
		t.Errorf("a repository with no recorded webhook must say so, got %q:\n%s", quiet, out)
	}
}

func TestRendererPiShowsTextAndTools(t *testing.T) {
	out := render(t, LogOptions{Harness: config.HarnessPi},
		`{"type":"session","id":"abc"}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"I opened the work."},{"type":"text","text":" Right."}]}}`,
		`{"type":"tool_execution_start","toolName":"bash"}`,
		`{"type":"tool_execution_end","toolName":"bash"}`,
		`{"type":"agent_settled"}`,
	)
	for _, want := range []string{"session start", "I opened the work.", "Right.", "→ bash", "← bash"} {
		if !strings.Contains(out, want) {
			t.Errorf("pi output should contain %q:\n%s", want, out)
		}
	}
}

func TestRendererPiIsolatedFromClaude(t *testing.T) {
	// A claude system line must not render as a pi assistant message.
	out := render(t, LogOptions{Harness: config.HarnessPi},
		`{"type":"system","subtype":"init","model":"opus"}`)
	if strings.Contains(out, "model=opus") {
		t.Errorf("pi renderer misread a claude system line:\n%s", out)
	}
	if strings.Contains(out, "assistant") {
		t.Errorf("pi renderer misread a claude system line as assistant text:\n%s", out)
	}
}

// A bare `project logs` must not land on a rebase row. git wrote no transcript
// for it, so selecting it as "the newest dispatch" answers with an empty path
// and the operator sees nothing at all instead of the last agent that ran.
func TestSelectDispatchSkipsARebaseRowWithNoLog(t *testing.T) {
	s := openLogsStore(t)
	cfg := &config.Config{Name: "execution", Repo: "o/r"}

	if _, err := s.CreateDispatch(store.Dispatch{
		Loop: "execution", Repo: "o/r", Number: 7, Kind: store.KindTend,
		SessionID: "s1", LogPath: "/logs/agent.jsonl",
	}); err != nil {
		t.Fatal(err)
	}
	// Newest, and the ones a naive selection would return. There are more of
	// them than any fixed page a filter-in-Go implementation would fetch: once
	// most tend work is agent-free, a run of rebases this long is ordinary,
	// and the last agent must still be findable behind it.
	for i := 0; i < 60; i++ {
		if err := s.RecordFinishedDispatch(store.Dispatch{
			Loop: "execution", Repo: "o/r", Number: 7, Kind: store.KindRebase, PRNumber: 12,
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := SelectDispatch(s, cfg, LogOptions{})
	if err != nil {
		t.Fatalf("SelectDispatch: %v", err)
	}
	if got.Kind != store.KindTend || got.LogPath != "/logs/agent.jsonl" {
		t.Errorf("selected %s dispatch with log %q; want the tend agent's log",
			got.Kind, got.LogPath)
	}

	// --dispatch still reaches it, which is where an operator who wants the
	// rebase row looks.
	ds, err := s.RecentDispatches("execution", "o/r", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 61 {
		t.Fatalf("dispatches = %d, want 61: --list must still show the rebase rows", len(ds))
	}
	byID, err := SelectDispatch(s, cfg, LogOptions{Dispatch: ds[0].ID})
	if err != nil {
		t.Fatalf("SelectDispatch by id: %v", err)
	}
	if byID.Kind != store.KindRebase {
		t.Errorf("--dispatch selected %q, want %q", byID.Kind, store.KindRebase)
	}
}

// With nothing but rebase rows there is no transcript to show, and the honest
// answer is the same one an empty loop gets.
func TestSelectDispatchReportsNoDispatchWhenOnlyRebasesExist(t *testing.T) {
	s := openLogsStore(t)
	cfg := &config.Config{Name: "execution", Repo: "o/r"}
	if err := s.RecordFinishedDispatch(store.Dispatch{
		Loop: "execution", Repo: "o/r", Number: 7, Kind: store.KindRebase,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := SelectDispatch(s, cfg, LogOptions{}); !errors.Is(err, ErrNoDispatch) {
		t.Errorf("err = %v, want ErrNoDispatch", err)
	}
}

func openLogsStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db.Project(testProject)
}
