package ghub

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/go-github/v77/github"
	"github.com/seanmcgary/agent-utils/internal/ghapp"
)

// WaitReader is the surface the waiting-label pass needs: the reads that
// decide whether an issue's blockers are done, and the two writes it makes on
// the WAITING issue itself.
//
// It is separate from Client and EpicReader for the reason EpicReader is
// separate from Client: no tick calls most of these, and a test faking one pass
// should not have to fake every other pass's methods too.
//
// # Which repository each method is pointed at
//
// The pass reads across repositories -- a blocker usually lives in another one
// -- but it WRITES only to the waiting issue, in that issue's own repository,
// which is the repository of the loop that owns the waiting label. Nothing in
// this interface enforces that; loopcmd.WaitSweep does, by addressing every
// EditLabels and PostComment with the loop's own owner/repo and nothing else.
// Blocking, ClosingPullRequests, DefaultBranch, WorkflowRuns and
// CompareCommits are the reads that may be pointed at a foreign repository.
type WaitReader interface {
	// Blocking returns the issues that declare number as a blocker -- the
	// reverse of BlockedBy. The answer may cross repositories.
	Blocking(ctx context.Context, owner, repo string, number int) ([]Issue, error)
	BlockedBy(ctx context.Context, owner, repo string, number int) ([]Issue, error)
	Issue(ctx context.Context, owner, repo string, number int) (Issue, error)
	// ListOpenIssuesLabeled returns every open issue carrying label.
	ListOpenIssuesLabeled(ctx context.Context, owner, repo, label string) ([]Issue, error)
	// ClosingPullRequests returns the pull requests GitHub records as closing
	// number, merged or not.
	ClosingPullRequests(ctx context.Context, owner, repo string, number int) ([]ClosingPR, error)
	DefaultBranch(ctx context.Context, owner, repo string) (string, error)
	// WorkflowRuns returns the runs of one workflow file matching q, newest
	// first, from a single page.
	WorkflowRuns(ctx context.Context, owner, repo string, q RunQuery) ([]WorkflowRun, error)
	// CompareCommits returns GitHub's status for base...head: "identical",
	// "ahead" (head contains base), "behind" or "diverged".
	CompareCommits(ctx context.Context, owner, repo, base, head string) (string, error)
	// CommentBodies returns the bodies of an issue's comments, oldest first.
	CommentBodies(ctx context.Context, owner, repo string, number int) ([]string, error)
	PostComment(ctx context.Context, owner, repo string, number int, body string) error
	EditLabels(ctx context.Context, owner, repo string, number int, add, remove []string) error
}

// ClosingPR is one pull request GitHub links to an issue as closing it.
type ClosingPR struct {
	Number int
	// Repo is the "owner/name" the pull request lives in. A pull request may
	// close an issue in another repository, and its merge commit lives in the
	// pull request's repository, not the issue's.
	Repo   string
	Merged bool
	// MergeCommit is the merge commit's sha, empty for an unmerged pull
	// request.
	MergeCommit string
}

// WorkflowRun is the subset of a GitHub Actions run the waiting pass decides
// from.
type WorkflowRun struct {
	ID         int64
	HeadSHA    string
	HeadBranch string
	Event      string
	// Status is "queued", "in_progress", "completed" and so on; Conclusion is
	// set only once Status is "completed".
	Status     string
	Conclusion string
	HTMLURL    string
	CreatedAt  time.Time
}

// RunQuery narrows a workflow-run listing. Workflow is the workflow's FILE
// name ("mirror.yml"), which is what the runs endpoint accepts in place of an
// id. Empty fields are not sent.
type RunQuery struct {
	Workflow string
	Branch   string
	HeadSHA  string
	// Status filters by status OR conclusion, as GitHub's endpoint does:
	// "success" returns completed runs that succeeded.
	Status  string
	PerPage int
}

// maxClosingPRs bounds the closing-pull-request lookup. An issue closed by more
// than a handful of merged pull requests is not a shape this pass has met;
// the bound exists so the request is finite, and a truncated answer is
// reported rather than read as complete.
const maxClosingPRs = 25

// maxCommentPages bounds CommentBodies. It reads comments to find its own
// marker, so a pathological issue with thousands of comments must not turn
// one check into an unbounded walk. 10 pages of 100 is far beyond any issue
// this pass waits on.
const maxCommentPages = 10

// Blocking returns every issue that declares number as a blocker, following
// pagination. The dependents may live in other repositories; each carries its
// own Repo, exactly as BlockedBy's blockers do.
func (g *GitHubClient) Blocking(ctx context.Context, owner, repo string, number int) ([]Issue, error) {
	return g.pagedIssues(ctx,
		fmt.Sprintf("repos/%s/%s/issues/%d/dependencies/blocking",
			url.PathEscape(owner), url.PathEscape(repo), number),
		fmt.Sprintf("blocking %s/%s#%d", owner, repo, number))
}

// ListOpenIssuesLabeled returns every open issue carrying label.
//
// ListOpenIssues deliberately sends no label filter, because the engine needs
// every issue. This pass needs only the issues carrying ONE label, and on a
// busy repository the filter is the difference between one page and dozens.
func (g *GitHubClient) ListOpenIssuesLabeled(ctx context.Context, owner, repo, label string) ([]Issue, error) {
	opts := &github.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{label},
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var all []Issue
	for {
		page, resp, err := g.c.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list issues labelled %q in %s/%s: %w", label, owner, repo, err)
		}
		all = append(all, ConvertIssues(page)...)
		if resp == nil || resp.NextPage == 0 {
			return all, nil
		}
		if resp.NextPage <= opts.ListOptions.Page {
			return all, fmt.Errorf("list issues labelled %q in %s/%s: pagination did not advance", label, owner, repo)
		}
		opts.ListOptions.Page = resp.NextPage
	}
}

// DefaultBranch returns a repository's default branch.
func (g *GitHubClient) DefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	r, _, err := g.c.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("get repository %s/%s: %w", owner, repo, err)
	}
	b := r.GetDefaultBranch()
	if b == "" {
		return "", fmt.Errorf("repository %s/%s reports no default branch", owner, repo)
	}
	return b, nil
}

// WorkflowRuns returns one page of a workflow's runs, newest first.
//
// One page, not every page: the caller asks a narrow question -- the newest
// successful runs on a branch, or the runs of one commit -- and the newest
// page is where every answer to it lives.
func (g *GitHubClient) WorkflowRuns(ctx context.Context, owner, repo string, q RunQuery) ([]WorkflowRun, error) {
	per := q.PerPage
	if per <= 0 || per > 100 {
		per = 30
	}
	runs, _, err := g.c.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, url.PathEscape(q.Workflow),
		&github.ListWorkflowRunsOptions{
			Branch:              q.Branch,
			HeadSHA:             q.HeadSHA,
			Status:              q.Status,
			ExcludePullRequests: true,
			ListOptions:         github.ListOptions{PerPage: per},
		})
	if err != nil {
		return nil, fmt.Errorf("list runs of %s in %s/%s: %w", q.Workflow, owner, repo, err)
	}
	out := make([]WorkflowRun, 0, len(runs.WorkflowRuns))
	for _, r := range runs.WorkflowRuns {
		if r == nil {
			continue
		}
		out = append(out, WorkflowRun{
			ID:         r.GetID(),
			HeadSHA:    r.GetHeadSHA(),
			HeadBranch: r.GetHeadBranch(),
			Event:      r.GetEvent(),
			Status:     r.GetStatus(),
			Conclusion: r.GetConclusion(),
			HTMLURL:    r.GetHTMLURL(),
			CreatedAt:  r.GetCreatedAt().Time,
		})
	}
	return out, nil
}

// CompareCommits returns GitHub's status for base...head.
//
// PerPage 1: the response carries the commit list between the two, and only
// the status is read. Without it a far-apart pair returns 250 commits nobody
// looks at.
func (g *GitHubClient) CompareCommits(ctx context.Context, owner, repo, base, head string) (string, error) {
	cmp, _, err := g.c.Repositories.CompareCommits(ctx, owner, repo, base, head,
		&github.ListOptions{PerPage: 1})
	if err != nil {
		return "", fmt.Errorf("compare %s...%s in %s/%s: %w", base, head, owner, repo, err)
	}
	return cmp.GetStatus(), nil
}

// CommentBodies returns the bodies of an issue's comments, up to
// maxCommentPages pages.
func (g *GitHubClient) CommentBodies(ctx context.Context, owner, repo string, number int) ([]string, error) {
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	var out []string
	for page := 0; page < maxCommentPages; page++ {
		cs, resp, err := g.c.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("list comments on %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, c := range cs {
			out = append(out, c.GetBody())
		}
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
	// Truncated. Reported as an error rather than returned short: the caller
	// reads this to decide whether it already commented, and a short answer
	// would make it comment again.
	return nil, fmt.Errorf("list comments on %s/%s#%d: more than %d pages", owner, repo, number, maxCommentPages)
}

// closingPRsQuery reads the pull requests GitHub links to an issue as closing
// it. includeClosedPrs is what makes a MERGED pull request appear: by the time
// this pass asks, the pull request that closed the issue is closed too.
const closingPRsQuery = `query($owner: String!, $name: String!, $number: Int!, $first: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      closedByPullRequestsReferences(first: $first, includeClosedPrs: true) {
        pageInfo { hasNextPage }
        nodes {
          number
          merged
          mergeCommit { oid }
          repository { nameWithOwner }
        }
      }
    }
  }
}`

// ClosingPullRequests returns the pull requests that close number.
//
// This is the one GraphQL call in the program: REST has no endpoint that names
// the pull request that closed an issue. It is built by hand on the REST
// client's transport so it carries the same authentication -- including a
// GitHub App's per-repository installation token, which needs the repository
// named on the context because a GraphQL path names none (ghapp.WithRepo).
func (g *GitHubClient) ClosingPullRequests(ctx context.Context, owner, repo string, number int) ([]ClosingPR, error) {
	what := fmt.Sprintf("closing pull requests of %s/%s#%d", owner, repo, number)
	body := map[string]any{
		"query": closingPRsQuery,
		"variables": map[string]any{
			"owner": owner, "name": repo, "number": number, "first": maxClosingPRs,
		},
	}
	req, err := g.c.NewRequest("POST", graphQLURL(g.c.BaseURL), body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	// On the context passed to Do, not on req: Do replaces the request's
	// context with the one it is given, so a value set on req alone is lost.
	ctx = ghapp.WithRepo(ctx, owner+"/"+repo)

	var out struct {
		Data struct {
			Repository *struct {
				Issue *struct {
					Refs struct {
						PageInfo struct {
							HasNextPage bool `json:"hasNextPage"`
						} `json:"pageInfo"`
						Nodes []struct {
							Number      int  `json:"number"`
							Merged      bool `json:"merged"`
							MergeCommit *struct {
								OID string `json:"oid"`
							} `json:"mergeCommit"`
							Repository struct {
								NameWithOwner string `json:"nameWithOwner"`
							} `json:"repository"`
						} `json:"nodes"`
					} `json:"closedByPullRequestsReferences"`
				} `json:"issue"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := g.c.Do(ctx, req, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	// GraphQL answers 200 with an errors array for a failed query. Treating
	// that as "no closing pull requests" would read as "a human closed it" and
	// promote without waiting for any check, so it is an error.
	if len(out.Errors) > 0 {
		msgs := make([]string, 0, len(out.Errors))
		for _, e := range out.Errors {
			msgs = append(msgs, e.Message)
		}
		return nil, fmt.Errorf("%s: %s", what, strings.Join(msgs, "; "))
	}
	if out.Data.Repository == nil || out.Data.Repository.Issue == nil {
		return nil, fmt.Errorf("%s: the response named no such issue", what)
	}
	refs := out.Data.Repository.Issue.Refs
	if refs.PageInfo.HasNextPage {
		// A partial list could omit the merged pull request and read as "no
		// merged pull request", which promotes without checks. Refuse it.
		return nil, fmt.Errorf("%s: more than %d closing pull requests", what, maxClosingPRs)
	}
	prs := make([]ClosingPR, 0, len(refs.Nodes))
	for _, n := range refs.Nodes {
		pr := ClosingPR{Number: n.Number, Repo: n.Repository.NameWithOwner, Merged: n.Merged}
		if n.MergeCommit != nil {
			pr.MergeCommit = n.MergeCommit.OID
		}
		prs = append(prs, pr)
	}
	return prs, nil
}

// graphQLURL returns the GraphQL endpoint for a REST base URL.
//
// github.com serves REST at api.github.com/ and GraphQL at api.github.com/graphql,
// so the relative "graphql" resolves correctly there. GitHub Enterprise Server
// serves REST under /api/v3/ and GraphQL at /api/graphql -- a sibling, not a
// child -- so the relative form would hit /api/v3/graphql and 404.
func graphQLURL(base *url.URL) string {
	if base != nil && strings.HasSuffix(base.Path, "/api/v3/") {
		u := *base
		u.Path = strings.TrimSuffix(base.Path, "v3/") + "graphql"
		return u.String()
	}
	return "graphql"
}

// ErrNoWaitReader reports that a pass needing a WaitReader was given none. It
// is a sentinel so the daemon can tell a test's narrow fake from a real
// failure.
var ErrNoWaitReader = errors.New("no WaitReader; Deps.Wait is nil")

// compile-time proof that the concrete client serves the pass.
var _ WaitReader = (*GitHubClient)(nil)
