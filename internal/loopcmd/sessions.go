package loopcmd

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/proc"
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
	// Live reports that the most recent dispatch's process is still running.
	Live bool
	// Orphaned reports a dispatch still marked running whose process is gone.
	Orphaned bool
}

// Sessions returns every session in a project, newest activity first.
//
// loopFilter restricts the result to one loop when it is not empty.
func Sessions(p *Project, loopFilter string) ([]Session, error) {
	entries, err := config.List(p.Dir)
	if err != nil {
		return nil, err
	}

	var out []Session
	for _, e := range entries {
		if e.Err != nil {
			continue // a configuration that does not load has no state to read
		}
		if loopFilter != "" && e.Name != loopFilter {
			continue
		}
		cfg, err := config.Load(e.Path)
		if err != nil {
			continue
		}
		stateDir, err := cfg.ResolveStateDir(e.Path)
		if err != nil {
			continue
		}
		dbPath := filepath.Join(stateDir, "state.db")
		if !fileExists(dbPath) {
			continue // configured but never run
		}

		sessions, err := sessionsForLoop(dbPath, cfg)
		if err != nil {
			return nil, err
		}
		out = append(out, sessions...)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	return out, nil
}

func sessionsForLoop(dbPath string, cfg *config.Config) ([]Session, error) {
	s, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	defer s.Close()

	ds, err := s.DispatchesForLoop(cfg.Name, cfg.Repo)
	if err != nil {
		return nil, err
	}

	// DispatchesForLoop is newest first, so the first row seen for a session is
	// its most recent dispatch.
	bySession := map[string]*Session{}
	var order []string
	for _, d := range ds {
		if d.SessionID == "" {
			continue
		}
		cur, ok := bySession[d.SessionID]
		if !ok {
			cur = &Session{
				ID:         d.SessionID,
				Loop:       cfg.Name,
				Issue:      d.Number,
				Title:      d.Title,
				Last:       d.StartedAt,
				LastStatus: d.Status,
				LastKind:   d.Kind,
			}
			if d.Status == store.StatusRunning {
				if proc.IsAlive(d.PID, d.ID) {
					cur.Live = true
				} else {
					cur.Orphaned = true
				}
			}
			bySession[d.SessionID] = cur
			order = append(order, d.SessionID)
		}
		cur.Dispatches++
		cur.Cost += d.CostUSD
		cur.First = d.StartedAt // ends on the oldest row
		if cur.Title == "" {
			cur.Title = d.Title
		}
	}

	out := make([]Session, 0, len(order))
	for _, id := range order {
		out = append(out, *bySession[id])
	}
	return out, nil
}

// RenderSessions formats sessions for a terminal.
func RenderSessions(p *Project, sessions []Session) string {
	var b strings.Builder
	fmt.Fprintf(&b, "project %s  (%s)\n\n", p.Config.Name, p.Root)

	if len(sessions) == 0 {
		fmt.Fprintf(&b, "No sessions yet. A session is created the first time a loop\n")
		fmt.Fprintf(&b, "dispatches an agent for an issue, and is reused on every resume.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "%-38s %-12s %-6s %-30s %-5s %-9s %-10s %s\n",
		"SESSION", "LOOP", "ISSUE", "TITLE", "RUNS", "COST", "STATE", "LAST RUN")
	for _, s := range sessions {
		state := s.LastStatus
		switch {
		case s.Live:
			state = "running"
		case s.Orphaned:
			state = "ORPHANED"
		}
		fmt.Fprintf(&b, "%-38s %-12s %-6d %-30s %-5d $%-8.2f %-10s %s\n",
			s.ID, truncate(s.Loop, 12), s.Issue, truncate(s.Title, 30),
			s.Dispatches, s.Cost, state, s.Last.Local().Format("2006-01-02 15:04"))
	}

	fmt.Fprintf(&b, "\nFollow one with: agent-utils project logs --session <SESSION>\n")
	return b.String()
}

// FindSession locates a session in a project and returns it with the path of
// the loop configuration that owns it.
//
// A session identifier is unique, so it names its own loop. Requiring --name
// alongside --session would make the operator supply something the identifier
// already determines.
func FindSession(p *Project, sessionID string) (Session, string, error) {
	entries, err := config.List(p.Dir)
	if err != nil {
		return Session{}, "", err
	}
	for _, e := range entries {
		if e.Err != nil {
			continue
		}
		cfg, err := config.Load(e.Path)
		if err != nil {
			continue
		}
		stateDir, err := cfg.ResolveStateDir(e.Path)
		if err != nil {
			continue
		}
		dbPath := filepath.Join(stateDir, "state.db")
		if !fileExists(dbPath) {
			continue
		}
		sessions, err := sessionsForLoop(dbPath, cfg)
		if err != nil {
			return Session{}, "", err
		}
		for _, sess := range sessions {
			if sess.ID == sessionID {
				return sess, e.Path, nil
			}
		}
	}
	return Session{}, "", fmt.Errorf("no session %q in project %s", sessionID, p.Config.Name)
}
