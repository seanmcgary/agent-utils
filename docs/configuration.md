# Loop configuration reference

One YAML file defines one loop. `agent-utils project loop tick --config <file>` reads it on every
run.

The parser is **strict**: an unknown key is an error, not a warning. A misspelled key fails
the load rather than being silently ignored. Every validation error for a file is reported
together, in a stable order.

`examples/planning.yaml` and `examples/execution.yaml` are complete working files. Read this
reference for what each field means; read those for a shape to copy.

## Where configuration files live

Configuration files live in `.agent-utils/configs/`, one YAML file per loop.

A loop's **name** is the `name:` field **inside** the file, not the file name. The same value
keys the loop's state directory, its lock file, and every row it writes, so selecting by it on
the command line selects the same thing the loop calls itself.

```
.agent-utils/
└── configs/
    ├── planning.yaml     name: planning   -> agent-utils project loop tick --name planning
    └── execution.yaml    name: execution  -> agent-utils project loop tick --name execution
```

File names are yours to choose; only `.yaml` and `.yml` are read. `backlog-planning.yaml`
declaring `name: planning` is still selected as `planning`.

**Every loop needs a unique name.** Two files declaring the same one would share a state
directory and a lock while looking like separate loops, so `--name` refuses to guess between
them and `list` prints a warning.

`agent-utils project list` prints what it finds:

```
$ agent-utils project list
/Users/you/.agent-utils/configs

NAME                 FILE                     REPO                                 STATUS
execution            execution.yaml           mcgarylabs/lawndominator-monorepo    ok
planning             backlog-planning.yaml    mcgarylabs/lawndominator-monorepo    ok
```

`NAME` is the `name:` field; `FILE` is where it came from. They are listed separately because
they need not agree.

A file that fails to load is still listed, marked `INVALID`, with its error printed below the
table. A configuration that silently does not appear is harder to debug than one that appears
broken.

### How the directory is found

In order:

1. `$AGENT_UTILS_DIR`, when set. If it is set but is not a directory, that is an error rather
   than a silent fallback.
2. A `.agent-utils` directory in the working directory or any parent, the way git finds
   `.git`. This is what makes the tool work from a subdirectory of a project.

There is deliberately **no fallback to `$HOME/.agent-utils`**. Configurations are
project-local: running in an unrelated directory reports that there is no project there
rather than silently adopting some other project's loops. Note that `$HOME/.agent-utils`
does exist on any machine that has used the tool, because the cross-project registry and the
one state database both live there — which is exactly why falling back to it was wrong.

If no directory is found, every command that needs a configuration fails with an error naming
where it looked. A cron entry should pass `--config` with an absolute path, which needs no
discovery at all.

### The machine-wide directory

`$AGENT_UTILS_HOME` names the machine-wide `.agent-utils` directory itself. It defaults to
`~/.agent-utils`, and it holds the registry, the canonical state database, and the machine-wide
`config.yaml` the webhook listener reads (see [Two different `config.yaml` files](#two-different-configyaml-files)
below). It is not a replacement for `$HOME`: nothing else the tool or the agent reads moves
with it.

Do not confuse it with `$AGENT_UTILS_DIR`: `AGENT_UTILS_HOME` names the ONE machine-wide
directory, and `AGENT_UTILS_DIR` names ONE project's `.agent-utils` directory.

| Variable | Names | Default |
|---|---|---|
| `AGENT_UTILS_HOME` | The machine-wide directory: the registry, the state database, and `config.yaml` | `~/.agent-utils` |
| `AGENT_UTILS_DIR` | One project's `.agent-utils` directory | Found by walking up from the working directory |

Pointing `AGENT_UTILS_HOME` at a path that exists and is not a directory is an error rather
than a silent fallback. Falling back would write this machine's state somewhere you did not
ask for, and the mistake would surface much later as missing state.

Set it to run a test, or a second machine-wide installation, against its own state. Moving
`$HOME` instead would also move the git and ssh configuration the agent still needs.

It also holds `env`, the shell-sourceable file `GITHUB_TOKEN` is read from — by the webhook
listener on every delivery, and by the cron entry in the README. Write it with:

```bash
agent-utils config token
```

The command looks for a token you already have before it asks for one. `$GITHUB_TOKEN`, if it
is set in the environment, is used as it stands and nothing is prompted for. Otherwise, if `gh`
is installed and logged in, the token `gh auth token` reports becomes the prompt's **default**,
which Enter accepts and typing anything else replaces. Only when neither turns one up does it
ask outright. A discovered token is never echoed, not even as a default: it is shown as a
masked fingerprint (`ghp_…AB12`, the last four characters) so you can tell which credential is
about to be stored without the value landing in your scrollback or a screen share. A `gh` that
is absent, not logged in, or slow to answer is simply "no token found" — never an error, and
never gh's own stderr reported as this program's.

It writes the file atomically at mode `0600`, creating `~/.agent-utils` if it does not exist;
anything else already in the file is preserved, since cron sources it and it may hold unrelated
exports. It never takes the token as a flag or an argument — a value on the command line shows
up in `ps` output and in shell history — but it does read one piped to it (`echo "$TOKEN" |
agent-utils config token`), for a scripted machine build, and a piped value wins over
`$GITHUB_TOKEN` because it was given explicitly. With no terminal, nothing piped, and no
`$GITHUB_TOKEN`, it refuses rather than hanging. Writing the file by hand still works, as long
as the mode is `0600` and the file is owned by the account the listener runs as, which is what
the reader enforces:

```bash
install -m 600 /dev/null ~/.agent-utils/env
echo 'export GITHUB_TOKEN=ghp_...' >> ~/.agent-utils/env
```

### Two different `config.yaml` files

`~/.agent-utils/config.yaml` (the machine-wide settings file, `AGENT_UTILS_HOME`) and
`<project>/.agent-utils/config.yaml` (one project's descriptor: its name and its UUID) are
**different files that happen to share a base name**, distinguished only by which directory
holds them. The machine-wide one is never described by this document — it is not a loop
configuration — and is read and written entirely through `agent-utils config`.

That name collision is not a coincidence to shrug off: it is exactly why `agent-utils project
init` refuses to run inside the machine-wide directory. Without that refusal, `cd ~ &&
agent-utils project init` would mint a *project* descriptor at `~/.agent-utils/config.yaml`,
overwriting the machine-wide settings file — or, run the other way, pointing `$AGENT_UTILS_HOME`
at a project's `.agent-utils` directory would hand a project descriptor to code expecting the
machine-wide settings shape. Both `internal/settings` (Load and Save) and `project init` guard
against this explicitly, by probing the file's shape before trusting it.

### How a configuration is chosen

`--config` and `--name` are alternatives. Passing both is an error rather than a silent
preference, so a mistake in a cron entry surfaces immediately instead of after it has been
running against the wrong loop.

| You run | What happens |
|---|---|
| `agent-utils project loop tick --config path/to/file.yaml` | That exact file. Nothing is scanned. |
| `agent-utils project loop tick --name planning` | The file declaring `name: planning` |
| `agent-utils project loop tick`, one config present | That one |
| `agent-utils project loop tick`, several present, terminal | Prompts you to pick one |
| `agent-utils project loop tick`, several present, **not** a terminal | Fails, listing the names |
| `agent-utils project loop tick --config … --name …` | Fails: pass only one |
| `--name x` when two files declare `name: x` | Fails, naming both files |

That last row matters. A prompt in a cron job would wait for input that never arrives, so the
prompt appears only when stdin is a real terminal. **cron should use `--config` with an
absolute path** — it depends on no working directory and can never prompt.

## Contents

- [Quick reference](#quick-reference)
- [Identity and paths](#identity-and-paths)
- [`labels`](#labels)
- [Agent overrides from labels](#agent-overrides-from-labels)
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
| `checkout_base_dir` | path, relative to the project root | yes | — |
| `worktree_dir` | path, relative to the project root | yes | — |
| `state_dir` | path | no | `<project>/.agent-utils/state/<name>` |
| `default_branch` | string | yes | — |
| `labels.trigger` | string | yes | — |
| `labels.in_flight` | string | yes | — |
| `labels.blocked` | string | yes | — |
| `labels.review` | string | yes | — |
| `labels.terminal` | string | no | empty |
| `labels.veto` | list of string | no | empty |
| `agent.harness` | enum | no | `claude` |
| `agent.model` | string | yes | — |
| `agent.effort` | enum | no | claude's own default |
| `agent.permission_mode` | enum | no | claude's own default |
| `agent.worktree` | enum | yes | — |
| `agent.max_budget_usd` | number | no | `0`, meaning no limit |
| `agent.background_tasks` | bool | no | `false` |
| `agent.timeout` | duration | yes | — |
| `i_understand_bypass_permissions` | bool | only with `bypassPermissions` | `false` |
| `tend_pr` | bool | no | `false` |
| `cleanup_closed_pr` | bool | no | `true` |
| `retry.max` | int | no | `0`, meaning never retry |
| `retry.backoff` | list of duration | yes if `retry.max > 0` | empty |
| `retry.backoff_ticks` (removed) | — | — | — |
| `retry.breaker.orphan_threshold` | int ≥ 1 | yes | — |
| `retry.breaker.cooldown` | duration | yes | — |
| `prompt` | template | yes | — |
| `resume_prompt` | template | yes | — |
| `tend_prompt` | template | yes if `tend_pr` | — |

## Identity and paths

### `name`

The loop's identity, and how it is selected on the command line with `--name`. Every row the
loop writes to SQLite is keyed by it, and it names the state directory, the lock file, and the
log directory. It is independent of the file name.

It must be unique across the configurations in one directory.

Two loops that share a `state_dir` **must** have different names. They would otherwise share
one lock, so only one of them could tick at a time, and they would read each other's issue
state. The name is also what separates their rows when a database from the old per-loop
layout is imported, so two loops sharing one name would arrive in the canonical database as a
single loop.

That rule crosses projects too. Two **projects** whose loops share both a `state_dir` and a
loop name cannot both own the rows in that one old database. The first project to run claims
them, and every later command for the second reports which project claimed the file and stops.
Give one of them its own `state_dir`, or rename one of the loops.

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

**A relative path is resolved against the project root** — the directory containing
`.agent-utils` — never against the working directory of whatever ran the command. That is
what makes `.` correct, and what the wizard writes: the same file then works for a CLI run
from anywhere, for `--name <project>` typed in another directory, and for the listener
daemon, whose working directory is `~/.agent-utils`. A leading `~` is expanded. An absolute
path is used as written.

A configuration outside any `.agent-utils` directory has no project root, so a relative path
there is an error naming the file; use an absolute path.

```yaml
checkout_base_dir: .   # the project itself; an absolute path works too
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

It is resolved exactly like `checkout_base_dir`: a relative path against the project root, a
leading `~` expanded, an absolute path as written. The wizard defaults it to
`.agent-utils/worktrees`, so each project's worktrees land beside that project rather than in
a shared directory under your home.

Because the default is inside the repository, the worktrees show up as untracked files there.
Add `.agent-utils/worktrees/` to the repository's `.gitignore`, or point this at a path
outside the checkout, if that matters to you.

```yaml
worktree_dir: .agent-utils/worktrees   # relative to the project root; an absolute path works too
```

### `state_dir`

**Optional.** When omitted it defaults to `<project>/.agent-utils/state/<name>`, derived from
the configuration file's own location.

Leave it unset. The default puts each loop's lock and logs beside the project they belong to,
and it moves with the project. A shared absolute path copied between two projects makes both
of them take one tick lock and write into one log tree, so their ticks serialize and their
transcripts mingle. Set it only to deliberately place those files elsewhere. A leading `~` is
expanded.

A configuration outside any `.agent-utils` directory has no project to derive from, so
`state_dir` is required there and its absence is an error naming the file.

It holds two things:

| Path | Contents |
|---|---|
| `{state_dir}/{name}.lock` | The per-loop tick lock |
| `{state_dir}/logs/{name}/` | Agent transcripts and runner logs |

**The database is not one of them.** Issue state, dispatches, pull request links, ticks and
webhook registrations live in one canonical database for the machine, at
`$HOME/.agent-utils/state.db`. Every row carries the UUID of the project that owns it, so one
file holds every project without any of them seeing another's state. That is why a `state_dir`
shared between two projects no longer mixes their state — only their lock and their logs.

Webhook registrations are keyed by project and repository, and hold the hook id GitHub
assigned. `agent-utils project register-webhook` writes the row once GitHub confirms, and
`agent-utils project deregister-webhook` deletes the hook by that id and removes the row.
The id is what makes a hook findable after `webhook.url` changes; see
[Webhooks](../README.md#webhooks).

The same directory holds `registry.json`, which records which projects have been used so
`agent-utils list` can list them. It is an index only: deleting it loses the list and nothing
else. Every project's configuration stays in its own directory.

The loop creates this directory `0700` and every log file `0600`. The machine-wide directory
is `0700`, and the canonical database and its `-wal` and `-shm` sidecars are `0600`. The
transcripts record everything the agent read and ran, and the database carries claude session
identifiers, so none of it is world-readable by design.

Two loops may share a `state_dir` as long as their names differ: the lock file and the log
directory are both named after the loop, and their rows are keyed by the loop name too. Give
each loop its own anyway unless you have a reason not to — one directory per loop is easier
to read and easier to delete.

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

**The loop writes a label in two situations**, and never in any other. It applies `blocked` when
an issue exhausts its retry budget — see `retry.max`. And it applies `trigger` to a sub-issue of
an epic that a closing sibling unblocked — see [Epics](../README.md#epics), which also explains
why only one loop of a project ever does that. Everything else is the agent's to apply.

### `labels.trigger` — required

The "go" signal. **You** apply it — and so does the epic sweep, for the loop at the front of the
pipeline, when a sub-issue's blockers all close. It means both "start this" and "resume this",
and it never means "approved".

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

## Agent overrides from labels

Three label prefixes let a label on the issue override one setting from `agent`, for that
issue's dispatch only. They are always active. No configuration field turns them on, and none
turns them off.

| Label prefix | Overrides | Example |
|---|---|---|
| `model:` | `agent.model` | `model:claude-opus-5` |
| `harness:` | `agent.harness` | `harness:pi` |
| `effort:` | `agent.effort` | `effort:high` |

See [`agent.model`](#agentmodel--required), [`agent.harness`](#agentharness--optional), and
[`agent.effort`](#agenteffort--optional) for what each setting does.

### Case

The prefix ignores case: `Model:`, `MODEL:`, and `model:` are the same label.

The value's case depends on what it names:

- `harness:` and `effort:` name one of a fixed, closed list of values, so the value is lowered.
  `harness:PI` and `harness:pi` are the same override.
- `model:` names a model identifier, which is case-sensitive, so its value is kept exactly as
  written.

### Rejected labels

The loop refuses a label instead of guessing what it meant. Each of these makes the label
invalid:

- The value is empty. `model:` names no model.
- The value contains a space or any other whitespace character, including a zero-width space
  or a word joiner.
- The value starts with `-`. The value becomes one argument in the list the agent binary is
  run with, and an argument starting with `-` is read as a flag, not a value.
- The value contains a character outside `A-Z`, `a-z`, `0-9`, `.`, `_`, `-`, or `/` — and even
  among those, the FIRST character must be one of `A-Z`, `a-z`, `0-9`, `.`, or `_`: a leading
  `-` or a leading `/` is rejected, though either may appear later in the value.
- Two labels on the same issue carry the same prefix. Two `model:` labels is an error, not a
  choice between them — nothing decides which one wins.
- `harness:` names anything other than `claude` or `pi`.
- `effort:` names anything other than `low`, `medium`, `high`, `xhigh`, or `max`.

A `harness:` label naming the loop's OWN configured harness is a no-op: it is accepted, but
changes nothing.

A `harness:` label that would switch the EFFECTIVE harness to `pi` is refused when the loop's
own configuration sets `agent.permission_mode` or a nonzero `agent.max_budget_usd`. The `pi`
harness accepts neither flag — see [`agent.harness`](#agentharness--optional) — so switching TO
`pi` would silently run the dispatch with no permission mode and no cost ceiling. The rule is
directional: switching to `claude` is never refused, however the loop is configured, because
`claude` only ever adds a bound `pi` did not enforce in the first place — it can never drop one.

On a loop that configures NEITHER `agent.permission_mode` nor `agent.max_budget_usd` — the
default — a `harness:` label changes which binary runs with no additional gate at all. This is
accepted and deliberate, not an oversight: the guard exists to protect two specific settings, and
a loop that never set them has nothing for the guard to protect.

### What an invalid label does

An invalid label does not fail silently and does not fall back to the configured value. It
stops the issue: the loop sets a `stopped` flag and records the label's error as the reason. No
agent runs for that issue until the flag is cleared.

The loop writes nothing to GitHub for this. The reason shows in `agent-utils sessions list` and
in `agent-utils project loop status`.

Clearing the flag needs `agent-utils sessions resume`, and only on the machine running the loop.
A label applied from GitHub can therefore halt an issue that only a local operator can restart —
fix the label, or remove it, then run `sessions resume`. See
[`README.md`](../README.md#sessions).

### Scope

A valid override applies to every dispatch that works the issue itself: its first run, every
resume, and every retry. It does not apply to a tend dispatch — the run that rebases a pull
request against its base branch — because a tend dispatch is not the issue's own work, and a
loop configures how it runs once, for every issue alike.

### Who controls this

Anyone who can add a label to an issue chooses that issue's model and harness. There is no
separate permission for it. Point a loop only at a repository whose issue and label population
you trust — see [Security](../README.md#security).

## `agent`

How to invoke the agent for a dispatch. The `agent.harness` field selects which agent
program runs; the rest of the `agent` fields map onto that program's flags (see below).

### `agent.harness` — optional

| Value | Agent run |
|---|---|
| `claude` | Runs `claude -p`, Stream stats stream-json. Default. |
| `pi` | Runs `pi -p --mode json`. |

Choose `pi` to use a model that `claude` does not offer. For a `pi` harness:

- `agent.model` must be a `provider/id` or a pattern that pi resolves (for example
  `anthropic/claude-sonnet-4-5`, `openrouter/...`), not a claude alias like `opus`.
- `agent.effort` maps to pi's `--thinking` level. `low`, `medium`, `high`, `xhigh`, `max`
  are valid for both harnesses.
- `agent.permission_mode` is claude-only. A `pi` config must not set it; the loop rejects it.
- `agent.background_tasks` is claude-only. A `pi` config must not set it; the loop rejects it.
- `agent.max_budget_usd` is a claude-only ceiling. For `pi` it is accepted but has no
  effect, because pi exposes no cost-ceiling flag.
- A `pi` run's session resumes by the same session id, because pi's `--session-id`
  creates a session when new and resumes it when known.

A `harness:` label on an issue can override this setting for that issue's dispatch. See
[Agent overrides from labels](#agent-overrides-from-labels).

### `agent.model` — required

Passed straight through as `--model`. Accepts an alias (`opus`, `sonnet`, `haiku`) or a full
model id.

There is no default on purpose. The reference loops were explicit that leaving the model
implicit silently downgrades the work and nothing fails loudly — you simply get worse output.

```yaml
model: opus
```

A `model:` label on an issue can override this setting for that issue's dispatch. See
[Agent overrides from labels](#agent-overrides-from-labels).

### `agent.effort` — optional

Passed as `--effort`. One of `low`, `medium`, `high`, `xhigh`, `max`.

Omit to use claude's own default. An invalid value fails the config load rather than the
dispatch, so a typo costs you a startup error instead of a retry slot.

```yaml
effort: high
```

An `effort:` label on an issue can override this setting for that issue's dispatch. See
[Agent overrides from labels](#agent-overrides-from-labels).

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

Passed to the agent as `--max-budget-usd`. The dispatch is stopped when the session's cost
exceeds it.

**`0`, or omitting the field, disables the cap.** The flag is then not passed at all and the
dispatch runs with no cost ceiling — bounded only by `agent.timeout`. That is deliberate and
supported; set it knowingly.

A negative value is rejected at load. It would otherwise behave exactly like `0`, so `-25`
typed for `25` would have run uncapped and said nothing.

This is a per-dispatch ceiling, not a per-issue or per-day one. An issue that is retried
three times can spend up to three times this amount.

```yaml
max_budget_usd: 25   # or 0 for no cap
```

### `agent.background_tasks` — optional

Whether claude may run its background tasks. **Default `false`, and you almost certainly want
to leave it there.**

Claude backgrounds a subagent unless told otherwise, and `claude -p` waits only a bounded time
for background work at the end of a run before it **kills that work and exits zero**. An agent
that fanned out to subagents could therefore have them killed mid-edit while the transcript's
last word was the agent describing what it had just delegated — and the loop, seeing exit zero
and a success result, recorded the dispatch succeeded and retired the issue. The work was
never picked up again.

With the default, the loop sets `CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1` and a subagent becomes
an ordinary blocking tool call. **Fan-out is not lost**: several subagents dispatched in one
turn still run concurrently. What is lost is *cross-turn* backgrounding — dispatching a subagent
and continuing to work while it runs — which for an unattended loop is a liability anyway, since
it lets two agents edit the same file with neither aware of the other.

The loop also sets `CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS=0` in both cases, so `agent.timeout`
is the only deadline for a dispatch. Claude's own ten-minute background ceiling is otherwise a
second, shorter, invisible deadline that silently preempts the one you configured.

Two further effects of the default, neither load-bearing for a loop: the Bash tool loses its
`run_in_background` parameter (shell `&` still works, and the process group is swept at the
end of a dispatch either way), and observer agents are off.

Whatever this is set to, a run whose background work is terminated is recorded **failed**, not
succeeded, so the issue is marked for retry and the next tick resumes the session and picks the
abandoned tasks back up.

Claude-only. A `pi` config must not set it; the loop rejects it.

```yaml
background_tasks: false   # true only if you know why you want it
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

## `cleanup_closed_pr`

Whether a closed pull request's worktrees are removed. Default `true`.

When GitHub reports a pull request closed — merged or not — the loop removes that pull
request's `pr-<N>` worktree, and the `issue-<M>` worktree of the issue it closes, as soon as
neither has a live dispatch. Nothing else in this program removes a worktree, and one of a large
repository is easily hundreds of megabytes, so a loop left to run would fill the disk.

It defaults on because that is the useful behavior, and it exists as a field because the action
is destructive and starts from a webhook. An operator who wants the loop without the deletion
should not have to rebuild to get there.

Two limits apply, and neither is configurable:

- The `Closes #M` link is honoured only for a **trusted** pull request — one whose head is in
  this repository and whose author is an `OWNER`, `MEMBER`, or `COLLABORATOR`. An outside
  contributor cannot name someone else's issue and have its work deleted. The `pr-<N>` worktree
  is removed either way, because that number comes from the delivery rather than from body text.
- The live-dispatch guard protects work **in progress**, not uncommitted or unpushed work in an
  idle worktree. That is removed too. The log names any worktree that had uncommitted changes or
  unpushed commits when it went.

```yaml
cleanup_closed_pr: false   # keep the worktrees
```

## `tend_pr`

Whether the loop rebases stale pull requests. Default `false`.

When true, for each issue carrying `labels.review`, on every tick and also when a merge sweeps
the loop (see below):

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

**Two things dispatch a tend agent.** A delivery for one issue — the issue carries
`labels.review`, its linked pull request is behind its base, and the delivery named that issue.
And a merge into `default_branch`: GitHub sends a `pull_request` delivery with `merged: true`,
and because the merge is what made every other pull request stale while naming none of them,
that one delivery sweeps the loop. Every issue carrying `labels.review` whose linked pull
request targets `default_branch` and is now behind gets a tend dispatch.

A pull request targeting any other branch is left alone. A merge into `master` says nothing
about a branch based on `release/1.0`.

The sweep dispatches **tend agents only**. It never starts, resumes, or retries an issue agent,
and it never parks an issue. A merge is a reason to rebase; it is not a reason to start work.

A sweep waits about a minute before it runs, so a merge train produces one sweep rather than one
per merge, and it dispatches at most ten rebases. If more pull requests are behind than that,
the rest are named in the log line and wait for the next merge.

**A closed pull request has its worktrees removed.** This is separate from `tend_pr` and is not
gated by it; `cleanup_closed_pr` turns it off. When GitHub reports a pull request closed — merged or not — the loop removes that
pull request's `pr-<N>` worktree, and the `issue-<M>` worktree of the issue it closes, as soon
as neither has a live dispatch. A worktree of a large repository is easily hundreds of
megabytes, and nothing removed one before.

Two limits apply. The issue link is honoured only for a trusted pull request, so an outside
contributor cannot name someone else's issue in a `Closes #M` body and have its work deleted.
And the live-dispatch guard protects work in progress, not uncommitted or unpushed work in an
idle worktree — that is removed too, with a warning naming the worktree in the log.

**A sweep does not replace a periodic tick.** `agent-utils project loop tick` is still the only
full reconcile: it is what retires a dead runner for an issue no delivery names, and what finds
a pull request that fell behind for any reason other than a merge. Schedule it.

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

**At the cap the loop makes one of its two non-agent GitHub writes.** It posts a comment saying
the retries are exhausted, then removes `in_flight` and `trigger` and applies `blocked`.

It removes `trigger` deliberately: leaving it would let the next tick resume the issue
immediately and the park would stop nothing. Re-apply `trigger` yourself to resume, which
also resets the retry budget.

The counter also resets on any successful dispatch, so an issue that fails three times over
its lifetime with successes in between is not parked on its next single failure.

```yaml
max: 3
```

### `retry.backoff`

How long to wait before each retry. One entry per retry, so the list must be at least as long
as `retry.max`. Entry 0 is the wait before the first retry.

`[0s, 15m, 30m]` means: retry 1 as soon as the failure is noticed, retry 2 at least 15 minutes
later, retry 3 at least 30 minutes after that. `[0s, 0s, 0s]` retries as fast as the loop is
ticked.

The computed deadline is a wall-clock timestamp stored on the issue's row, not a tick count.
That is what makes it correct under both drivers: `loop tick` running on a cron interval and
the webhook daemon ticking on delivery both read the same stored deadline and compare it
against the current time, so a retry due in 15 minutes waits 15 minutes regardless of which
one next calls tick. Waiting costs nothing — a deferred retry stays pending in the database,
so a tick that declines to act changes nothing and the next tick, from either driver, sees the
same failure.

```yaml
backoff: [0s, 15m, 30m]
```

#### `retry.backoff_ticks` (removed)

A tick was a fixed interval only under cron; the webhook daemon can tick a loop at any moment,
so a count of ticks no longer names a stable wait. A config that still sets it fails to load
with an error naming `retry.backoff` as the replacement. Multiply the old tick count by the
cron interval you were running to get a starting duration.

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
| `agent.max_budget_usd` ≥ 0 (`0` means no cap) | `agent.max_budget_usd must not be negative` |
| `agent.background_tasks` unset for `harness: pi` | `agent.background_tasks is claude-only` |
| `agent.timeout` > 0 | `agent.timeout must be greater than zero` |
| `retry.max` ≥ 0 | `retry.max must not be negative` |
| `len(retry.backoff)` ≥ `retry.max` | `it needs one entry per retry` |
| `retry.backoff_ticks` must be empty | `is no longer supported; ... replace it with retry.backoff` |
| `retry.breaker.orphan_threshold` ≥ 1 | `must be at least 1` |
| `retry.breaker.cooldown` > 0 | `must be greater than zero` |
| `checkout_base_dir`/`worktree_dir` relative needs a project root | `… so there is no project root to resolve it against` |
| `tend_prompt` non-empty when `tend_pr` | `tend_prompt is required when tend_pr is true` |
| All three prompts parse as templates | `prompt: template: …` |
| No unknown keys anywhere | `field … not found in type config.Config` |

Every failure is reported at once, so one load tells you everything wrong with the file.

To check every configuration in a project without touching GitHub:

```bash
agent-utils project list      # loads and validates each one; INVALID rows carry the reason
agent-utils project status    # the same, plus each loop's state directory and tick history
```

Both read only local state, so they need no token and work offline. A file that fails to load
is reported as `INVALID` with its error rather than being skipped.

`agent-utils project loop status --name <loop>` validates too, but it also lists the
repository's open issues, so it needs `GITHUB_TOKEN` (see [The machine-wide
directory](#the-machine-wide-directory), or `agent-utils config token`) and network access. A config error names
the offending field, so it is easy to tell from an authentication error.
