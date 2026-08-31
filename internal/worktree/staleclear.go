package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// staleLockNames are the lock files a crashed git process leaves in a
// worktree's own git directory.
//
// The list is deliberately short. These two are the ones that block the next
// agent outright: index.lock fails every command that touches the index, and
// HEAD.lock fails the ref update a checkout or a rebase ends with. Locks in
// the SHARED repository directory (config.lock, a branch's refs/heads lock)
// are excluded on purpose -- other worktrees run against that directory, and a
// live agent may legitimately hold one.
var staleLockNames = []string{"index.lock", "HEAD.lock"}

// ClearStaleLocks removes the git lock files left in a worktree by a process
// that is no longer running. It returns the paths it removed.
//
// It is called while reaping a dispatch whose runner has been PROVEN dead, by
// a caller holding that loop's lock. That is what makes "stale" a fact rather
// than a guess: no age heuristic is needed, because no process can be holding
// these files. Calling it in any other situation would be wrong -- a lock held
// by a live git process is how git makes concurrent operations safe.
//
// It removes ONLY lock files. An in-progress rebase, a merge, uncommitted
// edits, and every commit are left exactly as the crashed agent left them:
// they are that agent's work, and the retry is meant to find and resolve them.
//
// Nothing here is an error the caller must act on. A missing worktree, a path
// that is no longer a repository, and a lock file already gone are all
// ordinary: the reap still has a row to retire, and abandoning it over the
// debris would strand the issue.
func ClearStaleLocks(worktreePath string) ([]string, error) {
	gitDir, err := resolveGitDir(worktreePath)
	if err != nil {
		return nil, err
	}
	if gitDir == "" {
		return nil, nil
	}

	var cleared []string
	for _, name := range staleLockNames {
		path := filepath.Join(gitDir, name)
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return cleared, fmt.Errorf("remove %s: %w", path, err)
		}
		cleared = append(cleared, path)
	}
	return cleared, nil
}

// resolveGitDir returns the real git directory for a worktree path, or an
// empty string when the path is not a repository.
//
// A LINKED worktree's .git is a file holding "gitdir: <path>", pointing into
// the main repository's .git/worktrees/<name>. The locks live there, not in
// the worktree, so a naive filepath.Join(path, ".git") finds nothing and
// silently clears nothing -- which is the failure this function exists to
// avoid. A primary checkout's .git is an ordinary directory.
//
// git rev-parse would answer both cases, but this reads the file directly: it
// runs during a reap, on a path whose agent just died, and shelling out adds a
// process and a failure mode to a cleanup that must not be able to fail.
func resolveGitDir(worktreePath string) (string, error) {
	dotGit := filepath.Join(worktreePath, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("stat %s: %w", dotGit, err)
	}
	if info.IsDir() {
		return dotGit, nil
	}

	raw, err := os.ReadFile(dotGit)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", dotGit, err)
	}
	// The file's one line is "gitdir: <path>". Anything else is not a worktree
	// pointer, and guessing at it would delete a file in a directory this
	// function was never told about.
	line := strings.TrimSpace(string(raw))
	rest, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return "", nil
	}
	dir := strings.TrimSpace(rest)
	if dir == "" {
		return "", nil
	}
	// Relative pointers are legal and are resolved against the worktree.
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(worktreePath, dir)
	}
	return dir, nil
}
