package ghub

import (
	"regexp"
	"strings"
	"time"
)

// Issue is the subset of a GitHub issue that the engine needs.
type Issue struct {
	Number    int
	Title     string
	Labels    []string
	UpdatedAt time.Time
}

// HasLabel reports whether the issue carries name. The comparison ignores case.
func (i Issue) HasLabel(name string) bool {
	for _, l := range i.Labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

// HasAnyLabel reports whether the issue carries any of names. An entry that
// ends with "*" is a prefix rule, so "blocked:*" matches "blocked:design" and
// "blocked:legal". The reference loops state the rule that way.
func (i Issue) HasAnyLabel(names []string) bool {
	for _, n := range names {
		if strings.HasSuffix(n, "*") {
			prefix := strings.TrimSuffix(n, "*")
			for _, l := range i.Labels {
				if len(l) >= len(prefix) && strings.EqualFold(l[:len(prefix)], prefix) {
					return true
				}
			}
			continue
		}
		if i.HasLabel(n) {
			return true
		}
	}
	return false
}

// PullRequest is the subset of a GitHub pull request that the engine needs.
type PullRequest struct {
	Number  int
	HeadRef string
	BaseRef string
	Body    string
	Draft   bool
	// HeadRepo is the full name of the repository the head branch lives in. A
	// pull request from a fork has a different value here.
	HeadRepo string
	// AuthorAssociation is the author's relationship to the repository.
	AuthorAssociation string
	// Trusted is set at the API boundary. Only a trusted pull request may be
	// linked to an issue and tended, because tending checks the head branch out
	// and runs an agent inside it.
	Trusted bool
}

// safeRef matches a git branch name this program is willing to pass to git.
// It rejects a leading dash, which git would read as an option.
var safeRef = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)

// SafeRef reports whether a ref name is safe to pass to git.
func SafeRef(ref string) bool { return safeRef.MatchString(ref) }
