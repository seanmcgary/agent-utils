package runner

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// claudeResult renders a result line. since is nil for a claude that does not
// write model_usage_since_last_result.
func claudeResult(total float64, mu, since map[string]ModelUsage) string {
	enc := func(m map[string]ModelUsage) string {
		var parts []string
		for k, v := range m {
			parts = append(parts, fmt.Sprintf(`%q:{"costUSD":%v}`, k, v.CostUSD))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	line := fmt.Sprintf(`{"type":"result","session_id":"s","total_cost_usd":%v,"modelUsage":%s`,
		total, enc(mu))
	if since != nil {
		line += `,"model_usage_since_last_result":` + enc(since)
	}
	return line + "}\n"
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func parse(t *testing.T, stream string) Result {
	t.Helper()
	r, err := ParseStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// claude 2.1.323, measured: a three-run resumed session. total_cost_usd and
// modelUsage are the session's running totals; model_usage_since_last_result
// is the run's own.
func TestParseStreamTakesTheRunCostFromModelUsageSinceLastResult(t *testing.T) {
	r := parse(t, claudeResult(0.00999487,
		map[string]ModelUsage{"haiku": {CostUSD: 0.00999487}},
		map[string]ModelUsage{"haiku": {CostUSD: 0.00468859}}))
	if r.CostSource != CostSourceRun || !near(r.CostUSD, 0.00468859) {
		t.Errorf("cost = %v (%q), want 0.00468859 (run)", r.CostUSD, r.CostSource)
	}
}

// Each result line's since-usage covers only the stretch since the one before,
// so a run that writes two is the sum of both.
func TestParseStreamSumsTheSinceCostOfEveryResultLine(t *testing.T) {
	mu := map[string]ModelUsage{"m": {CostUSD: 3}}
	stream := claudeResult(2, mu, map[string]ModelUsage{"m": {CostUSD: 0.5}}) +
		claudeResult(3, mu, map[string]ModelUsage{"m": {CostUSD: 0.25}, "sub": {CostUSD: 0.75}})
	if r := parse(t, stream); !near(r.CostUSD, 1.5) || r.CostSource != CostSourceRun {
		t.Errorf("cost = %v (%q), want 1.5 (run)", r.CostUSD, r.CostSource)
	}
}

// claude 2.1.274, from real transcripts: no since field, and total_cost_usd
// is the run's own cost.
func TestParseStreamTakesAnUnmarkedTotalAsTheRunCost(t *testing.T) {
	r := parse(t, claudeResult(3.38, map[string]ModelUsage{"m": {CostUSD: 3.38}}, nil))
	if r.CostSource != CostSourceTotal || !near(r.CostUSD, 3.38) {
		t.Errorf("cost = %v (%q), want 3.38 (total)", r.CostUSD, r.CostSource)
	}
}

func TestParsePiStreamMarksItsCostPerRun(t *testing.T) {
	stream := `{"type":"session","id":"p"}
{"type":"message_end","message":{"role":"assistant","stopReason":"stop","usage":{"cost":{"total":0.2}}}}
`
	r, err := ParsePiStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if r.CostSource != CostSourcePi {
		t.Errorf("CostSource = %q, want pi", r.CostSource)
	}
}
