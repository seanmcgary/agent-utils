package listener

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// fakeSource is a PollSource that answers from tables and counts what it was
// asked. The counts are the point of two of the tests below: a pass that reads
// more than it must is the failure this whole design is shaped to avoid.
type fakeSource struct {
	subjects map[string][]ghub.Subject // keyed "owner/repo"
	heads    map[string]string
	prs      map[int]ghub.PullRequest
	err      error

	listed  int
	headed  int
	fetched []int
	since   time.Time
}

func (f *fakeSource) ListSubjectsUpdatedSince(
	_ context.Context, owner, repo string, since time.Time,
) ([]ghub.Subject, error) {
	f.listed++
	f.since = since
	if f.err != nil {
		return nil, f.err
	}
	return f.subjects[owner+"/"+repo], nil
}

func (f *fakeSource) BranchHead(_ context.Context, owner, repo, branch string) (string, error) {
	f.headed++
	return f.heads[owner+"/"+repo+"@"+branch], nil
}

func (f *fakeSource) PullRequest(
	_ context.Context, _, _ string, number int,
) (ghub.PullRequest, error) {
	f.fetched = append(f.fetched, number)
	return f.prs[number], nil
}

// pollHarness wires a Worker whose deliveries are recorded instead of acted
// on, over a real temporary database.
type pollHarness struct {
	w   *Worker
	db  *store.DB
	src *fakeSource
	got []Delivery
}

func newPollHarness(t *testing.T, src *fakeSource, targets []Target) *pollHarness {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	h := &pollHarness{db: db, src: src}
	h.w = NewWorker(db)
	h.w.Token = func() (string, error) { return "t", nil }
	h.w.NewPollSource = func(string) PollSource { return src }
	h.w.ScanTargets = func() (Routes, error) { return Routes{Targets: targets}, nil }
	h.w.deliver = func(_ context.Context, d Delivery) { h.got = append(h.got, d) }
	return h
}

func repoTarget() Target {
	return Target{
		ProjectID: "p1", ProjectName: "proj", LoopName: "planning",
		Repo: "o/r", DefaultBranch: "master",
	}
}

func issueSubject(n int, state string, labels []string, h int) ghub.Subject {
	return ghub.Subject{Number: n, State: state, Labels: labels, UpdatedAt: at(h)}
}

// A fresh install must not dispatch an agent for every issue in a
// repository's history. The first pass establishes what is true; only changes
// after that point are work.
func TestFirstPassSeedsAndDeliversNothing(t *testing.T) {
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{
			"o/r": {issueSubject(51, "open", []string{"status:ready"}, 10)},
		},
		heads: map[string]string{"o/r@master": "sha1"},
	}
	h := newPollHarness(t, src, []Target{repoTarget()})

	h.w.pollPass(context.Background())

	if len(h.got) != 0 {
		t.Fatalf("seeded pass delivered %+v, want nothing", h.got)
	}
	snap, err := h.db.PollSnapshot("o/r")
	if err != nil {
		t.Fatalf("PollSnapshot: %v", err)
	}
	if _, ok := snap[51]; !ok {
		t.Fatalf("snapshot = %+v, want issue 51 seeded", snap)
	}
	if _, ok, _ := h.db.PollCursor("o/r"); !ok {
		t.Fatal("no cursor written; the next pass would seed again")
	}
}

// The second pass is where work starts, and the flags must survive the trip
// from the diff to the runtime.
func TestSecondPassDeliversTheChange(t *testing.T) {
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{
			"o/r": {issueSubject(51, "open", []string{"status:ready"}, 10)},
		},
		heads: map[string]string{"o/r@master": "sha1"},
	}
	h := newPollHarness(t, src, []Target{repoTarget()})
	h.w.pollPass(context.Background())

	src.subjects["o/r"] = []ghub.Subject{issueSubject(51, "closed", []string{"status:ready"}, 11)}
	h.w.pollPass(context.Background())

	if len(h.got) != 1 {
		t.Fatalf("deliveries = %+v, want exactly one", h.got)
	}
	want := Delivery{Repo: "o/r", Number: 51, ClosedIssue: true}
	if h.got[0] != want {
		t.Errorf("delivery = %+v, want %+v", h.got[0], want)
	}
}

// The cursor is used as an inclusive lower bound, so the boundary subject is
// re-read on every pass. Re-reading must be free.
func TestRereadingTheBoundaryDeliversNothing(t *testing.T) {
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{
			"o/r": {issueSubject(51, "open", []string{"status:ready"}, 10)},
		},
		heads: map[string]string{"o/r@master": "sha1"},
	}
	h := newPollHarness(t, src, []Target{repoTarget()})
	h.w.pollPass(context.Background())
	h.w.pollPass(context.Background())
	h.w.pollPass(context.Background())

	if len(h.got) != 0 {
		t.Fatalf("re-reads delivered %+v, want nothing", h.got)
	}
	if !src.since.Equal(at(10)) {
		t.Errorf("since = %v, want the boundary %v re-read", src.since, at(10))
	}
}

// A merge moves the default branch too. Both arm the same sweep, and arming it
// twice would dispatch two tend agents for one merge.
func TestAMergeAndItsPushArmOneTend(t *testing.T) {
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{
			"o/r": {{Number: 52, IsPullRequest: true, State: "open", UpdatedAt: at(10)}},
		},
		heads: map[string]string{"o/r@master": "sha1"},
		prs:   map[int]ghub.PullRequest{52: {Number: 52, State: "closed", Merged: true, BaseRef: "master"}},
	}
	h := newPollHarness(t, src, []Target{repoTarget()})
	h.w.pollPass(context.Background())

	src.subjects["o/r"] = []ghub.Subject{{Number: 52, IsPullRequest: true, State: "closed", UpdatedAt: at(11)}}
	src.heads["o/r@master"] = "sha2"
	h.w.pollPass(context.Background())

	if len(h.got) != 1 {
		t.Fatalf("deliveries = %+v, want one (the merge), not a push beside it", h.got)
	}
	if h.got[0].MergedInto != "master" || !h.got[0].ClosedPR {
		t.Errorf("delivery = %+v, want the merge of 52 into master", h.got[0])
	}
	if len(src.fetched) != 1 || src.fetched[0] != 52 {
		t.Errorf("pull request fetches = %v, want exactly [52]", src.fetched)
	}
}

// Somebody pushing straight to the base branch makes every open pull request
// stale and names none of them. It is the one event with no number.
func TestAnUnexplainedBranchMoveDeliversAPush(t *testing.T) {
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{"o/r": nil},
		heads:    map[string]string{"o/r@master": "sha1"},
	}
	h := newPollHarness(t, src, []Target{repoTarget()})
	h.w.pollPass(context.Background())

	src.heads["o/r@master"] = "sha2"
	h.w.pollPass(context.Background())

	if len(h.got) != 1 {
		t.Fatalf("deliveries = %+v, want one push", h.got)
	}
	if h.got[0].PushedTo != "master" || h.got[0].Number != 0 {
		t.Errorf("delivery = %+v, want a numberless push to master", h.got[0])
	}
}

// One unreachable repository must not stop the others, and must not silently
// lose the window it covered.
func TestAFailingRepositoryDoesNotAdvanceItsCursorOrStopTheNext(t *testing.T) {
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{
			"o/r":  {issueSubject(51, "open", nil, 10)},
			"o/r2": {issueSubject(9, "open", nil, 10)},
		},
		heads: map[string]string{"o/r@master": "sha1", "o/r2@master": "sha1"},
	}
	second := repoTarget()
	second.Repo = "o/r2"
	h := newPollHarness(t, src, []Target{repoTarget(), second})
	h.w.pollPass(context.Background()) // seed both

	before, ok, err := h.db.PollCursor("o/r")
	if err != nil || !ok {
		t.Fatalf("PollCursor after seeding: %+v ok=%v err=%v", before, ok, err)
	}

	src.err = errors.New("github is down")
	h.w.pollPass(context.Background())
	src.err = nil

	after, ok, err := h.db.PollCursor("o/r")
	if err != nil || !ok {
		t.Fatalf("the seeded cursor vanished: %+v ok=%v err=%v", after, ok, err)
	}
	// The whole cursor, not merely its presence: a failing pass that zeroed
	// Since, wiped HeadSHA, or moved SeededAt would still pass an existence
	// check while losing exactly the window this test exists to protect.
	if !after.Since.Equal(before.Since) || after.HeadSHA != before.HeadSHA || !after.SeededAt.Equal(before.SeededAt) {
		t.Errorf("cursor after the failing pass = %+v, want unchanged from %+v", after, before)
	}
	// Both repositories were attempted: two listings on the failing pass.
	if src.listed != 4 {
		t.Errorf("listings = %d, want 4 (two repositories, two passes)", src.listed)
	}
}

// A pull request merged into a branch OTHER than the default must not
// suppress a genuine push to the default branch: the two events are
// unrelated, and swallowing the push silently drops a tend sweep a human
// push into the default branch is owed. This pins the bug where `merged`
// was set for ANY merge regardless of which branch it landed on.
func TestAMergeIntoAnotherBranchDoesNotSuppressTheDefaultBranchPush(t *testing.T) {
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{
			"o/r": {{Number: 52, IsPullRequest: true, State: "open", UpdatedAt: at(10)}},
		},
		heads: map[string]string{"o/r@master": "sha1"},
		prs:   map[int]ghub.PullRequest{52: {Number: 52, State: "closed", Merged: true, BaseRef: "release/1.x"}},
	}
	h := newPollHarness(t, src, []Target{repoTarget()})
	h.w.pollPass(context.Background())

	src.subjects["o/r"] = []ghub.Subject{{Number: 52, IsPullRequest: true, State: "closed", UpdatedAt: at(11)}}
	src.heads["o/r@master"] = "sha2" // an unrelated, genuine push to master
	h.w.pollPass(context.Background())

	if len(h.got) != 2 {
		t.Fatalf("deliveries = %+v, want two: the merge into release/1.x AND the push to master", h.got)
	}
	var sawMerge, sawPush bool
	for _, d := range h.got {
		if d.MergedInto == "release/1.x" {
			sawMerge = true
		}
		if d.PushedTo == "master" {
			sawPush = true
		}
	}
	if !sawMerge || !sawPush {
		t.Errorf("deliveries = %+v, want both the merge and the push", h.got)
	}
	// BranchHead is asked once per pass: the fake's counts are the point of
	// its comment, and nothing before this test asserted this one.
	if src.headed != 2 {
		t.Errorf("BranchHead calls = %d, want 2 (one per pass)", src.headed)
	}
}

// PollInterval <= 0 must produce a nil channel: a nil channel blocks forever,
// which is how Serve's select case simply never fires for a worker that does
// not poll. It is the same shape tendTicker uses.
func TestPollTickerIsNilWhenDisabled(t *testing.T) {
	w := NewWorker(nil)
	c, stop := w.pollTicker()
	defer stop()
	if c != nil {
		t.Error("a zero PollInterval must produce a nil channel")
	}

	w.PollInterval = time.Minute
	c2, stop2 := w.pollTicker()
	defer stop2()
	if c2 == nil {
		t.Error("a positive PollInterval must produce a ticker")
	}
}

// Every poll test above overrides Worker.deliver, so none of them would
// notice the seam being left unwired in NewWorker -- which would make the
// poller a silent no-op in production, dispatching nothing while logging
// success. This is the one test that looks at the constructor's own wiring.
func TestNewWorkerWiresDeliver(t *testing.T) {
	w := NewWorker(nil)
	if w.deliver == nil {
		t.Fatal("NewWorker left deliver unwired; pollRepo would call a nil func")
	}
}

// A pass given an already-cancelled context must do no work at all: no
// listing, no snapshot write, no cursor write. pollPass and pollRepo both
// check ctx.Err(), but only mid-loop -- this pins the case a caller hands in
// a context that is dead before the first repository is even reached.
func TestACancelledContextPassDeliversAndWritesNothing(t *testing.T) {
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{
			"o/r": {issueSubject(51, "open", nil, 10)},
		},
		heads: map[string]string{"o/r@master": "sha1"},
	}
	h := newPollHarness(t, src, []Target{repoTarget()})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.w.pollPass(ctx)

	if len(h.got) != 0 {
		t.Fatalf("a cancelled pass delivered %+v, want nothing", h.got)
	}
	if _, ok, _ := h.db.PollCursor("o/r"); ok {
		t.Fatal("a cancelled pass wrote a cursor; want no write at all")
	}
}
