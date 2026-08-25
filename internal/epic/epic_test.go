package epic

import (
	"reflect"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/ghub"
)

// Every helper below lives in "o/r", the repository the rule is scoped to. A
// blocker with no Repo is FOREIGN, not local, so omitting it here would quietly
// turn a blocking case into an ignored one.
func open(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, State: "open", Repo: "o/r", Labels: labels}
}

func closed(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, State: "closed", Repo: "o/r", Labels: labels}
}

// The reference loop's veto list. blocked:* is a prefix rule.
var veto = []string{"blocked:*"}

func TestPromote(t *testing.T) {
	cases := []struct {
		name     string
		children []Child
		want     []int
	}{
		{
			name:     "no blockers at all is trivially unblocked",
			children: []Child{{Issue: open(78)}},
			want:     []int{78},
		},
		{
			name: "every blocker closed",
			children: []Child{{
				Issue:    open(73),
				Blockers: []ghub.Issue{closed(71), closed(70)},
			}},
			want: []int{73},
		},
		{
			name: "one open blocker holds it",
			children: []Child{{
				Issue:    open(74),
				Blockers: []ghub.Issue{closed(73), open(72)},
			}},
			want: nil,
		},
		{
			// The property that protects work in flight. Issue #74 of the
			// reference repository sits at plan-ready-for-review with its
			// blockers closed; pulling it back would discard a plan a human
			// is reading.
			name: "a child already in the pipeline is left alone",
			children: []Child{{
				Issue:    open(74, "status:plan-ready-for-review"),
				Blockers: []ghub.Issue{closed(73)},
			}},
			want: nil,
		},
		{
			name: "a child already promoted is not promoted twice",
			children: []Child{{
				Issue:    open(74, "status:ready-for-spec"),
				Blockers: []ghub.Issue{closed(73)},
			}},
			want: nil,
		},
		{
			name: "a veto label holds it",
			children: []Child{{
				Issue:    open(74, "blocked:legal"),
				Blockers: []ghub.Issue{closed(73)},
			}},
			want: nil,
		},
		{
			name:     "a closed child is never promoted",
			children: []Child{{Issue: closed(71)}},
			want:     nil,
		},
		{
			// Failing closed. A blocker list that could not be read says
			// nothing, and the alternative reading promotes an issue whose
			// blockers may be open.
			name: "an unreadable blocker list holds it",
			children: []Child{{
				Issue:           open(74),
				BlockersUnknown: true,
			}},
			want: nil,
		},
		{
			// The operator's decision: this sweep is scoped to one repository
			// entirely, so a blocker outside it cannot hold a child back. This
			// is fail-OPEN and is the one place in this package that is.
			name: "an OPEN blocker in another repository is ignored",
			children: []Child{{
				Issue:    open(74),
				Blockers: []ghub.Issue{{Number: 9, State: "open", Repo: "other/repo"}},
			}},
			want: []int{74},
		},
		{
			// The mixed case is the one that matters: the foreign blocker is
			// skipped, and the local one still decides.
			name: "a local open blocker still holds it when a foreign one is ignored",
			children: []Child{{
				Issue: open(74),
				Blockers: []ghub.Issue{
					{Number: 9, State: "closed", Repo: "other/repo"},
					open(73),
				},
			}},
			want: nil,
		},
		{
			// Repo empty means the response did not say. It is not this
			// repository, so it is ignored like any other foreign blocker.
			name: "a blocker naming no repository is ignored",
			children: []Child{{
				Issue:    open(74),
				Blockers: []ghub.Issue{{Number: 9, State: "open"}},
			}},
			want: []int{74},
		},
		{
			// The diamond. Two blockers of one child close in the same sweep,
			// and the child appears once, not twice.
			name: "a diamond promotes the child exactly once",
			children: []Child{{
				Issue:    open(76),
				Blockers: []ghub.Issue{closed(73), closed(75)},
			}},
			want: []int{76},
		},
		{
			// Ascending, so a capped sweep takes the low-numbered batch every
			// time and the next sweep takes the next one.
			name: "results are ascending by number",
			children: []Child{
				{Issue: open(77)},
				{Issue: open(74)},
				{Issue: open(76)},
			},
			want: []int{74, 76, 77},
		},
		{
			name:     "no children promotes nothing",
			children: nil,
			want:     nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Promote(c.children, veto, "o", "r")
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Promote = %v, want %v", got, c.want)
			}
		})
	}
}

// The sweep filters before it spends a call per child. That filter must agree
// with the rule, or it saves a call by dropping a child that should have been
// promoted.
func TestNeedsBlockersAgreesWithPromote(t *testing.T) {
	cases := []struct {
		name  string
		issue ghub.Issue
		want  bool
	}{
		{name: "open, no status, no veto", issue: open(78), want: true},
		{name: "open but already in the pipeline", issue: open(74, "status:plan-ready-for-review"), want: false},
		{name: "open but vetoed", issue: open(74, "blocked:legal"), want: false},
		{name: "closed", issue: closed(71), want: false},
		{name: "open with an unrelated label", issue: open(70, "enhancement"), want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := NeedsBlockers(c.issue, veto); got != c.want {
				t.Errorf("NeedsBlockers(%d, %v) = %v, want %v", c.issue.Number, c.issue.Labels, got, c.want)
			}

			// A child with no blockers is promoted exactly when the rule says
			// it may be. If NeedsBlockers says "skip the call" for a child
			// Promote would have promoted, the sweep loses a promotion.
			promoted := len(Promote([]Child{{Issue: c.issue}}, veto, "o", "r")) == 1
			if promoted != c.want {
				t.Errorf("Promote treated issue %d %v as %v, want %v",
					c.issue.Number, c.issue.Labels,
					map[bool]string{true: "promoted", false: "declined"}[promoted],
					map[bool]string{true: "promoted", false: "declined"}[c.want])
			}
		})
	}
}
