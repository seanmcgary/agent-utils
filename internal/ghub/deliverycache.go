package ghub

import (
	"context"
	"sync"
	"time"
)

// DeliveryCache serves the repeated single-number reads of ONE webhook
// delivery from one fetch each.
//
// One delivery fans out across every loop that watches the repository, and
// every one of those loops starts by asking GitHub for the issue the delivery
// named (loopcmd.TickIssue -> subject). Two loops meant two identical fetches
// of the same issue for one event, ten loops meant ten, and a delivery about a
// pull request paid for the pull request and the issue it closes on top, per
// loop.
//
// # Lifetime
//
// A DeliveryCache is valid for exactly one delivery. It MUST be created inside
// the function that serves that delivery and dropped when it returns, and it
// must never be stored on a long-lived value.
//
// This is a correctness rule, not a tidiness one, and it is the thing a later
// reader is most likely to "optimise". The daemon decides from an issue's
// LABELS: a label is what triggers an agent, what vetoes one, and what marks
// an issue in flight. A cache that outlived its delivery would answer the NEXT
// delivery -- the one raised BECAUSE a label changed -- with the labels as they
// were before that change, so the daemon would decide from the state it was
// woken up to leave behind: no dispatch for a freshly triggered issue, or a
// dispatch for one whose trigger label was just removed. That is strictly worse
// than the extra fetch this type exists to avoid.
//
// Errors are memoised with the values, because a failure inside one delivery
// is not a transient the next loop of that same delivery could survive: the
// loops would each have failed identically a moment apart. ErrNotAnIssue in
// particular is an answer rather than a failure -- it is how the delivered
// number is recognised as a pull request -- and every loop walks that same
// path.
//
// Trust is NOT decided here. convertPR decides it at the API boundary, once,
// and this type stores the PullRequest it was given; tending checks the head
// branch out and runs an agent inside it, so a cache that re-derived Trusted
// would be a second, drifting copy of a code-execution guard.
//
// The mutex is not there for the fan-out as it stands today, which is
// sequential. It is there because this value crosses a package boundary into
// loopcmd, where a future concurrent pass would otherwise turn a saved API call
// into a data race.
type DeliveryCache struct {
	c Client

	mu     sync.Mutex
	issues map[numberKey]issueAnswer
	prs    map[numberKey]prAnswer
	// login and loginFetched memoise AuthenticatedLogin the same way GitHubClient
	// itself does: the token is a process-wide constant, not a per-delivery
	// fact, so this is safe to keep even though everything else on this type
	// is scoped to one delivery.
	loginFetched bool
	login        string
	loginErr     error
	// reviewActivity memoises LatestReviewActivity per pull request, for the
	// reason PullRequest is memoised at :104-115 and BehindBy deliberately is
	// NOT at :131-136: several loops of several projects answer one delivery
	// about the same pull request, the two REST reads behind this method
	// return the same answer for all of them at the same instant, and this
	// cache's lifetime is exactly one delivery -- so caching it can never
	// serve a later delivery a stale answer.
	reviewActivity map[numberKey]reviewAnswer
}

type reviewAnswer struct {
	t   time.Time
	err error
}

// numberKey identifies one number in one repository. The repository is part of
// the key even though a delivery names exactly one: the key costs nothing, and
// a bare number would make this type unsafe for any caller that ever holds two
// repositories at once.
type numberKey struct {
	owner  string
	repo   string
	number int
}

type issueAnswer struct {
	issue Issue
	err   error
}

type prAnswer struct {
	pr  PullRequest
	err error
}

// NewDeliveryCache wraps c for the lifetime of ONE delivery. See DeliveryCache
// for why that lifetime is a correctness rule.
func NewDeliveryCache(c Client) *DeliveryCache {
	return &DeliveryCache{
		c:              c,
		issues:         make(map[numberKey]issueAnswer),
		prs:            make(map[numberKey]prAnswer),
		reviewActivity: make(map[numberKey]reviewAnswer),
	}
}

// Issue returns the delivery's answer for one number, fetching it at most once.
func (d *DeliveryCache) Issue(ctx context.Context, owner, repo string, number int) (Issue, error) {
	key := numberKey{owner: owner, repo: repo, number: number}

	d.mu.Lock()
	defer d.mu.Unlock()
	if a, ok := d.issues[key]; ok {
		return a.issue, a.err
	}
	iss, err := d.c.Issue(ctx, owner, repo, number)
	d.issues[key] = issueAnswer{issue: iss, err: err}
	return iss, err
}

// PullRequest returns the delivery's answer for one pull request, fetching it
// at most once. The value carries the trust convertPR decided.
func (d *DeliveryCache) PullRequest(ctx context.Context, owner, repo string, number int) (PullRequest, error) {
	key := numberKey{owner: owner, repo: repo, number: number}

	d.mu.Lock()
	defer d.mu.Unlock()
	if a, ok := d.prs[key]; ok {
		return a.pr, a.err
	}
	pr, err := d.c.PullRequest(ctx, owner, repo, number)
	d.prs[key] = prAnswer{pr: pr, err: err}
	return pr, err
}

// AuthenticatedLogin returns the delivery's answer for the loop's own login,
// fetching it at most once per DeliveryCache. It is memoised the same way
// GitHubClient itself memoises it (a sync.Once there, a lock here): the token
// this cache wraps does not change while the daemon runs, so the identity it
// names is a process-wide constant, not a fact scoped to one delivery.
func (d *DeliveryCache) AuthenticatedLogin(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.loginFetched {
		return d.login, d.loginErr
	}
	login, err := d.c.AuthenticatedLogin(ctx)
	d.login, d.loginErr, d.loginFetched = login, err, true
	return login, err
}

// LatestReviewActivity returns the delivery's answer for one pull request's
// review activity, fetching it at most once.
//
// This IS memoised, unlike BehindBy just below. Several loops of several
// projects can answer one delivery about the same pull request, the two REST
// reads behind this method return the same answer for all of them at the
// same instant, and this cache is dropped at the end of the one delivery it
// was built for -- so caching cannot serve a later delivery a stale answer,
// the failure DeliveryCache's own lifetime rule exists to prevent.
func (d *DeliveryCache) LatestReviewActivity(ctx context.Context, owner, repo string, number int) (time.Time, error) {
	key := numberKey{owner: owner, repo: repo, number: number}

	d.mu.Lock()
	defer d.mu.Unlock()
	if a, ok := d.reviewActivity[key]; ok {
		return a.t, a.err
	}
	t, err := d.c.LatestReviewActivity(ctx, owner, repo, number)
	d.reviewActivity[key] = reviewAnswer{t: t, err: err}
	return t, err
}

// ListOpenIssues is not memoised: it belongs to a pass that reconciles a whole
// repository and must read it as it is now. loopcmd.TendSweep calls it on the
// delivery path, but through an access of its own -- see
// listener.Worker.tendFresh, which builds a fresh cache per sweep -- so no memo
// would span two loops there anyway.
func (d *DeliveryCache) ListOpenIssues(ctx context.Context, owner, repo string) ([]Issue, error) {
	return d.c.ListOpenIssues(ctx, owner, repo)
}

// ListOpenPullRequests is not memoised, for the same reason as ListOpenIssues.
func (d *DeliveryCache) ListOpenPullRequests(ctx context.Context, owner, repo string) ([]PullRequest, error) {
	return d.c.ListOpenPullRequests(ctx, owner, repo)
}

// BehindBy is passed through. It is a comparison of two refs rather than a
// repeated read of the delivered number, and each loop that reaches it has
// already decided the pull request is worth tending.
func (d *DeliveryCache) BehindBy(ctx context.Context, owner, repo, base, head string) (int, error) {
	return d.c.BehindBy(ctx, owner, repo, base, head)
}

// PostComment is passed through untouched. Nothing here may swallow a write.
func (d *DeliveryCache) PostComment(ctx context.Context, owner, repo string, number int, body string) error {
	return d.c.PostComment(ctx, owner, repo, number, body)
}

// EditLabels is passed through, and then drops what this cache holds for that
// number.
//
// This program makes exactly one label write (loopcmd.parkRetryExhausted, which
// removes the trigger and in-flight labels), and it happens part way through a
// delivery's fan-out. Every loop still to come decides from labels, so serving
// them the labels as they were BEFORE that write would dispatch the agent the
// park exists to stop -- the same stale-labels failure the lifetime rule above
// exists to prevent, on a smaller scale.
func (d *DeliveryCache) EditLabels(ctx context.Context, owner, repo string, number int, add, remove []string) error {
	err := d.c.EditLabels(ctx, owner, repo, number, add, remove)

	// Dropped whether or not the write reported success: a failed write may
	// still have been applied, and a fetch costs one call while a wrong
	// decision costs an agent.
	key := numberKey{owner: owner, repo: repo, number: number}
	d.mu.Lock()
	delete(d.issues, key)
	delete(d.prs, key)
	d.mu.Unlock()

	return err
}
