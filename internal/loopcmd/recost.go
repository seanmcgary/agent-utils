package loopcmd

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/runner"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// Settlement is the reconciled cost of one dispatch.
type Settlement struct {
	Dispatch store.Dispatch
	Cost     float64
	Source   string
}

// SettleReport is what one cost reconcile pass found.
type SettleReport struct {
	Settled []Settlement
}

// Changed returns the settlements whose cost differs from the recorded one.
func (r SettleReport) Changed() []Settlement {
	var out []Settlement
	for _, s := range r.Settled {
		if s.Cost != s.Dispatch.CostUSD {
			out = append(out, s)
		}
	}
	return out
}

// Unverified counts the dispatches whose cost could not be checked.
func (r SettleReport) Unverified() int {
	n := 0
	for _, s := range r.Settled {
		if s.Source == runner.CostSourceUnverified {
			n++
		}
	}
	return n
}

// SettleCosts reconciles the cost of every finished dispatch whose cost_source
// is still empty, across every project, from the run logs.
//
// Before cost_source existed, a dispatch's cost was its stream's
// total_cost_usd, and newer claude versions report that as the SESSION's
// running total. Every resume re-counted every earlier run, so a session's
// summed cost grew with the square of its resumes. This pass re-reads each
// run's log with the parser new runs use, and records what that run alone
// spent. See runner.CostSourceRun.
//
// It runs whenever the state database is opened, like the legacy import, so
// an upgrade corrects the history on its first tick with nothing for an
// operator to run. After that its query returns nothing. A row whose log
// cannot be read keeps its recorded cost and is marked unverified, so it is
// not retried on every open.
//
// With dryRun nothing is written.
func SettleCosts(db *store.DB, dryRun bool) (SettleReport, error) {
	rows, err := db.UnsettledCosts()
	if err != nil {
		return SettleReport{}, err
	}
	var report SettleReport
	for _, d := range rows {
		cost, source := settle(d)
		report.Settled = append(report.Settled, Settlement{Dispatch: d, Cost: cost, Source: source})
		if dryRun {
			continue
		}
		if err := db.SettleCost(d.ID, cost, source); err != nil {
			return report, err
		}
	}
	return report, nil
}

// settleCostsOnOpen runs SettleCosts and logs the outcome. A failure is logged,
// never returned: costs are a report, and a tick must not stop over one.
func settleCostsOnOpen(db *store.DB) {
	report, err := SettleCosts(db, false)
	if err != nil {
		slog.Error("reconcile recorded dispatch costs", "err", err)
		return
	}
	if len(report.Settled) == 0 {
		return
	}
	var before, after float64
	for _, s := range report.Settled {
		before += s.Dispatch.CostUSD
		after += s.Cost
	}
	slog.Info("reconciled recorded dispatch costs",
		"dispatches", len(report.Settled), "changed", len(report.Changed()),
		"unverified", report.Unverified(),
		"before_usd", fmt.Sprintf("%.2f", before), "after_usd", fmt.Sprintf("%.2f", after))
}

// settle reads one dispatch's log: as claude's stream first, then as pi's.
// Only a claude result line can change the recorded cost. pi has always
// summed its own per-message costs, and a run with no result line recorded
// none.
func settle(d store.Dispatch) (float64, string) {
	parse := func(fn func(io.Reader) (runner.Result, error)) (runner.Result, error) {
		f, err := os.Open(d.LogPath)
		if err != nil {
			return runner.Result{}, err
		}
		defer f.Close()
		return fn(f)
	}
	if strings.TrimSpace(d.LogPath) == "" {
		return d.CostUSD, runner.CostSourceUnverified
	}
	r, err := parse(runner.ParseStream)
	switch {
	case err == nil:
		return r.CostUSD, r.CostSource
	case !errors.Is(err, runner.ErrNoResult):
		return d.CostUSD, runner.CostSourceUnverified
	}
	if _, err := parse(runner.ParsePiStream); err == nil {
		return d.CostUSD, runner.CostSourcePi
	}
	return d.CostUSD, runner.CostSourceNone
}

// RenderSettleReport prints one row per session the pass changed, then totals.
func RenderSettleReport(report SettleReport, dryRun bool) string {
	var b strings.Builder
	if dryRun {
		fmt.Fprintf(&b, "Dry run: no cost was written.\n\n")
	}
	if len(report.Settled) == 0 {
		fmt.Fprintf(&b, "Every recorded dispatch cost is already settled.\n")
		return b.String()
	}

	type sessionSum struct {
		loop          string
		issue, runs   int
		before, after float64
	}
	sums := map[string]*sessionSum{}
	var order []string
	var before, after float64
	for _, s := range report.Changed() {
		key := s.Dispatch.SessionID
		sum, ok := sums[key]
		if !ok {
			sum = &sessionSum{loop: s.Dispatch.Loop, issue: s.Dispatch.Number}
			sums[key] = sum
			order = append(order, key)
		}
		sum.runs++
		sum.before += s.Dispatch.CostUSD
		sum.after += s.Cost
	}
	for _, s := range report.Settled {
		before += s.Dispatch.CostUSD
		after += s.Cost
	}

	if len(order) > 0 {
		sort.Slice(order, func(i, j int) bool {
			return sums[order[i]].before-sums[order[i]].after > sums[order[j]].before-sums[order[j]].after
		})
		fmt.Fprintf(&b, "%-10s %-16s %-6s %-5s %12s %12s\n", "SESSION", "LOOP", "ISSUE", "RUNS", "BEFORE", "AFTER")
		for _, key := range order {
			s := sums[key]
			fmt.Fprintf(&b, "%-10s %-16s %-6d %-5d %12s %12s\n",
				truncate(orDash(key), 8), truncate(orDash(s.loop), 16), s.issue, s.runs,
				fmt.Sprintf("$%.2f", s.before), fmt.Sprintf("$%.2f", s.after))
		}
		fmt.Fprintln(&b)
	}

	verb := "Settled"
	if dryRun {
		verb = "Would settle"
	}
	fmt.Fprintf(&b, "%s %d dispatches: %d changed, $%.2f recorded before, $%.2f after.\n",
		verb, len(report.Settled), len(report.Changed()), before, after)
	if n := report.Unverified(); n > 0 {
		fmt.Fprintf(&b, "%d could not be checked because a run log is missing; their recorded cost is kept.\n", n)
	}
	return b.String()
}
