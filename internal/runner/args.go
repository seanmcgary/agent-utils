// Package runner builds and supervises claude invocations.
package runner

import (
	"bytes"
	"fmt"
	"strconv"
	"text/template"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// Invocation describes one claude run.
type Invocation struct {
	SessionID string
	Prompt    string
	// Resume continues an existing session instead of assigning a new one.
	Resume bool
	// Overrides carries the per-issue label overrides read off the dispatch
	// row. The detached runner never sees the tick's GitHub snapshot, so this
	// is the only way an override reaches it.
	Overrides config.Overrides
	// Kind selects the configuration layer Effective applies: a store.Kind*
	// value. An EMPTY value means "not a tend", which is the correct reading
	// for a hand-built Invocation in a test -- every existing test constructs
	// one without setting this field, and none of them means to exercise the
	// tend: section.
	Kind string
}

// Settings is the resolved agent configuration for one invocation, after an
// override has been applied (or not) to the configured value.
type Settings struct {
	Harness string
	Model   string
	Effort  string
}

// Effective returns the agent settings for one invocation, resolved in three
// layers: the loop's agent: defaults, then the tend: overlay when this
// dispatch is a tend, then the per-issue label override. Each layer only
// replaces what the one before it set; a layer with nothing to say leaves the
// prior value alone.
//
// The tend: overlay applies only when kind == store.KindTend, and only its
// NON-EMPTY fields replace the agent: value -- a tend: section that sets only
// tend.model must leave tend.harness and tend.effort falling through to
// agent.harness and agent.effort, exactly as an absent field in Load leaves
// them (config.go's comment beside Load's harness default says why). Every
// other kind ignores cfg.Tend entirely: a start or resume dispatch has no
// tend: section to consult, and reading it anyway would make cfg.Tend.Model
// leak into a trigger dispatch the moment an operator set it.
//
// The label override wins over BOTH: tend: is a default for a whole CLASS of
// dispatch, and a label is an instruction about one issue, which must be able
// to override a class-wide default the same way it already overrides
// agent:'s loop-wide one. This is why the override layer stays last and the
// override re-validation below is unchanged -- it is still the last line of
// defence before a value becomes an argv element, whichever layer supplied
// the value it is now replacing.
//
// It returns a VALUE and never mutates cfg. Writing into cfg.Agent (or
// cfg.Tend) in place would leave every later reader -- the retry policy, the
// log paths -- holding a configuration that no longer matches the file it was
// loaded from.
//
// Each override value is RE-VALIDATED through config.ParseOverrides, and a
// value that fails is dropped rather than applied. This process did not parse
// the row it is reading: the tick did, in another process, possibly under an
// older binary, and internal/store/legacy.go writes the dispatches table by a
// second path. Effective is the last line of defence before a value becomes
// an argv element.
//
// It uses the PARSED, normalised value ParseOverrides returns, never the raw
// ov field: ParseOverrides lowercases harness and effort (overrides.go:88,
// 108), so a row carrying harness "PI" (an older binary, or a hand-edited
// database) must resolve to the lowercase form the harness switch in
// Supervise compares against -- otherwise it fails that comparison silently
// and launches claude with the pi model and claudeEnv.
//
// A harness override is never refused for the settings the other harness
// lacks. cfg.Agent.PermissionMode, MaxBudgetUSD and BackgroundTasks are
// claude-only; when the effective harness is pi, PiBuildArgs and claudeEnv
// never emit them, so the override IGNORES them rather than dropping a
// guard the harness could have honoured.
func Effective(cfg *config.Config, kind string, ov config.Overrides) Settings {
	s := Settings{
		Harness: cfg.Agent.Harness,
		Model:   cfg.Agent.Model,
		Effort:  cfg.Agent.Effort,
	}

	if kind == store.KindTend {
		if cfg.Tend.Harness != "" {
			s.Harness = cfg.Tend.Harness
		}
		if cfg.Tend.Model != "" {
			s.Model = cfg.Tend.Model
		}
		if cfg.Tend.Effort != "" {
			s.Effort = cfg.Tend.Effort
		}
	}

	var parsed config.Overrides
	if ov.Model != "" {
		if p, err := config.ParseOverrides([]string{config.OverrideModelPrefix + ov.Model}); err == nil {
			parsed.Model = p.Model
		}
	}
	if ov.Harness != "" {
		if p, err := config.ParseOverrides([]string{config.OverrideHarnessPrefix + ov.Harness}); err == nil {
			parsed.Harness = p.Harness
		}
	}
	if ov.Effort != "" {
		if p, err := config.ParseOverrides([]string{config.OverrideEffortPrefix + ov.Effort}); err == nil {
			parsed.Effort = p.Effort
		}
	}

	if parsed.Model != "" {
		s.Model = parsed.Model
	}
	if parsed.Harness != "" {
		s.Harness = parsed.Harness
	}
	if parsed.Effort != "" {
		s.Effort = parsed.Effort
	}
	return s
}

// BuildArgs returns the argument list for claude.
//
// The stream-json output format is mandatory, because it gives the log file and
// the machine readable result in one stream. Running the binary confirmed that
// --print with --output-format stream-json is rejected unless --verbose is also
// present, so --verbose is not optional here.
func BuildArgs(cfg *config.Config, inv Invocation) []string {
	s := Effective(cfg, inv.Kind, inv.Overrides)

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

	if s.Model != "" {
		args = append(args, "--model", s.Model)
	}
	if s.Effort != "" {
		args = append(args, "--effort", s.Effort)
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
	s := Effective(cfg, inv.Kind, inv.Overrides)

	args := []string{
		"-p",
		"--mode", "json",
		"--session-id", inv.SessionID,
		"--model", s.Model,
	}
	if s.Effort != "" {
		args = append(args, "--thinking", s.Effort)
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
