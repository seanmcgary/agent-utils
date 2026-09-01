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

	mu sync.Mutex
	// These four are set before the first Deliver and then read under mu by
	// open, runTend and runCleanup, so they sit with the guarded fields rather
	// than above the mutex with the write-once ones. defaultBranch and tendPR
	// are the two config fields the open seam builds; both are needed to arm a
	// sweep, which is gated on cfg.TendPR && d.IsMergeInto(cfg.DefaultBranch).
	// tendFn and cleanupFn decide what the RunTend and RunCleanup seams
	// return; nil means success.
	defaultBranch string                         // guarded by mu
	tendPR        bool                           // guarded by mu
	tendFn        func(cfg *config.Config) error // guarded by mu
	cleanupFn     func(cfg *config.Config) error // guarded by mu

	tokenErr error
	openErr  error
	// reapErr is what the ReapOrphans seam returns. lock.ErrHeld is the one
	// worth setting: it is the ordinary outcome when a tick is already
	// running, not a failure.
	reapErr error
	// reaped records the loop of each ReapOrphans call, in order, guarded by
	// mu like ran and tends.
	reaped []string
	// onReap fires after each recorded reap. It is how a Serve test observes
	// a sweep without polling: Serve runs in its own goroutine, and the sweep
	// is the only thing it does that a test can otherwise see.
	onReap       func()
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
	// tends records "loop@base" for each sweep RunTend was asked to run, in
	// order, guarded by mu like ran and ranIssues.
	tends []string
	// cleaned records "loop@prNumber" for each pull request RunCleanup was
	// asked to clean up, in order, guarded by mu like tends.
	cleaned  []string
	cleanups int
	backoff  []time.Duration
	max      int
	// live is what the RunIssue seam reports as Summary.Live: how many of
	// THIS issue's agents are still running. Non-zero means the pass decided
	// nothing because an agent already holds the issue, which is the case the
	// busy re-look exists for.
	live int // guarded by mu
	// busy is what the IssueBusy seam answers. It stands for the production
	// read -- the loop's running dispatch rows, and kill(0) on each pid --
	// which a test has no process to perform.
	busy bool // guarded by mu
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
	w.RunTend = h.runTend
	w.RunCleanup = h.runCleanup
	w.ReapOrphans = h.reap
	w.IssueBusy = h.issueBusy
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
	// Windowing OFF by default. Every test written before windows existed
	// asserts on the timers a delivery arms, and a window in every one of them
	// would shift those assertions without saying anything about what they
	// test. The tests that ARE about windows turn it on.
	w.IssueDelay = 0
	w.BusyDelay = time.Minute
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

// reap is the ReapOrphans seam. It records the loop it was asked to reap and
// reports whatever the test set, without touching a database.
func (h *harness) reap(cfg *config.Config, _ loopcmd.Deps) (loopcmd.Summary, error) {
	h.mu.Lock()
	h.reaped = append(h.reaped, cfg.Name)
	err, notify := h.reapErr, h.onReap
	h.mu.Unlock()

	// Outside the lock: a test's callback may do anything, and holding mu
	// across it would deadlock the moment one reads the harness.
	if notify != nil {
		notify()
	}
	return loopcmd.Summary{}, err
}

// reapedLoops returns the loops ReapOrphans was called for, in order.
func (h *harness) reapedLoops() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.reaped...)
}

func (h *harness) open(ref loopcmd.ProjectRef, path string, o loopcmd.Options) (*config.Config, loopcmd.Deps, func(), error) {
	h.mu.Lock()
	h.opens = append(h.opens, openCall{ref: ref, path: path, opts: o})
	openErr, backoff, max := h.openErr, h.backoff, h.max
	branch, tend := h.defaultBranch, h.tendPR
	h.mu.Unlock()

	if openErr != nil {
		// loopcmd.Open returns a nil cleanup alongside its error. Returning
		// one here is what proves work.go never calls it on that path.
		return nil, loopcmd.Deps{}, nil, openErr
	}

	cfg := &config.Config{
		Name: loopFromPath(path), Repo: "o/r",
		DefaultBranch: branch, TendPR: tend,
	}
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
	fn, live := h.runFn, h.live
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
	return loopcmd.Summary{Live: live}, nil
}

// issueBusy is the fake IssueBusy seam: the answer a test set, with no
// database and no process behind it.
func (h *harness) issueBusy(Target, int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.busy
}

// runTend is the fake RunTend seam. It records "loop@base" for each sweep,
// so a test can prove which loop was swept and against which branch.
func (h *harness) runTend(
	_ context.Context, cfg *config.Config, _ loopcmd.Deps, base string,
) (loopcmd.Summary, error) {
	h.mu.Lock()
	h.tends = append(h.tends, cfg.Name+"@"+base)
	fn := h.tendFn
	h.mu.Unlock()
	if fn != nil {
		return loopcmd.Summary{}, fn(cfg)
	}
	return loopcmd.Summary{}, nil
}

// runCleanup is the fake RunCleanup seam. It records "loop@prNumber" for each
// pull request it was asked to clean up, so a test can prove which loop
// cleaned up which pull request's worktrees.
func (h *harness) runCleanup(
	_ context.Context, cfg *config.Config, _ loopcmd.Deps, prNumber int,
) error {
	h.mu.Lock()
	h.cleaned = append(h.cleaned, fmt.Sprintf("%s@%d", cfg.Name, prNumber))
	fn := h.cleanupFn
	h.mu.Unlock()
	if fn != nil {
		return fn(cfg)
	}
	return nil
}

// deliveryGH is a ghub.Client that records the numbers it was asked to fetch.
// Only the issue fetch is exercised here; the scoped tick's own use of the
// rest is covered in internal/loopcmd.
type deliveryGH struct {
	mu      sync.Mutex
	fetched []int
	err     error
	// openIssues and openPRs are what the two list calls answer. They are
	// what the closure reconcile reads: everything a test dispatched against
	// and did NOT list here is what that pass should mark closed.
	openIssues []int
	openPRs    []int
	// listErr fails both list calls, for the case where one repository cannot
	// be read and must not stop the others.
	listErr error
	// listed records "owner/name" per list call, so a test can prove the pass
	// asked once per repository rather than once per issue.
	listed []string
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
func (f *deliveryGH) ListOpenIssues(_ context.Context, owner, name string) ([]ghub.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = append(f.listed, owner+"/"+name)
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]ghub.Issue, 0, len(f.openIssues))
	for _, n := range f.openIssues {
		out = append(out, ghub.Issue{Number: n, State: "open"})
	}
	return out, nil
}
func (f *deliveryGH) ListOpenPullRequests(context.Context, string, string) ([]ghub.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]ghub.PullRequest, 0, len(f.openPRs))
	for _, n := range f.openPRs {
		out = append(out, ghub.PullRequest{Number: n})
	}
	return out, nil
}

// listCalls returns the repositories the list calls were made against.
func (f *deliveryGH) listCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.listed...)
}
func (f *deliveryGH) BehindBy(context.Context, string, string, string, string) (int, error) {
	return 0, nil
}
func (f *deliveryGH) AuthenticatedLogin(context.Context) (string, error) {
	return "loop-bot", nil
}
func (f *deliveryGH) LatestReviewActivity(context.Context, string, string, int) (time.Time, error) {
	return time.Time{}, nil
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

// clientsBuilt reports how many GitHub clients were built, which is one per
// pass that read GitHub with access of its own.
func (h *harness) clientsBuilt() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clients
}

// pendingLen reports how many loops hold a scheduled retry.
func (h *harness) pendingLen() int {
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	return len(h.w.pending)
}

// tendedLoops returns "loop@base" for each sweep, in order, like ranLoops
// does for issue passes.
func (h *harness) tendedLoops() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.tends...)
}

// cleanedPRs returns "loop@prNumber" for each cleanup, in order, like
// tendedLoops does for sweeps.
func (h *harness) cleanedPRs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.cleaned...)
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

// A tick that lost the race for the loop's lock spends no RETRY budget: a
// held lock is not a failed tick, and a backoff entry burnt here would leave
// the next real failure with a shorter list than it was configured.
//
// It does arm a re-look, which is a different thing and is not on the retry
// path at all; see TestALockHeldTickLooksAgainInsteadOfBeingDropped.
func TestALockHeldTickSchedulesNoRetry(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.backoff = []time.Duration{5 * time.Minute}
	h.runFn = func(*config.Config) error { return fmt.Errorf("run tick: %w", lock.ErrHeld) }

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})

	if n := h.pendingLen(); n != 0 {
		t.Errorf("pending = %d, want 0", n)
	}
	for i := 0; i < h.timers.len(); i++ {
		if got := h.timers.at(t, i).d; got != h.w.BusyDelay {
			t.Errorf("timer %d waits %v; the only timer a held lock may arm is the re-look at %v",
				i, got, h.w.BusyDelay)
		}
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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})
	if n := h.pendingLen(); n != 1 {
		t.Fatalf("pending after a failed tick = %d, want 1", n)
	}

	h.runFn = func(*config.Config) error { return fmt.Errorf("run tick: %w", lock.ErrHeld) }
	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})

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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})

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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})
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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})

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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})

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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})

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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})

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

			h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})

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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})

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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})

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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})
	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})

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
	h.w.Deliver(ctx, Delivery{Repo: "o/r", Number: 7})
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
	h.w.Deliver(ctx, Delivery{Repo: "o/r", Number: 7})
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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})
	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})
	if n := h.timers.len(); n != 2 {
		t.Fatalf("armed %d timers for two Open failures, want 2", n)
	}

	// The database is imported; the next tick reaches Run and fails for a
	// real reason.
	h.mu.Lock()
	h.openErr = nil
	h.mu.Unlock()
	h.runFn = func(*config.Config) error { return errBoom }

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})

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
func TestDeliverLogsTheIssueAndTheLoopsThatWillEvaluateIt(t *testing.T) {
	buf := captureLogs(t)
	h := newHarness(nil)
	h.targets = []Target{h.target("planning"), h.target("execution")}

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 51})

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
	// The line says the issue is EVALUATED in each loop, not acted on. Every
	// watching loop does evaluate it, but most decide nothing -- usually only
	// one has a matching trigger label -- so "acting on" oversold the fan-out
	// and read as the full reconcile this branch removed. What each loop
	// decided is the per-loop tick line's job; it carries a reason now.
	if !strings.Contains(out, "evaluating this issue in every loop") {
		t.Errorf("the fan-out line does not say the issue is evaluated in each loop:\n%s", out)
	}
	if strings.Contains(out, "acting on this issue") {
		t.Errorf("the fan-out line still claims every loop acts on the issue:\n%s", out)
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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 51})

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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 51})
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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 51})

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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 51})
	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 51})

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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 51})

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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 51})

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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 51})
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

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 51})

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

// MergedInto is the ONE field that says "the default branch moved." An empty
// value must never match a branch name, or every ordinary delivery -- an
// opened issue, a moved label -- would start a repository-wide sweep. That is
// the regression Worker.RunIssue records.
func TestIsMergeIntoRequiresAMergedBaseRef(t *testing.T) {
	cases := []struct {
		name string
		d    Delivery
		arg  string
		want bool
	}{
		{"a merge into the branch", Delivery{Repo: "o/r", Number: 7, MergedInto: "master"}, "master", true},
		{"a merge into another branch", Delivery{Repo: "o/r", Number: 7, MergedInto: "feature"}, "master", false},
		{"not a merge", Delivery{Repo: "o/r", Number: 7}, "master", false},
		{"not a merge, and the loop names no branch", Delivery{Repo: "o/r", Number: 7}, "", false},
		{"a merge, but the loop names no branch", Delivery{Repo: "o/r", Number: 7, MergedInto: "master"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.IsMergeInto(tc.arg); got != tc.want {
				t.Errorf("IsMergeInto(%q) = %v, want %v", tc.arg, got, tc.want)
			}
		})
	}
}

// The same rule IsMergeInto states: an empty branch never matches, even
// against an empty PushedTo. A loop with no default_branch names no branch,
// and two absent values are not agreement.
func TestIsPushToRequiresAPushedBranch(t *testing.T) {
	cases := []struct {
		name string
		d    Delivery
		arg  string
		want bool
	}{
		{"a push to the branch", Delivery{Repo: "o/r", PushedTo: "master"}, "master", true},
		{"a push to another branch", Delivery{Repo: "o/r", PushedTo: "master"}, "main", false},
		{"not a push", Delivery{Repo: "o/r", Number: 7}, "master", false},
		{"two empty values", Delivery{}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.IsPushTo(tc.arg); got != tc.want {
				t.Errorf("IsPushTo(%q) = %v, want %v", tc.arg, got, tc.want)
			}
		})
	}
}

// A push names no issue, so the three passes that act on one issue must not
// run. Only the sweep is armed.
func TestPushArmsTheSweepAndRunsNoIssuePass(t *testing.T) {
	h := newHarness(nil)
	tgt := h.target("planning")
	tgt.DefaultBranch = "master"
	tgt.TendPR = true
	h.targets = []Target{tgt}
	h.defaultBranch = "master"
	h.tendPR = true

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", PushedTo: "master"})

	if got := h.ranLoops(); len(got) != 0 {
		t.Errorf("issue passes = %v, want none for a push", got)
	}
	if n := h.timers.len(); n != 1 {
		t.Fatalf("armed %d timers, want 1", n)
	}
	h.timers.at(t, 0).f()
	if got := h.tendedLoops(); len(got) != 1 || got[0] != "planning@master" {
		t.Errorf("tends = %v, want [planning@master]", got)
	}
}

// A merge and the push it produces arrive together. armTend already collapses
// a burst, and this proves the two triggers ride one timer rather than arming
// two sweeps.
func TestAMergeAndItsPushProduceOneSweep(t *testing.T) {
	h := newHarness(nil)
	tgt := h.target("planning")
	tgt.DefaultBranch = "master"
	tgt.TendPR = true
	h.targets = []Target{tgt}
	h.defaultBranch = "master"
	h.tendPR = true

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7, MergedInto: "master"})
	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", PushedTo: "master"})

	if n := h.timers.len(); n != 1 {
		t.Fatalf("armed %d timers, want 1: a merge and its push ride one sweep", n)
	}
}

// A push to a feature branch must cost nothing: no token read, no SQLite
// handle, no migration check. Open is the seam that proves it.
func TestPushToAnotherBranchOpensNothing(t *testing.T) {
	h := newHarness(nil)
	tgt := h.target("planning")
	tgt.DefaultBranch = "master"
	tgt.TendPR = true
	h.targets = []Target{tgt}

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", PushedTo: "feat/x"})

	if len(h.opens) != 0 {
		t.Errorf("Open calls = %d, want 0 for a push to a branch no loop tends", len(h.opens))
	}
	if h.tokenCalls != 0 {
		t.Errorf("token reads = %d, want 0: the push filter runs before access()", h.tokenCalls)
	}
}

// A push's Open failure must not enter the issue retry schedule: pending is
// keyed per loop, not per issue, so entering it would cancel a real issue's
// pending retry and spend the loop's Open budget on issue 0 for nothing.
func TestAPushsOpenFailureDoesNotCancelAnIssuesPendingRetry(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.openErr = errors.New("unimported legacy database")

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7})
	if n := h.timers.len(); n != 1 {
		t.Fatalf("armed %d timers, want 1 for the issue's Open failure", n)
	}

	tgt := h.target("planning")
	tgt.DefaultBranch = "master"
	tgt.TendPR = true
	h.targets = []Target{tgt}

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", PushedTo: "master"})

	if n := h.timers.len(); n != 1 {
		t.Errorf("armed %d timers after a push's Open failure, want still 1: "+
			"a push must not enter the issue retry schedule", n)
	}
	if h.timers.stopped(t, 0) {
		t.Error("the issue's pending retry was cancelled by the push's Open failure")
	}
}

// The sweep is armed for exactly one case, and the issue pass always runs.
func TestDeliverArmsATendSweepOnlyOnAMergeIntoTheLoopsDefaultBranch(t *testing.T) {
	cases := []struct {
		name     string
		delivery Delivery
		tendPR   bool
		wantArm  bool
	}{
		{"a merge into the default branch", Delivery{Repo: "o/r", Number: 7, MergedInto: "master"}, true, true},
		{"a merge into a feature branch", Delivery{Repo: "o/r", Number: 7, MergedInto: "feature"}, true, false},
		{"not a merge", Delivery{Repo: "o/r", Number: 7}, true, false},
		{"a merge, but the loop does not tend", Delivery{Repo: "o/r", Number: 7, MergedInto: "master"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(nil)
			h.targets = []Target{h.target("planning")}
			h.defaultBranch = "master"
			h.tendPR = tc.tendPR

			h.w.Deliver(context.Background(), tc.delivery)

			// The merged pull request's own pass moves its issue to a terminal
			// state, and runs immediately whatever the sweep does.
			if got := h.ranLoops(); len(got) != 1 {
				t.Errorf("issue passes = %d, want 1", len(got))
			}
			want := 0
			if tc.wantArm {
				want = 1
			}
			if got := h.timers.len(); got != want {
				t.Fatalf("armed %d timers, want %d", got, want)
			}
			if want == 1 {
				h.timers.at(t, 0).f()
				if got := h.tendedLoops(); len(got) != 1 || got[0] != "planning@master" {
					t.Errorf("tends = %v, want [planning@master]", got)
				}
			}
		})
	}
}

// A merge train is one sweep, not one per merge. Each sweep can dispatch up to
// maxTendPerSweep agents, and a tend agent that has already finished no longer
// suppresses anything, so an uncoalesced train multiplies.
func TestDeliverCoalescesAMergeTrainIntoOneSweep(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.defaultBranch = "master"
	h.tendPR = true

	for i := 0; i < 5; i++ {
		h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7 + i, MergedInto: "master"})
	}

	if got := h.timers.len(); got != 1 {
		t.Fatalf("armed %d timers, want 1: a train must ride the first timer", got)
	}
	h.timers.at(t, 0).f()
	if got := h.tendedLoops(); len(got) != 1 {
		t.Errorf("tends = %v, want exactly one", got)
	}
	// Every merge still got its own issue pass.
	if got := h.ranLoops(); len(got) != 5 {
		t.Errorf("issue passes = %d, want 5", len(got))
	}
}

// A failing issue pass schedules its retry as before, and the sweep is still
// armed: the base branch moved whatever happened to that one issue.
func TestDeliverArmsATendSweepEvenWhenTheIssuePassFails(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.defaultBranch = "master"
	h.tendPR = true
	h.max = 1
	h.backoff = []time.Duration{0}
	h.runFn = func(*config.Config) error { return errors.New("boom") }

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7, MergedInto: "master"})

	// One retry timer and one sweep timer.
	if got := h.timers.len(); got != 2 {
		t.Fatalf("armed %d timers, want 2 (one retry, one sweep)", got)
	}
}

// A failed sweep is logged and dropped. It must not schedule a retry: the
// retry path re-runs the ISSUE pass, and re-running it for a sweep failure
// would spend that issue's retry budget on something the issue did not do.
func TestDeliverDoesNotRetryAFailedTendSweep(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.defaultBranch = "master"
	h.tendPR = true
	h.max = 3
	h.backoff = []time.Duration{0, 0, 0}
	h.tendFn = func(*config.Config) error { return errors.New("sweep failed") }

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7, MergedInto: "master"})
	if got := h.timers.len(); got != 1 {
		t.Fatalf("armed %d timers, want 1 (the sweep)", got)
	}
	h.timers.at(t, 0).f()

	// The sweep failed. No SECOND timer may exist.
	if got := h.timers.len(); got != 1 {
		t.Errorf("armed %d timers after a failed sweep, want 1: a sweep must not schedule a retry", got)
	}
	if got := h.pendingLen(); got != 0 {
		t.Errorf("pending retries = %d, want 0", got)
	}
}

// A closed pull request runs cleanup at once -- no timer, unlike a sweep --
// and only when the delivery reports a close.
func TestDeliverRunsCleanupOnlyWhenTheDeliveryClosedAPullRequest(t *testing.T) {
	cases := []struct {
		name     string
		delivery Delivery
		wantRun  bool
	}{
		{"a merged close", Delivery{Repo: "o/r", Number: 11, MergedInto: "master", ClosedPR: true}, true},
		{"an unmerged close", Delivery{Repo: "o/r", Number: 11, ClosedPR: true}, true},
		{"not a close", Delivery{Repo: "o/r", Number: 11}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(nil)
			h.targets = []Target{h.target("planning")}

			h.w.Deliver(context.Background(), tc.delivery)

			got := h.cleanedPRs()
			if tc.wantRun {
				if len(got) != 1 || got[0] != "planning@11" {
					t.Errorf("cleaned = %v, want [planning@11]", got)
				}
			} else if len(got) != 0 {
				t.Errorf("cleaned = %v, want none", got)
			}
		})
	}
}

// Cleanup runs even when the loop does not tend and even when the merge
// landed on a branch other than the loop's default -- ClosedPR is the only
// gate, unlike the sweep, which also checks TendPR and IsMergeInto.
func TestDeliverRunsCleanupRegardlessOfTendPRAndDefaultBranch(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.tendPR = false
	h.defaultBranch = "master"

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 11, MergedInto: "feature", ClosedPR: true})

	if got := h.cleanedPRs(); len(got) != 1 || got[0] != "planning@11" {
		t.Errorf("cleaned = %v, want [planning@11]", got)
	}
	// No sweep was armed: the merge did not land on this loop's default
	// branch, and this loop does not tend either way.
	if got := h.timers.len(); got != 0 {
		t.Errorf("armed %d timers, want 0", got)
	}
}

// A failed cleanup is logged and dropped, exactly like a failed sweep: it
// must not schedule a retry, because the retry path re-runs the issue pass,
// which did nothing wrong.
func TestDeliverDoesNotRetryAFailedCleanup(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.max = 3
	h.backoff = []time.Duration{0, 0, 0}
	h.cleanupFn = func(*config.Config) error { return errors.New("cleanup failed") }

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 11, ClosedPR: true})

	if got := h.timers.len(); got != 0 {
		t.Errorf("armed %d timers, want 0: a failed cleanup must not schedule a retry", got)
	}
	if got := h.pendingLen(); got != 0 {
		t.Errorf("pending retries = %d, want 0", got)
	}
}

// A daemon told to stop must not dispatch a batch of rebase agents on the way
// out. stopAll stops armed tend timers for the same reason it stops retry
// timers; nothing pinned that, and deleting the loop left the suite green.
func TestStopAllStopsAnArmedTendSweep(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.defaultBranch = "master"
	h.tendPR = true

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7, MergedInto: "master"})
	if got := h.timers.len(); got != 1 {
		t.Fatalf("armed %d timers, want 1", got)
	}

	h.w.stopAll()

	h.w.mu.Lock()
	n := len(h.w.tends)
	h.w.mu.Unlock()
	if n != 0 {
		t.Errorf("tends holds %d entries after stopAll, want 0", n)
	}
}

// The timer can already be waiting on the mutex when shutdown begins, past
// anything stopAll could have stopped. The callback re-checks the context for
// exactly that case, and a daemon told to stop starts no new agent.
func TestAnArmedTendSweepDoesNotRunAfterTheContextIsCancelled(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.defaultBranch = "master"
	h.tendPR = true

	ctx, cancel := context.WithCancel(context.Background())
	h.w.Deliver(ctx, Delivery{Repo: "o/r", Number: 7, MergedInto: "master"})
	if got := h.timers.len(); got != 1 {
		t.Fatalf("armed %d timers, want 1", got)
	}

	cancel()
	h.timers.at(t, 0).f()

	if got := h.tendedLoops(); len(got) != 0 {
		t.Errorf("tends = %v, want none: a cancelled daemon must start no sweep", got)
	}
}

// sweptNumbers wires RunEpic to record the issues it was asked to sweep. The
// slice is guarded to match every other accessor in this file, though Deliver
// ticks its targets sequentially (see Worker.Deliver), not concurrently.
func sweptNumbers(h *harness) (*[]int, *sync.Mutex) {
	var mu sync.Mutex
	var swept []int
	h.w.RunEpic = func(_ context.Context, _ *config.Config, _ loopcmd.Deps, closed int) (loopcmd.Summary, error) {
		mu.Lock()
		defer mu.Unlock()
		swept = append(swept, closed)
		return loopcmd.Summary{}, nil
	}
	return &swept, &mu
}

// An issue closing is what unblocks its siblings. It is the ONLY event that
// starts an epic sweep.
func TestClosedIssueRunsTheEpicSweep(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	swept, mu := sweptNumbers(h)

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 71, ClosedIssue: true})

	mu.Lock()
	defer mu.Unlock()
	if len(*swept) != 1 || (*swept)[0] != 71 {
		t.Fatalf("epic sweep ran for %v, want [71]", *swept)
	}
}

func TestANonCloseDeliveryRunsNoEpicSweep(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	swept, mu := sweptNumbers(h)

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 71})

	mu.Lock()
	defer mu.Unlock()
	if len(*swept) != 0 {
		t.Fatalf("epic sweep ran for %v; only a closed issue starts one", *swept)
	}
}

// A merged pull request is not an issue close. ClosedPR arms the worktree
// cleanup and the tend sweep; it must arm nothing here.
func TestAMergedPullRequestRunsNoEpicSweep(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	swept, mu := sweptNumbers(h)

	h.w.Deliver(context.Background(), Delivery{
		Repo: "o/r", Number: 71, MergedInto: "master", ClosedPR: true,
	})

	mu.Lock()
	defer mu.Unlock()
	if len(*swept) != 0 {
		t.Fatalf("epic sweep ran for %v on a pull request delivery", *swept)
	}
}

// The closed issue's own pass is what moves ITS labels. The sweep is extra
// work, not a replacement, and a failing sweep must not cost the issue its
// pass.
func TestTheClosedIssuesOwnPassStillRuns(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.w.RunEpic = func(context.Context, *config.Config, loopcmd.Deps, int) (loopcmd.Summary, error) {
		return loopcmd.Summary{}, errBoom
	}

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 71, ClosedIssue: true})

	if got := h.ranNumbers(); len(got) != 1 || got[0] != 71 {
		t.Fatalf("the issue pass ran for %v, want [71]", got)
	}
	// A failed sweep schedules NO retry: the cron sweep re-derives the whole
	// thing from scratch, so there is nothing here for a retry to recover.
	if n := h.timers.len(); n != 0 {
		t.Errorf("armed %d retry timers for a failed sweep, want 0", n)
	}
}

// --- Delivery bursts and the busy re-look -----------------------------------
//
// One edit to an issue's labels is one delivery PER LABEL. Removing three
// labels and adding one, in a single edit in the GitHub UI, is four
// deliveries inside half a second. See armIssueWindow.

// The first delivery of a burst still ticks at once -- the fast path is the
// point of the daemon -- and the three behind it collapse into the ONE
// trailing tick that reads the settled labels.
func TestABurstForOneIssueTicksOnceAtEachEdge(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.w.IssueDelay = 2 * time.Second

	for i := 0; i < 4; i++ {
		h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 15})
	}

	// The leading tick ran during the first Deliver; the other three armed
	// nothing of their own.
	if got := h.ranLoops(); len(got) != 1 {
		t.Fatalf("issue passes during the burst = %d, want 1 (the leading edge)", len(got))
	}
	if got := h.timers.len(); got != 1 {
		t.Fatalf("armed %d timers, want 1: a burst rides one window", got)
	}
	if got := h.timers.at(t, 0).d; got != h.w.IssueDelay {
		t.Errorf("window delay = %v, want IssueDelay %v", got, h.w.IssueDelay)
	}

	h.timers.at(t, 0).f()

	if got := h.ranLoops(); len(got) != 2 {
		t.Fatalf("issue passes after the window closed = %d, want 2", len(got))
	}
	if got := h.ranNumbers(); got[1] != 15 {
		t.Errorf("the trailing tick decided issue %d, want 15", got[1])
	}
}

// A lone delivery is the common case and must not pay for the burst case: it
// ticks once and the window closes with nothing behind it.
func TestALoneDeliveryDoesNotTickTwice(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.w.IssueDelay = 2 * time.Second

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 15})
	if got := h.ranLoops(); len(got) != 1 {
		t.Fatalf("issue passes = %d, want 1", len(got))
	}

	// The window still closes; it just has nothing to run.
	h.timers.at(t, 0).f()
	if got := h.ranLoops(); len(got) != 1 {
		t.Errorf("issue passes after an empty window closed = %d, want 1", len(got))
	}
}

// The window is per ISSUE. A burst on one issue must never swallow another
// issue's delivery, which is a different decision entirely.
func TestABurstOnOneIssueDoesNotSuppressAnother(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.w.IssueDelay = 2 * time.Second

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 15})
	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 15})
	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 16})

	got := h.ranNumbers()
	if len(got) != 2 || got[0] != 15 || got[1] != 16 {
		t.Fatalf("leading ticks = %v, want [15 16]", got)
	}
	if n := h.timers.len(); n != 2 {
		t.Fatalf("armed %d windows, want 2 (one per issue)", n)
	}
}

// The whole point. A tick that decided nothing because an agent already holds
// the issue must come back; today it returns and the issue waits for an
// unrelated future delivery. Issue #15 of 2026-08-27 sat for hours this way.
func TestAPassThatFindsItsIssueBusyLooksAgain(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.live = 1

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 15})

	if n := h.timers.len(); n != 1 {
		t.Fatalf("armed %d timers, want 1 (the busy re-look)", n)
	}
	if got := h.timers.at(t, 0).d; got != h.w.BusyDelay {
		t.Errorf("busy delay = %v, want BusyDelay %v", got, h.w.BusyDelay)
	}
	if n := h.pendingLen(); n != 0 {
		t.Errorf("pending = %d, want 0: a busy issue is not a FAILED tick", n)
	}
}

// While the agent is still running, the re-look costs a dispatch-row read and
// a kill(0). It must not tick, which would mean a GitHub fetch a minute for
// the whole life of an eight-hour agent.
func TestABusyRelookDoesNotTickWhileTheAgentIsStillRunning(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.live = 1
	h.busy = true

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 15})
	before := len(h.ranLoops())

	h.timers.at(t, 0).f()

	if got := h.ranLoops(); len(got) != before {
		t.Errorf("issue passes = %d, want %d: a live agent must not be ticked around", len(got), before)
	}
	if n := h.timers.len(); n != 2 {
		t.Fatalf("armed %d timers, want 2: the re-look must arm another", n)
	}
	if got := h.timers.at(t, 1).d; got != h.w.BusyDelay {
		t.Errorf("second busy delay = %v, want %v", got, h.w.BusyDelay)
	}
}

// Once the agent's process is gone the re-look runs a real tick, which is
// what finally dispatches the work the trigger label has been asking for.
func TestABusyRelookTicksOnceTheAgentHasExited(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.live = 1
	h.busy = true

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 15})
	before := len(h.ranLoops())

	// The agent exits, and this pass is no longer busy.
	h.busy = false
	h.live = 0
	h.timers.at(t, 0).f()

	got := h.ranLoops()
	if len(got) != before+1 {
		t.Fatalf("issue passes = %d, want %d", len(got), before+1)
	}
	if n := h.ranNumbers(); n[len(n)-1] != 15 {
		t.Errorf("the re-look decided issue %d, want 15", n[len(n)-1])
	}
}

// A busy re-look re-reads its own labels rather than replaying the delivery's
// fetch. The agent it waited for changes labels as it finishes, so deciding
// from the burst's snapshot would decide from before the handoff.
func TestABusyRelookFetchesAgainInsteadOfReusingTheDeliverysFetch(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.live = 1

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 15})
	fetches := len(h.gh.fetches())

	h.live = 0
	h.timers.at(t, 0).f()

	if got := len(h.gh.fetches()); got != fetches+1 {
		t.Errorf("issue fetches = %d, want %d: the re-look must read GitHub again", got, fetches+1)
	}
	if got := h.clientsBuilt(); got < 2 {
		t.Errorf("clients built = %d, want at least 2: the re-look needs its own", got)
	}
}

// A held lock is the OTHER way a pass decides nothing, and it was dropped for
// the same wrong reason. It still schedules no retry -- it is not a failure --
// but it must be looked at again. tendPass already reached this conclusion for
// sweeps; see its ErrHeld branch.
func TestALockHeldTickLooksAgainInsteadOfBeingDropped(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.runFn = func(*config.Config) error { return fmt.Errorf("run tick: %w", lock.ErrHeld) }

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 15})

	if n := h.pendingLen(); n != 0 {
		t.Errorf("pending = %d, want 0: a held lock is not a failed tick", n)
	}
	if n := h.timers.len(); n != 1 {
		t.Fatalf("armed %d timers, want 1 (the busy re-look)", n)
	}
	if got := h.timers.at(t, 0).d; got != h.w.BusyDelay {
		t.Errorf("busy delay = %v, want %v", got, h.w.BusyDelay)
	}
}

// A busy re-look already armed is ridden, not doubled: an issue with two
// loops' deliveries landing on it must not accumulate one timer per delivery
// for as long as the agent runs.
func TestABusyRelookIsArmedOnlyOncePerIssue(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.live = 1

	// Two passes that both find the issue busy: the delivery's own, and the
	// one the second delivery's trailing tick runs.
	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 15})
	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 15})

	busy := 0
	for i := 0; i < h.timers.len(); i++ {
		if h.timers.at(t, i).d == h.w.BusyDelay {
			busy++
		}
	}
	if busy != 1 {
		t.Errorf("armed %d busy re-looks, want 1: the second pass must ride the first", busy)
	}
}

// A daemon told to stop starts no new agent, and an armed window or re-look
// is exactly the timer that would.
func TestStopAllStopsArmedWindowsAndBusyRelooks(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.w.IssueDelay = 2 * time.Second
	h.live = 1

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 15})
	if n := h.timers.len(); n != 2 {
		t.Fatalf("armed %d timers, want 2", n)
	}

	h.w.stopAll()

	for i := 0; i < 2; i++ {
		if !h.timers.stopped(t, i) {
			t.Errorf("timer %d was left armed after stopAll", i)
		}
	}
}

// Windowing is ON in production (NewWorker sets IssueDelay), so the paths a
// delivery drives beside the issue pass must still work with a window open.
// The window gates the ISSUE pass alone.
func TestAWindowedDeliveryStillRetriesAndTends(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.w.IssueDelay = 2 * time.Second
	h.defaultBranch = "master"
	h.tendPR = true
	h.max = 1
	h.backoff = []time.Duration{5 * time.Minute}
	h.runFn = func(*config.Config) error { return errBoom }

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7, MergedInto: "master"})

	// A window, the failed pass's retry, and the merge's sweep -- each told
	// apart by the wait it asked for.
	waits := map[time.Duration]int{}
	for i := 0; i < h.timers.len(); i++ {
		waits[h.timers.at(t, i).d]++
	}
	want := map[time.Duration]int{
		h.w.IssueDelay:   1, // the window
		5 * time.Minute:  1, // the retry, at the loop's own backoff
		defaultTendDelay: 1, // the sweep
	}
	for d, n := range want {
		if waits[d] != n {
			t.Errorf("timers waiting %v = %d, want %d (armed: %v)", d, waits[d], n, waits)
		}
	}

	// The sweep still runs: the window gates the ISSUE pass and nothing else.
	for i := 0; i < h.timers.len(); i++ {
		if h.timers.at(t, i).d == defaultTendDelay {
			h.timers.at(t, i).f()
		}
	}
	if got := h.tendedLoops(); len(got) != 1 || got[0] != "planning@master" {
		t.Errorf("tends = %v, want [planning@master]", got)
	}
}

// A daemon told to stop starts no new agent, and a window's trailing tick is
// exactly the timer that would. It mirrors the same rule for a tend sweep.
func TestATrailingTickDoesNotRunAfterTheContextIsCancelled(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.w.IssueDelay = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	h.w.Deliver(ctx, Delivery{Repo: "o/r", Number: 15})
	h.w.Deliver(ctx, Delivery{Repo: "o/r", Number: 15})
	before := len(h.ranLoops())
	cancel()

	h.timers.at(t, 0).f()

	if got := h.ranLoops(); len(got) != before {
		t.Errorf("issue passes = %d, want %d: a cancelled context runs no trailing tick", len(got), before)
	}
}

// The production IssueBusy reads dispatch rows and the process table, never
// GitHub. A row whose runner is this test process counts as busy; one whose
// pid cannot exist does not.
func TestIssueBusyAnswersFromTheDispatchRows(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	tgt := h.target("planning")

	// A row for a runner that is definitely gone: pid 0 is never alive, and
	// proc.IsAlive rejects it before it touches the process table.
	id, err := db.Project(workProject).CreateDispatch(store.Dispatch{
		ProjectID: workProject, Loop: "planning", Repo: "o/r", Number: 15,
		Kind: "start", Status: store.StatusRunning, StartedAt: workNow,
	})
	if err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}
	if err := db.Project(workProject).SetDispatchProcess(id, 0, workNow); err != nil {
		t.Fatalf("SetDispatchProcess: %v", err)
	}

	if h.w.issueBusy(tgt, 15) {
		t.Error("issueBusy = true for a dispatch whose runner is gone")
	}
	// Another issue's live row must not make this issue busy.
	if h.w.issueBusy(tgt, 16) {
		t.Error("issueBusy = true for an issue with no dispatch at all")
	}
}

// A database that cannot be read is not evidence an agent is alive. Answering
// "busy" there would re-arm on the same broken read for as long as the daemon
// ran and never tick; answering "not busy" sends it down the full pass, which
// re-reads under the loop's lock.
func TestIssueBusyIsFalseWhenTheDatabaseCannotBeRead(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	db.Close()

	if h.w.issueBusy(h.target("planning"), 15) {
		t.Error("issueBusy = true on a database it could not read")
	}
}

// --- the periodic tend check ---------------------------------------------

// tendCheckHarness returns a harness with TWO registered tending loops and
// the ScanTargets seam set.
//
// Setting that seam is not optional in any test below. It is the ONLY routing
// seam tendCheckPass uses, and the default reads this machine's real
// registry: a test that left it alone would find no target, call
// RunTendCheck never, and then pass while asserting nothing.
//
// Two loops, not one, because the whole reason this pass exists is that it
// walks the registry. With a single target, a pass that checked the first loop
// and returned would satisfy every assertion in this file -- while every other
// project on the machine went on stranding its pull requests.
func tendCheckHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(nil)
	// The open seam builds cfg from these two, and both are read downstream:
	// tendCheckOne arms the sweep against cfg.DefaultBranch, and
	// loopcmd.TendCheck itself refuses a loop with tend_pr false.
	h.defaultBranch = "master"
	h.tendPR = true
	h.scanTending("planning", "execution")
	return h
}

// tendTarget is the harness's target for one loop, marked as tending.
//
// DefaultBranch and TendPR are set here as well as on the config the open seam
// builds: the pass filters on Target.TendPR before it opens anything, and the
// harness's target helper leaves both fields zero.
func (h *harness) tendTarget(loop string) Target {
	t := h.target(loop)
	t.DefaultBranch = "master"
	t.TendPR = true
	return t
}

// scanTending points the ScanTargets seam at exactly these loops, all tending.
//
// A test that is about the per-loop confirm schedule narrows the scan to one
// loop with it: the schedule is kept per loop, so a second loop would double
// every recorded flag and say nothing extra.
func (h *harness) scanTending(loops ...string) {
	targets := make([]Target, 0, len(loops))
	for _, loop := range loops {
		targets = append(targets, h.tendTarget(loop))
	}
	h.w.ScanTargets = func() (Routes, error) {
		return Routes{Targets: targets}, nil
	}
}

// fireTends runs every armed tend sweep timer, told apart from the other
// timers by the wait it asked for.
func fireTends(t *testing.T, h *harness) {
	t.Helper()
	for i := 0; i < h.timers.len(); i++ {
		if h.timers.at(t, i).d == h.w.TendDelay {
			h.timers.at(t, i).f()
		}
	}
}

// The pass walks the registry, not the deliveries. That is what makes it
// reach a project whose webhook is missing, which is the failure this
// periodic check exists for.
func TestTendCheckPassArmsASweepForEachStaleLoop(t *testing.T) {
	h := tendCheckHarness(t)
	var checked []string
	h.w.RunTendCheck = func(
		_ context.Context, cfg *config.Config, _ loopcmd.Deps, _ bool,
	) (loopcmd.TendCheckResult, error) {
		checked = append(checked, cfg.Name)
		return loopcmd.TendCheckResult{Stale: 1, Confirmed: true}, nil
	}

	h.w.tendCheckPass(context.Background())

	if len(checked) != 2 || checked[0] != "planning" || checked[1] != "execution" {
		t.Fatalf("checked = %v, want [planning execution]", checked)
	}
	fireTends(t, h)
	got := h.tendedLoops()
	if len(got) != 2 || got[0] != "planning@master" || got[1] != "execution@master" {
		t.Errorf("tends = %v, want [planning@master execution@master]", got)
	}
}

// A loop that finds nothing arms nothing. A pass that armed a sweep anyway
// would dispatch the agents the gate exists to save.
func TestTendCheckPassArmsNothingWhenNothingIsStale(t *testing.T) {
	h := tendCheckHarness(t)
	h.w.RunTendCheck = func(
		context.Context, *config.Config, loopcmd.Deps, bool,
	) (loopcmd.TendCheckResult, error) {
		return loopcmd.TendCheckResult{}, nil
	}

	h.w.tendCheckPass(context.Background())

	if h.timers.len() != 0 {
		t.Errorf("timers armed = %d, want 0", h.timers.len())
	}
	fireTends(t, h)
	if got := h.tendedLoops(); len(got) != 0 {
		t.Errorf("tends = %v, want none", got)
	}
}

// A loop that does not tend is not even opened. Opening it would spend a
// SQLite handle and a git fetch on a loop whose answer is known.
func TestTendCheckPassSkipsALoopThatDoesNotTend(t *testing.T) {
	h := tendCheckHarness(t)
	target := h.target("planning")
	h.w.ScanTargets = func() (Routes, error) {
		return Routes{Targets: []Target{target}}, nil
	}
	checks := 0
	h.w.RunTendCheck = func(
		context.Context, *config.Config, loopcmd.Deps, bool,
	) (loopcmd.TendCheckResult, error) {
		checks++
		return loopcmd.TendCheckResult{}, nil
	}

	h.w.tendCheckPass(context.Background())

	if checks != 0 {
		t.Errorf("checks = %d, want 0", checks)
	}
	if opens, _, _ := h.counts(); opens != 0 {
		t.Errorf("opens = %d, want 0", opens)
	}
}

// The first pass after start forces the confirm, so a cold pr_links cache is
// populated instead of gating forever on rows that do not exist. The second
// pass does not force it, which is what keeps the interval cheap.
func TestTendCheckPassForcesTheFirstPassOnly(t *testing.T) {
	h := tendCheckHarness(t)
	h.scanTending("planning")
	var forced []bool
	h.w.RunTendCheck = func(
		_ context.Context, _ *config.Config, _ loopcmd.Deps, force bool,
	) (loopcmd.TendCheckResult, error) {
		forced = append(forced, force)
		return loopcmd.TendCheckResult{Confirmed: true}, nil
	}

	h.w.tendCheckPass(context.Background())
	h.w.tendCheckPass(context.Background())

	if len(forced) != 2 || !forced[0] || forced[1] {
		t.Errorf("force flags = %v, want [true false]", forced)
	}
}

// Six hours later the confirm runs again, so a row that drifted with no
// delivery is corrected.
func TestTendCheckPassForcesTheConfirmEverySixHours(t *testing.T) {
	h := tendCheckHarness(t)
	h.scanTending("planning")
	// A clock of its own, because the harness's is frozen: what this test is
	// about is time passing between two passes.
	now := workNow
	h.w.Now = func() time.Time { return now }
	var forced []bool
	h.w.RunTendCheck = func(
		_ context.Context, _ *config.Config, _ loopcmd.Deps, force bool,
	) (loopcmd.TendCheckResult, error) {
		forced = append(forced, force)
		return loopcmd.TendCheckResult{Confirmed: true}, nil
	}

	h.w.tendCheckPass(context.Background())
	now = now.Add(tendConfirmInterval + time.Hour)
	h.w.tendCheckPass(context.Background())

	if len(forced) != 2 || !forced[1] {
		t.Errorf("force flags = %v, want the second pass forced too", forced)
	}
}

// A pass that never confirmed keeps forcing. Recording the attempt rather
// than the confirmation would leave a loop whose check errored out gated on
// rows it never got.
func TestTendCheckPassKeepsForcingUntilAConfirmHappens(t *testing.T) {
	h := tendCheckHarness(t)
	h.scanTending("planning")
	var forced []bool
	h.w.RunTendCheck = func(
		_ context.Context, _ *config.Config, _ loopcmd.Deps, force bool,
	) (loopcmd.TendCheckResult, error) {
		forced = append(forced, force)
		return loopcmd.TendCheckResult{}, nil
	}

	h.w.tendCheckPass(context.Background())
	h.w.tendCheckPass(context.Background())

	if len(forced) != 2 || !forced[0] || !forced[1] {
		t.Errorf("force flags = %v, want both forced", forced)
	}
}

// The sweep is armed with the DAEMON's context, never with the bounded one
// the pass gives its own git and database work.
//
// armTend's timer tests ctx.Err() before it sweeps, so arming with the
// bounded context would cancel the sweep the moment the pass returned -- and
// the pass returning is exactly what cancels it. Nothing else in this file
// catches that, because both contexts are alive while the pass runs.
func TestTendCheckPassArmsWithTheDaemonContextNotItsOwnDeadline(t *testing.T) {
	h := tendCheckHarness(t)
	h.scanTending("planning")
	var checkCtx context.Context
	h.w.RunTendCheck = func(
		ctx context.Context, _ *config.Config, _ loopcmd.Deps, _ bool,
	) (loopcmd.TendCheckResult, error) {
		checkCtx = ctx
		return loopcmd.TendCheckResult{Stale: 1, Confirmed: true}, nil
	}

	h.w.tendCheckPass(context.Background())

	if checkCtx == nil {
		t.Fatal("the check was never run")
	}
	if checkCtx.Err() == nil {
		t.Fatal("the pass's own context outlived the pass; it must be cancelled on return")
	}
	fireTends(t, h)
	if got := h.tendedLoops(); len(got) != 1 || got[0] != "planning@master" {
		t.Errorf("tends = %v, want [planning@master]; the sweep was armed with the bounded context", got)
	}
}

// A registry that cannot be read stops the pass before it reads the token,
// and arms nothing.
func TestTendCheckPassStopsWhenTheScanFails(t *testing.T) {
	h := tendCheckHarness(t)
	h.w.ScanTargets = func() (Routes, error) { return Routes{}, errBoom }
	h.w.RunTendCheck = func(
		context.Context, *config.Config, loopcmd.Deps, bool,
	) (loopcmd.TendCheckResult, error) {
		t.Error("the check ran for a scan that failed")
		return loopcmd.TendCheckResult{}, nil
	}

	h.w.tendCheckPass(context.Background())

	if h.tokenCalls != 0 {
		t.Errorf("token reads = %d, want 0", h.tokenCalls)
	}
}

// Open holds a SQLite handle, and this pass calls Open once per loop per
// interval for the life of the daemon: a cleanup missed on the error path is
// one handle leaked every interval, forever.
func TestTendCheckPassReleasesTheLoopEvenWhenTheCheckFails(t *testing.T) {
	h := tendCheckHarness(t)
	h.scanTending("planning")
	h.w.RunTendCheck = func(
		context.Context, *config.Config, loopcmd.Deps, bool,
	) (loopcmd.TendCheckResult, error) {
		return loopcmd.TendCheckResult{}, errBoom
	}

	h.w.tendCheckPass(context.Background())

	if _, _, cleanups := h.counts(); cleanups != 1 {
		t.Errorf("cleanups = %d, want 1", cleanups)
	}
}

// One loop's failure must not strand every loop after it in the scan.
//
// This is the pass's own failure mode at machine scale: the loops share
// nothing but this goroutine, and "logged and dropped, the next interval tries
// again" is only true if the pass reaches the next interval having checked
// everything else.
func TestTendCheckPassChecksTheNextLoopAfterOneCheckFails(t *testing.T) {
	h := tendCheckHarness(t)
	var checked []string
	h.w.RunTendCheck = func(
		_ context.Context, cfg *config.Config, _ loopcmd.Deps, _ bool,
	) (loopcmd.TendCheckResult, error) {
		checked = append(checked, cfg.Name)
		if cfg.Name == "planning" {
			return loopcmd.TendCheckResult{}, errBoom
		}
		return loopcmd.TendCheckResult{Stale: 1, Confirmed: true}, nil
	}

	h.w.tendCheckPass(context.Background())

	if len(checked) != 2 || checked[1] != "execution" {
		t.Fatalf("checked = %v, want the second loop checked after the first failed", checked)
	}
	fireTends(t, h)
	if got := h.tendedLoops(); len(got) != 1 || got[0] != "execution@master" {
		t.Errorf("tends = %v, want [execution@master]", got)
	}
}

// The same, one layer earlier: a loop whose database or config cannot be
// opened at all. It schedules no retry -- the pass names no issue whose budget
// could pay for one -- so the only thing that saves the loops behind it is the
// pass continuing.
func TestTendCheckPassChecksTheNextLoopAfterOneCannotBeOpened(t *testing.T) {
	h := tendCheckHarness(t)
	// Wrapped rather than set through h.openErr, which would fail both loops
	// and prove nothing about continuing.
	open := h.w.Open
	h.w.Open = func(
		ref loopcmd.ProjectRef, path string, o loopcmd.Options,
	) (*config.Config, loopcmd.Deps, func(), error) {
		if loopFromPath(path) == "planning" {
			return nil, loopcmd.Deps{}, nil, errBoom
		}
		return open(ref, path, o)
	}
	var checked []string
	h.w.RunTendCheck = func(
		_ context.Context, cfg *config.Config, _ loopcmd.Deps, _ bool,
	) (loopcmd.TendCheckResult, error) {
		checked = append(checked, cfg.Name)
		return loopcmd.TendCheckResult{}, nil
	}

	h.w.tendCheckPass(context.Background())

	if len(checked) != 1 || checked[0] != "execution" {
		t.Errorf("checked = %v, want [execution]", checked)
	}
	if h.timers.len() != 0 {
		t.Errorf("timers armed = %d, want 0; a failed Open schedules no retry here", h.timers.len())
	}
}

// A token that cannot be read stops the pass before it opens anything.
//
// The token is machine-wide, so every loop would fail on the identical read --
// and there is no access for a loop to be checked WITH, which is what makes
// carrying on past this a nil dereference on the wake goroutine rather than a
// wasted pass.
func TestTendCheckPassStopsWhenTheTokenCannotBeRead(t *testing.T) {
	h := tendCheckHarness(t)
	h.tokenErr = errBoom
	h.w.RunTendCheck = func(
		context.Context, *config.Config, loopcmd.Deps, bool,
	) (loopcmd.TendCheckResult, error) {
		t.Error("the check ran without a token")
		return loopcmd.TendCheckResult{}, nil
	}

	h.w.tendCheckPass(context.Background())

	if opens, _, _ := h.counts(); opens != 0 {
		t.Errorf("opens = %d, want 0", opens)
	}
}

// Zero disables the pass outright.
//
// Asserted on the ticker rather than on Serve: Serve returns at its own
// ctx.Err() guard before it reaches the select, so a cancelled-context test
// passes identically with the pass enabled.
func TestTendTickerIsNilOnlyWhenTheIntervalIsZero(t *testing.T) {
	h := newHarness(nil)

	h.w.TendInterval = 0
	ch, stop := h.w.tendTicker()
	if ch != nil {
		t.Error("a zero interval must build no ticker; a nil channel blocks forever, which is the disable")
	}
	// Called, not merely non-nil: Serve defers it unconditionally, so a
	// disabled pass returning a nil func would panic the daemon on shutdown.
	stop()

	h.w.TendInterval = time.Minute
	ch, stop = h.w.tendTicker()
	if ch == nil {
		t.Error("a non-zero interval must build a ticker")
	}
	stop()
}

// The spec says a push skips the issue pass, the epic pass AND the cleanup
// pass. Only the issue pass had a d.Number > 0 guard; the other two were gated
// on d.ClosedIssue/d.ClosedPR alone, which was safe only because the handler
// never sets those for a push -- an invariant living in another file that
// nothing pinned. Both passes read d.Number as their subject, so a push that
// reached them would sweep the epic of issue 0 and remove the worktree "pr-0".
func TestAPushRunsNeitherTheEpicPassNorTheCleanupPass(t *testing.T) {
	h := newHarness(nil)
	tgt := h.target("planning")
	tgt.DefaultBranch = "master"
	tgt.TendPR = true
	h.targets = []Target{tgt}
	h.defaultBranch = "master"
	h.tendPR = true
	swept, mu := sweptNumbers(h)

	// ClosedIssue and ClosedPR are set deliberately: the handler does not set
	// them for a push, and this test exists so tickOne does not depend on
	// that.
	h.w.Deliver(context.Background(), Delivery{
		Repo: "o/r", PushedTo: "master", ClosedIssue: true, ClosedPR: true,
	})

	mu.Lock()
	got := append([]int(nil), *swept...)
	mu.Unlock()
	if len(got) != 0 {
		t.Errorf("epic sweeps = %v, want none for a push", got)
	}
	if cleaned := h.cleanedPRs(); len(cleaned) != 0 {
		t.Errorf("cleaned = %v, want none for a push", cleaned)
	}
}
