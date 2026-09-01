package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// TendState is the stored view one tend pass decides from.
//
// It is a separate type from State, and the difference is the whole reason the
// tend dispatcher exists. State is scoped to ONE loop: its issue rows, its
// running dispatches, its cooldown. The three fields below that guard against
// two agents in one branch are deliberately PROJECT-wide, because the agent a
// tend must not collide with belongs to some other loop entirely -- the one
// that wrote the branch. A tend reading only its own rows would see an empty
// project and force-push under a live agent every time.
type TendState struct {
	// LiveIssues holds every issue number with a live dispatch in ANY loop of
	// this project. An agent working the branch and a tend agent force-pushing
	// it are the same hazard as two agents, and the working agent flips its own
	// labels asynchronously, so the eligibility label can still be present
	// while it runs. The caller performs the liveness check, so this function
	// stays pure.
	LiveIssues map[int]bool

	// LiveTendPRs holds every pull request number with a live TEND dispatch.
	// Keyed by pull request rather than issue because that is what a tend acts
	// on, and two tends on one branch is the same hazard again.
	LiveTendPRs map[int]bool

	// Stopped maps an issue number to why it was stopped, in any loop of this
	// project. An operator who stopped an issue's session meant "run no more
	// agents at this issue", and a tend is one of that issue's agents: without
	// this a stopped issue with a behind pull request would get a tend agent
	// force-pushing the branch of the session the operator just killed.
	Stopped map[int]string

	// LastTend maps a PULL REQUEST number to the start time of its last
	// FINISHED tend dispatch, so review activity newer than it counts as
	// unanswered. Keyed by pull request, like Snapshot.ReviewedAt, because both
	// are facts about the pull request rather than about the issue that links
	// to it. A pull request absent from this map has never had a finished tend,
	// which is why any review activity on it counts as pending.
	LastTend map[int]time.Time
}

// TendPlan is what one tend pass decided.
//
// It is not Plan. Plan carries a breaker verdict and a cooldown, and a tend
// pass produces neither: it makes no retry decision, so it counts no eligible
// retries, and a pass that will not act on that evidence must not stop the
// passes that would.
type TendPlan struct {
	Decisions []Decision
	// Skips explains, per issue number, why an eligible-looking issue produced
	// no decision -- the same contract Plan.Skips has, for the same reason. An
	// operator reading "nothing happened" needs to know whether the pull
	// request was current, already being tended, a draft, or not linked at all.
	Skips map[int]string
}

// NoDecisionReason returns why issue got no decision this pass, or "" when it
// got one.
func (p TendPlan) NoDecisionReason(issue int) string { return p.Skips[issue] }

// DecideTend selects the pull requests this pass must tend. It is pure: it
// reads only its arguments, performs no input or output, and reads no clock.
//
// cfg is the tend dispatcher's own configuration -- config.LoadTend's -- so
// cfg.Name is the reserved name every row it writes is keyed by, and
// cfg.Tend.Label is the project's eligibility label.
//
// There is no veto list here, and its absence is a decision rather than an
// omission. Tending used to run inside a host loop and inherited that loop's
// labels.veto; with no host there is no one loop's list to inherit, and the
// union of every loop's list is worse than useless -- the lists name one
// another's states, so a union vetoes every status label the pipeline has,
// including the eligibility label itself. What gates a tend now is the
// eligibility label, the draft check, and the two project-wide guards in
// TendState.
//
// A tend decision carries no resolved Provider. It makes no retry decision and
// can never retire a cap, and loopcmd skips BeginDispatch entirely for a tend,
// so there is nothing for the value to reach.
func DecideTend(cfg *config.Config, snap Snapshot, st TendState) TendPlan {
	issues := append([]ghub.Issue(nil), snap.Issues...)
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })

	plan := TendPlan{Skips: map[int]string{}}
	for _, iss := range issues {
		if !iss.HasLabel(cfg.Tend.Label) {
			// Not in the tendable state at all. No skip reason: this pass looks
			// at every open issue in the repository, and recording one per
			// issue would bury the handful that mean something.
			continue
		}
		if st.LiveIssues[iss.Number] {
			plan.Skips[iss.Number] = "a dispatch is already live for this issue in this project"
			continue
		}
		if reason, stopped := st.Stopped[iss.Number]; stopped {
			plan.Skips[iss.Number] = stoppedSkipReason(reason)
			continue
		}
		pr, ok := LinkPR(iss.Number, snap.PRs)
		if !ok {
			plan.Skips[iss.Number] = "the issue carries the tend label and no trusted pull request is linked"
			continue
		}
		// A DRAFT is not ready to be maintained. It is the author's working
		// copy: nobody is blocked by it being behind, no reviewer is waiting on
		// a reply, and force-pushing a rebase under someone still assembling
		// the branch is the one thing tending must never do. The snapshot's
		// pull requests are already filtered to OPEN by ListOpenPullRequests,
		// so open-and-not-draft is the whole readiness test.
		if pr.Draft {
			plan.Skips[iss.Number] = "the linked pull request is still a draft"
			continue
		}
		if st.LiveTendPRs[pr.Number] {
			plan.Skips[iss.Number] = "a tend dispatch is already live for the linked pull request"
			continue
		}
		behind := snap.BehindBy[pr.Number] > 0
		reviewPending := snap.ReviewedAt[pr.Number].After(st.LastTend[pr.Number])
		if !behind && !reviewPending {
			// Silence is correct only when BOTH questions came back no. Naming
			// both in the skip reason is what lets an operator reading "nothing
			// happened" tell which one is false, rather than assuming staleness
			// was the only thing ever checked.
			plan.Skips[iss.Number] = "the linked pull request is up to date with its base and carries no review activity since the last tend"
			continue
		}
		// The issue's overrides still apply to a tend: a model: or harness:
		// label on an issue is the operator saying "run THIS issue's agents
		// like so", and a tend is one of that issue's agents.
		//
		// An override the dispatcher cannot parse skips the tend rather than
		// stopping the issue. A stale rebase is not the issue's own work, and
		// the loop that owns the issue already stops it where that work would
		// happen.
		ov, ovErr := config.ParseOverrides(iss.Labels)
		if ovErr != nil {
			plan.Skips[iss.Number] = ovErr.Error()
			continue
		}
		// Both halves of the reason can hold at once -- a pull request can be
		// behind AND carry unanswered review activity -- so both are named
		// when true, keeping the existing "N commits behind" wording for the
		// half that already had it.
		var reasons []string
		if behind {
			reasons = append(reasons, fmt.Sprintf("%s is %d commits behind",
				describeLink(iss.Number, pr), snap.BehindBy[pr.Number]))
		}
		if reviewPending {
			reasons = append(reasons, fmt.Sprintf("%s carries review activity newer than the last tend",
				describeLink(iss.Number, pr)))
		}
		plan.Decisions = append(plan.Decisions, Decision{
			Kind:    KindTend,
			Issue:   iss.Number,
			PR:      pr.Number,
			HeadRef: pr.HeadRef,
			BaseRef: pr.BaseRef,
			// A TEND GETS ITS OWN SESSION. An empty identifier is what tells
			// loopcmd.dispatch to mint a fresh one, and there is nothing left
			// that could be inherited instead: the issue's session belongs to
			// whichever loop wrote the branch, and this dispatcher is not that
			// loop and keeps no issue state of its own.
			//
			// It used to inherit, so a rebase agent carried the context of the
			// work it was rebasing rather than meeting the branch cold. Three
			// things removed the reason:
			//
			//  1. A clean rebase no longer runs an agent at all -- it is done
			//     in Go. What is left for the agent is a genuine conflict, or a
			//     reply to review activity. Both are fully described by the
			//     branch, the conflict hunks and the pull request thread;
			//     neither needs the conversation that originally wrote the code.
			//  2. Inheriting BLOCKED the issue. Two processes resuming one
			//     session identifier is the same hazard as two agents in one
			//     branch, so a live tend had to hold the issue's session and
			//     stop it dispatching -- a real cost paid for context that was
			//     rarely read.
			//  3. Tending is its own project-level dispatcher. "The issue's
			//     session" names one conversation per LOOP, and this dispatcher
			//     is none of them, so inheritance would mean inventing a rule
			//     for whose session to take -- wrong as often as right once
			//     several loops have touched an issue.
			//
			// A fresh session per tend also keeps maintenance work out of the
			// authoring session's context, which otherwise grows on every
			// rebase for the life of the pull request. Continuity across REPEAT
			// tends of one pull request is what actually matters -- it is what
			// the repeat-conflict backoff reasons about -- and that lives in
			// the store, keyed by pull request, not in a resumed conversation.
			SessionID:     "",
			Reason:        strings.Join(reasons, "; "),
			Overrides:     ov,
			ReviewPending: reviewPending,
		})
	}
	return plan
}

// TendLiveness splits a project's running dispatch rows into the two guards
// DecideTend needs.
//
// It lives here, beside the decision that reads them, because the split is a
// rule and not plumbing: a KindTend row blocks its PULL REQUEST, and every
// other kind blocks its ISSUE. Getting that backwards in a caller would either
// let two agents into one branch or stop tending entirely, and neither failure
// announces itself.
//
// rows must already be filtered to dispatches whose process is confirmed alive,
// and must span EVERY loop of the project. See TendState.
func TendLiveness(rows []store.Dispatch) (liveIssues, liveTendPRs map[int]bool) {
	liveIssues = make(map[int]bool, len(rows))
	liveTendPRs = make(map[int]bool, len(rows))
	for _, d := range rows {
		if d.Kind == store.KindTend {
			liveTendPRs[d.PRNumber] = true
			continue
		}
		liveIssues[d.Number] = true
	}
	return liveIssues, liveTendPRs
}
