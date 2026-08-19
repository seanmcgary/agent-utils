package listener

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/loopcmd"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// workNow is the instant every test in this file reads from the Worker's Now
// seam. It is whole-second and UTC because retry_after holds Unix seconds:
// a fractional deadline would not survive the round trip through the
// database and the literal comparisons below would fail for a reason that
// has nothing to do with the scheduling being tested.
var workNow = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

// workProject is the project id every seeded retry row and every fake target
// in this file belongs to.
const workProject = "11111111-1111-1111-1111-111111111111"

// errBoom is an ordinary tick failure: not lock.ErrHeld, so it is the case
// that must schedule a retry.
var errBoom = errors.New("boom")

// armed is one call to the Worker's After seam.
type armed struct {
	d     time.Duration
	f     func()
	timer *time.Timer
}

// timers is a fake After seam. It records what the worker asked for and
// hands back a real *time.Timer armed an hour out with a no-op, so that:
//
//   - a test fires a retry by calling the recorded f itself and never sleeps
//     for a real delay, which the acceptance forbids; and
//   - a test can ask whether the worker stopped a timer, because
//     time.Timer.Stop reports false for a timer that is already stopped.
type timers struct {
	mu   sync.Mutex
	arms []*armed
}

func (tm *timers) After(d time.Duration, f func()) *time.Timer {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	a := &armed{d: d, f: f, timer: time.AfterFunc(time.Hour, func() {})}
	tm.arms = append(tm.arms, a)
	return a.timer
}

func (tm *timers) len() int {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return len(tm.arms)
}

func (tm *timers) at(t *testing.T, i int) *armed {
	t.Helper()
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if i >= len(tm.arms) {
		t.Fatalf("no timer %d was armed; %d were", i, len(tm.arms))
	}
	return tm.arms[i]
}

// stopped reports whether the worker already stopped timer i.
func (tm *timers) stopped(t *testing.T, i int) bool {
	return !tm.at(t, i).timer.Stop()
}

// openCall records one call to the Open seam.
type openCall struct {
	ref  loopcmd.ProjectRef
	path string
	opts loopcmd.Options
}

// harness is a Worker with every seam replaced and a record of what each one
// was asked to do. Every seam is set before the Worker is used and none is
// written afterwards, which is the same rule work.go states for production.
type harness struct {
	w      *Worker
	timers *timers

	// targets is what the Targets seam returns. targetsCalls counts the
	// calls, so a Wake test can prove a deadline was NOT routed by
	// repository.
	targets []Target

	// targetFor is what the TargetFor seam returns. Default: the single
	// target named by the arguments.
	targetFor func(projectID, loop string) (Target, bool, error)

	// runFn decides what the Run seam returns. Nil means every tick
	// succeeds.
	runFn func(cfg *config.Config) error

	mu           sync.Mutex
	tokenErr     error
	openErr      error
	targetsCalls int
	opens        []openCall
	ran          []string
	cleanups     int
	backoff      []time.Duration
	max          int
}

// newHarness returns a Worker whose seams are all fakes. db may be nil for a
// test that never calls Wake.
func newHarness(db *store.DB) *harness {
	h := &harness{
		timers: &timers{},
		max:    1,
	}
	w := NewWorker(db)
	w.Now = func() time.Time { return workNow }
	w.After = h.timers.After
	w.Token = h.token
	w.Open = h.open
	w.Run = h.run
	w.Targets = h.targetsSeam
	w.TargetFor = func(projectID, loop string) (Target, bool, error) {
		if h.targetFor != nil {
			return h.targetFor(projectID, loop)
		}
		return h.target(loop), true, nil
	}
	// MinWakeInterval is an hour so a Serve test parks in its select until
	// the test cancels, rather than racing a real 30s tick.
	w.MinWakeInterval = time.Hour
	w.OpenRetryDelay = 90 * time.Second
	w.MinRetryDelay = 30 * time.Second
	h.w = w
	return h
}

// target is the fake Target for one loop of the harness's project.
func (h *harness) target(loop string) Target {
	return Target{
		ProjectID:   workProject,
		ProjectName: "web",
		Dir:         "/p/.agent-utils",
		ConfigPath:  "/p/.agent-utils/configs/" + loop + ".yaml",
		LoopName:    loop,
		Repo:        "o/r",
	}
}

func (h *harness) targetsSeam(repo string) ([]Target, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.targetsCalls++
	return h.targets, nil
}

func (h *harness) token() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tokenErr != nil {
		return "", h.tokenErr
	}
	return "gh-token", nil
}

func (h *harness) open(ref loopcmd.ProjectRef, path string, o loopcmd.Options) (*config.Config, loopcmd.Deps, func(), error) {
	h.mu.Lock()
	h.opens = append(h.opens, openCall{ref: ref, path: path, opts: o})
	openErr, backoff, max := h.openErr, h.backoff, h.max
	h.mu.Unlock()

	if openErr != nil {
		// loopcmd.Open returns a nil cleanup alongside its error. Returning
		// one here is what proves work.go never calls it on that path.
		return nil, loopcmd.Deps{}, nil, openErr
	}

	cfg := &config.Config{Name: loopFromPath(path), Repo: "o/r"}
	cfg.Retry.Max = max
	for _, d := range backoff {
		cfg.Retry.Backoff = append(cfg.Retry.Backoff, config.Duration(d))
	}
	cleanup := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.cleanups++
	}
	return cfg, loopcmd.Deps{}, cleanup, nil
}

func (h *harness) run(ctx context.Context, cfg *config.Config, deps loopcmd.Deps) (loopcmd.Summary, error) {
	h.mu.Lock()
	h.ran = append(h.ran, cfg.Name)
	fn := h.runFn
	h.mu.Unlock()

	if fn != nil {
		return loopcmd.Summary{}, fn(cfg)
	}
	return loopcmd.Summary{}, nil
}

// loopFromPath recovers a loop name from a fake target's config path, so the
// config the Open seam returns is named for the loop that was opened and the
// recorded Run calls identify which target ran.
func loopFromPath(path string) string {
	base := filepath.Base(path)
	return base[:len(base)-len(filepath.Ext(base))]
}

func (h *harness) counts() (opens, runs, cleanups int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.opens), len(h.ran), h.cleanups
}

func (h *harness) ranLoops() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ran...)
}

// pendingLen reports how many loops hold a scheduled retry.
func (h *harness) pendingLen() int {
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	return len(h.w.pending)
}

// openWorkDB returns an empty canonical state database in a temporary
// directory.
func openWorkDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// seedDeadline writes one issue row carrying a pending retry deadline.
func seedDeadline(t *testing.T, db *store.DB, loop string, number int, at time.Time) {
	t.Helper()
	err := db.Project(workProject).PutIssueState(store.IssueState{
		Loop: loop, Repo: "o/r", Number: number,
		NeedsRetry: true, RetryAfter: at, UpdatedAt: workNow,
	})
	if err != nil {
		t.Fatalf("PutIssueState: %v", err)
	}
}

// A tick that lost the race for the loop's lock schedules nothing: the
// delivery carries no state, so the tick that holds the lock reads the same
// GitHub state a moment later. A retry here would tick the same loop again
// for no new information.
func TestALockHeldTickSchedulesNoRetry(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.runFn = func(*config.Config) error { return fmt.Errorf("run tick: %w", lock.ErrHeld) }

	h.w.Deliver(context.Background(), "o/r")

	if n := h.timers.len(); n != 0 {
		t.Errorf("armed %d retry timers, want 0 for a held lock", n)
	}
	if n := h.pendingLen(); n != 0 {
		t.Errorf("pending = %d, want 0", n)
	}
}

// A held lock also clears an attempt an earlier failure left behind.
// Otherwise the next real failure would resume the backoff list part way
// through and give up early.
func TestALockHeldTickClearsAPendingAttempt(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.max = 2
	h.backoff = []time.Duration{time.Minute, 5 * time.Minute}
	h.runFn = func(*config.Config) error { return errBoom }

	h.w.Deliver(context.Background(), "o/r")
	if n := h.pendingLen(); n != 1 {
		t.Fatalf("pending after a failed tick = %d, want 1", n)
	}

	h.runFn = func(*config.Config) error { return fmt.Errorf("run tick: %w", lock.ErrHeld) }
	h.w.Deliver(context.Background(), "o/r")

	if n := h.pendingLen(); n != 0 {
		t.Errorf("pending after a held lock = %d, want 0", n)
	}
	if !h.timers.stopped(t, 0) {
		t.Error("the pending retry timer was left armed after a held lock")
	}
}

// An ordinary tick failure is retried, and the wait is the loop's own
// configured backoff entry for this attempt.
func TestATickErrorIsRetriedAfterTheConfiguredDelay(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.max = 1
	h.backoff = []time.Duration{45 * time.Minute}
	h.runFn = func(*config.Config) error { return errBoom }

	h.w.Deliver(context.Background(), "o/r")

	if n := h.timers.len(); n != 1 {
		t.Fatalf("armed %d retry timers, want 1", n)
	}
	if got := h.timers.at(t, 0).d; got != 45*time.Minute {
		t.Errorf("retry delay = %v, want the configured 45m", got)
	}

	// Firing the recorded function is what a real timer would do. No test
	// waits 45 minutes.
	h.timers.at(t, 0).f()

	if got := h.ranLoops(); len(got) != 2 {
		t.Errorf("ran %v, want the tick and one retry", got)
	}
}

// The retry budget is the loop's retry.max, counted across the timers, not
// per delivery. Without the cap a permanently failing loop would tick
// forever on its own backoff.
func TestTheRetryStopsAfterRetryMax(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.max = 2
	h.backoff = []time.Duration{time.Minute, 5 * time.Minute}
	h.runFn = func(*config.Config) error { return errBoom }

	h.w.Deliver(context.Background(), "o/r")
	h.timers.at(t, 0).f()
	h.timers.at(t, 1).f()

	if n := h.timers.len(); n != 2 {
		t.Errorf("armed %d retry timers, want 2 for retry.max = 2", n)
	}
	if got := h.timers.at(t, 1).d; got != 5*time.Minute {
		t.Errorf("second retry delay = %v, want the second backoff entry 5m", got)
	}
	if got := h.ranLoops(); len(got) != 3 {
		t.Errorf("ran %v, want the tick plus two retries", got)
	}
	if n := h.pendingLen(); n != 0 {
		t.Errorf("pending = %d after the budget was spent, want 0", n)
	}
}

// retry.max = 0 is legal and means never retry, and a config with it carries
// no backoff list at all. Indexing that empty list would panic a daemon with
// no supervisor.
func TestRetryMaxZeroSchedulesNothingAndDoesNotPanic(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.max = 0
	h.backoff = nil
	h.runFn = func(*config.Config) error { return errBoom }

	h.w.Deliver(context.Background(), "o/r")

	if n := h.timers.len(); n != 0 {
		t.Errorf("armed %d retry timers, want 0 for retry.max = 0", n)
	}
}

// A token that cannot be read is an operator problem no retry can fix: a bad
// mode or an absent file is the same on the next attempt, and retrying would
// log the identical error retry.max times per delivery.
func TestATokenErrorSchedulesNoRetry(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.tokenErr = errors.New("mode 0644 grants group access")

	h.w.Deliver(context.Background(), "o/r")

	opens, runs, cleanups := h.counts()
	if opens != 0 || runs != 0 || cleanups != 0 {
		t.Errorf("opens = %d, runs = %d, cleanups = %d; want no work after a token error",
			opens, runs, cleanups)
	}
	if n := h.timers.len(); n != 0 {
		t.Errorf("armed %d retry timers, want 0 after a token error", n)
	}
}

// An Open failure leaves no config, so the loop's backoff list is unknown.
// The retry runs at OpenRetryDelay rather than at some undefined value.
func TestAnOpenErrorSchedulesARetryAtOpenRetryDelay(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.openErr = errors.New("unimported legacy database")

	h.w.Deliver(context.Background(), "o/r")

	if n := h.timers.len(); n != 1 {
		t.Fatalf("armed %d retry timers, want 1 after an Open error", n)
	}
	if got := h.timers.at(t, 0).d; got != h.w.OpenRetryDelay {
		t.Errorf("retry delay = %v, want OpenRetryDelay %v", got, h.w.OpenRetryDelay)
	}
	_, runs, cleanups := h.counts()
	if runs != 0 {
		t.Errorf("ran %d ticks after an Open error, want 0", runs)
	}
	if cleanups != 0 {
		t.Errorf("called cleanup %d times after an Open error, want 0 (it is nil there)", cleanups)
	}
}

// The migrated first backoff entry is 0s. Retrying with no pause at all
// would burn the whole retry budget in the time it takes to make retry.max
// GitHub calls, so the floor is what makes the wait a wait.
func TestAZeroBackoffEntryStillWaitsMinRetryDelay(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.max = 1
	h.backoff = []time.Duration{0}
	h.runFn = func(*config.Config) error { return errBoom }

	h.w.Deliver(context.Background(), "o/r")

	if got := h.timers.at(t, 0).d; got != h.w.MinRetryDelay {
		t.Errorf("retry delay = %v, want the MinRetryDelay floor %v", got, h.w.MinRetryDelay)
	}
}

// Open holds a SQLite handle. In a daemon a missed cleanup is one leaked
// handle per delivery per target, so it is asserted on both outcomes.
func TestCleanupRunsExactlyOncePerTick(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*config.Config) error
	}{
		{"success", nil},
		{"run error", func(*config.Config) error { return errBoom }},
		{"lock held", func(*config.Config) error { return fmt.Errorf("run tick: %w", lock.ErrHeld) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(nil)
			h.targets = []Target{h.target("planning")}
			h.runFn = tc.run

			h.w.Deliver(context.Background(), "o/r")

			if _, _, cleanups := h.counts(); cleanups != 1 {
				t.Errorf("called cleanup %d times, want exactly 1", cleanups)
			}
		})
	}
}

// One loop's failure must not strand every other loop that watches the same
// repository: they are separate projects with separate state.
func TestTwoTargetsBothRunWhenTheFirstFails(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning"), h.target("review")}
	h.runFn = func(cfg *config.Config) error {
		if cfg.Name == "planning" {
			return errBoom
		}
		return nil
	}

	h.w.Deliver(context.Background(), "o/r")

	got := h.ranLoops()
	if len(got) != 2 || got[0] != "planning" || got[1] != "review" {
		t.Errorf("ran %v, want both loops in order", got)
	}
	if _, _, cleanups := h.counts(); cleanups != 2 {
		t.Errorf("called cleanup %d times, want one per target", cleanups)
	}
}

// Every Open carries the token, requires GitHub, and refuses to run against
// an unimported legacy database: a tick that wrote to a database missing this
// loop's rows would re-dispatch every open issue.
func TestOpenIsCalledWithTheDaemonsOptions(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}

	h.w.Deliver(context.Background(), "o/r")

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.opens) != 1 {
		t.Fatalf("opened %d times, want 1", len(h.opens))
	}
	got := h.opens[0]
	if got.opts.Token != "gh-token" || !got.opts.RequireGitHub {
		t.Errorf("options = %+v, want the token and RequireGitHub", got.opts)
	}
	if got.opts.MigrationPolicy != loopcmd.FailOnUnimported {
		t.Error("MigrationPolicy must be FailOnUnimported on the write path")
	}
	if got.ref.ID != workProject || got.path != h.target("planning").ConfigPath {
		t.Errorf("ref = %+v, path = %q; want the target's own identity", got.ref, got.path)
	}
}

// A second failure for a loop that already has a timer must not leave the
// first one armed, or one delivery storm would run several retries for the
// same loop at once.
func TestSchedulingAgainStopsTheOldTimer(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.max = 3
	h.backoff = []time.Duration{time.Minute, 5 * time.Minute, 10 * time.Minute}
	h.runFn = func(*config.Config) error { return errBoom }

	h.w.Deliver(context.Background(), "o/r")
	h.w.Deliver(context.Background(), "o/r")

	if n := h.timers.len(); n != 2 {
		t.Fatalf("armed %d timers, want 2", n)
	}
	if !h.timers.stopped(t, 0) {
		t.Error("the first timer was left armed when the second was scheduled")
	}
	if n := h.pendingLen(); n != 1 {
		t.Errorf("pending = %d, want one entry for the one loop", n)
	}
}

// A cancelled context means the daemon is shutting down. A retry timer that
// fired afterwards would start an agent nobody is waiting for.
func TestARetryDoesNotRunAfterTheContextIsCancelled(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.max = 1
	h.backoff = []time.Duration{time.Minute}
	h.runFn = func(*config.Config) error { return errBoom }

	ctx, cancel := context.WithCancel(context.Background())
	h.w.Deliver(ctx, "o/r")
	cancel()
	h.timers.at(t, 0).f()

	if got := h.ranLoops(); len(got) != 1 {
		t.Errorf("ran %v, want only the original tick", got)
	}
}

// A deadline belongs to one project's issue. Routing it by repository would
// dispatch agents in every other project watching that repository, on that
// project's own token budget, so Wake must go through TargetFor and tick
// exactly the one loop named by the row.
func TestWakeTicksOnlyTheLoopNamedByTheDeadline(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	// Both loops watch o/r. If Wake ever routed by repository, "review"
	// would run too.
	h.targets = []Target{h.target("planning"), h.target("review")}

	var forProject, forLoop string
	h.targetFor = func(projectID, loop string) (Target, bool, error) {
		forProject, forLoop = projectID, loop
		return h.target(loop), true, nil
	}
	seedDeadline(t, db, "planning", 7, workNow.Add(-time.Minute))

	next, ok := h.w.Wake(context.Background())

	if !ok {
		t.Fatal("ok = false, want the past deadline")
	}
	if !next.Equal(workNow.Add(-time.Minute)) {
		t.Errorf("next = %v, want the deadline that was handled", next)
	}
	if got := h.ranLoops(); len(got) != 1 || got[0] != "planning" {
		t.Errorf("ran %v, want only the loop named by the deadline", got)
	}
	if forProject != workProject || forLoop != "planning" {
		t.Errorf("TargetFor(%q, %q), want the deadline's own project and loop", forProject, forLoop)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.targetsCalls != 0 {
		t.Errorf("Targets was called %d times; a deadline must never be routed by repository", h.targetsCalls)
	}
}

// A deadline still in the future is returned, not acted on: the caller sets
// its timer for it.
func TestWakeWithAFutureDeadlineTicksNothing(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	due := workNow.Add(time.Hour)
	seedDeadline(t, db, "planning", 7, due)

	next, ok := h.w.Wake(context.Background())

	if !ok || !next.Equal(due) {
		t.Errorf("next = %v, ok = %v; want the future deadline returned unhandled", next, ok)
	}
	if got := h.ranLoops(); len(got) != 0 {
		t.Errorf("ran %v, want nothing before the deadline", got)
	}
}

func TestWakeWithNoDeadlineReturnsFalse(t *testing.T) {
	h := newHarness(openWorkDB(t))

	if _, ok := h.w.Wake(context.Background()); ok {
		t.Error("ok = true on an empty database, want false")
	}
	if got := h.ranLoops(); len(got) != 0 {
		t.Errorf("ran %v, want nothing", got)
	}
}

// The project or the loop can be deleted while a retry row survives. The row
// is then permanently past due and permanently unroutable, and without a
// break Serve would re-enter Wake every MinWakeInterval forever, re-logging
// the same warning and re-reading the database. Wake clears the flag, which
// is the same thing a tick does for a failure no retry can act on.
func TestWakeClearsAnOrphanedDeadlineSoItCannotSpin(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	h.targetFor = func(projectID, loop string) (Target, bool, error) {
		return Target{}, false, nil
	}
	seedDeadline(t, db, "planning", 7, workNow.Add(-time.Minute))

	if _, ok := h.w.Wake(context.Background()); !ok {
		t.Fatal("ok = false, want the past deadline on the first wake")
	}
	if got := h.ranLoops(); len(got) != 0 {
		t.Errorf("ran %v, want nothing for a loop that no longer exists", got)
	}

	// The cycle is broken in the database, so the very next wake finds
	// nothing at all rather than the same orphan.
	if _, ok := h.w.Wake(context.Background()); ok {
		t.Error("the orphaned deadline is still pending; Wake would spin on it forever")
	}
}

// A shut-down daemon starts no work: every timer a delivery left pending is
// stopped when Serve's context is cancelled.
func TestServeStopsEveryPendingTimerOnCancel(t *testing.T) {
	h := newHarness(openWorkDB(t))
	h.targets = []Target{h.target("planning")}
	h.max = 1
	h.backoff = []time.Duration{time.Minute}
	h.runFn = func(*config.Config) error { return errBoom }

	ctx, cancel := context.WithCancel(context.Background())
	h.w.Deliver(ctx, "o/r")
	if n := h.pendingLen(); n != 1 {
		t.Fatalf("pending = %d before Serve, want 1", n)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.w.Serve(ctx)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}

	if n := h.pendingLen(); n != 0 {
		t.Errorf("pending = %d after shutdown, want 0", n)
	}
	if !h.timers.stopped(t, 0) {
		t.Error("a retry timer was left armed after shutdown")
	}
}
