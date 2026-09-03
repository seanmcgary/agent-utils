# Loop configuration reference

One YAML file defines one loop. `agent-utils project loop tick --config <file>` reads it on every
run.

The parser is **strict**: an unknown key is an error, not a warning. A misspelled key fails
the load rather than being silently ignored. Every validation error for a file is reported
together, in a stable order.

`examples/planning.yaml`, `examples/execution.yaml`, `examples/pr-review.yaml` and
`examples/exec-pr-review-findings.yaml` are complete working files, and together they are one
chain: each loop's `trigger` is the loop before its `terminal`. Read this reference for what each
field means; read those for a shape to copy. `examples/pi.yaml` is the same loop as
`execution.yaml` under the `pi` harness, not a fifth stage.

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
- [Project settings: `tend` and `epic`](#project-settings-tend-and-epic)
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
| `labels.terminal` | string | no | empty |
| `labels.veto` | list of string | no | empty |
| `agent.harness` | enum | no | `claude` |
| `agent.model` | string | yes | — |
| `agent.effort` | enum | no | claude's own default |
| `agent.permission_mode` | enum | no | claude's own default |
| `agent.worktree` | enum | yes | — |
| `agent.max_budget_usd` | number | no | `0`, meaning no limit |
| `agent.background_tasks` | bool | no | `false` |
| `agent.timeout` | duration | no | `24h` |
| `i_understand_bypass_permissions` | bool | only with `bypassPermissions` | `false` |
| `cleanup_closed_pr` | bool | no | `true` |
| `retry.max` | int | no | `0`, meaning never retry |
| `retry.backoff` | list of duration | yes if `retry.max > 0` | empty |
| `retry.backoff_ticks` (removed) | — | — | — |
| `retry.breaker.orphan_threshold` | int ≥ 1 | yes | — |
| `retry.breaker.cooldown` | duration | yes | — |
| `prompt` | template | yes | — |
| `resume_prompt` | template | yes | — |
| `labels.review` (removed) | — | — | — |
| `tend_pr` (removed) | — | — | — |
| `tend_prompt` (moved) | — | — | — |
| `tend.*` (moved) | — | — | — |

The loop name **`tend` is reserved** and a file declaring it fails to load. That is the tend
dispatcher's own name, and every row this program writes is keyed by `(project, loop)`, so a loop
sharing it would read and write the dispatcher's dispatches, pull request links, conflict rows,
lock file and worktrees.

**The project descriptor**, `.agent-utils/config.yaml`, is a different file with a different
shape — see [Two different `config.yaml` files](#two-different-configyaml-files). It holds the
settings that belong to the project rather than to any one loop:

| Field | Type | Required | Default |
|---|---|---|---|
| `name` | string | yes | — |
| `id` | UUID | yes | minted on `project init` |
| `tend.enabled` | bool | no | `false` |
| `tend.label` | string | yes if `tend.enabled` | — |
| `tend.prompt` | template | yes if `tend.enabled` | — |
| `tend.model` | string | yes if `tend.enabled` | — |
| `tend.permission_mode` | enum | yes if `tend.enabled` | — |
| `tend.harness` | enum | no | `claude` |
| `tend.effort` | enum | no | none passed |
| `tend.i_understand_bypass_permissions` | bool | only with `bypassPermissions` | `false` |
| `epic.loop` | string | no; required to sweep epics | — |

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

**A loop declares only its own states.** Four labels: queued (`trigger`), working (`in_flight`),
stuck (`blocked`), and done (`terminal`). Nothing in a loop file names another loop, and nothing
in it describes what happens after the issue leaves.

**The chain is made of values, not of references.** Loops run in sequence because an operator
chose one loop's `terminal` to be the next one's `trigger` — that is a fact about the strings, and
neither loop asserts it or reads it back. The consequence worth holding on to: you can add,
remove or reorder a loop by changing label values, and no other loop file has to be edited.

Two questions genuinely span loops and so cannot be answered inside one: which pull requests to
keep rebased, and which loop may promote an epic's unblocked children. Both live in the project
descriptor — see [Project settings](#project-settings-tend-and-epic).

There used to be a fifth label, `labels.review`, meaning "the agent finished and its output is
waiting for a human to read". It is gone. It was the only label whose meaning depended on what
came *after* the loop, so it made every loop describe its neighbour, and two mechanisms were
quietly relying on it: tend eligibility (now `tend.label`) and the epic sweep's entry loop (now
`epic.loop`). A loop that hands to a human now does it the same way as one that hands to a
machine: it applies its terminal and stops. What makes a terminal a human gate is that no loop
triggers on it.

**The loop writes a label in three situations**, and never in any other. It applies `blocked` when
an issue exhausts its retry budget — see `retry.max`. It applies `trigger` to an unblocked
sub-issue of an epic. And it removes `status:epic-ready` from an epic it has just swept on
request. The last two are the epic sweep — see [Epics](../README.md#epics), which also explains
why only one loop of a project ever does that. Everything else is the agent's to apply.

### `labels.trigger` — required

The "go" signal. **You** apply it — and so does the epic sweep, for the loop at the front of the
pipeline, when a sub-issue's blockers are all closed. It means both "start this" and "resume this",
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

### `labels.terminal` — optional

The issue has left this loop. **Every loop ends by applying it**, as its agent's last action, and
then stops. It is the only label a loop declares that says anything about what comes next — and
even then it names a *state*, not a loop.

**Whether it is a human gate is not a property of the field.** It is whether any loop triggers on
that state. In the reference chain, `planning`'s terminal (`status:ready-for-plan-review`) and the
findings loop's terminal (`status:ready-for-review`) are gates, because nothing triggers on
either; `execution`'s and `pr-review`'s terminals are handoffs, because the next loop's trigger is
the same string. All four are applied by the agent, in the same way, and no loop file records
which kind it is.

That symmetry is why planning's approval label is *not* its terminal. Planning ends at
"planning is finished" and stops; you then apply `status:ready-for-execution`, which is
execution's trigger and the one label planning's prompt forbids its agent from ever applying.

**A terminal that another loop triggers on is only safe if the prompt makes applying it the
agent's strictly final action**, after the last push, and says so in numbered steps. Otherwise the
next agent starts on a branch the first is still writing to, and the two fight over the files and
the labels alike. The reference `execution` and `pr-review` prompts both number their completion
steps for exactly this reason.

It is optional only because a loop may have nowhere to hand on to — but every reference loop has
one, including the last, and a loop without one simply parks issues with no way out.

**It does not, on its own, make the issue leave.** Nothing in the engine reads it for selection.
To make an issue actually leave the loop, **list the same label under `veto`** — otherwise a
re-applied trigger can pull back work already handed downstream.

Vetoing the terminal costs nothing where tending is concerned. It used to: `veto` was checked
before *tend* decisions, so a loop that vetoed its own terminal also stopped itself rebasing a pull
request parked in that state. Tending is its own dispatcher now and reads no loop's veto list.

```yaml
terminal: status:ready-for-plan-review
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

A `harness:` label is never refused for a setting the chosen harness does not implement. The
claude-only settings — `agent.permission_mode`, `agent.max_budget_usd`, `agent.background_tasks`
— are **ignored** when the effective harness is `pi`, exactly as they are for a loop configured
`harness: pi` outright: pi has no permission model, no cost-ceiling flag and no background-task
switch, so the builder emits none of them. See [`agent.harness`](#agentharness--optional).

So a `harness:` label changes which binary runs with no additional gate, and a `harness:pi`
label on a loop that set `agent.permission_mode` or `agent.max_budget_usd` runs that issue
without either bound. This is accepted and deliberate: applying a label already requires triage
access or above, and the same collaborator's issues already run an agent with a
repository-write token. A loop where dropping those bounds matters should not leave `harness:`
reachable.

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

A valid override applies to every dispatch for that issue: its first run, every resume, every
retry, and the tend dispatch that rebases its pull request. A tend inherits the issue's session,
and a session identifier only means something to the harness that minted it — a tend running the
loop default against a session started under `harness:pi` would fail in a second, every tick.

An override label the loop cannot parse **skips** the tend rather than stopping the issue. A
stale rebase is not the issue's own work, and an invalid label already stops the issue where
that work would happen.

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
- `agent.permission_mode`, `agent.background_tasks` and `agent.max_budget_usd` are claude-only.
  A `pi` config may carry them — a `harness:claude` label can make them take effect for one
  issue — but a `pi` dispatch IGNORES all three: pi has no permission model, no background-task
  switch and no cost-ceiling flag, so none of them is ever emitted. `agent.permission_mode` is
  still validated as a claude mode whatever the harness is, acknowledgement included.
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

claude-only: a `pi` dispatch — whether from `harness: pi` or a `harness:pi` label — ignores it,
because pi has no permission model. The value is still validated whatever the harness is, since
a `harness:claude` label can make it take effect for one issue.

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

**`0`, or omitting the field, disables the cap**, and **`0` is what every reference loop sets.**
The flag is then not passed at all and the dispatch runs with no cost ceiling.

That is a considered default, not laziness. **A budget cap does not prevent an expensive run; it
interrupts one.** The dispatch is stopped wherever it happens to be — mid-edit, after the agent's
last push or before it — and recorded failed, so the loop retries it from a resumed session and
spends again on work the cap just threw away. The cases where a cap helps are the ones where the
run was going to be cheap anyway; the cases where it fires are the ones where it costs you the run
*and* the retry.

Control cost with the settings that shape the whole run — `agent.model` and `agent.effort`, and
the structure of what you ask the agent to do — rather than with a ceiling that lands in the
middle of it. The reference loops write `max_budget_usd: 0` out explicitly, rather than omitting
the field, so a reader can see that no ceiling was a decision.

A negative value is rejected at load. It would otherwise behave exactly like `0`, so `-25`
typed for `25` would have run uncapped and said nothing.

If you do set a cap: it is per **dispatch**, not per issue or per day. An issue that is retried
three times can spend up to three times this amount.

claude-only: a `pi` dispatch — whether from `harness: pi` or a `harness:pi` label — ignores it,
because pi exposes no cost-ceiling flag.

```yaml
max_budget_usd: 0   # no cost ceiling; the recommended value
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

claude-only: it reaches the child through the claude environment, which a `pi` dispatch —
whether from `harness: pi` or a `harness:pi` label — never builds, so such a dispatch ignores it.

```yaml
background_tasks: false   # true only if you know why you want it
```

### `agent.timeout` — optional, defaults to `24h`

How long one dispatch may run. A Go duration string: `90s`, `30m`, `3h`. **Omit it unless you have
a specific reason.**

On expiry the loop signals the agent's whole process group, not just the direct child, so a
dev server or watcher the agent started does not survive it. The dispatch is recorded failed
and becomes eligible for retry.

**This is a last resort, not a hang detector and not a budget.** A genuinely stuck dispatch is
caught by `retry.breaker.orphan_threshold` and by `agent-utils project loop kill`, both of which
act on evidence. All the timeout adds is a bound on a wedged process that nothing else noticed, and
the only cost of a high one is how long that rare case takes to clear.

The default is high for that reason. This field used to be required, which made every operator
invent a number for the setting they have least basis to choose — and the invented number is
always too small, because guessing low is invisible: the dispatch is recorded failed and retried
from a resumed session, so a real long run on a large branch reads as a flaky agent rather than as
a number that needed raising. A timeout mid-way through useful work also wastes everything after
the agent's last push.

An explicitly set value is never overwritten. `0` means "unset" and takes the default, because
YAML cannot distinguish an omitted field from an explicit zero; a negative value is rejected at
load.

```yaml
timeout: 24h   # the default; omit the field for the same result
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

## Project settings: `tend` and `epic`

**These live in `.agent-utils/config.yaml`, the PROJECT descriptor — not in a loop file.** A loop
file that sets them fails to load. See
[Two different `config.yaml` files](#two-different-configyaml-files).

Both answer questions that span loops, and a question that spans loops cannot be answered inside
one without making that loop describe its neighbours. Keeping them here is what lets a loop
declare only its own labels.

```yaml
name: lawndominator
id: 6f1c...        # minted by `project init`; never edit it

tend:
  enabled: true
  loop:    exec-pr-review-findings
  label:   status:ready-for-review
  model:   sonnet

epic:
  loop: planning
```

### `tend`

Whether the project keeps its open pull requests rebased, which ones, and the whole description of
the agent that does it. Off by default: tending force-pushes branches, and a policy that
destructive is opt-in.

**Tending is its own dispatcher.** It is not a loop, it has no loop file, and no loop file says
anything about it. It has a name of its own — `tend`, which no loop may take — and everything it
writes is keyed by that name: its dispatches, its pull request links, its conflict rows, its lock
file, its log tree, and its worktrees at `<worktree_dir>/tend/pr-N`. It gets a fresh session per
dispatch. Everything below describes it in full; there is no loop `agent:` block behind it and
nothing is inherited.

It used to be a per-loop `tend_pr: bool` gated on that loop's `labels.review`, and then briefly a
project-level policy with a `tend.loop` naming a host. Both arrangements existed for the same
reason — the tend work ran inside loop ticks, so some loop's rows had to hold its state — and both
are gone with it.

**Where the dispatcher shows up.** It answers to `tend` everywhere a loop name is accepted, which
is deliberate: it dispatches agents and holds sessions, so it must not be invisible to the operator.

```
agent-utils project loop status --name tend    # the pull request queue it maintains
agent-utils project loop tick   --name tend    # the full pass, for a cron entry
agent-utils project logs        --name tend --list
agent-utils project sessions list --name tend
```

`loop status --name tend` prints a different table from a loop's, because the dispatcher moves no
label and keeps no issue state: it reports which pull request each eligible issue links to, how far
behind it was at the last pass, when it was last tended, and whether an agent is in it now.
`loop tick --name tend` runs the dispatcher's own full pass — the one that reads both staleness and
review activity — and is what a machine with no listener daemon should have in cron. `--force` is
accepted there and does nothing: the gates it suspends belong to a loop's retry policy, and a tend
has none.

#### `tend.enabled` — optional, default `false`

The switch. When false, nothing else in the block is required and the project has no tend
dispatcher at all — the listener emits no target for it, and `--name tend` reports that tending is
off rather than failing obscurely.

#### `tend.label` — required when enabled

The issue label that makes a pull request eligible. This is the explicit statement of what used to
be implicit: *an issue in this state has a pull request worth keeping fresh*.

Point it at the state where a pull request waits longest — in the reference chain that is
`status:ready-for-review`, the human's merge queue. Every earlier wait is a machine that picks the
issue up within a tick; only that one lasts long enough for the base branch to move underneath it.

#### `tend.prompt` — required when enabled

The template the tend agent runs. It moved here out of the host loop's `tend_prompt`, because a
loop that does not tend has no business carrying the instructions for a dispatch it never makes.

It renders the same context a loop prompt does — `{{.Repo}}`, `{{.Issue.*}}`, `{{.PR.*}}`,
`{{.Worktree}}` — with **one difference that has teeth**: a tend has no loop, so there are no loop
labels. `{{.Labels.Trigger}}` and friends would render as empty strings and tell the agent to act
on a label named `""`, so **a `tend.prompt` mentioning `.Labels` is rejected at load time**. Name
the labels literally, and use `{{.Tend.Label}}` for the eligibility label above. See
`examples/project/config.yaml` for the reference template.

#### `tend.model` — required when enabled

Which model a tend runs on. It is required because it is the one question this policy exists to
answer and there is no longer a loop `agent.model` to fall back on.

A tend agent's job — resolve a rebase conflict, or answer review feedback — is smaller than the job
a trigger dispatch does, so a cheaper model is usually right.

#### `tend.permission_mode` — required when enabled

claude's permission mode for a tend dispatch, one of `acceptEdits`, `auto`, `manual`, `dontAsk`,
`plan`, `bypassPermissions`.

It is required rather than defaulted, and that is a deliberate refusal to choose for you. A tend
rebases, force-pushes and replies on review threads — the most destructive dispatch this program
makes — and it is also the one setting with no safe default: claude denies every prompt in a
detached `-p` run, so a tend with no mode fails at its first `git push` rather than doing nothing.

`bypassPermissions` additionally requires `tend.i_understand_bypass_permissions: true`, for the
reason a loop's own acknowledgement exists: the tend agent reads pull request review threads
written by third parties, and that mode runs whatever they contain with no gate.

#### `tend.harness` — optional, default `claude`

One of `claude` or `pi`. It defaults exactly as `agent.harness` does.

**Setting `tend.harness: pi` means setting `tend.model` to a `provider/id`.** A claude alias like
`opus` is not something pi can resolve, and nothing rejects the pair at load time (the model is
free text for both harnesses), so the mismatch surfaces as a failed dispatch:

```yaml
tend:
  harness: pi
  model: anthropic/claude-haiku-4-5
```

#### `tend.effort` — optional

One of `low`, `medium`, `high`, `xhigh`, `max`; an invalid value fails the descriptor load rather
than the dispatch. Omitted, no effort level is passed.

#### What is deliberately NOT here

There is no `worktree` mode: a tend always runs in a worktree of its own for the pull request it is
rebasing, under the dispatcher's name, so the choice could only ever be set wrong. There is no
`max_budget_usd`: a cap does not stop an expensive run, it stops it PART WAY THROUGH, and a tend
stopped mid-rebase leaves a half-resolved conflict. There is no `timeout`: it is 24h, the same
last-resort bound a loop gets by omitting it. There is no `retry` block: a tend is never retried,
and the next trigger is what tries again. And there is no `veto` list — see *How tending works*.

A `harness:`, `model:` or `effort:` label on the issue still beats all of these, the same way it
beats `agent`'s: a label is an instruction about one issue, and a tend is one of that issue's
agents.

### `epic`

`epic.loop` names the one loop allowed to promote an epic's unblocked sub-issues into its trigger
label. Nothing else is configurable; see [Epics](../README.md#epics).

The two labels the sweep works from are not configurable either. `epic`, on the parent, is the
switch that makes an issue an epic. `status:epic-ready`, on the parent, asks for a sweep now and
is consumed by it — it is how an epic's **first** sub-issue is promoted, since until something
closes there is no closure to arm the sweep with. Both are fixed strings, so the label an operator
applies is the same one in every project.

Exactly one loop may do it: if every loop promoted into its own trigger, the execution loop would
push a fresh issue straight to `status:ready-for-execution` and planning would be skipped —
silently, and only for issues that happen to be swept.

**This too used to be derived**, by asking which loop's trigger was no other loop's terminal or
review label. Declaring it is the change: the derivation made the answer a property of every loop
file at once, so renaming one label could move the front of the pipeline, or produce two
candidates and disable promotion for the whole project with a single log line.

A declaration that is missing, names a loop that does not exist, or names one watching another
repository, is reported where the sweep would have run and promotes nothing. So is a project whose
loop files will not all load — the broken one may be the loop that was named.

### How tending works

For each issue carrying `tend.label`, on every pass of the tend dispatcher (see the triggers
below):

1. Find the open pull request whose body closes it (`Closes #N`, `Fixes #N`, `Resolves #N`).
2. Ask how far behind its base it is (the local checkout for a sweep, the GitHub API for a
   full tick).
3. If it is behind by at least one commit, **git attempts the rebase first.** A clean replay
   costs no agent and no tokens: fetch, rebase, and a lease-guarded force-push, and nothing of
   that is a conversation. Only a rebase that conflicts falls through to the tend agent.

A current pull request costs no push and produces nothing — no comment, no rebase, no agent.
Silence is the correct output.

Three safeguards apply:

- **Only a trusted pull request is ever linked.** Its head branch must live in the target
  repository and its author must be an `OWNER`, `MEMBER`, or `COLLABORATOR`. A fork pull
  request claiming `Closes #7` cannot hijack the link, because tending checks the head branch
  out and rebases or runs an agent inside it.
- **An issue with a live agent ANYWHERE IN THE PROJECT is never tended**, so a rebase cannot
  force-push a branch some loop's build agent is committing to. The guard is project-wide, and it
  has to be: the agent a tend must not collide with belongs to the loop that wrote the branch, and
  the dispatcher's own rows hold only its own tends.
- **An issue an operator stopped, in any loop, is never tended.** `sessions kill` means "run no
  more agents at this issue", and a tend is one of that issue's agents.
- **There is no veto list, and its absence is a decision.** Tending used to inherit the host loop's
  `labels.veto`; with no host there is no one loop's list to inherit, and a union of every loop's
  is worse than useless — the lists name one another's states, so a union vetoes every status label
  the pipeline has, including the eligibility label itself. The two guards above, the eligibility
  label and the draft check are what gate a tend now.
- **Tending never changes a label**, unless its own prompt tells the agent to park.
- **A DRAFT pull request is never tended.** It is the author's working copy: nobody is blocked by
  it being behind, no reviewer is waiting on a reply, and force-pushing a rebase under someone
  still assembling the branch is the one thing tending must never do.
- **A clean rebase has no session at all.** git replays the branch in this process, so there is
  no conversation to be fresh or stale, and no agent is dispatched. The rest of this bullet
  applies only to the tend agent a conflict escalates to.
- **A tend agent gets its OWN session, always.** It never resumes the issue's, and never resumes
  a previous tend's. It used to inherit the issue's, so a rebase agent carried the context of the
  work it was rebasing; three things removed the reason. A clean rebase runs no agent now, so what
  is left for one is a conflict or a review reply — both fully described by the branch, the
  conflicted hunks and the pull request thread, none of them by the conversation that wrote the
  code. Inheriting also BLOCKED the issue for as long as the tend ran, because two processes on
  one session identifier is the same hazard as two agents in one branch. And the dispatcher is not
  a loop and keeps no issue state, so "the issue's session" names one conversation per loop and
  none of them is its own. Continuity across repeat tends of a pull request is what
  actually matters, and that lives in the database — the last-tend time and the conflict
  fingerprints — rather than in a resumed conversation.

**Three things arm a tend sweep.** A delivery for one issue still tends it directly — the
issue carries `tend.label`, its linked pull request is behind its base or carries unanswered review
activity, and the delivery named that issue. The delivery reaches the dispatcher the same way it
reaches a loop, as one more fan-out target. Beyond that:

- **A merge into `default_branch`.** GitHub sends a `pull_request` delivery with `merged:
  true`, and because the merge is what made every other pull request stale while naming none
  of them, that one delivery sweeps the project.
- **A push to `default_branch`.** A push with no pull request attached makes every open pull
  request stale the same way a merge does, and the listener now subscribes to `push` deliveries
  to catch it — see the main README's [Webhooks](../README.md#webhooks) section for the
  deployment step this requires.
- **The periodic tend check.** A timer independent of any delivery, described in the main
  README next to `tend_interval`. It runs only while the listener runs, and each pass costs no
  GitHub call when nothing is behind.

Whichever of these fires, every issue carrying `tend.label` whose linked pull request targets
`default_branch` and is now behind gets a rebase attempt — git first, the agent only on conflict,
exactly as above.

A pull request targeting any other branch is left alone. A merge into `master` says nothing
about a branch based on `release/1.0`.

The sweep dispatches **tend agents only**, and only for the pull requests git could not rebase
cleanly. It never starts, resumes, or retries an issue agent, and it never parks an issue. A
stale branch is a reason to rebase; it is not a reason to start work.

A sweep waits about a minute before it runs, so a merge train (or a push train) produces one
sweep rather than one per event, and it acts on at most ten pull requests per pass — a cap that
applies to the agent-free rebases as well as the agent dispatches, since the point is bounding
how much force-pushing and how many agents one sweep can start. If more pull requests are
behind than that, the rest are named in the log line and wait for the next sweep.

**A closed pull request has its worktrees removed.** This is separate from tending and is not
gated by it; `cleanup_closed_pr` turns it off. When GitHub reports a pull request closed — merged or not — the loop removes that
pull request's `pr-<N>` worktree, and the `issue-<M>` worktree of the issue it closes, as soon
as neither has a live dispatch. A worktree of a large repository is easily hundreds of
megabytes, and nothing removed one before.

Two limits apply. The issue link is honoured only for a trusted pull request, so an outside
contributor cannot name someone else's issue in a `Closes #M` body and have its work deleted.
And the live-dispatch guard protects work in progress, not uncommitted or unpushed work in an
idle worktree — that is removed too, with a warning naming the worktree in the log.

**None of the sweeps above replace the cron ticks.** `agent-utils project loop tick` is still the
only full reconcile for a LOOP: it is what retires a dead runner for an issue no delivery names.
`agent-utils project loop tick --name tend` is the same thing for tending, and it is the only tend
trigger at all on a machine that runs no listener daemon — the merge, push, delivery and periodic
triggers all belong to the listener. Schedule both under cron regardless of whether the daemon
runs.

**Point `tend.label` at a state no loop writes the branch in.** `plan-feature` opens a design
draft pull request whose body says `Closes #N`, so a `tend.label` pointing at a planning state
would force-push a draft you are in the middle of reading — and the drafts guard above only covers
the draft case, not the general one. A review or remediation state is the same hazard from the
other side: the reviewer commits its mechanical-gate fixes and the remediation loop force-pushes a
whole round of them.

**The label is the primary guard, and the live-dispatch guard is the backstop.** An issue being
worked has left `tend.label` — that is what a pipeline of states IS — and while an agent is
actually running, the project-wide live-dispatch guard suppresses the tend regardless of what the
labels say, because an agent flips its own labels asynchronously. In the reference chain
`tend.label` is the final loop's terminal, which no loop triggers on, so both hold.

**Two things trigger a tend, and either alone is enough.** The pull request is behind its base —
the trigger this section led with — or review activity on it is newer than the last *finished*
tend dispatch (a still-running tend is not counted twice: an issue with a live agent is never
tended, per the safeguard above, so no second decision is produced while one runs). When both are
true, the decision names both. A failed tend still counts as the last one: nothing here retries a
crashed tend agent on its own, only the next qualifying trigger does.

**Only a trusted reviewer's activity counts, and the loop's own comments never do.** A review or
review comment counts only when its author's association with the repository is `OWNER`,
`MEMBER`, or `COLLABORATOR` — the same bar a pull request itself must clear before tending
trusts it at all. Activity written by this program's own GitHub account is excluded outright,
regardless of association: without that exclusion, a `tend.prompt` that has the agent post a
reply would make its own comment newer than the dispatch that produced it, and every later pass
would see pending review activity and dispatch again — an unattended loop billing itself to
answer itself, forever.

**The merge and push sweeps, and the periodic check, trigger on staleness alone.** The sweep's
subject is the default branch MOVING, and review activity is not that subject — a merge to master
must not dispatch agents at pull requests that are current and merely carry comments. The periodic
check has a second reason: it reads local git refs, and nothing in a local checkout records who
reviewed what, so it cannot cheaply ask "is there new review activity" the way it cheaply asks "is
this behind."

Review activity reaches the dispatcher two other ways instead. A `pull_request_review` or
`pull_request_review_comment` delivery tends its linked issue directly, within seconds — that is
the fast path. `agent-utils project loop tick --name tend` is the safety net under it, since it
reads GitHub state rather than local refs.

**Replying to review feedback needs a `tend.prompt` that branches on `{{.PR.ReviewPending}}`.**
A pure rebase instruction stays correct even for a review-triggered tend: a dispatch carrying
`ReviewPending` still rebases first when it is also behind, and a template that never reads
`{{.PR.ReviewPending}}` behaves exactly as before. To have the tend agent read and answer
unresolved review threads, branch the prompt on that variable — see `tend.prompt` in
`examples/project/config.yaml`.

**A rebase that keeps conflicting the same way backs off, rather than paying for another agent
turn on it.** A conflict is fingerprinted by its conflicted paths together with the branch's
HEAD commit — deliberately not the base: a tend sweep is armed by the base moving, so the base
differs on every sweep by construction, and a fingerprint carrying it would suppress nothing.
The wait after the 1st, 2nd, and 3rd-or-later agent dispatch that met one fingerprint is 1 hour,
6 hours, and 24 hours; a pass that is still inside that window for the same fingerprint declines
to dispatch the agent and writes nothing — a sweep can observe the same unresolved conflict many
times an hour, and writing on every observation would push the wait forward faster than it ever
elapses. A HEAD that moves, by an agent's push or a human's, is a different fingerprint, and the
backoff starts over from the first sighting. One exception: a decision carrying `ReviewPending`
is never backed off, because the backoff's evidence is a repeated rebase conflict and says
nothing about whether a reviewer's comment has since been answered.

The backoff needs no configuration: the tend dispatcher always runs in a worktree of its own for
the pull request it is rebasing, which is where the conflicted paths the fingerprint is built from
come from.

## `retry`

What happens when a dispatch fails. A dispatch fails if its process dies without recording an
outcome, or if `claude` exits non-zero, or if its stream reports an API error.

### `retry.max`

How many times one issue may be retried before the loop gives up. `0` means never retry.

**At the cap the loop makes one of its two non-agent GitHub writes.** It posts a comment saying
the retries are exhausted, then removes `in_flight` and `trigger` and applies `blocked`.

It removes `trigger` deliberately: leaving it would let the next tick resume the issue
immediately and the park would stop nothing.

The cap comment names the failure it parked on — the harness's own error, the harness, the
model, and the pi provider — so the reason to change something is in the issue rather than in
a run log. URLs are stripped from that error before it is posted: a provider is free to put
anything in it, and OpenRouter's 402 embeds a key-management link. The unredacted text stays
on the dispatch row and in the run log, reachable with `agent-utils project logs`.

The counter also resets on any successful dispatch, so an issue that fails three times over
its lifetime with successes in between is not parked on its next single failure.

```yaml
max: 3
```

#### Retiring a cap

A retry cap is evidence about **one configuration**. An issue parked at `retry.max` un-parks
by itself when the configuration those failures describe is no longer the one in play:

- **Changing the harness** — the `harness:` label, or the loop's `agent.harness` — retires the
  accumulated failures and skips whatever backoff was left. Three pi failures say nothing
  about whether claude can build the issue.
- **Changing the model to one served by a different pi provider** does the same. `openrouter`
  and `openai-codex` are separate accounts with separate balances, so a credit failure on one
  is no evidence about the other. The provider is resolved with `pi --list-models`; when it
  cannot be resolved, the cap stands.
- **Changing the model within one provider does not.** Swapping one OpenRouter model for
  another while OpenRouter is out of credits changes nothing the failures were about.

Re-applying `trigger` on its own does **not** clear a cap whose configuration has not changed.
The comparison is against the harness and provider of the most recent *attempted* dispatch, so
one change buys one retirement: if the new configuration also fails, it spends its own budget
and parks on its own merits.

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

**It is counted per tick, and a tick scoped to ONE issue can never reach a threshold above 1.**
Both the webhook listener and the crash recovery it runs at start act one issue at a time, so
only a full `loop tick` — a cron entry, or a manual run — can trip this. That is deliberate for
recovery: a machine coming back with two dead agents is not a platform fault in progress, and a
whole-loop tick over them would reap both, count two eligible retries, trip a threshold of 2,
and go quiet for the cooldown having dispatched nothing. Recovery drains them one per wake
instead. If you run a cron sweep after a crash, expect exactly that trip.

### `retry.breaker.cooldown`

How long to dispatch nothing after the breaker trips. A Go duration string.

Ticks still run during a cooldown; they simply make no decisions. Set it long enough for a
platform incident to clear — the default in both examples is `30m`.

To dispatch before the cooldown expires — the incident is fixed and you do not want to wait —
run one tick with `--force`:

```bash
agent-utils project loop tick --name planning --force
```

That single tick ignores the cooldown, ignores every issue's retry backoff window, and cannot
trip the breaker (forcing makes the waiting retries eligible again, so a live breaker would
just drop them all and re-arm). The stored cooldown is left alone: the next unforced tick,
including every one the daemon runs, is back under it. The retry cap is not a time gate and
still applies — a forced tick parks an issue that has exhausted its retries, same as any other.

```yaml
cooldown: 30m
```

## Prompts

Go `text/template` strings. Whichever applies is rendered and passed to `claude` as a
single positional argument, so no amount of text in them can become a separate flag.

Each is parsed when its file loads. A typo like `{{.Issue.Titel}}` fails at startup rather than
inside a detached process three hours later.

| Field | Where | Rendered when |
|---|---|---|
| `prompt` | loop file | An issue starts for the first time, or restarts because its previous attempt never created a session |
| `resume_prompt` | loop file | An issue resumes an existing session |
| `tend.prompt` | project descriptor | A pull request is tended: it is behind its base, or it carries review activity newer than the last tend |

`prompt` and `resume_prompt` are always required on a loop. `tend.prompt` is required when
`tend.enabled` is true, and lives in the PROJECT descriptor: it used to be a loop's `tend_prompt`,
and a loop that does not tend — which is now every loop — has no business carrying one.

**A `tend.prompt` may not reference `{{.Labels.*}}`, and is rejected at load time if it does.**
A tend has no loop, so there are no loop labels: the reference would render as an empty string and
instruct the agent to act on a label named `""`. Name the labels literally, and use
`{{.Tend.Label}}` for the eligibility label.

`resume_prompt` can be short. The agent is in the same conversation and already knows what it
did; it needs to be told what changed, not told everything again.

Write these prompts as the agent's whole instruction set. The loop enforces nothing about the
agent's behaviour, so every rule — how to stop, which labels to apply, never to merge, never
to apply the terminal label — lives here. The example files carry the full set.

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
| `{{.PR.ReviewPending}}` | bool | Tend dispatches only; set when this tend was triggered, in whole or in part, by review activity newer than the last finished tend |
| `{{.Labels.Trigger}}` | string | |
| `{{.Labels.InFlight}}` | string | |
| `{{.Labels.Blocked}}` | string | |
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
| `agent.timeout` not negative (omitted or `0` takes the `24h` default) | `agent.timeout must not be negative` |
| `retry.max` ≥ 0 | `retry.max must not be negative` |
| `len(retry.backoff)` ≥ `retry.max` | `it needs one entry per retry` |
| `retry.backoff_ticks` must be empty | `is no longer supported; ... replace it with retry.backoff` |
| `retry.breaker.orphan_threshold` ≥ 1 | `must be at least 1` |
| `retry.breaker.cooldown` > 0 | `must be greater than zero` |
| `checkout_base_dir`/`worktree_dir` relative needs a project root | `… so there is no project root to resolve it against` |
| All three prompts parse as templates | `prompt: template: …` |
| No unknown keys anywhere | `field … not found in type config.Config` |

Every failure is reported at once, so one load tells you everything wrong with the file.

The **project descriptor** is validated separately. A failure there fails the TEND DISPATCHER,
not the loops — a loop no longer reads the descriptor at all — and the dispatcher fails closed:
the policy it could not read might have said "enabled", and force-pushing under a policy nobody
can read is the outcome worth refusing.

| Rule | Message |
|---|---|
| `tend.label` set when `tend.enabled` | `sets tend.enabled but no tend.label` |
| `tend.prompt` set when `tend.enabled` | `sets tend.enabled but no tend.prompt` |
| `tend.model` set when `tend.enabled` | `sets tend.enabled but no tend.model` |
| `tend.harness` ∈ {`claude`, `pi`} or empty | `tend.harness must be claude or pi` |
| `tend.effort` ∈ {`low`,`medium`,`high`,`xhigh`,`max`} or empty | `tend.effort must be …` |
| `tend.permission_mode` is a claude mode or empty | `tend.permission_mode … is not a valid claude permission mode` |
| `bypassPermissions` acknowledged | `set tend.i_understand_bypass_permissions: true to confirm` |
| `tend.prompt` parses and names no `.Labels` | `tend.prompt references .Labels …` |

Two rules are checked by the DISPATCHER rather than by the descriptor load, so an operator
mid-edit can park a policy without being stopped by a field only a dispatch needs. They are
reported by `--name tend`, and by the listener when it builds its routing table:

| Rule | Message |
|---|---|
| `tend.permission_mode` set when enabled | `enables tending but sets no tend.permission_mode` |
| The project's loops agree on `repo`, `default_branch`, `checkout_base_dir`, `worktree_dir` | `this project\'s loops disagree about … so its tend dispatcher cannot tell which repository it maintains` |

The second is the one place the dispatcher reads the loop files at all, and it reads only those
four fields: they describe the project's checkout, not any loop's behaviour, and a project whose
loops disagree about which repository they watch is broken for reasons that have nothing to do
with tending.

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
