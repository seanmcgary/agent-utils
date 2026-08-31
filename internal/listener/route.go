package listener

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/loopcmd"
	"github.com/seanmcgary/agent-utils/internal/registry"
)

// Target is one loop that watches a repository.
type Target struct {
	ProjectID   string
	ProjectName string
	Dir         string // the project's .agent-utils directory
	ConfigPath  string
	LoopName    string
	Repo        string
	// DefaultBranch and TendPR are what let Deliver drop a push delivery
	// before it opens anything. Open reads the token, opens a SQLite handle
	// and runs the migration check; a busy feature branch would pay all three
	// on every push, once per loop, for a delivery no loop can act on.
	DefaultBranch string
	TendPR        bool
}

// Ref returns the identity loopcmd.Open needs to run this target's tick. It
// carries only ID, Name, and Dir -- loopcmd.Open resolves everything else
// (the config itself, the state directory, the GitHub client) from
// t.ConfigPath and the token, which are not part of a ProjectRef.
func (t Target) Ref() loopcmd.ProjectRef {
	return loopcmd.ProjectRef{ID: t.ProjectID, Name: t.ProjectName, Dir: t.Dir}
}

// Skip is one thing a scan could not route, and why.
//
// It exists because the walk below already logs every one of these and then
// throws it away. A per-delivery log line is the wrong place for a
// misconfiguration that is permanent: it scrolls past, and the loop it names
// never routes again until somebody fixes it. Returning the skips lets
// `listener start` show them at the moment an operator is watching -- see
// routingTable in cmd/agent-utils/listener.go.
type Skip struct {
	// Project is the registered project's name.
	Project string
	// Dir is the project's .agent-utils directory.
	Dir string
	// File is the base name of the loop file that was skipped. It is empty
	// when the whole project was skipped, which is the difference between
	// "one loop of this project does not load" and "none of this project's
	// loops route at all".
	File string
	// Reason says what stopped it, in the same words the log line uses.
	Reason string
}

// Routes is one whole scan of this machine: every loop that can receive a
// delivery, and everything skipped on the way to that list.
type Routes struct {
	Targets []Target
	Skips   []Skip
}

// RepoRoute is every loop that watches one repository.
type RepoRoute struct {
	Repo    string
	Targets []Target
}

// ByRepo groups the targets by the repository they watch, sorted, so a
// routing table can be printed in the shape a delivery arrives in.
//
// Grouping folds case, exactly as Targets matches: two projects that spell
// one repository differently in their yaml both receive its deliveries, so
// showing them as two separate repositories would misreport what this daemon
// does. When they disagree, the label is the spelling used by the first loop
// listed under it, NOT the first one the scan happened to walk past:
// registry.List returns projects most-recently-used first, so scan order
// changes as loops tick, and a banner whose repository names shuffle between
// restarts cannot be compared against the last one. Nothing here is used to
// call GitHub, so no spelling is more correct than another.
func (r Routes) ByRepo() []RepoRoute {
	index := map[string]int{}
	var out []RepoRoute
	for _, t := range r.Targets {
		key := strings.ToLower(t.Repo)
		i, ok := index[key]
		if !ok {
			index[key] = len(out)
			out = append(out, RepoRoute{Repo: t.Repo})
			i = len(out) - 1
		}
		out[i].Targets = append(out[i].Targets, t)
	}
	for i := range out {
		g := &out[i]
		sort.Slice(g.Targets, func(a, b int) bool {
			if g.Targets[a].ProjectName != g.Targets[b].ProjectName {
				return g.Targets[a].ProjectName < g.Targets[b].ProjectName
			}
			return g.Targets[a].LoopName < g.Targets[b].LoopName
		})
		g.Repo = g.Targets[0].Repo
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Repo) < strings.ToLower(out[j].Repo)
	})
	return out
}

// Scan walks every registered project and returns every loop on this machine,
// plus what it had to skip.
//
// This is the whole of Targets' walk apart from the repo filter, and Targets
// is written in terms of it rather than beside it. A second copy of the skip
// rules would drift: a project this one showed and Targets dropped would make
// `listener start`'s routing table promise deliveries that never happen, and
// the divergence would only ever surface as "the webhook does nothing".
//
// Nothing here is cached, for the reason Targets documents.
func Scan() (Routes, error) {
	projects, err := registry.List()
	if err != nil {
		// Returned, not logged-and-skipped: an empty result here would look
		// exactly like "no loop watches this repository," and the delivery
		// would be dropped with no recorded outcome anywhere. The per-project
		// failures below are different -- they still leave every OTHER
		// project's loops routable.
		return Routes{}, fmt.Errorf("list registered projects: %w", err)
	}

	var routes Routes
	for _, p := range projects {
		if !p.Exists() {
			// registry.Project.Exists documents that a moved or deleted
			// project stays in the registry until pruned. One vanished
			// directory must not stop every other project's loop from
			// ticking, so this is logged and skipped, not returned.
			slog.Warn("skipping project: directory no longer exists",
				"project", p.Name, "dir", p.AgentUtilsDir)
			routes.Skips = append(routes.Skips, Skip{
				Project: p.Name, Dir: p.AgentUtilsDir,
				Reason: "the project directory no longer exists",
			})
			continue
		}

		entries, err := config.List(p.AgentUtilsDir)
		if err != nil {
			reason := "cannot list loop configurations: " + err.Error()
			if errors.Is(err, config.ErrNoConfigs) {
				// The normal state of a registered project that has not (or
				// not yet) grown a configs/ directory. Logged at Info, not
				// Warn, so it does not read as a problem when it is not one.
				slog.Info("skipping project: no loop configurations",
					"project", p.Name, "dir", p.AgentUtilsDir)
				reason = "no loop configurations"
			} else {
				slog.Warn("skipping project: cannot list loop configurations",
					"project", p.Name, "dir", p.AgentUtilsDir, "err", err)
			}
			routes.Skips = append(routes.Skips, Skip{
				Project: p.Name, Dir: p.AgentUtilsDir, Reason: reason,
			})
			continue
		}

		for _, e := range entries {
			if e.Err != nil {
				// One unparsable loop file must not stop this project's
				// other loops, or any other project's loops, from ticking.
				slog.Warn("skipping loop: cannot load config",
					"loop", e.Name, "project", p.Name, "file", e.File, "err", e.Err)
				routes.Skips = append(routes.Skips, Skip{
					Project: p.Name, Dir: p.AgentUtilsDir, File: e.File,
					Reason: "cannot load config: " + e.Err.Error(),
				})
				continue
			}
			routes.Targets = append(routes.Targets, Target{
				ProjectID:     p.ID,
				ProjectName:   p.Name,
				Dir:           p.AgentUtilsDir,
				ConfigPath:    e.Path,
				LoopName:      e.Name,
				Repo:          e.Repo,
				DefaultBranch: e.DefaultBranch,
				TendPR:        e.TendPR,
			})
		}
	}
	// Sorted for the same reason ByRepo sorts: registry.List orders projects
	// by how recently they were used, so an unsorted skip list reorders
	// itself between restarts and two runs of the same banner cannot be
	// compared.
	sort.Slice(routes.Skips, func(i, j int) bool {
		if routes.Skips[i].Project != routes.Skips[j].Project {
			return routes.Skips[i].Project < routes.Skips[j].Project
		}
		return routes.Skips[i].File < routes.Skips[j].File
	})
	return routes, nil
}

// Targets returns every loop on this machine whose repo matches, case
// insensitively.
//
// Nothing here is cached. An operator who adds a loop expects the very next
// delivery to use it, not one after some cache expires, and the scan below
// reads a registry file and a handful of small per-project yaml files -- far
// less work than the GitHub API call a match triggers next. Caching would
// buy back microseconds against a call that costs milliseconds, at the price
// of a loop that silently does not receive deliveries until a cache entry
// happens to turn over.
func Targets(repo string) ([]Target, error) {
	routes, err := Scan()
	if err != nil {
		return nil, err
	}

	var out []Target
	for _, t := range routes.Targets {
		// EqualFold, not ==: matches ghub.ListOpenPullRequests, which
		// folds for the same reason -- GitHub's own casing of a
		// full_name and the casing an operator typed into a yaml file
		// need not agree.
		if !strings.EqualFold(t.Repo, repo) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// Routing is what TargetFor was able to establish about a loop.
//
// It is three values rather than a bool because the caller acts differently
// on the two failures, and irreversibly on one of them: work.go's
// noteUnroutable clears a durable failure flag, which nothing re-derives. A
// single ok=false folded five conditions together -- an unregistered
// project, a directory not present right now, a configs directory that could
// not be listed, a loop whose yaml does not currently parse, and a loop that
// really is gone -- and forced the caller to guess between them with a
// timer. An operator saving a half-finished yaml file is exactly the case a
// timer does not cover: the file stays broken for as long as they are
// editing it.
type Routing int

const (
	// RouteFound: the loop exists and the returned Target names it.
	RouteFound Routing = iota
	// RouteGone: the registry answered, the project's configs directory
	// listed cleanly, every file in it parsed, and none of them declares
	// this loop. Nothing transient can produce this, so a caller may act on
	// it destructively.
	RouteGone
	// RouteUnknown: something stopped this call from answering -- a
	// directory that is not present, a configs directory that could not be
	// read, a file that does not parse right now. Every one of these is
	// something an operator fixes, and every one of them looks identical to
	// "gone" from the outside, so a caller must wait rather than act.
	RouteUnknown
)

func (r Routing) String() string {
	switch r {
	case RouteFound:
		return "found"
	case RouteGone:
		return "gone"
	default:
		return "unknown"
	}
}

// TargetFor returns the one loop named loop inside the project named
// projectID, and what it could establish when it does not.
//
// Waking a retry deadline uses this, never Targets: a deadline belongs to
// one project's issue, and routing it by repository would dispatch agents
// in every other project that happens to watch the same repository, on that
// project's own token budget.
func TargetFor(projectID, loop string) (Target, Routing, error) {
	projects, err := registry.List()
	if err != nil {
		return Target{}, RouteUnknown, fmt.Errorf("list registered projects: %w", err)
	}

	for _, p := range projects {
		if p.ID != projectID {
			continue
		}
		if !p.Exists() {
			// Not "gone": registry.Project.Exists documents that a moved or
			// deleted project stays registered until pruned, and an
			// unmounted volume or a directory being restored looks exactly
			// like a deleted one from here.
			slog.Warn("cannot route retry: project directory is not present",
				"project", p.Name, "dir", p.AgentUtilsDir)
			return Target{}, RouteUnknown, nil
		}

		entries, err := config.List(p.AgentUtilsDir)
		if err != nil {
			if !errors.Is(err, config.ErrNoConfigs) {
				slog.Warn("cannot route retry: cannot list loop configurations",
					"project", p.Name, "dir", p.AgentUtilsDir, "err", err)
				return Target{}, RouteUnknown, nil
			}
			// ErrNoConfigs folds two states together: the directory really
			// holds no loop file, and config.List could not stat it at all
			// (its isDir helper reports false for a permission or I/O error
			// as readily as for an absent directory). Only the first is
			// "gone", so the distinction is made here rather than inferred.
			configs := config.ConfigsDir(p.AgentUtilsDir)
			if _, statErr := os.Stat(configs); statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
				slog.Warn("cannot route retry: cannot read the configs directory",
					"project", p.Name, "dir", configs, "err", statErr)
				return Target{}, RouteUnknown, nil
			}
			return Target{}, RouteGone, nil
		}

		broken := false
		for _, e := range entries {
			if e.Err != nil {
				// A file that does not load declares no name, so it cannot
				// be ruled out as this loop: config.Entry.Name falls back to
				// the FILE's base name, which need not equal the `name:`
				// field inside it. Any unloadable file in the project
				// therefore makes the answer unknown rather than gone --
				// the cost is a repeated warning on a bounded wake
				// interval, and the alternative is destroying a pending
				// retry because a config was mid-save.
				slog.Warn("skipping loop: cannot load config",
					"loop", e.Name, "project", p.Name, "file", e.File, "err", e.Err)
				broken = true
				continue
			}
			if e.Name != loop {
				continue
			}
			return Target{
				ProjectID:   p.ID,
				ProjectName: p.Name,
				Dir:         p.AgentUtilsDir,
				ConfigPath:  e.Path,
				LoopName:    e.Name,
				Repo:        e.Repo,
			}, RouteFound, nil
		}
		if broken {
			return Target{}, RouteUnknown, nil
		}
		return Target{}, RouteGone, nil
	}
	// No project with this id is registered at all. registry.List answered,
	// so this is as definite as the empty-configs case above: there is no
	// project here to own the deadline.
	return Target{}, RouteGone, nil
}
