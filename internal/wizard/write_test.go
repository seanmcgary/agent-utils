package wizard

import (
	"os"
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
