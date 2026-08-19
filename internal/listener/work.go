package listener

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/loopcmd"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// Default delays. They are defaults on a struct field rather than constants
// used directly, so a test can shrink them and never sleep for a real delay.
const (
	// defaultOpenRetryDelay is the wait after loopcmd.Open itself failed.
	// Open reads the registry, the loop's configuration and the state
	// database, so its failures are the slow-moving kind an operator fixes
	// (a broken yaml file, an unimported legacy database); retrying sooner
	// only repeats the same error in the log.
	defaultOpenRetryDelay = time.Minute
	// defaultMinRetryDelay is the floor under every backoff entry. The
	// migrated first entry is 0s, and an unfloored delay would spend the
	// whole retry budget as fast as retry.max GitHub calls can be made.
	defaultMinRetryDelay = 30 * time.Second
	// defaultMinWakeInterval is the floor under Serve's wait. A clock skew
	// or a row whose deadline stays in the past would otherwise spin the
	// wake loop.
	defaultMinWakeInterval = 30 * time.Second
)

// openRetryMax caps the retries for a failure that happened inside
// loopcmd.Open. The loop's own retry.max lives in the configuration Open
// could not load, so it is not knowable on that path; without a cap here, a
// config file with a typo in it would retry once every OpenRetryDelay for as
// long as the daemon runs. After the cap the loop waits for the next
// delivery, exactly as an exhausted ordinary retry does.
const openRetryMax = 3

// loopKey identifies one loop of one project. Two projects may run loops of
// the same name, so the project is part of the key: without it, one
// project's failure would cancel the other's pending retry.
type loopKey struct {
	ProjectID string
	LoopName  string
}

// attempt is the retry state of one loop: how many retries have been
// scheduled for the current run of failures, and the timer that will run the
// next one.
type attempt struct {
	n     int
	timer *time.Timer
}

// Worker turns a delivery, or a retry deadline that has passed, into a tick.
//
// Every collaborator is a field so the acceptance tests can be written
// without a registry, a database, a GitHub token or a real clock;
// internal/loopcmd/tick.go states the same rule for Deps.
//
// Every seam and every delay is set once by NewWorker, before the Worker is
// shared with the HTTP handler and the wake loop, and is never written
// afterwards. Only pending is mutated at run time, and only under mu. That
// is what makes the type safe to use from the several goroutines a delivery
// storm creates, and it is checked by -race in CI.
type Worker struct {
	DB *store.DB
	// Targets, TargetFor, Open, and Run are seams. Production wires them to
	// listener.Targets, listener.TargetFor, loopcmd.Open, and loopcmd.RunTick.
	Targets   func(repo string) ([]Target, error)
	TargetFor func(projectID, loop string) (Target, bool, error)
	Token     func() (string, error)
	Open      func(ref loopcmd.ProjectRef, path string, o loopcmd.Options) (*config.Config, loopcmd.Deps, func(), error)
	Run       func(ctx context.Context, cfg *config.Config, deps loopcmd.Deps) (loopcmd.Summary, error)
	Now       func() time.Time
	// After schedules f. It is a seam: production wires it to time.AfterFunc,
	// and a test substitutes a controlled clock. Without it the retry tests
	// would have to sleep for the real delays, which the acceptance forbids.
	After func(d time.Duration, f func()) *time.Timer

	// Delays are fields, not constants, so a test can shrink them.
	OpenRetryDelay  time.Duration // default 1m
	MinRetryDelay   time.Duration // default 30s
	MinWakeInterval time.Duration // default 30s

	mu      sync.Mutex
	pending map[loopKey]*attempt // guarded by mu
}

// NewWorker returns a Worker with production seams and defaults.
//
// A constructor is required, not optional: pending is unexported, so a caller
// in package main cannot initialise it in a composite literal, and the first
// failing tick would write to a nil map and panic the daemon. Worker also
// holds a mutex, so it must never be copied.
func NewWorker(db *store.DB) *Worker {
	return &Worker{
		DB:              db,
		Targets:         Targets,
		TargetFor:       TargetFor,
		Token:           Token,
		Open:            loopcmd.Open,
		Run:             loopcmd.RunTick,
		Now:             time.Now,
		After:           time.AfterFunc,
		OpenRetryDelay:  defaultOpenRetryDelay,
		MinRetryDelay:   defaultMinRetryDelay,
		MinWakeInterval: defaultMinWakeInterval,
		pending:         make(map[loopKey]*attempt),
	}
}

// Deliver ticks every loop that watches repo.
func (w *Worker) Deliver(ctx context.Context, repo string) {
	targets, err := w.Targets(repo)
	if err != nil {
		// Logged and dropped: the routing failure is machine-wide (the
		// registry could not be read), so there is no single loop to record
		// it against and nothing a retry timer could usefully re-run.
		slog.Error("cannot route delivery", "repo", repo, "err", err)
		return
	}
	if len(targets) == 0 {
		slog.Info("no loop watches this repository", "repo", repo)
		return
	}
	for _, t := range targets {
		// Sequential, and one target's failure never returns early: the
		// loops that share a repository are separate projects with separate
		// state, and one broken project must not strand the others.
		w.tickOne(ctx, t)
	}
}

// tickOne runs one loop's tick and decides what its outcome means for the
// retry schedule.
func (w *Worker) tickOne(ctx context.Context, t Target) {
	key := loopKey{ProjectID: t.ProjectID, LoopName: t.LoopName}

	tok, err := w.Token()
	if err != nil {
		// No retry is scheduled. A bad file mode or an absent file is an
		// operator problem that retrying cannot fix, and retrying would log
		// the same error retry.max times per delivery. The token itself is
		// never in err: env.go keeps it out deliberately.
		slog.Error("cannot read the github token", "loop", t.LoopName,
			"project", t.ProjectName, "err", err)
		return
	}

	cfg, deps, cleanup, err := w.Open(t.Ref(), t.ConfigPath, loopcmd.Options{
		Token:         tok,
		RequireGitHub: true,
		// The write path. A tick against a database missing this loop's rows
		// would re-dispatch every open issue and start a second agent in a
		// worktree that already holds one.
		MigrationPolicy: loopcmd.FailOnUnimported,
	})
	// Deferred at once, before any branch below can return: Open holds a
	// SQLite handle, and this daemon calls Open once per target per
	// delivery, so a single missed cleanup leaks a handle on every delivery
	// for as long as the process lives. The nil check is for the error path
	// only -- loopcmd.Open returns a nil cleanup with its error.
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		// There is no cfg here, so the loop's backoff list is unknown. The
		// retry runs at OpenRetryDelay rather than at some undefined value.
		slog.Error("cannot open loop", "loop", t.LoopName, "project", t.ProjectName,
			"config", t.ConfigPath, "err", err)
		w.schedule(ctx, t, openRetryMax, func(int) time.Duration { return w.OpenRetryDelay })
		return
	}

	if _, err := w.Run(ctx, cfg, deps); err != nil {
		if errors.Is(err, lock.ErrHeld) {
			// No retry. The delivery carries no state of its own, so the
			// tick already holding the lock reads the same GitHub state a
			// moment later than this one would have. The pending attempt is
			// cleared too, or the next real failure would resume a backoff
			// list part way through and give up early.
			slog.Info("skipping tick: another tick holds the loop lock",
				"loop", cfg.Name, "project", t.ProjectName)
			w.clear(key)
			return
		}
		slog.Error("tick failed", "loop", cfg.Name, "project", t.ProjectName, "err", err)
		w.schedule(ctx, t, cfg.Retry.Max, func(n int) time.Duration {
			return w.backoffFor(cfg, n)
		})
		return
	}

	w.clear(key)
}

// backoffFor returns the wait before retry n (counted from zero) of cfg.
//
// The list is clamped to its last entry, treated as zero when it is empty,
// and floored at MinRetryDelay. Empty is not a defensive nicety: retry.max
// may legitimately be 0, which means never retry and leaves retry.backoff
// out of the file entirely, so an unguarded index would panic a daemon that
// has no supervisor to restart it.
func (w *Worker) backoffFor(cfg *config.Config, n int) time.Duration {
	d := time.Duration(0)
	if len(cfg.Retry.Backoff) > 0 {
		i := n
		if i >= len(cfg.Retry.Backoff) {
			i = len(cfg.Retry.Backoff) - 1
		}
		if i < 0 {
			i = 0
		}
		d = cfg.Retry.Backoff[i].Std()
	}
	if d < w.MinRetryDelay {
		d = w.MinRetryDelay
	}
	return d
}

// schedule arms the next retry for t, up to max of them, waiting the
// duration delay reports for the attempt about to be scheduled.
//
// delay is a function rather than a value because the two callers know
// different things: a failed tick has the loop's configuration and reads its
// backoff list, and a failed Open has no configuration at all.
func (w *Worker) schedule(ctx context.Context, t Target, max int, delay func(n int) time.Duration) {
	key := loopKey{ProjectID: t.ProjectID, LoopName: t.LoopName}

	w.mu.Lock()
	defer w.mu.Unlock()

	a := w.pending[key]
	if a == nil {
		a = &attempt{}
		w.pending[key] = a
	}
	// Scheduling again for a key that already has a timer stops the old one
	// first. Without this a burst of deliveries for one loop would leave
	// several timers armed for it and run several ticks at once, each of
	// which would then fight for the same loop lock.
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}

	if a.n >= max {
		// The budget is spent. The entry is dropped rather than kept at the
		// cap, so the next delivery for this loop starts a fresh run of
		// attempts instead of inheriting an exhausted one.
		delete(w.pending, key)
		slog.Warn("retry budget spent; waiting for the next delivery",
			"loop", t.LoopName, "project", t.ProjectName, "attempts", a.n)
		return
	}

	d := delay(a.n)
	a.n++
	n := a.n
	a.timer = w.After(d, func() {
		// A cancelled context means the daemon is shutting down. Serve stops
		// every pending timer on the way out, but a timer that had already
		// fired and was waiting on the mutex would still be running here, so
		// the check is repeated rather than assumed.
		if ctx.Err() != nil {
			return
		}
		slog.Info("retrying a failed tick", "loop", t.LoopName,
			"project", t.ProjectName, "attempt", n)
		w.tickOne(ctx, t)
	})
	slog.Info("scheduled a retry", "loop", t.LoopName, "project", t.ProjectName,
		"attempt", n, "in", d)
}

// clear drops a loop's retry schedule after an outcome that ends it.
func (w *Worker) clear(key loopKey) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if a, ok := w.pending[key]; ok {
		if a.timer != nil {
			a.timer.Stop()
		}
		delete(w.pending, key)
	}
}

// stopAll stops every pending retry timer. It runs on shutdown, so a daemon
// that has been told to stop starts no new agent.
func (w *Worker) stopAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for key, a := range w.pending {
		if a.timer != nil {
			a.timer.Stop()
		}
		delete(w.pending, key)
	}
}

// Wake ticks the one loop whose retry deadline has passed, and returns when the
// next deadline is due. ok is false when no deadline is pending.
func (w *Worker) Wake(ctx context.Context) (next time.Time, ok bool) {
	now := w.Now()

	// EarliestRetryAfterAt, not EarliestRetryAfter: the cooldown boundary is
	// judged against this worker's own clock, so a test can freeze both the
	// deadline comparison below and the one inside the query at the same
	// instant.
	due, ok, err := w.DB.EarliestRetryAfterAt(now)
	if err != nil {
		// Reported and treated as "nothing pending". The caller waits its
		// wake interval and asks again, which is the right response to a
		// database that is momentarily unreadable.
		slog.Error("cannot read the earliest retry deadline", "err", err)
		return time.Time{}, false
	}
	if !ok {
		return time.Time{}, false
	}
	if due.At.After(now) {
		// The caller sets its timer for this and does not tick.
		return due.At, true
	}

	slog.Info("waking a loop for a retry deadline", "loop", due.Loop,
		"issue", due.Number, "due", due.At)

	// TargetFor, never Targets(due.Repo): the deadline belongs to one
	// project's issue, and repository routing would dispatch agents in every
	// other project that watches the same repository, on that project's own
	// token budget.
	t, found, err := w.TargetFor(due.ProjectID, due.Loop)
	if err != nil {
		slog.Error("cannot route retry deadline", "loop", due.Loop,
			"project", due.ProjectID, "issue", due.Number, "err", err)
		return due.At, true
	}
	if !found {
		w.clearOrphanedDeadline(due)
		return due.At, true
	}

	w.tickOne(ctx, t)

	// The handled deadline is returned, not a fresh query for the next one.
	// The tick may legitimately have decided nothing (its own backoff, a
	// tripped breaker), which leaves this row past due, and Serve's
	// MinWakeInterval floor is what keeps that from spinning.
	return due.At, true
}

// clearOrphanedDeadline clears the failure flag behind a deadline whose loop
// no longer exists.
//
// A project can be deleted, or a loop's configuration file removed, while an
// issue row carrying needs_retry survives in the canonical database. That row
// is permanently past due and permanently unroutable, so without this the
// wake loop would re-enter Wake every MinWakeInterval for the life of the
// daemon, re-logging the same warning and re-reading the same row -- the very
// hot loop EarliestRetryAfter's own predicate exists to prevent. Clearing the
// flag removes the row from that predicate, which is exactly what a tick does
// for a failure no retry can act on (loopcmd.act, KindClearRetry).
//
// A failed clear is left to the next wake. It means the database itself is
// unwritable, and there is no state a daemon could keep that would fix that.
func (w *Worker) clearOrphanedDeadline(due store.RetryDue) {
	slog.Warn("clearing a retry deadline whose loop no longer exists",
		"loop", due.Loop, "project", due.ProjectID, "issue", due.Number)
	if err := w.DB.Project(due.ProjectID).ClearNeedsRetry(due.Loop, due.Repo, due.Number); err != nil {
		slog.Error("cannot clear an orphaned retry deadline", "loop", due.Loop,
			"project", due.ProjectID, "issue", due.Number, "err", err)
	}
}

// Serve runs the wake loop until ctx is done.
//
// It selects over ctx.Done and a single timer reset from Wake's return. The
// dynamic set of retry timers is deliberately not selected over: those are
// time.AfterFunc callbacks that do their own work, and gathering them into
// this select would mean rebuilding it on every schedule.
func (w *Worker) Serve(ctx context.Context) {
	// Created already fired and drained, so the first Reset below is a reset
	// of a stopped timer on every Go version, not a reset of a live one
	// whose channel might still hold a value.
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		next, ok := w.Wake(ctx)

		// The floor is what keeps a stale row or a clock skew from spinning:
		// Wake returns a past deadline whenever the tick it ran decided
		// nothing, and an unfloored wait would then re-tick that loop as
		// fast as the GitHub API answers.
		d := w.MinWakeInterval
		if ok {
			if until := next.Sub(w.Now()); until > d {
				d = until
			}
		}
		timer.Reset(d)

		select {
		case <-ctx.Done():
			w.stopAll()
			return
		case <-timer.C:
		}
	}
}
