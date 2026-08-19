package listener

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
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
}

// Ref returns the identity loopcmd.Open needs to run this target's tick. It
// carries only ID, Name, and Dir -- loopcmd.Open resolves everything else
// (the config itself, the state directory, the GitHub client) from
// t.ConfigPath and the token, which are not part of a ProjectRef.
func (t Target) Ref() loopcmd.ProjectRef {
	return loopcmd.ProjectRef{ID: t.ProjectID, Name: t.ProjectName, Dir: t.Dir}
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
	projects, err := registry.List()
	if err != nil {
		// Returned, not logged-and-skipped: an empty result here would look
		// exactly like "no loop watches this repository," and the delivery
		// would be dropped with no recorded outcome anywhere. The per-project
		// failures below are different -- they still leave every OTHER
		// project's loops routable.
		return nil, fmt.Errorf("list registered projects: %w", err)
	}

	var out []Target
	for _, p := range projects {
		if !p.Exists() {
			// registry.Project.Exists documents that a moved or deleted
			// project stays in the registry until pruned. One vanished
			// directory must not stop every other project's loop from
			// ticking, so this is logged and skipped, not returned.
			slog.Warn("skipping project: directory no longer exists",
				"project", p.Name, "dir", p.AgentUtilsDir)
			continue
		}

		entries, err := config.List(p.AgentUtilsDir)
		if err != nil {
			if errors.Is(err, config.ErrNoConfigs) {
				// The normal state of a registered project that has not (or
				// not yet) grown a configs/ directory. Logged at Info, not
				// Warn, so it does not read as a problem when it is not one.
				slog.Info("skipping project: no loop configurations",
					"project", p.Name, "dir", p.AgentUtilsDir)
			} else {
				slog.Warn("skipping project: cannot list loop configurations",
					"project", p.Name, "dir", p.AgentUtilsDir, "err", err)
			}
			continue
		}

		for _, e := range entries {
			if e.Err != nil {
				// One unparsable loop file must not stop this project's
				// other loops, or any other project's loops, from ticking.
				slog.Warn("skipping loop: cannot load config",
					"loop", e.Name, "project", p.Name, "file", e.File, "err", e.Err)
				continue
			}
			// EqualFold, not ==: matches ghub.ListOpenPullRequests, which
			// folds for the same reason -- GitHub's own casing of a
			// full_name and the casing an operator typed into a yaml file
			// need not agree.
			if !strings.EqualFold(e.Repo, repo) {
				continue
			}
			out = append(out, Target{
				ProjectID:   p.ID,
				ProjectName: p.Name,
				Dir:         p.AgentUtilsDir,
				ConfigPath:  e.Path,
				LoopName:    e.Name,
				Repo:        e.Repo,
			})
		}
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
