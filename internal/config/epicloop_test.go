package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/project"
)

// loopYAML is a minimal valid loop file. The fields Load requires are all
// present; only the parts EpicLoop reads vary between cases.
//
// Note what it no longer takes: a review label. A loop declares only its own
// states now, and the pipeline graph is not derived from labels at all.
func loopYAML(name, repo, trigger, terminal string) string {
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

// writeLoops creates a .agent-utils directory holding a project descriptor and
// the given loop files, keyed by file name. epicLoop is written into the
// descriptor; pass "" to write a descriptor that declares none.
func writeLoops(t *testing.T, epicLoop string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), DirName)
	cfgDir := ConfigsDir(dir)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := project.Save(dir, &project.Config{
		Name: "p", ID: "00000000-0000-0000-0000-000000000001",
		Epic: project.Epic{Loop: epicLoop},
	}); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(cfgDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// referenceLoops is the shape of the shipped example set: four loops chained by
// one loop's terminal being the next one's trigger. None of that chaining is
// read by EpicLoop -- it is here precisely to prove that. The answer comes from
// the declaration alone, so a set that chains and a set that does not resolve
// identically.
func referenceLoops() map[string]string {
	return map[string]string{
		"planning.yaml": loopYAML("planning", "o/r",
			"status:ready-for-spec", "status:ready-for-plan-review"),
		"execution.yaml": loopYAML("execution", "o/r",
			"status:ready-for-execution", "status:ready-for-pr-review"),
		"pr-review.yaml": loopYAML("pr-review", "o/r",
			"status:ready-for-pr-review", "status:ready-for-findings-exec"),
		"exec-findings.yaml": loopYAML("exec-findings", "o/r",
			"status:ready-for-findings-exec", "status:ready-for-review"),
	}
}

func TestEpicLoopResolvesTheDeclaredLoop(t *testing.T) {
	dir := writeLoops(t, "planning", referenceLoops())

	got, err := EpicLoop(dir, "o/r")
	if err != nil {
		t.Fatalf("EpicLoop: %v", err)
	}
	if got != "planning" {
		t.Errorf("EpicLoop = %q, want planning", got)
	}
}

// The declaration is the whole answer, so naming a MIDDLE loop resolves to it.
// Under the old derivation this was unrepresentable: the answer was forced by
// the label graph and an operator who wanted a different front had no way to say
// so. Whether promoting into execution's trigger is a good idea is the
// operator's call; it is not this function's to override.
func TestEpicLoopHonoursADeclarationThatIsNotTheFrontOfTheChain(t *testing.T) {
	dir := writeLoops(t, "execution", referenceLoops())

	got, err := EpicLoop(dir, "o/r")
	if err != nil {
		t.Fatalf("EpicLoop: %v", err)
	}
	if got != "execution" {
		t.Errorf("EpicLoop = %q, want execution", got)
	}
}

// No declaration is a legitimate configuration -- a project that does not use
// epics -- but it must be REPORTED where a sweep would have run, not treated as
// "sweep with some default". The old derivation had no equivalent state: it
// always produced an answer or an ambiguity.
func TestEpicLoopRefusesWhenNoneIsDeclared(t *testing.T) {
	dir := writeLoops(t, "", referenceLoops())

	_, err := EpicLoop(dir, "o/r")
	if !errors.Is(err, ErrNoEpicLoop) {
		t.Fatalf("err = %v, want ErrNoEpicLoop", err)
	}
	if !strings.Contains(err.Error(), "epic.loop") {
		t.Errorf("error must name the missing field, got %q", err)
	}
}

// A declaration naming a loop that does not exist is a typo, and the message
// has to carry the name that was not found or the operator is left diffing two
// files by eye.
func TestEpicLoopRefusesAnUnknownName(t *testing.T) {
	dir := writeLoops(t, "planing", referenceLoops())

	_, err := EpicLoop(dir, "o/r")
	if !errors.Is(err, ErrNoEpicLoop) {
		t.Fatalf("err = %v, want ErrNoEpicLoop", err)
	}
	if !strings.Contains(err.Error(), "planing") {
		t.Errorf("error must name the loop that was not found, got %q", err)
	}
}

// Promotion writes labels by issue number against one repository, so a loop
// watching a different repository would label whichever local issue happened to
// carry that number.
func TestEpicLoopRefusesALoopWatchingAnotherRepo(t *testing.T) {
	loops := referenceLoops()
	loops["other.yaml"] = loopYAML("other", "o/elsewhere", "status:ready-for-spec", "")
	dir := writeLoops(t, "other", loops)

	_, err := EpicLoop(dir, "o/r")
	if !errors.Is(err, ErrNoEpicLoop) {
		t.Fatalf("err = %v, want ErrNoEpicLoop", err)
	}
	if !strings.Contains(err.Error(), "o/elsewhere") {
		t.Errorf("error must name the repository it watches instead, got %q", err)
	}
}

// A duplicated name makes the declaration name two different loops with two
// different trigger labels, so promoting into either would be a guess.
func TestEpicLoopRefusesDuplicateLoopNames(t *testing.T) {
	dir := writeLoops(t, "planning", map[string]string{
		"planning.yaml": loopYAML("planning", "o/r", "status:ready-for-spec", ""),
		"copy.yaml":     loopYAML("planning", "o/r", "status:something-else", ""),
	})

	_, err := EpicLoop(dir, "o/r")
	if !errors.Is(err, ErrAmbiguousEpicLoop) {
		t.Fatalf("err = %v, want ErrAmbiguousEpicLoop", err)
	}
}

// A file that will not load may be the very loop that was named, so the whole
// resolution fails rather than sweeping past it.
func TestEpicLoopRefusesWhenALoopFileIsBroken(t *testing.T) {
	dir := writeLoops(t, "planning", map[string]string{
		"planning.yaml": loopYAML("planning", "o/r", "status:ready-for-spec", ""),
		"broken.yaml":   "name: broken\nthis is not: [valid",
	})

	if _, err := EpicLoop(dir, "o/r"); err == nil {
		t.Fatal("want an error for an unloadable loop file, got nil")
	}
}

// No descriptor at all is not a project. EpicLoop must say that rather than
// treating it as "no epic loop declared", because the fix is different.
func TestEpicLoopRefusesWithoutAProjectDescriptor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DirName)
	if err := os.MkdirAll(ConfigsDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ConfigsDir(dir), "planning.yaml"),
		[]byte(loopYAML("planning", "o/r", "status:ready-for-spec", "")), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := EpicLoop(dir, "o/r")
	if !errors.Is(err, project.ErrNoConfig) {
		t.Fatalf("err = %v, want project.ErrNoConfig", err)
	}
}
