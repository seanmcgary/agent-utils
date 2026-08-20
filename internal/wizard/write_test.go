package wizard

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
)

// validConfig returns a *config.Config that config.validate accepts, built by
// hand rather than through Run, so write_test.go exercises Write in
// isolation from the question script.
func validConfig(name string) *config.Config {
	return &config.Config{
		Name:            name,
		Repo:            "acme/example",
		CheckoutBaseDir: "/tmp/example",
		WorktreeDir:     "/tmp/worktrees",
		DefaultBranch:   "main",
		Labels: config.Labels{
			Trigger: "status:trigger", InFlight: "status:in-flight",
			Blocked: "status:blocked", Review: "status:review",
			Veto: []string{"blocked:*"},
		},
		Agent: config.Agent{
			Model: "opus", Effort: "high", PermissionMode: "acceptEdits",
			Worktree: config.WorktreePerIssue, MaxBudgetUSD: 25,
			Timeout: config.Duration(3 * time.Hour),
		},
		Retry: config.Retry{
			Max:     1,
			Backoff: []config.Duration{config.Duration(0)},
			Breaker: config.Breaker{
				OrphanThreshold: 2,
				Cooldown:        config.Duration(30 * time.Minute),
			},
		},
		Prompt:       "do the thing for #{{.Issue.Number}}",
		ResumePrompt: "resume the thing for #{{.Issue.Number}}",
	}
}

func TestWriteProducesAFileConfigLoadAccepts(t *testing.T) {
	dir := t.TempDir()
	cfg := validConfig("planning")

	path, err := Write(dir, cfg)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := config.Load(path); err != nil {
		t.Fatalf("config.Load(%s): %v", path, err)
	}
}

func TestWriteFileModeAndHeaderComment(t *testing.T) {
	dir := t.TempDir()
	cfg := validConfig("planning")

	path, err := Write(dir, cfg)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %v, want 0600", got)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.HasPrefix(string(raw), "#") {
		t.Fatalf("written file has no leading header comment:\n%s", raw)
	}
}

func TestWriteRefusesToOverwriteAndNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	cfg := validConfig("planning")

	if _, err := Write(dir, cfg); err != nil {
		t.Fatalf("first Write: %v", err)
	}

	_, err := Write(dir, cfg)
	if err == nil {
		t.Fatal("second Write to the same name did not error")
	}
	if !strings.Contains(err.Error(), "planning.yaml") {
		t.Fatalf("error does not name the file it refused to overwrite: %v", err)
	}
}

// Nothing checked that toYAMLDoc's twenty-five field mappings carry the right
// VALUES. TestWriteProducesAFileConfigLoadAccepts cannot: Write already calls
// config.Load itself and returns its error, so the reload afterwards is a
// tautology. Seven deliberate mis-mappings -- Blocked taking the review label,
// a hardcoded model, a dropped veto list, a hardcoded budget, Prompt taking
// resume_prompt, TendPR forced false, a dropped bypass acknowledgement --
// survived the whole suite. A blocked/review swap alone would ship a wizard
// that applies the wrong label to every issue it parks.
//
// So: reload the written file and require it to equal what went in, field for
// field. A new field added to config.Config and forgotten in toYAMLDoc fails
// here too, which is the other half of what this is for.
func TestWriteRoundTripsEveryValue(t *testing.T) {
	dir := t.TempDir()
	cfg := validConfig("planning")

	// Every optional field set as well, so nothing is compared as zero
	// against zero -- a mapping that dropped one would otherwise pass.
	cfg.StateDir = filepath.Join(dir, "state", "planning")
	cfg.Labels.Terminal = "status:done"
	cfg.Labels.Veto = []string{"blocked:*", "status:hold"}
	// Not "opus": the fixture's default is what a hardcoded mapping would
	// most plausibly be hardcoded TO, and this test has to catch that.
	cfg.Agent.Model = "sonnet"
	cfg.Agent.PermissionMode = "bypassPermissions"
	cfg.AcknowledgeBypassPermissions = true
	cfg.TendPR = true
	cfg.TendPrompt = "rebase #{{.Issue.Number}}"
	cfg.Retry.Max = 3
	cfg.Retry.Backoff = []config.Duration{
		config.Duration(0),
		config.Duration(15 * time.Minute),
		config.Duration(30 * time.Minute),
	}

	path, err := Write(dir, cfg)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(%s): %v", path, err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Errorf("the written configuration does not round-trip.\n got: %+v\nwant: %+v", got, cfg)
	}
}

// The reload at the end of Write is the wizard's own proof that the file it
// just produced is one every other command can load. Its FAILURE path matters
// as much: the file is kept (the path is still returned, so an operator can
// look at it) and the loader's own error is reported rather than a generic
// "write failed" that names nothing.
func TestWriteReportsAReloadFailureAndKeepsTheFile(t *testing.T) {
	dir := t.TempDir()
	cfg := validConfig("planning")
	// default_branch is required by config.validate, and toYAMLDoc copies it
	// through, so an empty one produces a file the strict loader rejects.
	cfg.DefaultBranch = ""

	path, err := Write(dir, cfg)
	if err == nil {
		t.Fatal("Write returned no error for a file config.Load rejects")
	}
	if !strings.Contains(err.Error(), "failed to reload") {
		t.Errorf("err = %v, want it to say the written file failed to reload", err)
	}
	if !strings.Contains(err.Error(), "default_branch") {
		t.Errorf("err = %v, want the loader's own reason, not a generic write failure", err)
	}
	if path == "" {
		t.Fatal("Write returned no path; the file it kept for inspection is unfindable")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("the rejected file was not kept for inspection: %v", statErr)
	}
}
