# Loop configuration reference

One YAML file defines one loop. `agent-utils loop tick --config <file>` reads it on every
run.

The parser is **strict**: an unknown key is an error, not a warning. A misspelled key fails
the load rather than being silently ignored. Every validation error for a file is reported
together, in a stable order.

`examples/planning.yaml` and `examples/execution.yaml` are complete working files. Read this
reference for what each field means; read those for a shape to copy.

## Contents

- [Quick reference](#quick-reference)
- [Identity and paths](#identity-and-paths)
- [`labels`](#labels)
- [`agent`](#agent)
- [`tend_pr`](#tend_pr)
- [`retry`](#retry)
- [Prompts](#prompts)
- [Template variables](#template-variables)
- [Validation rules](#validation-rules)

## Quick reference

| Field | Type | Required | Default |
|---|---|---|---|
| `name` | string | yes | — |
| `repo` | string `owner/name` | yes | — |
| `checkout_base_dir` | path | yes | — |
| `worktree_dir` | path | yes | — |
| `state_dir` | path | yes | — |
| `default_branch` | string | yes | — |
| `labels.trigger` | string | yes | — |
| `labels.in_flight` | string | yes | — |
| `labels.blocked` | string | yes | — |
| `labels.review` | string | yes | — |
| `labels.terminal` | string | no | empty |
| `labels.veto` | list of string | no | empty |
| `agent.model` | string | yes | — |
| `agent.effort` | enum | no | claude's own default |
| `agent.permission_mode` | enum | no | claude's own default |
| `agent.worktree` | enum | yes | — |
| `agent.max_budget_usd` | number | no | `0`, meaning no limit |
| `agent.timeout` | duration | yes | — |
| `i_understand_bypass_permissions` | bool | only with `bypassPermissions` | `false` |
| `tend_pr` | bool | no | `false` |
| `retry.max` | int | no | `0`, meaning never retry |
| `retry.backoff_ticks` | list of int | yes if `retry.max > 0` | empty |
| `retry.breaker.orphan_threshold` | int ≥ 1 | yes | — |
| `retry.breaker.cooldown` | duration | yes | — |
| `prompt` | template | yes | — |
| `resume_prompt` | template | yes | — |
| `tend_prompt` | template | yes if `tend_pr` | — |

## Identity and paths

### `name`

The loop's identity. Every row the loop writes to SQLite is keyed by it, and it names the
lock file and the log directory.

Two loops that share a `state_dir` **must** have different names. They would otherwise share
one lock, so only one of them could tick at a time, and they would read each other's issue
state.

```yaml
name: planning
```

### `repo`

The target repository in `owner/name` form. The loop reads its issues and pull requests
through the GitHub API.

This is the repository the loop **watches**. It is not this program's repository, and it is
not necessarily the checkout the agent works in — see `checkout_base_dir`.

```yaml
repo: mcgarylabs/lawndominator-monorepo
```

### `checkout_base_dir`

The primary local checkout of the target repository. The loop uses it for two things: it runs
`git fetch` there once per tick, and it is the parent every worktree is created from.

**The loop never changes its branch and never edits its files.** Leave it on your default
branch. You can keep working in it yourself while the loop runs.

When `agent.worktree` is `none`, this directory is also the agent's working directory. That
is only safe with one issue at a time.

```yaml
checkout_base_dir: /Users/seanmcgary/Code/lawndominator
```

### `worktree_dir`

The parent directory for every git worktree the loop creates. Paths under it are
deterministic:

| Kind | Path |
|---|---|
| Issue dispatch | `{worktree_dir}/{name}/issue-{n}` |
| Tend dispatch | `{worktree_dir}/{name}/pr-{n}` |

The path is stable across ticks, so a resumed run finds the branch state it left.

Each worktree is a full checkout. Nothing prunes them today: an issue the loop has touched
keeps its worktree until you run `loop reset`. Budget disk accordingly.

```yaml
worktree_dir: /Users/seanmcgary/.agent-utils/worktrees
```

### `state_dir`

Holds everything durable except the worktrees:

| Path | Contents |
|---|---|
| `{state_dir}/state.db` | SQLite: issue state, dispatches, pull request links, ticks |
| `{state_dir}/{name}.lock` | The per-loop tick lock |
| `{state_dir}/logs/{name}/` | Agent transcripts and runner logs |

The loop creates this directory `0700`, the database `0600`, and every log file `0600`. The
transcripts record everything the agent read and ran, so they are not world-readable by
design.

Give each loop its own `state_dir` unless you have a reason not to.

```yaml
state_dir: /Users/seanmcgary/.agent-utils/planning
```

### `default_branch`

The branch new worktrees start from. Worktrees are created **detached** at
`origin/{default_branch}`.

Detached is deliberate. Both reference loops make branch resolution the agent's job:
`plan-feature` may already have created the feature branch and committed design assets onto
it, and `build-feature` must check that branch out rather than re-create it over them. The
loop hands the agent a clean starting point and gets out of the way.

```yaml
default_branch: master
```

## `labels`

Labels are the entire interface between you, the agent, and the loop. The loop reads them
every tick and reconstructs its view from them, so it survives a restart with no memory.

**The loop writes a label exactly once**, in one situation: when an issue exhausts its retry
budget. Everything else is the agent's to apply. See `retry.max`.

### `labels.trigger` — required

The "go" signal. **You** apply it. It means both "start this" and "resume this", and it never
means "approved".

The loop starts an issue that carries it, and the agent's first act is to remove it and apply
`in_flight`. Re-applying it later resumes the issue's original session, so the agent still
knows what it planned and why.

Re-applying it is also the only way to recover a parked issue.

```yaml
trigger: status:ready-for-spec
```

### `labels.in_flight` — required

An agent is working on the issue now. The **agent** applies it.

The loop reads it as part of the failure path: an issue is only retried if it carries this
label, so an agent that finished its work and moved the label on is never woken by a retry.

This label is not what stops a second dispatch — a live dispatch record does that. The agent
owns its labels and may not have flipped them yet, so a label is never a concurrency guard.

```yaml
in_flight: status:speccing
```

### `labels.blocked` — required

The agent parked and needs a human answer. The **agent** applies it, and so does the loop
when it parks an issue at the retry cap.

To un-park, answer in a comment and re-apply `trigger`.

```yaml
blocked: status:needs-spec-input
```

### `labels.review` — required

The agent finished and its output is waiting for you to read. The **agent** applies it.

If `tend_pr` is true, this label is also what makes an issue eligible for tending.

```yaml
review: status:plan-ready-for-review
```

### `labels.terminal` — optional

The issue has left this loop. **You** apply it; no agent ever does.

It is optional because not every loop has one. The planning loop does — your approval of the
spec and plan. The execution loop does not: an issue leaves it when its pull request merges.
Requiring the field would force you to invent a value that changes nothing.

`labels.terminal` changes no engine behaviour. It exists so prompts can name the gate via
`{{.Labels.Terminal}}`. **To make an issue actually leave the loop, list the same label under
`veto`.** The planning example lists `status:ready-for-execution` in both places for exactly
this reason.

```yaml
terminal: status:ready-for-execution
```

### `labels.veto` — optional

Labels that make the loop skip an issue **even when it carries `trigger`**.

This is not the same as "no trigger label". An issue without `trigger` is never selected —
that is the default. `veto` handles the different case of an issue that *is* queued but must
still be skipped.

An entry ending in `*` is a prefix rule. `blocked:*` matches `blocked:design`,
`blocked:legal`, and anything else in that family. Matching ignores case.

```yaml
veto:
  - "blocked:*"
  - status:ready-for-execution
  - status:executing
  - status:ready-for-review
```

Quote any value containing `*`, or YAML may complain.

## `agent`

How to invoke `claude` for a dispatch.

### `agent.model` — required

Passed straight through as `--model`. Accepts an alias (`opus`, `sonnet`, `haiku`) or a full
model id.

There is no default on purpose. The reference loops were explicit that leaving the model
implicit silently downgrades the work and nothing fails loudly — you simply get worse output.

```yaml
model: opus
```

### `agent.effort` — optional

Passed as `--effort`. One of `low`, `medium`, `high`, `xhigh`, `max`.

Omit to use claude's own default. An invalid value fails the config load rather than the
dispatch, so a typo costs you a startup error instead of a retry slot.

```yaml
effort: high
```

### `agent.permission_mode` — optional

Passed as `--permission-mode`. One of `acceptEdits`, `auto`, `manual`, `dontAsk`, `plan`,
`bypassPermissions`. Omit to use claude's default.

**`bypassPermissions` requires `i_understand_bypass_permissions: true`.** It disables every
permission prompt, and the agent reads issue and comment text written by other people, so an
instruction hidden in a comment executes with no gate. The acknowledgement exists to make
that an explicit choice rather than something copied from an example.

```yaml
permission_mode: bypassPermissions
```

### `agent.worktree` — required

| Value | Behaviour |
|---|---|
| `per_issue` | Each issue and each tended pull request gets its own worktree |
| `none` | The agent runs directly in `checkout_base_dir` |

Use `none` only when you are certain a single agent runs at a time. There are no concurrency
caps, so every eligible issue dispatches at once, and `none` would put all of them in one
working tree.

```yaml
worktree: per_issue
```

### `agent.max_budget_usd` — optional

Passed as `--max-budget-usd`. Omit or set `0` for no limit.

This is a per-dispatch ceiling, not a per-issue or per-day one. An issue that is retried
three times can spend up to three times this amount.

```yaml
max_budget_usd: 25
```

### `agent.timeout` — required

How long one dispatch may run. A Go duration string: `90s`, `30m`, `3h`.

On expiry the loop signals the agent's whole process group, not just the direct child, so a
dev server or watcher the agent started does not survive it. The dispatch is recorded failed
and becomes eligible for retry.

Set it well above a realistic run. A timeout mid-way through useful work wastes everything
after the agent's last push.

```yaml
timeout: 3h
```

### `i_understand_bypass_permissions`

Top-level, not under `agent`. Required to be `true` only when
`agent.permission_mode: bypassPermissions`; ignored otherwise.

Setting it asserts you understand that the agent executes instructions found in issue and
comment text, and that you are pointing this loop at a repository whose issue and pull
request population you trust.

```yaml
i_understand_bypass_permissions: true
```

## `tend_pr`

Whether the loop rebases stale pull requests. Default `false`.

When true, on every tick, for each issue carrying `labels.review`:

1. Find the open pull request whose body closes it (`Closes #N`, `Fixes #N`, `Resolves #N`).
2. Ask the GitHub API how far behind its base it is.
3. Dispatch a tend agent **only** if it is behind by at least one commit.

A current pull request costs one API call and produces nothing — no comment, no push, no
agent. Silence is the correct output.

Three safeguards apply:

- **Only a trusted pull request is ever linked.** Its head branch must live in the target
  repository and its author must be an `OWNER`, `MEMBER`, or `COLLABORATOR`. A fork pull
  request claiming `Closes #7` cannot hijack the link, because tending checks the head branch
  out and runs an agent inside it.
- **An issue with a live agent is never tended**, so a rebase cannot force-push a branch its
  own build agent is committing to.
- **Tending never changes a label**, and each tend run gets a fresh session, because a rebase
  is idempotent and needs no memory of an earlier one.

**Set this `false` for a planning loop.** `plan-feature` opens a design draft pull request
whose body also says `Closes #N`, so a planning loop with tending on would force-push a draft
you are in the middle of reading.

Version 1 rebases only. It does not reply to review feedback.

```yaml
tend_pr: true
```

## `retry`

What happens when a dispatch fails. A dispatch fails if its process dies without recording an
outcome, or if `claude` exits non-zero, or if its stream reports an API error.

### `retry.max`

How many times one issue may be retried before the loop gives up. `0` means never retry.

**At the cap the loop performs its one and only GitHub write.** It posts a comment saying the
retries are exhausted, then removes `in_flight` and `trigger` and applies `blocked`.

It removes `trigger` deliberately: leaving it would let the next tick resume the issue
immediately and the park would stop nothing. Re-apply `trigger` yourself to resume, which
also resets the retry budget.

The counter also resets on any successful dispatch, so an issue that fails three times over
its lifetime with successes in between is not parked on its next single failure.

```yaml
max: 3
```

### `retry.backoff_ticks`

How many ticks to wait before each retry. One entry per retry, so the list must be at least
as long as `retry.max`.

`[0, 1, 2]` means: retry 1 on the tick the failure is noticed, retry 2 at least 1 tick later,
retry 3 at least 2 ticks after that. `[0, 0, 0]` retries as fast as the cron interval allows.

Waiting costs nothing. A deferred retry stays pending in the database, so a tick that
declines to act changes nothing and the next tick sees the same failure.

```yaml
backoff_ticks: [0, 1, 2]
```

### `retry.breaker.orphan_threshold`

How many issues must fail in one tick before the loop treats it as a platform problem rather
than several unrelated crashes. Must be at least 1.

When it trips, the loop **skips every dispatch that tick** — retries, new starts, and tends
alike — and starts a cooldown. Parks still happen: an issue already at its cap still gets its
comment.

One issue misbehaving is noise. Two at once, in the same window, is the platform.

```yaml
orphan_threshold: 2
```

### `retry.breaker.cooldown`

How long to dispatch nothing after the breaker trips. A Go duration string.

Ticks still run during a cooldown; they simply make no decisions. Set it long enough for a
platform incident to clear — the default in both examples is `30m`.

```yaml
cooldown: 30m
```

## Prompts

Three Go `text/template` strings. Whichever applies is rendered and passed to `claude` as a
single positional argument, so no amount of text in them can become a separate flag.

All three are parsed when the config loads. A typo like `{{.Issue.Titel}}` fails at startup
rather than inside a detached process three hours later.

| Field | Rendered when |
|---|---|
| `prompt` | An issue starts for the first time, or restarts because its previous attempt never created a session |
| `resume_prompt` | An issue resumes an existing session |
| `tend_prompt` | A stale pull request is rebased |

`prompt` and `resume_prompt` are always required. `tend_prompt` is required only when
`tend_pr` is true.

`resume_prompt` can be short. The agent is in the same conversation and already knows what it
did; it needs to be told what changed, not told everything again.

Write these prompts as the agent's whole instruction set. The loop enforces nothing about the
agent's behaviour, so every rule — how to stop, which labels to apply, never to merge, never
to apply the terminal label — lives here. The two example files carry the full set.

```yaml
prompt: |
  Run the /plan-feature skill for GitHub issue #{{.Issue.Number}}, repo {{.Repo}}.
  As your first action, remove {{.Labels.Trigger}} and add {{.Labels.InFlight}}.
  ...
```

## Template variables

Available to all three prompts.

| Variable | Type | Notes |
|---|---|---|
| `{{.Repo}}` | string | `owner/name` |
| `{{.Loop}}` | string | The `name` field |
| `{{.SessionID}}` | string | This dispatch's claude session id |
| `{{.Worktree}}` | string | Absolute path the agent runs in |
| `{{.Issue.Number}}` | int | |
| `{{.Issue.Title}}` | string | As of dispatch time |
| `{{.PR.Number}}` | int | Tend dispatches only; `0` otherwise |
| `{{.PR.HeadRef}}` | string | Tend dispatches only |
| `{{.PR.BaseRef}}` | string | Tend dispatches only |
| `{{.PR.BehindBy}}` | int | Commits the head lacks from the base |
| `{{.Labels.Trigger}}` | string | |
| `{{.Labels.InFlight}}` | string | |
| `{{.Labels.Blocked}}` | string | |
| `{{.Labels.Review}}` | string | |
| `{{.Labels.Terminal}}` | string | Empty when `labels.terminal` is unset |

Referring to a field that does not exist is a load-time error. Referring to one that exists
but is empty is not — `{{.Labels.Terminal}}` in a config that omits `labels.terminal` renders
an empty string silently. Name labels through these variables rather than hardcoding them, so
a renamed label updates every prompt at once.

## Validation rules

Beyond the required fields in the quick reference:

| Rule | Message |
|---|---|
| `repo` must contain exactly one `/`, both parts non-empty | `repo must be in owner/name form` |
| `agent.worktree` ∈ {`per_issue`, `none`} | `agent.worktree must be …` |
| `agent.effort` ∈ {`low`,`medium`,`high`,`xhigh`,`max`} or empty | `… is not a valid effort level` |
| `agent.permission_mode` is a real claude mode or empty | `… is not a valid claude permission mode` |
| `bypassPermissions` needs the acknowledgement | `set i_understand_bypass_permissions: true` |
| `agent.timeout` > 0 | `agent.timeout must be greater than zero` |
| `retry.max` ≥ 0 | `retry.max must not be negative` |
| `len(retry.backoff_ticks)` ≥ `retry.max` | `it needs one entry per retry` |
| `retry.breaker.orphan_threshold` ≥ 1 | `must be at least 1` |
| `retry.breaker.cooldown` > 0 | `must be greater than zero` |
| `tend_prompt` non-empty when `tend_pr` | `tend_prompt is required when tend_pr is true` |
| All three prompts parse as templates | `prompt: template: …` |
| No unknown keys anywhere | `field … not found in type config.Config` |

Every failure is reported at once, so one load tells you everything wrong with the file.

To check a file, run:

```bash
agent-utils loop status --config planning.yaml
```

`status` loads and validates the config before it does anything else, so a bad file fails
there and nothing is dispatched. It makes no change to the repository, the state, or the
worktrees.

It is not a pure config check, though: it also needs `GITHUB_TOKEN` and network access,
because it lists the repository's open issues to render its view. A config error and an
authentication error look different, so you can tell them apart — a config error names the
offending field.
