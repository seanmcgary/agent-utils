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
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rl resultLine
		if err := json.Unmarshal(line, &rl); err != nil {
			continue
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
		return Result{}, ErrNoResult
	}

	out := Result{
		SessionID:  last.SessionID,
		CostUSD:    last.TotalCostUSD,
		DurationMS: last.DurationMS,
		IsError:    last.IsError,
		Text:       last.ResultText,
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
		return Result{}, ErrNoResult
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
