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

// describeLink renders a link for a log line.
func describeLink(issue int, pr ghub.PullRequest) string {
	return fmt.Sprintf("issue #%d -> PR #%d (%s...%s)", issue, pr.Number, pr.BaseRef, pr.HeadRef)
}
