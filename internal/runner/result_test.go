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
