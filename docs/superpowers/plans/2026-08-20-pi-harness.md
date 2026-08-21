# Implementation plan: pi harness

**For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development
(recommended) or executing-plans to implement this plan task-by-task.

Design: [`docs/superpowers/specs/2026-08-20-pi-harness-design.md`](../specs/2026-08-20-pi-harness-design.md)

## Pipeline State

| Field   | Value                                                                 |
|---------|-----------------------------------------------------------------------|
| stage   | 4 (commit review)                                                  |
| class   | standard (adds a config surface and a second output parser; contained) |
| profile | backend                                                               |
| branch  | feat/pi-harness                                                       |
| pr      | #6                                                                    |
| gate    | approved 2026-08-20                                                   |
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
  |  binary    = Harness == "pi" ? "pi" : "claude"
  |  args      = Harness == "pi" ? PiBuildArgs(cfg, inv) : BuildArgs(cfg, inv)
  |  stream    = Harness == "pi" ? ParsePiStream(tee) : ParseStream(tee)
  |  duration  = Harness == "pi" ? wallclock measure : stream duration
  v
store.DispatchResult (sessions, retry, breaker all unchanged)
```

Session continuity: both harnesses carry one session id. For pi, `--session-id <id>` creates
the session when the id is new and resumes it when the id exists (verified below), so the same
stored session id covers start and resume. The claude path keeps `--session-id` for a start
and `-r <id>` for resume.

## Verified external API (do not re-derive)

The complete pi contract was confirmed by running `pi -p --mode json "prompt"` on this
machine with the `anthropic` and `openrouter` providers configured. Do not re-derive these
facts; the exact shapes and flags are:

- Print mode is `pi -p --mode json "prompt"`. Stdout is one JSON object per line.
- The first line is the session header: `{"type":"session","id":"<uuid>",...}`.
- Each assistant reply is `{"type":"message_end","message":{...,"stopReason":"stop"|"error",...`"errorMessage":"...","usage":{"cost":{"total":<n>}}}}`.
- A clean run ends with `agent_end` then `agent_settled`.
- A failed run ends after internal retries with `auto_retry_end` `success:false`.
- `--session-id <id>` creates the session when the id is new and resumes it when the id was
  seen before. Verified across two separate `pi -p` invocations that shared one id.
- pi has no `--max-budget-usd` and no `--permission-mode`. Its thinking flag is
  `--thinking <level>`, whose levels include `low`, `medium`, `high`, `xhigh`, `max`.
- `-p` print mode runs tools to completion with no permission gate (verified with `bash`).

Keep `agent.harness`, the JSON field names, and the pi argument spelling exactly as declared
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
> operator who typed "-25" meaning "25" would have run uncapped and no warning.

From `cmd/agent-utils/main.go`, the rule that governs every credential:

> // The token must come from the environment, never a flag. A flag value
> // shows up in `ps` output and in the shell history of anyone who typed it.

From `cmd/agent-utils/main.go`, `selectedProject`, the flag-shadowing rule:

> // It cannot use c.String("name"): the loop subcommands define their OWN --name
> // for the loop, and urfave/cli lets a child shadow a parent's flag of the same
> // name. Reading the flag from the command that always declares it.

The code style follows the codebase precedent observed in the existing `runner`, `config`,
and `engine` packages: package-level doc comments, no unused imports, one idea per comment,
no silent field marshalling, conventional-commit doc commits, and a `review: yes` gate on
anything that touches the process boundary or the trust boundary.

## Task list

### 1. Add `agent.harness` to config (review: yes)

**Files:** `internal/config/config.go` and `internal/config/config_test.go`.

- [ ] Add field `Harness` `yaml:"harness"` to the `Agent` struct.
- [ ] After `Load` decodes, default an empty `Harness` to `claude` so existing configs do not
      change.
- [ ] Accept `claude` and `pi`. Reject any other value with a message naming the two values.
- [ ] When `Harness` is `pi`, reject a non-empty `PermissionMode` with a message saying
      `agent.permission_mode` is claude-only.
      the value is a documented no-op, not an error and not a silent drop.

**Acceptance.**
- [ ] `TestLoadAcceptsHarnessDefault` loads a config with no `harness` and reads `"claude"`.
- [ ] `TestLoadAcceptsPiHarness` loads `harness: pi` and reads `"pi"`.
- [ ] `TestLoadRejectsBadHarness` rejects `harness: gemini` with the two-value message.
- [ ] `TestRejectsPiPermissionMode` rejects `harness: pi` with a non-empty `permission_mode`.
- [ ] `TestAcceptsPiBudgetNoOp` loads `harness: pi` with a non-zero `max_budget_usd` and
      succeeds, proving the value is accepted for pi.
- [ ] The claude path tests (`TestLoadValid`, effort, timeout, budget) still pass untouched.

### 2. Add the pi argument builder (review: no)

**Files:** `internal/runner/args.go` and `internal/runner/args_test.go`.

- [ ] Keep `BuildArgs(cfg, inv)` as the claude builder.
- [ ] Add `PiBuildArgs(cfg, inv)` that returns, in order:
      `["-p", "--mode", "json", "--session-id", inv.SessionID, "--model", cfg.Agent.Model]`,
      then `["--thinking", cfg.Agent.Effort]` when `Effort` is non-empty, then the prompt last.
- [ ] `PiBuildArgs` does not read `permission_mode` or `max_budget_usd`. The config layer
      already rejects them for a pi harness.
- [ ] Each `--model` and `--thinking` value is one full argument-list slot, never a value
      glued into another flag string. A config value must not be able to smuggle a second
      flag.

**Acceptance:**
- [ ] `TestPiBuildArgsPrintMode` asserts the first arguments are `-p` then `--mode` `json`.
- [ ] `TestPiBuildArgsCarriesModelAndSession` asserts `--model` and `--session-id` appear in
      order after the header.
- [ ] `TestPiBuildArgsAddsThinking` asserts `--thinking` with the effort level appears when
      the effort is set.
- [ ] `TestPiBuildArgsOmitsEmptyEffort` asserts no `--thinking` appears when effort is empty.
- [ ] `TestBuildArgsCarriesAgentSettings` and the other claude tests still pass unchanged.

### 3. Add the pi stream parser (review: yes)

**Files:** `internal/runner/result.go` and `internal/runner/result_test.go`.

- [ ] Add `ParsePiStream(r io.Reader) (Result, error)`.
- [ ] It reads the `session` line type; cap the buffer for long lines, like claude.
- [ ] It sums `usage.cost.total` over every `message_end` whose `message.role` is
      `assistant`. Summing the whole run, including failed internal retries, is acceptable;
      it records what the stream reports.
- [ ] It remembers the last assistant message and its `stopReason`, `errorMessage`, and text.
- [ ] It succeeds only when the last assistant `stopReason` is `stop`. It fails when the last
      `stopReason` is `error` (APIError is the `errorMessage`) or any other value, such as a
      `max_tokens` early stop (APIError names the stopReason). Fail closed.
- [ ] It returns `ErrNoResult` when no assistant message appears.
- [ ] It returns a `Result` whose `SessionID` is the stream's session id, `CostUSD` the summed
      cost, and `IsError` and `APIError` from the failure rule.

**Acceptance:**
- [ ] `TestParsePiStreamReadsSessionId` feeds a `session` line and a `message_end` and asserts
      the session id.
- [ ] `TestParsePiStreamSumsCost` feeds two assistant `message_end` lines and asserts the sum.
- [ ] `TestParsePiStreamCapturesError` feeds an assistant `stopReason:"error"` with
      `errorMessage` and asserts `IsError` and `APIError`.
- [ ] `TestParsePiStreamIgnoresNoise` feeds non-JSON lines and `tool_execution` events and
      asserts the parser does not choke.
- [ ] `TestParsePiStreamErrorsWhenNoMessage` asserts `ErrNoResult`.
- [ ] `TestParsePiStreamTreatsNonStopAsFailure` feeds a `stopReason:"max_tokens"` and asserts
      `IsError` with an APIError naming the stop reason.
- [ ] `TestParsePiStreamIgnoresNonAssistant` feeds a `message_end` with role `user` and asserts
      it is neither summed nor counted as terminal.

### 4. Wire the harness into `Supervise` (review: yes)

**Files:** `internal/runner/runner.go` and `internal/runner/runner_test.go`.

- [ ] In `Supervise`, choose the binary `"pi"` when `cfg.Agent.Harness` is `pi`, else
      `"claude"`.
- [ ] Choose `BuildArgs` or `PiBuildArgs` and `ParseStream` or `ParsePiStream` from the same
      field.
- [ ] For the `pi` harness, measure wall clock around the child and set `res.DurationMS` to
      the measured value, because the pi stream carries no duration.
- [ ] Keep the stderr file, the process-group kill, the tee, and the drain unchanged. The
      environment filter (`agentEnv()`) is applied identically for both harnesses.

**Acceptance:**
- [ ] `TestSuperviseRecordsSuccess` still passes (claude path unchanged).
- [ ] `TestSuperviseRecordsFailureOnNonZeroExit` still passes.
- [ ] `TestSuperviseRecordsFailureWhenStreamHasNoResult` still passes.
- [ ] `TestSupervisePiUsesPiExecutable` asserts the spawned binary is `pi` when the harness
      is `pi`. The existing tests stub by rewriting PATH; add a stub that records its argv and
      assert argv[0] is `pi` and the header is `-p`, `--mode`, `json`.

### 5. Logs renderer understands the pi stream (review: no)

**Files:** `internal/loopcmd/logs.go`, `internal/loopcmd/logs_test.go`,
`cmd/agent-utils/main.go`.

- [ ] Add a `Harness` field to `LogOptions`. The handler that builds `opts`
      (`loopcmd.Tail` caller in `cmd/agent-utils/main.go`) sets it from `cfg.Agent.Harness`.
- [ ] `newRenderer` reads `opts.Harness`. For a pi harness it recognizes `session`,
      `message_end` (assistant text), `tool_execution_start` (→) and `tool_execution_end`
      (←), and the `agent_settled` boundary, mirroring the claude `renderer.line`
      presentation.
- [ ] Keep `--raw` pass-through as today.

**Acceptance:**
- [ ] `TestRendererShowsWhatMattersAndHidesNoise` still passes (claude path).
- [ ] `TestRendererPiShowsTextAndTools` feeds a small pi stream and asserts the text line and
      the tool arrows.
- [ ] `TestRendererSwitchesOnHarness` asserts the pi branch renders only the pi harness and
      the claude branch only the claude harness.

### 6. Wizard asks for the harness (review: no)

**Files:** `internal/wizard/run.go`, `internal/wizard/write.go`, the wizard templates,
`internal/wizard/run_test.go`, `internal/wizard/write_test.go`,
`internal/wizard/templates_test.go`.

- [ ] Add an `agent.harness` question before `agent.model`, defaulting to `claude` with
      choices `claude` and `pi`.
- [ ] When the answer is `pi`, skip the `agent.permission_mode` question and set the budget
      to `0`.
- [ ] The `write` step persists `harness`. The YAML mirror type in `internal/wizard/write.go`
      (`yamlAgent`) gains a `Harness` field in step with the YAML, so the strict reader accepts
      the generated file.
- [ ] The wizard template files gain `harness` lines.
- [ ] Renumber the wizard question comments. Inserting one question before `agent.model`
      pushes each later question number by one, and the closing comment that names the last
      question shifts too. Update the numbers to match the ask order.

**Acceptance:**
- [ ] Wizard golden tests accept the new answer sequence.
- [ ] `write_test` asserts a pi loop's YAML carries `harness: pi`.
- [ ] The templates still parse.

### 7. Add a pi example (review: no)

**Files:** `examples/pi.yaml` (new), `internal/config/examples_test.go`, and
`examples/execution.yaml` (unchanged).

- [ ] Add `examples/pi.yaml` copying the `execution.yaml` shape with `harness: pi` and a
      `provider/id` model, and `permission_mode` and `max_budget_usd` absent.
- [ ] Extend `internal/config/examples_test.go` so its hardcoded list includes `pi.yaml`.
- [ ] `examples/` regressions (config load tests) pass.

**Acceptance:**
- [ ] `examples_test` loads `pi.yaml` with a harness of `pi` and no permission_mode.
- [ ] The two existing examples still load as `claude`.

### 8. Document the field (review: no)

**Files:** `docs/configuration.md`, `README.md`.

- [ ] In `README.md` and `docs/configuration.md`, do not claim the tool dispatches `claude`
      as the only path; describe the `agent.harness` selector.
- [ ] Add an `agent.harness` entry to the reference's quick table, and add a dedicated
      `agent.harness` subsection (not under `agent.worktree`) with the per-harness mapping
      table from the design.
- [ ] Update the Security section. State that `pi -p` is non-interactive and runs tools with
      no prompt, like claude with permission off the agent's environment is filtered and the
      two-hop token removal still holds; and pi reads its own provider configuration off
      disk, with no secrets added to the environment. State how `defaultProjectTrust` works:
      in a `pi` worktree, pi trusts `AGENTS.md`/`CLAUDE.md` in the worktree only when the
      saved decision allows it; an unset open default lets pi auto-trust those files. Tell
      the operator to keep trust off for a loop whose worktree is fed with untrusted issue
      text, per the README's trust rule.
- [ ] In the model row, note that for `pi` the model must be a `provider/id` path or pattern,
      and that this is documentation-only (no load-time check), exactly as the claude model
      row is.
- [ ] In the `agent.max_budget_usd` row, state that it is a claude-only ceiling: for a `pi`
      harness it is accepted but has no effect, because pi exposes no cost-ceiling flag.

**Acceptance:**
- [ ] The docs mention `harness: pi` and no longer claim that claude is the only path.
- [ ] `TestEveryConfigFieldIsDocumented` stays green after the new field is added.

### 9. Full-suite gate

**Command:** `make check` at the repository root (fmtcheck + vet + lint + all tests).

**Acceptance:** all gated targets pass with the new harness coverage green. Proceed to the
stage-4 commit review and the branch-diff review only after this gate is green.

## Pipeline State updates

Stage 2: the plan is reviewed and the review findings are triaged into the tasks above. The
human gate comes next. On approval, update `stage` to 3 and `gate: approved <date>`, then
implement the tasks in order.