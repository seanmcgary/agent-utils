package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

func f64(v float64) *float64 { return &v }

// The incident: the agent re-added its own trigger, the run exited zero, and
// the loop resumed the session on every tick. The first such trigger parks.
func TestASelfRetriggeredIssueParksInsteadOfResuming(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{
		Issues:          map[int]store.IssueState{1: {Number: 1, SessionID: "s", SessionStarted: true}},
		SelfRetriggered: map[int]bool{1: true},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindParkNoProgress {
		t.Fatalf("decisions = %v, want one no-progress park", kinds(p))
	}
}

func TestASelfRetriggeredIssueWithNoSessionStillParks(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{SelfRetriggered: map[int]bool{1: true}}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindParkNoProgress {
		t.Fatalf("decisions = %v, want one no-progress park", kinds(p))
	}
}

// A park is not a dispatch, so a tripped breaker must not drop it.
func TestANoProgressParkSurvivesATrippedBreaker(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{
		issue(1, cfg.Labels.InFlight),
		issue(2, cfg.Labels.InFlight),
		issue(3, cfg.Labels.Trigger),
	}}
	st := State{
		Issues: map[int]store.IssueState{
			1: {Number: 1, NeedsRetry: true},
			2: {Number: 2, NeedsRetry: true},
		},
		SelfRetriggered: map[int]bool{3: true},
	}
	p := Decide(cfg, snap, st, time.Now())
	if !p.BreakerTripped {
		t.Fatal("breaker did not trip")
	}
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindParkNoProgress || p.Decisions[0].Issue != 3 {
		t.Fatalf("decisions = %v, want only the no-progress park of #3", kinds(p))
	}
}

func TestResumesUnderTheDefaultSessionCeiling(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{
		Issues:      map[int]store.IssueState{1: {Number: 1, SessionID: "s", SessionStarted: true}},
		SessionCost: map[int]float64{1: config.DefaultMaxSessionCostUSD - 0.01},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindResume {
		t.Fatalf("decisions = %v, want one resume", kinds(p))
	}
}

func TestParksAResumeAtTheDefaultSessionCeiling(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{
		Issues:      map[int]store.IssueState{1: {Number: 1, SessionID: "s", SessionStarted: true}},
		SessionCost: map[int]float64{1: config.DefaultMaxSessionCostUSD},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindParkSessionCost {
		t.Fatalf("decisions = %v, want one session-cost park", kinds(p))
	}
	d := p.Decisions[0]
	if d.SessionCost != config.DefaultMaxSessionCostUSD || d.SessionLimit != config.DefaultMaxSessionCostUSD {
		t.Errorf("cost/limit = %v/%v", d.SessionCost, d.SessionLimit)
	}
}

func TestAZeroSessionCeilingIsNoCeiling(t *testing.T) {
	cfg := testConfig()
	cfg.Agent.MaxSessionCostUSD = f64(0)
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{
		Issues:      map[int]store.IssueState{1: {Number: 1, SessionID: "s", SessionStarted: true}},
		SessionCost: map[int]float64{1: 10000},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindResume {
		t.Fatalf("decisions = %v, want one resume", kinds(p))
	}
}

// A new session has spent nothing, so a start is never held by the ceiling,
// whatever the issue's previous session cost.
func TestTheSessionCeilingNeverHoldsAStart(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{SessionCost: map[int]float64{1: 10000}}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindStart {
		t.Fatalf("decisions = %v, want one start", kinds(p))
	}
}

func TestABudgetLabelRaisesTheCeiling(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, "budget:500")}}
	st := State{
		Issues:      map[int]store.IssueState{1: {Number: 1, SessionID: "s", SessionStarted: true}},
		SessionCost: map[int]float64{1: 300},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindResume {
		t.Fatalf("decisions = %v, want one resume", kinds(p))
	}

	st.SessionCost[1] = 500
	p = Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindParkSessionCost {
		t.Fatalf("decisions = %v, want one session-cost park at the label's ceiling", kinds(p))
	}
	if !strings.Contains(p.Decisions[0].Reason, "budget: label") {
		t.Errorf("reason %q does not name the label as the ceiling's source", p.Decisions[0].Reason)
	}
}

func TestABudgetLabelCanLowerTheCeiling(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, "budget:20")}}
	st := State{
		Issues:      map[int]store.IssueState{1: {Number: 1, SessionID: "s", SessionStarted: true}},
		SessionCost: map[int]float64{1: 25},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindParkSessionCost {
		t.Fatalf("decisions = %v, want one session-cost park", kinds(p))
	}
}

func TestBudgetNoneRemovesTheCeiling(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, "budget:none")}}
	st := State{
		Issues:      map[int]store.IssueState{1: {Number: 1, SessionID: "s", SessionStarted: true}},
		SessionCost: map[int]float64{1: 10000},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindResume {
		t.Fatalf("decisions = %v, want one resume", kinds(p))
	}
}

func TestAnInvalidBudgetLabelStopsTheIssue(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, "budget:0")}}
	p := Decide(cfg, snap, State{}, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindStop {
		t.Fatalf("decisions = %v, want one stop", kinds(p))
	}
}

// A retry resumes the same session, so it spends from the same ceiling, and a
// park in its place must not count toward the breaker.
func TestARetryResumeParksAtTheSessionCeilingAndIsNotCounted(t *testing.T) {
	cfg := testConfig()
	cfg.Retry.Breaker.OrphanThreshold = 1
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	st := State{
		Issues: map[int]store.IssueState{
			1: {Number: 1, SessionID: "s", SessionStarted: true, NeedsRetry: true},
		},
		SessionCost: map[int]float64{1: 300},
	}
	p := Decide(cfg, snap, st, time.Now())
	if p.BreakerTripped {
		t.Fatal("a session-cost park tripped the breaker")
	}
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindParkSessionCost {
		t.Fatalf("decisions = %v, want one session-cost park", kinds(p))
	}
}
