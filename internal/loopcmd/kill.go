package loopcmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/project"
	"github.com/seanmcgary/agent-utils/internal/registry"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// killedByOperatorReason is the stopped_reason recorded on an issue an
// operator killed, and the api_error recorded on the dispatch row. It is
// distinct from a label parse error, which carries its own text instead.
const killedByOperatorReason = "killed by operator"

// DefaultKillTimeout is how long `sessions kill` waits for a runner to exit
// after SIGTERM before reporting it "still alive".
const DefaultKillTimeout = 30 * time.Second

// Selector names the target of a kill or a resume. Exactly one of Session,
// Issue, or All chooses the set of issues to act on -- the flag lives on the
// issue, and the signal goes to the dispatch.
type Selector struct {
	Session string
	Issue   int
	All     bool
	// Project and Loop narrow --issue and --all. They are never selectors on
	// their own.
	Project string
	Loop    string
}

// Validate reports whether exactly one of Session, Issue, or All is set. It
// is EXPORTED because package main (Task 4's `sessions kill`/`resume`
// commands) calls it before opening any state, so a mistyped command fails
// before it touches the database.
func (s Selector) Validate() error {
	if s.Issue < 0 {
		return fmt.Errorf("--issue must be a positive issue number, got %d", s.Issue)
	}
	n := 0
	if s.Session != "" {
		n++
	}
	if s.Issue != 0 {
		n++
	}
	if s.All {
		n++
	}
	switch n {
	case 0:
		return errors.New("exactly one of --session, --issue, or --all is required")
	case 1:
		return nil
	default:
		return errors.New("--session, --issue, and --all are mutually exclusive")
	}
}

// Describe names what a selector will act on, for the --all confirmation
// prompt.
func (s Selector) Describe() string {
	switch {
	case s.Session != "":
		return fmt.Sprintf("session %s", s.Session)
	case s.Issue != 0:
		if s.Loop != "" {
			return fmt.Sprintf("issue #%d in loop %q", s.Issue, s.Loop)
		}
		return fmt.Sprintf("issue #%d", s.Issue)
	case s.All:
		scope := "every matching target on this machine"
		switch {
		case s.Project != "" && s.Loop != "":
			scope = fmt.Sprintf("every matching target in project %q, loop %q", s.Project, s.Loop)
		case s.Project != "":
			scope = fmt.Sprintf("every matching target in project %q", s.Project)
		case s.Loop != "":
			scope = fmt.Sprintf("every matching target in loop %q, across every project", s.Loop)
		}
		return scope
	}
	return "nothing"
}

// Target is the IDENTITY of one thing a kill or resume acts on. It carries no
// store.Dispatch: a loopcmd.Session has no repo, no pid, and no dispatch id,
// so resolve cannot build one. Kill and Resume open each loop anyway to take
// its lock, and THAT is where cfg.Repo and a scoped Store exist -- so
// dispatch rows are bound there, not here.
type Target struct {
	ProjectID string
	// Project is the project's display name, for the report.
	Project string
	// Dir is the project's .agent-utils directory. It becomes ProjectRef.Dir,
	// which drives config.ResolveWorkDirs and migrate.Discover -- empty, it
	// resolves different worktree paths and makes MigrationPolicy a hard
	// error. It must never be left unset.
	Dir  string
	Loop string
	// Issue is the issue number this target names. It is always known, even
	// for a session selector, because a session belongs to exactly one issue.
	Issue int
	// Session is set when the target was resolved from --session, or from a
	// running dispatch under --all. It is empty for a bare --issue selector.
	Session    string
	ConfigPath string
}

// describeLine renders one target for a result line.
func (t Target) describeLine() string {
	if t.Session != "" {
		return fmt.Sprintf("session %s (issue #%d, loop %s, project %s)",
			t.Session, t.Issue, t.Loop, t.Project)
	}
	return fmt.Sprintf("issue #%d (loop %s, project %s)", t.Issue, t.Loop, t.Project)
}

// work binds a Target to the loop it belongs to and, when one exists, the
// running dispatch row it names. Repo and Dispatch are filled in AFTER Open,
// inside Kill/Resume, because Target alone cannot carry them.
type work struct {
	Target   Target
	Repo     string
	Dispatch store.Dispatch
}

// Action is the outcome recorded for one target.
type Action string

// Every Action below is reachable: see kill_test.go.
const (
	// ActionSignalled means SIGTERM was sent and the runner exited within the
	// timeout.
	ActionSignalled Action = "signalled"
	// ActionAlreadyGone means no signal was sent because the runner could not
	// be verified: it was already gone, or the pid was never this dispatch's
	// runner in the first place.
	ActionAlreadyGone Action = "already gone"
	// ActionForced means --force killed the agent's process group and then
	// the runner.
	ActionForced Action = "forced"
	// ActionStillAlive means the runner outlived --timeout. The issue is
	// already stopped, so this is safe, but the agent is still running.
	ActionStillAlive Action = "still alive"
	// ActionResumed means a stopped issue's flags were cleared.
	ActionResumed Action = "resumed"
	// ActionRefused means Resume declined to act because the runner still
	// verifies as live.
	ActionRefused Action = "refused"
	// ActionNotStopped means Resume found this target not actually stopped --
	// spec section 8's "resume finds no stopped issue" case, reported as a
	// no-op rather than silently touching retry state that was never the
	// stopped flag's to begin with.
	ActionNotStopped Action = "not stopped"
	// ActionCouldNotVerify means the runner's identity could not be confirmed
	// at all (a `ps` failure, not a mismatch), so no signal was sent and the
	// row was left exactly as it was: distinct from "already gone", which
	// asserts the runner IS gone.
	ActionCouldNotVerify Action = "could not verify"
	// ActionFailed means an operation that was attempted did not succeed --
	// a signal EPERM'd, a write failed -- distinct from "already gone", which
	// means nothing was attempted at all.
	ActionFailed Action = "failed"
)

// Result is the outcome for one target.
type Result struct {
	Target Target
	Action Action
	Err    error
}

// KillOptions configures Kill.
type KillOptions struct {
	Selector Selector
	Force    bool
	// Timeout is how long to wait for the runner to exit after SIGTERM. Zero
	// means DefaultKillTimeout.
	Timeout time.Duration
}

// killer performs the ordered kill procedure (spec section 4.2 and 5.3) as a
// struct of function fields, so the procedure is testable without a real
// process or a real database.
type killer struct {
	// markStopped sets stopped/stopped_reason on the issue this work item
	// names. It is not called for a KindTend dispatch: a tend run holds no
	// issue state at all (runner.go:311).
	markStopped func(w work) error
	// verify confirms pid is CONFIRMED to be dispatchID's runner.
	verify func(pid int, dispatchID int64) error
	// signal sends sig to pid, after its own verification.
	signal func(pid int, dispatchID int64, sig syscall.Signal) error
	// waitGone polls until pid is no longer verifiable as the runner, up to
	// timeout, and reports whether it went away in time.
	waitGone func(pid int, dispatchID int64, timeout time.Duration) bool
	// reread re-reads the dispatch row, so the caller can tell whether the
	// runner's own handler already recorded an outcome.
	reread func(dispatchID int64) (store.Dispatch, error)
	// finish records the outcome store.FinishDispatch would record. It is
	// NOT runner.Finish: see the comment at its call sites for why.
	finish func(dispatchID int64, res store.DispatchResult) error
	// killAgent sends SIGKILL to the process group led by the agent's pid.
	// The caller must have verified the RUNNER first: agent_pid is never
	// cleared, so an unverified runner's row can carry a stale one.
	killAgent func(agentPID int) error
	// killRunner sends SIGKILL to the runner itself.
	killRunner func(pid int, dispatchID int64) error
}

// killedResult is the DispatchResult recorded for a killed dispatch. The
// design adds no new dispatch status: a new one would have to be understood
// by every existing query, the retry rules, and both renderers, and the
// stopped flag already carries the meaning that matters to the loop.
var killedResult = store.DispatchResult{
	Status: store.StatusFailed, ExitCode: -1, APIError: killedByOperatorReason,
}

// one runs the kill procedure for one target that has a live dispatch row,
// in the order spec section 4.2 specifies.
func (k killer) one(w work, opts KillOptions) Result {
	res := Result{Target: w.Target}

	// A tend dispatch holds no issue state, so there is no flag to set.
	if w.Dispatch.Kind != store.KindTend {
		// This write happens BEFORE any signal. A tick that starts in the
		// window between the agent dying and the flag landing would see the
		// trigger label and no live dispatch, and start a second agent. A
		// failed write here means no signal is sent at all: a signal without
		// the flag first is the exact race this order exists to close.
		if err := k.markStopped(w); err != nil {
			res.Action = ActionFailed
			res.Err = fmt.Errorf("mark issue #%d stopped: %w", w.Target.Issue, err)
			return res
		}
	}

	verifyErr := k.verify(w.Dispatch.PID, w.Dispatch.RunnerID())

	if opts.Force {
		return k.forceOne(w, res, verifyErr)
	}
	return k.gracefulOne(w, res, verifyErr, opts.Timeout)
}

func (k killer) gracefulOne(w work, res Result, verifyErr error, timeout time.Duration) Result {
	if verifyErr != nil {
		if couldNotVerifyResult, ok := k.couldNotVerify(w, res, verifyErr); ok {
			return couldNotVerifyResult
		}
		// Confirmed not this dispatch's live runner (or the pid is not even
		// positive): nothing to signal. Record the outcome ourselves.
		return k.recordIfNeeded(w, res, ActionAlreadyGone)
	}

	if err := k.signal(w.Dispatch.PID, w.Dispatch.RunnerID(), syscall.SIGTERM); err != nil {
		if couldNotVerifyResult, ok := k.couldNotVerify(w, res, err); ok {
			return couldNotVerifyResult
		}
		if errors.Is(err, proc.ErrNotRunner) {
			return k.recordIfNeeded(w, res, ActionAlreadyGone)
		}
		// A real signal failure (EPERM, for example) is reported as a
		// failure, not as success and not as "already gone": the issue stays
		// stopped, so the loop dispatches nothing, but the agent is still
		// alive, and the report must say so plainly.
		res.Action = ActionFailed
		res.Err = fmt.Errorf("signal runner for dispatch %d: %w", w.Dispatch.ID, err)
		return res
	}

	if timeout <= 0 {
		timeout = DefaultKillTimeout
	}
	if !k.waitGone(w.Dispatch.PID, w.Dispatch.RunnerID(), timeout) {
		res.Action = ActionStillAlive
		res.Err = fmt.Errorf(
			"dispatch %d (issue #%d) did not exit within %s; the issue stays stopped, retry with --force",
			w.Dispatch.ID, w.Target.Issue, timeout)
		return res
	}

	return k.recordIfNeeded(w, res, ActionSignalled)
}

// couldNotVerify reports whether err is proc.ErrVerifyFailed -- ps itself
// failed, rather than confirming the pid is not this dispatch's runner -- and
// if so returns the Result to report for it. This is NOT evidence the runner
// is gone (that is ErrNotRunner), so unlike ActionAlreadyGone it must not call
// recordIfNeeded: failing closed should suppress the SIGNAL, not also assert
// death and retire a `running` row the runner may still be about to finish
// itself.
func (k killer) couldNotVerify(w work, res Result, err error) (Result, bool) {
	if !errors.Is(err, proc.ErrVerifyFailed) {
		return Result{}, false
	}
	res.Action = ActionCouldNotVerify
	res.Err = fmt.Errorf(
		"could not verify the runner for dispatch %d (issue #%d): %w; the issue stays stopped, but the row was left alone",
		w.Dispatch.ID, w.Target.Issue, err)
	return res, true
}

func (k killer) forceOne(w work, res Result, verifyErr error) Result {
	if verifyErr != nil {
		if couldNotVerifyResult, ok := k.couldNotVerify(w, res, verifyErr); ok {
			return couldNotVerifyResult
		}
		// --force must NOT group-kill when the runner is unverified.
		// agent_pid is written once and never cleared, so a dead-runner row
		// carries a STALE pid, and after a reboot that number leads an
		// unrelated process group -- machine-wide under
		// `--all --yes --force`. A live, verified runner is the only
		// evidence agent_pid is current.
		return k.recordIfNeeded(w, res, ActionAlreadyGone)
	}

	// Agent group first, then runner, then record. The reverse order leaves
	// the agent alive in a worktree the loop believes is free.
	if w.Dispatch.AgentPID > 0 {
		if err := k.killAgent(w.Dispatch.AgentPID); err != nil {
			res.Action = ActionFailed
			res.Err = fmt.Errorf("kill agent process group %d: %w", w.Dispatch.AgentPID, err)
			return res
		}
	}
	if err := k.killRunner(w.Dispatch.PID, w.Dispatch.RunnerID()); err != nil {
		res.Action = ActionFailed
		res.Err = fmt.Errorf("kill runner for dispatch %d: %w", w.Dispatch.ID, err)
		return res
	}

	// No process survives to write the outcome, so this always writes it --
	// unlike the graceful path, there is no race with a runner recording its
	// own death to guard against.
	if err := k.finish(w.Dispatch.ID, killedResult); err != nil {
		res.Action = ActionFailed
		res.Err = err
		return res
	}
	res.Action = ActionForced
	return res
}

// recordIfNeeded re-reads the dispatch row and writes the killed outcome only
// if it is still marked running. The runner's own signal handler may have
// already recorded an outcome on the graceful path (SIGTERM cancelled its
// context, and its `finish` ran) -- writing a second one would be a stray
// write on top of whatever the runner itself decided.
//
// It uses store.FinishDispatch, NOT runner.Finish. runner.Finish also calls
// MarkNeedsRetry, and tick.go warns against skipping that call in the
// ordinary failure path -- but here arming a retry is exactly wrong, because
// the issue is stopped and a retry must not race a later `sessions resume`.
func (k killer) recordIfNeeded(w work, res Result, action Action) Result {
	d, err := k.reread(w.Dispatch.ID)
	if err != nil {
		res.Action = ActionFailed
		res.Err = fmt.Errorf("re-read dispatch %d: %w", w.Dispatch.ID, err)
		return res
	}
	if d.Status == store.StatusRunning {
		if err := k.finish(w.Dispatch.ID, killedResult); err != nil {
			res.Action = ActionFailed
			res.Err = err
			return res
		}
	}
	res.Action = action
	return res
}

// narrowByLoop resolves an --issue selector's candidates (one per loop in the
// project) down to exactly one, using --loop to break a tie.
func narrowByLoop(candidates []Target, loopFilter string) ([]Target, error) {
	if loopFilter != "" {
		for _, c := range candidates {
			if c.Loop == loopFilter {
				return []Target{c}, nil
			}
		}
		return nil, fmt.Errorf("no loop %q found for issue #%d", loopFilter, issueOf(candidates))
	}
	switch len(candidates) {
	case 0:
		return nil, errors.New("no loop configurations found")
	case 1:
		return candidates, nil
	default:
		loops := make([]string, len(candidates))
		for i, c := range candidates {
			loops[i] = c.Loop
		}
		return nil, fmt.Errorf(
			"issue #%d matches more than one loop (%s); narrow with --loop",
			candidates[0].Issue, strings.Join(loops, ", "))
	}
}

func issueOf(candidates []Target) int {
	if len(candidates) == 0 {
		return 0
	}
	return candidates[0].Issue
}

// resolve turns a Selector into a set of identity-only Targets. forResume
// reports which machine-wide table --all reads: running dispatches for
// Kill, stopped issues for Resume.
func resolve(sel Selector, forResume bool) ([]Target, error) {
	switch {
	case sel.Session != "":
		return resolveBySession(sel)
	case sel.Issue != 0:
		return resolveByIssue(sel, forResume)
	case sel.All:
		return resolveAll(sel, forResume)
	}
	return nil, errors.New("no selector")
}

func resolveBySession(sel Selector) ([]Target, error) {
	sessions, err := AllSessions(SessionFilter{Project: sel.Project, Loop: sel.Loop})
	if err != nil {
		return nil, err
	}
	entries, err := registry.List()
	if err != nil {
		return nil, err
	}
	for _, s := range sessions {
		if s.ID != sel.Session {
			continue
		}
		for _, p := range entries {
			if p.ID != s.ProjectID {
				continue
			}
			path, err := config.Resolve(p.AgentUtilsDir, s.Loop)
			if err != nil {
				return nil, fmt.Errorf("resolve loop %q for session %s: %w", s.Loop, s.ID, err)
			}
			return []Target{{
				ProjectID: p.ID, Project: p.Name, Dir: p.AgentUtilsDir,
				Loop: s.Loop, Issue: s.Issue, Session: s.ID, ConfigPath: path,
			}}, nil
		}
		return nil, fmt.Errorf("session %s belongs to a project no longer in the registry", s.ID)
	}
	return nil, fmt.Errorf("no session %q found", sel.Session)
}

// resolveByIssue narrows an --issue selector to the loop(s) that actually
// name this issue, reading real rows rather than enumerating every loop
// CONFIG in the project. An issue number is unique within a loop, not within
// a project, so a project with several loop configs is not automatically
// ambiguous for a given issue -- only a project where the issue genuinely
// appears in more than one loop's rows is. Enumerating configs made every
// multi-loop project ambiguous regardless of where the issue actually lived,
// contradicting spec section 4.1.1 and README.md's promise that --loop is
// needed only when the number matches more than one loop.
func resolveByIssue(sel Selector, forResume bool) ([]Target, error) {
	p, err := resolveRegistryProject(sel.Project)
	if err != nil {
		return nil, err
	}

	loops, err := loopsNamingIssue(p.ID, sel.Issue, forResume)
	if err != nil {
		return nil, err
	}
	if len(loops) == 0 {
		// No row names this issue at all: a runner that already crashed
		// leaves no running dispatch row (kill's B3 fix still needs to mark
		// the issue stopped), or a resume is being asked about an issue that
		// merely sits in retry backoff with no stopped row (spec section 8's
		// "resume finds no stopped issue" no-op). Fall back to every
		// configured loop, exactly as before this fix, so a single-loop
		// project still resolves and a genuinely ambiguous one still asks
		// for --loop.
		return resolveByIssueFromConfigs(p, sel)
	}

	entries, err := config.List(p.AgentUtilsDir)
	if err != nil {
		return nil, err
	}
	pathByLoop := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Err == nil {
			pathByLoop[e.Name] = e.Path
		}
	}

	var candidates []Target
	for _, loop := range loops {
		candidates = append(candidates, Target{
			ProjectID: p.ID, Project: p.Name, Dir: p.AgentUtilsDir,
			Loop: loop, Issue: sel.Issue, ConfigPath: pathByLoop[loop],
		})
	}
	return narrowByLoop(candidates, sel.Loop)
}

// loopsNamingIssue returns the distinct loop names, in this project, that
// have a row for this issue number: running dispatches for Kill, stopped
// issues for Resume -- the same machine-wide tables resolveAll already reads
// for --all.
func loopsNamingIssue(projectID string, issue int, forResume bool) ([]string, error) {
	db, err := openCanonical()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	seen := map[string]bool{}
	var loops []string
	add := func(loop string) {
		if !seen[loop] {
			seen[loop] = true
			loops = append(loops, loop)
		}
	}

	if forResume {
		stopped, err := db.StoppedIssues()
		if err != nil {
			return nil, err
		}
		for _, si := range stopped {
			if si.ProjectID == projectID && si.Number == issue {
				add(si.Loop)
			}
		}
		return loops, nil
	}

	running, err := db.RunningDispatches()
	if err != nil {
		return nil, err
	}
	for _, d := range running {
		if d.ProjectID == projectID && d.Number == issue {
			add(d.Loop)
		}
	}
	return loops, nil
}

// resolveByIssueFromConfigs is the pre-B9 fallback: it enumerates every loop
// configuration in the project, for the case where no row names this issue
// at all and there is therefore nothing more specific to narrow by.
func resolveByIssueFromConfigs(p registry.Project, sel Selector) ([]Target, error) {
	entries, err := config.List(p.AgentUtilsDir)
	if err != nil {
		return nil, err
	}
	var candidates []Target
	for _, e := range entries {
		if e.Err != nil {
			continue
		}
		candidates = append(candidates, Target{
			ProjectID: p.ID, Project: p.Name, Dir: p.AgentUtilsDir,
			Loop: e.Name, Issue: sel.Issue, ConfigPath: e.Path,
		})
	}
	return narrowByLoop(candidates, sel.Loop)
}

func resolveRegistryProject(sel string) (registry.Project, error) {
	if sel != "" {
		return registry.Find(sel)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return registry.Project{}, err
	}
	dir, err := config.FindDir(cwd)
	if err != nil {
		return registry.Project{}, err
	}
	pc, err := project.Load(dir)
	if err != nil {
		return registry.Project{}, err
	}
	return registry.Project{ID: pc.ID, Name: pc.Name, AgentUtilsDir: dir, Root: filepath.Dir(dir)}, nil
}

func resolveAll(sel Selector, forResume bool) ([]Target, error) {
	entries, err := registry.List()
	if err != nil {
		return nil, err
	}
	byID := make(map[string]registry.Project, len(entries))
	for _, p := range entries {
		byID[p.ID] = p
	}
	matchesProject := func(projectID string) bool {
		if sel.Project == "" {
			return true
		}
		p, ok := byID[projectID]
		return ok && (p.ID == sel.Project || p.Name == sel.Project)
	}

	db, err := openCanonical()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var targets []Target
	if forResume {
		stopped, err := db.StoppedIssues()
		if err != nil {
			return nil, err
		}
		for _, si := range stopped {
			if !matchesProject(si.ProjectID) || (sel.Loop != "" && si.Loop != sel.Loop) {
				continue
			}
			targets = append(targets, targetFor(byID, si.ProjectID, si.Loop, si.Number, ""))
		}
		return targets, nil
	}

	running, err := db.RunningDispatches()
	if err != nil {
		return nil, err
	}
	for _, d := range running {
		if !matchesProject(d.ProjectID) || (sel.Loop != "" && d.Loop != sel.Loop) {
			continue
		}
		targets = append(targets, targetFor(byID, d.ProjectID, d.Loop, d.Number, d.SessionID))
	}
	return targets, nil
}

// targetFor builds a Target for --all. A project the registry cannot resolve,
// or a loop config that config.Resolve cannot find, is NOT a fatal error: the
// target survives with an empty ConfigPath, and Kill/Resume turn that into a
// per-target failed Result instead of aborting every other target.
func targetFor(byID map[string]registry.Project, projectID, loop string, issue int, session string) Target {
	p, ok := byID[projectID]
	if !ok {
		return Target{ProjectID: projectID, Loop: loop, Issue: issue, Session: session}
	}
	path, err := config.Resolve(p.AgentUtilsDir, loop)
	if err != nil {
		path = ""
	}
	return Target{
		ProjectID: p.ID, Project: p.Name, Dir: p.AgentUtilsDir,
		Loop: loop, Issue: issue, Session: session, ConfigPath: path,
	}
}

// runByLoop groups targets by their loop's configuration path, opens and
// locks each loop exactly once, and runs fn for every target of that loop
// while the lock is held. A loop lock already held by a tick fails every
// target of THAT loop, with the same wording `loop reset` uses, and does not
// abandon the other loops: each group is independent.
func runByLoop(targets []Target, fn func(cfg *config.Config, st *store.Store, t Target) Result) []Result {
	type group struct {
		path    string
		targets []Target
	}
	var groups []group
	index := map[string]int{}
	for _, t := range targets {
		if i, ok := index[t.ConfigPath]; ok {
			groups[i].targets = append(groups[i].targets, t)
			continue
		}
		index[t.ConfigPath] = len(groups)
		groups = append(groups, group{path: t.ConfigPath, targets: []Target{t}})
	}

	var results []Result
	for _, g := range groups {
		if g.path == "" {
			for _, t := range g.targets {
				results = append(results, Result{Target: t, Action: ActionFailed,
					Err: fmt.Errorf("could not resolve the loop configuration for issue #%d", t.Issue)})
			}
			continue
		}

		first := g.targets[0]
		cfg, deps, cleanup, err := Open(
			ProjectRef{ID: first.ProjectID, Name: first.Project, Dir: first.Dir},
			g.path,
			Options{MigrationPolicy: WarnOnUnimported},
		)
		if err != nil {
			for _, t := range g.targets {
				results = append(results, Result{Target: t, Action: ActionFailed, Err: err})
			}
			continue
		}

		l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
		if errors.Is(err, lock.ErrHeld) {
			lockErr := fmt.Errorf("a tick is running for loop %q; try again", cfg.Name)
			for _, t := range g.targets {
				results = append(results, Result{Target: t, Action: ActionFailed, Err: lockErr})
			}
			cleanup()
			continue
		}
		if err != nil {
			for _, t := range g.targets {
				results = append(results, Result{Target: t, Action: ActionFailed, Err: err})
			}
			cleanup()
			continue
		}

		for _, t := range g.targets {
			results = append(results, fn(cfg, deps.Store, t))
		}

		l.Release()
		cleanup()
	}
	return results
}

// matchDispatch finds the running dispatch a target names: by session
// identifier when the target carries one, otherwise by issue number.
func matchDispatch(running []store.Dispatch, t Target) (store.Dispatch, bool) {
	for _, d := range running {
		if t.Session != "" {
			if d.SessionID == t.Session {
				return d, true
			}
			continue
		}
		if d.Number == t.Issue {
			return d, true
		}
	}
	return store.Dispatch{}, false
}

// newWaitGone builds a killer's waitGone hook around a liveness check. It is
// separated from productionKiller so a test can inject a fake liveness
// function and assert the polling semantics without exec'ing a real process,
// and so production always wires proc.IsAlive here -- never proc.VerifyRunner,
// whose opposite (fail-closed) bias belongs to the pre-signal identity gate,
// not to a liveness poll. See productionKiller's comment for why the two must
// not be swapped.
func newWaitGone(isAlive func(pid int, dispatchID int64) bool) func(pid int, dispatchID int64, timeout time.Duration) bool {
	return func(pid int, dispatchID int64, timeout time.Duration) bool {
		deadline := time.Now().Add(timeout)
		for {
			if !isAlive(pid, dispatchID) {
				return true
			}
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// productionKiller wires killer's function fields to the real internal/proc
// signal helpers and internal/store methods.
func productionKiller(cfg *config.Config, st *store.Store) killer {
	return killer{
		markStopped: func(w work) error {
			return st.MarkStopped(cfg.Name, w.Repo, w.Target.Issue, killedByOperatorReason, time.Now())
		},
		verify: proc.VerifyRunner,
		signal: proc.Signal,
		// IsAlive, not VerifyRunner: the two want opposite biases, and this is
		// a liveness poll, not the pre-signal identity gate. VerifyRunner fails
		// CLOSED on any ps error, by design (signal.go), because a caller about
		// to send a signal must not act on an inconclusive answer. Reusing that
		// bias HERE would invert it: a transient ps failure (EAGAIN under
		// load) would make this report "gone", and recordIfNeeded would then
		// write "killed by operator" onto a dispatch whose runner is still
		// alive -- the real runner's own finish would hit ErrDispatchNotRunning
		// and never record its true outcome, and the operator would be told
		// "signalled" while the agent still writes to the worktree. IsAlive
		// fails OPEN on the same ps error (proc.go:36-46), which is the correct
		// bias for a poll deciding whether to keep waiting.
		waitGone: newWaitGone(proc.IsAlive),
		reread:   st.GetDispatch,
		finish:   st.FinishDispatch,
		killAgent: func(pid int) error {
			return proc.SignalGroup(pid, syscall.SIGKILL)
		},
		killRunner: func(pid int, dispatchID int64) error {
			return proc.Signal(pid, dispatchID, syscall.SIGKILL)
		},
	}
}

// Kill signals the runner (and, with --force, the agent) for every target the
// selector resolves to.
func Kill(opts KillOptions) ([]Result, error) {
	if err := opts.Selector.Validate(); err != nil {
		return nil, err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultKillTimeout
	}
	targets, err := resolve(opts.Selector, false)
	if err != nil {
		return nil, err
	}

	results := runByLoop(targets, func(cfg *config.Config, st *store.Store, t Target) Result {
		running, err := st.RunningDispatches(cfg.Name, cfg.Repo)
		if err != nil {
			return Result{Target: t, Action: ActionFailed, Err: err}
		}
		d, ok := matchDispatch(running, t)
		if !ok {
			// Killing nothing is not an error, but the trigger label may still
			// be set on GitHub: set the stopped flag anyway, so a tick racing
			// this moment (a runner that just crashed) reads it and does not
			// start a fresh agent -- exactly the race the write-before-signal
			// ordering in killer.one exists to close for the ordinary path.
			// killer.one's own tend exception does not apply here: this
			// target names an ISSUE the selector was asked to stop, and there
			// is no dispatch row at all to say it was a tend run instead.
			if err := st.MarkStopped(cfg.Name, cfg.Repo, t.Issue, killedByOperatorReason, time.Now()); err != nil {
				return Result{Target: t, Action: ActionFailed,
					Err: fmt.Errorf("mark issue #%d stopped: %w", t.Issue, err)}
			}
			// Killing nothing is not an error: report it and move on.
			return Result{Target: t, Action: ActionAlreadyGone}
		}
		return productionKiller(cfg, st).one(work{Target: t, Repo: cfg.Repo, Dispatch: d}, opts)
	})
	return results, nil
}

// Resume clears the stopped state (and the retry flags a killed dispatch
// left behind) for every target the selector resolves to. It refuses a
// target whose dispatch still verifies as a live runner: the runner holds no
// loop lock, and its own `finish` calls MarkNeedsRetry, so a resume issued
// while the runner is still dying would have its clear written straight back
// -- leaving the issue un-stopped AND flagged for retry.
func Resume(sel Selector) ([]Result, error) {
	if err := sel.Validate(); err != nil {
		return nil, err
	}
	targets, err := resolve(sel, true)
	if err != nil {
		return nil, err
	}

	results := runByLoop(targets, resumeTarget)
	return results, nil
}

// resumeTarget is Resume's per-target action, extracted so a test can drive
// it directly through runByLoop against a real store fixture (the shape
// TestRunByLoopAHeldLockFailsOnlyThatLoopsTargets already uses) instead of
// re-implementing the refusal rule in the test file.
func resumeTarget(cfg *config.Config, st *store.Store, t Target) Result {
	running, err := st.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		return Result{Target: t, Action: ActionFailed, Err: err}
	}
	if d, ok := matchDispatch(running, t); ok {
		if verifyErr := proc.VerifyRunner(d.PID, d.RunnerID()); verifyErr == nil {
			return Result{Target: t, Action: ActionRefused, Err: fmt.Errorf(
				"dispatch %d for issue #%d has a live runner; wait for it to exit, or use `sessions kill --force`",
				d.ID, t.Issue)}
		}
	}
	cleared, err := st.ClearStopped(cfg.Name, cfg.Repo, t.Issue, time.Now())
	if err != nil {
		return Result{Target: t, Action: ActionFailed, Err: err}
	}
	if !cleared {
		// Not actually stopped: --issue/--session resolve from config or the
		// registry, not from the stopped table, so this target can legitimately
		// name an issue that merely sits in retry backoff. Report it as a
		// no-op instead of having silently cleared needs_retry/retry_after.
		return Result{Target: t, Action: ActionNotStopped}
	}
	return Result{Target: t, Action: ActionResumed}
}

// RenderResults formats one line per result for a terminal. verb is "kill" or
// "resume".
func RenderResults(verb string, rs []Result) string {
	var b strings.Builder
	if len(rs) == 0 {
		fmt.Fprint(&b, "nothing matched\n")
		return b.String()
	}
	for _, r := range rs {
		switch r.Action {
		case ActionFailed:
			fmt.Fprintf(&b, "%s %s: failed: %v\n", verb, r.Target.describeLine(), r.Err)
		case ActionStillAlive:
			fmt.Fprintf(&b, "%s %s: still alive after the timeout; retry with --force\n",
				verb, r.Target.describeLine())
		case ActionCouldNotVerify:
			fmt.Fprintf(&b, "%s %s: %v\n", verb, r.Target.describeLine(), r.Err)
		default:
			fmt.Fprintf(&b, "%s %s: %s\n", verb, r.Target.describeLine(), r.Action)
		}
	}
	return b.String()
}
