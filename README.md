# agent-utils

A Go CLI that reads GitHub issues by label and dispatches `claude -p` agents
deterministically. It replaces an LLM orchestrator in a planning loop and an
execution loop: Go owns every deterministic decision (counting slots, flipping
labels before dispatch, retries, the circuit breaker, scheduling); the agent
owns every judgment and every GitHub write, with one stated exception (see
Security).

## Install

```bash
go install github.com/seanmcgary/agent-utils/cmd/agent-utils@latest
```

## Commands

```bash
# Run one reconcile-and-dispatch pass against a loop configuration, then exit.
# This is the command a cron entry runs on an interval.
agent-utils loop tick --config path/to/loop.yaml

# Print the reconciled view of the loop -- issues, dispatch state, retries --
# without changing anything.
agent-utils loop status --config path/to/loop.yaml

# Drop the stored session and worktree for one issue, so its next trigger
# starts a fresh session instead of resuming.
agent-utils loop reset --config path/to/loop.yaml --issue 123
```

Two example loop configurations, ported from the reference planning and
execution orchestrators, live in `examples/planning.yaml` and
`examples/execution.yaml`.

## Security

A loop dispatches an agent that runs with permission prompts disabled, inside a
git worktree, on text written by other people. Issue bodies, issue comments, and
pull request bodies are UNTRUSTED input. An instruction hidden in a comment
executes.

Point a loop only at a repository whose issue and pull request population you
trust. The engine reduces the blast radius in three ways, none of which is a
substitute for that rule:

- The agent process gets a minimal environment. `GITHUB_TOKEN` is removed.
- Only a pull request opened by an OWNER, MEMBER, or COLLABORATOR, whose head
  branch lives in the target repository, is ever linked to an issue or tended.
- `bypassPermissions` requires `i_understand_bypass_permissions: true`.

## Cron

Do NOT put the token inline in the crontab. cron runs the whole line through
`/bin/sh -c`, so a `VAR=value command` prefix puts the token in the shell's
argument list, where `ps` shows it to every user on the machine.

Put it in a file instead:

```bash
install -m 600 /dev/null ~/.agent-utils/env
echo 'export GITHUB_TOKEN=ghp_...' >> ~/.agent-utils/env
```

```cron
*/15 * * * * . $HOME/.agent-utils/env && /usr/local/bin/agent-utils loop tick --config $HOME/.agent-utils/planning.yaml >> $HOME/.agent-utils/planning.log 2>&1
```
