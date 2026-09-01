package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/project"
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
agent: {model: opus, worktree: per_issue, timeout: 1h}
retry: {max: 1, backoff: [0s], breaker: {orphan_threshold: 2, cooldown: 1m}}
prompt: p
resume_prompt: rp
`
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want error for missing labels.blocked, got nil")
	}
}

// agent.timeout is optional and defaults to 24 hours.
//
// It used to be required, which made every operator invent a number for the one
// setting they have no basis to choose -- and the number is always too small,
// because guessing low is invisible: the dispatch is recorded failed and
// retried from a resumed session, so it reads as a flaky agent rather than a
// misconfiguration.
func TestLoadDefaultsAgentTimeout(t *testing.T) {
	body := replaceOnce(validYAML, "  timeout: 3h\n", "")
	cfg, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("agent.timeout must be optional: %v", err)
	}
	if got := cfg.Agent.Timeout.Std(); got != DefaultAgentTimeout {
		t.Errorf("Agent.Timeout = %s, want %s", got, DefaultAgentTimeout)
	}
}

// An explicit timeout is never overwritten by the default.
func TestLoadKeepsAnExplicitAgentTimeout(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Agent.Timeout.Std(); got != 3*time.Hour {
		t.Errorf("Agent.Timeout = %s, want 3h", got)
	}
}

// A negative timeout is still an error. Zero cannot be one, because YAML cannot
// distinguish it from an omitted field -- but a negative duration sorts before
// every deadline and would kill the dispatch the moment it started.
func TestLoadRejectsNegativeAgentTimeout(t *testing.T) {
	body := replaceOnce(validYAML, "  timeout: 3h\n", "  timeout: -1h\n")
	if _, err := Load(writeTemp(t, body)); err == nil {
		t.Fatal("want error for a negative agent.timeout, got nil")
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
	// max: 3 with two backoff entries: the acceptance criterion is literal
	// about "two entries", so use two rather than relying on one to exercise
	// the same len(backoff) < retry.max branch.
	body := replaceOnce(validYAML, "backoff: [0s, 15m, 30m]", "backoff: [0s, 15m]")
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want error when len(backoff) < retry.max, got nil")
	}
}

// retry.backoff_ticks is a rejection shim: a stale config that still uses it
// must fail with a message that names the old key, the new key, and a value
// to copy, not a bare "unknown field" error. This asserts on text only the
// shim's own error branch can produce (not the separate "len(backoff) <
// retry.max" length error, which would also contain "retry.backoff" and so
// would let this test pass even if the shim were deleted).
func TestLoadRejectsBackoffTicks(t *testing.T) {
	body := replaceOnce(validYAML, "backoff: [0s, 15m, 30m]", "backoff_ticks: [0, 1, 2]")
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want error for retry.backoff_ticks, got nil")
	}
	for _, want := range []string{
		"retry.backoff_ticks is no longer supported",
		"backoff: [0s, 15m, 30m]",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
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

// tend_prompt is optional now. Whether a loop ever runs a tend is the project
// descriptor's business -- tend.enabled, tend.label and tend.loop -- and none of
// those are readable from a loop file, so a loop configuration cannot know
// whether it needs the template. The dispatch path checks instead, which also
// reports better: it names one pull request that was not tended rather than
// refusing to load the loop at all.
func TestLoadAcceptsMissingTendPrompt(t *testing.T) {
	body := replaceOnce(validYAML, `tend_prompt: "rebase PR {{.PR.Number}}"`, "")
	if _, err := Load(writeTemp(t, body)); err != nil {
		t.Fatalf("tend_prompt must be optional: %v", err)
	}
}

// A loop declares only its own states, and "waiting for a human to read" is not
// one of them: it is a claim about what happens AFTER the loop. The field is
// gone, so a file that still sets it must be rejected by the strict decoder
// rather than silently ignored -- an operator upgrading needs to be told.
func TestLoadRejectsTheRemovedReviewLabel(t *testing.T) {
	body := replaceOnce(validYAML,
		"  blocked: status:needs-spec-input\n",
		"  blocked: status:needs-spec-input\n  review: status:plan-ready-for-review\n")
	if _, err := Load(writeTemp(t, body)); err == nil {
		t.Fatal("want error for the removed labels.review, got nil")
	}
}

// Same for tend_pr, which moved to the project descriptor whole.
func TestLoadRejectsTheRemovedTendPR(t *testing.T) {
	body := replaceOnce(validYAML, "default_branch: master\n", "default_branch: master\ntend_pr: true\n")
	if _, err := Load(writeTemp(t, body)); err == nil {
		t.Fatal("want error for the removed tend_pr, got nil")
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

// max_budget_usd: 0 is how a loop runs with no cost ceiling:
// internal/runner/args.go only appends --max-budget-usd when the value is greater
// than zero, so 0 omits the flag entirely. Nothing in validate may start
// requiring a positive number without deliberately taking that away, so the
// behaviour is pinned here rather than left to be discovered by an operator
// whose uncapped loop suddenly refuses to load.
func TestLoadAcceptsZeroMaxBudget(t *testing.T) {
	body := replaceOnce(validYAML, "  max_budget_usd: 25\n", "  max_budget_usd: 0\n")
	cfg, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("max_budget_usd: 0 must load, it means no cap: %v", err)
	}
	if cfg.Agent.MaxBudgetUSD != 0 {
		t.Errorf("MaxBudgetUSD = %v, want 0", cfg.Agent.MaxBudgetUSD)
	}
}

// A negative budget is silently identical to no budget: args.go gates on
// "> 0", so -25 omits --max-budget-usd and the dispatch runs uncapped. An
// operator who typed a stray minus sign asked for a $25 ceiling and would
// have got none, with nothing said. Reject it at load, so a hand-edited file
// is caught and not only a wizard answer.
func TestLoadRejectsNegativeMaxBudget(t *testing.T) {
	body := replaceOnce(validYAML, "  max_budget_usd: 25\n", "  max_budget_usd: -25\n")
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want error for a negative max_budget_usd, got nil")
	}
	if !strings.Contains(err.Error(), "max_budget_usd") {
		t.Errorf("err = %v, want it to name max_budget_usd", err)
	}
}

func TestLoadAcceptsHarnessDefault(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.Harness != HarnessClaude {
		t.Errorf("Harness (unset) = %q, want %q", cfg.Agent.Harness, HarnessClaude)
	}
}

const piYAML = `
name: planning
repo: mcgarylabs/lawndominator-monorepo
checkout_base_dir: /tmp/checkout
worktree_dir: /tmp/worktrees
state_dir: /tmp/state
default_branch: master
labels:
  trigger: t
  in_flight: f
  blocked: b
i_understand_bypass_permissions: false
agent:
  harness: pi
  model: anthropic/claude-sonnet-4-5
  effort: high
  worktree: per_issue
  max_budget_usd: 0
  timeout: 3h
retry:
  max: 1
  backoff: [0s]
  breaker: {orphan_threshold: 2, cooldown: 1m}
prompt: "plan {{.Issue.Number}}"
resume_prompt: "resume {{.Issue.Number}}"
`

func TestLoadAcceptsPiHarness(t *testing.T) {
	cfg, err := Load(writeTemp(t, piYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.Harness != HarnessPi {
		t.Errorf("Harness = %q, want %q", cfg.Agent.Harness, HarnessPi)
	}
}

func TestLoadRejectsBadHarness(t *testing.T) {
	body := replaceOnce(piYAML, "  harness: pi\n", "  harness: gemini\n")
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want error for a bad harness, got nil")
	}
	if !strings.Contains(err.Error(), "harness") {
		t.Errorf("err = %v, want it to name harness", err)
	}
}

// A claude-only setting is IGNORED by pi, never rejected: PiBuildArgs emits
// no permission mode, and a harness: label can flip either harness to the
// other for one issue, so a config that carries the field must stay loadable
// under both.
func TestAcceptsPiPermissionModeAsANoOp(t *testing.T) {
	body := replaceOnce(piYAML, "  worktree: per_issue\n",
		"  permission_mode: acceptEdits\n  worktree: per_issue\n")
	cfg, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("Load pi with permission_mode: %v", err)
	}
	if cfg.Agent.PermissionMode != "acceptEdits" {
		t.Errorf("PermissionMode = %q, want it kept for a harness:claude override",
			cfg.Agent.PermissionMode)
	}
}

// The VALUE is still checked under pi. A harness:claude label makes it take
// effect, so a typo must not survive to that dispatch.
func TestRejectsAnInvalidPermissionModeUnderPi(t *testing.T) {
	body := replaceOnce(piYAML, "  worktree: per_issue\n",
		"  permission_mode: nonsense\n  worktree: per_issue\n")
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want reject an invalid permission_mode under pi, got nil")
	}
	if !strings.Contains(err.Error(), "permission_mode") {
		t.Errorf("err = %v, want it to name permission_mode", err)
	}
}

// Same reason for the acknowledgement: bypassPermissions on a pi config is
// one harness:claude label away from disabling every prompt.
func TestRequiresBypassAcknowledgementUnderPi(t *testing.T) {
	body := replaceOnce(piYAML, "  worktree: per_issue\n",
		"  permission_mode: bypassPermissions\n  worktree: per_issue\n")
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want reject unacknowledged bypassPermissions under pi, got nil")
	}
	if !strings.Contains(err.Error(), "i_understand_bypass_permissions") {
		t.Errorf("err = %v, want it to name the acknowledgement", err)
	}
}

func TestAcceptsPiBudgetNoOp(t *testing.T) {
	body := replaceOnce(piYAML, "  max_budget_usd: 0\n", "  max_budget_usd: 50\n")
	cfg, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("Load pi with non-zero budget: %v", err)
	}
	if cfg.Agent.Harness != HarnessPi {
		t.Errorf("Harness = %q, want %q", cfg.Agent.Harness, HarnessPi)
	}
}

// background_tasks is claude-only and reaches the child through claudeEnv,
// which a pi dispatch never builds. Accepted, and a no-op.
func TestAcceptsPiBackgroundTasksAsANoOp(t *testing.T) {
	body := replaceOnce(piYAML, "  worktree: per_issue\n",
		"  background_tasks: true\n  worktree: per_issue\n")
	cfg, err := Load(writeTemp(t, body))
	if err != nil {
		t.Fatalf("Load pi with background_tasks: %v", err)
	}
	if !cfg.Agent.BackgroundTasksEnabled() {
		t.Error("BackgroundTasks must be kept for a harness:claude override")
	}
}

// The field is a pointer for the tri-state, and the whole point of the pointer
// is that an absent field and an explicit false must both mean disabled while
// only an explicit true opts in. A plain bool would read absent as false too --
// correct today, and silently wrong the moment the safe default has to change.
func TestBackgroundTasksIsOffUnlessExplicitlyOn(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.BackgroundTasks != nil {
		t.Errorf("BackgroundTasks = %v, want nil for an absent field", *cfg.Agent.BackgroundTasks)
	}
	if cfg.Agent.BackgroundTasksEnabled() {
		t.Error("an absent background_tasks must mean disabled")
	}

	for _, tc := range []struct {
		value string
		want  bool
	}{{"false", false}, {"true", true}} {
		body := replaceOnce(validYAML, "  worktree: per_issue\n",
			"  background_tasks: "+tc.value+"\n  worktree: per_issue\n")
		cfg, err := Load(writeTemp(t, body))
		if err != nil {
			t.Fatalf("Load background_tasks: %s: %v", tc.value, err)
		}
		if got := cfg.Agent.BackgroundTasksEnabled(); got != tc.want {
			t.Errorf("background_tasks: %s -> Enabled() = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// A loop file outside a project loads with NO tend policy, which is tending
// off.
//
// This is the safe direction and it is worth pinning: the alternative failure
// is a pull request force-pushed by a policy nobody declared. `loop tick
// --config <path>` and every test fixture take this path.
func TestLoadWithoutAProjectLeavesTendOff(t *testing.T) {
	cfg, err := Load(writeTemp(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tend.Enabled || cfg.Tend.Label != "" || cfg.Tend.Loop != "" {
		t.Errorf("Tend = %+v, want the zero policy", cfg.Tend)
	}
	if cfg.TendsPRs() {
		t.Error("TendsPRs = true with no project descriptor")
	}
}

// A loop file INSIDE a project inherits the descriptor's policy, and TendsPRs
// is true only for the loop the descriptor names.
func TestLoadInheritsTheProjectTendPolicy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DirName)
	if err := os.MkdirAll(ConfigsDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := project.Save(dir, &project.Config{
		Name: "p", ID: "00000000-0000-0000-0000-000000000001",
		Tend: project.Tend{
			Enabled: true,
			Loop:    "planning",
			Label:   "status:ready-for-review",
			Model:   "sonnet",
		},
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ConfigsDir(dir), "planning.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tend.Label != "status:ready-for-review" || cfg.Tend.Model != "sonnet" {
		t.Errorf("Tend = %+v, want the descriptor's policy", cfg.Tend)
	}
	if !cfg.TendsPRs() {
		t.Error("TendsPRs = false for the loop the descriptor names")
	}

	// Load defaults agent.harness so it always resolves, but must NOT default
	// tend.harness: an absent value there means "fall back to agent.harness",
	// and defaulting it would silently pin every tend dispatch to claude on a
	// harness: pi loop.
	if cfg.Tend.Harness != "" {
		t.Errorf("Tend.Harness = %q, want empty: nothing may default it", cfg.Tend.Harness)
	}
}

// The same descriptor, read by a loop it does not name, does not tend. This is
// what stops two loops in one project both force-pushing the same branch.
func TestLoadDoesNotTendOnALoopTheProjectDoesNotName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DirName)
	if err := os.MkdirAll(ConfigsDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := project.Save(dir, &project.Config{
		Name: "p", ID: "00000000-0000-0000-0000-000000000001",
		Tend: project.Tend{Enabled: true, Loop: "execution", Label: "status:ready-for-review"},
	}); err != nil {
		t.Fatal(err)
	}
	// validYAML is named "planning"; the descriptor names "execution".
	path := filepath.Join(ConfigsDir(dir), "planning.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TendsPRs() {
		t.Error("TendsPRs = true on a loop the descriptor does not name")
	}
}

// A descriptor that exists but will not parse fails the loop load, rather than
// silently leaving tending off. It might have said "enabled", and running the
// loop with a policy nobody can read is the outcome worth refusing.
func TestLoadRefusesABrokenProjectDescriptor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DirName)
	if err := os.MkdirAll(ConfigsDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := "name: p\nid: x\ntend: [not a map"
	if err := os.WriteFile(project.Path(dir), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ConfigsDir(dir), "planning.yaml")
	if err := os.WriteFile(path, []byte(validYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("want an error for an unreadable project descriptor, got nil")
	}
}
