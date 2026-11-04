package loopcmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/metrics/metricstest"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// one returns the single metric recorded under name, failing the test when
// there is not exactly one.
func one(t *testing.T, rec *metricstest.Recorder, name string) metricstest.Metric {
	t.Helper()
	got := rec.Named(name)
	if len(got) != 1 {
		t.Fatalf("recorded %d %s, want 1; everything recorded: %+v", len(got), name, rec.All())
	}
	return got[0]
}

func wantTags(t *testing.T, m metricstest.Metric, tags ...string) {
	t.Helper()
	for _, tag := range tags {
		if !m.HasTag(tag) {
			t.Errorf("%s lacks tag %q; has %v", m.Name, tag, m.Tags)
		}
	}
}

// noNumberTags pins the cardinality rule: no metric is ever tagged with an
// issue or pull request number.
func noNumberTags(t *testing.T, rec *metricstest.Recorder) {
	t.Helper()
	for _, m := range rec.All() {
		for _, tag := range m.Tags {
			if strings.HasPrefix(tag, "issue:") || strings.HasPrefix(tag, "pr:") ||
				strings.HasPrefix(tag, "number:") {
				t.Errorf("%s carries a per-item tag %q", m.Name, tag)
			}
		}
	}
}

func TestTickEmitsDispatchStartedWithRunTags(t *testing.T) {
	rec := metricstest.Install(t)
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.ProjectName = "web"

	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}

	started := one(t, rec, mDispatchStarted)
	wantTags(t, started, "project:web", "repo:o/r", "loop:planning",
		"kind:fresh", "harness:claude", "model:opus")
	if _, ok := started.TagValue("effort"); ok {
		t.Errorf("an unset effort must be left off, not sent empty: %v", started.Tags)
	}
	if n := len(rec.Named(mDispatchRetry)); n != 0 {
		t.Errorf("a first start counted %d retries", n)
	}

	// The sweep reports concurrency before it dispatches, so the gauge is
	// the count it decided from.
	g := one(t, rec, mDispatchConcurrent)
	if g.Type != "g" || g.Value != 0 {
		t.Errorf("concurrency gauge = %+v, want g 0", g)
	}
	wantTags(t, one(t, rec, mTickDuration), "pass:sweep", "loop:planning")
	noNumberTags(t, rec)
}

// A model: label is resolved into the tag, and a dispatch with no override
// still names the loop's model: grouping by an empty override would put every
// default-model dispatch in one unnamed bucket.
func TestTickTagsTheEffectiveModel(t *testing.T) {
	rec := metricstest.Install(t)
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger", "model:sonnet"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	wantTags(t, one(t, rec, mDispatchStarted), "model:sonnet")
}

// A runner found dead never reported its own outcome, so the reap reports it:
// otherwise started and finished drift apart for every runner that dies.
func TestReapEmitsFinishedForADeadRunner(t *testing.T) {
	rec := metricstest.Install(t)
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

	fin := one(t, rec, mDispatchFinished)
	wantTags(t, fin, "status:failed", "exit_code:none", "kind:fresh", "loop:planning")
	wantTags(t, one(t, rec, mDispatchAPIError), "error_class:runner_died")
	if n := len(rec.Named(mDispatchDuration)); n != 0 {
		t.Errorf("a reaped runner sent %d durations; how long it ran is unknown", n)
	}

	// The same tick retries it, into the session it had started.
	retry := one(t, rec, mDispatchRetry)
	wantTags(t, retry, "kind:resume")
	wantTags(t, one(t, rec, mDispatchStarted), "kind:resume")
	noNumberTags(t, rec)
}

func TestParkEmitsRetryCapAndParkOnce(t *testing.T) {
	rec := metricstest.Install(t)
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

	for i := 0; i < 2; i++ {
		if _, err := Tick(context.Background(), cfg, deps); err != nil {
			t.Fatal(err)
		}
	}
	wantTags(t, one(t, rec, mRetryCapReached), "loop:planning")
	wantTags(t, one(t, rec, mPark), "reason:retry_cap")
}

func TestBreakerTripIsCounted(t *testing.T) {
	rec := metricstest.Install(t)
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{
		{Number: 1, Labels: []string{"in-flight"}},
		{Number: 2, Labels: []string{"in-flight"}},
	}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.IsAlive = func(int, int64) bool { return false }
	for _, n := range []int{1, 2} {
		id, _ := deps.Store.CreateDispatch(store.Dispatch{
			Loop: cfg.Name, Repo: cfg.Repo, Number: n, Kind: store.KindStart, SessionID: "s",
		})
		_ = deps.Store.SetDispatchProcess(id, 999999, time.Now().Add(-time.Hour))
	}

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.BreakerTripped {
		t.Fatalf("fixture did not trip the breaker: %+v", sum)
	}
	wantTags(t, one(t, rec, mBreakerTripped), "loop:planning")
}

// A worktree that cannot be built and a spawn that fails both leave a failed
// row and no runner. Each has its own counter, and neither counts as started:
// started and finished pair up only because both describe runners.
func TestWorktreeAndSpawnFailuresAreCountedNotStarted(t *testing.T) {
	t.Run("worktree", func(t *testing.T) {
		rec := metricstest.Install(t)
		cfg := tickConfig(t)
		// The checkout is a plain temporary directory, not a git repository,
		// so creating a worktree in it fails.
		cfg.Agent.Worktree = config.WorktreePerIssue
		gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
		spawned := 0
		deps := newDeps(t, cfg, gh, &spawned)

		if _, err := Tick(context.Background(), cfg, deps); err != nil {
			t.Fatal(err)
		}
		wantTags(t, one(t, rec, mWorktreeFailed), "kind:fresh")
		if n := len(rec.Named(mDispatchStarted)); n != 0 {
			t.Errorf("started = %d, want 0 when no runner was spawned", n)
		}
	})
	t.Run("spawn", func(t *testing.T) {
		rec := metricstest.Install(t)
		cfg := tickConfig(t)
		gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
		spawned := 0
		deps := newDeps(t, cfg, gh, &spawned)
		deps.Spawn = func(string, int64, string, string, string) (int, error) {
			return 0, errors.New("fork: resource temporarily unavailable")
		}

		if _, err := Tick(context.Background(), cfg, deps); err != nil {
			t.Fatal(err)
		}
		one(t, rec, mDispatchSpawnFailed)
		if n := len(rec.Named(mDispatchStarted)); n != 0 {
			t.Errorf("started = %d, want 0 when the spawn failed", n)
		}
	})
}

// `loop reset` retiring a dead runner's row is the last place a dispatch can
// leave the running state, so it reports dispatch.finished too.
func TestResetEmitsFinishedForTheRowItRetires(t *testing.T) {
	rec := metricstest.Install(t)
	cfg := tickConfig(t)
	spawned := 0
	deps := newDeps(t, cfg, &fakeGH{}, &spawned)
	id, _ := deps.Store.CreateDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 5, Kind: store.KindStart, SessionID: "s",
	})
	_ = deps.Store.SetDispatchProcess(id, 999999, time.Now().Add(-time.Hour))

	if err := Reset(cfg, deps.Store, deps.WT, 5, func(int, int64) bool { return false }); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	wantTags(t, one(t, rec, mDispatchFinished), "status:failed", "exit_code:none", "kind:fresh")
	wantTags(t, one(t, rec, mDispatchAPIError), "error_class:operator")
}

// runAgentDeps builds the Deps a detached runner would have, around one
// running dispatch row.
func runAgentDeps(t *testing.T, cfg *config.Config, kind string) (Deps, int64) {
	t.Helper()
	spawned := 0
	deps := newDeps(t, cfg, &fakeGH{}, &spawned)
	deps.ProjectName = "web"
	id, err := deps.Store.CreateDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 3, Kind: kind, SessionID: "abc",
		LogPath: filepath.Join(t.TempDir(), "run.jsonl"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return deps, id
}

// RunAgent's every exit records through runner.Finish, and one deferred read
// of the row emits from all of them. A prompt that will not render is the
// path with no subprocess at all, so it proves the emit does not depend on
// Supervise.
func TestRunAgentEmitsFinishedWhenThePromptFailsToRender(t *testing.T) {
	rec := metricstest.Install(t)
	cfg := tickConfig(t)
	cfg.ResumePrompt = "resume {{.Nope"
	deps, id := runAgentDeps(t, cfg, store.KindResume)

	if err := RunAgent(context.Background(), cfg, deps, id); err == nil {
		t.Fatal("RunAgent succeeded with a broken template")
	}

	fin := one(t, rec, mDispatchFinished)
	wantTags(t, fin, "status:failed", "exit_code:none", "kind:resume",
		"project:web", "loop:planning", "model:opus")
	if c := one(t, rec, mDispatchCost); c.Type != "d" || c.Value != 0 {
		t.Errorf("cost = %+v, want a 0 distribution sample", c)
	}
	if d := one(t, rec, mDispatchDuration); d.Type != "d" {
		t.Errorf("duration = %+v, want a distribution", d)
	}
	one(t, rec, mDispatchAPIError)
	wantTags(t, one(t, rec, mResumeFailed), "kind:resume")
	noNumberTags(t, rec)
}

func TestRunAgentEmitsCostAndSuccess(t *testing.T) {
	dir := t.TempDir()
	stream := `{"type":"result","subtype":"success","session_id":"abc","total_cost_usd":0.5,"duration_ms":1200,"is_error":false,"result":"ok"}`
	script := "#!/bin/sh\ncat <<'EOF'\n" + stream + "\nEOF\n"
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	rec := metricstest.Install(t)
	cfg := tickConfig(t)
	deps, id := runAgentDeps(t, cfg, store.KindStart)

	if err := RunAgent(context.Background(), cfg, deps, id); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	wantTags(t, one(t, rec, mDispatchFinished), "status:succeeded", "exit_code:zero", "kind:fresh")
	if c := one(t, rec, mDispatchCost); c.Value != 0.5 {
		t.Errorf("cost = %v, want 0.5", c.Value)
	}
	if n := len(rec.Named(mDispatchAPIError)); n != 0 {
		t.Errorf("a success counted %d api errors", n)
	}
	if n := len(rec.Named(mResumeFailed)); n != 0 {
		t.Errorf("a fresh success counted %d failed resumes", n)
	}
}

// A tend that git settles costs no agent and is counted as agent-free.
func TestCleanTendRebaseIsCountedAgentFree(t *testing.T) {
	rec := metricstest.Install(t)
	cfg := rebaseConfig(t)
	deps, _ := rebaseDeps(t, cfg)
	d := tendDecision()
	d.BehindBy = 4

	var sum Summary
	if err := act(context.Background(), cfg, deps, d, time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	one(t, rec, mRebaseAgentFree)
	if b := one(t, rec, mPRBehindBy); b.Value != 4 {
		t.Errorf("behind_by = %v, want 4", b.Value)
	}
	for _, name := range []string{mTendDispatch, mRebaseAgent, mRebaseConflict, mDispatchStarted} {
		if n := len(rec.Named(name)); n != 0 {
			t.Errorf("%s = %d, want 0 for a clean replay", name, n)
		}
	}
}

// A conflict is counted, and the agent it escalates to is counted as an
// agent rebase with the trigger the decision carried.
func TestConflictedTendIsCountedAndEscalated(t *testing.T) {
	rec := metricstest.Install(t)
	cfg := rebaseConfig(t)
	deps, git := rebaseDeps(t, cfg)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	d := tendDecision()
	d.BehindBy = 2

	var sum Summary
	if err := act(context.Background(), cfg, deps, d, time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	one(t, rec, mRebaseConflict)
	one(t, rec, mRebaseAgent)
	wantTags(t, one(t, rec, mTendDispatch), "trigger:behind", "kind:tend", "model:opus")
	wantTags(t, one(t, rec, mDispatchStarted), "kind:tend")
	if n := len(rec.Named(mRebaseAgentFree)); n != 0 {
		t.Errorf("agent_free = %d, want 0 after a conflict", n)
	}
}

// Review activity on a current branch dispatches an agent to answer it, which
// is a tend but not a rebase.
func TestReviewOnlyTendIsNotARebase(t *testing.T) {
	rec := metricstest.Install(t)
	cfg := rebaseConfig(t)
	deps, _ := rebaseDeps(t, cfg)
	d := tendDecision()
	d.ReviewPending = true

	var sum Summary
	if err := act(context.Background(), cfg, deps, d, time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	wantTags(t, one(t, rec, mTendDispatch), "trigger:review")
	if n := len(rec.Named(mRebaseAgent)); n != 0 {
		t.Errorf("rebase.agent = %d, want 0 for a branch that was not behind", n)
	}
}

func TestErrorClass(t *testing.T) {
	cases := map[string]string{
		"":                    "",
		"runner process died": "runner_died",
		"killed by operator":  "operator",
		"reset by operator":   "operator",
		"claude refused the session id as already in use; the session is recorded as started": "session_in_use",
		`402: {"message":"Insufficient credits","code":402}`:                                  "billing",
		`429 Too Many Requests`:                      "rate_limit",
		"API Error: 529 overloaded_error":            "overloaded",
		"401 Unauthorized":                           "auth",
		"signal: killed":                             "timeout",
		"502 Bad Gateway":                            "server",
		"No conversation found with session ID: abc": "session_missing",
		`start claude: exec: "claude": executable file not found in $PATH`: "harness_start",
		"exit status 1":                      "other",
		"request id req_4029 failed weirdly": "other",
	}
	for in, want := range cases {
		if got := errorClass(in); got != want {
			t.Errorf("errorClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExitClassAndTendTrigger(t *testing.T) {
	for code, want := range map[int]string{0: "zero", 1: "nonzero", 137: "nonzero", -1: "none"} {
		if got := exitClass(code); got != want {
			t.Errorf("exitClass(%d) = %q, want %q", code, got, want)
		}
	}
	cases := []struct {
		d    engine.Decision
		want string
	}{
		{engine.Decision{BehindBy: 3}, "behind"},
		{engine.Decision{ReviewPending: true}, "review"},
		{engine.Decision{BehindBy: 1, ReviewPending: true}, "behind_review"},
		{engine.Decision{}, "unknown"},
	}
	for _, tc := range cases {
		if got := tendTrigger(tc.d); got != tc.want {
			t.Errorf("tendTrigger(%+v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
