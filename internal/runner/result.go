package runner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
