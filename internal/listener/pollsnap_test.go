package listener

import (
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/epic"
	"github.com/seanmcgary/agent-utils/internal/store"
)

func at(h int) time.Time { return time.Date(2026, 9, 15, h, 0, 0, 0, time.UTC) }

func TestPollDelivery(t *testing.T) {
	openIssue := store.PollSubject{
		Repo: "o/r", Number: 51, State: "open", Labels: "status:ready", UpdatedAt: at(10),
	}
	openPR := store.PollSubject{
		Repo: "o/r", Number: 52, IsPR: true, State: "open", UpdatedAt: at(10),
	}

	cases := []struct {
		name  string
		prev  store.PollSubject
		known bool
		cur   pollSubject
		want  Delivery
		none  bool
	}{
		{
			name:  "a number never seen before is a plain tick",
			known: false,
			cur:   pollSubject{Number: 51, State: "open", UpdatedAt: at(10)},
			want:  Delivery{Repo: "o/r", Number: 51},
		},
		{
			// known is the sole authority for "never seen before", not
			// whether prev happens to be the zero value. This prev is
			// non-zero (open, updated at 10) yet known is false: a number
			// first seen already closed says nothing about WHEN it closed,
			// so the correct implementation must still yield a plain tick,
			// not arm ClosedIssue off a transition it cannot have observed.
			// Arming an epic sweep here would sweep on the poller's own
			// schedule rather than the issue's.
			//
			// This state is not reachable through pollRepo today, where a
			// map miss always yields a zero prev -- this test pins the
			// function's contract so a future caller cannot violate it
			// silently.
			name:  "known false with a non-zero prev is still a plain tick",
			prev:  store.PollSubject{Repo: "o/r", Number: 51, State: "open", UpdatedAt: at(10)},
			known: false,
			cur:   pollSubject{Number: 51, State: "closed", UpdatedAt: at(11)},
			want:  Delivery{Repo: "o/r", Number: 51},
		},
		{
			name: "an unchanged subject re-read at the inclusive boundary delivers nothing",
			prev: openIssue, known: true,
			cur:  pollSubject{Number: 51, State: "open", Labels: []string{"status:ready"}, UpdatedAt: at(10)},
			none: true,
		},
		{
			name:  "a reordered label set is not a change",
			prev:  store.PollSubject{Repo: "o/r", Number: 51, State: "open", Labels: "a,b", UpdatedAt: at(10)},
			known: true,
			cur:   pollSubject{Number: 51, State: "open", Labels: []string{"B", "A"}, UpdatedAt: at(10)},
			none:  true,
		},
		{
			name: "a moved updated_at is a plain tick",
			prev: openIssue, known: true,
			cur:  pollSubject{Number: 51, State: "open", Labels: []string{"status:ready"}, UpdatedAt: at(11)},
			want: Delivery{Repo: "o/r", Number: 51},
		},
		{
			name: "a label change with no updated_at move is still a tick",
			prev: openIssue, known: true,
			cur:  pollSubject{Number: 51, State: "open", Labels: []string{"status:blocked"}, UpdatedAt: at(10)},
			want: Delivery{Repo: "o/r", Number: 51},
		},
		{
			name: "an issue that closed arms the epic sweep",
			prev: openIssue, known: true,
			cur:  pollSubject{Number: 51, State: "closed", Labels: []string{"status:ready"}, UpdatedAt: at(11)},
			want: Delivery{Repo: "o/r", Number: 51, ClosedIssue: true},
		},
		{
			name: "a pull request that closed arms worktree cleanup, not the epic sweep",
			prev: openPR, known: true,
			cur:  pollSubject{Number: 52, IsPR: true, State: "closed", UpdatedAt: at(11)},
			want: Delivery{Repo: "o/r", Number: 52, ClosedPR: true},
		},
		{
			name: "a merged pull request also names the branch it landed on",
			prev: openPR, known: true,
			cur: pollSubject{
				Number: 52, IsPR: true, State: "closed", Merged: true,
				MergedBase: "master", UpdatedAt: at(11),
			},
			want: Delivery{Repo: "o/r", Number: 52, ClosedPR: true, MergedInto: "master"},
		},
		{
			name:  "a reopen erases the recorded closure",
			prev:  store.PollSubject{Repo: "o/r", Number: 51, State: "closed", UpdatedAt: at(10)},
			known: true,
			cur:   pollSubject{Number: 51, State: "open", UpdatedAt: at(11)},
			want:  Delivery{Repo: "o/r", Number: 51, Reopened: true},
		},
		{
			name: "the epic ready label appearing on an issue arms the requested sweep",
			prev: openIssue, known: true,
			cur: pollSubject{
				Number: 51, State: "open",
				Labels: []string{"status:ready", epic.ReadyLabel}, UpdatedAt: at(11),
			},
			want: Delivery{Repo: "o/r", Number: 51, EpicReady: true},
		},
		{
			name: "the epic ready label REMOVED presses no button",
			prev: store.PollSubject{
				Repo: "o/r", Number: 51, State: "open", Labels: epic.ReadyLabel, UpdatedAt: at(10),
			},
			known: true,
			cur:   pollSubject{Number: 51, State: "open", UpdatedAt: at(11)},
			want:  Delivery{Repo: "o/r", Number: 51},
		},
		{
			// Issues and pull requests share a number space. EpicReady set from
			// a pull request would sweep the epic of whichever ISSUE carries
			// that number -- the bug the webhook handler narrows by event.
			name: "the epic ready label on a pull request presses no button",
			prev: openPR, known: true,
			cur: pollSubject{
				Number: 52, IsPR: true, State: "open",
				Labels: []string{epic.ReadyLabel}, UpdatedAt: at(11),
			},
			want: Delivery{Repo: "o/r", Number: 52},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := pollDelivery("o/r", c.prev, c.known, c.cur)
			if c.none {
				if ok {
					t.Fatalf("delivered %+v, want nothing", got)
				}
				return
			}
			if !ok {
				t.Fatal("delivered nothing, want a delivery")
			}
			if got != c.want {
				t.Errorf("delivery = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestLabelKeyIsOrderAndCaseIndependent(t *testing.T) {
	if a, b := labelKey([]string{"B", "a"}), labelKey([]string{"A", "b"}); a != b {
		t.Errorf("labelKey disagrees on the same set: %q vs %q", a, b)
	}
	if labelKey(nil) != "" {
		t.Errorf("labelKey(nil) = %q, want empty", labelKey(nil))
	}
}

// row is what Task 4 persists after a pass: the labels a pollSubject carries
// reduced to the same joined key a stored PollSubject compares against, so a
// later pass's labelKey(cur.Labels) != prev.Labels test lines up with what an
// earlier pass wrote.
func TestPollSubjectRow(t *testing.T) {
	p := pollSubject{
		Number: 51, IsPR: true, State: "closed", Merged: true,
		MergedBase: "master", Labels: []string{"B", "a"}, UpdatedAt: at(11),
	}
	want := store.PollSubject{
		Repo: "o/r", Number: 51, IsPR: true, State: "closed",
		Merged: true, Labels: "a,b", UpdatedAt: at(11),
	}
	if got := p.row("o/r"); got != want {
		t.Errorf("row = %+v, want %+v", got, want)
	}
}

// The previous label set is stored joined; a membership test done by substring
// would see "status:epic-ready" inside "status:epic-ready-later" and suppress
// the sweep the operator just asked for.
func TestEpicReadyIsMembershipNotSubstring(t *testing.T) {
	prev := store.PollSubject{
		Repo: "o/r", Number: 51, State: "open",
		Labels: "status:epic-ready-later", UpdatedAt: at(10),
	}
	cur := pollSubject{
		Number: 51, State: "open",
		Labels:    []string{"status:epic-ready-later", epic.ReadyLabel},
		UpdatedAt: at(11),
	}
	got, ok := pollDelivery("o/r", prev, true, cur)
	if !ok || !got.EpicReady {
		t.Fatalf("delivery = %+v (ok=%v), want EpicReady", got, ok)
	}
}
