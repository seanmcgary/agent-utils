package listener

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/loopcmd"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// seedRunning writes one dispatch row still marked running, which is what a
// machine that went down leaves behind.
func seedRunning(t *testing.T, db *store.DB, projectID, loop string, number int) {
	t.Helper()
	id, err := db.Project(projectID).CreateDispatch(store.Dispatch{
		Loop: loop, Repo: "o/r", Number: number, Kind: store.KindStart, SessionID: "s",
	})
	if err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}
	if err := db.Project(projectID).SetDispatchProcess(id, 4242, workNow); err != nil {
		t.Fatalf("SetDispatchProcess: %v", err)
	}
}

// The sweep is the whole of crash recovery: it turns rows nothing can see into
// retry deadlines, which the scheduler that already exists then serves.
func TestReapOrphansSweepsEveryLoopThatHasARunningRow(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	seedRunning(t, db, workProject, "planning", 1)
	seedRunning(t, db, workProject, "execution", 2)

	h.w.reapOrphans(context.Background())

	if got := h.reapedLoops(); len(got) != 2 {
		t.Errorf("reaped = %v, want both loops", got)
	}
}

// One loop with several dead rows is still ONE reap: the reap reads every
// running row of the loop it is given, and calling it per row would take the
// loop's lock once per orphan for no gain.
func TestReapOrphansReapsALoopOnceHoweverManyRowsItHas(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	seedRunning(t, db, workProject, "planning", 1)
	seedRunning(t, db, workProject, "planning", 2)
	seedRunning(t, db, workProject, "planning", 3)

	h.w.reapOrphans(context.Background())

	if got := h.reapedLoops(); len(got) != 1 {
		t.Errorf("reaped = %v, want exactly one reap for the one loop", got)
	}
}

// A machine with nothing running is the ordinary case, and the sweep runs on a
// timer forever. It must cost one query and stop.
func TestReapOrphansOpensNothingWhenNoRowIsRunning(t *testing.T) {
	h := newHarness(openWorkDB(t))

	h.w.reapOrphans(context.Background())

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.opens) != 0 {
		t.Errorf("opens = %d, want none: there was nothing to reap", len(h.opens))
	}
}

// The reap needs no GitHub. Requiring a token would make crash recovery fail
// exactly when the machine is least healthy -- and the rows it repairs are
// read from the local database and the filesystem alone.
func TestReapOrphansRecoversWithoutAGithubToken(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	h.tokenErr = errors.New("no token file")
	seedRunning(t, db, workProject, "planning", 1)

	h.w.reapOrphans(context.Background())

	if got := h.reapedLoops(); len(got) != 1 {
		t.Errorf("reaped = %v, want the loop reaped with no token", got)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, o := range h.opens {
		if o.opts.RequireGitHub {
			t.Error("the sweep opened a loop demanding GitHub access it does not use")
		}
	}
}

// A loop the registry can no longer route must not stop the sweep. One deleted
// project directory would otherwise strand every other project's orphans,
// which is the starvation Wake's own skip list exists to prevent.
func TestReapOrphansKeepsGoingPastAnUnroutableLoop(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	seedRunning(t, db, workProject, "planning", 1)
	seedRunning(t, db, workProject, "execution", 2)
	h.targetFor = func(_ string, loop string) (Target, Routing, error) {
		if loop == "planning" {
			return Target{}, RouteGone, nil
		}
		return h.target(loop), RouteFound, nil
	}

	h.w.reapOrphans(context.Background())

	got := h.reapedLoops()
	if len(got) != 1 || got[0] != "execution" {
		t.Errorf("reaped = %v, want the routable loop alone", got)
	}
}

// A loop that cannot be opened is the same story: report it and carry on.
func TestReapOrphansKeepsGoingPastALoopItCannotOpen(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	h.openErr = errors.New("unimported legacy database")
	seedRunning(t, db, workProject, "planning", 1)
	seedRunning(t, db, workProject, "execution", 2)

	h.w.reapOrphans(context.Background())

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.opens) != 2 {
		t.Errorf("opens = %d, want both loops attempted", len(h.opens))
	}
}

// A held lock means a tick is running, and every tick reaps as part of its own
// pass. Nothing is wrong, so nothing is logged as wrong and the sweep moves on.
func TestReapOrphansTreatsAHeldLockAsAlreadyHandled(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	h.reapErr = lock.ErrHeld
	seedRunning(t, db, workProject, "planning", 1)
	seedRunning(t, db, workProject, "execution", 2)

	h.w.reapOrphans(context.Background())

	if got := h.reapedLoops(); len(got) != 2 {
		t.Errorf("reaped = %v, want the sweep to have tried both loops", got)
	}
}

// The sweep never dispatches. Recovery makes an orphan VISIBLE and stops; the
// backoff, the cooldown and the retry cap stay in the scheduler that owns them.
func TestReapOrphansNeverRunsAnIssue(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	seedRunning(t, db, workProject, "planning", 1)

	h.w.reapOrphans(context.Background())

	if got := h.ranLoops(); len(got) != 0 {
		t.Errorf("ran = %v, want no issue dispatched by the sweep", got)
	}
}

// A crash is discovered at START, before the first wake: that is the whole
// point. Waiting for the sweep timer would leave the machine idle for the
// interval after every restart.
func TestServeSweepsForOrphansBeforeItsFirstWake(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	seedRunning(t, db, workProject, "planning", 1)
	ctx, cancel := context.WithCancel(context.Background())

	swept := make(chan struct{})
	h.onReap = func() { close(swept) }

	go h.w.Serve(ctx)
	select {
	case <-swept:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not sweep for orphans at start")
	}
	cancel()
}

// And again on its own interval, so an agent killed while the daemon is
// running -- an OOM kill, a stray SIGKILL -- is recovered too rather than
// waiting for a delivery that may never arrive.
func TestServeSweepsForOrphansAgainOnItsInterval(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	seedRunning(t, db, workProject, "planning", 1)
	h.w.OrphanSweepInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sweeps := make(chan struct{}, 8)
	h.onReap = func() {
		select {
		case sweeps <- struct{}{}:
		default:
		}
	}

	go h.w.Serve(ctx)
	for i := 0; i < 2; i++ {
		select {
		case <-sweeps:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d sweeps; the interval did not fire", i)
		}
	}
}

// The interval is floored like MinWakeInterval, and for the same reason: the
// field is exported so tests can shrink it, and a zero would turn the sweep
// into a tight loop over every project on the machine.
func TestOrphanSweepIntervalHasAFloor(t *testing.T) {
	h := newHarness(nil)
	h.w.OrphanSweepInterval = 0
	if got := h.w.orphanSweepEvery(); got != defaultOrphanSweepInterval {
		t.Errorf("orphanSweepEvery() = %v, want the default %v", got, defaultOrphanSweepInterval)
	}
}

// A sweep must not outlive a shutdown: it opens databases and touches
// worktrees, and holding a shutdown open for a machine-wide pass is the
// behaviour Serve's own cancellation check exists to avoid.
func TestReapOrphansStopsWhenTheContextIsCancelled(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	seedRunning(t, db, workProject, "planning", 1)
	seedRunning(t, db, workProject, "execution", 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h.w.reapOrphans(ctx)

	if got := h.reapedLoops(); len(got) != 0 {
		t.Errorf("reaped = %v, want none after cancellation", got)
	}
}

// reapSeam is the production signature, kept here so a change to it fails this
// file rather than silently leaving the fake behind.
var _ func(*config.Config, loopcmd.Deps) (loopcmd.Summary, error) = loopcmd.ReapOrphans
