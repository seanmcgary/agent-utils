// Package ghub reads issues and pull requests from GitHub.
package ghub

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v77/github"
)

// Client is the read surface the engine needs, plus the two writes reserved
// for the retry-cap park. Every method is safe to fake in a test.
type Client interface {
	ListOpenIssues(ctx context.Context, owner, repo string) ([]Issue, error)
	// Issue and PullRequest fetch ONE number. A webhook delivery names one
	// issue, and answering it with the two list calls plus a comparison per
	// review issue is what burned a token budget on an unrelated issue; see
	// loopcmd.TickIssue.
	Issue(ctx context.Context, owner, repo string, number int) (Issue, error)
	PullRequest(ctx context.Context, owner, repo string, number int) (PullRequest, error)
	ListOpenPullRequests(ctx context.Context, owner, repo string) ([]PullRequest, error)
	BehindBy(ctx context.Context, owner, repo, base, head string) (int, error)
	// LatestReviewActivity returns the time of the most recent review or
	// review comment on a pull request that was NOT written by this loop and
	// was written by a trusted repository member. See the GitHubClient
	// implementation for why both filters are load-bearing.
	LatestReviewActivity(ctx context.Context, owner, repo string, number int) (time.Time, error)
	PostComment(ctx context.Context, owner, repo string, number int, body string) error
	EditLabels(ctx context.Context, owner, repo string, number int, add, remove []string) error
}

// GitHubClient is the go-github backed implementation of Client.
type GitHubClient struct {
	c *github.Client

	// loginMu and login memoise AuthenticatedLogin for the life of this
	// client. The token this client holds does not change while the process
	// runs, so the account it belongs to is a constant -- and a call per tend
	// candidate would spend a request to learn that constant over and over.
	//
	// Only SUCCESS is cached, which is why this is a mutex and not a
	// sync.Once. A Once would remember the first FAILURE for the life of the
	// daemon, so one network blip on the first lookup would disable the
	// self-comment filter until somebody restarted the process -- and that
	// filter is the only thing standing between the tend agent's own review
	// comments and a dispatch loop. The read fails closed either way (no
	// login, no review activity, no trigger), but a permanent silent
	// degradation is not a failure mode worth keeping to save one retry.
	loginMu sync.Mutex
	login   string
}

// New returns a client authenticated with token.
func New(token string) *GitHubClient {
	return &GitHubClient{c: github.NewClient(nil).WithAuthToken(token)}
}

// ConvertIssues maps go-github issues onto the engine type. It drops pull
// requests. The issues endpoint returns pull requests together with issues, so
// this filter is required, not optional.
func ConvertIssues(in []*github.Issue) []Issue {
	out := make([]Issue, 0, len(in))
	for _, gi := range in {
		if gi == nil || gi.IsPullRequest() {
			continue
		}
		labels := make([]string, 0, len(gi.Labels))
		for _, l := range gi.Labels {
			if l.GetName() != "" {
				labels = append(labels, l.GetName())
			}
		}
		out = append(out, Issue{
			Number:    gi.GetNumber(),
			Title:     gi.GetTitle(),
			Labels:    labels,
			UpdatedAt: gi.GetUpdatedAt().Time,
			State:     gi.GetState(),
			Repo:      issueRepo(gi),
		})
	}
	return out
}

// issueRepo returns the "owner/name" an issue belongs to.
//
// Two sources, because the three endpoints do not agree. blocked_by returns a
// full repository object; parent and sub_issues return repository_url. Reading
// only one of them would leave Repo empty for two endpoints out of three, and
// an empty Repo blocks every write -- a silent, total failure rather than a
// loud one.
func issueRepo(gi *github.Issue) string {
	if full := gi.GetRepository().GetFullName(); full != "" {
		return full
	}
	// repository_url is "https://api.github.com/repos/{owner}/{name}". Take the
	// last two path elements rather than trimming a hard-coded host prefix:
	// GitHub Enterprise serves a different base URL.
	u := gi.GetRepositoryURL()
	if u == "" {
		return ""
	}
	parts := strings.Split(strings.Trim(u, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}

// convertPR maps one go-github pull request onto the engine type and decides
// its trust.
//
// Trust is decided here, once, at the boundary. A pull request is trusted only
// when its head branch lives in this repository and its author is a repository
// insider. Tending checks the head branch out and runs an agent in it, so an
// untrusted head is code execution.
//
// EqualFold: full_name comes back in GitHub's canonical casing while
// owner/repo come from config unchecked. A casing mismatch would fail closed
// and silently disable tending with no diagnostic.
//
// The list path and the single-fetch path share this function rather than each
// computing trust for itself. Two copies could drift, and a single-fetch path
// that drifted toward "trusted" would let one webhook delivery about a fork's
// pull request run an agent inside that fork's branch.
func convertPR(owner, repo string, pr *github.PullRequest) PullRequest {
	head := pr.GetHead().GetRef()
	base := pr.GetBase().GetRef()
	assoc := pr.GetAuthorAssociation()
	headRepo := pr.GetHead().GetRepo().GetFullName()

	trusted := strings.EqualFold(headRepo, owner+"/"+repo) &&
		(assoc == "OWNER" || assoc == "MEMBER" || assoc == "COLLABORATOR") &&
		SafeRef(head) && SafeRef(base)

	return PullRequest{
		Number:            pr.GetNumber(),
		HeadRef:           head,
		BaseRef:           base,
		Body:              pr.GetBody(),
		Draft:             pr.GetDraft(),
		HeadRepo:          headRepo,
		AuthorAssociation: assoc,
		Trusted:           trusted,
	}
}

// ErrNotAnIssue reports that a number names a pull request rather than an
// issue.
//
// It is a distinct sentinel, not a "not found", because the caller acts on the
// difference: a webhook delivery about a pull request is resolved to the issue
// that pull request closes (engine.ClosesIssue), while a genuinely missing
// issue is an error. GitHub's issues endpoint answers a pull request number
// with a pull request, which is the same overlap ConvertIssues filters out of
// a list.
var ErrNotAnIssue = errors.New("this number names a pull request, not an issue")

// Issue returns one open or closed issue.
//
// This exists so a webhook delivery costs one API call instead of the two list
// calls plus a comparison per review issue that a full reconcile costs. That
// difference is the whole point of the scoped tick: a delivery about a single
// unlabelled issue used to read the entire repository.
func (g *GitHubClient) Issue(ctx context.Context, owner, repo string, number int) (Issue, error) {
	gi, _, err := g.c.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return Issue{}, fmt.Errorf("get issue %s/%s#%d: %w", owner, repo, number, err)
	}
	if gi == nil {
		return Issue{}, fmt.Errorf("get issue %s/%s#%d: empty response", owner, repo, number)
	}
	if gi.IsPullRequest() {
		return Issue{}, fmt.Errorf("%s/%s#%d: %w", owner, repo, number, ErrNotAnIssue)
	}
	// ConvertIssues, not a hand-written mapping: the label and timestamp
	// handling must be identical to the list path, or a scoped tick would
	// decide from a differently shaped issue than a cron tick.
	out := ConvertIssues([]*github.Issue{gi})
	if len(out) != 1 {
		return Issue{}, fmt.Errorf("get issue %s/%s#%d: unusable response", owner, repo, number)
	}
	return out[0], nil
}

// PullRequest returns one pull request, carrying the same trust decision
// ListOpenPullRequests makes; see convertPR.
func (g *GitHubClient) PullRequest(ctx context.Context, owner, repo string, number int) (PullRequest, error) {
	pr, _, err := g.c.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return PullRequest{}, fmt.Errorf("get pull request %s/%s#%d: %w", owner, repo, number, err)
	}
	if pr == nil {
		return PullRequest{}, fmt.Errorf("get pull request %s/%s#%d: empty response", owner, repo, number)
	}
	return convertPR(owner, repo, pr), nil
}

// ListOpenIssues returns every open issue in the repository.
//
// The call sends no label filter on purpose. IssueListByRepoOptions.Labels is
// an AND filter, so it cannot express "carries any of these labels". The engine
// also needs the complete label set of each issue to evaluate the veto list.
func (g *GitHubClient) ListOpenIssues(ctx context.Context, owner, repo string) ([]Issue, error) {
	opts := &github.IssueListByRepoOptions{
		State:       "open",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var all []Issue
	for {
		page, resp, err := g.c.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list issues %s/%s: %w", owner, repo, err)
		}
		all = append(all, ConvertIssues(page)...)
		if resp.NextPage == 0 {
			return all, nil
		}
		// IssueListByRepoOptions embeds BOTH ListCursorOptions (Page string) and
		// ListOptions (Page int) at the same depth, so a bare opts.Page is an
		// ambiguous selector and does not compile. Qualify it.
		opts.ListOptions.Page = resp.NextPage
	}
}

// ListOpenPullRequests returns every open pull request in the repository.
func (g *GitHubClient) ListOpenPullRequests(ctx context.Context, owner, repo string) ([]PullRequest, error) {
	opts := &github.PullRequestListOptions{
		State:       "open",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var all []PullRequest
	for {
		page, resp, err := g.c.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list pull requests %s/%s: %w", owner, repo, err)
		}
		for _, pr := range page {
			all = append(all, convertPR(owner, repo, pr))
		}
		if resp.NextPage == 0 {
			return all, nil
		}
		// PullRequestListOptions embeds only ListOptions, so this one is fine.
		opts.Page = resp.NextPage
	}
}

// BehindBy returns how many commits head lacks from base.
func (g *GitHubClient) BehindBy(ctx context.Context, owner, repo, base, head string) (int, error) {
	cmp, _, err := g.c.Repositories.CompareCommits(ctx, owner, repo, base, head, nil)
	if err != nil {
		return 0, fmt.Errorf("compare %s...%s: %w", base, head, err)
	}
	return cmp.GetBehindBy(), nil
}

// AuthenticatedLogin returns the login of the account this client's token
// belongs to.
//
// It is deliberately NOT on the Client interface. Only LatestReviewActivity
// below consumes it, and it does so on the concrete receiver, so putting it
// on the interface bought nothing and cost every fake in the test tree a
// method it never answers. The original
// belongs to, fetched once and cached for the life of the process.
//
// Users.Get(ctx, "") with an empty user name answers the AUTHENTICATED user,
// which is exactly the identity LatestReviewActivity needs to exclude: the
// tend prompt tells the agent to comment, so without knowing this login,
// LatestReviewActivity cannot tell the agent's own reply from a reviewer's.
func (g *GitHubClient) AuthenticatedLogin(ctx context.Context) (string, error) {
	g.loginMu.Lock()
	defer g.loginMu.Unlock()
	if g.login != "" {
		return g.login, nil
	}
	u, _, err := g.c.Users.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("get authenticated user: %w", err)
	}
	// An empty login is an ERROR, not an answer. countsAsReviewActivity
	// compares an author against this value, and "" matches no author, so
	// returning it would silently disable the self-filter while every caller
	// believed it had one. Every other new failure in this path reports
	// itself; this one must too.
	if u.GetLogin() == "" {
		return "", errors.New("the authenticated user has no login")
	}
	g.login = u.GetLogin()
	return g.login, nil
}

// maxReviewPages bounds the walk over a pull request's reviews. ListReviews
// takes no sort option, so the newest review is only found by paging through
// every one and tracking the maximum SubmittedAt seen -- and any user who can
// review a pull request can post thousands of reviews. An unbounded walk
// would hold the loop lock while it exhausts the daemon's GitHub rate limit
// for every project on the machine. What was seen within the cap is treated
// as the answer; a pull request with more reviews than this cap can hold is
// vanishingly rare, and the alternative -- refusing to answer at all -- would
// fail the caller closed on exactly the pull requests with the most reviewer
// engagement.
const maxReviewPages = 10

// LatestReviewActivity returns the time of the most recent review or review
// comment on a pull request that this loop did not write itself, and the zero
// time when there is none.
//
// Two filters apply, and BOTH are load-bearing.
//
//  1. Activity written by this loop's own authenticated login is skipped.
//     The tend prompt tells the agent to comment (examples/execution.yaml
//     tend_prompt), so a reply the loop itself wrote must never read as
//     feedback awaiting an answer.
//
//     This filter is DEFENCE IN DEPTH, not the money-loop guard, and the
//     difference matters. The agent runs with GITHUB_TOKEN stripped from its
//     environment (runner.agentEnv), so its gh calls authenticate as whatever
//     ~/.config/gh holds -- on the ordinary deployment a human's login, not
//     the daemon's bot. This comparison therefore MISSES the agent's own
//     comments on exactly the setup it was written for. What actually closes
//     the loop is store.LastTendAt returning the dispatch's FINISH time, so a
//     comment written during the dispatch is older than it whoever wrote it.
//
//  2. An author whose AuthorAssociation is not OWNER, MEMBER, or COLLABORATOR
//     is skipped -- the same three values convertPR requires before it will
//     trust a pull request (see convertPR). A review comment can be written by
//     anyone with read access to the repository. Without this filter a
//     stranger can spend the loop's budget at will, and can put chosen text
//     in front of an agent that holds push rights on the branch.
//
// Comments and reviews are read differently because the two endpoints
// differ: ListComments accepts a Sort/Direction pair, so the newest comment
// is one page away regardless of how many exist; ListReviews has no sort
// option at all, so its results are oldest first and finding the newest one
// means walking every page (capped at maxReviewPages; see its comment). A
// page of comments, not a single row, is read because the single newest
// comment may be one this loop wrote itself -- the filters above then have
// candidates left in the page to consider instead of nothing.
//
// A review with no SubmittedAt is a pending review, visible only to its own
// author, and is skipped rather than counted as happening now.
func (g *GitHubClient) LatestReviewActivity(ctx context.Context, owner, repo string, number int) (time.Time, error) {
	self, err := g.AuthenticatedLogin(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("latest review activity for %s/%s#%d: %w", owner, repo, number, err)
	}

	var latest time.Time

	comments, _, err := g.c.PullRequests.ListComments(ctx, owner, repo, number,
		&github.PullRequestListCommentsOptions{
			Sort:        "created",
			Direction:   "desc",
			ListOptions: github.ListOptions{PerPage: 100},
		})
	if err != nil {
		return time.Time{}, fmt.Errorf("list review comments %s/%s#%d: %w", owner, repo, number, err)
	}
	for _, c := range comments {
		if !countsAsReviewActivity(self, c.GetUser().GetLogin(), c.GetAuthorAssociation()) {
			continue
		}
		if t := c.GetCreatedAt().Time; t.After(latest) {
			latest = t
		}
	}

	opts := &github.ListOptions{PerPage: 100}
	for page := 0; page < maxReviewPages; page++ {
		reviews, resp, err := g.c.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return time.Time{}, fmt.Errorf("list reviews %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, r := range reviews {
			if !countsAsReviewActivity(self, r.GetUser().GetLogin(), r.GetAuthorAssociation()) {
				continue
			}
			// A review with no SubmittedAt is still pending -- only its
			// author can see it -- and counting it as "now" would make the
			// trigger fire on activity nobody but its own writer has read.
			if r.SubmittedAt == nil {
				continue
			}
			if t := r.GetSubmittedAt().Time; t.After(latest) {
				latest = t
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return latest, nil
}

// countsAsReviewActivity applies the two filters LatestReviewActivity
// documents: not this loop's own login, and a trusted author association.
func countsAsReviewActivity(self, author, assoc string) bool {
	// EqualFold, not ==. A GitHub login is case-insensitive, and the two
	// sides reach here by different routes: self comes from Users.Get and
	// author from the review payload. A casing difference between them would
	// fail OPEN -- the loop's own comment would stop being recognised as its
	// own, the trigger would re-fire on the agent's own output, and the
	// dispatch loop this filter exists to prevent would run at about $0.75 a
	// turn. convertPR folds case on full_name for the mirror-image reason.
	if strings.EqualFold(author, self) {
		return false
	}
	return assoc == "OWNER" || assoc == "MEMBER" || assoc == "COLLABORATOR"
}

// PostComment adds a comment to an issue. Task 10 is its only caller.
func (g *GitHubClient) PostComment(ctx context.Context, owner, repo string, number int, body string) error {
	_, _, err := g.c.Issues.CreateComment(ctx, owner, repo, number,
		&github.IssueComment{Body: github.Ptr(body)})
	if err != nil {
		return fmt.Errorf("comment on #%d: %w", number, err)
	}
	return nil
}

// EditLabels adds and removes labels on an issue. Task 10 is its only caller.
func (g *GitHubClient) EditLabels(ctx context.Context, owner, repo string, number int, add, remove []string) error {
	for _, name := range remove {
		if _, err := g.c.Issues.RemoveLabelForIssue(ctx, owner, repo, number, name); err != nil {
			// A label that is already absent is not an error for this caller.
			var ge *github.ErrorResponse
			if !errors.As(err, &ge) || ge.Response == nil || ge.Response.StatusCode != 404 {
				return fmt.Errorf("remove label %q from #%d: %w", name, number, err)
			}
		}
	}
	if len(add) > 0 {
		if _, _, err := g.c.Issues.AddLabelsToIssue(ctx, owner, repo, number, add); err != nil {
			return fmt.Errorf("add labels %v to #%d: %w", add, number, err)
		}
	}
	return nil
}
