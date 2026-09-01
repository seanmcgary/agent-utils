package config

import (
	"errors"
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

// tend_prompt is GONE from a loop file, not merely optional. A loop does not
// tend, so the instructions for a tend belong to the project descriptor with
// the rest of the policy. A file that still carries one must be told, not
// silently ignored: an operator upgrading has to move it.
func TestLoadRejectsTheRemovedTendPrompt(t *testing.T) {
	body := replaceOnce(validYAML,
		`resume_prompt: "resume issue {{.Issue.Number}}"`,
		"resume_prompt: \"resume issue {{.Issue.Number}}\"\ntend_prompt: \"rebase PR {{.PR.Number}}\"")
	if _, err := Load(writeTemp(t, body)); err == nil {
		t.Fatal("want error for the removed tend_prompt, got nil")
	}
}

// The tend dispatcher's name is reserved, and a loop that takes it must be
// refused rather than allowed to share its rows. Both would key dispatches,
// pr_links, tend_conflicts, the tick counter, the lock file and the worktree
// tree by the same (project, loop) pair, and both would write them.
func TestLoadRejectsTheReservedTendName(t *testing.T) {
	body := replaceOnce(validYAML, "name: planning", "name: "+project.Reserved)
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want error for a loop named after the tend dispatcher, got nil")
	}
	// The message must say WHY. "reserved" on its own reads as bureaucracy,
	// and an operator who does not know what they would have collided with
	// will rename the loop and wonder what they lost.
	if !strings.Contains(err.Error(), "tend dispatcher") {
		t.Errorf("error must name the tend dispatcher, got: %v", err)
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

// A loop file NEVER carries a tend policy, inside a project or outside one.
//
// Load stopped reading the project descriptor entirely when tending became its
// own dispatcher: a loop that does not tend has no reason to know the policy
// exists, and the zero value here is what every consumer of a loop
// configuration now sees.
func TestLoadNeverFillsInTheTendPolicy(t *testing.T) {
	dir := tendProject(t, tendPolicy())

	cfg, err := Load(filepath.Join(ConfigsDir(dir), "planning.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tend.Enabled || cfg.Tend.Label != "" || cfg.Tend.Prompt != "" {
		t.Errorf("Tend = %+v, want the zero policy on a loop configuration", cfg.Tend)
	}
}

// tendPolicy is a descriptor tend block that LoadTend accepts.
func tendPolicy() project.Tend {
	return project.Tend{
		Enabled:        true,
		Label:          "status:ready-for-review",
		Model:          "sonnet",
		PermissionMode: "acceptEdits",
		Prompt:         "rebase PR {{.PR.Number}} for issue {{.Issue.Number}}",
	}
}

// tendProject writes a descriptor and one loop file, and returns the
// .agent-utils directory.
func tendProject(t *testing.T, tend project.Tend) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), DirName)
	if err := os.MkdirAll(ConfigsDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := project.Save(dir, &project.Config{
		Name: "p", ID: "00000000-0000-0000-0000-000000000001", Tend: tend,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(ConfigsDir(dir), "planning.yaml"), []byte(validYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// LoadTend synthesises the dispatcher: the policy's agent and prompt, the
// loops' repository facts, and the reserved name.
func TestLoadTendSynthesisesTheDispatcher(t *testing.T) {
	dir := tendProject(t, tendPolicy())

	cfg, err := LoadTend(dir)
	if err != nil {
		t.Fatalf("LoadTend: %v", err)
	}
	if cfg.Name != project.Reserved {
		t.Errorf("Name = %q, want %q", cfg.Name, project.Reserved)
	}
	// The repository facts come from the loop, which is the only thing that
	// knows them. A dispatcher that got these wrong would tend the wrong
	// repository or rebase onto the wrong branch.
	if cfg.Repo != "mcgarylabs/lawndominator-monorepo" || cfg.DefaultBranch != "master" {
		t.Errorf("repo/branch = %q/%q, want the loop's", cfg.Repo, cfg.DefaultBranch)
	}
	// The agent comes from the policy, and NOTHING is inherited from the
	// loop's agent: the loop runs opus at high effort and the tend must not.
	if cfg.Agent.Model != "sonnet" || cfg.Agent.Harness != HarnessClaude {
		t.Errorf("agent = %+v, want the tend policy's model on the default harness", cfg.Agent)
	}
	if cfg.Agent.Effort == "high" {
		t.Error("effort was inherited from the loop's agent, which must never happen")
	}
	// A tend always gets its own worktree for the pull request it rebases.
	if cfg.Agent.Worktree != WorktreePerIssue {
		t.Errorf("Agent.Worktree = %q, want %q", cfg.Agent.Worktree, WorktreePerIssue)
	}
	// The prompt is the dispatcher's only prompt, so it lands where the runner
	// already looks.
	if !strings.Contains(cfg.Prompt, "rebase PR") {
		t.Errorf("Prompt = %q, want the descriptor's tend.prompt", cfg.Prompt)
	}
	if cfg.Tend.Label != "status:ready-for-review" {
		t.Errorf("Tend.Label = %q, want the descriptor's", cfg.Tend.Label)
	}
}

// Tending switched off is not an error, and the error it returns is
// DISTINGUISHABLE. The listener skips such a project silently on every scan;
// an operator who typed --name tend gets a sentence instead.
func TestLoadTendReportsTendingOff(t *testing.T) {
	tend := tendPolicy()
	tend.Enabled = false
	dir := tendProject(t, tend)

	_, err := LoadTend(dir)
	if !errors.Is(err, ErrNoTend) {
		t.Fatalf("LoadTend err = %v, want ErrNoTend", err)
	}
}

// A dispatcher with no permission mode would fail at its first `git push`, in
// a detached process, a long way from the file that caused it. It is refused
// here instead -- and NOT in project.Load, so an operator mid-edit can park a
// policy without being stopped by a field only the dispatcher needs.
func TestLoadTendRequiresAPermissionMode(t *testing.T) {
	tend := tendPolicy()
	tend.PermissionMode = ""
	dir := tendProject(t, tend)

	if _, err := LoadTend(dir); err == nil {
		t.Fatal("want an error for a tend policy with no permission_mode, got nil")
	}
}

// Loops that disagree about the repository leave the dispatcher unable to say
// what it maintains. The error names BOTH loops: "the loops disagree" alone
// leaves the operator to diff every file themselves.
func TestLoadTendRejectsLoopsThatDisagreeAboutTheRepo(t *testing.T) {
	dir := tendProject(t, tendPolicy())
	other := replaceOnce(validYAML, "name: planning", "name: execution")
	other = replaceOnce(other, "repo: mcgarylabs/lawndominator-monorepo", "repo: mcgarylabs/other")
	if err := os.WriteFile(
		filepath.Join(ConfigsDir(dir), "execution.yaml"), []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadTend(dir)
	if err == nil {
		t.Fatal("want an error when the loops disagree about repo, got nil")
	}
	for _, want := range []string{"planning", "execution", "repo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q, got: %v", want, err)
		}
	}
}

// A descriptor that will not parse fails the DISPATCHER, rather than silently
// leaving tending off. It might have said "enabled", and tending on a policy
// nobody can read is the outcome worth refusing. A loop load is unaffected: a
// loop no longer reads the descriptor at all.
func TestLoadTendRefusesABrokenProjectDescriptor(t *testing.T) {
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

	if _, err := Load(path); err != nil {
		t.Fatalf("a loop load must not read the descriptor at all: %v", err)
	}
	if _, err := LoadTend(dir); err == nil {
		t.Fatal("want an error for an unreadable project descriptor, got nil")
	}
}

// The tend dispatcher is addressed by the PROJECT DESCRIPTOR's path, and
// IsTendPath is the one seam that recognises it. loopcmd.Open, the detached
// runner's --config, and the four operator commands all carry that path, so a
// round trip that broke here would send `--name tend` into config.Load and
// report a parse error about a file that is not a loop.
func TestTendPathRoundTripsThroughIsTendPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DirName)
	if !IsTendPath(TendPath(dir)) {
		t.Errorf("IsTendPath(%q) = false, want true", TendPath(dir))
	}
	// A loop file must never be mistaken for it: they live in configs/ and are
	// named for their loops.
	if IsTendPath(filepath.Join(ConfigsDir(dir), "planning.yaml")) {
		t.Error("a loop file was recognised as the tend dispatcher's path")
	}
}

// A loop file may legally be CALLED config.yaml. List accepts every *.yaml and
// *.yml in configs/ and takes the loop's name from inside the file, never from
// the file name -- so recognising the dispatcher by base name alone routed
// `--config .../configs/config.yaml` (and `--name <that loop>`, and the
// detached runner spawned with it) into LoadTend(".../configs"), which then
// parsed a loop file as a project descriptor and failed naming a file the
// operator never mentioned.
func TestALoopFileNamedConfigYAMLIsNotTheTendDispatcher(t *testing.T) {
	dir := filepath.Join(t.TempDir(), DirName)
	loop := filepath.Join(ConfigsDir(dir), project.FileName)
	if IsTendPath(loop) {
		t.Errorf("IsTendPath(%q) = true: a loop file in configs/ is not the project descriptor", loop)
	}
	// The control, so this cannot pass by IsTendPath simply never saying yes.
	if !IsTendPath(TendPath(dir)) {
		t.Errorf("IsTendPath(%q) = false, want true", TendPath(dir))
	}
}

// That the loop file really is loadable through the ordinary discovery path,
// which is what makes the case above reachable rather than hypothetical.
func TestListLoadsALoopFileNamedConfigYAML(t *testing.T) {
	t.Setenv("AGENT_UTILS_DIR", "")
	dir := filepath.Join(t.TempDir(), DirName)
	if err := os.MkdirAll(ConfigsDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(ConfigsDir(dir), project.FileName), []byte(validYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Err != nil {
		t.Fatalf("entries = %+v, want one that loads", entries)
	}
	path, err := Resolve(dir, entries[0].Name)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", entries[0].Name, err)
	}
	if IsTendPath(path) {
		t.Errorf("Resolve(%q) = %q, which IsTendPath claims is the tend dispatcher",
			entries[0].Name, path)
	}
}

// A loop file taking the reserved name is reported by DISCOVERY, not merely by
// a direct Load: List is what the operator commands and the webhook router
// walk, and an entry that silently did not appear would be harder to debug
// than one that appears with its error.
func TestListReportsALoopThatTakesTheReservedName(t *testing.T) {
	t.Setenv("AGENT_UTILS_DIR", "")
	dir := filepath.Join(t.TempDir(), DirName)
	if err := os.MkdirAll(ConfigsDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	body := replaceOnce(validYAML, "name: planning", "name: "+project.Reserved)
	if err := os.WriteFile(
		filepath.Join(ConfigsDir(dir), "tend.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Err == nil {
		t.Fatal("a loop named after the tend dispatcher must carry its load error")
	}
	if !strings.Contains(entries[0].Err.Error(), "tend dispatcher") {
		t.Errorf("error must name the reason, got: %v", entries[0].Err)
	}
}
