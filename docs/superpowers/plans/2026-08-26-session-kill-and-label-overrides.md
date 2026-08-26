# Session kill and label overrides — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator stop a running session from the command line, and let a label on an issue override the agent model, harness, or effort for that issue.

**Architecture:** A new per-issue `stopped` flag is the one mechanism both features use. `engine.Decide` refuses to dispatch a stopped issue. `sessions kill` sets the flag and then signals the runner; `sessions resume` clears it. `config.ParseOverrides` reads three label prefixes; an invalid label makes `Decide` stop the issue instead of dispatching it. A valid override rides on the dispatch row into the detached runner, where one `runner.Effective` helper feeds the argument builders.

**Tech Stack:** Go, `urfave/cli/v3` (v3.11.0), `modernc.org/sqlite`, `gopkg.in/yaml.v3`. Tests are standard `go test`.

**Spec:** `docs/superpowers/specs/2026-08-26-session-kill-and-label-overrides-design.md`

**Class:** Large. **Profile:** backend.

## Global Constraints

This repository has no root conventions document. The binding rules below were
read out of the code; each names where it is enforced.

- **`make check` is the gate.** It runs `fmtcheck`, `vet` (twice — the second
  with `GOOS=darwin`), `lint` (golangci-lint v2.5.0), and `test`.
- **Tests run with `-p 1` and `-count=1`.** The `worktree` package shells out to
  real git and the `runner` package spawns real processes.
- **`errorlint` is enabled** (`.golangci.yml`). Compare wrapped errors with
  `errors.Is`, never `==`.
- **Every `Config` yaml field must be documented** in `docs/configuration.md`,
  enforced by `TestEveryConfigFieldIsDocumented`
  (`internal/config/docs_test.go:14`). This plan adds no yaml field.
- **`parkRetryExhausted` is the ONE GitHub write this program performs**
  (`internal/loopcmd/tick.go:493`). No task may add another.
- **Every `Store` read and write is project-scoped.** `Store` carries a
  `projectID` (`internal/store/store.go:180`); `DB` methods are the only
  machine-wide reads. `db.Project(id)` makes a scoped `Store`
  (`internal/store/store.go:225`).
- **A new column is added through `addedColumns`, never by editing `CREATE
  TABLE` alone** (`internal/store/store.go:314`).
- **Never add a `Co-Authored-By:` trailer to a commit.** Not for any tool, not
  for anyone.
- **Comment style:** this codebase writes long comments that explain *why* and
  name the failure the code prevents (`internal/store/store.go:632-655`,
  `internal/engine/engine.go:42-47`). Match it.

## Verified external API (do not re-derive)

Read out of the source on 2026-08-26. Line numbers verified.

```go
// internal/store/store.go:225
func (d *DB) Project(projectID string) *Store
// internal/store/store.go:830 — the SELECT list every dispatch read shares
const dispatchColumns = `id, project_id, loop, repo, number, kind, session_id, pid, ...`
// internal/store/store.go:834 — scans that list, in that order
func scanDispatch(sc interface{ Scan(...any) error }) (Dispatch, error)
// internal/store/store.go:776, :790, :802, :435, :480, :715
func (s *Store) CreateDispatch(d Dispatch) (int64, error)
func (s *Store) SetDispatchProcess(id int64, pid int, startedAt time.Time) error
func (s *Store) FinishDispatch(id int64, r DispatchResult) error
func (s *Store) IssueStates(loop, repo string) (map[int]IssueState, error)
func (s *Store) PutIssueState(st IssueState) error
func (s *Store) IssueState(loop, repo string, number int) (IssueState, error)
// internal/store/store.go:1038 — machine-wide, unscoped. The DB/Store pair precedent.
func (d *DB) RunningDispatches() ([]Dispatch, error)

// internal/proc/proc.go:34, :17, :13
func IsAlive(pid int, dispatchID int64) bool
func CommandLine(pid int) (string, error)
const DispatchFlag = "--dispatch"
// internal/store/types.go:99
func (d Dispatch) RunnerID() int64

// internal/engine/engine.go:17, :199
func Decide(cfg *config.Config, snap Snapshot, st State, now time.Time) Plan
func retryDecision(cfg *config.Config, number int, state store.IssueState, now time.Time) (*Decision, bool, string)

// internal/runner/args.go:26, :64 ; internal/runner/runner.go:301
func BuildArgs(cfg *config.Config, inv Invocation) []string
func PiBuildArgs(cfg *config.Config, inv Invocation) []string
func Finish(cfg *config.Config, st *store.Store, d store.Dispatch, res store.DispatchResult, now time.Time) error

// internal/config/discover.go:189 ; internal/registry/registry.go
func Resolve(agentUtilsDir, name string) (string, error)
func Find(selector string) (Project, error)   // Project has ID, Name, Root, AgentUtilsDir
func List() ([]Project, error)
```

Facts that drove the design, each confirmed by reading:

- The runner is spawned with `Setsid` (`internal/runner/runner.go:46`); the
  agent child with `Setpgid: true` (`:154`). They lead DIFFERENT process
  groups, so a signal to the runner's group does not reach the agent.
- `cmd.Cancel` already SIGTERMs the agent's group on context cancel (`:157`),
  and a SIGKILL sweep already follows `Wait` (`:215`).
- `main` uses `context.Background()` (`cmd/agent-utils/main.go:49`). Nothing in
  the runner path installs a signal handler.
- **`IsAlive` fails SAFE by reporting ALIVE when `ps` errors**
  (`internal/proc/proc.go:42`). Correct for liveness; INVERTED for signalling.
- **`tendDecisions` skips only issues marked `decided`**
  (`internal/engine/engine.go:259`). An issue skipped without setting `decided`
  can still be tended.
- **`retryDecision` receives no labels** (`internal/engine/engine.go:199`), so
  the caller must attach overrides to the decision it returns.
- **`agent.model` is required for EVERY harness**
  (`internal/config/config.go:261`). A "pi needs a model" rule could never fire.
- **`config.validate` forbids `agent.permission_mode` with `harness: pi`**
  (`internal/config/config.go:218`), and **`PiBuildArgs` emits neither a
  permission mode nor a cost ceiling** (`internal/runner/args.go:60`).
- Test helpers that EXIST: `openTemp(t) *Store` and `openTempAt(t, path)`
  (`internal/store/store_test.go:15`, `:20`); `testConfig()`
  (`internal/engine/engine_test.go:12`); `stubClaude` / `stubPi`
  (`internal/runner/runner_test.go:18`, `:222`); `joined(args)`
  (`internal/runner/args_test.go:21`); `fakeGH` (`internal/loopcmd/tick_test.go`).
  There is **no** `newTestStore` and **no** extracted old-database helper — the
  old-schema fixture is inline SQL in `TestOpenMigratesAnOlderDatabase`
  (`internal/store/store_test.go:199`).

---

## File map

| File | Responsibility |
|------|----------------|
| `internal/config/overrides.go` (new) | `Overrides`, `ParseOverrides`, the label syntax and its validation. |
| `internal/proc/signal.go` (new) | `VerifyRunner`, `Signal`, `SignalGroup`. |
| `internal/store/store.go`, `types.go` | Six new columns, the rebuild list, five new methods. |
| `internal/engine/types.go`, `engine.go` | `KindStop`, `Decision.Overrides`, the stopped skip, the override gate. |
| `internal/runner/args.go`, `runner.go` | `Settings`, `Effective`, `Invocation.Overrides`, `agent_pid`. |
| `internal/loopcmd/kill.go` (new) | `Kill`, `Resume`, target resolution, the ordered procedure. |
| `internal/loopcmd/tick.go` | Persist overrides; apply `KindStop`; pass overrides to the invocation. |
| `internal/loopcmd/sessions.go`, `status.go` | The `STOPPED` state and the reason. |
| `cmd/agent-utils/main.go`, `runagent.go` (new) | `sessions kill`, `sessions resume`, the runner's signal handler. |
| `docs/configuration.md`, `README.md` | The label syntax, the commands, the limits, the exposure. |

---

## Task 1: The override parser

**review: yes** — this is the security boundary.

**Files:**
- Create: `internal/config/overrides.go`, `internal/config/overrides_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  ```go
  type Overrides struct{ Model, Harness, Effort string }
  func ParseOverrides(labels []string) (Overrides, error)
  const OverrideModelPrefix   = "model:"
  const OverrideHarnessPrefix = "harness:"
  const OverrideEffortPrefix  = "effort:"
  ```
  Tasks 4, 5, and 6 depend on these names exactly.

- [ ] **Step 1: Write the failing test**

Create `internal/config/overrides_test.go`:

```go
package config

import (
	"strings"
	"testing"
)

func TestParseOverrides(t *testing.T) {
	tests := []struct {
		name    string
		labels  []string
		want    Overrides
		wantErr string
	}{
		{"none", []string{"status:ready", "bug"}, Overrides{}, ""},
		{
			"all three",
			[]string{"model:claude-opus-5", "harness:pi", "effort:high"},
			Overrides{Model: "claude-opus-5", Harness: "pi", Effort: "high"},
			"",
		},
		{
			"the model prefix ignores case, the model value keeps it",
			[]string{"Model:Claude-Opus-5"},
			Overrides{Model: "Claude-Opus-5"},
			"",
		},
		{
			"an enum value is lowered, because it is a closed list",
			[]string{"harness:PI", "effort:HIGH"},
			Overrides{Harness: "pi", Effort: "high"},
			"",
		},
		{"empty value", []string{"model:"}, Overrides{}, "names no value"},
		{"whitespace", []string{"model:claude opus"}, Overrides{}, "is not a valid"},
		{"flag shaped", []string{"model:--dangerously-skip-permissions"}, Overrides{}, "starts with"},
		{"zero width space", []string{"model:claude\u200bopus"}, Overrides{}, "is not a valid"},
		{"duplicate prefix", []string{"model:a", "model:b"}, Overrides{}, "carries two"},
		{"unknown harness", []string{"harness:gpt"}, Overrides{}, "harness must be"},
		{"unknown effort", []string{"effort:bogus"}, Overrides{}, "effort must be"},
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
				// An error must not also return a partly filled value: a caller
				// that ignored the error would dispatch under half an override.
				if got != (Overrides{}) {
					t.Fatalf("ParseOverrides returned %+v alongside an error", got)
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
	for _, v := range []string{"-p", "--model", "-", "--", "-x"} {
		if _, err := ParseOverrides([]string{"model:" + v}); err == nil {
			t.Fatalf("ParseOverrides accepted the flag-shaped value %q", v)
		}
	}
}

// The reason text is persisted, logged, and printed to a terminal. A label
// carrying a newline or an escape must not travel through it unquoted.
func TestParseOverridesQuotesTheLabelInEveryError(t *testing.T) {
	_, err := ParseOverrides([]string{"model:a\nb"})
	if err == nil {
		t.Fatal("ParseOverrides accepted a label with a newline")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("error %q carries a raw newline; quote the label with %%q", err)
	}
}

// The duplicate check must not report a raw second label before that label has
// been validated.
func TestParseOverridesQuotesADuplicateLabel(t *testing.T) {
	_, err := ParseOverrides([]string{"model:a", "model:b\nc"})
	if err == nil {
		t.Fatal("ParseOverrides accepted duplicate model labels")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("error %q carries a raw newline", err)
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/config/ -run TestParseOverrides -count=1`
Expected: FAIL — `undefined: ParseOverrides`.

- [ ] **Step 3: Write the implementation**

Create `internal/config/overrides.go`:

```go
package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Override label prefixes. They are fixed rather than configurable: an
// operator types them on an issue, and a per-loop spelling would mean the same
// label meant different things in two loops of one project.
const (
	OverrideModelPrefix   = "model:"
	OverrideHarnessPrefix = "harness:"
	OverrideEffortPrefix  = "effort:"
)

// overrideValue is an ALLOWLIST, not a denylist.
//
// The value becomes one element of the argument list handed to exec (see
// runner.BuildArgs). Go passes a list rather than a shell string, so there is
// no quoting hazard -- but a value beginning with "-" is read by the agent
// binary as a FLAG, which is how "model:--dangerously-skip-permissions" would
// turn a label into a permission bypass. ghub.SafeRef rejects a leading dash
// for the same reason, and this follows it.
//
// An allowlist is what makes the rule hold for input nobody thought of. A
// denylist of "-" plus unicode.IsSpace would still admit U+200B ZERO WIDTH
// SPACE and U+2060 WORD JOINER, which are not spaces to Go and are invisible
// to the operator reading the label back.
var overrideValue = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)

// validEfforts is the closed list config.validate enforces for agent.effort.
// The label path applies the SAME list: a rule the configuration closes must
// not be reopened by a label.
var validEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// Overrides holds the agent settings an issue's labels replace. An empty field
// means "no override", never "the empty value".
type Overrides struct {
	Model   string
	Harness string
	Effort  string
}

// ParseOverrides reads the agent overrides from an issue's labels.
//
// It is the ONLY place that knows this syntax. The engine validates the result
// against the loop's configuration and the runner receives values already
// checked here; neither re-implements the rule.
//
// Every error quotes the label with %q and is worded for a human. The text is
// persisted as stopped_reason, logged, and printed to a terminal, so a raw
// label carrying a newline or an escape must never travel through it.
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
		// lowerValue marks a CLOSED list. A harness or an effort is an enum, and
		// every other label comparison in this program folds case
		// (ghub.HasLabel uses EqualFold). A model identifier is not an enum and
		// is case-sensitive, so it alone keeps what was typed.
		var lowerValue bool
		switch {
		case strings.HasPrefix(lower, OverrideModelPrefix):
			field, prefix = &out.Model, OverrideModelPrefix
		case strings.HasPrefix(lower, OverrideHarnessPrefix):
			field, prefix, lowerValue = &out.Harness, OverrideHarnessPrefix, true
		case strings.HasPrefix(lower, OverrideEffortPrefix):
			field, prefix, lowerValue = &out.Effort, OverrideEffortPrefix, true
		default:
			continue
		}

		// Slice the ORIGINAL label, not the lowered copy: the prefix is matched
		// without case, but a model identifier must survive exactly as written.
		value := l[len(prefix):]
		// Validate BEFORE the duplicate check reports anything. The duplicate
		// error interpolates both labels, and an unvalidated one may carry a
		// newline or a terminal escape.
		if err := validOverrideValue(prefix, value); err != nil {
			return Overrides{}, err
		}
		if first, ok := seen[prefix]; ok {
			return Overrides{}, fmt.Errorf(
				"the issue carries two %s labels, %q and %q; remove one",
				strings.TrimSuffix(prefix, ":"), first, l)
		}
		seen[prefix] = l

		if lowerValue {
			value = strings.ToLower(value)
		}
		*field = value
	}

	if out.Harness != "" && out.Harness != HarnessClaude && out.Harness != HarnessPi {
		return Overrides{}, fmt.Errorf(
			"label %q selects harness %q; harness must be %q or %q",
			OverrideHarnessPrefix+out.Harness, out.Harness, HarnessClaude, HarnessPi)
	}
	if out.Effort != "" && !containsString(validEfforts, out.Effort) {
		return Overrides{}, fmt.Errorf(
			"label %q selects effort %q; effort must be one of %s",
			OverrideEffortPrefix+out.Effort, out.Effort, strings.Join(validEfforts, ", "))
	}
	return out, nil
}

// validOverrideValue rejects a value that must never reach an argument list.
// See overrideValue for why the rule is an allowlist.
func validOverrideValue(prefix, value string) error {
	label := prefix + value
	switch {
	case value == "":
		return fmt.Errorf("label %q names no value", label)
	case strings.HasPrefix(value, "-"):
		return fmt.Errorf(
			"label %q starts with %q, which the agent reads as a flag", label, "-")
	case !overrideValue.MatchString(value):
		return fmt.Errorf(
			"label %q is not a valid override value; use letters, digits, and any of . _ / -",
			label)
	}
	return nil
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
```

If the package already has an equivalent helper, use it and delete
`containsString`. Check with `grep -n 'func contains' internal/config/`.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/config/ -count=1`
Expected: PASS, including `TestEveryConfigFieldIsDocumented`.

- [ ] **Step 5: Commit**

```bash
git add internal/config/overrides.go internal/config/overrides_test.go
git commit -m "feat: parse agent overrides from issue labels"
```

**Acceptance criteria:**
- Every rejection rule in spec section 6.3 has a test.
- `ParseOverrides` never returns a non-zero `Overrides` alongside an error.
- No error message can carry a raw newline from a label.
- No code outside `internal/config` contains the strings `"model:"`,
  `"harness:"`, or `"effort:"`. Verify:
  `grep -rn '"model:"\|"harness:"\|"effort:"' --include='*.go' . | grep -v internal/config`

---

## Task 2: The store columns

**review: no** — gated by its own migration tests.

**Files:**
- Modify: `internal/store/types.go`, `internal/store/store.go`,
  `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  ```go
  // on IssueState
  Stopped bool; StoppedReason string
  // on Dispatch
  AgentPID int; Model, Harness, Effort string
  // new
  type StoppedIssue struct { ProjectID, Loop, Repo string; Number int; Reason string }
  func (s *Store) MarkStopped(loop, repo string, number int, reason string, now time.Time) error
  func (s *Store) ClearStopped(loop, repo string, number int, now time.Time) error
  func (s *Store) StoppedIssues(loop, repo string) ([]IssueState, error)
  func (s *Store) SetDispatchAgentPID(id int64, pid int) error
  func (d *DB) StoppedIssues() ([]StoppedIssue, error)
  ```
  Tasks 4, 5, 6, and 7 use these names exactly.

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/store_test.go`. **The package helper is
`openTemp(t)`, which returns a `*Store`.** There is no `newTestStore`.

```go
func TestMarkAndClearStopped(t *testing.T) {
	s := openTemp(t)
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
	s := openTemp(t)
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

// Three writes must leave the flag alone. BeginDispatch and MarkSucceeded
// because a dispatch can only begin on an issue that is NOT stopped, so a
// clear there is a silent un-stop. PutIssueState because parkRetryExhausted
// reads a whole state and writes it back: a state read before a kill and
// written after it would carry stopped = 0.
func TestOtherWritesLeaveStoppedAlone(t *testing.T) {
	t.Run("BeginDispatch", func(t *testing.T) {
		s := openTemp(t)
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.MarkStopped("loop", "o/r", 7, "stopped", now); err != nil {
			t.Fatalf("MarkStopped: %v", err)
		}
		if err := s.BeginDispatch("loop", "o/r", 7, "sess", false, now); err != nil {
			t.Fatalf("BeginDispatch: %v", err)
		}
		assertStopped(t, s, 7)
	})

	t.Run("MarkSucceeded", func(t *testing.T) {
		s := openTemp(t)
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.MarkStopped("loop", "o/r", 7, "stopped", now); err != nil {
			t.Fatalf("MarkStopped: %v", err)
		}
		if err := s.MarkSucceeded("loop", "o/r", 7); err != nil {
			t.Fatalf("MarkSucceeded: %v", err)
		}
		assertStopped(t, s, 7)
	})

	// Read BEFORE the stop, write AFTER it -- exactly what parkRetryExhausted
	// does across a concurrent kill.
	t.Run("PutIssueState with a stale state", func(t *testing.T) {
		s := openTemp(t)
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.BeginDispatch("loop", "o/r", 7, "sess", false, now); err != nil {
			t.Fatalf("BeginDispatch: %v", err)
		}
		stale, err := s.IssueState("loop", "o/r", 7)
		if err != nil {
			t.Fatalf("IssueState: %v", err)
		}
		if err := s.MarkStopped("loop", "o/r", 7, "stopped", now); err != nil {
			t.Fatalf("MarkStopped: %v", err)
		}
		stale.Parked = true
		if err := s.PutIssueState(stale); err != nil {
			t.Fatalf("PutIssueState: %v", err)
		}
		assertStopped(t, s, 7)
	})
}

func assertStopped(t *testing.T, s *Store, number int) {
	t.Helper()
	got, err := s.IssueState("loop", "o/r", number)
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}
	if !got.Stopped {
		t.Fatal("the stopped flag was cleared by a write that must not touch it")
	}
}

func TestStoppedIssuesScopedAndMachineWide(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now().UTC().Truncate(time.Second)

	a, b := db.Project("project-a"), db.Project("project-b")
	if err := a.MarkStopped("loop", "o/r", 7, "one", now); err != nil {
		t.Fatalf("MarkStopped: %v", err)
	}
	if err := b.MarkStopped("loop", "o/r", 7, "two", now); err != nil {
		t.Fatalf("MarkStopped: %v", err)
	}
	if err := a.BeginDispatch("loop", "o/r", 9, "sess", false, now); err != nil {
		t.Fatalf("BeginDispatch: %v", err)
	}

	scoped, err := a.StoppedIssues("loop", "o/r")
	if err != nil {
		t.Fatalf("Store.StoppedIssues: %v", err)
	}
	if len(scoped) != 1 || scoped[0].Number != 7 {
		t.Fatalf("Store.StoppedIssues = %+v, want only issue 7 of project a", scoped)
	}

	// Two projects hold an issue 7 in a loop with the same name. A read that
	// forgot the project would merge them, which is why the machine-wide row
	// carries ProjectID.
	all, err := db.StoppedIssues()
	if err != nil {
		t.Fatalf("DB.StoppedIssues: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("DB.StoppedIssues = %+v, want both projects", all)
	}
	byProject := map[string]string{}
	for _, si := range all {
		byProject[si.ProjectID] = si.Reason
	}
	if byProject["project-a"] != "one" || byProject["project-b"] != "two" {
		t.Fatalf("DB.StoppedIssues = %+v, want each project's own reason", all)
	}
}

func TestDispatchCarriesOverridesAndAgentPID(t *testing.T) {
	s := openTemp(t)
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

Add `"path/filepath"` and `"strings"` to the test imports. The file currently
imports only `database/sql`, `path/filepath`, `testing`, and `time`, so
`strings` is genuinely missing and `TestRebuildCarriesTheStoppedColumns` needs
it.

- [ ] **Step 2: Write the failing migration test**

`TestOpenMigratesAnOlderDatabase` (`internal/store/store_test.go:199`) builds an
old-schema database with inline SQL. **Read it and follow that pattern** — do
not invent a helper that does not exist. Extract its fixture into
`writeOldSchema(t, path)` and have the existing test call it too, so both share
one definition; the existing test must keep passing.

```go
// A database created before this feature must gain the six columns, and the
// primary-key rebuild must carry the two new issues columns across. A rebuild
// that dropped them would silently un-stop every stopped issue.
func TestMigrationAddsStoppedAndOverrideColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	writeOldSchema(t, path)

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
		q := `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`
		if err := db.db.QueryRow(q, c.table, c.column).Scan(&n); err != nil {
			t.Fatalf("pragma_table_info(%s): %v", c.table, err)
		}
		if n != 1 {
			t.Fatalf("column %s.%s is missing after the migration", c.table, c.column)
		}
	}
}

// The rebuild list is written by hand, so it is the one place a new issues
// column is easy to forget.
func TestRebuildCarriesTheStoppedColumns(t *testing.T) {
	var columns string
	for _, r := range rebuilt {
		if r.table == "issues" {
			columns = r.columns
		}
	}
	for _, col := range []string{"stopped", "stopped_reason"} {
		if !strings.Contains(columns, col) {
			t.Fatalf("the rebuild list for issues omits %q", col)
		}
	}
}
```

- [ ] **Step 3: Run the tests and confirm they fail**

Run: `go test ./internal/store/ -count=1`
Expected: FAIL — unknown fields, undefined methods.

- [ ] **Step 4: Add the struct fields**

In `internal/store/types.go`, add to `IssueState` after `Parked`:

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
	//
	// Only MarkStopped and ClearStopped write it. PutIssueState deliberately
	// does not: it round-trips a whole state, and the park path reads one
	// BEFORE a kill and writes it back after, which would un-stop the issue.
	Stopped bool
	// StoppedReason is why. It is the whole explanation an operator gets in
	// `loop status`, so it is written for a human.
	StoppedReason string
```

And to `Dispatch` after `Title`:

```go
	// AgentPID is the agent child's process identifier, recorded by Supervise.
	//
	// The agent runs in its own process group, which the runner's own group
	// does not cover, so nothing outside the runner could otherwise reach it.
	// `sessions kill --force` needs it: a SIGKILL to the runner alone would
	// leave the agent working in a worktree the loop believes is free.
	//
	// It is written once and never cleared, so it is STALE on any row whose
	// runner died without recording an outcome -- and after a reboot the
	// identifier space is reused wholesale. A caller must confirm the runner is
	// live before it trusts this number. See loopcmd.Kill.
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

Add near the other types:

```go
// StoppedIssue is one stopped issue in the machine-wide report.
//
// It carries ProjectID because the report spans projects and an issue number
// is unique only within a loop of ONE project. Keyed on loop and number alone,
// two projects' issues merge -- the same hazard loopcmd.sessionKey exists to
// prevent.
type StoppedIssue struct {
	ProjectID string
	Loop      string
	Repo      string
	Number    int
	Reason    string
}
```

- [ ] **Step 5: Add the columns and the queries**

In `internal/store/store.go`:

1. Add to the `issues` `CREATE TABLE`: `stopped INTEGER NOT NULL DEFAULT 0,`
   and `stopped_reason TEXT NOT NULL DEFAULT '',`
2. Add to the `dispatches` `CREATE TABLE`: `agent_pid INTEGER NOT NULL DEFAULT 0,`,
   `model TEXT NOT NULL DEFAULT '',`, `harness TEXT NOT NULL DEFAULT '',`,
   `effort TEXT NOT NULL DEFAULT '',`
3. Append to `addedColumns` (`:314`), in this order:

```go
	{"issues", "stopped", "INTEGER NOT NULL DEFAULT 0"},
	{"issues", "stopped_reason", "TEXT NOT NULL DEFAULT ''"},
	{"dispatches", "agent_pid", "INTEGER NOT NULL DEFAULT 0"},
	{"dispatches", "model", "TEXT NOT NULL DEFAULT ''"},
	{"dispatches", "harness", "TEXT NOT NULL DEFAULT ''"},
	{"dispatches", "effort", "TEXT NOT NULL DEFAULT ''"},
```

4. Add `stopped, stopped_reason` to the `issues` entry of `rebuilt` (`:352`).
5. Add `agent_pid, model, harness, effort` to the END of `dispatchColumns`
   (`:830`) and the matching `&d.AgentPID, &d.Model, &d.Harness, &d.Effort` to
   the END of the `scanDispatch` argument list. **The two lists must match
   exactly, in order.**
6. Extend `CreateDispatch` (`:776`) to insert `model, harness, effort`.
   `agent_pid` is NOT inserted; `SetDispatchAgentPID` writes it later.
7. Extend the `issues` `SELECT` in `IssueStates` (`:435`) and its scan with the
   two new columns. **Do NOT touch `PutIssueState`** — see the field comment.

- [ ] **Step 6: Add the five methods**

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

// StoppedIssues returns every stopped issue in one loop of this project.
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
// Dispatch.AgentPID for why it is needed and why it can be stale.
func (s *Store) SetDispatchAgentPID(id int64, pid int) error {
	_, err := s.db.Exec(
		`UPDATE dispatches SET agent_pid = ? WHERE id = ? AND project_id = ?`,
		pid, id, s.projectID)
	if err != nil {
		return fmt.Errorf("set agent pid for dispatch %d: %w", id, err)
	}
	return nil
}

// StoppedIssues returns every stopped issue on this machine, in every project.
//
// It is the machine-wide read the per-project view cannot answer, exactly as
// DB.RunningDispatches is. The sessions report spans projects, so labelling its
// rows from a scoped read would need the caller to know every project up front.
func (d *DB) StoppedIssues() ([]StoppedIssue, error) {
	rows, err := d.db.Query(`
		SELECT project_id, loop, repo, number, stopped_reason
		FROM issues WHERE stopped = 1 ORDER BY project_id, loop, number`)
	if err != nil {
		return nil, fmt.Errorf("query stopped issues: %w", err)
	}
	defer rows.Close()
	var out []StoppedIssue
	for rows.Next() {
		var si StoppedIssue
		if err := rows.Scan(&si.ProjectID, &si.Loop, &si.Repo, &si.Number, &si.Reason); err != nil {
			return nil, fmt.Errorf("scan stopped issue: %w", err)
		}
		out = append(out, si)
	}
	return out, rows.Err()
}
```

Add `"sort"` to the imports if absent.

- [ ] **Step 7: Run the tests and confirm they pass**

Run: `go test ./internal/store/ -count=1`
Expected: PASS, including the pre-existing `TestOpenMigratesAnOlderDatabase`.

- [ ] **Step 8: Build every package**

Run: `go build ./... && go test ./internal/store/ ./internal/loopcmd/ -count=1 -p 1`
Expected: PASS. `internal/store/legacy.go` and `internal/legacydb` write the
LEGACY column subset and were checked to need no change; if either fails to
compile, extend it the same way and say so in the commit message.

- [ ] **Step 9: Commit**

```bash
git add internal/store/
git commit -m "feat: store the stopped flag, agent pid and dispatch overrides"
```

**Acceptance criteria:**
- `dispatchColumns` and `scanDispatch` list the same columns in the same order.
- The `issues` entry of `rebuilt` names both new columns.
- `PutIssueState` does not write `stopped` or `stopped_reason`, and a test
  proves a stale round-trip cannot un-stop an issue.
- `DB.StoppedIssues` keeps two projects' identically numbered issues apart.

---

## Task 3: The signal helpers

**review: yes** — this is where a wrong process could be signalled.

**Files:**
- Create: `internal/proc/signal.go`, `internal/proc/signal_test.go`

**Interfaces:**
- Consumes: `proc.IsAlive`, `proc.CommandLine`, `proc.DispatchFlag`,
  `proc.matchesDispatch`.
- Produces:
  ```go
  var ErrNotRunner = errors.New("not this dispatch's runner")
  func VerifyRunner(pid int, dispatchID int64) error
  func Signal(pid int, dispatchID int64, sig syscall.Signal) error
  func SignalGroup(pid int, sig syscall.Signal) error
  ```
  Task 5 uses all four.

- [ ] **Step 1: Write the failing test**

Create `internal/proc/signal_test.go`.

Two constraints shape the fixture:

- **Do not use `sleep` with extra operands.** `sleep 30 --dispatch 7` is a
  usage error, and the process exits immediately on both macOS and Linux, so
  the assertion would race a corpse.
- **Do not use `sh -c`.** It works, but the helper-process idiom below is the
  standard Go pattern, needs no shell, and re-executes the test binary itself —
  so the fixture is a real Go process whose argv this package controls exactly.

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

// TestSignalHelperProcess is not a real test. It is the fixture: the test
// binary re-executes ITSELF with this filter, so the child is a long-lived
// process whose argv this package controls exactly. It exits at once unless
// the parent set the marker variable, so a normal `go test` run skips it.
//
// This is the standard Go helper-process idiom. The alternative -- `sh -c` --
// needs a shell, and `sleep 30 --dispatch 7` is a usage error that exits
// immediately, which would make the fixture race a corpse.
func TestSignalHelperProcess(t *testing.T) {
	if os.Getenv("AGENT_UTILS_SIGNAL_HELPER") != "1" {
		return
	}
	time.Sleep(60 * time.Second)
}

// startFakeRunner starts a long-lived process whose command line carries the
// dispatch flag, so VerifyRunner recognises it as this dispatch's runner.
func startFakeRunner(t *testing.T, dispatchID string) *exec.Cmd {
	t.Helper()
	// The trailing DispatchFlag and id are what VerifyRunner matches on. They
	// are inert to the test binary's own flag parsing because they follow the
	// -test.run filter and name no registered flag... so pass them AFTER a
	// "--" separator to be certain.
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestSignalHelperProcess$", "--", DispatchFlag, dispatchID)
	cmd.Env = append(os.Environ(), "AGENT_UTILS_SIGNAL_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake runner: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	waitVisible(t, cmd.Process.Pid)
	return cmd
}

// startUnrelatedProcess starts a live process that is NOT a runner: same
// binary, no dispatch flag.
func startUnrelatedProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalHelperProcess$")
	cmd.Env = append(os.Environ(), "AGENT_UTILS_SIGNAL_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	waitVisible(t, cmd.Process.Pid)
	return cmd
}

func waitVisible(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if line, err := CommandLine(pid); err == nil && line != "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d never became visible to ps", pid)
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d is still alive", pid)
}

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

// SignalGroup negates its argument, so pid 1 becomes kill(-1, sig) -- every
// process this user owns. A positive-only check is NOT enough for a group
// signal, and 1 is a live identifier in any container.
func TestSignalGroupRefusesPIDOne(t *testing.T) {
	if err := SignalGroup(1, syscall.SIGKILL); err == nil {
		t.Fatal("SignalGroup(1) returned no error; it would broadcast to kill(-1)")
	}
}

// The operating system reuses process identifiers, so a stale row can name a
// live process that has nothing to do with this program.
func TestSignalRefusesAProcessThatIsNotTheRunner(t *testing.T) {
	cmd := startUnrelatedProcess(t)

	err := Signal(cmd.Process.Pid, 7, syscall.SIGTERM)
	if !errors.Is(err, ErrNotRunner) {
		t.Fatalf("Signal = %v, want ErrNotRunner", err)
	}
	// Refusing means NOT signalling. The process must still be alive.
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatalf("the refused process died anyway: %v", err)
	}
}

func TestSignalDeliversToTheRunner(t *testing.T) {
	cmd := startFakeRunner(t, "7")
	if err := Signal(cmd.Process.Pid, 7, syscall.SIGKILL); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	// No reaper goroutine: waitDead polls kill(pid, 0), and t.Cleanup already
	// owns the one Wait on this process. A second concurrent Wait on one
	// os.Process would be a data race the -race build could catch.
	waitDead(t, cmd.Process.Pid)
}

// The match is on whole tokens: "--dispatch 7" must not satisfy dispatch 70,
// which would strand dispatch 7 and kill the wrong runner.
func TestVerifyRunnerMatchesWholeTokens(t *testing.T) {
	cmd := startFakeRunner(t, "7")
	if err := VerifyRunner(cmd.Process.Pid, 7); err != nil {
		t.Fatalf("VerifyRunner(7) = %v, want nil", err)
	}
	if err := VerifyRunner(cmd.Process.Pid, 70); !errors.Is(err, ErrNotRunner) {
		t.Fatalf("VerifyRunner(70) = %v, want ErrNotRunner", err)
	}
}
```

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

// VerifyRunner reports an error unless pid is CONFIRMED to be the runner for
// dispatchID.
//
// It exists instead of a reuse of IsAlive because the two want OPPOSITE
// biases. IsAlive fails SAFE by reporting alive when ps fails: for liveness
// that is right, because a transient error must not cause a duplicate
// dispatch. For signalling the same bias is inverted -- a ps that failed means
// the process was never confirmed to be ours, and the safe answer is to refuse
// rather than to send a signal to a number that may now belong to anything.
//
// The checks are IsAlive's, minus the fail-open: the kernel must say the
// process exists, and its command line must carry this dispatch's --dispatch
// argument as whole tokens.
func VerifyRunner(pid int, dispatchID int64) error {
	if err := validPID(pid); err != nil {
		return err
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return fmt.Errorf("pid %d is not running: %w", pid, ErrNotRunner)
	}
	cmdline, err := CommandLine(pid)
	if err != nil {
		// Fail CLOSED. See the doc comment.
		return fmt.Errorf(
			"cannot read the command line of pid %d, so it is %w: %v",
			pid, ErrNotRunner, err)
	}
	if !matchesDispatch(cmdline, dispatchID) {
		return fmt.Errorf("pid %d: %w", pid, ErrNotRunner)
	}
	return nil
}

// Signal sends sig to pid, but only after VerifyRunner confirms that pid is
// the runner for dispatchID.
//
// The check is the point of the function. A process identifier read from a
// database row can be stale, and the operating system reuses identifiers, so
// the number may now name an unrelated program this command must not touch.
// The signal lives here, beside that rule, rather than in the command layer: a
// caller that could reach kill(2) without the check is exactly the mistake
// this package exists to prevent.
func Signal(pid int, dispatchID int64, sig syscall.Signal) error {
	if err := VerifyRunner(pid, dispatchID); err != nil {
		return err
	}
	if err := syscall.Kill(pid, sig); err != nil {
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	return nil
}

// SignalGroup sends sig to the process group led by pid.
//
// It takes a POSITIVE identifier and negates it here, because a caller passing
// a negative number directly would be one typo away from -1.
//
// There is no runner check, because the process this reaches is the AGENT, not
// the runner, and the agent carries no --dispatch argument. The caller is
// therefore responsible for establishing that the identifier is CURRENT -- see
// loopcmd.Kill, which refuses to call this unless the dispatch's runner
// verifies live.
func SignalGroup(pid int, sig syscall.Signal) error {
	// Stricter than validPID, and deliberately so: this function negates its
	// argument, and kill(-1, sig) signals EVERY process this user owns. 1 is
	// positive, is a live identifier in any container, and is exactly one
	// off-by-one away from that broadcast.
	if pid <= 1 {
		return fmt.Errorf(
			"refusing to signal the process group of pid %d: negated it would be a broadcast",
			pid)
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
// that came out of a database row -- which a truncated write or an old schema
// could leave at zero -- must never be handed to it. The listener's stop
// command refuses one for the same reason.
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
- `VerifyRunner` fails closed on a `CommandLine` error, and a test proves it
  matches whole tokens (dispatch 7 does not satisfy dispatch 70).
- `SignalGroup` refuses pid 1, with its own test.
- A refused signal leaves the target alive — asserted, not assumed.
- No test uses `sleep` with trailing operands, and none uses `sh -c`.

---

## Task 4: The engine — stop an issue and carry the override

**review: yes** — this decides what runs.

**Files:**
- Modify: `internal/engine/types.go`, `internal/engine/engine.go:49-138`,
  `internal/engine/engine_test.go`

**Interfaces:**
- Consumes: `config.ParseOverrides`, `config.Overrides`,
  `config.OverrideHarnessPrefix`, `config.HarnessClaude`, `config.HarnessPi`
  (Task 1); `store.IssueState.Stopped` (Task 2).
- Produces: `engine.KindStop`, `engine.Decision.Overrides config.Overrides`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/engine/engine_test.go`, using the file's `testConfig()`
helper (`:12`):

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

// A stopped issue must also be excluded from TENDING. tendDecisions skips only
// issues marked decided, so a stopped issue awaiting review with a behind pull
// request would otherwise get a tend agent force-pushing the branch of the
// very session the operator killed.
func TestDecideStopsATendableIssueFromBeingTended(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{
		Issues: []ghub.Issue{
			{Number: 7, Labels: []string{cfg.Labels.Review}, State: "open"},
		},
		PRs: []ghub.PullRequest{{
			Number: 100, HeadRef: "issue-7", BaseRef: "master",
			Trusted: true, Body: "Closes #7",
		}},
		BehindBy: map[int]int{100: 5},
	}
	st := State{Issues: map[int]store.IssueState{
		7: {Number: 7, Stopped: true, StoppedReason: "killed by operator"},
	}}

	plan := Decide(cfg, snap, st, time.Now())

	for _, d := range plan.Decisions {
		if d.Kind == KindTend {
			t.Fatalf("a stopped issue produced a tend decision: %+v", d)
		}
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

func TestDecideCarriesAValidOverrideOnAStart(t *testing.T) {
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

// retryDecision receives no labels, so the caller must attach the overrides to
// the decision it RETURNS. Without this every retry silently reverts to the
// configured model.
func TestDecideCarriesAValidOverrideOnARetry(t *testing.T) {
	cfg := testConfig()
	cfg.Retry.Max = 3
	snap := Snapshot{Issues: []ghub.Issue{
		{Number: 7, Labels: []string{
			cfg.Labels.Trigger, cfg.Labels.InFlight, "model:claude-opus-5",
		}, State: "open"},
	}}
	st := State{Issues: map[int]store.IssueState{
		7: {Number: 7, NeedsRetry: true, SessionID: "sess", SessionStarted: true},
	}}

	plan := Decide(cfg, snap, st, time.Now())

	if len(plan.Decisions) != 1 {
		t.Fatalf("decisions = %+v, want one retry", plan.Decisions)
	}
	d := plan.Decisions[0]
	if d.Kind != KindRetryResume && d.Kind != KindRetryStart {
		t.Fatalf("kind = %q, want a retry kind", d.Kind)
	}
	if d.Overrides.Model != "claude-opus-5" {
		t.Fatalf("Overrides.Model = %q, want the override to survive onto the retry",
			d.Overrides.Model)
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

// A harness override must not silently drop a safety setting the loop
// configured. PiBuildArgs emits no permission mode and no cost ceiling, so a
// harness:pi label on a claude loop that sets either would weaken exactly the
// bounds that exist because the agent reads third-party issue text.
func TestDecideStopsAHarnessOverrideThatWouldDropASafetySetting(t *testing.T) {
	for _, tt := range []struct {
		name string
		set  func(cfg *config.Config)
		want string
	}{
		{"permission mode", func(cfg *config.Config) { cfg.Agent.PermissionMode = "plan" }, "permission_mode"},
		{"budget ceiling", func(cfg *config.Config) { cfg.Agent.MaxBudgetUSD = 5 }, "max_budget_usd"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Agent.Harness = config.HarnessClaude
			tt.set(cfg)
			snap := Snapshot{Issues: []ghub.Issue{
				{Number: 7, Labels: []string{cfg.Labels.Trigger, "harness:pi"}, State: "open"},
			}}

			plan := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())

			if len(plan.Decisions) != 1 || plan.Decisions[0].Kind != KindStop {
				t.Fatalf("decisions = %+v, want one KindStop", plan.Decisions)
			}
			if !strings.Contains(plan.Decisions[0].Reason, tt.want) {
				t.Fatalf("reason = %q, want it to name %s", plan.Decisions[0].Reason, tt.want)
			}
		})
	}
}

// A harness override that drops nothing is allowed.
func TestDecideAllowsAHarnessOverrideWithNoSafetySetting(t *testing.T) {
	cfg := testConfig()
	cfg.Agent.Harness = config.HarnessClaude
	cfg.Agent.PermissionMode = ""
	cfg.Agent.MaxBudgetUSD = 0
	snap := Snapshot{Issues: []ghub.Issue{
		{Number: 7, Labels: []string{cfg.Labels.Trigger, "harness:pi"}, State: "open"},
	}}

	plan := Decide(cfg, snap, State{Issues: map[int]store.IssueState{}}, time.Now())

	if len(plan.Decisions) != 1 || plan.Decisions[0].Kind == KindStop {
		t.Fatalf("decisions = %+v, want a dispatch", plan.Decisions)
	}
	if plan.Decisions[0].Overrides.Harness != config.HarnessPi {
		t.Fatalf("Overrides.Harness = %q, want pi", plan.Decisions[0].Overrides.Harness)
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

// An invalid label must NOT block the repair decisions. KindClearRetry is the
// only thing that retires a retry flag the engine can no longer act on, so an
// issue whose label is bad AND whose flag is stale would be stranded
// permanently with no way back.
func TestDecideStillClearsAStaleRetryFlagWithAnInvalidOverride(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{
		// NeedsRetry but NOT in flight: the KindClearRetry path.
		{Number: 7, Labels: []string{cfg.Labels.Trigger, "harness:gpt"}, State: "open"},
	}}
	st := State{Issues: map[int]store.IssueState{7: {Number: 7, NeedsRetry: true}}}

	plan := Decide(cfg, snap, st, time.Now())

	if len(plan.Decisions) != 1 || plan.Decisions[0].Kind != KindClearRetry {
		t.Fatalf("decisions = %+v, want one KindClearRetry", plan.Decisions)
	}
}

// A tripped circuit breaker drops every DISPATCH, but a stop is the refusal
// to dispatch. Swallowing it would leave an invalid label unrecorded and
// unexplained for the whole cooldown.
func TestDecideStopsSurviveATrippedBreaker(t *testing.T) {
	cfg := testConfig()
	cfg.Retry.Max = 3
	cfg.Retry.Breaker.OrphanThreshold = 1
	snap := Snapshot{Issues: []ghub.Issue{
		{Number: 7, Labels: []string{cfg.Labels.Trigger, "harness:gpt"}, State: "open"},
		{Number: 8, Labels: []string{cfg.Labels.Trigger, cfg.Labels.InFlight}, State: "open"},
	}}
	st := State{Issues: map[int]store.IssueState{
		8: {Number: 8, NeedsRetry: true, SessionID: "sess", SessionStarted: true},
	}}

	plan := Decide(cfg, snap, st, time.Now())

	if !plan.BreakerTripped {
		t.Fatalf("plan = %+v, want the breaker tripped", plan)
	}
	var stops int
	for _, d := range plan.Decisions {
		if d.Kind == KindStop {
			stops++
			if !strings.Contains(d.Reason, "harness must be") {
				t.Fatalf("stop reason = %q, want the parse error kept", d.Reason)
			}
		}
	}
	if stops != 1 {
		t.Fatalf("decisions = %+v, want the stop for issue 7 to survive", plan.Decisions)
	}
}

// A label must not be able to push the breaker over its threshold. A retry
// that becomes a stop never dispatches, so it is not an eligible retry.
func TestDecideDoesNotCountAStoppedRetryTowardTheBreaker(t *testing.T) {
	cfg := testConfig()
	cfg.Retry.Max = 3
	cfg.Retry.Breaker.OrphanThreshold = 1
	snap := Snapshot{Issues: []ghub.Issue{
		{Number: 7, Labels: []string{
			cfg.Labels.Trigger, cfg.Labels.InFlight, "harness:gpt",
		}, State: "open"},
	}}
	st := State{Issues: map[int]store.IssueState{
		7: {Number: 7, NeedsRetry: true, SessionID: "sess", SessionStarted: true},
	}}

	plan := Decide(cfg, snap, st, time.Now())

	if plan.BreakerTripped {
		t.Fatal("a label-stopped retry tripped the circuit breaker")
	}
}

// A park is not a dispatch either: the retry cap is a fact about the issue,
// not about its labels.
func TestDecideStillParksWithAnInvalidOverride(t *testing.T) {
	cfg := testConfig()
	cfg.Retry.Max = 1
	snap := Snapshot{Issues: []ghub.Issue{
		{Number: 7, Labels: []string{
			cfg.Labels.Trigger, cfg.Labels.InFlight, "harness:gpt",
		}, State: "open"},
	}}
	st := State{Issues: map[int]store.IssueState{
		7: {Number: 7, NeedsRetry: true, RetryCount: 1},
	}}

	plan := Decide(cfg, snap, st, time.Now())

	if len(plan.Decisions) != 1 || plan.Decisions[0].Kind != KindParkRetryExhausted {
		t.Fatalf("decisions = %+v, want one KindParkRetryExhausted", plan.Decisions)
	}
}
```

Add `"strings"` and `"github.com/seanmcgary/agent-utils/internal/config"` to
the test imports if absent.

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
	// It writes only LOCAL state. parkRetryExhausted stays the one GitHub
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

- [ ] **Step 4: Add the stopped skip**

In `internal/engine/engine.go`, immediately AFTER the `liveIssues` block
(ending at `:65`) and BEFORE `state := st.Issues[iss.Number]` at `:67`:

```go
		// An operator stopped this issue, or the loop refused an override
		// label. The check sits ABOVE the retry path deliberately: a killed
		// dispatch records a FAILURE, so a stopped issue almost always carries
		// the retry flag as well, and a retry that won here would redispatch
		// the very issue the operator stopped.
		//
		// decided is set for the reason the live-dispatch branch above sets it.
		// tendDecisions skips only issues marked decided, so without it a
		// stopped issue awaiting review would still get a tend agent
		// force-pushing the branch of the session that was just killed.
		if st.Issues[iss.Number].Stopped {
			decided[iss.Number] = true
			reason := st.Issues[iss.Number].StoppedReason
			if reason == "" {
				reason = "an operator stopped this issue"
			}
			skips[iss.Number] = reason + "; clear it with `agent-utils sessions resume`"
			continue
		}
```

- [ ] **Step 5: Parse the overrides once, and gate only the dispatches**

Still inside the issue loop, immediately after the stopped skip and BEFORE the
`if state.NeedsRetry` branch at `:72`:

```go
		// Parse ONCE, here, above the retry path. retryDecision receives no
		// labels, so a parse below it could never reach a retry decision and
		// every retry would silently fall back to the configured model.
		//
		// The result is not ACTED on here. An invalid label must stop only a
		// DISPATCH; it must never block KindClearRetry or
		// KindParkRetryExhausted, which are repair actions. An issue whose
		// stale retry flag could no longer be cleared would be stranded
		// permanently, with re-applying the trigger label doing nothing.
		ov, ovErr := config.ParseOverrides(iss.Labels)
		if ovErr == nil {
			ovErr = validateOverrides(cfg, ov)
		}
```

Apply it at the dispatch sites, and only there.

First declare a slice beside `parks`, near `var decisions []Decision`:

```go
	// stops collects KindStop decisions. They are kept OUT of `decisions` for
	// the reason parks are: the circuit-breaker branch drops every entry of
	// `decisions` and rewrites its skip reason. A stop is not a dispatch --
	// it is the refusal to dispatch -- so a breaker tick must not swallow it,
	// or an invalid label would go unrecorded and unexplained for the whole
	// cooldown.
	var stops []Decision
```

Inside the `state.NeedsRetry` branch, replace the `d, eligible, skip :=
retryDecision(...)` block's opening so the stop conversion happens BEFORE the
eligibility count:

```go
			d, eligible, skip := retryDecision(cfg, iss.Number, state, now)
			// A retry IS a dispatch, so an invalid label stops it. A park is
			// not, so it proceeds unchanged.
			//
			// This runs BEFORE eligibleRetries is incremented. A retry that
			// becomes a stop never dispatches, so counting it would let a
			// label push the circuit breaker over its threshold and drop every
			// other loop's dispatches for the cooldown.
			if d != nil && d.Kind != KindParkRetryExhausted && ovErr != nil {
				decided[iss.Number] = true
				stops = append(stops, Decision{
					Kind: KindStop, Issue: iss.Number, Title: iss.Title,
					Reason: ovErr.Error(),
				})
				continue
			}
			if d != nil && d.Kind != KindParkRetryExhausted {
				d.Overrides = ov
			}
			if eligible {
				eligibleRetries++
			}
```

Delete the original `if eligible { eligibleRetries++ }` that followed the call,
so it is not counted twice.

And after the trigger-label check (`:113-119`), before
`decided[iss.Number] = true`:

```go
		if ovErr != nil {
			decided[iss.Number] = true
			stops = append(stops, Decision{
				Kind: KindStop, Issue: iss.Number, Title: iss.Title,
				Reason: ovErr.Error(),
			})
			continue
		}
```

Add `Overrides: ov,` to BOTH the `KindResume` and the `KindStart` decisions.

Finally, carry `stops` through BOTH exits, exactly as `parks` is carried. In the
breaker branch (`:159`), change `Decisions: parks` to
`Decisions: append(stops, parks...)`, and leave the loop that rewrites skip
reasons iterating `decisions` only — a stop already has its own reason and must
keep it. After that branch, beside `decisions = append(decisions, parks...)`,
add `decisions = append(decisions, stops...)`.

- [ ] **Step 6: Add `validateOverrides`**

At the end of `engine.go`:

```go
// validateOverrides applies the override rule that needs the loop's
// configuration as well as the labels.
//
// A harness override must not silently drop a safety setting the operator
// configured. config.validate FORBIDS agent.permission_mode together with
// harness: pi, and PiBuildArgs emits neither a permission mode nor a cost
// ceiling. So on a loop configured `harness: claude` with a restrictive
// permission mode and a budget, a harness:pi label would run the dispatch with
// NEITHER -- a label weakening exactly the two bounds that exist because the
// agent reads third-party issue text.
//
// There is deliberately no "pi requires a model" rule. agent.model is required
// for EVERY harness (config.validate), so a validated configuration always has
// one and such a rule could never fire.
func validateOverrides(cfg *config.Config, ov config.Overrides) error {
	if ov.Harness == "" {
		return nil
	}
	// An absent harness in the file means claude; config.Load normalises it,
	// but Decide must not depend on having been handed a normalised config.
	configured := cfg.Agent.Harness
	if configured == "" {
		configured = config.HarnessClaude
	}
	if ov.Harness == configured {
		return nil
	}
	if cfg.Agent.PermissionMode != "" {
		return fmt.Errorf(
			"label %q would change the harness to %q, which supports no permission mode, "+
				"but this loop sets agent.permission_mode %q; remove the label or the setting",
			config.OverrideHarnessPrefix+ov.Harness, ov.Harness, cfg.Agent.PermissionMode)
	}
	if cfg.Agent.MaxBudgetUSD > 0 {
		return fmt.Errorf(
			"label %q would change the harness to %q, which enforces no cost ceiling, "+
				"but this loop sets agent.max_budget_usd %g; remove the label or the setting",
			config.OverrideHarnessPrefix+ov.Harness, ov.Harness, cfg.Agent.MaxBudgetUSD)
	}
	return nil
}
```

- [ ] **Step 7: Run the tests and confirm they pass**

Run: `go test ./internal/engine/ -count=1`
Expected: PASS, including every pre-existing test unchanged.

- [ ] **Step 8: Commit**

```bash
git add internal/engine/
git commit -m "feat: skip stopped issues and carry label overrides in Decide"
```

**Acceptance criteria:**
- The stopped branch sets `decided`, and a test proves a stopped issue is not
  tended.
- The stopped check runs before the `NeedsRetry` branch, proven by a test.
- Overrides reach retry decisions, proven by a test.
- `KindClearRetry` and `KindParkRetryExhausted` still fire when the label is
  invalid, each proven by a test.
- A `KindStop` survives a tripped circuit breaker, and a stopped retry does not
  count toward the breaker threshold. Both proven by tests.
- `validateOverrides` has a reachable rule and a test for each branch.

---

## Task 5: The kill and resume actions

**review: yes** — the ordered procedure and its guards are the whole feature.

**Files:**
- Create: `internal/loopcmd/kill.go`, `internal/loopcmd/kill_test.go`

**Interfaces:**
- Consumes, from Task 2: `Store.MarkStopped`, `Store.ClearStopped`,
  `Store.StoppedIssues`, `DB.StoppedIssues`, `Dispatch.AgentPID`.
  From Task 3: `proc.VerifyRunner`, `proc.Signal`, `proc.SignalGroup`,
  `proc.ErrNotRunner`.
  Pre-existing: `loopcmd.Open`, `loopcmd.ProjectRef`, `loopcmd.AllSessions`,
  `loopcmd.ResolveProject`, `Store.RunningDispatches`, `DB.RunningDispatches`,
  `lock.Acquire`, `lock.ErrHeld`, `config.List`, `config.Resolve`,
  `registry.List`.
- Produces:
  ```go
  type Selector struct {
      Session string
      Issue   int
      All     bool
      Project string
      Loop    string
  }
  func (s Selector) Validate() error   // EXPORTED: cmd/agent-utils calls it
  func (s Selector) Describe() string  // for the confirmation prompt

  // Target is IDENTITY ONLY. It deliberately carries no store.Dispatch: a
  // loopcmd.Session has no repo, no pid and no dispatch id, so resolve cannot
  // build one. Kill and Resume open each loop anyway to take its lock, and
  // that is where cfg.Repo and a scoped Store exist -- so the dispatch rows
  // are read THERE, not here.
  type Target struct {
      ProjectID  string
      Project    string // display name
      Dir        string // the project's .agent-utils directory; ProjectRef.Dir
      Loop       string
      Issue      int
      Session    string // set only when --session selected this target
      ConfigPath string
  }

  // work is one target bound to the loop it was resolved in, after Open.
  type work struct {
      Target   Target
      Repo     string
      Dispatch store.Dispatch
  }
  type KillOptions struct {
      Selector Selector
      Force    bool
      Timeout  time.Duration
  }
  type Action string
  const (
      ActionSignalled   Action = "signalled"    // SIGTERM sent, runner exited
      ActionAlreadyGone Action = "already gone" // no verifiable runner; outcome recorded
      ActionForced      Action = "forced"       // SIGKILLed agent group, then runner
      ActionStillAlive  Action = "still alive"  // stopped, but the runner outlived the timeout
      ActionResumed     Action = "resumed"
      ActionRefused     Action = "refused"      // resume only: the runner is still live
  )
  type Result struct {
      Target Target
      Action Action
      Err    error
  }
  // narrowByLoop and resolve are unexported; Kill/Resume are the entry points.
  func Kill(opts KillOptions) ([]Result, error)
  func Resume(sel Selector) ([]Result, error)
  func RenderResults(verb string, rs []Result) string
  ```
  Task 7 calls `Validate`, `Describe`, `Kill`, `Resume`, and `RenderResults`.

- [ ] **Step 1: Write the failing selector and render tests**

Create `internal/loopcmd/kill_test.go`:

```go
package loopcmd

import (
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/proc"
	"github.com/seanmcgary/agent-utils/internal/store"
)

func TestSelectorValidate(t *testing.T) {
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
			err := tt.sel.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("Validate() = nil, want an error naming %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("Validate() = %v, want it to name %q", err, tt.wantErr)
			}
		})
	}
}

// An empty report has two meanings that read very differently, exactly as
// RenderAllSessions documents.
func TestRenderResultsNamesEveryTargetAndItsOutcome(t *testing.T) {
	rs := []Result{
		{Target: Target{Project: "p", Loop: "l", Issue: 7}, Action: ActionSignalled},
		{Target: Target{Project: "p", Loop: "l", Issue: 9}, Action: ActionStillAlive},
		{Target: Target{Project: "p", Loop: "l", Issue: 11}, Err: errors.New("boom")},
	}
	out := RenderResults("killed", rs)
	for _, want := range []string{"7", "9", "11", "signalled", "still alive", "boom"} {
		if !strings.Contains(out, want) {
			t.Fatalf("RenderResults omitted %q:\n%s", want, out)
		}
	}
	// The timeout case must tell the operator what to do next.
	if !strings.Contains(out, "--force") {
		t.Fatalf("RenderResults does not name --force for a still-alive runner:\n%s", out)
	}
	if empty := RenderResults("killed", nil); !strings.Contains(empty, "nothing") {
		t.Fatalf("empty report = %q, want it to say nothing matched", empty)
	}
}
```

- [ ] **Step 2: Write the failing ordering tests**

The ordered procedure is the feature. `killer` is a struct of function fields
so it can be driven without real processes; the real constructor fills them
with the store and `proc` calls. Append:

```go
var errWriteFailed = errors.New("write failed")

func testWork(runnerPID, agentPID int) work {
	return work{
		Target: Target{Project: "p", Loop: "l", Issue: 7},
		Repo:   "o/r",
		Dispatch: store.Dispatch{
			ID: 1, PID: runnerPID, AgentPID: agentPID,
			Status: store.StatusRunning, Number: 7, Kind: store.KindStart,
		},
	}
}

// The stopped flag MUST be written before the signal. A tick that runs in the
// window between the agent dying and the flag being written would see the
// trigger label and no live dispatch, and would start a new agent -- exactly
// what the operator asked not to happen.
func TestKillWritesTheStoppedFlagBeforeItSignals(t *testing.T) {
	var order []string
	k := killer{
		markStopped: func(work, string) error { order = append(order, "stopped"); return nil },
		verify:      func(work) error { return nil },
		signal:      func(work) error { order = append(order, "signalled"); return nil },
		waitGone:    func(work, time.Duration) bool { return true },
		reread:      func(w work) (store.Dispatch, error) { return w.Dispatch, nil },
		finish:      func(work) error { return nil },
	}

	res, err := k.one(testWork(11, 22), KillOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("one: %v", err)
	}
	if res.Action != ActionSignalled {
		t.Fatalf("action = %q, want %q", res.Action, ActionSignalled)
	}
	if len(order) != 2 || order[0] != "stopped" || order[1] != "signalled" {
		t.Fatalf("order = %v, want [stopped signalled]", order)
	}
}

// A failure to write the flag must abandon the target. Signalling anyway would
// kill the agent and let the next tick start another one.
func TestKillDoesNotSignalWhenTheFlagCannotBeWritten(t *testing.T) {
	signalled := false
	k := killer{
		markStopped: func(work, string) error { return errWriteFailed },
		verify:      func(work) error { return nil },
		signal:      func(work) error { signalled = true; return nil },
	}

	res, _ := k.one(testWork(11, 22), KillOptions{})
	if res.Err == nil {
		t.Fatal("one() recorded no error, want the write error")
	}
	if signalled {
		t.Fatal("the command signalled after the flag write failed")
	}
}

// A runner that cannot be verified is "already gone": the command records the
// outcome and does NOT signal a number that may now belong to anything.
func TestKillRecordsWithoutSignallingWhenTheRunnerIsGone(t *testing.T) {
	signalled, finished := false, false
	k := killer{
		markStopped: func(work, string) error { return nil },
		verify:      func(work) error { return proc.ErrNotRunner },
		signal:      func(work) error { signalled = true; return nil },
		finish:      func(work) error { finished = true; return nil },
	}

	res, err := k.one(testWork(11, 22), KillOptions{})
	if err != nil {
		t.Fatalf("one: %v", err)
	}
	if signalled {
		t.Fatal("the command signalled a runner it could not verify")
	}
	if !finished || res.Action != ActionAlreadyGone {
		t.Fatalf("action = %q, finished = %v; want the outcome recorded", res.Action, finished)
	}
}

// --force kills the AGENT first. SIGKILL on the runner alone leaves the agent
// alive in a worktree the loop believes is free.
func TestForceKillsTheAgentBeforeTheRunner(t *testing.T) {
	var order []string
	k := killer{
		markStopped: func(work, string) error { return nil },
		verify:      func(work) error { return nil },
		killAgent:   func(work) error { order = append(order, "agent"); return nil },
		killRunner:  func(work) error { order = append(order, "runner"); return nil },
		finish:      func(work) error { order = append(order, "finish"); return nil },
	}

	res, err := k.one(testWork(11, 22), KillOptions{Force: true})
	if err != nil {
		t.Fatalf("one: %v", err)
	}
	if res.Action != ActionForced {
		t.Fatalf("action = %q, want %q", res.Action, ActionForced)
	}
	want := []string{"agent", "runner", "finish"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// --force must NOT group-kill an unverified agent_pid. agent_pid is written
// once and never cleared, so a row whose runner died carries a STALE number --
// and after a reboot that number leads an unrelated process group.
func TestForceDoesNotKillTheAgentGroupWhenTheRunnerIsUnverified(t *testing.T) {
	agentKilled := false
	k := killer{
		markStopped: func(work, string) error { return nil },
		verify:      func(work) error { return proc.ErrNotRunner },
		killAgent:   func(work) error { agentKilled = true; return nil },
		killRunner:  func(work) error { return nil },
		finish:      func(work) error { return nil },
	}

	res, err := k.one(testWork(11, 22), KillOptions{Force: true})
	if err != nil {
		t.Fatalf("one: %v", err)
	}
	if agentKilled {
		t.Fatal("--force SIGKILLed a process group it never verified as current")
	}
	if res.Action != ActionAlreadyGone {
		t.Fatalf("action = %q, want %q", res.Action, ActionAlreadyGone)
	}
}

// A signal that fails for a real reason (EPERM) is NOT "already gone". The
// issue stays stopped, but the report must not imply the agent is dead.
func TestKillReportsARealSignalFailure(t *testing.T) {
	k := killer{
		markStopped: func(work, string) error { return nil },
		verify:      func(work) error { return nil },
		signal:      func(work) error { return syscall.EPERM },
	}

	res, _ := k.one(testWork(11, 22), KillOptions{Timeout: time.Millisecond})
	if res.Err == nil {
		t.Fatal("a failed signal was not reported")
	}
	if res.Action == ActionAlreadyGone || res.Action == ActionSignalled {
		t.Fatalf("action = %q, want a failure action", res.Action)
	}
}

// A runner that outlives the timeout leaves the issue stopped and safe, but
// the operator must be told and pointed at --force.
func TestKillReportsARunnerThatOutlivesTheTimeout(t *testing.T) {
	k := killer{
		markStopped: func(work, string) error { return nil },
		verify:      func(work) error { return nil },
		signal:      func(work) error { return nil },
		waitGone:    func(work, time.Duration) bool { return false },
	}

	res, err := k.one(testWork(11, 22), KillOptions{Timeout: time.Millisecond})
	if err != nil {
		t.Fatalf("one: %v", err)
	}
	if res.Action != ActionStillAlive {
		t.Fatalf("action = %q, want %q", res.Action, ActionStillAlive)
	}
}

// The runner records its own outcome on the graceful path, so the command
// re-reads the row after the wait rather than assuming.
func TestKillDoesNotDoubleRecordWhenTheRunnerRecordedItsOwnOutcome(t *testing.T) {
	finished := false
	k := killer{
		markStopped: func(work, string) error { return nil },
		verify:      func(work) error { return nil },
		signal:      func(work) error { return nil },
		waitGone:    func(work, time.Duration) bool { return true },
		reread: func(w work) (store.Dispatch, error) {
			d := w.Dispatch
			d.Status = store.StatusFailed // the runner's handler got there first
			return d, nil
		},
		finish: func(work) error { finished = true; return nil },
	}

	if _, err := k.one(testWork(11, 22), KillOptions{Timeout: time.Second}); err != nil {
		t.Fatalf("one: %v", err)
	}
	if finished {
		t.Fatal("the command recorded an outcome the runner had already written")
	}
}

// A tend dispatch holds no issue state, so there is no flag to write.
func TestKillDoesNotStopAnIssueForATendDispatch(t *testing.T) {
	stopped := false
	w := testWork(11, 22)
	w.Dispatch.Kind = store.KindTend
	k := killer{
		markStopped: func(work, string) error { stopped = true; return nil },
		verify:      func(work) error { return nil },
		signal:      func(work) error { return nil },
		waitGone:    func(work, time.Duration) bool { return true },
		reread:      func(w work) (store.Dispatch, error) { return w.Dispatch, nil },
		finish:      func(work) error { return nil },
	}

	if _, err := k.one(w, KillOptions{Timeout: time.Second}); err != nil {
		t.Fatalf("one: %v", err)
	}
	if stopped {
		t.Fatal("a tend kill wrote an issue flag; a tend dispatch holds no issue state")
	}
}
```

- [ ] **Step 3: Write the failing resolution tests**

`narrowByLoop` holds the ambiguity rule an acceptance criterion names, so it is
tested directly:

```go
// An issue number is unique within a LOOP, not within a project. A project
// with two loops can hold two rows for one number, and guessing which one the
// operator meant is not acceptable for a destructive command.
func TestNarrowByLoopRejectsAnAmbiguousIssue(t *testing.T) {
	candidates := []Target{
		{Project: "p", Loop: "planning", Issue: 7},
		{Project: "p", Loop: "execution", Issue: 7},
	}
	_, err := narrowByLoop(candidates, "")
	if err == nil {
		t.Fatal("narrowByLoop accepted an ambiguous issue")
	}
	for _, want := range []string{"planning", "execution", "--loop"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}

func TestNarrowByLoopAcceptsANarrowedIssue(t *testing.T) {
	candidates := []Target{
		{Project: "p", Loop: "planning", Issue: 7},
		{Project: "p", Loop: "execution", Issue: 7},
	}
	got, err := narrowByLoop(candidates, "planning")
	if err != nil {
		t.Fatalf("narrowByLoop: %v", err)
	}
	if len(got) != 1 || got[0].Loop != "planning" {
		t.Fatalf("narrowByLoop = %+v, want only the planning row", got)
	}
}

// One loop's row is never ambiguous, with or without --loop.
func TestNarrowByLoopAcceptsASingleCandidate(t *testing.T) {
	got, err := narrowByLoop([]Target{{Project: "p", Loop: "planning", Issue: 7}}, "")
	if err != nil {
		t.Fatalf("narrowByLoop: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("narrowByLoop = %+v, want the one row", got)
	}
}
```

- [ ] **Step 4: Write the failing Kill, Resume and Describe tests**

`killer.one` is unit-tested above. These cover the rules that live in `Kill`
and `Resume` themselves, each of which an acceptance criterion names. Drive
them through the same seam: give `Kill` and `Resume` an unexported
`runLoop` function field (defaulting to the real Open-and-lock pass) so a test
can supply loops without a real project on disk. Append:

```go
// A tick holding the loop lock must fail EVERY target of that loop, with the
// wording `loop reset` uses -- and must not abandon the other loops.
func TestKillReportsALockedLoopAndContinuesToTheNext(t *testing.T) {
	targets := []Target{
		{Project: "p", Loop: "locked", Issue: 7},
		{Project: "p", Loop: "locked", Issue: 8},
		{Project: "p", Loop: "free", Issue: 9},
	}
	rs := killTargets(targets, KillOptions{}, func(loop string) (loopPass, error) {
		if loop == "locked" {
			return nil, lock.ErrHeld
		}
		return func(w work, _ KillOptions) (Result, error) {
			return Result{Target: w.Target, Action: ActionSignalled}, nil
		}, nil
	})

	if len(rs) != 3 {
		t.Fatalf("results = %+v, want one per target", rs)
	}
	for _, r := range rs[:2] {
		if r.Err == nil || !strings.Contains(r.Err.Error(), "a tick is running for loop") {
			t.Fatalf("result = %+v, want the held-lock wording", r)
		}
	}
	if rs[2].Err != nil || rs[2].Action != ActionSignalled {
		t.Fatalf("the unlocked loop was abandoned: %+v", rs[2])
	}
}

// Resume must refuse while the runner is still alive, or its retry clear is
// written straight back by the dying runner's finish().
func TestResumeRefusesALiveRunner(t *testing.T) {
	cleared := false
	r := resumeOne(
		work{
			Target:   Target{Project: "p", Loop: "l", Issue: 7},
			Repo:     "o/r",
			Dispatch: store.Dispatch{ID: 1, PID: 11, Status: store.StatusRunning},
		},
		func(work) error { return nil },                 // verify: the runner IS live
		func(work) error { cleared = true; return nil }, // clearStopped
	)

	if cleared {
		t.Fatal("resume cleared the stopped flag while the runner was still alive")
	}
	if r.Action != ActionRefused || r.Err == nil {
		t.Fatalf("result = %+v, want a refusal naming the dispatch", r)
	}
	if !strings.Contains(r.Err.Error(), "--force") {
		t.Fatalf("refusal %q does not tell the operator what to do next", r.Err)
	}
}

// A dead runner is the normal case: the flag clears.
func TestResumeClearsWhenTheRunnerIsGone(t *testing.T) {
	cleared := false
	r := resumeOne(
		work{
			Target:   Target{Project: "p", Loop: "l", Issue: 7},
			Repo:     "o/r",
			Dispatch: store.Dispatch{ID: 1, PID: 11, Status: store.StatusRunning},
		},
		func(work) error { return proc.ErrNotRunner },
		func(work) error { cleared = true; return nil },
	)

	if !cleared || r.Action != ActionResumed {
		t.Fatalf("result = %+v, cleared = %v; want the flag cleared", r, cleared)
	}
}

// Describe is what the confirmation prompt shows before a destructive --all.
func TestSelectorDescribe(t *testing.T) {
	tests := []struct {
		sel  Selector
		want []string
	}{
		{Selector{Session: "abc"}, []string{"abc"}},
		{Selector{Issue: 7, Loop: "planning"}, []string{"7", "planning"}},
		{Selector{All: true}, []string{"every"}},
		{Selector{All: true, Project: "p"}, []string{"every", "p"}},
	}
	for _, tt := range tests {
		got := tt.sel.Describe()
		for _, want := range tt.want {
			if !strings.Contains(got, want) {
				t.Fatalf("Describe(%+v) = %q, want it to name %q", tt.sel, got, want)
			}
		}
	}
}
```

Name the seam types in the implementation step below; `loopPass` is
`func(work, KillOptions) (Result, error)`, and `killTargets` /`resumeOne` are
the unexported cores `Kill` and `Resume` wrap.

- [ ] **Step 5: Write `internal/loopcmd/kill.go`**

Implement in this order. Every rule below must appear in the code WITH a
comment saying why — this file is the feature.

1. `Selector`, with **exported** `Validate()` (package `main` calls it) and
   `Describe()`. Exactly one of `Session`, `Issue`, `All` must be set.
2. `Target`, `KillOptions`, `Action` with its constants, `Result`.
3. `narrowByLoop(candidates []Target, loop string) ([]Target, error)` — the
   ambiguity rule, tested in step 3.
4. `resolve(sel Selector, forResume bool) ([]Target, error)` — identity only.
   It never reads a dispatch row; see the `Target` comment for why.
   - `Session`: `AllSessions(SessionFilter{Project: sel.Project, Loop: sel.Loop})`
     matched on `Session.ID`. A session names one project, one loop, one issue.
     Set `Target.Session` so the loop pass can pick the right dispatch row.
   - `Issue`: resolve the project with `ResolveProject(sel.Project)`, list its
     loops with `config.List(p.Dir)`, build a candidate per loop, then
     `narrowByLoop`.
   - `All`: for `Kill`, `db.RunningDispatches()`; for `Resume`,
     `db.StoppedIssues()`. Narrow both by `sel.Project` and `sel.Loop`, and map
     each row's `ProjectID` back to a registry entry for `Project` and `Dir`.
   - **Fill `Dir` from the registry entry's `AgentUtilsDir`, and `ConfigPath`
     with `config.Resolve(p.Dir, loopName)`.** `Dir` is not optional: it becomes
     `ProjectRef.Dir`, which drives `cfg.ResolveWorkDirs` and
     `migrate.Discover` (`internal/loopcmd/open.go:114`, `:179`). An empty one
     silently resolves different worktree paths and, under
     `MigrationPolicy: FailOnUnimported`, turns into a hard error.
   - A target whose configuration cannot be resolved becomes a FAILED `Result`,
     never a fatal error: one broken loop must not abandon the rest.
5. `killer` — the struct of function fields the tests drive:
   `markStopped`, `verify`, `signal`, `waitGone`, `reread`, `finish`,
   `killAgent`, `killRunner`.
6. `(k killer) one(w work, opts KillOptions) (Result, error)`, implementing
   spec section 4.2 exactly:
   - A TEND dispatch sets no flag — it holds no issue state
     (`internal/runner/runner.go:311`). Skip `markStopped` only; still signal
     and record.
   - `markStopped` FIRST for every other kind. On error, return the failed
     `Result` and do NOT signal.
   - `verify` next. On `proc.ErrNotRunner` or a non-positive identifier:
     `ActionAlreadyGone`, `finish`, no signal — and under `--force`, no
     `killAgent` either.
   - Under `--force`: `killAgent`, then `killRunner`, then `finish`,
     `ActionForced`.
   - Otherwise `signal` (SIGTERM), then `waitGone(opts.Timeout)`.
     - gone: `reread` the row. If it is no longer `running`, the runner recorded
       its own outcome — do NOT `finish`. If it is still `running`, `finish`.
       `ActionSignalled`.
     - not gone: `ActionStillAlive`, no `finish`. The flag is already written,
       so the issue is safe; the report names `--force`.
   - A `signal` error that is NOT "already gone" sets `Result.Err` and must not
     claim the agent is dead.
7. `Kill(opts KillOptions) ([]Result, error)` — `Validate`, `resolve`, group
   the targets by loop, then per loop:
   - `Open(ProjectRef{ID: t.ProjectID, Name: t.Project, Dir: t.Dir},
     t.ConfigPath, Options{RequireGitHub: false, MigrationPolicy:
     FailOnUnimported})`.
   - `lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))`. On
     `lock.ErrHeld`, report EVERY target of that loop as failed with the
     `loop reset` wording — `a tick is running for loop %q; try again` — and
     move to the next loop.
   - **Bind each target to its dispatch row HERE**, where `cfg.Repo` and a
     scoped `Store` exist: `deps.Store.RunningDispatches(cfg.Name, cfg.Repo)`,
     matched on `Target.Session` when set and on `Target.Issue` otherwise. A
     target with no running dispatch yields a `Result` saying so, not an error.
   - Run `one` per `work`. Release the lock before the next loop.
8. `Resume(sel Selector) ([]Result, error)` — the same resolution, opening and
   locking, then `ClearStopped(cfg.Name, cfg.Repo, t.Issue, now)`.

   It REFUSES a target whose dispatch is still marked running and whose runner
   VERIFIES LIVE, reporting `ActionRefused`. The runner holds no lock, and its
   `finish` calls `MarkNeedsRetry` (`internal/runner/runner.go:321`), so a
   resume issued while the runner is still dying would have its retry clear
   written straight back — leaving the issue un-stopped AND flagged for retry,
   so the next tick takes the retry path instead of a clean start. The refusal
   names the dispatch and says to wait or to use `kill --force`.
9. `RenderResults(verb string, rs []Result) string` — one line per target
   naming project, loop, issue, and action; `--force` named on
   `ActionStillAlive`; the "nothing matched" sentence when `rs` is empty.

The recorded outcome is
`store.DispatchResult{Status: store.StatusFailed, ExitCode: -1, APIError: "killed by operator"}`,
written with `store.FinishDispatch` and **not** `runner.Finish`. Comment why:
`runner.Finish` also calls `MarkNeedsRetry`, and `internal/loopcmd/tick.go:604`
warns against skipping that — but here arming a retry is exactly wrong, because
the issue is stopped and `ClearStopped` clears the flag on resume anyway.

- [ ] **Step 6: Run the tests and confirm they pass**

Run: `go test ./internal/loopcmd/ -count=1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/loopcmd/kill.go internal/loopcmd/kill_test.go
git commit -m "feat: add the session kill and resume actions"
```

**Acceptance criteria:**
- Tests prove: flag before signal; no signal after a failed flag write;
  `--force` kills the agent before the runner; `--force` does NOT group-kill an
  unverified `agent_pid`; a real signal failure is not reported as success; a
  timeout is reported and names `--force`; no double-recording when the runner
  wrote its own outcome; a tend kill writes no issue flag.
- `narrowByLoop` has tests for the ambiguous, narrowed, and single cases.
- `RenderResults` and `Selector.Describe` each have a test, including
  `RenderResults`' empty case.
- A held loop lock fails every target of that loop with the `loop reset`
  wording and does NOT abandon the other loops. Proven by a test.
- `Resume` refuses a live runner and clears a dead one. Both proven by tests.
- `Selector.Validate` is EXPORTED.
- `Target` carries `Dir`, and it reaches `ProjectRef.Dir`.
- No `Action` constant is dead.

---

## Task 6: The runner — signal handling, agent pid, and effective settings

**review: yes** — a mistake here orphans agents.

**Files:**
- Modify: `internal/runner/args.go`, `internal/runner/runner.go:139-160`, `:196`
- Modify: `internal/loopcmd/tick.go` — `Summary` at `:97`, `act` at `:359`,
  `dispatch` at `:431`, `RunAgent` at `:615`
- Modify: `cmd/agent-utils/main.go:698`
- Modify: `internal/runner/args_test.go`, `internal/runner/runner_test.go`,
  `internal/loopcmd/tick_test.go`
- Create: `cmd/agent-utils/runagent.go`, `cmd/agent-utils/runagent_test.go`

**Interfaces:**
- Consumes, from Task 1: `config.Overrides`, `config.ParseOverrides`,
  `config.OverrideModelPrefix`, `config.OverrideHarnessPrefix`,
  `config.OverrideEffortPrefix`.
  From Task 2: `Store.SetDispatchAgentPID`, `Store.MarkStopped`,
  `Dispatch.Model`, `Dispatch.Harness`, `Dispatch.Effort`.
  From Task 4: `engine.Decision.Overrides`, `engine.KindStop`.
- Produces:
  ```go
  type Settings struct{ Harness, Model, Effort string }
  func Effective(cfg *config.Config, ov config.Overrides) Settings
  // runner.Invocation gains: Overrides config.Overrides
  // loopcmd.Summary gains:   Stopped int `json:"stopped"`
  // cmd/agent-utils gains:   func runAgentContext(ctx context.Context) (context.Context, context.CancelFunc)
  ```

- [ ] **Step 1: Write the failing `Effective` tests**

Append to `internal/runner/args_test.go`. **Use the package's existing `joined`
helper (`:21`)** rather than a new assertion helper:

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

	// The configuration must be untouched. A caller holding cfg afterwards
	// would otherwise have one that no longer matches the file it was loaded
	// from -- including the retry policy and the log paths derived from it.
	if cfg.Agent.Model != "configured-model" {
		t.Fatalf("Effective mutated cfg.Agent.Model to %q", cfg.Agent.Model)
	}
}

// Defence in depth. The values come off a dispatch row that ANOTHER process
// wrote, possibly under an older binary, and this process never parsed them.
func TestEffectiveDropsARowValueThatWouldNotParseToday(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Model = "configured-model"

	got := Effective(cfg, config.Overrides{Model: "--dangerously-skip-permissions"})
	if got.Model != "configured-model" {
		t.Fatalf("Model = %q, want the flag-shaped row value dropped", got.Model)
	}
}

func TestBuildArgsUsesTheOverride(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Model = "configured-model"

	args := joined(BuildArgs(cfg, Invocation{
		SessionID: "s", Prompt: "p",
		Overrides: config.Overrides{Model: "override-model", Effort: "high"},
	}))

	if !strings.Contains(args, "--model override-model") {
		t.Fatalf("args = %q, want --model override-model", args)
	}
	if !strings.Contains(args, "--effort high") {
		t.Fatalf("args = %q, want --effort high", args)
	}
	if strings.Contains(args, "configured-model") {
		t.Fatalf("args = %q, want the configured model replaced", args)
	}
}

func TestPiBuildArgsUsesTheOverride(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agent.Harness = config.HarnessPi
	cfg.Agent.Model = "configured-model"

	args := joined(PiBuildArgs(cfg, Invocation{
		SessionID: "s", Prompt: "p",
		Overrides: config.Overrides{Model: "override-model"},
	}))

	if !strings.Contains(args, "--model override-model") {
		t.Fatalf("args = %q, want --model override-model", args)
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/runner/ -run 'TestEffective|UsesTheOverride' -count=1`
Expected: FAIL — `undefined: Effective`.

- [ ] **Step 3: Implement `Settings` and `Effective`**

In `internal/runner/args.go`, add `Overrides config.Overrides` to `Invocation`,
then:

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
// Supervise. They MUST agree: a run whose arguments say one harness and whose
// binary is another fails in a way that is very hard to read.
func Effective(cfg *config.Config, ov config.Overrides) Settings {
	s := Settings{
		Harness: cfg.Agent.Harness,
		Model:   cfg.Agent.Model,
		Effort:  cfg.Agent.Effort,
	}
	// Defence in depth. These values came off a dispatch row, and THIS process
	// did not parse them -- the tick did, in another process, possibly under an
	// older binary, and store.legacy.go writes the dispatches table by a second
	// path. A value that would not pass the parser today is dropped rather than
	// placed in an argument list.
	if ov.Harness != "" && validRowOverride(config.OverrideHarnessPrefix, ov.Harness) {
		s.Harness = ov.Harness
	}
	if ov.Model != "" && validRowOverride(config.OverrideModelPrefix, ov.Model) {
		s.Model = ov.Model
	}
	if ov.Effort != "" && validRowOverride(config.OverrideEffortPrefix, ov.Effort) {
		s.Effort = ov.Effort
	}
	return s
}

// validRowOverride re-checks one row value through the parser that produced it.
func validRowOverride(prefix, value string) bool {
	_, err := config.ParseOverrides([]string{prefix + value})
	return err == nil
}
```

Rewrite `BuildArgs` and `PiBuildArgs` to open with
`s := Effective(cfg, inv.Overrides)` and read `s.Model` / `s.Effort` in place
of `cfg.Agent.Model` / `cfg.Agent.Effort`. Change nothing else.

- [ ] **Step 4: Use the effective harness in Supervise**

In `internal/runner/runner.go`, compute `s := Effective(cfg, inv.Overrides)`
once before the harness switch and replace both
`cfg.Agent.Harness == config.HarnessPi` comparisons (`:143` and `:230`) with
`s.Harness == config.HarnessPi`. The first also selects `extraEnv`, so the
override correctly stops `claudeEnv` reaching a pi child.

- [ ] **Step 5: Record the agent's process identifier**

In `internal/runner/runner.go`, immediately after the successful `cmd.Start()`
(`:196`):

```go
	// Record the agent's process identifier. It leads its OWN process group
	// (see the SysProcAttr above), which the runner's group does not cover, so
	// nothing outside this process could otherwise reach it. `sessions kill
	// --force` needs it: a SIGKILL to the runner alone would leave the agent
	// working in a worktree the loop believes is free.
	//
	// A failure here is logged and ignored. The agent is already running, and
	// abandoning the run over a bookkeeping write would be a far worse outcome
	// than a --force that has to fall back to the runner alone.
	if err := st.SetDispatchAgentPID(d.ID, cmd.Process.Pid); err != nil {
		slog.Warn("record agent pid", "dispatch", d.ID, "err", err)
	}
```

**`internal/runner/runner.go` does not currently import `log/slog`.** Add it.
This makes the runner the first package in its path to log; that is
deliberate — the alternative is a silent failure of the one field `--force`
depends on.

- [ ] **Step 6: Persist and read the overrides in the tick**

In `internal/loopcmd/tick.go`:

- In `dispatch` (the `store.Dispatch` literal at `:431`), add
  `Model: d.Overrides.Model, Harness: d.Overrides.Harness, Effort: d.Overrides.Effort`.
  A tend decision carries no overrides, so this writes empty strings for it,
  which is spec section 6.7's rule with no extra branch.
- In `RunAgent` (`:615`):

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

- [ ] **Step 7: Apply a `KindStop` decision in the tick**

`act` returns `unknown decision kind` for anything it does not handle
(`internal/loopcmd/tick.go:382`), so the case is required.

Add to `Summary` (`:97`, beside `Parked`): `Stopped int \`json:"stopped"\``

Add to the switch in `act` (`:359`), beside `KindClearRetry`:

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

Add to `internal/loopcmd/tick_test.go`, using the file's `fakeGH` and its
existing tick harness:

```go
// An issue whose override label the loop cannot honour is stopped, not
// dispatched, and NOTHING is written to GitHub.
func TestTickStopsAnIssueWithAnInvalidOverrideLabel(t *testing.T) {
	// Build a tick whose snapshot holds ONE triggered issue carrying
	// "harness:gpt", following this file's existing tick harness.
	//
	// Assert, in this order:
	//   1. sum.Started == 0 && sum.Stopped == 1.
	//   2. The issue state is Stopped and its reason names the harness.
	//   3. The fakeGH recorded NO EditLabels call and NO comment. This is the
	//      assertion that must not be dropped: it proves the one-GitHub-write
	//      invariant survives.
	//   4. A second tick over the same snapshot dispatches nothing, adds no
	//      second dispatch row, and still writes nothing to GitHub.
}
```

Write the body with the file's own helpers. **A commented-out body does not
satisfy this step.**

- [ ] **Step 8: Add the signal handler, in a testable form**

Create `cmd/agent-utils/runagent.go`:

```go
package main

import (
	"context"
	"os/signal"
	"syscall"
)

// runAgentContext returns a context that cancels on SIGINT or SIGTERM.
//
// The runner is the ONLY command that needs this. It supervises a long-lived
// agent, and `sessions kill` stops it with a SIGTERM; without a handler the
// process would die on the spot, recording nothing and leaving the agent alive
// in its own process group. With one, the cancel walks the path Supervise
// already has: SIGTERM to the agent's group, Wait, the SIGKILL sweep, and
// finish() writing the outcome.
//
// It is a named function rather than two lines inside the action closure so
// that the wiring can be TESTED. A handler that is never installed fails
// silently and stays invisible until an operator's kill orphans an agent.
func runAgentContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
}
```

In `cmd/agent-utils/main.go`, in the `run-agent` action (`:698`), before
`loopcmd.RunAgent`:

```go
					ctx, stop := runAgentContext(ctx)
					defer stop()
					return loopcmd.RunAgent(ctx, cfg, deps, int64(c.Int("dispatch")))
```

Create `cmd/agent-utils/runagent_test.go`:

```go
package main

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

// A SIGTERM must cancel the context the runner supervises under. Without the
// handler the process dies instead, recording nothing.
//
// The test signals ITSELF. signal.NotifyContext installs a handler, so the
// default terminate action does not run while the notification is registered --
// which is precisely the behaviour under test.
func TestRunAgentContextCancelsOnSIGTERM(t *testing.T) {
	ctx, stop := runAgentContext(context.Background())
	defer stop()

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("signal self: %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not cancel the context")
	}
}
```

- [ ] **Step 9: Write the end-to-end supervised-cancel test**

Append to `internal/runner/runner_test.go`, using the package's `stubClaude`
helper (`:18`):

```go
// Cancelling the context under a LIVE agent must record an outcome and leave
// no agent process behind. This is the second half of what `sessions kill`
// relies on; runAgentContext (cmd/agent-utils) is the first half.
func TestSuperviseRecordsAndLeavesNoAgentOnCancel(t *testing.T) {
	// 1. stubClaude with a script that prints one stream-json line, writes its
	//    own pid to a file under t.TempDir(), then `sleep 60`.
	// 2. Start Supervise in a goroutine with a cancellable context.
	// 3. Poll until the pid file exists and holds a pid.
	// 4. Cancel.
	// 5. Assert Supervise returns within a bounded time.
	// 6. Assert the dispatch row is no longer store.StatusRunning.
	// 7. Assert the stub process is GONE: poll syscall.Kill(pid, 0) until it
	//    errors, with a 10s deadline. THIS is the assertion that proves the
	//    SIGKILL sweep reaches the agent's process group.
}
```

Write the body. **A `t.Skip` does not satisfy this step.**

- [ ] **Step 10: Run the tests**

Run: `go test ./internal/runner/ ./internal/loopcmd/ ./cmd/agent-utils/ -count=1 -p 1`
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add internal/runner/ internal/loopcmd/tick.go internal/loopcmd/tick_test.go cmd/agent-utils/
git commit -m "feat: handle SIGTERM in the runner and apply agent overrides"
```

**Acceptance criteria:**
- `Effective` is the only place that resolves an override against the
  configuration. Verify with a glob that EXCLUDES tests:
  `grep -n 'Agent\.\(Model\|Harness\|Effort\)' $(ls internal/runner/*.go | grep -v _test)`
  — hits only inside `Effective`.
- `cfg` is never mutated; a test asserts it.
- All three new tests run and pass. None is skipped or left as comments.
- `act` handles `KindStop`; no decision kind reaches the `default` branch.
- The tick test proves the stop writes nothing to GitHub.

---

## Task 7: The commands and the operator's view

**review: no** — wiring plus two renderers, gated by its own tests.

**Files:**
- Modify: `cmd/agent-utils/main.go:321-368`
- Modify: `internal/loopcmd/sessions.go`, `internal/loopcmd/status.go:115`
- Create: `cmd/agent-utils/sessions_test.go`

**Interfaces:**
- Consumes: `loopcmd.Kill`, `loopcmd.Resume`, `loopcmd.RenderResults`,
  `loopcmd.Selector` with `Validate`/`Describe`, `loopcmd.KillOptions`
  (Task 5); `store.DB.StoppedIssues`, `Store.StoppedIssues` (Task 2).
- Produces:
  ```go
  type killArgs struct {
      Selector loopcmd.Selector
      Yes      bool
      Force    bool          // kill only; resume ignores it
      Timeout  time.Duration // kill only; resume ignores it
      // Confirm asks the operator to approve a destructive --all. It is a
      // field, not a direct call, so the branch is testable without a tty --
      // the same seam registerWebhookRun uses for its own --yes prompt.
      // Nil means "not interactive".
      Confirm func(prompt string) (bool, error)
  }
  func sessionsKillRun(args killArgs) error
  func sessionsResumeRun(args killArgs) error
  // loopcmd.Session gains: Stopped bool
  ```

- [ ] **Step 1: Write the failing flag tests**

The tests here drive an extracted `*Run` function, following
`registerWebhookRun` (`cmd/agent-utils/project_test.go:221`). Create
`cmd/agent-utils/sessions_test.go`:

```go
package main

import (
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/loopcmd"
)

// The --yes guard is at this layer because it protects a human at a terminal,
// and nothing below knows there is one.
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

// An interactive session confirms instead of erroring, and a decline does
// nothing at all.
func TestSessionsKillAllConfirmsWhenInteractive(t *testing.T) {
	asked := ""
	err := sessionsKillRun(killArgs{
		Selector: loopcmd.Selector{All: true},
		Confirm: func(prompt string) (bool, error) {
			asked = prompt
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("a declined confirmation returned %v, want nil", err)
	}
	if asked == "" {
		t.Fatal("sessionsKillRun did not ask for confirmation")
	}
	if !strings.Contains(asked, "every") {
		t.Fatalf("prompt %q does not describe what --all will act on", asked)
	}
}

// Both guards must run BEFORE anything opens the database or resolves a
// project, so a mistyped command touches no state. AGENT_UTILS_HOME points at
// a directory that does not exist, so any read would fail loudly and
// differently from the errors asserted here.
func TestSessionsGuardsRunBeforeAnyRead(t *testing.T) {
	t.Setenv("AGENT_UTILS_HOME", "/nonexistent/agent-utils-home")

	if err := sessionsResumeRun(killArgs{Selector: loopcmd.Selector{}}); err == nil ||
		!strings.Contains(err.Error(), "one of --session") {
		t.Fatalf("resume err = %v, want the selector error", err)
	}
	if err := sessionsKillRun(killArgs{Selector: loopcmd.Selector{All: true}}); err == nil ||
		!strings.Contains(err.Error(), "--yes") {
		t.Fatalf("kill err = %v, want the --yes error", err)
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./cmd/agent-utils/ -count=1`
Expected: FAIL — undefined.

- [ ] **Step 3: Add the two commands**

In `sessionsCommand()` (`cmd/agent-utils/main.go:321`), add `kill` and `resume`
beside `list`. Every flag carries a `Usage:` string, as every flag in this file
does (`:340-350`):

| Flag | Type | Usage |
|------|------|-------|
| `--project` | string | `restrict to one project, by name, id or path` |
| `--loop` | string | `restrict to loops with this name` |
| `--session` | string | `the session id to act on, as printed by sessions list` |
| `--issue` | int | `the issue number to act on; needs --loop when a project has several loops` |
| `--all` | bool | `act on every matching session; requires --yes` |
| `--yes` | bool | `skip the confirmation prompt` |
| `--force` | bool | `(kill) SIGKILL the agent and the runner instead of asking them to stop` |
| `--timeout` | duration | `(kill) how long to wait for the runner to exit` (`Value: 30 * time.Second`) |

The loop selector is spelled `--loop`, matching `sessions list`. The comment at
`:329-338` explains why this surface differs from the project-scoped twin; do
not add an alias.

Each `Action` reads the flags into a `killArgs`, sets `Confirm` only when
`isInteractive()` (`cmd/agent-utils/main.go:197`), and calls the matching
`*Run`.
The two `*Run` functions apply their rules in this order:

1. `args.Selector.Validate()`. A bad selector fails before anything opens the
   database.
2. The destructive-`--all` gate, in ONE branch. `--yes` means "skip the
   prompt", exactly as it does at `project.go:574`:
   - `!Selector.All` or `Yes` → proceed.
   - `Confirm == nil` (not interactive) → error naming `--yes`. This is the
     case step 1's tests assert, because a test has no tty.
   - otherwise → `Confirm(args.Selector.Describe())`; a decline returns nil and
     prints nothing, following `project.go:429`.
3. `loopcmd.Kill` / `loopcmd.Resume`, then `fmt.Print(RenderResults(...))`.

They return an error only when EVERY target failed. A partial failure prints
its lines and exits 0: the report already names what went wrong per target.

- [ ] **Step 4: Show the stopped state in `sessions list`**

In `internal/loopcmd/sessions.go`, add `Stopped bool` to `Session`, and extend
the state switch in BOTH `RenderSessions` and `RenderAllSessions`:

```go
		state := s.LastStatus
		switch {
		case s.Live:
			state = "running"
		case s.Stopped:
			// Above ORPHANED: a stopped session's runner is gone BY DESIGN, and
			// reporting it as an orphan would send the operator looking for a
			// crash that did not happen.
			state = "STOPPED"
		case s.Orphaned:
			state = "ORPHANED"
		}
```

`sessionsFrom` takes only dispatch rows today. Give it a second argument, a set
keyed like this:

```go
// stoppedKey identifies one stopped issue.
//
// It carries the project for the reason sessionKey does: AllSessions spans
// every project on the machine, and an issue number is unique only within a
// loop of ONE project. Keyed on loop and number alone, two projects' issue 7
// merge, and one project's session is reported STOPPED because of the other's.
type stoppedKey struct {
	ProjectID string
	Loop      string
	Number    int
}
```

BOTH renderers fill the set from `DB.StoppedIssues()`. `Sessions` filters it to
`p.Config.ID`; `AllSessions` keeps all of it.

`Store.StoppedIssues(loop, repo)` is deliberately NOT used here. It needs a
repo, and `Sessions(p *Project, loopFilter string)` has none — a project holds
several loops and each names its own repo, so the renderer would have to load
every loop configuration just to label a column. `DB.StoppedIssues` already
returns the loop and the project on each row, which is exactly what the key
needs. The scoped read stays for `Resume`, which runs after `Open` and does
have `cfg.Repo`.

A session with no matching entry is not stopped.

- [ ] **Step 5: Show the reason in `loop status`**

`stopped_reason` is written to be read, and the session table has no room for a
sentence. In `internal/loopcmd/status.go`, extend the state column beside the
existing `parked` case (`:115`):

```go
		if s.Parked {
			state = "parked"
		}
		if s.Stopped {
			// Last, so it wins: a stopped issue is one an operator is waiting
			// on, and it is the state that names an action they must take.
			state = "stopped"
		}
```

And after the table, list each stopped issue with its reason:

```go
	// Collected while rendering the table above -- NOT re-read. The render
	// loop's `default: continue` skips an issue carrying none of the label
	// states, so a stopped issue could otherwise be missing from the table AND
	// from this list. Build `stopped` from the `states` map directly, before
	// the loop, so it is complete either way:
	//
	//   var stopped []store.IssueState
	//   for _, s := range states {
	//       if s.Stopped { stopped = append(stopped, s) }
	//   }
	//   sort.Slice(stopped, func(i, j int) bool { return stopped[i].Number < stopped[j].Number })
	//
	// The reason is a sentence, and no column is wide enough for one. It is
	// also the whole point of the flag: without it an operator sees "stopped"
	// and has no way to learn why.
	if len(stopped) > 0 {
		fmt.Fprintf(&b, "\nstopped issues:\n")
		for _, s := range stopped {
			fmt.Fprintf(&b, "  #%d  %s\n", s.Number, s.StoppedReason)
		}
		fmt.Fprintf(&b, "\nClear one with: agent-utils sessions resume --issue <n>\n")
	}
```

Add a test in `internal/loopcmd/status_test.go` asserting that a stopped issue
renders BOTH the `stopped` state and its reason, including one that carries no
label state at all.

Add a test in `internal/loopcmd/sessions_test.go` for the two session
renderers:

```go
// The stopped set is keyed by PROJECT, loop and number. Two projects can hold
// an issue 7 in a loop with the same name, and the machine-wide report is the
// first thing that sees both at once.
func TestRenderAllSessionsMarksOnlyTheStoppedProjectsSession(t *testing.T) {
	// Two sessions, same loop name and issue number, different ProjectID.
	// Only project-a's issue is stopped. Assert project-a's row reads STOPPED
	// and project-b's does not.
}

// A live agent outranks the flag: the operator needs to know something is
// still running.
func TestRenderSessionsPrefersRunningOverStopped(t *testing.T) {
	// One session that is both Live and Stopped renders "running".
}
```

Write both bodies with the file's existing helpers.

- [ ] **Step 6: Run the tests**

Run: `go test ./cmd/agent-utils/ ./internal/loopcmd/ -count=1 -p 1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/agent-utils/ internal/loopcmd/sessions.go internal/loopcmd/status.go internal/loopcmd/status_test.go
git commit -m "feat: add sessions kill and sessions resume commands"
```

**Acceptance criteria:**
- Every new flag has a `Usage:` string.
- `--all` without `--yes` fails and names `--yes`; interactively it confirms.
- The stopped set is keyed by project, loop, and number, proven by a test that
  uses two projects.
- `loop status` shows both the `stopped` state and the reason, with a test,
  including an issue carrying no label state.
- `running` still wins over `STOPPED` in the session table, proven by a test.
- A declined confirmation does nothing and returns nil, proven by a test.

---

## Task 8: Documentation

**review: no** — gated by `TestEveryConfigFieldIsDocumented` and by reading.

**Files:**
- Modify: `docs/configuration.md`, `README.md`

- [ ] **Step 1: Document the override labels**

In `docs/configuration.md`, add `## Agent overrides from labels` after the
`## labels` section (`:366`). It must state:

- The three prefixes, with an example of each.
- That they are always active and need no configuration.
- That the prefix comparison ignores case; that `harness` and `effort` values
  are lowered because they are closed lists; that a `model` value keeps its case.
- Every rejection rule from spec section 6.3, and what happens when one fires:
  the loop does not dispatch, it stops the issue, and the reason appears in
  `agent-utils project loop status`.
- **That a `harness:` label is refused when the loop sets `agent.permission_mode`
  or `agent.max_budget_usd`**, and why: the `pi` harness supports neither, so
  the override would silently drop a safety setting.
- That overrides DO apply to retries and do NOT apply to a tend dispatch.
- That anyone who can label an issue can choose the model and the harness.
- That an invalid label stops the issue, and that clearing it needs
  `sessions resume` on the machine that runs the loop — so a label applied from
  GitHub can halt an issue that only a local operator can restart.

Cross-reference `agent.harness` (`:470`), `agent.model` (`:490`) and
`agent.effort` (`:502`) from the new section, and add a pointer back from each.

- [ ] **Step 2: Document the commands**

In `README.md`, extend `## Sessions` (`:155`) with `sessions kill` and
`sessions resume`: the selectors, `--force`, `--timeout`, `--yes`, the
`STOPPED` state, and the `stopped` state in `loop status`. State plainly that a
kill HOLDS the issue until a resume, and why — without the hold, the next tick
would dispatch it again.

State the three limits honestly:

- `--force` will not signal an agent whose runner cannot be verified, so an
  agent orphaned by a runner something else killed must be stopped by hand.
- A resume is refused while the runner is still alive.
- A kill whose runner outlives `--timeout` leaves the issue stopped and safe,
  but the agent is still running until `--force`.

Add the override labels to `## Configuration` (`:244`) with a pointer to the
reference, and record the exposure in `## Security` (`:289`).

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
- The `harness:` safety-setting refusal is documented with its reason.
- The tend limit, the retry inclusion, and all three operational limits are
  stated.
- The security exposure and the GitHub-halts/local-restarts asymmetry are
  stated.
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
