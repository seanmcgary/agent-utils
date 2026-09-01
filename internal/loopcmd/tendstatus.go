package loopcmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// TendStatus renders the tend dispatcher's reconciled view. It changes nothing.
//
// It is a separate renderer from Status, not a mode of it, because the two
// answer different questions. Status is a table of ISSUES in a loop's own
// states -- queued, in-flight, blocked, parked -- and the tend dispatcher has
// none of those: it moves no label and keeps no issue state. What an operator
// needs here is the queue it maintains: which pull request each eligible issue
// links to, how far behind it is, when it was last tended, and whether an agent
// is in it right now.
//
// It answers the visibility half of "tending is its own dispatcher". Without
// it, moving the work out of loop ticks would have made a whole class of agent
// dispatch invisible to `loop status`, which is the command an operator reaches
// for first.
func TendStatus(ctx context.Context, cfg *config.Config, deps Deps) (string, error) {
	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	issues, err := deps.GH.ListOpenIssues(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	prs, err := deps.GH.ListOpenPullRequests(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	links, err := deps.Store.PRLinks(cfg.Name, cfg.Repo)
	if err != nil {
		return "", err
	}
	lastTend, err := deps.Store.LastTendByPR(cfg.Name, cfg.Repo)
	if err != nil {
		return "", err
	}
	running, err := deps.Store.RunningDispatchesForRepo(cfg.Repo)
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
	stopped, err := deps.Store.StoppedIssuesForRepo(cfg.Repo)
	if err != nil {
		return "", err
	}

	// Alive rows only, split the way DecideTend splits them: this report must
	// agree with the decision it is describing, or an operator reading "idle"
	// would be looking at the state that suppressed a dispatch.
	alive := make([]store.Dispatch, 0, len(running))
	for _, d := range running {
		if proc.IsAlive(d.PID, d.RunnerID()) {
			alive = append(alive, d)
		}
	}
	liveIssues, liveTendPRs := engine.TendLiveness(alive)

	var b strings.Builder
	last := "never"
	if !lastTick.IsZero() {
		last = lastTick.Local().Format("2006-01-02 15:04:05")
	}
	fmt.Fprintf(&b, "tend dispatcher  repo %s  label %s  agent %s/%s  passes %d  last pass %s\n",
		cfg.Repo, cfg.Tend.Label, cfg.Agent.Harness, cfg.Agent.Model, ticks, last)
	fmt.Fprintln(&b)

	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	fmt.Fprintf(&b, "%-6s %-6s %-44s %-9s %-20s %s\n",
		"ISSUE", "PR", "TITLE", "BEHIND", "LAST TEND", "STATE")

	rows := 0
	for _, iss := range issues {
		if !iss.HasLabel(cfg.Tend.Label) {
			continue
		}
		rows++

		prCell, behindCell, tendCell := "-", "-", "never"
		state := "idle"

		pr, linked := engine.LinkPR(iss.Number, prs)
		switch {
		case !linked:
			state = "no trusted pull request"
		case pr.Draft:
			prCell = fmt.Sprintf("#%d", pr.Number)
			state = "draft"
		default:
			prCell = fmt.Sprintf("#%d", pr.Number)
			// behind_by comes from the stored link rather than a fresh
			// comparison: this command changes nothing and calls no
			// CompareCommits, so what it can honestly report is what the last
			// pass recorded. A row that has never been written shows "-".
			if l, ok := links[iss.Number]; ok && l.PRNumber == pr.Number {
				behindCell = fmt.Sprintf("%d", l.BehindBy)
			}
			if t, ok := lastTend[pr.Number]; ok && !t.IsZero() {
				tendCell = t.Local().Format("2006-01-02 15:04")
			}
			switch {
			case liveTendPRs[pr.Number]:
				state = "TENDING"
			case liveIssues[iss.Number]:
				state = "held: an agent is working this issue"
			}
		}
		if _, ok := stopped[iss.Number]; ok {
			// Stopped wins over every other cell: it is the one state an
			// operator has to clear by hand, and reporting "idle" for an issue
			// tending will refuse to touch is the report that wastes an hour.
			// The reason does not fit a column; it is printed in full below.
			state = "stopped"
		}

		fmt.Fprintf(&b, "%-6d %-6s %-44s %-9s %-20s %s\n",
			iss.Number, prCell, truncate(iss.Title, 44), behindCell, tendCell, state)
	}
	if rows == 0 {
		fmt.Fprintf(&b, "(no open issue carries %s)\n", cfg.Tend.Label)
	}

	fmt.Fprintf(&b, "\nlive tend dispatches: %d\n", len(liveTendPRs))

	var stoppedNumbers []int
	for number := range stopped {
		stoppedNumbers = append(stoppedNumbers, number)
	}
	if len(stoppedNumbers) > 0 {
		// The reasons, in full, for the same reason Status prints them: no
		// table column fits a sentence, and an operator who sees only
		// "stopped" cannot learn why tending is declining to act.
		sort.Ints(stoppedNumbers)
		fmt.Fprintln(&b, "\nstopped issues (tending will not touch these):")
		for _, number := range stoppedNumbers {
			reason := stopped[number]
			if reason == "" {
				reason = "(no reason recorded)"
			}
			fmt.Fprintf(&b, "  #%d: %s\n", number, reason)
		}
		fmt.Fprintln(&b, "clear with: agent-utils sessions resume")
	}

	return b.String(), nil
}
