package loopcmd

import (
	"fmt"
	"os"

	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/migrate"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// openCanonical opens the one state database and imports anything still left in
// the old per-loop files.
//
// It is the READ path, so a source that cannot be imported is a warning and not
// a failure: one broken project must not stop a report about all the others. The
// write path uses migrate.EnsureProject instead, which refuses to continue.
//
// The warning goes to stderr. Everything these commands print to stdout is a
// table an operator may pipe somewhere.
func openCanonical() (*store.DB, error) {
	if _, err := home.EnsureDir(); err != nil {
		return nil, err
	}
	path, err := home.StateDBPath()
	if err != nil {
		return nil, err
	}
	db, err := store.Open(path)
	if err != nil {
		return nil, err
	}

	report, err := migrate.Sweep(db, migrate.Options{})
	if err != nil {
		db.Close()
		return nil, err
	}
	for _, res := range report.Failed() {
		fmt.Fprintf(os.Stderr, "warning: state in %s (loop %s) was not imported: %s\n",
			res.Source.Path, res.Source.Loop, res.Reason)
	}
	return db, nil
}

// snapshot is one read of the whole database, shared by every summary a command
// prints.
//
// Before the canonical database existed, each of these numbers cost one file
// open per loop. Now they are three queries for the machine.
type snapshot struct {
	loops map[store.LoopKey]store.LoopState
	// live and orphans are keyed by repository as well, because a loop that was
	// pointed at a new repository still holds the old one's dispatches. The
	// per-loop reads this replaced filtered on repo, and a summary that counted
	// both would report a live dispatch for a repository the loop no longer
	// watches.
	live    map[loopRepo]int
	orphans map[loopRepo]int
}

// loopRepo is one loop's work on one repository.
type loopRepo struct {
	store.LoopKey
	Repo string
}

func readSnapshot(db *store.DB) (*snapshot, error) {
	snap := &snapshot{
		loops:   map[store.LoopKey]store.LoopState{},
		live:    map[loopRepo]int{},
		orphans: map[loopRepo]int{},
	}

	states, err := db.LoopStates()
	if err != nil {
		return nil, err
	}
	for _, st := range states {
		snap.loops[store.LoopKey{ProjectID: st.ProjectID, Loop: st.Loop}] = st
	}

	running, err := db.RunningDispatches()
	if err != nil {
		return nil, err
	}
	for _, d := range running {
		k := loopRepo{
			LoopKey: store.LoopKey{ProjectID: d.ProjectID, Loop: d.Loop},
			Repo:    d.Repo,
		}
		// An imported dispatch carries the identifier its runner was started
		// with, which is what the process actually advertises.
		if proc.IsAlive(d.PID, d.RunnerID()) {
			snap.live[k]++
		} else {
			snap.orphans[k]++
		}
	}
	return snap, nil
}
