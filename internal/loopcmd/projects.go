package loopcmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/registry"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// LoopSummary is one loop's state within a project.
type LoopSummary struct {
	Name     string
	Repo     string
	StateDir string
	LastTick time.Time
	Ticks    int64
	Live     int
	Orphans  int
	Cost     float64
	// Err records why this loop could not be read. A loop that cannot be
	// summarised is reported rather than omitted.
	Err error
}

// ProjectSummary is one registered project and every loop it defines.
type ProjectSummary struct {
	Name    string
	ID      string
	Root    string
	Dir     string
	Missing bool
	Loops   []LoopSummary
	Err     error
}

// Projects reads every registered project and summarises its loops.
//
// It reads only local state: the registry, each project's configuration files,
// and the one state database. It makes no GitHub call, so it is fast, works
// offline, and needs no token.
func Projects() ([]ProjectSummary, error) {
	entries, err := registry.List()
	if err != nil {
		return nil, err
	}

	db, err := openCanonical()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	snap, err := readSnapshot(db)
	if err != nil {
		return nil, err
	}

	out := make([]ProjectSummary, 0, len(entries))
	for _, p := range entries {
		summary := ProjectSummary{Name: p.Name, ID: p.ID, Root: p.Root, Dir: p.AgentUtilsDir}
		if !p.Exists() {
			// Keep it in the report. A project whose directory has moved is
			// something the operator should see, not something to hide.
			summary.Missing = true
			out = append(out, summary)
			continue
		}

		configs, err := config.List(p.AgentUtilsDir)
		if err != nil {
			summary.Err = err
			out = append(out, summary)
			continue
		}
		for _, c := range configs {
			summary.Loops = append(summary.Loops, summariseLoop(c, p.ID, snap))
		}
		sort.Slice(summary.Loops, func(i, j int) bool {
			return summary.Loops[i].Name < summary.Loops[j].Name
		})
		out = append(out, summary)
	}
	return out, nil
}

// summariseLoop describes one loop from the snapshot the caller already read.
//
// It takes the project identifier because that is half of every key in the
// database: a loop called "planning" exists in more than one project, and the
// name alone would sum them together.
func summariseLoop(entry config.Entry, projectID string, snap *snapshot) LoopSummary {
	sum := LoopSummary{Name: entry.Name, Repo: entry.Repo}
	if entry.Err != nil {
		sum.Err = entry.Err
		return sum
	}

	cfg, err := config.Load(entry.Path)
	if err != nil {
		sum.Err = err
		return sum
	}
	// The state directory no longer holds the database. It still holds the tick
	// lock and the logs, and `project status` prints it for both.
	stateDir, err := cfg.ResolveStateDir(entry.Path)
	if err != nil {
		sum.Err = err
		return sum
	}
	sum.StateDir = stateDir

	k := store.LoopKey{ProjectID: projectID, Loop: cfg.Name}
	st := snap.loops[k]
	sum.Ticks = st.Ticks
	sum.LastTick = st.LastTick
	// Ticks belong to the loop; everything else belongs to the loop's work on the
	// repository it watches today.
	sum.Cost = st.CostByRepo[cfg.Repo]
	byRepo := loopRepo{LoopKey: k, Repo: cfg.Repo}
	sum.Live = snap.live[byRepo]
	sum.Orphans = snap.orphans[byRepo]
	return sum
}

// RenderProjects formats the project summaries for a terminal.
func RenderProjects(projects []ProjectSummary) string {
	var b strings.Builder

	if len(projects) == 0 {
		fmt.Fprintf(&b, "No projects are registered yet.\n\n")
		fmt.Fprintf(&b, "A project is registered the first time a project command runs in it:\n\n")
		fmt.Fprintf(&b, "  mkdir -p %s/%s\n", config.DirName, config.ConfigsSubdir)
		fmt.Fprintf(&b, "  cp <a loop config>.yaml %s/%s/\n", config.DirName, config.ConfigsSubdir)
		fmt.Fprintf(&b, "  agent-utils project list\n")
		return b.String()
	}

	for i, p := range projects {
		if i > 0 {
			fmt.Fprintln(&b)
		}
		// A project registered before it had a descriptor has no name. Fall back
		// to the path so the forget hint below is still something that resolves.
		name, selector := p.Name, p.Name
		if name == "" {
			name, selector = "(unnamed)", p.Root
		}
		fmt.Fprintf(&b, "%s  %s\n", name, p.Root)
		if p.ID != "" {
			fmt.Fprintf(&b, "  id %s\n", p.ID)
		}

		switch {
		case p.Missing:
			fmt.Fprintf(&b, "  MISSING: %s no longer exists\n", p.Dir)
			fmt.Fprintf(&b, "  Remove it with: agent-utils forget %s\n", selector)
			continue
		case p.Err != nil:
			fmt.Fprintf(&b, "  ERROR: %v\n", p.Err)
			continue
		case len(p.Loops) == 0:
			fmt.Fprintf(&b, "  no loop configurations\n")
			continue
		}

		fmt.Fprintf(&b, "  %-14s %-38s %-8s %-6s %-8s %s\n",
			"LOOP", "REPO", "TICKS", "LIVE", "COST", "LAST TICK")
		for _, l := range p.Loops {
			if l.Err != nil {
				fmt.Fprintf(&b, "  %-14s %s\n", l.Name, "INVALID: "+l.Err.Error())
				continue
			}
			last := "never"
			if !l.LastTick.IsZero() {
				last = l.LastTick.Local().Format("2006-01-02 15:04")
			}
			live := fmt.Sprintf("%d", l.Live)
			if l.Orphans > 0 {
				live = fmt.Sprintf("%d+%d!", l.Live, l.Orphans)
			}
			fmt.Fprintf(&b, "  %-14s %-38s %-8d %-6s $%-7.2f %s\n",
				l.Name, l.Repo, l.Ticks, live, l.Cost, last)
		}
	}
	return b.String()
}
