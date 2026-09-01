package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/seanmcgary/agent-utils/internal/worktree"
)

type fakeGH struct {
	issues   []ghub.Issue
	prs      []ghub.PullRequest
	behind   map[int]int
	comments []string
	added    []string
	removed  []string

	// The counters and the fetch logs are what a scoped tick is judged by:
	// the token burn this whole change exists to stop is invisible in the
	// dispatch count and visible only in which endpoints were called.
	listedIssues  int
	listedPRs     int
	fetchedIssues []int
	fetchedPRs    []int

	// behindErr makes BehindBy fail for one pull request. A comparison CAN
	// fail in production -- a force-pushed head, a deleted branch -- and a
	// sweep must survive it, so a fake that cannot fail leaves the branch
	// that survives it untested.
	behindErr map[int]error

	// compared records every pull request BehindBy was actually asked to
	// compare, distinct from fetchedPRs (which counts single-PR fetches via
	// PullRequest). A test asserting "the comparison was never made" needs
	// this counter, not that one.
	compared []int

	// listIssuesErr makes ListOpenIssues fail from the call numbered
	// failListIssuesFrom onward (1-indexed against listedIssues, after it is
	// incremented). 0 means never fail.
	//
	// It is call-numbered, not a flat toggle, because one Tick makes TWO
	// ListOpenIssues calls against the same fake: its own snapshot read
	// (call 1), and EpicSweepAll's listing (call 2). A test that wants to
	// prove the sweep's error genuinely reaches Tick's error branch must fail
	// only call 2 -- failing call 1 as well would make Tick return before its
	// own dispatch work ran, which is a different, uninteresting failure.
	listIssuesErr      error
	failListIssuesFrom int

	// reviewActivity and reviewActivityErr choose LatestReviewActivity's
	// answer per pull request number. A nil/zero entry answers the zero
	// time, matching "no review activity", and reviewActivityErr lets a test
	// prove the review trigger's failure direction: a failed read must leave
	// the pull request judged on staleness alone, never treated as pending.
	reviewActivity    map[int]time.Time
	reviewActivityErr map[int]error
	// reviewActivityCalls counts LatestReviewActivity reads, so a test can
	// prove the read was SKIPPED. Skipping it is how the trigger fails closed
	// when the last-tend time is unknown, and a test that only checked the
	// decision could not tell "skipped" from "read and ignored".
	reviewActivityCalls int
}

func (f *fakeGH) ListOpenIssues(context.Context, string, string) ([]ghub.Issue, error) {
	f.listedIssues++
	if f.listIssuesErr != nil && f.failListIssuesFrom > 0 && f.listedIssues >= f.failListIssuesFrom {
		return nil, f.listIssuesErr
	}
	return f.issues, nil
}
func (f *fakeGH) ListOpenPullRequests(context.Context, string, string) ([]ghub.PullRequest, error) {
	f.listedPRs++
	return f.prs, nil
}

// Issue answers from the same fixture the list does, so a scoped tick and a
// full tick decide from identical data. A number the fixture holds as a pull
// request answers ErrNotAnIssue, exactly as GitHub's issues endpoint does.
func (f *fakeGH) Issue(_ context.Context, _, _ string, number int) (ghub.Issue, error) {
	f.fetchedIssues = append(f.fetchedIssues, number)
	for _, pr := range f.prs {
		if pr.Number == number {
			return ghub.Issue{}, fmt.Errorf("o/r#%d: %w", number, ghub.ErrNotAnIssue)
		}
	}
	for _, iss := range f.issues {
		if iss.Number == number {
			return iss, nil
		}
	}
	return ghub.Issue{}, fmt.Errorf("issue #%d not found", number)
}

func (f *fakeGH) PullRequest(_ context.Context, _, _ string, number int) (ghub.PullRequest, error) {
	f.fetchedPRs = append(f.fetchedPRs, number)
	for _, pr := range f.prs {
		if pr.Number == number {
			return pr, nil
		}
	}
	return ghub.PullRequest{}, fmt.Errorf("pull request #%d not found", number)
}
func (f *fakeGH) BehindBy(_ context.Context, _, _, _, head string) (int, error) {
	for _, pr := range f.prs {
		if pr.HeadRef == head {
			f.compared = append(f.compared, pr.Number)
			if err := f.behindErr[pr.Number]; err != nil {
				return 0, err
			}
			return f.behind[pr.Number], nil
		}
	}
	return 0, nil
}
func (f *fakeGH) AuthenticatedLogin(context.Context) (string, error) {
	return "loop-bot", nil
}
func (f *fakeGH) LatestReviewActivity(_ context.Context, _, _ string, number int) (time.Time, error) {
	f.reviewActivityCalls++
	if err := f.reviewActivityErr[number]; err != nil {
		return time.Time{}, err
	}
	return f.reviewActivity[number], nil
}
func (f *fakeGH) PostComment(_ context.Context, _, _ string, _ int, body string) error {
	f.comments = append(f.comments, body)
	return nil
}
func (f *fakeGH) EditLabels(_ context.Context, _, _ string, _ int, add, remove []string) error {
	f.added = append(f.added, add...)
	f.removed = append(f.removed, remove...)
	return nil
}

func tickConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	return &config.Config{
		Name:            "planning",
		Repo:            "o/r",
		CheckoutBaseDir: dir,
		WorktreeDir:     filepath.Join(dir, "wt"),
		StateDir:        filepath.Join(dir, "state"),
		Labels: config.Labels{
			Trigger:  "trigger",
			InFlight: "in-flight",
			Blocked:  "blocked",
			Review:   "review",
			Terminal: "terminal",
		},
		Agent: config.Agent{
			Model: "opus", Worktree: config.WorktreeNone, Timeout: config.Duration(time.Hour),
		},
		Retry: config.Retry{
			// 0s, 15m, 30m: the first retry is immediate and the rest escalate,
			// so a test that advances no clock sees exactly one retry.
			Max: 3,
			Backoff: []config.Duration{
				0,
				config.Duration(15 * time.Minute),
				config.Duration(30 * time.Minute),
			},
			Breaker: config.Breaker{OrphanThreshold: 2, Cooldown: config.Duration(30 * time.Minute)},
		},
		Prompt:       "plan #{{.Issue.Number}}",
		ResumePrompt: "resume #{{.Issue.Number}}",
	}
}

// testProject stands in for a real project UUID. Every row is keyed by one, and
// the detached runner is told which.
const testProject = "11111111-1111-1111-1111-111111111111"

func newDeps(t *testing.T, cfg *config.Config, gh ghub.Client, spawned *int) Deps {
	t.Helper()
	deps, _ := newDepsAt(t, cfg, gh, spawned)
	return deps
}

// newDepsAt is newDeps plus the database's PATH, for the one test that has to
// break a single store read from outside. Deps.Store is a concrete type with
// no seam to inject a failure through, and introducing an interface for one
// test would be a wider change than the property is worth.
func newDepsAt(t *testing.T, cfg *config.Config, gh ghub.Client, spawned *int) (Deps, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	return Deps{
		Store:      db.Project(testProject),
		ProjectID:  testProject,
		GH:         gh,
		WT:         worktree.New(cfg.CheckoutBaseDir, cfg.WorktreeDir, cfg.Name, cfg.DefaultBranch),
		SelfPath:   "/bin/true",
		ConfigPath: "/tmp/loop.yaml",
		Now:        time.Now,
		Spawn: func(string, int64, string, string, string) (int, error) {
			*spawned++
			return 4242, nil
		},
		// Default to "the runner is alive", which is the common case. A test
		// that wants the failure path overrides this.
		IsAlive: func(int, int64) bool { return true },
		Fetch:   nil,
	}, path
}

func TestTickStartsTriggeredIssue(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if spawned != 1 {
		t.Errorf("spawned = %d, want 1", spawned)
	}
	if sum.Started != 1 {
		t.Errorf("Started = %d, want 1", sum.Started)
	}

	running, _ := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if len(running) != 1 {
		t.Fatalf("running dispatches = %d, want 1", len(running))
	}
	states, _ := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if states[1].SessionID == "" {
		t.Error("a session identifier must be stored for a started issue")
	}
}

// The single most important safety property: while an agent is alive, no second
// agent is dispatched for the same issue. IsAlive is a seam so this is
// deterministic rather than dependent on a real process.
func TestTickDoesNotDoubleDispatchWhileRunning(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	if spawned != 1 {
		t.Fatalf("first tick spawned = %d, want 1", spawned)
	}

	// The issue still carries the trigger label, because the agent has not
	// flipped it yet. The live dispatch must be what stops a second spawn.
	for i := 0; i < 3; i++ {
		if _, err := Tick(context.Background(), cfg, deps); err != nil {
			t.Fatal(err)
		}
	}
	if spawned != 1 {
		t.Errorf("spawned = %d after four ticks, want 1 while the agent is alive", spawned)
	}
}

// A dead runner must be retried exactly once per tick, under the cap.
func TestTickRetriesDeadRunnerOnce(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"in-flight"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.IsAlive = func(int, int64) bool { return false }

	id, _ := deps.Store.CreateDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, Kind: store.KindStart, SessionID: "s",
	})
	_ = deps.Store.SetDispatchProcess(id, 999999, time.Now().Add(-time.Hour))
	_ = deps.Store.PutIssueState(store.IssueState{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, SessionID: "s",
		SessionStarted: true, UpdatedAt: time.Now(),
	})

	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	if spawned != 1 {
		t.Fatalf("spawned = %d, want exactly 1 retry", spawned)
	}
	states, _ := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if states[1].RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1", states[1].RetryCount)
	}
}

// This is the reason the project exists: a resumed issue must continue its
// ORIGINAL session, not start a new one.
func TestTickResumePreservesSession(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	states, _ := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	original := states[1].SessionID
	if original == "" {
		t.Fatal("first tick stored no session identifier")
	}

	// The agent finishes cleanly and parks. The human answers and re-applies
	// the trigger label.
	running, _ := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	for _, d := range running {
		_ = deps.Store.FinishDispatch(d.ID, store.DispatchResult{Status: store.StatusSucceeded})
	}
	_ = deps.Store.MarkSucceeded(cfg.Name, cfg.Repo, 1)

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Resumed != 1 {
		t.Fatalf("Resumed = %d, want 1", sum.Resumed)
	}
	states, _ = deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if states[1].SessionID != original {
		t.Errorf("session changed on resume: %q -> %q", original, states[1].SessionID)
	}

	// The resume must be dispatched as a resume, so claude gets "-r".
	all, _ := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if len(all) != 1 || all[0].Kind != store.KindResume {
		t.Errorf("dispatch kind = %+v, want one resume", all)
	}
	if all[0].SessionID != original {
		t.Errorf("dispatch session = %q, want %q", all[0].SessionID, original)
	}
}

// A tick must never wake an issue whose retry budget is spent, and the park must
// remove the trigger label so nothing picks it up again.
func TestParkRemovesTriggerLabel(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"in-flight"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.IsAlive = func(int, int64) bool { return false }

	id, _ := deps.Store.CreateDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, Kind: store.KindStart, SessionID: "s",
	})
	_ = deps.Store.SetDispatchProcess(id, 999999, time.Now().Add(-time.Hour))
	_ = deps.Store.PutIssueState(store.IssueState{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, SessionID: "s",
		RetryCount: 3, UpdatedAt: time.Now(),
	})

	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	if spawned != 0 {
		t.Errorf("spawned = %d, want 0 at the retry cap", spawned)
	}
	wantRemoved := map[string]bool{cfg.Labels.InFlight: true, cfg.Labels.Trigger: true}
	for _, l := range gh.removed {
		delete(wantRemoved, l)
	}
	if len(wantRemoved) != 0 {
		t.Errorf("park did not remove %v; removed = %v", wantRemoved, gh.removed)
	}
}

func TestTickPostsCommentAndLabelsAtRetryCap(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"in-flight"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	deps.IsAlive = func(int, int64) bool { return false }

	// A failure at the cap: a running dispatch row whose process is dead.
	id, _ := deps.Store.CreateDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, Kind: store.KindStart, SessionID: "s",
	})
	_ = deps.Store.SetDispatchProcess(id, 999999, time.Now().Add(-time.Hour))
	_ = deps.Store.PutIssueState(store.IssueState{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, SessionID: "s",
		RetryCount: 3, UpdatedAt: time.Now(),
	})

	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	if len(gh.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(gh.comments))
	}
	if spawned != 0 {
		t.Errorf("spawned = %d, want 0 at the retry cap", spawned)
	}
	if len(gh.added) != 1 || gh.added[0] != cfg.Labels.Blocked {
		t.Errorf("added = %v, want [%s]", gh.added, cfg.Labels.Blocked)
	}
	// The park must remove BOTH the in-flight label and the trigger label (see
	// TestParkRemovesTriggerLabel and the parkRetryExhausted implementation);
	// removing only in-flight would leave the trigger label in place and the
	// issue would resume on the very next tick.
	if len(gh.removed) != 2 || gh.removed[0] != cfg.Labels.InFlight || gh.removed[1] != cfg.Labels.Trigger {
		t.Errorf("removed = %v, want [%s %s]", gh.removed, cfg.Labels.InFlight, cfg.Labels.Trigger)
	}
}

func TestTickIsQuietWhenNothingMatches(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"unrelated"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if spawned != 0 || len(gh.comments) != 0 {
		t.Errorf("a quiet tick must produce nothing: spawned=%d comments=%d",
			spawned, len(gh.comments))
	}
	if sum.Started != 0 || sum.Resumed != 0 || sum.Tended != 0 {
		t.Errorf("summary = %+v, want all zero", sum)
	}
}

// TestTickStopsAnIssueCarryingAnInvalidHarnessOverride is spec section 6.4:
// an invalid label makes Decide emit KindStop instead of dispatching, and the
// tick applies it locally -- MarkStopped -- with no GitHub write at all. That
// last part is the one-GitHub-write invariant (parkRetryExhausted is the
// only write this program performs, tick.go:493) surviving a second decision
// kind, so this asserts the absence of EditLabels and PostComment as strongly
// as it asserts the presence of the stopped state.
func TestTickStopsAnIssueCarryingAnInvalidHarnessOverride(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{
		{Number: 1, Labels: []string{"trigger", "harness:gpt"}},
	}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Started != 0 {
		t.Errorf("Started = %d, want 0", sum.Started)
	}
	if sum.Stopped != 1 {
		t.Errorf("Stopped = %d, want 1", sum.Stopped)
	}
	if spawned != 0 {
		t.Errorf("spawned = %d, want 0: an invalid override must not dispatch", spawned)
	}
	if len(gh.added) != 0 || len(gh.removed) != 0 {
		t.Errorf("EditLabels must not be called for a stop: added=%v removed=%v", gh.added, gh.removed)
	}
	if len(gh.comments) != 0 {
		t.Errorf("PostComment must not be called for a stop: %v", gh.comments)
	}

	states, err := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := states[1]
	if !ok || !st.Stopped {
		t.Fatalf("issue state = %+v, ok=%v, want Stopped=true", st, ok)
	}
	if !strings.Contains(st.StoppedReason, "harness") {
		t.Errorf("StoppedReason = %q, want it to name the harness override", st.StoppedReason)
	}

	// A second tick must change nothing: the issue is stopped, and Decide
	// refuses to dispatch a stopped issue before it ever reaches the override
	// parse.
	sum2, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if sum2.Started != 0 || sum2.Stopped != 0 || spawned != 0 {
		t.Errorf("second tick = %+v spawned=%d, want no further action", sum2, spawned)
	}
}

// TestTickCarriesAModelOverrideToTheDispatchRow is spec section 6.5: a valid
// `model:` label must survive the full path from Decide's decision through
// dispatch's CreateDispatch call to the row a real runner would read. A test
// that only checks Decide's returned decision, or only checks ParseOverrides,
// would stay green even if dispatch dropped Model/Harness/Effort from the
// CreateDispatch call entirely -- every label override would then be lost
// silently, end to end. Driving a full Tick and reading the row back with
// GetDispatch is what closes that gap.
func TestTickCarriesAModelOverrideToTheDispatchRow(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{
		{Number: 1, Labels: []string{"trigger", "model:claude-opus-5"}},
	}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Started != 1 {
		t.Fatalf("Started = %d, want 1", sum.Started)
	}

	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	var found *store.Dispatch
	for i := range running {
		if running[i].Number == 1 {
			found = &running[i]
		}
	}
	if found == nil {
		t.Fatalf("no running dispatch found for issue #1: %+v", running)
	}

	got, err := deps.Store.GetDispatch(found.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want %q", got.Model, "claude-opus-5")
	}
}

func TestTruncateKeepsColumnsAligned(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"short", 10, "short"},
		{"exactlyten", 10, "exactlyten"},
		{"this title is far too long to fit", 10, "this titl\u2026"},
		{"", 10, ""},
	}
	for _, c := range cases {
		if got := truncate(c.in, c.width); got != c.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", c.in, c.width, got, c.want)
		}
		if got := truncate(c.in, c.width); len([]rune(got)) > c.width {
			t.Errorf("truncate(%q, %d) = %q, longer than the column", c.in, c.width, got)
		}
	}
}

// A multi-byte title must not be cut mid-rune.
func TestTruncateIsRuneSafe(t *testing.T) {
	got := truncate("日本語のタイトルはとても長い", 5)
	if len([]rune(got)) != 5 {
		t.Errorf("got %q (%d runes), want 5 runes", got, len([]rune(got)))
	}
}

func TestRenderProjectsExplainsAnEmptyRegistry(t *testing.T) {
	out := RenderProjects(nil)
	for _, want := range []string{"No projects", ".agent-utils/configs", "agent-utils project list"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should mention %q:\n%s", want, out)
		}
	}
}

func TestRenderProjectsSurfacesAMissingProject(t *testing.T) {
	out := RenderProjects([]ProjectSummary{
		{Root: "/gone", Dir: "/gone/.agent-utils", Missing: true},
	})
	if !strings.Contains(out, "MISSING") || !strings.Contains(out, "forget /gone") {
		t.Errorf("a moved project must be reported with a fix that resolves:\n%s", out)
	}
}

// An orphaned dispatch has to be visible at a glance, not buried.
func TestRenderProjectsFlagsOrphans(t *testing.T) {
	out := RenderProjects([]ProjectSummary{{
		Root: "/p", Dir: "/p/.agent-utils",
		Loops: []LoopSummary{{Name: "planning", Repo: "o/r", Live: 2, Orphans: 1}},
	}})
	if !strings.Contains(out, "2+1!") {
		t.Errorf("orphans should be marked in the LIVE column:\n%s", out)
	}
}

// A retry deadline is only reachable through engine.Decide, which iterates the
// OPEN issues. Close the issue and the row is stranded: nothing clears it, and
// the webhook daemon's wake query hands the same past deadline back every
// MinWakeInterval, running a full tick -- GitHub reads included -- each time,
// forever. The tick that can see the issue is gone must retire the deadline.
func TestTickClearsTheDeadlineOfAnIssueTheLoopCanNoLongerSee(t *testing.T) {
	cfg := tickConfig(t)
	// Issue 7 is not in the snapshot: closed, or transferred away.
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	now := time.Now().UTC()
	if err := deps.Store.MarkNeedsRetry(cfg.Name, cfg.Repo, 7, now,
		[]time.Duration{time.Minute}); err != nil {
		t.Fatal(err)
	}

	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}

	st, err := deps.Store.IssueState(cfg.Name, cfg.Repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !st.RetryAfter.IsZero() {
		t.Errorf("RetryAfter = %v, want zero: the daemon's wake query would spin on it forever", st.RetryAfter)
	}
	// The flag survives on purpose. Nothing re-derives it, so destroying it
	// would strand the issue holding an in-flight label with no agent; keeping
	// it means reopening the issue retries at once.
	if !st.NeedsRetry {
		t.Error("NeedsRetry = false; a closed issue's failure must survive so reopening it retries")
	}
}

// The same strand, reached by the other ordinary operator action: a veto label
// makes engine.Decide skip the issue before it ever reads NeedsRetry, so the
// row is just as unreachable as a closed one.
func TestTickClearsTheDeadlineOfAVetoedIssue(t *testing.T) {
	cfg := tickConfig(t)
	cfg.Labels.Veto = []string{"hold"}
	gh := &fakeGH{issues: []ghub.Issue{{Number: 7, Labels: []string{"in-flight", "hold"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	if err := deps.Store.MarkNeedsRetry(cfg.Name, cfg.Repo, 7, time.Now().UTC(),
		[]time.Duration{time.Minute}); err != nil {
		t.Fatal(err)
	}

	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}

	st, err := deps.Store.IssueState(cfg.Name, cfg.Repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !st.RetryAfter.IsZero() {
		t.Errorf("RetryAfter = %v, want zero for a vetoed issue", st.RetryAfter)
	}
	if !st.NeedsRetry {
		t.Error("NeedsRetry = false; removing the veto label must resume the retry")
	}
}

// The sweep must not touch a row the engine CAN reach. An issue inside its
// backoff window is the case that would break loudest: clearing its deadline
// would run the retry immediately and spend the whole escalating list as fast
// as the GitHub API answers.
func TestTickKeepsTheDeadlineOfAnIssueStillInTheSnapshot(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 7, Labels: []string{"in-flight"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	if err := deps.Store.MarkNeedsRetry(cfg.Name, cfg.Repo, 7, time.Now().UTC(),
		[]time.Duration{time.Hour}); err != nil {
		t.Fatal(err)
	}

	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}

	st, err := deps.Store.IssueState(cfg.Name, cfg.Repo, 7)
	if err != nil {
		t.Fatal(err)
	}
	if st.RetryAfter.IsZero() {
		t.Error("RetryAfter was cleared for an issue the engine can still reach; its backoff window is gone")
	}
}

// A dispatch row whose pid is NON-POSITIVE and younger than the grace period
// is a live agent whose pid has not been recorded yet, not a dead runner.
//
// runner.Spawn used to return -1 for every successful spawn (os.Process.Release
// invalidates the handle), and the grace period covered pid 0 only, so a tick
// landing in that window called proc.IsAlive(-1) -- always false -- retired the
// row, flagged the issue for retry, and let a later tick put a SECOND agent in
// a worktree that already held one. Under cron the window was minutes wide and
// effectively unreachable; under the webhook daemon deliveries arrive seconds
// apart, which is why it surfaced. Both halves are fixed; this covers the row
// as it may still be found on disk.
func TestAYoungDispatchWithANonPositivePidIsNotReaped(t *testing.T) {
	for _, pid := range []int{0, -1} {
		t.Run(fmt.Sprintf("pid%d", pid), func(t *testing.T) {
			cfg := tickConfig(t)
			gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"in-flight"}}}}
			spawned := 0
			deps := newDeps(t, cfg, gh, &spawned)
			// The agent IS alive, but nothing can prove it from a pid the
			// kernel never issued; the row's age is the only evidence there is.
			deps.IsAlive = func(int, int64) bool { return false }

			id, _ := deps.Store.CreateDispatch(store.Dispatch{
				Loop: cfg.Name, Repo: cfg.Repo, Number: 1, Kind: store.KindStart, SessionID: "s",
			})
			_ = deps.Store.SetDispatchProcess(id, pid, time.Now())
			_ = deps.Store.PutIssueState(store.IssueState{
				Loop: cfg.Name, Repo: cfg.Repo, Number: 1, SessionID: "s",
				SessionStarted: true, UpdatedAt: time.Now(),
			})

			sum, err := Tick(context.Background(), cfg, deps)
			if err != nil {
				t.Fatal(err)
			}
			if sum.Orphans != 0 {
				t.Errorf("Orphans = %d, want 0: a live agent was reaped", sum.Orphans)
			}
			if sum.Live != 1 {
				t.Errorf("Live = %d, want 1", sum.Live)
			}
			if spawned != 0 {
				t.Errorf("spawned = %d, want 0: a second agent was sent into the same worktree", spawned)
			}
			running, _ := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
			if len(running) != 1 {
				t.Errorf("running dispatches = %d, want the row left alone", len(running))
			}
			states, _ := deps.Store.IssueStates(cfg.Name, cfg.Repo)
			if states[1].NeedsRetry {
				t.Error("the issue was queued for retry while its agent was still working")
			}
		})
	}
}

// The grace period is a window, not an exemption: a row carrying a
// non-positive pid past it is a runner that died before it could register,
// and it must still be reaped.
func TestAnOldDispatchWithANonPositivePidIsStillReaped(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"in-flight"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.IsAlive = func(int, int64) bool { return false }
	// The row's age is measured from its INSERT (started_at), which the store
	// stamps itself, so the clock is moved rather than the row: the tick reads
	// the grace period against deps.Now.
	deps.Now = func() time.Time { return time.Now().Add(2 * pidGracePeriod) }

	id, _ := deps.Store.CreateDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, Kind: store.KindStart, SessionID: "s",
	})
	_ = deps.Store.SetDispatchProcess(id, -1, time.Now())
	_ = deps.Store.PutIssueState(store.IssueState{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, SessionID: "s",
		SessionStarted: true, UpdatedAt: time.Now(),
	})

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Orphans != 1 {
		t.Errorf("Orphans = %d, want 1: a runner that never registered is dead", sum.Orphans)
	}
}

// The cron tick is the backstop for a delivery the daemon never saw.
func TestTickRunsTheEpicSweep(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	// Built with the helpers, which set Repo. A bare ghub.Issue literal has an
	// empty Repo, and InRepo answers false for that, so the sweep would skip
	// every one of them and this test would fail for a reason that has nothing
	// to do with what it is testing.
	gh.issues = []ghub.Issue{epicParent(69), openIssue(73)}
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73)}
	f.blockers[73] = []ghub.Issue{closedIssue(71)}

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if sum.Promoted != 1 {
		t.Fatalf("Summary.Promoted = %d, want 1", sum.Promoted)
	}
	if got := f.promotedNumbers(); len(got) != 1 || got[0] != 73 {
		t.Errorf("promoted %v, want [73]", got)
	}
}

// The self-deadlock regression. RunTick holds the loop lock and then calls
// Tick, so anything Tick calls that acquires the SAME lock gets ErrHeld and
// promotes nothing, forever. Calling Tick directly cannot catch that; this
// calls RunTick, which is what cron runs.
func TestRunTickRunsTheEpicSweepWithoutDeadlocking(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{epicParent(69)}
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73)}
	f.blockers[73] = []ghub.Issue{closedIssue(71)}

	sum, err := RunTick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("RunTick: %v", err)
	}
	if sum.Promoted != 1 {
		t.Fatalf("Promoted = %d, want 1; a sweep that self-deadlocks reports 0", sum.Promoted)
	}
}

// A sweep that cannot read GitHub must not cost the tick its dispatch work.
// The tick's job is dispatch; a failed sweep says nothing about that.
//
// The failure is injected by failing the fake's SECOND ListOpenIssues call --
// EpicSweepAll's own listing, not Tick's own snapshot read (call 1, which must
// succeed for the tick's dispatch work to run at all) -- so the error
// genuinely reaches EpicSweepAll and Tick's handling of it, rather than
// stopping Tick before it does anything.
//
// It is deliberately NOT injected at SubIssues or BlockedBy: both of those are
// handled INSIDE sweepEpic (a failed SubIssues logs and continues to the next
// epic per TestEpicSweepAllContinuesPastAFailedEpic; a failed BlockedBy holds
// the child and returns nil), so neither ever reaches EpicSweepAll's own
// error return and would test nothing about Tick's handling of THAT.
func TestTickSurvivesAFailedEpicSweep(t *testing.T) {
	cfg, deps, _, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{
		openIssue(1, cfg.Labels.Trigger),
	}
	gh.listIssuesErr = errors.New("502 bad gateway")
	gh.failListIssuesFrom = 2

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("Tick must not fail because a sweep did: %v", err)
	}
	if sum.Started != 1 {
		t.Errorf("Started = %d, want 1; the tick's own work must still happen", sum.Started)
	}
	if sum.Promoted != 0 {
		t.Errorf("Promoted = %d, want 0", sum.Promoted)
	}
}

// Deps.Force is the whole of `loop tick --force`: everything else is engine
// behaviour tested in that package. This asserts the wire between them, in
// both positions, on the gate that halts the entire tick.
func TestTickForceOverridesCooldown(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	if err := deps.Store.SetCooldown(cfg.Name, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetCooldown: %v", err)
	}

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if spawned != 0 {
		t.Fatalf("spawned = %d, want 0: an unforced tick honours the cooldown", spawned)
	}
	if sum.Forced {
		t.Error("Forced = true, want false on an unforced tick")
	}

	deps.Force = true
	sum, err = Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("forced Tick: %v", err)
	}
	if spawned != 1 {
		t.Errorf("spawned = %d, want 1: --force dispatches inside the cooldown", spawned)
	}
	if !sum.Forced {
		t.Error("Forced = false, want true: the recorded tick must say it was forced")
	}
}
