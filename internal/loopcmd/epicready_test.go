package loopcmd

import (
	"context"
	"errors"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/epic"
	"github.com/seanmcgary/agent-utils/internal/ghub"
)

// readyEpic is a parent carrying BOTH the epic label and the ready label --
// the state an operator leaves behind by pressing the button.
func readyEpic(n int) ghub.Issue {
	return ghub.Issue{Number: n, State: "open", Repo: "o/r",
		Labels: []string{epic.Label, epic.ReadyLabel}}
}

// The whole point of this pass: an epic whose children have NEVER closed gets
// its first promotion. EpicSweep cannot do this -- it enters at a closed child,
// and there is no closed child here.
func TestEpicReadyPromotesWithNothingClosed(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{readyEpic(48)}
	f.children[48] = []ghub.Issue{openIssue(49), openIssue(50)}
	f.blockers[49] = nil
	f.blockers[50] = []ghub.Issue{openIssue(49)}

	sum, err := EpicReady(context.Background(), cfg, deps, 48)
	if err != nil {
		t.Fatalf("EpicReady: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 1 || got[0] != 49 {
		t.Fatalf("promoted %v, want [49]", got)
	}
	if got := f.added[49]; len(got) != 1 || got[0] != "status:ready-for-spec" {
		t.Errorf("added %v to 49, want [status:ready-for-spec]", got)
	}
	if sum.Promoted != 1 {
		t.Errorf("Summary.Promoted = %d, want 1", sum.Promoted)
	}
}

// The label is a BUTTON, not a state: the pass consumes it so pressing it again
// is a fresh trigger. Applying a label that is already present produces no
// delivery, so a label left in place could never arm a second sweep.
func TestEpicReadyConsumesTheLabel(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{readyEpic(48)}
	f.children[48] = []ghub.Issue{openIssue(49)}

	if _, err := EpicReady(context.Background(), cfg, deps, 48); err != nil {
		t.Fatalf("EpicReady: %v", err)
	}
	if got := f.removed[48]; len(got) != 1 || got[0] != epic.ReadyLabel {
		t.Fatalf("removed %v from the epic, want [%s]", got, epic.ReadyLabel)
	}
	if got := f.added[48]; len(got) != 0 {
		t.Errorf("the epic itself must gain no label, got %v", got)
	}
}

// Promotions stand when the consume fails. They are already written to GitHub,
// and the alternative -- reporting the pass as failed -- invites a retry that
// would promote nothing and fail again.
func TestEpicReadyKeepsPromotionsWhenTheConsumeFails(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{readyEpic(48)}
	f.children[48] = []ghub.Issue{openIssue(49)}
	f.labelErr[48] = errors.New("boom")

	sum, err := EpicReady(context.Background(), cfg, deps, 48)
	if err != nil {
		t.Fatalf("EpicReady must not fail on a failed consume: %v", err)
	}
	if sum.Promoted != 1 {
		t.Errorf("Summary.Promoted = %d, want 1", sum.Promoted)
	}
}

// The ready label is not the switch -- the epic label is. Without it, anyone who
// can apply one label could sweep any issue in the repository as though it were
// an epic.
func TestEpicReadyIgnoresAnIssueThatIsNotAnEpic(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{{Number: 48, State: "open", Repo: "o/r",
		Labels: []string{epic.ReadyLabel}}}
	f.children[48] = []ghub.Issue{openIssue(49)}

	if _, err := EpicReady(context.Background(), cfg, deps, 48); err != nil {
		t.Fatalf("EpicReady: %v", err)
	}
	if got := f.subIssuesCalls; len(got) != 0 {
		t.Errorf("a non-epic must cost no sub-issue read, got %v", got)
	}
	if got := f.removed[48]; len(got) != 0 {
		t.Errorf("a non-epic's labels must not be touched, got %v", got)
	}
}

// The pass runs while the button is held down. A delivery that arrives after
// the operator took the label off again must not sweep: the re-read is the
// authority, not the delivery.
func TestEpicReadyIgnoresAnEpicNoLongerCarryingTheReadyLabel(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{epicParent(48)}
	f.children[48] = []ghub.Issue{openIssue(49)}

	if _, err := EpicReady(context.Background(), cfg, deps, 48); err != nil {
		t.Fatalf("EpicReady: %v", err)
	}
	if got := f.subIssuesCalls; len(got) != 0 {
		t.Errorf("a released button must cost no sub-issue read, got %v", got)
	}
}

// The same guard EpicSweep applies. Only one loop may promote, or the execution
// loop would push a fresh issue straight past planning.
func TestEpicReadyRefusesWhenThisLoopIsNotTheEntry(t *testing.T) {
	cfg, deps, f, gh := fixtureFor(t, "execution")
	gh.issues = []ghub.Issue{readyEpic(48)}
	f.children[48] = []ghub.Issue{openIssue(49)}

	sum, err := EpicReady(context.Background(), cfg, deps, 48)
	if err != nil {
		t.Fatalf("EpicReady: %v", err)
	}
	if sum.Promoted != 0 || len(f.promotedNumbers()) != 0 {
		t.Fatalf("the downstream loop promoted %v", f.promotedNumbers())
	}
	if got := gh.fetchedIssues; len(got) != 0 {
		t.Errorf("the guard must come before the read, got %v", got)
	}
}

// An epic in another repository is not this loop's. The sweep below writes
// labels by NUMBER against this loop's owner/repo, so a foreign epic would
// expand whichever local issue shares its number.
func TestEpicReadySkipsAnEpicInAnotherRepository(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{{Number: 48, State: "open", Repo: "other/repo",
		Labels: []string{epic.Label, epic.ReadyLabel}}}
	f.children[48] = []ghub.Issue{openIssue(49)}

	if _, err := EpicReady(context.Background(), cfg, deps, 48); err != nil {
		t.Fatalf("EpicReady: %v", err)
	}
	if got := f.subIssuesCalls; len(got) != 0 {
		t.Errorf("a foreign epic must cost no sub-issue read, got %v", got)
	}
}

// A read that fails says nothing about whether the issue is an epic, so it is
// an error rather than a silent stop -- the same reading EpicSweep gives a
// failed parent lookup.
func TestEpicReadyFailsWhenTheEpicCannotBeRead(t *testing.T) {
	cfg, deps, _, gh := sweepAllFixture(t)
	gh.issues = nil

	if _, err := EpicReady(context.Background(), cfg, deps, 48); err == nil {
		t.Fatal("an unreadable epic must be an error, not a silent stop")
	}
}
