package wizard

import (
	"os/exec"
	"regexp"
	"strings"
)

// Detected holds what git can tell us about the working directory.
type Detected struct {
	Repo            string // owner/name, from the origin remote
	DefaultBranch   string // from origin/HEAD
	CheckoutBaseDir string // the work tree root
}

// Detect shells out to git the way internal/worktree already does
// (worktree.go:127, exec.Command("git", ...)) to prefill the wizard's
// defaults from the directory the operator is already standing in.
//
// It never fails. A loop may legitimately point at a repository the operator
// has not cloned here, so a directory that is not a git work tree — or one
// with no origin remote, or no origin/HEAD — is not an error: Detect returns
// whatever it found and leaves the rest empty, and the corresponding question
// simply has no default.
func Detect(dir string) Detected {
	var d Detected

	if out, err := gitOutput(dir, "rev-parse", "--show-toplevel"); err == nil {
		d.CheckoutBaseDir = strings.TrimSpace(out)
	}
	if out, err := gitOutput(dir, "remote", "get-url", "origin"); err == nil {
		d.Repo = ownerName(strings.TrimSpace(out))
	}
	if out, err := gitOutput(dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		d.DefaultBranch = strings.TrimPrefix(strings.TrimSpace(out), "origin/")
	}

	return d
}

func gitOutput(dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	out, err := exec.Command("git", full...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// sshRemote and httpsRemote match the two remote URL shapes GitHub issues:
// git@github.com:owner/name.git and https://github.com/owner/name.git. Either
// form, and its .git suffix, is optional in the wild, so both are captured
// loosely rather than pinned to github.com specifically.
var (
	sshRemote   = regexp.MustCompile(`^[\w.-]+@[^:]+:(.+?)(\.git)?$`)
	httpsRemote = regexp.MustCompile(`^https?://[^/]+/(.+?)(\.git)?$`)
)

// ownerName extracts "owner/name" from a remote URL. An unrecognized shape
// returns "", which leaves the repo question with no default rather than a
// wrong one.
func ownerName(url string) string {
	if m := sshRemote.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	if m := httpsRemote.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	return ""
}
