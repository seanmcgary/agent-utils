package store

import (
	"errors"
	"testing"
	"time"
)

// The dispatch row is a CLAIM on the issue, and this is the property that makes
// it one.
//
// A loop agent and a tend agent must never be in one branch at once: both
// rebase, both commit, and both force-push. Two independent passes decide that,
// under two different flocks (<loop>.lock and tend.lock), so they run
// concurrently by design -- each can read "nothing live on the other side" and
// then insert. The decision-time checks narrow that window; only the insert
// itself can close it, which is why the test is written against the store and
// not against a pass.
//
// The two directions are asserted separately because the failure is silent in
// whichever one is missing: a tend that force-pushes under a live loop agent and
// a loop agent that starts under a live tend are the same incident reached from
// opposite ends.
func TestCreateDispatchRefusesATendWhileALoopAgentHoldsTheIssue(t *testing.T) {
	db := openDB(t)
	st := db.Project(testProject)
	now := time.Now().UTC()

	if _, err := st.CreateDispatch(Dispatch{
		Loop: "execution", Repo: "o/r", Number: 7, Kind: KindStart,
		SessionID: "s1", StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}

	_, err := st.CreateDispatch(Dispatch{
		Loop: "tend", Repo: "o/r", Number: 7, PRNumber: 70, Kind: KindTend,
		SessionID: "s2", StartedAt: now,
	})
	if !errors.Is(err, ErrDispatchClaimed) {
		t.Fatalf("CreateDispatch for a tend under a live loop agent: err = %v, want ErrDispatchClaimed", err)
	}
	assertRunningCount(t, st, "o/r", 7, 1)
}

func TestCreateDispatchRefusesALoopAgentWhileATendHoldsTheIssue(t *testing.T) {
	db := openDB(t)
	st := db.Project(testProject)
	now := time.Now().UTC()

	if _, err := st.CreateDispatch(Dispatch{
		Loop: "tend", Repo: "o/r", Number: 7, PRNumber: 70, Kind: KindTend,
		SessionID: "s1", StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}

	_, err := st.CreateDispatch(Dispatch{
		Loop: "execution", Repo: "o/r", Number: 7, Kind: KindStart,
		SessionID: "s2", StartedAt: now,
	})
	if !errors.Is(err, ErrDispatchClaimed) {
		t.Fatalf("CreateDispatch for a loop agent under a live tend: err = %v, want ErrDispatchClaimed", err)
	}
	assertRunningCount(t, st, "o/r", 7, 1)
}

// The claim releases when the holder finishes, and it is scoped to the issue,
// the repository and the project. Without this the guard would read as "one
// dispatch per issue, ever", which is not what it says.
func TestCreateDispatchAdmitsATendOnceTheLoopAgentIsFinished(t *testing.T) {
	db := openDB(t)
	st := db.Project(testProject)
	now := time.Now().UTC()

	id, err := st.CreateDispatch(Dispatch{
		Loop: "execution", Repo: "o/r", Number: 7, Kind: KindStart,
		SessionID: "s1", StartedAt: now,
	})
	if err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}
	if err := st.FinishDispatch(id, DispatchResult{Status: StatusSucceeded}); err != nil {
		t.Fatalf("FinishDispatch: %v", err)
	}

	if _, err := st.CreateDispatch(Dispatch{
		Loop: "tend", Repo: "o/r", Number: 7, PRNumber: 70, Kind: KindTend,
		SessionID: "s2", StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateDispatch after the loop agent finished: %v", err)
	}
	assertRunningCount(t, st, "o/r", 7, 1)
}

// A live agent elsewhere is not a reason to refuse: another issue, another
// repository of the same project, and another project watching the same
// repository are all separate branches.
func TestCreateDispatchClaimsOnlyItsOwnIssueRepoAndProject(t *testing.T) {
	db := openDB(t)
	a, b := db.Project(testProject), db.Project(otherProject)
	now := time.Now().UTC()

	if _, err := a.CreateDispatch(Dispatch{
		Loop: "execution", Repo: "o/r", Number: 7, Kind: KindStart,
		SessionID: "s1", StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}

	for _, d := range []Dispatch{
		// Another issue of the same loop.
		{Loop: "tend", Repo: "o/r", Number: 8, PRNumber: 80, Kind: KindTend,
			SessionID: "s2", StartedAt: now},
		// The same issue number in another repository this project watches.
		{Loop: "tend", Repo: "o/other", Number: 7, PRNumber: 70, Kind: KindTend,
			SessionID: "s3", StartedAt: now},
	} {
		if _, err := a.CreateDispatch(d); err != nil {
			t.Fatalf("CreateDispatch(%+v): %v", d, err)
		}
	}
	// Another project watching the same repository is a supported
	// configuration and holds its own rows.
	if _, err := b.CreateDispatch(Dispatch{
		Loop: "tend", Repo: "o/r", Number: 7, PRNumber: 70, Kind: KindTend,
		SessionID: "s4", StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateDispatch for another project: %v", err)
	}
}

// Two rows on the SAME side of the divide are still admitted. Their own pass
// governs them, under a lock that serialises it with itself, and refusing them
// here would refuse legitimate work -- two loops of a pipeline can hold one
// issue -- for a hazard already held shut somewhere with more context.
func TestCreateDispatchDoesNotClaimAcrossTwoLoops(t *testing.T) {
	db := openDB(t)
	st := db.Project(testProject)
	now := time.Now().UTC()

	for _, d := range []Dispatch{
		{Loop: "planning", Repo: "o/r", Number: 7, Kind: KindStart, SessionID: "s1", StartedAt: now},
		{Loop: "execution", Repo: "o/r", Number: 7, Kind: KindStart, SessionID: "s2", StartedAt: now},
	} {
		if _, err := st.CreateDispatch(d); err != nil {
			t.Fatalf("CreateDispatch(%s): %v", d.Loop, err)
		}
	}
	assertRunningCount(t, st, "o/r", 7, 2)
}

// A refused claim must insert NOTHING. The insert and the test are one
// statement, so a partial write is not reachable -- but the identifier the
// caller gets back is: LastInsertId on a statement that inserted no row hands
// back whatever this connection inserted last, which is somebody else's live
// row, and the caller would go on to finish it.
func TestCreateDispatchReturnsNoIdentifierWhenItRefuses(t *testing.T) {
	db := openDB(t)
	st := db.Project(testProject)
	now := time.Now().UTC()

	if _, err := st.CreateDispatch(Dispatch{
		Loop: "execution", Repo: "o/r", Number: 7, Kind: KindStart,
		SessionID: "s1", StartedAt: now,
	}); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}
	id, err := st.CreateDispatch(Dispatch{
		Loop: "tend", Repo: "o/r", Number: 7, PRNumber: 70, Kind: KindTend,
		SessionID: "s2", StartedAt: now,
	})
	if err == nil {
		t.Fatal("CreateDispatch: want a refusal")
	}
	if id != 0 {
		t.Errorf("id = %d, want 0 for a refused claim", id)
	}
}

func assertRunningCount(t *testing.T, st *Store, repo string, number, want int) {
	t.Helper()
	rows, err := st.RunningDispatchesForRepo(repo)
	if err != nil {
		t.Fatalf("RunningDispatchesForRepo: %v", err)
	}
	got := 0
	for _, d := range rows {
		if d.Number == number {
			got++
		}
	}
	if got != want {
		t.Errorf("running dispatches for issue %d = %d, want %d", number, got, want)
	}
}
