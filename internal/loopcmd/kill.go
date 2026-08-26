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
func (k killer) one(w work, opts KillOptions) (Result, error) {
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
			return res, res.Err
		}
	}

	verifyErr := k.verify(w.Dispatch.PID, w.Dispatch.RunnerID())

	if opts.Force {
		return k.forceOne(w, res, verifyErr)
	}
	return k.gracefulOne(w, res, verifyErr, opts.Timeout)
}

func (k killer) gracefulOne(w work, res Result, verifyErr error, timeout time.Duration) (Result, error) {
	if verifyErr != nil {
		// Not this dispatch's live runner (or the pid is not even positive):
		// nothing to signal. Record the outcome ourselves.
		return k.recordIfNeeded(w, res, ActionAlreadyGone)
	}

	if err := k.signal(w.Dispatch.PID, w.Dispatch.RunnerID(), syscall.SIGTERM); err != nil {
		if errors.Is(err, proc.ErrNotRunner) {
			return k.recordIfNeeded(w, res, ActionAlreadyGone)
		}
		// A real signal failure (EPERM, for example) is reported as a
		// failure, not as success and not as "already gone": the issue stays
		// stopped, so the loop dispatches nothing, but the agent is still
		// alive, and the report must say so plainly.
		res.Action = ActionFailed
		res.Err = fmt.Errorf("signal runner for dispatch %d: %w", w.Dispatch.ID, err)
		return res, res.Err
	}

	if timeout <= 0 {
		timeout = DefaultKillTimeout
	}
	if !k.waitGone(w.Dispatch.PID, w.Dispatch.RunnerID(), timeout) {
		res.Action = ActionStillAlive
		res.Err = fmt.Errorf(
			"dispatch %d (issue #%d) did not exit within %s; the issue stays stopped, retry with --force",
			w.Dispatch.ID, w.Target.Issue, timeout)
		return res, nil
	}

	return k.recordIfNeeded(w, res, ActionSignalled)
}

func (k killer) forceOne(w work, res Result, verifyErr error) (Result, error) {
	if verifyErr != nil {
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
			return res, res.Err
		}
	}
	if err := k.killRunner(w.Dispatch.PID, w.Dispatch.RunnerID()); err != nil {
		res.Action = ActionFailed
		res.Err = fmt.Errorf("kill runner for dispatch %d: %w", w.Dispatch.ID, err)
		return res, res.Err
	}

	// No process survives to write the outcome, so this always writes it --
	// unlike the graceful path, there is no race with a runner recording its
	// own death to guard against.
	if err := k.finish(w.Dispatch.ID, killedResult); err != nil {
		res.Action = ActionFailed
		res.Err = err
		return res, res.Err
	}
	res.Action = ActionForced
	return res, nil
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
func (k killer) recordIfNeeded(w work, res Result, action Action) (Result, error) {
	d, err := k.reread(w.Dispatch.ID)
	if err != nil {
		res.Action = ActionFailed
		res.Err = fmt.Errorf("re-read dispatch %d: %w", w.Dispatch.ID, err)
		return res, res.Err
	}
	if d.Status == store.StatusRunning {
		if err := k.finish(w.Dispatch.ID, killedResult); err != nil {
			res.Action = ActionFailed
			res.Err = err
			return res, res.Err
		}
	}
	res.Action = action
	return res, nil
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
		return resolveByIssue(sel)
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

func resolveByIssue(sel Selector) ([]Target, error) {
	p, err := resolveRegistryProject(sel.Project)
	if err != nil {
		return nil, err
	}
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

// productionKiller wires killer's function fields to the real internal/proc
// signal helpers and internal/store methods.
func productionKiller(cfg *config.Config, st *store.Store) killer {
	return killer{
		markStopped: func(w work) error {
			return st.MarkStopped(cfg.Name, w.Repo, w.Target.Issue, killedByOperatorReason, time.Now())
		},
		verify: proc.VerifyRunner,
		signal: proc.Signal,
		waitGone: func(pid int, dispatchID int64, timeout time.Duration) bool {
			deadline := time.Now().Add(timeout)
			for {
				if err := proc.VerifyRunner(pid, dispatchID); err != nil {
					return true
				}
				if time.Now().After(deadline) {
					return false
				}
				time.Sleep(200 * time.Millisecond)
			}
		},
		reread: st.GetDispatch,
		finish: st.FinishDispatch,
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
			// Killing nothing is not an error: report it and move on.
			return Result{Target: t, Action: ActionAlreadyGone}
		}
		res, _ := productionKiller(cfg, st).one(work{Target: t, Repo: cfg.Repo, Dispatch: d}, opts)
		return res
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

	results := runByLoop(targets, func(cfg *config.Config, st *store.Store, t Target) Result {
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
		if err := st.ClearStopped(cfg.Name, cfg.Repo, t.Issue, time.Now()); err != nil {
			return Result{Target: t, Action: ActionFailed, Err: err}
		}
		return Result{Target: t, Action: ActionResumed}
	})
	return results, nil
}

// RenderResults formats one line per result for a terminal. verb is "kill" or
// "resume".
func RenderResults(verb string, rs []Result) string {
	var b strings.Builder
	if len(rs) == 0 {
		fmt.Fprintf(&b, "nothing matched\n")
		return b.String()
	}
	for _, r := range rs {
		switch r.Action {
		case ActionFailed:
			fmt.Fprintf(&b, "%s %s: failed: %v\n", verb, r.Target.describeLine(), r.Err)
		case ActionStillAlive:
			fmt.Fprintf(&b, "%s %s: still alive after the timeout; retry with --force\n",
				verb, r.Target.describeLine())
		default:
			fmt.Fprintf(&b, "%s %s: %s\n", verb, r.Target.describeLine(), r.Action)
		}
	}
	return b.String()
}
