package listener

import (
	"errors"
	"fmt"
	"log/slog"
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

// TargetFor returns the one loop named loop inside the project named
// projectID.
//
// Waking a retry deadline uses this, never Targets: a deadline belongs to
// one project's issue, and routing it by repository would dispatch agents
// in every other project that happens to watch the same repository, on that
// project's own token budget.
func TargetFor(projectID, loop string) (Target, bool, error) {
	projects, err := registry.List()
	if err != nil {
		return Target{}, false, fmt.Errorf("list registered projects: %w", err)
	}

	for _, p := range projects {
		if p.ID != projectID {
			continue
		}
		if !p.Exists() {
			slog.Warn("cannot route retry: project directory no longer exists",
				"project", p.Name, "dir", p.AgentUtilsDir)
			return Target{}, false, nil
		}

		entries, err := config.List(p.AgentUtilsDir)
		if err != nil {
			if !errors.Is(err, config.ErrNoConfigs) {
				slog.Warn("cannot route retry: cannot list loop configurations",
					"project", p.Name, "dir", p.AgentUtilsDir, "err", err)
			}
			return Target{}, false, nil
		}

		for _, e := range entries {
			if e.Err != nil || e.Name != loop {
				continue
			}
			return Target{
				ProjectID:   p.ID,
				ProjectName: p.Name,
				Dir:         p.AgentUtilsDir,
				ConfigPath:  e.Path,
				LoopName:    e.Name,
				Repo:        e.Repo,
			}, true, nil
		}
		return Target{}, false, nil
	}
	// No project with this id is registered at all -- not distinguished
	// from "no such loop in that project" above, because both mean the same
	// thing to a caller: there is nothing here to wake.
	return Target{}, false, nil
}
