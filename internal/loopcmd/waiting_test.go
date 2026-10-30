package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/lock"
)

// The fixtures mirror the case this was built for: a pin issue in o/r (the
// loop's repository, standing in for exchange-infra) waits on a mirror issue
// in svc/mirror (standing in for exchange-service).
const (
	waitingLabel = "status:awaiting-mirror"
	blockerRepo  = "svc/mirror"
	mergeSHA     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	laterSHA     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	olderSHA     = "cccccccccccccccccccccccccccccccccccccccc"
)

// write is one GitHub write the pass made, with the repository it was
// addressed to. The repository is the property under test: every write must
// land on the waiting issue's own repository.
type write struct {
	repo   string
	number int
	add    []string
	remove []string
	body   string
}

// fakeWait is a ghub.WaitReader over in-memory state.
type fakeWait struct {
	mu sync.Mutex

	// issues is keyed "owner/name#n".
	issues map[string]ghub.Issue
	// blockedBy maps a waiting issue (in o/r) to its blockers.
	blockedBy map[int][]ghub.Issue
	// blocking maps "owner/name#n" to the issues it blocks.
	blocking map[string][]ghub.Issue
	// closing maps "owner/name#n" to its closing pull requests.
	closing map[string][]ghub.ClosingPR
	// runs is every workflow run, of every workflow, in every repository,
	// newest FIRST, as GitHub lists them.
	runs []fakeRun
	// compare maps "base...head" to GitHub's status.
	compare map[string]string
	// comments maps a waiting issue to its comment bodies.
	comments map[int][]string

	writes []write
	reads  int
}

type fakeRun struct {
	repo, workflow, branch string
	run                    ghub.WorkflowRun
}

func newFakeWait() *fakeWait {
	return &fakeWait{
		issues:    map[string]ghub.Issue{},
		blockedBy: map[int][]ghub.Issue{},
		blocking:  map[string][]ghub.Issue{},
		closing:   map[string][]ghub.ClosingPR{},
		compare:   map[string]string{},
		comments:  map[int][]string{},
	}
}

func key(repo string, n int) string { return fmt.Sprintf("%s#%d", repo, n) }

func (f *fakeWait) Blocking(_ context.Context, owner, repo string, n int) ([]ghub.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.blocking[key(owner+"/"+repo, n)], nil
}

func (f *fakeWait) BlockedBy(_ context.Context, owner, repo string, n int) ([]ghub.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if owner+"/"+repo != "o/r" {
		return nil, fmt.Errorf("blocked_by asked of %s/%s; the pass reads blockers of its own issues only", owner, repo)
	}
	return f.blockedBy[n], nil
}

func (f *fakeWait) Issue(_ context.Context, owner, repo string, n int) (ghub.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	iss, ok := f.issues[key(owner+"/"+repo, n)]
	if !ok {
		return ghub.Issue{}, fmt.Errorf("no issue %s/%s#%d", owner, repo, n)
	}
	return iss, nil
}

func (f *fakeWait) ListOpenIssuesLabeled(_ context.Context, owner, repo, label string) ([]ghub.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	var out []ghub.Issue
	for _, iss := range f.issues {
		if iss.InRepo(owner, repo) && iss.IsOpen() && iss.HasLabel(label) {
			out = append(out, iss)
		}
	}
	return out, nil
}

func (f *fakeWait) ClosingPullRequests(_ context.Context, owner, repo string, n int) ([]ghub.ClosingPR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.closing[key(owner+"/"+repo, n)], nil
}

func (f *fakeWait) DefaultBranch(context.Context, string, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return "main", nil
}

func (f *fakeWait) WorkflowRuns(_ context.Context, owner, repo string, q ghub.RunQuery) ([]ghub.WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	var out []ghub.WorkflowRun
	for _, r := range f.runs {
		if r.repo != owner+"/"+repo || r.workflow != q.Workflow {
			continue
		}
		if q.Branch != "" && r.branch != q.Branch {
			continue
		}
		if q.HeadSHA != "" && r.run.HeadSHA != q.HeadSHA {
			continue
		}
		if q.Status == "success" && r.run.Conclusion != "success" {
			continue
		}
		out = append(out, r.run)
	}
	return out, nil
}

func (f *fakeWait) CompareCommits(_ context.Context, _, _, base, head string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	s, ok := f.compare[base+"..."+head]
	if !ok {
		return "", fmt.Errorf("unexpected compare %s...%s", base, head)
	}
	return s, nil
}

func (f *fakeWait) CommentBodies(_ context.Context, owner, repo string, n int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return append([]string(nil), f.comments[n]...), nil
}

func (f *fakeWait) PostComment(_ context.Context, owner, repo string, n int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, write{repo: owner + "/" + repo, number: n, body: body})
	f.comments[n] = append(f.comments[n], body)
	return nil
}

func (f *fakeWait) EditLabels(_ context.Context, owner, repo string, n int, add, remove []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, write{repo: owner + "/" + repo, number: n, add: add, remove: remove})
	// Reflect the write, so a second pass sees the issue as it now stands.
	k := key(owner+"/"+repo, n)
	iss := f.issues[k]
	labels := append([]string(nil), iss.Labels...)
	labels = append(labels, add...)
	var kept []string
	for _, l := range labels {
		drop := false
		for _, r := range remove {
			if strings.EqualFold(l, r) {
				drop = true
			}
		}
		if !drop {
			kept = append(kept, l)
		}
	}
	iss.Labels = kept
	f.issues[k] = iss
	return nil
}

// labelWrites returns the label writes made, in order.
func (f *fakeWait) labelWrites() []write {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []write
	for _, w := range f.writes {
		if w.body == "" {
			out = append(out, w)
		}
	}
	return out
}

func (f *fakeWait) commentWrites() []write {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []write
	for _, w := range f.writes {
		if w.body != "" {
			out = append(out, w)
		}
	}
	return out
}

// released reports whether n was released: trigger added, waiting removed.
func (f *fakeWait) released(cfg *config.Config, n int) bool {
	var added, removed bool
	for _, w := range f.labelWrites() {
		if w.number != n {
			continue
		}
		for _, l := range w.add {
			added = added || l == cfg.Labels.Trigger
		}
		for _, l := range w.remove {
			removed = removed || l == cfg.Labels.Waiting
		}
	}
	return added && removed
}

// waitFixture is a loop in o/r with a waiting label, and a waiting issue #945
// blocked by svc/mirror#697.
func waitFixture(t *testing.T, checks ...string) (*config.Config, Deps, *fakeWait) {
	t.Helper()
	cfg := tickConfig(t)
	cfg.Labels.Trigger = "status:ready-for-cve-bump"
	cfg.Labels.Waiting = waitingLabel
	cfg.WaitForChecks = checks
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	spawned := 0
	deps := newDeps(t, cfg, &fakeGH{}, &spawned)
	f := newFakeWait()
	deps.Wait = f
	f.issues[key("o/r", 945)] = ghub.Issue{
		Number: 945, State: "open", Repo: "o/r", Labels: []string{"cve-bump", waitingLabel},
	}
	return cfg, deps, f
}

func closedCompleted(repo string, n int) ghub.Issue {
	return ghub.Issue{Number: n, State: "closed", StateReason: "completed", Repo: repo}
}

func mergedBy(f *fakeWait, repo string, n int, sha string) {
	f.closing[key(repo, n)] = []ghub.ClosingPR{{Number: n + 1, Repo: repo, Merged: true, MergeCommit: sha}}
}

func run(f *fakeWait, sha, conclusion string, id int64) {
	status := "completed"
	if conclusion == "" {
		status = "in_progress"
	}
	f.runs = append(f.runs, fakeRun{
		repo: blockerRepo, workflow: "mirror.yml", branch: "main",
		run: ghub.WorkflowRun{
			ID: id, HeadSHA: sha, HeadBranch: "main", Status: status, Conclusion: conclusion,
			HTMLURL: fmt.Sprintf("https://github.com/%s/actions/runs/%d", blockerRepo, id),
		},
	})
}

// The case this was built for: the blocker is in ANOTHER repository, it
// closes as completed, and the waiting issue in this loop's repository is
// released -- with every write addressed to this loop's repository.
func TestWaitReleasesAcrossRepositories(t *testing.T) {
	cfg, deps, f := waitFixture(t)
	f.blockedBy[945] = []ghub.Issue{closedCompleted(blockerRepo, 697)}

	sum, err := WaitSweep(context.Background(), cfg, deps, []int{945})
	if err != nil {
		t.Fatalf("WaitSweep: %v", err)
	}
	if sum.Released != 1 || !f.released(cfg, 945) {
		t.Fatalf("released = %d, writes = %+v; want #945 released", sum.Released, f.writes)
	}
	for _, w := range f.writes {
		if w.repo != "o/r" || w.number != 945 {
			t.Errorf("write %+v; every write must be to o/r#945, the waiting issue itself", w)
		}
	}
	// The ADD comes first, so a failed second write leaves the issue vetoed
	// rather than with neither label.
	lw := f.labelWrites()
	if len(lw) != 2 || len(lw[0].add) != 1 || len(lw[1].remove) != 1 {
		t.Fatalf("label writes = %+v, want add-trigger then remove-waiting", lw)
	}
}

func TestWaitHoldsWhileAnyBlockerIsOpen(t *testing.T) {
	cfg, deps, f := waitFixture(t)
	f.blockedBy[945] = []ghub.Issue{
		closedCompleted(blockerRepo, 697),
		{Number: 12, State: "open", Repo: "o/r"},
	}
	if _, err := WaitSweep(context.Background(), cfg, deps, []int{945}); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("writes = %+v, want none while #12 is open", f.writes)
	}
}

// A blocker closed as not planned did not deliver what the issue waits on.
func TestWaitHoldsForABlockerClosedNotPlanned(t *testing.T) {
	for _, reason := range []string{"not_planned", "duplicate", ""} {
		cfg, deps, f := waitFixture(t)
		f.blockedBy[945] = []ghub.Issue{{Number: 697, State: "closed", StateReason: reason, Repo: blockerRepo}}
		if _, err := WaitSweep(context.Background(), cfg, deps, []int{945}); err != nil {
			t.Fatal(err)
		}
		if len(f.writes) != 0 {
			t.Errorf("reason %q: writes = %+v, want none", reason, f.writes)
		}
	}
}

// No blocker at all is the moment between a job applying the label and
// recording the edge. Releasing then would start the work early.
func TestWaitHoldsAnIssueWithNoBlocker(t *testing.T) {
	cfg, deps, f := waitFixture(t)
	if _, err := WaitSweep(context.Background(), cfg, deps, []int{945}); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("writes = %+v, want none for an issue with no blocker", f.writes)
	}
}

// The issue is re-read under the lock: one that lost the waiting label since
// the delivery listed it is not touched.
func TestWaitSkipsAnIssueNoLongerWaiting(t *testing.T) {
	cfg, deps, f := waitFixture(t)
	f.issues[key("o/r", 945)] = ghub.Issue{Number: 945, State: "open", Repo: "o/r", Labels: []string{"cve-bump"}}
	f.blockedBy[945] = []ghub.Issue{closedCompleted(blockerRepo, 697)}
	if _, err := WaitSweep(context.Background(), cfg, deps, []int{945}); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("writes = %+v, want none", f.writes)
	}
}

// The race this option closes: the closure arrives before the push-triggered
// run on the merge commit exists. Silence is not success.
func TestWaitForChecksHoldsWhenNoRunExistsYet(t *testing.T) {
	cfg, deps, f := waitFixture(t, "mirror.yml")
	f.blockedBy[945] = []ghub.Issue{closedCompleted(blockerRepo, 697)}
	mergedBy(f, blockerRepo, 697, mergeSHA)

	if _, err := WaitSweep(context.Background(), cfg, deps, []int{945}); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("writes = %+v, want none before any run exists", f.writes)
	}

	// An in-progress run is still waiting.
	run(f, mergeSHA, "", 1)
	if _, err := WaitSweep(context.Background(), cfg, deps, []int{945}); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("writes = %+v, want none while the run is in progress", f.writes)
	}
}

func TestWaitForChecksReleasesOnAGreenRunOfTheMergeCommit(t *testing.T) {
	cfg, deps, f := waitFixture(t, "mirror.yml")
	f.blockedBy[945] = []ghub.Issue{closedCompleted(blockerRepo, 697)}
	mergedBy(f, blockerRepo, 697, mergeSHA)
	run(f, mergeSHA, "success", 1)

	sum, err := WaitSweep(context.Background(), cfg, deps, []int{945})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Released != 1 || !f.released(cfg, 945) {
		t.Fatalf("released = %d, writes = %+v", sum.Released, f.writes)
	}
}

// A concurrency group can cancel the merge's own run; a newer green run whose
// head contains the merge commit counts.
func TestWaitForChecksReleasesOnAGreenRunOfALaterCommit(t *testing.T) {
	cfg, deps, f := waitFixture(t, "mirror.yml")
	f.blockedBy[945] = []ghub.Issue{closedCompleted(blockerRepo, 697)}
	mergedBy(f, blockerRepo, 697, mergeSHA)
	run(f, laterSHA, "success", 2)
	run(f, mergeSHA, "cancelled", 1)
	f.compare[mergeSHA+"..."+laterSHA] = "ahead"

	sum, err := WaitSweep(context.Background(), cfg, deps, []int{945})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Released != 1 {
		t.Fatalf("released = %d, writes = %+v; a later green run containing the commit counts", sum.Released, f.writes)
	}
	// A cancelled run is not a failure, and nothing was commented.
	if c := f.commentWrites(); len(c) != 0 {
		t.Errorf("comments = %+v, want none", c)
	}
}

// A green run OLDER than the merge commit does not count.
func TestWaitForChecksIgnoresAGreenRunThatPredatesTheCommit(t *testing.T) {
	cfg, deps, f := waitFixture(t, "mirror.yml")
	f.blockedBy[945] = []ghub.Issue{closedCompleted(blockerRepo, 697)}
	mergedBy(f, blockerRepo, 697, mergeSHA)
	run(f, olderSHA, "success", 1)
	f.compare[mergeSHA+"..."+olderSHA] = "behind"

	if _, err := WaitSweep(context.Background(), cfg, deps, []int{945}); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("writes = %+v, want none: the only green run predates the merge", f.writes)
	}
}

// A failed run on the merge commit is reported ONCE, and the issue keeps
// waiting; a later green run still releases it.
func TestWaitForChecksReportsAFailedRunOnceAndKeepsWaiting(t *testing.T) {
	cfg, deps, f := waitFixture(t, "mirror.yml")
	f.blockedBy[945] = []ghub.Issue{closedCompleted(blockerRepo, 697)}
	mergedBy(f, blockerRepo, 697, mergeSHA)
	run(f, mergeSHA, "failure", 41)

	for i := 0; i < 3; i++ {
		if _, err := WaitSweep(context.Background(), cfg, deps, []int{945}); err != nil {
			t.Fatal(err)
		}
	}
	comments := f.commentWrites()
	if len(comments) != 1 {
		t.Fatalf("comments = %d, want exactly one across three passes", len(comments))
	}
	c := comments[0]
	if c.repo != "o/r" || c.number != 945 {
		t.Errorf("comment addressed to %s#%d, want o/r#945", c.repo, c.number)
	}
	for _, want := range []string{"mirror.yml", "/actions/runs/41", failMarker(41), blockerRepo + "#697"} {
		if !strings.Contains(c.body, want) {
			t.Errorf("comment %q does not name %q", c.body, want)
		}
	}
	if len(f.labelWrites()) != 0 {
		t.Fatalf("label writes = %+v, want none while the check is red", f.labelWrites())
	}

	// A later green run that contains the commit releases it.
	f.runs = append([]fakeRun{{
		repo: blockerRepo, workflow: "mirror.yml", branch: "main",
		run: ghub.WorkflowRun{ID: 42, HeadSHA: laterSHA, Status: "completed", Conclusion: "success"},
	}}, f.runs...)
	f.compare[mergeSHA+"..."+laterSHA] = "ahead"
	sum, err := WaitSweep(context.Background(), cfg, deps, []int{945})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Released != 1 {
		t.Fatalf("released = %d after a green run, want 1", sum.Released)
	}
	if len(f.commentWrites()) != 1 {
		t.Errorf("comments = %d, want still exactly one", len(f.commentWrites()))
	}
}

// A blocker closed as completed with no merged pull request is a human's
// explicit close: there is no commit to wait on.
func TestWaitForChecksReleasesABlockerClosedByHand(t *testing.T) {
	cfg, deps, f := waitFixture(t, "mirror.yml")
	f.blockedBy[945] = []ghub.Issue{closedCompleted(blockerRepo, 697)}
	f.closing[key(blockerRepo, 697)] = []ghub.ClosingPR{{Number: 700, Repo: blockerRepo, Merged: false}}

	sum, err := WaitSweep(context.Background(), cfg, deps, []int{945})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Released != 1 {
		t.Fatalf("released = %d, want 1 for a blocker closed with no merged pull request", sum.Released)
	}
}

// The repository filter of the workflow_run path: an issue none of whose
// blockers lives in the run's repository is not re-decided at all.
func TestWaitSweepRepoConsidersOnlyIssuesBlockedInThatRepository(t *testing.T) {
	cfg, deps, f := waitFixture(t)
	f.blockedBy[945] = []ghub.Issue{closedCompleted(blockerRepo, 697)}
	f.issues[key("o/r", 946)] = ghub.Issue{Number: 946, State: "open", Repo: "o/r", Labels: []string{waitingLabel}}
	f.blockedBy[946] = []ghub.Issue{closedCompleted("else/where", 3)}

	sum, err := WaitSweepRepo(context.Background(), cfg, deps, blockerRepo)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Released != 1 || !f.released(cfg, 945) || f.released(cfg, 946) {
		t.Fatalf("released = %d, writes = %+v; want only #945", sum.Released, f.writes)
	}
}

// The pass takes the loop lock, and a held lock is reported as ErrHeld for the
// daemon to retry, not swallowed.
func TestWaitSweepTakesTheLoopLock(t *testing.T) {
	cfg, deps, f := waitFixture(t)
	f.blockedBy[945] = []ghub.Issue{closedCompleted(blockerRepo, 697)}
	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()

	if _, err := WaitSweep(context.Background(), cfg, deps, []int{945}); !errors.Is(err, lock.ErrHeld) {
		t.Fatalf("err = %v, want lock.ErrHeld", err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("writes = %+v, want none without the lock", f.writes)
	}
}

// A loop with no waiting label makes no call at all.
func TestWaitSweepIsInertWithoutAWaitingLabel(t *testing.T) {
	cfg, deps, f := waitFixture(t)
	cfg.Labels.Waiting = ""
	if _, err := WaitSweepRepo(context.Background(), cfg, deps, ""); err != nil {
		t.Fatal(err)
	}
	if f.reads != 0 {
		t.Fatalf("reads = %d, want none for a loop with no waiting label", f.reads)
	}
}

// The cron backstop: Tick releases a waiting issue whose delivery never
// arrived.
func TestTickReleasesWaitingIssues(t *testing.T) {
	cfg, deps, f := waitFixture(t)
	f.blockedBy[945] = []ghub.Issue{closedCompleted(blockerRepo, 697)}

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if sum.Released != 1 || !f.released(cfg, 945) {
		t.Fatalf("Tick released %d, writes = %+v; want #945 released", sum.Released, f.writes)
	}
}
