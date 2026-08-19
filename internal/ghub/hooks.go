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
}

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
	created, _, err := g.c.Repositories.CreateHook(ctx, owner, repo, hookRequest(h))
	if err != nil {
		return 0, missingScopeErr(owner, repo, err)
	}
	return created.GetID(), nil
}

// EditHook updates an existing webhook's delivery URL, secret, and events.
func (g *GitHubClient) EditHook(ctx context.Context, owner, repo string, id int64, h HookSpec) error {
	_, _, err := g.c.Repositories.EditHook(ctx, owner, repo, id, hookRequest(h))
	if err != nil {
		return missingScopeErr(owner, repo, err)
	}
	return nil
}

// hookRequest builds the go-github Hook the API expects from a HookSpec.
func hookRequest(h HookSpec) *github.Hook {
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
	}
}

// missingScopeErr turns a 404 from a hooks endpoint into an error that names
// the scope a caller is missing. GitHub answers a token without
// admin:repo_hook with 404, not 403, so an unwrapped error reads to an
// operator as "the repository does not exist" and sends them down the wrong
// path entirely.
func missingScopeErr(owner, repo string, err error) error {
	var ge *github.ErrorResponse
	if errors.As(err, &ge) && ge.Response != nil && ge.Response.StatusCode == 404 {
		return fmt.Errorf("hooks %s/%s: 404 (token likely missing the admin:repo_hook scope): %w", owner, repo, err)
	}
	return fmt.Errorf("hooks %s/%s: %w", owner, repo, err)
}
