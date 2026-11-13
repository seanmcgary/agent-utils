package ghub

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-github/v77/github"
)

// LabelHistory reads when a label was last applied to an issue. It is narrow
// for the reason EpicReader is: a fake for any other pass need not grow it.
type LabelHistory interface {
	// LastLabeledAt returns the time of the most recent "labeled" event for
	// label on issue number. found is false when the issue has no such event.
	LastLabeledAt(ctx context.Context, owner, repo string, number int, label string) (t time.Time, found bool, err error)
}

// LastLabeledAt walks the issue's event list, oldest first, and keeps the last
// "labeled" event naming label.
func (g *GitHubClient) LastLabeledAt(ctx context.Context, owner, repo string, number int, label string) (time.Time, bool, error) {
	var last time.Time
	found := false
	opts := &github.ListOptions{PerPage: 100}
	for {
		events, resp, err := g.c.Issues.ListIssueEvents(ctx, owner, repo, number, opts)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("list events of #%d: %w", number, err)
		}
		for _, e := range events {
			if e.GetEvent() != "labeled" || e.GetLabel().GetName() != label {
				continue
			}
			if t := e.GetCreatedAt().Time; !found || t.After(last) {
				last, found = t, true
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return last, found, nil
		}
		opts.Page = resp.NextPage
	}
}

// LastLabeledAt passes through to the wrapped client. It is not memoised: one
// delivery asks it at most once per loop, and an answer cached across the loops
// of a delivery would be no fresher than the issue it was asked about.
func (d *DeliveryCache) LastLabeledAt(ctx context.Context, owner, repo string, number int, label string) (time.Time, bool, error) {
	lh, ok := d.c.(LabelHistory)
	if !ok {
		return time.Time{}, false, fmt.Errorf("the GitHub client cannot read label history")
	}
	return lh.LastLabeledAt(ctx, owner, repo, number, label)
}
