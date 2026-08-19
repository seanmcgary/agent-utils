package loopcmd

import (
	"fmt"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/config"
)

// RenderConfigs formats the loop configurations of one project.
func RenderConfigs(p *Project, entries []config.Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "project %s  (%s)\n%s\n\n", p.Config.Name, p.Root, config.ConfigsDir(p.Dir))

	// NAME is the `name` field inside each file; FILE is where it came from.
	// They are shown separately because they need not agree.
	fmt.Fprintf(&b, "%-20s %-24s %-40s %s\n", "NAME", "FILE", "REPO", "STATUS")
	for _, e := range entries {
		status, repo := "ok", e.Repo
		if e.Err != nil {
			status, repo = "INVALID", "-"
		}
		fmt.Fprintf(&b, "%-20s %-24s %-40s %s\n", e.Name, e.File, repo, status)
	}
	for _, e := range entries {
		if e.Err != nil {
			fmt.Fprintf(&b, "\n%s: %v\n", e.Name, e.Err)
		}
	}
	// A duplicated name is not cosmetic: the name is half the key of every row
	// this loop owns, and it names the lock and the log tree. Two loops sharing
	// one would read and write each other's state while looking separate.
	if dupes := config.Duplicates(entries); len(dupes) > 0 {
		fmt.Fprintf(&b, "\nWARNING: %d name(s) declared by more than one file: %s\n",
			len(dupes), strings.Join(dupes, ", "))
		fmt.Fprintf(&b, "Each loop needs a unique name; they share a state directory and lock otherwise.\n")
	}
	return b.String()
}
