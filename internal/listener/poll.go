package listener

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// PollSource is the GitHub surface a poll needs.
//
// It is declared here, narrow, rather than added to ghub.Client: only the
// poller calls these three, and widening that interface would cost every fake
// in this tree two methods it never answers.
type PollSource interface {
	ListSubjectsUpdatedSince(ctx context.Context, owner, repo string, since time.Time) ([]ghub.Subject, error)
	BranchHead(ctx context.Context, owner, repo, branch string) (string, error)
	PullRequest(ctx context.Context, owner, repo string, number int) (ghub.PullRequest, error)
}

// pollTicker returns the channel pollPass runs on, and a stop function.
//
// A PollInterval of zero returns a NIL channel, and a nil channel blocks
// forever, so Serve's select case simply never fires for a worker that does
// not poll. tendTicker is built the same way and for the same reason.
func (w *Worker) pollTicker() (<-chan time.Time, func()) {
	if w.PollInterval <= 0 {
		return nil, func() {}
	}
	tk := time.NewTicker(w.PollInterval)
	return tk.C, tk.Stop
}

// pollPass asks GitHub what changed in every repository this machine routes,
// and turns each change into the delivery a webhook would have sent.
//
// It is the second source of events for this runtime, beside the HTTP handler,
// and it ends where that one does: at Deliver. Nothing downstream can tell
// which source produced a delivery, which is the property that makes a
// repository nobody has ADMIN on behave like one with a webhook.
func (w *Worker) pollPass(ctx context.Context) {
	if w.DB == nil {
		return
	}
	routes, err := w.ScanTargets()
	if err != nil {
		slog.Error("cannot route a poll", "err", err)
		return
	}
	byRepo := routes.ByRepo()
	if len(byRepo) == 0 {
		return
	}

	token, err := w.Token()
	if err != nil {
		slog.Error("cannot read the github token to poll", "err", err)
		return
	}
	src := w.NewPollSource(token)

	for _, r := range byRepo {
		// Checked per repository, not only on entry: a shutdown must not wait
		// out every repository on a many-project machine. reconcileClosed
		// makes the same check for the same reason.
		if ctx.Err() != nil {
			return
		}
		if err := w.pollRepo(ctx, src, r); err != nil {
			// Logged and skipped, never fatal to the pass. One unreachable
			// repository must not stop the others -- and pollRepo leaves the
			// cursor where it was, so the window this pass failed to cover is
			// re-read by the next one rather than lost.
			slog.Error("poll failed", "repo", r.Repo, "err", err)
		}
	}
}

// pollRepo polls one repository. It returns an error rather than logging one
// so its caller owns the "skip and continue" decision in one place.
func (w *Worker) pollRepo(ctx context.Context, src PollSource, r RepoRoute) error {
	owner, name, ok := splitRepo(r.Repo)
	if !ok {
		return fmt.Errorf("not an owner/name repository: %q", r.Repo)
	}

	cursor, seeded, err := w.DB.PollCursor(r.Repo)
	if err != nil {
		return err
	}
	since := cursor.Since
	if !seeded {
		// A repository never polled is SEEDED: its snapshot is written and
		// nothing is delivered. Otherwise a fresh install dispatches an agent
		// for every issue in the repository's history. The listing still has
		// to be made -- the snapshot is what it produces -- so the zero time
		// asks for everything, once.
		since = time.Time{}
	}

	subjects, err := src.ListSubjectsUpdatedSince(ctx, owner, name, since)
	if err != nil {
		return err
	}
	// Ascending, so the cursor can be advanced to the last subject fully
	// processed. The API is asked for this order; sorting here makes the
	// property the code relies on independent of that.
	sort.Slice(subjects, func(i, j int) bool {
		return subjects[i].UpdatedAt.Before(subjects[j].UpdatedAt)
	})

	snap, err := w.DB.PollSnapshot(r.Repo)
	if err != nil {
		return err
	}

	// The default branch, hoisted above the subject loop: the merge
	// suppression below must compare against THIS branch specifically, not
	// merely "some merge happened somewhere in this pass".
	branch := defaultBranchOf(r)

	var (
		rows       []store.PollSubject
		deliveries []Delivery
		high       = cursor.Since
		merged     bool
	)
	for _, s := range subjects {
		cur := pollSubject{
			Number: s.Number, IsPR: s.IsPullRequest, State: s.State,
			Labels: s.Labels, UpdatedAt: s.UpdatedAt,
		}

		prev, known := snap[s.Number]
		// The one extra fetch a pass can make, and only for a pull request
		// that JUST closed on an ALREADY-seeded pass: the listing carries no
		// merged flag and no base ref, and without them a merge arms no tend
		// sweep. Gated on seeded too, not only known -- a seeding pass can
		// already have a snapshot row (a repeat seed after a wipe, or a
		// number an earlier pass already recorded) but must deliver, and
		// therefore fetch, nothing.
		if seeded && known && cur.IsPR && isOpen(prev.State) && !isOpen(cur.State) {
			pr, err := src.PullRequest(ctx, owner, name, s.Number)
			if err != nil {
				return err
			}
			cur.Merged = pr.Merged
			cur.MergedBase = pr.BaseRef
		}

		rows = append(rows, cur.row(r.Repo))
		if s.UpdatedAt.After(high) {
			high = s.UpdatedAt
		}
		if !seeded {
			continue
		}
		if d, ok := pollDelivery(r.Repo, prev, known, cur); ok {
			deliveries = append(deliveries, d)
			// Scoped to the branch actually being compared below: a merge
			// into release/1.x must not suppress a genuine push to master.
			// branch != "" follows IsMergeInto's rule (see work.go) -- a
			// repository whose targets name no default branch names no
			// branch to compare against, so an empty MergedInto must not
			// match an empty branch.
			if d.MergedInto == branch && branch != "" {
				merged = true
			}
		}
	}

	// The default branch, for the event nothing else can report.
	head := ""
	if branch != "" {
		// Carried forward from the cursor, so a FAILING read costs this pass
		// its push detection and nothing else. Returning the error here
		// instead would discard the subject stream with it, and a permanent
		// failure -- a default_branch typo in a loop's yaml, a repository
		// renamed master->main with stale config, both 404s -- would then wedge
		// the repository forever: no issue, close or merge ever delivered
		// again, and a cursor that never advances, so every pass re-lists the
		// whole paginated history against the rate limit. defaultBranchOf
		// takes the FIRST target's branch, so one misconfigured project would
		// take every other project watching the repository down with it. The
		// carried-forward head arms no spurious push: it compares equal below.
		// (src.PullRequest above does still abort the repository, deliberately:
		// skipping it loses a merge, and there is one high-water mark to
		// resume from.)
		head = cursor.HeadSHA
		if got, headErr := src.BranchHead(ctx, owner, name, branch); headErr != nil {
			slog.Error("cannot read the default branch head; this pass reports no push for it",
				"repo", r.Repo, "branch", branch, "err", headErr)
		} else {
			head = got
		}
		// Suppressed when a merge INTO THIS BRANCH in this same pass explains
		// the move: both arm the same sweep, and arming it twice dispatches
		// two tend agents for one merge. A merge into a different branch must
		// not suppress this: the default branch moving is then unexplained
		// and still needs its own push delivery.
		if seeded && head != "" && head != cursor.HeadSHA && !merged {
			deliveries = append(deliveries, Delivery{Repo: r.Repo, PushedTo: branch})
		}
	}

	// Written BEFORE the deliveries go out. At-most-once: a crash between
	// this write and the loop below could otherwise redeliver a window
	// already acted on. The cost is the mirror image, not a comfort -- a
	// crash or shutdown in that same gap DROPS whatever had not yet gone
	// out, because the next pass diffs new-against-new and sees no change
	// where this pass's undelivered remainder used to be. Cron's full
	// reconcile tick is what recovers from that loss; this ordering only
	// trades which failure mode a crash produces.
	if err := w.DB.SavePollSubjects(r.Repo, rows); err != nil {
		return err
	}
	if err := w.DB.SavePollCursor(store.PollCursor{
		Repo: r.Repo, Since: high, HeadSHA: head, SeededAt: seededAt(cursor, seeded, w.Now()),
	}); err != nil {
		return err
	}

	if !seeded {
		slog.Info("seeded a repository for polling; delivering nothing for it",
			"repo", r.Repo, "subjects", len(rows))
		return nil
	}
	for _, d := range deliveries {
		if ctx.Err() != nil {
			return nil
		}
		slog.Info("polled change", "repo", d.Repo, "number", d.Number,
			"closed_issue", d.ClosedIssue, "closed_pr", d.ClosedPR,
			"reopened", d.Reopened, "merged_into", d.MergedInto,
			"pushed_to", d.PushedTo, "epic_ready", d.EpicReady)
		w.PollDeliver(ctx, d)
	}
	return nil
}

// seededAt keeps the original seeding time across every later write.
func seededAt(c store.PollCursor, seeded bool, now time.Time) time.Time {
	if seeded {
		return c.SeededAt
	}
	return now.UTC()
}

// defaultBranchOf returns the first default branch this repository's targets
// name, or "" when none does.
//
// KNOWN LIMITATION: when two loops watch the same repository with different
// default branches, only the first one's pushes are ever detected -- there is
// one head SHA per repository in poll_cursors, not one per branch, so the
// second branch's moves are silently absorbed into "no change" against the
// first branch's cursor. Fixing that needs a per-branch head store, which is
// a schema change this task does not take. The periodic tend check still
// catches the resulting staleness later by asking git directly, so a push to
// the second branch is delayed rather than lost, only late.
func defaultBranchOf(r RepoRoute) string {
	for _, t := range r.Targets {
		if t.DefaultBranch != "" {
			return t.DefaultBranch
		}
	}
	return ""
}

// splitRepo splits "owner/name".
func splitRepo(repo string) (string, string, bool) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}
