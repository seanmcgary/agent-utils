package ghub

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/go-github/v77/github"
)

// HookAdmin administers repository webhooks. It is separate from Client because
// no tick calls it: hook administration is an operator command that runs once.
type HookAdmin interface {
	ListHooks(ctx context.Context, owner, repo string) ([]Hook, error)
	CreateHook(ctx context.Context, owner, repo string, h HookSpec) (int64, error)
	EditHook(ctx context.Context, owner, repo string, id int64, h HookSpec) error
	DeleteHook(ctx context.Context, owner, repo string, id int64) error
}

// ErrHookNotFound reports that GitHub answered a hook call with 404.
//
// It says nothing more than that. GitHub answers BOTH "no such hook" and "your
// token lacks admin:repo_hook" with 404 (see missingScopeErr), so a caller that
// treats this as "already deleted" is choosing the first reading. That choice
// is safe only where the alternative is worse -- deregister-webhook makes it,
// because refusing there would leave a recorded row that nothing on this
// machine can ever clear -- and the wrapped message still names the scope so
// the operator can tell the two apart.
var ErrHookNotFound = errors.New("hook not found")

// ListHooks returns every webhook configured on the repository.
func (g *GitHubClient) ListHooks(ctx context.Context, owner, repo string) ([]Hook, error) {
	opts := &github.ListOptions{PerPage: 100}
	var all []Hook
	for {
		page, resp, err := g.c.Repositories.ListHooks(ctx, owner, repo, opts)
		if err != nil {
			return nil, missingScopeErr(owner, repo, err)
		}
		for _, h := range page {
			if h == nil {
				continue
			}
			all = append(all, Hook{
				ID:     h.GetID(),
				URL:    h.GetConfig().GetURL(),
				Events: h.Events,
				Active: h.GetActive(),
			})
		}
		if resp.NextPage == 0 {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
}

// CreateHook registers a new webhook and returns its ID.
func (g *GitHubClient) CreateHook(ctx context.Context, owner, repo string, h HookSpec) (int64, error) {
	req, err := hookRequest(h)
	if err != nil {
		return 0, err
	}
	created, _, err := g.c.Repositories.CreateHook(ctx, owner, repo, req)
	if err != nil {
		return 0, missingScopeErr(owner, repo, err)
	}
	return created.GetID(), nil
}

// EditHook updates an existing webhook's delivery URL, secret, and events.
func (g *GitHubClient) EditHook(ctx context.Context, owner, repo string, id int64, h HookSpec) error {
	req, err := hookRequest(h)
	if err != nil {
		return err
	}
	_, _, err = g.c.Repositories.EditHook(ctx, owner, repo, id, req)
	if err != nil {
		return missingScopeErr(owner, repo, err)
	}
	return nil
}

// DeleteHook removes a webhook by its GitHub identifier.
//
// It deletes by id, never by matching a delivery URL, which is the entire
// reason the id is recorded locally: after `config set webhook.url` the live
// hook still points at the previous endpoint, and a URL match would fail to
// find exactly the orphan the operator is trying to remove.
func (g *GitHubClient) DeleteHook(ctx context.Context, owner, repo string, id int64) error {
	if _, err := g.c.Repositories.DeleteHook(ctx, owner, repo, id); err != nil {
		return missingScopeErr(owner, repo, err)
	}
	return nil
}

// hookRequest builds the go-github Hook the API expects from a HookSpec.
//
// It refuses an empty secret rather than sending one. The Config below is a
// full replacement, so EditHook with an empty HookSpec.Secret would REMOVE the
// secret from a live hook -- after which GitHub sends deliveries with no
// signature at all and the listener answers 400 to every one of them, which
// reads as a broken daemon rather than a stripped hook. Today's only caller
// validates before it gets here; this package is shared, and a fail-open
// default in it is the kind that is discovered in production.
func hookRequest(h HookSpec) (*github.Hook, error) {
	if h.Secret == "" {
		return nil, errors.New("hooks: refusing to write a webhook with an empty secret")
	}
	return &github.Hook{
		Name:   github.Ptr("web"),
		Active: github.Ptr(true),
		Events: h.Events,
		Config: &github.HookConfig{
			ContentType: github.Ptr("json"),
			InsecureSSL: github.Ptr("0"),
			URL:         github.Ptr(h.URL),
			Secret:      github.Ptr(h.Secret),
		},
	}, nil
}

// missingScopeErr turns a 404 from a hooks endpoint into an error that names
// the scope a caller is missing. GitHub answers a token without
// admin:repo_hook with 404, not 403, so an unwrapped error reads to an
// operator as "the repository does not exist" and sends them down the wrong
// path entirely.
func missingScopeErr(owner, repo string, err error) error {
	if isNotFound(err) {
		// ErrHookNotFound is joined in so a caller can branch on the 404
		// without parsing the message, while the message still names the
		// scope: the two causes are indistinguishable from the response
		// alone, so both readings have to survive into the error.
		return fmt.Errorf("hooks %s/%s: 404 (token likely missing the admin:repo_hook scope): %w",
			owner, repo, errors.Join(ErrHookNotFound, err))
	}
	return fmt.Errorf("hooks %s/%s: %w", owner, repo, err)
}

// isNotFound reports whether err is GitHub's 404.
//
// One copy, two callers: missingScopeErr reads it as "no such hook, or no
// admin:repo_hook scope", and Parent reads it as "this issue has no parent".
// The two READINGS differ and that is fine; the test must not, or one of them
// silently stops recognising a 404.
func isNotFound(err error) bool {
	var ge *github.ErrorResponse
	return errors.As(err, &ge) && ge.Response != nil && ge.Response.StatusCode == 404
}
