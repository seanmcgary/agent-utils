package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validYAML = `
name: planning
repo: mcgarylabs/lawndominator-monorepo
checkout_base_dir: /tmp/checkout
worktree_dir: /tmp/worktrees
state_dir: /tmp/state
labels:
  trigger: status:ready-for-spec
  in_flight: status:speccing
  blocked: status:needs-spec-input
  review: status:plan-ready-for-review
  terminal: status:ready-for-execution
  veto:
    - blocked:design
default_branch: master
i_understand_bypass_permissions: true
agent:
  model: opus
  effort: high
  permission_mode: bypassPermissions
  worktree: per_issue
  max_budget_usd: 25
  timeout: 3h
tend_pr: true
retry:
  max: 3
  backoff: [0s, 15m, 30m]
  breaker:
    orphan_threshold: 2
    cooldown: 30m
prompt: "plan issue {{.Issue.Number}}"
resume_prompt: "resume issue {{.Issue.Number}}"
tend_prompt: "rebase PR {{.PR.Number}}"
`

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "loop.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RepoOwner() != "mcgarylabs" {
		t.Errorf("RepoOwner = %q, want mcgarylabs", cfg.RepoOwner())
	}
	if cfg.RepoName() != "lawndominator-monorepo" {
		t.Errorf("RepoName = %q, want lawndominator-monorepo", cfg.RepoName())
	}
	if got := cfg.Agent.Timeout.Std(); got != 3*time.Hour {
		t.Errorf("Timeout = %v, want 3h", got)
	}
	if got := cfg.Retry.Breaker.Cooldown.Std(); got != 30*time.Minute {
		t.Errorf("Cooldown = %v, want 30m", got)
	}
	if len(cfg.Retry.Backoff) != 3 {
		t.Fatalf("Backoff = %v, want 3 entries", cfg.Retry.Backoff)
	}
	if got := cfg.Retry.Backoff[1].Std(); got != 15*time.Minute {
		t.Errorf("Backoff[1] = %v, want 15m", got)
	}
	if !cfg.TendPR {
		t.Error("TendPR = false, want true")
	}
	if len(cfg.Labels.Veto) != 1 || cfg.Labels.Veto[0] != "blocked:design" {
		t.Errorf("Veto = %v", cfg.Labels.Veto)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	_, err := Load(writeTemp(t, validYAML+"\nbogus_key: 1\n"))
	if err == nil {
		t.Fatal("want error for unknown key, got nil")
	}
}

func TestLoadRejectsMissingLabelRole(t *testing.T) {
	body := `
name: planning
repo: a/b
checkout_base_dir: /tmp/c
worktree_dir: /tmp/w
state_dir: /tmp/s
default_branch: master
labels:
  trigger: t
  in_flight: f
  blocked: b
agent: {model: opus, worktree: per_issue, timeout: 1h}
retry: {max: 1, backoff: [0s], breaker: {orphan_threshold: 2, cooldown: 1m}}
prompt: p
resume_prompt: rp
`
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want error for missing labels.review, got nil")
	}
}

// labels.terminal is optional: the execution loop has no terminal label.
func TestLoadAcceptsMissingTerminalLabel(t *testing.T) {
	body := replaceOnce(validYAML, "  terminal: status:ready-for-execution\n", "")
	if _, err := Load(writeTemp(t, body)); err != nil {
		t.Fatalf("labels.terminal must be optional: %v", err)
	}
}

// bypassPermissions disables every permission gate on third-party issue text.
func TestBypassPermissionsNeedsExplicitAcknowledgement(t *testing.T) {
	body := replaceOnce(validYAML, "i_understand_bypass_permissions: true\n", "")
	if _, err := Load(writeTemp(t, body)); err == nil {
		t.Fatal("want error when bypassPermissions is set without the acknowledgement")
	}
}

func TestRejectsUnknownPermissionMode(t *testing.T) {
	body := replaceOnce(validYAML, "permission_mode: bypassPermissions", "permission_mode: nonsense")
	if _, err := Load(writeTemp(t, body)); err == nil {
		t.Fatal("want error for an invalid permission mode")
	}
}

func TestLoadRejectsShortBackoff(t *testing.T) {
	body := replaceOnce(validYAML, "backoff: [0s, 15m, 30m]", "backoff: [0s]")
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want error when len(backoff) < retry.max, got nil")
	}
}

// retry.backoff_ticks is a rejection shim: a stale config that still uses it
// must fail with a message that names the old key, the new key, and a value
// to copy, not a bare "unknown field" error.
func TestLoadRejectsBackoffTicks(t *testing.T) {
	body := replaceOnce(validYAML, "backoff: [0s, 15m, 30m]", "backoff_ticks: [0, 1, 2]")
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want error for retry.backoff_ticks, got nil")
	}
	if !strings.Contains(err.Error(), "retry.backoff") {
		t.Errorf("error %q does not mention retry.backoff", err.Error())
	}
}

// retry.max: 0 means never retry, so retry.backoff may legitimately be
// empty. Nothing may index it without a length check first.
func TestLoadAcceptsEmptyBackoffWhenMaxZero(t *testing.T) {
	body := replaceOnce(validYAML, "max: 3\n  backoff: [0s, 15m, 30m]", "max: 0")
	cfg, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Retry.Backoff) != 0 {
		t.Errorf("Backoff = %v, want empty", cfg.Retry.Backoff)
	}
}

func TestLoadRejectsBadRepo(t *testing.T) {
	body := replaceOnce(validYAML, "repo: mcgarylabs/lawndominator-monorepo", "repo: noslash")
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want error for malformed repo, got nil")
	}
}

func TestLoadRejectsBadWorktreeMode(t *testing.T) {
	body := replaceOnce(validYAML, "worktree: per_issue", "worktree: nonsense")
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want error for bad worktree mode, got nil")
	}
}

func TestTendPRRequiresTendPrompt(t *testing.T) {
	body := replaceOnce(validYAML, `tend_prompt: "rebase PR {{.PR.Number}}"`, "")
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want error when tend_pr is true and tend_prompt is empty, got nil")
	}
}

func replaceOnce(s, old, new string) string {
	i := indexOf(s, old)
	if i < 0 {
		panic("fixture does not contain: " + old)
	}
	return s[:i] + new + s[i+len(old):]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
