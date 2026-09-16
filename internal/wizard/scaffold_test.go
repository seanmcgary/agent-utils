package wizard

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
)

// The whole point of Scaffold is that the templates' own agent settings
// survive, which is exactly what Run discards. Pinning them here means a
// template edit that changes a model or an effort has to change this table
// too, deliberately.
var wantAgent = map[string]struct {
	model  string
	effort string
}{
	"planning":                {"opus", "high"},
	"execution":               {"sonnet", "medium"},
	"pr-review":               {"opus", "medium"},
	"exec-pr-review-findings": {"sonnet", "medium"},
}

func TestScaffoldReturnsEveryTemplateInPipelineOrder(t *testing.T) {
	got, err := Scaffold(Detected{Repo: "owner/name"})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if len(got) != len(templateNames) {
		t.Fatalf("Scaffold returned %d configurations, want %d", len(got), len(templateNames))
	}
	for i, cfg := range got {
		if cfg.Name != templateNames[i] {
			t.Errorf("Scaffold()[%d].Name = %q, want %q", i, cfg.Name, templateNames[i])
		}
	}
}

// The four machine-specific fields are the only ones Scaffold overrides; a
// template's hardcoded repo and absolute checkout path would otherwise point
// every generated loop at the repository the template was written from.
func TestScaffoldOverridesTheMachineSpecificFields(t *testing.T) {
	got, err := Scaffold(Detected{
		Repo: "owner/name",
		// Deliberately set: Detect fills these from git, and Scaffold must
		// ignore both in favour of project-relative values.
		CheckoutBaseDir: "/somewhere/else",
		DefaultBranch:   "main",
	})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	for _, cfg := range got {
		t.Run(cfg.Name, func(t *testing.T) {
			if cfg.Repo != "owner/name" {
				t.Errorf("repo = %q, want %q", cfg.Repo, "owner/name")
			}
			if cfg.CheckoutBaseDir != scaffoldCheckoutBaseDir {
				t.Errorf("checkout_base_dir = %q, want %q", cfg.CheckoutBaseDir, scaffoldCheckoutBaseDir)
			}
			if cfg.WorktreeDir != worktreeDirDefault {
				t.Errorf("worktree_dir = %q, want %q", cfg.WorktreeDir, worktreeDirDefault)
			}
			if cfg.StateDir != "" {
				t.Errorf("state_dir = %q, want the derived default (empty)", cfg.StateDir)
			}
		})
	}
}

// master is the base branch every template names, and Scaffold takes it
// verbatim rather than from Detect -- a detected origin/HEAD of "main" must
// not win. A template edit that moved off master would silently change every
// scaffolded project, so it fails here instead.
func TestScaffoldKeepsMasterAsTheDefaultBranch(t *testing.T) {
	got, err := Scaffold(Detected{Repo: "owner/name", DefaultBranch: "main"})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	for _, cfg := range got {
		if cfg.DefaultBranch != "master" {
			t.Errorf("%s: default_branch = %q, want %q", cfg.Name, cfg.DefaultBranch, "master")
		}
	}
}

func TestScaffoldKeepsTheTemplatesAgentAndRetrySettings(t *testing.T) {
	got, err := Scaffold(Detected{Repo: "owner/name"})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	for _, cfg := range got {
		t.Run(cfg.Name, func(t *testing.T) {
			want, ok := wantAgent[cfg.Name]
			if !ok {
				t.Fatalf("no expected agent settings for template %q; add it to wantAgent", cfg.Name)
			}
			if cfg.Agent.Model != want.model {
				t.Errorf("agent.model = %q, want %q", cfg.Agent.Model, want.model)
			}
			if cfg.Agent.Effort != want.effort {
				t.Errorf("agent.effort = %q, want %q", cfg.Agent.Effort, want.effort)
			}
			if cfg.Agent.PermissionMode != "bypassPermissions" {
				t.Errorf("agent.permission_mode = %q, want %q", cfg.Agent.PermissionMode, "bypassPermissions")
			}
			if !cfg.AcknowledgeBypassPermissions {
				t.Error("i_understand_bypass_permissions is false; config.validate rejects bypassPermissions without it")
			}
			if cfg.Retry.Max != 3 {
				t.Errorf("retry.max = %d, want 3", cfg.Retry.Max)
			}
			if len(cfg.Retry.Backoff) != 3 {
				t.Errorf("retry.backoff has %d entries, want 3", len(cfg.Retry.Backoff))
			}
			if cfg.Labels.Trigger == "" || cfg.Prompt == "" || cfg.ResumePrompt == "" {
				t.Error("labels and prompts did not survive the scaffold")
			}
		})
	}
}

// A repository cannot be asked for: --all-loops runs with no prompt at all,
// so an undetectable origin remote has to be an error here rather than a set
// of four loop files pointing at nothing.
func TestScaffoldRequiresARepo(t *testing.T) {
	_, err := Scaffold(Detected{})
	if err == nil {
		t.Fatal("Scaffold with no detected repo returned no error")
	}
	if !strings.Contains(err.Error(), "repo") {
		t.Errorf("error does not mention the repository: %v", err)
	}
}

// Scaffold's output is written through Write, whose yamlDoc is a hand-written
// mirror of config.Config -- so a field Write does not know about is silently
// dropped. Round-tripping every scaffolded configuration through Write and
// comparing what reloads is the only thing that catches that.
func TestScaffoldSurvivesWriteAndReload(t *testing.T) {
	dir := t.TempDir()
	got, err := Scaffold(Detected{Repo: "owner/name"})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	for _, cfg := range got {
		path, err := Write(dir, cfg)
		if err != nil {
			t.Fatalf("Write(%s): %v", cfg.Name, err)
		}
		if want := filepath.Join(dir, config.ConfigsSubdir, cfg.Name+".yaml"); path != want {
			t.Errorf("Write returned %q, want %q", path, want)
		}

		reloaded, err := config.Load(path)
		if err != nil {
			t.Fatalf("reload %s: %v", path, err)
		}
		if reloaded.Agent.Model != cfg.Agent.Model || reloaded.Agent.Effort != cfg.Agent.Effort {
			t.Errorf("%s: agent settings did not survive Write: got %s/%s, want %s/%s",
				cfg.Name, reloaded.Agent.Model, reloaded.Agent.Effort, cfg.Agent.Model, cfg.Agent.Effort)
		}
		if reloaded.DefaultBranch != cfg.DefaultBranch {
			t.Errorf("%s: default_branch did not survive Write: got %q, want %q",
				cfg.Name, reloaded.DefaultBranch, cfg.DefaultBranch)
		}
		if reloaded.Prompt != cfg.Prompt || reloaded.ResumePrompt != cfg.ResumePrompt {
			t.Errorf("%s: prompts did not survive Write", cfg.Name)
		}
	}
}

// Write's yamlDoc has no entry for the two tri-state pointer fields, so a
// template that set either one would have it dropped on the way to disk --
// scaffolding a loop whose configuration differs from the template it came
// from, silently. No template sets them today; this fails the moment one
// does, which is when yamlDoc needs the field.
func TestNoTemplateSetsAFieldWriteWouldDrop(t *testing.T) {
	for _, name := range templateNames {
		cfg, err := loadEmbeddedTemplate(name)
		if err != nil {
			t.Fatalf("load template %q: %v", name, err)
		}
		if cfg.CleanupClosedPR != nil {
			t.Errorf("template %q sets cleanup_closed_pr, which wizard.Write's yamlDoc drops; add the field to yamlDoc", name)
		}
		if cfg.Agent.BackgroundTasks != nil {
			t.Errorf("template %q sets agent.background_tasks, which wizard.Write's yamlDoc drops; add the field to yamlDoc", name)
		}
	}
}
