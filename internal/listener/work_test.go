package listener

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
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
	targetFor func(projectID, loop string) (Target, Routing, error)

	// runFn decides what the RunIssue seam returns. Nil means every tick
	// succeeds.
	runFn func(cfg *config.Config) error

	// gh is the one underlying GitHub client every pass in a test is built
	// on. The saving a shared fetch buys is invisible in what a tick decides
	// and visible only in this fake's counters.
	gh *deliveryGH

	mu           sync.Mutex
	tokenErr     error
	openErr      error
	tokenCalls   int
	clients      int
	targetsCalls int
	nowCalls     int
	opens        []openCall
	ran          []string
	// ranIssues records the issue number each recorded run was scoped to, in
	// the same order as ran. A delivery decides one issue, so "which loop ran"
	// is only half of what a test has to be able to assert.
	ranIssues []int
	cleanups  int
	backoff   []time.Duration
	max       int
}

// newHarness returns a Worker whose seams are all fakes. db may be nil for a
// test that never calls Wake.
func newHarness(db *store.DB) *harness {
	h := &harness{
		timers: &timers{},
		gh:     &deliveryGH{},
		max:    1,
	}
	w := NewWorker(db)
	// Every pass builds its client through this seam, so a test can count both
	// the clients a delivery builds and the fetches they make.
	w.NewClient = func(token string) ghub.Client {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.clients++
		return h.gh
	}
	w.Now = h.now
	w.After = h.timers.After
	w.Token = h.token
	w.Open = h.open
	w.RunIssue = h.runIssue
	w.Targets = h.targetsSeam
	w.TargetFor = func(projectID, loop string) (Target, Routing, error) {
		if h.targetFor != nil {
			return h.targetFor(projectID, loop)
		}
		return h.target(loop), RouteFound, nil
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

// now is the frozen clock. It counts its calls, which is how a test proves
// Serve did or did not enter Wake: Wake reads the clock before it touches
// anything else, and nothing else in a tick reads it.
func (h *harness) now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nowCalls++
	return workNow
}

func (h *harness) token() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tokenCalls++
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
	// The client the caller handed in is what the tick would use, exactly as
	// loopcmd.Open does with Options.GH.
	return cfg, loopcmd.Deps{GH: o.GH}, cleanup, nil
}

func (h *harness) runIssue(
	ctx context.Context,
	cfg *config.Config,
	deps loopcmd.Deps,
	number int,
) (loopcmd.Summary, error) {
	h.mu.Lock()
	h.ran = append(h.ran, cfg.Name)
	h.ranIssues = append(h.ranIssues, number)
	fn := h.runFn
	h.mu.Unlock()

	// The first thing loopcmd.TickIssue does is fetch the issue the delivery
	// named (subject). The fake does it too, because a fake that ignored
	// deps.GH would let a "one fetch per delivery" assertion pass without the
	// client ever being shared.
	if deps.GH != nil {
		if _, err := deps.GH.Issue(ctx, "o", "r", number); err != nil {
			return loopcmd.Summary{}, err
		}
	}

	if fn != nil {
		return loopcmd.Summary{}, fn(cfg)
	}
	return loopcmd.Summary{}, nil
}

// deliveryGH is a ghub.Client that records the numbers it was asked to fetch.
// Only the issue fetch is exercised here; the scoped tick's own use of the
// rest is covered in internal/loopcmd.
type deliveryGH struct {
	mu      sync.Mutex
	fetched []int
	err     error
}

func (f *deliveryGH) Issue(_ context.Context, _, _ string, number int) (ghub.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched = append(f.fetched, number)
	if f.err != nil {
		return ghub.Issue{}, f.err
	}
	return ghub.Issue{Number: number}, nil
}

func (f *deliveryGH) fetches() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.fetched...)
}

func (f *deliveryGH) PullRequest(context.Context, string, string, int) (ghub.PullRequest, error) {
	return ghub.PullRequest{}, nil
}
func (f *deliveryGH) ListOpenIssues(context.Context, string, string) ([]ghub.Issue, error) {
	return nil, nil
}
func (f *deliveryGH) ListOpenPullRequests(context.Context, string, string) ([]ghub.PullRequest, error) {
	return nil, nil
}
func (f *deliveryGH) BehindBy(context.Context, string, string, string, string) (int, error) {
	return 0, nil
}
func (f *deliveryGH) PostComment(context.Context, string, string, int, string) error { return nil }
func (f *deliveryGH) EditLabels(context.Context, string, string, int, []string, []string) error {
	return nil
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

// ranNumbers reports the issue each recorded run was scoped to.
func (h *harness) ranNumbers() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.ranIssues...)
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

	h.w.Deliver(context.Background(), "o/r", 7)

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

	h.w.Deliver(context.Background(), "o/r", 7)
	if n := h.pendingLen(); n != 1 {
		t.Fatalf("pending after a failed tick = %d, want 1", n)
	}

	h.runFn = func(*config.Config) error { return fmt.Errorf("run tick: %w", lock.ErrHeld) }
	h.w.Deliver(context.Background(), "o/r", 7)

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

	h.w.Deliver(context.Background(), "o/r", 7)

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

	h.w.Deliver(context.Background(), "o/r", 7)
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

	h.w.Deliver(context.Background(), "o/r", 7)

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

	h.w.Deliver(context.Background(), "o/r", 7)

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

	h.w.Deliver(context.Background(), "o/r", 7)

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

	h.w.Deliver(context.Background(), "o/r", 7)

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

			h.w.Deliver(context.Background(), "o/r", 7)

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

	h.w.Deliver(context.Background(), "o/r", 7)

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

	h.w.Deliver(context.Background(), "o/r", 7)

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

	h.w.Deliver(context.Background(), "o/r", 7)
	h.w.Deliver(context.Background(), "o/r", 7)

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
	h.w.Deliver(ctx, "o/r", 7)
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
	h.targetFor = func(projectID, loop string) (Target, Routing, error) {
		forProject, forLoop = projectID, loop
		return h.target(loop), RouteFound, nil
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

// stillPending reports whether the seeded deadline is still in the wake
// query, i.e. whether its failure flag survived.
func (h *harness) stillPending(t *testing.T, db *store.DB) bool {
	t.Helper()
	_, ok, err := db.EarliestRetryAfterAt(workNow, nil)
	if err != nil {
		t.Fatalf("EarliestRetryAfterAt: %v", err)
	}
	return ok
}

// The project or the loop can be deleted while a retry row survives. The row
// is then permanently past due and permanently unroutable, and without a
// break Serve would re-enter Wake every MinWakeInterval forever, re-logging
// the same warning and re-reading the database. Wake clears the flag, which
// is the same thing a tick does for a failure no retry can act on.
//
// But it does not clear it at once: TargetFor cannot distinguish a loop that
// is gone from one whose yaml is unparsable this minute or whose volume is
// not mounted yet, and a cleared flag is not re-derivable, so an immediate
// clear would let a config broken for an hour destroy a loop's whole pending
// retry set, one issue per wake.
func TestAnOrphanedDeadlineIsClearedOnlyAfterRepeatedObservations(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	h.targetFor = func(projectID, loop string) (Target, Routing, error) {
		return Target{}, RouteGone, nil
	}
	seedDeadline(t, db, "planning", 7, workNow.Add(-time.Minute))

	// The counts below are written out rather than derived from
	// orphanClearAfter, so that lowering the constant fails this test instead
	// of quietly rewriting what it checks. Two wakes must preserve the
	// deadline; the third clears it.
	if orphanClearAfter != 3 {
		t.Fatalf("orphanClearAfter = %d; this test is written against 3 wakes and must be updated with it",
			orphanClearAfter)
	}

	for i := 1; i <= 2; i++ {
		if _, ok := h.w.Wake(context.Background()); !ok {
			t.Fatalf("wake %d: ok = false, want the past deadline", i)
		}
		if !h.stillPending(t, db) {
			t.Fatalf("wake %d cleared the deadline; a transient unroutable window must not destroy it", i)
		}
	}

	if _, ok := h.w.Wake(context.Background()); !ok {
		t.Fatal("the clearing wake returned ok = false, want the past deadline it acted on")
	}
	if h.stillPending(t, db) {
		t.Error("the orphaned deadline is still pending; Wake would spin on it forever")
	}
	if got := h.ranLoops(); len(got) != 0 {
		t.Errorf("ran %v, want nothing for a loop that cannot be routed", got)
	}
}

// A loop that becomes routable again starts the count over. Without the
// reset, a loop whose config is momentarily unreadable once a day would
// eventually accumulate enough observations to have a live deadline deleted.
func TestRoutingAgainResetsTheOrphanCount(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	routable := false
	h.targetFor = func(projectID, loop string) (Target, Routing, error) {
		if !routable {
			return Target{}, RouteGone, nil
		}
		return h.target(loop), RouteFound, nil
	}
	seedDeadline(t, db, "planning", 7, workNow.Add(-time.Minute))

	// Two wakes: one short of the threshold, matching the test above.
	for i := 1; i <= 2; i++ {
		h.w.Wake(context.Background())
	}

	// The config file is saved correctly again: the loop routes and ticks.
	routable = true
	h.w.Wake(context.Background())
	if got := h.ranLoops(); len(got) != 1 {
		t.Fatalf("ran %v, want the one tick for the loop that came back", got)
	}

	// The window reopens. The count started over, so the deadline survives
	// exactly as long as it did the first time.
	routable = false
	for i := 1; i <= 2; i++ {
		h.w.Wake(context.Background())
		if !h.stillPending(t, db) {
			t.Fatalf("wake %d after the loop routed cleared the deadline; the count did not reset", i)
		}
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
	h.w.Deliver(ctx, "o/r", 7)
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

// The wake delay is the only thing standing between a stale past-due row and
// a loop that re-ticks as fast as the GitHub API answers, and Serve has no
// other guard. Each row is a way that arithmetic can be got wrong.
func TestWakeDelay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		next     time.Time
		ok       bool
		interval time.Duration
		want     time.Duration
	}{
		{
			name:     "no deadline waits the interval",
			ok:       false,
			interval: 30 * time.Second,
			want:     30 * time.Second,
		},
		{
			name:     "a past deadline waits the interval, never zero",
			next:     workNow.Add(-2 * time.Hour),
			ok:       true,
			interval: 30 * time.Second,
			want:     30 * time.Second,
		},
		{
			name:     "a future deadline waits for it",
			next:     workNow.Add(2 * time.Hour),
			ok:       true,
			interval: 30 * time.Second,
			want:     2 * time.Hour,
		},
		{
			name:     "a deadline inside the interval still waits the interval",
			next:     workNow.Add(time.Second),
			ok:       true,
			interval: 30 * time.Second,
			want:     30 * time.Second,
		},
		{
			// The field is exported and documented as shrinkable, so a
			// caller can zero it. Without the reassertion this row is a
			// tight database-poll-and-tick loop.
			name:     "a zero interval falls back to the default floor",
			ok:       false,
			interval: 0,
			want:     defaultMinWakeInterval,
		},
		{
			name:     "a negative interval falls back too",
			next:     workNow.Add(-time.Hour),
			ok:       true,
			interval: -time.Minute,
			want:     defaultMinWakeInterval,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(nil)
			h.w.MinWakeInterval = tc.interval

			if got := h.w.wakeDelay(tc.next, tc.ok); got != tc.want {
				t.Errorf("wakeDelay(%v, %v) = %v, want %v", tc.next, tc.ok, got, tc.want)
			}
		})
	}
}

// Wake is synchronous and runs a whole tick. Entering it after cancellation
// would start an agent during shutdown and hold the shutdown open for the
// length of that tick, so Serve checks the context before every wake, not
// only in its select.
func TestServeDoesNotWakeWithAnAlreadyCancelledContext(t *testing.T) {
	h := newHarness(openWorkDB(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.w.Serve(ctx)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return for an already cancelled context")
	}

	// Wake reads the clock before it does anything else, and nothing else in
	// a tick reads it, so an untouched clock means Wake was never entered.
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.nowCalls != 0 {
		t.Errorf("the clock was read %d times; Serve entered Wake after cancellation", h.nowCalls)
	}
}

// The Open budget and the tick budget are separate counters. Sharing one
// would let two Open failures spend a loop's whole retry.max, so the first
// genuine tick failure after them would get no retry at all -- the failure
// the operator actually cares about, silently dropped.
func TestOpenFailuresDoNotSpendTheTickRetryBudget(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.max = 1
	h.backoff = []time.Duration{45 * time.Minute}
	h.openErr = errors.New("unimported legacy database")

	h.w.Deliver(context.Background(), "o/r", 7)
	h.w.Deliver(context.Background(), "o/r", 7)
	if n := h.timers.len(); n != 2 {
		t.Fatalf("armed %d timers for two Open failures, want 2", n)
	}

	// The database is imported; the next tick reaches Run and fails for a
	// real reason.
	h.mu.Lock()
	h.openErr = nil
	h.mu.Unlock()
	h.runFn = func(*config.Config) error { return errBoom }

	h.w.Deliver(context.Background(), "o/r", 7)

	if n := h.timers.len(); n != 3 {
		t.Fatalf("armed %d timers, want a third for the tick failure", n)
	}
	if got := h.timers.at(t, 2).d; got != 45*time.Minute {
		t.Errorf("tick retry delay = %v, want the loop's own first backoff entry 45m", got)
	}
}

// The finding this exists for: an operator saves a loop's yaml mid-edit and
// three wakes later -- about ninety seconds -- the earliest pending retry for
// that loop is destroyed, then the next one, then the next. A condition
// TargetFor cannot resolve is waited on indefinitely instead, because clearing
// needs_retry is irreversible and nothing re-derives it.
func TestADeadlineIsNeverClearedWhileTheLoopCannotBeResolved(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	h.targetFor = func(projectID, loop string) (Target, Routing, error) {
		return Target{}, RouteUnknown, nil
	}
	seedDeadline(t, db, "planning", 7, workNow.Add(-time.Minute))

	// Far past orphanClearAfter: no number of wakes may clear it.
	for i := 1; i <= orphanClearAfter*4; i++ {
		if _, ok := h.w.Wake(context.Background()); !ok {
			t.Fatalf("wake %d: ok = false, want the past deadline", i)
		}
		if !h.stillPending(t, db) {
			t.Fatalf("wake %d destroyed a pending retry for a loop that is merely unreadable", i)
		}
	}
	if got := h.ranLoops(); len(got) != 0 {
		t.Errorf("ran %v, want nothing for a loop that cannot be routed", got)
	}
}

// The count is of CONSECUTIVE "definitely gone" observations. A wake that
// cannot tell is not one, so it resets the count rather than adding to it:
// without that, a loop deleted while its project's volume flaps would still be
// cleared by three observations that never agreed with each other.
func TestAnUnresolvableWakeResetsTheGoneCount(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	routing := RouteGone
	h.targetFor = func(projectID, loop string) (Target, Routing, error) {
		return Target{}, routing, nil
	}
	seedDeadline(t, db, "planning", 7, workNow.Add(-time.Minute))

	if orphanClearAfter != 3 {
		t.Fatalf("orphanClearAfter = %d; this test is written against 3 wakes and must be updated with it",
			orphanClearAfter)
	}

	// Two gone observations: one short of the threshold.
	for i := 1; i <= 2; i++ {
		h.w.Wake(context.Background())
	}
	// One wake that cannot tell.
	routing = RouteUnknown
	h.w.Wake(context.Background())
	if !h.stillPending(t, db) {
		t.Fatal("the unresolvable wake itself cleared the deadline")
	}

	// Two more gone observations. Under a count that survived the gap this
	// would be the fourth and would clear.
	routing = RouteGone
	for i := 1; i <= 2; i++ {
		h.w.Wake(context.Background())
		if !h.stillPending(t, db) {
			t.Fatalf("gone wake %d after the gap cleared the deadline; the count did not reset", i)
		}
	}
}

// The wake query serves ONE row: the single earliest deadline on the machine.
// A deadline whose loop cannot be resolved is never cleared and never skipped
// by the query, so without the skip set it is returned again on every wake for
// as long as the condition lasts -- and no other loop's durable retry ever runs
// again. It would fire only if a webhook delivery happened to tick that loop.
// One project with a single unparsable yaml, or one registered project whose
// directory was deleted, is enough to reach that state.
func TestAnUnresolvableDeadlineDoesNotStarveOtherLoops(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	h.targetFor = func(projectID, loop string) (Target, Routing, error) {
		if loop == "planning" {
			return Target{}, RouteUnknown, nil
		}
		return h.target(loop), RouteFound, nil
	}
	// planning holds the EARLIER deadline, so it is the row the wake query
	// returns first and the one that does the starving.
	seedDeadline(t, db, "planning", 7, workNow.Add(-time.Hour))
	seedDeadline(t, db, "review", 8, workNow.Add(-time.Minute))

	if _, ok := h.w.Wake(context.Background()); !ok {
		t.Fatal("ok = false, want the past deadline")
	}

	if got := h.ranLoops(); len(got) != 1 || got[0] != "review" {
		t.Errorf("ran %v, want the later deadline's loop to be served past the unresolvable one", got)
	}
	// The unresolvable deadline is stepped over, not resolved: it must still
	// be the earliest pending row, ready to run the moment the loop routes.
	due, pending, err := db.EarliestRetryAfterAt(workNow, nil)
	if err != nil {
		t.Fatalf("EarliestRetryAfterAt: %v", err)
	}
	if !pending || due.Loop != "planning" || due.Number != 7 {
		t.Errorf("earliest pending = %+v (pending=%v), want the unresolvable deadline untouched", due, pending)
	}
}

// A loop that becomes routable again must be served, not left in a skip set
// that outlived the condition. Nothing about being unresolvable is durable:
// the yaml gets saved, the volume gets mounted.
func TestALoopThatRoutesAgainIsServedAfterBeingSkipped(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	routable := false
	h.targetFor = func(projectID, loop string) (Target, Routing, error) {
		if loop == "planning" && !routable {
			return Target{}, RouteUnknown, nil
		}
		return h.target(loop), RouteFound, nil
	}
	seedDeadline(t, db, "planning", 7, workNow.Add(-time.Hour))
	seedDeadline(t, db, "review", 8, workNow.Add(-time.Minute))

	h.w.Wake(context.Background())
	routable = true
	h.w.Wake(context.Background())

	got := h.ranLoops()
	if len(got) != 2 || got[0] != "review" || got[1] != "planning" {
		t.Errorf("ran %v, want review while planning was unresolvable and planning once it routed again", got)
	}
}

// An unresolvable deadline is waited on for as long as the condition lasts,
// which is unbounded: an unparsable config left over a weekend would write one
// warning per loop per wake -- about 2,880 lines a day -- into the same
// unrotated launchd stdout log the HTTP rejections are throttled for.
func TestTheUnroutableWarningIsThrottledPerLoop(t *testing.T) {
	buf := captureLogs(t)
	db := openWorkDB(t)
	h := newHarness(db)
	clock := workNow
	h.w.Now = func() time.Time { return clock }
	h.targetFor = func(projectID, loop string) (Target, Routing, error) {
		return Target{}, RouteUnknown, nil
	}
	seedDeadline(t, db, "planning", 7, workNow.Add(-time.Hour))

	const wakes = 20
	for i := 0; i < wakes; i++ {
		h.w.Wake(context.Background())
	}
	if got := strings.Count(buf.String(), "cannot route a retry deadline"); got != 1 {
		t.Errorf("logged %d warnings across %d wakes, want only the transition", got, wakes)
	}

	// Once the interval passes, one line gets through and says how many
	// probes it stands for: a silently sampled log would understate how long
	// the loop has been stuck.
	buf.Reset()
	clock = clock.Add(unroutableLogInterval)
	h.w.Wake(context.Background())
	if got := strings.Count(buf.String(), "cannot route a retry deadline"); got != 1 {
		t.Fatalf("logged %d warnings after the interval, want the periodic summary", got)
	}
	if !strings.Contains(buf.String(), "suppressed_since_last="+fmt.Sprint(wakes-1)) {
		t.Errorf("summary does not carry the suppressed count: %s", buf.String())
	}
}

// The fan-out a delivery causes has to be visible. Deliver already logged the
// zero case ("no loop watches this repository"), so the ONLY delivery that
// said anything was the one that did nothing: a delivery that ticked three
// loops produced ticks with nothing tying them back to it. Without this line
// an operator cannot tell "my issue caused this" from "cron would have done it
// anyway".
//
// The line names the ISSUE as well as the loops. One delivery still fans out
// across every project watching the repository -- separate state, separate
// token budgets -- but each of those passes now acts on one issue, and a line
// that omitted it would still read as a repository-wide reconcile.
func TestDeliverLogsTheIssueAndTheLoopsItIsAboutToTick(t *testing.T) {
	buf := captureLogs(t)
	h := newHarness(nil)
	h.targets = []Target{h.target("planning"), h.target("execution")}

	h.w.Deliver(context.Background(), "o/r", 51)

	out := buf.String()
	if !strings.Contains(out, "repo=o/r") {
		t.Errorf("the fan-out line does not name the repository:\n%s", out)
	}
	if !strings.Contains(out, "number=51") {
		t.Errorf("the fan-out line does not name the issue:\n%s", out)
	}
	if strings.Contains(out, "reconciling every loop") {
		t.Errorf("the fan-out line still claims a repository-wide reconcile:\n%s", out)
	}
	for _, loop := range []string{"planning", "execution"} {
		if !strings.Contains(out, loop) {
			t.Errorf("the fan-out line does not name loop %q:\n%s", loop, out)
		}
	}
}

// A delivery names one issue, and every loop watching the repository must be
// told WHICH. Passing only the repository is what made a delivery about an
// unlabelled issue dispatch an agent for an unrelated one, once per project.
func TestDeliverScopesEveryTargetToTheDeliveredIssue(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning"), h.target("execution")}

	h.w.Deliver(context.Background(), "o/r", 51)

	if got := h.ranLoops(); len(got) != 2 {
		t.Fatalf("ran %v, want both loops", got)
	}
	for i, n := range h.ranNumbers() {
		if n != 51 {
			t.Errorf("run %d was scoped to issue %d, want 51", i, n)
		}
	}
}

// A retry re-runs the SAME scoped pass. A retry that widened into a reconcile
// would spend the whole repository's budget a minute after a delivery about
// one issue failed -- and would dispatch agents the delivery never asked for.
func TestARetryStaysScopedToTheDeliveredIssue(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.backoff = []time.Duration{time.Minute}
	h.runFn = func(*config.Config) error { return errBoom }

	h.w.Deliver(context.Background(), "o/r", 51)
	if h.timers.len() != 1 {
		t.Fatalf("armed %d timers, want 1", h.timers.len())
	}
	h.timers.at(t, 0).f()

	got := h.ranNumbers()
	if len(got) != 2 || got[0] != 51 || got[1] != 51 {
		t.Fatalf("runs scoped to %v, want [51 51]", got)
	}
}

// store.RetryDue carries the issue the deadline belongs to, and the wake must
// act on that issue alone. Reconciling the whole loop because one of its
// issues came due is the same repository-wide cost a delivery used to pay,
// on a timer.
func TestWakeActsOnTheIssueNamedByTheDeadline(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	seedDeadline(t, db, "planning", 51, workNow.Add(-time.Minute))

	if _, ok := h.w.Wake(context.Background()); !ok {
		t.Fatal("ok = false, want the past deadline")
	}
	got := h.ranNumbers()
	if len(got) != 1 || got[0] != 51 {
		t.Fatalf("wake ran for issues %v, want only [51]", got)
	}
}

// openedClients reports the client each Open was handed, in call order.
func (h *harness) openedClients() []ghub.Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]ghub.Client, 0, len(h.opens))
	for _, o := range h.opens {
		out = append(out, o.opts.GH)
	}
	return out
}

// One delivery, N loops, ONE fetch of the issue it named. Every loop watching
// the repository used to ask GitHub for the same issue at the same instant:
// two loops meant two identical fetches per event, ten meant ten.
func TestOneDeliveryFetchesTheDeliveredIssueOnce(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning"), h.target("execution")}

	h.w.Deliver(context.Background(), "o/r", 51)

	if got := h.gh.fetches(); len(got) != 1 || got[0] != 51 {
		t.Errorf("fetched %v, want exactly [51] for one delivery to two loops", got)
	}
	// The control: both loops really did run. Without it the assertion above
	// would also pass if the second loop had been skipped entirely.
	if got := h.ranLoops(); len(got) != 2 {
		t.Errorf("ran %v, want both loops", got)
	}
}

// The staleness guard, and the reason the shared value dies with Deliver.
//
// The daemon decides from an issue's LABELS, and a delivery exists BECAUSE
// something about the issue changed. A memo that outlived one delivery would
// answer the next one with the labels from before the change that raised it.
// This test must fail if anyone widens that lifetime.
func TestTwoDeliveriesForOneIssueFetchItTwice(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}

	h.w.Deliver(context.Background(), "o/r", 51)
	h.w.Deliver(context.Background(), "o/r", 51)

	if got := h.gh.fetches(); len(got) != 2 {
		t.Errorf("fetched %v, want one fetch per delivery", got)
	}
	if h.clients != 2 {
		t.Errorf("built %d clients, want one per delivery", h.clients)
	}
}

// The sharing is what makes the single fetch possible: every loop of one
// delivery is opened with the SAME client, so the second loop's fetch is
// answered from the first loop's.
func TestEveryLoopOfOneDeliveryIsOpenedWithTheSameClient(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning"), h.target("execution")}

	h.w.Deliver(context.Background(), "o/r", 51)

	got := h.openedClients()
	if len(got) != 2 {
		t.Fatalf("opened %d times, want 2", len(got))
	}
	if got[0] == nil || got[1] == nil {
		t.Fatalf("Open was handed a nil client: %v", got)
	}
	if got[0] != got[1] {
		t.Errorf("each loop was opened with a client of its own; want one per delivery")
	}
}

// The token is machine-wide, so a delivery reads it once instead of once per
// loop. It is still read per delivery, which is what keeps a rotated token
// working without a daemon restart.
func TestTheTokenIsReadOncePerDelivery(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning"), h.target("execution"), h.target("review")}

	h.w.Deliver(context.Background(), "o/r", 51)

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tokenCalls != 1 {
		t.Errorf("read the token %d times, want once for the whole delivery", h.tokenCalls)
	}
	if h.clients != 1 {
		t.Errorf("built %d clients, want one for the whole delivery", h.clients)
	}
}

// A retry fires minutes after the delivery that armed it, and it re-runs
// precisely because something failed. It must look at GitHub afresh: deciding
// it from the labels the failed delivery fetched would make the retry a replay
// of a stale moment rather than a new attempt.
func TestARetryFetchesAgainInsteadOfReusingTheDeliverysFetch(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.backoff = []time.Duration{time.Minute}
	h.runFn = func(*config.Config) error { return errBoom }

	h.w.Deliver(context.Background(), "o/r", 51)
	if h.timers.len() != 1 {
		t.Fatalf("armed %d timers, want 1", h.timers.len())
	}
	h.timers.at(t, 0).f()

	if got := h.gh.fetches(); len(got) != 2 {
		t.Errorf("fetched %v, want the delivery's fetch and a fresh one for the retry", got)
	}
	got := h.openedClients()
	if len(got) != 2 {
		t.Fatalf("opened %d times, want the tick and the retry", len(got))
	}
	if got[0] == got[1] {
		t.Error("the retry reused the delivery's client; it must read GitHub afresh")
	}
}

// Wake serves one loop at a moment of its own, hours after whatever flagged
// the issue. It goes through the same scoped path and gets access of its own.
func TestWakeTicksThroughTheScopedPathWithItsOwnClient(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	seedDeadline(t, db, "planning", 51, workNow.Add(-time.Minute))

	if _, ok := h.w.Wake(context.Background()); !ok {
		t.Fatal("ok = false, want the past deadline")
	}

	if got := h.ranNumbers(); len(got) != 1 || got[0] != 51 {
		t.Fatalf("wake ran for issues %v, want only [51]", got)
	}
	if got := h.gh.fetches(); len(got) != 1 || got[0] != 51 {
		t.Errorf("fetched %v, want the wake's own fetch of [51]", got)
	}
	got := h.openedClients()
	if len(got) != 1 || got[0] == nil {
		t.Fatalf("Open was handed %v, want one non-nil client", got)
	}
}

// A shared fetch fails every loop of the delivery at once, which is the same
// outcome as before -- each would have failed identically a moment apart. What
// must not be lost is WHICH loops it stopped: the failure is still reported
// per loop, and each loop still schedules its own retry.
func TestASharedFetchFailureIsStillAttributedToEveryLoop(t *testing.T) {
	buf := captureLogs(t)
	h := newHarness(nil)
	h.targets = []Target{h.target("planning"), h.target("execution")}
	h.backoff = []time.Duration{time.Minute}
	h.gh.err = errors.New("403 rate limit exceeded")

	h.w.Deliver(context.Background(), "o/r", 51)

	out := buf.String()
	for _, loop := range []string{"planning", "execution"} {
		if !strings.Contains(out, "loop="+loop) {
			t.Errorf("the failure was not attributed to loop %q:\n%s", loop, out)
		}
	}
	if !strings.Contains(out, "rate limit exceeded") {
		t.Errorf("the failure does not carry the error:\n%s", out)
	}
	if n := h.pendingLen(); n != 2 {
		t.Errorf("pending = %d, want a retry for each loop the shared fetch stopped", n)
	}
}
