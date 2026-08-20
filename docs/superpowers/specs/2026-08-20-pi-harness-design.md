# Design: pi harness

## Premise and blast-radius check (step-0 findings)

The tool dispatches a coding agent for each task. Today it only drives `claude -p`.
`internal/runner` owns every agent invocation. This makes pi a second, selectable harness on
one existing path.

- **Entry path.** `internal/loopcmd.RunAgent` renders a prompt and calls
  `runner.Supervise` (`internal/loopcmd/tick.go`). `Supervise` runs `claude -p` with
  `runner.BuildArgs`, parses the claude stream-json with `runner.ParseStream`, and records a
  `store.DispatchResult`. The detached runner process is `internal/runner/runner.go` (spawned
  via `runner.Spawn` as `internal run-agent`).
- **Blast radius.** These packages consume the dispatch or the agent the `claude` path:
  `internal/config` (loop fields), `internal/runner` (Spark = executable + parse), `internal/
  loopcmd` (tick, logs render, sessions), `internal/wizard` (config authoring),
  `internal/store` (dispatch and session tables), `docs/configuration.md`, `README.md`,
  `examples/`. All are in this repository.
- **Prior art.** The `agent` section already exists with `model`, `effort`,
  `permission_mode`, `max_budget_usd`, `timeout`. A new `harness` field selects the binary
  and the argument mapping. The `agent.worktree` fence, the `agent.timeout` process-group
  kill, and the `retry` / breaker / tend paths are harness-agnostic and stay unchanged.
- **Contradiction scan.** The code matches the docs and the user's description: the loop
  dispatches `claude` only. There is no harness abstraction today. The task is to make `pi`
  a selectable alternative without changing the claude path.

## Verified external API (the pi contract)

Confirmed by running pi on this machine with `pi -p --mode json "prompt"`. The output is one
JSON object per line on stdout, beginning with a session header:

```json
{"type":"session","version":3,"id":"<uuid>","timestamp":"...","cwd":"..."}
```

Each assistant reply is a `message_end` event whose `message` carries `role`,
`stopReason`, and `usage`:

```json
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"..."}],"provider":"anthropic","model":"anthropic/claude-sonnet-4-5","stopReason":"stop","usage":{"input":n,"output":n,"cacheRead":n,"cacheWrite":n,"totalTokens":n,"cost":{"input":n,"output":n,"cacheRead":n,"cacheWrite":n,"total":n}}}}
```

A clean run ends with `{"type":"agent_end",...}` then `{"type":"agent_settled"}`. A failed
run has an assistant `stopReason:"error"` and an `errorMessage` field; pi retries internally
and finishes with `{"type":"auto_retry_end","success":false,...}`. Tool calls stream as
`tool_execution_start` / `tool_execution_update` / `tool_execution_end` events.

**Session resume is verified.** Running `pi -p --mode json --session-id <id>` twice against a
cheap model, with a second-priority task ("remember X"; "what is X?"), the second run
answered with the value stored by the first. So `--session-id <id>` creates the session when
the id is new and resumes it when the id exists. This is the session-continuity contract.

**No cost ceiling, no duration in the stream.** pi has no `--max-budget-usd` flag and the
JSON stream carries no wall-clock duration. The `agent.timeout` group kill and `retry`
accounting are unaffected. The plan rejects a non-zero `max_budget_usd` for `harness: pi`.

**Non-interactive mode has no permission gate.** In `-p` print mode pi runs tools (for
example `bash`) to completion with no prompt, matching the claude no-prompt posture. Trust
of project-local files follows pi's `defaultProjectTrust` setting in non-interactive mode.

## Design

Add one field, `agent.harness`, with values `claude` (the default) and `pi`. A missing value
defaults to `claude`. Every harness-specific behaviour selects on this field.

| Behaviour | claude | pi |
|---|---|---|
| Executable | `claude` | `pi` |
| Print mode | `-p` | `-p --mode json` |
| Model | `--model <m>` | `--model <m>` (must be `provider/id` or a pattern) |
| Thinking | `--effort <level>` | `--thinking <level>` |
| Session start | `--session-id <id>` | `--session-id <id>` |
| Session resume | `-r <id>` | `--session-id <id>` |
| Permission mode | `--permission-mode <m>` | none (invalid for pi) |
| Cost ceiling | `--max-budget-usd <n>` | none (non-zero invalid for pi) |
| Stream parse | claude stream-json | pi event stream |
| Duration | from the stream | wall clock in `Supervise` |

The `agent.timeout`, `agent.worktree`, `retry.*`, `tend_pr`, labels, and the whole
`internal/engine` decision loop are harness-agnostic and are not changed.

### pi stream parser

A dedicated parser reads the pi event stream and emits the same `Result` shape the claude
parser emits.

- It reads the session id from the first `session` line.
- It sums `usage.cost.total` over every assistant `message_end` to get the run cost.
- It keeps the last assistant message.
- A run succeeds when the last assistant `stopReason` is `stop` and no error appeared,
  and fails when any assistant message has `stopReason "error"`.

Because the shape is the same, the store, the sessions list, and the retry accounting need no
structural change.

### Authoring and documentation

The wizard gains a harness question. For a `pi` harness it skips the permission-mode
question and the budget default. A new example `examples/pi.yaml` shows a pi loop.
`docs/configuration.md` and `README.md` learn the field, the per-harness mapping, and the
pi security notes.

## Task order

Config → arguments → result parsing → runner → logs render → wizard → examples → docs →
README, in that order. Each is a file-level change. There is no schema migration, one new
config field, and one new output parser.