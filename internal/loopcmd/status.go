package loopcmd

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// truncate shortens a string to width, marking that it was cut. Issue titles
// are arbitrary length and would otherwise break the column layout.
func truncate(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width <= 1 {
		return string(r[:width])
	}
	return string(r[:width-1]) + "\u2026"
}

// pad right-pads s to width display columns, counting runes rather than bytes.
// fmt's %-Ns pads by byte length, so a cell holding truncate's ellipsis (one
// column, three bytes) comes out two columns short and drags every column to
// its right out of alignment.
func pad(s string, width int) string {
	n := utf8.RuneCountInString(s)
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}

// fit renders a cell exactly width columns wide: truncated if too long, padded
// if too short.
func fit(s string, width int) string { return pad(truncate(s, width), width) }

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
	lastTick, err := deps.Store.LastTick(cfg.Name)
	if err != nil {
		return "", err
	}
	cost, err := deps.Store.CostByIssue(cfg.Name, cfg.Repo)
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
		if proc.IsAlive(d.PID, d.RunnerID()) {
			live[d.Number] = d
		} else {
			dead[d.Number] = d
		}
	}

	var b strings.Builder
	last := "never"
	if !lastTick.IsZero() {
		last = lastTick.Local().Format("2006-01-02 15:04:05")
	}
	fmt.Fprintf(&b, "loop %s  repo %s  ticks %d  last tick %s\n",
		cfg.Name, cfg.Repo, ticks, last)
	if !cooldown.IsZero() {
		fmt.Fprintf(&b, "cooldown until %s\n", cooldown.Format("2006-01-02 15:04:05Z"))
	}
	fmt.Fprintln(&b)

	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	fmt.Fprintf(&b, "%-6s %-44s %-14s %-9s %-9s %-38s %s\n",
		"ISSUE", "TITLE", "STATE", "RETRIES", "COST", "SESSION", "WORKTREE")

	for _, iss := range issues {
		var state string
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
		// There is no "tending" row here any more. A loop does not tend, so an
		// issue sitting at the project's tend label has left every loop's
		// states and listing it here would claim work this loop is not doing.
		// `agent-utils project loop status --name tend` is where that queue is
		// reported now; see TendStatus.
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
		if s.NeedsRetry {
			state += "!"
		}
		if s.Parked {
			state = "parked"
		}
		// stopped wins over parked: both mean the loop will not dispatch
		// this issue, but stopped is the actionable one -- an operator
		// clears it with `sessions resume`, where a parked issue waits out
		// its own cooldown.
		if s.Stopped {
			state = "stopped"
		}
		fmt.Fprintf(&b, "%-6d %-44s %-14s %-9d %-9s %-38s %s\n",
			iss.Number, truncate(iss.Title, 44), state, s.RetryCount,
			fmt.Sprintf("$%.2f", cost[iss.Number]), session, wt)
	}

	fmt.Fprintf(&b, "\nlive dispatches: %d   orphaned: %d\n", len(live), len(dead))

	// Built from states directly, NOT from the render loop above: that
	// loop's `default: continue` skips any issue carrying none of the
	// recognised labels, which would drop a stopped issue (and its reason)
	// from the report entirely if this list were built the same way. No
	// table column fits a full sentence, and the reason is the whole point
	// of the flag -- an operator who sees only `stopped` cannot learn why.
	var stoppedNumbers []int
	for number, s := range states {
		if s.Stopped {
			stoppedNumbers = append(stoppedNumbers, number)
		}
	}
	if len(stoppedNumbers) > 0 {
		sort.Ints(stoppedNumbers)
		fmt.Fprintln(&b, "\nstopped issues:")
		for _, number := range stoppedNumbers {
			reason := states[number].StoppedReason
			if reason == "" {
				reason = "(no reason recorded)"
			}
			fmt.Fprintf(&b, "  #%d: %s\n", number, reason)
		}
		fmt.Fprintln(&b, "clear with: agent-utils sessions resume")
	}

	return b.String(), nil
}
