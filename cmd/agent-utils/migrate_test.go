package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/migrate"
)

// An empty report must say so in words. Printing a bare header with no rows
// under it reads as a machine that lost the state it was asked about.
func TestRenderMigrateReportEmptyReportSaysNothingIsLeft(t *testing.T) {
	out := renderMigrateReport(migrate.Report{}, false)

	if !strings.Contains(out, "Nothing left to import") {
		t.Fatalf("empty report does not say there is nothing to import:\n%s", out)
	}
	if strings.Contains(out, "PROJECT") {
		t.Fatalf("empty report printed a table header:\n%s", out)
	}
}

// A dry run must state both halves of the truth: nothing was imported, and the
// schema of the canonical database was upgraded anyway by opening it.
func TestRenderMigrateReportDryRunSaysWhatItStillDid(t *testing.T) {
	out := renderMigrateReport(migrate.Report{}, true)

	if !strings.Contains(out, "no state was imported") {
		t.Fatalf("dry run does not say nothing was imported:\n%s", out)
	}
	if !strings.Contains(out, "schema up to date") {
		t.Fatalf("dry run does not admit to the schema upgrade:\n%s", out)
	}
}

// Both outcomes have to survive the same render: a failure that is only counted
// in the totals leaves the operator with state they cannot find.
func TestRenderMigrateReportShowsImportedAndFailedSources(t *testing.T) {
	report := migrate.Report{Results: []migrate.Result{
		{
			Source: migrate.Source{
				Path:        "/home/dev/web/.agent-utils/state/planning/state.db",
				ProjectID:   "8f14e45f-ceea-467a-9d24-2b5f6f2e0000",
				ProjectName: "web",
				Loop:        "planning",
				Repo:        "acme/web",
			},
			State: migrate.StateImported,
			Rows:  42,
		},
		{
			Source: migrate.Source{
				Path:        "/home/dev/api/.agent-utils/state/execution/state.db",
				ProjectID:   "c9f0f895-fb98-4b47-9a6f-1e4a1d2f0000",
				ProjectName: "api",
				Loop:        "execution",
			},
			State:  migrate.StateFailed,
			Reason: "open the legacy database: file is not a database",
			Err:    errors.New("file is not a database"),
		},
	}}

	out := renderMigrateReport(report, false)

	for _, want := range []string{
		"PROJECT", "LOOP", "STATE", "ROWS", "SOURCE",
		"web", "planning", migrate.StateImported, "42",
		"/home/dev/web/.agent-utils/state/planning/state.db",
		"api", "execution", migrate.StateFailed,
		"/home/dev/api/.agent-utils/state/execution/state.db",
		"2 source(s); 42 row(s) imported.",
		"open the legacy database: file is not a database",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}

	// The reason belongs under the table, not inside a fixed-width cell that
	// would push every following column out of line.
	table := strings.Index(out, "/home/dev/api/.agent-utils/state/execution/state.db")
	reason := strings.Index(out, "open the legacy database:")
	if table < 0 || reason < 0 || reason < table {
		t.Errorf("the reason is not printed under the table:\n%s", out)
	}
}
