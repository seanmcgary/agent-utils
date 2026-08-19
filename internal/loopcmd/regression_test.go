package loopcmd

import (
	"context"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/ghub"
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
// Original: still fires 1,2,3 then parks, with backoff [0,1,2].
func TestRetryLadderFiresThreeTimesThenParksOnce(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"in-flight"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.IsAlive = func(int, int64) bool { return false }

	_ = deps.Store.PutIssueState(store.IssueState{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, SessionID: "s",
		SessionStarted: true, UpdatedAt: time.Now(),
	})
	mkDead := func() {
		id, _ := deps.Store.CreateDispatch(store.Dispatch{
			Loop: cfg.Name, Repo: cfg.Repo, Number: 1, Kind: store.KindStart, SessionID: "s",
		})
		_ = deps.Store.SetDispatchProcess(id, 999999, time.Now().Add(-time.Hour))
		_ = deps.Store.MarkNeedsRetry(cfg.Name, cfg.Repo, 1)
	}
	mkDead()
	for i := 0; i < 8; i++ {
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
