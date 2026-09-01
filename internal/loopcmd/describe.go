package loopcmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/runner"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// SessionDetail is one session and every run that used it.
//
// It is the answer to "what happened to this session", which `sessions list`
// cannot give: that table carries one row per session, so a session whose
// first run succeeded and whose next three failed reports only the last state.
// The runs are what say the loop is stuck rather than merely unlucky.
type SessionDetail struct {
	Session Session
	// PR is the pull request the session's runs worked on, from the most
	// recent run that named one. Only tend dispatches carry it.
	PR int
	// Runs is every dispatch that used the session, in any order. The
	// renderer sorts them.
	Runs []SessionRun
}

// SessionRun is one dispatch and the agent settings it actually ran under.
//
// The settings are per RUN, not per session, because they can differ within
// one: an issue's harness: label applies to the dispatches that work the
// issue, and a dispatch carrying no override runs the loop's default. Session
// carries only the NEWEST run's settings (sessions.go:352), which is why this
// report resolves its own -- a session whose work ran under pi and whose last
// run was a tend would otherwise be reported as a claude session.
type SessionRun struct {
	Dispatch store.Dispatch
	// Model and Harness are resolved through runner.Effective: the row's
	// override where it carried one, the loop's configured value otherwise.
	// Both are empty when the loop's configuration can no longer be read.
	Model   string
	Harness string
}

// DescribeSession collects one session and its runs.
//
// It reads only rows the store already holds: the session summary that
// `sessions list` renders, and the dispatches keyed by that session. Nothing
// here opens a log file. The reason a run failed was recorded when it
// finished, which is what makes this report cheap enough to be the first
// thing an operator runs.
func DescribeSession(p *Project, sessionID string) (SessionDetail, error) {
	sess, path, err := FindSession(p, sessionID)
	if err != nil {
		return SessionDetail{}, err
	}
	// Loaded once for every run's settings. A configuration that can no longer
	// be read leaves them empty rather than failing the report: the run history
	// is the point, and the settings are context for it.
	cfg, _ := config.Load(path)
	// The canonical database directly, as Sessions does: this reads rows and
	// needs neither the loop's GitHub client nor a second config load.
	db, err := openCanonical()
	if err != nil {
		return SessionDetail{}, err
	}
	defer db.Close()

	runs, err := db.Project(p.Config.ID).DispatchesBySession(sess.ID)
	if err != nil {
		return SessionDetail{}, err
	}
	// FindSession fills the project name only for the machine-wide report,
	// and this report's header names the project either way.
	sess.Project = p.Config.Name
	out := SessionDetail{Session: sess}
	for _, d := range runs {
		r := SessionRun{Dispatch: d}
		if cfg != nil {
			s := runner.Effective(cfg, config.Overrides{
				Model: d.Model, Harness: d.Harness,
			})
			r.Model, r.Harness = s.Model, s.Harness
		}
		out.Runs = append(out.Runs, r)
		// DispatchesBySession returns newest first, so the first row carrying
		// a pull request is the most recent one to name it.
		if out.PR == 0 && d.PRNumber > 0 {
			out.PR = d.PRNumber
		}
	}
	return out, nil
}

// DescribeSessionAnywhere is DescribeSession without being told the project.
//
// The machine-wide `sessions list` prints ids from every project at once, so
// the operator who copies one from it has no project to name. The session
// identifier already determines it: this finds the owning project and hands
// off. projectSelector narrows the search when the operator does know, taking
// whatever --project accepts elsewhere.
func DescribeSessionAnywhere(projectSelector, sessionID string) (SessionDetail, error) {
	sessions, err := AllSessions(SessionFilter{Project: projectSelector})
	if err != nil {
		return SessionDetail{}, err
	}
	for _, s := range sessions {
		if s.ID != sessionID {
			continue
		}
		p, err := ResolveProject(s.ProjectID)
		if err != nil {
			return SessionDetail{}, fmt.Errorf(
				"session %s belongs to project %s, which cannot be opened: %w",
				sessionID, s.ProjectID, err)
		}
		return DescribeSession(p, sessionID)
	}
	return SessionDetail{}, fmt.Errorf("no session %q found on this machine", sessionID)
}

// RenderSessionDetail formats one session's history for a terminal.
func RenderSessionDetail(sd SessionDetail) string {
	var b strings.Builder

	fmt.Fprintf(&b, "session %s\n", sd.Session.ID)
	if sd.Session.Project != "" {
		fmt.Fprintf(&b, "  project  %-18s loop   %s\n", sd.Session.Project, sd.Session.Loop)
	} else {
		fmt.Fprintf(&b, "  loop     %s\n", sd.Session.Loop)
	}
	fmt.Fprintf(&b, "  issue    #%d %s\n", sd.Session.Issue, sd.Session.Title)

	// Oldest first: this is a history, and a failure means more when the run
	// it followed is above it rather than below.
	runs := make([]SessionRun, len(sd.Runs))
	copy(runs, sd.Runs)
	sort.Slice(runs, func(i, j int) bool { return runs[i].Dispatch.ID < runs[j].Dispatch.ID })

	// The CREATING run's settings, not the session summary's: that summary
	// carries the newest run's, and the newest run is often a tend under the
	// loop's defaults. A session is owned by the harness that minted it.
	harness, model := sd.Session.Harness, sd.Session.Model
	if len(runs) > 0 && runs[0].Harness != "" {
		harness, model = runs[0].Harness, runs[0].Model
	}
	fmt.Fprintf(&b, "  harness  %s (%s)\n", orDash(harness), orDash(model))
	if sd.PR > 0 {
		fmt.Fprintf(&b, "  pr       #%d\n", sd.PR)
	}

	if len(runs) == 0 {
		// A session row can exist before its first run finishes. Saying so is
		// the difference between "nothing has happened yet" and a report the
		// operator reads as broken.
		fmt.Fprintf(&b, "\nThis session has no runs recorded yet.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "\n  %-5s %-7s %-10s %-9s %-9s %s\n",
		"RUN", "KIND", "STATE", "COST", "DURATION", "WHEN")
	for _, r := range runs {
		d := r.Dispatch
		fmt.Fprintf(&b, "  %-5d %-7s %-10s $%-8.2f %-9s %s\n",
			d.ID, truncate(d.Kind, 7), d.Status, d.CostUSD,
			shortDuration(d.DurationMS),
			d.StartedAt.Local().Format("01-02 15:04"))
		if reason := failureReason(d); reason != "" {
			fmt.Fprintf(&b, "        └ %s\n", reason)
		}
	}

	if summary := failureSummary(runs); summary != "" {
		fmt.Fprintf(&b, "\n  %s\n", summary)
	}
	if note := harnessChangeNote(runs); note != "" {
		fmt.Fprintf(&b, "  %s\n", note)
	}
	fmt.Fprintf(&b, "\nRead one run in full with: agent-utils project logs --dispatch <RUN>\n")
	return b.String()
}

// failureReason is what to print under a failed run.
//
// A run that failed always says something. api_error carries what the harness
// reported when the runner could record it; when it could not, the exit code
// is all there is, and printing nothing would leave the failure unexplained
// in the one report meant to explain it.
func failureReason(d store.Dispatch) string {
	if d.Status != store.StatusFailed {
		return ""
	}
	if s := strings.TrimSpace(d.APIError); s != "" {
		return s
	}
	return fmt.Sprintf("exit %d, with no reason recorded", d.ExitCode)
}

// failureSummary is the line that turns a table into a diagnosis.
//
// A session failing the same way on every run is a loop that cannot make
// progress -- it will fail that way again on the next tick, and no amount of
// waiting changes it. That is a different problem from runs that failed for
// unrelated reasons, so the two are never reported alike.
func failureSummary(runs []SessionRun) string {
	var failed []store.Dispatch
	for _, r := range runs {
		if r.Dispatch.Status == store.StatusFailed {
			failed = append(failed, r.Dispatch)
		}
	}
	if len(failed) == 0 {
		return ""
	}
	out := fmt.Sprintf("%d of %d runs failed", len(failed), len(runs))
	if len(failed) > 1 && sameReason(failed) {
		out += ", every one with the same error: the next tick will fail the same way"
	}
	return out + "."
}

// harnessChangeNote reports a session whose runs did not all use one harness.
//
// It is a diagnosis rather than trivia. A session identifier is only
// meaningful to the harness that minted it: claude exits non-zero on an id it
// has never seen, and pi quietly creates a new session under it. So a run
// under a harness the session did not start with either fails immediately or
// loses the whole conversation, and neither is visible from the run table.
func harnessChangeNote(runs []SessionRun) string {
	first := runs[0].Harness
	if first == "" {
		return ""
	}
	for _, r := range runs[1:] {
		if r.Harness != "" && r.Harness != first {
			return fmt.Sprintf(
				"Run %d ran under %s, but the session was created by %s: "+
					"a session id means nothing to a harness that did not mint it.",
				r.Dispatch.ID, r.Harness, first)
		}
	}
	return ""
}

// sameReason reports whether every run failed for one identical reason. It
// compares the recorded text: two runs that failed with nothing recorded are
// not evidence of a repeating cause, so an empty reason never matches.
func sameReason(failed []store.Dispatch) bool {
	first := strings.TrimSpace(failed[0].APIError)
	if first == "" {
		return false
	}
	for _, d := range failed[1:] {
		if strings.TrimSpace(d.APIError) != first {
			return false
		}
	}
	return true
}

// shortDuration renders a run's wall clock compactly. A dispatch that died
// before doing anything reports 0, and "0s" is a fact worth seeing: it is what
// a failure that never reached the model looks like.
func shortDuration(ms int64) string {
	d := (time.Duration(ms) * time.Millisecond).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}
