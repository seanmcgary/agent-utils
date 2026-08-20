// Package runner builds and supervises claude invocations.
package runner

import (
	"bytes"
	"fmt"
	"strconv"
	"text/template"

	"github.com/seanmcgary/agent-utils/internal/config"
)

// Invocation describes one claude run.
type Invocation struct {
	SessionID string
	Prompt    string
	// Resume continues an existing session instead of assigning a new one.
	Resume bool
}

// BuildArgs returns the argument list for claude.
//
// The stream-json output format is mandatory, because it gives the log file and
// the machine readable result in one stream. Running the binary confirmed that
// --print with --output-format stream-json is rejected unless --verbose is also
// present, so --verbose is not optional here.
func BuildArgs(cfg *config.Config, inv Invocation) []string {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
	}

	if inv.Resume {
		args = append(args, "-r", inv.SessionID)
	} else {
		args = append(args, "--session-id", inv.SessionID)
	}

	if cfg.Agent.Model != "" {
		args = append(args, "--model", cfg.Agent.Model)
	}
	if cfg.Agent.Effort != "" {
		args = append(args, "--effort", cfg.Agent.Effort)
	}
	if cfg.Agent.PermissionMode != "" {
		args = append(args, "--permission-mode", cfg.Agent.PermissionMode)
	}
	if cfg.Agent.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd",
			strconv.FormatFloat(cfg.Agent.MaxBudgetUSD, 'f', -1, 64))
	}

	// The prompt is positional and must come last.
	return append(args, inv.Prompt)
}

// PiBuildArgs returns the argument list for pi.
//
// pi in print mode with json event output is the pi equivalent of claude's
// --output-format stream-json. --session-id both creates and resumes a session, so
// start and resume carry the same flag. The model must be a provider/id or pattern.
// pi has no cost-ceiling flag: the config layer accepts max_budget_usd but this
// builder never emits it.
func PiBuildArgs(cfg *config.Config, inv Invocation) []string {
	args := []string{
		"-p",
		"--mode", "json",
		"--session-id", inv.SessionID,
		"--model", cfg.Agent.Model,
	}
	if cfg.Agent.Effort != "" {
		args = append(args, "--thinking", cfg.Agent.Effort)
	}
	// The prompt is positional and must come last.
	return append(args, inv.Prompt)
}

// PromptIssue is the issue view a prompt template can read.
type PromptIssue struct {
	Number int
	Title  string
}

// PromptPR is the pull request view a prompt template can read.
type PromptPR struct {
	Number   int
	HeadRef  string
	BaseRef  string
	BehindBy int
}

// PromptLabels is the label view a prompt template can read.
type PromptLabels struct {
	Trigger  string
	InFlight string
	Blocked  string
	Review   string
	Terminal string
}

// PromptData is the full template context.
type PromptData struct {
	Repo      string
	Loop      string
	SessionID string
	Worktree  string
	Issue     PromptIssue
	PR        PromptPR
	Labels    PromptLabels
}

// RenderPrompt renders a prompt template. An unknown field is an error, so a
// typo in a configuration file fails at dispatch rather than sending the agent
// a prompt with a hole in it.
func RenderPrompt(tmpl string, data PromptData) (string, error) {
	t, err := template.New("prompt").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse prompt template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render prompt template: %w", err)
	}
	return buf.String(), nil
}
