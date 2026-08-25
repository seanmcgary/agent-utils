// Package epic decides which sub-issues of an epic a closure unblocked.
//
// Everything here is pure. It opens no socket, reads no clock, and keeps no
// state. The sweep in internal/loopcmd does the reading and the writing; the
// decision is only ever made here, so there is one place to read to know what
// the sweep will do.
package epic

import (
	"sort"

	"github.com/seanmcgary/agent-utils/internal/ghub"
)

// Label is the label a parent issue must carry for its children to be swept.
// It is the only switch: there is no configuration field, so removing this
// label from a parent is how an operator opts an epic out.
const Label = "epic"

// StatusPrefix matches every pipeline status label, using the "prefix*" rule
// ghub.Issue.HasAnyLabel implements.
//
// Carrying ANY status label is what makes a child ineligible. That is what
// makes this sweep idempotent without a single row of stored state: a promoted
// child now carries the trigger label, so the next sweep declines it, and a
// sweep that failed halfway is simply re-run.
//
// It is deliberately the whole namespace and not a list of known labels. A
// status label this program has never heard of still means "a human or an agent
// has this issue in hand", and the safe answer to an unknown state is to leave
// it alone.
const StatusPrefix = "status:*"

// Child is one sub-issue of an epic, with the blockers it declares.
type Child struct {
	// Issue is the sub-issue itself, as sub_issues returned it.
	Issue ghub.Issue
	// Blockers is what blocked_by returned for it. Empty means the issue
	// declares no dependency, which satisfies the rule.
	Blockers []ghub.Issue
	// BlockersUnknown reports that the blocker list could not be read.
	//
	// It is NOT the same as an empty list, and conflating the two is the one
	// mistake in this package that would be actively harmful: an empty list
	// means "nothing blocks this issue" and promotes it, while a failed read
	// means "this is unknown" and must not. The sweep sets this when GitHub
	// fails it, and the child is held until a later sweep can read it.
	BlockersUnknown bool
}

// Promote returns the numbers to promote, ascending.
//
// A child is promoted when all of these hold:
//   - it is open;
//   - its blocker list was read, and every blocker IN owner/repo is closed;
//   - it carries no status label;
//   - it carries none of the loop's veto labels.
//
// owner and repo scope the whole rule to one repository. See unblocked for what
// that means for a blocker outside it, and why it is the operator's decision
// rather than this package's.
//
// The result is ascending so that a capped sweep takes the low-numbered batch
// every time and the next sweep takes the next one. Without an order the batch
// identity would depend on GitHub's page order, and the same child could be
// deferred forever.
func Promote(children []Child, veto []string, owner, repo string) []int {
	var out []int
	for _, c := range children {
		if !NeedsBlockers(c.Issue, veto) {
			continue
		}
		if c.BlockersUnknown {
			continue
		}
		if !unblocked(c.Blockers, owner, repo) {
			continue
		}
		out = append(out, c.Issue.Number)
	}
	sort.Ints(out)
	return out
}

// NeedsBlockers reports whether child could be promoted if its blockers turned
// out to be closed.
//
// It is every part of the rule that can be decided WITHOUT reading the blocker
// list, and it exists so the sweep can skip a call it does not need. It is an
// optimization, never the decision: Promote tests the same conditions again, so
// a sweep that passed it a child this would have skipped still gets the right
// answer. TestNeedsBlockersAgreesWithPromote pins the two together.
func NeedsBlockers(child ghub.Issue, veto []string) bool {
	if !child.IsOpen() {
		return false
	}
	if child.HasAnyLabel([]string{StatusPrefix}) {
		return false
	}
	return !child.HasAnyLabel(veto)
}

// unblocked reports whether every blocker in owner/repo is closed. An empty
// list is unblocked: an issue that declares no dependency is waiting for
// nothing.
//
// # A blocker outside owner/repo is IGNORED, not honored
//
// GitHub lets an issue declare a blocker in another repository. This sweep
// scopes itself to one repository entirely, so such a blocker is skipped and
// cannot hold a child back.
//
// This is the operator's decision, and it is deliberately fail-OPEN, which is
// the opposite of what this package does everywhere else. The cost is real and
// worth stating plainly: a child whose only remaining blocker lives in another
// repository is promoted while that blocker is still open, and planning starts
// on work whose prerequisite is not done. The reasoning for accepting it is
// that a loop watches one repository, its labels mean nothing outside that
// repository, and honoring a dependency the loop can neither see change nor
// act on makes the sweep's behavior depend on a repository nobody here
// administers.
//
// It also removes a failure this design could not otherwise detect: a blocker
// in a repository the token cannot read may be OMITTED from the response
// rather than reported, and an omitted blocker is indistinguishable from one
// that was never declared. Every such blocker is foreign by definition, so
// ignoring foreign blockers makes that case decided rather than silent.
func unblocked(blockers []ghub.Issue, owner, repo string) bool {
	for _, b := range blockers {
		if !b.InRepo(owner, repo) {
			continue
		}
		if b.IsOpen() {
			return false
		}
	}
	return true
}
