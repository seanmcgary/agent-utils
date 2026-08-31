package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/seanmcgary/agent-utils/internal/worktree"
)

// tendCheckLoop is the loop every fixture below belongs to. It is a constant
// because a pr_links row is keyed by the loop name, so a seed written under one
// name and read under another would make a test pass for the wrong reason.
const tendCheckLoop = "execution"

// countingGH is a ghub.Client that COUNTS its list calls. The zero-call case is
// the property this whole pass exists for, and it is invisible in the result
// TendCheck returns: a pass that gated correctly and a pass that called GitHub
// and found nothing both report Stale 0.
type countingGH struct {
	issues []ghub.Issue
	prs    []ghub.PullRequest

	// calls counts every request the pass made, of any kind.
	calls int
}

func (c *countingGH) ListOpenIssues(context.Context, string, string) ([]ghub.Issue, error) {
	c.calls++
	return c.issues, nil
}

func (c *countingGH) ListOpenPullRequests(context.Context, string, string) ([]ghub.PullRequest, error) {
	c.calls++
	return c.prs, nil
}

func (c *countingGH) Issue(_ context.Context, _, _ string, number int) (ghub.Issue, error) {
	c.calls++
	for _, iss := range c.issues {
		if iss.Number == number {
			return iss, nil
		}
	}
	return ghub.Issue{}, fmt.Errorf("issue #%d not found", number)
}

func (c *countingGH) PullRequest(_ context.Context, _, _ string, number int) (ghub.PullRequest, error) {
	c.calls++
	for _, pr := range c.prs {
		if pr.Number == number {
			return pr, nil
		}
	}
	return ghub.PullRequest{}, fmt.Errorf("pull request #%d not found", number)
}

// BehindBy counts like the rest. The pass must never reach it -- the local
// compare is what answers this question -- so a test that starts failing here
// is a test reporting that the gate was replaced by an API call.
func (c *countingGH) BehindBy(context.Context, string, string, string, string) (int, error) {
	c.calls++
	return 0, nil
}

func (c *countingGH) PostComment(context.Context, string, string, int, string) error {
	c.calls++
	return nil
}

func (c *countingGH) EditLabels(context.Context, string, string, int, []string, []string) error {
	c.calls++
	return nil
}

// tendCheckConfig is a loop that tends. It takes t so StateDir lands in a
// temporary directory the test framework removes: the pass takes the loop lock,
// and a lock file shared between tests would make them serialise on each other.
func tendCheckConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	return &config.Config{
		Name:            tendCheckLoop,
		Repo:            "o/r",
		DefaultBranch:   "master",
		TendPR:          true,
		TendPrompt:      "rebase #{{.Issue.Number}}",
		CheckoutBaseDir: dir,
		WorktreeDir:     filepath.Join(dir, "wt"),
		StateDir:        filepath.Join(dir, "state"),
		Labels: config.Labels{
			Trigger:  "status:todo",
			InFlight: "status:doing",
			Blocked:  "status:blocked",
			Review:   "status:review",
			Terminal: "status:done",
		},
		Agent: config.Agent{
			Model: "opus", Worktree: config.WorktreeNone, Timeout: config.Duration(time.Hour),
		},
		Prompt:       "plan #{{.Issue.Number}}",
		ResumePrompt: "resume #{{.Issue.Number}}",
	}
}

// tendCheckDeps builds a Deps whose Behind answers from behind: a head branch
// present in the map is that many commits behind, and one absent from it does
// not resolve -- which is what BehindLocal reports for a branch the prune
// removed.
//
// An unknown branch answers a NON-ZERO count alongside known=false. Production
// returns 0 there, but a fake that did the same would let a caller drop the
// known check and still pass every test, because n <= 0 skips the row on its
// own. The two skips have different reasons and must fail separately.
func tendCheckDeps(t *testing.T, gh ghub.Client, behind map[string]int) Deps {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	return Deps{
		Store:      db.Project(testProject),
		ProjectID:  testProject,
		GH:         gh,
		WT:         worktree.New(dir, filepath.Join(dir, "wt"), tendCheckLoop, "master"),
		SelfPath:   "/bin/true",
		ConfigPath: "/tmp/loop.yaml",
		Now:        time.Now,
		Spawn: func(string, int64, string, string, string) (int, error) {
			t.Error("TendCheck dispatched an agent; it reports a count and dispatches nothing")
			return 0, nil
		},
		IsAlive: func(int, int64) bool { return true },
		Fetch:   func(context.Context) error { return nil },
		Behind: func(_ context.Context, headRef, _ string) (int, bool, error) {
			n, ok := behind[headRef]
			if !ok {
				return 5, false, nil
			}
			return n, true, nil
		},
	}
}

func seedPRLink(t *testing.T, deps Deps, issue, pr int, headRef, baseRef string) {
	t.Helper()
	if err := deps.Store.PutPRLink(store.PRLink{
		Loop: tendCheckLoop, Repo: "o/r", Number: issue, PRNumber: pr,
		HeadRef: headRef, BaseRef: baseRef,
	}); err != nil {
		t.Fatal(err)
	}
}

// tendCheckPR is a fixture engine.LinkPR will actually link. Trusted and a
// closing reference are both required (internal/engine/prlink.go:20-39); a
// fixture missing either links to nothing, and every assertion below it passes
// vacuously.
func tendCheckPR(number int, headRef string, issue int) ghub.PullRequest {
	return ghub.PullRequest{
		Number: number, HeadRef: headRef, BaseRef: "master",
		Trusted: true, Body: fmt.Sprintf("Closes #%d", issue),
	}
}

// The whole point of the gate. A loop whose branches are all current must cost
// no GitHub request at all.
func TestTendCheckMakesNoGitHubCallWhenNothingIsBehind(t *testing.T) {
	gh := &countingGH{}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/x": 0})
	seedPRLink(t, deps, 7, 9, "feat/x", "master")

	got, err := TendCheck(context.Background(), tendCheckConfig(t), deps, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Confirmed {
		t.Error("Confirmed = true; the pass must not call GitHub when nothing is behind")
	}
	if gh.calls != 0 {
		t.Errorf("GitHub calls = %d, want 0", gh.calls)
	}
	if got.Stale != 0 {
		t.Errorf("Stale = %d, want 0", got.Stale)
	}
}

// One behind branch buys exactly two calls: the open pull requests and the
// open issues. Not one per pull request.
func TestTendCheckConfirmsWithTwoCallsWhenSomethingIsBehind(t *testing.T) {
	gh := &countingGH{
		prs:    []ghub.PullRequest{tendCheckPR(9, "feat/x", 7)},
		issues: []ghub.Issue{{Number: 7, Repo: "o/r", Labels: []string{"status:review"}}},
	}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/x": 3})
	seedPRLink(t, deps, 7, 9, "feat/x", "master")

	got, err := TendCheck(context.Background(), tendCheckConfig(t), deps, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stale != 1 {
		t.Errorf("Stale = %d, want 1", got.Stale)
	}
	if gh.calls != 2 {
		t.Errorf("GitHub calls = %d, want 2", gh.calls)
	}
}

// A branch the prune removed is a pull request whose branch is gone. It is not
// a candidate, and it is not an error -- and it is skipped for that reason
// alone: the fake answers a count of 5, so a caller that dropped the known
// check would treat this row as behind and call GitHub.
func TestTendCheckSkipsARowWhoseBranchIsGone(t *testing.T) {
	gh := &countingGH{}
	deps := tendCheckDeps(t, gh, nil) // Behind reports known=false for everything
	seedPRLink(t, deps, 7, 9, "feat/gone", "master")

	got, err := TendCheck(context.Background(), tendCheckConfig(t), deps, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stale != 0 || gh.calls != 0 {
		t.Errorf("Stale = %d, calls = %d; want 0 and 0", got.Stale, gh.calls)
	}
}

// A row whose local compare fails is skipped, not fatal. Anyone able to open a
// pull request could otherwise stop every rebase this loop would do.
func TestTendCheckSurvivesAFailedLocalCompare(t *testing.T) {
	gh := &countingGH{}
	deps := tendCheckDeps(t, gh, nil)
	deps.Behind = func(context.Context, string, string) (int, bool, error) {
		return 0, false, errors.New("unsafe branch name")
	}
	seedPRLink(t, deps, 7, 9, "feat/../../etc", "master")

	got, err := TendCheck(context.Background(), tendCheckConfig(t), deps, false)
	if err != nil {
		t.Fatalf("one unusable row must not fail the pass: %v", err)
	}
	if got.Stale != 0 || gh.calls != 0 {
		t.Errorf("Stale = %d, calls = %d; want 0 and 0", got.Stale, gh.calls)
	}
}

// The cold cache. A loop with no rows has nothing to gate on, so a forced pass
// is the only thing that can ever populate it.
func TestTendCheckForcedRunsTheConfirmWithNoRows(t *testing.T) {
	gh := &countingGH{
		prs:    []ghub.PullRequest{tendCheckPR(9, "feat/x", 7)},
		issues: []ghub.Issue{{Number: 7, Repo: "o/r", Labels: []string{"status:review"}}},
	}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/x": 2})

	got, err := TendCheck(context.Background(), tendCheckConfig(t), deps, true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Confirmed {
		t.Error("Confirmed = false; a forced pass must call GitHub")
	}
	if got.Stale != 1 {
		t.Errorf("Stale = %d, want 1", got.Stale)
	}
}

// The drifted cache. A row whose pull request is no longer open is deleted, so
// the gate stops counting a merged branch as behind forever.
func TestTendCheckDeletesARowWhosePullRequestIsClosed(t *testing.T) {
	gh := &countingGH{
		prs:    nil, // nothing open
		issues: nil,
	}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/merged": 4})
	seedPRLink(t, deps, 7, 9, "feat/merged", "master")

	if _, err := TendCheck(context.Background(), tendCheckConfig(t), deps, true); err != nil {
		t.Fatal(err)
	}
	links, err := deps.Store.PRLinks(tendCheckLoop, "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := links[7]; ok {
		t.Error("the row for a closed pull request was not deleted")
	}
}

// An issue that lost its review label is not tended, even though its branch is
// behind. The gate never decides on the local cache alone.
func TestTendCheckDropsACandidateWhoseIssueLostTheReviewLabel(t *testing.T) {
	gh := &countingGH{
		prs:    []ghub.PullRequest{tendCheckPR(9, "feat/x", 7)},
		issues: []ghub.Issue{{Number: 7, Repo: "o/r", Labels: []string{"status:doing"}}},
	}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/x": 3})
	seedPRLink(t, deps, 7, 9, "feat/x", "master")

	got, err := TendCheck(context.Background(), tendCheckConfig(t), deps, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stale != 0 {
		t.Errorf("Stale = %d, want 0", got.Stale)
	}
}

// An untrusted pull request -- a fork's branch, or an outside contributor's --
// is not linked, not counted, and never rebased. ghub.convertPR is what sets
// Trusted, and engine.LinkPR is what enforces it; this test is here so a
// future refactor of the confirm step cannot quietly drop the check.
func TestTendCheckIgnoresAnUntrustedPullRequest(t *testing.T) {
	gh := &countingGH{
		prs:    []ghub.PullRequest{{Number: 9, HeadRef: "feat/x", BaseRef: "master", Body: "Closes #7"}},
		issues: []ghub.Issue{{Number: 7, Repo: "o/r", Labels: []string{"status:review"}}},
	}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/x": 3})
	seedPRLink(t, deps, 7, 9, "feat/x", "master")

	got, err := TendCheck(context.Background(), tendCheckConfig(t), deps, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stale != 0 {
		t.Errorf("Stale = %d, want 0 for an untrusted pull request", got.Stale)
	}
}

// A loop that does not tend must not fetch, must not compare, and must not call
// GitHub -- not even when forced. The check is first in the function for the
// reason TendSweep gives: TendCheck is exported, so a loop that does not tend
// costs nothing whoever calls it.
func TestTendCheckDoesNothingWhenTheLoopDoesNotTend(t *testing.T) {
	gh := &countingGH{}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/x": 3})
	seedPRLink(t, deps, 7, 9, "feat/x", "master")
	cfg := tendCheckConfig(t)
	cfg.TendPR = false

	fetched := 0
	deps.Fetch = func(context.Context) error {
		fetched++
		return nil
	}
	deps.Behind = func(context.Context, string, string) (int, bool, error) {
		t.Error("a loop that does not tend compared a branch")
		return 0, false, nil
	}

	got, err := TendCheck(context.Background(), cfg, deps, true)
	if fetched != 0 {
		t.Errorf("fetches = %d; a loop that does not tend must not touch git", fetched)
	}
	if err != nil || got.Confirmed || gh.calls != 0 {
		t.Errorf("got %+v, err %v, calls %d; want a no-op", got, err, gh.calls)
	}
}

// A hand-built Deps -- there are several in this program -- leaves Behind nil.
// The pass runs on the daemon's Serve goroutine, where a panic takes every
// project down with it.
func TestTendCheckDoesNothingWithoutTheLocalSeam(t *testing.T) {
	gh := &countingGH{}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/x": 3})
	deps.Behind = nil
	seedPRLink(t, deps, 7, 9, "feat/x", "master")

	got, err := TendCheck(context.Background(), tendCheckConfig(t), deps, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Confirmed || got.Stale != 0 || gh.calls != 0 {
		t.Errorf("got %+v, calls %d; want a no-op", got, gh.calls)
	}
}

// A failed fetch makes every comparison below it stale, so the pass stops
// rather than reporting a branch as current after the base moved.
func TestTendCheckFailsWhenTheFetchFails(t *testing.T) {
	deps := tendCheckDeps(t, &countingGH{}, nil)
	deps.Fetch = func(context.Context) error { return errors.New("no route to host") }

	if _, err := TendCheck(context.Background(), tendCheckConfig(t), deps, false); err == nil {
		t.Error("a failed fetch must fail the pass")
	}
}

// A held lock means a tick is already running for this loop, and that tick does
// this pass's work as part of its own. The pass returns rather than waiting,
// because the caller is the daemon's timer goroutine and waiting would pin it
// behind an agent dispatch.
func TestTendCheckSkipsWhenTheLoopLockIsHeld(t *testing.T) {
	gh := &countingGH{
		prs:    []ghub.PullRequest{tendCheckPR(9, "feat/x", 7)},
		issues: []ghub.Issue{{Number: 7, Repo: "o/r", Labels: []string{"status:review"}}},
	}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/x": 3})
	seedPRLink(t, deps, 7, 9, "feat/x", "master")
	cfg := tendCheckConfig(t)

	held, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		t.Fatalf("lock.Acquire: %v", err)
	}
	defer held.Release()

	got, err := TendCheck(context.Background(), cfg, deps, true)
	if err != nil {
		t.Fatalf("a held lock is not an error: %v", err)
	}
	if got.Confirmed || got.Stale != 0 || gh.calls != 0 {
		t.Errorf("got %+v, calls %d; want a no-op while another tick holds the lock", got, gh.calls)
	}
}

// The fetch is what makes the local refs worth reading. A pass that compared
// against refs from the last delivery would report a branch as current after
// the base moved -- silently, and for as long as the daemon stays up.
func TestTendCheckFetchesBeforeItCompares(t *testing.T) {
	deps := tendCheckDeps(t, &countingGH{}, map[string]int{"feat/x": 0})
	seedPRLink(t, deps, 7, 9, "feat/x", "master")

	fetched := 0
	deps.Fetch = func(context.Context) error {
		fetched++
		return nil
	}
	deps.Behind = func(context.Context, string, string) (int, bool, error) {
		if fetched == 0 {
			t.Error("the local compare ran before the fetch updated the refs it reads")
		}
		return 0, true, nil
	}

	if _, err := TendCheck(context.Background(), tendCheckConfig(t), deps, false); err != nil {
		t.Fatal(err)
	}
	if fetched != 1 {
		t.Errorf("fetches = %d, want 1", fetched)
	}
}

// The boundary that keeps this pass off a release branch. A pull request
// targeting release/1.0 is behind for reasons the loop default branch knows
// nothing about, and later tasks in this plan FORCE-PUSH what this pass counts,
// so a base the loop does not own must never reach that path. TendSweep
// enforces the same rule against the branch a merge landed on.
func TestTendCheckIgnoresAPullRequestTargetingAnotherBase(t *testing.T) {
	release := ghub.PullRequest{
		Number: 10, HeadRef: "feat/rel", BaseRef: "release/1.0",
		Trusted: true, Body: "Closes #8",
	}
	gh := &countingGH{
		prs:    []ghub.PullRequest{release},
		issues: []ghub.Issue{{Number: 8, Repo: "o/r", Labels: []string{"status:review"}}},
	}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/rel": 3})
	seedPRLink(t, deps, 8, 10, "feat/rel", "release/1.0")

	cfg := tendCheckConfig(t) // DefaultBranch is master
	got, err := TendCheck(context.Background(), cfg, deps, false)
	if err != nil {
		t.Fatal(err)
	}
	// The gate opened -- the branch really is three commits behind its own base
	// -- so this is the base check doing the work, not an accident of the
	// fixture.
	if !got.Confirmed {
		t.Fatal("Confirmed = false; the fixture must reach the confirm step for this test to mean anything")
	}
	if got.Stale != 0 {
		t.Errorf("Stale = %d, want 0 for a pull request targeting %q while default_branch is %q",
			got.Stale, release.BaseRef, cfg.DefaultBranch)
	}
}
