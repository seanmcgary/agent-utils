package listener

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/loopcmd"
)

// waitRetryMax is how many times a waiting pass that found its loop's lock held
// is tried again. See runWait for why a held lock is retried here when the
// epic sweep simply drops it.
const waitRetryMax = 3

// The waiting pass is the one thing this daemon does ACROSS projects.
//
// Every other pass answers a delivery in the loops that watch the delivery's
// repository. This one cannot: the issue a closure or a green run releases
// usually lives in ANOTHER repository -- exchange-service#697 closing releases
// exchange-infra#945 -- watched by another project's loop. So it starts from
// the scan of every loop on the machine, keeps the loops that declare a
// waiting label, and runs each through that loop's OWN routing: its project,
// its configuration, its lock, its repository. The delivery's repository is
// only ever read.
//
// Three things start it, and they share runWait:
//
//   - an issue closing (waitClosePass): the issues that declared it a blocker
//     are re-decided;
//   - a workflow run completing (waitRunPass): the waiting issues of loops
//     that wait on that workflow, and that are blocked by an issue in the
//     run's repository, are re-decided;
//   - the daemon starting (catchUpWaiting): every waiting issue is re-decided,
//     for the closures and runs that happened while it was down.
//
// `loop tick` under cron is the fourth, through loopcmd.Tick, and is the
// backstop for a delivery that never arrived.

// waitingLoops returns every loop on this machine that declares a waiting
// label. The tend dispatcher never does.
func (w *Worker) waitingLoops() ([]Target, error) {
	routes, err := w.ScanTargets()
	if err != nil {
		return nil, err
	}
	var out []Target
	for _, t := range routes.Targets {
		if t.IsTend() || strings.TrimSpace(t.Waiting) == "" {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// waitClosePass re-decides the waiting issues that declared the delivery's
// closed issue a blocker.
//
// It runs ONCE per delivery, not once per target of the delivery's
// repository: the issues it acts on belong to whichever loops wait on them,
// which need not be any of those targets.
func (w *Worker) waitClosePass(ctx context.Context, d Delivery, acc *access) {
	// A close as not planned or as a duplicate cannot release anything --
	// the pass would re-read the blocker and wait -- so it costs no call.
	// An EMPTY reason is a polled close, which carries none, and is evaluated.
	if d.StateReason != "" && !strings.EqualFold(d.StateReason, "completed") {
		slog.Info("waiting pass skipped: the issue closed as "+d.StateReason+", which releases nothing",
			"repo", d.Repo, "issue", d.Number)
		return
	}
	loops, err := w.waitingLoops()
	if err != nil {
		slog.Error("waiting pass cannot list this machine's loops", "repo", d.Repo,
			"issue", d.Number, "err", err)
		return
	}
	// The ordinary case on a machine where no loop waits on anything: no
	// GitHub call at all.
	if len(loops) == 0 {
		return
	}
	if acc.wait == nil {
		slog.Warn("waiting pass skipped: this client cannot read issue dependencies",
			"repo", d.Repo, "issue", d.Number)
		return
	}
	owner, name, ok := splitRepo(d.Repo)
	if !ok {
		return
	}
	dependents, err := acc.wait.Blocking(ctx, owner, name, d.Number)
	if err != nil {
		// No retry: the cron tick re-decides every waiting issue from scratch.
		slog.Error("waiting pass cannot read the issues this one was blocking",
			"repo", d.Repo, "issue", d.Number, "err", err)
		return
	}
	if len(dependents) == 0 {
		return
	}

	for _, t := range loops {
		var numbers []int
		for _, dep := range dependents {
			// Every condition is about the WRITE target, and loopcmd.WaitSweep
			// re-checks each of them on the issue as it stands under the
			// loop's lock. Matching here only decides which loops are worth
			// opening.
			if dep.Number <= 0 || !dep.IsOpen() || !strings.EqualFold(dep.Repo, t.Repo) ||
				!dep.HasLabel(t.Waiting) {
				continue
			}
			numbers = append(numbers, dep.Number)
		}
		if len(numbers) == 0 {
			continue
		}
		slog.Info("a closed blocker may release waiting issues",
			"blocker_repo", d.Repo, "blocker", d.Number,
			"project", t.ProjectName, "loop", t.LoopName, "repo", t.Repo, "issues", loggedNumbers(numbers))
		w.runWait(ctx, t, acc, "closure", func(cfg *config.Config, deps loopcmd.Deps) (loopcmd.Summary, error) {
			return w.RunWait(ctx, cfg, deps, numbers)
		}, 0)
	}
}

// waitRunPass re-decides the waiting issues a completed workflow run may have
// released: those of loops that wait on that workflow, blocked by an issue in
// the run's repository.
//
// The run's own payload decides nothing -- not its commit, not its
// conclusion. The pass re-reads the runs of the blocker's merge commit from
// GitHub, so this delivery only says WHEN to look.
func (w *Worker) waitRunPass(ctx context.Context, d Delivery) {
	loops, err := w.waitingLoops()
	if err != nil {
		slog.Error("waiting pass cannot list this machine's loops", "repo", d.Repo,
			"workflow", d.CompletedWorkflow, "err", err)
		return
	}
	var keep []Target
	for _, t := range loops {
		for _, f := range t.WaitForChecks {
			if strings.EqualFold(f, d.CompletedWorkflow) {
				keep = append(keep, t)
				break
			}
		}
	}
	if len(keep) == 0 {
		// Deliberately SILENT, for the reason Deliver gives about a push to a
		// branch nothing tends: every workflow of every watched repository
		// completes here, and a line per run that nothing waits on would bury
		// every line that means something.
		return
	}

	acc, err := w.access()
	if err != nil {
		slog.Error("cannot read the github token", "repo", d.Repo,
			"workflow", d.CompletedWorkflow, "err", err)
		return
	}
	for _, t := range keep {
		slog.Info("a completed workflow run may release waiting issues",
			"run_repo", d.Repo, "workflow", d.CompletedWorkflow,
			"project", t.ProjectName, "loop", t.LoopName, "repo", t.Repo)
		repo := d.Repo
		w.runWait(ctx, t, acc, "workflow_run", func(cfg *config.Config, deps loopcmd.Deps) (loopcmd.Summary, error) {
			return w.RunWaitRepo(ctx, cfg, deps, repo)
		}, 0)
	}
}

// catchUpWaiting re-decides every waiting issue on the machine.
//
// Serve runs it once, at start, beside reconcileClosed and for the same
// reason: a closure or a green run that happened while the daemon was down is
// a delivery nothing will ever send again. Not on a timer -- between starts,
// the deliveries are the fast path and `loop tick` is the backstop.
func (w *Worker) catchUpWaiting(ctx context.Context) {
	loops, err := w.waitingLoops()
	if err != nil {
		slog.Error("waiting catch-up cannot list this machine's loops", "err", err)
		return
	}
	if len(loops) == 0 {
		return
	}
	acc, err := w.access()
	if err != nil {
		slog.Error("cannot read the github token for the waiting catch-up", "err", err)
		return
	}
	for _, t := range loops {
		if ctx.Err() != nil {
			return
		}
		w.runWait(ctx, t, acc, "start-up catch-up", func(cfg *config.Config, deps loopcmd.Deps) (loopcmd.Summary, error) {
			return w.RunWaitRepo(ctx, cfg, deps, "")
		}, 0)
	}
}

// runWait opens one loop through its own routing and runs one waiting pass in
// it.
//
// # A held lock is retried
//
// The epic sweep drops a pass that finds the loop's lock held and leaves the
// rest to cron. That is not good enough here: a workflow_run completing is a
// ONE-SHOT signal -- nothing else in the daemon will mention that run again --
// and the lock is held by every ordinary tick of the loop, so a green mirror
// run landing during an unrelated tick would leave the issue waiting until the
// next cron tick, or forever on a machine with no cron. So a held lock is
// tried again after BusyDelay, up to waitRetryMax times, each attempt with
// fresh GitHub access, for tickFresh's reason.
func (w *Worker) runWait(
	ctx context.Context, t Target, acc *access, why string,
	run func(cfg *config.Config, deps loopcmd.Deps) (loopcmd.Summary, error), attempt int,
) {
	if ctx.Err() != nil {
		return
	}
	cfg, deps, cleanup, err := w.Open(t.Ref(), t.ConfigPath, loopcmd.Options{
		Token:   acc.token,
		GH:      acc.gh,
		Authors: acc.authors,
		Epic:    acc.epic,
		Wait:    acc.wait,
		// The pass writes labels that dispatch an agent, so it takes the
		// write path's guards: a token, and a database holding this loop's
		// rows.
		RequireGitHub:   true,
		MigrationPolicy: loopcmd.FailOnUnimported,
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		slog.Error("cannot open loop for the waiting pass", "loop", t.LoopName,
			"project", t.ProjectName, "trigger", why, "err", err)
		return
	}

	sum, err := run(cfg, deps)
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			if attempt+1 >= waitRetryMax {
				slog.Warn("waiting pass gave up on a busy loop lock; the next tick or delivery decides it",
					"loop", cfg.Name, "project", t.ProjectName, "trigger", why, "attempts", attempt+1)
				return
			}
			slog.Info("another tick holds the loop lock; running the waiting pass again later",
				"loop", cfg.Name, "project", t.ProjectName, "trigger", why, "in", w.BusyDelay)
			w.After(w.BusyDelay, func() {
				if ctx.Err() != nil {
					return
				}
				fresh, err := w.access()
				if err != nil {
					slog.Error("cannot read the github token", "loop", t.LoopName,
						"project", t.ProjectName, "err", err)
					return
				}
				w.runWait(ctx, t, fresh, why, run, attempt+1)
			})
			return
		}
		slog.Error("waiting pass failed", "loop", cfg.Name, "project", t.ProjectName,
			"trigger", why, "err", err)
		return
	}
	if sum.Released > 0 {
		slog.Info("waiting pass released issues", "loop", cfg.Name, "project", t.ProjectName,
			"trigger", why, "released", sum.Released)
	}
}

// loggedNumbers bounds an issue-number list carried into a log line. The list
// is a dependency fan-out anyone with triage can grow.
func loggedNumbers(ns []int) []int {
	const max = 10
	if len(ns) <= max {
		return ns
	}
	return ns[:max]
}
