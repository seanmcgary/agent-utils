# Session kill and label overrides — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator stop a running session from the command line, and let a label on an issue override the agent model, harness, or effort for that issue.

**Architecture:** A new per-issue `stopped` flag is the one mechanism both features use. `engine.Decide` refuses to dispatch a stopped issue. `sessions kill` sets the flag and then signals the runner; `sessions resume` clears it. `config.ParseOverrides` reads three label prefixes; an invalid label makes `Decide` stop the issue instead of dispatching it. A valid override rides on the dispatch row into the detached runner, where one `runner.Effective` helper feeds the argument builders.

**Tech Stack:** Go, `urfave/cli/v3` (v3.11.0), `modernc.org/sqlite`, `gopkg.in/yaml.v3`.

**Spec:** `docs/superpowers/specs/2026-08-26-session-kill-and-label-overrides-design.md` — read it; this plan argues from it and does not restate it.

**Class:** Large. **Profile:** backend.

**Execution:** one fresh subagent per task, TDD within it — write the listed tests, watch them fail, implement, watch them pass, run the task's gate, commit. Tasks are deliberately broad so there are four passes, not eight.

**Review:** NO per-task reviewers. Every task is gated by its own tests and acceptance criteria, which is what a specified-enough plan buys. There is exactly ONE authoritative review: the stage-4 whole-diff fan-out over the finished branch. Do not add a review between tasks — that is the ratchet this cadence exists to prevent, where each round's fixes hand the next round a changed diff to find new findings in.

## Global Constraints

This repository has no root conventions document. The rules below were read out
of the code; each names where it is enforced.

- **`make check` is the final gate**: `fmtcheck`, `vet` (twice — the second with
  `GOOS=darwin`), `lint` (golangci-lint v2.5.0), `test`. Per-task gates run the
  touched packages with `-count=1 -p 1`.
- **`errorlint` is on** (`.golangci.yml`). Compare wrapped errors with
  `errors.Is`.
- **`TestEveryConfigFieldIsDocumented`** (`internal/config/docs_test.go:14`)
  fails the build on an undocumented `Config` yaml field. This plan adds none.
- **`parkRetryExhausted` is the ONE GitHub write this program performs**
  (`internal/loopcmd/tick.go:493`). No task may add another.
- **Every `Store` read and write is project-scoped** (`store.go:180`); `DB`
  methods are the machine-wide reads; `db.Project(id)` bridges them (`:225`).
- **A new column goes through `addedColumns`** (`store.go:314`), never by
  editing `CREATE TABLE` alone.
- **Never add a `Co-Authored-By:` trailer.**
- **Comment style:** long comments that explain *why* and name the failure the
  code prevents (`store.go:632-655`, `engine.go:42-47`). Match it. The comments
  quoted in this plan are load-bearing — they carry review findings that are not
  otherwise recoverable from the code.

## Verified external API (do not re-derive)

Read out of the source on 2026-08-26; line numbers verified.

```go
func (d *DB) Project(projectID string) *Store                    // store.go:225
func (d *DB) RunningDispatches() ([]Dispatch, error)             // store.go:1038 — machine-wide
func (s *Store) RunningDispatches(loop, repo string) ([]Dispatch, error) // store.go:867
func (s *Store) CreateDispatch(d Dispatch) (int64, error)        // store.go:776
func (s *Store) FinishDispatch(id int64, r DispatchResult) error // store.go:802
func (s *Store) IssueStates(loop, repo string) (map[int]IssueState, error) // store.go:435
func (s *Store) PutIssueState(st IssueState) error               // store.go:480
const dispatchColumns = `id, project_id, ...`                    // store.go:830
func scanDispatch(sc interface{ Scan(...any) error }) (Dispatch, error) // store.go:834 — same order

func IsAlive(pid int, dispatchID int64) bool                     // proc.go:34
func CommandLine(pid int) (string, error)                        // proc.go:17
func matchesDispatch(cmdline string, dispatchID int64) bool      // proc.go:55 — unexported, same package
const DispatchFlag = "--dispatch"                                // proc.go:13
func (d Dispatch) RunnerID() int64                               // types.go:99

func Decide(cfg *config.Config, snap Snapshot, st State, now time.Time) Plan   // engine.go:17
func retryDecision(cfg *config.Config, number int, state store.IssueState, now time.Time) (*Decision, bool, string) // engine.go:199

func BuildArgs(cfg *config.Config, inv Invocation) []string       // args.go:26
func PiBuildArgs(cfg *config.Config, inv Invocation) []string     // args.go:64
func Open(ref ProjectRef, configPath string, opts Options) (*config.Config, Deps, func(), error) // open.go:90
type ProjectRef struct{ ID, Name, Dir string }                    // open.go:36 — Dir is load-bearing
func Resolve(agentUtilsDir, name string) (string, error)          // discover.go:189
```

**Facts that drove the design.** Each was confirmed by reading, and each exists
because a review round found the plan wrong without it.

- The runner is spawned `Setsid` (`runner.go:46`); the agent child `Setpgid`
  (`:154`). **Different process groups** — a signal to the runner's group does
  not reach the agent.
- `cmd.Cancel` already SIGTERMs the agent's group (`:157`); a SIGKILL sweep
  already follows `Wait` (`:215`).
- `main` uses `context.Background()` (`main.go:49`). **No signal handler exists
  in the runner path.**
- **`IsAlive` fails SAFE by reporting ALIVE when `ps` errors** (`proc.go:42`).
  Right for liveness, inverted for signalling.
- **`tendDecisions` skips only issues marked `decided`** (`engine.go:259`).
- **`retryDecision` receives no labels** (`engine.go:199`).
- **The breaker drops every entry of `decisions` and rewrites their skip
  reasons, keeping only `parks`** (`engine.go:159-168`).
- **`agent.model` is required for EVERY harness** (`config.go:261`) — a "pi
  needs a model" rule could never fire.
- **`config.validate` forbids `agent.permission_mode` with `harness: pi`**
  (`config.go:218`), and **`PiBuildArgs` emits neither a permission mode nor a
  cost ceiling** (`args.go:60`).
- Helpers that EXIST: `openTemp(t) *Store`, `openTempAt(t, path)`
  (`store_test.go:15`, `:20`); `testConfig()` (`engine_test.go:12`);
  `stubClaude`, `stubPi` (`runner_test.go:18`, `:222`); `joined(args)`
  (`args_test.go:21`); `fakeGH` (`tick_test.go`); `Confirm`, `isInteractive`
  (`project.go:519`, `main.go:197`). There is **no `newTestStore`** and no
  extracted old-schema helper — that fixture is inline at `store_test.go:199`.

---

## Task 1: Foundations — parser, columns, signal helpers

**review: no** — three leaf additions, each gated by its own table tests.

These three share a task because none depends on the others, all are pure
additions to leaf packages, and splitting them buys three commit boundaries and
nothing else.

**Files:** create `internal/config/overrides.go`, `internal/proc/signal.go`;
modify `internal/store/{types.go,store.go}`; tests beside each.

**Produces** (Tasks 2–4 consume these names exactly):

```go
// internal/config
type Overrides struct{ Model, Harness, Effort string }
func ParseOverrides(labels []string) (Overrides, error)
const OverrideModelPrefix, OverrideHarnessPrefix, OverrideEffortPrefix = "model:", "harness:", "effort:"

// internal/proc
var ErrNotRunner = errors.New("not this dispatch's runner")
func VerifyRunner(pid int, dispatchID int64) error
func Signal(pid int, dispatchID int64, sig syscall.Signal) error
func SignalGroup(pid int, sig syscall.Signal) error

// internal/store
// IssueState gains: Stopped bool; StoppedReason string
// Dispatch gains:   AgentPID int; Model, Harness, Effort string
type StoppedIssue struct{ ProjectID, Loop, Repo string; Number int; Reason string }
func (s *Store) MarkStopped(loop, repo string, number int, reason string, now time.Time) error
func (s *Store) ClearStopped(loop, repo string, number int, now time.Time) error
func (s *Store) StoppedIssues(loop, repo string) ([]IssueState, error)
func (s *Store) SetDispatchAgentPID(id int64, pid int) error
func (d *DB) StoppedIssues() ([]StoppedIssue, error)
```

### 1a. The override parser

- [ ] **Write `internal/config/overrides_test.go`.** A table over
  `ParseOverrides` covering: no override labels; all three at once;
  `Model:Claude-Opus-5` (prefix folds case, model value does not);
  `harness:PI` / `effort:HIGH` (enum values ARE lowered); and one case per
  rejection rule — empty value, whitespace, leading `-`, `model:claude​opus`
  (zero-width space), duplicate prefix, `harness:gpt`, `effort:bogus`. Assert on
  every error path that the returned `Overrides` is the zero value: a caller
  that ignored the error must not get half an override.

  Three assertions carry findings and must be written as their own tests:

```go
// The security rule. A rejected value must never reach an argument list.
func TestParseOverridesRejectsEveryFlagShapedValue(t *testing.T) {
	for _, v := range []string{"-p", "--model", "-", "--", "-x"} {
		if _, err := ParseOverrides([]string{"model:" + v}); err == nil {
			t.Fatalf("ParseOverrides accepted the flag-shaped value %q", v)
		}
	}
}

// The reason text is persisted as stopped_reason, logged, and printed to a
// terminal. A label carrying a newline or an escape must not travel raw.
func TestParseOverridesQuotesTheLabelInEveryError(t *testing.T) {
	for _, labels := range [][]string{
		{"model:a\nb"},
		{"model:a", "model:b\nc"}, // the duplicate error interpolates BOTH labels
	} {
		err := mustErr(t, labels)
		if strings.Contains(err.Error(), "\n") {
			t.Fatalf("error %q carries a raw newline; quote the label with %%q", err)
		}
	}
}
```

- [ ] **Implement `internal/config/overrides.go`.** Key decisions, each of which
  must appear as a comment:

  - **The value rule is an ALLOWLIST**, `^[A-Za-z0-9._][A-Za-z0-9._/-]*$`, not a
    denylist. The value becomes one element of the argv `exec` receives
    (`args.go:26`); Go passes a list, not a shell string, so quoting is not the
    hazard — a leading `-` is, because the agent reads it as a FLAG
    (`model:--dangerously-skip-permissions`). `ghub.SafeRef`
    (`ghub/types.go:141`) rejects a leading dash for the same reason. An
    allowlist additionally excludes U+200B and U+2060, which `unicode.IsSpace`
    does not match.
  - **`effort` is checked against the same closed list `config.validate`
    enforces** — `low, medium, high, xhigh, max` (`config.go:263`). A rule the
    configuration closes must not be reopened by a label.
  - **`harness` and `effort` values are lowered; `model` is not.** The first two
    are enums, and every other label comparison in this program folds case
    (`ghub.HasLabel` uses `EqualFold`). A model identifier is case-sensitive.
  - **Validate each value BEFORE reporting a duplicate.** The duplicate error
    interpolates both labels, and the second one is unvalidated at that point.
  - A duplicate prefix is an error, not a last-one-wins: two people adding
    `model:` labels must not get an answer that depends on GitHub's ordering.

- [ ] **Gate:** `go test ./internal/config/ -count=1`.

### 1b. The store columns

- [ ] **Write the tests** in `internal/store/store_test.go`. **The helper is
  `openTemp(t)`, returning a `*Store`** — there is no `newTestStore`. Add
  `"strings"` to the imports; the file has only `database/sql`, `path/filepath`,
  `testing`, `time`.

  - `MarkStopped` then `IssueState` round-trips the flag and reason;
    `ClearStopped` clears both.
  - `ClearStopped` **also clears `needs_retry` and `retry_after`, and does NOT
    clear `parked`.** The killed runner records a FAILED dispatch and `finish`
    marks the issue for retry (`runner.go:320`), so a resumed issue would
    otherwise carry a failure it did not earn.
  - **Three writes must leave `stopped` alone**: `BeginDispatch`,
    `MarkSucceeded`, and `PutIssueState`. The third is the subtle one and needs
    the park path's exact shape — begin a dispatch, read the state, mark
    stopped, write the STALE state back, assert still stopped. That fails if
    `stopped` is added to `PutIssueState`'s conflict set.
  - `Store.StoppedIssues` is scoped; `DB.StoppedIssues` spans projects. Prove it
    with **two projects each holding an issue 7 in a loop with the same name** —
    a read that forgot the project would merge them.
  - `CreateDispatch` round-trips `Model`/`Harness`/`Effort`;
    `SetDispatchAgentPID` round-trips `AgentPID`.
  - Migration: extract the inline old-schema fixture at `store_test.go:199` into
    `writeOldSchema(t, path)`, have the existing test call it too, then assert
    all six columns exist after `Open`. Separately assert `rebuilt`'s `issues`
    entry names `stopped` and `stopped_reason` — that list is written by hand and
    is the one place a new column is easy to forget; a rebuild that dropped them
    would silently un-stop every stopped issue.

- [ ] **Implement.** Add the fields with their doc comments (`Stopped` explains
  why it is not `parked`; `AgentPID` explains that it is never cleared and so is
  stale on any row whose runner died). Then:

  1. Add the columns to both `CREATE TABLE`s **and** to `addedColumns`
     (`store.go:314`), in the order listed in the spec.
  2. Add `stopped, stopped_reason` to `rebuilt`'s `issues` entry (`:352`).
  3. Append `agent_pid, model, harness, effort` to `dispatchColumns` (`:830`)
     and the matching scan targets to `scanDispatch` — **same order, both
     lists**.
  4. Extend `CreateDispatch` to insert the three override columns. `agent_pid`
     is written later by `SetDispatchAgentPID`.
  5. Extend `IssueStates`' SELECT and scan. **Do NOT touch `PutIssueState`.**
  6. `MarkStopped` is an UPSERT (an invalid label is found on the FIRST tick,
     before any row exists) with `ON CONFLICT(project_id, loop, repo, number)`.
     It is a targeted write, not a read-modify-write, for the reason
     `BeginDispatch` gives at `store.go:632`.

- [ ] **Gate:** `go test ./internal/store/ -count=1 && go build ./...`.

### 1c. The signal helpers

- [ ] **Write `internal/proc/signal_test.go`.** The fixture is the standard Go
  helper-process idiom — **not `sh -c`, and not `sleep` with trailing operands**
  (`sleep 30 --dispatch 7` is a usage error and exits immediately, so the
  assertion would race a corpse):

```go
// Not a real test: the fixture. The test binary re-executes itself, so the
// child is a Go process whose argv this package controls exactly. It returns
// at once unless the parent set the marker, so a normal run skips it.
func TestSignalHelperProcess(t *testing.T) {
	if os.Getenv("AGENT_UTILS_SIGNAL_HELPER") != "1" {
		return
	}
	time.Sleep(60 * time.Second)
}

func startFakeRunner(t *testing.T, dispatchID string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestSignalHelperProcess$", "--", DispatchFlag, dispatchID)
	cmd.Env = append(os.Environ(), "AGENT_UTILS_SIGNAL_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake runner: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	waitVisible(t, cmd.Process.Pid) // poll CommandLine until ps sees it
	return cmd
}
```

  Cases: `Signal` and `SignalGroup` both refuse 0, -1, -1000; **`SignalGroup`
  refuses 1** (negated it is `kill(-1)`, every process the user owns — a
  positive-only check is not enough for a group signal); `Signal` on a live
  non-runner returns `ErrNotRunner` **and leaves it alive**; `Signal` on the
  fake runner kills it; `VerifyRunner(pid, 7)` passes but `VerifyRunner(pid, 70)`
  returns `ErrNotRunner` (whole-token match). Do not add a reaper goroutine —
  `t.Cleanup` owns the one `Wait`, and a second concurrent `Wait` on one
  `os.Process` is a race.

- [ ] **Implement `internal/proc/signal.go`.** `VerifyRunner` exists rather than
  reusing `IsAlive` because **the two want opposite biases**: `IsAlive` fails
  safe by reporting alive when `ps` fails (`proc.go:42`), which is right for
  liveness and inverted for signalling — a `ps` that failed means the process
  was never confirmed to be ours, so refuse. It reuses `matchesDispatch`
  (same package, `proc.go:55`). `SignalGroup` takes a POSITIVE pid and negates
  it internally, so no call site is one typo from -1, and it carries no runner
  check because the agent has no `--dispatch` argument — the CALLER must
  establish the pid is current (Task 3 does).

- [ ] **Gate:** `go test ./internal/proc/ -count=1`.

- [ ] **Commit** the three units separately:
  `feat: parse agent overrides from issue labels`,
  `feat: store the stopped flag, agent pid and dispatch overrides`,
  `feat: add guarded signal helpers to internal/proc`.

**Acceptance criteria:**
- Every rejection rule in spec §6.3 has a test; no error can carry a raw newline.
- Outside `internal/config`, no Go file contains `"model:"`, `"harness:"`, or
  `"effort:"`.
- `dispatchColumns` and `scanDispatch` agree in order; `rebuilt` names both new
  `issues` columns; `PutIssueState` writes neither.
- `SignalGroup` refuses pid 1; `VerifyRunner` fails closed on a `ps` error.

---

## Task 2: The engine

**review: no** — small, and every branch below has a test that names the failure
it prevents.

**Files:** `internal/engine/{types.go,engine.go,engine_test.go}`.

**Consumes:** `config.ParseOverrides`, `config.Overrides`,
`config.OverrideHarnessPrefix`, `config.HarnessClaude`, `config.HarnessPi`,
`store.IssueState.Stopped` (Task 1).
**Produces:** `engine.KindStop`, `engine.Decision.Overrides config.Overrides`.

- [ ] **Write the tests** in `engine_test.go`, using `testConfig()` (`:12`).
  Each of these encodes a defect a review round found:

  - A stopped issue produces no decision, and `NoDecisionReason` carries its
    reason.
  - **A stopped issue awaiting review, with a behind PR, produces NO
    `KindTend`.** `tendDecisions` skips only `decided` issues (`engine.go:259`),
    so without setting it a tend agent force-pushes the branch of the session
    the operator just killed.
  - A stopped issue that ALSO needs retry produces nothing — the stop check must
    sit above the retry path, because a killed dispatch always records a failure.
  - An override reaches a `KindStart` decision **and a retry decision**.
    `retryDecision` gets no labels, so this is the test that catches every retry
    silently reverting to the configured model.
  - `harness:gpt` on a triggered issue produces one `KindStop` carrying the
    parse error.
  - **`harness:pi` produces `KindStop` when the loop sets `permission_mode`, and
    again when it sets `max_budget_usd`** — and is ALLOWED when it sets neither.
  - An invalid label on an issue with no trigger label produces nothing.
  - An invalid label **does not block `KindClearRetry`** (the only thing that
    retires an unreachable retry flag — blocking it strands the issue forever)
    **or `KindParkRetryExhausted`** (the cap is a fact about the issue, not its
    labels).
  - **A `KindStop` survives a tripped circuit breaker with its reason intact**,
    and **a retry converted to a stop does not count toward the breaker
    threshold** — otherwise a label can trip the breaker and drop every other
    loop's dispatches for the cooldown.

- [ ] **Implement.** In `types.go` add `KindStop` and `Decision.Overrides`,
  importing `internal/config` (verified: no cycle — `config` imports only
  `internal/home`). In `engine.go`, inside the issue loop:

  1. **After the `liveIssues` block (`:65`), before `state :=` (`:67`)** — the
     stopped skip. It sets `decided[iss.Number] = true` for the reason the
     live-dispatch branch does, then records the reason plus
     `"; clear it with `+"`agent-utils sessions resume`"+`"`.
  2. **Before the `if state.NeedsRetry` branch (`:72`)** — parse once:

```go
		// Parse ONCE, here, above the retry path. retryDecision receives no
		// labels, so a parse below it could never reach a retry decision and
		// every retry would silently fall back to the configured model.
		//
		// The result is not ACTED on here. An invalid label must stop only a
		// DISPATCH; it must never block KindClearRetry or
		// KindParkRetryExhausted, which are repair actions.
		ov, ovErr := config.ParseOverrides(iss.Labels)
		if ovErr == nil {
			ovErr = validateOverrides(cfg, ov)
		}
```

  3. Declare `var stops []Decision` beside `var parks []Decision`, with a
     comment: they are kept OUT of `decisions` because **the breaker branch
     (`:159`) drops every entry of `decisions` and rewrites its skip reason** —
     a stop is the refusal to dispatch, not a dispatch, so it must survive.
  4. In the retry branch, convert to a stop **before** `eligibleRetries++`, so a
     label cannot push the breaker over its threshold; otherwise set
     `d.Overrides = ov`. Both only when `d.Kind != KindParkRetryExhausted`.
  5. After the trigger check (`:113-119`), an `ovErr` becomes a stop; otherwise
     add `Overrides: ov` to the `KindStart` and `KindResume` decisions.
  6. Carry `stops` through **both** exits: `Decisions: append(stops, parks...)`
     in the breaker branch, and `decisions = append(decisions, stops...)` beside
     the existing `parks` append.

- [ ] **Add `validateOverrides`.** It refuses a `harness:` override that changes
  the harness when the loop sets `agent.permission_mode` or a non-zero
  `agent.max_budget_usd`, because **`PiBuildArgs` emits neither** (`args.go:60`)
  and `config.validate` forbids a permission mode with pi (`config.go:218`) — so
  the label would silently drop both bounds that exist because the agent reads
  third-party issue text. Treat an empty configured harness as `claude`
  (`config.Load` normalises it at `config.go:165`, but `Decide` must not depend
  on having been handed a normalised config). **There is deliberately no
  "pi requires a model" rule**: `agent.model` is required for every harness
  (`config.go:261`), so it could never fire.

- [ ] **Gate:** `go test ./internal/engine/ -count=1` — every pre-existing test
  must pass unchanged.

- [ ] **Commit:** `feat: skip stopped issues and carry label overrides in Decide`.

**Acceptance criteria:**
- The stopped branch sets `decided`, proven by the tend test.
- Overrides reach retry decisions, proven by a test.
- A `KindStop` survives the breaker; a stopped retry does not feed it.
- `KindClearRetry` and `KindParkRetryExhausted` still fire with a bad label.
- `validateOverrides` has a reachable rule and a test per branch.

---

## Task 3: Kill, resume, and the signal path

**review: no** — the largest task, and the one whose tests are most load-bearing.
Every guard below has a test that names the failure it prevents; the whole-diff
fan-out at the end is what reviews it.

**Files:** create `internal/loopcmd/kill.go` + test, `cmd/agent-utils/runagent.go`
+ test; modify `internal/runner/{args.go,runner.go}`,
`internal/loopcmd/tick.go`, `cmd/agent-utils/main.go`.

**Consumes:** everything Tasks 1–2 produce.
**Produces:**

```go
// internal/loopcmd
type Selector struct{ Session string; Issue int; All bool; Project, Loop string }
func (s Selector) Validate() error   // EXPORTED — package main calls it
func (s Selector) Describe() string  // the confirmation prompt

// Target is IDENTITY ONLY. It carries no store.Dispatch: a loopcmd.Session has
// no repo, no pid and no dispatch id, so resolve cannot build one. Kill opens
// each loop anyway to take its lock, and THAT is where cfg.Repo and a scoped
// Store exist -- so dispatch rows are bound there.
type Target struct{ ProjectID, Project, Dir, Loop string; Issue int; Session, ConfigPath string }
type work struct{ Target Target; Repo string; Dispatch store.Dispatch }

type Action string // signalled | already gone | forced | still alive | resumed | refused
type Result struct{ Target Target; Action Action; Err error }
type KillOptions struct{ Selector Selector; Force bool; Timeout time.Duration }
func Kill(opts KillOptions) ([]Result, error)
func Resume(sel Selector) ([]Result, error)
func RenderResults(verb string, rs []Result) string

// internal/runner
type Settings struct{ Harness, Model, Effort string }
func Effective(cfg *config.Config, ov config.Overrides) Settings
// Invocation gains: Overrides config.Overrides

// internal/loopcmd — Summary gains: Stopped int `json:"stopped"`
// cmd/agent-utils
func runAgentContext(ctx context.Context) (context.Context, context.CancelFunc)
```

### 3a. `runner.Effective` and the agent pid

- [ ] **Tests** in `args_test.go`, using the existing `joined` helper (`:21`):
  an override replaces the configured value; an unset one keeps it;
  **`cfg` is not mutated**; a flag-shaped value read back off a row is DROPPED;
  both `BuildArgs` and `PiBuildArgs` emit the override.

- [ ] **Implement.** `Effective` returns a value and **never mutates `cfg`** —
  writing into `cfg.Agent` would leave every later reader holding a config that
  no longer matches its file, including the retry policy and log paths. It
  re-validates each row value through `ParseOverrides` as defence in depth: this
  process did not parse them, the tick did, possibly under an older binary, and
  `store/legacy.go:253` writes the dispatches table by a second path. Rewrite
  both builders to open with `s := Effective(cfg, inv.Overrides)`, and replace
  both `cfg.Agent.Harness == config.HarnessPi` comparisons in `Supervise`
  (`:143`, `:230`) — the first also selects `extraEnv`, so the override
  correctly keeps `claudeEnv` out of a pi child.

- [ ] **Record the agent pid** after a successful `cmd.Start()` (`:196`) via
  `SetDispatchAgentPID`, logged-and-ignored on failure (the agent is already
  running; abandoning the run over a bookkeeping write is worse). **Add the
  `log/slog` import — `internal/runner/runner.go` has none today.**

### 3b. The signal handler

- [ ] **`cmd/agent-utils/runagent.go`**: `runAgentContext` wrapping
  `signal.NotifyContext(ctx, SIGINT, SIGTERM)`. It is a named function rather
  than two lines in the action closure **so the wiring can be tested** — a
  handler that is never installed fails silently and stays invisible until an
  operator's kill orphans an agent. Call it in the `run-agent` action (`:698`).

- [ ] **Test** that a SIGTERM to the test's own process cancels the returned
  context within 5s. (`NotifyContext` installs a handler, so the default
  terminate action does not run — that is the behaviour under test.)

- [ ] **Test in `runner_test.go`** that cancelling under a LIVE agent records an
  outcome and leaves no agent behind: `stubClaude` prints one stream-json line,
  writes its own pid to a file, then sleeps 60; start `Supervise` in a goroutine;
  poll for the pid file; cancel; assert `Supervise` returns, the row is no longer
  `running`, and **the stub process is gone** (poll `syscall.Kill(pid, 0)` to a
  10s deadline). That last assertion is the one proving the SIGKILL sweep reaches
  the agent's process group. **Not skipped, not a comment.**

### 3c. Kill and resume

- [ ] **Tests.** `killer` is a struct of function fields (`markStopped`,
  `verify`, `signal`, `waitGone`, `reread`, `finish`, `killAgent`, `killRunner`)
  so the ordered procedure is testable without real processes. Each test below
  is a review finding:

  - **The flag is written BEFORE the signal.** A tick running in the window
    between the agent dying and the flag landing would see the trigger label and
    no live dispatch, and start a new agent.
  - **A failed flag write means no signal at all.**
  - An unverifiable runner is `already gone`: record the outcome, do not signal.
  - **`--force` kills the agent group, then the runner, then records** — the
    reverse order leaves the agent alive in a worktree the loop thinks is free.
  - **`--force` does NOT group-kill when the runner is unverified.** `agent_pid`
    is never cleared, so a dead-runner row carries a stale number, and after a
    reboot that number leads an unrelated process group.
  - A real signal failure (EPERM) is reported as a failure, not as success.
  - A runner outliving the timeout yields `still alive` and the report names
    `--force`.
  - **No double-recording**: after the wait, re-read the row; if the runner's own
    handler already recorded an outcome, do not write a second one.
  - A **tend** dispatch writes no issue flag — it holds no issue state
    (`runner.go:311`).
  - `narrowByLoop` rejects an ambiguous `--issue` across two loops and names
    both; accepts it narrowed by `--loop`; accepts a single candidate.
  - **A held loop lock fails every target of THAT loop** with the `loop reset`
    wording, and does not abandon the other loops.
  - **`Resume` refuses a live runner** (naming `--force`) and clears a dead one.
    The runner holds no lock and its `finish` calls `MarkNeedsRetry`
    (`runner.go:321`), so a resume issued mid-death has its clear written
    straight back.
  - `RenderResults` names every target and outcome, names `--force` on
    `still alive`, and says "nothing matched" when empty. `Selector.Validate`
    covers the seven selector combinations; `Selector.Describe` names what
    `--all` will act on.

- [ ] **Implement `kill.go`**, in this order, each rule commented:

  1. `Selector` with **exported** `Validate()` and `Describe()`.
  2. `Target`, `work`, `KillOptions`, `Action`, `Result`, `narrowByLoop`.
  3. `resolve(sel, forResume)` — identity only. `--session` matches
     `AllSessions` on `Session.ID` and sets `Target.Session`; `--issue` builds a
     candidate per loop then `narrowByLoop`; `--all` reads
     `DB.RunningDispatches()` or `DB.StoppedIssues()` and maps each `ProjectID`
     back through the registry. **Fill `Dir` from the registry entry's
     `AgentUtilsDir`** — it becomes `ProjectRef.Dir`, which drives
     `ResolveWorkDirs` and `migrate.Discover` (`open.go:114`, `:179`); empty, it
     resolves different worktree paths and makes `FailOnUnimported` a hard error.
     A target whose config cannot be resolved becomes a failed `Result`, never a
     fatal error.
  4. `killer.one(w work, opts KillOptions)` — spec §4.2 exactly: tend skips the
     flag; `markStopped` first; `verify`; then either the force path or
     SIGTERM-and-wait with the re-read.
  5. `Kill` — validate, resolve, group by loop; per loop `Open` with the full
     `ProjectRef`, take `lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))`,
     **bind dispatch rows here** via `Store.RunningDispatches(cfg.Name, cfg.Repo)`
     matched on `Target.Session` or `Target.Issue`, run `one`, release.
  6. `Resume` — same shape, calling `ClearStopped`, with the live-runner refusal.
  7. `RenderResults`.

  The recorded outcome is `{StatusFailed, ExitCode: -1, APIError: "killed by
  operator"}` written with **`store.FinishDispatch`, not `runner.Finish`** —
  comment why: `runner.Finish` also calls `MarkNeedsRetry`, and `tick.go:604`
  warns against skipping that, but here arming a retry is exactly wrong because
  the issue is stopped.

### 3d. Tick plumbing

- [ ] Persist `d.Overrides` into the `store.Dispatch` literal (`tick.go:431`) —
  a tend decision carries none, which is spec §6.7 with no extra branch. Read
  them back into the `Invocation` in `RunAgent` (`:615`), **from the row**,
  because the detached runner never sees the tick's snapshot.
- [ ] Add `Summary.Stopped` and a `case engine.KindStop` to `act` (`:359`) —
  `act` returns `unknown decision kind` for anything unhandled (`:382`). It
  calls `MarkStopped`; the write is LOCAL only.
- [ ] **Test in `tick_test.go`** (using `fakeGH`): a triggered issue carrying
  `harness:gpt` yields `Started == 0`, `Stopped == 1`, a stopped issue state
  naming the harness, **no `EditLabels` and no comment** (this is what proves the
  one-GitHub-write invariant survives), and a second tick that changes nothing.
  **Not a comment body.**

- [ ] **Gate:** `go test ./internal/runner/ ./internal/loopcmd/ ./cmd/agent-utils/ -count=1 -p 1`.

- [ ] **Commit** in two: `feat: handle SIGTERM in the runner and apply agent overrides`,
  then `feat: add the session kill and resume actions`.

**Acceptance criteria:**
- `Effective` is the only place resolving an override against the config:
  `grep -n 'Agent\.\(Model\|Harness\|Effort\)' $(ls internal/runner/*.go | grep -v _test)`
  hits only inside it.
- All three signal/cancel tests run and pass; none is skipped.
- `Target` carries `Dir`, and it reaches `ProjectRef.Dir`.
- Every `Action` constant is reachable.
- The tick test proves the stop writes nothing to GitHub.

---

## Task 4: Commands, the operator's view, and docs

**review: no** — wiring plus renderers, gated by its own tests and by `make check`.

**Files:** `cmd/agent-utils/main.go` + `sessions_test.go`;
`internal/loopcmd/{sessions.go,status.go}` + tests; `docs/configuration.md`;
`README.md`.

- [ ] **Add `sessions kill` and `sessions resume`** beside `sessions list`
  (`main.go:321`). Flags — each with a `Usage:` string, as every flag in this
  file has (`:340-350`): `--project`, `--loop`, `--session`, `--issue`, `--all`,
  `--yes`, plus `--force` and `--timeout` (default 30s) on `kill`. The loop
  selector is `--loop`, matching `sessions list`; the comment at `:329-338`
  explains why this surface differs from the project-scoped twin — do not add an
  alias.

  Each action fills a `killArgs{Selector, Yes, Force, Timeout, Confirm}` and
  calls `sessionsKillRun` / `sessionsResumeRun`. `Confirm` is a **function
  field, not a direct call**, so the branch is testable without a tty — the seam
  `registerWebhookRun` uses (`project.go:519`). Set it only when
  `isInteractive()` (`main.go:197`). The `*Run` functions apply, in order:

  1. `Selector.Validate()` — a bad selector fails before anything opens the
     database, so a mistyped command touches no state.
  2. The destructive-`--all` gate, as **one** branch: not `--all` or `--yes` →
     proceed; `Confirm == nil` → error naming `--yes`; else
     `Confirm(Selector.Describe())`, a decline returning nil silently
     (`project.go:429`).
  3. `Kill`/`Resume`, then print `RenderResults`. Return an error only when
     EVERY target failed — a partial failure prints its lines and exits 0,
     because the report already names what went wrong per target.

- [ ] **Tests** (`sessions_test.go`), driving the `*Run` functions as
  `project_test.go:221` does: `--all` without `--yes` and without a tty errors
  naming `--yes`; a bad selector errors; **an interactive decline returns nil and
  acts on nothing**; and both guards fire with `AGENT_UTILS_HOME` pointed at a
  nonexistent directory, proving they run before any read.

- [ ] **`sessions list`**: add `Session.Stopped` and render `STOPPED` above
  `ORPHANED` in both renderers — a stopped session's runner is gone BY DESIGN,
  and calling it an orphan sends the operator hunting a crash that did not
  happen. `running` still wins. Key the lookup on
  `{ProjectID, Loop, Number}` — **loop and number alone merge two projects'
  issue 7**, the reason `sessionKey` carries the project
  (`sessions.go:231`). **Both** renderers read `DB.StoppedIssues()`; `Sessions`
  filters to `p.Config.ID`. The scoped `Store.StoppedIssues` is not used here —
  it needs a repo, and `Sessions(p, loopFilter)` has none.

- [ ] **`loop status`**: render `stopped` in the state column beside `parked`
  (`status.go:115`), winning over it, and list each stopped issue with its
  reason under the table plus the `sessions resume` hint. Build the list from the
  `states` map directly, **not** from the render loop — that loop's
  `default: continue` skips an issue carrying no label state, which would drop a
  stopped issue from both the table and the list. The reason is a sentence and no
  column fits one; without it an operator sees `stopped` and cannot learn why.

- [ ] **Tests**: `status` renders both the state and the reason, including for an
  issue with no label state; the session renderers mark only the right project's
  row and prefer `running` over `STOPPED`.

- [ ] **Docs.** `docs/configuration.md` gains `## Agent overrides from labels`
  after `## labels` (`:366`), cross-referenced from `agent.harness` (`:470`),
  `agent.model` (`:490`), `agent.effort` (`:502`). It must state: the three
  prefixes with examples; that they are always active; the case rules; every
  rejection rule from spec §6.3; **that a `harness:` label is refused when the
  loop sets `permission_mode` or `max_budget_usd`, and why**; that overrides DO
  apply to retries and do NOT apply to a tend dispatch; that anyone who can label
  an issue chooses the model and harness; and that an invalid label stops the
  issue, clearable only by `sessions resume` on the loop's machine — so a label
  applied from GitHub can halt an issue only a local operator can restart.

  `README.md`: extend `## Sessions` (`:155`) with both commands, their selectors
  and flags, the `STOPPED` and `stopped` states, and why a kill HOLDS the issue.
  State the three limits honestly: `--force` will not signal an agent whose
  runner cannot be verified, so one orphaned by an externally killed runner must
  be stopped by hand; a resume is refused while the runner is alive; a kill whose
  runner outlives `--timeout` leaves the issue safe but the agent running. Add
  the labels to `## Configuration` (`:244`) and the exposure to `## Security`
  (`:289`).

- [ ] **Gate:** `make check`.

- [ ] **Commit:** `feat: add sessions kill and sessions resume commands`, then
  `docs: record the override labels and the session kill commands`.

**Acceptance criteria:**
- Every new flag has a `Usage:` string; `--all` without `--yes` fails naming it;
  an interactive decline is a no-op.
- The stopped set is keyed by project, proven with two projects.
- `loop status` shows the reason; `running` beats `STOPPED`.
- Every rejection rule, the `harness:` refusal and its reason, the tend limit,
  the retry inclusion, all three operational limits, and the
  GitHub-halts/local-restarts asymmetry are documented.
- `make check` passes.

---

## Pipeline State

| Field   | Value                                                        |
|---------|--------------------------------------------------------------|
| stage   | 5 (pr feedback loop)                                         |
| class   | large (new command, new columns, signals, argv-bound values) |
| profile | backend                                                      |
| branch  | feat/session-kill-and-label-overrides                        |
| pr      | #12                                                          |
| gate    | approved 2026-08-26                                          |
| round   | 1 (awaiting review)                                          |
