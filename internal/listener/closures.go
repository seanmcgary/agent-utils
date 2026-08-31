package listener

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/store"
)

// recordClosure writes what a delivery says about whether its issue is closed.
//
// It is what makes `sessions list` able to hide finished work without asking
// GitHub anything: the report reads the closures table, and this is the writer
// that keeps it current while the daemon is up. reconcileClosed is the other
// one, and covers the window the daemon was down for.
//
// It is deliberately cheap and unconditional. It reads no configuration, takes
// no loop lock, makes no GitHub call and does not care whether any loop can be
// opened -- the payload already carries the fact. Everything else in the
// delivery path can fail without losing it.
//
// Once per PROJECT, not once per target: the closure key carries no loop, so
// two loops of one project watching the same repository would write the same
// row twice. t.Repo is what is recorded rather than d.Repo, because a dispatch
// row records the loop's CONFIGURED spelling of the repository, and the report
// joins the two on that string.
func (w *Worker) recordClosure(targets []Target, d Delivery) {
	closed := d.ClosedIssue || d.ClosedPR
	if d.Number <= 0 || (!closed && !d.Reopened) {
		return
	}
	// A worker built without a database serves the tests that drive Deliver
	// through stub hooks. issueBusy makes the same check for the same reason.
	if w.DB == nil {
		return
	}

	type key struct{ projectID, repo string }
	seen := map[key]bool{}
	for _, t := range targets {
		k := key{t.ProjectID, t.Repo}
		if seen[k] {
			continue
		}
		seen[k] = true

		s := w.DB.Project(t.ProjectID)
		var err error
		if closed {
			err = s.MarkClosed(t.Repo, d.Number, time.Now().UTC())
		} else {
			err = s.ClearClosed(t.Repo, d.Number)
		}
		if err != nil {
			// Logged, never fatal to the delivery. The issue pass, the tend
			// sweep and the worktree cleanup are the work; this is a report's
			// bookkeeping, and losing a row costs a hidden line in a table,
			// not a dispatch.
			slog.Error("cannot record whether this issue is closed",
				"project", t.ProjectName, "repo", t.Repo, "number", d.Number,
				"closed", closed, "err", err)
		}
	}
}

// reconcileClosed asks GitHub which of the issues this machine still believes
// are open have closed since it last looked.
//
// Serve runs it once, at start, beside the orphan sweep and for the same
// reason: a delivery tells this daemon about a close only while it is running,
// and everything that closed while it was down is a fact nothing else will ever
// deliver. Without this pass a `sessions list` on a machine that has been up
// for a day would still show months of finished work, because the closures
// table would only ever hold what today's deliveries reported.
//
// It is NOT on a timer. Between two starts the deliveries are the source of
// truth, and re-listing every watched repository on an interval would spend the
// token budget re-learning what the daemon already knows.
//
// The candidate set shrinks as it works. BelievedOpen excludes what is already
// marked closed, so each restart asks only about issues that were still open
// the last time -- a machine with years of history does not pay for that
// history on every start.
func (w *Worker) reconcileClosed(ctx context.Context) {
	if w.DB == nil {
		return
	}
	refs, err := w.DB.BelievedOpen()
	if err != nil {
		slog.Error("cannot read the issues to check for closure", "err", err)
		return
	}
	if len(refs) == 0 {
		return
	}

	// One token read and one client for the whole pass, as every other pass
	// does. This one has nothing to memoise -- it makes list calls, not
	// repeated single-issue fetches -- so acc.gh's DeliveryCache simply passes
	// them through.
	acc, err := w.access()
	if err != nil {
		slog.Error("cannot read the github token to check for closed issues", "err", err)
		return
	}

	for _, g := range groupByRepo(refs) {
		// Checked per repository, not only on entry: a machine with many
		// projects makes one round trip per repository here, and a shutdown
		// must not wait for all of them.
		if ctx.Err() != nil {
			return
		}
		w.reconcileRepo(ctx, acc, g)
	}
}

// repoGroup is every believed-open number of one project's repository.
type repoGroup struct {
	ProjectID string
	Repo      string
	Numbers   []int
}

// groupByRepo collapses the refs onto one entry per {project, repo}, in a
// stable order so two runs log the same way.
func groupByRepo(refs []store.IssueRef) []repoGroup {
	index := map[string]int{}
	var out []repoGroup
	for _, r := range refs {
		k := r.ProjectID + "\x00" + r.Repo
		i, ok := index[k]
		if !ok {
			i = len(out)
			index[k] = i
			out = append(out, repoGroup{ProjectID: r.ProjectID, Repo: r.Repo})
		}
		out[i].Numbers = append(out[i].Numbers, r.Number)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectID != out[j].ProjectID {
			return out[i].ProjectID < out[j].ProjectID
		}
		return out[i].Repo < out[j].Repo
	})
	for i := range out {
		sort.Ints(out[i].Numbers)
	}
	return out
}

// reconcileRepo marks every believed-open number of one repository that GitHub
// no longer lists as open.
//
// One repository's failure is reported and skipped, never returned: a token
// that cannot see one project's repository must not stop the others from being
// reconciled, and this runs during startup.
func (w *Worker) reconcileRepo(ctx context.Context, acc *access, g repoGroup) {
	owner, name, ok := strings.Cut(g.Repo, "/")
	if !ok || owner == "" || name == "" {
		// A dispatch row whose repo is not owner/name cannot be asked about.
		// It is data this daemon did not write in that shape, so it is worth
		// one line rather than a silent skip.
		slog.Warn("skipping a repository that is not in owner/name form",
			"repo", safeText(g.Repo), "project", g.ProjectID)
		return
	}

	open, err := openNumbers(ctx, acc, owner, name)
	if err != nil {
		slog.Error("cannot list open issues to check for closures",
			"repo", g.Repo, "project", g.ProjectID, "err", err)
		return
	}

	s := w.DB.Project(g.ProjectID)
	now := time.Now().UTC()
	marked := 0
	for _, n := range g.Numbers {
		if open[n] {
			continue
		}
		// Absent from BOTH open lists. That covers a closed issue, a closed or
		// merged pull request, and an issue that no longer exists at all
		// (deleted, transferred, or dispatched against a repository this
		// project no longer watches). All four are finished work as far as the
		// report is concerned, and a reopen puts the row back through the
		// live delivery path.
		if err := s.MarkClosed(g.Repo, n, now); err != nil {
			slog.Error("cannot record a closed issue",
				"repo", g.Repo, "number", n, "err", err)
			continue
		}
		marked++
	}
	if marked > 0 {
		slog.Info("issues closed while the listener was not running",
			"repo", g.Repo, "project", g.ProjectID, "closed", marked)
	}
}

// openNumbers returns every issue and pull request number GitHub currently
// lists as open in one repository.
//
// Two calls, not one. The issues endpoint returns pull requests alongside
// issues, but ghub.ConvertIssues drops them -- deliberately, because every
// other caller decides from issue labels -- so a pull request would be absent
// from the issue list and get marked closed while it is open. Issues and pull
// requests share one number space, so the union is exactly "the numbers that
// are open".
//
// Neither call is optional and neither failure is tolerated: a partial answer
// here reads as "these numbers are closed", which is the one wrong direction.
func openNumbers(ctx context.Context, acc *access, owner, name string) (map[int]bool, error) {
	issues, err := acc.gh.ListOpenIssues(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	prs, err := acc.gh.ListOpenPullRequests(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	out := make(map[int]bool, len(issues)+len(prs))
	for _, i := range issues {
		out[i.Number] = true
	}
	for _, p := range prs {
		out[p.Number] = true
	}
	return out, nil
}
