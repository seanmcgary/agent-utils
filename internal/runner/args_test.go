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
