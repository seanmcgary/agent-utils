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

## Watching a run

`loop tick` starts agents and exits, by design: it runs from cron and must not
block. The agents keep writing, and `agent-utils logs` is how you watch them.

```bash
agent-utils logs --name planning --list        # recent dispatches and their ids
agent-utils logs --name planning -f            # follow the newest one live
agent-utils logs --name planning --issue 42    # the newest dispatch for one issue
agent-utils logs --name planning --dispatch 17 # one dispatch exactly
```

The transcript is rendered rather than dumped: session start, the agent's text,
each tool call and its result, and a final line with turns, cost and duration.
Thinking blocks and token counters are hidden.

| Flag | Effect |
|---|---|
| `-f`, `--follow` | Stream while the agent is alive, then stop. Following a finished run exits rather than hanging. |
| `--thinking` | Include the agent's thinking blocks |
| `--raw` | Print the stream-json verbatim |
| `--stderr` | The agent's standard error instead |
| `--runner` | The runner's own structured log, for when a dispatch fails before the agent starts |
| `--path` | Print the log file path and exit, for piping into your own tools |

Each dispatch writes three files under `{state_dir}/logs/{loop}/`:

| File | Contents |
|---|---|
| `{kind}-{issue}-{timestamp}.jsonl` | The agent's stream-json transcript |
| `{kind}-{issue}-{timestamp}.jsonl.stderr` | The agent's standard error |
| `runner-{dispatch}.log` | The detached runner's own log |

All three are mode `0600`: a transcript records everything the agent read and ran.

## Versioning and releases

The semantic version lives in the `VERSION` file at the repository root. It is the
single source of truth; nothing infers a version from a tag.

`scripts/version.sh <ref>` reconciles the file with the git ref:

| Ref | Behaviour |
|---|---|
| Not a tag | Rewrites `VERSION` to `<version>+<short-sha>` so the build identifies its commit |
| `refs/tags/vX.Y.Z` matching `VERSION` | Leaves the file alone; the release ships a bare semantic version |
| `refs/tags/vX.Y.Z` **not** matching | Exits non-zero and fails the build |

That last row is the whole point: a tag cannot produce a release whose binary reports a
different version than the tag says.

The Makefile stamps both values in with `-ldflags -X`:

```bash
make build && ./bin/agent-utils version
# agent-utils v0.1.0 (d6e9df9)
```

A binary built without the Makefile reports `unknown` for both, which is accurate rather
than misleading.

### Cutting a release

1. Bump `VERSION` (e.g. `v0.2.0`) and merge it to the default branch.
2. Tag that commit with exactly the same string and push the tag:

   ```bash
   git tag v0.2.0 && git push origin v0.2.0
   ```

The release workflow then verifies the tag is an ancestor of the default branch, verifies
`VERSION` equals the tag, builds four static binaries, and publishes a GitHub release with
generated notes.

| Artifact | Platform |
|---|---|
| `agent-utils-linux-amd64-<version>.tar.gz` | Linux x86-64 |
| `agent-utils-linux-arm64-<version>.tar.gz` | Linux ARM64 |
| `agent-utils-darwin-amd64-<version>.tar.gz` | macOS Intel |
| `agent-utils-darwin-arm64-<version>.tar.gz` | macOS Apple Silicon |

Every binary is built `CGO_ENABLED=0` and is fully static, which is possible because every
dependency is pure Go — `modernc.org/sqlite` was chosen partly for this. One Linux binary
runs on any distribution with no libc to match.

Build them locally with `make release`.

## Continuous integration

`.github/workflows/main.yml` runs on every push and pull request:

| Job | Does |
|---|---|
| `fmt` | Fails on unformatted code |
| `lint` | `golangci-lint` |
| `test` | Full suite, then again under `-race` |
| `build` | Cross-compiles all four platforms and asserts each carries the expected version stamp |
| `release` | Tags only. Gated on all of the above passing |

## Install

```bash
go install github.com/seanmcgary/agent-utils/cmd/agent-utils@latest
```

This works once the code is on the default branch. Pin a release instead when you
want a known version:

```bash
go install github.com/seanmcgary/agent-utils/cmd/agent-utils@v0.1.0
```

A `go install` binary reports the module version and the VCS revision, because the
Go toolchain embeds both:

```
$ agent-utils version
agent-utils v0.1.0 (071ca17)
```

Note that `@<branch>` only works for a branch whose name has no `/` in it; Go rejects
`@feat/my-branch` as an invalid version string. Use a commit SHA for an unmerged
branch, or build from a checkout:

```bash
git clone https://github.com/seanmcgary/agent-utils && cd agent-utils
make install     # into GOBIN, with the VERSION file's value stamped in
make build       # into ./bin
```

Prebuilt static binaries for linux and darwin on amd64 and arm64 are attached to each
[release](https://github.com/seanmcgary/agent-utils/releases).

## Configuration

Configuration files live in `.agent-utils/configs/`, one YAML file per loop. A loop's name is
the `name:` field inside the file, not the file name, and it must be unique within a
directory.

```bash
mkdir -p .agent-utils/configs
cp examples/planning.yaml .agent-utils/configs/

agent-utils list                        # configs in THIS project
agent-utils loop tick --name planning   # select by name
agent-utils loop status                 # the only one, or a prompt, or an error listing names
agent-utils status                      # every project onboarded, across the machine
```

Each project keeps its own state under its own `.agent-utils/state/`, so two projects never
share a database. `agent-utils status` reads only local state, so it needs no token and works
offline:

```
$ agent-utils status
/Users/you/Code/lawndominator
  LOOP           REPO                             TICKS  LIVE   COST     LAST TICK
  execution      mcgarylabs/lawndominator-monorepo  128   1      $84.20   2026-08-18 14:55
  planning       mcgarylabs/lawndominator-monorepo   96   0+1!   $31.05   2026-08-18 14:50
```

`LIVE` shows running agents; a `+n!` suffix marks dispatches whose process is gone. A project
is recorded the first time any command runs against it; `agent-utils forget <path>` removes it
from the list without touching its files.

The directory is found in `$AGENT_UTILS_DIR`, then by walking up from the working directory the
way git finds `.git`. There is no fallback to `$HOME`: configurations are project-local, so
running in an unrelated directory says there is no project there rather than adopting another
project's loops. A cron entry should pass `--config` with an absolute path instead: it depends
on no working directory and never prompts.

`docs/configuration.md` documents every field: what it means, what reads it, and what happens
if you get it wrong. `examples/planning.yaml` and `examples/execution.yaml` are complete
working files.

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
*/15 * * * * . $HOME/.agent-utils/env && /usr/local/bin/agent-utils loop tick --config $HOME/.agent-utils/configs/planning.yaml >> $HOME/.agent-utils/planning.log 2>&1
```
