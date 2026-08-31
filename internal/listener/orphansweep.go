package listener

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/loopcmd"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// defaultOrphanSweepInterval is how often Serve re-sweeps for orphaned
// dispatches after the one it runs at start.
//
// It is far longer than MinWakeInterval because it answers a rarer question.
// A wake serves deadlines that the daemon itself wrote and knows are coming;
// a sweep looks for rows nothing wrote a deadline for, which happens when a
// process died without recording an outcome -- a machine going down, an OOM
// kill, a SIGKILL. Minutes of latency on that is not worth a machine-wide
// query every thirty seconds.
const defaultOrphanSweepInterval = 5 * time.Minute

// orphanSweepEvery is the interval Serve actually uses.
//
// OrphanSweepInterval is exported so a test can shrink it, so the default is
// reasserted here rather than only in NewWorker: a zero would turn the sweep
// into a tight loop that opens every project on the machine. This is
// wakeDelay's floor, for the same reason.
func (w *Worker) orphanSweepEvery() time.Duration {
	if w.OrphanSweepInterval <= 0 {
		return defaultOrphanSweepInterval
	}
	return w.OrphanSweepInterval
}

// reapOrphans retires every dispatch on this machine whose runner is gone, and
// queues the retry each one is owed.
//
// It is the entire crash-recovery path, and it is this small because it only
// has to make the orphans VISIBLE. Everything after it already exists: the
// reap stamps a retry deadline, Wake serves deadlines, and Wake serves ONE per
// pass through tickFresh -- so N recovered agents restart one per wake
// interval rather than all at once, and each restarts through the per-issue
// tick that the backoff, the cooldown and the retry cap already govern.
//
// Nothing here dispatches. A recovery path that started agents itself would be
// a second copy of those rules, and the day either changed they would
// disagree.
//
// Worth stating plainly, because the design leans on it: a per-issue tick does
// not consult the circuit breaker (engine.go's KNOWN GAP -- eligibleRetries
// counts within one call, and a call scoped to one issue can never reach a
// threshold above 1). That is what keeps recovery working. Reaping two orphans
// of one loop and then running a WHOLE-loop tick would count two eligible
// retries, trip a breaker whose threshold is 2, drop every dispatch and set a
// cooldown -- recovering nothing and going quiet for half an hour. If that gap
// is ever closed, the breaker must learn that a machine which has just come
// back is not a platform fault in progress.
func (w *Worker) reapOrphans(ctx context.Context) {
	running, err := w.DB.RunningDispatches()
	if err != nil {
		slog.Error("cannot read running dispatches to sweep for orphans", "err", err)
		return
	}
	if len(running) == 0 {
		return
	}

	// One reap per LOOP, not per row. The reap reads every running row of the
	// loop it is given, so a loop with three dead agents needs one call -- and
	// three would take that loop's lock three times to do the same work.
	for _, key := range loopsOf(running) {
		// Checked per loop, not only on entry. A sweep opens a database and
		// touches worktrees for every project on the machine, and a shutdown
		// must not wait for all of them.
		if ctx.Err() != nil {
			return
		}
		w.reapLoop(key)
	}
}

// loopsOf returns the distinct {project, loop} pairs among the dispatches, in
// a stable order so a sweep's log reads the same way twice.
func loopsOf(running []store.Dispatch) []loopKey {
	seen := map[loopKey]bool{}
	var out []loopKey
	for _, d := range running {
		k := loopKey{ProjectID: d.ProjectID, LoopName: d.Loop}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectID != out[j].ProjectID {
			return out[i].ProjectID < out[j].ProjectID
		}
		return out[i].LoopName < out[j].LoopName
	})
	return out
}

// reapLoop reaps one loop, reporting whatever went wrong rather than returning
// it: one unreadable project must not strand every other project's orphans.
func (w *Worker) reapLoop(key loopKey) {
	t, routing, err := w.TargetFor(key.ProjectID, key.LoopName)
	if err != nil || routing != RouteFound {
		// Logged at INFO, not ERROR. A loop that has been renamed or removed
		// while a stale row survives is an ordinary state, and this runs on a
		// timer forever -- an ERROR here would repeat for the life of the
		// daemon. Wake's unroutable handling says the same thing at more
		// length, and owns the counting that eventually clears such a row.
		slog.Info("skipping a loop that cannot be routed while sweeping for orphans",
			"loop", key.LoopName, "project", key.ProjectID, "routing", routing, "err", err)
		return
	}

	// RequireGitHub is FALSE, and no token is read. The reap asks the
	// operating system which processes are alive and writes rows; it calls
	// GitHub for nothing. Demanding a token would make crash recovery fail
	// exactly when the machine is least healthy.
	cfg, deps, cleanup, err := w.Open(t.Ref(), t.ConfigPath, loopcmd.Options{
		RequireGitHub: false,
		// The same write-path guard every other caller uses: reaping against a
		// database missing this loop's rows would retire nothing and report
		// success.
		MigrationPolicy: loopcmd.FailOnUnimported,
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		slog.Error("cannot open loop to sweep for orphans",
			"loop", key.LoopName, "project", t.ProjectName, "err", err)
		return
	}

	sum, err := w.ReapOrphans(cfg, deps)
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			// Not a failure. A tick holds the lock, and every tick reaps as
			// part of its own pass, so the work is already being done.
			slog.Info("a tick holds the loop lock; it reaps its own orphans",
				"loop", cfg.Name, "project", t.ProjectName)
			return
		}
		slog.Error("cannot sweep a loop for orphans",
			"loop", cfg.Name, "project", t.ProjectName, "err", err)
		return
	}
	if sum.Orphans > 0 {
		slog.Info("recovered dispatches whose runner was gone",
			"loop", cfg.Name, "project", t.ProjectName, "orphans", sum.Orphans)
	}
}
