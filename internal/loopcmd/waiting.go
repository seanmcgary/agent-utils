package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/lock"
)

// maxReleasePerPass bounds the issues one waiting pass may release. It is the
// epic sweep's maxPromotePerSweep, for the epic sweep's reasons: a release is
// one label write, and the waiting label costs one triage permission to apply,
// so the pass's write fan-out must not be proportional to what a triager cares
// to label. What is left over waits for the next pass and is logged.
const maxReleasePerPass = 25

// maxWaitReadsPerPass bounds the GitHub READS one waiting pass may make, as
// maxBlockerReadsPerSweep does for the epic sweep, and for its reason: the
// steady state -- every waiting issue still waiting -- spends no write budget,
// so only a read budget bounds it. A waiting issue costs roughly one
// blocked_by read, plus, under wait_for_checks, a closing-pull-request read
// and two or three run reads per blocker; 300 covers dozens of waiting issues
// and stays far inside the 5000/hour token every loop on the machine shares.
const maxWaitReadsPerPass = 300

// maxComparesPerCheck bounds the compare calls one workflow check makes while
// looking for a later green run that contains a merge commit. The newest green
// run either contains the commit or is older than it, so the first compare
// almost always answers; the bound is for a history that diverged.
const maxComparesPerCheck = 3

// failedConclusions are the run conclusions the pass reports as a failed check.
// "cancelled" is deliberately absent: a run cancelled by a concurrency group is
// the ordinary fate of a merge's run when another merge lands behind it, and
// the newer run is what the pass then waits for. Reporting it would comment on
// every merge train.
var failedConclusions = map[string]bool{
	"failure":         true,
	"timed_out":       true,
	"startup_failure": true,
}

// failMarker is the hidden line a failed-check comment carries, keyed by run.
// The pass reads the waiting issue's comments for it before posting, which is
// what keeps the comment to ONE per failed run across passes and across daemon
// restarts, with no state on this machine. A re-run of the same run keeps its
// id, so a second failed attempt is not a second comment either.
func failMarker(runID int64) string {
	return fmt.Sprintf("<!-- agent-utils:wait-check-failed run=%d -->", runID)
}

// errWaitBudget reports that the pass's read budget is spent. The issue being
// considered is held, never released on a guess.
var errWaitBudget = errors.New("the waiting pass's read budget is spent")

// WaitSweep reconsiders the given waiting issues of this loop, because a
// delivery says one of their blockers just closed. It TAKES the loop lock.
//
// numbers are issues in THIS loop's repository; the daemon found them through
// the closed issue's `blocking` edges and checked their repository before
// handing them over. Each is re-read here, under the lock, so the decision is
// made from the issue as it stands rather than as the edge listing described
// it.
//
// # What it writes, and where
//
// Two writes, both on the waiting issue, both addressed with THIS loop's
// owner/repo: the label swap that releases it, and at most one comment per
// failed check run. The blocker's repository -- usually another one -- is only
// ever read. That is the property that makes it safe to act on a closure in a
// repository this loop does not watch: the closure chooses the MOMENT, and
// the waiting label plus the dependency edge, both set by someone with triage
// here, chose WHICH issue.
//
// # Why it takes the loop lock
//
// For the epic sweep's reason (see EpicSweep): the label it adds is the
// trigger, and the next tick to see it dispatches. Holding the lock makes the
// release and the dispatch decision serial.
func WaitSweep(ctx context.Context, cfg *config.Config, deps Deps, numbers []int) (Summary, error) {
	if !waitEnabled(cfg) || len(numbers) == 0 {
		return Summary{}, nil
	}
	if deps.Wait == nil {
		return Summary{}, fmt.Errorf("waiting pass: %w", ghub.ErrNoWaitReader)
	}
	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		return Summary{}, fmt.Errorf("waiting pass: lock loop %s: %w", cfg.Name, err)
	}
	defer l.Release()

	p := newWaitPass(cfg, deps.Wait)
	owner, repo := cfg.RepoOwner(), cfg.RepoName()
	for _, n := range numbers {
		if n <= 0 {
			continue
		}
		if err := p.spend(); err != nil {
			slog.Warn("waiting pass hit its read cap; the rest wait for the next pass",
				"loop", cfg.Name, "issue", n)
			break
		}
		iss, err := p.r.Issue(ctx, owner, repo, n)
		if err != nil {
			slog.Warn("waiting pass cannot read the waiting issue; holding it",
				"loop", cfg.Name, "repo", cfg.Repo, "issue", n, "err", err)
			continue
		}
		p.consider(ctx, iss, "")
	}
	return p.sum, nil
}

// WaitSweepRepo reconsiders every open issue of this loop carrying its waiting
// label, and TAKES the loop lock.
//
// blockerRepo narrows the pass to the issues with at least one blocker in that
// repository; empty considers them all. The daemon passes the repository of a
// completed workflow run, so a green run in one repository costs a read per
// waiting issue and re-decides only the ones it could have changed. It passes
// empty for its start-up catch-up.
func WaitSweepRepo(ctx context.Context, cfg *config.Config, deps Deps, blockerRepo string) (Summary, error) {
	if !waitEnabled(cfg) {
		return Summary{}, nil
	}
	if deps.Wait == nil {
		return Summary{}, fmt.Errorf("waiting pass: %w", ghub.ErrNoWaitReader)
	}
	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		return Summary{}, fmt.Errorf("waiting pass: lock loop %s: %w", cfg.Name, err)
	}
	defer l.Release()
	return waitSweepListed(ctx, cfg, deps, blockerRepo)
}

// WaitSweepAll is the cron backstop: every open waiting issue of this loop.
//
// It takes NO lock, for EpicSweepAll's reason: its only caller is Tick, whose
// caller already holds it, and a second Acquire in the same process answers
// ErrHeld -- which would make the backstop silently release nothing, forever.
func WaitSweepAll(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error) {
	if !waitEnabled(cfg) {
		return Summary{}, nil
	}
	if deps.Wait == nil {
		return Summary{}, fmt.Errorf("waiting pass: %w", ghub.ErrNoWaitReader)
	}
	return waitSweepListed(ctx, cfg, deps, "")
}

// waitSweepListed lists this loop's waiting issues and considers each. The
// CALLER holds the lock.
func waitSweepListed(ctx context.Context, cfg *config.Config, deps Deps, blockerRepo string) (Summary, error) {
	p := newWaitPass(cfg, deps.Wait)
	if err := p.spend(); err != nil {
		return p.sum, err
	}
	issues, err := p.r.ListOpenIssuesLabeled(ctx, cfg.RepoOwner(), cfg.RepoName(), cfg.Labels.Waiting)
	if err != nil {
		return p.sum, fmt.Errorf("waiting pass: list issues labelled %q: %w", cfg.Labels.Waiting, err)
	}
	for _, iss := range issues {
		if p.reads <= 0 {
			slog.Warn("waiting pass hit its read cap; the rest wait for the next pass",
				"loop", cfg.Name, "repo", cfg.Repo, "released", p.sum.Released)
			break
		}
		p.consider(ctx, iss, blockerRepo)
	}
	return p.sum, nil
}

// waitEnabled reports whether this loop has a waiting label at all. A loop
// without one has nothing to release, and every entry point returns at once
// for it -- the common case, and it must cost nothing.
func waitEnabled(cfg *config.Config) bool {
	return strings.TrimSpace(cfg.Labels.Waiting) != ""
}

// waitPass is one pass's budgets and memos.
//
// The memos live for ONE pass, never longer, for the reason listener.access
// gives about its own: a verdict that outlived its pass would answer the next
// one -- raised because a run went green -- with the answer from before it did.
// Within a pass they collapse the repeated reads that several waiting issues
// sharing one blocker would otherwise make.
type waitPass struct {
	cfg *config.Config
	r   ghub.WaitReader

	reads    int
	releases int
	sum      Summary

	branches map[string]string       // repo (lowered) -> default branch
	checks   map[string]checkVerdict // repo|sha|workflow -> verdict
}

func newWaitPass(cfg *config.Config, r ghub.WaitReader) *waitPass {
	return &waitPass{
		cfg: cfg, r: r,
		reads:    maxWaitReadsPerPass,
		releases: maxReleasePerPass,
		branches: map[string]string{},
		checks:   map[string]checkVerdict{},
	}
}

// spend takes one read from the budget.
func (p *waitPass) spend() error {
	if p.reads <= 0 {
		return errWaitBudget
	}
	p.reads--
	return nil
}

// verdict is what the pass concluded about one blocker, or about one workflow
// on one merge commit.
type verdict int

const (
	verdictReady verdict = iota
	verdictWait
	verdictFailed
)

// checkVerdict is one reason a waiting issue is not released yet, or the
// ready answer that releases it. Blocker fields are filled in by the caller
// that knows which blocker it was asking about.
type checkVerdict struct {
	kind   verdict
	reason string

	blocker     int
	blockerRepo string

	repo     string // where the commit and the workflow live
	commit   string
	workflow string
	run      ghub.WorkflowRun // the failed run, for verdictFailed
}

// consider decides one waiting issue and releases it when every blocker is
// done.
//
// blockerRepo, when set, skips an issue none of whose blockers lives there;
// see WaitSweepRepo.
func (p *waitPass) consider(ctx context.Context, iss ghub.Issue, blockerRepo string) {
	cfg := p.cfg
	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	// The guards are on the write target, and every one is re-checked here
	// whatever the caller already filtered on. The write below is addressed by
	// NUMBER against this loop's repository, so an issue from anywhere else
	// would label whichever local issue shares its number -- the epic sweep's
	// cross-repository rule, for the same reason.
	if iss.Number <= 0 || !iss.InRepo(owner, repo) {
		slog.Warn("waiting pass skipped an issue outside this loop's repository",
			"loop", cfg.Name, "repo", cfg.Repo, "issue", iss.Number, "issue_repo", iss.Repo)
		return
	}
	if !iss.IsOpen() || !iss.HasLabel(cfg.Labels.Waiting) {
		slog.Info("waiting pass skipped an issue that is no longer waiting",
			"loop", cfg.Name, "repo", cfg.Repo, "issue", iss.Number,
			"state", iss.State, "waiting_label", cfg.Labels.Waiting)
		return
	}

	if err := p.spend(); err != nil {
		slog.Warn("waiting pass hit its read cap; holding this issue",
			"loop", cfg.Name, "repo", cfg.Repo, "issue", iss.Number)
		return
	}
	blockers, err := p.r.BlockedBy(ctx, owner, repo, iss.Number)
	if err != nil {
		// Held. An unreadable blocker list says nothing, and the alternative
		// reading releases an issue whose blockers may be open.
		slog.Warn("waiting pass cannot read blockers; holding this issue",
			"loop", cfg.Name, "repo", cfg.Repo, "issue", iss.Number, "err", err)
		return
	}
	if blockerRepo != "" && !anyInRepo(blockers, blockerRepo) {
		return
	}
	if len(blockers) == 0 {
		// NOT "every blocker is done". An issue labelled waiting with no edge
		// yet is the ordinary moment between a job applying the label and
		// recording the dependency, and releasing it then would start the
		// work before the thing it waits for exists. A human who means "go"
		// applies the trigger and removes the label.
		slog.Info("waiting issue declares no blocker; leaving it waiting",
			"loop", cfg.Name, "repo", cfg.Repo, "issue", iss.Number)
		return
	}

	var pending []checkVerdict
	for _, b := range blockers {
		vs, err := p.blocker(ctx, b)
		if err != nil {
			slog.Warn("waiting pass cannot judge a blocker; holding this issue",
				"loop", cfg.Name, "repo", cfg.Repo, "issue", iss.Number,
				"blocker", b.Number, "blocker_repo", b.Repo, "err", err)
			return
		}
		pending = append(pending, vs...)
	}

	if len(pending) > 0 {
		for _, v := range pending {
			attrs := []any{"loop", cfg.Name, "repo", cfg.Repo, "issue", iss.Number,
				"blocker", v.blocker, "blocker_repo", v.blockerRepo, "reason", v.reason}
			if v.workflow != "" {
				attrs = append(attrs, "workflow", v.workflow, "commit", shortSHA(v.commit))
			}
			if v.kind == verdictFailed {
				attrs = append(attrs, "run_url", v.run.HTMLURL)
				slog.Warn("waiting issue's required check failed; still waiting", attrs...)
				p.reportFailure(ctx, iss, v)
				continue
			}
			slog.Info("issue is still waiting", attrs...)
		}
		return
	}

	if p.releases <= 0 {
		slog.Warn("waiting pass hit its release cap; this issue waits for the next pass",
			"loop", cfg.Name, "repo", cfg.Repo, "issue", iss.Number, "released", p.sum.Released)
		return
	}
	p.release(ctx, iss, len(blockers))
}

// release swaps the waiting label for the trigger.
//
// Two writes, the ADD first. If the second fails the issue carries both
// labels, and the waiting label keeps vetoing it -- so a half-done release
// dispatches nothing, and the next pass, finding it still waiting with every
// blocker done, finishes the job. The other order would leave an issue with
// neither label after a failed add: out of the loop with nothing to say why.
func (p *waitPass) release(ctx context.Context, iss ghub.Issue, blockers int) {
	cfg := p.cfg
	owner, repo := cfg.RepoOwner(), cfg.RepoName()
	if err := p.r.EditLabels(ctx, owner, repo, iss.Number, []string{cfg.Labels.Trigger}, nil); err != nil {
		slog.Error("waiting pass cannot apply the trigger; the issue keeps waiting",
			"loop", cfg.Name, "repo", cfg.Repo, "issue", iss.Number,
			"label", cfg.Labels.Trigger, "err", err)
		return
	}
	if err := p.r.EditLabels(ctx, owner, repo, iss.Number, nil, []string{cfg.Labels.Waiting}); err != nil {
		slog.Error("waiting pass applied the trigger but cannot remove the waiting label; "+
			"the waiting label still vetoes dispatch, and the next pass retries the removal",
			"loop", cfg.Name, "repo", cfg.Repo, "issue", iss.Number,
			"label", cfg.Labels.Waiting, "err", err)
		return
	}
	p.releases--
	p.sum.Released++
	// One line per release, naming both labels: a GitHub write with no human
	// and no agent in the loop, and this line is the only record of it on this
	// machine.
	slog.Info("released a waiting issue: every blocker is done",
		"loop", cfg.Name, "repo", cfg.Repo, "issue", iss.Number, "blockers", blockers,
		"removed", cfg.Labels.Waiting, "added", cfg.Labels.Trigger,
		"checks", cfg.WaitForChecks)
}

// blocker judges one blocker and returns why it is not done yet, or nothing
// when it is.
func (p *waitPass) blocker(ctx context.Context, b ghub.Issue) ([]checkVerdict, error) {
	wait := func(reason string) []checkVerdict {
		return []checkVerdict{{kind: verdictWait, reason: reason, blocker: b.Number, blockerRepo: b.Repo}}
	}
	if b.IsOpen() {
		return wait("the blocker is open"), nil
	}
	// Closed is not enough: a blocker closed as not planned or as a duplicate
	// did not deliver what its dependent was waiting for. Empty is held too --
	// it is an issue closed before GitHub recorded reasons, and the
	// conservative reading of an unknown reason is "not completed".
	if !strings.EqualFold(b.StateReason, "completed") {
		reason := b.StateReason
		if reason == "" {
			reason = "no recorded reason"
		}
		return wait("the blocker closed as " + reason + ", not completed"), nil
	}
	if len(p.cfg.WaitForChecks) == 0 {
		return nil, nil
	}

	bOwner, bName, ok := strings.Cut(b.Repo, "/")
	if !ok || bOwner == "" || bName == "" {
		return nil, fmt.Errorf("the blocker's repository is unknown (%q)", b.Repo)
	}
	if err := p.spend(); err != nil {
		return nil, err
	}
	prs, err := p.r.ClosingPullRequests(ctx, bOwner, bName, b.Number)
	if err != nil {
		return nil, err
	}

	var out []checkVerdict
	merged := 0
	for _, pr := range prs {
		if !pr.Merged {
			continue
		}
		if pr.MergeCommit == "" {
			// Merged with no merge commit is not a shape GitHub produces; held
			// rather than read as "no merged pull request", which would release
			// without any check.
			return nil, fmt.Errorf("closing pull request #%d is merged but names no merge commit", pr.Number)
		}
		merged++
		// The merge commit lives in the PULL REQUEST's repository, which is
		// the blocker's in every case this was built for. A pull request in
		// another repository may close an issue too, and its commit and its
		// workflows are then over there.
		repo := pr.Repo
		if repo == "" {
			repo = b.Repo
		}
		for _, wf := range p.cfg.WaitForChecks {
			v, err := p.check(ctx, repo, pr.MergeCommit, wf)
			if err != nil {
				return nil, err
			}
			if v.kind == verdictReady {
				continue
			}
			v.blocker, v.blockerRepo = b.Number, b.Repo
			out = append(out, v)
		}
	}
	if merged == 0 {
		// A blocker closed as completed with no merged pull request is a
		// human's explicit "done": there is no commit for a check to have run
		// on, and waiting for one would wait forever.
		slog.Info("blocker closed as completed with no merged pull request; no check to wait for",
			"loop", p.cfg.Name, "blocker", b.Number, "blocker_repo", b.Repo)
	}
	return out, nil
}

// check decides whether workflow has a green run covering commit on repo's
// default branch.
//
// A run covers the commit when its head IS the commit, or when its head is a
// later commit that contains it. The second case is not a nicety: a workflow
// under a concurrency group cancels the merge's own run when the next merge
// lands, and only the newer run ever goes green.
//
// No run at all is a WAIT, never a pass. A closure can arrive before the
// push-triggered run on the merge commit even exists, and reading that silence
// as success is the race this whole option exists to close.
func (p *waitPass) check(ctx context.Context, repo, commit, workflow string) (checkVerdict, error) {
	key := strings.ToLower(repo) + "|" + strings.ToLower(commit) + "|" + strings.ToLower(workflow)
	if v, ok := p.checks[key]; ok {
		return v, nil
	}
	v, err := p.checkUncached(ctx, repo, commit, workflow)
	if err != nil {
		return v, err
	}
	p.checks[key] = v
	return v, nil
}

func (p *waitPass) checkUncached(ctx context.Context, repo, commit, workflow string) (checkVerdict, error) {
	v := checkVerdict{repo: repo, commit: commit, workflow: workflow}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return v, fmt.Errorf("repository %q is not owner/name", repo)
	}
	branch, err := p.defaultBranch(ctx, owner, name)
	if err != nil {
		return v, err
	}

	if err := p.spend(); err != nil {
		return v, err
	}
	green, err := p.r.WorkflowRuns(ctx, owner, name, ghub.RunQuery{
		Workflow: workflow, Branch: branch, Status: "success", PerPage: 20,
	})
	if err != nil {
		return v, err
	}
	compares := 0
scan:
	for _, run := range green {
		// Re-checked whatever the filter asked for: the decision is "a
		// SUCCESSFUL COMPLETED run", and it must not rest on a query
		// parameter's semantics.
		if run.Status != "completed" || run.Conclusion != "success" {
			continue
		}
		if strings.EqualFold(run.HeadSHA, commit) {
			v.kind = verdictReady
			return v, nil
		}
		if compares >= maxComparesPerCheck {
			break scan
		}
		if err := p.spend(); err != nil {
			return v, err
		}
		compares++
		status, err := p.r.CompareCommits(ctx, owner, name, commit, run.HeadSHA)
		if err != nil {
			return v, err
		}
		switch status {
		case "identical", "ahead":
			v.kind = verdictReady
			return v, nil
		case "behind":
			// The newest green run predates the commit, and every run after
			// it in this list is older still.
			break scan
		}
	}

	// No green run covers it. What happened to the commit's OWN run decides
	// between "still waiting" and "failed".
	if err := p.spend(); err != nil {
		return v, err
	}
	own, err := p.r.WorkflowRuns(ctx, owner, name, ghub.RunQuery{
		Workflow: workflow, Branch: branch, HeadSHA: commit, PerPage: 10,
	})
	if err != nil {
		return v, err
	}
	if len(own) == 0 {
		v.kind, v.reason = verdictWait, "no run of the workflow on the merge commit yet"
		return v, nil
	}
	newest := own[0]
	if newest.Status != "completed" {
		v.kind, v.reason = verdictWait, "the workflow's run on the merge commit is "+newest.Status
		return v, nil
	}
	if failedConclusions[newest.Conclusion] {
		v.kind, v.reason, v.run = verdictFailed, "the workflow's run on the merge commit concluded "+newest.Conclusion, newest
		return v, nil
	}
	v.kind, v.reason = verdictWait, "the workflow's run on the merge commit concluded "+newest.Conclusion+
		"; waiting for a later green run that includes it"
	return v, nil
}

// defaultBranch returns repo's default branch, once per pass.
func (p *waitPass) defaultBranch(ctx context.Context, owner, name string) (string, error) {
	key := strings.ToLower(owner + "/" + name)
	if b, ok := p.branches[key]; ok {
		return b, nil
	}
	if err := p.spend(); err != nil {
		return "", err
	}
	b, err := p.r.DefaultBranch(ctx, owner, name)
	if err != nil {
		return "", err
	}
	p.branches[key] = b
	return b, nil
}

// reportFailure posts one comment on the waiting issue naming a failed check,
// unless an earlier pass already did.
//
// A comment, not a label: the issue keeps waiting, and a later green run still
// releases it, so nothing about its state changed -- but a human should know
// the thing it waits on is red, because nothing else will tell them.
func (p *waitPass) reportFailure(ctx context.Context, iss ghub.Issue, v checkVerdict) {
	cfg := p.cfg
	owner, repo := cfg.RepoOwner(), cfg.RepoName()
	marker := failMarker(v.run.ID)

	if err := p.spend(); err != nil {
		slog.Warn("waiting pass hit its read cap before reporting a failed check",
			"loop", cfg.Name, "repo", cfg.Repo, "issue", iss.Number, "run_url", v.run.HTMLURL)
		return
	}
	bodies, err := p.r.CommentBodies(ctx, owner, repo, iss.Number)
	if err != nil {
		// Not posted. Without the comment list there is no way to tell this
		// pass's comment from a repeat, and one missing comment beats a
		// comment on every pass.
		slog.Warn("cannot read comments to report a failed check; not commenting",
			"loop", cfg.Name, "repo", cfg.Repo, "issue", iss.Number, "err", err)
		return
	}
	for _, b := range bodies {
		if strings.Contains(b, marker) {
			return
		}
	}

	body := fmt.Sprintf("%s\nThis issue is waiting on %s#%d. The workflow `%s` failed on its merge commit `%s` in %s: %s\n\n"+
		"It keeps waiting. A later successful run of `%s` that includes that commit releases it to `%s`.",
		marker, v.blockerRepo, v.blocker, v.workflow, shortSHA(v.commit), v.repo, v.run.HTMLURL,
		v.workflow, cfg.Labels.Trigger)
	if err := p.r.PostComment(ctx, owner, repo, iss.Number, body); err != nil {
		slog.Error("cannot comment about a failed check",
			"loop", cfg.Name, "repo", cfg.Repo, "issue", iss.Number, "err", err)
		return
	}
	slog.Info("commented on a waiting issue about a failed check",
		"loop", cfg.Name, "repo", cfg.Repo, "issue", iss.Number,
		"workflow", v.workflow, "run_url", v.run.HTMLURL)
}

// anyInRepo reports whether any of the issues lives in repo.
func anyInRepo(issues []ghub.Issue, repo string) bool {
	for _, i := range issues {
		if strings.EqualFold(i.Repo, repo) {
			return true
		}
	}
	return false
}

// shortSHA is the first twelve characters of a sha, for logs and comments.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
