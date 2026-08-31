// Package worktree manages the git worktrees that dispatched agents run in.
package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Manager creates worktrees for one loop from one primary checkout.
type Manager struct {
	checkoutBaseDir string
	worktreeDir     string
	loop            string
	defaultBranch   string
}

// New returns a Manager.
func New(checkoutBaseDir, worktreeDir, loop, defaultBranch string) *Manager {
	return &Manager{
		checkoutBaseDir: checkoutBaseDir,
		worktreeDir:     worktreeDir,
		loop:            loop,
		defaultBranch:   defaultBranch,
	}
}

// PathForIssue returns the stable worktree path for an issue.
func (m *Manager) PathForIssue(number int) string {
	return filepath.Join(m.worktreeDir, m.loop, fmt.Sprintf("issue-%d", number))
}

// PathForPR returns the stable worktree path for a pull request.
func (m *Manager) PathForPR(number int) string {
	return filepath.Join(m.worktreeDir, m.loop, fmt.Sprintf("pr-%d", number))
}

// Fetch updates the primary checkout. It never changes its branch and never
// edits its files.
func (m *Manager) Fetch() error {
	return m.git(m.checkoutBaseDir, "fetch", "origin", "--prune")
}

// EnsureIssue creates the worktree for an issue if it does not exist. The path
// is stable across ticks, so a resumed run finds the branch state it left.
func (m *Manager) EnsureIssue(number int) (string, error) {
	path := m.PathForIssue(number)
	if exists(path) {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create worktree parent: %w", err)
	}
	// Create the worktree DETACHED at an explicit start point.
	//
	// Two reasons. First, "worktree add -B <branch> <path>" with no start point
	// branches from whatever the primary checkout has checked out, which this
	// program does not control. Second, both reference loops make branch
	// resolution the AGENT's job: plan-feature may already have created the
	// feature branch and committed design assets on it, and build-feature must
	// check that branch out rather than re-create it. Inventing a branch here
	// would fight that rule.
	start := "origin/" + m.defaultBranch
	if err := m.git(m.checkoutBaseDir, "worktree", "add", "--detach", path, start); err != nil {
		return "", err
	}
	return path, nil
}

// EnsurePR creates the worktree for a pull request and checks out its head
// branch.
func (m *Manager) EnsurePR(number int, headRef string) (string, error) {
	return m.EnsurePRCtx(context.Background(), number, headRef)
}

// EnsurePRCtx is EnsurePR with a deadline on every git command.
//
// It runs "fetch origin <headRef>", the command on the automatic rebase path
// most likely to block on a network stall, and it runs FIRST -- while the
// caller holds the loop lock. Bounding the rebase and the push while leaving
// the fetch unbounded would leave the stall this whole path guards against.
func (m *Manager) EnsurePRCtx(ctx context.Context, number int, headRef string) (string, error) {
	if !SafeRef(headRef) {
		return "", fmt.Errorf("unsafe branch name %q", headRef)
	}
	path := m.PathForPR(number)

	if exists(path) {
		// Refresh an existing tend worktree. Without the fetch the rebase agent
		// would operate on a stale head and could force-push a regression.
		if err := m.gitC(ctx, path, "fetch", "origin", headRef); err != nil {
			return "", err
		}
		if err := m.gitC(ctx, path, "checkout", "--detach", "FETCH_HEAD"); err != nil {
			return "", err
		}
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create worktree parent: %w", err)
	}
	// Detached, so this never collides with the same branch checked out in the
	// issue worktree. Git refuses to check one branch out in two worktrees, and
	// that collision would hit exactly the pull requests most in need of a rebase.
	err := m.gitC(ctx, m.checkoutBaseDir, "worktree", "add", "--detach", path, "origin/"+headRef)
	if err != nil {
		return "", err
	}
	return path, nil
}

// SafeRef reports whether a ref name is safe to pass to git as an argument.
// It rejects a leading dash, which git would read as an option.
func SafeRef(ref string) bool { return safeRef.MatchString(ref) }

var safeRef = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)

// BehindLocal counts the commits origin/baseRef has that origin/headRef does
// not, using only the local checkout.
//
// It is the gate of the periodic tend pass. The equivalent GitHub call costs
// one request per pull request, per loop, per project, on every interval;
// this costs a rev-list against refs the loop's fetch already updated. It
// answers only "is this branch behind", never "should this branch be
// rebased" -- the caller stays the one place that decides that, so a stale
// local ref can never cause a dispatch on its own.
//
// known is false when origin/headRef or origin/baseRef does not resolve.
// That is not an error: Manager.Fetch prunes, so a pull request whose branch
// was deleted loses its remote ref, and the caller must skip the row rather
// than fail the pass.
func (m *Manager) BehindLocal(headRef, baseRef string) (behind int, known bool, err error) {
	if !SafeRef(headRef) {
		return 0, false, fmt.Errorf("unsafe branch name %q", headRef)
	}
	if !SafeRef(baseRef) {
		return 0, false, fmt.Errorf("unsafe branch name %q", baseRef)
	}
	head, base := "origin/"+headRef, "origin/"+baseRef
	if _, err := m.gitOutput(m.checkoutBaseDir, "rev-parse", "--verify", "--quiet", head+"^{commit}"); err != nil {
		return 0, false, nil
	}
	if _, err := m.gitOutput(m.checkoutBaseDir, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		return 0, false, nil
	}
	out, err := m.gitOutput(m.checkoutBaseDir, "rev-list", "--count", head+".."+base)
	if err != nil {
		return 0, false, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, false, fmt.Errorf("parse rev-list count %q: %w", strings.TrimSpace(out), err)
	}
	return n, true, nil
}

// Remove deletes a worktree and its directory.
func (m *Manager) Remove(path string) error {
	return m.RemoveCtx(context.Background(), path)
}

// RemoveCtx is Remove with a deadline on every git command.
//
// It is the last resort of the automatic rebase: when the abort of a failed
// replay itself fails, the worktree is destroyed rather than handed to an
// agent, because a directory stuck mid-rebase fails every later pass for that
// pull request.
func (m *Manager) RemoveCtx(ctx context.Context, path string) error {
	if !exists(path) {
		return nil
	}
	if err := m.gitC(ctx, m.checkoutBaseDir, "worktree", "remove", "--force", path); err != nil {
		// Fall back to a plain delete plus a prune, so a corrupt registration
		// cannot strand the directory forever -- but only for something that
		// really is a worktree. "worktree remove" also fails with "not a
		// working tree", and recursively deleting whatever happens to sit at
		// that path on the strength of that error is a much bigger action than
		// the one being retried.
		if !exists(filepath.Join(path, ".git")) {
			return fmt.Errorf("remove worktree %s: %w", path, err)
		}
		if rmErr := os.RemoveAll(path); rmErr != nil {
			return fmt.Errorf("remove worktree %s: %w", path, err)
		}
		return m.gitC(ctx, m.checkoutBaseDir, "worktree", "prune")
	}
	return nil
}

// Dirty reports whether path has uncommitted changes.
//
// A path that does not exist is not dirty and not an error: Remove is
// idempotent on an absent path, and a caller deciding whether removal will
// destroy something must get the same answer for a worktree that is simply
// already gone.
func (m *Manager) Dirty(path string) (bool, error) {
	return m.DirtyCtx(context.Background(), path)
}

// DirtyCtx is Dirty with a deadline on every git command. The automatic
// rebase asks it before removing a worktree, on the same lock-holding path as
// the fetch and the push.
func (m *Manager) DirtyCtx(ctx context.Context, path string) (bool, error) {
	if !exists(path) {
		return false, nil
	}
	out, err := m.gitCtx(ctx, path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(out) != "" {
		return true, nil
	}
	// A clean tree can still hold work nobody else has: commits made here and
	// never pushed. That is the MOST valuable thing a removal can destroy, and
	// "git status" is silent about it, so a caller warning only on a dirty
	// tree would delete it without a word.
	//
	// No upstream is not an error and not dirty: a detached tend worktree has
	// none by construction (see EnsurePR), and neither does a branch that was
	// never pushed -- for which the ahead count below is meaningless anyway.
	ahead, err := m.gitCtx(ctx, path, "rev-list", "--count", "@{upstream}..HEAD")
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(ahead) != "0" && strings.TrimSpace(ahead) != "", nil
}

// HeadSHA returns the object id HEAD resolves to in path. It is the value the
// automatic rebase later hands to PushWithLease as the lease, so it is read
// before the replay rewrites HEAD.
//
// It reads stdout ONLY, unlike every other helper here. git prints advice,
// hook output and fsmonitor warnings on stderr even when it succeeds, and
// CombinedOutput would prepend all of it to the object id. A polluted lease
// fails --force-with-lease forever, and silently: a refused push is how the
// caller decides to leave a branch alone, so nothing would ever report it.
func (m *Manager) HeadSHA(ctx context.Context, path string) (string, error) {
	out, err := m.gitStdout(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	// Refuse anything that is not a bare object id rather than hand a caller a
	// lease that cannot match. This is the same check PushWithLease makes; it
	// is made twice because the two are separated by a rebase, and the useful
	// error names the command that actually produced the bad value.
	if !leaseSHA.MatchString(sha) {
		return "", fmt.Errorf("rev-parse HEAD in %s returned %q, which is not an object id", path, redact(sha))
	}
	return sha, nil
}

// Rebase replays the commits in path onto origin/baseRef.
//
// It rebases onto the REMOTE ref, not the local branch: a tend worktree is
// detached and its local branches are whatever the primary checkout last left
// behind, while origin/<base> is what Manager.Fetch just updated.
//
// A non-nil error means the replay stopped, which usually but not always
// means a conflict. The caller must abort before doing anything else with the
// worktree.
func (m *Manager) Rebase(ctx context.Context, path, baseRef string) error {
	if !SafeRef(baseRef) {
		return fmt.Errorf("unsafe branch name %q", baseRef)
	}
	return m.gitC(ctx, path, "rebase", "origin/"+baseRef)
}

// AbortRebase returns path to the state it had before a rebase started.
//
// It reports success when no rebase is in progress. The caller aborts
// unconditionally after any Rebase failure -- it cannot tell a conflict from a
// command that never started -- and "git rebase --abort" exits non-zero with
// nothing to abort. Treating that as an error would send every non-conflict
// failure down the destroy-the-worktree path.
func (m *Manager) AbortRebase(ctx context.Context, path string) error {
	if !m.rebaseInProgress(ctx, path) {
		return nil
	}
	return m.gitC(ctx, path, "rebase", "--abort")
}

// rebaseInProgress reports whether git has a rebase state directory for path.
// Both names are checked because git uses rebase-merge for the default
// backend and rebase-apply for the older one.
func (m *Manager) rebaseInProgress(ctx context.Context, path string) bool {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		out, err := m.gitStdout(ctx, path, "rev-parse", "--git-path", name)
		if err != nil {
			continue
		}
		p := strings.TrimSpace(out)
		if p == "" {
			continue
		}
		// "--git-path" answers relative to the working directory it ran in.
		if !filepath.IsAbs(p) {
			p = filepath.Join(path, p)
		}
		if exists(p) {
			return true
		}
	}
	return false
}

// PushWithLease force-pushes HEAD in path to origin's headRef, but only while
// that branch still points at lease.
//
// The lease is the entire safety of the automatic rebase: it is the head this
// pass fetched, so git refuses the push if anybody wrote to the branch in
// between and the other writer's commit survives. HEAD is pushed by object id
// because a tend worktree is detached and has no branch to name.
func (m *Manager) PushWithLease(ctx context.Context, path, headRef, lease string) error {
	if !SafeRef(headRef) {
		return fmt.Errorf("unsafe branch name %q", headRef)
	}
	if !leaseSHA.MatchString(lease) {
		return fmt.Errorf("refusing to push %s with lease %q, which is not a full object id", headRef, redact(lease))
	}
	return m.gitC(ctx, path,
		"push", "--force-with-lease="+headRef+":"+lease,
		"origin", "HEAD:refs/heads/"+headRef)
}

// leaseSHA matches a full git object id, in either hash size.
//
// An abbreviated or otherwise malformed id is refused, and an EMPTY one above
// all: --force-with-lease with an empty value silently degrades to
// remote-tracking semantics, and a detached tend worktree has no useful
// remote-tracking ref -- EnsurePRCtx fetches into FETCH_HEAD, never into
// refs/remotes/origin/<head>. That degraded form is close to an unconditional
// force push, and from the outside it looks exactly like the strong one, so
// the check lives here rather than being trusted from the caller.
var leaseSHA = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

// gitTimeout bounds one git command on the automatic rebase path.
const gitTimeout = 2 * time.Minute

// gitC runs one git command with a deadline and discards its output.
func (m *Manager) gitC(ctx context.Context, dir string, args ...string) error {
	_, err := m.gitCtx(ctx, dir, args...)
	return err
}

// gitCtx runs one git command with a deadline.
//
// The other git helpers here have none, which is correct for a command-line
// tick: a human watching sees it hang and stops it. The daemon has no such
// reader. A hung push holds the loop lock and stalls the tend ticker for
// every project on the machine, so every command on the automatic rebase path
// is bounded.
func (m *Manager) gitCtx(ctx context.Context, dir string, args ...string) (string, error) {
	return m.runCtx(ctx, dir, false, args...)
}

// gitStdout runs one git command with a deadline and returns stdout alone.
// Use it whenever the output is parsed rather than logged; see HeadSHA.
func (m *Manager) gitStdout(ctx context.Context, dir string, args ...string) (string, error) {
	return m.runCtx(ctx, dir, true, args...)
}

func (m *Manager) runCtx(ctx context.Context, dir string, stdoutOnly bool, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var (
		out []byte
		err error
	)
	if stdoutOnly {
		// Output leaves stderr out of the returned value but still captures it,
		// so a failure reports what git said rather than an exit code alone.
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err = cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git %s in %s: %w: %s",
				strings.Join(args, " "), dir, err, redact(strings.TrimSpace(stderr.String())))
		}
		return string(out), nil
	}
	out, err = cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %w: %s",
			strings.Join(args, " "), dir, err, redact(strings.TrimSpace(string(out))))
	}
	return string(out), nil
}

func (m *Manager) git(dir string, args ...string) error {
	_, err := m.gitOutput(dir, args...)
	return err
}

func (m *Manager) gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %w: %s",
			strings.Join(args, " "), dir, err, redact(strings.TrimSpace(string(out))))
	}
	return string(out), nil
}

// credentialInURL matches the userinfo part of a remote URL, and the two GitHub
// token shapes.
var (
	credentialInURL = regexp.MustCompile(`://[^/@\s]*@`)
	tokenShape      = regexp.MustCompile(`\b(ghp_|github_pat_|gho_|ghs_)[A-Za-z0-9_]+`)
)

// redact removes credentials from git output. That output is stored in the
// dispatch row and written to the cron log, and an https remote can carry a
// token in its userinfo.
func redact(s string) string {
	s = credentialInURL.ReplaceAllString(s, "://REDACTED@")
	return tokenShape.ReplaceAllString(s, "REDACTED")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
