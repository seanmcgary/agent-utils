package loopcmd

import (
	"context"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/runner"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// Regression: the strand bug.
// A failure recorded while the issue was NOT in flight used to set a flag that
// nothing could ever clear, so the issue left the loop permanently and no human
// action recovered it. An issue whose failure was recorded while it was NOT
// in flight must recover once the human re-applies the trigger label.
func TestStrandedFailureRecoversOnHumanRetrigger(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	// Agent finished, moved in-flight -> review, THEN the run recorded failed.
	_ = deps.Store.PutIssueState(store.IssueState{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, SessionID: "orig",
		SessionStarted: true, NeedsRetry: true, UpdatedAt: time.Now(),
	})

	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	st, _ := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if st[1].NeedsRetry {
		t.Fatalf("stale flag survived, issue is stranded: %+v", st[1])
	}

	// Human re-applies the trigger label. It must now resume the ORIGINAL session.
	gh.issues = []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}
	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Resumed != 1 {
		t.Fatalf("human re-trigger did not resume, sum=%+v", sum)
	}
	running, _ := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if len(running) != 1 || running[0].SessionID != "orig" {
		t.Fatalf("session not preserved: %+v", running)
	}

}

// Regression: a pre-agent failure.
// The spawn and worktree failure paths used to set the retry flag on an issue
// that by construction has no in-flight label, stranding it the same way.
// Original: must not strand the issue.
func TestSpawnFailureDoesNotStrandIssue(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.Spawn = func(string, int64, string, string, string) (int, error) {
		return 0, context.DeadlineExceeded
	}
	_, _ = Tick(context.Background(), cfg, deps)

	st, _ := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if st[1].NeedsRetry {
		t.Fatalf("spawn failure set the retry flag with no in-flight label: %+v", st[1])
	}

	// Spawn recovers; the issue must dispatch again.
	deps.Spawn = func(string, int64, string, string, string) (int, error) { spawned++; return 4242, nil }
	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	if spawned != 1 {
		t.Fatalf("issue did not recover after a transient spawn failure (spawned=%d)", spawned)
	}

}

// Regression: the retry ladder end to end across real ticks.
// Original: still fires 1,2,3 then parks, with backoff [0s, 15m, 30m].
//
// The clock is advanced an hour between ticks, past every entry of the ladder.
// Without that the wall-clock deadline defers the second retry and the ladder
// never reaches the cap inside the test.
func TestRetryLadderFiresThreeTimesThenParksOnce(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"in-flight"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.IsAlive = func(int, int64) bool { return false }

	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	deps.Now = func() time.Time { return now }

	_ = deps.Store.PutIssueState(store.IssueState{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, SessionID: "s",
		SessionStarted: true, UpdatedAt: time.Now(),
	})
	mkDead := func() {
		id, _ := deps.Store.CreateDispatch(store.Dispatch{
			Loop: cfg.Name, Repo: cfg.Repo, Number: 1, Kind: store.KindStart, SessionID: "s",
		})
		_ = deps.Store.SetDispatchProcess(id, 999999, time.Now().Add(-time.Hour))
		_ = deps.Store.MarkNeedsRetry(cfg.Name, cfg.Repo, 1, now, runner.RetryBackoff(cfg))
	}
	mkDead()
	for i := 0; i < 8; i++ {
		// Advance past the longest wait in the ladder BEFORE the tick decides.
		now = now.Add(time.Hour)
		before := spawned
		if _, err := Tick(context.Background(), cfg, deps); err != nil {
			t.Fatal(err)
		}
		st, _ := deps.Store.IssueStates(cfg.Name, cfg.Repo)
		t.Logf("tick %d: spawned=+%d retry=%d needs_retry=%v parked=%v comments=%d",
			i, spawned-before, st[1].RetryCount, st[1].NeedsRetry, st[1].Parked, len(gh.comments))
		if spawned > before {
			mkDead() // the redispatched agent dies too
		}
	}
	if spawned != 3 {
		t.Errorf("spawned=%d, want exactly 3 retries", spawned)
	}
	if len(gh.comments) != 1 {
		t.Errorf("comments=%d, want exactly 1 cap comment", len(gh.comments))
	}

}

// Regression: the two-writer bug that plan review caught and no unit test would
// have.
//
// MarkNeedsRetry is the only writer of a NON-ZERO retry_after; every other
// statement that touches the column only ever clears it (see store.go). If
// dispatch also stamped a deadline, every needs-retry transition would still
// run through MarkNeedsRetry and overwrite it, so the escalating list would
// collapse to its first entry forever -- and after the migration that entry is
// 0s, which means the backoff would silently be nothing at all.
//
// The whole sequence is driven here, because only the sequence catches it:
// record a failure, tick so the retry dispatches, record a second failure, and
// read the deadline back.
func TestRetryDeadlineEscalatesAcrossTheDispatch(t *testing.T) {
	cfg := tickConfig(t) // backoff 0s, 15m, 30m
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"in-flight"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	base := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	now := base
	deps.Now = func() time.Time { return now }

	if err := deps.Store.PutIssueState(store.IssueState{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, SessionID: "s",
		SessionStarted: true, UpdatedAt: base,
	}); err != nil {
		t.Fatal(err)
	}

	// First failure, at retry_count 0: the deadline is Backoff[0], which is 0s.
	if err := deps.Store.MarkNeedsRetry(
		cfg.Name, cfg.Repo, 1, now, runner.RetryBackoff(cfg)); err != nil {
		t.Fatal(err)
	}
	st, err := deps.Store.IssueState(cfg.Name, cfg.Repo, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !st.RetryAfter.Equal(base) {
		t.Fatalf("after the first failure RetryAfter = %v, want %v (Backoff[0])",
			st.RetryAfter, base)
	}

	// The tick dispatches the retry. It must not touch the deadline.
	//
	// It runs AFTER the deadline, not on it. Backoff[0] is 0s, so a dispatch
	// that stamped now + Backoff[state.RetryCount] before the increment would
	// land on the very instant the assertion below expects and hide itself.
	now = base.Add(5 * time.Minute)
	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	if spawned != 1 {
		t.Fatalf("spawned = %d, want 1 retry once the deadline has passed", spawned)
	}
	st, err = deps.Store.IssueState(cfg.Name, cfg.Repo, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !st.RetryAfter.Equal(base) {
		t.Errorf("the dispatch stamped RetryAfter = %v, want it untouched at %v; "+
			"MarkNeedsRetry is the only writer of a real deadline", st.RetryAfter, base)
	}
	if st.RetryCount != 1 {
		t.Fatalf("RetryCount = %d, want 1; it is what the next failure indexes with",
			st.RetryCount)
	}

	// Second failure, an hour later and at retry_count 1: the deadline is now
	// Backoff[1], measured from THIS failure.
	now = base.Add(time.Hour)
	if err := deps.Store.MarkNeedsRetry(
		cfg.Name, cfg.Repo, 1, now, runner.RetryBackoff(cfg)); err != nil {
		t.Fatal(err)
	}
	st, err = deps.Store.IssueState(cfg.Name, cfg.Repo, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := now.Add(15 * time.Minute)
	if !st.RetryAfter.Equal(want) {
		t.Errorf("after the second failure RetryAfter = %v, want %v: Backoff[1] "+
			"from the second failure, not Backoff[0] and not a dispatch stamp",
			st.RetryAfter, want)
	}
}
