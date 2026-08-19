package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/project"
	"github.com/seanmcgary/agent-utils/internal/registry"
)

// StateDBFile is the name a per-loop database has, in every layout. It is the
// same name the canonical database carries.
const StateDBFile = home.StateDBFile

// Discover returns every legacy database one project still holds.
//
// It looks in two places, because neither alone is complete:
//
//  1. The project's loop configurations, resolved through ResolveStateDir. This
//     is the only way to find a loop whose state_dir points somewhere else.
//  2. A direct scan of <agentUtilsDir>/state/*/state.db, the derived layout.
//     This is the only way to find the state of a loop whose configuration was
//     deleted or renamed, which would otherwise sit unimported forever.
//
// A configuration file that does not load is reported and skipped. The reason
// travels with the report, so `agent-utils migrate` can show it.
func Discover(agentUtilsDir, projectID, projectName string) ([]Source, []Result) {
	var (
		sources []Source
		results []Result
		seen    = map[string]bool{}
	)
	add := func(s Source) {
		if seen[s.Key()] {
			return
		}
		seen[s.Key()] = true
		sources = append(sources, s)
	}

	entries, err := config.List(agentUtilsDir)
	if err != nil && !errors.Is(err, config.ErrNoConfigs) {
		results = append(results, skipped(projectID, projectName, "",
			fmt.Sprintf("the loop configurations cannot be listed: %v", err)))
		return sources, results
	}

	// A configuration that does not load is reported and skipped, not failed.
	// State is per loop, so a broken sibling hides nothing this command needs,
	// and that loop cannot run until the file is fixed anyway. The loop a command
	// actually acts on is always added by the caller through SourceFor, so it is
	// never discovery that decides whether the write path can see its own rows.
	for _, e := range entries {
		if e.Err != nil {
			results = append(results, skipped(projectID, projectName, e.Name,
				fmt.Sprintf("%s does not load: %v", e.Path, e.Err)))
			continue
		}
		cfg, err := config.Load(e.Path)
		if err != nil {
			results = append(results, skipped(projectID, projectName, e.Name,
				fmt.Sprintf("%s does not load: %v", e.Path, err)))
			continue
		}
		stateDir, err := cfg.ResolveStateDir(e.Path)
		if err != nil {
			results = append(results, skipped(projectID, projectName, cfg.Name,
				fmt.Sprintf("the state directory cannot be resolved: %v", err)))
			continue
		}
		path, ok := legacyPath(stateDir)
		if !ok {
			continue // configured but never run
		}
		add(Source{
			Path: path, ProjectID: projectID, ProjectName: projectName,
			Loop: cfg.Name, Repo: cfg.Repo,
		})
	}

	// The derived layout, for a loop whose configuration is gone.
	stateRoot := filepath.Join(agentUtilsDir, config.StateSubdir)
	dirs, err := os.ReadDir(stateRoot)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// A project that has never run has no state directory. Normal.
	case err != nil:
		// Anything else hides state this scan is the only way to find, so it is
		// reported rather than passed over in silence.
		results = append(results, skipped(projectID, projectName, "",
			fmt.Sprintf("%s cannot be read: %v", stateRoot, err)))
	default:
		for _, d := range dirs {
			if !d.IsDir() {
				continue
			}
			path, ok := legacyPath(filepath.Join(stateRoot, d.Name()))
			if !ok {
				continue
			}
			add(Source{
				Path: path, ProjectID: projectID, ProjectName: projectName,
				Loop: d.Name(),
			})
		}
	}

	sort.Slice(sources, func(i, j int) bool {
		if sources[i].Path != sources[j].Path {
			return sources[i].Path < sources[j].Path
		}
		return sources[i].Loop < sources[j].Loop
	})
	return sources, results
}

// DiscoverAll returns every legacy database on the machine.
//
// A project whose directory has moved, or which has no descriptor, is reported
// and skipped. Only a failure to read the registry itself is an error: one
// unreadable project must not stop `agent-utils list`.
func DiscoverAll() ([]Source, []Result, error) {
	projects, err := registry.List()
	if err != nil {
		return nil, nil, err
	}

	var (
		sources []Source
		results []Result
	)
	for _, p := range projects {
		if !p.Exists() {
			results = append(results, Result{
				Source: Source{ProjectID: p.ID, ProjectName: p.Name, Path: p.AgentUtilsDir},
				State:  StateSkipped,
				Reason: "the project directory is gone",
			})
			continue
		}
		cfg, err := project.Load(p.AgentUtilsDir)
		if err != nil {
			results = append(results, Result{
				Source: Source{ProjectID: p.ID, ProjectName: p.Name, Path: p.AgentUtilsDir},
				State:  StateSkipped,
				Reason: fmt.Sprintf("the project descriptor does not load: %v", err),
			})
			continue
		}
		found, problems := Discover(p.AgentUtilsDir, cfg.ID, cfg.Name)
		sources = append(sources, found...)
		results = append(results, problems...)
	}
	return sources, results, nil
}

func skipped(projectID, projectName, loop, reason string) Result {
	return Result{
		Source: Source{ProjectID: projectID, ProjectName: projectName, Loop: loop},
		State:  StateSkipped,
		Reason: reason,
	}
}

// Add appends a source unless the list already holds that file and loop.
//
// The caller adds its own loop explicitly, and discovery usually found it too.
// Importing it twice per command is wasted work, and would double-count it in
// any report.
func Add(sources []Source, s Source) []Source {
	for _, existing := range sources {
		if existing.Key() == s.Key() {
			return sources
		}
	}
	return append(sources, s)
}

// SourceFor returns the source for one already-resolved loop.
//
// The command line can name a configuration by an arbitrary path, and such a
// loop's state directory is not always inside the directory Discover scans. The
// caller therefore always adds its own loop explicitly.
func SourceFor(stateDir, projectID, projectName, loop, repo string) (Source, bool) {
	path, ok := legacyPath(stateDir)
	if !ok {
		return Source{}, false
	}
	return Source{
		Path: path, ProjectID: projectID, ProjectName: projectName,
		Loop: loop, Repo: repo,
	}, true
}

// legacyPath returns the resolved state.db in a state directory, and whether it
// exists.
//
// The path is made absolute and symlink-free. One file recorded under two
// spellings would be imported twice, and every dispatch in it would be
// duplicated.
func legacyPath(stateDir string) (string, bool) {
	path := filepath.Join(stateDir, StateDBFile)
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return home.Resolve(path), true
}

// IsCanonical reports whether a source IS the canonical database. Such a source
// is stamped in place rather than copied.
func IsCanonical(path string) bool {
	canonical, err := home.StateDBPath()
	if err != nil {
		return false
	}
	return home.Resolve(canonical) == path
}
