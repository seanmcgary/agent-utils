package engine

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/seanmcgary/agent-utils/internal/ghub"
)

// closingRef matches a GitHub closing keyword and its issue number. The
// trailing boundary stops "#123" from matching issue 12.
var closingRef = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)\b`)

// LinkPR returns the open pull request whose body closes issueNumber.
// It returns false when no pull request closes the issue.
// It ignores an untrusted pull request. Selection is deterministic: when more
// than one trusted pull request closes the issue, the lowest number wins, so the
// result does not depend on the order the API returned.
func LinkPR(issueNumber int, prs []ghub.PullRequest) (ghub.PullRequest, bool) {
	want := strconv.Itoa(issueNumber)
	var best ghub.PullRequest
	found := false
	for _, pr := range prs {
		if !pr.Trusted || pr.Body == "" {
			continue
		}
		for _, m := range closingRef.FindAllStringSubmatch(pr.Body, -1) {
			if m[1] != want {
				continue
			}
			if !found || pr.Number < best.Number {
				best, found = pr, true
			}
			break
		}
	}
	return best, found
}

// ClosesIssue returns the issue number pr closes, and false when its body
// names none. It is the reverse of LinkPR and shares closingRef with it, so
// the two can never disagree about what "closes" means.
//
// A pull_request, pull_request_review or pull_request_review_comment
// delivery names a PR, and issue_comment fires on pull requests too -- but
// every row this program writes (sessions, retries, the in-flight label,
// dispatch rows) is keyed by ISSUE number. Without this, a delivery about a
// PR has no state to act on.
//
// The lowest number wins when a body names several, for the same reason
// LinkPR prefers the lowest PR number: two identical deliveries must resolve
// to the same issue, or one would dispatch an agent the next would not.
//
// Trust is deliberately NOT judged here. This answers only "which issue does
// this delivery concern"; every action taken afterwards re-checks trust on
// its own -- LinkPR drops an untrusted pull request before anything is
// tended, and a dispatch still requires the issue to carry the trigger label
// a repository member applied.
func ClosesIssue(pr ghub.PullRequest) (int, bool) {
	best := 0
	found := false
	for _, m := range closingRef.FindAllStringSubmatch(pr.Body, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			// An unparsable run of digits is one too long for an int. It
			// names no issue GitHub could have, so it is skipped rather than
			// reported as an error the caller would have to invent a policy
			// for.
			continue
		}
		if !found || n < best {
			best, found = n, true
		}
	}
	return best, found
}

// describeLink renders a link for a log line.
func describeLink(issue int, pr ghub.PullRequest) string {
	return fmt.Sprintf("issue #%d -> PR #%d (%s...%s)", issue, pr.Number, pr.BaseRef, pr.HeadRef)
}
