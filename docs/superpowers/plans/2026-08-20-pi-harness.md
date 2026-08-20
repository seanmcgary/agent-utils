# Implementation plan: pi harness

**For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development
(recommended) or executing-plans to implement this plan task-by-task.

Design: [`docs/superpowers/specs/2026-08-20-pi-harness-design.md`](../specs/2026-08-20-pi-harness-design.md)

## Pipeline State

| Field   | Value                                                                 |
|---------|-----------------------------------------------------------------------|
| stage   | 2 (plan review)                                                       |
| class   | standard (adds a config surface and a second output parser; contained) |
| profile | backend                                                               |
| branch  | feat/pi-harness                                                       |
| pr      | n/a                                                                   |
| gate    | pending                                                               |
| round   | 0                                                                     |

## Architecture

The dispatch path stays one function. `runner.Supervise` selects a binary, builds its
arguments, parses its stream, and records one `store.DispatchResult`. The only new surface is
the `agent.harness` field and the pi branch of the parse. Everything downstream of
`store.DispatchResult` is unchanged.

```
internal/loopcmd.RunAgent (tick.go)
  |  render prompt -> runner.Invocation{SessionID, Resume, Prompt}
  v
internal/runner.Supervise (runner.go)
  |  binary  = Harness ? "pi" : "claude"
  |  args    = Harness ? PiBuildArgs(cfg, inv) : BuildArgs(cfg, inv)
  |  stream  = Harness ? ParsePiStream(tee) : ParseStream(tee)
  |  duration = Harness ? wallclock : stream duration
  v
store.DispatchResult (sessions, retry, breaker all unchanged)
```

Session continuity: both harnesses carry one session id. for pi, `--session-id <id>` creates
the session when new and resumes it when it exists, so the same stored id covers start and
resume.

## Verified external API (do not re-derive)

The complete pi contract was confirmed by running `pi -p --mode json "prompt"` on this
machine with the `anthropic` and `openrouter` providers configured. Do not re-derive these
facts; the exact shapes and flags are:

- Print mode is `pi -p --mode json "prompt"`. stdout is one JSON object per line.
- The first line is the session header: `{"type":"session","id":"<uuid>",...}`.
- Each assistant reply is `{"type":"message_end","message":{...,"stopReason":"stop"|"error","errorMessage":"...","usage":{"cost":{"total":<n>}}}}`.
- A clean run ends with `agent_end` then `agent_settled`.
- A failed run ends after internal retries with `auto_retry_end` `success:false`.
- `--session-id <id>` creates the session when the id is new and resumes it when the id was
  seen before. Verified across two separate `pi -p` invocations.
- pi has no `--max-budget-usd` flag and no `--permission-mode` flag; its thinking flag is
  `--thinking <level>`, whose levels include `low, medium, high, xhigh, max`.
- `-p` print mode runs tools to completion with no permission gate (verified with `bash`).

Keep `agent.harness`, JSON field names, and the pi argument spelling exactly as declared
above and in the design doc. Any contract change is a plan revision, not an inline fix.

## Global Constraints

**This repository has no `AGENTS.md`, `CLAUDE.md`, `CONTRIBUTING.md`, `STANDARDS.md`, or
`STYLEGUIDE.md` at its root.** The binding conventions come from `README.md`, `Makefile`,
`.golangci.yml`, `docs/configuration.md`, and codebase precedent. The text below is copied
word for word. Do not paraphrase it. Do not violate it.

From `README.md`, "Development":

> ```bash
> make check       # fmtcheck + vet + lint + test, in that order
> ```

> Tests run with `-p 1` and no cache on purpose: the `worktree` package shells out to real git
> and `runner` spawns real processes, so package-level parallelism is not safe, and a cached
> PASS is not evidence about the working tree.

From `README.md`, "Continuous integration":

> | `test` | Full suite, then again under `-race` |

From `README.md`, "Security":

> A loop dispatches an agent that runs with permission prompts disabled, inside a git worktree,
> on text written by other people. Issue bodies, issue comments, and pull request bodies are
> UNTRUSTED input. An instruction hidden in a comment executes.

> - The agent process gets a filtered environment and `GITHUB_TOKEN` is removed from it, at both
>   hops (the detached runner and the agent itself).

From `README.md`, "Cron":

> Do NOT put the token inline in the crontab. cron runs the whole line through `/bin/sh -c`, so
> a `VAR=value command` prefix puts the token in the shell's argument list, where `ps` shows it
> to every user on the machine.

From `README.md`, "Versioning and releases":

> The semantic version lives in the `VERSION` file at the repository root. It is the single
> source of truth; nothing infers a version from a tag.

From `docs/configuration.md`, the strict-parser rule:

> The parser is **strict**: an unknown key is an error, not a warning. A misspelled key fails
> the load rather than being silently ignored.

From `internal/config/config.go`, the never-silently-ignore rule for budget:

> A NEGATIVE value hits that same "> 0" gate, so it is silently identical to no cap -- an
> #operator who typed "-25" meaning "25" would have run uncapped and no warning.

From `cmd/agent-utils/main.go`, the rule that governs every credential:

> // The token must come from the environment, never a flag. A flag value
> // shows up in `ps` output and in the shell history of anyone who typed it.

From `cmd/agent-utils/main.go`, `selectedProject`, the flag-shadowing rule:

> // It cannot use c.String("name"): the loop subcommands define their OWN --name
> // for the loop, and urfave/cli lets a child shadow a parent's flag of the same
> // name. Reading the flag from the command that declares it is what keeps
> // `project --name web loop tick --name planning` unambiguous.

The code style follows the codebase precedent observed in the existing `runner`,
`config`, and `engine` packages: package-level doc comments, no unused imports, one idea per
comment, no silent field marshalling, and a `review: yes` gate on anything that touches the
process boundary or the trust boundary.

## Task list

### 1. Add `agent.harness` to config (review: yes)

**File:** `internal/config/config.go` and `internal/config/config_test.go`.

- [ ] Add field `Harness` `yaml:"harness"` to the `Agent` struct.
- [ ] After `Load` decodes, default an empty `Harness` to `claude` so existing configs do not
      change.
- [ ] Accept `claude` and `pi`. Reject any other value with a message naming the two values.
- [ ] When `Harness` is `pi`, reject a non-empty `PermissionMode` with a message saying
      `agent.permission_mode` is claude-only.
- [ ] When `Harness` is `pi`, reject a non-zero `MaxBudgetUSD` with a message saying pi has
      no cost ceiling and a non-zero value is a user error.

**Acceptance.**
- `TestLoadAcceptsHarnessDefault` loads a config with no `harness` and reads `"claude"`.
- `TestLoadAcceptsPiHarness` loads `harness: pi` and reads `"pi"`.
- `TestLoadRejectsBadHarness` rejects `harness: gemini` with the two-value message.
- `TestRejectsPiPermissionMode` rejects `harness: pi` with a non-empty `permission_mode`.
- `TestRejectsPiBudget` rejects `harness: pi` with a non-zero `max_budget_usd`.
- The claude path tests (`TestLoadValid`, effort, timeout, budget) still pass untouched.

### 2. Add the pi argument builder (review: no)

**File:** `internal/runner/args.go` and `internal/runner/args_test.go`.

- [ ] Keep `BuildArgs(cfg, inv)` as the claude builder.
- [ ] Add `PiBuildArgs(cfg, inv)` that returns:
      `["-p", "--mode", "json", "--session-id", inv.SessionID, "--model", cfg.Agent.Model]`,
      then `["--thinking", cfg.Agent.Effort]` when `Effort` is non-empty, then the prompt last.
- [ ] `PiBuildArgs` does not read `permission_mode` or `max_budget_usd` (the config layer
      already rejects them for pi).

**Acceptance:**
- `TestPiBuildArgsPrintMode` asserts the first arguments are `-p` then `--mode` `json`.
- `TestPiBuildArgsCarriesModelAndSession` asserts `--model` and `--session-id` are present
  in the right order after the header.
- `TestPiBuildArgsOmitsEmptyEffort` asserts no `--thinking` appears when effort is empty.
- `TestBuildArgsCarriesAgentSettings` and the other claude tests still pass unchanged.

### 3. Add the pi stream parser (review: yes)

**Files:** `internal/runner/result.go` and `internal/runner/result_test.go`.

- [ ] Add `ParsePiStream(r io.Reader) (Result, error)`.
- [ ] It reads the `session` line type; cap the buffer for long lines, like claude.
- [ ] It sums `usage.cost.total` over every `message_end` whose `message.role` is
      `assistant`.
- [ ] It remembers the last assistant message and its `stopReason`, `errorMessage`, and text.
- [ ] It succeeds when the last assistant `stopReason` is `stop`; it fails when any assistant
      `stopReason` is `error`.
- [ ] It returns `ErrNoResult` when no assistant message appears.
- [ ] It returns a `Result` whose `SessionID` is the session id, `CostUSD` the summed cost,
      and `IsError` and `APIError` from the failure rule.

**Acceptance:**
- `TestParsePiStreamReadsSessionId` feeds a `session` line and a `message_end` and asserts
  the session id.
- `TestParsePiStreamSumsCost` feeds two assistant `message_end` lines and asserts the sum.
- `TestParsePiStreamMissesError` feeds an assistant `stopReason:"error"` with `errorMessage`
  and asserts `IsError` and `APIError`.
- `TestParsePiStreamIgnoresNoise` feeds non-JSON lines and `tool_execution` events and
  asserts the parser does not choke.
- `TestParsePiStreamErrorsWhenNoMessage` asserts `ErrNoResult`.

### 4. Wire the harness into `Supervise` (review: yes)

**Files:** `internal/runner/runner.go` and `internal/runner/runner_test.go`.

- [ ] In `Supervise`, choose the binary `"pi"` when `cfg.Agent.Harness` is `pi`, else
      `"claude"`.
- [ ] Choose `BuildArgs` or `PiBuildArgs` and `ParseStream` or `ParsePiStream` on the same
      field.
- [ ] For a `pi` harness, measure wall clock around the child process and set
      `res.DurationMS` to the measured value, because the pi stream carries no duration.
- [ ] Keep the stderr file, the process-group kill, the tee, and the drain unchanged.

**Acceptance:**
- `TestSuperviseRecordsSuccess` still passes (claude path unchanged).
- `TestSuperviseRecordsFailureOnNonZeroExit` still passes.
- `TestSuperviseRecordsFailureWhenStreamHasNoResult` still passes.
- A new `TestSupervisePiUsesPiExecutable` asserts the spawned binary is `pi` when the harness
  is pi (inspect `argv` via the facade used by existing runner tests).

### 5. Logs renderer understands the pi stream (review: no)

**Files:** `internal/loopcmd/logs.go` and `internal/loopcmd/logs_test.go`.

- [ ] Thread the harness into the renderer from the loop's config.
- [ ] For a `pi` harness, recognize `session`, `message_end` (assistant text),
      `tool_execution_start` (→) and `tool_execution_end` (←), and the `agent_settled`
      boundary, mirroring the claude `renderer.line` presentation.
- [ ] Keep `--raw` pass-through as today.

**Acceptance:**
- `TestRendererShowsWhatMattersAndHidesNoise` still passes (claude path).
- A new `TestRendererPiShowsTextAndTools` feeds a small pi stream and asserts the text
  line and the tool arrows.

### 6. Wizard asks for the harness (review: no)

**Files:** `internal/wizard/run.go`, `internal/wizard/write.go`, the wizard templates,
`internal/wizard/run_test.go`, `internal/wizard/write_test.go`,
`internal/wizard/templates_test.go`.

- [ ] Add a `agent.harness` question before `agent.model`, defaulting to `claude` with
      choices `claude` and `pi`.
- [ ] When the answer is `pi`, skip the `agent.permission_mode` question and set the budget
      to `0`.
- [ ] The `write` step persists `harness` into the YAML, and the generator adds it.
- [ ] The wizard template files gain `harness` lines.

**Acceptance:**
- Wizard golden tests accept the new answer sequence.
- `write_test:TestWrite` asserts a pi loop's YAML carries `harness: pi`.
- The templates still parse.

### 7. Add a pi example (review: no)

**Files:** `examples/pi.yaml` (new) and `examples/execution.yaml` (unchanged).

- [ ] Add `examples/pi.yaml` copying the `execution.yaml` shape with `harness: pi` and a
      `provider/id` model, and `permission_mode` and `max_budget_usd` absent.
- [ ] `examples/` regressions (config load tests) pass.

**Acceptance:**
- `examples_test` loads `pi.yaml` with a harness of `pi` and no permission_mode.
- The two existing examples still load as `claude`.

### 8. Document the field (review: no)

**Files:** `docs/configuration.md`, `README.md`.

- [ ] In `README.md` and `docs/configuration.md`, do not claim the tool "dispatch claude"
      as the only path; describe the `agent.harness` selector.
- [ ] Add an `agent.harness` entry to the reference's quick table and to the `agent.worktree`
      section with the per-harness mapping table from the design.
- [ ] Update the Security section: `pi -p` is non-interactive and runs tools with no gate,
      like claude with permission off; state that the filtered environment and
      trust rule still hold.
- [ ] In the model row, note that for `pi` the model must be a `provider/id` or pattern.

**Acceptance:**
- The docs mention `harness: pi` and no longer claim that claude is the only path.

### 9. Full-suite gate

**Command:** `make check` at the repository root (fmtcheck + vet + lint + all tests).

**Acceptance:** all gated targets pass with the new harness coverage green. Proceed to the
stage-4 commit review and the branch-diff review only after this gate is green.

## Pipeline State updates

Stage 2: plan is ready. The human gate comes next. On approval, update `stage` to 3 and
`gate: approved <date>`, then implement in the order above.