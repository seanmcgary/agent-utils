package wizard

import (
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
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

func TestRunAcceptsEveryDefault(t *testing.T) {
	for _, tmplName := range templateNames {
		t.Run(tmplName, func(t *testing.T) {
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
				"",       // 11. labels.terminal
				"",       // 12. labels.veto
				"",       // 13. agent.harness
				"",       // 14. agent.model
				"",       // 15. agent.effort
				"",       // 16. agent.permission_mode
				"",       // 17. agent.worktree
				"",       // 18. agent.max_budget_usd
				"",       // 19. agent.timeout
				"",       // 21. retry.max
				"",       // 22. retry.backoff
				"",       // 23. retry.breaker.orphan_threshold
				"",       // 24. retry.breaker.cooldown
			}
			p := &scriptPrompter{t: t, answers: answers}

			cfg, err := Run(p, d)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			// The loop name defaults to the template just chosen, so
			// accepting every default from the execution template produces
			// an "execution" loop, not a "planning" one.
			if cfg.Name != tmplName {
				t.Errorf("Name = %q, want %q -- the chosen template's name", cfg.Name, tmplName)
			}

			// The one security-relevant default: accepting every default must
			// never produce bypassPermissions. config.validate would accept
			// bypassPermissions just as happily as acceptEdits once the
			// acknowledgement flag is set, so a flipped default here would
			// pass config.Load and every other assertion in this test while
			// silently disabling every permission prompt on third-party
			// issue text.
			if cfg.Agent.PermissionMode != "acceptEdits" {
				t.Fatalf("Agent.PermissionMode = %q, want %q", cfg.Agent.PermissionMode, "acceptEdits")
			}
			if cfg.AcknowledgeBypassPermissions {
				t.Fatal("AcknowledgeBypassPermissions = true on the default path, want false")
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

// TestRunBypassPermissionsDeclineThenAcceptEditsStaysUnacknowledged covers
// the branch at the top of the permission_mode loop in run.go: after
// declining the bypassPermissions confirmation once, the operator picks a
// different, non-bypass mode. AcknowledgeBypassPermissions must land false,
// not just default false — this exercises the loop actually resetting it,
// not merely never having set it.
func TestRunBypassPermissionsDeclineThenAcceptEditsStaysUnacknowledged(t *testing.T) {
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
		"",                  // 11. labels.terminal
		"",                  // 12. labels.veto
		"",                  // 13. agent.harness
		"",                  // 14. agent.model
		"",                  // 15. agent.effort
		"bypassPermissions", // 16. agent.permission_mode, attempt 1
		"acceptEdits",       // 16. agent.permission_mode, re-asked after decline: pick a different mode
		"",                  // 17. agent.worktree
		"",                  // 18. agent.max_budget_usd
		"",                  // 19. agent.timeout
		"",                  // 21. retry.max
		"",                  // 22. retry.backoff
		"",                  // 23. retry.breaker.orphan_threshold
		"",                  // 24. retry.breaker.cooldown
	}
	confirms := []bool{false} // decline the one bypassPermissions confirmation
	p := &scriptPrompter{t: t, answers: answers, confirms: confirms}

	cfg, err := Run(p, d)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.Agent.PermissionMode != "acceptEdits" {
		t.Fatalf("Agent.PermissionMode = %q, want %q", cfg.Agent.PermissionMode, "acceptEdits")
	}
	if cfg.AcknowledgeBypassPermissions {
		t.Fatal("AcknowledgeBypassPermissions = true after switching away from bypassPermissions, want false")
	}
}

func TestRunBypassPermissionsDeclineReasksThenAccepts(t *testing.T) {
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
		"",                  // 11. labels.terminal
		"",                  // 12. labels.veto
		"",                  // 13. agent.harness
		"",                  // 14. agent.model
		"",                  // 15. agent.effort
		"bypassPermissions", // 16. agent.permission_mode, attempt 1
		"bypassPermissions", // 16. agent.permission_mode, re-asked after decline
		"",                  // 17. agent.worktree
		"",                  // 18. agent.max_budget_usd
		"",                  // 19. agent.timeout
		"",                  // 21. retry.max
		"",                  // 22. retry.backoff
		"",                  // 23. retry.breaker.orphan_threshold
		"",                  // 24. retry.breaker.cooldown
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
		"",             // 11. labels.terminal
		"",             // 12. labels.veto
		"",             // 13. agent.harness
		"",             // 14. agent.model
		"",             // 15. agent.effort
		"",             // 16. agent.permission_mode
		"",             // 17. agent.worktree
		"",             // 18. agent.max_budget_usd
		"",             // 19. agent.timeout
		"3",            // 21. retry.max
		"0s, 15m",      // 22. retry.backoff, attempt 1: only 2 entries for max=3
		"0s, 15m, 30m", // 22. retry.backoff, re-asked: 3 entries
		"",             // 23. retry.breaker.orphan_threshold
		"",             // 24. retry.breaker.cooldown
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
		"",             // 11. labels.terminal
		"",             // 12. labels.veto
		"",             // 13. agent.harness
		"",             // 14. agent.model
		"",             // 15. agent.effort
		"",             // 16. agent.permission_mode
		"",             // 17. agent.worktree
		"",             // 18. agent.max_budget_usd
		"",             // 19. agent.timeout
		"",             // 21. retry.max
		"",             // 22. retry.backoff
		"",             // 23. retry.breaker.orphan_threshold
		"",             // 24. retry.breaker.cooldown
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
		"",             // 11. labels.terminal
		"",             // 12. labels.veto
		"",             // 13. agent.harness
		"",             // 14. agent.model
		"",             // 15. agent.effort
		"",             // 16. agent.permission_mode
		"",             // 17. agent.worktree
		"",             // 18. agent.max_budget_usd
		"notaduration", // 19. agent.timeout, attempt 1: not a valid duration
		"3h",           // 19. agent.timeout, re-asked: valid
		"",             // 21. retry.max
		"",             // 22. retry.backoff
		"",             // 23. retry.breaker.orphan_threshold
		"",             // 24. retry.breaker.cooldown
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
		"",         // 11. labels.terminal
		"",         // 12. labels.veto (take the template default)
		"",         // 13. agent.harness
		"",         // 14. agent.model
		"",         // 15. agent.effort
		"",         // 16. agent.permission_mode
		"",         // 17. agent.worktree
		"",         // 18. agent.max_budget_usd
		"",         // 19. agent.timeout
		"",         // 21. retry.max
		"",         // 22. retry.backoff
		"",         // 23. retry.breaker.orphan_threshold
		"",         // 24. retry.breaker.cooldown
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

// TestRunRequiredFieldWithEmptyDefaultReasks covers the exact failure a
// reviewer found end to end: outside a git work tree Detect leaves
// default_branch with no default. (Neither checkout_base_dir nor worktree_dir
// belongs in that list any more: both default to a literal relative path,
// which is portable and never empty.) Before Ask consulted Question.Optional,
// an empty answer to such a question sailed through as "", and the resulting
// invalid file only failed at Write's reload -- by which point the target
// filename was already claimed, so a retry was refused by the overwrite
// guard. This proves the re-ask happens immediately, mid-script, the same way
// a failed Validate does.
func TestRunRequiredFieldWithEmptyDefaultReasks(t *testing.T) {
	// Detected{} is what Detect returns outside a git work tree: every
	// git-derived default is empty, which is precisely the case this test
	// covers.
	d := Detected{}

	answers := []string{
		"planning",     // 24. prompt template
		"",             // 1.  name
		"acme/example", // 2.  repo
		"",             // 3.  checkout_base_dir: defaults to "." -- resolved against the project root, so it is never empty
		"",             // 4.  worktree_dir: defaults to .agent-utils/worktrees -- resolved against the project root, so it is never empty
		"",             // 5.  state_dir (optional; empty is fine)
		"",             // 6.  default_branch, attempt 1: no default, required -> re-asked
		"main",         // 6.  default_branch, re-asked
		"",             // 7.  labels.trigger
		"",             // 8.  labels.in_flight
		"",             // 9.  labels.blocked
		"",             // 11. labels.terminal
		"",             // 12. labels.veto
		"",             // 13. agent.harness
		"",             // 14. agent.model
		"",             // 15. agent.effort
		"",             // 16. agent.permission_mode
		"",             // 17. agent.worktree
		"",             // 18. agent.max_budget_usd
		"",             // 19. agent.timeout
		"",             // 21. retry.max
		"",             // 22. retry.backoff
		"",             // 23. retry.breaker.orphan_threshold
		"",             // 24. retry.breaker.cooldown
	}
	p := &scriptPrompter{t: t, answers: answers}

	cfg, err := Run(p, d)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.CheckoutBaseDir != "." {
		t.Fatalf("CheckoutBaseDir = %q, want %q", cfg.CheckoutBaseDir, ".")
	}
	if cfg.WorktreeDir != worktreeDirDefault {
		t.Fatalf("WorktreeDir = %q, want %q", cfg.WorktreeDir, worktreeDirDefault)
	}
	if cfg.DefaultBranch != "main" {
		t.Fatalf("DefaultBranch = %q, want %q", cfg.DefaultBranch, "main")
	}

	// Prove the result is actually writable: the whole point of re-asking
	// mid-script is that the file Write produces loads on the first try.
	dir := t.TempDir()
	if _, err := Write(dir, cfg); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func TestRunInvalidNameReasks(t *testing.T) {
	d := detected(t)

	answers := []string{
		"planning",  // 24. prompt template
		"../escape", // 1.  name, attempt 1: escapes configs/ via ".." and "/"
		"safe-name", // 1.  name, re-asked: valid
		"",          // 2.  repo
		"",          // 3.  checkout_base_dir
		"",          // 4.  worktree_dir
		"",          // 5.  state_dir
		"",          // 6.  default_branch
		"",          // 7.  labels.trigger
		"",          // 8.  labels.in_flight
		"",          // 9.  labels.blocked
		"",          // 11. labels.terminal
		"",          // 12. labels.veto
		"",          // 13. agent.harness
		"",          // 14. agent.model
		"",          // 15. agent.effort
		"",          // 16. agent.permission_mode
		"",          // 17. agent.worktree
		"",          // 18. agent.max_budget_usd
		"",          // 19. agent.timeout
		"",          // 21. retry.max
		"",          // 22. retry.backoff
		"",          // 23. retry.breaker.orphan_threshold
		"",          // 24. retry.breaker.cooldown
	}
	p := &scriptPrompter{t: t, answers: answers}

	cfg, err := Run(p, d)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.Name != "safe-name" {
		t.Fatalf("Name = %q, want %q", cfg.Name, "safe-name")
	}
}

func TestRunNonPositiveAgentTimeoutReasks(t *testing.T) {
	d := detected(t)

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
		"",         // 11. labels.terminal
		"",         // 12. labels.veto
		"",         // 13. agent.harness
		"",         // 14. agent.model
		"",         // 15. agent.effort
		"",         // 16. agent.permission_mode
		"",         // 17. agent.worktree
		"",         // 18. agent.max_budget_usd
		"0s",       // 19. agent.timeout, attempt 1: config.validate requires > 0
		"1h",       // 19. agent.timeout, re-asked: valid
		"",         // 21. retry.max
		"",         // 22. retry.backoff
		"",         // 23. retry.breaker.orphan_threshold
		"",         // 24. retry.breaker.cooldown
	}
	p := &scriptPrompter{t: t, answers: answers}

	cfg, err := Run(p, d)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.Agent.Timeout.String() != "1h0m0s" {
		t.Fatalf("Agent.Timeout = %v, want 1h", cfg.Agent.Timeout)
	}
}

func TestRunNegativeRetryBreakerCooldownReasks(t *testing.T) {
	d := detected(t)

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
		"",         // 11. labels.terminal
		"",         // 12. labels.veto
		"",         // 13. agent.harness
		"",         // 14. agent.model
		"",         // 15. agent.effort
		"",         // 16. agent.permission_mode
		"",         // 17. agent.worktree
		"",         // 18. agent.max_budget_usd
		"",         // 19. agent.timeout
		"",         // 21. retry.max
		"",         // 22. retry.backoff
		"",         // 23. retry.breaker.orphan_threshold
		"-5m",      // 24. retry.breaker.cooldown, attempt 1: config.validate requires > 0
		"45m",      // 24. retry.breaker.cooldown, re-asked: valid
	}
	p := &scriptPrompter{t: t, answers: answers}

	cfg, err := Run(p, d)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cfg.Retry.Breaker.Cooldown.String() != "45m0s" {
		t.Fatalf("Retry.Breaker.Cooldown = %v, want 45m", cfg.Retry.Breaker.Cooldown)
	}
}

// A negative budget is not merely odd: internal/runner/args.go gates on
// "> 0", so it silently means "no cap" -- the opposite of what someone who
// typed a stray minus sign asked for. 0 is the deliberate way to say that,
// and stays accepted.
func TestRunNegativeAgentMaxBudgetReasksAndZeroIsAccepted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers []string
		want    float64
	}{
		{"negative is re-asked", []string{"-25", "25"}, 25},
		{"zero means no cap", []string{"0"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := detected(t)

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
				"",         // 11. labels.terminal
				"",         // 12. labels.veto
				"",         // 13. agent.harness
				"",         // 14. agent.model
				"",         // 15. agent.effort
				"",         // 16. agent.permission_mode
				"",         // 17. agent.worktree
			}
			answers = append(answers, tc.answers...) // 18. agent.max_budget_usd
			answers = append(answers,
				"", // 19. agent.timeout
				"", // 21. retry.max
				"", // 22. retry.backoff
				"", // 23. retry.breaker.orphan_threshold
				"", // 24. retry.breaker.cooldown
			)
			p := &scriptPrompter{t: t, answers: answers}

			cfg, err := Run(p, d)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if cfg.Agent.MaxBudgetUSD != tc.want {
				t.Fatalf("Agent.MaxBudgetUSD = %v, want %v", cfg.Agent.MaxBudgetUSD, tc.want)
			}
		})
	}
}
