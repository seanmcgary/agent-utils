# agent-utils

A Go CLI that reads GitHub issues by label and dispatches `claude -p` agents
deterministically. It replaces an LLM orchestrator in a planning loop and an
execution loop: Go owns the deterministic decisions: selecting issues by label, session and
worktree bookkeeping, retries, backoff, and the circuit breaker. The agent owns
every judgement and every GitHub write except one (see Security). Cron does the
scheduling; the engine has no timer of its own.
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

## Development

```bash
make deps        # install golangci-lint and staticcheck
make build       # build ./bin/agent-utils, commit stamped in
make check       # fmtcheck + vet + lint + test, in that order
```

| Target | Does |
|---|---|
| `make all` | `deps` then `build` |
| `make build` | Build `./bin/agent-utils` with the commit stamped into `--version` |
| `make install` | Same, into `GOBIN`. Point cron at this, not at a bare `go install` |
| `make test` | Full suite, cache disabled, one package at a time |
| `make test/race` | Same under `-race`. Roughly 3x slower; worth it before a release |
| `make test/verbose` | Full suite with per-test output |
| `make cover` | Coverage profile plus a total |
| `make lint` | `golangci-lint` |
| `make vet` | `go vet` |
| `make fmt` | `gofmt -w .` |
| `make fmtcheck` | Fail if anything is unformatted |
| `make check` | Everything that must pass before pushing |
| `make clean` | Remove build output and coverage |

Tests run with `-p 1` and no cache on purpose: the `worktree` package shells out to
real git and `runner` spawns real processes, so package-level parallelism is not
safe, and a cached PASS is not evidence about the working tree.

## Configuration

One YAML file defines one loop. `docs/configuration.md` documents every field: what it means,
what reads it, and what happens if you get it wrong. `examples/planning.yaml` and
`examples/execution.yaml` are complete working files.

## Security

A loop dispatches an agent that runs with permission prompts disabled, inside a
git worktree, on text written by other people. Issue bodies, issue comments, and
pull request bodies are UNTRUSTED input. An instruction hidden in a comment
executes.

Point a loop only at a repository whose issue and pull request population you
trust. The engine reduces the blast radius in three ways, none of which is a
substitute for that rule:

- The agent process gets a filtered environment and `GITHUB_TOKEN` is removed
  from it, at both hops (the detached runner and the agent itself).

  **This does not make the agent unprivileged.** It keeps `HOME` and
  `SSH_AUTH_SOCK`, because it has to push branches and use `gh`. Anything
  readable by your user is readable by the agent: `~/.config/gh/hosts.yml`,
  `~/.ssh`, git credential helpers, and the very `~/.agent-utils/env` file this
  README tells you to create. Treat the environment filter as defence in depth,
  not as a boundary. If you need a real boundary, run the loop as a separate
  user with its own `HOME` and a narrowly scoped token.
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
