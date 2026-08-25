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
	// State is "open" or "closed", as GitHub spells it.
	//
	// Every caller before the epic sweep listed OPEN issues only, so this was
	// not needed and was not carried. The sweep reads sub-issues and blockers,
	// and both lists mix open and closed, so the state has to survive the
	// conversion. Compare it with IsOpen rather than by ==: GitHub's spelling
	// is stable, but nothing here forces the case.
	State string
	// Repo is the "owner/name" this issue lives in, or "" when the response did
	// not say.
	//
	// It exists because sub-issue, parent and dependency relations may CROSS
	// repositories, and the epic sweep writes a label by NUMBER against one
	// loop's owner/repo. A foreign sub-issue carried through without this field
	// would label whichever local issue happens to share its number -- the same
	// class of bug as answering a pull_request delivery as an issue, which
	// handler.go guards against by checking the event.
	//
	// Empty is NOT "local". It is "unknown", and InRepo answers false for it.
	Repo string
}

// IsOpen reports whether the issue is open. The comparison ignores case.
func (i Issue) IsOpen() bool { return strings.EqualFold(i.State, "open") }

// InRepo reports whether the issue lives in owner/repo.
//
// An issue whose Repo is empty answers false. The field is empty only when the
// response did not name a repository, which is "unknown", and the sweep's write
// is by number: guessing "local" there is how a foreign issue gets labelled.
func (i Issue) InRepo(owner, repo string) bool {
	return i.Repo != "" && strings.EqualFold(i.Repo, owner+"/"+repo)
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
			// Compare whole lowered strings rather than slicing by byte index,
			// which could split a multi-byte rune on a non-ASCII label.
			prefix := strings.ToLower(strings.TrimSuffix(n, "*"))
			for _, l := range i.Labels {
				if strings.HasPrefix(strings.ToLower(l), prefix) {
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

// Hook is the subset of a GitHub repository webhook that register-webhook
// needs to decide whether one already exists.
type Hook struct {
	ID  int64
	URL string // Config.URL, the delivery target
	// GitHub returns Config.Secret obfuscated (see CreateHook), so it is
	// never carried into this type: nothing here can compare it, and a field
	// that always reads as obfuscated would invite exactly that mistake.
	Events []string
	Active bool
}

// HookSpec is what register-webhook sends to create or update a hook.
type HookSpec struct {
	URL    string
	Secret string
	Events []string
}

// HookEvents is the event set a loop reacts to. It is declared once, here,
// because two callers must agree: register-webhook subscribes to it, and the
// listener drops any delivery outside it. Two independent lists would drift,
// and the daemon would answer every delivery and do nothing.
var HookEvents = []string{
	"issues",
	"issue_comment",
	"pull_request",
	"pull_request_review",
	"pull_request_review_comment",
}

// IsHookEvent reports whether name is one this daemon acts on.
func IsHookEvent(name string) bool {
	for _, e := range HookEvents {
		if e == name {
			return true
		}
	}
	return false
}

// safeRef matches a git branch name this program is willing to pass to git.
// It rejects a leading dash, which git would read as an option.
var safeRef = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)

// SafeRef reports whether a ref name is safe to pass to git.
func SafeRef(ref string) bool { return safeRef.MatchString(ref) }
