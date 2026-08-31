package loopcmd

import (
	"path/filepath"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/lock"
)

// ReapOrphans retires one loop's dispatches whose runner process is gone, and
// queues the retry each one is owed.
//
// It exists because an orphan is invisible to everything that would otherwise
// fix it. The daemon's scheduler wakes on retry DEADLINES
// (store.EarliestRetryAfterAt), and only a reap writes one; a reap happens
// only inside a tick; and a tick happens only when a delivery arrives or a
// deadline comes due. A row left running by a machine that went down satisfies
// none of those, so it waits for a webhook that may never come -- which is how
// three agents sat dead for two hours across a reboot.
//
// It is the reap alone. It never dispatches: the point is to make the orphan
// VISIBLE, after which the existing scheduler applies the backoff, the
// cooldown and the retry cap that already live in one place. Recovery that
// dispatched would be a second copy of those rules, disagreeing with the first
// one the day either changed.
//
// This is Tick's reap, called directly rather than reimplemented -- one rule
// for what a dead runner means, whichever caller noticed it.
func ReapOrphans(cfg *config.Config, deps Deps) (Summary, error) {
	// The same lock every other entry point takes, for the same reason: a tick
	// may be dispatching into these worktrees right now.
	//
	// A held lock is not a failure and not a retry. It means a tick is running,
	// and every tick reaps as part of its own pass -- so the work this function
	// would do is already being done. The caller skips the loop and comes back
	// on the next sweep.
	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		return Summary{}, err
	}
	defer l.Release()

	var sum Summary
	now := deps.Now()

	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		return sum, err
	}
	if len(running) == 0 {
		return sum, nil
	}
	states, err := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if err != nil {
		return sum, err
	}

	live, err := reapDead(cfg, deps, running, states, now, &sum)
	if err != nil {
		return sum, err
	}
	sum.Live = len(live)
	return sum, nil
}
