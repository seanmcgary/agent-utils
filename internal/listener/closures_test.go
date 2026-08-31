package listener

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/store"
)

// closures returns the {repo, number} of every recorded closure, so a test can
// assert on what a pass wrote without repeating the read.
func closureNumbers(t *testing.T, db *store.DB) []int {
	t.Helper()
	all, err := db.Closures()
	if err != nil {
		t.Fatalf("Closures: %v", err)
	}
	out := make([]int, 0, len(all))
	for _, c := range all {
		out = append(out, c.Number)
	}
	return out
}

func seedDispatch(t *testing.T, db *store.DB, projectID, loop string, number int) {
	t.Helper()
	if _, err := db.Project(projectID).CreateDispatch(store.Dispatch{
		Loop: loop, Repo: "o/r", Number: number, Kind: store.KindStart,
	}); err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}
}

// A close delivery is what keeps the report current while the daemon runs.
func TestDeliveryRecordsAClose(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	h.targets = []Target{h.target("planning")}

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 11, ClosedIssue: true})

	if got := closureNumbers(t, db); len(got) != 1 || got[0] != 11 {
		t.Fatalf("closures = %v, want the closed issue 11", got)
	}
}

// A closed pull request is recorded too. Issues and pull requests share one
// number space, and a pr-review loop's sessions are keyed by pull request
// number.
func TestDeliveryRecordsAClosedPullRequest(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	h.targets = []Target{h.target("pr-review")}

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 12, ClosedPR: true})

	if got := closureNumbers(t, db); len(got) != 1 || got[0] != 12 {
		t.Fatalf("closures = %v, want the closed pull request 12", got)
	}
}

// A reopen erases the closure, so the issue's sessions come back into the
// report rather than staying hidden forever.
func TestDeliveryClearsAClosureOnReopen(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	h.targets = []Target{h.target("planning")}
	ctx := context.Background()

	h.w.Deliver(ctx, Delivery{Repo: "o/r", Number: 11, ClosedIssue: true})
	h.w.Deliver(ctx, Delivery{Repo: "o/r", Number: 11, Reopened: true})

	if got := closureNumbers(t, db); len(got) != 0 {
		t.Fatalf("closures = %v, want none after the reopen", got)
	}
}

// Every other delivery writes nothing. A label change, a comment and a push
// say nothing about whether the issue is closed.
func TestAnOrdinaryDeliveryRecordsNoClosure(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	h.targets = []Target{h.target("planning")}
	h.defaultBranch = "master"
	h.tendPR = true
	ctx := context.Background()

	h.w.Deliver(ctx, Delivery{Repo: "o/r", Number: 11})
	h.w.Deliver(ctx, Delivery{Repo: "o/r", PushedTo: "master"})

	if got := closureNumbers(t, db); len(got) != 0 {
		t.Fatalf("closures = %v, want none", got)
	}
}

// The closure key carries the project, so a repository watched by two projects
// records one row for each. They are separate reports over separate state.
func TestACloseIsRecordedForEveryProjectWatchingTheRepository(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	other := h.target("planning")
	other.ProjectID = "22222222-2222-2222-2222-222222222222"
	other.ProjectName = "other"
	// Two loops of the harness's own project as well: the key has no loop, so
	// they must collapse onto one row rather than two.
	h.targets = []Target{h.target("planning"), h.target("execution"), other}

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 11, ClosedIssue: true})

	all, err := db.Closures()
	if err != nil {
		t.Fatalf("Closures: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("closures = %+v, want one per project", all)
	}
}

// The reconcile is the other writer: it covers everything that closed while
// the daemon was not running, which no delivery will ever carry.
func TestReconcileClosedMarksWhatIsNoLongerOpen(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	for _, n := range []int{1, 2, 3, 4} {
		seedDispatch(t, db, workProject, "planning", n)
	}
	// 1 is still an open issue and 3 is still an open pull request. 2 and 4
	// are gone from both lists.
	h.gh.openIssues = []int{1}
	h.gh.openPRs = []int{3}

	h.w.reconcileClosed(context.Background())

	got := closureNumbers(t, db)
	if len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Fatalf("closures = %v, want exactly 2 and 4", got)
	}
	if calls := h.gh.listCalls(); len(calls) != 1 || calls[0] != "o/r" {
		t.Errorf("list calls = %v, want one per repository", calls)
	}
}

// An open pull request must survive the pass. The issues endpoint drops pull
// requests, so a reconcile that consulted it alone would close every open pull
// request on the machine at every restart.
func TestReconcileClosedKeepsOpenPullRequests(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	seedDispatch(t, db, workProject, "pr-review", 7)
	h.gh.openPRs = []int{7}

	h.w.reconcileClosed(context.Background())

	if got := closureNumbers(t, db); len(got) != 0 {
		t.Fatalf("closures = %v, want none: pull request 7 is open", got)
	}
}

// One repository that cannot be listed must mark nothing, rather than marking
// everything closed on an empty answer.
func TestReconcileClosedSkipsARepositoryItCannotList(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	seedDispatch(t, db, workProject, "planning", 1)
	h.gh.listErr = errors.New("403 rate limited")

	h.w.reconcileClosed(context.Background())

	if got := closureNumbers(t, db); len(got) != 0 {
		t.Fatalf("closures = %v, want none: the repository could not be read", got)
	}
}

// A token that cannot be read stops the pass before it asks anything, exactly
// as it does in every other pass.
func TestReconcileClosedStopsWithoutAToken(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	seedDispatch(t, db, workProject, "planning", 1)
	h.tokenErr = errors.New("mode 0644 grants group access")

	h.w.reconcileClosed(context.Background())

	if calls := h.gh.listCalls(); len(calls) != 0 {
		t.Errorf("list calls = %v, want none without a token", calls)
	}
	if got := closureNumbers(t, db); len(got) != 0 {
		t.Fatalf("closures = %v, want none", got)
	}
}

// A machine with nothing dispatched costs nothing: no token read, no call.
func TestReconcileClosedCostsNothingWithNoHistory(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)

	h.w.reconcileClosed(context.Background())

	if h.tokenCalls != 0 {
		t.Errorf("token reads = %d, want 0 with nothing to check", h.tokenCalls)
	}
}

// An issue already known to be closed is not re-checked. That is what keeps a
// restart from paying for the machine's whole history.
func TestReconcileClosedAsksOnlyAboutIssuesBelievedOpen(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	seedDispatch(t, db, workProject, "planning", 1)
	if err := db.Project(workProject).MarkClosed("o/r", 1, time.Now()); err != nil {
		t.Fatalf("MarkClosed: %v", err)
	}

	h.w.reconcileClosed(context.Background())

	if h.tokenCalls != 0 {
		t.Errorf("token reads = %d, want 0: every issue is already closed", h.tokenCalls)
	}
}

// The pass must not outlive a shutdown, for the reason the orphan sweep's own
// cancellation check exists: it makes one round trip per repository.
func TestReconcileClosedStopsWhenTheContextIsCancelled(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	seedDispatch(t, db, workProject, "planning", 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h.w.reconcileClosed(ctx)

	if calls := h.gh.listCalls(); len(calls) != 0 {
		t.Errorf("list calls = %v, want none after cancellation", calls)
	}
}

// Serve runs it at start, beside the orphan sweep: a restart is the moment the
// daemon learns what closed while it was down.
func TestServeReconcilesClosuresAtStart(t *testing.T) {
	db := openWorkDB(t)
	h := newHarness(db)
	seedDispatch(t, db, workProject, "planning", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go h.w.Serve(ctx)

	deadline := time.After(5 * time.Second)
	for {
		if len(closureNumbers(t, db)) == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("Serve did not reconcile closures at start")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
