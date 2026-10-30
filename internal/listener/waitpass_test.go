package listener

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/go-github/v77/github"
	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/loopcmd"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// waitGH is the harness's client plus the waiting pass's reads. Only Blocking
// is answered: the pass itself is behind the RunWait seams, and is tested in
// internal/loopcmd.
type waitGH struct {
	*deliveryGH

	mu       sync.Mutex
	blocking map[int][]ghub.Issue
	asked    []string
}

func (f *waitGH) Blocking(_ context.Context, owner, repo string, n int) ([]ghub.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, owner+"/"+repo)
	return f.blocking[n], nil
}

func (f *waitGH) askedRepos() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func (*waitGH) BlockedBy(context.Context, string, string, int) ([]ghub.Issue, error) {
	return nil, nil
}
func (*waitGH) ListOpenIssuesLabeled(context.Context, string, string, string) ([]ghub.Issue, error) {
	return nil, nil
}
func (*waitGH) ClosingPullRequests(context.Context, string, string, int) ([]ghub.ClosingPR, error) {
	return nil, nil
}
func (*waitGH) DefaultBranch(context.Context, string, string) (string, error) { return "main", nil }
func (*waitGH) WorkflowRuns(context.Context, string, string, ghub.RunQuery) ([]ghub.WorkflowRun, error) {
	return nil, nil
}
func (*waitGH) CompareCommits(context.Context, string, string, string, string) (string, error) {
	return "", nil
}
func (*waitGH) CommentBodies(context.Context, string, string, int) ([]string, error) {
	return nil, nil
}

// waitCall is one call of a RunWait seam.
type waitCall struct {
	loop    string
	numbers []int
	repo    string // RunWaitRepo's blockerRepo
	byRepo  bool
}

// waitHarness is the harness with a waiting-capable client, two projects --
// the service repository svc/mirror and the infra repository o/r, whose
// "cve-bump" loop waits -- and the two RunWait seams recording their calls.
type waitHarness struct {
	*harness
	gh *waitGH

	mu    sync.Mutex
	calls []waitCall
	err   error
}

func newWaitHarness() *waitHarness { return newWaitHarnessDB(nil) }

func newWaitHarnessDB(db *store.DB) *waitHarness {
	h := newHarness(db)
	wh := &waitHarness{harness: h, gh: &waitGH{deliveryGH: h.gh, blocking: map[int][]ghub.Issue{}}}
	h.w.NewClient = func(string) ghub.Client { return wh.gh }

	svc := h.target("mirror")
	svc.ProjectName, svc.ProjectID, svc.Repo = "service", "svc-project", "svc/mirror"
	infra := h.target("cve-bump")
	infra.Repo = "o/r"
	infra.Waiting = "status:awaiting-mirror"
	infra.WaitForChecks = []string{"mirror.yml"}
	plain := h.target("planning")
	h.targets = []Target{svc, infra, plain}

	h.w.RunWait = func(_ context.Context, cfg *config.Config, _ loopcmd.Deps, numbers []int) (loopcmd.Summary, error) {
		wh.mu.Lock()
		defer wh.mu.Unlock()
		wh.calls = append(wh.calls, waitCall{loop: cfg.Name, numbers: numbers})
		return loopcmd.Summary{}, wh.err
	}
	h.w.RunWaitRepo = func(_ context.Context, cfg *config.Config, _ loopcmd.Deps, repo string) (loopcmd.Summary, error) {
		wh.mu.Lock()
		defer wh.mu.Unlock()
		wh.calls = append(wh.calls, waitCall{loop: cfg.Name, repo: repo, byRepo: true})
		return loopcmd.Summary{}, wh.err
	}
	return wh
}

func (wh *waitHarness) waitCalls() []waitCall {
	wh.mu.Lock()
	defer wh.mu.Unlock()
	return append([]waitCall(nil), wh.calls...)
}

// A closure in one repository re-decides the waiting issue it blocked in
// ANOTHER, through the loop that owns the waiting label -- not through any
// loop of the delivery's repository.
func TestAClosureRunsTheWaitingPassForADependentInAnotherRepository(t *testing.T) {
	wh := newWaitHarness()
	wh.gh.blocking[697] = []ghub.Issue{
		{Number: 945, State: "open", Repo: "o/r", Labels: []string{"status:awaiting-mirror"}},
		// Open, right repository, but not waiting: nothing to release.
		{Number: 946, State: "open", Repo: "o/r", Labels: []string{"cve-bump"}},
		// Waiting label, but in a repository no waiting loop watches.
		{Number: 3, State: "open", Repo: "else/where", Labels: []string{"status:awaiting-mirror"}},
	}

	wh.w.Deliver(context.Background(), Delivery{
		Repo: "svc/mirror", Number: 697, ClosedIssue: true, StateReason: "completed",
	})

	calls := wh.waitCalls()
	if len(calls) != 1 || calls[0].loop != "cve-bump" || len(calls[0].numbers) != 1 || calls[0].numbers[0] != 945 {
		t.Fatalf("waiting pass calls = %+v, want cve-bump for [945]", calls)
	}
	if got := wh.gh.askedRepos(); len(got) != 1 || got[0] != "svc/mirror" {
		t.Errorf("blocking asked of %v, want the closed issue's own repository once", got)
	}
}

// A close as not planned releases nothing, so it costs no call.
func TestAClosureNotPlannedRunsNoWaitingPass(t *testing.T) {
	wh := newWaitHarness()
	wh.gh.blocking[697] = []ghub.Issue{
		{Number: 945, State: "open", Repo: "o/r", Labels: []string{"status:awaiting-mirror"}},
	}
	wh.w.Deliver(context.Background(), Delivery{
		Repo: "svc/mirror", Number: 697, ClosedIssue: true, StateReason: "not_planned",
	})
	if calls := wh.waitCalls(); len(calls) != 0 {
		t.Fatalf("waiting pass calls = %+v, want none", calls)
	}
	if got := wh.gh.askedRepos(); len(got) != 0 {
		t.Errorf("blocking was read %v; a not-planned close should cost nothing", got)
	}
}

// A machine where no loop waits pays no GitHub call for a closure.
func TestAClosureCostsNothingWhenNoLoopWaits(t *testing.T) {
	wh := newWaitHarness()
	wh.targets = []Target{wh.target("planning")}
	wh.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 697, ClosedIssue: true})
	if got := wh.gh.askedRepos(); len(got) != 0 {
		t.Fatalf("blocking was read %v with no waiting loop on the machine", got)
	}
}

// A completed run re-decides the waiting issues of the loops that wait on
// that workflow, narrowed to blockers in the run's repository -- and starts no
// issue pass, because it names no issue.
func TestACompletedWorkflowRunRunsTheWaitingPass(t *testing.T) {
	wh := newWaitHarness()
	wh.w.Deliver(context.Background(), Delivery{Repo: "svc/mirror", CompletedWorkflow: "mirror.yml"})

	calls := wh.waitCalls()
	if len(calls) != 1 || calls[0].loop != "cve-bump" || !calls[0].byRepo || calls[0].repo != "svc/mirror" {
		t.Fatalf("waiting pass calls = %+v, want cve-bump narrowed to svc/mirror", calls)
	}
	if got := wh.ranNumbers(); len(got) != 0 {
		t.Errorf("issue passes ran for %v; a workflow run names no issue", got)
	}
}

// Every workflow of every watched repository completes here. One nothing waits
// on must cost nothing: no token read, no client, no open.
func TestAWorkflowRunNothingWaitsOnIsDroppedQuietly(t *testing.T) {
	wh := newWaitHarness()
	wh.w.Deliver(context.Background(), Delivery{Repo: "svc/mirror", CompletedWorkflow: "ci.yml"})
	if calls := wh.waitCalls(); len(calls) != 0 {
		t.Fatalf("waiting pass calls = %+v, want none", calls)
	}
	if opens, _, _ := wh.counts(); opens != 0 {
		t.Errorf("opened %d loops for a run nothing waits on", opens)
	}
	if n := wh.clientsBuilt(); n != 0 {
		t.Errorf("built %d clients for a run nothing waits on", n)
	}
}

// A run's signal is one-shot, so a held lock is retried rather than dropped.
func TestAWaitingPassFindingTheLockHeldIsRetried(t *testing.T) {
	wh := newWaitHarness()
	wh.err = lock.ErrHeld
	wh.w.Deliver(context.Background(), Delivery{Repo: "svc/mirror", CompletedWorkflow: "mirror.yml"})

	if n := wh.timers.len(); n != 1 {
		t.Fatalf("armed %d timers, want one retry", n)
	}
	wh.mu.Lock()
	wh.err = nil
	wh.mu.Unlock()
	wh.timers.at(t, 0).f()
	if calls := wh.waitCalls(); len(calls) != 2 {
		t.Fatalf("waiting pass calls = %+v, want the first attempt and the retry", calls)
	}
	if n := wh.timers.len(); n != 1 {
		t.Errorf("armed %d timers, want no further retry after success", n)
	}
}

// The start-up catch-up re-decides every waiting loop, unnarrowed.
func TestServeCatchesUpOnWaitingIssues(t *testing.T) {
	wh := newWaitHarnessDB(openWorkDB(t))
	// catchUpWaiting is handed Serve's context, and it stops at a cancelled
	// one; so the context is cancelled from inside the first pass instead,
	// which also ends Serve at its first look.
	ctx, cancel := context.WithCancel(context.Background())
	inner := wh.w.RunWaitRepo
	wh.w.RunWaitRepo = func(c context.Context, cfg *config.Config, d loopcmd.Deps, repo string) (loopcmd.Summary, error) {
		defer cancel()
		return inner(c, cfg, d, repo)
	}
	wh.w.Serve(ctx)

	calls := wh.waitCalls()
	if len(calls) == 0 || calls[0].loop != "cve-bump" || !calls[0].byRepo || calls[0].repo != "" {
		t.Fatalf("waiting pass calls = %+v, want cve-bump over every blocker", calls)
	}
	for _, c := range calls {
		if c.loop != "cve-bump" {
			t.Errorf("catch-up ran loop %q, which has no waiting label", c.loop)
		}
	}
}

// The handler: a completed workflow_run reaches Tick with its workflow file
// and no number; the other actions never do.
func TestTheHandlerPassesACompletedWorkflowRun(t *testing.T) {
	tickCh := make(chan tickCall, 1)
	s := newServer(t, tickCh)
	srv := httptest.NewServer(s.Handler(context.Background()))
	defer srv.Close()

	body := []byte(`{"action":"completed","repository":{"full_name":"svc/mirror"},` +
		`"workflow_run":{"path":".github/workflows/mirror.yml","head_sha":"abc","conclusion":"success"}}`)
	resp := doRequest(t, srv.URL+"/webhook", body, map[string]string{
		github.EventTypeHeader:       "workflow_run",
		github.SHA256SignatureHeader: sha256Sig(testSecret, body),
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	got := waitTick(t, tickCh)
	if got.workflow != "mirror.yml" || got.number != 0 || got.repo != "svc/mirror" {
		t.Fatalf("tick = %+v, want mirror.yml in svc/mirror with no number", got)
	}
}

func TestTheHandlerDropsAWorkflowRunThatHasNotCompleted(t *testing.T) {
	for _, body := range []string{
		`{"action":"in_progress","repository":{"full_name":"svc/mirror"},"workflow_run":{"path":".github/workflows/mirror.yml"}}`,
		`{"action":"requested","repository":{"full_name":"svc/mirror"},"workflow_run":{"path":".github/workflows/mirror.yml"}}`,
		// A dynamic workflow: no file a wait_for_checks entry could name.
		`{"action":"completed","repository":{"full_name":"svc/mirror"},"workflow_run":{"path":"dynamic/pages/pages-build-deployment"}}`,
	} {
		tickCh := make(chan tickCall, 1)
		s := newServer(t, tickCh)
		srv := httptest.NewServer(s.Handler(context.Background()))
		b := []byte(body)
		resp := doRequest(t, srv.URL+"/webhook", b, map[string]string{
			github.EventTypeHeader:       "workflow_run",
			github.SHA256SignatureHeader: sha256Sig(testSecret, b),
		})
		resp.Body.Close()
		srv.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("%s: status = %d, want 204", body, resp.StatusCode)
		}
		assertNoTick(t, tickCh)
	}
}

func TestTheHandlerCarriesAClosedIssuesStateReason(t *testing.T) {
	tickCh := make(chan tickCall, 1)
	s := newServer(t, tickCh)
	srv := httptest.NewServer(s.Handler(context.Background()))
	defer srv.Close()

	body := []byte(`{"action":"closed","repository":{"full_name":"svc/mirror"},` +
		`"issue":{"number":697,"state_reason":"not_planned"}}`)
	resp := doRequest(t, srv.URL+"/webhook", body, map[string]string{
		github.EventTypeHeader:       "issues",
		github.SHA256SignatureHeader: sha256Sig(testSecret, body),
	})
	defer resp.Body.Close()
	got := waitTick(t, tickCh)
	if !got.closedIssue || got.stateReason != "not_planned" {
		t.Fatalf("tick = %+v, want a closed issue carrying not_planned", got)
	}
}
