package engine

import (
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

func testConfig() *config.Config {
	return &config.Config{
		Name: "planning",
		Repo: "o/r",
		Labels: config.Labels{
			Trigger:  "status:ready-for-spec",
			InFlight: "status:speccing",
			Blocked:  "status:needs-spec-input",
			Review:   "status:plan-ready-for-review",
			Terminal: "status:ready-for-execution",
			Veto:     []string{"blocked:design"},
		},
		Agent:  config.Agent{Model: "opus", Worktree: config.WorktreePerIssue},
		TendPR: true,
		Retry: config.Retry{
			Max:          3,
			BackoffTicks: []int{0, 1, 2},
			Breaker: config.Breaker{
				OrphanThreshold: 2,
				Cooldown:        config.Duration(30 * time.Minute),
			},
		},
	}
}

func issue(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, Labels: labels}
}

func kinds(p Plan) []Kind {
	out := make([]Kind, 0, len(p.Decisions))
	for _, d := range p.Decisions {
		out = append(out, d.Kind)
	}
	return out
}

func TestStartsNewIssue(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{Issues: map[int]store.IssueState{}}

	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindStart {
		t.Fatalf("decisions = %v, want one start", kinds(p))
	}
	if p.Decisions[0].Issue != 1 {
		t.Errorf("Issue = %d, want 1", p.Decisions[0].Issue)
	}
}

func TestResumesIssueWithStoredSession(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, cfg.Labels.Blocked)}}
	st := State{Issues: map[int]store.IssueState{
		1: {Number: 1, SessionID: "sess-1", SessionStarted: true},
	}}

	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindResume {
		t.Fatalf("decisions = %v, want one resume", kinds(p))
	}
	if p.Decisions[0].SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", p.Decisions[0].SessionID)
	}
}

func TestVetoLabelSkipsEvenWithTrigger(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, "blocked:design")}}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none", kinds(p))
	}
}

func TestNoTriggerLabelIsSkipped(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, "some:other-label")}}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none", kinds(p))
	}
}

// A live dispatch is the guard against double dispatch, not the label. An agent
// that has not yet flipped trigger -> in_flight must not be dispatched twice.
func TestVetoSupportsPrefixWildcard(t *testing.T) {
	cfg := testConfig()
	cfg.Labels.Veto = []string{"blocked:*"}
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, "blocked:legal")}}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none; blocked:* must match blocked:legal", kinds(p))
	}
}

func TestLiveDispatchBlocksSecondDispatch(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{
		Issues:  map[int]store.IssueState{1: {Number: 1, SessionID: "sess-1", SessionStarted: true}},
		Running: []store.Dispatch{{Number: 1, Kind: store.KindStart, Status: store.StatusRunning}},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none while a dispatch is live", kinds(p))
	}
}

func TestHealthyInFlightIssueProducesNothing(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	st := State{
		Issues:  map[int]store.IssueState{1: {Number: 1, SessionID: "s"}},
		Running: []store.Dispatch{{Number: 1, Status: store.StatusRunning}},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none", kinds(p))
	}
}

func TestFailedIssueRetriesImmediatelyOnFirstAttempt(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	st := State{
		Issues: map[int]store.IssueState{
			1: {Number: 1, SessionID: "s", SessionStarted: true, NeedsRetry: true},
		},
		TickCount: 5,
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindRetryResume {
		t.Fatalf("decisions = %v, want one retry_resume", kinds(p))
	}
}

// A dispatch that died before claude created the session must retry as a START.
// Resuming a session that was never created fails identically every time.
func TestRetryStartsWhenSessionWasNeverCreated(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	st := State{
		Issues: map[int]store.IssueState{
			1: {Number: 1, SessionID: "s", SessionStarted: false, NeedsRetry: true},
		},
		TickCount: 5,
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindRetryStart {
		t.Fatalf("decisions = %v, want one retry_start", kinds(p))
	}
}

// The reference loops define an orphan as carrying the in-flight label. An agent
// that finished its work and moved the label on must not be woken by a retry.
func TestFailedIssueWithoutInFlightLabelIsLeftAlone(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Blocked)}}
	st := State{
		Issues:    map[int]store.IssueState{1: {Number: 1, NeedsRetry: true}},
		TickCount: 5,
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none", kinds(p))
	}
}

// This is the regression test for the strand bug: a deferred retry must still be
// pending on the NEXT tick, because NeedsRetry is durable state rather than a
// dispatch row the reconcile pass consumes.
func TestBackoffDefersButDoesNotLoseTheRetry(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	st := State{
		Issues: map[int]store.IssueState{
			1: {Number: 1, RetryCount: 2, LastRetryTick: 5, NeedsRetry: true,
				SessionID: "s", SessionStarted: true},
		},
		TickCount: 5,
	}
	// backoff_ticks[2] == 2, so tick 5 and tick 6 defer.
	for _, tick := range []int64{5, 6} {
		st.TickCount = tick
		if p := Decide(cfg, snap, st, time.Now()); len(p.Decisions) != 0 {
			t.Fatalf("tick %d: decisions = %v, want none inside backoff", tick, kinds(p))
		}
	}
	st.TickCount = 7
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindRetryResume {
		t.Fatalf("tick 7: decisions = %v, want the retry to fire", kinds(p))
	}
}

func TestParksAtRetryCap(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	st := State{
		Issues: map[int]store.IssueState{
			1: {Number: 1, RetryCount: 3, LastRetryTick: 1, NeedsRetry: true},
		},
		TickCount: 99,
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindParkRetryExhausted {
		t.Fatalf("decisions = %v, want one park", kinds(p))
	}
}

// A parked issue must stay quiet. parkRetryExhausted removes the trigger label,
// so the issue carries only the blocked label and nothing picks it up.
func TestParkedIssueIsNotResumed(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Blocked)}}
	st := State{Issues: map[int]store.IssueState{
		1: {Number: 1, SessionID: "s", SessionStarted: true, Parked: true},
	}}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none for a parked issue", kinds(p))
	}
}

// A human re-applying the trigger label un-parks the issue and resumes its
// original session. This is the operator's only way out of a park.
func TestHumanRetriggerUnparks(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, cfg.Labels.Blocked)}}
	st := State{Issues: map[int]store.IssueState{
		1: {Number: 1, SessionID: "s", SessionStarted: true, Parked: true},
	}}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindResume {
		t.Fatalf("decisions = %v, want one resume", kinds(p))
	}
	if p.Decisions[0].SessionID != "s" {
		t.Errorf("SessionID = %q, want the original session s", p.Decisions[0].SessionID)
	}
}

func TestCircuitBreakerDropsDispatchesButKeepsParks(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{
		issue(1, cfg.Labels.InFlight),
		issue(2, cfg.Labels.InFlight),
		issue(3, cfg.Labels.Trigger),
		issue(4, cfg.Labels.InFlight),
	}}
	st := State{
		Issues: map[int]store.IssueState{
			1: {Number: 1, NeedsRetry: true},
			2: {Number: 2, NeedsRetry: true},
			4: {Number: 4, NeedsRetry: true, RetryCount: 3},
		},
		TickCount: 10,
	}
	p := Decide(cfg, snap, st, time.Now())
	if !p.BreakerTripped {
		t.Fatal("BreakerTripped = false, want true with two eligible retries")
	}
	// Issue 3's start and issues 1 and 2's retries are dropped. Issue 4's park
	// survives: the reference loop still posts a cap-reached comment that is due.
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindParkRetryExhausted {
		t.Errorf("decisions = %v, want only the park", kinds(p))
	}
	if p.CooldownUntil.IsZero() {
		t.Error("CooldownUntil must be set when the breaker trips")
	}
}

func TestCooldownSuppressesEverything(t *testing.T) {
	cfg := testConfig()
	now := time.Now()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{
		Issues:        map[int]store.IssueState{},
		CooldownUntil: now.Add(10 * time.Minute),
	}
	p := Decide(cfg, snap, st, now)
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none during cooldown", kinds(p))
	}
}

func TestTendsStalePullRequest(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Labels.Review)},
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1", HeadRef: "feat/a", BaseRef: "master", Trusted: true}},
		BehindBy: map[int]int{20: 4},
	}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindTend {
		t.Fatalf("decisions = %v, want one tend", kinds(p))
	}
	d := p.Decisions[0]
	if d.PR != 20 || d.HeadRef != "feat/a" || d.BaseRef != "master" {
		t.Errorf("decision = %+v", d)
	}
}

func TestDoesNotTendCurrentPullRequest(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Labels.Review)},
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1"}},
		BehindBy: map[int]int{20: 0},
	}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none for a current pull request", kinds(p))
	}
}

func TestDoesNotTendWhenTendIsDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.TendPR = false
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Labels.Review)},
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1"}},
		BehindBy: map[int]int{20: 9},
	}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none when tend_pr is false", kinds(p))
	}
}

func TestDoesNotTendWhileTendDispatchIsLive(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Labels.Review)},
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1"}},
		BehindBy: map[int]int{20: 4},
	}
	st := State{
		Issues:  map[int]store.IssueState{},
		Running: []store.Dispatch{{Number: 1, PRNumber: 20, Kind: store.KindTend}},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none while a tend dispatch is live", kinds(p))
	}
}

func TestDecisionsAreOrderedByIssueNumber(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{
		issue(9, cfg.Labels.Trigger),
		issue(2, cfg.Labels.Trigger),
		issue(5, cfg.Labels.Trigger),
	}}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 3 {
		t.Fatalf("len = %d, want 3", len(p.Decisions))
	}
	want := []int{2, 5, 9}
	for i, d := range p.Decisions {
		if d.Issue != want[i] {
			t.Errorf("position %d = issue %d, want %d", i, d.Issue, want[i])
		}
	}
}
