package config_test

import (
	"path/filepath"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/runner"
)

// The example configurations are the port of the two reference loops. A typo in
// a template must fail here, not at three in the morning inside a detached
// process whose output nobody is reading.
func TestExampleConfigsLoadAndRender(t *testing.T) {
	for _, name := range []string{"planning.yaml", "execution.yaml", "pi.yaml"} {
		t.Run(name, func(t *testing.T) {
			cfg, err := config.Load(filepath.Join("..", "..", "examples", name))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			data := runner.PromptData{
				Repo:      cfg.Repo,
				Loop:      cfg.Name,
				SessionID: "sess",
				Worktree:  "/tmp/wt",
				Issue:     runner.PromptIssue{Number: 12, Title: "a title"},
				PR: runner.PromptPR{
					Number: 34, HeadRef: "feat/x", BaseRef: "master", BehindBy: 3,
				},
				Labels: runner.PromptLabels{
					Trigger:  cfg.Labels.Trigger,
					InFlight: cfg.Labels.InFlight,
					Blocked:  cfg.Labels.Blocked,
					Review:   cfg.Labels.Review,
					Terminal: cfg.Labels.Terminal,
				},
			}

			for label, tmpl := range map[string]string{
				"prompt":        cfg.Prompt,
				"resume_prompt": cfg.ResumePrompt,
				"tend_prompt":   cfg.TendPrompt,
			} {
				if tmpl == "" {
					continue
				}
				if _, err := runner.RenderPrompt(tmpl, data); err != nil {
					t.Errorf("%s: %v", label, err)
				}
			}
		})
	}
}

// The pi example must be a pi harness with the claude-only fields absent.
func TestPiExampleIsPiHarness(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "examples", "pi.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.Harness != config.HarnessPi {
		t.Errorf("Harness = %q, want %q", cfg.Agent.Harness, config.HarnessPi)
	}
	if cfg.Agent.PermissionMode != "" {
		t.Errorf("pi example must omit permission_mode, got %q", cfg.Agent.PermissionMode)
	}
}

// The planning loop must never tend. plan-feature's design draft pull request
// says "Closes #N", so tending would force-push a draft the human is reading.
func TestPlanningExampleDoesNotTend(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "examples", "planning.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TendPR {
		t.Error("tend_pr must be false for the planning loop")
	}
}

// No template may tell an agent to apply the terminal label. That gate is the
// human's, and it is the one rule the whole pipeline depends on.
//
// cfg.Prompt holds the template source, where the label is a
// {{.Labels.Terminal}} placeholder rather than a literal value, so the check
// runs against the rendered prompt an agent would actually receive.
func TestNoTemplateApprovesItself(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "..", "examples", "planning.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	data := runner.PromptData{
		Repo:      cfg.Repo,
		Loop:      cfg.Name,
		SessionID: "sess",
		Worktree:  "/tmp/wt",
		Issue:     runner.PromptIssue{Number: 12, Title: "a title"},
		Labels: runner.PromptLabels{
			Trigger:  cfg.Labels.Trigger,
			InFlight: cfg.Labels.InFlight,
			Blocked:  cfg.Labels.Blocked,
			Review:   cfg.Labels.Review,
			Terminal: cfg.Labels.Terminal,
		},
	}
	rendered, err := runner.RenderPrompt(cfg.Prompt, data)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}

	if !containsAll(rendered, "NEVER apply", cfg.Labels.Terminal) {
		t.Error("the planning prompt must forbid applying the terminal label")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
