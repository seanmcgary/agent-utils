package loopcmd

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/runner"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// legacyRun records a finished dispatch the way the binary before cost_source
// did: the stream's total_cost_usd as the cost, and no source. log is written
// to a file unless it is empty, in which case the row names a file that does
// not exist.
func legacyRun(t *testing.T, s *store.Store, dir, session, kind string, cost float64, log string) int64 {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("%s-%d.jsonl", session, len(mustDispatches(t, s, session))))
	if log != "" {
		if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id, err := s.CreateDispatch(store.Dispatch{
		Loop: "execution", Repo: "o/r", Number: 1, Kind: kind, SessionID: session, LogPath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDispatch(id, store.DispatchResult{Status: store.StatusSucceeded, CostUSD: cost}); err != nil {
		t.Fatal(err)
	}
	return id
}

func mustDispatches(t *testing.T, s *store.Store, session string) []store.Dispatch {
	t.Helper()
	ds, err := s.DispatchesBySession(session)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

// result renders a claude result line. A negative since omits the
// model_usage_since_last_result field, as claude versions before it did.
func result(total, since float64) string {
	line := fmt.Sprintf(`{"type":"result","session_id":"x","total_cost_usd":%v,`+
		`"modelUsage":{"m":{"costUSD":%v}}`, total, total)
	if since >= 0 {
		line += fmt.Sprintf(`,"model_usage_since_last_result":{"m":{"costUSD":%v}}`, since)
	}
	return `{"type":"system","subtype":"init","session_id":"x"}` + "\n" + line + "}\n"
}

func sessionCost(t *testing.T, s *store.Store, session string) (float64, []string) {
	t.Helper()
	var total float64
	var sources []string
	for _, d := range mustDispatches(t, s, session) {
		total += d.CostUSD
		sources = append([]string{d.CostSource}, sources...)
	}
	return total, sources
}

func TestSettleCostsCorrectsTheHistory(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := db.Project(testProject)
	dir := t.TempDir()

	// A claude that marks its running total: 0.32, then 0.04 and 0.04 more.
	legacyRun(t, s, dir, "marked", store.KindStart, 0.32, result(0.32, 0.32))
	legacyRun(t, s, dir, "marked", store.KindResume, 0.36, result(0.36, 0.04))
	legacyRun(t, s, dir, "marked", store.KindResume, 0.40, result(0.40, 0.04))

	// An older claude, without the marker, that reported each run's own
	// cost: kept as recorded, even where a run cost more than the one before.
	legacyRun(t, s, dir, "perrun", store.KindStart, 0.64, result(0.64, -1))
	legacyRun(t, s, dir, "perrun", store.KindResume, 6.66, result(6.66, -1))
	legacyRun(t, s, dir, "perrun", store.KindResume, 4.39, result(4.39, -1))

	// A run killed before its result line: no cost to correct.
	legacyRun(t, s, dir, "killed", store.KindStart, 0, `{"type":"system","subtype":"init"}`+"\n")

	// A pi run: its own summed costs, kept.
	legacyRun(t, s, dir, "pi", store.KindStart, 0.20,
		`{"type":"session","id":"p"}`+"\n"+`{"type":"message_end","message":{"role":"assistant","stopReason":"stop","usage":{"cost":{"total":0.2}}}}`+"\n")

	// A run whose log is gone: kept, and marked so it is not retried.
	legacyRun(t, s, dir, "gone", store.KindStart, 7, "")

	report, err := SettleCosts(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Settled) != 9 || report.Unverified() != 1 {
		t.Errorf("settled %d, unverified %d; want 9 and 1", len(report.Settled), report.Unverified())
	}

	for _, c := range []struct {
		session string
		want    float64
		sources string
	}{
		{"marked", 0.40, "run,run,run"},
		{"perrun", 11.69, "total,total,total"},
		{"killed", 0, "none"},
		{"pi", 0.20, "pi"},
		{"gone", 7, "unverified"},
	} {
		got, sources := sessionCost(t, s, c.session)
		if math.Abs(got-c.want) > 1e-9 || strings.Join(sources, ",") != c.sources {
			t.Errorf("%s: $%v %v, want $%v [%s]", c.session, got, sources, c.want, c.sources)
		}
	}

	// Settled rows are never revisited.
	again, err := SettleCosts(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Settled) != 0 {
		t.Errorf("a second pass settled %d rows, want 0", len(again.Settled))
	}
}

func TestSettleCostsDryRunWritesNothing(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := db.Project(testProject)
	dir := t.TempDir()
	legacyRun(t, s, dir, "marked", store.KindStart, 0.32, result(0.32, 0.32))
	legacyRun(t, s, dir, "marked", store.KindResume, 0.36, result(0.36, 0.04))

	report, err := SettleCosts(db, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Changed()) != 1 {
		t.Errorf("changed = %d, want 1", len(report.Changed()))
	}
	if got, sources := sessionCost(t, s, "marked"); math.Abs(got-0.68) > 1e-9 || strings.Join(sources, "") != "" {
		t.Errorf("dry run wrote: $%v %v", got, sources)
	}
	out := RenderSettleReport(report, true)
	if !strings.Contains(out, "Would settle 2 dispatches: 1 changed, $0.68 recorded before, $0.36 after.") {
		t.Errorf("report:\n%s", out)
	}
}

// A run written by this binary records its own source and is never handed to
// the reconcile pass.
func TestARunWithItsOwnCostSourceIsNotUnsettled(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := db.Project(testProject)
	id, err := s.CreateDispatch(store.Dispatch{Loop: "l", Repo: "o/r", Number: 1, Kind: store.KindResume, SessionID: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDispatch(id, store.DispatchResult{
		Status: store.StatusSucceeded, CostUSD: 0.04, CostSource: runner.CostSourceRun,
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.UnsettledCosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("unsettled = %d, want 0", len(rows))
	}
}
