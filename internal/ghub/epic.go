package ghub

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/google/go-github/v77/github"
)

// EpicReader is the read surface the epic dependency sweep needs, plus the one
// write it performs.
//
// It is separate from Client for the reason HookAdmin is: no tick calls these,
// and a caller that must fake them should not have to fake the eight methods of
// Client as well. Four fakes of Client exist in this repository's tests, and
// none of them has anything to say about sub-issues.
//
// EditLabels is repeated here rather than referenced through Client so that the
// sweep depends on exactly one interface. It is the SECOND non-agent GitHub
// write in this program -- the first is the retry-cap park -- and the README's
// Security section names them both.
type EpicReader interface {
	Parent(ctx context.Context, owner, repo string, number int) (Issue, error)
	SubIssues(ctx context.Context, owner, repo string, number int) ([]Issue, error)
	BlockedBy(ctx context.Context, owner, repo string, number int) ([]Issue, error)
	EditLabels(ctx context.Context, owner, repo string, number int, add, remove []string) error
}

// ErrNoParent reports that an issue has no parent issue.
//
// GitHub answers the parent endpoint with 404 for that case, which is the
// ordinary answer for nearly every issue in nearly every repository. A caller
// must be able to tell it apart from a real failure without parsing a message:
// the sweep stops quietly on this one and logs loudly on any other.
var ErrNoParent = errors.New("issue has no parent")

// Parent returns the issue that holds number as a sub-issue.
//
// go-github v77 has no accessor for this endpoint, so the request is built by
// hand. hooks.go does the same for its own calls.
func (g *GitHubClient) Parent(ctx context.Context, owner, repo string, number int) (Issue, error) {
	u := fmt.Sprintf("repos/%s/%s/issues/%d/parent",
		url.PathEscape(owner), url.PathEscape(repo), number)
	req, err := g.c.NewRequest("GET", u, nil)
	if err != nil {
		return Issue{}, fmt.Errorf("parent %s/%s#%d: %w", owner, repo, number, err)
	}
	var gi github.Issue
	if _, err := g.c.Do(ctx, req, &gi); err != nil {
		if isNotFound(err) {
			return Issue{}, fmt.Errorf("parent %s/%s#%d: %w", owner, repo, number,
				errors.Join(ErrNoParent, err))
		}
		return Issue{}, fmt.Errorf("parent %s/%s#%d: %w", owner, repo, number, err)
	}
	// ConvertIssues drops pull requests. Passing through it keeps ONE mapping
	// from a GitHub issue to this type, so a field added there is carried by
	// every reader.
	out := ConvertIssues([]*github.Issue{&gi})
	if len(out) == 0 {
		// NOT ErrNoParent. A 404 means "this issue has no parent", which is
		// ordinary; landing here means a parent was returned and it was a pull
		// request. Those have different fixes, so they get different sentinels
		// -- the rule internal/config/discover.go:20 states for its own.
		return Issue{}, fmt.Errorf("parent %s/%s#%d: %w", owner, repo, number, ErrNotAnIssue)
	}
	return out[0], nil
}

// SubIssues returns every sub-issue of number, following pagination.
func (g *GitHubClient) SubIssues(ctx context.Context, owner, repo string, number int) ([]Issue, error) {
	return g.pagedIssues(ctx,
		fmt.Sprintf("repos/%s/%s/issues/%d/sub_issues",
			url.PathEscape(owner), url.PathEscape(repo), number),
		fmt.Sprintf("sub_issues %s/%s#%d", owner, repo, number))
}

// BlockedBy returns every issue number declares as a blocker, following
// pagination.
//
// A blocker may live in ANOTHER repository, and its state comes back in this
// same response -- no second call and no second client are needed to read it.
// Repo is carried through for the same reason it is on every other reader: the
// sweep ignores a blocker outside its own repository, by the operator's
// decision, and that filter has nothing to gate on without this field.
func (g *GitHubClient) BlockedBy(ctx context.Context, owner, repo string, number int) ([]Issue, error) {
	return g.pagedIssues(ctx,
		fmt.Sprintf("repos/%s/%s/issues/%d/dependencies/blocked_by",
			url.PathEscape(owner), url.PathEscape(repo), number),
		fmt.Sprintf("blocked_by %s/%s#%d", owner, repo, number))
}

// pagedIssues reads every page of an endpoint that returns issue objects.
//
// One implementation, two callers: both endpoints are paginated the same way,
// and a second copy would be free to stop after page one for only one of them.
// That failure is invisible -- a 40-child epic would simply promote nothing for
// its tail.
func (g *GitHubClient) pagedIssues(ctx context.Context, path, what string) ([]Issue, error) {
	// The query is built by hand because go-github's addOptions is unexported.
	// Do not reach for github.AddOptions; there is no such symbol.
	page := 1
	var all []Issue
	for {
		u := fmt.Sprintf("%s?per_page=100&page=%d", path, page)
		req, err := g.c.NewRequest("GET", u, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		var got []*github.Issue
		resp, err := g.c.Do(ctx, req, &got)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		all = append(all, ConvertIssues(got)...)
		// NextPage is filled from the Link header for a hand-built request too,
		// so this needs no helper. A response with no Link header leaves it 0
		// and ends the loop.
		if resp == nil || resp.NextPage == 0 {
			return all, nil
		}
		// GitHub caps a parent at 100 sub-issues, so this loop is bounded in
		// practice at one or two pages. The guard is here anyway: this reads a
		// remote server's pagination cursor, and a server that always reports a
		// next page would otherwise spin forever inside a webhook handler.
		if resp.NextPage <= page {
			return all, fmt.Errorf("%s: pagination did not advance past page %d", what, page)
		}
		page = resp.NextPage
	}
}
