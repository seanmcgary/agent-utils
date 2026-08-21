package runner

import (
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
