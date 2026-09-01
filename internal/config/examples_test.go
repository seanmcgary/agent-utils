package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/runner"
)

// The example configurations are the port of the two reference loops. A typo in
// a template must fail here, not at three in the morning inside a detached
// process whose output nobody is reading.
func TestExampleConfigsLoadAndRender(t *testing.T) {
	// Every file in examples/, not a subset. pr-review.yaml and
	// exec-pr-review-findings.yaml are also embedded as wizard templates and so
	// are loaded a second time there, but that test is about the wizard's copy;
	// a reader of this list should not have to know which examples happen to be
	// covered somewhere else.
	for _, name := range []string{
		"planning.yaml",
		"execution.yaml",
		"pr-review.yaml",
		"exec-pr-review-findings.yaml",
		"pi.yaml",
	} {
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

// The four reference loops must resolve to exactly ONE entry loop, and it must
// be planning.
//
// This is not a test of EntryLoop -- entryloop_test.go covers the rule. It is a
// test of the example FILES, and it exists because the failure it catches is
// invisible. EntryLoop reads the chain out of labels.terminal and labels.review,
// so a loop file that simply omits a terminal creates a second loop whose
// trigger is nobody's terminal: two entries, ErrAmbiguousEntryLoop, and an epic
// sweep that logs a warning and promotes nothing for the whole project. Every
// individual file still loads and every loop still dispatches, so nothing else
// in this package or in the wizard notices. Before the review/remediation split
// this was already true of the shipped examples, undetected: pr-review's trigger
// was no other loop's terminal because execution declared none.
//
// pi.yaml is copied in too. It shares execution's trigger and names the same
// repo, which is exactly the shape most likely to reintroduce ambiguity.
func TestExampleLoopsResolveToOneEntryLoop(t *testing.T) {
	agentUtilsDir := t.TempDir()
	configs := config.ConfigsDir(agentUtilsDir)
	if err := os.MkdirAll(configs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join("..", "..", "examples"))
	if err != nil {
		t.Fatalf("read examples: %v", err)
	}
	var repo string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		body, err := os.ReadFile(filepath.Join("..", "..", "examples", e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(configs, e.Name()), body, 0o644); err != nil {
			t.Fatalf("write %s: %v", e.Name(), err)
		}
		cfg, err := config.Load(filepath.Join(configs, e.Name()))
		if err != nil {
			t.Fatalf("load %s: %v", e.Name(), err)
		}
		// The examples all target one repository; assert it rather than
		// assuming, so a future example pointed elsewhere fails loudly here
		// instead of quietly narrowing what this test covers.
		if repo == "" {
			repo = cfg.Repo
		} else if cfg.Repo != repo {
			t.Fatalf("examples/%s names repo %q, want %q: this test assumes one repo",
				e.Name(), cfg.Repo, repo)
		}
	}

	got, err := config.EntryLoop(agentUtilsDir, repo)
	if err != nil {
		t.Fatalf("EntryLoop: %v", err)
	}
	if got != "planning" {
		t.Errorf("EntryLoop = %q, want %q", got, "planning")
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
