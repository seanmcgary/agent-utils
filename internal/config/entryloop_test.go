package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loopYAML is a minimal valid loop file. The fields Load requires are all
// present; only the parts EntryLoop reads vary between cases.
func loopYAML(name, repo, trigger, review, terminal string) string {
	body := `
name: ` + name + `
repo: ` + repo + `
checkout_base_dir: /tmp/checkout
worktree_dir: /tmp/worktrees
state_dir: /tmp/state
default_branch: master
labels:
  trigger: ` + trigger + `
  in_flight: status:in-flight-` + name + `
  blocked: status:blocked-` + name + `
  review: ` + review + `
`
	if terminal != "" {
		body += "  terminal: " + terminal + "\n"
	}
	body += `agent: {model: opus, worktree: per_issue, timeout: 1h}
retry: {max: 1, backoff: [0s], breaker: {orphan_threshold: 2, cooldown: 1m}}
prompt: p
resume_prompt: rp
`
	return body
}

// writeLoops creates a .agent-utils/configs directory holding the given files,
// keyed by file name.
func writeLoops(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), DirName)
	cfgDir := ConfigsDir(dir)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(cfgDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The reference pair. planning.terminal IS execution.trigger, so execution is
// downstream and planning is the entry.
func TestEntryLoopResolvesTheReferencePair(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"planning.yaml": loopYAML("planning", "o/r",
			"status:ready-for-spec", "status:plan-ready-for-review", "status:ready-for-execution"),
		"execution.yaml": loopYAML("execution", "o/r",
			"status:ready-for-execution", "status:ready-for-review", ""),
	})

	got, err := EntryLoop(dir, "o/r")
	if err != nil {
		t.Fatalf("EntryLoop: %v", err)
	}
	if got != "planning" {
		t.Errorf("EntryLoop = %q, want planning", got)
	}
}

func TestEntryLoopResolvesASingleLoop(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"only.yaml": loopYAML("only", "o/r", "status:ready-for-spec", "status:in-review", ""),
	})

	got, err := EntryLoop(dir, "o/r")
	if err != nil {
		t.Fatalf("EntryLoop: %v", err)
	}
	if got != "only" {
		t.Errorf("EntryLoop = %q, want only", got)
	}
}

// Two loops neither of which is downstream of the other. Guessing here would
// promote issues into the wrong stage of the pipeline, silently.
func TestEntryLoopRefusesWhenAmbiguous(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"a.yaml": loopYAML("a", "o/r", "status:ready-for-a", "status:review-a", ""),
		"b.yaml": loopYAML("b", "o/r", "status:ready-for-b", "status:review-b", ""),
	})

	_, err := EntryLoop(dir, "o/r")
	if !errors.Is(err, ErrAmbiguousEntryLoop) {
		t.Fatalf("EntryLoop error = %v, want ErrAmbiguousEntryLoop", err)
	}
	// The loops must be NAMED. An operator cannot fix "it is ambiguous".
	for _, want := range []string{"a", "b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name loop %q: %v", want, err)
		}
	}
}

// A cycle: each loop's trigger is the other's terminal. No loop is the entry.
func TestEntryLoopRefusesWhenNoneIsTheEntry(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"a.yaml": loopYAML("a", "o/r", "status:x", "status:review-a", "status:y"),
		"b.yaml": loopYAML("b", "o/r", "status:y", "status:review-b", "status:x"),
	})

	_, err := EntryLoop(dir, "o/r")
	if !errors.Is(err, ErrNoEntryLoop) {
		t.Fatalf("EntryLoop error = %v, want ErrNoEntryLoop", err)
	}
}

// A loop file that does not load leaves the graph incomplete. The loop it
// declares might be the one that makes another downstream, so the derivation
// cannot be trusted and nothing sweeps.
func TestEntryLoopRefusesWhenALoopFileIsBroken(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"planning.yaml": loopYAML("planning", "o/r",
			"status:ready-for-spec", "status:plan-ready-for-review", "status:ready-for-execution"),
		"broken.yaml": "name: broken\nrepo: o/r\nthis is not: [valid",
	})

	if _, err := EntryLoop(dir, "o/r"); err == nil {
		t.Fatal("want an error when a loop file cannot be loaded, got nil")
	}
}

// Two files declaring one name. Each would exclude the other as "itself", so
// the graph would lose an edge and the message would name one loop twice.
func TestEntryLoopRefusesDuplicateLoopNames(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"a.yaml": loopYAML("planning", "o/r",
			"status:ready-for-spec", "status:plan-ready-for-review", "status:ready-for-execution"),
		"b.yaml": loopYAML("planning", "o/r",
			"status:ready-for-execution", "status:ready-for-review", ""),
	})

	_, err := EntryLoop(dir, "o/r")
	if !errors.Is(err, ErrAmbiguousEntryLoop) {
		t.Fatalf("EntryLoop error = %v, want ErrAmbiguousEntryLoop", err)
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error does not say the names are duplicated: %v", err)
	}
}

// A loop watching another repository is not part of this repository's pipeline
// and must not make its trigger look downstream.
func TestEntryLoopIgnoresAnotherRepositorysLoops(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"planning.yaml": loopYAML("planning", "o/r",
			"status:ready-for-spec", "status:plan-ready-for-review", "status:ready-for-execution"),
		"other.yaml": loopYAML("other", "other/repo",
			"status:whatever", "status:review-other", "status:ready-for-spec"),
	})

	got, err := EntryLoop(dir, "o/r")
	if err != nil {
		t.Fatalf("EntryLoop: %v", err)
	}
	if got != "planning" {
		t.Errorf("EntryLoop = %q, want planning", got)
	}
}

// The repository is spelled by hand in each loop file, so two files may differ
// in case while naming one repository.
func TestEntryLoopMatchesTheRepositoryCaseInsensitively(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"planning.yaml": loopYAML("planning", "O/R",
			"status:ready-for-spec", "status:plan-ready-for-review", "status:ready-for-execution"),
		"execution.yaml": loopYAML("execution", "o/r",
			"status:ready-for-execution", "status:ready-for-review", ""),
	})

	got, err := EntryLoop(dir, "o/r")
	if err != nil {
		t.Fatalf("EntryLoop: %v", err)
	}
	if got != "planning" {
		t.Errorf("EntryLoop = %q, want planning", got)
	}
}

func TestEntryLoopRefusesWhenNoLoopWatchesTheRepository(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"other.yaml": loopYAML("other", "other/repo", "status:x", "status:review-other", ""),
	})

	if _, err := EntryLoop(dir, "o/r"); !errors.Is(err, ErrNoEntryLoop) {
		t.Fatalf("EntryLoop error = %v, want ErrNoEntryLoop", err)
	}
}
