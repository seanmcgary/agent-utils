package listener

import (
	"sort"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/epic"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// pollSubject is one issue or pull request as a pass sees it NOW, after the
// listing and, for a pull request that just closed, the single fetch that says
// whether it merged.
type pollSubject struct {
	Number int
	IsPR   bool
	// State is "open" or "closed", as GitHub spells it.
	State  string
	Merged bool
	// MergedBase is the branch a merged pull request landed on, and is empty
	// for everything else.
	MergedBase string
	Labels     []string
	UpdatedAt  time.Time
}

// labelKey reduces a label set to a value that changes when the SET changes and
// not when its order or casing does. GitHub returns labels in an order nothing
// promises to keep stable, and a poll that treated a reshuffle as a change
// would dispatch an agent for it.
func labelKey(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, strings.ToLower(l))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// row is what this subject should be remembered as.
func (p pollSubject) row(repo string) store.PollSubject {
	return store.PollSubject{
		Repo: repo, Number: p.Number, IsPR: p.IsPR, State: p.State,
		Merged: p.Merged, Labels: labelKey(p.Labels), UpdatedAt: p.UpdatedAt,
	}
}

// isOpen reports whether a GitHub state string says open, ignoring case, as
// ghub.Issue.IsOpen does.
func isOpen(state string) bool { return strings.EqualFold(state, "open") }

// pollDelivery turns "here is what this subject was, here is what it is now"
// into the delivery a webhook would have sent, or reports that nothing
// happened.
//
// It is pure, and it is the whole of what this feature decides. Everything
// around it -- the listing, the cursor, the snapshot writes, the ticker, the
// command -- is plumbing that exists to call this function with the right two
// arguments.
//
// known is not derivable from prev. A zero PollSubject is a real possible
// state, and "we have never seen this number" must produce a tick while "we
// saw it and nothing moved" must produce nothing.
func pollDelivery(repo string, prev store.PollSubject, known bool, cur pollSubject) (Delivery, bool) {
	d := Delivery{Repo: repo, Number: cur.Number}

	if !known {
		// A first sighting is a plain tick and nothing more. The transition
		// flags below all describe a CHANGE, and there is no previous state
		// to have changed from -- a number first seen already closed says
		// nothing about when it closed, so arming an epic sweep off it would
		// sweep on the poller's schedule rather than the issue's.
		return d, true
	}

	changed := !cur.UpdatedAt.Equal(prev.UpdatedAt) || labelKey(cur.Labels) != prev.Labels

	switch {
	case isOpen(prev.State) && !isOpen(cur.State):
		changed = true
		// Narrowed by kind, exactly as the webhook handler narrows by event:
		// issues and pull requests share a number space, so a closed pull
		// request setting ClosedIssue would sweep the epic of whichever ISSUE
		// carries its number.
		if cur.IsPR {
			d.ClosedPR = true
			if cur.Merged {
				d.MergedInto = cur.MergedBase
			}
		} else {
			d.ClosedIssue = true
		}
	case !isOpen(prev.State) && isOpen(cur.State):
		changed = true
		d.Reopened = true
	}

	// APPLYING the label presses the button; removing it does not. Issues
	// only, for the number-space reason above.
	//
	// The previous set is tested by MEMBERSHIP, not with strings.Contains
	// against the joined key: "status:epic-ready" is a substring of
	// "status:epic-ready-later", and a substring test would silently swallow
	// the sweep an operator had just asked for.
	if !cur.IsPR && hasLabel(cur.Labels, epic.ReadyLabel) && !keyHasLabel(prev.Labels, epic.ReadyLabel) {
		changed = true
		d.EpicReady = true
	}

	if !changed {
		return Delivery{}, false
	}
	return d, true
}

// hasLabel reports whether labels carries name, ignoring case.
func hasLabel(labels []string, name string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

// keyHasLabel reports whether a stored label key -- the lowercased, sorted,
// comma-joined form labelKey produces -- carries name.
func keyHasLabel(key, name string) bool {
	if key == "" {
		return false
	}
	return hasLabel(strings.Split(key, ","), name)
}
