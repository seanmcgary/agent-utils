// Package config loads and validates a loop configuration file.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/seanmcgary/agent-utils/internal/project"
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

	// Tend is the PROJECT's pull-request maintenance policy, and it is set on
	// exactly one Config: the synthetic one LoadTend builds for the tend
	// dispatcher. Load NEVER sets it, so on every configuration that came from
	// a loop file it is the zero value and tending is off.
	//
	// `yaml:"-"` is load-bearing: the strict decoder rejects a `tend:` key in a
	// loop file, so an operator who writes the policy in the wrong file is told
	// rather than ignored. This field is the only tend-shaped thing left on
	// this type, and it is here because the tend dispatcher reuses Config
	// wholesale -- one worktree manager, one dispatch path, one runner -- with
	// its identity, its agent and its prompt filled in from the project
	// descriptor instead of from a file.
	Tend project.Tend `yaml:"-"`

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

	// Prompt and ResumePrompt are the only prompts a loop file carries. There
	// used to be a third, tend_prompt, and it moved to the project descriptor
	// with the rest of the tend policy: a loop that does not tend -- which is
	// now every loop -- has no business carrying the instructions for a
	// dispatch it never makes. LoadTend puts the descriptor's tend.prompt in
	// Prompt on the synthetic tend configuration, so the runner renders one
	// field rather than choosing between two.
	Prompt       string `yaml:"prompt"`
	ResumePrompt string `yaml:"resume_prompt"`
}

// Labels holds the five label roles and the veto list.
type Labels struct {
	Trigger  string   `yaml:"trigger"`
	InFlight string   `yaml:"in_flight"`
	Blocked  string   `yaml:"blocked"`
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

// DefaultAgentTimeout bounds a dispatch when agent.timeout is omitted.
//
// It is long on purpose. This deadline is not a budget and it is not a hang
// detector -- the orphan breaker and `loop kill` handle a stuck dispatch on
// evidence. It is the last resort that stops a wedged process living forever,
// and the only cost of setting it high is how long that one case takes to
// clear. The cost of setting it low is paid by every honest long run: a
// dispatch killed at its deadline is recorded FAILED and retried from a resumed
// session, so an agent doing real work on a large branch looks like a flaky
// agent instead of like a number that was too small.
const DefaultAgentTimeout = 24 * time.Hour

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
	// The project descriptor's tend.harness is deliberately NOT defaulted, in
	// project.Load or here. agent.harness must always resolve, because every
	// dispatch needs an agent to run, but an empty tend.harness carries its own
	// meaning: "use agent.harness for this tend too." Defaulting it to
	// HarnessClaude would silently pin every tend dispatch to claude on a loop
	// configured with harness: pi, which is exactly the divergence a tend policy
	// that only sets a cheaper model was never meant to introduce.
	// runner.Effective and engine.EffectiveHarness both read the empty string as
	// "fall through to agent.harness."

	// An omitted timeout means DefaultAgentTimeout, not an error.
	//
	// This field used to be required, and requiring it made every operator
	// invent a number for the one setting they have no basis to choose. The
	// number they invent is always too small, because the cost of guessing low
	// is invisible: a dispatch killed at its deadline is recorded failed and
	// retried from a resumed session, so a too-short timeout looks like a flaky
	// agent rather than like a misconfiguration. A long default is the safe
	// direction -- a dispatch that genuinely hangs is caught by the orphan
	// breaker and by `loop kill`, both of which act on evidence rather than on
	// a clock.
	//
	// Zero is the only value this can key on, and that is why validate() still
	// rejects a NEGATIVE duration below: "unset" and "explicitly zero" are the
	// same thing in YAML, and neither can mean "no deadline at all", since
	// agent.timeout is the only bound on a dispatch once
	// CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS is zeroed.
	if cfg.Agent.Timeout.Std() == 0 {
		cfg.Agent.Timeout = Duration(DefaultAgentTimeout)
	}
	// The tend dispatcher's name is not available to a LOOP FILE, and the check
	// lives here rather than in validate() precisely because that is the scope
	// of the rule: validate() is shared with LoadTend, whose whole job is to
	// build the configuration that legitimately holds this name.
	//
	// Every row this program writes is keyed by (project, loop), so a loop that
	// took the name would share the dispatch rows, the pull request links, the
	// tend_conflicts, the tick counter, the lock file and the worktree tree
	// with the project's tending -- and both would write them. The message
	// names the reason rather than just the rule: "reserved" alone reads as
	// bureaucracy, and an operator who does not know what they collided with
	// will rename the loop and wonder what they lost.
	if strings.EqualFold(strings.TrimSpace(cfg.Name), project.Reserved) {
		return nil, fmt.Errorf(
			"invalid config %s: name %q is reserved for this project's tend dispatcher, "+
				"which keeps its dispatches, pull request links, lock file and worktrees "+
				"under that name; a loop sharing it would read and write the same rows. "+
				"Rename this loop",
			path, project.Reserved)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	// Nothing reads the project descriptor here any more. A loop file used to
	// have the project's tend policy injected into it, because the tend work
	// ran inside this loop's ticks; it does not, so a loop no longer needs to
	// know the policy exists. LoadTend is the only reader now.
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

	// Every loop has the same three states of its own: queued, working, stuck.
	// labels.terminal is the fourth and is optional only because a loop that
	// nothing follows has nowhere to hand on to.
	//
	// There used to be a fifth, labels.review, meaning "the agent finished and
	// its output is waiting for a human to read". It is gone, and its removal is
	// the point rather than a tidy-up: it was the only label whose meaning
	// depended on what came AFTER the loop, so it forced every loop to describe
	// its neighbour. A loop now declares only its own states and ends the same
	// way -- apply the terminal, stop -- and whether that terminal is read by a
	// human or by the next loop is a fact about the label an operator chose, not
	// something either loop knows.
	//
	// Two things had been keeping it alive, and both moved to the project
	// descriptor, where a cross-loop concern belongs: tend eligibility is now
	// tend.label, and the epic sweep's entry loop is now epic.loop.
	roles := []struct{ field, value string }{
		{"labels.trigger", c.Labels.Trigger},
		{"labels.in_flight", c.Labels.InFlight},
		{"labels.blocked", c.Labels.Blocked},
	}
	for _, r := range roles {
		if strings.TrimSpace(r.value) == "" {
			errs = append(errs, fmt.Errorf("%s is required", r.field))
		}
	}

	switch c.Agent.Harness {
	case "", HarnessClaude, HarnessPi:
	default:
		errs = append(errs, fmt.Errorf(
			"agent.harness must be %q or %q, got %q",
			HarnessClaude, HarnessPi, c.Agent.Harness))
	}

	// The claude-only settings -- agent.permission_mode,
	// agent.background_tasks, agent.max_budget_usd -- are ACCEPTED whatever
	// the harness is, and IGNORED by the harness that has no equivalent: pi
	// has no permission model, no background-task switch and no cost
	// ceiling, so PiBuildArgs and claudeEnv simply never emit them. Refusing
	// them for harness: pi would also make a config unusable with a
	// per-issue harness: label, which can flip either harness to the other
	// for one issue.
	//
	// The VALUE is still validated whatever the harness is: a harness:claude
	// label on a pi loop makes the value take effect, so the enum and the
	// bypassPermissions acknowledgement must hold for every configuration
	// carrying the field.
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
	// Nothing validates cfg.Tend here. A loop file cannot set it -- the field
	// is `yaml:"-"` and the strict decoder rejects a `tend:` key -- so the only
	// Config that ever carries one is LoadTend's, and LoadTend validates it
	// where it is read, from the descriptor that declared it.

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
	// Load defaults an omitted or zero timeout to DefaultAgentTimeout before
	// this runs, so only a negative value can reach here -- and a negative
	// duration would sort before every deadline and kill the dispatch at once.
	if c.Agent.Timeout.Std() < 0 {
		errs = append(errs, fmt.Errorf(
			"agent.timeout must not be negative, got %s; omit it for the default of %s",
			c.Agent.Timeout.Std(), DefaultAgentTimeout))
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

	// Parse every template at load time. A typo such as {{.Issue.Titel}} would
	// otherwise surface only inside a detached runner, where it recorded a failed
	// dispatch and redispatched on the next tick.
	for name, tmpl := range map[string]string{
		"prompt": c.Prompt, "resume_prompt": c.ResumePrompt,
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
