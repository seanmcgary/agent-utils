# Session kill and label overrides — design

Source: issue #11, "QOL improvements".

This document specifies two features:

1. An operator stops a running session from the command line.
2. A label on an issue overrides the agent model, the harness, or the effort
   for that issue.

## 1. Premise check

The check read the code. It records the findings here with `file:line`
evidence.

### Entry path

- A tick reads the GitHub snapshot and the stored state. `engine.Decide`
  returns a plan of decisions (`internal/engine/engine.go:17`).
- `dispatch` writes a dispatch row, creates the worktree, and spawns a
  DETACHED runner process (`internal/loopcmd/tick.go:386`).
- The runner is this program again, invoked as
  `agent-utils internal run-agent --dispatch <id>`
  (`internal/runner/runner.go:39`). It starts a new session with `Setsid`.
- The runner renders the prompt and calls `runner.Supervise`
  (`internal/loopcmd/tick.go:541`). Supervise runs `claude` or `pi` in a NEW
  process group (`internal/runner/runner.go:154`) and records the outcome.

### Blast radius

The change is inside this repository only. It touches these packages:

- `internal/config` — the override syntax and its validation.
- `internal/engine` — the decision to stop an issue, and the override on a
  decision.
- `internal/store` — two new `issues` columns and four new `dispatches`
  columns.
- `internal/loopcmd` — the kill action, the resume action, and the dispatch
  path.
- `internal/proc` — the signal helper.
- `internal/runner` — the signal handler, the effective agent settings, and
  the agent process identifier.
- `cmd/agent-utils` — the `sessions kill` and `sessions resume` commands.
- `docs/configuration.md` and `README.md` — the reference and the guide.

### Prior art to reuse

- `proc.IsAlive` confirms that a process identifier is really this dispatch's
  runner (`internal/proc/proc.go:34`). The kill action reuses it.
- `listener stop` refuses a non-positive process identifier before it calls
  `kill(2)` (`cmd/agent-utils/listener.go:925`). The kill action reuses the
  rule.
- `loop reset` takes the loop's file lock before it changes state
  (`cmd/agent-utils/main.go:662`). The kill action takes the same lock.
- `addedColumns` adds a column to an existing database
  (`internal/store/store.go:314`). The new columns use it.
- `Issue.HasAnyLabel` shows the repository's label rules
  (`internal/ghub/types.go:60`).
- `PullRequest.Trusted` shows how the repository draws a trust boundary
  (`internal/ghub/types.go:94`).

### Outcome

The premise holds. Neither feature exists. Two facts changed the design, and
section 2 and section 4 record them.

### Class and profile

- Class: **Large**. The change adds a command, adds configuration syntax, adds
  database columns, and sends signals to processes.
- Profile: **backend**. The program is a command line tool and a daemon. It
  has no user interface.

## 2. Finding: an operator stop needs its own state

`engine.Decide` dispatches an issue when the issue carries the trigger label
and no dispatch is live (`internal/engine/engine.go:113`). An operator who
kills the agent does not remove the trigger label. The next tick therefore
starts a new dispatch at once.

The program has one existing "do not dispatch" flag, `issues.parked`
(`internal/store/types.go:52`). The design does NOT reuse it. `Decide` never
reads `parked`, because `parkRetryExhausted` also removes the trigger label,
and the check for the trigger label is what stops the issue
(`internal/engine/engine.go:110`). A human who adds the trigger label again
un-parks the issue. If `Decide` started to read `parked`, that documented
recovery path would stop working.

The design adds a separate flag. The two flags have different meanings:

| Flag | Meaning | How it clears |
|------|---------|---------------|
| `parked` | The loop gave up after the retry cap. | A human adds the trigger label again. |
| `stopped` | An operator stopped this issue, or a label is invalid. | An operator runs `sessions resume`. |

## 3. Finding: the runner has no signal handler

`main` builds the root context with `context.Background()`
(`cmd/agent-utils/main.go:49`). No code in the runner path calls
`signal.NotifyContext`. The agent child runs in its own process group
(`internal/runner/runner.go:154`), and the runner runs in its own session
(`internal/runner/runner.go:46`).

A SIGTERM sent to the runner today has two bad effects:

- The runner dies without a record. A later tick reaps the row as "runner
  process died" and marks the issue for retry
  (`internal/loopcmd/tick.go:270`).
- The agent survives. It is in a different process group, so the signal does
  not reach it. It keeps work in a worktree that the loop believes is free.

Section 5 specifies the fix.

## 4. Feature 1: stop a session from the command line

### 4.1 The commands

    agent-utils sessions kill   --session <id> | --issue <n> | --all
    agent-utils sessions resume --session <id> | --issue <n> | --all

Both commands sit at the top level, beside `sessions list`. They accept the
same `--project` and `--loop` selectors that `sessions list` accepts
(`cmd/agent-utils/main.go:321`). The operator reads a session identifier from
the `sessions list` table and types it back.

Rules:

- The three selectors are mutually exclusive. Exactly one is required.
- `--issue` needs a project. The command resolves the project from `--project`
  or from the working directory.
- `--all` is destructive, so it requires `--yes`.
- `kill` accepts `--force`. Section 5.3 specifies it.
- `kill` accepts `--timeout`. It sets how long the command waits for the
  runner to exit. The default is 30 seconds.

### 4.1.1 How a selector resolves

A selector names a set of issues, because the flag lives on the issue and the
signal goes to the dispatch.

- `--session <id>` resolves through the existing session grouping
  (`internal/loopcmd/sessions.go:246`). A session identifier names one project,
  one loop, and one issue, so no other flag is needed. If `--project` is also
  given, the command restricts the search to that project.
- `--issue <n>` needs a project and a loop. An issue number is unique within a
  loop, not within a project, so a project with two loops can hold two rows for
  one number. The command resolves the project from `--project` or from the
  working directory. If `--loop` is absent and the number matches rows in more
  than one loop, the command fails and names the loops.
- `--all` selects every running dispatch (for `kill`) or every stopped issue
  (for `resume`), narrowed by `--project` and `--loop`. With neither narrowing
  flag it spans the machine.

### 4.2 What `kill` does, in order

The command performs these steps for each target. The order is part of the
specification.

1. Resolve the targets. A target is a dispatch row whose status is `running`.
2. Take the loop's file lock, as `loop reset` does. A tick must not dispatch
   while the command changes state. If a tick holds the lock, the command
   fails and names the loop.
3. Set `stopped` and `stopped_reason` on the issue. **This write happens
   BEFORE the signal.** A tick that starts immediately after the agent dies
   then reads the flag and does not dispatch.
4. Refuse to signal a process identifier that is not positive. The rule and
   its reason come from `listener stop`.
5. Confirm the target with `proc.IsAlive(pid, d.RunnerID())`. The operating
   system reuses process identifiers, so the command must not signal an
   unrelated process. If the process is gone, go to step 8.
6. Send SIGTERM to the runner.
7. Wait for the runner to exit, up to `--timeout`. Poll with `proc.IsAlive`.
8. Record the outcome, if the runner did not record one itself. Section 4.4
   specifies this.
9. Print one line for each target.

A tend dispatch holds no issue state (`internal/runner/runner.go:311`), so a
kill of a tend dispatch signals the process and records the outcome, and sets
no flag. There is no issue to stop.

### 4.3 What `resume` does

`resume` clears four fields on the issue:

- `stopped`
- `stopped_reason`
- `needs_retry`
- `retry_after`

It clears the last two because the killed runner records a failed dispatch,
and `runner.finish` marks the issue for retry on a failure
(`internal/runner/runner.go:320`). An operator who stops an issue and starts
it again must not inherit a failure the agent did not earn, or a backoff
deadline that delays the next dispatch.

`resume` does NOT clear `parked`. The two flags are independent, and a resume
must not silently undo a retry-cap park.

### 4.4 The recorded outcome

A killed dispatch is recorded with status `failed` and with the api_error
`killed by operator`. The design adds no new dispatch status. A new status
would have to be understood by `isLive`, by the two renderers, by the retry
rules, and by every existing query, and the `stopped` flag already carries the
meaning that matters to the loop.

The runner records the outcome itself on the graceful path, because the
signal handler lets `Supervise` finish normally. The command writes the
outcome only when the runner is already gone, or when `--force` killed it.

`sessions list` shows `STOPPED` in the STATE column for a session whose issue
carries the `stopped` flag. The flag ranks above `failed` and below `running`
in that column.

## 5. Feature 1: the signal path

### 5.1 The runner handles SIGTERM

`internal run-agent` builds its context with
`signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)`.

The change is scoped to that one command. Other commands are short, and a
handler on them buys nothing.

On a signal the existing machinery does the work:

1. The context cancels.
2. `cmd.Cancel` sends SIGTERM to the agent's process group
   (`internal/runner/runner.go:157`).
3. `cmd.Wait` returns, bounded by `WaitDelay`.
4. The existing sweep sends SIGKILL to the agent's process group
   (`internal/runner/runner.go:215`).
5. `finish` records the outcome (`internal/runner/runner.go:305`).

The runner therefore records its own death, and it leaves no agent behind.

### 5.2 The runner records the agent's process identifier

`Supervise` writes `cmd.Process.Pid` to a new `dispatches.agent_pid` column
immediately after `cmd.Start`.

The column exists for `--force`. Without it, nothing outside the runner knows
which process group the agent occupies.

### 5.3 `--force`

`--force` kills the agent first, then the runner:

1. Send SIGKILL to the agent's process group, addressed as the negative of
   `agent_pid`.
2. Send SIGKILL to the runner.
3. Write the outcome, because no process survives to write it.

The order matters. SIGKILL on the runner alone leaves the agent running in a
worktree that the loop believes is free.

`--force` skips the wait in step 7 of section 4.2.

The command validates `agent_pid` exactly as it validates the runner's
identifier. A value that is not positive is not signalled.

### 5.4 A new helper in `internal/proc`

`internal/proc` gains two functions:

    // Signal sends sig to pid after it confirms that pid is the runner for
    // dispatchID.
    func Signal(pid int, dispatchID int64, sig syscall.Signal) error

    // SignalGroup sends sig to the process group led by pid.
    func SignalGroup(pid int, sig syscall.Signal) error

The package already owns the rule that a process identifier must be checked
against the dispatch before it is trusted. The signal belongs with that rule,
not in the command layer.

## 6. Feature 2: label overrides

### 6.1 Syntax

Three label prefixes override three agent settings:

| Label | Overrides | Example |
|-------|-----------|---------|
| `model:<value>` | `agent.model` | `model:claude-opus-5` |
| `harness:<value>` | `agent.harness` | `harness:pi` |
| `effort:<value>` | `agent.effort` | `effort:high` |

The prefixes are fixed. They are not configurable. The feature is always
active; no configuration field enables it.

The comparison of the prefix ignores case, as every other label comparison in
this program does (`internal/ghub/types.go:48`). The VALUE keeps its case,
because a model identifier is case-sensitive.

### 6.2 The parser

One new function owns the syntax:

    // ParseOverrides reads the agent overrides from an issue's labels.
    func ParseOverrides(labels []string) (Overrides, error)

    type Overrides struct {
        Model   string
        Harness string
        Effort  string
    }

It lives in `internal/config`, beside the rules it enforces. Nothing else
parses these labels.

### 6.3 Validation

`ParseOverrides` returns an error in each of these cases:

1. The value is empty. `model:` names no model.
2. The value contains a space or any other whitespace.
3. The value starts with `-`. The value becomes an element of an `exec`
   argument list, and a value that starts with `-` is read as a flag.
4. Two labels carry the same prefix. `model:a` and `model:b` on one issue is
   an error, not a choice.
5. `harness` is not `claude` and is not `pi`. The list is the one
   `config.validate` already enforces (`internal/config/config.go:215`).

One more rule needs the configuration as well as the labels, so `Decide`
applies it after the parse: the `pi` harness requires a model
(`internal/config/config.go:232`). An issue that selects `harness:pi` on a
loop whose `agent.model` is empty must also carry a `model:` label.

Rule 3 is the security rule. Section 9 explains it.

### 6.4 An invalid label stops the issue

`engine.Decide` gains a decision kind, `KindStop`. `Decide` emits it when
`ParseOverrides` fails for an issue that would otherwise be dispatched.

The tick applies a `KindStop` decision by setting `stopped` and
`stopped_reason` on the issue. The reason is the parse error.

This makes the two features share one mechanism. The operator sees the reason
in `loop status` and in `sessions list`, fixes the label, and runs
`sessions resume`.

Nothing is written to GitHub. `parkRetryExhausted` stays the only GitHub write
this program performs (`internal/loopcmd/tick.go:493`).

`Decide` emits `KindStop` only for an issue it would otherwise dispatch. An
invalid label on an issue with no trigger label changes nothing.

### 6.5 How an override reaches the agent

The runner is a detached process. It never sees the tick's GitHub snapshot, so
it cannot read the labels. The override travels the same way `title` and
`behind_by` already travel: on the row.

1. `engine.Decision` gains an `Overrides` field. `Decide` fills it.
2. `dispatch` writes the three values to three new `dispatches` columns:
   `model`, `harness`, and `effort` (`internal/loopcmd/tick.go:429`).
3. `RunAgent` reads them from the row and puts them in the invocation
   (`internal/loopcmd/tick.go:541`).
4. `runner.Invocation` gains an `Overrides` field.

### 6.6 The effective settings

`internal/runner` gains one function:

    // Effective returns the agent settings for one invocation. An override
    // replaces the configured value; an empty override keeps it.
    func Effective(cfg *config.Config, ov config.Overrides) Settings

    type Settings struct {
        Harness string
        Model   string
        Effort  string
    }

Three call sites use it: `BuildArgs`, `PiBuildArgs`, and the harness switch in
`Supervise` (`internal/runner/runner.go:139`).

**`Effective` never mutates the configuration.** A function that changed
`cfg.Agent` in place would leave a caller holding a configuration that no
longer matches the file it was loaded from.

### 6.7 Scope

Overrides apply to a dispatch that comes from an issue: `start`, `resume`,
`retry_start`, and `retry_resume`.

Overrides do NOT apply to a `tend` dispatch. A tend run rebases a pull
request. It is not the issue's work, and the reference loops configure it
once. The documentation states this limit.

## 7. Data model

### 7.1 New columns

Six columns are added through `addedColumns` (`internal/store/store.go:314`).
Each has a default, so no backfill is needed.

| Table | Column | Type | Meaning |
|-------|--------|------|---------|
| `issues` | `stopped` | `INTEGER NOT NULL DEFAULT 0` | An operator stopped this issue. |
| `issues` | `stopped_reason` | `TEXT NOT NULL DEFAULT ''` | Why it is stopped. |
| `dispatches` | `agent_pid` | `INTEGER NOT NULL DEFAULT 0` | The agent child's process identifier. |
| `dispatches` | `model` | `TEXT NOT NULL DEFAULT ''` | The model override for this dispatch. |
| `dispatches` | `harness` | `TEXT NOT NULL DEFAULT ''` | The harness override for this dispatch. |
| `dispatches` | `effort` | `TEXT NOT NULL DEFAULT ''` | The effort override for this dispatch. |

An empty override column means "no override". It does not mean "the empty
model".

### 7.2 Which writes touch `stopped`

- `sessions kill` sets it.
- A `KindStop` decision sets it.
- `sessions resume` clears it.
- `BeginDispatch` must NOT clear it. A dispatch can only begin when the issue
  is not stopped, so a clear there would be unreachable at best and a silent
  un-stop at worst.
- `MarkSucceeded` must NOT clear it, for the same reason.

### 7.3 The rebuild list

`upgradeKeys` rebuilds the `issues` table and carries its columns by name
(`internal/store/store.go:350`). The two new `issues` columns are added to
that list. A rebuild that dropped them would silently un-stop every stopped
issue.

## 8. Errors

| Case | Behaviour |
|------|-----------|
| No selector, or more than one | The command fails and names the selectors. |
| `--all` without `--yes` | The command fails and names `--yes`. |
| The session identifier matches nothing | The command fails and names the identifier. |
| A tick holds the loop's lock | The command fails and names the loop, as `loop reset` does. |
| The process identifier is not positive | The command does not signal. It records the outcome and reports the row. |
| The process is not this dispatch's runner | The command does not signal. It records the outcome and reports that the process was gone. |
| The runner does not exit within the timeout | The command reports it and names `--force`. The `stopped` flag is already written, so the issue is safe. |
| `kill` finds no running dispatch | The command reports it and exits 0. Killing nothing is not an error. |
| `resume` finds no stopped issue | The command reports it and exits 0. |

A failure on one target does not abandon the others. The command tries every
target and reports each one.

## 9. Security

### 9.1 The exposure

The override labels are always active. Anyone who can label an issue can
therefore choose which binary the loop runs and which model it bills.

GitHub grants the label permission at triage level or above, so the actor is
a collaborator on the repository, not the public. The repository already runs
an agent with a repository-write token on any issue such an actor labels. The
new exposure is therefore the choice of binary and model, not a new path to
code execution.

The specification records the exposure. The owner chose an always-active
feature over a configuration gate.

### 9.2 The argument injection rule

Rule 3 in section 6.3 is the one that must not be dropped. An override value
becomes an element of the argument list that `exec` receives
(`internal/runner/args.go:26`). A value such as `--dangerously-skip-permissions`
would become a flag rather than a model name.

The rule rejects any value that starts with `-`. Go's `exec` passes an
argument list, not a shell string, so no quoting or shell metacharacter rule
is needed; the flag rule is the whole hazard.

`harness` is checked against a closed list, so it can never name another
binary.

### 9.3 What a signal may reach

The kill command signals a process identifier that it read from the database.
Two rules bound it:

- The identifier must be positive. A non-positive value is a broadcast to
  `kill(2)`.
- `proc.IsAlive` must confirm that the process is this dispatch's runner. The
  operating system reuses identifiers, and the row can be old.

The same two rules apply to `agent_pid` under `--force`.

## 10. Testing

| Area | Test |
|------|------|
| `ParseOverrides` | A table test for each valid case and each of the five errors. |
| The `pi` model rule | A test that `harness:pi` with no model, from either source, is rejected. |
| `Decide` | A stopped issue produces no decision and reports a reason. |
| `Decide` | An invalid label produces `KindStop`, and only for an issue that would otherwise be dispatched. |
| `Effective` | An override replaces a value; an empty override keeps it; the configuration is unchanged. |
| `BuildArgs` and `PiBuildArgs` | The override reaches the argument list. |
| Store | The migration adds the six columns to an old database, and the rebuild keeps the two `issues` columns. |
| Store | `BeginDispatch` and `MarkSucceeded` leave `stopped` alone. |
| Runner | A SIGTERM to a live runner records an outcome and leaves no agent process. The package already spawns real processes. |
| Kill | Every guard: no selector, two selectors, `--all` without `--yes`, a non-positive identifier, a reused identifier, a held lock. |
| Kill | The `stopped` write happens before the signal. |
| Resume | The four fields clear, and `parked` does not. |
| Docs | `TestEveryConfigFieldIsDocumented` continues to pass. |

The gate is `make check`, which runs `fmtcheck`, `vet`, `lint`, and `test`.

## 11. Out of scope

- A configuration field that disables the override labels. The owner chose an
  always-active feature.
- Overrides on a tend dispatch.
- A new dispatch status for a killed run.
- Any new GitHub write.
- A change to `parked` or to the retry-cap park path.
