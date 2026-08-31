package loopcmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/registry"
	"github.com/seanmcgary/agent-utils/internal/runner"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// Session is one claude conversation, aggregated over every dispatch that used
// it.
//
// A session is the unit that survives resumes: an issue keeps one session
// across a park and its answer, which is the whole reason the loop stores the
// identifier. Several dispatches therefore share one session.
type Session struct {
	ID string
	// ProjectID is the owning project, copied from the dispatch row. It is
	// half the grouping key: a session identifier is only unique within a
	// project.
	ProjectID string
	// Repo is the "owner/name" the session's issue lives in, copied from the
	// dispatch row. It is carried for the closed lookup: a closure is keyed by
	// repository and number, because two loops watching one repository see the
	// same closure.
	Repo string
	// Project is the project's display name. Only the machine-wide report
	// fills it in; the per-project report already names the project in its
	// header.
	Project string
	// Loop is the loop that owns the session.
	Loop string
	// Issue is the issue the session works on.
	Issue int
	// Title is the issue title as of the most recent dispatch.
	Title string
	// Dispatches is how many runs used this session.
	Dispatches int
	// Cost is the total across those runs.
	Cost float64
	// First and Last bound the session's activity.
	First time.Time
	Last  time.Time
	// LastStatus is the status of the most recent dispatch.
	LastStatus string
	// LastKind is the kind of the most recent dispatch.
	LastKind string
	// Model and Harness are the agent settings the most recent dispatch ran
	// under: the per-issue label override when the row carried one, else the
	// value configured for the loop. sessionsFrom fills in the override
	// alone, and applySettings resolves the rest -- the override columns are
	// empty for the ordinary case, which is most rows, and "no override" is
	// not an answer to which model ran. Either may still end up empty, when
	// the loop's configuration can no longer be read; the renderers print a
	// dash rather than inventing a default the run may not have used.
	Model   string
	Harness string
	// Live reports that the most recent dispatch's process is still running.
	Live bool
	// Orphaned reports a dispatch still marked running whose process is gone.
	Orphaned bool
	// Closed reports that the issue or pull request this session worked on is
	// closed. It is filled in by applyClosed, a separate pass for the reason
	// applyStopped is one.
	//
	// It is a fact about the ISSUE, so it marks EVERY session that ever worked
	// that issue, unlike Stopped, which marks only the newest. Hiding old work
	// on a finished issue is the whole point; leaving the first of five
	// sessions on a closed issue in the table would defeat it.
	Closed bool
	// Stopped reports that the issue this session belongs to is stopped --
	// by an operator's `sessions kill`, or by an invalid override label. It
	// is filled in by applyStopped, a separate pass over sessionsFrom's
	// output, because sessionsFrom groups dispatches and knows nothing about
	// the issues table a stopped flag lives in.
	Stopped bool
}

// Sessions returns every session in a project, newest activity first.
//
// loopFilter restricts the result to one loop when it is not empty.
//
// It reads the whole project in one query. Before the canonical database it
// opened one file per loop, which also meant a session in a loop whose
// configuration had been deleted could not be found at all.
func Sessions(p *Project, loopFilter string) ([]Session, error) {
	db, err := openCanonical()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	ds, err := db.DispatchesForProject(p.Config.ID)
	if err != nil {
		return nil, err
	}
	out := sessionsFrom(ds, loopFilter)

	// DB.StoppedIssues(), not the scoped Store.StoppedIssues: the latter
	// needs a repo, and this function is handed a *Project with no loop
	// filter to pick one repo from -- p.Config.ID alone is what scopes the
	// machine-wide read back down to this project.
	stopped, err := db.StoppedIssues()
	if err != nil {
		return nil, err
	}
	applyStopped(out, stoppedSet(stopped, p.Config.ID))

	closures, err := db.Closures()
	if err != nil {
		return nil, err
	}
	applyClosed(out, closedSet(closures, p.Config.ID))
	applySettings(out, map[string]string{p.Config.ID: p.Dir})

	// Stable, because sessionsFrom returns the rows in the query's id DESC
	// order. Two sessions can share a Last timestamp -- a resume dispatched
	// in the same second, or a legacy source imported with a coarse clock --
	// and the newest dispatch first is the tiebreak worth keeping.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out, nil
}

// SessionFilter narrows the machine-wide sessions report.
//
// Project is a registry selector -- a name, an id, or a path -- resolved
// through registry.Find, so it accepts whatever the operator already types for
// --project elsewhere.
//
// Running and Orphaned name the states to report rather than filtering
// independently: neither flag means every state, and both flags mean the union
// of the two, never the empty intersection.
type SessionFilter struct {
	Project  string
	Loop     string
	Running  bool
	Orphaned bool
	// All keeps the sessions of closed issues and pull requests in the report.
	// They are hidden by default: an issue that is closed is work that is
	// over, and a machine that has been running loops for months has far more
	// of that than of anything an operator is looking for.
	//
	// It is NOT one of the state flags above, and does not go through
	// keepState. Those name which states to report; this one only says whether
	// finished work is included, and it is applied by the renderer so that
	// FindSession, `sessions kill` and `sessions describe` keep resolving a
	// closed issue's session by name.
	All bool
}

// filtered reports that the operator narrowed the report.
//
// The renderer branches its empty-list text on it: nothing has run yet is a
// different message from nothing matched what you asked for, and only the
// filter knows which one is true. Keeping the rule on the type is what stops
// the command layer from restating filter semantics it would then have to keep
// in step with this file.
func (f SessionFilter) filtered() bool {
	return f.Project != "" || f.Loop != "" || f.Running || f.Orphaned
}

// closedKey identifies one issue for the closed lookup. It carries the project
// and the repository but NOT the loop, matching the closures table: two loops
// watching one repository see the same closure, and a key with a loop in it
// would need the same fact recorded once per loop.
type closedKey struct {
	ProjectID string
	Repo      string
	Number    int
}

// closedSet builds the lookup applyClosed uses, from DB.Closures(). projectID
// narrows it to one project, exactly as stoppedSet does, and an empty
// projectID keeps every project.
func closedSet(all []store.Closure, projectID string) map[closedKey]bool {
	out := make(map[closedKey]bool, len(all))
	for _, c := range all {
		if projectID != "" && c.ProjectID != projectID {
			continue
		}
		out[closedKey{ProjectID: c.ProjectID, Repo: c.Repo, Number: c.Number}] = true
	}
	return out
}

// applyClosed marks every session whose issue is closed.
//
// EVERY session, not just the newest -- the one place it deliberately differs
// from applyStopped. See Session.Closed.
//
// A session whose issue nothing has reported on stays unmarked, and therefore
// visible. The two writers of the closures table are the listener's close
// deliveries and its startup reconcile, so a machine that has never run the
// daemon marks nothing and its report is exactly what it was before.
func applyClosed(sessions []Session, closed map[closedKey]bool) {
	for i := range sessions {
		key := closedKey{
			ProjectID: sessions[i].ProjectID,
			Repo:      sessions[i].Repo,
			Number:    sessions[i].Issue,
		}
		sessions[i].Closed = closed[key]
	}
}

// visible splits sessions into the ones a report shows and a count of the ones
// it hid, so the footer can say how much it is not showing.
//
// all short-circuits rather than filtering to the same slice, because "hid
// nothing" and "was not asked to hide" print differently: with --all there is
// no hidden count to report at all.
func visible(sessions []Session, all bool) ([]Session, int) {
	if all {
		return sessions, 0
	}
	kept := make([]Session, 0, len(sessions))
	hidden := 0
	for _, s := range sessions {
		if s.Closed {
			hidden++
			continue
		}
		kept = append(kept, s)
	}
	return kept, hidden
}

// hiddenNote is the footer line that says how many closed sessions a report
// left out. It is empty when none were.
func hiddenNote(hidden int) string {
	if hidden == 0 {
		return ""
	}
	noun := "sessions"
	if hidden == 1 {
		noun = "session"
	}
	return fmt.Sprintf("\n%d closed %s hidden (--all to show)\n", hidden, noun)
}

// keepState reports whether a session survives the state flags.
//
// The rule is not inferable from the signature. The flags name the states to
// report, so neither flag means every state rather than none, and both flags
// mean the union: no session is at once live and orphaned, so treating the
// flags as independent filters would make --running --orphaned print nothing
// at all.
func keepState(s Session, running, orphaned bool) bool {
	switch {
	case running && orphaned:
		return s.Live || s.Orphaned
	case running:
		return s.Live
	case orphaned:
		return s.Orphaned
	default:
		return true
	}
}

// nameProjects fills in each session's display name from a map of project id
// to registered name.
//
// A session whose project the registry cannot name stays in the report with a
// marker, following the precedent RenderProjects sets: a project the operator
// can no longer name is something to see, not something to hide. The two ways
// a name can be missing are different states, and they read differently:
//
//   - No project id at all. These are the pre-project rows that the sweep
//     could not claim (see upgradeKeys, internal/store/store.go:360). No
//     scoped query returns them and no --project selector can ever reach them,
//     so they are marked (unclaimed).
//   - An id the registry has forgotten, or recorded before it had a name. The
//     first eight characters are enough to tell one such project's rows from
//     another's. They are NOT a selector: registry.Find compares an id with
//     ==, so a prefix resolves to nothing, and a forgotten project is not in
//     the registry to be found at all. A shorter id is used whole.
func nameProjects(sessions []Session, names map[string]string) {
	for i := range sessions {
		id := sessions[i].ProjectID
		switch name := names[id]; {
		case id == "":
			sessions[i].Project = "(unclaimed)"
		case name != "":
			sessions[i].Project = name
		case len(id) > 8:
			sessions[i].Project = id[:8]
		default:
			sessions[i].Project = id
		}
	}
}

// AllSessions returns every session on this machine, newest activity first.
//
// The registry is read before the database on purpose: an unknown or unusable
// --project must fail before the command opens the state database and runs the
// legacy sweep, so a mistyped selector migrates nothing.
//
// Scoping stays in SQL. A resolved project narrows the query itself; this
// function never reads every row and drops the unwanted ones, because that
// would make one condition here the only thing separating two projects.
//
// It reads local state only -- the registry and the one state database. It
// makes no GitHub call, so it is fast, works offline, and needs no token.
func AllSessions(f SessionFilter) ([]Session, error) {
	var projectID string
	if f.Project != "" {
		// Unwrapped: Find already names the selector and says whether it
		// matched nothing or matched too much.
		p, err := registry.Find(f.Project)
		if err != nil {
			return nil, err
		}
		// A registry entry may legitimately hold no id. Scoping the query to
		// an empty one would report no sessions for a project that has many,
		// so refuse instead and name the command that mints the identifier.
		if p.ID == "" {
			return nil, fmt.Errorf(
				"project %q has no identifier; run agent-utils project init in %s",
				f.Project, p.Root)
		}
		projectID = p.ID
	}

	entries, err := registry.List()
	if err != nil {
		return nil, err
	}
	names := make(map[string]string, len(entries))
	dirs := make(map[string]string, len(entries))
	for _, p := range entries {
		names[p.ID] = p.Name
		dirs[p.ID] = p.AgentUtilsDir
	}

	db, err := openCanonical()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	var ds []store.Dispatch
	if projectID != "" {
		ds, err = db.DispatchesForProject(projectID)
	} else {
		ds, err = db.Dispatches()
	}
	if err != nil {
		return nil, err
	}

	out := sessionsFrom(ds, f.Loop)
	kept := make([]Session, 0, len(out))
	for _, s := range out {
		if keepState(s, f.Running, f.Orphaned) {
			kept = append(kept, s)
		}
	}

	// DB.StoppedIssues() is the machine-wide read; projectID is "" unless
	// --project narrowed it, and stoppedSet treats "" as every project.
	stopped, err := db.StoppedIssues()
	if err != nil {
		return nil, err
	}
	applyStopped(kept, stoppedSet(stopped, projectID))

	// Machine-wide for the reason StoppedIssues is read machine-wide here:
	// projectID is "" unless --project narrowed it, and closedSet treats "" as
	// every project.
	closures, err := db.Closures()
	if err != nil {
		return nil, err
	}
	applyClosed(kept, closedSet(closures, projectID))
	applySettings(kept, dirs)

	nameProjects(kept, names)
	// Stable, for the reason Sessions is: sessionsFrom returns the rows in the
	// query's id DESC order, and two sessions can share a Last timestamp, so
	// the newest dispatch first is the tiebreak worth keeping.
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Last.After(kept[j].Last) })
	return kept, nil
}

// sessionKey identifies one session. It holds the project as well as the
// session identifier because nothing makes a session identifier unique across
// projects: a copied worktree or an imported legacy source can reproduce one,
// and the machine-wide read is the first that sees two projects' rows at once.
type sessionKey struct {
	ProjectID string
	SessionID string
}

// stoppedKey identifies one issue for the stopped lookup. It carries the
// project for the same reason sessionKey does: loop and number alone collide
// across projects, and a stopped set keyed on just those two would mark one
// project's issue 7 STOPPED because another project's issue 7 was killed.
type stoppedKey struct {
	ProjectID string
	Loop      string
	Number    int
}

// stoppedSet builds the lookup applyStopped uses, from DB.StoppedIssues().
// projectID narrows the set to one project -- Sessions' case, which reports
// one project and has no loop filter of its own to scope by -- and an empty
// projectID keeps every project, for AllSessions' machine-wide report.
func stoppedSet(all []store.StoppedIssue, projectID string) map[stoppedKey]bool {
	out := make(map[stoppedKey]bool, len(all))
	for _, si := range all {
		if projectID != "" && si.ProjectID != projectID {
			continue
		}
		out[stoppedKey{ProjectID: si.ProjectID, Loop: si.Loop, Number: si.Number}] = true
	}
	return out
}

// applySettings resolves each session's Model and Harness against the
// configuration of the loop that owns it. dirs maps a project id to that
// project's .agent-utils directory; a session whose project is not in the map
// keeps whatever override it already carries.
//
// It is a separate pass over sessionsFrom's output for the reason applyStopped
// is: sessionsFrom groups dispatch rows and reads no files, while this needs
// the loop configuration on disk. Configurations are read once per
// {project, loop} and cached, because a machine-wide report holds many
// sessions per loop and this is the only file read in an otherwise
// database-only path.
//
// A configuration that cannot be read is not an error. A loop can be renamed
// or deleted while its old sessions stay in the database -- FindSession says
// so in its own error text -- and a report that failed outright because one
// loop's file is gone would hide every other row. Such a session keeps its
// override, or shows a dash.
//
// runner.Effective is what merges the two, rather than a local "override wins"
// check: it re-validates each override through config.ParseOverrides and drops
// what fails, so this table reports the same value the runner would actually
// have launched with.
func applySettings(sessions []Session, dirs map[string]string) {
	type key struct{ ProjectID, Loop string }
	cache := map[key]*config.Config{}

	for i := range sessions {
		dir, ok := dirs[sessions[i].ProjectID]
		if !ok || dir == "" {
			continue
		}
		k := key{sessions[i].ProjectID, sessions[i].Loop}
		cfg, seen := cache[k]
		if !seen {
			if path, err := config.Resolve(dir, sessions[i].Loop); err == nil {
				if loaded, err := config.Load(path); err == nil {
					cfg = loaded
				}
			}
			cache[k] = cfg
		}
		if cfg == nil {
			continue
		}
		s := runner.Effective(cfg, config.Overrides{
			Model: sessions[i].Model, Harness: sessions[i].Harness,
		})
		sessions[i].Model = s.Model
		sessions[i].Harness = s.Harness
	}
}

// applyStopped marks only the MOST RECENT session for a stopped issue's
// {ProjectID, Loop, Issue} key. It is a separate pass over sessionsFrom's
// output, not a parameter threaded through sessionsFrom itself: sessionsFrom
// groups dispatches into sessions and has no reason to know about the
// issues table's stopped flag, and threading it through would touch every
// existing caller and test of sessionsFrom for a fact only the renderers
// need.
//
// Live and Orphaned are per-dispatch facts, but Stopped is a fact about the
// ISSUE, and an issue can accumulate several sessions over its history (a
// resume after a park, for example, starts a new one). Marking every
// session sharing the key would show a column of STOPPED rows for runs that
// finished long before the issue was ever stopped; only the session that
// was actually running (or most recently ran) when the operator stopped it
// should read STOPPED.
func applyStopped(sessions []Session, stopped map[stoppedKey]bool) {
	latest := map[stoppedKey]int{}
	for i := range sessions {
		key := stoppedKey{ProjectID: sessions[i].ProjectID, Loop: sessions[i].Loop, Number: sessions[i].Issue}
		if !stopped[key] {
			continue
		}
		if j, ok := latest[key]; !ok || sessions[i].Last.After(sessions[j].Last) {
			latest[key] = i
		}
	}
	for _, i := range latest {
		sessions[i].Stopped = true
	}
}

// sessionsFrom groups dispatches into sessions. The rows arrive newest first, so
// the first row seen for a session is its most recent dispatch.
//
// The key is the project AND the session identifier. Keyed on the identifier
// alone, an identifier that repeated across projects would merge two projects
// into one row, reporting one project's runs and cost under the other's name.
func sessionsFrom(ds []store.Dispatch, loopFilter string) []Session {
	bySession := map[sessionKey]*Session{}
	var order []sessionKey
	for _, d := range ds {
		if d.SessionID == "" {
			continue
		}
		if loopFilter != "" && d.Loop != loopFilter {
			continue
		}
		key := sessionKey{ProjectID: d.ProjectID, SessionID: d.SessionID}
		cur, ok := bySession[key]
		if !ok {
			cur = &Session{
				ID:         d.SessionID,
				ProjectID:  d.ProjectID,
				Repo:       d.Repo,
				Loop:       d.Loop,
				Issue:      d.Number,
				Title:      d.Title,
				Last:       d.StartedAt,
				LastStatus: d.Status,
				LastKind:   d.Kind,
				Model:      d.Model,
				Harness:    d.Harness,
			}
			if d.Status == store.StatusRunning {
				if proc.IsAlive(d.PID, d.RunnerID()) {
					cur.Live = true
				} else {
					cur.Orphaned = true
				}
			}
			bySession[key] = cur
			order = append(order, key)
		}
		cur.Dispatches++
		cur.Cost += d.CostUSD
		cur.First = d.StartedAt // ends on the oldest row
		if cur.Title == "" {
			cur.Title = d.Title
		}
	}

	out := make([]Session, 0, len(order))
	for _, key := range order {
		out = append(out, *bySession[key])
	}
	return out
}

// RenderSessions formats sessions for a terminal.
//
// all keeps the sessions of closed issues, which are hidden otherwise. The
// hiding happens HERE rather than in Sessions so that FindSession -- and
// therefore `project logs --session` and `sessions describe` -- can still
// resolve a closed issue's session by name. A report is the only place the
// distinction matters.
func RenderSessions(p *Project, sessions []Session, all bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "project %s  (%s)\n\n", p.Config.Name, p.Root)

	shown, hidden := visible(sessions, all)
	if len(shown) == 0 {
		if hidden > 0 {
			// Distinct from the never-ran text below: this project HAS
			// sessions, and every one of them is finished work.
			fmt.Fprintf(&b, "No open sessions.%s", hiddenNote(hidden))
			return b.String()
		}
		fmt.Fprintf(&b, "No sessions yet. A session is created the first time a loop\n")
		fmt.Fprintf(&b, "dispatches an agent for an issue, and is reused on every resume.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "%-38s %-12s %-6s %-24s %-5s %-9s %-24s %-8s %-10s %s\n",
		"SESSION", "LOOP", "ISSUE", "TITLE", "RUNS", "COST", "MODEL", "HARNESS",
		"STATE", "LAST RUN")
	for _, s := range shown {
		state := s.LastStatus
		switch {
		case s.Live:
			state = "running"
		case s.Stopped:
			// Above ORPHANED, below running: a stopped session's runner is
			// gone BY DESIGN (a kill sets the flag before it ever signals),
			// so calling it an orphan would send the operator hunting a
			// crash that did not happen. A live agent still outranks the
			// flag -- see the Live case above, which is checked first.
			state = "STOPPED"
		case s.Orphaned:
			state = "ORPHANED"
		case s.Closed:
			// Below ORPHANED: a closed issue whose agent never recorded an
			// outcome is still an orphan to recover, and the row is only
			// visible at all because --all asked for it.
			state = "CLOSED"
		}
		fmt.Fprintf(&b, "%-38s %-12s %-6d %-24s %-5d $%-8.2f %-24s %-8s %-10s %s\n",
			s.ID, truncate(s.Loop, 12), s.Issue, truncate(s.Title, 24),
			s.Dispatches, s.Cost, truncate(orDash(s.Model), 24),
			truncate(orDash(s.Harness), 8), state,
			s.Last.Local().Format("2006-01-02 15:04"))
	}

	fmt.Fprint(&b, hiddenNote(hidden))
	fmt.Fprintf(&b, "\nFollow one with: agent-utils project logs --session <SESSION>\n")
	return b.String()
}

// RenderAllSessions formats the machine-wide sessions table for a terminal.
//
// The table spans every project, so it leads with a PROJECT column and prints
// no project header: there is no single project to name, and the column is what
// tells two projects' rows apart. Every other column is byte-identical to
// RenderSessions, so an operator reading both reports reads one layout.
//
// It takes the filter because an empty result has two meanings that read very
// differently. Nothing has run on this machine yet is a state to fix by
// starting a loop; nothing matched what you asked for is a state to fix by
// asking for something else. Only the filter knows which one is true, and the
// renderer is where the operator sees the answer.
func RenderAllSessions(sessions []Session, f SessionFilter) string {
	var b strings.Builder

	shown, hidden := visible(sessions, f.All)
	if len(shown) == 0 {
		if hidden > 0 {
			// Checked before f.filtered(): "nothing matched that filter" is
			// misleading when what actually happened is that every match was
			// finished work, and --all is the flag that answers it.
			fmt.Fprintf(&b, "No open sessions.%s", hiddenNote(hidden))
			return b.String()
		}
		if f.filtered() {
			fmt.Fprintf(&b, "No sessions matched that filter.\n")
			return b.String()
		}
		fmt.Fprintf(&b, "No sessions yet on this machine. A session is created the first time\n")
		fmt.Fprintf(&b, "a loop dispatches an agent for an issue, and is reused on every resume.\n")
		fmt.Fprintf(&b, "See the registered projects with: agent-utils list\n")
		return b.String()
	}

	fmt.Fprintf(&b, "%-16s %-38s %-12s %-6s %-24s %-5s %-9s %-24s %-8s %-10s %s\n",
		"PROJECT", "SESSION", "LOOP", "ISSUE", "TITLE", "RUNS", "COST", "MODEL",
		"HARNESS", "STATE", "LAST RUN")
	for _, s := range shown {
		state := s.LastStatus
		switch {
		case s.Live:
			state = "running"
		case s.Stopped:
			// Same ordering as RenderSessions: above ORPHANED, below
			// running. See that renderer's comment for why.
			state = "STOPPED"
		case s.Orphaned:
			state = "ORPHANED"
		case s.Closed:
			state = "CLOSED"
		}
		// Project is padded but never truncated. The footer asks the operator
		// to type this value back into --name, and registry.Find matches a
		// name exactly, so an elided name is a selector that cannot resolve.
		// RenderProjects pads loop names the same way, for the same reason.
		fmt.Fprintf(&b, "%-16s %-38s %-12s %-6d %-24s %-5d $%-8.2f %-24s %-8s %-10s %s\n",
			s.Project, s.ID, truncate(s.Loop, 12), s.Issue,
			truncate(s.Title, 24), s.Dispatches, s.Cost,
			truncate(orDash(s.Model), 24), truncate(orDash(s.Harness), 8),
			state, s.Last.Local().Format("2006-01-02 15:04"))
	}

	// The long form, naming the project. The top-level logs command resolves
	// the project from the working directory, and this table is read from
	// anywhere, so the short form the per-project report prints would fail for
	// most of the rows here.
	fmt.Fprint(&b, hiddenNote(hidden))
	fmt.Fprintf(&b, "\nFollow one with: agent-utils project --name <PROJECT> logs --session <SESSION>\n")
	return b.String()
}

// FindSession locates a session in a project and returns it with the path of
// the loop configuration that owns it.
//
// A session identifier is unique, so it names its own loop. Requiring --name
// alongside --session would make the operator supply something the identifier
// already determines.
func FindSession(p *Project, sessionID string) (Session, string, error) {
	sessions, err := Sessions(p, "")
	if err != nil {
		return Session{}, "", err
	}
	var found Session
	for _, sess := range sessions {
		if sess.ID == sessionID {
			found = sess
			break
		}
	}
	if found.ID == "" {
		return Session{}, "", fmt.Errorf("no session %q in project %s", sessionID, p.Config.Name)
	}

	// The session names its loop; the loop names its configuration file, which
	// is what the caller needs to read the log.
	path, err := config.Resolve(p.Dir, found.Loop)
	if err != nil {
		return Session{}, "", fmt.Errorf(
			"session %s belongs to loop %q, whose configuration is gone: %w",
			sessionID, found.Loop, err)
	}
	return found, path, nil
}
