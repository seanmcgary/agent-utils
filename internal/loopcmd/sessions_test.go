package loopcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// Loop and number alone merge two projects' issue 7 -- the same reason
// sessionKey carries the project (sessions.go:231). The stopped set the
// renderers build from DB.StoppedIssues() has to carry the project too, or
// killing one project's issue 7 would mark the other project's issue 7
// STOPPED as well.
func TestApplyStoppedKeysByProjectLoopAndNumber(t *testing.T) {
	now := time.Now().UTC()
	ds := []store.Dispatch{
		{
			ID: 1, ProjectID: projectA, Loop: "planning", Number: 7,
			SessionID: "a", Status: store.StatusSucceeded, StartedAt: now,
		},
		{
			ID: 2, ProjectID: projectB, Loop: "planning", Number: 7,
			SessionID: "b", Status: store.StatusSucceeded, StartedAt: now,
		},
	}
	sessions := sessionsFrom(ds, "")

	stopped := stoppedSet([]store.StoppedIssue{
		{ProjectID: projectA, Loop: "planning", Number: 7, Reason: "killed by operator"},
	}, "")
	applyStopped(sessions, stopped)

	var a, b Session
	for _, s := range sessions {
		switch s.ProjectID {
		case projectA:
			a = s
		case projectB:
			b = s
		}
	}
	if !a.Stopped {
		t.Errorf("project A's issue 7 must read Stopped: %+v", a)
	}
	if b.Stopped {
		t.Errorf("project B's issue 7 must NOT read Stopped: loop and number alone would merge the two projects: %+v", b)
	}
}

// C4: an issue can accumulate several sessions over its history (a resume
// after a park starts a new one). Stopped is a fact about the issue, but
// only the MOST RECENT session should read STOPPED -- older, already
// finished sessions must not.
func TestApplyStoppedMarksOnlyTheMostRecentSession(t *testing.T) {
	older := time.Now().UTC().Add(-time.Hour)
	newer := time.Now().UTC()
	ds := []store.Dispatch{
		{
			ID: 1, ProjectID: projectA, Loop: "planning", Number: 7,
			SessionID: "old-session", Status: store.StatusSucceeded, StartedAt: older,
		},
		{
			ID: 2, ProjectID: projectA, Loop: "planning", Number: 7,
			SessionID: "new-session", Status: store.StatusFailed, StartedAt: newer,
		},
	}
	sessions := sessionsFrom(ds, "")

	stopped := stoppedSet([]store.StoppedIssue{
		{ProjectID: projectA, Loop: "planning", Number: 7, Reason: "killed by operator"},
	}, "")
	applyStopped(sessions, stopped)

	var old, new_ Session
	for _, s := range sessions {
		switch s.ID {
		case "old-session":
			old = s
		case "new-session":
			new_ = s
		}
	}
	if old.Stopped {
		t.Errorf("the older, already-finished session must NOT read Stopped: %+v", old)
	}
	if !new_.Stopped {
		t.Errorf("the most recent session must read Stopped: %+v", new_)
	}
}

// stoppedSet narrows to one project when given one, and spans every project
// when given none -- the shape Sessions and AllSessions each need.
func TestStoppedSetNarrowsToOneProject(t *testing.T) {
	all := []store.StoppedIssue{
		{ProjectID: projectA, Loop: "planning", Number: 1, Reason: "a"},
		{ProjectID: projectB, Loop: "planning", Number: 2, Reason: "b"},
	}
	scoped := stoppedSet(all, projectA)
	if len(scoped) != 1 {
		t.Fatalf("stoppedSet(%q) = %d entries, want 1", projectA, len(scoped))
	}
	if !scoped[stoppedKey{ProjectID: projectA, Loop: "planning", Number: 1}] {
		t.Errorf("stoppedSet(%q) missing project A's issue", projectA)
	}

	unscoped := stoppedSet(all, "")
	if len(unscoped) != 2 {
		t.Fatalf("stoppedSet(\"\") = %d entries, want 2", len(unscoped))
	}
}

// RenderSessions and RenderAllSessions render STOPPED above ORPHANED but
// below running: a stopped session's runner is gone BY DESIGN, so calling it
// an orphan sends the operator hunting a crash that never happened -- but a
// live agent still outranks the flag.
func TestRenderSessionsShowsStoppedAboveOrphanedBelowRunning(t *testing.T) {
	sessions := []Session{
		{ID: "sess-stopped", Loop: "planning", Issue: 7, Stopped: true, Last: time.Now()},
		{ID: "sess-live-and-stopped", Loop: "planning", Issue: 8, Live: true, Stopped: true, Last: time.Now()},
	}
	out := RenderSessions(&Project{Config: &projectConfigStub, Root: "/p", Dir: "/p/.agent-utils"}, sessions, false)

	if !strings.Contains(out, "STOPPED") {
		t.Fatalf("output must show STOPPED for the stopped-only session:\n%s", out)
	}
	if !strings.Contains(out, "running") {
		t.Errorf("a session that is both Live and Stopped must render running:\n%s", out)
	}

	lines := strings.Split(out, "\n")
	var liveLine string
	for _, l := range lines {
		if strings.Contains(l, "sess-live-and-stopped") {
			liveLine = l
		}
	}
	if strings.Contains(liveLine, "STOPPED") {
		t.Errorf("a live session must not also render STOPPED: %q", liveLine)
	}
}

func TestRenderAllSessionsShowsStopped(t *testing.T) {
	out := RenderAllSessions([]Session{
		{ID: "sess", Project: "demo", Loop: "planning", Issue: 7, Stopped: true, Last: time.Now()},
	}, SessionFilter{})
	if !strings.Contains(out, "STOPPED") {
		t.Fatalf("output must show STOPPED:\n%s", out)
	}
}

// writeSettingsLoop writes one loop configuration under a fresh .agent-utils
// directory and returns that directory. The file is real because applySettings
// resolves and loads it -- a fake would not exercise the config defaults an
// unset harness relies on.
func writeSettingsLoop(t *testing.T, loop, harness, model string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), config.DirName)
	cfgDir := config.ConfigsDir(dir)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("name: %s\nrepo: o/r\n"+
		"checkout_base_dir: /tmp/checkout\nworktree_dir: /tmp/worktrees\n"+
		"state_dir: /tmp/state\ndefault_branch: master\n"+
		"labels:\n  trigger: status:go\n  in_flight: status:doing\n"+
		"  blocked: status:blocked\n  review: status:review\n"+
		"agent: {harness: %s, model: %s, worktree: per_issue, timeout: 1h}\n"+
		"retry: {max: 1, backoff: [0s], breaker: {orphan_threshold: 2, cooldown: 1m}}\n"+
		"prompt: p\nresume_prompt: rp\n", loop, harness, model)
	if err := os.WriteFile(filepath.Join(cfgDir, loop+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The override columns on a dispatch row are empty for the ordinary run, so
// the loop's configured values are what the report must show.
func TestApplySettingsFillsFromTheLoopConfiguration(t *testing.T) {
	dir := writeSettingsLoop(t, "planning", "claude", "opus")

	sessions := []Session{{ID: "sess-a", ProjectID: projectA, Loop: "planning"}}
	applySettings(sessions, map[string]string{projectA: dir})

	if got := sessions[0].Model; got != "opus" {
		t.Errorf("Model = %q, want the configured opus", got)
	}
	if got := sessions[0].Harness; got != "claude" {
		t.Errorf("Harness = %q, want the configured claude", got)
	}
}

// A label override is what the dispatch actually ran under, so it wins over
// the configured value -- and an unparseable one is dropped, exactly as
// runner.Effective drops it before building argv.
func TestApplySettingsPrefersTheDispatchOverride(t *testing.T) {
	dir := writeSettingsLoop(t, "planning", "claude", "opus")
	dirs := map[string]string{projectA: dir}

	sessions := []Session{
		{ID: "sess-a", ProjectID: projectA, Loop: "planning", Model: "sonnet", Harness: "pi"},
		{ID: "sess-b", ProjectID: projectA, Loop: "planning", Harness: "nonsense"},
	}
	applySettings(sessions, dirs)

	if sessions[0].Model != "sonnet" || sessions[0].Harness != "pi" {
		t.Errorf("the override must win: %+v", sessions[0])
	}
	if sessions[1].Harness != "claude" {
		t.Errorf("Harness = %q, want the invalid override dropped back to claude",
			sessions[1].Harness)
	}
}

// A loop can be renamed or deleted while its sessions stay in the database. A
// report that dropped or failed on those rows would hide every other session,
// so the row survives with nothing filled in.
func TestApplySettingsLeavesAnUnreadableLoopAlone(t *testing.T) {
	dir := writeSettingsLoop(t, "planning", "claude", "opus")

	sessions := []Session{
		{ID: "sess-a", ProjectID: projectA, Loop: "deleted-loop"},
		// A project the registry no longer knows has no directory to read.
		{ID: "sess-b", ProjectID: projectB, Loop: "planning"},
	}
	applySettings(sessions, map[string]string{projectA: dir})

	for _, s := range sessions {
		if s.Model != "" || s.Harness != "" {
			t.Errorf("session %s must keep empty settings: %+v", s.ID, s)
		}
	}
}

// Both renderers gained the two columns at once, and both print a dash for a
// session whose configuration could not be read.
func TestRenderersShowModelAndHarness(t *testing.T) {
	sessions := []Session{
		{ID: "sess-a", Project: "weather", Loop: "planning", Issue: 42,
			Dispatches: 1, LastStatus: store.StatusSucceeded,
			Model: "opus", Harness: "claude"},
		{ID: "sess-b", Project: "weather", Loop: "gone", Issue: 43,
			Dispatches: 1, LastStatus: store.StatusSucceeded},
	}
	perProject := RenderSessions(
		&Project{Config: &projectConfigStub, Root: "/p", Dir: "/p/.agent-utils"}, sessions, false)
	machine := RenderAllSessions(sessions, SessionFilter{})

	for _, out := range []string{perProject, machine} {
		for _, want := range []string{"MODEL", "HARNESS", "opus", "claude", "-"} {
			if !strings.Contains(out, want) {
				t.Errorf("output should contain %q:\n%s", want, out)
			}
		}
	}
}
