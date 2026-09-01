package engine

import (
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// Kind is the type of a decision.
type Kind string

// Decision kinds.
const (
	// KindStart begins a new session for an issue.
	KindStart Kind = "start"
	// KindResume continues the stored session for an issue.
	KindResume Kind = "resume"
	// KindRetryStart redispatches a failed issue with a NEW session, because the
	// previous attempt never created one.
	KindRetryStart Kind = "retry_start"
	// KindRetryResume redispatches a failed issue into its existing session.
	KindRetryResume Kind = "retry_resume"
	// KindTend rebases a stale pull request.
	KindTend Kind = "tend"
	// KindParkRetryExhausted is the one decision that writes to GitHub.
	KindParkRetryExhausted Kind = "park_retry_exhausted"
	// KindClearRetry clears a failure flag that no retry can ever act on,
	// because the issue is not in flight. Without it such an issue is stranded
	// permanently and no human action recovers it.
	KindClearRetry Kind = "clear_retry"
	// KindStop refuses to dispatch an issue: an operator stopped it, or an
	// issue label's override is invalid. It is a refusal, not a dispatch, so
	// it must survive a tripped circuit breaker, which drops every entry of
	// Decisions and rewrites its skip reason (engine.go:159-168).
	KindStop Kind = "stop"
)

// Snapshot is the GitHub view for one tick.
type Snapshot struct {
	Issues []ghub.Issue
	PRs    []ghub.PullRequest
	// BehindBy maps a pull request number to how many commits it lacks.
	BehindBy map[int]int
	// ReviewedAt maps a PULL REQUEST number -- not an issue number -- to the
	// time of its most recent trusted review activity. Review activity is a
	// fact about a pull request, not the issue that links to it, and a pull
	// request number is what LastTend below is keyed by too, so the two maps
	// compare directly without a lookup through LinkPR. A pull request absent
	// from this map has no known review activity, which a failed GitHub read
	// also produces -- see the failure-direction comments in loopcmd.Tick and loopcmd.tickIssue.
	ReviewedAt map[int]time.Time
}

// State is the stored view for one tick.
type State struct {
	Issues map[int]store.IssueState
	// Running holds every dispatch row still marked running whose process is
	// confirmed alive. The caller performs the liveness check, so Decide stays
	// pure.
	Running []store.Dispatch
	// Providers maps an issue number to the pi provider that would serve the
	// model its next dispatch runs. The CALLER resolves it: resolution shells
	// out to `pi --list-models`, and Decide performs no I/O.
	//
	// A missing entry means unresolved, which is never read as a provider
	// change. Callers that can never retire a cap -- the tend sweep, which
	// makes no retry decision -- leave it nil.
	Providers map[int]string
	// CooldownUntil is the time before which the loop must not dispatch.
	CooldownUntil time.Time
	// Force is the operator override behind `loop tick --force`. It is the one
	// field here that is not read from the store: it says this tick was asked
	// for by a human at a terminal, not by cron or a webhook delivery.
	//
	// It suspends all three of the engine's time gates for THIS call -- the
	// breaker cooldown, each issue's retry backoff window, and the breaker's
	// own trip -- and changes nothing else. The retry CAP is not a time gate
	// and still holds: a forced tick parks an issue that has exhausted its
	// retries exactly as an ordinary one does.
	//
	// The trip is suspended along with the rest because forcing makes the
	// waiting retries eligible again, so a live breaker would count them,
	// drop every dispatch, and re-arm the cooldown -- leaving --force a no-op
	// in exactly the situation an operator reaches for it.
	Force bool
	// LastTend maps a PULL REQUEST number to the start time of its last
	// FINISHED tend dispatch. Keyed by pull request, like Snapshot.ReviewedAt,
	// because both are facts about the pull request, not the issue: an issue
	// is worked by several loops in turn, but a tend dispatch and a review
	// comment both land on one pull request regardless of which loop tends it.
	// A pull request absent from this map has never had a finished tend
	// dispatch, which is why any review activity on it counts as pending.
	LastTend map[int]time.Time
}

// Decision is one action the tick must perform.
type Decision struct {
	Kind      Kind
	Issue     int
	Title     string
	PR        int
	SessionID string
	HeadRef   string
	BaseRef   string
	Reason    string
	// Overrides carries the per-issue label overrides for a dispatch that
	// starts, resumes, or retries a session. The runner is a detached
	// process that never sees the tick's GitHub snapshot, so this is how an
	// override reaches it: on the row, the same way title and behind_by
	// already travel.
	Overrides config.Overrides
	// Provider is the pi provider serving this dispatch's model, copied from
	// State.Providers. It travels on the decision so the tick can stamp it
	// through BeginDispatch without resolving a second time -- and so the
	// value the engine COMPARED is the value the store records, which is what
	// keeps the comparison stable from one tick to the next.
	//
	// A KindTend decision carries none. A tend makes no retry decision, can
	// never retire a cap, and does not reach BeginDispatch at all.
	Provider string
	// ReviewPending is set when this KindTend decision fired because review
	// activity on the linked pull request is newer than its last finished
	// tend dispatch, rather than (or in addition to) the pull request being
	// behind its base. It travels to the detached runner on the dispatch row
	// -- never on pr_links, see loopcmd.dispatch -- because the runner never
	// sees this Decision or the tick's Snapshot.
	ReviewPending bool
}

// Plan is the full output of one decision pass.
type Plan struct {
	Decisions []Decision
	// Skips explains, per issue number, why an issue Decide examined produced
	// no decision. It exists because a scoped tick's summary is all zeros for
	// half a dozen quite different reasons -- a veto label, a live agent, an
	// unexpired backoff window, no trigger label -- and an operator reading
	// "nothing happened" cannot tell which. The reason has to come from HERE
	// rather than be re-derived by the caller: a second copy of these rules
	// would be free to disagree with the one that actually decided.
	//
	// An issue that DID get a decision is absent from the map, so a caller can
	// log the entry whenever there is one.
	Skips map[int]string
	// Halted explains why the pass returned without examining any issue at
	// all. Only the breaker cooldown does that, and it happens before any
	// issue is looked at, so it cannot be a per-issue skip.
	Halted         string
	BreakerTripped bool
	CooldownUntil  time.Time
}

// NoDecisionReason returns why issue got no decision this pass, or "" when it
// got one. It is the single accessor a caller needs, so no caller has to know
// that a whole-pass halt and a per-issue skip are recorded differently.
func (p Plan) NoDecisionReason(issue int) string {
	if p.Halted != "" {
		return p.Halted
	}
	return p.Skips[issue]
}
