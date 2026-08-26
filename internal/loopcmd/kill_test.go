package loopcmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/home"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// --- Selector ---

func TestSelectorValidate(t *testing.T) {
	cases := []struct {
		name string
		sel  Selector
		ok   bool
	}{
		{"none", Selector{}, false},
		{"session only", Selector{Session: "s1"}, true},
		{"issue only", Selector{Issue: 7}, true},
		{"all only", Selector{All: true}, true},
		{"session and issue", Selector{Session: "s1", Issue: 7}, false},
		{"session and all", Selector{Session: "s1", All: true}, false},
		{"issue and all", Selector{Issue: 7, All: true}, false},
		{"all three", Selector{Session: "s1", Issue: 7, All: true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.sel.Validate()
			if c.ok && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
			if !c.ok && err == nil {
				t.Errorf("Validate() = nil, want an error")
			}
		})
	}
}

func TestSelectorDescribe(t *testing.T) {
	if got := (Selector{Session: "abc"}).Describe(); got != "session abc" {
		t.Errorf("Describe() = %q", got)
	}
	if got := (Selector{Issue: 7}).Describe(); !strings.Contains(got, "#7") {
		t.Errorf("Describe() = %q, want it to name the issue", got)
	}
	if got := (Selector{Issue: 7, Loop: "planning"}).Describe(); !strings.Contains(got, "planning") {
		t.Errorf("Describe() = %q, want it to name the loop", got)
	}
	if got := (Selector{All: true}).Describe(); !strings.Contains(got, "machine") {
		t.Errorf("Describe() = %q, want it to name the machine-wide scope", got)
	}
	if got := (Selector{All: true, Project: "demo"}).Describe(); !strings.Contains(got, "demo") {
		t.Errorf("Describe() = %q, want it to name the project", got)
	}
}

// --- narrowByLoop ---

func TestNarrowByLoopAcceptsASingleCandidate(t *testing.T) {
	out, err := narrowByLoop([]Target{{Loop: "planning", Issue: 7}}, "")
	if err != nil {
		t.Fatalf("narrowByLoop: %v", err)
	}
	if len(out) != 1 || out[0].Loop != "planning" {
		t.Errorf("out = %+v", out)
	}
}

func TestNarrowByLoopAcceptsANarrowedLoop(t *testing.T) {
	cands := []Target{{Loop: "planning", Issue: 7}, {Loop: "building", Issue: 7}}
	out, err := narrowByLoop(cands, "building")
	if err != nil {
		t.Fatalf("narrowByLoop: %v", err)
	}
	if len(out) != 1 || out[0].Loop != "building" {
		t.Errorf("out = %+v", out)
	}
}

func TestNarrowByLoopRejectsAnAmbiguousIssueAndNamesBothLoops(t *testing.T) {
	cands := []Target{{Loop: "planning", Issue: 7}, {Loop: "building", Issue: 7}}
	_, err := narrowByLoop(cands, "")
	if err == nil {
		t.Fatal("want an error for an ambiguous issue across two loops")
	}
	if !strings.Contains(err.Error(), "planning") || !strings.Contains(err.Error(), "building") {
		t.Errorf("err = %v, want it to name both loops", err)
	}
	if !strings.Contains(err.Error(), "--loop") {
		t.Errorf("err = %v, want it to name --loop", err)
	}
}

// --- killer.one ---

// call is used to assert ordering between the killer's function fields.
type callLog struct {
	calls []string
}

func (c *callLog) add(s string) { c.calls = append(c.calls, s) }

func baseWork() work {
	return work{
		Target: Target{Issue: 7, Loop: "planning", Project: "demo"},
		Repo:   "o/r",
		Dispatch: store.Dispatch{
			ID: 42, Kind: store.KindStart, PID: 100, AgentPID: 200,
			Status: store.StatusRunning,
		},
	}
}

// noopKiller returns a killer whose fields succeed and do nothing beyond
// logging their call, for tests that only care about a subset of behaviour.
func noopKiller(log *callLog) killer {
	return killer{
		markStopped: func(w work) error { log.add("markStopped"); return nil },
		verify:      func(pid int, id int64) error { log.add("verify"); return nil },
		signal: func(pid int, id int64, sig syscall.Signal) error {
			log.add("signal")
			return nil
		},
		waitGone: func(pid int, id int64, timeout time.Duration) bool {
			log.add("waitGone")
			return true
		},
		reread: func(id int64) (store.Dispatch, error) {
			log.add("reread")
			return store.Dispatch{ID: id, Status: store.StatusRunning}, nil
		},
		finish: func(id int64, res store.DispatchResult) error {
			log.add("finish")
			return nil
		},
		killAgent: func(pid int) error { log.add("killAgent"); return nil },
		killRunner: func(pid int, id int64) error {
			log.add("killRunner")
			return nil
		},
	}
}

func TestKillerOneWritesTheStoppedFlagBeforeAnySignal(t *testing.T) {
	log := &callLog{}
	k := noopKiller(log)
	if _, err := k.one(baseWork(), KillOptions{Timeout: time.Second}); err != nil {
		t.Fatalf("one: %v", err)
	}
	if len(log.calls) < 2 || log.calls[0] != "markStopped" {
		t.Fatalf("call order = %v, want markStopped first", log.calls)
	}
	// verify must not precede markStopped either.
	for i, c := range log.calls {
		if c == "verify" {
			if i == 0 {
				t.Fatalf("verify ran before markStopped: %v", log.calls)
			}
			break
		}
	}
}

func TestKillerOneSendsNoSignalWhenMarkStoppedFails(t *testing.T) {
	log := &callLog{}
	k := noopKiller(log)
	k.markStopped = func(w work) error { log.add("markStopped"); return errors.New("disk full") }

	res, err := k.one(baseWork(), KillOptions{Timeout: time.Second})
	if err == nil {
		t.Fatal("want an error when markStopped fails")
	}
	if res.Action != ActionFailed {
		t.Errorf("Action = %q, want %q", res.Action, ActionFailed)
	}
	if len(log.calls) != 1 {
		t.Fatalf("calls = %v, want only markStopped -- no signal after a failed flag write", log.calls)
	}
}

func TestKillerOneTendDispatchSkipsTheIssueFlag(t *testing.T) {
	log := &callLog{}
	k := noopKiller(log)
	w := baseWork()
	w.Dispatch.Kind = store.KindTend

	if _, err := k.one(w, KillOptions{Timeout: time.Second}); err != nil {
		t.Fatalf("one: %v", err)
	}
	for _, c := range log.calls {
		if c == "markStopped" {
			t.Fatalf("markStopped was called for a tend dispatch, which holds no issue state: %v", log.calls)
		}
	}
}

func TestKillerOneUnverifiableRunnerIsAlreadyGoneAndSendsNoSignal(t *testing.T) {
	log := &callLog{}
	k := noopKiller(log)
	k.verify = func(pid int, id int64) error { log.add("verify"); return proc.ErrNotRunner }

	res, err := k.one(baseWork(), KillOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("one: %v", err)
	}
	if res.Action != ActionAlreadyGone {
		t.Errorf("Action = %q, want %q", res.Action, ActionAlreadyGone)
	}
	for _, c := range log.calls {
		if c == "signal" || c == "killAgent" || c == "killRunner" {
			t.Fatalf("a signal was sent to an unverified runner: %v", log.calls)
		}
	}
	// The outcome is still recorded.
	hasReread, hasFinish := false, false
	for _, c := range log.calls {
		hasReread = hasReread || c == "reread"
		hasFinish = hasFinish || c == "finish"
	}
	if !hasReread || !hasFinish {
		t.Errorf("calls = %v, want reread and finish even when already gone", log.calls)
	}
}

func TestKillerOneRealSignalFailureIsReportedAsFailed(t *testing.T) {
	log := &callLog{}
	k := noopKiller(log)
	k.signal = func(pid int, id int64, sig syscall.Signal) error {
		return errors.New("operation not permitted")
	}

	res, err := k.one(baseWork(), KillOptions{Timeout: time.Second})
	if err == nil {
		t.Fatal("want an error on a real signal failure")
	}
	if res.Action != ActionFailed {
		t.Errorf("Action = %q, want %q (not already-gone, not success)", res.Action, ActionFailed)
	}
}

func TestKillerOneStillAliveNamesForce(t *testing.T) {
	log := &callLog{}
	k := noopKiller(log)
	k.waitGone = func(pid int, id int64, timeout time.Duration) bool { return false }

	res, _ := k.one(baseWork(), KillOptions{Timeout: time.Second})
	if res.Action != ActionStillAlive {
		t.Errorf("Action = %q, want %q", res.Action, ActionStillAlive)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "--force") {
		t.Errorf("err = %v, want it to name --force", res.Err)
	}
	// finish must not be called: the runner may still be about to record its
	// own outcome, and this is not the double-record path.
	for _, c := range log.calls {
		if c == "finish" {
			t.Fatalf("finish called while the runner was still alive: %v", log.calls)
		}
	}
}

func TestKillerOneDoesNotDoubleRecordWhenTheRunnerAlreadyDidSo(t *testing.T) {
	log := &callLog{}
	k := noopKiller(log)
	k.reread = func(id int64) (store.Dispatch, error) {
		log.add("reread")
		return store.Dispatch{ID: id, Status: store.StatusFailed}, nil
	}

	res, err := k.one(baseWork(), KillOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("one: %v", err)
	}
	if res.Action != ActionSignalled {
		t.Errorf("Action = %q, want %q", res.Action, ActionSignalled)
	}
	for _, c := range log.calls {
		if c == "finish" {
			t.Fatalf("finish called after the runner already recorded its own outcome: %v", log.calls)
		}
	}
}

func TestKillerOneForceKillsAgentThenRunnerThenRecords(t *testing.T) {
	log := &callLog{}
	k := noopKiller(log)

	res, err := k.one(baseWork(), KillOptions{Force: true})
	if err != nil {
		t.Fatalf("one: %v", err)
	}
	if res.Action != ActionForced {
		t.Errorf("Action = %q, want %q", res.Action, ActionForced)
	}
	want := []string{"markStopped", "verify", "killAgent", "killRunner", "finish"}
	if len(log.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", log.calls, want)
	}
	for i, c := range want {
		if log.calls[i] != c {
			t.Fatalf("calls = %v, want %v", log.calls, want)
		}
	}
}

func TestKillerOneForceDoesNotGroupKillAnUnverifiedRunner(t *testing.T) {
	log := &callLog{}
	k := noopKiller(log)
	k.verify = func(pid int, id int64) error { log.add("verify"); return proc.ErrNotRunner }

	res, err := k.one(baseWork(), KillOptions{Force: true})
	if err != nil {
		t.Fatalf("one: %v", err)
	}
	if res.Action != ActionAlreadyGone {
		t.Errorf("Action = %q, want %q", res.Action, ActionAlreadyGone)
	}
	for _, c := range log.calls {
		if c == "killAgent" || c == "killRunner" {
			t.Fatalf("--force group-killed an unverified runner's agent_pid: %v", log.calls)
		}
	}
}

// --- RenderResults ---

func TestRenderResultsReportsNothingMatched(t *testing.T) {
	if got := RenderResults("kill", nil); !strings.Contains(got, "nothing matched") {
		t.Errorf("RenderResults(nil) = %q", got)
	}
}

func TestRenderResultsNamesForceOnStillAlive(t *testing.T) {
	rs := []Result{{Target: Target{Issue: 7, Loop: "planning", Project: "demo"}, Action: ActionStillAlive}}
	got := RenderResults("kill", rs)
	if !strings.Contains(got, "--force") {
		t.Errorf("RenderResults = %q, want it to name --force", got)
	}
}

func TestRenderResultsNamesEveryTargetAndOutcome(t *testing.T) {
	rs := []Result{
		{Target: Target{Issue: 1, Loop: "planning", Project: "demo"}, Action: ActionSignalled},
		{Target: Target{Issue: 2, Loop: "planning", Project: "demo"}, Action: ActionFailed, Err: fmt.Errorf("boom")},
	}
	got := RenderResults("kill", rs)
	if !strings.Contains(got, "#1") || !strings.Contains(got, "#2") {
		t.Errorf("RenderResults = %q, want both issues named", got)
	}
	if !strings.Contains(got, "boom") {
		t.Errorf("RenderResults = %q, want the failure reason", got)
	}
}

// --- resumeTarget / Resume's refusal rule ---

// TestKillFakeRunnerHelperProcess is a fixture, not a real test: the test
// binary re-execs itself so the child is a Go process whose argv this
// package controls, carrying "--dispatch <id>" the way a real runner does.
func TestKillFakeRunnerHelperProcess(t *testing.T) {
	if os.Getenv("AGENT_UTILS_KILL_HELPER") != "1" {
		return
	}
	time.Sleep(60 * time.Second)
}

func startFakeRunnerProcess(t *testing.T, dispatchID int64) (pid int, cleanup func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestKillFakeRunnerHelperProcess$", "--",
		proc.DispatchFlag, strconv.FormatInt(dispatchID, 10))
	cmd.Env = append(os.Environ(), "AGENT_UTILS_KILL_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake runner: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cmdline, err := proc.CommandLine(cmd.Process.Pid); err == nil && cmdline != "" {
			return cmd.Process.Pid, func() {}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake runner pid %d never became visible to ps", cmd.Process.Pid)
	return 0, func() {}
}

// seedResumeFixture opens the loop at path through the real Open path (so it
// shares the exact database runByLoop will reopen), seeds a stopped issue,
// and returns the Target the resume test drives through runByLoop.
func seedResumeFixture(t *testing.T, path string, withLiveRunner bool) Target {
	t.Helper()
	cfg, deps, cleanup, err := Open(
		ProjectRef{ID: "demo", Name: "demo", Dir: filepath.Dir(path)},
		path,
		Options{MigrationPolicy: WarnOnUnimported},
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cleanup()

	if err := deps.Store.MarkStopped(cfg.Name, cfg.Repo, 7, killedByOperatorReason, time.Now()); err != nil {
		t.Fatal(err)
	}
	if withLiveRunner {
		id, err := deps.Store.CreateDispatch(store.Dispatch{Loop: cfg.Name, Repo: cfg.Repo, Number: 7, Kind: store.KindStart})
		if err != nil {
			t.Fatal(err)
		}
		pid, runnerCleanup := startFakeRunnerProcess(t, id)
		t.Cleanup(runnerCleanup)
		if err := deps.Store.SetDispatchProcess(id, pid, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return Target{ConfigPath: path, Loop: cfg.Name, Issue: 7, Project: "demo", ProjectID: "demo", Dir: filepath.Dir(path)}
}

// TestResumeRefusesALiveRunner proves the exact refusal rule Resume applies,
// by driving the real runByLoop/resumeTarget production path -- deleting the
// refusal branch at resumeTarget's live-runner check makes this test fail.
// A dispatch whose runner still verifies as live must not be cleared: the
// runner holds no loop lock and its own finish calls MarkNeedsRetry
// (runner.go:321), so clearing the flag while it might still be dying would
// have the clear written straight back by the runner's own exit.
func TestResumeRefusesALiveRunner(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	path := writeKillTestConfig(t, "planning")
	target := seedResumeFixture(t, path, true)

	results := runByLoop([]Target{target}, resumeTarget)
	if len(results) != 1 || results[0].Action != ActionRefused {
		t.Fatalf("results = %+v, want a single ActionRefused", results)
	}
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "--force") {
		t.Errorf("err = %v, want it to name --force", results[0].Err)
	}

	verifyResumeIssueState(t, path, target, true)
}

func TestResumeClearsADeadRunner(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	path := writeKillTestConfig(t, "planning")
	// No running dispatch at all -- the ordinary case for a fully retired kill.
	target := seedResumeFixture(t, path, false)

	results := runByLoop([]Target{target}, resumeTarget)
	if len(results) != 1 || results[0].Action != ActionResumed {
		t.Fatalf("results = %+v, want a single ActionResumed", results)
	}

	verifyResumeIssueState(t, path, target, false)
}

// verifyResumeIssueState re-opens the same store to check the stopped flag
// after runByLoop's cleanup has run.
func verifyResumeIssueState(t *testing.T, path string, target Target, wantStopped bool) {
	t.Helper()
	cfg, deps, cleanup, err := Open(
		ProjectRef{ID: "demo", Name: "demo", Dir: filepath.Dir(path)},
		path,
		Options{MigrationPolicy: WarnOnUnimported},
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cleanup()
	st, err := deps.Store.IssueState(cfg.Name, cfg.Repo, target.Issue)
	if err != nil {
		t.Fatal(err)
	}
	if st.Stopped != wantStopped {
		t.Errorf("Stopped = %v, want %v", st.Stopped, wantStopped)
	}
}

// --- runByLoop / the loop lock ---

const killTestYAML = `
name: %s
repo: o/r
checkout_base_dir: %s
worktree_dir: %s
state_dir: %s
labels:
  trigger: trigger
  in_flight: in-flight
  blocked: blocked
  review: review
default_branch: master
agent:
  model: opus
  worktree: none
  timeout: 1h
retry:
  max: 0
  breaker:
    orphan_threshold: 1
    cooldown: 1m
prompt: "p"
resume_prompt: "r"
`

func writeKillTestConfig(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	body := fmt.Sprintf(killTestYAML, name, dir, filepath.Join(dir, "wt"), filepath.Join(dir, "state"))
	p := filepath.Join(dir, "loop.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunByLoopAHeldLockFailsOnlyThatLoopsTargets(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())

	heldPath := writeKillTestConfig(t, "held")
	freePath := writeKillTestConfig(t, "free")

	heldCfg, err := config.Load(heldPath)
	if err != nil {
		t.Fatal(err)
	}
	stateDir, err := heldCfg.ResolveStateDir(heldPath)
	if err != nil {
		t.Fatal(err)
	}
	heldCfg.StateDir = stateDir
	held, err := lock.Acquire(filepath.Join(heldCfg.StateDir, heldCfg.Name+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	targets := []Target{
		{ConfigPath: heldPath, Loop: "held", Issue: 1},
		{ConfigPath: freePath, Loop: "free", Issue: 2},
	}

	var ran []int
	results := runByLoop(targets, func(cfg *config.Config, st *store.Store, tgt Target) Result {
		ran = append(ran, tgt.Issue)
		return Result{Target: tgt, Action: ActionSignalled}
	})

	if len(ran) != 1 || ran[0] != 2 {
		t.Fatalf("ran = %v, want only the free loop's target (issue 2)", ran)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want one result per target", results)
	}
	var heldResult, freeResult Result
	for _, r := range results {
		if r.Target.Issue == 1 {
			heldResult = r
		} else {
			freeResult = r
		}
	}
	if heldResult.Action != ActionFailed || heldResult.Err == nil ||
		!strings.Contains(heldResult.Err.Error(), "a tick is running for loop") {
		t.Errorf("held loop result = %+v, want the loop-reset wording", heldResult)
	}
	if freeResult.Action != ActionSignalled {
		t.Errorf("free loop result = %+v, want it to have run", freeResult)
	}
}
