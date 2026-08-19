package migrate

import (
	"fmt"
	"sort"
	"strings"
)

// What happened to one source.
const (
	// StateImported means every row was copied in, on a first pass.
	StateImported = "imported"
	// StateRefreshed means an open source was read again and its outcomes
	// carried over.
	StateRefreshed = "refreshed"
	// StateSealed means the source is final and will never be read again.
	StateSealed = "sealed"
	// StateSkipped means nothing was done, for a stated reason.
	StateSkipped = "skipped"
	// StateFailed means the source could not be imported. On the write path that
	// is fatal, because a tick against missing state re-dispatches everything.
	StateFailed = "failed"
)

// MarkerFile is left beside a sealed source, for a human reading the directory.
const MarkerFile = "MIGRATED.txt"

// Source is one legacy per-loop database and the loop inside it.
//
// One file can hold two loops, because two loops may share a state_dir. Each
// loop is a separate source, claimed by the project that configures it.
type Source struct {
	// Path is the legacy state.db, absolute and with symlinks resolved.
	Path string
	// ProjectID is the UUID of the project that claims this loop.
	ProjectID string
	// ProjectName is for the report only.
	ProjectName string
	// Loop is the loop whose rows this source holds.
	Loop string
	// Repo is the repository that loop watches, for the report.
	Repo string
}

// Key identifies a source without regard to who is reporting it.
func (s Source) Key() string { return s.Path + "\x00" + s.Loop }

// Result is what happened to one source.
type Result struct {
	Source Source
	State  string
	// Rows is how many rows were written into the canonical database.
	Rows int
	// Reason says why a source was skipped or failed, in one sentence.
	Reason string
	Err    error
}

// Report is the outcome of one migration run.
type Report struct {
	Results []Result
}

// Failed returns every source that could not be imported.
func (r Report) Failed() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.State == StateFailed {
			out = append(out, res)
		}
	}
	return out
}

// Rows is how many rows the run wrote in total.
func (r Report) Rows() int {
	var n int
	for _, res := range r.Results {
		n += res.Rows
	}
	return n
}

// Err returns one error naming every failure, or nil when there is none.
//
// The write path turns this into a hard failure: a tick that ran against a
// database missing this loop's rows would re-dispatch every open issue and start
// a second agent in a worktree that already holds one.
func (r Report) Err() error {
	failed := r.Failed()
	if len(failed) == 0 {
		return nil
	}
	var lines []string
	for _, res := range failed {
		lines = append(lines, fmt.Sprintf("  %s (loop %s): %s",
			res.Source.Path, res.Source.Loop, res.Reason))
	}
	sort.Strings(lines)
	return fmt.Errorf("state from an earlier layout could not be imported:\n%s\n\n"+
		"Run `agent-utils migrate` to see the whole picture. Nothing was deleted.",
		strings.Join(lines, "\n"))
}
