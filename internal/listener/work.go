package listener

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/loopcmd"
	"github.com/seanmcgary/agent-utils/internal/proc"
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
	// defaultTendDelay is how long a merge waits before its tend sweep runs,
	// so a merge train produces one sweep rather than one per merge. The loop
	// lock only collapses sweeps that OVERLAP; merges a minute apart do not,
	// and a tend agent that has already finished suppresses nothing, so an
	// uncoalesced train multiplies by the number of merges in it.
	defaultTendDelay = time.Minute
	// defaultIssueDelay is how long an issue's delivery window stays open.
	//
	// One edit to an issue's labels is one delivery PER LABEL: removing three
	// and adding one, in a single edit, is four deliveries inside half a
	// second. Each was a tick of its own, and the first of them read the
	// labels HALF APPLIED -- after a removal, before the addition that was
	// the point of the edit.
	//
	// Two seconds is long enough to cover an edit made in one action and
	// short enough that the trailing tick still belongs to it. See
	// openIssueWindow for why the leading tick is not delayed by it.
	defaultIssueDelay = 2 * time.Second
	// defaultBusyDelay is how long a pass that found an agent already working
	// its issue waits before looking again.
	//
	// A minute, not seconds: what it waits for is an agent, and agents run for
	// minutes to hours. It is also not a poll of GitHub -- see armBusy, which
	// re-arms from local state until the agent's process is gone -- so the
	// interval costs a dispatch-row read and a kill(0), and buys at most a
	// minute of latency on the tick that finally dispatches.
	defaultBusyDelay = time.Minute
)

// openRetryMax caps the retries for a failure that happened inside
// loopcmd.Open. The loop's own retry.max lives in the configuration Open
// could not load, so it is not knowable on that path; without a cap here, a
// config file with a typo in it would retry once every OpenRetryDelay for as
// long as the daemon runs. After the cap the loop waits for the next
// delivery, exactly as an exhausted ordinary retry does.
const openRetryMax = 3

// orphanClearAfter is how many consecutive wakes must report RouteGone
// before a deadline's failure flag is cleared. At the default wake interval
// that is about 90 seconds.
//
// It is NOT a way to tell "gone" from "cannot tell right now": TargetFor
// answers that question itself now (see Routing), and a wake that cannot
// tell resets this count instead of adding to it. What is left for a counter
// to defend against is narrow and mechanical -- a configs directory being
// rewritten non-atomically, where a listing lands in the instant between the
// old file being removed and the new one appearing. Clearing needs_retry is
// irreversible: store.IssueState says only a retry, a park, or a success
// clears it, and nothing re-derives it. Three agreeing observations across
// three wake intervals cost nothing (the wake loop is already bounded by
// MinWakeInterval) and cannot all land inside a single rename.
const orphanClearAfter = 3

// unroutableLogInterval is how often ONE loop's "cannot route this deadline"
// warning may reach the log.
//
// The condition it reports has no bound: Wake waits on an unresolvable
// deadline for as long as it lasts, so at the default wake interval an
// unparsable config left over a weekend writes about 2,880 identical lines a
// day per loop, into the unrotated file rejectionLogInterval describes. Ten
// minutes keeps the transition and a periodic reminder carrying the count it
// stands for, which is everything an operator reads out of it.
const unroutableLogInterval = 10 * time.Minute

// tendConfirmInterval is how often the periodic check calls GitHub even
// though nothing looks behind. It is what corrects a pr_links row that
// drifted with no delivery: a pull request that closed, or an issue that lost
// its review label, leaves a row the local gate would otherwise trust
// forever.
const tendConfirmInterval = 6 * time.Hour

// tendPassTimeout bounds one whole tend check across every loop on the
// machine. See tendCheckPass: the pass runs on the single wake loop, and it
// shells out to a network git cannot be trusted to return from.
const tendPassTimeout = 10 * time.Minute

// loopKey identifies one loop of one project. Two projects may run loops of
// the same name, so the project is part of the key: without it, one
// project's failure would cancel the other's pending retry.
type loopKey struct {
	ProjectID string
	LoopName  string
}

// retryKind names which failure a retry is being scheduled for. It selects
// the counter to spend, which is why it exists at all: see attempt.
type retryKind int

const (
	kindTick retryKind = iota
	kindOpen
)

// attempt is the retry state of one loop: how many retries have been
// scheduled for the current run of failures, and the timer that will run the
// next one.
//
// The two counters are deliberate. A failed tick spends the loop's own
// retry.max, and a failed Open spends openRetryMax, because the loop's value
// lives in the file Open could not read. One shared counter would mix the
// two budgets: two Open failures followed by a genuine tick failure on a
// loop with retry.max: 1 would give that real failure no retry at all, and a
// single Open failure would drop a loop already four attempts into a
// five-attempt budget. Only the timer is shared, because a loop may have
// only one retry armed at a time.
type attempt struct {
	n     int
	openN int
	timer *time.Timer
}

// counter returns the budget kind spends.
func (a *attempt) counter(kind retryKind) *int {
	if kind == kindOpen {
		return &a.openN
	}
	return &a.n
}

// issueKey identifies ONE issue of one loop of one project. It is the key the
// delivery window and the busy re-look are held under, and it carries the
// issue number where loopKey does not: a burst on one issue must not suppress
// another issue's delivery, which is a different decision entirely.
type issueKey struct {
	ProjectID string
	LoopName  string
	Number    int
}

// issueWindow is one issue's open delivery window.
//
// pending records that a delivery arrived while the window was open, which is
// what says the labels the leading tick read may already be stale. It is a
// pointer for the same reason tendTimer is: openIssueWindow has to tell ITS
// entry from one a later delivery registered after this timer fired.
type issueWindow struct {
	timer   *time.Timer // guarded by Worker.mu
	pending bool        // guarded by Worker.mu
}

// busyTimer is one issue's armed re-look. It is a pointer for the same reason
// tendTimer is; see armBusy.
type busyTimer struct {
	timer *time.Timer // guarded by Worker.mu
}

// tendTimer is one loop's armed tend sweep. It is a pointer so armTend can
// tell ITS entry from one a later merge registered after this timer fired.
type tendTimer struct {
	timer *time.Timer // guarded by Worker.mu
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
	// Targets, TargetFor, Open, and RunIssue are seams. Production wires them
	// to listener.Targets, listener.TargetFor, loopcmd.Open, and
	// loopcmd.TickIssue.
	Targets   func(repo string) ([]Target, error)
	TargetFor func(projectID, loop string) (Target, Routing, error)
	Token     func() (string, error)
	// NewClient builds the GitHub client ONE pass shares across the loops it
	// ticks. Production wires it to ghub.New; it is a seam so a test can count
	// the calls a delivery makes, which is the only place the saving is
	// visible.
	NewClient func(token string) ghub.Client
	Open      func(ref loopcmd.ProjectRef, path string, o loopcmd.Options) (*config.Config, loopcmd.Deps, func(), error)
	// RunIssue acts on ONE issue, taking the loop's lock first. It is
	// loopcmd.TickIssue, never loopcmd.RunTick: the daemon answers events, and
	// an event names an issue. The full reconcile is the cron sweep's job --
	// see loopcmd.Tick -- and running it per delivery burned a token budget on
	// every open issue of every project watching the repository.
	RunIssue func(ctx context.Context, cfg *config.Config, deps loopcmd.Deps, number int) (loopcmd.Summary, error)
	// RunTend rebases the stale pull requests of one loop, taking the loop's
	// lock itself. Production wires it to loopcmd.TendSweep. base is the branch
	// the merge landed on.
	//
	// It runs for ONE delivery -- a pull request merged into the loop's default
	// branch -- because that is the only event that makes many pull requests
	// stale at once and names none of them. Every other delivery gets RunIssue
	// and nothing more; see RunIssue for the reconcile that was removed and
	// must not come back.
	RunTend func(ctx context.Context, cfg *config.Config, deps loopcmd.Deps, base string) (loopcmd.Summary, error)
	// RunCleanup removes the worktree of a pull request that just closed, and
	// the worktree of the issue it closes, once neither is in use. Production
	// wires it to loopcmd.CleanupClosedPR. prNumber is the delivery's number:
	// Delivery.ClosedPR is only set on a pull_request delivery, so the number
	// IS the pull request's.
	RunCleanup func(ctx context.Context, cfg *config.Config, deps loopcmd.Deps, prNumber int) error
	// RunEpic promotes the sub-issues that a closed issue unblocked. Production
	// wires it to loopcmd.EpicSweep, which takes the loop's lock itself.
	//
	// It runs for ONE delivery -- an issue closing -- because that is the only
	// event that unblocks anything. It dispatches no agent: its whole output is
	// label writes, which is what makes it safe for a delivery to act on more
	// than the issue it names. See loopcmd.EpicSweep.
	RunEpic func(ctx context.Context, cfg *config.Config, deps loopcmd.Deps, closed int) (loopcmd.Summary, error)
	// RunTendCheck answers "is any pull request of this loop behind its base",
	// as cheaply as it can. Production wires it to loopcmd.TendCheck. It is a
	// seam because the real one shells out to git and reads a database, and
	// what this file owns is the scheduling around it.
	//
	// force asks it to call GitHub even when nothing looks behind; see
	// tendCheckOne for when this daemon sets it.
	RunTendCheck func(ctx context.Context, cfg *config.Config, deps loopcmd.Deps, force bool) (loopcmd.TendCheckResult, error)
	// ScanTargets lists every loop of every registered project. Production
	// wires it to listener.Scan.
	//
	// It is a seam of its own rather than a reuse of Targets or TargetFor:
	// both of those answer "which loop matches this name", and the periodic
	// check starts from no name at all -- a repository that sends no delivery
	// is precisely the case it exists for, so it has to walk the whole
	// registry.
	ScanTargets func() (Routes, error)
	// ReapOrphans retires one loop's dispatches whose runner process is gone
	// and queues the retry each is owed, taking the loop's lock itself.
	// Production wires it to loopcmd.ReapOrphans.
	//
	// It is what makes a machine that went down recover on its own. The
	// scheduler below wakes on retry DEADLINES, and only a reap writes one, so
	// a row left running by a crash is invisible to it until some delivery
	// happens to tick that loop. This is the sweep that finds them.
	//
	// It dispatches nothing, by design. See Worker.reapOrphans.
	ReapOrphans func(cfg *config.Config, deps loopcmd.Deps) (loopcmd.Summary, error)
	// IssueBusy reports whether an agent of this loop is still running on this
	// issue. Production wires it to Worker.issueBusy, which answers from the
	// dispatch rows and the kernel; it is a seam because a test has no agent
	// process to ask about.
	//
	// It exists so the busy re-look can wait for an agent WITHOUT ticking: a
	// tick reads GitHub, and an eight-hour agent would mean eight hours of
	// fetches for an answer the local process table already has.
	IssueBusy func(t Target, number int) bool
	Now       func() time.Time
	// After schedules f. It is a seam: production wires it to time.AfterFunc,
	// and a test substitutes a controlled clock. Without it the retry tests
	// would have to sleep for the real delays, which the acceptance forbids.
	After func(d time.Duration, f func()) *time.Timer

	// Delays are fields, not constants, so a test can shrink them.
	OpenRetryDelay  time.Duration // default 1m
	MinRetryDelay   time.Duration // default 30s
	MinWakeInterval time.Duration // default 30s
	TendDelay       time.Duration // default 1m
	IssueDelay      time.Duration // default 2s
	BusyDelay       time.Duration // default 1m
	// TendInterval is how often tendCheckPass runs. A value of 0 disables it
	// and leaves the merge and push triggers untouched. It comes from the
	// machine-wide settings file, and NewWorker leaves it zero: a Worker built
	// without one runs no periodic check rather than a surprise one.
	TendInterval time.Duration
	// OrphanSweepInterval is how often Serve re-sweeps for dispatches whose
	// runner is gone, after the sweep it runs at start. Read through
	// orphanSweepEvery, which floors it.
	OrphanSweepInterval time.Duration // default 5m

	mu      sync.Mutex
	pending map[loopKey]*attempt // guarded by mu
	// orphans counts consecutive wakes that found a loop's deadline
	// definitely gone (RouteGone), guarded by mu. See orphanClearAfter. An
	// entry is dropped as soon as the loop routes again OR as soon as a wake
	// cannot tell, so only agreeing observations accumulate.
	orphans map[loopKey]int // guarded by mu
	// tends holds the armed tend timer of each loop, guarded by mu. A merge
	// arriving while one is armed rides it rather than arming a second.
	//
	// Named tends, not sweeps: "sweep" already means the full reconcile in
	// this program (see loopcmd.Tick), and a bare SweepDelay beside
	// OpenRetryDelay would read as a delay before that.
	tends map[loopKey]*tendTimer // guarded by mu
	// windows holds each issue's open delivery window, guarded by mu. A
	// delivery arriving while one is open rides it rather than ticking again.
	windows map[issueKey]*issueWindow // guarded by mu
	// busy holds the armed busy re-look of each issue, guarded by mu. Like
	// tends, a second pass that finds one armed rides it rather than arming a
	// second, which is what keeps an issue to one re-look however many
	// deliveries land on it while its agent runs.
	busy map[issueKey]*busyTimer // guarded by mu
	// confirms holds, per loop, when the periodic check last called GitHub.
	//
	// It is memory only: a restart costs one forced pass per loop, which is
	// one ListOpenIssues plus one ListOpenPullRequests -- and both paginate at
	// 100 per page, so a busy repository costs two REQUESTS PER PAGE, not two
	// requests. Still cheaper than the column and the migration a durable
	// version would need, and paid once per loop per restart.
	//
	// Pruned in tendCheckPass, like every other map on this struct. Nothing
	// else removes an entry, and a loop deleted from the registry would
	// otherwise keep one for the daemon's life; see pruneConfirms.
	confirms map[loopKey]time.Time // guarded by mu
	// unroutable throttles the "cannot route this deadline" warning per
	// loop; it carries its own lock. See warnUnroutable.
	unroutable *throttledLog
}

// NewWorker returns a Worker with production seams and defaults.
//
// A constructor is required, not optional: pending and orphans are
// unexported, so a caller in package main cannot initialise them in a
// composite literal, and the first failing tick would write to a nil map and
// panic the daemon. Worker also holds a mutex, so it must never be copied.
func NewWorker(db *store.DB) *Worker {
	w := &Worker{
		DB:              db,
		Targets:         Targets,
		TargetFor:       TargetFor,
		Token:           Token,
		NewClient:       func(token string) ghub.Client { return ghub.New(token) },
		Open:            loopcmd.Open,
		RunIssue:        loopcmd.TickIssue,
		RunTend:         loopcmd.TendSweep,
		RunCleanup:      loopcmd.CleanupClosedPR,
		RunEpic:         loopcmd.EpicSweep,
		RunTendCheck:    loopcmd.TendCheck,
		ScanTargets:     Scan,
		ReapOrphans:     loopcmd.ReapOrphans,
		Now:             time.Now,
		After:           time.AfterFunc,
		OpenRetryDelay:  defaultOpenRetryDelay,
		MinRetryDelay:   defaultMinRetryDelay,
		MinWakeInterval: defaultMinWakeInterval,
		TendDelay:       defaultTendDelay,
		IssueDelay:      defaultIssueDelay,
		BusyDelay:       defaultBusyDelay,
		pending:         make(map[loopKey]*attempt),
		orphans:         make(map[loopKey]int),
		tends:           make(map[loopKey]*tendTimer),
		windows:         make(map[issueKey]*issueWindow),
		busy:            make(map[issueKey]*busyTimer),
		confirms:        make(map[loopKey]time.Time),
		unroutable:      newThrottledLog(unroutableLogInterval),

		OrphanSweepInterval: defaultOrphanSweepInterval,
	}
	// Bound to the Worker after the literal, like the throttle's clock below:
	// the default reads w.DB, which the literal is still building.
	w.IssueBusy = w.issueBusy
	// The throttle reads the Worker's OWN clock, and reads it late: a test
	// replaces Now after NewWorker returns, and a value captured here would
	// leave it waiting a real ten minutes.
	w.unroutable.now = func() time.Time { return w.Now() }
	return w
}

// Delivery is what one accepted webhook delivery tells the worker.
//
// It replaced a bare (repo, number) pair because a merged pull request must
// start more work than an ordinary delivery, and the handler cannot judge
// that on its own: the decision needs a loop's default_branch, and one
// repository may be watched by several loops with different ones.
type Delivery struct {
	// Repo is the "owner/name" the delivery named. handleWebhook has already
	// matched it against repoFullName, so nothing downstream re-validates it.
	Repo string
	// Number is the issue or pull request the delivery named. Every accepted
	// delivery carries one; handleWebhook rejects a delivery without one
	// rather than answering 202 for work it cannot name.
	Number int
	// MergedInto is the base branch of a pull request this delivery reports as
	// MERGED, and is empty for every other delivery. Empty is the only safe
	// default: it is what keeps an opened issue or a moved label from arming a
	// repository-wide sweep. See Worker.Deliver for the regression that makes
	// this the important property of this type.
	MergedInto string
	// PushedTo is the branch a push delivery moved, and is empty for every
	// other delivery. It arms the same tend sweep MergedInto does, for the
	// case a merge cannot cover: a direct push to the default branch produces
	// no pull_request delivery, so nothing else tells this daemon that every
	// open pull request just fell behind.
	PushedTo string
	// ClosedPR is true when this delivery closed a pull request, merged or
	// not. It is what arms worktree cleanup: on any close the pr-<N>
	// worktree (and, when the pull request closes one, the issue-<M>
	// worktree) is a checkout nothing will push to again. See
	// loopcmd.CleanupClosedPR for the operator's decision to remove both on
	// ANY close, not only a merge.
	ClosedPR bool
	// ClosedIssue is true when this delivery closed an ISSUE, not a pull
	// request. It is what arms the epic sweep.
	//
	// It is deliberately narrower than ClosedPR's counterpart: issues and pull
	// requests share a number space, so a pull_request delivery that set this
	// would sweep the epic of whichever issue happens to carry that number.
	// The event is checked as well as the action.
	ClosedIssue bool
}

// IsMergeInto reports whether this delivery merged a pull request into branch.
//
// An empty branch never matches, even against an empty MergedInto. A loop with
// no default_branch names no branch to compare against, so "they are both
// empty" is not agreement, it is two absent values.
func (d Delivery) IsMergeInto(branch string) bool {
	return branch != "" && d.MergedInto == branch
}

// IsPushTo reports whether this delivery pushed to branch.
//
// It follows IsMergeInto's rule exactly: an empty branch never matches, even
// against an empty PushedTo, because a loop with no default_branch names no
// branch to compare against.
func (d Delivery) IsPushTo(branch string) bool {
	return branch != "" && d.PushedTo == branch
}

// Deliver evaluates ONE subject of repo -- an issue, a pull request, or (for a
// push) a branch -- in every loop that watches it, and acts wherever a loop
// decides there is something to do.
//
// A delivery says "something about this subject changed, figure out what and
// dispatch the correct executor to handle it." It used to trigger a full
// reconcile of every loop watching the repository instead, which is how
// opening one unlabelled test issue dispatched a tend agent for an unrelated
// issue whose pull request was 16 commits behind -- and how every delivery
// spent a token budget proportional to the whole repository rather than to the
// thing that changed.
//
// number may name a pull request rather than an issue; the two share a number
// space, and five of the six subscribed events carry a pull request or issue
// number. Resolving that is loopcmd.TickIssue's job, because it is the layer
// with a GitHub client. The sixth event, push, carries no number at all --
// its subject is a branch, and the only thing it can start is the tend sweep
// armed in tickOne.
//
// The fan-out is still per loop: several projects may watch one repository, and
// each keeps its own state and spends its own budget. The log lines here and in
// handler.go are what makes the chain delivery -> repository -> issue -> loops
// readable after the fact.
func (w *Worker) Deliver(ctx context.Context, d Delivery) {
	targets, err := w.Targets(d.Repo)
	if err != nil {
		// Logged and dropped: the routing failure is machine-wide (the
		// registry could not be read), so there is no single loop to record
		// it against and nothing a retry timer could usefully re-run.
		slog.Error("cannot route delivery", "repo", d.Repo, "err", err)
		return
	}
	if len(targets) == 0 {
		slog.Info("no loop watches this repository", "repo", d.Repo)
		return
	}
	// The push filter runs BEFORE w.access(), not only before tickOne.
	// access() reads the token file and builds a client; subscribing to push
	// multiplies delivery volume by every branch of every watched repository,
	// so a filter placed after it would read a secret from disk on every
	// feature-branch push. Build the kept list first, and return when it is
	// empty.
	kept := make([]Target, 0, len(targets))
	for _, t := range targets {
		// A push names no issue, so the only work it can start is the sweep.
		tends := t.TendPR && d.IsPushTo(t.DefaultBranch)
		if d.Number == 0 && !tends {
			continue
		}
		kept = append(kept, t)
	}
	if len(kept) == 0 {
		// Deliberately SILENT, and the one log line in Deliver that is.
		//
		// Every other "nothing to do" here reports something an operator did
		// not already know. This reports the ordinary case: a developer pushed
		// a feature branch, which is what feature branches are for. The daemon
		// subscribes to push for every branch of every watched repository, so
		// a line here is one line per developer push per repository, forever,
		// into the unrotated launchd log unroutableLogInterval was invented
		// for. tendCheckOne states the same rule where it declines to log its
		// own common case: a line here would bury every line that means
		// something.
		//
		// Throttling was the alternative and was rejected: a throttled line
		// still says only "somebody pushed a branch nothing tends", which is
		// not a condition an operator acts on, so the surviving line would be
		// noise at a slower rate rather than a signal.
		return
	}

	// Built from what SURVIVED the filter, and built after it. Naming every
	// target watching the repository would claim a loop with tend_pr false
	// will sweep when it never will, and building the list first allocated it
	// for every feature-branch push this daemon then dropped. An issue
	// delivery keeps every target, so this is the same list it always was.
	//
	// The non-zero case is logged too, not only the zero one. Without it the
	// only delivery that said anything was the one that did nothing, and the
	// fan-out -- one delivery, several projects, several agents, on several
	// token budgets -- was invisible.
	loops := make([]string, 0, len(kept))
	for _, t := range kept {
		loops = append(loops, t.ProjectName+"/"+t.LoopName)
	}

	if d.Number == 0 {
		// safeText, because PushedTo is attacker-written. SafeRef bounds its
		// charset but not its length, so a multi-kilobyte branch name would go
		// verbatim into an unrotated log -- the failure handler.go:185-196
		// documents. work.go is the same package and gets the same treatment.
		slog.Info("a push moved a branch; every loop that tends it will sweep",
			"repo", d.Repo, "pushed_to", safeText(d.PushedTo), "loops", loops)
	} else {
		// EVALUATED, not "acted on". Every watching loop does evaluate the
		// issue, and this line is what ties the ticks below back to the
		// delivery that caused them -- but most of those loops decide
		// nothing, because only the loop whose trigger label the issue
		// carries usually has anything to do. "Acting on" read as the full
		// repository reconcile this daemon no longer performs, so an
		// operator reasonably read the line as stale or wrong. What each
		// loop DECIDED belongs to the per-loop tick line, which carries a
		// reason now.
		slog.Info("evaluating this issue in every loop that watches the repository",
			"repo", d.Repo, "number", d.Number, "loops", loops)
	}

	// One token read, one client, one memo, for the whole delivery -- created
	// HERE and dropped when Deliver returns. See access: the lifetime is the
	// correctness property, not the saving.
	acc, err := w.access()
	if err != nil {
		// Reported once for the delivery rather than once per loop: the token
		// is machine-wide, so every loop would have failed on the identical
		// read. No retry is scheduled, for the reason access states.
		slog.Error("cannot read the github token", "repo", d.Repo, "number", d.Number,
			"loops", loops, "err", err)
		return
	}

	for _, t := range kept {
		// Sequential, and one target's failure never returns early: the
		// loops that share a repository are separate projects with separate
		// state, and one broken project must not strand the others.
		w.tickOne(ctx, t, d, acc)
	}
}

// access is the GitHub access ONE pass hands to the loops it ticks: the token
// read for that pass, and a client that answers the repeated reads of the
// delivered number from a single fetch.
//
// A delivery fans out across every loop watching the repository, and each of
// those loops began by fetching the SAME issue: two loops meant two identical
// fetches for one event, ten meant ten, plus a pull request and the issue it
// closes on top for a delivery that named a pull request.
//
// # Lifetime
//
// A value of this type belongs to ONE pass and must not outlive it. Every
// producer creates it inside the function that serves the pass -- Deliver for a
// delivery, tickFresh for a retry timer or a wake -- and drops it on return, so
// no caller can hold one by accident and none is stored on the Worker.
//
// That is a correctness rule, not a tidiness one, and it is what a later reader
// would be tempted to "optimise" by caching the client on the Worker. This
// daemon decides from an issue's LABELS. A memo that outlived its pass would
// answer the NEXT delivery -- the one raised BECAUSE a label changed -- with the
// labels from before that change, so a freshly triggered issue would get no
// agent and an issue whose trigger label was just removed would get one. That
// is strictly worse than the extra fetch the sharing avoids. See
// ghub.DeliveryCache, which states the same rule where the memo lives.
//
// The token is read once per pass rather than once per loop. It is machine-wide
// (~/.agent-utils/env), so it is not per-project, and a pass is a single moment:
// re-reading it per pass keeps the property that rotating the token needs no
// daemon restart.
type access struct {
	token string
	gh    ghub.Client
	// epic is the SAME authenticated client as gh, before the DeliveryCache
	// wrapper. The cache exists to collapse the repeated single-issue fetch a
	// fan-out makes; the epic reads are made once per delivery and have nothing
	// to collapse, so they bypass it rather than teaching it three more methods
	// it would only ever pass through.
	epic ghub.EpicReader
}

// access reads the token and builds the pass's client and memo.
//
// A token that cannot be read schedules no retry anywhere it is called from: a
// bad file mode or an absent file is an operator problem that is identical on
// the next attempt, and retrying would log the same error retry.max times. The
// token itself is never in err: env.go keeps it out deliberately.
func (w *Worker) access() (*access, error) {
	tok, err := w.Token()
	if err != nil {
		return nil, err
	}
	c := w.NewClient(tok)
	acc := &access{token: tok, gh: ghub.NewDeliveryCache(c)}
	// A test's fake client may implement ghub.Client only. Leaving epic nil
	// there is correct: EpicSweep refuses rather than panicking.
	if er, ok := c.(ghub.EpicReader); ok {
		acc.epic = er
	}
	return acc, nil
}

// tickFresh ticks one loop with GitHub access of its OWN.
//
// It is the entry for the two paths that serve a single loop at a moment of
// their own: a retry timer that fires minutes after the delivery that armed it,
// and a wake for a retry deadline. Neither may reuse a delivery's access, and
// this is where that is enforced -- a retry re-runs precisely because something
// failed, and re-deciding it from labels fetched before the failure would make
// the retry a replay of a stale moment rather than a fresh look.
func (w *Worker) tickFresh(ctx context.Context, t Target, number int) {
	acc, err := w.access()
	if err != nil {
		slog.Error("cannot read the github token", "loop", t.LoopName,
			"project", t.ProjectName, "issue", number, "err", err)
		return
	}
	// A retry re-runs the ISSUE pass only. A retry may fire minutes after the
	// merge that caused it, and a sweep then is not what that merge asked for:
	// the base has moved again or has not, and the next trigger -- a merge, a
	// push, or the periodic check -- arms a sweep either way. MergedInto is
	// left empty here on purpose.
	w.tickOne(ctx, t, Delivery{Repo: t.Repo, Number: number}, acc)
}

// tickOne acts on one issue in one loop and decides what the outcome means
// for the retry schedule.
//
// number is carried through every retry this schedules, so a retry re-runs the
// SAME scoped pass rather than widening into a reconcile: a delivery that
// failed and is retried a minute later must still be about the issue the
// delivery named.
func (w *Worker) tickOne(ctx context.Context, t Target, d Delivery, acc *access) {
	key := loopKey{ProjectID: t.ProjectID, LoopName: t.LoopName}

	cfg, deps, cleanup, err := w.Open(t.Ref(), t.ConfigPath, loopcmd.Options{
		Token: acc.token,
		// The pass's own client, so every loop of this delivery reads the
		// delivered issue from one fetch. Open builds its own only when this
		// is nil, which is what keeps `project loop tick` unchanged.
		GH:            acc.gh,
		Epic:          acc.epic,
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
		// A numberless delivery names no issue, so it must not enter the
		// issue retry schedule: that schedule is keyed per loop and would
		// cancel a real issue's pending retry. The sweep is not lost -- the
		// periodic check finds the same stale branches on its next tick.
		if d.Number > 0 {
			w.schedule(ctx, t, d.Number, kindOpen, openRetryMax, func(int) time.Duration { return w.OpenRetryDelay })
		}
		return
	}

	// Gated on the window, not called outright. The leading delivery of a
	// burst ticks here as it always has; the ones behind it are collapsed into
	// the single trailing tick the window fires when it closes.
	//
	// Only the ISSUE pass is gated. The passes below decide different things
	// and state their own timing: tending is already delayed by a timer of its
	// own, and the epic sweep deliberately is not delayed at all.
	//
	// d.Number > 0 keeps a push out of this pass entirely: a push names no
	// issue, so the only work it can start is the sweep armed below.
	if d.Number > 0 && w.openIssueWindow(ctx, t, d.Number) {
		w.issuePass(ctx, t, d, cfg, deps, key)
	}

	// Armed by a merge OR by a push. A merge into the default branch produces
	// a push event too, so the two overlap; armTend collapses them onto one
	// timer, which is why the overlap costs nothing and why the merge trigger
	// stays. It has to stay: a hook that nobody re-registers after this change
	// still carries the old event list, and the merge path is what keeps
	// working for it.
	if cfg.TendPR && (d.IsMergeInto(cfg.DefaultBranch) || d.IsPushTo(cfg.DefaultBranch)) {
		w.armTend(ctx, t, cfg.DefaultBranch)
	}

	// Runs after the issue's own pass, and independently of whether that pass
	// succeeded: the closed issue's pass moves ITS labels, and that says
	// nothing about the siblings it unblocked.
	//
	// It is NOT armed on a timer the way tending is. A tend sweep is delayed to
	// collapse a merge train into one batch of agents; this writes labels, so a
	// burst costs a few more API calls and nothing else. A delay would only
	// postpone the promotion.
	//
	// d.Number > 0 for the reason the issue pass carries it, and it is not
	// redundant: this pass reads d.Number as the CLOSED ISSUE, so a push whose
	// body happened to set ClosedIssue would sweep the epic of issue 0. The
	// handler never sets it for a push today, which makes this an invariant
	// living in another file with nothing pinning it. The guard is one
	// condition and removes the coupling.
	if d.Number > 0 && d.ClosedIssue {
		w.epicPass(ctx, t, d, cfg, deps)
	}

	// Cleanup runs on EVERY close, merged or not -- see loopcmd.CleanupClosedPR
	// for the operator's decision -- so it is gated on ClosedPR alone, not on
	// cfg.TendPR or the merge check above.
	//
	// d.Number > 0 for the reason the epic pass carries it: prNumber IS
	// d.Number here, and a cleanup for pull request 0 would ask the worktree
	// manager to remove "pr-0".
	if d.Number > 0 && d.ClosedPR {
		w.cleanupPass(ctx, t, d, cfg, deps)
	}
}

// issuePass runs the ONE issue this delivery named and decides what the
// outcome means for the retry schedule.
func (w *Worker) issuePass(
	ctx context.Context, t Target, d Delivery, cfg *config.Config, deps loopcmd.Deps, key loopKey,
) {
	number := d.Number

	sum, err := w.RunIssue(ctx, cfg, deps, number)
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			// Still no RETRY: a held lock is not a failed tick, and the
			// pending attempt is cleared too, or the next real failure would
			// resume a backoff list part way through and give up early.
			//
			// But no longer DROPPED. The reasoning that justified dropping it
			// -- that the tick holding the lock reads the same GitHub state a
			// moment later -- holds only if that tick is free to act on this
			// issue, and the commonest holder is the tick that is dispatching
			// FOR it. tendPass reached this conclusion first, for sweeps; see
			// its own ErrHeld branch.
			slog.Info("another tick holds the loop lock; looking at this issue again",
				"loop", cfg.Name, "project", t.ProjectName, "issue", number)
			w.clear(key)
			w.armBusy(ctx, t, number)
			return
		}
		slog.Error("tick failed", "loop", cfg.Name, "project", t.ProjectName,
			"issue", number, "err", err)
		w.schedule(ctx, t, number, kindTick, cfg.Retry.Max, func(n int) time.Duration {
			return w.backoffFor(cfg, n)
		})
		return
	}

	// A pass that decided nothing because an agent already holds this issue
	// has to come back on its own. Nothing else will bring it back: the
	// delivery that would have is the one being answered here, and the labels
	// that ask for the work are already set. This is the whole reason an
	// issue could sit at its trigger label for hours -- the handoff landed
	// while the previous stage's agent was still running, every delivery in
	// that window declined, and the last one to decline armed nothing.
	//
	// sum.Live is scoped to THIS issue, not the loop; see loopcmd.TickIssue.
	if sum.Live > 0 {
		slog.Info("an agent is still working this issue; looking again later",
			"loop", cfg.Name, "project", t.ProjectName, "issue", number,
			"live", sum.Live, "in", w.BusyDelay)
		w.armBusy(ctx, t, number)
	}

	w.clear(key)
}

// openIssueWindow reports whether the caller should tick this issue now, and
// opens the issue's delivery window if it was closed.
//
// The window is LEADING and trailing. The first delivery ticks at once --
// answering an event quickly is the whole point of the daemon, and delaying
// every dispatch by IssueDelay to serve the burst case would be paying the
// burst's cost on every issue that never bursts. The deliveries that arrive
// while the window is open tick nothing of their own; they mark the window
// pending, and one trailing tick runs when it closes.
//
// The trailing tick is not merely a saving. A burst is one edit, and its
// deliveries arrive in the order the labels changed, so the leading tick can
// read the issue HALF EDITED -- with the old status label already removed and
// the new one not yet added. Only a tick after the window closes is
// guaranteed to have seen the edit whole.
//
// A zero or negative IssueDelay turns windowing off: every delivery ticks, as
// every delivery did before windows existed. It is the escape hatch for an
// operator who would rather pay the duplicate ticks than the trailing one.
func (w *Worker) openIssueWindow(ctx context.Context, t Target, number int) bool {
	if w.IssueDelay <= 0 {
		return true
	}
	key := issueKey{ProjectID: t.ProjectID, LoopName: t.LoopName, Number: number}

	w.mu.Lock()
	if ent, open := w.windows[key]; open {
		// Marked, not counted: a window closes into ONE trailing tick however
		// many deliveries rode it.
		ent.pending = true
		w.mu.Unlock()
		slog.Info("a delivery window is already open for this issue; riding it",
			"loop", t.LoopName, "project", t.ProjectName, "issue", number)
		return false
	}
	// Registered before the timer is built, so a delivery arriving between
	// these two statements rides this window instead of opening its own. The
	// entry is an identity token for the same reason armTend's is.
	ent := &issueWindow{}
	w.windows[key] = ent
	w.mu.Unlock()

	timer := w.After(w.IssueDelay, func() {
		w.mu.Lock()
		trailing := false
		if cur, ok := w.windows[key]; ok && cur == ent {
			trailing = ent.pending
			delete(w.windows, key)
		}
		w.mu.Unlock()
		// Nothing rode this window, so its leading tick is the whole story.
		if !trailing {
			return
		}
		// Same rule schedule and armTend state: a cancelled context means the
		// daemon is shutting down, and a daemon told to stop starts no agent.
		if ctx.Err() != nil {
			return
		}
		w.issueFresh(ctx, t, number)
	})

	w.mu.Lock()
	if cur, ok := w.windows[key]; ok && cur == ent {
		cur.timer = timer
	}
	w.mu.Unlock()

	return true
}

// armBusy schedules another look at an issue whose own agent is still
// running, unless a look is already armed for it.
//
// It does NOT re-tick blindly. The timer first asks IssueBusy, which reads the
// dispatch rows and the process table and never GitHub: while the agent is
// alive it simply arms again, so waiting out an eight-hour agent costs a
// row read and a kill(0) a minute rather than eight hours of fetches. Only
// when the agent is gone does it run a real pass.
//
// It is uncapped, and self-terminating: the re-arm happens only while a
// process is alive, so the chain ends when that process does.
func (w *Worker) armBusy(ctx context.Context, t Target, number int) {
	key := issueKey{ProjectID: t.ProjectID, LoopName: t.LoopName, Number: number}

	w.mu.Lock()
	if _, armed := w.busy[key]; armed {
		w.mu.Unlock()
		slog.Info("a re-look is already armed for this issue; riding it",
			"loop", t.LoopName, "project", t.ProjectName, "issue", number)
		return
	}
	ent := &busyTimer{}
	w.busy[key] = ent
	w.mu.Unlock()

	timer := w.After(w.BusyDelay, func() {
		w.mu.Lock()
		if cur, ok := w.busy[key]; ok && cur == ent {
			delete(w.busy, key)
		}
		w.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if w.IssueBusy(t, number) {
			// Still working. Wait again, without a tick and without a line in
			// the log: this fires every BusyDelay for the life of the agent,
			// and a line each time would bury everything else the daemon says.
			w.armBusy(ctx, t, number)
			return
		}
		slog.Info("the agent working this issue has finished; looking again now",
			"loop", t.LoopName, "project", t.ProjectName, "issue", number)
		w.issueFresh(ctx, t, number)
	})

	w.mu.Lock()
	if cur, ok := w.busy[key]; ok && cur == ent {
		cur.timer = timer
	}
	w.mu.Unlock()
}

// issueBusy is the production IssueBusy: it answers from the machine-wide
// dispatch rows and the kernel, and never from GitHub.
func (w *Worker) issueBusy(t Target, number int) bool {
	if w.DB == nil {
		return false
	}
	running, err := w.DB.RunningDispatches()
	if err != nil {
		// NOT "still busy". A database this pass could not read is no evidence
		// that an agent is alive, and answering true would re-arm on that same
		// broken read for as long as the daemon ran, never ticking. Answering
		// false sends it down the full pass, which re-reads the rows under the
		// loop's lock and arms another re-look if the agent really is there.
		slog.Error("cannot read the running dispatches; looking at the issue instead",
			"loop", t.LoopName, "project", t.ProjectName, "issue", number, "err", err)
		return false
	}
	for _, d := range running {
		if d.ProjectID != t.ProjectID || d.Loop != t.LoopName || d.Number != number {
			continue
		}
		if proc.IsAlive(d.PID, d.RunnerID()) {
			return true
		}
	}
	return false
}

// issueFresh runs the ISSUE pass of one loop with GitHub access of its own.
//
// It is what the delivery window's trailing tick and the busy re-look both
// run, and it exists for the reason tickFresh states: both fire long after the
// delivery that armed them, and re-deciding from that delivery's fetched
// labels would decide from a moment that has passed. The busy re-look makes
// that sharper still -- what it waited for was an agent, and an agent's last
// act is to change the labels this pass is about to read.
//
// It runs the issue pass ALONE. Going back through tickOne would re-enter the
// window that fired it, and would re-run the tend, epic and cleanup passes
// that belong to the delivery rather than to this issue.
func (w *Worker) issueFresh(ctx context.Context, t Target, number int) {
	acc, err := w.access()
	if err != nil {
		slog.Error("cannot read the github token", "loop", t.LoopName,
			"project", t.ProjectName, "issue", number, "err", err)
		return
	}
	cfg, deps, cleanup, err := w.Open(t.Ref(), t.ConfigPath, loopcmd.Options{
		Token: acc.token, GH: acc.gh, Epic: acc.epic, RequireGitHub: true,
		MigrationPolicy: loopcmd.FailOnUnimported,
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		slog.Error("cannot open loop", "loop", t.LoopName, "project", t.ProjectName,
			"config", t.ConfigPath, "err", err)
		w.schedule(ctx, t, number, kindOpen, openRetryMax, func(int) time.Duration { return w.OpenRetryDelay })
		return
	}
	key := loopKey{ProjectID: t.ProjectID, LoopName: t.LoopName}
	w.issuePass(ctx, t, Delivery{Repo: t.Repo, Number: number}, cfg, deps, key)
}

// armTend schedules the tend sweep a merge calls for, unless one is already
// armed for this loop.
//
// The wait exists because a merge train is normal: several pull requests merge
// within a few minutes, and each merge leaves every other branch further
// behind. Sweeping per merge would dispatch up to maxTendPerSweep agents each
// time, and the loop lock does not prevent it -- the lock only collapses
// sweeps that OVERLAP, and a sweep whose agents have finished suppresses
// nothing. Riding the armed timer rather than resetting it bounds the wait, so
// a long train still gets a sweep every TendDelay rather than none until it
// stops.
func (w *Worker) armTend(ctx context.Context, t Target, base string) {
	key := loopKey{ProjectID: t.ProjectID, LoopName: t.LoopName}

	w.mu.Lock()
	if _, armed := w.tends[key]; armed {
		w.mu.Unlock()
		slog.Info("a tend sweep is already armed for this loop; riding it",
			"loop", t.LoopName, "project", t.ProjectName, "base", base)
		return
	}
	// Registered before the timer is built, so a second merge arriving between
	// these two statements rides this one instead of arming its own.
	//
	// The entry is an identity token, not a presence flag. Storing the timer
	// back below has to know the entry is still THIS arm's: the timer can fire
	// and delete the entry before the store, a later merge can then register
	// its own, and a presence check would overwrite that live timer with this
	// already-fired one -- leaving stopAll stopping a dead timer while a live
	// one fires after shutdown.
	ent := &tendTimer{}
	w.tends[key] = ent
	w.mu.Unlock()

	timer := w.After(w.TendDelay, func() {
		w.mu.Lock()
		if cur, ok := w.tends[key]; ok && cur == ent {
			delete(w.tends, key)
		}
		w.mu.Unlock()
		// Same rule schedule states: a cancelled context here means the daemon
		// is shutting down, and a daemon told to stop starts no new agent.
		if ctx.Err() != nil {
			return
		}
		w.tendFresh(ctx, t, base)
	})

	w.mu.Lock()
	if cur, ok := w.tends[key]; ok && cur == ent {
		cur.timer = timer
	}
	w.mu.Unlock()

	slog.Info("armed a tend sweep", "loop", t.LoopName, "project", t.ProjectName,
		"base", base, "in", w.TendDelay)
}

// tendFresh reads its own token and opens its own loop, like tickFresh: the
// access of the delivery that armed the timer is gone with Deliver's frame,
// and reusing one would decide from labels read a minute ago.
func (w *Worker) tendFresh(ctx context.Context, t Target, base string) {
	acc, err := w.access()
	if err != nil {
		slog.Error("cannot read the github token for a tend sweep",
			"loop", t.LoopName, "project", t.ProjectName, "err", err)
		return
	}
	cfg, deps, cleanup, err := w.Open(t.Ref(), t.ConfigPath, loopcmd.Options{
		Token: acc.token, GH: acc.gh, RequireGitHub: true,
		MigrationPolicy: loopcmd.FailOnUnimported,
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		slog.Error("cannot open loop for a tend sweep", "loop", t.LoopName,
			"project", t.ProjectName, "config", t.ConfigPath, "err", err)
		return
	}
	w.tendPass(ctx, t, cfg, deps, base)
}

// tendPass runs the sweep and decides what its outcome means.
//
// Nothing. A failure is logged and dropped, and schedules NO retry: the retry
// path re-runs the ISSUE pass, so a sweep failure would spend an issue's retry
// budget on something that issue did not do, and the work is not lost because
// the next trigger arms another sweep: a merge, a push to the default branch,
// or the periodic tend check, whichever comes first.
func (w *Worker) tendPass(
	ctx context.Context, t Target, cfg *config.Config, deps loopcmd.Deps, base string,
) {
	if _, err := w.RunTend(ctx, cfg, deps, base); err != nil {
		if errors.Is(err, lock.ErrHeld) {
			// Re-armed, not dropped. issuePass can drop a delivery that finds
			// the lock held because the holder decides that same issue a moment
			// later; TendSweep's own comment records that this reasoning is
			// FALSE for a sweep, which decides no issue but the ones it tends.
			// Dropping it here would lose the merge's rebases until another
			// merge landed. The coalescing map bounds this to one arm in
			// flight.
			slog.Info("another tick holds the loop lock; re-arming the tend sweep",
				"loop", cfg.Name, "project", t.ProjectName)
			w.armTend(ctx, t, base)
			return
		}
		slog.Error("tend sweep failed", "loop", cfg.Name, "project", t.ProjectName, "err", err)
	}
}

// tendCheckPass looks for a pull request that has fallen behind its base, in
// every loop on this machine that tends.
//
// It walks the REGISTRY rather than reacting to a delivery. That is the whole
// point: a repository whose webhook is missing, broken, or simply quiet sends
// nothing, and before this pass its stale pull requests waited for a merge
// that might never arrive.
//
// It dispatches nothing itself. A loop with a stale pull request gets an
// armTend, so the periodic trigger and the merge trigger end in the same
// sweep, on the same timer, holding the same loop lock.
func (w *Worker) tendCheckPass(ctx context.Context) {
	// TWO contexts, deliberately, and mixing them up is a real bug.
	//
	// checkCtx bounds this pass's own git and database work. The pass runs on
	// the Serve goroutine -- the single wake loop for every project on this
	// machine, the thing that fires retry deadlines -- and it shells out to
	// git, which talks to a network it does not control. With no deadline one
	// unreachable remote stops every retry of every loop until the daemon is
	// restarted.
	//
	// ctx stays the DAEMON's context, and it is what armTend must receive.
	// armTend's timer callback tests ctx.Err() before it runs the sweep, so
	// arming with checkCtx would cancel the sweep this pass just asked for as
	// soon as the pass returned.
	checkCtx, cancel := context.WithTimeout(ctx, tendPassTimeout)
	defer cancel()

	routes, err := w.ScanTargets()
	if err != nil {
		// Logged and dropped, like Deliver's routing failure: the failure is
		// machine-wide, so there is no single loop to record it against, and
		// the next interval re-runs the whole pass anyway.
		slog.Error("cannot scan projects for the tend check", "err", err)
		return
	}
	// ONE access for the whole pass, which bends the lifetime rule access
	// states, so it is bounded here rather than left to be discovered.
	// ghub.DeliveryCache memoises Issue and PullRequest, and this cache lives
	// for every loop of the pass -- up to tendPassTimeout -- where a delivery's
	// lives for one fan-out. That is safe only because loopcmd.TendCheck reads
	// ListOpenPullRequests and ListOpenIssues, neither of which is memoised. A
	// TendCheck that reached for Issue or PullRequest would start deciding
	// from labels fetched minutes ago, for a different loop; give this pass an
	// access per loop before making that change.
	acc, err := w.access()
	if err != nil {
		slog.Error("cannot read the github token for the tend check", "err", err)
		return
	}

	// Before the loops below, so a scan that named fewer loops than the last
	// one drops their entries in the same pass that stopped using them.
	w.pruneConfirms(routes.Targets)

	for _, t := range routes.Targets {
		if !t.TendPR {
			continue
		}
		// Checked between loops, not only at the top: the deadline above and a
		// daemon shutdown both land here, and a pass that ignored them would
		// keep opening databases through the stop.
		if checkCtx.Err() != nil {
			return
		}
		w.tendCheckOne(ctx, checkCtx, t, acc)
	}
}

// pruneConfirms drops the confirm timestamp of every loop the scan no longer
// names.
//
// confirms is the only map on Worker with no removal path of its own -- tends,
// windows and busy each delete their entry when their timer fires. An entry is
// two words, so this is a small leak, but it is an unbounded one: a project
// deleted and re-registered under a new id, or a loop renamed, leaves a key
// nothing will ever look up again for as long as the daemon runs. Deleting a
// live loop's entry would cost one forced pass, which is what a restart costs
// anyway, so the failure mode of being wrong here is bounded too.
func (w *Worker) pruneConfirms(targets []Target) {
	live := make(map[loopKey]bool, len(targets))
	for _, t := range targets {
		live[loopKey{ProjectID: t.ProjectID, LoopName: t.LoopName}] = true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for key := range w.confirms {
		if !live[key] {
			delete(w.confirms, key)
		}
	}
}

// tendCheckOne runs the check for one loop and arms a sweep when it finds
// something.
//
// It takes both contexts for the reason tendCheckPass states: checkCtx bounds
// the work this function does, and ctx -- the daemon's -- outlives it and is
// what armTend must be handed.
func (w *Worker) tendCheckOne(ctx, checkCtx context.Context, t Target, acc *access) {
	cfg, deps, cleanup, err := w.Open(t.Ref(), t.ConfigPath, loopcmd.Options{
		Token: acc.token, GH: acc.gh, RequireGitHub: true,
		MigrationPolicy: loopcmd.FailOnUnimported,
	})
	// Deferred before any branch below can return. Open holds a SQLite
	// handle, and this pass calls Open once per loop per interval for the life
	// of the daemon, so a single missed cleanup is not a leak that stops -- it
	// is one handle every interval, forever. tickOne records the same rule for
	// the delivery path. The nil check is for the error path, where
	// loopcmd.Open returns a nil cleanup.
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		// Logged and dropped, with NO retry scheduled. The retry schedule is
		// keyed per loop and spends an issue's budget; this pass names no
		// issue, and the next interval re-runs it in full.
		slog.Error("cannot open loop for the tend check", "loop", t.LoopName,
			"project", t.ProjectName, "config", t.ConfigPath, "err", err)
		return
	}

	key := loopKey{ProjectID: t.ProjectID, LoopName: t.LoopName}
	w.mu.Lock()
	last, seen := w.confirms[key]
	w.mu.Unlock()
	// The first pass after start forces the confirm, and so does one every
	// tendConfirmInterval after that: the local gate can only skip the API
	// calls when it has rows to trust, so a loop with no rows would otherwise
	// stay silent forever and a row that drifted would stay wrong forever.
	force := !seen || w.Now().Sub(last) >= tendConfirmInterval

	res, err := w.RunTendCheck(checkCtx, cfg, deps, force)
	if err != nil {
		// Logged and dropped, for the reason tendPass gives: this pass decides
		// no issue, so there is no issue whose retry budget could pay for it,
		// and the next interval tries again.
		slog.Error("tend check failed", "loop", cfg.Name, "project", t.ProjectName, "err", err)
		return
	}
	if res.Confirmed {
		w.mu.Lock()
		w.confirms[key] = w.Now()
		w.mu.Unlock()
	}
	if res.Stale == 0 {
		// Deliberately silent. This is the common case, once every interval,
		// for every loop on the machine; a line here would bury every line
		// that means something.
		return
	}
	slog.Info("the tend check found a pull request behind its base",
		"loop", cfg.Name, "project", t.ProjectName, "stale", res.Stale)
	w.armTend(ctx, t, cfg.DefaultBranch)
}

// tendTicker returns the channel the periodic tend check fires on and the
// func that stops it. The channel is nil, and the stop a no-op, when the check
// is disabled.
//
// A nil channel blocks forever in a select, which is exactly what "disabled"
// means here -- no branch, no flag, nothing for a later reader to get subtly
// wrong. It is a method rather than four lines inside Serve so that the
// decision itself can be tested: Serve is a loop around seams, and a test of
// it would return at its own ctx.Err() guard before ever reaching the select.
//
// The stop func is returned rather than left to the garbage collector. An
// unreachable ticker is collectable on this Go version whether it was stopped
// or not, so nothing leaks either way -- but that is a language-version
// property, and handing the caller the same defer every other timer in this
// file gets costs one line and needs no such argument.
func (w *Worker) tendTicker() (<-chan time.Time, func()) {
	if w.TendInterval <= 0 {
		return nil, func() {}
	}
	tk := time.NewTicker(w.TendInterval)
	return tk.C, tk.Stop
}

// epicPass promotes the sub-issues the delivery's closed issue unblocked.
//
// A failure is logged and dropped. It schedules NO retry, for the same reason
// the cleanup pass does not: the cron sweep re-derives this from scratch on its
// next tick, so a missed promotion is recovered without any state kept here.
func (w *Worker) epicPass(
	ctx context.Context, t Target, d Delivery, cfg *config.Config, deps loopcmd.Deps,
) {
	sum, err := w.RunEpic(ctx, cfg, deps, d.Number)
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			slog.Info("skipping epic sweep: another tick holds the loop lock",
				"loop", cfg.Name, "project", t.ProjectName, "issue", d.Number)
			return
		}
		slog.Error("epic sweep failed", "loop", cfg.Name, "project", t.ProjectName,
			"issue", d.Number, "err", err)
		return
	}
	if sum.Promoted > 0 {
		slog.Info("epic sweep promoted sub-issues", "loop", cfg.Name,
			"project", t.ProjectName, "issue", d.Number, "promoted", sum.Promoted)
	}
}

// cleanupPass removes the worktrees a closed pull request leaves behind.
//
// It runs on every close, merged or not: an unmerged close often means the
// work continues and a replacement gets pushed, but the operator chose to
// reclaim the disk rather than guess. See loopcmd.CleanupClosedPR for the
// live-dispatch guard that keeps this from touching work in progress -- it
// does not protect uncommitted, unpushed work sitting in an otherwise idle
// worktree, which is the accepted risk.
//
// A failure is logged and dropped, like a failed tend sweep: it schedules no
// retry, because a retry re-runs the issue pass, and a cleanup failure is not
// something that pass caused or could fix. The worktree, if it survived,
// simply waits for this loop's next closed-pull-request delivery.
func (w *Worker) cleanupPass(
	ctx context.Context, t Target, d Delivery, cfg *config.Config, deps loopcmd.Deps,
) {
	if err := w.RunCleanup(ctx, cfg, deps, d.Number); err != nil {
		if errors.Is(err, lock.ErrHeld) {
			slog.Info("skipping worktree cleanup: another tick holds the loop lock",
				"loop", cfg.Name, "project", t.ProjectName, "pr", d.Number)
			return
		}
		slog.Error("worktree cleanup failed", "loop", cfg.Name, "project", t.ProjectName,
			"pr", d.Number, "err", err)
	}
}

// backoffFor returns the wait before retry n (counted from zero) of cfg.
//
// The list is clamped to its last entry, treated as zero when it is empty,
// and floored at MinRetryDelay. Empty is not a defensive nicety: retry.max
// may legitimately be 0, which means never retry and leaves retry.backoff
// out of the file entirely, so an unguarded index would panic a daemon that
// has no supervisor to restart it.
//
// The clamp is kept even though config.validate enforces
// len(retry.backoff) >= retry.max, which makes n >= len unreachable through
// the loaded-configuration path today. That invariant lives in another
// package, and the cost of relying on it here is an index panic that takes
// the whole daemon down -- every loop on the machine, not just the one with
// the short list. Two lines is the right price for not owning that risk.
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
func (w *Worker) schedule(ctx context.Context, t Target, number int, kind retryKind, max int, delay func(n int) time.Duration) {
	key := loopKey{ProjectID: t.ProjectID, LoopName: t.LoopName}

	w.mu.Lock()
	defer w.mu.Unlock()

	a := w.pending[key]
	if a == nil {
		a = &attempt{}
		w.pending[key] = a
	}
	// The budget spent is the one belonging to this failure kind; see
	// attempt.
	spent := a.counter(kind)
	// Scheduling again for a key that already has a timer stops the old one
	// first. Without this a burst of deliveries for one loop would leave
	// several timers armed for it and run several ticks at once, each of
	// which would then fight for the same loop lock.
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}

	if *spent >= max {
		// The budget is spent. The entry is dropped rather than kept at the
		// cap, so the next delivery for this loop starts a fresh run of
		// attempts instead of inheriting an exhausted one.
		delete(w.pending, key)
		slog.Warn("retry budget spent; waiting for the next delivery",
			"loop", t.LoopName, "project", t.ProjectName, "attempts", *spent)
		return
	}

	d := delay(*spent)
	*spent++
	n := *spent
	// The callback runs the tick itself rather than handing it back to the
	// caller, which means a retry does not pass through the handler's
	// in-flight semaphore. It is bounded instead by the number of loops on
	// this machine (one timer per loop at a time, stopped before another is
	// armed), which is not something a stranger controls. The two bounds are
	// separate, and nothing joins them today; a shared one would belong in
	// the command that owns both, not here.
	a.timer = w.After(d, func() {
		// A cancelled context here means the daemon is shutting down for ONE
		// of the two retry origins: a retry armed from Wake captures Serve's
		// workerCtx, which drainAndClose cancels. It proves nothing for a
		// retry armed from an HTTP-delivered tick, which captures tickCtx --
		// deliberately NOT cancelled during shutdown, so an in-flight tick is
		// allowed to finish. That origin is covered by the shuttingDown gate
		// in cmd/agent-utils/listener.go's instrumentRetries, which is why
		// this check is not the whole story. It stays because a Wake-armed
		// timer that had already fired and was waiting on the mutex is still
		// running here, past everything stopAll could have stopped.
		if ctx.Err() != nil {
			return
		}
		slog.Info("retrying a failed tick", "loop", t.LoopName,
			"project", t.ProjectName, "issue", number, "attempt", n)
		// tickFresh, never the access of the delivery that armed this timer:
		// that value is gone with Deliver's frame, and reusing one would
		// re-decide the issue from labels read before the failure.
		w.tickFresh(ctx, t, number)
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

// stopAll stops every pending retry timer AND every armed tend sweep timer,
// so no ALREADY-ARMED timer of either kind fires.
//
// A sweep timer left out of this would fire after the daemon was told to
// stop and dispatch up to maxTendPerSweep agents on the way out -- the same
// hazard an already-armed retry timer would be, for a batch of agents rather
// than one.
//
// It is not, on its own, "a daemon that has been told to stop starts no new
// agent": it runs when Serve returns, which is drainAndClose step 2, BEFORE
// the drain in step 3. A tick still draining afterwards can call schedule or
// armTend and arm a NEW timer that this pass never saw. That timer is
// stopped instead by the shuttingDown gate in cmd/agent-utils/listener.go's
// instrumentRetries, which wraps Worker.After itself and so is checked when
// the timer FIRES -- whichever seam armed it -- rather than when it is
// armed.
func (w *Worker) stopAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for key, a := range w.pending {
		if a.timer != nil {
			a.timer.Stop()
		}
		delete(w.pending, key)
	}
	for key, ent := range w.tends {
		if ent != nil && ent.timer != nil {
			ent.timer.Stop()
		}
		delete(w.tends, key)
	}
	// A window's trailing tick and a busy re-look both dispatch, so both are
	// the kind of already-armed timer this exists to stop.
	for key, ent := range w.windows {
		if ent != nil && ent.timer != nil {
			ent.timer.Stop()
		}
		delete(w.windows, key)
	}
	for key, ent := range w.busy {
		if ent != nil && ent.timer != nil {
			ent.timer.Stop()
		}
		delete(w.busy, key)
	}
}

// Wake acts on the one ISSUE whose retry deadline has passed, and returns when
// the next deadline is due. ok is false when no deadline is pending.
func (w *Worker) Wake(ctx context.Context) (next time.Time, ok bool) {
	now := w.Now()

	// skip collects the loops this pass has found it cannot serve, so the
	// next query steps PAST them instead of being handed the same row again.
	//
	// Without it one unservable deadline starves every other loop on the
	// machine. EarliestRetryAfterAt returns the single earliest row; a loop
	// that cannot be routed is deliberately neither cleared nor advanced, so
	// that row stays the earliest forever and no other project's durable
	// retry ever runs again -- they fire only if a webhook delivery happens
	// to tick them. One registered project whose directory was deleted, or
	// one permanently unparsable yaml in a project's configs (which makes
	// EVERY loop of that project unroutable), is enough to reach it. This is
	// not a micro-optimisation to fold back into a single query.
	//
	// It is rebuilt on every wake rather than held on the Worker: nothing
	// about being unservable is durable, so a set that outlived the pass
	// would have to be expired, and an entry expired wrongly is a loop whose
	// retries stop. Rebuilding bounds it by the loops that are unservable
	// right now, cannot keep an entry for a loop that has since been fixed,
	// and re-probes everything after a restart. The cost is one TargetFor
	// per unservable loop per wake interval, which is the same probe the
	// single-row version already paid for the first of them.
	var skip []store.LoopKey

	// skipped is the first past-due deadline this pass stepped over, and it
	// is what the caller is told to wake for. Every skipped deadline is past
	// due, so it is earlier than anything the pass goes on to find:
	// returning it holds Serve at its MinWakeInterval floor and re-probes
	// the skipped loops a wake later. Returning the later deadline instead
	// would let a loop whose config was fixed a minute ago sit unserved
	// until some unrelated deadline hours away.
	var skipped time.Time
	answer := func(at time.Time, ok bool) (time.Time, bool) {
		if !skipped.IsZero() {
			return skipped, true
		}
		return at, ok
	}

	// Each pass adds one loop to skip and the query excludes it, so this
	// ends after at most one iteration per loop that has a pending row.
	for {
		// EarliestRetryAfterAt, not EarliestRetryAfter: the cooldown
		// boundary is judged against this worker's own clock, so a test can
		// freeze both the deadline comparison below and the one inside the
		// query at the same instant.
		due, found, err := w.DB.EarliestRetryAfterAt(now, skip)
		if err != nil {
			// Reported and treated as "nothing pending". The caller waits
			// its wake interval and asks again, which is the right response
			// to a database that is momentarily unreadable.
			slog.Error("cannot read the earliest retry deadline", "err", err)
			return answer(time.Time{}, false)
		}
		if !found {
			return answer(time.Time{}, false)
		}
		if due.At.After(now) {
			// Nothing else is due. The caller sets its timer for this and
			// does not tick.
			return answer(due.At, true)
		}

		slog.Info("waking a loop for a retry deadline", "loop", due.Loop,
			"issue", due.Number, "due", due.At)

		// TargetFor, never Targets(due.Repo): the deadline belongs to one
		// project's issue, and repository routing would dispatch agents in
		// every other project that watches the same repository, on that
		// project's own token budget.
		t, routing, err := w.TargetFor(due.ProjectID, due.Loop)
		if err != nil {
			// Returned, not skipped: TargetFor fails only when the registry
			// itself cannot be read, which is machine-wide, so every other
			// loop this pass tried would fail the same way.
			slog.Error("cannot route retry deadline", "loop", due.Loop,
				"project", due.ProjectID, "issue", due.Number, "err", err)
			return answer(due.At, true)
		}
		key := loopKey{ProjectID: due.ProjectID, LoopName: due.Loop}

		if routing == RouteFound {
			// The loop routed, so whatever made it unroutable before is
			// over. The count starts again from zero, which is what keeps a
			// loop that is briefly unreadable every so often from ever
			// reaching the threshold, and the throttle is dropped so the
			// next occurrence logs its first line rather than being
			// suppressed by an interval opened hours ago.
			w.forgetUnroutable(key)
			w.unroutable.forget(unroutableKey(key))

			// The deadline names the ISSUE it belongs to, so the wake acts on
			// that issue alone. A whole-loop reconcile here would decide every
			// open issue of the loop because ONE of them came due, which is the
			// same repository-wide cost a delivery used to pay.
			// tickFresh: a wake is its own moment, minutes or hours after
			// whatever delivery flagged this issue, so it reads GitHub afresh.
			w.tickFresh(ctx, t, due.Number)

			// The handled deadline is returned, not a fresh query for the
			// next one. The tick may legitimately have decided nothing (its
			// own backoff, a tripped breaker), which leaves this row past
			// due, and Serve's MinWakeInterval floor is what keeps that
			// from spinning.
			return answer(due.At, true)
		}

		if routing == RouteGone {
			w.noteUnroutable(key, due)
		} else {
			// RouteUnknown is waited on indefinitely, never cleared. This is
			// the operator mid-edit, the volume not mounted yet, the
			// permission changed during a restore -- all conditions that end
			// when someone fixes them, none of them a reason to destroy a
			// pending retry. The cost of waiting is now one throttled
			// warning; the cost of acting would be an issue left holding an
			// in-flight label with no agent and nothing to re-derive its
			// flag.
			//
			// The count is forgotten, not merely left alone: orphanClearAfter
			// counts CONSECUTIVE gone observations, and an observation that
			// could not tell is not one of them.
			w.forgetUnroutable(key)
			w.warnUnroutable(key, due)
		}

		if skipped.IsZero() {
			skipped = due.At
		}
		skip = append(skip, store.LoopKey{ProjectID: due.ProjectID, Loop: due.Loop})
	}
}

// unroutableKey is one loop's throttle key. It is not loopKey itself only
// because throttledLog is shared with the HTTP side, which keys by string.
func unroutableKey(key loopKey) string {
	return key.ProjectID + "/" + key.LoopName
}

// warnUnroutable reports a deadline whose loop cannot be resolved right now,
// at most once per unroutableLogInterval for that loop.
//
// Throttled because the condition is unbounded in time by design: Wake never
// clears such a deadline, so an unparsable config left over a weekend would
// otherwise write one line per loop per wake -- thousands a day into the same
// unrotated launchd stdout log the HTTP rejections are throttled for. The
// operator needs to know it started and that it is still going, not a line per
// probe.
func (w *Worker) warnUnroutable(key loopKey, due store.RetryDue) {
	if ok, suppressed := w.unroutable.allow(unroutableKey(key)); ok {
		slog.Warn("cannot route a retry deadline right now; leaving it pending",
			"loop", due.Loop, "project", due.ProjectID, "issue", due.Number,
			"suppressed_since_last", suppressed)
	}
}

// noteUnroutable records that a past-due deadline's loop is definitely gone,
// and clears its failure flag once that has been observed enough times.
//
// It is called for RouteGone only. A project can be deleted, or a loop's
// configuration file removed, while an issue row carrying needs_retry
// survives in the canonical database. That row is permanently past due and
// permanently unroutable, so leaving it alone would make the wake loop
// re-enter Wake every MinWakeInterval for the life of the daemon, re-logging
// the same warning and re-reading the same row -- the very hot loop
// EarliestRetryAfterAt's own predicate exists to prevent. Clearing the flag
// removes the row from that predicate, which is exactly what a tick does for
// a failure no retry can act on (loopcmd.act, KindClearRetry), and it is
// durable: an in-memory skip list would lose the fix on the next restart and
// spin again.
//
// The clear still waits for orphanClearAfter consecutive observations,
// because it is irreversible; see orphanClearAfter for what a counter still
// buys once the signal itself is no longer ambiguous. Until then the wake
// loop is already bounded by its own MinWakeInterval, so the delay costs
// nothing but a repeated warning.
//
// A failed clear leaves the count in place, so the next wake retries it at
// once: a write that fails means the database itself is unwritable, and there
// is no state a daemon could keep that would fix that.
//
// While the count runs, the wake pass steps over this loop as it does over an
// unresolvable one, so the three observations cost the OTHER loops nothing.
func (w *Worker) noteUnroutable(key loopKey, due store.RetryDue) {
	w.mu.Lock()
	w.orphans[key]++
	seen := w.orphans[key]
	w.mu.Unlock()

	if seen < orphanClearAfter {
		slog.Warn("a retry deadline names a loop that no longer exists",
			"loop", due.Loop, "project", due.ProjectID, "issue", due.Number,
			"observations", seen)
		return
	}

	slog.Warn("clearing a retry deadline whose loop has stayed gone",
		"loop", due.Loop, "project", due.ProjectID, "issue", due.Number,
		"observations", seen)
	if err := w.DB.Project(due.ProjectID).ClearNeedsRetry(due.Loop, due.Repo, due.Number); err != nil {
		slog.Error("cannot clear an orphaned retry deadline", "loop", due.Loop,
			"project", due.ProjectID, "issue", due.Number, "err", err)
		return
	}
	// The row is out of the wake query now, so the counter has nothing left
	// to count. Dropping it keeps the map bounded by the loops that are
	// currently unroutable rather than by every loop that ever was.
	w.forgetUnroutable(key)
}

// forgetUnroutable drops a loop's unroutable count.
func (w *Worker) forgetUnroutable(key loopKey) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.orphans, key)
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

	// Built once, outside the loop: a ticker rebuilt on every wake would
	// restart its period on every retry deadline and, on a busy machine, never
	// reach the interval at all. It is deliberately not folded into the wake
	// timer above -- that one is driven by retry deadlines and floored at
	// MinWakeInterval, which is a different schedule entirely.
	tendC, stopTend := w.tendTicker()
	defer stopTend()

	// Swept BEFORE the first wake, because a daemon starting is the moment a
	// crash is discovered. The rows a machine leaves behind carry no retry
	// deadline -- only a reap writes one -- so Wake cannot see them, and
	// waiting for the sweep interval would idle the machine for five minutes
	// after every restart for no reason.
	w.reapOrphans(ctx)

	// A separate timer from the wake, on its own much longer interval. The two
	// answer different questions: a wake serves deadlines this daemon wrote
	// and knows are coming, and a sweep looks for rows nothing wrote a
	// deadline for. Folding the sweep into the wake would run a machine-wide
	// query every thirty seconds to find something that appears when a process
	// dies without recording an outcome.
	sweep := time.NewTicker(w.orphanSweepEvery())
	defer sweep.Stop()

	for {
		// Checked before Wake, not only in the select below. Wake is
		// synchronous and runs a whole tick, so a cancellation that arrived
		// while the timer was firing would otherwise start an agent during
		// shutdown and hold the shutdown open for the length of that tick.
		if ctx.Err() != nil {
			w.stopAll()
			return
		}

		next, ok := w.Wake(ctx)
		timer.Reset(w.wakeDelay(next, ok))

		select {
		case <-ctx.Done():
			w.stopAll()
			return
		case <-tendC:
			// nil when the check is disabled, and a nil channel blocks
			// forever, so this case simply never fires then.
			w.tendCheckPass(ctx)
		case <-sweep.C:
			// Falls through to the top of the loop, which calls Wake. That is
			// deliberate: the sweep has just stamped deadlines that are due
			// now, and waiting a wake interval to serve the first of them
			// would add latency to exactly the case this exists for.
			w.reapOrphans(ctx)
		case <-timer.C:
		}
	}
}

// wakeDelay returns how long Serve waits before the next Wake, given what
// the last one reported.
//
// It is split out of Serve because this arithmetic is the only thing
// standing between a stale past-due row and a loop that re-ticks as fast as
// the GitHub API answers: Wake returns a deadline already in the past
// whenever the tick it ran decided nothing (its own backoff, a tripped
// breaker), and a wait taken straight from that value would be zero or
// negative. Inside Serve it could only be tested through a goroutine and a
// real clock; here three table rows cover it.
func (w *Worker) wakeDelay(next time.Time, ok bool) time.Duration {
	d := w.MinWakeInterval
	// MinWakeInterval is exported and documented as shrinkable for tests, so
	// a caller can set it to zero -- which would produce exactly the tight
	// poll-and-tick loop the floor exists to prevent. The default is
	// reasserted here rather than only in NewWorker, so no assignment to the
	// field can falsify the floor.
	if d <= 0 {
		d = defaultMinWakeInterval
	}
	if ok {
		if until := next.Sub(w.Now()); until > d {
			d = until
		}
	}
	return d
}
