// Package config loads and validates a loop configuration file.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// Config is one loop definition.
type Config struct {
	Name            string `yaml:"name"`
	Repo            string `yaml:"repo"`
	CheckoutBaseDir string `yaml:"checkout_base_dir"`
	WorktreeDir     string `yaml:"worktree_dir"`
	StateDir        string `yaml:"state_dir"`

	// DefaultBranch is the branch new worktrees start from. It is not always
	// "master", so it is configuration rather than an assumption.
	DefaultBranch string `yaml:"default_branch"`

	Labels Labels `yaml:"labels"`
	Agent  Agent  `yaml:"agent"`
	TendPR bool   `yaml:"tend_pr"`

	// CleanupClosedPR removes the worktrees of a closed pull request. It
	// defaults to ON: nothing else removes a worktree, and one of a large
	// repository is easily hundreds of megabytes, so a loop that never
	// cleaned up would fill the disk. It exists because the action is
	// DESTRUCTIVE and webhook-triggered, and an operator who wants it off
	// should not have to rebuild to get there.
	//
	// A pointer for the tri-state. Absent must mean ON, and a plain bool
	// cannot tell an absent field from an explicit false.
	CleanupClosedPR *bool `yaml:"cleanup_closed_pr"`

	Retry Retry `yaml:"retry"`

	// AcknowledgeBypassPermissions must be true to select the
	// bypassPermissions agent permission mode. See validate.
	AcknowledgeBypassPermissions bool `yaml:"i_understand_bypass_permissions"`

	Prompt       string `yaml:"prompt"`
	ResumePrompt string `yaml:"resume_prompt"`
	TendPrompt   string `yaml:"tend_prompt"`
}

// Labels holds the five label roles and the veto list.
type Labels struct {
	Trigger  string   `yaml:"trigger"`
	InFlight string   `yaml:"in_flight"`
	Blocked  string   `yaml:"blocked"`
	Review   string   `yaml:"review"`
	Terminal string   `yaml:"terminal"`
	Veto     []string `yaml:"veto"`
}

// Agent holds the agent invocation settings.
type Agent struct {
	Harness        string   `yaml:"harness"`
	Model          string   `yaml:"model"`
	Effort         string   `yaml:"effort"`
	PermissionMode string   `yaml:"permission_mode"`
	Worktree       string   `yaml:"worktree"`
	MaxBudgetUSD   float64  `yaml:"max_budget_usd"`
	Timeout        Duration `yaml:"timeout"`

	// BackgroundTasks re-enables claude's background tasks, which this
	// program disables by default. Claude backgrounds a subagent unless told
	// otherwise, and "claude -p" waits only a bounded time for background
	// work before killing it and exiting ZERO. A dispatch whose agent fanned
	// out to subagents was therefore recorded as succeeded with its work
	// abandoned mid-flight, and the loop retired the issue. Disabled, a
	// subagent is an ordinary blocking tool call: fan-out within one turn
	// still runs concurrently, but no turn can end with work outstanding.
	//
	// A pointer for the tri-state. Absent must mean disabled, and a plain
	// bool cannot distinguish an absent field from an explicit false.
	// claude-only: pi has no equivalent.
	BackgroundTasks *bool `yaml:"background_tasks"`
}

// BackgroundTasksEnabled reports whether the claude child may background its
// subagents and shells. Absent means disabled; see Agent.BackgroundTasks.
func (a Agent) BackgroundTasksEnabled() bool {
	return a.BackgroundTasks != nil && *a.BackgroundTasks
}

// Retry holds the failure policy.
type Retry struct {
	Max int `yaml:"max"`

	// Backoff is how long to wait before each retry, one entry per retry.
	// Entry 0 is the wait before the first retry. retry.max: 0 means never
	// retry, so this may legitimately be empty; nothing may index it
	// without a length check.
	Backoff []Duration `yaml:"backoff"`

	// BackoffTicks is a rejection shim, not a live field. A tick used to be
	// a fixed interval under cron, but the webhook daemon can tick a loop
	// at any moment, so a tick count no longer names a stable wait. validate
	// rejects a non-empty value with a message that names the replacement,
	// rather than letting KnownFields(true) reject it with a bare "field
	// backoff_ticks not found" that does not tell the operator what to
	// write instead.
	BackoffTicks []int   `yaml:"backoff_ticks"`
	Breaker      Breaker `yaml:"breaker"`
}

// Breaker holds the cross-issue circuit breaker policy.
type Breaker struct {
	OrphanThreshold int      `yaml:"orphan_threshold"`
	Cooldown        Duration `yaml:"cooldown"`
}

// Worktree modes.
const (
	WorktreePerIssue = "per_issue"
	WorktreeNone     = "none"
)

// Harness names.
const (
	HarnessClaude = "claude"
	HarnessPi     = "pi"
)

// CleanupClosedPREnabled reports whether a closed pull request's worktrees are
// removed. Absent means enabled; see Config.CleanupClosedPR.
func (c *Config) CleanupClosedPREnabled() bool {
	return c.CleanupClosedPR == nil || *c.CleanupClosedPR
}

// RepoOwner returns the owner part of repo.
func (c *Config) RepoOwner() string {
	owner, _, _ := strings.Cut(c.Repo, "/")
	return owner
}

// RepoName returns the name part of repo.
func (c *Config) RepoName() string {
	_, name, _ := strings.Cut(c.Repo, "/")
	return name
}

// Load reads and validates a loop configuration file.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Agent.Harness == "" {
		cfg.Agent.Harness = HarnessClaude
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	var errs []error

	// A slice, not a map: Go randomises map iteration, so a map would print the
	// same bad config's errors in a different order on every run.
	required := []struct{ field, value string }{
		{"name", c.Name},
		{"repo", c.Repo},
		{"checkout_base_dir", c.CheckoutBaseDir},
		{"worktree_dir", c.WorktreeDir},
		{"default_branch", c.DefaultBranch},
		{"prompt", c.Prompt},
		{"resume_prompt", c.ResumePrompt},
	}
	for _, r := range required {
		if strings.TrimSpace(r.value) == "" {
			errs = append(errs, fmt.Errorf("%s is required", r.field))
		}
	}

	owner, name, ok := strings.Cut(c.Repo, "/")
	if !ok || owner == "" || name == "" {
		errs = append(errs, fmt.Errorf("repo must be in owner/name form, got %q", c.Repo))
	}

	// labels.terminal is deliberately NOT required. The planning loop has a
	// terminal label (the human's approval); the execution loop has none,
	// because an issue leaves it when its pull request merges. Requiring it
	// would force an operator to invent a value that changes no behavior.
	roles := []struct{ field, value string }{
		{"labels.trigger", c.Labels.Trigger},
		{"labels.in_flight", c.Labels.InFlight},
		{"labels.blocked", c.Labels.Blocked},
		{"labels.review", c.Labels.Review},
	}
	for _, r := range roles {
		if strings.TrimSpace(r.value) == "" {
			errs = append(errs, fmt.Errorf("%s is required", r.field))
		}
	}

	switch c.Agent.Harness {
	case "", HarnessClaude:
	case HarnessPi:
		if c.Agent.PermissionMode != "" {
			errs = append(errs, errors.New(
				"agent.permission_mode is claude-only; remove it for harness: pi"))
		}
		if c.Agent.BackgroundTasks != nil {
			errs = append(errs, errors.New(
				"agent.background_tasks is claude-only; remove it for harness: pi"))
		}
	default:
		errs = append(errs, fmt.Errorf(
			"agent.harness must be %q or %q, got %q",
			HarnessClaude, HarnessPi, c.Agent.Harness))
	}

	if c.Agent.Harness != HarnessPi {
		switch c.Agent.PermissionMode {
		case "", "acceptEdits", "auto", "manual", "dontAsk", "plan":
		case "bypassPermissions":
			// bypassPermissions disables every permission prompt. The agent reads
			// issue and comment text written by third parties, so an injected
			// instruction executes with no gate. Require the operator to say so.
			if !c.AcknowledgeBypassPermissions {
				errs = append(errs, errors.New(
					"agent.permission_mode is \"bypassPermissions\", which disables every "+
						"permission prompt on third-party issue text; set "+
						"i_understand_bypass_permissions: true to confirm"))
			}
		default:
			errs = append(errs, fmt.Errorf(
				"agent.permission_mode %q is not a valid claude permission mode",
				c.Agent.PermissionMode))
		}
	}

	switch c.Agent.Worktree {
	case WorktreePerIssue, WorktreeNone:
	case "":
		errs = append(errs, errors.New("agent.worktree is required"))
	default:
		errs = append(errs, fmt.Errorf("agent.worktree must be %q or %q, got %q",
			WorktreePerIssue, WorktreeNone, c.Agent.Worktree))
	}

	if c.Agent.Model == "" {
		errs = append(errs, errors.New("agent.model is required"))
	}
	switch c.Agent.Effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		errs = append(errs, fmt.Errorf(
			"agent.effort %q is not a valid effort level", c.Agent.Effort))
	}
	// 0 is legitimate and documented: it means no cost ceiling, and
	// internal/runner/args.go omits --max-budget-usd for it. A NEGATIVE value
	// hits that same "> 0" gate, so it is silently identical to no cap -- an
	// operator who typed "-25" meaning "25" would have got an uncapped
	// dispatch and no warning. Only the negative case is an error.
	if c.Agent.MaxBudgetUSD < 0 {
		errs = append(errs, fmt.Errorf(
			"agent.max_budget_usd must not be negative, got %v; use 0 for no limit",
			c.Agent.MaxBudgetUSD))
	}
	if c.Agent.Timeout.Std() <= 0 {
		errs = append(errs, errors.New("agent.timeout must be greater than zero"))
	}

	if c.Retry.Max < 0 {
		errs = append(errs, errors.New("retry.max must not be negative"))
	}
	if len(c.Retry.BackoffTicks) > 0 {
		// A tick was a fixed interval only under cron. The webhook daemon
		// can tick a loop at any moment, so a tick count no longer names a
		// stable wait; give the operator the replacement rather than a bare
		// "unknown field" once the field is removed from the struct.
		errs = append(errs, errors.New(
			"retry.backoff_ticks is no longer supported; a tick is no longer a fixed\n"+
				"interval, because a webhook can tick a loop at any moment. Replace it with\n"+
				"retry.backoff, a list of durations:\n\n"+
				"  backoff: [0s, 15m, 30m]"))
	}
	if len(c.Retry.Backoff) < c.Retry.Max {
		errs = append(errs, fmt.Errorf(
			"retry.backoff has %d entries but retry.max is %d; it needs one entry per retry",
			len(c.Retry.Backoff), c.Retry.Max))
	}
	if c.Retry.Breaker.OrphanThreshold < 1 {
		errs = append(errs, errors.New("retry.breaker.orphan_threshold must be at least 1"))
	}
	if c.Retry.Breaker.Cooldown.Std() <= 0 {
		errs = append(errs, errors.New("retry.breaker.cooldown must be greater than zero"))
	}

	if c.TendPR && strings.TrimSpace(c.TendPrompt) == "" {
		errs = append(errs, errors.New("tend_prompt is required when tend_pr is true"))
	}

	// Parse every template at load time. A typo such as {{.Issue.Titel}} would
	// otherwise surface only inside a detached runner, where it recorded a failed
	// dispatch and redispatched on the next tick.
	for name, tmpl := range map[string]string{
		"prompt": c.Prompt, "resume_prompt": c.ResumePrompt, "tend_prompt": c.TendPrompt,
	} {
		if strings.TrimSpace(tmpl) == "" {
			continue
		}
		if _, err := template.New(name).Parse(tmpl); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}

	return errors.Join(errs...)
}
