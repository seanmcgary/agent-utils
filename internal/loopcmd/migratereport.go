package loopcmd

import (
	"fmt"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/migrate"
)

// RenderMigrateReport formats a migration report for a terminal.
//
// It lives here with every other renderer, and away from the command, so the
// wording can be checked without a home directory, a registry or a legacy file
// on disk.
func RenderMigrateReport(report migrate.Report, dryRun bool) string {
	var b strings.Builder

	if dryRun {
		// Opening the canonical database applies the schema upgrade, and the
		// report cannot be produced without opening it. Say so rather than let
		// an operator believe --dry-run touched nothing at all.
		fmt.Fprintf(&b, "Dry run: no state was imported and no legacy file was touched.\n")
		fmt.Fprintf(&b, "Opening the canonical database still brought its schema up to date;\n")
		fmt.Fprintf(&b, "that part cannot be avoided.\n\n")
	}

	if len(report.Results) == 0 {
		fmt.Fprintf(&b, "Nothing left to import. Every registered project's state is already\n")
		fmt.Fprintf(&b, "in the canonical database.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "%-20s %-16s %-10s %-7s %s\n",
		"PROJECT", "LOOP", "STATE", "ROWS", "SOURCE")
	for _, res := range report.Results {
		// Truncate the two named columns. A project name wider than its column
		// would shift every column after it, and the table stops being readable.
		fmt.Fprintf(&b, "%-20s %-16s %-10s %-7d %s\n",
			truncate(orDash(res.Source.ProjectName), 20),
			truncate(orDash(res.Source.Loop), 16),
			res.State, res.Rows, orDash(res.Source.Path))
	}

	verb := "imported"
	if dryRun {
		verb = "would be imported"
	}
	fmt.Fprintf(&b, "\n%d source(s); %d row(s) %s.\n",
		len(report.Results), report.Rows(), verb)

	// A reason does not fit the table, so it goes under it, one paragraph per
	// source, the way a loop's error does in `project status`.
	for _, res := range report.Results {
		if res.Reason == "" {
			continue
		}
		fmt.Fprintf(&b, "\n%s (loop %s): %s\n",
			orDash(res.Source.Path), orDash(res.Source.Loop), res.Reason)
	}
	return b.String()
}

// orDash keeps a column filled. A discovery failure has no path and no loop, and
// an empty cell in the middle of a table reads as a rendering bug.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
