package runner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Result is the outcome of one claude run.
type Result struct {
	SessionID  string
	CostUSD    float64
	DurationMS int64
	IsError    bool
	APIError   string
	Text       string
}

// ErrNoResult reports that the stream held no result line.
var ErrNoResult = errors.New("stream contains no result line")

type resultLine struct {
	Type           string  `json:"type"`
	SessionID      string  `json:"session_id"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	DurationMS     int64   `json:"duration_ms"`
	IsError        bool    `json:"is_error"`
	APIErrorStatus *string `json:"api_error_status"`
	ResultText     string  `json:"result"`
}

// ParseStream reads a claude stream-json stream and returns the final result.
//
// The parser keeps the last line whose type is "result". It ignores every other
// line and every line that is not JSON, because the stream also carries system,
// assistant, and rate limit events, and a wrapper can print plain text.
func ParseStream(r io.Reader) (Result, error) {
	sc := bufio.NewScanner(r)
	// A single stream line can be far larger than the 64 KiB default.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var last *resultLine
	// The session id is taken from the FIRST line that carries one, not from
	// the result line alone.
	//
	// claude announces the session in a system event before it does any work,
	// and every later line repeats it, but a run that is killed never writes a
	// result line at all. Reading the id only from that line threw it away for
	// exactly the runs that need it: the caller then recorded "no session was
	// started", the next tick dispatched a START against the id already on the
	// issue row, and claude refuses a reused --session-id outright. That wedged
	// the loop permanently -- every later tick failed the same way, at no cost,
	// so the retry budget never ran down and the issue never parked.
	var sessionID string
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rl resultLine
		if err := json.Unmarshal(line, &rl); err != nil {
			continue
		}
		if sessionID == "" {
			sessionID = rl.SessionID
		}
		if rl.Type != "result" {
			continue
		}
		copied := rl
		last = &copied
	}
	if err := sc.Err(); err != nil {
		return Result{}, fmt.Errorf("read stream: %w", err)
	}
	if last == nil {
		// ErrNoResult, but the session id still travels: Supervise reads it to
		// decide whether the next dispatch must RESUME rather than start.
		return Result{SessionID: sessionID}, ErrNoResult
	}

	out := Result{
		SessionID:  last.SessionID,
		CostUSD:    last.TotalCostUSD,
		DurationMS: last.DurationMS,
		IsError:    last.IsError,
		Text:       last.ResultText,
	}
	if out.SessionID == "" {
		out.SessionID = sessionID
	}
	if last.APIErrorStatus != nil {
		out.APIError = *last.APIErrorStatus
	}
	return out, nil
}

// piEvent is one line of pi's json event stream. Only the fields the parser
// reads are unmarshalled; the rest of the event (thinking blocks, token
// counters, tool arguments) is ignored.
type piEvent struct {
	Type    string     `json:"type"`
	ID      string     `json:"id"`
	Message *piMessage `json:"message"`
}

// piMessage is the inner message on a message_end event.
type piMessage struct {
	Role         string      `json:"role"`
	StopReason   string      `json:"stopReason"`
	ErrorMessage string      `json:"errorMessage"`
	Content      []piContent `json:"content"`
	Usage        piUsage     `json:"usage"`
}

// piContent is a text block inside an assistant message.
type piContent struct {
	Text string `json:"text"`
}

// piUsage carries the cost pi reports for one assistant message.
type piUsage struct {
	Cost piCost `json:"cost"`
}

// piCost is the per-message cost breakdown pi reports.
type piCost struct {
	Total float64 `json:"total"`
}

// ParsePiStream reads a pi JSON event stream and returns the final result.
//
// The pi shape differs from claude. The first `session` line carries the session id;
// each assistant reply is a message_end event whose message carries a stopReason
// and a per-message cost. The run's cost is the sum of every assistant message's
// total, and the final outcome is decided by the last assistant message's
// stopReason. The parser ignores every other line and every line that is not
// JSON.
func ParsePiStream(r io.Reader) (Result, error) {
	sc := bufio.NewScanner(r)
	// A single stream line can be far larger than the 64 KiB default.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var sessionID string
	var costTotal float64
	var last *piMessage

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var e piEvent
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		switch e.Type {
		case "session":
			sessionID = e.ID
		case "message_end":
			if e.Message == nil || e.Message.Role != "assistant" {
				continue
			}
			costTotal += e.Message.Usage.Cost.Total
			copied := *e.Message
			last = &copied
		}
	}
	if err := sc.Err(); err != nil {
		return Result{}, fmt.Errorf("read stream: %w", err)
	}
	if last == nil {
		// As in ParseStream: the session header proves pi created the session,
		// so the id travels even though this run produced no message.
		return Result{SessionID: sessionID}, ErrNoResult
	}

	out := Result{
		SessionID:  sessionID,
		CostUSD:    costTotal,
		DurationMS: 0, // pi carries no wall-clock duration; Supervise measures it.
		IsError:    last.StopReason != "stop",
		Text:       piText(last.Content),
	}
	if last.StopReason == "error" {
		out.APIError = last.ErrorMessage
	} else if out.IsError {
		out.APIError = fmt.Sprintf("unexpected server stop reason %q", last.StopReason)
	}
	return out, nil
}

// piText joins the text blocks of an assistant message.
func piText(content []piContent) string {
	var parts []string
	for _, c := range content {
		if c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "")
}
