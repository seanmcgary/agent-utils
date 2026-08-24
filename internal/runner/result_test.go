package runner

import (
	"errors"
	"strings"
	"testing"
)

const streamFixture = `{"type":"system","subtype":"init","session_id":"abc"}
{"type":"assistant"}
{"type":"result","subtype":"success","session_id":"abc","total_cost_usd":0.0145,"duration_ms":2519,"is_error":false,"api_error_status":null,"result":"done"}
`

func TestParseStreamReadsResultLine(t *testing.T) {
	got, err := ParseStream(strings.NewReader(streamFixture))
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if got.SessionID != "abc" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if got.CostUSD != 0.0145 {
		t.Errorf("CostUSD = %v", got.CostUSD)
	}
	if got.DurationMS != 2519 {
		t.Errorf("DurationMS = %d", got.DurationMS)
	}
	if got.IsError {
		t.Error("IsError = true, want false")
	}
	if got.Text != "done" {
		t.Errorf("Text = %q", got.Text)
	}
}

func TestParseStreamCapturesAPIError(t *testing.T) {
	line := `{"type":"result","subtype":"error","is_error":true,"api_error_status":"529"}`
	got, err := ParseStream(strings.NewReader(line))
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsError || got.APIError != "529" {
		t.Errorf("got %+v", got)
	}
}

func TestParseStreamIgnoresNonJSONNoise(t *testing.T) {
	body := "warning: something\n" + streamFixture
	got, err := ParseStream(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if got.SessionID != "abc" {
		t.Errorf("noise broke the parse: %+v", got)
	}
}

func TestParseStreamErrorsWhenNoResultLine(t *testing.T) {
	if _, err := ParseStream(strings.NewReader(`{"type":"assistant"}`)); err == nil {
		t.Fatal("want an error when the stream holds no result line")
	}
}

func TestParseStreamHandlesVeryLongLines(t *testing.T) {
	// bufio.Scanner has a 64 KiB default limit. A real stream easily exceeds it,
	// so the parser must raise the buffer.
	big := strings.Repeat("x", 300000)
	body := `{"type":"assistant","text":"` + big + `"}` + "\n" + streamFixture
	got, err := ParseStream(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if got.SessionID != "abc" {
		t.Errorf("long line broke the parse: %+v", got)
	}
}

const piSuccess = `{"type":"session","id":"abc"}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stopReason":"stop","usage":{"cost":{"total":0.01}}}}
`

func TestParsePiStreamReadsSessionId(t *testing.T) {
	got, err := ParsePiStream(strings.NewReader(piSuccess))
	if err != nil {
		t.Fatalf("ParsePiStream: %v", err)
	}
	if got.SessionID != "abc" {
		t.Errorf("SessionID = %q, want abc", got.SessionID)
	}
	if got.IsError {
		t.Error("IsError = true, want false")
	}
}

func TestParsePiStreamSumsCost(t *testing.T) {
	body := `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"stop","usage":{"cost":{"total":0.01}}}}` + "\n" +
		`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"stop","usage":{"cost":{"total":0.02}}}}` + "\n"
	got, err := ParsePiStream(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if got.CostUSD != 0.03 {
		t.Errorf("CostUSD = %v, want 0.03", got.CostUSD)
	}
}

func TestParsePiStreamCapturesError(t *testing.T) {
	body := `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"rate limited 429"}}` + "\n"
	got, err := ParsePiStream(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsError || !strings.Contains(got.APIError, "429") {
		t.Errorf("got %+v", got)
	}
}

func TestParsePiStreamIgnoresNoise(t *testing.T) {
	body := "not-json\n{\n" + piSuccess
	got, err := ParsePiStream(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if got.IsError {
		t.Error("noise broke the parse")
	}
}

func TestParsePiStreamErrorsWhenNoMessage(t *testing.T) {
	body := `{"type":"session","id":"x"}` + "\n" +
		`{"type":"tool_execution_end","toolName":"bash"}` + "\n"
	if _, err := ParsePiStream(strings.NewReader(body)); err == nil {
		t.Fatal("want ErrNoResult when no assistant message appears")
	}
}

func TestParsePiStreamTreatsNonStopAsFailure(t *testing.T) {
	body := `{"type":"session","id":"x"}` + "\n" +
		`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"max_tokens","usage":{"cost":{"total":0.1}}}}` + "\n"
	got, err := ParsePiStream(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsError || !strings.Contains(got.APIError, "max_tokens") {
		t.Errorf("got %+v", got)
	}
}

func TestParsePiStreamIgnoresNonAssistant(t *testing.T) {
	body := `{"type":"session","id":"x"}` + "\n" +
		`{"type":"message_end","message":{"role":"user","content":[],"usage":{"cost":{"total":0.5}}}}` + "\n" +
		`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"stop","usage":{"cost":{"total":0.1}}}}` + "\n"
	got, err := ParsePiStream(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if got.CostUSD != 0.1 {
		t.Errorf("CostUSD = %v, want 0.1 (user message must not sum)", got.CostUSD)
	}
	if got.SessionID != "x" {
		t.Errorf("SessionID = %q, want x", got.SessionID)
	}
}

// A killed run writes no result line, but it DID create a session. The id is
// on the first line claude emits, and it must survive ErrNoResult.
//
// This is the koinos issue-73 wedge, reduced: the run was killed after 16
// minutes with 960KB of stream and no result line, so the caller recorded "no
// session started". Every later tick then dispatched a START against the id
// already on the issue row, claude refused it, and the dispatch failed at no
// cost -- which spent no retry budget, so the issue never parked and the loop
// could not leave that state without hand-editing the database.
func TestParseStreamKeepsTheSessionIDWhenTheRunWasKilled(t *testing.T) {
	stream := `{"type":"system","subtype":"hook_started","session_id":"b3b1a9e5-fe9a-4b69-b681-5ed247fe01ff"}
{"type":"assistant","session_id":"b3b1a9e5-fe9a-4b69-b681-5ed247fe01ff"}
{"type":"system","subtype":"task_progress","session_id":"b3b1a9e5-fe9a-4b69-b681-5ed247fe01ff"}`

	res, err := ParseStream(strings.NewReader(stream))

	if !errors.Is(err, ErrNoResult) {
		t.Fatalf("err = %v, want ErrNoResult", err)
	}
	if res.SessionID != "b3b1a9e5-fe9a-4b69-b681-5ed247fe01ff" {
		t.Errorf("SessionID = %q, want the id from the system event: a killed run "+
			"that loses its session id wedges the loop permanently", res.SessionID)
	}
}

// The ordinary path must not regress: a completed run still reports the id its
// result line carries.
func TestParseStreamStillReadsTheSessionIDFromTheResultLine(t *testing.T) {
	stream := `{"type":"system","subtype":"init","session_id":"abc"}
{"type":"result","subtype":"success","session_id":"abc","total_cost_usd":1.5,"is_error":false}`

	res, err := ParseStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if res.SessionID != "abc" {
		t.Errorf("SessionID = %q, want abc", res.SessionID)
	}
	if res.CostUSD != 1.5 {
		t.Errorf("CostUSD = %v, want 1.5", res.CostUSD)
	}
}

// pi announces its session in a header event and can be killed just the same.
func TestParsePiStreamKeepsTheSessionIDWhenTheRunWasKilled(t *testing.T) {
	stream := `{"type":"session","version":3,"id":"11111111-2222-3333-4444-555555555555"}
{"type":"tool_execution_start"}`

	res, err := ParsePiStream(strings.NewReader(stream))

	if !errors.Is(err, ErrNoResult) {
		t.Fatalf("err = %v, want ErrNoResult", err)
	}
	if res.SessionID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("SessionID = %q, want the id from the session header", res.SessionID)
	}
}
