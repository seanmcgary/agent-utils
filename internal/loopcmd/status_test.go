package loopcmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// TestStatusRendersStoppedWinningOverParked covers: "render `stopped` in the
// state column beside `parked` ... winning over it." An issue marked both
// Parked and Stopped must show `stopped`, not `parked`: the retry budget is
// exhausted either way, but `stopped` is the operator-actionable fact.
func TestStatusRendersStoppedWinningOverParked(t *testing.T) {
	cfg := tickConfig(t)
	st, err := store.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	scoped := st.Project(testProject)

	gh := &fakeGH{issues: []ghub.Issue{
		{Number: 1, Title: "exhausted retries", Labels: []string{cfg.Labels.InFlight}},
	}}

	now := time.Now()
	if err := scoped.PutIssueState(store.IssueState{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, Parked: true, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := scoped.MarkStopped(cfg.Name, cfg.Repo, 1, "killed by operator", now); err != nil {
		t.Fatal(err)
	}

	out, err := Status(context.Background(), cfg, Deps{Store: scoped, GH: gh})
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(out, "\n")
	var row string
	for _, l := range lines {
		if strings.HasPrefix(l, "1 ") || strings.HasPrefix(strings.TrimLeft(l, " "), "1 ") {
			row = l
			break
		}
	}
	if row == "" {
		t.Fatalf("no row for issue #1 in:\n%s", out)
	}
	if !strings.Contains(row, "stopped") {
		t.Errorf("row = %q, want it to show stopped", row)
	}
	if strings.Contains(row, "parked") {
		t.Errorf("row = %q, stopped must win over parked", row)
	}
}

// TestStatusListsStoppedReasonsIncludingUnlabeledIssue covers: "list each
// stopped issue with its reason under the table ... including one carrying
// no label state at all." The list must be built from the states map
// directly, not from the render loop above (whose default: continue would
// otherwise drop an issue with none of the recognised labels).
func TestStatusListsStoppedReasonsIncludingUnlabeledIssue(t *testing.T) {
	cfg := tickConfig(t)
	st, err := store.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	scoped := st.Project(testProject)

	// Issue #1 carries a recognised label and appears as a table row.
	gh := &fakeGH{issues: []ghub.Issue{
		{Number: 1, Title: "in flight, stopped", Labels: []string{cfg.Labels.InFlight}},
	}}

	now := time.Now()
	if err := scoped.MarkStopped(cfg.Name, cfg.Repo, 1, "bad model label", now); err != nil {
		t.Fatal(err)
	}
	// Issue #2 has no GitHub issue at all -- "no label state" in the sense
	// that matters here: nothing in the render loop's issues slice would
	// ever produce a row for it, so the below-table list is the only place
	// it can be reported.
	if err := scoped.MarkStopped(cfg.Name, cfg.Repo, 2, "harness override refused: loop sets agent.permission_mode", now); err != nil {
		t.Fatal(err)
	}

	out, err := Status(context.Background(), cfg, Deps{Store: scoped, GH: gh})
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"#1", "bad model label",
		"#2", "harness override refused: loop sets agent.permission_mode",
		"sessions resume",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
