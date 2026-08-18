package loopcmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// Status renders the reconciled view. It changes nothing.
func Status(ctx context.Context, cfg *config.Config, deps Deps) (string, error) {
	issues, err := deps.GH.ListOpenIssues(ctx, cfg.RepoOwner(), cfg.RepoName())
	if err != nil {
		return "", err
	}
	states, err := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if err != nil {
		return "", err
	}
	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		return "", err
	}
	ticks, err := deps.Store.TickCount(cfg.Name)
	if err != nil {
		return "", err
	}
	cooldown, err := deps.Store.CooldownUntil(cfg.Name)
	if err != nil {
		return "", err
	}

	live := map[int]store.Dispatch{}
	dead := map[int]store.Dispatch{}
	for _, d := range running {
		if proc.IsAlive(d.PID, d.ID) {
			live[d.Number] = d
		} else {
			dead[d.Number] = d
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "loop %s  repo %s  ticks %d\n", cfg.Name, cfg.Repo, ticks)
	if !cooldown.IsZero() {
		fmt.Fprintf(&b, "cooldown until %s\n", cooldown.Format("2006-01-02 15:04:05Z"))
	}
	fmt.Fprintln(&b)

	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	fmt.Fprintf(&b, "%-6s %-14s %-9s %-38s %s\n",
		"ISSUE", "STATE", "RETRIES", "SESSION", "WORKTREE")

	for _, iss := range issues {
		state := "-"
		switch {
		case iss.HasAnyLabel(cfg.Labels.Veto):
			state = "veto"
		case iss.HasLabel(cfg.Labels.InFlight):
			state = "in-flight"
			if _, ok := dead[iss.Number]; ok {
				state = "ORPHAN"
			}
		case iss.HasLabel(cfg.Labels.Trigger):
			state = "queued"
		case iss.HasLabel(cfg.Labels.Blocked):
			state = "blocked"
		case iss.HasLabel(cfg.Labels.Review):
			state = "in-review"
		default:
			continue
		}
		s := states[iss.Number]
		session := s.SessionID
		if session == "" {
			session = "-"
		}
		wt := s.WorktreePath
		if wt == "" {
			wt = "-"
		}
		fmt.Fprintf(&b, "%-6d %-14s %-9d %-38s %s\n",
			iss.Number, state, s.RetryCount, session, wt)
	}

	fmt.Fprintf(&b, "\nlive dispatches: %d   orphaned: %d\n", len(live), len(dead))
	return b.String(), nil
}
