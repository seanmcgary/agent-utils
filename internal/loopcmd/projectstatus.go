package loopcmd

import (
	"fmt"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/project"
)

// ProjectDetail is everything `project status` reports about one project.
type ProjectDetail struct {
	Project *Project
	Entries []config.Entry
	Loops   []LoopSummary
}

// Describe gathers a project's identity, its configurations, and the state of
// each loop. It reads only local state, so it needs no token.
func Describe(p *Project) (*ProjectDetail, error) {
	d := &ProjectDetail{Project: p}

	entries, err := config.List(p.Dir)
	if err != nil {
		// A project with no configurations yet is a normal state, not a
		// failure: report the identity and say there are none.
		d.Entries = nil
		return d, nil
	}
	d.Entries = entries

	db, err := openCanonical()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	snap, err := readSnapshot(db)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		d.Loops = append(d.Loops, summariseLoop(e, p.Config.ID, snap))
	}
	return d, nil
}

// RenderProjectDetail formats a project's status for a terminal.
func RenderProjectDetail(d *ProjectDetail) string {
	var b strings.Builder
	p := d.Project

	fmt.Fprintf(&b, "%s\n", p.Config.Name)
	for _, row := range [][2]string{
		{"id", p.Config.ID},
		{"path", p.Root},
		{"dir", p.Dir},
		{"configs", config.ConfigsDir(p.Dir)},
		{"descriptor", project.Path(p.Dir)},
	} {
		fmt.Fprintf(&b, "  %-11s %s\n", row[0], row[1])
	}

	if len(d.Entries) == 0 {
		fmt.Fprintf(&b, "\nNo loop configurations. Add one:\n")
		fmt.Fprintf(&b, "  cp <a loop config>.yaml %s/\n", config.ConfigsDir(p.Dir))
		return b.String()
	}

	fmt.Fprintf(&b, "\n%-16s %-36s %-7s %-6s %-9s %-19s %s\n",
		"LOOP", "REPO", "TICKS", "LIVE", "COST", "LAST TICK", "STATE DIR")
	for _, l := range d.Loops {
		if l.Err != nil {
			// Keep the row on one line so the table stays readable; the full
			// error is printed under the table.
			fmt.Fprintf(&b, "%-16s %s\n", truncate(l.Name, 16), "INVALID")
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
		fmt.Fprintf(&b, "%-16s %-36s %-7d %-6s $%-8.2f %-19s %s\n",
			truncate(l.Name, 16), truncate(l.Repo, 36), l.Ticks, live, l.Cost, last, l.StateDir)
	}

	for _, l := range d.Loops {
		if l.Err != nil {
			fmt.Fprintf(&b, "\n%s: %v\n", l.Name, l.Err)
		}
	}

	// A duplicated name is not cosmetic: the name keys the state directory, the
	// lock and every database row, so two loops sharing one would write the same
	// database while looking separate.
	if dupes := config.Duplicates(d.Entries); len(dupes) > 0 {
		fmt.Fprintf(&b, "\nWARNING: %d name(s) declared by more than one file: %s\n",
			len(dupes), strings.Join(dupes, ", "))
	}
	return b.String()
}
