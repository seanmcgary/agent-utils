# Session kill and label overrides — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator stop a running session from the command line, and let a label on an issue override the agent model, harness, or effort for that issue.

**Architecture:** A new per-issue `stopped` flag is the one mechanism both features use. `engine.Decide` refuses to dispatch a stopped issue. `sessions kill` sets the flag and then signals the runner; `sessions resume` clears it. `config.ParseOverrides` reads three label prefixes; an invalid label makes `Decide` stop the issue instead of dispatching it. A valid override rides on the dispatch row into the detached runner, where one `runner.Effective` helper feeds the argument builders.

**Tech Stack:** Go 1.x, `urfave/cli/v3`, `modernc.org/sqlite`, `gopkg.in/yaml.v3`. Tests are standard `go test`.

**Spec:** `docs/superpowers/specs/2026-08-26-session-kill-and-label-overrides-design.md`

**Class:** Large. **Profile:** backend.

## Global Constraints

This repository has no root conventions document. The binding rules below were
read out of the code; each names where it is enforced.

- **`make check` is the gate.** It runs `fmtcheck`, `vet` (twice — the second
  with `GOOS=darwin`), `lint` (golangci-lint v2.5.0), and `test`
  (`Makefile:170`).
- **Tests run with `-p 1` and `-count=1`.** The `worktree` package shells out to
  real git and the `runner` package spawns real processes (`Makefile:120`).
- **Every `Config` yaml field must be documented** in `docs/configuration.md`,
  enforced by `TestEveryConfigFieldIsDocumented`
  (`internal/config/docs_test.go:14`). This plan adds no yaml field, so the test
  should keep passing unchanged. If a task adds one, that task documents it.
- **`parkRetryExhausted` is the ONE GitHub write this program performs**
  (`internal/loopcmd/tick.go:493`). No task in this plan may add another.
- **Every `Store` read and write is project-scoped.** `Store` carries a
  `projectID` (`internal/store/store.go:180`); `DB` methods are the only
  machine-wide reads. `db.Project(id)` makes a scoped `Store`
  (`internal/store/store.go:225`).
- **A new column is added through `addedColumns`, never by editing `CREATE
  TABLE` alone** (`internal/store/store.go:314`). `CREATE TABLE IF NOT EXISTS`
  does nothing to an existing database.
- **Comment style:** this codebase writes comments that explain *why*, often at
  length, and names the failure the code prevents. Match it. Do not write
  comments that restate the code.

## Verified external API (do not re-derive)

Read out of the source on 2026-08-26.

```go
// internal/store/store.go:225
func (d *DB) Project(projectID string) *Store

// internal/store/store.go:830 — the SELECT list every dispatch read shares
const dispatchColumns = `id, project_id, loop, repo, number, kind, session_id, pid,
	pid_start_at, status, started_at, finished_at, exit_code, cost_usd, duration_ms,
	api_error, log_path, pr_number, title, legacy_source, legacy_id`

// internal/store/store.go:834 — scanDispatch scans that list, in that order
func scanDispatch(sc interface{ Scan(...any) error }) (Dispatch, error)

// internal/store/store.go:776
func (s *Store) CreateDispatch(d Dispatch) (int64, error)
// internal/store/store.go:790
func (s *Store) SetDispatchProcess(id int64, pid int, startedAt time.Time) error
// internal/store/store.go:802
func (s *Store) FinishDispatch(id int64, r DispatchResult) error
// internal/store/store.go:1038 — machine-wide, unscoped
func (d *DB) RunningDispatches() ([]Dispatch, error)
// internal/store/store.go:715
func (s *Store) IssueState(loop, repo string, number int) (IssueState, error)

// internal/proc/proc.go:34
func IsAlive(pid int, dispatchID int64) bool
// internal/proc/proc.go:13
const DispatchFlag = "--dispatch"

// internal/store/types.go:99 — the id the runner process actually carries
func (d Dispatch) RunnerID() int64

// internal/engine/engine.go:17
func Decide(cfg *config.Config, snap Snapshot, st State, now time.Time) Plan

// internal/runner/runner.go:26 (BuildArgs at :26 of args.go)
func BuildArgs(cfg *config.Config, inv Invocation) []string
func PiBuildArgs(cfg *config.Config, inv Invocation) []string
// internal/runner/runner.go:301
func Finish(cfg *config.Config, st *store.Store, d store.Dispatch, res store.DispatchResult, now time.Time) error

// internal/config/discover.go:189 — loop name to config path
func Resolve(agentUtilsDir, name string) (string, error)
// internal/registry/registry.go:31 — has ID, Name, Root, AgentUtilsDir
func Find(selector string) (Project, error)
func List() ([]Project, error)

// internal/loopcmd/sessions.go — the existing session view
type Session struct { ID, ProjectID, Project, Loop string; Issue int; /* ... */ Live, Orphaned bool }
func AllSessions(f SessionFilter) ([]Session, error)
type SessionFilter struct { Project, Loop string; Running, Orphaned bool }
```

Signal facts, confirmed by reading:

- The runner is spawned with `Setsid` (`internal/runner/runner.go:46`), so it
  leads its own session and its own process group.
- The agent child is started with `Setpgid: true`
  (`internal/runner/runner.go:154`), so it leads a *different* process group.
  A signal to the runner's group does not reach the agent.
- `cmd.Cancel` already SIGTERMs the agent's group on context cancel
  (`internal/runner/runner.go:157`), and a SIGKILL sweep already follows `Wait`
  (`internal/runner/runner.go:215`).
- `main` uses `context.Background()` (`cmd/agent-utils/main.go:49`). Nothing in
  the runner path calls `signal.NotifyContext`.

---

## File map

| File | Responsibility |
|------|----------------|
| `internal/config/overrides.go` (new) | `Overrides`, `ParseOverrides`, the label syntax and its validation. |
| `internal/config/overrides_test.go` (new) | Table tests for every valid case and every error. |
| `internal/proc/signal.go` (new) | `Signal`, `SignalGroup` — signalling guarded by the dispatch check. |
| `internal/store/store.go` | Six new columns, the rebuild list, `dispatchColumns`, `scanDispatch`, `CreateDispatch`, and four new methods. |
| `internal/store/types.go` | `IssueState.Stopped`, `StoppedReason`; `Dispatch.AgentPID`, `Model`, `Harness`, `Effort`. |
| `internal/engine/types.go` | `KindStop`, `Decision.Overrides`. |
| `internal/engine/engine.go` | The stopped-issue skip and the invalid-label stop. |
| `internal/runner/args.go` | `Settings`, `Effective`, `Invocation.Overrides`; both arg builders read the effective settings. |
| `internal/runner/runner.go` | Record `agent_pid`; read the effective harness. |
| `internal/loopcmd/kill.go` (new) | `Kill`, `Resume`, target resolution, and the ordered kill procedure. |
| `internal/loopcmd/tick.go` | Persist overrides on the row; apply a `KindStop` decision; pass overrides to the invocation. |
| `internal/loopcmd/sessions.go` | The `STOPPED` state in both renderers. |
| `cmd/agent-utils/main.go` | `sessions kill`, `sessions resume`, and the runner's signal handler. |
| `docs/configuration.md`, `README.md` | The label syntax, the commands, and the tend limit. |

---

## Task 1: The override parser

**review: yes** — this is the security boundary. Rule 3 must not be dropped.

**Files:**
- Create: `internal/config/overrides.go`
- Create: `internal/config/overrides_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `config.Overrides{Model, Harness, Effort string}` and
  `config.ParseOverrides(labels []string) (Overrides, error)`. Tasks 4, 5, and 6
  depend on both names exactly as spelled.

- [ ] **Step 1: Write the failing test**

Create `internal/config/overrides_test.go`:

```go
package config

import "testing"

func TestParseOverrides(t *testing.T) {
	tests := []struct {
		name    string
		labels  []string
		want    Overrides
		wantErr string
	}{
		{
			name:   "no override labels",
			labels: []string{"status:ready", "bug"},
			want:   Overrides{},
		},
		{
			name:   "all three",
			labels: []string{"model:claude-opus-5", "harness:pi", "effort:high"},
			want:   Overrides{Model: "claude-opus-5", Harness: "pi", Effort: "high"},
		},
		{
			name:   "the prefix ignores case, the value keeps it",
			labels: []string{"Model:Claude-Opus-5"},
			want:   Overrides{Model: "Claude-Opus-5"},
		},
		{
			name:    "an empty value names nothing",
			labels:  []string{"model:"},
			wantErr: "names no value",
		},
		{
			name:    "whitespace in a value",
			labels:  []string{"model:claude opus"},
			wantErr: "contains whitespace",
		},
		{
			name:    "a value that reads as a flag",
			labels:  []string{"model:--dangerously-skip-permissions"},
			wantErr: "starts with \"-\"",
		},
		{
			name:    "two labels with one prefix",
			labels:  []string{"model:a", "model:b"},
			wantErr: "carries two",
		},
		{
			name:    "an unknown harness",
			labels:  []string{"harness:gpt"},
			wantErr: "harness must be",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseOverrides(tt.labels)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseOverrides(%q) = %+v, want an error", tt.labels, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseOverrides(%q): %v", tt.labels, err)
			}
			if got != tt.want {
				t.Fatalf("ParseOverrides(%q) = %+v, want %+v", tt.labels, got, tt.want)
			}
		})
	}
}

// A rejected value must never reach an argument list. This is the security
// rule, so it is asserted on its own rather than only inside the table.
func TestParseOverridesRejectsEveryFlagShapedValue(t *testing.T) {
	for _, v := range []string{"-p", "--model", "-", "--"} {
		if _, err := ParseOverrides([]string{"model:" + v}); err == nil {
			t.Fatalf("ParseOverrides accepted the flag-shaped value %q", v)
		}
	}
}
```

Add `"strings"` to the test file's imports.

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/config/ -run TestParseOverrides -count=1`
Expected: FAIL — `undefined: ParseOverrides`.

- [ ] **Step 3: Write the implementation**

Create `internal/config/overrides.go`:

```go
package config

import (
	"fmt"
	"strings"
	"unicode"
)

// Override label prefixes. They are fixed rather than configurable: an
// operator types them on an issue, and a per-loop spelling would mean the same
// label meant different things in two loops of one project.
const (
	OverrideModelPrefix   = "model:"
	OverrideHarnessPrefix = "harness:"
	OverrideEffortPrefix  = "effort:"
)

// Overrides holds the agent settings an issue's labels replace. An empty field
// means "no override", never "the empty value".
type Overrides struct {
	Model   string
	Harness string
	Effort  string
}

// Empty reports that no label overrode anything.
func (o Overrides) Empty() bool { return o == Overrides{} }

// ParseOverrides reads the agent overrides from an issue's labels.
//
// It is the ONLY place that knows this syntax. The tick parses labels, the
// engine validates the result against the loop's configuration, and the runner
// receives values that were already checked here; none of them re-implement
// the rule.
//
// Every error is worded for a human reading `loop status`, because an invalid
// label stops the issue and this text is the whole explanation they get.
func ParseOverrides(labels []string) (Overrides, error) {
	var out Overrides
	// Track which prefix set which field. A second label with the same prefix
	// is an error rather than a silent last-one-wins: two people adding
	// "model:" labels must not get an answer that depends on GitHub's ordering.
	seen := map[string]string{}

	for _, l := range labels {
		lower := strings.ToLower(l)
		var field *string
		var prefix string
		switch {
		case strings.HasPrefix(lower, OverrideModelPrefix):
			field, prefix = &out.Model, OverrideModelPrefix
		case strings.HasPrefix(lower, OverrideHarnessPrefix):
			field, prefix = &out.Harness, OverrideHarnessPrefix
		case strings.HasPrefix(lower, OverrideEffortPrefix):
			field, prefix = &out.Effort, OverrideEffortPrefix
		default:
			continue
		}
		if first, ok := seen[prefix]; ok {
			return Overrides{}, fmt.Errorf(
				"the issue carries two %q labels, %q and %q; remove one",
				strings.TrimSuffix(prefix, ":"), first, l)
		}
		seen[prefix] = l

		// Slice the ORIGINAL label, not the lowered copy. The prefix is matched
		// without case, but a model identifier is case-sensitive, so the value
		// must survive exactly as it was written.
		value := l[len(prefix):]
		if err := validOverrideValue(prefix, value); err != nil {
			return Overrides{}, err
		}
		*field = value
	}

	if out.Harness != "" && out.Harness != HarnessClaude && out.Harness != HarnessPi {
		return Overrides{}, fmt.Errorf(
			"label %q selects harness %q; harness must be %q or %q",
			OverrideHarnessPrefix+out.Harness, out.Harness, HarnessClaude, HarnessPi)
	}
	return out, nil
}

// validOverrideValue rejects a value that must never reach an argument list.
//
// The value becomes one element of the list handed to exec (see
// runner.BuildArgs). Go passes an argument list rather than a shell string, so
// there is no quoting or metacharacter hazard here -- but a value that starts
// with "-" is read by the agent binary as a FLAG rather than as a model name.
// "model:--dangerously-skip-permissions" is the case this rule exists for.
//
// Whitespace is rejected for the same family of reasons: a value with a space
// in it is either a mistake or an attempt to smuggle a second argument, and no
// legitimate model, harness or effort value contains one.
func validOverrideValue(prefix, value string) error {
	label := prefix + value
	switch {
	case value == "":
		return fmt.Errorf("label %q names no value", label)
	case strings.IndexFunc(value, unicode.IsSpace) >= 0:
		return fmt.Errorf("label %q contains whitespace", label)
	case strings.HasPrefix(value, "-"):
		return fmt.Errorf(
			"label %q starts with \"-\", which the agent reads as a flag", label)
	}
	return nil
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `go test ./internal/config/ -run TestParseOverrides -count=1`
Expected: PASS.

- [ ] **Step 5: Run the whole package**

Run: `go test ./internal/config/ -count=1`
Expected: PASS. `TestEveryConfigFieldIsDocumented` must still pass; this task
adds no yaml field.

- [ ] **Step 6: Commit**

```bash
git add internal/config/overrides.go internal/config/overrides_test.go
git commit -m "feat: parse agent overrides from issue labels"
```

**Acceptance criteria:**
- Every one of the five error cases in spec section 6.3 has a test.
- `ParseOverrides` never returns a partly-filled `Overrides` alongside an error.
- No code outside `internal/config` contains the strings `"model:"`,
  `"harness:"`, or `"effort:"`. Verify: `grep -rn '"model:"\|"harness:"\|"effort:"' --include='*.go' . | grep -v internal/config`

---

## Task 2: The store columns

**review: no** — gated by its own migration tests.

**Files:**
- Modify: `internal/store/types.go`
- Modify: `internal/store/store.go` (schema at `:33` and `:56`, `addedColumns`
  at `:314`, `rebuilt` at `:350`, `dispatchColumns` at `:830`, `scanDispatch` at
  `:834`, `CreateDispatch` at `:776`)
- Modify: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  ```go
  // on IssueState
  Stopped       bool
  StoppedReason string
  // on Dispatch
  AgentPID int
  Model    string
  Harness  string
  Effort   string
  // new Store methods
  func (s *Store) MarkStopped(loop, repo string, number int, reason string, now time.Time) error
  func (s *Store) ClearStopped(loop, repo string, number int, now time.Time) error
  func (s *Store) StoppedIssues(loop, repo string) ([]IssueState, error)
  func (s *Store) SetDispatchAgentPID(id int64, pid int) error
  ```
  Tasks 3, 4, 5, 6, and 7 use these names exactly.

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/store_test.go`. Follow the file's existing helper for
opening a temporary database — read the top of the file and reuse it rather
than writing a new one.

```go
func TestMarkAndClearStopped(t *testing.T) {
	s := newTestStore(t) // reuse this package's existing helper
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.MarkStopped("loop", "o/r", 7, "killed by operator", now); err != nil {
		t.Fatalf("MarkStopped: %v", err)
	}
	got, err := s.IssueState("loop", "o/r", 7)
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}
	if !got.Stopped || got.StoppedReason != "killed by operator" {
		t.Fatalf("state = %+v, want stopped with a reason", got)
	}

	if err := s.ClearStopped("loop", "o/r", 7, now); err != nil {
		t.Fatalf("ClearStopped: %v", err)
	}
	got, err = s.IssueState("loop", "o/r", 7)
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}
	if got.Stopped || got.StoppedReason != "" {
		t.Fatalf("state = %+v, want not stopped and no reason", got)
	}
}

// ClearStopped also clears the failure the killed runner recorded. Without
// this, an issue an operator stopped and started again returns carrying a
// failure it did not earn and a backoff deadline that delays it.
func TestClearStoppedClearsTheRetryStateButNotParked(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.MarkNeedsRetry("loop", "o/r", 7, now, []time.Duration{time.Hour}); err != nil {
		t.Fatalf("MarkNeedsRetry: %v", err)
	}
	st, err := s.IssueState("loop", "o/r", 7)
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}
	st.Parked = true
	if err := s.PutIssueState(st); err != nil {
		t.Fatalf("PutIssueState: %v", err)
	}
	if err := s.MarkStopped("loop", "o/r", 7, "killed by operator", now); err != nil {
		t.Fatalf("MarkStopped: %v", err)
	}
	if err := s.ClearStopped("loop", "o/r", 7, now); err != nil {
		t.Fatalf("ClearStopped: %v", err)
	}

	got, err := s.IssueState("loop", "o/r", 7)
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}
	if got.NeedsRetry || !got.RetryAfter.IsZero() {
		t.Fatalf("state = %+v, want the retry flag and deadline cleared", got)
	}
	if !got.Parked {
		t.Fatal("ClearStopped cleared parked; the two flags are independent")
	}
}

// A dispatch must not be able to begin on an issue an operator stopped, and a
// success must not silently un-stop one. Both writes therefore leave the flag
// alone; only ClearStopped clears it.
func TestBeginDispatchAndMarkSucceededLeaveStoppedAlone(t *testing.T) {
	for _, step := range []struct {
		name string
		run  func(s *Store, now time.Time) error
	}{
		{"BeginDispatch", func(s *Store, now time.Time) error {
			return s.BeginDispatch("loop", "o/r", 7, "sess", false, now)
		}},
		{"MarkSucceeded", func(s *Store, _ time.Time) error {
			return s.MarkSucceeded("loop", "o/r", 7)
		}},
	} {
		t.Run(step.name, func(t *testing.T) {
			s := newTestStore(t)
			now := time.Now().UTC().Truncate(time.Second)
			if err := s.MarkStopped("loop", "o/r", 7, "stopped", now); err != nil {
				t.Fatalf("MarkStopped: %v", err)
			}
			if err := step.run(s, now); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
			got, err := s.IssueState("loop", "o/r", 7)
			if err != nil {
				t.Fatalf("IssueState: %v", err)
			}
			if !got.Stopped {
				t.Fatalf("%s cleared the stopped flag", step.name)
			}
		})
	}
}

func TestStoppedIssues(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.MarkStopped("loop", "o/r", 7, "one", now); err != nil {
		t.Fatalf("MarkStopped: %v", err)
	}
	if err := s.BeginDispatch("loop", "o/r", 9, "sess", false, now); err != nil {
		t.Fatalf("BeginDispatch: %v", err)
	}
	got, err := s.StoppedIssues("loop", "o/r")
	if err != nil {
		t.Fatalf("StoppedIssues: %v", err)
	}
	if len(got) != 1 || got[0].Number != 7 {
		t.Fatalf("StoppedIssues = %+v, want only issue 7", got)
	}
}

func TestDispatchCarriesOverridesAndAgentPID(t *testing.T) {
	s := newTestStore(t)
	id, err := s.CreateDispatch(Dispatch{
		Loop: "loop", Repo: "o/r", Number: 7, Kind: KindStart,
		SessionID: "sess", Model: "claude-opus-5", Harness: "pi", Effort: "high",
	})
	if err != nil {
		t.Fatalf("CreateDispatch: %v", err)
	}
	if err := s.SetDispatchAgentPID(id, 4242); err != nil {
		t.Fatalf("SetDispatchAgentPID: %v", err)
	}
	got, err := s.GetDispatch(id)
	if err != nil {
		t.Fatalf("GetDispatch: %v", err)
	}
	if got.Model != "claude-opus-5" || got.Harness != "pi" || got.Effort != "high" {
		t.Fatalf("dispatch = %+v, want the overrides round-tripped", got)
	}
	if got.AgentPID != 4242 {
		t.Fatalf("AgentPID = %d, want 4242", got.AgentPID)
	}
}
```

- [ ] **Step 2: Write the failing migration test**

The repository's existing migration tests show how an OLD database is built and
then opened. Read `internal/store/store_test.go` for the pattern that exercises
`addedColumns` and copy it. Add:

```go
// A database created before this feature must gain the six columns, and the
// primary-key rebuild must carry the two new issues columns across. A rebuild
// that dropped them would silently un-stop every stopped issue.
func TestMigrationAddsStoppedAndOverrideColumns(t *testing.T) {
	path := oldDatabaseWithoutTheNewColumns(t) // build with the file's existing helper
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, c := range []struct{ table, column string }{
		{"issues", "stopped"},
		{"issues", "stopped_reason"},
		{"dispatches", "agent_pid"},
		{"dispatches", "model"},
		{"dispatches", "harness"},
		{"dispatches", "effort"},
	} {
		var n int
		q := "SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?"
		if err := db.db.QueryRow(q, c.table, c.column).Scan(&n); err != nil {
			t.Fatalf("pragma_table_info(%s): %v", c.table, err)
		}
		if n != 1 {
			t.Fatalf("column %s.%s is missing after the migration", c.table, c.column)
		}
	}
}

// The rebuild list is written by hand, so it is the one place a new issues
// column is easy to forget. Assert the stopped flag survives a rebuild.
func TestRebuildCarriesTheStoppedColumns(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.MarkStopped("loop", "o/r", 7, "stopped", now); err != nil {
		t.Fatalf("MarkStopped: %v", err)
	}
	for _, col := range []string{"stopped", "stopped_reason"} {
		if !strings.Contains(rebuiltColumnsFor("issues"), col) {
			t.Fatalf("the rebuild list for issues omits %q", col)
		}
	}
}

// rebuiltColumnsFor returns the carried-column list for a rebuilt table.
func rebuiltColumnsFor(table string) string {
	for _, r := range rebuilt {
		if r.table == table {
			return r.columns
		}
	}
	return ""
}
```

- [ ] **Step 3: Run the tests and confirm they fail**

Run: `go test ./internal/store/ -count=1`
Expected: FAIL — unknown fields and undefined methods.

- [ ] **Step 4: Add the struct fields**

In `internal/store/types.go`, add to `IssueState` (after `Parked`):

```go
	// Stopped records that an OPERATOR stopped this issue, or that the loop
	// refused to dispatch it because an override label is invalid.
	//
	// It is deliberately NOT the same flag as Parked. Parked means the loop
	// gave up after the retry cap, and parkRetryExhausted also removes the
	// trigger label, which is why Decide never has to read it: a human who
	// adds the label again un-parks the issue. An operator stop leaves the
	// trigger label in place, so Decide MUST read this one -- and overloading
	// Parked would break that documented recovery path.
	Stopped bool
	// StoppedReason is why. It is the whole explanation an operator gets in
	// `loop status`, so it is written for a human.
	StoppedReason string
```

And to `Dispatch` (after `Title`):

```go
	// AgentPID is the agent child's process identifier, recorded by Supervise.
	//
	// The agent runs in its own process group (see Supervise), which the
	// runner's own group does not cover. Nothing outside the runner could
	// otherwise reach it, so `sessions kill --force` would SIGKILL the runner
	// and leave the agent working in a worktree the loop believes is free.
	AgentPID int
	// Model, Harness and Effort are this dispatch's agent overrides, resolved
	// from the issue's labels at decision time.
	//
	// They ride on the row for the reason Title does: the runner is a detached
	// process that never sees the tick's GitHub snapshot, so it cannot read
	// the labels itself. Empty means "no override", never "the empty value".
	Model   string
	Harness string
	Effort  string
```

- [ ] **Step 5: Add the columns and the queries**

In `internal/store/store.go`:

1. Add to the `issues` `CREATE TABLE` (near `:44`):
   `stopped INTEGER NOT NULL DEFAULT 0,` and
   `stopped_reason TEXT NOT NULL DEFAULT '',`
2. Add to the `dispatches` `CREATE TABLE` (near `:76`):
   `agent_pid INTEGER NOT NULL DEFAULT 0,`, `model TEXT NOT NULL DEFAULT '',`,
   `harness TEXT NOT NULL DEFAULT '',`, `effort TEXT NOT NULL DEFAULT '',`
3. Append to `addedColumns` (`:314`), in this order:

```go
	{"issues", "stopped", "INTEGER NOT NULL DEFAULT 0"},
	{"issues", "stopped_reason", "TEXT NOT NULL DEFAULT ''"},
	{"dispatches", "agent_pid", "INTEGER NOT NULL DEFAULT 0"},
	{"dispatches", "model", "TEXT NOT NULL DEFAULT ''"},
	{"dispatches", "harness", "TEXT NOT NULL DEFAULT ''"},
	{"dispatches", "effort", "TEXT NOT NULL DEFAULT ''"},
```

4. Add `stopped, stopped_reason` to the `issues` entry of `rebuilt` (`:350`).
5. Add `agent_pid, model, harness, effort` to the END of `dispatchColumns`
   (`:830`), and the matching `&d.AgentPID, &d.Model, &d.Harness, &d.Effort`
   to the END of the `scanDispatch` argument list (`:837`). **The order of the
   two lists must match exactly.**
6. Extend `CreateDispatch` (`:776`) to insert `model, harness, effort`.
   `agent_pid` is not inserted; `SetDispatchAgentPID` writes it later.
7. Extend the `issues` `SELECT` in `IssueStates` (`:438`) and the scan at
   `:452` with the two new columns, and extend `PutIssueState` (`:487`) to
   write them.

- [ ] **Step 6: Add the four methods**

```go
// MarkStopped records that an operator stopped this issue, or that the loop
// refused to dispatch it.
//
// A targeted UPDATE rather than a read-modify-write through PutIssueState, for
// the reason BeginDispatch gives: a detached runner may be writing this same
// row through MarkNeedsRetry at the same moment, and a whole-state write would
// drop what it recorded.
//
// It is an UPSERT because an issue can be stopped before it has ever been
// dispatched: an invalid override label is found on the FIRST tick that would
// have dispatched it, and no row exists yet.
func (s *Store) MarkStopped(loop, repo string, number int, reason string, now time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO issues (project_id, loop, repo, number, stopped, stopped_reason, updated_at)
		VALUES (?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(project_id, loop, repo, number) DO UPDATE SET
		  stopped        = 1,
		  stopped_reason = excluded.stopped_reason,
		  updated_at     = excluded.updated_at`,
		s.projectID, loop, repo, number, reason, now.UTC())
	if err != nil {
		return fmt.Errorf("mark issue %d stopped: %w", number, err)
	}
	return nil
}

// ClearStopped lets the loop dispatch this issue again.
//
// It clears the retry flag and the retry deadline as well. The killed runner
// records a FAILED dispatch, and finish marks the issue for retry on a failure,
// so an issue an operator stopped and started again would otherwise return
// carrying a failure it did not earn and a backoff deadline that delays it.
//
// It does NOT clear parked. The two flags mean different things and have
// different recovery paths; a resume must not silently undo a retry-cap park.
func (s *Store) ClearStopped(loop, repo string, number int, now time.Time) error {
	_, err := s.db.Exec(`
		UPDATE issues
		SET stopped = 0, stopped_reason = '', needs_retry = 0, retry_after = 0,
		    updated_at = ?
		WHERE project_id = ? AND loop = ? AND repo = ? AND number = ?`,
		now.UTC(), s.projectID, loop, repo, number)
	if err != nil {
		return fmt.Errorf("clear the stopped flag on issue %d: %w", number, err)
	}
	return nil
}

// StoppedIssues returns every stopped issue in a loop. `sessions resume --all`
// is its only caller.
func (s *Store) StoppedIssues(loop, repo string) ([]IssueState, error) {
	states, err := s.IssueStates(loop, repo)
	if err != nil {
		return nil, err
	}
	out := make([]IssueState, 0, len(states))
	for _, st := range states {
		if st.Stopped {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// SetDispatchAgentPID records the agent child's process identifier. See
// Dispatch.AgentPID for why it is needed.
func (s *Store) SetDispatchAgentPID(id int64, pid int) error {
	_, err := s.db.Exec(
		`UPDATE dispatches SET agent_pid = ? WHERE id = ? AND project_id = ?`,
		pid, id, s.projectID)
	if err != nil {
		return fmt.Errorf("set agent pid for dispatch %d: %w", id, err)
	}
	return nil
}
```

Add `"sort"` to the file's imports if it is absent.

- [ ] **Step 7: Run the tests and confirm they pass**

Run: `go test ./internal/store/ -count=1`
Expected: PASS.

- [ ] **Step 8: Run every package that reads these types**

Run: `go build ./... && go test ./internal/store/ ./internal/loopcmd/ -count=1`
Expected: PASS. `internal/legacydb` and `internal/store/legacy.go` also write
`IssueState`; if either fails to compile, extend it the same way and note it in
the commit message.

- [ ] **Step 9: Commit**

```bash
git add internal/store/
git commit -m "feat: store the stopped flag, agent pid and dispatch overrides"
```

**Acceptance criteria:**
- `dispatchColumns` and `scanDispatch` list the same columns in the same order.
- The `issues` entry of `rebuilt` names `stopped` and `stopped_reason`.
- `BeginDispatch` and `MarkSucceeded` are unchanged with respect to `stopped`.
- `go test ./... -count=1 -p 1` builds every package.

---

## Task 3: The signal helpers

**review: yes** — this is where a wrong process could be signalled.

**Files:**
- Create: `internal/proc/signal.go`
- Create: `internal/proc/signal_test.go`

**Interfaces:**
- Consumes: `proc.IsAlive`, `proc.DispatchFlag` (already present).
- Produces:
  ```go
  var ErrNotRunner = errors.New("not this dispatch's runner")
  func Signal(pid int, dispatchID int64, sig syscall.Signal) error
  func SignalGroup(pid int, sig syscall.Signal) error
  ```
  Task 5 uses all three.

- [ ] **Step 1: Write the failing test**

Create `internal/proc/signal_test.go`:

```go
package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// A non-positive identifier must never reach kill(2): 0 signals the caller's
// own process group and -1 signals every process this user owns.
func TestSignalRefusesANonPositivePID(t *testing.T) {
	for _, pid := range []int{0, -1, -1000} {
		if err := Signal(pid, 1, syscall.SIGTERM); err == nil {
			t.Fatalf("Signal(%d) returned no error", pid)
		}
		if err := SignalGroup(pid, syscall.SIGKILL); err == nil {
			t.Fatalf("SignalGroup(%d) returned no error", pid)
		}
	}
}

// The operating system reuses process identifiers, so a stale row can name a
// live process that has nothing to do with this program.
func TestSignalRefusesAProcessThatIsNotTheRunner(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	err := Signal(cmd.Process.Pid, 7, syscall.SIGTERM)
	if !errors.Is(err, ErrNotRunner) {
		t.Fatalf("Signal = %v, want ErrNotRunner", err)
	}
	// The process must still be alive: refusing means NOT signalling.
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("the refused process died anyway: %v", err)
	}
}

// A process whose command line carries the dispatch flag IS the runner, so the
// signal is delivered.
func TestSignalDeliversToTheRunner(t *testing.T) {
	// "sleep" with the dispatch flag in its argument list is enough: IsAlive
	// matches on the command line, not on the binary.
	cmd := exec.Command("sleep", "30", DispatchFlag, "7")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _, _ = cmd.Process.Wait() })

	if err := Signal(cmd.Process.Pid, 7, syscall.SIGKILL); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the runner did not die")
	}
	_ = os.Getpid()
}
```

Note: `sleep 30 --dispatch 7` exits immediately on some systems because `sleep`
rejects the extra arguments. If the test proves flaky for that reason, replace
`sleep` with `sh -c 'sleep 30' --` plus the flag, or with a tiny helper built
by the test. Verify the process is actually alive before asserting.

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/proc/ -count=1`
Expected: FAIL — `undefined: Signal`.

- [ ] **Step 3: Write the implementation**

Create `internal/proc/signal.go`:

```go
package proc

import (
	"errors"
	"fmt"
	"syscall"
)

// ErrNotRunner reports that a process identifier does not name the runner for
// the dispatch it was read from.
var ErrNotRunner = errors.New("not this dispatch's runner")

// Signal sends sig to pid, but only after it confirms that pid is the runner
// for dispatchID.
//
// The check is the point of the function. A process identifier read from a
// database row can be stale, and the operating system reuses identifiers, so
// the number may now name an unrelated program that this command must not
// touch. IsAlive matches the runner's --dispatch argument, which is the only
// evidence that the process is ours.
//
// The signal lives here, beside that rule, rather than in the command layer.
// A caller that could reach kill(2) without the check is exactly the mistake
// this package exists to prevent.
func Signal(pid int, dispatchID int64, sig syscall.Signal) error {
	if err := validPID(pid); err != nil {
		return err
	}
	if !IsAlive(pid, dispatchID) {
		return fmt.Errorf("pid %d: %w", pid, ErrNotRunner)
	}
	if err := syscall.Kill(pid, sig); err != nil {
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	return nil
}

// SignalGroup sends sig to the process group led by pid.
//
// It takes a POSITIVE identifier and negates it here. A caller that passed a
// negative number directly would be one typo away from -1, which signals every
// process the user owns, and the negation is not something a call site should
// be trusted to remember.
//
// There is no runner check, because the process this reaches is the AGENT, not
// the runner, and the agent carries no --dispatch argument. The caller must
// have read the identifier from the dispatch row it is acting on.
func SignalGroup(pid int, sig syscall.Signal) error {
	if err := validPID(pid); err != nil {
		return err
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		return fmt.Errorf("signal process group %d: %w", pid, err)
	}
	return nil
}

// validPID rejects an identifier that kill(2) reads as a broadcast.
//
// kill(2) reads 0 as "the caller's whole process group", -1 as "every process
// this user owns", and any other negative value as a process group. A number
// that came out of a database row -- which a truncated write, a hand-edited
// file, or an old schema could leave at zero -- must never be handed to it.
// The listener's stop command refuses one for the same reason.
func validPID(pid int) error {
	if pid <= 0 {
		return fmt.Errorf(
			"refusing to signal pid %d: it is not a process, and kill would read it as a broadcast",
			pid)
	}
	return nil
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `go test ./internal/proc/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/proc/
git commit -m "feat: add guarded signal helpers to internal/proc"
```

**Acceptance criteria:**
- `Signal` never calls `syscall.Kill` before both guards pass.
- `SignalGroup` negates the identifier itself.
- A refused signal leaves the target process alive — asserted, not assumed.

---

## Task 4: The engine — stop an issue and carry the override

**review: yes** — this decides what runs.

**Files:**
- Modify: `internal/engine/types.go`
- Modify: `internal/engine/engine.go:49-138`
- Modify: `internal/engine/engine_test.go`

**Interfaces:**
- Consumes: `config.ParseOverrides`, `config.Overrides` (Task 1);
  `store.IssueState.Stopped` (Task 2).
- Produces: `engine.KindStop`, `engine.Decision.Overrides config.Overrides`.
  Task 5 and Task 6 use both.

- [ ] **Step 1: Write the failing test**

Append to `internal/engine/engine_test.go`, following that file's existing
helpers for building a `Snapshot` and a `State`:

```go
// A stopped issue is not dispatched, and the operator is told why. Without
// this, killing an agent would achieve nothing: the trigger label is still
// present, so the next tick would start a new dispatch at once.
func TestDecideSkipsAStoppedIssue(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{
		{Number: 7, Title: "seven", Labels: []string{cfg.Labels.Trigger}, State: "open"},
	}}
	st := State{Issues: map[int]store.IssueState{
		7: {Number: 7, Stopped: true, StoppedReason: "killed by operator"},
	}}

	plan := Decide(cfg, snap, st, time.Now())

	if len(plan.Decisions) != 0 {
		t.Fatalf("decisions = %+v, want none", plan.Decisions)
	}
	if got := plan.NoDecisionReason(7); !strings.Contains(got, "killed by operator") {
		t.Fatalf("reason = %q, want it to carry the stop reason", got)
	}
}

// The stop check runs before the retry path. A killed dispatch records a
// failure, so a stopped issue almost always carries the retry flag too; if the
// retry path won, the loop would redispatch the issue it was told to stop.
func TestDecideSkipsAStoppedIssueThatAlsoNeedsRetry(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{
		{Number: 7, Labels: []string{cfg.Labels.Trigger, cfg.Labels.InFlight}, State: "open"},
	}}
	st := State{Issues: map[int]store.IssueState{
		7: {Number: 7, Stopped: true, StoppedReason: "stopped", NeedsRetry: true},
	}}

	if plan := Decide(cfg, snap, st, time.Now()); len(plan.Decisions) != 0 {
		t.Fatalf("decisions = %+v, want none", plan.Decisions)
	}
}

func TestDecideCarriesAValidOverride(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{
		{Number: 7, Labels: []string{cfg.Labels.Trigger, "model:claude-opus-5"}, State: "open"},
	}}

	plan := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())

	if len(plan.Decisions) != 1 {
		t.Fatalf("decisions = %+v, want one", plan.Decisions)
	}
	if got := plan.Decisions[0].Overrides.Model; got != "claude-opus-5" {
		t.Fatalf("Overrides.Model = %q, want claude-opus-5", got)
	}
}

func TestDecideStopsAnIssueWithAnInvalidOverride(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{
		{Number: 7, Labels: []string{cfg.Labels.Trigger, "harness:gpt"}, State: "open"},
	}}

	plan := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())

	if len(plan.Decisions) != 1 || plan.Decisions[0].Kind != KindStop {
		t.Fatalf("decisions = %+v, want one KindStop", plan.Decisions)
	}
	if !strings.Contains(plan.Decisions[0].Reason, "harness must be") {
		t.Fatalf("reason = %q, want the parse error", plan.Decisions[0].Reason)
	}
}

// The pi harness requires a model. The rule needs BOTH sources, so the parser
// cannot enforce it alone.
func TestDecideStopsPiWithNoModelFromEitherSource(t *testing.T) {
	cfg := testConfig()
	cfg.Agent.Model = ""
	snap := Snapshot{Issues: []ghub.Issue{
		{Number: 7, Labels: []string{cfg.Labels.Trigger, "harness:pi"}, State: "open"},
	}}

	plan := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())

	if len(plan.Decisions) != 1 || plan.Decisions[0].Kind != KindStop {
		t.Fatalf("decisions = %+v, want one KindStop", plan.Decisions)
	}
}

// An invalid label on an issue the loop would not dispatch changes nothing. A
// stop there would punish an issue nobody asked the loop to work on.
func TestDecideIgnoresAnInvalidOverrideWithNoTriggerLabel(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{
		{Number: 7, Labels: []string{"harness:gpt"}, State: "open"},
	}}

	plan := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())

	if len(plan.Decisions) != 0 {
		t.Fatalf("decisions = %+v, want none", plan.Decisions)
	}
}
```

Use the file's own helper for `cfg` if one exists; otherwise write `testConfig`
to return a `*config.Config` with the five label roles filled in and
`Agent.Model` set to something non-empty.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/engine/ -count=1`
Expected: FAIL — `undefined: KindStop`.

- [ ] **Step 3: Add the kind and the field**

In `internal/engine/types.go`, add to the decision kinds:

```go
	// KindStop marks an issue as not to be dispatched, and records why.
	//
	// It is the loop's answer to an override label it cannot honour. Running
	// under the configured model instead would silently do something other
	// than what the label asked for, and dispatching a value the parser
	// rejected is not an option at all.
	//
	// It writes only local state. parkRetryExhausted stays the one GitHub
	// write this program performs.
	KindStop Kind = "stop"
```

And to `Decision`:

```go
	// Overrides are the agent settings this issue's labels replace. They are
	// carried on the decision because the tick persists them on the dispatch
	// row, which is the only way they reach the detached runner.
	Overrides config.Overrides
```

Import `internal/config` in `types.go`.

- [ ] **Step 4: Add the two branches to Decide**

In `internal/engine/engine.go`, inside the issue loop:

Immediately AFTER the `liveIssues` check (`:65`) and BEFORE
`state := st.Issues[iss.Number]`, insert:

```go
		// An operator stopped this issue, or the loop refused an override
		// label. The check sits ABOVE the retry path deliberately: a killed
		// dispatch records a FAILURE, so a stopped issue almost always carries
		// the retry flag as well, and a retry that won here would redispatch
		// the very issue the operator stopped.
		if state := st.Issues[iss.Number]; state.Stopped {
			reason := state.StoppedReason
			if reason == "" {
				reason = "an operator stopped this issue"
			}
			skips[iss.Number] = reason + "; clear it with `agent-utils sessions resume`"
			continue
		}
```

Then, immediately AFTER the trigger-label check (`:113-118`) and BEFORE
`decided[iss.Number] = true`, insert:

```go
		// Read the override labels only once the issue is known to be
		// dispatchable. An invalid label on an issue with no trigger label is
		// not this loop's business, and stopping it would punish an issue
		// nobody asked the loop to work on.
		ov, err := config.ParseOverrides(iss.Labels)
		if err == nil {
			err = validateOverrides(cfg, ov)
		}
		if err != nil {
			decided[iss.Number] = true
			decisions = append(decisions, Decision{
				Kind:   KindStop,
				Issue:  iss.Number,
				Title:  iss.Title,
				Reason: err.Error(),
			})
			continue
		}
```

Add `Overrides: ov,` to BOTH the `KindResume` and the `KindStart` decisions
built below it.

Add this function at the end of `engine.go`:

```go
// validateOverrides applies the one override rule that needs the loop's
// configuration as well as the labels.
//
// ParseOverrides checks everything it can see from the labels alone. The pi
// harness additionally requires a model (see config.validate), and the model
// may come from EITHER source: an issue selecting harness:pi on a loop with no
// agent.model must carry a model: label too. Neither source knows that on its
// own, so the rule lives here, where both are in hand.
func validateOverrides(cfg *config.Config, ov config.Overrides) error {
	harness := cfg.Agent.Harness
	if ov.Harness != "" {
		harness = ov.Harness
	}
	model := cfg.Agent.Model
	if ov.Model != "" {
		model = ov.Model
	}
	if harness == config.HarnessPi && model == "" {
		return fmt.Errorf(
			"harness %q needs a model, and neither agent.model nor a %s label supplies one",
			config.HarnessPi, config.OverrideModelPrefix)
	}
	return nil
}
```

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./internal/engine/ -count=1`
Expected: PASS, including every pre-existing test in the package.

- [ ] **Step 6: Commit**

```bash
git add internal/engine/
git commit -m "feat: skip stopped issues and carry label overrides in Decide"
```

**Acceptance criteria:**
- The stopped check runs before the `NeedsRetry` branch. A test proves it.
- `KindStop` is produced only for an issue that carries the trigger label.
- Every existing engine test still passes without modification.

---

## Task 5: The kill and resume actions

**review: yes** — the ordered procedure and its guards are the whole feature.

**Files:**
- Create: `internal/loopcmd/kill.go`
- Create: `internal/loopcmd/kill_test.go`

**Interfaces:**
- Consumes: `store.MarkStopped`, `store.ClearStopped`, `store.StoppedIssues`,
  `store.Dispatch.AgentPID` (Task 2); `proc.Signal`, `proc.SignalGroup`,
  `proc.ErrNotRunner` (Task 3).
- Produces:
  ```go
  type Target struct {
      ProjectID, Project, Loop, Repo string
      Issue      int
      Dispatch   store.Dispatch
      ConfigPath string
  }
  type KillOptions struct {
      Selector Selector
      Force    bool
      Timeout  time.Duration
  }
  type Selector struct {
      Session string
      Issue   int
      All     bool
      Project string
      Loop    string
  }
  type Result struct {
      Target  Target
      Action  string // "signalled", "already gone", "forced", "stopped"
      Err     error
  }
  func Kill(opts KillOptions) ([]Result, error)
  func Resume(sel Selector) ([]Result, error)
  func RenderResults(verb string, rs []Result) string
  ```
  Task 7 calls `Kill`, `Resume`, and `RenderResults`.

- [ ] **Step 1: Write the failing guard tests**

Create `internal/loopcmd/kill_test.go`. Start with the guards, which need no
processes:

```go
package loopcmd

import (
	"strings"
	"testing"
	"time"
)

func TestSelectorValidation(t *testing.T) {
	tests := []struct {
		name    string
		sel     Selector
		wantErr string
	}{
		{"no selector", Selector{}, "one of --session"},
		{"session and issue", Selector{Session: "s", Issue: 7}, "mutually exclusive"},
		{"session and all", Selector{Session: "s", All: true}, "mutually exclusive"},
		{"issue and all", Selector{Issue: 7, All: true}, "mutually exclusive"},
		{"session alone", Selector{Session: "s"}, ""},
		{"issue alone", Selector{Issue: 7}, ""},
		{"all alone", Selector{All: true}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.sel.validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("validate() = %v, want nil", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("validate() = nil, want an error naming %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("validate() = %v, want it to name %q", err, tt.wantErr)
			}
		})
	}
}
```

- [ ] **Step 2: Write the failing ordering test**

This is the test that matters most. Append to `kill_test.go`:

```go
// The stopped flag MUST be written before the signal. A tick that runs in the
// window between the agent dying and the flag being written would see the
// trigger label and no live dispatch, and would start a new agent -- which is
// exactly what the operator asked not to happen.
func TestKillWritesTheStoppedFlagBeforeItSignals(t *testing.T) {
	var order []string
	k := killer{
		markStopped: func(Target, string) error {
			order = append(order, "stopped")
			return nil
		},
		signal: func(Target) error {
			order = append(order, "signalled")
			return nil
		},
		waitGone: func(Target, time.Duration) bool { return true },
	}

	if _, err := k.one(Target{Issue: 7}, KillOptions{Timeout: time.Second}); err != nil {
		t.Fatalf("one: %v", err)
	}
	want := []string{"stopped", "signalled"}
	if len(order) != 2 || order[0] != want[0] || order[1] != want[1] {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

// A failure to write the flag must abandon the target. Signalling anyway would
// kill the agent and let the next tick start another one.
func TestKillDoesNotSignalWhenTheFlagCannotBeWritten(t *testing.T) {
	signalled := false
	k := killer{
		markStopped: func(Target, string) error { return errWriteFailed },
		signal:      func(Target) error { signalled = true; return nil },
		waitGone:    func(Target, time.Duration) bool { return true },
	}

	if _, err := k.one(Target{Issue: 7}, KillOptions{}); err == nil {
		t.Fatal("one() = nil, want the write error")
	}
	if signalled {
		t.Fatal("the command signalled after the flag write failed")
	}
}

// --force kills the AGENT first. SIGKILL on the runner alone leaves the agent
// alive in a worktree the loop believes is free.
func TestForceKillsTheAgentBeforeTheRunner(t *testing.T) {
	var order []string
	k := killer{
		markStopped: func(Target, string) error { return nil },
		killAgent:   func(Target) error { order = append(order, "agent"); return nil },
		killRunner:  func(Target) error { order = append(order, "runner"); return nil },
		finish:      func(Target) error { order = append(order, "finish"); return nil },
	}

	if _, err := k.one(Target{Issue: 7, Dispatch: dispatchWithPIDs(11, 22)},
		KillOptions{Force: true}); err != nil {
		t.Fatalf("one: %v", err)
	}
	want := []string{"agent", "runner", "finish"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}
```

`killer` is an internal struct of function fields. It exists so the ordered
procedure is testable without real processes: the real constructor fills the
fields with the store and `proc` calls. Define `errWriteFailed` and
`dispatchWithPIDs` in the test file.

- [ ] **Step 3: Run the tests and confirm they fail**

Run: `go test ./internal/loopcmd/ -run 'TestSelector|TestKill|TestForce' -count=1`
Expected: FAIL — undefined types.

- [ ] **Step 4: Write `internal/loopcmd/kill.go`**

Implement, in this order:

1. `Selector`, with `validate()`. Exactly one of `Session`, `Issue`, `All` must
   be set. The error names the three flags.
2. `Target`, `KillOptions`, `Result`.
3. `resolve(sel Selector) ([]Target, error)`:
   - `Session`: use `AllSessions(SessionFilter{Project: sel.Project, Loop:
     sel.Loop})` and match on `Session.ID`. A session names one project, one
     loop, and one issue.
   - `Issue`: resolve the project with `ResolveProject(sel.Project)`, list its
     loops with `config.List(p.Dir)`, and take the loops that hold a row for
     that number. **If more than one loop matches and `sel.Loop` is empty,
     return an error naming the loops.**
   - `All`: for `Kill`, `db.RunningDispatches()` narrowed by `sel.Project` and
     `sel.Loop`; for `Resume`, every project's `StoppedIssues`.
   - Fill `ConfigPath` with `config.Resolve(p.Dir, loopName)`. A target whose
     configuration cannot be resolved is reported as a failed `Result`, not a
     fatal error: one broken loop must not abandon the rest.
4. `killer`, the struct of function fields the tests drive, and `one(t Target,
   opts KillOptions) (Result, error)` implementing spec section 4.2 exactly:
   lock, flag, then either the force path or SIGTERM-and-wait.
5. `Kill(opts KillOptions) ([]Result, error)` — resolve, group by loop, and for
   each loop open it once with `Open(ref, configPath, Options{RequireGitHub:
   false, MigrationPolicy: FailOnUnimported})`, take the lock at
   `filepath.Join(cfg.StateDir, cfg.Name+".lock")` with `lock.Acquire`, and run
   `one` for each of that loop's targets. Release the lock before the next
   loop. On `lock.ErrHeld`, report every target of that loop as failed with the
   `loop reset` wording: "a tick is running for loop %q; try again".
6. `Resume(sel Selector) ([]Result, error)` — the same resolution and locking,
   calling `ClearStopped`.
7. `RenderResults(verb string, rs []Result) string` — one line per target,
   naming the project, the loop, the issue, and the action. When `rs` is empty,
   return the "nothing matched" sentence for the verb.

Rules that must appear in the code, each with a comment saying why:

- A tend dispatch sets no flag. It holds no issue state.
- The flag is written before the signal.
- `proc.ErrNotRunner` and a non-positive identifier both mean "already gone":
  record the outcome with `store.FinishDispatch`, do not signal, and report it.
- On the graceful path the command does NOT write the outcome. The runner's own
  signal handler does. The command writes it only when the process was already
  gone, or after `--force`.
- The killed outcome is `store.DispatchResult{Status: store.StatusFailed,
  ExitCode: -1, APIError: "killed by operator"}`.

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./internal/loopcmd/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/loopcmd/kill.go internal/loopcmd/kill_test.go
git commit -m "feat: add the session kill and resume actions"
```

**Acceptance criteria:**
- A test proves the flag is written before the signal.
- A test proves a failed flag write prevents the signal.
- A test proves `--force` kills the agent before the runner.
- An ambiguous `--issue` across two loops is an error that names both loops.
- One failing target does not abandon the others.

---

## Task 6: The runner — signal handling, agent pid, and effective settings

**review: yes** — a mistake here orphans agents.

**Files:**
- Modify: `internal/runner/args.go`
- Modify: `internal/runner/runner.go:139-160`, `:196`
- Modify: `internal/loopcmd/tick.go` — `Summary` at `:97`, `act` at `:359`,
  `dispatch` at `:429`, `RunAgent` at `:541`
- Modify: `cmd/agent-utils/main.go:698`
- Modify: `internal/runner/args_test.go`, `internal/runner/runner_test.go`,
  `internal/loopcmd/tick_test.go`

**Interfaces:**
- Consumes: `config.Overrides` (Task 1); `store.SetDispatchAgentPID`,
  `Dispatch.Model/Harness/Effort` (Task 2); `Decision.Overrides` (Task 4).
- Produces:
  ```go
  type Settings struct { Harness, Model, Effort string }
  func Effective(cfg *config.Config, ov config.Overrides) Settings
  // Invocation gains:
  Overrides config.Overrides
  ```

- [ ] **Step 1: Write the failing `Effective` tests**

Append to `internal/runner/args_test.go`:

```go
func TestEffective(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Harness = config.HarnessClaude
	cfg.Agent.Model = "configured-model"
	cfg.Agent.Effort = "medium"

	got := Effective(cfg, config.Overrides{Model: "override-model"})
	if got.Model != "override-model" {
		t.Fatalf("Model = %q, want the override", got.Model)
	}
	if got.Harness != config.HarnessClaude || got.Effort != "medium" {
		t.Fatalf("Settings = %+v, want the unset fields kept", got)
	}

	// The configuration must be untouched. A caller holding cfg after this
	// would otherwise have one that no longer matches the file it was loaded
	// from.
	if cfg.Agent.Model != "configured-model" {
		t.Fatalf("Effective mutated cfg.Agent.Model to %q", cfg.Agent.Model)
	}
}

func TestBuildArgsUsesTheOverride(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Model = "configured-model"

	args := BuildArgs(cfg, Invocation{
		SessionID: "s", Prompt: "p",
		Overrides: config.Overrides{Model: "override-model", Effort: "high"},
	})

	if !containsPair(args, "--model", "override-model") {
		t.Fatalf("args = %v, want --model override-model", args)
	}
	if !containsPair(args, "--effort", "high") {
		t.Fatalf("args = %v, want --effort high", args)
	}
}

func TestPiBuildArgsUsesTheOverride(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Harness = config.HarnessPi
	cfg.Agent.Model = "configured-model"

	args := PiBuildArgs(cfg, Invocation{
		SessionID: "s", Prompt: "p",
		Overrides: config.Overrides{Model: "override-model"},
	})

	if !containsPair(args, "--model", "override-model") {
		t.Fatalf("args = %v, want --model override-model", args)
	}
}
```

Write `containsPair(args []string, flag, value string) bool` in the test file if
the package has no equivalent: it must match adjacent whole tokens, not a
substring.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/runner/ -run 'TestEffective|TestBuildArgsUses|TestPiBuildArgsUses' -count=1`
Expected: FAIL — `undefined: Effective`.

- [ ] **Step 3: Implement `Settings` and `Effective`**

In `internal/runner/args.go`, add the `Overrides` field to `Invocation`, then:

```go
// Settings are the agent settings one invocation actually runs with.
type Settings struct {
	Harness string
	Model   string
	Effort  string
}

// Effective returns the agent settings for one invocation. An override
// replaces the configured value; an empty override keeps it.
//
// It returns a value and NEVER mutates cfg. Writing the override into
// cfg.Agent would be shorter, and would leave every later reader of that
// configuration holding something that no longer matches the file it was
// loaded from -- including the retry policy and the log paths derived from it.
//
// Three call sites use it: the two argument builders and the harness switch in
// Supervise. They must agree, because a run whose arguments say one harness and
// whose binary is another fails in a way that is very hard to read.
func Effective(cfg *config.Config, ov config.Overrides) Settings {
	s := Settings{
		Harness: cfg.Agent.Harness,
		Model:   cfg.Agent.Model,
		Effort:  cfg.Agent.Effort,
	}
	if ov.Harness != "" {
		s.Harness = ov.Harness
	}
	if ov.Model != "" {
		s.Model = ov.Model
	}
	if ov.Effort != "" {
		s.Effort = ov.Effort
	}
	return s
}
```

Rewrite `BuildArgs` and `PiBuildArgs` to open with
`s := Effective(cfg, inv.Overrides)` and to read `s.Model` and `s.Effort` in
place of `cfg.Agent.Model` and `cfg.Agent.Effort`. Leave every other line
unchanged.

- [ ] **Step 4: Use the effective harness in Supervise**

In `internal/runner/runner.go`, replace the two `cfg.Agent.Harness ==
config.HarnessPi` comparisons (`:142` and `:229`) with a single
`s := Effective(cfg, inv.Overrides)` computed once before the harness switch,
compared as `s.Harness == config.HarnessPi`.

- [ ] **Step 5: Record the agent's process identifier**

In `internal/runner/runner.go`, immediately after the `cmd.Start()` success
path (`:196`), add:

```go
	// Record the agent's process identifier. It leads its OWN process group
	// (see the SysProcAttr above), which the runner's group does not cover, so
	// nothing outside this process could otherwise reach it. `sessions kill
	// --force` needs it: a SIGKILL to the runner alone would leave the agent
	// working in a worktree the loop believes is free.
	//
	// A failure here is logged, not fatal. The agent is already running, and
	// abandoning the run over a bookkeeping write would be a far worse outcome
	// than a --force that has to fall back to the runner alone.
	if err := st.SetDispatchAgentPID(d.ID, cmd.Process.Pid); err != nil {
		slog.Warn("record agent pid", "dispatch", d.ID, "err", err)
	}
```

- [ ] **Step 6: Persist and read the overrides in the tick**

In `internal/loopcmd/tick.go`:

- In `dispatch` (`:429`), add `Model: d.Overrides.Model, Harness:
  d.Overrides.Harness, Effort: d.Overrides.Effort` to the `store.Dispatch`
  literal handed to `CreateDispatch`.
- In `RunAgent` (`:614`), pass the row's values into the invocation:

```go
	return runner.Supervise(ctx, cfg, deps.Store, d,
		runner.Invocation{
			SessionID: d.SessionID,
			Prompt:    prompt,
			Resume:    resume,
			// From the ROW, not from the labels: this process is detached and
			// never sees the tick's GitHub snapshot. Title and BehindBy travel
			// the same way, for the same reason.
			Overrides: config.Overrides{
				Model: d.Model, Harness: d.Harness, Effort: d.Effort,
			},
		},
		workDir, d.LogPath)
```

- [ ] **Step 7: Apply a KindStop decision in the tick**

`Decide` now emits `KindStop`, and `act` returns `unknown decision kind` for
anything it does not handle (`internal/loopcmd/tick.go:382`). Add the case.

In `internal/loopcmd/tick.go`, add to the `Summary` struct (`:97`, beside
`Parked`):

```go
	Stopped int `json:"stopped"`
```

And to the switch in `act` (`:359`), beside `KindClearRetry`:

```go
	case engine.KindStop:
		// Not a dispatch. The loop refused an override label it cannot honour,
		// so it holds the issue instead of running it under the configured
		// model -- which would silently do something other than what the label
		// asked for.
		//
		// The write is LOCAL only. parkRetryExhausted stays the one GitHub
		// write this program performs, and the operator reads the reason from
		// `loop status` and clears it with `agent-utils sessions resume`.
		slog.Info("stopping issue", "loop", cfg.Name, "issue", d.Issue,
			"reason", d.Reason)
		return count(&sum.Stopped, deps.Store.MarkStopped(
			cfg.Name, cfg.Repo, d.Issue, d.Reason, now))
```

Add a test to `internal/loopcmd/tick_test.go`, following that file's existing
pattern for driving one tick against a fake GitHub client:

```go
// An issue whose override label the loop cannot honour is stopped, not
// dispatched, and nothing is written to GitHub.
func TestTickStopsAnIssueWithAnInvalidOverrideLabel(t *testing.T) {
	// Build a tick whose snapshot holds one triggered issue carrying
	// "harness:gpt". Assert:
	//   1. sum.Started == 0 and sum.Stopped == 1.
	//   2. The issue state is Stopped with a reason naming the harness.
	//   3. The fake GitHub client recorded no EditLabels and no comment.
	//   4. A second tick over the same snapshot dispatches nothing and adds no
	//      second row -- the stop is idempotent.
}
```

Write the body with the helpers the file already has. Assertion 3 is the one
that must not be dropped: it is what proves the one-GitHub-write invariant
survives.

- [ ] **Step 8: Add the signal handler**

In `cmd/agent-utils/main.go`, in the `run-agent` action (`:698`), wrap the
context before `loopcmd.RunAgent`:

```go
				// The runner is the ONLY command that needs this. It supervises
				// a long-lived agent, and `sessions kill` stops it with a
				// SIGTERM; without a handler the process would die on the spot,
				// recording nothing and leaving the agent alive in its own
				// process group. With one, the cancel walks the path Supervise
				// already has: SIGTERM to the agent's group, Wait, the SIGKILL
				// sweep, and finish() writing the outcome.
				ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
				defer stop()
				return loopcmd.RunAgent(ctx, cfg, deps, int64(c.Int("dispatch")))
```

Add `"os/signal"` and `"syscall"` to the imports.

- [ ] **Step 9: Write the end-to-end signal test**

Append to `internal/runner/runner_test.go`. The package already spawns real
processes, so this is in keeping with it:

```go
// A SIGTERM to a live runner must record an outcome and leave no agent behind.
// Before the signal handler existed it did neither: the runner died silently
// and the agent, in its own process group, kept running.
func TestSupervisedAgentDiesAndRecordsOnSIGTERM(t *testing.T) {
	// Use this package's existing fake-agent helper (the one that puts a stub
	// binary on PATH). Make the stub sleep long enough to be signalled.
	// Then: start Supervise in a goroutine with a cancellable context, wait
	// until the agent process exists, cancel, and assert:
	//   1. Supervise returns.
	//   2. The dispatch row is no longer "running".
	//   3. The agent process is gone -- poll syscall.Kill(pid, 0) until it
	//      errors, with a bounded timeout.
	t.Skip("fill in using this package's fake-agent helper; see the steps above")
}
```

Replace the `t.Skip` with the real body, modelled on whatever the package
already does to run a stub agent. **A skipped test does not satisfy this
step.** If the package has no such helper, write one: a shell script on a
temporary `PATH` that prints one stream-json line and then sleeps.

- [ ] **Step 10: Run the tests and confirm they pass**

Run: `go test ./internal/runner/ ./internal/loopcmd/ -count=1 -p 1`
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add internal/runner/ internal/loopcmd/tick.go cmd/agent-utils/main.go
git commit -m "feat: handle SIGTERM in the runner and apply agent overrides"
```

**Acceptance criteria:**
- `Effective` is the only place that resolves an override against the
  configuration. Verify: `grep -n 'Agent.Model\|Agent.Harness\|Agent.Effort' internal/runner/*.go`
  returns hits only inside `Effective`.
- `cfg` is never mutated; a test asserts it.
- The signal test runs and passes. It is not skipped.
- `agent_pid` is written after a successful `cmd.Start`, and a failure to write
  it does not fail the run.
- `act` handles `KindStop`; no decision kind reaches the `default` branch.
- A tick test proves the stop writes nothing to GitHub.

---

## Task 7: The commands

**review: no** — thin wiring over Task 5, gated by its own tests.

**Files:**
- Modify: `cmd/agent-utils/main.go:321-368`
- Modify: `internal/loopcmd/sessions.go:291-360`
- Create or modify: `cmd/agent-utils/sessions_test.go`

**Interfaces:**
- Consumes: `loopcmd.Kill`, `loopcmd.Resume`, `loopcmd.RenderResults`,
  `loopcmd.Selector`, `loopcmd.KillOptions` (Task 5).
- Produces: the two commands.

- [ ] **Step 1: Write the failing flag tests**

The tests in this directory drive an extracted `*Run` function rather than a
`*cli.Command` — `registerWebhookRun` is the precedent
(`cmd/agent-utils/project_test.go:214`). Follow it: the `Action` closure reads
the flags into a struct and calls `sessionsKillRun`, and the test calls that.

Create `cmd/agent-utils/sessions_test.go`:

```go
package main

import (
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/loopcmd"
)

// The guard is here rather than only in loopcmd because --yes is a command
// layer concern: it protects a human at a terminal, and nothing below this
// layer knows there is one.
func TestSessionsKillAllRequiresYes(t *testing.T) {
	err := sessionsKillRun(killArgs{Selector: loopcmd.Selector{All: true}})
	if err == nil {
		t.Fatal("sessionsKillRun with --all and no --yes returned no error")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error %q does not name --yes", err)
	}
}

func TestSessionsKillRejectsABadSelector(t *testing.T) {
	tests := []struct {
		name string
		sel  loopcmd.Selector
		want string
	}{
		{"none", loopcmd.Selector{}, "one of --session"},
		{"two", loopcmd.Selector{Session: "s", Issue: 7}, "mutually exclusive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := sessionsKillRun(killArgs{Selector: tt.sel})
			if err == nil {
				t.Fatalf("sessionsKillRun(%+v) returned no error", tt.sel)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not name %q", err, tt.want)
			}
		})
	}
}

// Both guards must run BEFORE anything opens the database or resolves a
// project, so a mistyped command touches no state.
func TestSessionsResumeRejectsABadSelectorBeforeAnyRead(t *testing.T) {
	// AGENT_UTILS_HOME points at a directory that does not exist, so any
	// attempt to open the canonical database fails loudly. The selector error
	// is what must come back instead.
	t.Setenv("AGENT_UTILS_HOME", "/nonexistent/agent-utils-home")

	err := sessionsResumeRun(killArgs{Selector: loopcmd.Selector{}})
	if err == nil || !strings.Contains(err.Error(), "one of --session") {
		t.Fatalf("err = %v, want the selector error", err)
	}
}
```

`killArgs` is the small struct the two `Action` closures fill:

```go
type killArgs struct {
	Selector loopcmd.Selector
	Yes      bool
	Force    bool
	Timeout  time.Duration
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./cmd/agent-utils/ -count=1`
Expected: FAIL — the commands do not exist.

- [ ] **Step 3: Add the two commands**

In `sessionsCommand()` (`cmd/agent-utils/main.go:321`), add `kill` and `resume`
beside `list`. Shared flags: `--project`, `--loop`, `--session`, `--issue`,
`--all`, `--yes`. `kill` also takes `--force` and
`--timeout` (`&cli.DurationFlag{Name: "timeout", Value: 30 * time.Second}`).

The loop selector is spelled `--loop` here, matching `sessions list`. The
comment at `:337` explains why the two surfaces differ; do not add an alias.

Each `Action` closure reads the flags into a `killArgs` and calls
`sessionsKillRun` or `sessionsResumeRun`. Those two functions hold every rule
the tests in step 1 assert, in this order:

1. `args.Selector.validate()`. A bad selector must fail before anything opens
   the database, so a mistyped command touches no state.
2. The `--yes` guard, when `Selector.All` is set. It lives at this layer
   because it protects a human at a terminal, and nothing below knows there is
   one.
3. `loopcmd.Kill` or `loopcmd.Resume`, then `fmt.Print(RenderResults(...))`.

They return an error only when EVERY target failed. A partial failure prints
its lines and exits 0, because the report already names what went wrong per
target.

Usage strings, written for an operator:

- `kill`: "stop a running session's agent and hold its issue until it is resumed"
- `resume`: "let the loop dispatch an issue that was stopped"

- [ ] **Step 4: Show the stopped state in both renderers**

In `internal/loopcmd/sessions.go`, add a `Stopped bool` field to `Session`,
fill it in `sessionsFrom` from the issue state, and extend the state switch in
both `RenderSessions` (`:311`) and `RenderAllSessions` (`:355`):

```go
		state := s.LastStatus
		switch {
		case s.Live:
			state = "running"
		case s.Stopped:
			// Above ORPHANED: a stopped session's runner is gone by design, and
			// reporting it as an orphan would send the operator looking for a
			// crash that did not happen.
			state = "STOPPED"
		case s.Orphaned:
			state = "ORPHANED"
		}
```

`sessionsFrom` takes only dispatch rows today. Pass it the issue states as well,
keyed by loop and number, and update its existing callers (`Sessions` and
`AllSessions`). A session with no matching issue state is not stopped.

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./cmd/agent-utils/ ./internal/loopcmd/ -count=1 -p 1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/agent-utils/ internal/loopcmd/sessions.go
git commit -m "feat: add sessions kill and sessions resume commands"
```

**Acceptance criteria:**
- `agent-utils sessions kill --help` names every flag.
- `--all` without `--yes` fails and names `--yes`.
- `sessions list` shows `STOPPED` for a stopped session, and `running` still
  wins over it.

---

## Task 8: Documentation

**review: no** — gated by `TestEveryConfigFieldIsDocumented` and by reading.

**Files:**
- Modify: `docs/configuration.md`
- Modify: `README.md`

- [ ] **Step 1: Document the override labels**

In `docs/configuration.md`, add a `## Agent overrides from labels` section
after the `## labels` section (`:366`). It must state:

- The three prefixes, with an example of each.
- That they are always active and need no configuration.
- That the prefix comparison ignores case and the value does not.
- Every rejection rule from spec section 6.3, and what happens when one fires:
  the loop does not dispatch, it stops the issue, and the reason appears in
  `agent-utils project loop status`.
- That `harness: pi` needs a model from `agent.model` or from a `model:` label.
- That overrides do NOT apply to a tend dispatch.
- That anyone who can label an issue can choose the model and the harness.

Cross-reference `agent.harness` (`:470`), `agent.model` (`:490`) and
`agent.effort` (`:502`) from the new section, and add a line to each of those
three pointing back to it.

- [ ] **Step 2: Document the commands**

In `README.md`, extend `## Sessions` (`:155`) with `sessions kill` and
`sessions resume`: the selectors, `--force`, `--timeout`, `--yes`, and the
`STOPPED` state in the table. State plainly that a kill holds the issue until a
resume, and why: without the hold, the next tick would dispatch it again.

Add the override labels to `## Configuration` (`:244`) with a pointer to the
reference, and note the exposure in `## Security` (`:289`).

- [ ] **Step 3: Verify the documentation gate**

Run: `go test ./internal/config/ -run TestEveryConfigFieldIsDocumented -count=1`
Expected: PASS.

- [ ] **Step 4: Run the full gate**

Run: `make check`
Expected: PASS — `fmtcheck`, `vet` (both passes), `lint`, `test`.

- [ ] **Step 5: Commit**

```bash
git add docs/configuration.md README.md
git commit -m "docs: record the override labels and the session kill commands"
```

**Acceptance criteria:**
- Every rejection rule in spec section 6.3 appears in the reference.
- The tend limit is stated.
- The security exposure is stated in both files.
- `make check` passes.

---

## Pipeline State

| Field   | Value                                                        |
|---------|--------------------------------------------------------------|
| stage   | 2 (plan review)                                              |
| class   | large (new command, new columns, signals, argv-bound values) |
| profile | backend                                                      |
| branch  | feat/session-kill-and-label-overrides                        |
| pr      | #12                                                          |
| gate    | pending                                                      |
| round   | 0                                                            |
