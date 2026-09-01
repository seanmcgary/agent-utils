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

// status:ready-for-review is the human's merge queue: exactly one loop may end
// there, and no loop may trigger on it.
//
// The pipeline allows the human exactly two touches -- approving the plan, and
// merging -- and every handoff between them is machine-to-machine. The way that
// breaks is a loop reaching for the human's label to get something else: it used
// to be tend eligibility, which was gated on a per-loop label and made the
// execution loop declare status:ready-for-review purely to be tended. That
// summoned the human at execution time, before the branch had been reviewed or
// fixed at all.
//
// Nothing else in the suite would catch a regression here. Every file would
// still load, every loop would still dispatch, and the epic graph is a
// declaration now, so it would not shift either.
func TestOnlyTheFinalLoopEndsAtTheHumansQueue(t *testing.T) {
	const humanQueue = "status:ready-for-review"
	const finalLoop = "exec-pr-review-findings"

	entries, err := os.ReadDir(filepath.Join("..", "..", "examples"))
	if err != nil {
		t.Fatalf("read examples: %v", err)
	}

	var ending []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		cfg, err := config.Load(filepath.Join("..", "..", "examples", e.Name()))
		if err != nil {
			t.Fatalf("load %s: %v", e.Name(), err)
		}
		if cfg.Labels.Terminal == humanQueue {
			ending = append(ending, cfg.Name)
		}
		// Triggering on it would be worse than merely filling it: a loop would
		// RUN on the queue the human is reading.
		if cfg.Labels.Trigger == humanQueue {
			t.Errorf("loop %q triggers on %s, which is the human's merge queue", cfg.Name, humanQueue)
		}
	}

	if len(ending) != 1 || ending[0] != finalLoop {
		t.Errorf("loops ending at %s = %v, want exactly [%s]", humanQueue, ending, finalLoop)
	}
}

// Every loop declares the same four labels and nothing else. This is the shape
// the design rests on: a loop describes its own states -- queued, working,
// stuck, done -- and says nothing about its neighbours. The chain exists only
// because an operator chose one loop's terminal to be another's trigger, which
// is a fact about the values, not a field either loop sets.
//
// The terminal is what makes it uniform. Every loop now has one, including the
// last: exec-pr-review-findings ends at the human's queue, which is a terminal
// like any other and happens to have no loop triggering on it.
func TestEveryExampleLoopDeclaresTheSameLabelRoles(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "examples"))
	if err != nil {
		t.Fatalf("read examples: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			cfg, err := config.Load(filepath.Join("..", "..", "examples", e.Name()))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			for _, l := range []struct{ role, value string }{
				{"trigger", cfg.Labels.Trigger},
				{"in_flight", cfg.Labels.InFlight},
				{"blocked", cfg.Labels.Blocked},
				{"terminal", cfg.Labels.Terminal},
			} {
				if l.value == "" {
					t.Errorf("labels.%s is empty: every loop ends by applying its terminal, so every loop needs all four", l.role)
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

// No example loop configures tending, because no loop file can any more. The
// policy is the project descriptor's, and this test exists to catch a
// reintroduction: examples/ has no descriptor beside it, so every example loads
// with the zero policy and TendsPRs must be false for all of them.
//
// It matters most for planning. plan-feature's design draft pull request says
// "Closes #N", so a planning loop that tended would force-push a draft the human
// is reading.
func TestNoExampleLoopTends(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "examples"))
	if err != nil {
		t.Fatalf("read examples: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		cfg, err := config.Load(filepath.Join("..", "..", "examples", e.Name()))
		if err != nil {
			t.Fatalf("load %s: %v", e.Name(), err)
		}
		if cfg.TendsPRs() {
			t.Errorf("%s: TendsPRs = true, want false: tending is the project's policy, not a loop's", e.Name())
		}
	}
}

// The planning agent must never apply the APPROVAL label.
//
// Every loop now ends by applying its own terminal, planning included -- its
// terminal means "planning is finished", not "the plan is approved". Approval is
// a separate label the human applies after reading, and it is the one gate the
// whole pipeline depends on, so the prompt has to forbid the agent from ever
// applying it.
//
// cfg.Prompt holds the template source, so the check runs against the rendered
// prompt an agent would actually receive.
func TestPlanningNeverApprovesItself(t *testing.T) {
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
			Terminal: cfg.Labels.Terminal,
		},
	}
	rendered, err := runner.RenderPrompt(cfg.Prompt, data)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}

	// The approval label is not planning's terminal and is not any label the
	// loop declares, so it is named literally here, exactly as the prompt names
	// it. That is the point of the test: a value no field carries is one a
	// refactor can silently drop.
	const approval = "status:ready-for-execution"
	if !containsAll(rendered, "NEVER apply", approval) {
		t.Errorf("the planning prompt must forbid applying %s", approval)
	}
	if cfg.Labels.Terminal == approval {
		t.Errorf("planning's terminal must not BE the approval label: it is applied by the agent")
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
