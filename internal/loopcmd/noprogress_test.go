package loopcmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// fakeLabels answers LastLabeledAt with one fixed time for every issue.
type fakeLabels struct {
	at    time.Time
	found bool
	err   error
	calls int
}

func (f *fakeLabels) LastLabeledAt(context.Context, string, string, int, string) (time.Time, bool, error) {
	f.calls++
	return f.at, f.found, f.err
}

// finishedRun records one succeeded trigger-path run of issue number under
// session, costing cost, the way a tick and its runner would.
func finishedRun(t *testing.T, cfg *config.Config, deps Deps, number int, session string, cost float64) {
	t.Helper()
	id, err := deps.Store.CreateDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: number, Kind: store.KindResume, SessionID: session,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.BeginDispatch(cfg.Name, cfg.Repo, number, session, "claude", "", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.MarkSessionStarted(cfg.Name, cfg.Repo, number, "claude"); err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.FinishDispatch(id, store.DispatchResult{Status: store.StatusSucceeded, CostUSD: cost}); err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.MarkSucceeded(cfg.Name, cfg.Repo, number); err != nil {
		t.Fatal(err)
	}
}

func assertParkedForHuman(t *testing.T, gh *fakeGH, spawned int, want string) {
	t.Helper()
	if spawned != 0 {
		t.Errorf("spawned = %d, want 0", spawned)
	}
	if len(gh.comments) != 1 || !strings.Contains(gh.comments[0], want) {
		t.Errorf("comments = %q, want one containing %q", gh.comments, want)
	}
	if !slices.Equal(gh.added, []string{"blocked"}) {
		t.Errorf("added = %v, want [blocked]", gh.added)
	}
	if !slices.Contains(gh.removed, "trigger") || !slices.Contains(gh.removed, "in-flight") {
		t.Errorf("removed = %v, want trigger and in-flight", gh.removed)
	}
}

// The incident: the agent put its own trigger back inside the run, so the
// label's last event precedes the run's end. The loop parks instead of
// resuming.
func TestTickParksAnIssueWhoseAgentReappliedItsOwnTrigger(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	labels := &fakeLabels{at: time.Now().Add(-5 * time.Second), found: true}
	deps.Labels = labels
	finishedRun(t, cfg, deps, 1, "sess-1", 0.40)

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Parked != 1 {
		t.Errorf("Parked = %d, want 1", sum.Parked)
	}
	assertParkedForHuman(t, gh, spawned, "Paused: waiting for input")

	st, _ := deps.Store.IssueState(cfg.Name, cfg.Repo, 1)
	if !st.Parked {
		t.Error("the park was not recorded")
	}
}

// The webhook path decides through the same check.
func TestTickIssueParksAnIssueWhoseAgentReappliedItsOwnTrigger(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.Labels = &fakeLabels{at: time.Now().Add(-5 * time.Second), found: true}
	finishedRun(t, cfg, deps, 1, "sess-1", 0.40)

	if _, err := TickIssue(context.Background(), cfg, deps, 1); err != nil {
		t.Fatal(err)
	}
	assertParkedForHuman(t, gh, spawned, "Paused: waiting for input")
}

// A human re-applies the trigger after the run ended. That is a real request,
// and it resumes.
func TestTickResumesWhenTheTriggerWasAppliedAfterTheRunEnded(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.Labels = &fakeLabels{at: time.Now().Add(10 * time.Minute), found: true}
	finishedRun(t, cfg, deps, 1, "sess-1", 0.40)

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Resumed != 1 || spawned != 1 {
		t.Errorf("Resumed = %d, spawned = %d, want 1 and 1", sum.Resumed, spawned)
	}
	if len(gh.comments) != 0 {
		t.Errorf("comments = %q, want none", gh.comments)
	}
}

// A failed history read dispatches as before: the check must never park on
// evidence it does not have.
func TestTickResumesWhenTheLabelHistoryCannotBeRead(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.Labels = &fakeLabels{err: errors.New("boom")}
	finishedRun(t, cfg, deps, 1, "sess-1", 0.40)

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Resumed != 1 {
		t.Errorf("Resumed = %d, want 1", sum.Resumed)
	}
}

// An issue with no earlier run, or whose last run failed, costs no GitHub read.
func TestTickReadsNoLabelHistoryWithoutAnEarlierSucceededRun(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	labels := &fakeLabels{at: time.Now().Add(-time.Hour), found: true}
	deps.Labels = labels

	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	if labels.calls != 0 {
		t.Errorf("label history read %d times, want 0", labels.calls)
	}
	if spawned != 1 {
		t.Errorf("spawned = %d, want 1", spawned)
	}
}

func TestTickParksAResumeAtTheSessionCostCeiling(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	finishedRun(t, cfg, deps, 1, "sess-1", 200)
	finishedRun(t, cfg, deps, 1, "sess-1", 60)

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Parked != 1 {
		t.Errorf("Parked = %d, want 1", sum.Parked)
	}
	assertParkedForHuman(t, gh, spawned, "$260.00")
}

func TestTickResumesPastTheDefaultCeilingWithABudgetLabel(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger", "budget:1000"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	finishedRun(t, cfg, deps, 1, "sess-1", 260)

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Resumed != 1 || spawned != 1 {
		t.Errorf("Resumed = %d, spawned = %d, want 1 and 1", sum.Resumed, spawned)
	}
}

// A park whose label edit already happened must not post a second comment when
// the decision repeats.
func TestASecondParkPostsNoSecondComment(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	finishedRun(t, cfg, deps, 1, "sess-1", 300)

	for i := 0; i < 2; i++ {
		if _, err := Tick(context.Background(), cfg, deps); err != nil {
			t.Fatal(err)
		}
	}
	if len(gh.comments) != 1 {
		t.Errorf("comments = %d, want 1", len(gh.comments))
	}
}
