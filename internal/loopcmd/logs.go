package loopcmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/runner"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// LogOptions selects which log to show and how.
type LogOptions struct {
	// Session selects the dispatches that used one claude session. A session
	// survives resumes, so it identifies an issue's whole conversation.
	Session string
	// Issue restricts selection to one issue. Zero means any.
	Issue int
	// Dispatch selects one dispatch by identifier. Zero means the most recent.
	Dispatch int64
	// Stream picks which of the three files to read.
	Stream LogStream
	// Follow keeps reading while the dispatch is alive.
	Follow bool
	// Raw prints the file verbatim instead of rendering it.
	Raw bool
	// Thinking includes the agent's thinking blocks when rendering.
	Thinking bool
	// PathOnly prints the log's path and nothing else.
	PathOnly bool
	// Harness selects the stream shape the renderer expects.
	Harness string
}

// LogStream identifies one of the three files a dispatch writes.
type LogStream string

// The three streams a dispatch produces.
const (
	// StreamAgent is the agent's stream-json transcript.
	StreamAgent LogStream = "agent"
	// StreamStderr is the agent's standard error.
	StreamStderr LogStream = "stderr"
	// StreamRunner is the detached runner's own structured log.
	StreamRunner LogStream = "runner"
)

// ErrNoDispatch reports that no dispatch matched the selection.
var ErrNoDispatch = errors.New("no dispatch found")

// SelectDispatch finds the dispatch whose logs should be shown.
func SelectDispatch(s *store.Store, cfg *config.Config, opts LogOptions) (store.Dispatch, error) {
	if opts.Dispatch > 0 {
		return s.GetDispatch(opts.Dispatch)
	}
	if opts.Session != "" {
		ds, err := s.DispatchesBySession(opts.Session)
		if err != nil {
			return store.Dispatch{}, err
		}
		if len(ds) == 0 {
			return store.Dispatch{}, fmt.Errorf("%w for session %q", ErrNoDispatch, opts.Session)
		}
		return ds[0], nil
	}
	recent, err := s.RecentDispatches(cfg.Name, cfg.Repo, opts.Issue, 1)
	if err != nil {
		return store.Dispatch{}, err
	}
	if len(recent) == 0 {
		if opts.Issue > 0 {
			return store.Dispatch{}, fmt.Errorf("%w for issue #%d in loop %q",
				ErrNoDispatch, opts.Issue, cfg.Name)
		}
		return store.Dispatch{}, fmt.Errorf("%w in loop %q", ErrNoDispatch, cfg.Name)
	}
	return recent[0], nil
}

// LogPathFor returns the file backing one stream of a dispatch.
func LogPathFor(cfg *config.Config, d store.Dispatch, stream LogStream) string {
	switch stream {
	case StreamStderr:
		return d.LogPath + ".stderr"
	case StreamRunner:
		return runner.RunnerLogPath(cfg.StateDir, cfg.Name, d.RunnerID())
	default:
		return d.LogPath
	}
}

// RenderDispatchList formats recent dispatches so an operator can pick one.
func RenderDispatchList(ds []store.Dispatch) string {
	var b strings.Builder
	if len(ds) == 0 {
		return "no dispatches recorded yet\n"
	}
	fmt.Fprintf(&b, "%-6s %-6s %-8s %-11s %-8s %-9s %s\n",
		"ID", "ISSUE", "KIND", "STATUS", "COST", "PID", "STARTED")
	for _, d := range ds {
		status := d.Status
		if d.Status == store.StatusRunning {
			if proc.IsAlive(d.PID, d.RunnerID()) {
				status = "running"
			} else {
				status = "DEAD"
			}
		}
		fmt.Fprintf(&b, "%-6d %-6d %-8s %-11s $%-7.2f %-9d %s\n",
			d.ID, d.Number, d.Kind, status, d.CostUSD, d.PID,
			d.StartedAt.Local().Format("2006-01-02 15:04:05"))
	}
	return b.String()
}

// Tail streams a log file to out.
//
// With Follow set it keeps reading while the dispatch's process is alive, the
// way `tail -f` does. It stops at end of file once the process is gone, so
// following a finished run terminates instead of hanging.
func Tail(ctx context.Context, out io.Writer, path string, d store.Dispatch, opts LogOptions) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no log at %s yet; the dispatch may not have started writing", path)
		}
		return err
	}
	defer f.Close()

	r := bufio.NewReader(f)
	render := newRenderer(out, opts)

	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			if opts.Raw {
				if _, werr := io.WriteString(out, line); werr != nil {
					return werr
				}
			} else {
				render.line(strings.TrimRight(line, "\n"))
				if render.err != nil {
					// The reader closed the pipe. Stop rather than following a
					// live agent with nowhere to write.
					return render.err
				}
			}
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, io.EOF) {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if !opts.Follow {
			return nil
		}

		// At end of file. Keep waiting only while the writer is alive.
		if !proc.IsAlive(d.PID, d.RunnerID()) {
			// Drain whatever landed between the last read and the process
			// exiting, then stop.
			rest, _ := io.ReadAll(r)
			if len(rest) > 0 {
				if opts.Raw {
					if _, werr := out.Write(rest); werr != nil {
						return werr
					}
				} else {
					for _, l := range strings.Split(strings.TrimRight(string(rest), "\n"), "\n") {
						render.line(l)
					}
				}
			}
			return render.err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// renderer turns stream-json into something readable.
//
// The raw transcript is one JSON object per line, most of them thinking blocks
// and token counters. Printing it verbatim is unreadable, which is the whole
// reason this exists; --raw is there for when the JSON itself is wanted.
type renderer struct {
	out  io.Writer
	opts LogOptions
	// harness selects the stream shape the renderer expects.
	harness string
	// err holds the first write failure. Writing to standard output really can
	// fail: `agent-utils logs -f` | head` closes the pipe, and a renderer that
	// ignored that would spin until the agent exited.
	err error
}

func newRenderer(out io.Writer, opts LogOptions) *renderer {
	return &renderer{out: out, opts: opts, harness: opts.Harness}
}

// printf writes unless a previous write already failed.
func (r *renderer) printf(format string, args ...any) {
	if r.err != nil {
		return
	}
	if _, err := fmt.Fprintf(r.out, format, args...); err != nil {
		r.err = err
	}
}

type streamLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message struct {
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
			Content  json.RawMessage `json:"content"`
			IsError  bool            `json:"is_error"`
		} `json:"content"`
	} `json:"message"`
	Model          string  `json:"model"`
	Cwd            string  `json:"cwd"`
	PermissionMode string  `json:"permissionMode"`
	Result         string  `json:"result"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	DurationMS     int64   `json:"duration_ms"`
	NumTurns       int     `json:"num_turns"`
	IsError        bool    `json:"is_error"`
	APIError       *string `json:"api_error_status"`
	// Errors carries what the harness said went wrong when the failure was not
	// an API status. A refused resume reports api_error_status null and one
	// error here, so a renderer that reads only APIError shows the run failing
	// and never says why.
	Errors []string `json:"errors"`
}

func (r *renderer) line(raw string) {
	if r.harness == config.HarnessPi {
		r.piLine(raw)
		return
	}
	if raw == "" {
		return
	}
	if !strings.HasPrefix(raw, "{") {
		// A wrapper can print plain text. Show it rather than dropping it.
		r.printf("%s\n", raw)
		return
	}
	var l streamLine
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		r.printf("%s\n", raw)
		return
	}

	switch l.Type {
	case "system":
		if l.Subtype == "init" {
			r.printf("── session start  model=%s  permissions=%s\n   cwd=%s\n",
				l.Model, l.PermissionMode, l.Cwd)
		}
		// Every other system line is a hook or a token counter: noise here.
	case "assistant":
		for _, c := range l.Message.Content {
			switch c.Type {
			case "text":
				if s := strings.TrimSpace(c.Text); s != "" {
					r.printf("\n%s\n", s)
				}
			case "thinking":
				if r.opts.Thinking {
					r.printf("\n[thinking] %s\n", strings.TrimSpace(c.Thinking))
				}
			case "tool_use":
				r.printf("  → %s %s\n", c.Name, summarise(c.Input, 100))
			}
		}
	case "user":
		for _, c := range l.Message.Content {
			if c.Type == "tool_result" {
				marker := "←"
				if c.IsError {
					marker = "←!"
				}
				r.printf("  %s %s\n", marker, summarise(c.Content, 100))
			}
		}
	case "result":
		r.printf("\n── result: %s  turns=%d  cost=$%.4f  duration=%s\n",
			l.Subtype, l.NumTurns, l.TotalCostUSD,
			(time.Duration(l.DurationMS) * time.Millisecond).Round(time.Second))
		if l.APIError != nil && *l.APIError != "" {
			r.printf("   api error: %s\n", *l.APIError)
		}
		for _, e := range l.Errors {
			if s := strings.TrimSpace(e); s != "" {
				r.printf("   error: %s\n", s)
			}
		}
		if s := strings.TrimSpace(l.Result); s != "" {
			r.printf("   %s\n", s)
		}
	}
}

// summarise renders a JSON value on one line, clipped to width.
func summarise(raw json.RawMessage, width int) string {
	if len(raw) == 0 {
		return ""
	}
	s := string(raw)
	// Try to unwrap a plain string, which is the common tool_result shape.
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		s = str
	}
	s = strings.Join(strings.Fields(s), " ")
	return truncate(s, width)
}

// piStreamLine is the subset of a pi event line the renderer reads.
type piStreamLine struct {
	Type    string `json:"type"`
	Message struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	ToolName string `json:"toolName"`
}

// piLine renders one pi event line. It mirrors the claude renderer's
// presentation: assistant text as plain lines, tool calls as arrows.
func (r *renderer) piLine(raw string) {
	if raw == "" {
		return
	}
	if !strings.HasPrefix(raw, "{") {
		r.printf("%s\n", raw)
		return
	}
	var l piStreamLine
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		r.printf("%s\n", raw)
		return
	}
	switch l.Type {
	case "session":
		r.printf("── session start\n")
	case "message_end":
		if l.Message.Role == "assistant" {
			for _, c := range l.Message.Content {
				if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
					r.printf("\n%s\n", strings.TrimSpace(c.Text))
				}
			}
		}
	case "tool_execution_start":
		r.printf("  → %s\n", l.ToolName)
	case "tool_execution_end":
		r.printf("  ← %s\n", l.ToolName)
	}
}
