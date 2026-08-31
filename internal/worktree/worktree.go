// Package worktree manages the git worktrees that dispatched agents run in.
package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
	if !SafeRef(headRef) {
		return "", fmt.Errorf("unsafe branch name %q", headRef)
	}
	path := m.PathForPR(number)

	if exists(path) {
		// Refresh an existing tend worktree. Without the fetch the rebase agent
		// would operate on a stale head and could force-push a regression.
		if err := m.git(path, "fetch", "origin", headRef); err != nil {
			return "", err
		}
		if err := m.git(path, "checkout", "--detach", "FETCH_HEAD"); err != nil {
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
	err := m.git(m.checkoutBaseDir, "worktree", "add", "--detach", path, "origin/"+headRef)
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
	if !exists(path) {
		return nil
	}
	if err := m.git(m.checkoutBaseDir, "worktree", "remove", "--force", path); err != nil {
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
		return m.git(m.checkoutBaseDir, "worktree", "prune")
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
	if !exists(path) {
		return false, nil
	}
	out, err := m.gitOutput(path, "status", "--porcelain")
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
	ahead, err := m.gitOutput(path, "rev-list", "--count", "@{upstream}..HEAD")
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(ahead) != "0" && strings.TrimSpace(ahead) != "", nil
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
