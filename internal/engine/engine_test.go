package engine

import (
	"strings"
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
			Max: 3,
			// 0s, 15m, 30m. Wall-clock waits, one per retry.
			Backoff: []config.Duration{
				0,
				config.Duration(15 * time.Minute),
				config.Duration(30 * time.Minute),
			},
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
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindRetryStart {
		t.Fatalf("decisions = %v, want one retry_start", kinds(p))
	}
}

// The reference loops define an orphan as carrying the in-flight label. An agent
// that finished its work and moved the label on must not be woken by a RETRY --
// but the stale failure flag must be CLEARED, not left set.
//
// Regression test. Leaving the flag set stranded the issue permanently: every
// later tick took the failure branch, never reached the trigger check, and
// re-applying the trigger label did nothing at all.
func TestFailedIssueWithoutInFlightLabelIsCleared(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Blocked)}}
	st := State{
		Issues: map[int]store.IssueState{1: {Number: 1, NeedsRetry: true}},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindClearRetry {
		t.Fatalf("decisions = %v, want one clear_retry", kinds(p))
	}
	// It must NOT be a dispatch of any kind.
	for _, d := range p.Decisions {
		if d.Kind == KindRetryStart || d.Kind == KindRetryResume {
			t.Errorf("issue not in flight must never be retried, got %v", d.Kind)
		}
	}
}

// A retry whose previous attempt never created a session must carry NO session
// identifier, so dispatch mints a fresh one. claude refuses a reused
// --session-id outright ("Session ID <uuid> is already in use"), so passing the
// old one made every retry fail in under a second and then park the issue.
func TestRetryStartCarriesNoSessionID(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	st := State{
		Issues: map[int]store.IssueState{
			1: {Number: 1, SessionID: "burned-uuid", SessionStarted: false, NeedsRetry: true},
		},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindRetryStart {
		t.Fatalf("decisions = %v, want one retry_start", kinds(p))
	}
	if p.Decisions[0].SessionID != "" {
		t.Errorf("SessionID = %q, want empty so a fresh one is minted",
			p.Decisions[0].SessionID)
	}
}

// A live issue agent must suppress tending of its own branch. The agent flips
// its own labels asynchronously, so an issue can still carry the review label
// while its dispatch is live -- and a tend agent force-pushes the branch the
// issue agent is committing to.
func TestLiveIssueDispatchSuppressesTend(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Labels.Review)},
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1", HeadRef: "feat/a", BaseRef: "master", Trusted: true}},
		BehindBy: map[int]int{20: 4},
	}
	st := State{
		Issues:  map[int]store.IssueState{},
		Running: []store.Dispatch{{Number: 1, Kind: store.KindStart, Status: store.StatusRunning}},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none while the issue agent is live", kinds(p))
	}
}

// This is the regression test for the strand bug: a deferred retry must still be
// pending on the NEXT tick, because NeedsRetry is durable state rather than a
// dispatch row the reconcile pass consumes.
func TestBackoffDefersButDoesNotLoseTheRetry(t *testing.T) {
	cfg := testConfig()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	st := State{
		Issues: map[int]store.IssueState{
			1: {Number: 1, RetryCount: 2, NeedsRetry: true,
				SessionID: "s", SessionStarted: true,
				RetryAfter: now.Add(30 * time.Minute)},
		},
	}
	// The deadline is wall-clock now, so a tick before it defers however many
	// ticks have run in between.
	for _, at := range []time.Time{now, now.Add(29 * time.Minute)} {
		if p := Decide(cfg, snap, st, at); len(p.Decisions) != 0 {
			t.Fatalf("at %v: decisions = %v, want none inside backoff", at, kinds(p))
		}
	}
	p := Decide(cfg, snap, st, now.Add(30*time.Minute))
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindRetryResume {
		t.Fatalf("at the deadline: decisions = %v, want the retry to fire", kinds(p))
	}
}

// The same issue, the same state, one field apart: the decision is the clock
// against the stored deadline and nothing else.
func TestRetryAfterGatesTheRetryOnTheClock(t *testing.T) {
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		after time.Time
		want  int
	}{
		{"deadline in the future", now.Add(time.Minute), 0},
		{"deadline in the past", now.Add(-time.Minute), 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := testConfig()
			snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
			st := State{Issues: map[int]store.IssueState{
				1: {Number: 1, NeedsRetry: true, SessionID: "s", SessionStarted: true,
					RetryAfter: c.after},
			}}
			p := Decide(cfg, snap, st, now)
			if len(p.Decisions) != c.want {
				t.Fatalf("decisions = %v, want %d", kinds(p), c.want)
			}
			if c.want == 1 && p.Decisions[0].Kind != KindRetryResume {
				t.Errorf("kind = %v, want retry_resume", p.Decisions[0].Kind)
			}
		})
	}
}

// A zero deadline means "no deadline". retry.max may be 0, so retry.backoff may
// be absent entirely, and a row imported from a per-loop database carries no
// deadline at all; neither may strand the issue.
func TestZeroRetryAfterRetriesAtOnce(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	st := State{Issues: map[int]store.IssueState{
		1: {Number: 1, NeedsRetry: true, SessionID: "s", SessionStarted: true},
	}}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindRetryResume {
		t.Fatalf("decisions = %v, want the retry to fire with no deadline", kinds(p))
	}
}

// The cap is checked before the deadline. A future deadline must not postpone a
// park, or an issue past its budget sits in the loop carrying an in-flight label
// no agent owns.
func TestRetryCapParksEvenInsideTheBackoffWindow(t *testing.T) {
	cfg := testConfig()
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	st := State{Issues: map[int]store.IssueState{
		1: {Number: 1, RetryCount: 3, NeedsRetry: true,
			RetryAfter: now.Add(time.Hour)},
	}}
	p := Decide(cfg, snap, st, now)
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindParkRetryExhausted {
		t.Fatalf("decisions = %v, want one park", kinds(p))
	}
}

func TestParksAtRetryCap(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	st := State{
		Issues: map[int]store.IssueState{
			1: {Number: 1, RetryCount: 3, NeedsRetry: true},
		},
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
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1", Trusted: true}},
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
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1", Trusted: true}},
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
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1", Trusted: true}},
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

func TestTendResumesTheIssuesSession(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Labels.Review)},
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1", HeadRef: "feat/a", BaseRef: "master", Trusted: true}},
		BehindBy: map[int]int{20: 4},
	}
	st := State{Issues: map[int]store.IssueState{
		1: {SessionID: "sess-1", SessionStarted: true},
	}}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindTend {
		t.Fatalf("decisions = %v, want one tend", kinds(p))
	}
	if got := p.Decisions[0].SessionID; got != "sess-1" {
		t.Errorf("session = %q, want the issue's session %q", got, "sess-1")
	}
}

func TestTendStartsAFreshSessionWhenNoneWasStarted(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Labels.Review)},
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1", HeadRef: "feat/a", BaseRef: "master", Trusted: true}},
		BehindBy: map[int]int{20: 4},
	}
	// The identifier exists but claude never created the session, so "-r" would
	// fail every time. This is the same rule retryDecision applies.
	st := State{Issues: map[int]store.IssueState{
		1: {SessionID: "sess-1", SessionStarted: false},
	}}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindTend {
		t.Fatalf("decisions = %v, want one tend", kinds(p))
	}
	if got := p.Decisions[0].SessionID; got != "" {
		t.Errorf("session = %q, want empty so dispatch mints a fresh one", got)
	}
}

func TestLiveTendHoldingTheIssueSessionSuppressesResume(t *testing.T) {
	cfg := testConfig()
	// Both labels are present: a human re-applied the trigger while the pull
	// request was still awaiting review. Without the guard this resumes the
	// very session the live tend is already using.
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Labels.Review, cfg.Labels.Trigger)},
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1", Trusted: true}},
		BehindBy: map[int]int{20: 4},
	}
	st := State{
		Issues:  map[int]store.IssueState{1: {SessionID: "sess-1", SessionStarted: true}},
		Running: []store.Dispatch{{Number: 1, PRNumber: 20, Kind: store.KindTend, SessionID: "sess-1"}},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none while a tend holds the issue's session", kinds(p))
	}
}

func TestLiveTendOnItsOwnSessionDoesNotSuppressResume(t *testing.T) {
	cfg := testConfig()
	// A tend that minted its own session -- a row written before tend inherited
	// the issue's, or one dispatched when no session had started -- shares
	// nothing with the issue, so it must not block the issue's own work.
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Labels.Review, cfg.Labels.Trigger)},
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1", Trusted: true}},
		BehindBy: map[int]int{20: 4},
	}
	st := State{
		Issues:  map[int]store.IssueState{1: {SessionID: "sess-1", SessionStarted: true}},
		Running: []store.Dispatch{{Number: 1, PRNumber: 20, Kind: store.KindTend, SessionID: "throwaway"}},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindResume {
		t.Fatalf("decisions = %v, want one resume", kinds(p))
	}
}

// --- stopped issues and label overrides ---

func TestStoppedIssueProducesNoDecision(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{Issues: map[int]store.IssueState{
		1: {Number: 1, Stopped: true, StoppedReason: "operator killed the session"},
	}}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none for a stopped issue", kinds(p))
	}
	reason := p.NoDecisionReason(1)
	if !strings.Contains(reason, "operator killed the session") {
		t.Errorf("NoDecisionReason = %q, want it to carry the stopped reason", reason)
	}
}

// C5: an empty StoppedReason (a hand-edited database, or a row predating
// this field) must not render as a sentence starting with a semicolon.
func TestStoppedIssueWithNoReasonDoesNotRenderALeadingSemicolon(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{Issues: map[int]store.IssueState{
		1: {Number: 1, Stopped: true, StoppedReason: ""},
	}}
	p := Decide(cfg, snap, st, time.Now())
	reason := p.NoDecisionReason(1)
	if strings.HasPrefix(strings.TrimSpace(reason), ";") {
		t.Errorf("NoDecisionReason = %q, want no leading semicolon for an empty StoppedReason", reason)
	}
	if !strings.Contains(reason, "sessions resume") {
		t.Errorf("NoDecisionReason = %q, want it to still name the resume command", reason)
	}
}

// tendDecisions skips only issues marked decided (engine.go:259). Without
// the stopped branch setting decided, a tend agent would force-push the
// branch of the session the operator just killed.
func TestStoppedIssueAwaitingReviewProducesNoTend(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Labels.Review)},
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1", HeadRef: "feat/a", BaseRef: "master", Trusted: true}},
		BehindBy: map[int]int{20: 4},
	}
	st := State{Issues: map[int]store.IssueState{
		1: {Number: 1, Stopped: true, StoppedReason: "operator killed the session"},
	}}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none: a stopped issue must not be tended", kinds(p))
	}
}

// A killed dispatch always records a failure, so a stopped issue almost
// always also carries NeedsRetry. The stop check must sit above the retry
// path, or the loop would redispatch the issue it was told to stop.
func TestStoppedIssueWithNeedsRetryProducesNothing(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	st := State{Issues: map[int]store.IssueState{
		1: {Number: 1, Stopped: true, StoppedReason: "operator killed the session",
			NeedsRetry: true, SessionID: "s", SessionStarted: true},
	}}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none: stop must win over retry", kinds(p))
	}
}

func TestOverrideReachesStartDecision(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, "model:claude-opus-5")}}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindStart {
		t.Fatalf("decisions = %v, want one start", kinds(p))
	}
	if p.Decisions[0].Overrides.Model != "claude-opus-5" {
		t.Errorf("Overrides.Model = %q, want claude-opus-5", p.Decisions[0].Overrides.Model)
	}
}

// retryDecision receives no labels (engine.go:199), so this is the test that
// catches every retry silently reverting to the configured model.
func TestOverrideReachesRetryDecision(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight, "model:claude-opus-5")}}
	st := State{Issues: map[int]store.IssueState{
		1: {Number: 1, NeedsRetry: true, SessionID: "s", SessionStarted: true},
	}}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindRetryResume {
		t.Fatalf("decisions = %v, want one retry_resume", kinds(p))
	}
	if p.Decisions[0].Overrides.Model != "claude-opus-5" {
		t.Errorf("Overrides.Model = %q, want claude-opus-5", p.Decisions[0].Overrides.Model)
	}
}

func TestInvalidHarnessLabelStopsTheIssue(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, "harness:gpt")}}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindStop {
		t.Fatalf("decisions = %v, want one stop", kinds(p))
	}
	if p.Decisions[0].Reason == "" {
		t.Error("Reason must carry the parse error")
	}
}

// A claude-only setting must never refuse a harness: override. pi has no
// permission model and no cost ceiling, so PiBuildArgs emits neither; the
// override IGNORES them and the issue dispatches.
func TestHarnessOverrideIgnoresTheClaudeOnlySettings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*config.Config)
	}{
		{"permission_mode", func(c *config.Config) { c.Agent.PermissionMode = "plan" }},
		{"max_budget_usd", func(c *config.Config) { c.Agent.MaxBudgetUSD = 5 }},
		{"both", func(c *config.Config) {
			c.Agent.PermissionMode = "plan"
			c.Agent.MaxBudgetUSD = 5
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			tc.apply(cfg)
			snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, "harness:pi")}}
			p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
			if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindStart {
				t.Fatalf("decisions = %v, want one start: pi ignores what it does not implement", kinds(p))
			}
			if p.Decisions[0].Overrides.Harness != config.HarnessPi {
				t.Errorf("Overrides.Harness = %q, want pi", p.Decisions[0].Overrides.Harness)
			}
		})
	}
}

func TestHarnessOverrideAllowedWithNeitherSafetySetting(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, "harness:pi")}}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindStart {
		t.Fatalf("decisions = %v, want one start", kinds(p))
	}
	if p.Decisions[0].Overrides.Harness != "pi" {
		t.Errorf("Overrides.Harness = %q, want pi", p.Decisions[0].Overrides.Harness)
	}
}

// TestHarnessOverrideToClaudeIsNeverRefusedOnAPiLoop is spec B6: the hazard
// is directional. A pi-configured loop with agent.max_budget_usd set is a
// legal configuration -- config.validate accepts it, PiBuildArgs silently
// ignores it -- and switching TO claude only ever ADDS the ceiling
// BuildArgs enforces, never drops one. The old rule keyed off "the harness
// changed" rather than "the effective harness would be pi", so it wrongly
// refused this exact case with a message claiming the override "would drop
// the ceiling" when the opposite is true.
func TestHarnessOverrideToClaudeIsNeverRefusedOnAPiLoop(t *testing.T) {
	cfg := testConfig()
	cfg.Agent.Harness = config.HarnessPi
	cfg.Agent.MaxBudgetUSD = 5
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, "harness:claude")}}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindStart {
		t.Fatalf("decisions = %v, want one start: switching to claude never drops a bound", kinds(p))
	}
	if p.Decisions[0].Overrides.Harness != "claude" {
		t.Errorf("Overrides.Harness = %q, want claude", p.Decisions[0].Overrides.Harness)
	}
}

// The KindStop reason is stored verbatim as StoppedReason, and
// stoppedSkipReason appends the resume hint every time that row is read
// back. A reason that carried the hint itself rendered it twice.
func TestStopReasonDoesNotCarryTheResumeHint(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, "harness:gpt")}}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindStop {
		t.Fatalf("decisions = %v, want one stop", kinds(p))
	}
	if strings.Contains(p.Decisions[0].Reason, "sessions resume") {
		t.Errorf("Reason = %q, want the bare cause: stoppedSkipReason adds the hint",
			p.Decisions[0].Reason)
	}
	if got := stoppedSkipReason(p.Decisions[0].Reason); strings.Count(got, "sessions resume") != 1 {
		t.Errorf("rendered = %q, want the hint exactly once", got)
	}
}

func TestInvalidLabelWithNoTriggerProducesNothing(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, "harness:gpt")}}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none: no trigger label means nothing to stop", kinds(p))
	}
}

// KindClearRetry is the only thing that retires an unreachable retry flag;
// blocking it on a bad label strands the issue forever.
func TestInvalidLabelDoesNotBlockClearRetry(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, "harness:gpt")}}
	st := State{Issues: map[int]store.IssueState{
		1: {Number: 1, NeedsRetry: true},
	}}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindClearRetry {
		t.Fatalf("decisions = %v, want one clear_retry", kinds(p))
	}
}

// The retry cap is a fact about the issue, not its labels, so an invalid
// label must not block the park.
func TestInvalidLabelDoesNotBlockParkAtRetryCap(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight, "harness:gpt")}}
	st := State{Issues: map[int]store.IssueState{
		1: {Number: 1, RetryCount: 3, NeedsRetry: true},
	}}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindParkRetryExhausted {
		t.Fatalf("decisions = %v, want one park", kinds(p))
	}
}

func TestStopSurvivesTrippedBreakerWithReasonIntact(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{
		issue(1, cfg.Labels.InFlight),
		issue(2, cfg.Labels.InFlight),
		issue(3, cfg.Labels.Trigger, "harness:gpt"),
	}}
	st := State{Issues: map[int]store.IssueState{
		1: {Number: 1, NeedsRetry: true},
		2: {Number: 2, NeedsRetry: true},
	}}
	p := Decide(cfg, snap, st, time.Now())
	if !p.BreakerTripped {
		t.Fatal("BreakerTripped = false, want true with two eligible retries")
	}
	var stop *Decision
	for i := range p.Decisions {
		if p.Decisions[i].Kind == KindStop {
			stop = &p.Decisions[i]
		}
	}
	if stop == nil {
		t.Fatalf("decisions = %v, want the stop to survive the breaker", kinds(p))
	}
	if stop.Reason == "" {
		t.Error("Reason must survive the breaker intact")
	}
}

// A retry converted to a stop by an invalid label must not count toward the
// breaker threshold -- otherwise a label could trip the breaker and drop
// every other issue's dispatches for the whole cooldown.
func TestStoppedRetryDoesNotTripTheBreaker(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{
		issue(1, cfg.Labels.InFlight, "harness:gpt"),
		issue(2, cfg.Labels.InFlight, "harness:gpt"),
	}}
	st := State{Issues: map[int]store.IssueState{
		1: {Number: 1, NeedsRetry: true},
		2: {Number: 2, NeedsRetry: true},
	}}
	p := Decide(cfg, snap, st, time.Now())
	if p.BreakerTripped {
		t.Fatal("BreakerTripped = true, want false: a stopped retry must not count")
	}
	if len(p.Decisions) != 2 {
		t.Fatalf("decisions = %v, want two stops", kinds(p))
	}
	for _, d := range p.Decisions {
		if d.Kind != KindStop {
			t.Errorf("kind = %v, want stop", d.Kind)
		}
	}
}
