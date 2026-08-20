package loopcmd

import (
	"fmt"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/project"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// ProjectDetail is everything `project status` reports about one project.
type ProjectDetail struct {
	Project *Project
	Entries []config.Entry
	Loops   []LoopSummary
	// Webhooks is one entry per repository this project's loops watch,
	// recorded or not. A repository with no record is kept in the list rather
	// than omitted: silence reads as "fine", and "no webhook is registered" is
	// exactly the answer an operator debugging a loop that stopped reacting to
	// GitHub is trying to rule out.
	Webhooks []WebhookStatus
}

// WebhookStatus is what `project status` reports about one repository's
// webhook registration.
type WebhookStatus struct {
	Repo string
	// Recorded distinguishes "no hook" from "hook 0", which is why it is not
	// inferred from HookID.
	Recorded bool
	HookID   int64
	// URL is the delivery target the hook was registered with. It can differ
	// from today's webhook.url, and when it does that difference IS the
	// finding: the live hook is still delivering to the previous endpoint.
	URL string
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
	d.Webhooks, err = summariseWebhooks(db.Project(p.Config.ID), entries)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// summariseWebhooks pairs each repository this project watches with its
// recorded registration, if it has one.
//
// It reads every row once and joins in memory rather than querying per
// repository, matching how readSnapshot replaced one database open per loop.
func summariseWebhooks(s *store.Store, entries []config.Entry) ([]WebhookStatus, error) {
	rows, err := s.Webhooks()
	if err != nil {
		return nil, err
	}
	byRepo := map[string]store.Webhook{}
	for _, w := range rows {
		byRepo[w.Repo] = w
	}

	seen := map[string]bool{}
	var out []WebhookStatus
	for _, e := range entries {
		// A loop whose file failed to load names no repository to report on;
		// RenderProjectDetail already prints its error under the table.
		if e.Err != nil || e.Repo == "" || seen[e.Repo] {
			continue
		}
		seen[e.Repo] = true
		st := WebhookStatus{Repo: e.Repo}
		if w, ok := byRepo[e.Repo]; ok {
			st.Recorded, st.HookID, st.URL = true, w.HookID, w.URL
		}
		out = append(out, st)
	}
	return out, nil
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

	if len(d.Webhooks) > 0 {
		fmt.Fprintf(&b, "\nWEBHOOKS\n")
		for _, w := range d.Webhooks {
			if !w.Recorded {
				fmt.Fprintf(&b, "  %-36s not recorded\n", truncate(w.Repo, 36))
				continue
			}
			fmt.Fprintf(&b, "  %-36s hook %d (%s)\n", truncate(w.Repo, 36), w.HookID, w.URL)
		}
	}

	for _, l := range d.Loops {
		if l.Err != nil {
			fmt.Fprintf(&b, "\n%s: %v\n", l.Name, l.Err)
		}
	}

	// A duplicated name is not cosmetic: the name is half the key of every row
	// this loop owns, and it names the lock and the log tree. Two loops sharing
	// one would read and write each other's state while looking separate.
	if dupes := config.Duplicates(d.Entries); len(dupes) > 0 {
		fmt.Fprintf(&b, "\nWARNING: %d name(s) declared by more than one file: %s\n",
			len(dupes), strings.Join(dupes, ", "))
	}
	return b.String()
}
