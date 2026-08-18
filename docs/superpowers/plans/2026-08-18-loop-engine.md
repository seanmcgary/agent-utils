# Loop Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go CLI that reads GitHub issues by label and dispatches `claude -p` agents deterministically, replacing the LLM orchestrator in the planning and execution loops.

**Architecture:** A cron-triggered `loop tick` command takes a file lock, reconciles GitHub issue labels against SQLite dispatch rows, runs a pure decision function, and spawns detached `agent-utils internal run-agent` processes. Each detached process runs `claude` as its child and records the exit code, cost, and duration. Go owns every deterministic decision. The agent owns every judgment and every GitHub write, with one stated exception.

**Tech Stack:** Go 1.25.9, `urfave/cli` v3.11.0, `go-github` v77.0.0, `modernc.org/sqlite` v1.56.0 (no CGO), `gopkg.in/yaml.v3` v3.0.1, `log/slog`.

**Spec:** `docs/superpowers/specs/2026-08-18-loop-engine-design.md`

## Global Constraints

This repository has **no** conventions document at its root. There is no `AGENTS.md`, `CLAUDE.md`, `CONTRIBUTING.md`, `STANDARDS.md`, or `STYLEGUIDE.md`. The only binding convention available is the operator's global instruction file at `~/.claude/CLAUDE.md`. It is copied here word for word:

> - when indenting javascript/typescript chained function calls, always align the .functionName() with the start of the line above. e.g.
> Promise.resolve()
> .then(() => 'stuff');
>
> NOT
>
> Promsie.resolve()
>     .then(() => 'stuff');

That rule governs JavaScript and TypeScript. This project is Go, so no task changes because of it. Record it and move on.

The remaining constraints come from the spec. Every task must respect them:

- Go module path is `github.com/seanmcgary/agent-utils`. Go version is `1.25.9`.
- Pin these versions exactly: `github.com/urfave/cli/v3 v3.11.0`, `github.com/google/go-github/v77 v77.0.0`, `modernc.org/sqlite v1.56.0`, `gopkg.in/yaml.v3 v3.0.1`.
- Use `modernc.org/sqlite`. Do not use a driver that needs CGO. The driver name is `sqlite`.
- Go must not write to GitHub, except for the single retry-cap park in Task 10.
- The engine must never apply the `terminal` label.
- The engine must never merge a pull request.
- `engine.Decide` must stay pure. It must perform no I/O.
- Format with `gofmt`. Vet with `go vet ./...`.

---

## File structure

| Path | Responsibility |
|---|---|
| `cmd/agent-utils/main.go` | Command wiring only. No logic. |
| `internal/config/config.go` | Config types, YAML load, strict validation. |
| `internal/config/duration.go` | `Duration` type. `yaml.v3` cannot decode `time.Duration`. |
| `internal/store/store.go` | SQLite open, schema, queries. |
| `internal/store/types.go` | `IssueState`, `Dispatch`, `PRLink`, `Tick`. |
| `internal/ghub/ghub.go` | `Client` interface and the go-github implementation. |
| `internal/ghub/types.go` | `Issue`, `PullRequest`. |
| `internal/engine/engine.go` | `Decide`. Pure. |
| `internal/engine/types.go` | `Snapshot`, `State`, `Decision`, `Plan`. |
| `internal/engine/prlink.go` | Pure `LinkPR` closing-keyword parser. |
| `internal/proc/proc.go` | Process liveness by identity. |
| `internal/lock/lock.go` | Per-loop advisory file lock. |
| `internal/worktree/worktree.go` | Git worktree create, remove, checkout. |
| `internal/runner/args.go` | Pure `claude` argv construction. |
| `internal/runner/result.go` | Pure stream-json result parser. |
| `internal/runner/runner.go` | Detached spawn and child supervision. |
| `internal/loopcmd/tick.go` | Tick orchestration: lock, reconcile, decide, act. |
| `internal/loopcmd/status.go` | Status rendering. |

---

### Task 1: Scaffolding and the config package

**Files:**
- Create: `internal/config/duration.go`
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`
- Modify: `go.mod`

**Interfaces:**
- Consumes: nothing.
- Produces: `config.Load(path string) (*Config, error)`; types `Config`, `Labels`, `Agent`, `Retry`, `Breaker`, `Duration`; methods `(*Config).RepoOwner() string`, `(*Config).RepoName() string`, `(Duration).Std() time.Duration`.

**review: yes** — this task defines the configuration surface every later task consumes. A wrong field name here propagates everywhere.

**Acceptance criteria:** `go test ./internal/config/...` passes. A YAML file with an unknown key is rejected. A missing required label role is rejected. `3h` decodes into a `Duration`.

- [ ] **Step 1: Add dependencies**

```bash
cd /Users/seanmcgary/Code/agent-utils
go get github.com/urfave/cli/v3@v3.11.0
go get github.com/google/go-github/v77@v77.0.0
go get modernc.org/sqlite@v1.56.0
go get gopkg.in/yaml.v3@v3.0.1
go get github.com/google/uuid@v1.6.0
```

- [ ] **Step 2: Write the failing test**

Create `internal/config/config_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
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
  backoff_ticks: [0, 1, 2]
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
labels:
  trigger: t
  in_flight: f
  blocked: b
  review: r
agent: {model: opus, worktree: per_issue, timeout: 1h}
retry: {max: 1, backoff_ticks: [0], breaker: {orphan_threshold: 2, cooldown: 1m}}
prompt: p
resume_prompt: rp
`
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want error for missing labels.terminal, got nil")
	}
}

func TestLoadRejectsShortBackoff(t *testing.T) {
	body := replaceOnce(validYAML, "backoff_ticks: [0, 1, 2]", "backoff_ticks: [0]")
	_, err := Load(writeTemp(t, body))
	if err == nil {
		t.Fatal("want error when len(backoff_ticks) < retry.max, got nil")
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
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/config/...`
Expected: FAIL. The package does not compile because `Load` does not exist.

- [ ] **Step 4: Write `internal/config/duration.go`**

`yaml.v3` does not decode a string into `time.Duration`. This type does it.

```go
package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration decodes a YAML string such as "3h" into a time.Duration.
type Duration time.Duration

// UnmarshalYAML parses the scalar with time.ParseDuration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string such as \"30m\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String renders the value for logs.
func (d Duration) String() string { return time.Duration(d).String() }
```

- [ ] **Step 5: Write `internal/config/config.go`**

```go
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is one loop definition.
type Config struct {
	Name            string `yaml:"name"`
	Repo            string `yaml:"repo"`
	CheckoutBaseDir string `yaml:"checkout_base_dir"`
	WorktreeDir     string `yaml:"worktree_dir"`
	StateDir        string `yaml:"state_dir"`

	Labels Labels `yaml:"labels"`
	Agent  Agent  `yaml:"agent"`
	TendPR bool   `yaml:"tend_pr"`
	Retry  Retry  `yaml:"retry"`

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

// Agent holds the claude invocation settings.
type Agent struct {
	Model          string   `yaml:"model"`
	Effort         string   `yaml:"effort"`
	PermissionMode string   `yaml:"permission_mode"`
	Worktree       string   `yaml:"worktree"`
	MaxBudgetUSD   float64  `yaml:"max_budget_usd"`
	Timeout        Duration `yaml:"timeout"`
}

// Retry holds the failure policy.
type Retry struct {
	Max          int     `yaml:"max"`
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
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	var errs []error

	required := map[string]string{
		"name":              c.Name,
		"repo":              c.Repo,
		"checkout_base_dir": c.CheckoutBaseDir,
		"worktree_dir":      c.WorktreeDir,
		"state_dir":         c.StateDir,
		"prompt":            c.Prompt,
		"resume_prompt":     c.ResumePrompt,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			errs = append(errs, fmt.Errorf("%s is required", field))
		}
	}

	owner, name, ok := strings.Cut(c.Repo, "/")
	if !ok || owner == "" || name == "" {
		errs = append(errs, fmt.Errorf("repo must be in owner/name form, got %q", c.Repo))
	}

	roles := map[string]string{
		"labels.trigger":   c.Labels.Trigger,
		"labels.in_flight": c.Labels.InFlight,
		"labels.blocked":   c.Labels.Blocked,
		"labels.review":    c.Labels.Review,
		"labels.terminal":  c.Labels.Terminal,
	}
	for field, value := range roles {
		if strings.TrimSpace(value) == "" {
			errs = append(errs, fmt.Errorf("%s is required", field))
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
	if c.Agent.Timeout.Std() <= 0 {
		errs = append(errs, errors.New("agent.timeout must be greater than zero"))
	}

	if c.Retry.Max < 0 {
		errs = append(errs, errors.New("retry.max must not be negative"))
	}
	if len(c.Retry.BackoffTicks) < c.Retry.Max {
		errs = append(errs, fmt.Errorf(
			"retry.backoff_ticks has %d entries but retry.max is %d; it needs one entry per retry",
			len(c.Retry.BackoffTicks), c.Retry.Max))
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

	return errors.Join(errs...)
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/config/... && gofmt -l internal/config && go vet ./internal/config/...`
Expected: PASS. `gofmt -l` prints nothing.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/config
git commit -m "feat(config): loop configuration loading and validation"
```

---

### Task 2: The store package

**Files:**
- Create: `internal/store/types.go`
- Create: `internal/store/store.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `store.Open(path string) (*Store, error)`
  - `(*Store).Close() error`
  - `(*Store).IssueStates(loop, repo string) (map[int]IssueState, error)`
  - `(*Store).PutIssueState(s IssueState) error`
  - `(*Store).DeleteIssueState(loop, repo string, number int) error`
  - `(*Store).CreateDispatch(d Dispatch) (int64, error)`
  - `(*Store).SetDispatchProcess(id int64, pid int, startedAt time.Time) error`
  - `(*Store).FinishDispatch(id int64, r DispatchResult) error`
  - `(*Store).RunningDispatches(loop, repo string) ([]Dispatch, error)`
  - `(*Store).GetDispatch(id int64) (Dispatch, error)`
  - `(*Store).PutPRLink(l PRLink) error`
  - `(*Store).PRLinks(loop, repo string) (map[int]PRLink, error)`
  - `(*Store).RecordTick(loop string, breakerTripped bool, summary string) (int64, error)`
  - `(*Store).TickCount(loop string) (int64, error)`
  - `(*Store).CooldownUntil(loop string) (time.Time, error)`
  - `(*Store).SetCooldown(loop string, until time.Time) error`
  - Types `IssueState`, `Dispatch`, `DispatchResult`, `PRLink`.
  - Constants `KindStart`, `KindResume`, `KindTend`, `StatusRunning`, `StatusSucceeded`, `StatusFailed`.

**review: yes** — this task creates the schema. The spec classes a schema as a new surface and a data-integrity boundary.

**Acceptance criteria:** `go test ./internal/store/...` passes against a temporary database file. A second `Open` on the same path succeeds and reads the same rows.

- [ ] **Step 1: Write the failing test**

Create `internal/store/store_test.go`:

```go
package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestIssueStateRoundTrip(t *testing.T) {
	s := openTemp(t)
	want := IssueState{
		Loop:          "planning",
		Repo:          "o/r",
		Number:        42,
		SessionID:     "sess-1",
		WorktreePath:  "/tmp/wt/issue-42",
		RetryCount:    2,
		LastRetryTick: 7,
		UpdatedAt:     time.Now().UTC().Truncate(time.Second),
	}
	if err := s.PutIssueState(want); err != nil {
		t.Fatalf("PutIssueState: %v", err)
	}

	got, err := s.IssueStates("planning", "o/r")
	if err != nil {
		t.Fatalf("IssueStates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[42].SessionID != "sess-1" || got[42].RetryCount != 2 || got[42].LastRetryTick != 7 {
		t.Errorf("round trip mismatch: %+v", got[42])
	}
}

func TestPutIssueStateIsUpsert(t *testing.T) {
	s := openTemp(t)
	base := IssueState{Loop: "planning", Repo: "o/r", Number: 1, SessionID: "a", UpdatedAt: time.Now()}
	if err := s.PutIssueState(base); err != nil {
		t.Fatal(err)
	}
	base.SessionID = "b"
	base.RetryCount = 3
	if err := s.PutIssueState(base); err != nil {
		t.Fatal(err)
	}
	got, _ := s.IssueStates("planning", "o/r")
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 after upsert", len(got))
	}
	if got[1].SessionID != "b" || got[1].RetryCount != 3 {
		t.Errorf("upsert did not overwrite: %+v", got[1])
	}
}

func TestDispatchLifecycle(t *testing.T) {
	s := openTemp(t)
	id, err := s.CreateDispatch(Dispatch{
		Loop: "planning", Repo: "o/r", Number: 5,
		Kind: KindStart, SessionID: "sess-5", LogPath: "/tmp/l.jsonl",
	})
	if err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}

	running, err := s.RunningDispatches("planning", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || running[0].Status != StatusRunning {
		t.Fatalf("running = %+v", running)
	}

	if err := s.SetDispatchProcess(id, 4242, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetDispatch(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.PID != 4242 {
		t.Errorf("PID = %d, want 4242", got.PID)
	}

	err = s.FinishDispatch(id, DispatchResult{
		Status: StatusSucceeded, ExitCode: 0, CostUSD: 1.25, DurationMS: 900,
	})
	if err != nil {
		t.Fatalf("FinishDispatch: %v", err)
	}

	running, _ = s.RunningDispatches("planning", "o/r")
	if len(running) != 0 {
		t.Errorf("running = %d, want 0 after finish", len(running))
	}
	got, _ = s.GetDispatch(id)
	if got.CostUSD != 1.25 || got.Status != StatusSucceeded {
		t.Errorf("finished dispatch = %+v", got)
	}
}

func TestPRLinkRoundTrip(t *testing.T) {
	s := openTemp(t)
	if err := s.PutPRLink(PRLink{
		Loop: "exec", Repo: "o/r", Number: 9, PRNumber: 31,
		HeadRef: "feat/x", BaseRef: "master",
	}); err != nil {
		t.Fatal(err)
	}
	links, err := s.PRLinks("exec", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if links[9].PRNumber != 31 || links[9].HeadRef != "feat/x" {
		t.Errorf("links = %+v", links)
	}
}

func TestTickCountAndCooldown(t *testing.T) {
	s := openTemp(t)
	for i := 0; i < 3; i++ {
		if _, err := s.RecordTick("planning", false, "{}"); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.TickCount("planning")
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("TickCount = %d, want 3", n)
	}

	until := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	if err := s.SetCooldown("planning", until); err != nil {
		t.Fatal(err)
	}
	got, err := s.CooldownUntil("planning")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(until) {
		t.Errorf("CooldownUntil = %v, want %v", got, until)
	}
}

func TestReopenPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.PutIssueState(IssueState{
		Loop: "planning", Repo: "o/r", Number: 1, SessionID: "keep", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, _ := s2.IssueStates("planning", "o/r")
	if got[1].SessionID != "keep" {
		t.Errorf("session did not persist across reopen: %+v", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/...`
Expected: FAIL. The package does not compile.

- [ ] **Step 3: Write `internal/store/types.go`**

```go
package store

import "time"

// Dispatch kinds.
const (
	KindStart  = "start"
	KindResume = "resume"
	KindTend   = "tend"
)

// Dispatch statuses.
const (
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// IssueState is the durable per-issue record.
type IssueState struct {
	Loop          string
	Repo          string
	Number        int
	SessionID     string
	WorktreePath  string
	RetryCount    int
	LastRetryTick int64
	UpdatedAt     time.Time
}

// Dispatch is one agent run.
type Dispatch struct {
	ID         int64
	Loop       string
	Repo       string
	Number     int
	Kind       string
	SessionID  string
	PID        int
	PIDStartAt time.Time
	Status     string
	StartedAt  time.Time
	FinishedAt time.Time
	ExitCode   int
	CostUSD    float64
	DurationMS int64
	APIError   string
	LogPath    string
	PRNumber   int
}

// DispatchResult is the outcome recorded when a dispatch ends.
type DispatchResult struct {
	Status     string
	ExitCode   int
	CostUSD    float64
	DurationMS int64
	APIError   string
}

// PRLink maps an issue to the pull request that closes it.
type PRLink struct {
	Loop     string
	Repo     string
	Number   int
	PRNumber int
	HeadRef  string
	BaseRef  string
}
```

- [ ] **Step 4: Write `internal/store/store.go`**

```go
package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS issues (
  loop            TEXT NOT NULL,
  repo            TEXT NOT NULL,
  number          INTEGER NOT NULL,
  session_id      TEXT NOT NULL DEFAULT '',
  worktree_path   TEXT NOT NULL DEFAULT '',
  retry_count     INTEGER NOT NULL DEFAULT 0,
  last_retry_tick INTEGER NOT NULL DEFAULT 0,
  updated_at      TIMESTAMP NOT NULL,
  PRIMARY KEY (loop, repo, number)
);

CREATE TABLE IF NOT EXISTS dispatches (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  loop         TEXT NOT NULL,
  repo         TEXT NOT NULL,
  number       INTEGER NOT NULL,
  kind         TEXT NOT NULL,
  session_id   TEXT NOT NULL DEFAULT '',
  pid          INTEGER NOT NULL DEFAULT 0,
  pid_start_at TIMESTAMP,
  status       TEXT NOT NULL,
  started_at   TIMESTAMP NOT NULL,
  finished_at  TIMESTAMP,
  exit_code    INTEGER NOT NULL DEFAULT 0,
  cost_usd     REAL NOT NULL DEFAULT 0,
  duration_ms  INTEGER NOT NULL DEFAULT 0,
  api_error    TEXT NOT NULL DEFAULT '',
  log_path     TEXT NOT NULL DEFAULT '',
  pr_number    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS dispatches_running
  ON dispatches (loop, repo, status);

CREATE TABLE IF NOT EXISTS pr_links (
  loop      TEXT NOT NULL,
  repo      TEXT NOT NULL,
  number    INTEGER NOT NULL,
  pr_number INTEGER NOT NULL,
  head_ref  TEXT NOT NULL,
  base_ref  TEXT NOT NULL,
  PRIMARY KEY (loop, repo, number)
);

CREATE TABLE IF NOT EXISTS ticks (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  loop            TEXT NOT NULL,
  started_at      TIMESTAMP NOT NULL,
  breaker_tripped INTEGER NOT NULL DEFAULT 0,
  summary_json    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS cooldowns (
  loop  TEXT PRIMARY KEY,
  until TIMESTAMP NOT NULL
);
`

// Store is the durable loop state.
type Store struct {
	db *sql.DB
}

// Open opens the database at path and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// The driver is not safe for unlimited parallel writers on one file.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// IssueStates returns every issue record for one loop and repository.
func (s *Store) IssueStates(loop, repo string) (map[int]IssueState, error) {
	rows, err := s.db.Query(`
		SELECT number, session_id, worktree_path, retry_count, last_retry_tick, updated_at
		FROM issues WHERE loop = ? AND repo = ?`, loop, repo)
	if err != nil {
		return nil, fmt.Errorf("query issues: %w", err)
	}
	defer rows.Close()

	out := make(map[int]IssueState)
	for rows.Next() {
		st := IssueState{Loop: loop, Repo: repo}
		if err := rows.Scan(&st.Number, &st.SessionID, &st.WorktreePath,
			&st.RetryCount, &st.LastRetryTick, &st.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan issue: %w", err)
		}
		out[st.Number] = st
	}
	return out, rows.Err()
}

// PutIssueState inserts or replaces one issue record.
func (s *Store) PutIssueState(st IssueState) error {
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(`
		INSERT INTO issues (loop, repo, number, session_id, worktree_path,
		                    retry_count, last_retry_tick, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(loop, repo, number) DO UPDATE SET
		  session_id      = excluded.session_id,
		  worktree_path   = excluded.worktree_path,
		  retry_count     = excluded.retry_count,
		  last_retry_tick = excluded.last_retry_tick,
		  updated_at      = excluded.updated_at`,
		st.Loop, st.Repo, st.Number, st.SessionID, st.WorktreePath,
		st.RetryCount, st.LastRetryTick, st.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("put issue state: %w", err)
	}
	return nil
}

// DeleteIssueState removes one issue record.
func (s *Store) DeleteIssueState(loop, repo string, number int) error {
	_, err := s.db.Exec(`DELETE FROM issues WHERE loop = ? AND repo = ? AND number = ?`,
		loop, repo, number)
	if err != nil {
		return fmt.Errorf("delete issue state: %w", err)
	}
	return nil
}

// CreateDispatch inserts a running dispatch row and returns its identifier.
func (s *Store) CreateDispatch(d Dispatch) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO dispatches (loop, repo, number, kind, session_id,
		                        status, started_at, log_path, pr_number)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Loop, d.Repo, d.Number, d.Kind, d.SessionID,
		StatusRunning, time.Now().UTC(), d.LogPath, d.PRNumber)
	if err != nil {
		return 0, fmt.Errorf("create dispatch: %w", err)
	}
	return res.LastInsertId()
}

// SetDispatchProcess records the operating system process for a dispatch.
func (s *Store) SetDispatchProcess(id int64, pid int, startedAt time.Time) error {
	_, err := s.db.Exec(`UPDATE dispatches SET pid = ?, pid_start_at = ? WHERE id = ?`,
		pid, startedAt.UTC(), id)
	if err != nil {
		return fmt.Errorf("set dispatch process: %w", err)
	}
	return nil
}

// FinishDispatch records the outcome of a dispatch.
func (s *Store) FinishDispatch(id int64, r DispatchResult) error {
	_, err := s.db.Exec(`
		UPDATE dispatches
		SET status = ?, exit_code = ?, cost_usd = ?, duration_ms = ?,
		    api_error = ?, finished_at = ?
		WHERE id = ?`,
		r.Status, r.ExitCode, r.CostUSD, r.DurationMS, r.APIError, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("finish dispatch: %w", err)
	}
	return nil
}

const dispatchColumns = `id, loop, repo, number, kind, session_id, pid, pid_start_at,
	status, started_at, finished_at, exit_code, cost_usd, duration_ms, api_error,
	log_path, pr_number`

func scanDispatch(sc interface{ Scan(...any) error }) (Dispatch, error) {
	var d Dispatch
	var pidStart, finished sql.NullTime
	err := sc.Scan(&d.ID, &d.Loop, &d.Repo, &d.Number, &d.Kind, &d.SessionID,
		&d.PID, &pidStart, &d.Status, &d.StartedAt, &finished, &d.ExitCode,
		&d.CostUSD, &d.DurationMS, &d.APIError, &d.LogPath, &d.PRNumber)
	if err != nil {
		return Dispatch{}, err
	}
	if pidStart.Valid {
		d.PIDStartAt = pidStart.Time
	}
	if finished.Valid {
		d.FinishedAt = finished.Time
	}
	return d, nil
}

// RunningDispatches returns every dispatch still marked running.
func (s *Store) RunningDispatches(loop, repo string) ([]Dispatch, error) {
	rows, err := s.db.Query(
		`SELECT `+dispatchColumns+` FROM dispatches
		 WHERE loop = ? AND repo = ? AND status = ?`, loop, repo, StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("query running dispatches: %w", err)
	}
	defer rows.Close()

	var out []Dispatch
	for rows.Next() {
		d, err := scanDispatch(rows)
		if err != nil {
			return nil, fmt.Errorf("scan dispatch: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDispatch returns one dispatch by identifier.
func (s *Store) GetDispatch(id int64) (Dispatch, error) {
	row := s.db.QueryRow(`SELECT `+dispatchColumns+` FROM dispatches WHERE id = ?`, id)
	d, err := scanDispatch(row)
	if err != nil {
		return Dispatch{}, fmt.Errorf("get dispatch %d: %w", id, err)
	}
	return d, nil
}

// PutPRLink inserts or replaces one issue-to-pull-request mapping.
func (s *Store) PutPRLink(l PRLink) error {
	_, err := s.db.Exec(`
		INSERT INTO pr_links (loop, repo, number, pr_number, head_ref, base_ref)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(loop, repo, number) DO UPDATE SET
		  pr_number = excluded.pr_number,
		  head_ref  = excluded.head_ref,
		  base_ref  = excluded.base_ref`,
		l.Loop, l.Repo, l.Number, l.PRNumber, l.HeadRef, l.BaseRef)
	if err != nil {
		return fmt.Errorf("put pr link: %w", err)
	}
	return nil
}

// PRLinks returns every issue-to-pull-request mapping for one loop.
func (s *Store) PRLinks(loop, repo string) (map[int]PRLink, error) {
	rows, err := s.db.Query(
		`SELECT number, pr_number, head_ref, base_ref FROM pr_links
		 WHERE loop = ? AND repo = ?`, loop, repo)
	if err != nil {
		return nil, fmt.Errorf("query pr links: %w", err)
	}
	defer rows.Close()

	out := make(map[int]PRLink)
	for rows.Next() {
		l := PRLink{Loop: loop, Repo: repo}
		if err := rows.Scan(&l.Number, &l.PRNumber, &l.HeadRef, &l.BaseRef); err != nil {
			return nil, fmt.Errorf("scan pr link: %w", err)
		}
		out[l.Number] = l
	}
	return out, rows.Err()
}

// RecordTick appends one tick row and returns its identifier.
func (s *Store) RecordTick(loop string, breakerTripped bool, summary string) (int64, error) {
	tripped := 0
	if breakerTripped {
		tripped = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO ticks (loop, started_at, breaker_tripped, summary_json)
		 VALUES (?, ?, ?, ?)`, loop, time.Now().UTC(), tripped, summary)
	if err != nil {
		return 0, fmt.Errorf("record tick: %w", err)
	}
	return res.LastInsertId()
}

// TickCount returns how many ticks this loop has recorded.
func (s *Store) TickCount(loop string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE loop = ?`, loop).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("tick count: %w", err)
	}
	return n, nil
}

// SetCooldown records the time before which the loop must not dispatch.
func (s *Store) SetCooldown(loop string, until time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO cooldowns (loop, until) VALUES (?, ?)
		ON CONFLICT(loop) DO UPDATE SET until = excluded.until`, loop, until.UTC())
	if err != nil {
		return fmt.Errorf("set cooldown: %w", err)
	}
	return nil
}

// CooldownUntil returns the recorded cooldown, or the zero time when none is set.
func (s *Store) CooldownUntil(loop string) (time.Time, error) {
	var t time.Time
	err := s.db.QueryRow(`SELECT until FROM cooldowns WHERE loop = ?`, loop).Scan(&t)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("cooldown until: %w", err)
	}
	return t.UTC(), nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/store/... && gofmt -l internal/store && go vet ./internal/store/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store
git commit -m "feat(store): sqlite state for issues, dispatches, pr links and ticks"
```

---

### Task 3: The GitHub read layer

**Files:**
- Create: `internal/ghub/types.go`
- Create: `internal/ghub/ghub.go`
- Test: `internal/ghub/ghub_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - Types `ghub.Issue{Number int; Title string; Labels []string; UpdatedAt time.Time}` and `ghub.PullRequest{Number int; HeadRef, BaseRef, Body string; Draft bool}`.
  - Interface `ghub.Client` with `ListOpenIssues`, `ListOpenPullRequests`, `BehindBy`, `PostComment`, `EditLabels`.
  - `ghub.New(token string) *GitHubClient`.
  - `ghub.ConvertIssues(in []*github.Issue) []Issue` — exported so the pull-request filter is unit testable.

**review: yes** — this layer crosses an external API boundary and holds the two premise-check findings in section 3.4 and 3.5 of the spec.

**Acceptance criteria:** `go test ./internal/ghub/...` passes. `ConvertIssues` drops every item for which `IsPullRequest()` is true. `HasLabel` is case insensitive.

- [ ] **Step 1: Write the failing test**

Create `internal/ghub/ghub_test.go`:

```go
package ghub

import (
	"testing"

	"github.com/google/go-github/v77/github"
)

func TestConvertIssuesDropsPullRequests(t *testing.T) {
	in := []*github.Issue{
		{
			Number: github.Ptr(1),
			Title:  github.Ptr("a real issue"),
			Labels: []*github.Label{{Name: github.Ptr("status:ready-for-spec")}},
		},
		{
			Number:           github.Ptr(2),
			Title:            github.Ptr("actually a pull request"),
			PullRequestLinks: &github.PullRequestLinks{URL: github.Ptr("https://example/pr/2")},
		},
	}

	got := ConvertIssues(in)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1; pull requests must be dropped", len(got))
	}
	if got[0].Number != 1 {
		t.Errorf("Number = %d, want 1", got[0].Number)
	}
	if len(got[0].Labels) != 1 || got[0].Labels[0] != "status:ready-for-spec" {
		t.Errorf("Labels = %v", got[0].Labels)
	}
}

func TestConvertIssuesTolerantOfNilFields(t *testing.T) {
	in := []*github.Issue{{Number: github.Ptr(7)}}
	got := ConvertIssues(in)
	if len(got) != 1 || got[0].Number != 7 {
		t.Fatalf("got = %+v", got)
	}
	if got[0].Labels == nil {
		t.Error("Labels must be a non-nil empty slice")
	}
}

func TestHasLabel(t *testing.T) {
	i := Issue{Labels: []string{"Status:Ready-For-Spec", "blocked:design"}}
	if !i.HasLabel("status:ready-for-spec") {
		t.Error("HasLabel must be case insensitive")
	}
	if !i.HasAnyLabel([]string{"nope", "blocked:design"}) {
		t.Error("HasAnyLabel must match the second entry")
	}
	if i.HasAnyLabel(nil) {
		t.Error("HasAnyLabel(nil) must be false")
	}
	if i.HasLabel("missing") {
		t.Error("HasLabel must be false for an absent label")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/ghub/...`
Expected: FAIL. The package does not compile.

- [ ] **Step 3: Write `internal/ghub/types.go`**

```go
package ghub

import (
	"strings"
	"time"
)

// Issue is the subset of a GitHub issue that the engine needs.
type Issue struct {
	Number    int
	Title     string
	Labels    []string
	UpdatedAt time.Time
}

// HasLabel reports whether the issue carries name. The comparison ignores case.
func (i Issue) HasLabel(name string) bool {
	for _, l := range i.Labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

// HasAnyLabel reports whether the issue carries any of names.
func (i Issue) HasAnyLabel(names []string) bool {
	for _, n := range names {
		if i.HasLabel(n) {
			return true
		}
	}
	return false
}

// PullRequest is the subset of a GitHub pull request that the engine needs.
type PullRequest struct {
	Number  int
	HeadRef string
	BaseRef string
	Body    string
	Draft   bool
}
```

- [ ] **Step 4: Write `internal/ghub/ghub.go`**

```go
package ghub

import (
	"context"
	"fmt"

	"github.com/google/go-github/v77/github"
)

// Client is the read surface the engine needs, plus the two writes reserved
// for the retry-cap park. Every method is safe to fake in a test.
type Client interface {
	ListOpenIssues(ctx context.Context, owner, repo string) ([]Issue, error)
	ListOpenPullRequests(ctx context.Context, owner, repo string) ([]PullRequest, error)
	BehindBy(ctx context.Context, owner, repo, base, head string) (int, error)
	PostComment(ctx context.Context, owner, repo string, number int, body string) error
	EditLabels(ctx context.Context, owner, repo string, number int, add, remove []string) error
}

// GitHubClient is the go-github backed implementation of Client.
type GitHubClient struct {
	c *github.Client
}

// New returns a client authenticated with token.
func New(token string) *GitHubClient {
	return &GitHubClient{c: github.NewClient(nil).WithAuthToken(token)}
}

// ConvertIssues maps go-github issues onto the engine type. It drops pull
// requests. The issues endpoint returns pull requests together with issues, so
// this filter is required, not optional.
func ConvertIssues(in []*github.Issue) []Issue {
	out := make([]Issue, 0, len(in))
	for _, gi := range in {
		if gi == nil || gi.IsPullRequest() {
			continue
		}
		labels := make([]string, 0, len(gi.Labels))
		for _, l := range gi.Labels {
			if l.GetName() != "" {
				labels = append(labels, l.GetName())
			}
		}
		out = append(out, Issue{
			Number:    gi.GetNumber(),
			Title:     gi.GetTitle(),
			Labels:    labels,
			UpdatedAt: gi.GetUpdatedAt().Time,
		})
	}
	return out
}

// ListOpenIssues returns every open issue in the repository.
//
// The call sends no label filter on purpose. IssueListByRepoOptions.Labels is
// an AND filter, so it cannot express "carries any of these labels". The engine
// also needs the complete label set of each issue to evaluate the veto list.
func (g *GitHubClient) ListOpenIssues(ctx context.Context, owner, repo string) ([]Issue, error) {
	opts := &github.IssueListByRepoOptions{
		State:       "open",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var all []Issue
	for {
		page, resp, err := g.c.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list issues %s/%s: %w", owner, repo, err)
		}
		all = append(all, ConvertIssues(page)...)
		if resp.NextPage == 0 {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
}

// ListOpenPullRequests returns every open pull request in the repository.
func (g *GitHubClient) ListOpenPullRequests(ctx context.Context, owner, repo string) ([]PullRequest, error) {
	opts := &github.PullRequestListOptions{
		State:       "open",
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var all []PullRequest
	for {
		page, resp, err := g.c.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list pull requests %s/%s: %w", owner, repo, err)
		}
		for _, pr := range page {
			all = append(all, PullRequest{
				Number:  pr.GetNumber(),
				HeadRef: pr.GetHead().GetRef(),
				BaseRef: pr.GetBase().GetRef(),
				Body:    pr.GetBody(),
				Draft:   pr.GetDraft(),
			})
		}
		if resp.NextPage == 0 {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
}

// BehindBy returns how many commits head lacks from base.
func (g *GitHubClient) BehindBy(ctx context.Context, owner, repo, base, head string) (int, error) {
	cmp, _, err := g.c.Repositories.CompareCommits(ctx, owner, repo, base, head, nil)
	if err != nil {
		return 0, fmt.Errorf("compare %s...%s: %w", base, head, err)
	}
	return cmp.GetBehindBy(), nil
}

// PostComment adds a comment to an issue. Task 10 is its only caller.
func (g *GitHubClient) PostComment(ctx context.Context, owner, repo string, number int, body string) error {
	_, _, err := g.c.Issues.CreateComment(ctx, owner, repo, number,
		&github.IssueComment{Body: github.Ptr(body)})
	if err != nil {
		return fmt.Errorf("comment on #%d: %w", number, err)
	}
	return nil
}

// EditLabels adds and removes labels on an issue. Task 10 is its only caller.
func (g *GitHubClient) EditLabels(ctx context.Context, owner, repo string, number int, add, remove []string) error {
	for _, name := range remove {
		if _, err := g.c.Issues.RemoveLabelForIssue(ctx, owner, repo, number, name); err != nil {
			// A label that is already absent is not an error for this caller.
			var ge *github.ErrorResponse
			if !errors.As(err, &ge) || ge.Response == nil || ge.Response.StatusCode != 404 {
				return fmt.Errorf("remove label %q from #%d: %w", name, number, err)
			}
		}
	}
	if len(add) > 0 {
		if _, _, err := g.c.Issues.AddLabelsToIssue(ctx, owner, repo, number, add); err != nil {
			return fmt.Errorf("add labels %v to #%d: %w", add, number, err)
		}
	}
	return nil
}
```

The import block for this file is:

```go
import (
	"context"
	"errors"
	"fmt"

	"github.com/google/go-github/v77/github"
)
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/ghub/... && gofmt -l internal/ghub && go vet ./internal/ghub/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/ghub
git commit -m "feat(ghub): github read layer with pull-request filtering"
```

---

### Task 4: Engine types and the pull-request linker

**Files:**
- Create: `internal/engine/types.go`
- Create: `internal/engine/prlink.go`
- Test: `internal/engine/prlink_test.go`

**Interfaces:**
- Consumes: `config.Config`, `ghub.Issue`, `ghub.PullRequest`, `store.IssueState`, `store.Dispatch`.
- Produces: `engine.Snapshot`, `engine.State`, `engine.Decision`, `engine.Plan`, `engine.Kind` constants, and `engine.LinkPR(issueNumber int, prs []ghub.PullRequest) (ghub.PullRequest, bool)`.

**review: no** — these are type declarations and one pure parser, fully covered by their own tests and by the whole-diff review.

**Acceptance criteria:** `go test ./internal/engine/...` passes. `LinkPR` matches `Closes`, `Fixes`, and `Resolves` in any case, and does not match `#12` inside `#123`.

- [ ] **Step 1: Write the failing test**

Create `internal/engine/prlink_test.go`:

```go
package engine

import (
	"testing"

	"github.com/seanmcgary/agent-utils/internal/ghub"
)

func TestLinkPR(t *testing.T) {
	prs := []ghub.PullRequest{
		{Number: 10, Body: "Closes #5", HeadRef: "feat/five", BaseRef: "master"},
		{Number: 11, Body: "unrelated work"},
		{Number: 12, Body: "fixes #123"},
		{Number: 13, Body: "Resolves #7\n\nmore text"},
	}

	cases := []struct {
		issue   int
		wantPR  int
		wantHit bool
	}{
		{5, 10, true},
		{7, 13, true},
		{123, 12, true},
		{12, 0, false},  // "#123" must not match issue 12
		{999, 0, false},
	}

	for _, c := range cases {
		got, ok := LinkPR(c.issue, prs)
		if ok != c.wantHit {
			t.Errorf("LinkPR(%d) ok = %v, want %v", c.issue, ok, c.wantHit)
			continue
		}
		if ok && got.Number != c.wantPR {
			t.Errorf("LinkPR(%d) = PR %d, want PR %d", c.issue, got.Number, c.wantPR)
		}
	}
}

func TestLinkPRIgnoresEmptyBody(t *testing.T) {
	if _, ok := LinkPR(1, []ghub.PullRequest{{Number: 2}}); ok {
		t.Error("an empty body must not link")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/engine/...`
Expected: FAIL. The package does not compile.

- [ ] **Step 3: Write `internal/engine/types.go`**

```go
package engine

import (
	"time"

	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// Kind is the type of a decision.
type Kind string

// Decision kinds.
const (
	// KindStart begins a new session for an issue.
	KindStart Kind = "start"
	// KindResume continues the stored session for an issue.
	KindResume Kind = "resume"
	// KindRetry redispatches an orphaned issue.
	KindRetry Kind = "retry"
	// KindTend rebases a stale pull request.
	KindTend Kind = "tend"
	// KindParkRetryExhausted is the one decision that writes to GitHub.
	KindParkRetryExhausted Kind = "park_retry_exhausted"
)

// Snapshot is the GitHub view for one tick.
type Snapshot struct {
	Issues []ghub.Issue
	PRs    []ghub.PullRequest
	// BehindBy maps a pull request number to how many commits it lacks.
	BehindBy map[int]int
}

// State is the stored view for one tick.
type State struct {
	Issues map[int]store.IssueState
	// Running holds every dispatch row still marked running whose process is
	// confirmed alive. The caller performs the liveness check, so Decide stays
	// pure.
	Running []store.Dispatch
	// TickCount is how many ticks this loop has recorded, including this one.
	TickCount int64
	// CooldownUntil is the time before which the loop must not dispatch.
	CooldownUntil time.Time
	// Orphans holds every dispatch row still marked running whose process is
	// confirmed dead.
	Orphans []store.Dispatch
}

// Decision is one action the tick must perform.
type Decision struct {
	Kind      Kind
	Issue     int
	PR        int
	SessionID string
	HeadRef   string
	BaseRef   string
	Reason    string
}

// Plan is the full output of one decision pass.
type Plan struct {
	Decisions      []Decision
	BreakerTripped bool
	CooldownUntil  time.Time
}
```

- [ ] **Step 4: Write `internal/engine/prlink.go`**

```go
package engine

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/seanmcgary/agent-utils/internal/ghub"
)

// closingRef matches a GitHub closing keyword and its issue number. The
// trailing boundary stops "#123" from matching issue 12.
var closingRef = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)\b`)

// LinkPR returns the open pull request whose body closes issueNumber.
// It returns false when no pull request closes the issue.
func LinkPR(issueNumber int, prs []ghub.PullRequest) (ghub.PullRequest, bool) {
	want := strconv.Itoa(issueNumber)
	for _, pr := range prs {
		if pr.Body == "" {
			continue
		}
		for _, m := range closingRef.FindAllStringSubmatch(pr.Body, -1) {
			if m[1] == want {
				return pr, true
			}
		}
	}
	return ghub.PullRequest{}, false
}

// describeLink renders a link for a log line.
func describeLink(issue int, pr ghub.PullRequest) string {
	return fmt.Sprintf("issue #%d -> PR #%d (%s...%s)", issue, pr.Number, pr.BaseRef, pr.HeadRef)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/engine/... && gofmt -l internal/engine && go vet ./internal/engine/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/engine
git commit -m "feat(engine): decision types and closing-keyword pull request linker"
```

---

### Task 5: The decision function

**Files:**
- Create: `internal/engine/engine.go`
- Test: `internal/engine/engine_test.go`

**Interfaces:**
- Consumes: `engine.Snapshot`, `engine.State`, `config.Config` from Tasks 1 and 4.
- Produces: `engine.Decide(cfg *config.Config, snap Snapshot, st State, now time.Time) Plan`.

**review: yes** — this is the state machine. Every behavior in the spec converges here, and the concurrency guard against double dispatch lives in it.

**Acceptance criteria:** `go test ./internal/engine/...` passes with every case in the table below. `Decide` performs no I/O and calls no clock other than the `now` argument.

Decision rules, in order:

1. When `now` is before `st.CooldownUntil`, return an empty plan.
2. Build the set of issue numbers that have a live dispatch. **A live dispatch is the only guard against double dispatch.** A label is not a guard, because an agent flips its own labels and may not have done so yet.
3. For each issue, skip it when it carries any veto label.
4. Skip an issue that has a live dispatch.
5. When the issue has an orphan dispatch, apply the retry rules.
6. Otherwise, when the issue carries the trigger label, start it or resume it. Resume when a stored session identifier exists.
7. When `tend_pr` is true, select stale pull requests for issues carrying the review label.
8. When the number of eligible orphans reaches the breaker threshold, drop every decision and set the cooldown.

- [ ] **Step 1: Write the failing test**

Create `internal/engine/engine_test.go`:

```go
package engine

import (
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

func testConfig() *config.Config {
	return &config.Config{
		Name: "planning",
		Repo: "o/r",
		Labels: config.Labels{
			Trigger:  "status:ready-for-spec",
			InFlight: "status:speccing",
			Blocked:  "status:needs-spec-input",
			Review:   "status:plan-ready-for-review",
			Terminal: "status:ready-for-execution",
			Veto:     []string{"blocked:design"},
		},
		Agent:  config.Agent{Model: "opus", Worktree: config.WorktreePerIssue},
		TendPR: true,
		Retry: config.Retry{
			Max:          3,
			BackoffTicks: []int{0, 1, 2},
			Breaker: config.Breaker{
				OrphanThreshold: 2,
				Cooldown:        config.Duration(30 * time.Minute),
			},
		},
	}
}

func issue(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, Labels: labels}
}

func kinds(p Plan) []Kind {
	out := make([]Kind, 0, len(p.Decisions))
	for _, d := range p.Decisions {
		out = append(out, d.Kind)
	}
	return out
}

func TestStartsNewIssue(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{Issues: map[int]store.IssueState{}}

	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindStart {
		t.Fatalf("decisions = %v, want one start", kinds(p))
	}
	if p.Decisions[0].Issue != 1 {
		t.Errorf("Issue = %d, want 1", p.Decisions[0].Issue)
	}
}

func TestResumesIssueWithStoredSession(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, cfg.Labels.Blocked)}}
	st := State{Issues: map[int]store.IssueState{
		1: {Number: 1, SessionID: "sess-1"},
	}}

	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindResume {
		t.Fatalf("decisions = %v, want one resume", kinds(p))
	}
	if p.Decisions[0].SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", p.Decisions[0].SessionID)
	}
}

func TestVetoLabelSkipsEvenWithTrigger(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger, "blocked:design")}}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none", kinds(p))
	}
}

func TestNoTriggerLabelIsSkipped(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, "some:other-label")}}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none", kinds(p))
	}
}

// A live dispatch is the guard against double dispatch, not the label. An agent
// that has not yet flipped trigger -> in_flight must not be dispatched twice.
func TestLiveDispatchBlocksSecondDispatch(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{
		Issues:  map[int]store.IssueState{1: {Number: 1, SessionID: "sess-1"}},
		Running: []store.Dispatch{{Number: 1, Kind: store.KindStart, Status: store.StatusRunning}},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none while a dispatch is live", kinds(p))
	}
}

func TestHealthyInFlightIssueProducesNothing(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	st := State{
		Issues:  map[int]store.IssueState{1: {Number: 1, SessionID: "s"}},
		Running: []store.Dispatch{{Number: 1, Status: store.StatusRunning}},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none", kinds(p))
	}
}

func TestOrphanRetriesImmediatelyOnFirstAttempt(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	st := State{
		Issues:    map[int]store.IssueState{1: {Number: 1, SessionID: "s", RetryCount: 0}},
		Orphans:   []store.Dispatch{{Number: 1, Status: store.StatusRunning}},
		TickCount: 5,
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindRetry {
		t.Fatalf("decisions = %v, want one retry", kinds(p))
	}
}

func TestOrphanWaitsInsideBackoffWindow(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	// Retry 2 needs one extra tick after the retry-1 marker at tick 5.
	st := State{
		Issues:    map[int]store.IssueState{1: {Number: 1, RetryCount: 1, LastRetryTick: 5}},
		Orphans:   []store.Dispatch{{Number: 1}},
		TickCount: 5,
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none inside the backoff window", kinds(p))
	}

	st.TickCount = 6
	p = Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindRetry {
		t.Fatalf("decisions = %v, want one retry once backoff clears", kinds(p))
	}
}

func TestOrphanParksAtRetryCap(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.InFlight)}}
	st := State{
		Issues:    map[int]store.IssueState{1: {Number: 1, RetryCount: 3, LastRetryTick: 1}},
		Orphans:   []store.Dispatch{{Number: 1}},
		TickCount: 99,
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindParkRetryExhausted {
		t.Fatalf("decisions = %v, want one park", kinds(p))
	}
}

func TestCircuitBreakerDropsEveryDecision(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{
		issue(1, cfg.Labels.InFlight),
		issue(2, cfg.Labels.InFlight),
		issue(3, cfg.Labels.Trigger),
	}}
	st := State{
		Issues: map[int]store.IssueState{
			1: {Number: 1, RetryCount: 0},
			2: {Number: 2, RetryCount: 0},
		},
		Orphans:   []store.Dispatch{{Number: 1}, {Number: 2}},
		TickCount: 10,
	}
	p := Decide(cfg, snap, st, time.Now())
	if !p.BreakerTripped {
		t.Fatal("BreakerTripped = false, want true with two eligible orphans")
	}
	if len(p.Decisions) != 0 {
		t.Errorf("decisions = %v, want none when the breaker trips", kinds(p))
	}
	if p.CooldownUntil.IsZero() {
		t.Error("CooldownUntil must be set when the breaker trips")
	}
}

func TestCooldownSuppressesEverything(t *testing.T) {
	cfg := testConfig()
	now := time.Now()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{
		Issues:        map[int]store.IssueState{},
		CooldownUntil: now.Add(10 * time.Minute),
	}
	p := Decide(cfg, snap, st, now)
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none during cooldown", kinds(p))
	}
}

func TestTendsStalePullRequest(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Labels.Review)},
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1", HeadRef: "feat/a", BaseRef: "master"}},
		BehindBy: map[int]int{20: 4},
	}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 1 || p.Decisions[0].Kind != KindTend {
		t.Fatalf("decisions = %v, want one tend", kinds(p))
	}
	d := p.Decisions[0]
	if d.PR != 20 || d.HeadRef != "feat/a" || d.BaseRef != "master" {
		t.Errorf("decision = %+v", d)
	}
}

func TestDoesNotTendCurrentPullRequest(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Labels.Review)},
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1"}},
		BehindBy: map[int]int{20: 0},
	}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none for a current pull request", kinds(p))
	}
}

func TestDoesNotTendWhenTendIsDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.TendPR = false
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Labels.Review)},
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1"}},
		BehindBy: map[int]int{20: 9},
	}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none when tend_pr is false", kinds(p))
	}
}

func TestDoesNotTendWhileTendDispatchIsLive(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{
		Issues:   []ghub.Issue{issue(1, cfg.Labels.Review)},
		PRs:      []ghub.PullRequest{{Number: 20, Body: "Closes #1"}},
		BehindBy: map[int]int{20: 4},
	}
	st := State{
		Issues:  map[int]store.IssueState{},
		Running: []store.Dispatch{{Number: 1, PRNumber: 20, Kind: store.KindTend}},
	}
	p := Decide(cfg, snap, st, time.Now())
	if len(p.Decisions) != 0 {
		t.Fatalf("decisions = %v, want none while a tend dispatch is live", kinds(p))
	}
}

func TestDecisionsAreOrderedByIssueNumber(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{
		issue(9, cfg.Labels.Trigger),
		issue(2, cfg.Labels.Trigger),
		issue(5, cfg.Labels.Trigger),
	}}
	p := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())
	if len(p.Decisions) != 3 {
		t.Fatalf("len = %d, want 3", len(p.Decisions))
	}
	want := []int{2, 5, 9}
	for i, d := range p.Decisions {
		if d.Issue != want[i] {
			t.Errorf("position %d = issue %d, want %d", i, d.Issue, want[i])
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/engine/...`
Expected: FAIL. `Decide` is not defined.

- [ ] **Step 3: Write `internal/engine/engine.go`**

```go
package engine

import (
	"fmt"
	"sort"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// Decide returns the actions for one tick. It is pure: it reads only its
// arguments, it performs no input or output, and it reads no clock. The caller
// supplies now.
func Decide(cfg *config.Config, snap Snapshot, st State, now time.Time) Plan {
	if !st.CooldownUntil.IsZero() && now.Before(st.CooldownUntil) {
		return Plan{CooldownUntil: st.CooldownUntil}
	}

	liveIssues := make(map[int]bool, len(st.Running))
	liveTendPRs := make(map[int]bool, len(st.Running))
	for _, d := range st.Running {
		if d.Kind == store.KindTend {
			liveTendPRs[d.PRNumber] = true
			continue
		}
		liveIssues[d.Number] = true
	}

	orphanIssues := make(map[int]bool, len(st.Orphans))
	for _, d := range st.Orphans {
		if d.Kind == store.KindTend {
			// A dead tend run holds no label and needs no retry bookkeeping.
			// The next tick re-evaluates the pull request from scratch.
			continue
		}
		orphanIssues[d.Number] = true
	}

	issues := append([]ghub.Issue(nil), snap.Issues...)
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })

	var decisions []Decision
	eligibleOrphans := 0

	for _, iss := range issues {
		if iss.HasAnyLabel(cfg.Labels.Veto) {
			continue
		}
		if liveIssues[iss.Number] {
			// A live dispatch is the guard against double dispatch. Labels are
			// not, because the agent owns them and may not have flipped yet.
			continue
		}

		if orphanIssues[iss.Number] {
			d, eligible := orphanDecision(cfg, iss.Number, st)
			if eligible {
				eligibleOrphans++
			}
			if d != nil {
				decisions = append(decisions, *d)
			}
			continue
		}

		if !iss.HasLabel(cfg.Labels.Trigger) {
			continue
		}

		state := st.Issues[iss.Number]
		if state.SessionID != "" {
			decisions = append(decisions, Decision{
				Kind:      KindResume,
				Issue:     iss.Number,
				SessionID: state.SessionID,
				Reason:    "trigger label present and a session exists",
			})
			continue
		}
		decisions = append(decisions, Decision{
			Kind:   KindStart,
			Issue:  iss.Number,
			Reason: "trigger label present and no session exists",
		})
	}

	// The breaker treats several orphans in one tick as a platform failure
	// rather than several unrelated crashes. It drops every decision, including
	// starts and tends, exactly as the reference documents require.
	if eligibleOrphans >= cfg.Retry.Breaker.OrphanThreshold {
		return Plan{
			BreakerTripped: true,
			CooldownUntil:  now.Add(cfg.Retry.Breaker.Cooldown.Std()),
		}
	}

	if cfg.TendPR {
		decisions = append(decisions, tendDecisions(cfg, issues, snap, liveTendPRs)...)
	}

	return Plan{Decisions: decisions}
}

// orphanDecision returns the action for one orphaned issue. The second result
// reports whether the orphan cleared its backoff window, which is what the
// circuit breaker counts.
func orphanDecision(cfg *config.Config, number int, st State) (*Decision, bool) {
	state := st.Issues[number]

	if state.RetryCount >= cfg.Retry.Max {
		return &Decision{
			Kind:   KindParkRetryExhausted,
			Issue:  number,
			Reason: fmt.Sprintf("retry cap reached (%d/%d)", state.RetryCount, cfg.Retry.Max),
		}, false
	}

	wait := 0
	if state.RetryCount < len(cfg.Retry.BackoffTicks) {
		wait = cfg.Retry.BackoffTicks[state.RetryCount]
	}
	if wait > 0 && st.TickCount-state.LastRetryTick < int64(wait) {
		// Still inside the backoff window. Take no action and post no comment.
		return nil, false
	}

	return &Decision{
		Kind:      KindRetry,
		Issue:     number,
		SessionID: state.SessionID,
		Reason:    fmt.Sprintf("orphan retry %d/%d", state.RetryCount+1, cfg.Retry.Max),
	}, true
}

// tendDecisions selects stale pull requests for issues awaiting review.
func tendDecisions(cfg *config.Config, issues []ghub.Issue, snap Snapshot, liveTendPRs map[int]bool) []Decision {
	var out []Decision
	for _, iss := range issues {
		if iss.HasAnyLabel(cfg.Labels.Veto) {
			continue
		}
		if !iss.HasLabel(cfg.Labels.Review) {
			continue
		}
		pr, ok := LinkPR(iss.Number, snap.PRs)
		if !ok {
			continue
		}
		if liveTendPRs[pr.Number] {
			continue
		}
		if snap.BehindBy[pr.Number] <= 0 {
			// A current pull request produces nothing. Silence is correct.
			continue
		}
		out = append(out, Decision{
			Kind:    KindTend,
			Issue:   iss.Number,
			PR:      pr.Number,
			HeadRef: pr.HeadRef,
			BaseRef: pr.BaseRef,
			Reason: fmt.Sprintf("%s is %d commits behind",
				describeLink(iss.Number, pr), snap.BehindBy[pr.Number]),
		})
	}
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/engine/... -v && gofmt -l internal/engine && go vet ./internal/engine/...`
Expected: PASS for every test.

- [ ] **Step 5: Commit**

```bash
git add internal/engine
git commit -m "feat(engine): pure decision function for start, resume, retry and tend"
```

---

### Task 6: Process liveness and the loop lock

**Files:**
- Create: `internal/proc/proc.go`
- Create: `internal/lock/lock.go`
- Test: `internal/proc/proc_test.go`
- Test: `internal/lock/lock_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `proc.IsAlive(pid int, dispatchID int64) bool`
  - `proc.CommandLine(pid int) (string, error)`
  - `lock.Acquire(path string) (*Lock, error)`; `(*Lock).Release() error`; `lock.ErrHeld`.

**review: yes** — this task guards against two concurrent ticks dispatching the same issue. That is a concurrency boundary.

**Acceptance criteria:** `go test ./internal/proc/... ./internal/lock/...` passes. A second `Acquire` on a held lock returns `lock.ErrHeld`. `IsAlive` returns false for a process identifier that exists but belongs to a different program.

Design note. A process identifier alone is not proof of identity, because the operating system reuses identifiers. This implementation confirms identity by reading the command line of the process and matching the dispatch identifier that the runner puts in its own arguments. That check needs no extra dependency and works on macOS and Linux.

- [ ] **Step 1: Write the failing tests**

Create `internal/proc/proc_test.go`:

```go
package proc

import (
	"os"
	"os/exec"
	"testing"
)

func TestIsAliveFalseForDeadProcess(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if IsAlive(cmd.Process.Pid, 1) {
		t.Error("IsAlive = true for an exited process")
	}
}

func TestIsAliveFalseForUnrelatedProcess(t *testing.T) {
	// This test process is alive, but its arguments do not contain the
	// dispatch marker, so it must not be mistaken for a dispatch runner.
	if IsAlive(os.Getpid(), 987654) {
		t.Error("IsAlive = true for a live but unrelated process")
	}
}

func TestIsAliveFalseForImpossiblePID(t *testing.T) {
	if IsAlive(-1, 1) {
		t.Error("IsAlive = true for pid -1")
	}
	if IsAlive(0, 1) {
		t.Error("IsAlive = true for pid 0")
	}
}

func TestCommandLineReturnsOwnName(t *testing.T) {
	out, err := CommandLine(os.Getpid())
	if err != nil {
		t.Fatalf("CommandLine: %v", err)
	}
	if out == "" {
		t.Error("CommandLine returned an empty string for this process")
	}
}
```

Create `internal/lock/lock_test.go`:

```go
package lock

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquireAndRelease(t *testing.T) {
	p := filepath.Join(t.TempDir(), "loop.lock")

	l, err := Acquire(p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	l2, err := Acquire(p)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	l2.Release()
}

func TestSecondAcquireReportsHeld(t *testing.T) {
	p := filepath.Join(t.TempDir(), "loop.lock")

	l, err := Acquire(p)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()

	_, err = Acquire(p)
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("second Acquire error = %v, want ErrHeld", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/proc/... ./internal/lock/...`
Expected: FAIL. Neither package compiles.

- [ ] **Step 3: Write `internal/proc/proc.go`**

```go
// Package proc reports whether a dispatch runner process is still alive.
package proc

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// DispatchFlag is the argument the runner carries. Liveness matches on it, so
// a reused process identifier cannot be mistaken for a live runner.
const DispatchFlag = "--dispatch"

// CommandLine returns the full command line of pid.
func CommandLine(pid int) (string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", fmt.Errorf("read command line of %d: %w", pid, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// IsAlive reports whether pid is a live runner for dispatchID.
//
// Two checks run. The first asks the kernel whether the process exists. The
// second confirms the process is the expected runner, because the operating
// system reuses process identifiers and an unrelated program could hold this
// one.
func IsAlive(pid int, dispatchID int64) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	cmdline, err := CommandLine(pid)
	if err != nil {
		return false
	}
	marker := DispatchFlag + " " + strconv.FormatInt(dispatchID, 10)
	return strings.Contains(cmdline, marker)
}
```

- [ ] **Step 4: Write `internal/lock/lock.go`**

```go
// Package lock provides a per-loop advisory file lock. It stops two cron ticks
// from dispatching the same issue.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrHeld reports that another process holds the lock.
var ErrHeld = errors.New("lock is held by another process")

// Lock is an acquired advisory lock.
type Lock struct {
	f *os.File
}

// Acquire takes an exclusive non-blocking lock on path.
// It returns ErrHeld when another process already holds it.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrHeld
		}
		return nil, fmt.Errorf("flock: %w", err)
	}
	return &Lock{f: f}, nil
}

// Release unlocks and closes the lock file.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	if err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN); err != nil {
		l.f.Close()
		return fmt.Errorf("unlock: %w", err)
	}
	return l.f.Close()
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/proc/... ./internal/lock/... && gofmt -l internal/proc internal/lock && go vet ./internal/proc/... ./internal/lock/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/proc internal/lock
git commit -m "feat(proc,lock): process identity checks and the per-loop tick lock"
```

---

### Task 7: Git worktrees

**Files:**
- Create: `internal/worktree/worktree.go`
- Test: `internal/worktree/worktree_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `worktree.Manager` with `New(checkoutBaseDir, worktreeDir, loop string) *Manager`
  - `(*Manager).PathForIssue(number int) string`
  - `(*Manager).PathForPR(number int) string`
  - `(*Manager).EnsureIssue(number int) (string, error)`
  - `(*Manager).EnsurePR(number int, headRef string) (string, error)`
  - `(*Manager).Remove(path string) error`
  - `(*Manager).Fetch() error`

**review: no** — this task shells out to git with fixed arguments. Its correctness is covered by its own integration test and by the whole-diff review.

**Acceptance criteria:** `go test ./internal/worktree/...` passes against a real temporary git repository. `EnsureIssue` is idempotent: a second call returns the same path and creates nothing.

- [ ] **Step 1: Write the failing test**

Create `internal/worktree/worktree_test.go`:

```go
package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "master")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "initial")
	return dir
}

func TestEnsureIssueCreatesWorktreeAndIsIdempotent(t *testing.T) {
	repo := initRepo(t)
	wtDir := filepath.Join(t.TempDir(), "worktrees")
	m := New(repo, wtDir, "planning")

	path, err := m.EnsureIssue(42)
	if err != nil {
		t.Fatalf("EnsureIssue: %v", err)
	}
	want := filepath.Join(wtDir, "planning", "issue-42")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	if _, err := os.Stat(filepath.Join(path, "f.txt")); err != nil {
		t.Errorf("worktree does not contain the repository file: %v", err)
	}

	again, err := m.EnsureIssue(42)
	if err != nil {
		t.Fatalf("second EnsureIssue: %v", err)
	}
	if again != path {
		t.Errorf("second call returned %q, want %q", again, path)
	}
}

func TestRemoveDeletesWorktree(t *testing.T) {
	repo := initRepo(t)
	wtDir := filepath.Join(t.TempDir(), "worktrees")
	m := New(repo, wtDir, "planning")

	path, err := m.EnsureIssue(7)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("worktree still exists after Remove")
	}
}

func TestPathHelpers(t *testing.T) {
	m := New("/repo", "/wt", "exec")
	if got := m.PathForIssue(3); got != "/wt/exec/issue-3" {
		t.Errorf("PathForIssue = %q", got)
	}
	if got := m.PathForPR(9); got != "/wt/exec/pr-9" {
		t.Errorf("PathForPR = %q", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/worktree/...`
Expected: FAIL. The package does not compile.

- [ ] **Step 3: Write `internal/worktree/worktree.go`**

```go
// Package worktree manages the git worktrees that dispatched agents run in.
package worktree

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Manager creates worktrees for one loop from one primary checkout.
type Manager struct {
	checkoutBaseDir string
	worktreeDir     string
	loop            string
}

// New returns a Manager.
func New(checkoutBaseDir, worktreeDir, loop string) *Manager {
	return &Manager{
		checkoutBaseDir: checkoutBaseDir,
		worktreeDir:     worktreeDir,
		loop:            loop,
	}
}

// PathForIssue returns the stable worktree path for an issue.
func (m *Manager) PathForIssue(number int) string {
	return filepath.Join(m.worktreeDir, m.loop, fmt.Sprintf("issue-%d", number))
}

// PathForPR returns the stable worktree path for a pull request.
func (m *Manager) PathForPR(number int) string {
	return filepath.Join(m.worktreeDir, m.loop, fmt.Sprintf("pr-%d", number))
}

// Fetch updates the primary checkout. It never changes its branch and never
// edits its files.
func (m *Manager) Fetch() error {
	return m.git(m.checkoutBaseDir, "fetch", "origin", "--prune")
}

// EnsureIssue creates the worktree for an issue if it does not exist. The path
// is stable across ticks, so a resumed run finds the branch state it left.
func (m *Manager) EnsureIssue(number int) (string, error) {
	path := m.PathForIssue(number)
	if exists(path) {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create worktree parent: %w", err)
	}
	branch := fmt.Sprintf("agent-utils/%s/issue-%d", m.loop, number)
	if err := m.git(m.checkoutBaseDir, "worktree", "add", "-B", branch, path); err != nil {
		return "", err
	}
	return path, nil
}

// EnsurePR creates the worktree for a pull request and checks out its head
// branch.
func (m *Manager) EnsurePR(number int, headRef string) (string, error) {
	path := m.PathForPR(number)
	if exists(path) {
		if err := m.git(path, "checkout", headRef); err != nil {
			return "", err
		}
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create worktree parent: %w", err)
	}
	err := m.git(m.checkoutBaseDir, "worktree", "add", path,
		"--track", "-B", headRef, "origin/"+headRef)
	if err != nil {
		return "", err
	}
	return path, nil
}

// Remove deletes a worktree and its directory.
func (m *Manager) Remove(path string) error {
	if !exists(path) {
		return nil
	}
	if err := m.git(m.checkoutBaseDir, "worktree", "remove", "--force", path); err != nil {
		// Fall back to a plain delete plus a prune, so a corrupt registration
		// cannot strand the directory forever.
		if rmErr := os.RemoveAll(path); rmErr != nil {
			return fmt.Errorf("remove worktree %s: %w", path, err)
		}
		return m.git(m.checkoutBaseDir, "worktree", "prune")
	}
	return nil
}

func (m *Manager) git(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s in %s: %w: %s",
			strings.Join(args, " "), dir, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/worktree/... && gofmt -l internal/worktree && go vet ./internal/worktree/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/worktree
git commit -m "feat(worktree): stable per-issue and per-pull-request worktrees"
```

---

### Task 8: The claude argv builder and result parser

**Files:**
- Create: `internal/runner/args.go`
- Create: `internal/runner/result.go`
- Test: `internal/runner/args_test.go`
- Test: `internal/runner/result_test.go`

**Interfaces:**
- Consumes: `config.Config`.
- Produces:
  - `runner.Invocation{SessionID, Prompt string; Resume bool}`
  - `runner.BuildArgs(cfg *config.Config, inv Invocation) []string`
  - `runner.Result{SessionID string; CostUSD float64; DurationMS int64; IsError bool; APIError string; Text string}`
  - `runner.ParseStream(r io.Reader) (Result, error)`
  - `runner.RenderPrompt(tmpl string, data PromptData) (string, error)` and `runner.PromptData`.

**review: yes** — a wrong flag here silently changes agent behavior, and the `--verbose` requirement below was found by running the binary, not by reading documentation.

**Acceptance criteria:** `go test ./internal/runner/...` passes. `BuildArgs` always emits `--verbose` together with `--output-format stream-json`. `BuildArgs` emits `--session-id` for a start and `-r` for a resume, never both. `ParseStream` reads the last `type:"result"` line.

Verified facts, from running the binary:

- `claude -p --output-format stream-json` fails with `Error: When using --print, --output-format=stream-json requires --verbose`. The `--verbose` flag is mandatory.
- With `--verbose`, the stream ends with one line of `{"type":"result","subtype":"success",...}`. That line carries `session_id`, `total_cost_usd`, `duration_ms`, `is_error`, `api_error_status`, and `result`.

- [ ] **Step 1: Write the failing tests**

Create `internal/runner/args_test.go`:

```go
package runner

import (
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
)

func cfg() *config.Config {
	return &config.Config{
		Agent: config.Agent{
			Model:          "opus",
			Effort:         "high",
			PermissionMode: "bypassPermissions",
			MaxBudgetUSD:   25,
		},
	}
}

func joined(args []string) string { return strings.Join(args, " ") }

func TestBuildArgsAlwaysIncludesVerbose(t *testing.T) {
	// Verified by running the binary: --output-format stream-json under
	// --print is rejected without --verbose.
	args := BuildArgs(cfg(), Invocation{SessionID: "s1", Prompt: "go"})
	j := joined(args)
	if !strings.Contains(j, "--output-format stream-json") {
		t.Errorf("missing stream-json: %s", j)
	}
	if !strings.Contains(j, "--verbose") {
		t.Errorf("stream-json without --verbose is rejected by claude: %s", j)
	}
}

func TestBuildArgsStartUsesSessionID(t *testing.T) {
	args := BuildArgs(cfg(), Invocation{SessionID: "s1", Prompt: "go"})
	j := joined(args)
	if !strings.Contains(j, "--session-id s1") {
		t.Errorf("missing --session-id: %s", j)
	}
	if strings.Contains(j, " -r ") {
		t.Errorf("a start must not resume: %s", j)
	}
}

func TestBuildArgsResumeUsesResumeFlag(t *testing.T) {
	args := BuildArgs(cfg(), Invocation{SessionID: "s1", Prompt: "go", Resume: true})
	j := joined(args)
	if !strings.Contains(j, "-r s1") {
		t.Errorf("missing -r: %s", j)
	}
	if strings.Contains(j, "--session-id") {
		t.Errorf("a resume must not assign a new session id: %s", j)
	}
}

func TestBuildArgsCarriesAgentSettings(t *testing.T) {
	j := joined(BuildArgs(cfg(), Invocation{SessionID: "s", Prompt: "p"}))
	for _, want := range []string{
		"--model opus",
		"--effort high",
		"--permission-mode bypassPermissions",
		"--max-budget-usd 25",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in %s", want, j)
		}
	}
}

func TestBuildArgsOmitsEmptyOptionalSettings(t *testing.T) {
	c := cfg()
	c.Agent.Effort = ""
	c.Agent.PermissionMode = ""
	c.Agent.MaxBudgetUSD = 0
	j := joined(BuildArgs(c, Invocation{SessionID: "s", Prompt: "p"}))
	for _, bad := range []string{"--effort", "--permission-mode", "--max-budget-usd"} {
		if strings.Contains(j, bad) {
			t.Errorf("unset option %q must be omitted: %s", bad, j)
		}
	}
}

func TestBuildArgsPutsPromptLast(t *testing.T) {
	args := BuildArgs(cfg(), Invocation{SessionID: "s", Prompt: "the prompt"})
	if args[len(args)-1] != "the prompt" {
		t.Errorf("last argument = %q, want the prompt", args[len(args)-1])
	}
}

func TestRenderPrompt(t *testing.T) {
	got, err := RenderPrompt("issue {{.Issue.Number}} in {{.Repo}}", PromptData{
		Repo:  "o/r",
		Issue: PromptIssue{Number: 12},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "issue 12 in o/r" {
		t.Errorf("got %q", got)
	}
}

func TestRenderPromptRejectsUnknownField(t *testing.T) {
	if _, err := RenderPrompt("{{.Nope}}", PromptData{}); err == nil {
		t.Fatal("want an error for an unknown template field")
	}
}
```

Create `internal/runner/result_test.go`:

```go
package runner

import (
	"strings"
	"testing"
)

const streamFixture = `{"type":"system","subtype":"init","session_id":"abc"}
{"type":"assistant"}
{"type":"result","subtype":"success","session_id":"abc","total_cost_usd":0.0145,"duration_ms":2519,"is_error":false,"api_error_status":null,"result":"done"}
`

func TestParseStreamReadsResultLine(t *testing.T) {
	got, err := ParseStream(strings.NewReader(streamFixture))
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if got.SessionID != "abc" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if got.CostUSD != 0.0145 {
		t.Errorf("CostUSD = %v", got.CostUSD)
	}
	if got.DurationMS != 2519 {
		t.Errorf("DurationMS = %d", got.DurationMS)
	}
	if got.IsError {
		t.Error("IsError = true, want false")
	}
	if got.Text != "done" {
		t.Errorf("Text = %q", got.Text)
	}
}

func TestParseStreamCapturesAPIError(t *testing.T) {
	line := `{"type":"result","subtype":"error","is_error":true,"api_error_status":"529"}`
	got, err := ParseStream(strings.NewReader(line))
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsError || got.APIError != "529" {
		t.Errorf("got %+v", got)
	}
}

func TestParseStreamIgnoresNonJSONNoise(t *testing.T) {
	body := "warning: something\n" + streamFixture
	got, err := ParseStream(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if got.SessionID != "abc" {
		t.Errorf("noise broke the parse: %+v", got)
	}
}

func TestParseStreamErrorsWhenNoResultLine(t *testing.T) {
	if _, err := ParseStream(strings.NewReader(`{"type":"assistant"}`)); err == nil {
		t.Fatal("want an error when the stream holds no result line")
	}
}

func TestParseStreamHandlesVeryLongLines(t *testing.T) {
	// bufio.Scanner has a 64 KiB default limit. A real stream easily exceeds it,
	// so the parser must raise the buffer.
	big := strings.Repeat("x", 300000)
	body := `{"type":"assistant","text":"` + big + `"}` + "\n" + streamFixture
	got, err := ParseStream(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if got.SessionID != "abc" {
		t.Errorf("long line broke the parse: %+v", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/runner/...`
Expected: FAIL. The package does not compile.

- [ ] **Step 3: Write `internal/runner/args.go`**

```go
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
```

- [ ] **Step 4: Write `internal/runner/result.go`**

```go
package runner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Result is the outcome of one claude run.
type Result struct {
	SessionID  string
	CostUSD    float64
	DurationMS int64
	IsError    bool
	APIError   string
	Text       string
}

// ErrNoResult reports that the stream held no result line.
var ErrNoResult = errors.New("stream contains no result line")

type resultLine struct {
	Type           string  `json:"type"`
	SessionID      string  `json:"session_id"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	DurationMS     int64   `json:"duration_ms"`
	IsError        bool    `json:"is_error"`
	APIErrorStatus *string `json:"api_error_status"`
	ResultText     string  `json:"result"`
}

// ParseStream reads a claude stream-json stream and returns the final result.
//
// The parser keeps the last line whose type is "result". It ignores every other
// line and every line that is not JSON, because the stream also carries system,
// assistant, and rate limit events, and a wrapper can print plain text.
func ParseStream(r io.Reader) (Result, error) {
	sc := bufio.NewScanner(r)
	// A single stream line can be far larger than the 64 KiB default.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var last *resultLine
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rl resultLine
		if err := json.Unmarshal(line, &rl); err != nil {
			continue
		}
		if rl.Type != "result" {
			continue
		}
		copied := rl
		last = &copied
	}
	if err := sc.Err(); err != nil {
		return Result{}, fmt.Errorf("read stream: %w", err)
	}
	if last == nil {
		return Result{}, ErrNoResult
	}

	out := Result{
		SessionID:  last.SessionID,
		CostUSD:    last.TotalCostUSD,
		DurationMS: last.DurationMS,
		IsError:    last.IsError,
		Text:       last.ResultText,
	}
	if last.APIErrorStatus != nil {
		out.APIError = *last.APIErrorStatus
	}
	return out, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/runner/... && gofmt -l internal/runner && go vet ./internal/runner/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/runner
git commit -m "feat(runner): claude argv builder and stream-json result parser"
```

---

### Task 9: Detached spawn and child supervision

**Files:**
- Create: `internal/runner/runner.go`
- Test: `internal/runner/runner_test.go`

**Interfaces:**
- Consumes: `runner.BuildArgs`, `runner.ParseStream`, `store.Store`, `config.Config`.
- Produces:
  - `runner.Spawn(selfPath string, dispatchID int64, configPath string) (int, error)`
  - `runner.Supervise(ctx context.Context, cfg *config.Config, st *store.Store, d store.Dispatch, inv Invocation, workDir, logPath string) error`

**review: yes** — this task spawns processes and owns the only path that records a dispatch outcome. A bug here strands dispatch rows in the running state forever.

**Acceptance criteria:** `go test ./internal/runner/...` passes. The supervision test uses a stub `claude` on `PATH` that prints a fixture stream, and it confirms the dispatch row moves to `succeeded` with the recorded cost.

- [ ] **Step 1: Write the failing test**

Create `internal/runner/runner_test.go`:

```go
package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// stubClaude writes a fake claude onto PATH. It prints a stream-json stream and
// exits with the given code.
func stubClaude(t *testing.T, exitCode int, body string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOF'\n" + body + "\nEOF\nexit " + strconv.Itoa(exitCode) + "\n"
	p := filepath.Join(dir, "claude")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSuperviseRecordsSuccess(t *testing.T) {
	stubClaude(t, 0, `{"type":"result","subtype":"success","session_id":"abc","total_cost_usd":0.5,"duration_ms":1200,"is_error":false,"result":"ok"}`)

	s := newStore(t)
	id, err := s.CreateDispatch(store.Dispatch{
		Loop: "planning", Repo: "o/r", Number: 1,
		Kind: store.KindStart, SessionID: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDispatch(id)

	logPath := filepath.Join(t.TempDir(), "run.jsonl")
	cfg := &config.Config{Agent: config.Agent{Model: "opus", Timeout: config.Duration(60e9)}}

	err = Supervise(context.Background(), cfg, s, d,
		Invocation{SessionID: "abc", Prompt: "go"}, t.TempDir(), logPath)
	if err != nil {
		t.Fatalf("Supervise: %v", err)
	}

	got, _ := s.GetDispatch(id)
	if got.Status != store.StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", got.Status)
	}
	if got.CostUSD != 0.5 {
		t.Errorf("CostUSD = %v, want 0.5", got.CostUSD)
	}
	if got.DurationMS != 1200 {
		t.Errorf("DurationMS = %d, want 1200", got.DurationMS)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("log file was not written: %v", err)
	}
}

func TestSuperviseRecordsFailureOnNonZeroExit(t *testing.T) {
	stubClaude(t, 1, `{"type":"result","subtype":"error","is_error":true,"api_error_status":"529"}`)

	s := newStore(t)
	id, _ := s.CreateDispatch(store.Dispatch{
		Loop: "planning", Repo: "o/r", Number: 2, Kind: store.KindStart, SessionID: "x",
	})
	d, _ := s.GetDispatch(id)

	cfg := &config.Config{Agent: config.Agent{Model: "opus", Timeout: config.Duration(60e9)}}
	_ = Supervise(context.Background(), cfg, s, d,
		Invocation{SessionID: "x", Prompt: "go"}, t.TempDir(),
		filepath.Join(t.TempDir(), "run.jsonl"))

	got, _ := s.GetDispatch(id)
	if got.Status != store.StatusFailed {
		t.Errorf("Status = %q, want failed", got.Status)
	}
	if got.APIError != "529" {
		t.Errorf("APIError = %q, want 529", got.APIError)
	}
}

func TestSuperviseRecordsFailureWhenStreamHasNoResult(t *testing.T) {
	stubClaude(t, 0, `{"type":"assistant"}`)

	s := newStore(t)
	id, _ := s.CreateDispatch(store.Dispatch{
		Loop: "planning", Repo: "o/r", Number: 3, Kind: store.KindStart, SessionID: "y",
	})
	d, _ := s.GetDispatch(id)

	cfg := &config.Config{Agent: config.Agent{Model: "opus", Timeout: config.Duration(60e9)}}
	_ = Supervise(context.Background(), cfg, s, d,
		Invocation{SessionID: "y", Prompt: "go"}, t.TempDir(),
		filepath.Join(t.TempDir(), "run.jsonl"))

	got, _ := s.GetDispatch(id)
	if got.Status != store.StatusFailed {
		t.Errorf("Status = %q, want failed when no result line is present", got.Status)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/runner/...`
Expected: FAIL. `Supervise` is not defined.

- [ ] **Step 3: Write `internal/runner/runner.go`**

```go
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// Spawn starts a detached runner process for one dispatch and returns its
// process identifier.
//
// The tick process must exit quickly, because cron starts it, but the agent
// runs for a long time. Some process must therefore outlive the tick to record
// how the agent ended. The runner is this program invoked again, so it survives
// the tick and writes the outcome itself.
func Spawn(selfPath string, dispatchID int64, configPath string) (int, error) {
	cmd := exec.Command(selfPath, "internal", "run-agent",
		proc.DispatchFlag, strconv.FormatInt(dispatchID, 10),
		"--config", configPath)

	// Detach from the tick: a new session, and no inherited standard streams.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawn runner for dispatch %d: %w", dispatchID, err)
	}
	// Release the child so it is not left as a zombie when the tick exits.
	if err := cmd.Process.Release(); err != nil {
		return cmd.Process.Pid, fmt.Errorf("release runner process: %w", err)
	}
	return cmd.Process.Pid, nil
}

// Supervise runs claude for one dispatch, writes the stream to logPath, and
// records the outcome. It always records an outcome, so no dispatch row is left
// in the running state.
func Supervise(
	ctx context.Context,
	cfg *config.Config,
	st *store.Store,
	d store.Dispatch,
	inv Invocation,
	workDir string,
	logPath string,
) error {
	if timeout := cfg.Agent.Timeout.Std(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return finish(st, d.ID, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1,
			APIError: fmt.Sprintf("create log directory: %v", err),
		})
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return finish(st, d.ID, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1,
			APIError: fmt.Sprintf("create log file: %v", err),
		})
	}
	defer logFile.Close()

	cmd := exec.CommandContext(ctx, "claude", BuildArgs(cfg, inv)...)
	cmd.Dir = workDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return finish(st, d.ID, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1,
			APIError: fmt.Sprintf("stdout pipe: %v", err),
		})
	}
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		return finish(st, d.ID, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1,
			APIError: fmt.Sprintf("start claude: %v", err),
		})
	}

	// Tee the stream to the log file and parse it at the same time, so one read
	// serves both the record and the operator.
	tee := io.TeeReader(stdout, logFile)
	result, parseErr := ParseStream(tee)

	waitErr := cmd.Wait()

	res := store.DispatchResult{
		Status:     store.StatusSucceeded,
		CostUSD:    result.CostUSD,
		DurationMS: result.DurationMS,
		APIError:   result.APIError,
	}

	switch {
	case waitErr != nil:
		res.Status = store.StatusFailed
		res.ExitCode = exitCodeOf(waitErr)
		if res.APIError == "" {
			res.APIError = waitErr.Error()
		}
	case parseErr != nil:
		// A clean exit with no result line means the run did not complete in a
		// way this program can record. Treat it as a failure so the next tick
		// retries it.
		res.Status = store.StatusFailed
		res.ExitCode = -1
		if res.APIError == "" {
			res.APIError = parseErr.Error()
		}
	case result.IsError:
		res.Status = store.StatusFailed
	}

	return finish(st, d.ID, res)
}

func finish(st *store.Store, id int64, res store.DispatchResult) error {
	if err := st.FinishDispatch(id, res); err != nil {
		return fmt.Errorf("record dispatch %d: %w", id, err)
	}
	if res.Status == store.StatusFailed {
		return fmt.Errorf("dispatch %d failed: exit %d: %s", id, res.ExitCode, res.APIError)
	}
	return nil
}

func exitCodeOf(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// LogPath returns the log file path for one dispatch.
func LogPath(stateDir, loop string, number int, kind string, now time.Time) string {
	name := fmt.Sprintf("%s-%d-%s.jsonl", kind, number, now.UTC().Format("20060102T150405Z"))
	return filepath.Join(stateDir, "logs", loop, name)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/runner/... && gofmt -l internal/runner && go vet ./internal/runner/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/runner
git commit -m "feat(runner): detached spawn and child supervision that always records an outcome"
```

---

### Task 10: Tick orchestration, the commands, and the one GitHub write

**Files:**
- Create: `internal/loopcmd/tick.go`
- Create: `internal/loopcmd/status.go`
- Create: `cmd/agent-utils/main.go`
- Create: `examples/planning.yaml`
- Create: `examples/execution.yaml`
- Create: `README.md`
- Test: `internal/loopcmd/tick_test.go`

**Interfaces:**
- Consumes: every package from Tasks 1 through 9.
- Produces:
  - `loopcmd.Tick(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error)`
  - `loopcmd.Deps{Store *store.Store; GH ghub.Client; WT *worktree.Manager; SelfPath, ConfigPath string; Now func() time.Time; Spawn func(string, int64, string) (int, error)}`
  - `loopcmd.Status(ctx context.Context, cfg *config.Config, deps Deps) (string, error)`
  - `loopcmd.RunAgent(ctx context.Context, cfg *config.Config, deps Deps, dispatchID int64) error`
  - `loopcmd.Reset(cfg *config.Config, s *store.Store, wt *worktree.Manager, number int) error`

**review: yes** — this task holds the only GitHub write and the ordering that makes a tick safe to repeat.

**Acceptance criteria:** `go test ./internal/loopcmd/...` passes with a fake `ghub.Client` and a fake spawn function. A tick with one triggered issue creates one dispatch row and calls spawn once. A second tick, while that dispatch is live, calls spawn zero times. `go build ./...` succeeds.

The one GitHub write. When `Decide` returns `KindParkRetryExhausted`, the tick posts a comment and moves the issue from `in_flight` to `blocked`. This is the single exception to the rule that Go never writes to GitHub. The reference documents make the same exception, and for the same reason: the failing action is the dispatch itself, so an agent dispatched to report the failure would fail the same way.

- [ ] **Step 1: Write the failing test**

Create `internal/loopcmd/tick_test.go`:

```go
package loopcmd

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/seanmcgary/agent-utils/internal/worktree"
)

type fakeGH struct {
	issues   []ghub.Issue
	prs      []ghub.PullRequest
	behind   map[int]int
	comments []string
	added    []string
	removed  []string
}

func (f *fakeGH) ListOpenIssues(context.Context, string, string) ([]ghub.Issue, error) {
	return f.issues, nil
}
func (f *fakeGH) ListOpenPullRequests(context.Context, string, string) ([]ghub.PullRequest, error) {
	return f.prs, nil
}
func (f *fakeGH) BehindBy(_ context.Context, _, _, _, head string) (int, error) {
	for _, pr := range f.prs {
		if pr.HeadRef == head {
			return f.behind[pr.Number], nil
		}
	}
	return 0, nil
}
func (f *fakeGH) PostComment(_ context.Context, _, _ string, _ int, body string) error {
	f.comments = append(f.comments, body)
	return nil
}
func (f *fakeGH) EditLabels(_ context.Context, _, _ string, _ int, add, remove []string) error {
	f.added = append(f.added, add...)
	f.removed = append(f.removed, remove...)
	return nil
}

func tickConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	return &config.Config{
		Name:            "planning",
		Repo:            "o/r",
		CheckoutBaseDir: dir,
		WorktreeDir:     filepath.Join(dir, "wt"),
		StateDir:        filepath.Join(dir, "state"),
		Labels: config.Labels{
			Trigger:  "trigger",
			InFlight: "in-flight",
			Blocked:  "blocked",
			Review:   "review",
			Terminal: "terminal",
		},
		Agent: config.Agent{
			Model: "opus", Worktree: config.WorktreeNone, Timeout: config.Duration(time.Hour),
		},
		Retry: config.Retry{
			Max: 3, BackoffTicks: []int{0, 1, 2},
			Breaker: config.Breaker{OrphanThreshold: 2, Cooldown: config.Duration(30 * time.Minute)},
		},
		Prompt:       "plan #{{.Issue.Number}}",
		ResumePrompt: "resume #{{.Issue.Number}}",
	}
}

func newDeps(t *testing.T, cfg *config.Config, gh ghub.Client, spawned *int) Deps {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	return Deps{
		Store:      s,
		GH:         gh,
		WT:         worktree.New(cfg.CheckoutBaseDir, cfg.WorktreeDir, cfg.Name),
		SelfPath:   "/bin/true",
		ConfigPath: "/tmp/loop.yaml",
		Now:        time.Now,
		Spawn: func(string, int64, string) (int, error) {
			*spawned++
			return 4242, nil
		},
		SkipFetch: true,
	}
}

func TestTickStartsTriggeredIssue(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if spawned != 1 {
		t.Errorf("spawned = %d, want 1", spawned)
	}
	if sum.Started != 1 {
		t.Errorf("Started = %d, want 1", sum.Started)
	}

	running, _ := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if len(running) != 1 {
		t.Fatalf("running dispatches = %d, want 1", len(running))
	}
	states, _ := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if states[1].SessionID == "" {
		t.Error("a session identifier must be stored for a started issue")
	}
}

func TestTickDoesNotDoubleDispatchWhileRunning(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"trigger"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	// The stub spawn returns pid 4242, which is not a live runner, so the
	// second tick must treat it as an orphan rather than as a fresh start.
	// Either way it must not start the issue a second time.
	before := spawned
	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	states, _ := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if states[1].RetryCount == 0 && spawned != before {
		t.Errorf("issue was started twice: spawned went %d -> %d", before, spawned)
	}
}

func TestTickPostsCommentAndLabelsAtRetryCap(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"in-flight"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	// An orphan at the cap: a running dispatch row whose process is dead.
	id, _ := deps.Store.CreateDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, Kind: store.KindStart, SessionID: "s",
	})
	_ = deps.Store.SetDispatchProcess(id, 999999, time.Now())
	_ = deps.Store.PutIssueState(store.IssueState{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, SessionID: "s",
		RetryCount: 3, UpdatedAt: time.Now(),
	})

	if _, err := Tick(context.Background(), cfg, deps); err != nil {
		t.Fatal(err)
	}
	if len(gh.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(gh.comments))
	}
	if spawned != 0 {
		t.Errorf("spawned = %d, want 0 at the retry cap", spawned)
	}
	if len(gh.added) != 1 || gh.added[0] != cfg.Labels.Blocked {
		t.Errorf("added = %v, want [%s]", gh.added, cfg.Labels.Blocked)
	}
	if len(gh.removed) != 1 || gh.removed[0] != cfg.Labels.InFlight {
		t.Errorf("removed = %v, want [%s]", gh.removed, cfg.Labels.InFlight)
	}
}

func TestTickIsQuietWhenNothingMatches(t *testing.T) {
	cfg := tickConfig(t)
	gh := &fakeGH{issues: []ghub.Issue{{Number: 1, Labels: []string{"unrelated"}}}}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if spawned != 0 || len(gh.comments) != 0 {
		t.Errorf("a quiet tick must produce nothing: spawned=%d comments=%d",
			spawned, len(gh.comments))
	}
	if sum.Started != 0 || sum.Resumed != 0 || sum.Tended != 0 {
		t.Errorf("summary = %+v, want all zero", sum)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/loopcmd/...`
Expected: FAIL. The package does not compile.

- [ ] **Step 3: Write `internal/loopcmd/tick.go`**

```go
// Package loopcmd holds the tick orchestration and the operator commands.
package loopcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/runner"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/seanmcgary/agent-utils/internal/worktree"
)

// Deps holds everything a tick needs. Each field is replaceable in a test.
type Deps struct {
	Store      *store.Store
	GH         ghub.Client
	WT         *worktree.Manager
	SelfPath   string
	ConfigPath string
	Now        func() time.Time
	Spawn      func(selfPath string, dispatchID int64, configPath string) (int, error)
	// SkipFetch turns off the git fetch. Tests set it.
	SkipFetch bool
}

// Summary reports what one tick did.
type Summary struct {
	Started        int  `json:"started"`
	Resumed        int  `json:"resumed"`
	Retried        int  `json:"retried"`
	Tended         int  `json:"tended"`
	Parked         int  `json:"parked"`
	Live           int  `json:"live"`
	Orphans        int  `json:"orphans"`
	BreakerTripped bool `json:"breaker_tripped"`
}

// Tick runs one reconcile and dispatch pass.
func Tick(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error) {
	var sum Summary
	now := deps.Now()

	if !deps.SkipFetch {
		if err := deps.WT.Fetch(); err != nil {
			// A failed fetch makes every branch comparison stale. Stop here.
			return sum, fmt.Errorf("fetch primary checkout: %w", err)
		}
	}

	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	issues, err := deps.GH.ListOpenIssues(ctx, owner, repo)
	if err != nil {
		return sum, err
	}

	snap := engine.Snapshot{Issues: issues, BehindBy: map[int]int{}}
	if cfg.TendPR {
		prs, err := deps.GH.ListOpenPullRequests(ctx, owner, repo)
		if err != nil {
			return sum, err
		}
		snap.PRs = prs
		for _, iss := range issues {
			if !iss.HasLabel(cfg.Labels.Review) {
				continue
			}
			pr, ok := engine.LinkPR(iss.Number, prs)
			if !ok {
				continue
			}
			behind, err := deps.GH.BehindBy(ctx, owner, repo, pr.BaseRef, pr.HeadRef)
			if err != nil {
				return sum, err
			}
			snap.BehindBy[pr.Number] = behind
			_ = deps.Store.PutPRLink(store.PRLink{
				Loop: cfg.Name, Repo: cfg.Repo, Number: iss.Number,
				PRNumber: pr.Number, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef,
			})
		}
	}

	states, err := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if err != nil {
		return sum, err
	}
	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		return sum, err
	}

	// Split running rows into live and dead by asking the operating system.
	// This is the fact the LLM orchestrator could not obtain, and it is why the
	// retry policy here needs no marker comments.
	st := engine.State{Issues: states, CooldownUntil: time.Time{}}
	for _, d := range running {
		if proc.IsAlive(d.PID, d.ID) {
			st.Running = append(st.Running, d)
			continue
		}
		st.Orphans = append(st.Orphans, d)
		// A dead runner must not stay running forever.
		_ = deps.Store.FinishDispatch(d.ID, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1, APIError: "runner process died",
		})
	}
	sum.Live = len(st.Running)
	sum.Orphans = len(st.Orphans)

	if st.CooldownUntil, err = deps.Store.CooldownUntil(cfg.Name); err != nil {
		return sum, err
	}
	if st.TickCount, err = deps.Store.TickCount(cfg.Name); err != nil {
		return sum, err
	}

	plan := engine.Decide(cfg, snap, st, now)
	sum.BreakerTripped = plan.BreakerTripped

	if plan.BreakerTripped {
		if err := deps.Store.SetCooldown(cfg.Name, plan.CooldownUntil); err != nil {
			return sum, err
		}
		slog.Warn("circuit breaker tripped; skipping all dispatch",
			"loop", cfg.Name, "cooldown_until", plan.CooldownUntil)
	}

	for _, d := range plan.Decisions {
		if err := act(ctx, cfg, deps, d, issues, now, &sum); err != nil {
			// One failed decision must not abandon the rest of the tick.
			slog.Error("decision failed", "loop", cfg.Name, "kind", d.Kind,
				"issue", d.Issue, "err", err)
		}
	}

	body, _ := json.Marshal(sum)
	if _, err := deps.Store.RecordTick(cfg.Name, plan.BreakerTripped, string(body)); err != nil {
		return sum, err
	}
	slog.Info("tick complete", "loop", cfg.Name, "summary", string(body))
	return sum, nil
}

func act(
	ctx context.Context,
	cfg *config.Config,
	deps Deps,
	d engine.Decision,
	issues []ghub.Issue,
	now time.Time,
	sum *Summary,
) error {
	switch d.Kind {
	case engine.KindParkRetryExhausted:
		sum.Parked++
		return parkRetryExhausted(ctx, cfg, deps, d)
	case engine.KindTend:
		sum.Tended++
		return dispatch(ctx, cfg, deps, d, issues, now, store.KindTend)
	case engine.KindResume:
		sum.Resumed++
		return dispatch(ctx, cfg, deps, d, issues, now, store.KindResume)
	case engine.KindRetry:
		sum.Retried++
		return dispatch(ctx, cfg, deps, d, issues, now, store.KindResume)
	case engine.KindStart:
		sum.Started++
		return dispatch(ctx, cfg, deps, d, issues, now, store.KindStart)
	default:
		return fmt.Errorf("unknown decision kind %q", d.Kind)
	}
}

func dispatch(
	ctx context.Context,
	cfg *config.Config,
	deps Deps,
	d engine.Decision,
	issues []ghub.Issue,
	now time.Time,
	kind string,
) error {
	state := store.IssueState{Loop: cfg.Name, Repo: cfg.Repo, Number: d.Issue}
	if existing, err := deps.Store.IssueStates(cfg.Name, cfg.Repo); err == nil {
		if s, ok := existing[d.Issue]; ok {
			state = s
		}
	}

	sessionID := d.SessionID
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	// A tend run keeps no memory between runs, because a rebase is idempotent.
	if kind == store.KindTend {
		sessionID = uuid.NewString()
	}

	workDir := cfg.CheckoutBaseDir
	if cfg.Agent.Worktree == config.WorktreePerIssue {
		var err error
		if kind == store.KindTend {
			workDir, err = deps.WT.EnsurePR(d.PR, d.HeadRef)
		} else {
			workDir, err = deps.WT.EnsureIssue(d.Issue)
		}
		if err != nil {
			return err
		}
	}

	logPath := runner.LogPath(cfg.StateDir, cfg.Name, d.Issue, kind, now)

	dispatchID, err := deps.Store.CreateDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: d.Issue, Kind: kind,
		SessionID: sessionID, LogPath: logPath, PRNumber: d.PR,
	})
	if err != nil {
		return err
	}

	pid, err := deps.Spawn(deps.SelfPath, dispatchID, deps.ConfigPath)
	if err != nil {
		_ = deps.Store.FinishDispatch(dispatchID, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1, APIError: err.Error(),
		})
		return err
	}
	if err := deps.Store.SetDispatchProcess(dispatchID, pid, now); err != nil {
		return err
	}

	if kind != store.KindTend {
		state.SessionID = sessionID
		state.WorktreePath = workDir
		state.UpdatedAt = now
		if d.Kind == engine.KindRetry {
			state.RetryCount++
			state.LastRetryTick, _ = deps.Store.TickCount(cfg.Name)
		}
		if err := deps.Store.PutIssueState(state); err != nil {
			return err
		}
	}

	slog.Info("dispatched", "loop", cfg.Name, "kind", kind, "issue", d.Issue,
		"pr", d.PR, "dispatch", dispatchID, "pid", pid, "session", sessionID,
		"reason", d.Reason)
	return nil
}

const retryCapComment = `🔁 **Orphan retry cap reached (%d/%d)** — %d consecutive agent dispatches for this issue failed to complete. This usually indicates a sustained platform-side issue rather than a problem with the issue itself. Parking here rather than retrying indefinitely.

To proceed: re-add the ` + "`%s`" + ` label once the underlying issue has cleared, and this resumes normally.`

// parkRetryExhausted is the ONE GitHub write this program performs. Every other
// comment and label change belongs to the dispatched agent. The exception
// exists because the failing action is the dispatch itself, so an agent sent to
// report the failure would fail the same way.
func parkRetryExhausted(ctx context.Context, cfg *config.Config, deps Deps, d engine.Decision) error {
	owner, repo := cfg.RepoOwner(), cfg.RepoName()
	body := fmt.Sprintf(retryCapComment,
		cfg.Retry.Max, cfg.Retry.Max, cfg.Retry.Max, cfg.Labels.Trigger)

	if err := deps.GH.PostComment(ctx, owner, repo, d.Issue, body); err != nil {
		return err
	}
	if err := deps.GH.EditLabels(ctx, owner, repo, d.Issue,
		[]string{cfg.Labels.Blocked}, []string{cfg.Labels.InFlight}); err != nil {
		return err
	}
	slog.Warn("parked at retry cap", "loop", cfg.Name, "issue", d.Issue)
	return nil
}

// RunAgent executes one dispatch. The detached runner process calls it.
func RunAgent(ctx context.Context, cfg *config.Config, deps Deps, dispatchID int64) error {
	d, err := deps.Store.GetDispatch(dispatchID)
	if err != nil {
		return err
	}

	tmpl := cfg.Prompt
	resume := false
	switch d.Kind {
	case store.KindResume:
		tmpl, resume = cfg.ResumePrompt, true
	case store.KindTend:
		tmpl = cfg.TendPrompt
	}

	link, _ := deps.Store.PRLinks(cfg.Name, cfg.Repo)
	pr := link[d.Number]

	workDir := cfg.CheckoutBaseDir
	if cfg.Agent.Worktree == config.WorktreePerIssue {
		if d.Kind == store.KindTend {
			workDir = deps.WT.PathForPR(d.PRNumber)
		} else {
			workDir = deps.WT.PathForIssue(d.Number)
		}
	}

	prompt, err := runner.RenderPrompt(tmpl, runner.PromptData{
		Repo:      cfg.Repo,
		Loop:      cfg.Name,
		SessionID: d.SessionID,
		Worktree:  workDir,
		Issue:     runner.PromptIssue{Number: d.Number},
		PR: runner.PromptPR{
			Number: d.PRNumber, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef,
		},
		Labels: runner.PromptLabels{
			Trigger:  cfg.Labels.Trigger,
			InFlight: cfg.Labels.InFlight,
			Blocked:  cfg.Labels.Blocked,
			Review:   cfg.Labels.Review,
			Terminal: cfg.Labels.Terminal,
		},
	})
	if err != nil {
		_ = deps.Store.FinishDispatch(dispatchID, store.DispatchResult{
			Status: store.StatusFailed, ExitCode: -1, APIError: err.Error(),
		})
		return err
	}

	return runner.Supervise(ctx, cfg, deps.Store, d,
		runner.Invocation{SessionID: d.SessionID, Prompt: prompt, Resume: resume},
		workDir, d.LogPath)
}

// Reset drops the stored session and worktree for one issue, so the next tick
// starts it clean.
func Reset(cfg *config.Config, s *store.Store, wt *worktree.Manager, number int) error {
	if err := wt.Remove(wt.PathForIssue(number)); err != nil {
		return err
	}
	return s.DeleteIssueState(cfg.Name, cfg.Repo, number)
}
```

- [ ] **Step 4: Write `internal/loopcmd/status.go`**

```go
package loopcmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// Status renders the reconciled view. It changes nothing.
func Status(ctx context.Context, cfg *config.Config, deps Deps) (string, error) {
	issues, err := deps.GH.ListOpenIssues(ctx, cfg.RepoOwner(), cfg.RepoName())
	if err != nil {
		return "", err
	}
	states, err := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if err != nil {
		return "", err
	}
	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		return "", err
	}
	ticks, err := deps.Store.TickCount(cfg.Name)
	if err != nil {
		return "", err
	}
	cooldown, err := deps.Store.CooldownUntil(cfg.Name)
	if err != nil {
		return "", err
	}

	live := map[int]store.Dispatch{}
	dead := map[int]store.Dispatch{}
	for _, d := range running {
		if proc.IsAlive(d.PID, d.ID) {
			live[d.Number] = d
		} else {
			dead[d.Number] = d
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "loop %s  repo %s  ticks %d\n", cfg.Name, cfg.Repo, ticks)
	if !cooldown.IsZero() {
		fmt.Fprintf(&b, "cooldown until %s\n", cooldown.Format("2006-01-02 15:04:05Z"))
	}
	fmt.Fprintln(&b)

	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	fmt.Fprintf(&b, "%-6s %-14s %-9s %-38s %s\n",
		"ISSUE", "STATE", "RETRIES", "SESSION", "WORKTREE")

	for _, iss := range issues {
		state := "-"
		switch {
		case iss.HasAnyLabel(cfg.Labels.Veto):
			state = "veto"
		case iss.HasLabel(cfg.Labels.InFlight):
			state = "in-flight"
			if _, ok := dead[iss.Number]; ok {
				state = "ORPHAN"
			}
		case iss.HasLabel(cfg.Labels.Trigger):
			state = "queued"
		case iss.HasLabel(cfg.Labels.Blocked):
			state = "blocked"
		case iss.HasLabel(cfg.Labels.Review):
			state = "in-review"
		default:
			continue
		}
		s := states[iss.Number]
		session := s.SessionID
		if session == "" {
			session = "-"
		}
		wt := s.WorktreePath
		if wt == "" {
			wt = "-"
		}
		fmt.Fprintf(&b, "%-6d %-14s %-9d %-38s %s\n",
			iss.Number, state, s.RetryCount, session, wt)
	}

	fmt.Fprintf(&b, "\nlive dispatches: %d   orphaned: %d\n", len(live), len(dead))
	return b.String(), nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/loopcmd/...`
Expected: PASS.

- [ ] **Step 6: Write `cmd/agent-utils/main.go`**

```go
// Command agent-utils holds utilities for agent workflows.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/loopcmd"
	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/runner"
	"github.com/seanmcgary/agent-utils/internal/store"
	"github.com/seanmcgary/agent-utils/internal/worktree"
	"github.com/urfave/cli/v3"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	cmd := &cli.Command{
		Name:  "agent-utils",
		Usage: "utilities for agent workflows",
		Commands: []*cli.Command{
			loopCommand(),
			internalCommand(),
		},
	}
	if err := cmd.Run(context.Background(), os.Args); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func configFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:     "config",
		Usage:    "path to the loop configuration file",
		Required: true,
	}
}

func loopCommand() *cli.Command {
	return &cli.Command{
		Name:  "loop",
		Usage: "run and inspect an issue-driven agent loop",
		Commands: []*cli.Command{
			{
				Name:  "tick",
				Usage: "run one reconcile and dispatch pass, then exit",
				Flags: []cli.Flag{configFlag()},
				Action: func(ctx context.Context, c *cli.Command) error {
					cfg, deps, cleanup, err := setup(c.String("config"))
					if err != nil {
						return err
					}
					defer cleanup()

					l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
					if err == lock.ErrHeld {
						slog.Info("another tick is running; exiting", "loop", cfg.Name)
						return nil
					}
					if err != nil {
						return err
					}
					defer l.Release()

					_, err = loopcmd.Tick(ctx, cfg, deps)
					return err
				},
			},
			{
				Name:  "status",
				Usage: "print the reconciled view without changing anything",
				Flags: []cli.Flag{configFlag()},
				Action: func(ctx context.Context, c *cli.Command) error {
					cfg, deps, cleanup, err := setup(c.String("config"))
					if err != nil {
						return err
					}
					defer cleanup()

					out, err := loopcmd.Status(ctx, cfg, deps)
					if err != nil {
						return err
					}
					fmt.Print(out)
					return nil
				},
			},
			{
				Name:  "reset",
				Usage: "drop the stored session and worktree for one issue",
				Flags: []cli.Flag{
					configFlag(),
					&cli.IntFlag{Name: "issue", Usage: "issue number", Required: true},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					cfg, deps, cleanup, err := setup(c.String("config"))
					if err != nil {
						return err
					}
					defer cleanup()
					return loopcmd.Reset(cfg, deps.Store, deps.WT, c.Int("issue"))
				},
			},
		},
	}
}

func internalCommand() *cli.Command {
	return &cli.Command{
		Name:   "internal",
		Usage:  "internal commands; not for direct use",
		Hidden: true,
		Commands: []*cli.Command{
			{
				Name:  "run-agent",
				Usage: "run one dispatch and record its outcome",
				Flags: []cli.Flag{
					configFlag(),
					&cli.IntFlag{Name: "dispatch", Usage: "dispatch id", Required: true},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					cfg, deps, cleanup, err := setup(c.String("config"))
					if err != nil {
						return err
					}
					defer cleanup()
					return loopcmd.RunAgent(ctx, cfg, deps, int64(c.Int("dispatch")))
				},
			},
		},
	}
}

func setup(configPath string) (*config.Config, loopcmd.Deps, func(), error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, loopcmd.Deps{}, nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, loopcmd.Deps{}, nil, fmt.Errorf("create state directory: %w", err)
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, loopcmd.Deps{}, nil, fmt.Errorf("GITHUB_TOKEN is not set")
	}

	s, err := store.Open(filepath.Join(cfg.StateDir, "state.db"))
	if err != nil {
		return nil, loopcmd.Deps{}, nil, err
	}

	self, err := os.Executable()
	if err != nil {
		s.Close()
		return nil, loopcmd.Deps{}, nil, fmt.Errorf("locate this executable: %w", err)
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		s.Close()
		return nil, loopcmd.Deps{}, nil, fmt.Errorf("resolve config path: %w", err)
	}

	deps := loopcmd.Deps{
		Store:      s,
		GH:         ghub.New(token),
		WT:         worktree.New(cfg.CheckoutBaseDir, cfg.WorktreeDir, cfg.Name),
		SelfPath:   self,
		ConfigPath: abs,
		Now:        time.Now,
		Spawn:      runner.Spawn,
	}
	_ = proc.DispatchFlag // keep the dependency explicit for readers
	return cfg, deps, func() { s.Close() }, nil
}
```

- [ ] **Step 7: Write the example configurations and the README**

Create `examples/planning.yaml` and `examples/execution.yaml` using the label roles from the two reference documents. Create `README.md` with the install command, a cron line, and the three operator commands. Include this cron example verbatim:

```cron
*/15 * * * * GITHUB_TOKEN=... /usr/local/bin/agent-utils loop tick --config ~/.agent-utils/planning.yaml >> ~/.agent-utils/planning.log 2>&1
```

- [ ] **Step 8: Verify the whole build and every test**

Run:

```bash
go build ./... && go vet ./... && gofmt -l . && go test ./...
```

Expected: the build succeeds, vet is silent, `gofmt -l` prints nothing, and every test passes.

- [ ] **Step 9: Commit**

```bash
git add cmd internal/loopcmd examples README.md go.mod go.sum
git commit -m "feat(cli): tick orchestration, status, reset and the detached run-agent command"
```

---

## Pipeline State

| Field   | Value                                                        |
|---------|--------------------------------------------------------------|
| stage   | 2 (plan review)                                              |
| class   | large (new subsystem, new schema, process supervision)        |
| profile | backend                                                      |
| branch  | feat/loop-engine                                             |
| pr      | #1                                                           |
| gate    | pending                                                      |
| round   | 0                                                            |
