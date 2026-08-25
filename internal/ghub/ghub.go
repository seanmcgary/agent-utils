// Package ghub reads issues and pull requests from GitHub.
package ghub

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	PostComment(ctx context.Context, owner, repo string, number int, body string) error
	EditLabels(ctx context.Context, owner, repo string, number int, add, remove []string) error
}

// GitHubClient is the go-github backed implementation of Client.
type GitHubClient struct {
	c *github.Client
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
