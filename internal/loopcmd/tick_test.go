package loopcmd

import (
	"context"
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
}

func (f *fakeGH) ListOpenIssues(context.Context, string, string) ([]ghub.Issue, error) {
	return f.issues, nil
}
func (f *fakeGH) ListOpenPullRequests(context.Context, string, string) ([]ghub.PullRequest, error) {
	return f.prs, nil
}
func (f *fakeGH) BehindBy(_ context.Context, _, _, _, head string) (int, error) {
	for _, pr := range f.prs {
		if pr.HeadRef == head {
			return f.behind[pr.Number], nil
		}
	}
	return 0, nil
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
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
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
	}
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
