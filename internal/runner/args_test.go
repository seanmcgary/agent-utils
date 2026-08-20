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
