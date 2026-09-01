package wizard

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
)

// worktreeDirDefault is the default answer to question 4. It is relative, so
// config.ResolveWorkDirs resolves it against the project root, giving every
// project its own worktrees under its own .agent-utils directory.
const worktreeDirDefault = config.DirName + "/worktrees"

// Run asks every question and returns a configuration that config.validate
// accepts.
//
// Question 24 (which template to start from) is asked FIRST, even though it
// is last in the table, so its Labels/TendPR values can be offered as the
// defaults for questions 7-12 and 19. Everything else follows the table's
// order.
func Run(p Prompter, d Detected) (*config.Config, error) {
	tmplName, err := p.Ask(Question{
		Key:     "template",
		Label:   "Start from which template?",
		Help:    "Supplies the label and tend_pr defaults below, and the three prompt bodies.",
		Default: "planning",
		Choices: templateNames,
	})
	if err != nil {
		return nil, err
	}
	tmpl, ok := TemplateNamed(tmplName)
	if !ok {
		// Choices restricts what Ask can return to a known template name, so
		// this is unreachable; treated as a defect rather than swallowed.
		return nil, fmt.Errorf("wizard: unknown template %q", tmplName)
	}

	cfg := &config.Config{
		Prompt:       tmpl.Prompt,
		ResumePrompt: tmpl.ResumePrompt,
		TendPrompt:   tmpl.TendPrompt,
	}

	// 1. name
	//
	// Defaulted from the template just chosen, like the label and prompt
	// answers below: someone starting from the execution template is far more
	// likely to want a loop called "execution" than one called "planning".
	cfg.Name, err = p.Ask(Question{
		Key:      "name",
		Label:    "Loop name",
		Help:     "Unique in this project. Keys the loop's state, its lock file, and its state directory.",
		Default:  tmpl.Name,
		Validate: validateName,
	})
	if err != nil {
		return nil, err
	}

	// 2. repo
	cfg.Repo, err = p.Ask(Question{
		Key:      "repo",
		Label:    "Repository",
		Help:     "owner/name: the GitHub repository this loop watches.",
		Default:  d.Repo,
		Validate: validateRepo,
	})
	if err != nil {
		return nil, err
	}

	// 3. checkout_base_dir
	//
	// "." rather than Detected.CheckoutBaseDir, which is git's absolute path
	// to this machine's checkout. A relative value is resolved against the
	// project root (config.ResolveWorkDirs), so "." is the project itself and
	// the generated file survives being cloned to another machine or another
	// path. The detected absolute path would have baked one layout in.
	cfg.CheckoutBaseDir, err = p.Ask(Question{
		Key:   "checkout_base_dir",
		Label: "Checkout base directory",
		Help: "The work tree root this loop's per-issue worktrees branch from. " +
			"A relative path is resolved against the project root, so \".\" is the project itself.",
		Default: ".",
	})
	if err != nil {
		return nil, err
	}

	// 4. worktree_dir
	//
	// Relative, like question 3, and resolved against the project root the
	// same way: the worktrees belong to the project they are checked out
	// from, not to whoever's home directory the wizard happened to run under.
	// Keeping them under .agent-utils puts them beside the project's other
	// generated state instead of loose in the repository.
	cfg.WorktreeDir, err = p.Ask(Question{
		Key:   "worktree_dir",
		Label: "Worktree directory",
		Help: "Where this loop's per-issue worktrees are checked out. " +
			"A relative path is resolved against the project root.",
		Default: worktreeDirDefault,
	})
	if err != nil {
		return nil, err
	}

	// 5. state_dir
	cfg.StateDir, err = p.Ask(Question{
		Key:      "state_dir",
		Label:    "State directory override",
		Help:     "Leave empty to use the derived default: <project>/.agent-utils/state/<name>.",
		Optional: true,
	})
	if err != nil {
		return nil, err
	}

	// 6. default_branch
	cfg.DefaultBranch, err = p.Ask(Question{
		Key:     "default_branch",
		Label:   "Default branch",
		Help:    "New worktrees are checked out detached from this branch.",
		Default: d.DefaultBranch,
	})
	if err != nil {
		return nil, err
	}

	// 7-11. labels, defaulted from the chosen template now that it is known.
	cfg.Labels.Trigger, err = p.Ask(Question{
		Key: "labels.trigger", Label: "Trigger label",
		Help: "A human applies this to start the loop on an issue.", Default: tmpl.Labels.Trigger,
	})
	if err != nil {
		return nil, err
	}
	cfg.Labels.InFlight, err = p.Ask(Question{
		Key: "labels.in_flight", Label: "In-flight label",
		Help: "Applied while this loop's agent is running on the issue.", Default: tmpl.Labels.InFlight,
	})
	if err != nil {
		return nil, err
	}
	cfg.Labels.Blocked, err = p.Ask(Question{
		Key: "labels.blocked", Label: "Blocked label",
		Help: "Applied when the loop parks waiting on a human.", Default: tmpl.Labels.Blocked,
	})
	if err != nil {
		return nil, err
	}
	cfg.Labels.Terminal, err = p.Ask(Question{
		Key: "labels.terminal", Label: "Terminal label",
		Help:     "Applied only by a human, to approve. Leave empty if this loop has no terminal label (the execution loop has none: an issue leaves it when its pull request merges).",
		Default:  tmpl.Labels.Terminal,
		Optional: true,
	})
	if err != nil {
		return nil, err
	}

	// 12. labels.veto
	vetoAnswer, err := p.Ask(Question{
		Key:     "labels.veto",
		Label:   "Veto labels",
		Help:    "Comma-separated. Any label in this list stops the loop from acting on the issue.",
		Default: strings.Join(tmpl.Labels.Veto, ", "),
		List:    true,
	})
	if err != nil {
		return nil, err
	}
	cfg.Labels.Veto = splitList(vetoAnswer)

	// 13. agent.harness
	harness, err := p.Ask(Question{
		Key:     "agent.harness",
		Label:   "Agent harness",
		Help:    "claude runs claude -p. pi runs pi -p; choose pi for a model claude does not offer.",
		Default: config.HarnessClaude,
		Choices: []string{config.HarnessClaude, config.HarnessPi},
	})
	if err != nil {
		return nil, err
	}
	cfg.Agent.Harness = harness
	pi := harness == config.HarnessPi

	modelHelp := "Passed to claude as the model name."
	if pi {
		modelHelp = "A pi model id pi resolves, like anthropic/claude-sonnet-4-5."
	}

	// 14. agent.model
	cfg.Agent.Model, err = p.Ask(Question{
		Key: "agent.model", Label: "Agent model",
		Help: modelHelp, Default: "opus",
	})
	if err != nil {
		return nil, err
	}

	// 15. agent.effort
	cfg.Agent.Effort, err = p.Ask(Question{
		Key: "agent.effort", Label: "Agent effort",
		Help: "How hard the agent thinks before acting.", Default: "high",
		Choices: []string{"low", "medium", "high", "xhigh", "max"},
	})
	if err != nil {
		return nil, err
	}

	// 16. agent.permission_mode, gated by a separate confirmation for
	// bypassPermissions. A pi harness has none and skips the question.
	if pi {
		cfg.Agent.PermissionMode = ""
	} else {
		for {
			mode, err := p.Ask(Question{
				Key:     "agent.permission_mode",
				Label:   "Agent permission mode",
				Help:    "acceptEdits prompts for anything beyond file edits. bypassPermissions disables every prompt; choosing it asks a separate confirmation.",
				Default: "acceptEdits",
				Choices: []string{"acceptEdits", "auto", "manual", "dontAsk", "plan", "bypassPermissions"},
			})
			if err != nil {
				return nil, err
			}
			if mode != "bypassPermissions" {
				cfg.Agent.PermissionMode = mode
				cfg.AcknowledgeBypassPermissions = false
				break
			}

			// bypassPermissions disables every permission prompt on text the
			// agent reads from the issue and its comments, which third parties
			// write. An instruction hidden in a comment then executes with no
			// gate — the same reasoning config.validate gives when the
			// acknowledgement is missing. The confirmation defaults to No, and
			// declining returns to question 16 rather than aborting the wizard.
			confirmed, err := p.Confirm(
				"Confirm bypassPermissions",
				"This disables every permission prompt on third-party issue text; an instruction hidden in an issue comment executes.",
				false,
			)
			if err != nil {
				return nil, err
			}
			if confirmed {
				cfg.Agent.PermissionMode = mode
				cfg.AcknowledgeBypassPermissions = true
				break
			}
			// Declined: loop back and re-ask question 16, not the whole wizard.
		}
	}

	// 17. agent.worktree
	cfg.Agent.Worktree, err = p.Ask(Question{
		Key: "agent.worktree", Label: "Agent worktree mode",
		Help: "per_issue isolates each issue's dispatch in its own worktree.", Default: config.WorktreePerIssue,
		Choices: []string{config.WorktreePerIssue, config.WorktreeNone},
	})
	if err != nil {
		return nil, err
	}

	// 18. agent.max_budget_usd
	if pi {
		// pi exposes no cost-ceiling flag; the budget is a no-op.
		cfg.Agent.MaxBudgetUSD = 0
	} else {
		budgetAnswer, err := p.Ask(Question{
			Key: "agent.max_budget_usd", Label: "Agent max budget (USD)",
			Help: "Dispatch stops if the agent's session cost exceeds this; 0 means no limit, which is the recommended value.", Default: "0",
			Validate: validateNonNegativeFloat,
		})
		if err != nil {
			return nil, err
		}
		cfg.Agent.MaxBudgetUSD, err = strconv.ParseFloat(budgetAnswer, 64)
		if err != nil {
			return nil, fmt.Errorf("agent.max_budget_usd: %w", err)
		}
	}

	// 19. agent.timeout
	timeoutAnswer, err := p.Ask(Question{
		Key: "agent.timeout", Label: "Agent timeout",
		Help: "Last-resort bound on a wedged dispatch, not a budget; a stuck run is caught by the orphan breaker. Guessing low makes real long runs look flaky.", Default: "24h",
		Validate: validatePositiveDuration,
	})
	if err != nil {
		return nil, err
	}
	cfg.Agent.Timeout, err = parseDuration(timeoutAnswer)
	if err != nil {
		return nil, fmt.Errorf("agent.timeout: %w", err)
	}

	// Tending is NOT asked here any more. It is a project-level policy in
	// .agent-utils/config.yaml -- which pull requests to keep fresh, and which
	// loop hosts the dispatches -- because it describes a repository's pull
	// requests rather than one loop's issue lifecycle. Asking it per loop is how
	// two loops used to end up rebasing one branch.

	// 21. retry.max
	maxAnswer, err := p.Ask(Question{
		Key: "retry.max", Label: "Retry max",
		Help: "How many times a failed dispatch is retried before it is parked.", Default: "3",
		Validate: validateNonNegativeInt,
	})
	if err != nil {
		return nil, err
	}
	cfg.Retry.Max, err = strconv.Atoi(maxAnswer)
	if err != nil {
		return nil, fmt.Errorf("retry.max: %w", err)
	}

	// 22. retry.backoff
	backoffAnswer, err := p.Ask(Question{
		Key:   "retry.backoff",
		Label: "Retry backoff",
		Help: fmt.Sprintf(
			"Comma-separated durations, one per retry; needs at least %d entries because retry.max is %d.",
			cfg.Retry.Max, cfg.Retry.Max),
		Default:  "0s, 15m, 30m",
		List:     true,
		Validate: validateBackoff(cfg.Retry.Max),
	})
	if err != nil {
		return nil, err
	}
	cfg.Retry.Backoff, err = parseDurations(splitList(backoffAnswer))
	if err != nil {
		return nil, fmt.Errorf("retry.backoff: %w", err)
	}

	// 23. retry.breaker.orphan_threshold
	orphanAnswer, err := p.Ask(Question{
		Key: "retry.breaker.orphan_threshold", Label: "Retry breaker orphan threshold",
		Help: "Trips the breaker when this many issues need a retry in the same tick.", Default: "2",
		Validate: validatePositiveInt,
	})
	if err != nil {
		return nil, err
	}
	cfg.Retry.Breaker.OrphanThreshold, err = strconv.Atoi(orphanAnswer)
	if err != nil {
		return nil, fmt.Errorf("retry.breaker.orphan_threshold: %w", err)
	}

	// 24. retry.breaker.cooldown
	cooldownAnswer, err := p.Ask(Question{
		Key: "retry.breaker.cooldown", Label: "Retry breaker cooldown",
		Help: "How long the breaker stays tripped once it trips.", Default: "30m",
		Validate: validatePositiveDuration,
	})
	if err != nil {
		return nil, err
	}
	cfg.Retry.Breaker.Cooldown, err = parseDuration(cooldownAnswer)
	if err != nil {
		return nil, fmt.Errorf("retry.breaker.cooldown: %w", err)
	}

	return cfg, nil
}

func validateRepo(s string) error {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" {
		return fmt.Errorf("repo must be in owner/name form, got %q", s)
	}
	return nil
}

// validateNonNegativeFloat is used for agent.max_budget_usd. 0 is accepted
// and means no cap -- internal/runner/args.go omits --max-budget-usd for it. A
// negative value reaches that same "> 0" gate, so it would be silently
// identical to 0: an operator who typed "-25" meaning "25" would have run
// uncapped. config.validate rejects it too, for a hand-edited file; catching
// it here means the wizard says so instead of failing at Write's reload,
// after all 24 answers have been given.
func validateNonNegativeFloat(s string) error {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("%q is not a number", s)
	}
	if f < 0 {
		return fmt.Errorf("%q must not be negative; use 0 for no limit", s)
	}
	return nil
}

// validName matches internal/project.Slug's own constraint on a project name:
// the value becomes part of a file name (<dir>/configs/<name>.yaml, per
// Write) and a lock file name, so it stays to characters that need no
// quoting and cannot walk the path outside configs/ via "/" or "..".
var validName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func validateName(s string) error {
	if !validName.MatchString(s) {
		return fmt.Errorf(
			"%q is not a valid loop name; it becomes part of a file path, so use only letters, digits, '.', '_', and '-'", s)
	}
	return nil
}

// validatePositiveDuration is used for agent.timeout and
// retry.breaker.cooldown, which config.validate requires to be greater than
// zero. Rejecting "0s" and a negative value here means the wizard's own
// Write reload catches the same defect config.Load would, before the
// operator has spent all 24 answers.
func validatePositiveDuration(s string) error {
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is not a valid duration such as \"30m\"", s)
	}
	if d <= 0 {
		return fmt.Errorf("%q must be greater than zero", s)
	}
	return nil
}

func validateNonNegativeInt(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return fmt.Errorf("%q is not a non-negative whole number", s)
	}
	return nil
}

func validatePositiveInt(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return fmt.Errorf("%q is not a whole number of at least 1", s)
	}
	return nil
}

// validateBackoff closes over retry.max, already answered by the time
// question 21 is asked, so a backoff list shorter than it can be rejected
// before Write ever hands the file to config.Load.
func validateBackoff(max int) func(string) error {
	return func(s string) error {
		entries := splitList(s)
		if len(entries) < max {
			return fmt.Errorf(
				"retry.backoff has %d entries but retry.max is %d; it needs one entry per retry",
				len(entries), max)
		}
		for _, e := range entries {
			if _, err := time.ParseDuration(e); err != nil {
				return fmt.Errorf("%q is not a valid duration such as \"30m\"", e)
			}
		}
		return nil
	}
}

// splitList turns a comma-separated answer into a trimmed, non-empty slice.
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseDuration(s string) (config.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	return config.Duration(d), nil
}

func parseDurations(entries []string) ([]config.Duration, error) {
	out := make([]config.Duration, 0, len(entries))
	for _, e := range entries {
		d, err := parseDuration(e)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}
