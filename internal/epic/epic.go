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

// ReadyLabel is the label an operator applies to an epic to say "the graph is
// entered; sweep it now". It answers the one case a closure cannot: the FIRST
// promotion, before any sub-issue has ever closed.
//
// It is a separate label from Label, and the separation is the whole point.
// Applying Label is how an issue BECOMES an epic, which happens before its
// sub-issues and dependencies exist -- a sweep armed on it would walk an empty
// epic and promote nothing. This one is applied last, when the graph is ready,
// which is the moment the sweep has something to decide.
//
// It is CONSUMED: the sweep removes it. GitHub sends no delivery for applying a
// label that is already present, so a label left in place could never arm a
// second sweep, and an operator who adds children later would have no way to
// press it again. Removing it makes the label a button rather than a state.
//
// It lives in the status: namespace deliberately, so an epic that is itself
// some other epic's sub-issue is held by StatusPrefix while its own button is
// pressed, rather than being promoted mid-sweep.
//
// Hard-coded, like Label and for the same reason: there is no configuration
// field, so the label an operator applies is the same one in every project.
const ReadyLabel = "status:epic-ready"

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

// Rule is everything Promote needs besides the children themselves.
//
// It is a struct rather than positional owner, repo, trigger string
// arguments because three same-typed strings in a row is exactly the
// transposition hazard this branch already spent two review rounds
// guarding against: a caller that swapped owner and trigger would compile
// and silently label the wrong issue.
type Rule struct {
	// Veto holds the loop's veto label rules ("prefix*" supported).
	Veto []string
	// Owner and Repo scope the sweep. A blocker outside them is ignored; a
	// child outside them is never promoted.
	Owner, Repo string
	// Trigger is the label a promotion adds. A child already carrying it is
	// ineligible, which is what makes a re-run promote nothing WITHOUT
	// depending on Trigger being inside the status: namespace. StatusPrefix
	// covers the conventional case; this covers every other one -- an
	// operator whose trigger label does not start with "status:" is still
	// protected from unbounded re-promotion.
	Trigger string
}

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
//   - its blocker list was read, and every blocker IN r.Owner/r.Repo is closed;
//   - it carries no status label;
//   - it does not already carry r.Trigger;
//   - it carries none of the loop's veto labels.
//
// r.Owner and r.Repo scope the whole rule to one repository. See unblocked for
// what that means for a blocker outside it, and why it is the operator's
// decision rather than this package's.
//
// The result is ascending so that a capped sweep takes the low-numbered batch
// every time and the next sweep takes the next one. Without an order the batch
// identity would depend on GitHub's page order, and the same child could be
// deferred forever.
func Promote(children []Child, r Rule) []int {
	var out []int
	for _, c := range children {
		if !NeedsBlockers(c.Issue, r) {
			continue
		}
		if c.BlockersUnknown {
			continue
		}
		if !unblocked(c.Blockers, r.Owner, r.Repo) {
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
func NeedsBlockers(child ghub.Issue, r Rule) bool {
	if !child.IsOpen() {
		return false
	}
	if child.HasAnyLabel([]string{StatusPrefix}) {
		return false
	}
	// A child already carrying the trigger label is ineligible. StatusPrefix
	// covers the conventional case where Trigger lives inside the status:
	// namespace; this covers every other one, so idempotence does not rest on
	// an operator's naming convention. Without it, a trigger label outside
	// status:* would be re-selected by every sweep -- spending budget on an
	// already-promoted child, and, combined with the retry-cap park removing
	// a same-namespace blocked label, an unbounded park/re-promote/dispatch
	// cycle.
	if child.HasLabel(r.Trigger) {
		return false
	}
	return !child.HasAnyLabel(r.Veto)
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
