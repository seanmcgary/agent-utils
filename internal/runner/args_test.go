package runner

import (
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
)

func cfg() *config.Config {
	return &config.Config{
		Agent: config.Agent{
			Model:          "opus",
			Effort:         "high",
			PermissionMode: "bypassPermissions",
			MaxBudgetUSD:   25,
		},
	}
}

func joined(args []string) string { return strings.Join(args, " ") }

func TestBuildArgsAlwaysIncludesVerbose(t *testing.T) {
	// Verified by running the binary: --output-format stream-json under
	// --print is rejected without --verbose.
	args := BuildArgs(cfg(), Invocation{SessionID: "s1", Prompt: "go"})
	j := joined(args)
	if !strings.Contains(j, "--output-format stream-json") {
		t.Errorf("missing stream-json: %s", j)
	}
	if !strings.Contains(j, "--verbose") {
		t.Errorf("stream-json without --verbose is rejected by claude: %s", j)
	}
}

func TestBuildArgsStartUsesSessionID(t *testing.T) {
	args := BuildArgs(cfg(), Invocation{SessionID: "s1", Prompt: "go"})
	j := joined(args)
	if !strings.Contains(j, "--session-id s1") {
		t.Errorf("missing --session-id: %s", j)
	}
	if strings.Contains(j, " -r ") {
		t.Errorf("a start must not resume: %s", j)
	}
}

func TestBuildArgsResumeUsesResumeFlag(t *testing.T) {
	args := BuildArgs(cfg(), Invocation{SessionID: "s1", Prompt: "go", Resume: true})
	j := joined(args)
	if !strings.Contains(j, "-r s1") {
		t.Errorf("missing -r: %s", j)
	}
	if strings.Contains(j, "--session-id") {
		t.Errorf("a resume must not assign a new session id: %s", j)
	}
}

func TestBuildArgsCarriesAgentSettings(t *testing.T) {
	j := joined(BuildArgs(cfg(), Invocation{SessionID: "s", Prompt: "p"}))
	for _, want := range []string{
		"--model opus",
		"--effort high",
		"--permission-mode bypassPermissions",
		"--max-budget-usd 25",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in %s", want, j)
		}
	}
}

func TestBuildArgsOmitsEmptyOptionalSettings(t *testing.T) {
	c := cfg()
	c.Agent.Effort = ""
	c.Agent.PermissionMode = ""
	c.Agent.MaxBudgetUSD = 0
	j := joined(BuildArgs(c, Invocation{SessionID: "s", Prompt: "p"}))
	for _, bad := range []string{"--effort", "--permission-mode", "--max-budget-usd"} {
		if strings.Contains(j, bad) {
			t.Errorf("unset option %q must be omitted: %s", bad, j)
		}
	}
}

func TestBuildArgsPutsPromptLast(t *testing.T) {
	args := BuildArgs(cfg(), Invocation{SessionID: "s", Prompt: "the prompt"})
	if args[len(args)-1] != "the prompt" {
		t.Errorf("last argument = %q, want the prompt", args[len(args)-1])
	}
}

func piCfg() *config.Config {
	return &config.Config{
		Agent: config.Agent{
			Harness:  config.HarnessPi,
			Model:    "anthropic/claude-sonnet-4-5",
			Effort:   "high",
			Worktree: "per_issue",
		},
	}
}

func TestPiBuildArgsPrintMode(t *testing.T) {
	args := PiBuildArgs(piCfg(), Invocation{SessionID: "s1", Prompt: "go"})
	if args[0] != "-p" {
		t.Errorf("argv[0] = %q, want -p", args[0])
	}
	if args[1] != "--mode" || args[2] != "json" {
		t.Errorf("print mode header = %v, want --mode json", []string{args[1], args[2]})
	}
}

func TestPiBuildArgsCarriesModelAndSession(t *testing.T) {
	j := joined(PiBuildArgs(piCfg(), Invocation{SessionID: "s9", Prompt: "p"}))
	for _, want := range []string{"--session-id s9", "--model anthropic/claude-sonnet-4-5"} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in %s", want, j)
		}
	}
}

func TestPiBuildArgsAddsThinking(t *testing.T) {
	j := joined(PiBuildArgs(piCfg(), Invocation{SessionID: "s", Prompt: "p"}))
	if !strings.Contains(j, "--thinking high") {
		t.Errorf("missing --thinking high in %s", j)
	}
}

func TestPiBuildArgsOmitsEmptyEffort(t *testing.T) {
	c := piCfg()
	c.Agent.Effort = ""
	j := joined(PiBuildArgs(c, Invocation{SessionID: "s", Prompt: "p"}))
	if strings.Contains(j, "--thinking") {
		t.Errorf("effort empty must omit --thinking: %s", j)
	}
}

func TestPiBuildArgsPutsPromptLast(t *testing.T) {
	args := PiBuildArgs(piCfg(), Invocation{SessionID: "s", Prompt: "the prompt"})
	if args[len(args)-1] != "the prompt" {
		t.Errorf("last argument = %q, want the prompt", args[len(args)-1])
	}
}

func TestRenderPrompt(t *testing.T) {
	got, err := RenderPrompt("issue {{.Issue.Number}} in {{.Repo}}", PromptData{
		Repo:  "o/r",
		Issue: PromptIssue{Number: 12},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "issue 12 in o/r" {
		t.Errorf("got %q", got)
	}
}

func TestRenderPromptRejectsUnknownField(t *testing.T) {
	if _, err := RenderPrompt("{{.Nope}}", PromptData{}); err == nil {
		t.Fatal("want an error for an unknown template field")
	}
}

func TestEffectiveOverrideReplacesConfiguredValue(t *testing.T) {
	s := Effective(cfg(), config.Overrides{Model: "claude-opus-5", Effort: "xhigh"})
	if s.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want override", s.Model)
	}
	if s.Effort != "xhigh" {
		t.Errorf("Effort = %q, want override", s.Effort)
	}
	// Harness was not overridden, so the configured value (empty here) survives.
	if s.Harness != "" {
		t.Errorf("Harness = %q, want unset (no override, no configured harness)", s.Harness)
	}
}

func TestEffectiveUnsetOverrideKeepsConfiguredValue(t *testing.T) {
	s := Effective(cfg(), config.Overrides{})
	if s.Model != "opus" {
		t.Errorf("Model = %q, want configured %q", s.Model, "opus")
	}
	if s.Effort != "high" {
		t.Errorf("Effort = %q, want configured %q", s.Effort, "high")
	}
}

func TestEffectiveNeverMutatesTheConfiguration(t *testing.T) {
	c := cfg()
	_ = Effective(c, config.Overrides{Model: "gpt-5", Harness: config.HarnessPi, Effort: "low"})
	if c.Agent.Model != "opus" || c.Agent.Effort != "high" || c.Agent.Harness != "" {
		t.Fatalf("Effective mutated cfg.Agent: %+v", c.Agent)
	}
}

func TestEffectiveDropsAFlagShapedOverride(t *testing.T) {
	// A row value did not pass through this process's own ParseOverrides --
	// the tick wrote it, possibly under an older binary, and
	// internal/store/legacy.go writes the dispatches table by a second path.
	// Effective re-validates and drops anything that would not have parsed.
	s := Effective(cfg(), config.Overrides{Model: "--dangerously-skip-permissions"})
	if s.Model != "opus" {
		t.Errorf("Model = %q, want the configured value with the bad override dropped", s.Model)
	}
}

func TestEffectiveDropsAnInvalidHarnessOverride(t *testing.T) {
	s := Effective(cfg(), config.Overrides{Harness: "gpt"})
	if s.Harness != "" {
		t.Errorf("Harness = %q, want the invalid override dropped", s.Harness)
	}
}

// TestEffectiveUsesTheNormalisedHarnessValue is spec B7: Effective must use
// the value config.ParseOverrides returns, not the raw ov.Harness field.
// ParseOverrides lowercases harness and effort, so a row carrying harness
// "PI" -- written by an older binary, or via internal/store/legacy.go's
// second write path -- must resolve to the lowercase form the harness
// switch in Supervise (runner.go:149) compares with ==. Using the raw value
// would silently fail that comparison and launch claude with the pi model
// and claudeEnv instead.
func TestEffectiveUsesTheNormalisedHarnessValue(t *testing.T) {
	c := &config.Config{Agent: config.Agent{Model: "opus"}}
	s := Effective(c, config.Overrides{Harness: "PI"})
	if s.Harness != config.HarnessPi {
		t.Errorf("Harness = %q, want the normalised lowercase %q", s.Harness, config.HarnessPi)
	}
}

// The claude-only settings never refuse a harness override. cfg() sets both
// PermissionMode and MaxBudgetUSD; pi implements neither, so the override is
// applied and PiBuildArgs simply emits neither flag.
func TestEffectiveKeepsAPiOverrideOverTheClaudeOnlySettings(t *testing.T) {
	c := cfg()
	s := Effective(c, config.Overrides{Harness: config.HarnessPi})
	if s.Harness != config.HarnessPi {
		t.Fatalf("Harness = %q, want %q: pi ignores what it does not implement",
			s.Harness, config.HarnessPi)
	}
	j := joined(PiBuildArgs(c, Invocation{
		SessionID: "s", Prompt: "p",
		Overrides: config.Overrides{Harness: config.HarnessPi},
	}))
	if strings.Contains(j, "--permission-mode") || strings.Contains(j, "--max-budget-usd") {
		t.Errorf("pi args = %s, want neither claude-only flag", j)
	}
}

func TestBuildArgsUsesTheEffectiveOverride(t *testing.T) {
	j := joined(BuildArgs(cfg(), Invocation{
		SessionID: "s", Prompt: "p",
		Overrides: config.Overrides{Model: "claude-opus-5"},
	}))
	if !strings.Contains(j, "--model claude-opus-5") {
		t.Errorf("missing overridden model: %s", j)
	}
}

func TestPiBuildArgsUsesTheEffectiveOverride(t *testing.T) {
	j := joined(PiBuildArgs(piCfg(), Invocation{
		SessionID: "s", Prompt: "p",
		Overrides: config.Overrides{Model: "openai/gpt-5"},
	}))
	if !strings.Contains(j, "--model openai/gpt-5") {
		t.Errorf("missing overridden model: %s", j)
	}
}
