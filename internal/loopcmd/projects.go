package loopcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/proc"
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
	Root    string
	Dir     string
	Missing bool
	Loops   []LoopSummary
	Err     error
}

// Projects reads every registered project and summarises its loops.
//
// It reads only local state: the registry, each project's configuration files,
// and each loop's database. It makes no GitHub call, so it is fast, works
// offline, and needs no token.
func Projects() ([]ProjectSummary, error) {
	entries, err := registry.List()
	if err != nil {
		return nil, err
	}

	out := make([]ProjectSummary, 0, len(entries))
	for _, p := range entries {
		summary := ProjectSummary{Root: p.Root, Dir: p.AgentUtilsDir}
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
			summary.Loops = append(summary.Loops, summariseLoop(c))
		}
		sort.Slice(summary.Loops, func(i, j int) bool {
			return summary.Loops[i].Name < summary.Loops[j].Name
		})
		out = append(out, summary)
	}
	return out, nil
}

func summariseLoop(entry config.Entry) LoopSummary {
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
	stateDir, err := cfg.ResolveStateDir(entry.Path)
	if err != nil {
		sum.Err = err
		return sum
	}
	sum.StateDir = stateDir

	dbPath := filepath.Join(stateDir, "state.db")
	if !fileExists(dbPath) {
		// Configured but never run. That is a normal state, not an error.
		return sum
	}

	s, err := store.Open(dbPath)
	if err != nil {
		sum.Err = err
		return sum
	}
	defer s.Close()

	if sum.Ticks, err = s.TickCount(cfg.Name); err != nil {
		sum.Err = err
		return sum
	}
	if sum.LastTick, err = s.LastTick(cfg.Name); err != nil {
		sum.Err = err
		return sum
	}
	running, err := s.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		sum.Err = err
		return sum
	}
	for _, d := range running {
		if proc.IsAlive(d.PID, d.ID) {
			sum.Live++
		} else {
			sum.Orphans++
		}
	}
	costs, err := s.CostByIssue(cfg.Name, cfg.Repo)
	if err != nil {
		sum.Err = err
		return sum
	}
	for _, c := range costs {
		sum.Cost += c
	}
	return sum
}

// RenderProjects formats the project summaries for a terminal.
func RenderProjects(projects []ProjectSummary) string {
	var b strings.Builder

	if len(projects) == 0 {
		fmt.Fprintf(&b, "No projects have been used yet.\n\n")
		fmt.Fprintf(&b, "A project is recorded the first time a command runs against its\n")
		fmt.Fprintf(&b, "%s directory. To onboard one:\n\n", config.DirName)
		fmt.Fprintf(&b, "  mkdir -p %s/%s\n", config.DirName, config.ConfigsSubdir)
		fmt.Fprintf(&b, "  cp <a loop config>.yaml %s/%s/\n", config.DirName, config.ConfigsSubdir)
		fmt.Fprintf(&b, "  agent-utils list\n")
		return b.String()
	}

	for i, p := range projects {
		if i > 0 {
			fmt.Fprintln(&b)
		}
		fmt.Fprintf(&b, "%s\n", p.Root)

		switch {
		case p.Missing:
			fmt.Fprintf(&b, "  MISSING: %s no longer exists\n", p.Dir)
			fmt.Fprintf(&b, "  Remove it with: agent-utils forget %s\n", p.Root)
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

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
