package engine

import (
	"strconv"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/project"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// tendConfig is what config.LoadTend synthesises: the reserved name, the
// project's eligibility label, and the tend agent. It carries no veto list and
// no meaningful loop labels, exactly as the real one does not.
func tendConfig() *config.Config {
	return &config.Config{
		Name:  project.Reserved,
		Repo:  "o/r",
		Agent: config.Agent{Model: "sonnet", Worktree: config.WorktreePerIssue},
		Tend: project.Tend{
			Enabled: true,
			Label:   "status:ready-for-review",
			Model:   "sonnet",
		},
	}
}

func tendKinds(p TendPlan) []Kind {
	out := make([]Kind, 0, len(p.Decisions))
	for _, d := range p.Decisions {
		out = append(out, d.Kind)
	}
	return out
}

func trustedPR(number, issue int) ghub.PullRequest {
	return ghub.PullRequest{
		Number: number, Body: "Closes #" + strconv.Itoa(issue),
		HeadRef: "feat/a", BaseRef: "master", Trusted: true,
	}
}

func TestTendsStalePullRequest(t *testing.T) {
	cfg := tendConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Tend.Label)},
		PRs:      []ghub.PullRequest{trustedPR(20, 1)},
		BehindBy: map[int]int{20: 3},
	}
	p := DecideTend(cfg, snap, TendState{})
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindTend {
		t.Fatalf("decisions = %v, want one tend", tendKinds(p))
	}
	if p.Decisions[0].PR != 20 || p.Decisions[0].Issue != 1 {
		t.Errorf("decision = %+v, want issue 1 / PR 20", p.Decisions[0])
	}
}

func TestDoesNotTendCurrentPullRequest(t *testing.T) {
	cfg := tendConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Tend.Label)},
		PRs:      []ghub.PullRequest{trustedPR(20, 1)},
		BehindBy: map[int]int{20: 0},
	}
	p := DecideTend(cfg, snap, TendState{})
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none for a current pull request", tendKinds(p))
	}
}

// An issue without the eligibility label is not a candidate and gets no skip
// reason either. This pass walks every open issue in the repository, so a
// reason per issue would bury the handful that mean something.
func TestIssueWithoutTheTendLabelIsNotEvenConsidered(t *testing.T) {
	cfg := tendConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, "status:executing")},
		PRs:      []ghub.PullRequest{trustedPR(20, 1)},
		BehindBy: map[int]int{20: 5},
	}
	p := DecideTend(cfg, snap, TendState{})
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none", tendKinds(p))
	}
	if got := p.NoDecisionReason(1); got != "" {
		t.Errorf("skip reason = %q, want none for an issue that is not a candidate", got)
	}
}

// A live agent ANYWHERE in the project suppresses tending of its issue's
// branch. It is the whole reason TendState's guards are project-wide: the
// agent a tend must not collide with belongs to the loop that wrote the
// branch, and the dispatcher cannot see that loop's rows through its own name.
func TestLiveIssueDispatchAnywhereSuppressesTend(t *testing.T) {
	cfg := tendConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Tend.Label)},
		PRs:      []ghub.PullRequest{trustedPR(20, 1)},
		BehindBy: map[int]int{20: 3},
	}
	p := DecideTend(cfg, snap, TendState{LiveIssues: map[int]bool{1: true}})
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none while an agent holds the issue", tendKinds(p))
	}
	if p.NoDecisionReason(1) == "" {
		t.Error("a suppressed tend must say why")
	}
}

func TestDoesNotTendWhileATendDispatchIsLiveForThePR(t *testing.T) {
	cfg := tendConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Tend.Label)},
		PRs:      []ghub.PullRequest{trustedPR(20, 1)},
		BehindBy: map[int]int{20: 3},
	}
	p := DecideTend(cfg, snap, TendState{LiveTendPRs: map[int]bool{20: true}})
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none while a tend is live", tendKinds(p))
	}
}

// An operator who stopped an issue's session in ANY loop meant "run no more
// agents at this issue". A tend is one of that issue's agents, and without the
// project-wide read it would force-push the branch of the session they just
// killed -- the dispatcher's own scope is always clean.
func TestStoppedIssueIsNotTended(t *testing.T) {
	cfg := tendConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Tend.Label)},
		PRs:      []ghub.PullRequest{trustedPR(20, 1)},
		BehindBy: map[int]int{20: 3},
	}
	p := DecideTend(cfg, snap, TendState{Stopped: map[int]string{1: "killed by the operator"}})
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none: a stopped issue must not be tended", tendKinds(p))
	}
	if got := p.NoDecisionReason(1); got == "" {
		t.Error("a stopped issue must carry its reason")
	}
}

// A DRAFT is the author's working copy: nobody is blocked by it being behind,
// no reviewer waits on a reply, and force-pushing a rebase under someone still
// assembling the branch is the one thing tending must never do.
func TestTendSkipsADraftPullRequest(t *testing.T) {
	cfg := tendConfig()
	pr := trustedPR(20, 1)
	pr.Draft = true
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Tend.Label)},
		PRs:      []ghub.PullRequest{pr},
		BehindBy: map[int]int{20: 3},
	}
	p := DecideTend(cfg, snap, TendState{})
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none for a draft", tendKinds(p))
	}
	want := "the linked pull request is still a draft"
	if got := p.NoDecisionReason(1); got != want {
		t.Errorf("skip reason = %q, want %q", got, want)
	}
}

// A tend NEVER inherits a session. It used to, so a rebase agent carried the
// context of the work it was rebasing; three things removed the reason.
//
// A clean rebase no longer runs an agent at all, so what is left for one is a
// conflict or a review reply -- both fully described by the branch, the hunks
// and the thread. Inheriting BLOCKED the issue, because two processes on one
// session identifier is the same hazard as two agents in one branch. And
// tending is its own dispatcher now, which keeps no issue state at all, so
// "the issue's session" names one conversation per LOOP and none of them is
// this dispatcher's.
func TestTendAlwaysStartsItsOwnSession(t *testing.T) {
	cfg := tendConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Tend.Label)},
		PRs:      []ghub.PullRequest{trustedPR(20, 1)},
		BehindBy: map[int]int{20: 3},
	}
	p := DecideTend(cfg, snap, TendState{})
	if len(p.Decisions) != 1 {
		t.Fatalf("decisions = %v, want one tend", tendKinds(p))
	}
	if p.Decisions[0].SessionID != "" {
		t.Errorf("SessionID = %q, want empty so dispatch mints a fresh one",
			p.Decisions[0].SessionID)
	}
}

// The issue's overrides still reach a tend: a model: or harness: label is the
// operator saying "run THIS issue's agents like so", and a tend is one of that
// issue's agents.
func TestTendCarriesTheIssuesOverrides(t *testing.T) {
	cfg := tendConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Tend.Label, "harness:pi", "model:opus")},
		PRs:      []ghub.PullRequest{trustedPR(20, 1)},
		BehindBy: map[int]int{20: 3},
	}
	p := DecideTend(cfg, snap, TendState{})
	if len(p.Decisions) != 1 {
		t.Fatalf("decisions = %v, want one tend", tendKinds(p))
	}
	d := p.Decisions[0]
	if d.Overrides.Harness != "pi" || d.Overrides.Model != "opus" {
		t.Errorf("overrides = %+v, want the issue's", d.Overrides)
	}
}

// An override the dispatcher cannot parse SKIPS the tend rather than stopping
// the issue. A stale rebase is not the issue's own work, and the loop that
// owns the issue already stops it where that work would happen.
func TestTendIsSkippedWhenAnOverrideLabelIsInvalid(t *testing.T) {
	cfg := tendConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Tend.Label, "harness:nope")},
		PRs:      []ghub.PullRequest{trustedPR(20, 1)},
		BehindBy: map[int]int{20: 3},
	}
	p := DecideTend(cfg, snap, TendState{})
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none for an unparsable override", tendKinds(p))
	}
	if p.NoDecisionReason(1) == "" {
		t.Error("the skip must carry the override's own error")
	}
}

func TestTendSkipsAnIssueWithNoTrustedPullRequest(t *testing.T) {
	cfg := tendConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Tend.Label)}}
	p := DecideTend(cfg, snap, TendState{})
	want := "the issue carries the tend label and no trusted pull request is linked"
	if got := p.NoDecisionReason(1); got != want {
		t.Errorf("skip reason = %q, want %q", got, want)
	}
}

func TestReviewActivityNewerThanLastTendProducesAReviewPendingTend(t *testing.T) {
	cfg := tendConfig()
	now := time.Now()
	snap := Snapshot{
		Issues:     []ghub.Issue{issue(1, cfg.Tend.Label)},
		PRs:        []ghub.PullRequest{trustedPR(20, 1)},
		BehindBy:   map[int]int{20: 0},
		ReviewedAt: map[int]time.Time{20: now},
	}
	st := TendState{LastTend: map[int]time.Time{20: now.Add(-time.Hour)}}
	p := DecideTend(cfg, snap, st)
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindTend {
		t.Fatalf("decisions = %v, want one tend", tendKinds(p))
	}
	if !p.Decisions[0].ReviewPending {
		t.Error("ReviewPending = false, want true: review activity is newer than the last tend")
	}
}

// The same pull request with the last tend NEWER than the review activity must
// produce no decision, and the skip reason must name both halves of the
// question.
func TestReviewActivityOlderThanLastTendProducesNoDecision(t *testing.T) {
	cfg := tendConfig()
	now := time.Now()
	snap := Snapshot{
		Issues:     []ghub.Issue{issue(1, cfg.Tend.Label)},
		PRs:        []ghub.PullRequest{trustedPR(20, 1)},
		BehindBy:   map[int]int{20: 0},
		ReviewedAt: map[int]time.Time{20: now.Add(-time.Hour)},
	}
	p := DecideTend(cfg, snap, TendState{LastTend: map[int]time.Time{20: now}})
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none: the last tend is newer", tendKinds(p))
	}
	want := "the linked pull request is up to date with its base and carries no review activity since the last tend"
	if got := p.NoDecisionReason(1); got != want {
		t.Errorf("skip reason = %q, want %q", got, want)
	}
}

// A behind pull request with no review activity still produces a decision, and
// ReviewPending must be false: the staleness trigger alone must not be mistaken
// for the review trigger.
func TestBehindPullRequestWithNoReviewActivityIsNotReviewPending(t *testing.T) {
	cfg := tendConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Tend.Label)},
		PRs:      []ghub.PullRequest{trustedPR(20, 1)},
		BehindBy: map[int]int{20: 3},
	}
	p := DecideTend(cfg, snap, TendState{})
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindTend {
		t.Fatalf("decisions = %v, want one tend", tendKinds(p))
	}
	if p.Decisions[0].ReviewPending {
		t.Error("ReviewPending = true, want false: no review activity was reported")
	}
}

// A pull request with no prior tend and review activity is pending: LastTend
// absent from the map reads as the zero time, and any review activity is after
// that.
func TestReviewActivityWithNoPriorTendIsPending(t *testing.T) {
	cfg := tendConfig()
	now := time.Now()
	snap := Snapshot{
		Issues:     []ghub.Issue{issue(1, cfg.Tend.Label)},
		PRs:        []ghub.PullRequest{trustedPR(20, 1)},
		BehindBy:   map[int]int{20: 0},
		ReviewedAt: map[int]time.Time{20: now},
	}
	p := DecideTend(cfg, snap, TendState{})
	if len(p.Decisions) != 1 || !p.Decisions[0].ReviewPending {
		t.Fatalf("decisions = %v, want one review-pending tend", tendKinds(p))
	}
}

// Decisions come back in issue order, because a capped sweep takes the
// low-numbered batch every time and the next sweep takes the next one.
func TestTendDecisionsAreOrderedByIssueNumber(t *testing.T) {
	cfg := tendConfig()
	snap := Snapshot{
		Issues: []ghub.Issue{
			issue(9, cfg.Tend.Label), issue(2, cfg.Tend.Label), issue(5, cfg.Tend.Label),
		},
		PRs: []ghub.PullRequest{
			trustedPR(90, 9), trustedPR(20, 2), trustedPR(50, 5),
		},
		BehindBy: map[int]int{90: 1, 20: 1, 50: 1},
	}
	p := DecideTend(cfg, snap, TendState{})
	want := []int{2, 5, 9}
	if len(p.Decisions) != 3 {
		t.Fatalf("decisions = %v, want three", tendKinds(p))
	}
	for i, d := range p.Decisions {
		if d.Issue != want[i] {
			t.Errorf("position %d = issue %d, want %d", i, d.Issue, want[i])
		}
	}
}

// TendLiveness is the rule, not plumbing: a KindTend row blocks its PULL
// REQUEST and every other kind blocks its ISSUE. Getting it backwards would
// either let two agents into one branch or stop tending entirely, and neither
// failure announces itself.
func TestTendLivenessSplitsByKind(t *testing.T) {
	issues, tends := TendLiveness([]store.Dispatch{
		{Number: 1, Kind: store.KindStart},
		{Number: 2, PRNumber: 22, Kind: store.KindTend},
	})
	if !issues[1] || issues[2] {
		t.Errorf("liveIssues = %v, want only the non-tend row's issue", issues)
	}
	if !tends[22] || len(tends) != 1 {
		t.Errorf("liveTendPRs = %v, want only the tend row's pull request", tends)
	}
}
