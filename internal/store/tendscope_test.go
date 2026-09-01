package store

import (
	"testing"
	"time"
)

// The tend dispatcher's two project-wide reads.
//
// Every other reader here is a loop deciding its own issues, and a loop must
// not see its neighbours' rows. Tending is the one pass whose safety question
// spans loops -- "is any agent, anywhere in this project, already working this
// issue's branch?" -- and it cannot be answered from the reserved name's own
// rows, which hold only the project's tends.
//
// Both are QUERIES over the existing key, not new state: dispatches and issues
// are already keyed (project_id, loop, repo), so dropping `loop` from the WHERE
// clause is the whole of the change. What these tests pin is that dropping it
// did not also drop the PROJECT: a dispatcher that saw another project's live
// agents would decline to tend for reasons that have nothing to do with its own
// repository.
func TestRunningDispatchesForRepoSpansLoopsButNotProjects(t *testing.T) {
	db := openDB(t)
	a, b := db.Project(testProject), db.Project(otherProject)

	now := time.Now().UTC()
	for _, d := range []Dispatch{
		{Loop: "planning", Repo: "o/r", Number: 1, Kind: KindStart, SessionID: "s1", StartedAt: now},
		{Loop: "tend", Repo: "o/r", Number: 2, PRNumber: 22, Kind: KindTend, SessionID: "s2", StartedAt: now},
		// Another repository of the same project: not this dispatcher's.
		{Loop: "planning", Repo: "o/other", Number: 3, Kind: KindStart, SessionID: "s3", StartedAt: now},
	} {
		if _, err := a.CreateDispatch(d); err != nil {
			t.Fatalf("CreateDispatch: %v", err)
		}
	}
	// Another PROJECT watching the same repository, which is a supported
	// configuration and must be invisible here.
	if _, err := b.CreateDispatch(Dispatch{
		Loop: "planning", Repo: "o/r", Number: 4, Kind: KindStart, SessionID: "s4", StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}

	rows, err := a.RunningDispatchesForRepo("o/r")
	if err != nil {
		t.Fatalf("RunningDispatchesForRepo: %v", err)
	}
	got := map[int]string{}
	for _, d := range rows {
		got[d.Number] = d.Loop
	}
	if len(got) != 2 || got[1] != "planning" || got[2] != "tend" {
		t.Errorf("rows = %v, want issue 1 from planning and issue 2 from tend", got)
	}
}

// A finished dispatch is not live and must not suppress a tend. The guard is
// about agents that are running now.
func TestRunningDispatchesForRepoIgnoresFinishedRows(t *testing.T) {
	db := openDB(t)
	s := db.Project(testProject)

	id, err := s.CreateDispatch(Dispatch{
		Loop: "planning", Repo: "o/r", Number: 1, Kind: KindStart,
		SessionID: "s1", StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}
	if err := s.FinishDispatch(id, DispatchResult{Status: StatusSucceeded}); err != nil {
		t.Fatalf("FinishDispatch: %v", err)
	}

	rows, err := s.RunningDispatchesForRepo("o/r")
	if err != nil {
		t.Fatalf("RunningDispatchesForRepo: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("rows = %+v, want none: a finished dispatch is not live", rows)
	}
}

// An operator who stopped an issue in ANY loop meant "run no more agents at
// this issue", and a tend is one of that issue's agents. The read spans loops
// for that reason, and stops at the project for the reason above.
func TestStoppedIssuesForRepoSpansLoopsButNotProjects(t *testing.T) {
	db := openDB(t)
	a, b := db.Project(testProject), db.Project(otherProject)

	now := time.Now().UTC()
	if err := a.MarkStopped("execution", "o/r", 7, "killed by the operator", now); err != nil {
		t.Fatalf("MarkStopped: %v", err)
	}
	if err := a.MarkStopped("planning", "o/other", 8, "another repository", now); err != nil {
		t.Fatalf("MarkStopped: %v", err)
	}
	if err := b.MarkStopped("planning", "o/r", 9, "another project", now); err != nil {
		t.Fatalf("MarkStopped: %v", err)
	}

	got, err := a.StoppedIssuesForRepo("o/r")
	if err != nil {
		t.Fatalf("StoppedIssuesForRepo: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("stopped = %v, want exactly issue 7", got)
	}
	// The REASON comes back, not just the flag: no table column fits a
	// sentence, and an operator who sees only "stopped" cannot learn why
	// tending is declining to act.
	if got[7] != "killed by the operator" {
		t.Errorf("reason = %q, want the recorded one", got[7])
	}

	// Clearing it in the loop that stopped it clears it for tending too.
	if _, err := a.ClearStopped("execution", "o/r", 7, now); err != nil {
		t.Fatalf("ClearStopped: %v", err)
	}
	got, err = a.StoppedIssuesForRepo("o/r")
	if err != nil {
		t.Fatalf("StoppedIssuesForRepo: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("stopped = %v, want none after the flag was cleared", got)
	}
}
