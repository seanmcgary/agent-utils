package wizard

import (
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/home"
)

// detected is a fully-populated Detected so every default that depends on it
// (repo, checkout_base_dir, default_branch) is itself valid, and an
// "accept every default" run never needs an answer beyond "".
func detected(t *testing.T) Detected {
	t.Helper()
	return Detected{
		Repo:            "acme/example",
		DefaultBranch:   "main",
		CheckoutBaseDir: t.TempDir(),
	}
}

// setHome points internal/home at a scratch directory so Run's worktree_dir
// default does not depend on, or write anything under, the real user's home.
func setHome(t *testing.T) {
	t.Helper()
	t.Setenv(home.EnvVar, t.TempDir())
}

func TestRunAcceptsEveryDefault(t *testing.T) {
	for _, tmplName := range templateNames {
		t.Run(tmplName, func(t *testing.T) {
			setHome(t)
			d := detected(t)

			answers := []string{
				tmplName, // 24. prompt template
				"",       // 1.  name
				"",       // 2.  repo
				"",       // 3.  checkout_base_dir
				"",       // 4.  worktree_dir
				"",       // 5.  state_dir
				"",       // 6.  default_branch
				"",       // 7.  labels.trigger
				"",       // 8.  labels.in_flight
				"",       // 9.  labels.blocked
				"",       // 10. labels.review
				"",       // 11. labels.terminal
				"",       // 12. labels.veto
				"",       // 13. agent.model
				"",       // 14. agent.effort
				"",       // 15. agent.permission_mode
				"",       // 16. agent.worktree
				"",       // 17. agent.max_budget_usd
				"",       // 18. agent.timeout
				"",       // 20. retry.max
				"",       // 21. retry.backoff
				"",       // 22. retry.breaker.orphan_threshold
				"",       // 23. retry.breaker.cooldown
			}
			p := &scriptPrompter{t: t, answers: answers}

			cfg, err := Run(p, d)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			dir := t.TempDir()
			path, err := Write(dir, cfg)
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if _, err := config.Load(path); err != nil {
				t.Fatalf("config.Load(%s): %v", path, err)
			}
		})
	}
}

func TestRunBypassPermissionsDeclineReasksThenAccepts(t *testing.T) {
	setHome(t)
	d := detected(t)

	answers := []string{
		"planning",          // 24. prompt template
		"",                  // 1.  name
		"",                  // 2.  repo
		"",                  // 3.  checkout_base_dir
		"",                  // 4.  worktree_dir
		"",                  // 5.  state_dir
		"",                  // 6.  default_branch
		"",                  // 7.  labels.trigger
		"",                  // 8.  labels.in_flight
		"",                  // 9.  labels.blocked
		"",                  // 10. labels.review
		"",                  // 11. labels.terminal
		"",                  // 12. labels.veto
		"",                  // 13. agent.model
		"",                  // 14. agent.effort
		"bypassPermissions", // 15. agent.permission_mode, attempt 1
		"bypassPermissions", // 15. agent.permission_mode, re-asked after decline
		"",                  // 16. agent.worktree
		"",                  // 17. agent.max_budget_usd
		"",                  // 18. agent.timeout
		"",                  // 20. retry.max
		"",                  // 21. retry.backoff
		"",                  // 22. retry.breaker.orphan_threshold
		"",                  // 23. retry.breaker.cooldown
	}
	// First bypassPermissions confirmation is declined, so question 15 must
	// be re-asked rather than aborting the wizard; the second is accepted.
	confirms := []bool{false, true}
	p := &scriptPrompter{t: t, answers: answers, confirms: confirms}

	cfg, err := Run(p, d)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.Agent.PermissionMode != "bypassPermissions" {
		t.Fatalf("Agent.PermissionMode = %q, want %q", cfg.Agent.PermissionMode, "bypassPermissions")
	}
	if !cfg.AcknowledgeBypassPermissions {
		t.Fatal("AcknowledgeBypassPermissions = false, want true after accepting the confirmation")
	}
}

func TestRunRetryBackoffFewerEntriesThanMaxReasks(t *testing.T) {
	setHome(t)
	d := detected(t)

	answers := []string{
		"planning",     // 24. prompt template
		"",             // 1.  name
		"",             // 2.  repo
		"",             // 3.  checkout_base_dir
		"",             // 4.  worktree_dir
		"",             // 5.  state_dir
		"",             // 6.  default_branch
		"",             // 7.  labels.trigger
		"",             // 8.  labels.in_flight
		"",             // 9.  labels.blocked
		"",             // 10. labels.review
		"",             // 11. labels.terminal
		"",             // 12. labels.veto
		"",             // 13. agent.model
		"",             // 14. agent.effort
		"",             // 15. agent.permission_mode
		"",             // 16. agent.worktree
		"",             // 17. agent.max_budget_usd
		"",             // 18. agent.timeout
		"3",            // 20. retry.max
		"0s, 15m",      // 21. retry.backoff, attempt 1: only 2 entries for max=3
		"0s, 15m, 30m", // 21. retry.backoff, re-asked: 3 entries
		"",             // 22. retry.breaker.orphan_threshold
		"",             // 23. retry.breaker.cooldown
	}
	p := &scriptPrompter{t: t, answers: answers}

	cfg, err := Run(p, d)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cfg.Retry.Backoff) != 3 {
		t.Fatalf("Retry.Backoff has %d entries, want 3", len(cfg.Retry.Backoff))
	}
}

func TestRunRepoNotOwnerSlashNameReasks(t *testing.T) {
	setHome(t)
	d := detected(t)

	answers := []string{
		"planning",     // 24. prompt template
		"",             // 1.  name
		"not-a-repo",   // 2.  repo, attempt 1: fails owner/name validation
		"acme/example", // 2.  repo, re-asked: valid
		"",             // 3.  checkout_base_dir
		"",             // 4.  worktree_dir
		"",             // 5.  state_dir
		"",             // 6.  default_branch
		"",             // 7.  labels.trigger
		"",             // 8.  labels.in_flight
		"",             // 9.  labels.blocked
		"",             // 10. labels.review
		"",             // 11. labels.terminal
		"",             // 12. labels.veto
		"",             // 13. agent.model
		"",             // 14. agent.effort
		"",             // 15. agent.permission_mode
		"",             // 16. agent.worktree
		"",             // 17. agent.max_budget_usd
		"",             // 18. agent.timeout
		"",             // 20. retry.max
		"",             // 21. retry.backoff
		"",             // 22. retry.breaker.orphan_threshold
		"",             // 23. retry.breaker.cooldown
	}
	p := &scriptPrompter{t: t, answers: answers}

	cfg, err := Run(p, d)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.Repo != "acme/example" {
		t.Fatalf("Repo = %q, want %q", cfg.Repo, "acme/example")
	}
}

func TestRunInvalidAgentTimeoutReasks(t *testing.T) {
	setHome(t)
	d := detected(t)

	answers := []string{
		"planning",     // 24. prompt template
		"",             // 1.  name
		"",             // 2.  repo
		"",             // 3.  checkout_base_dir
		"",             // 4.  worktree_dir
		"",             // 5.  state_dir
		"",             // 6.  default_branch
		"",             // 7.  labels.trigger
		"",             // 8.  labels.in_flight
		"",             // 9.  labels.blocked
		"",             // 10. labels.review
		"",             // 11. labels.terminal
		"",             // 12. labels.veto
		"",             // 13. agent.model
		"",             // 14. agent.effort
		"",             // 15. agent.permission_mode
		"",             // 16. agent.worktree
		"",             // 17. agent.max_budget_usd
		"notaduration", // 18. agent.timeout, attempt 1: not a valid duration
		"3h",           // 18. agent.timeout, re-asked: valid
		"",             // 20. retry.max
		"",             // 21. retry.backoff
		"",             // 22. retry.breaker.orphan_threshold
		"",             // 23. retry.breaker.cooldown
	}
	p := &scriptPrompter{t: t, answers: answers}

	cfg, err := Run(p, d)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.Agent.Timeout.String() != "3h0m0s" {
		t.Fatalf("Agent.Timeout = %v, want 3h", cfg.Agent.Timeout)
	}
}

func TestRunVetoListIsSplitFromTemplateDefault(t *testing.T) {
	setHome(t)
	d := detected(t)

	tmpl, ok := TemplateNamed("planning")
	if !ok {
		t.Fatal("TemplateNamed(planning) = _, false")
	}

	answers := []string{
		"planning", // 24. prompt template
		"",         // 1.  name
		"",         // 2.  repo
		"",         // 3.  checkout_base_dir
		"",         // 4.  worktree_dir
		"",         // 5.  state_dir
		"",         // 6.  default_branch
		"",         // 7.  labels.trigger
		"",         // 8.  labels.in_flight
		"",         // 9.  labels.blocked
		"",         // 10. labels.review
		"",         // 11. labels.terminal
		"",         // 12. labels.veto (take the template default)
		"",         // 13. agent.model
		"",         // 14. agent.effort
		"",         // 15. agent.permission_mode
		"",         // 16. agent.worktree
		"",         // 17. agent.max_budget_usd
		"",         // 18. agent.timeout
		"",         // 20. retry.max
		"",         // 21. retry.backoff
		"",         // 22. retry.breaker.orphan_threshold
		"",         // 23. retry.breaker.cooldown
	}
	p := &scriptPrompter{t: t, answers: answers}

	cfg, err := Run(p, d)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Join(cfg.Labels.Veto, ",") != strings.Join(tmpl.Labels.Veto, ",") {
		t.Fatalf("Labels.Veto = %v, want %v", cfg.Labels.Veto, tmpl.Labels.Veto)
	}
}
