package loopcmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// A crash leaves the row marked running with its process gone. Nothing else
// finds it: a retry deadline is what the daemon's scheduler wakes on, and only
// a reap writes one. So this is the whole of crash recovery -- everything
// after it is the machinery that already exists.
func TestReapOrphansRetiresADeadRunnerAndQueuesTheRetry(t *testing.T) {
	cfg, deps, _ := reapFixture(t, store.KindStart, 1, 0)

	sum, err := ReapOrphans(cfg, deps)
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if sum.Orphans != 1 {
		t.Fatalf("Orphans = %d, want 1", sum.Orphans)
	}
	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 0 {
		t.Errorf("running = %d, want the dead row retired", len(running))
	}
	states, err := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if !states[1].NeedsRetry {
		t.Error("no retry was queued, so nothing will ever wake this issue")
	}
	if states[1].RetryAfter.IsZero() {
		t.Error("no retry DEADLINE was stamped; the daemon wakes on deadlines alone")
	}
}

// It must never dispatch. Recovery's whole job is to make the orphan visible
// to the scheduler, which then applies the backoff, the cooldown and the
// retry cap that already live in one place. Dispatching here would put a
// second copy of those rules in the recovery path.
func TestReapOrphansNeverDispatches(t *testing.T) {
	cfg, deps, _ := reapFixture(t, store.KindStart, 1, 0)
	spawned := 0
	deps.Spawn = func(string, int64, string, string, string) (int, error) {
		spawned++
		return 1, nil
	}

	if _, err := ReapOrphans(cfg, deps); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if spawned != 0 {
		t.Errorf("spawned = %d, want 0: recovery stamps a deadline and stops", spawned)
	}
}

// A live agent's row is left exactly as it is. This is the guard that makes
// the sweep safe to run on a schedule rather than only after a crash.
func TestReapOrphansLeavesALiveAgentAlone(t *testing.T) {
	cfg, deps, _ := reapFixture(t, store.KindStart, 1, 0)
	deps.IsAlive = func(int, int64) bool { return true }

	sum, err := ReapOrphans(cfg, deps)
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if sum.Orphans != 0 {
		t.Errorf("Orphans = %d, want 0", sum.Orphans)
	}
	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 {
		t.Errorf("running = %d, want the live row untouched", len(running))
	}
}

// A held lock means a tick is running, and every tick reaps as part of its own
// pass. So there is nothing to do and nothing to report: returning ErrHeld
// lets the caller skip this loop and come back on the next sweep.
func TestReapOrphansYieldsToARunningTick(t *testing.T) {
	cfg, deps, _ := reapFixture(t, store.KindStart, 1, 0)
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	if _, err := ReapOrphans(cfg, deps); !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("err = %v, want lock.ErrHeld", err)
	}
	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 {
		t.Errorf("running = %d, want the row untouched while a tick holds the lock", len(running))
	}
}

// A tend's row is retired, but no retry is queued for the issue: a tend is not
// the issue's own work, and flagging the issue would retry the AGENT over a
// rebase that failed. This is reapDead's existing rule, and it must not change
// depending on which caller drove the reap.
func TestReapOrphansDoesNotQueueARetryForATend(t *testing.T) {
	cfg, deps, _ := reapFixture(t, store.KindTend, 1, 20)

	if _, err := ReapOrphans(cfg, deps); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	states, err := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if states[1].NeedsRetry {
		t.Error("a dead tend queued a retry of the issue's own work")
	}
}

// Nothing running is the ordinary case: a machine that did not crash, or a
// sweep a minute after the last one. It must be silent and cheap.
func TestReapOrphansIsQuietWhenNothingIsRunning(t *testing.T) {
	cfg := tickConfig(t)
	spawned := 0
	deps := newDeps(t, cfg, &fakeGH{}, &spawned)

	sum, err := ReapOrphans(cfg, deps)
	if err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if sum.Orphans != 0 {
		t.Errorf("Orphans = %d, want 0", sum.Orphans)
	}
}

// The seam the whole design rests on: the deadline the reap writes must be one
// the DAEMON'S OWN QUERY returns. Everything after recovery is the existing
// scheduler, and it finds work through EarliestRetryAfterAt alone -- so a row
// this query does not return is a row that is still invisible, however
// correctly the reap filled it in.
//
// It is a separate assertion from "RetryAfter is set" because the query has
// conditions of its own (needs_retry, not parked, no live cooldown, a positive
// deadline), and a reap that satisfied the column but not the query would pass
// every other test in this file.
func TestReapOrphansWritesADeadlineTheSchedulerCanFind(t *testing.T) {
	cfg := tickConfig(t)
	cfg.Agent.Worktree = config.WorktreeNone
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	spawned := 0
	deps := newDeps(t, cfg, &fakeGH{}, &spawned)
	deps.Store = db.Project(testProject)
	deps.IsAlive = func(int, int64) bool { return false }

	id, err := deps.Store.CreateDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 7, Kind: store.KindStart, SessionID: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.SetDispatchProcess(id, 4242, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.PutIssueState(store.IssueState{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 7, SessionID: "s",
		SessionStarted: true, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := ReapOrphans(cfg, deps); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}

	due, found, err := db.EarliestRetryAfterAt(time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the scheduler's query found nothing; the recovered issue is still invisible")
	}
	if due.Number != 7 || due.Loop != cfg.Name {
		t.Errorf("due = %+v, want issue 7 of loop %s", due, cfg.Name)
	}
}

// The deadline is the loop's FIRST backoff entry, not a value recovery makes
// up. A crash is a failed attempt like any other, and the retry cap is what
// keeps a crash-looping machine from dispatching forever.
func TestReapOrphansStampsTheLoopsFirstBackoff(t *testing.T) {
	cfg, deps, _ := reapFixture(t, store.KindStart, 1, 0)
	// 0s, so the deadline is now: the issue is eligible at the next wake.
	before := time.Now()

	if _, err := ReapOrphans(cfg, deps); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	states, err := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if got := states[1].RetryAfter; got.After(before.Add(time.Minute)) {
		t.Errorf("RetryAfter = %v, want the first backoff entry (0s), not a later one", got)
	}
}
