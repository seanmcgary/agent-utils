# agent-utils

A Go CLI that reads GitHub issues by label and dispatches `claude -p` agents on a schedule.

It replaces the LLM orchestrator in an issue-driven planning loop and execution loop. Go owns
the deterministic decisions — selecting issues by label, session and worktree bookkeeping,
retries, backoff, the circuit breaker. The agent owns every judgement and every GitHub write
but one (see [Security](#security)). Cron does the scheduling; the engine has no timer.

The load-bearing property is session continuity: an issue keeps one `claude` session across a
park and its answer, so a resumed run continues the conversation instead of re-deriving its
plan.

## Install

```bash
go install github.com/seanmcgary/agent-utils/cmd/agent-utils@latest
```

Pin a release when you want a known version:

```bash
go install github.com/seanmcgary/agent-utils/cmd/agent-utils@v0.4.0
```

Prebuilt static binaries for linux and darwin on amd64 and arm64 are attached to each
[release](https://github.com/seanmcgary/agent-utils/releases). Or build from a checkout:

```bash
git clone https://github.com/seanmcgary/agent-utils && cd agent-utils
make install     # into GOBIN, with the VERSION file's value stamped in
```

`@<branch>` only works for a branch whose name has no `/` in it; Go rejects
`@feat/my-branch` as an invalid version string. Use a commit SHA for an unmerged branch.

## Quick start

`agent-utils project init` is the first step. A hand-made `.agent-utils/configs/` directory is
no longer enough to make something a project — the machine-wide directory
(`~/.agent-utils`) looks like an ordinary directory too, and without an explicit init step a
stray `cd ~ && agent-utils project status` would happily register it as one. `project init`
refuses to run there, and is otherwise the one place a project is born:

```bash
cd ~/Code/my-repo
agent-utils project init
```

```
Created project "my-repo" (/Users/you/Code/my-repo/.agent-utils)
Start from which template?
  Supplies the label and tend_pr defaults below, and the three prompt bodies.
  1) planning
  2) execution
[planning]:
Loop name
  Unique in this project. Keys the loop's state, its lock file, and its state directory.
[planning]:
Repository
  owner/name: the GitHub repository this loop watches.
[you/my-repo]:
Checkout base directory
  The work tree root this loop's per-issue worktrees branch from. A relative path is
  resolved against the project root, so "." is the project itself.
[.]:
... asks for every remaining field: worktree and state directories, labels, the agent's
model and permission mode, and retry policy; the three prompt bodies come from the template
already chosen above, not from a question ...
Wrote loop configuration /Users/you/Code/my-repo/.agent-utils/configs/planning.yaml
Next: agent-utils project --name my-repo loop tick --name planning
```

`project init` writes `.agent-utils/config.yaml` with a name (from the directory, or the
positional argument you give it) and a UUID that never changes, then registers the project. If
that name is already taken by another project, a suffix is added and you are told:

```
Created project "lawndominator-2" (/tmp/wsB/lawndominator/.agent-utils)
The name "lawndominator" was already taken by another project, so this one is "lawndominator-2".
Change it by editing /tmp/wsB/lawndominator/.agent-utils/config.yaml
```

Cloning a repository that was already set up somewhere else is the one case where `project
init` does nothing but register it. The committed `.agent-utils/config.yaml` keeps its name and
UUID, the committed `.agent-utils/configs/*.yaml` loops are left alone, and the wizard is
skipped — there is nothing to ask:

```
Registered "my-repo" (0f5c...c31a) at /Users/you/Code/my-repo/.agent-utils with 2 existing loop configurations; nothing else to do.
```

If that committed name already belongs to a *different* project on this machine, init refuses
rather than registering a second project under the same name — a name that matches two projects
makes every later `--name` command act on whichever one ran most recently. Edit the `name:` in
the clone's `.agent-utils/config.yaml` and run init again. (An already-duplicated registry is
caught on the other side too: selecting by an ambiguous name is an error listing both
candidates, and you can select by id or path instead.)

Unless you pass `--no-loop`, and unless the project already has at least one loop
configuration, `project init` then walks an interactive wizard that asks for every field the first loop needs — labels, repository, agent model, retry policy — and takes
the three prompt bodies from the template, which you edit in the written file. It writes
`.agent-utils/configs/<name>.yaml`. Run from a script or a cron job (any
non-terminal stdin), it skips the wizard rather than hanging on a prompt that will never come;
add the loop later with `agent-utils project loop new`, which runs the same wizard for a
second (or first) loop on an already-initialised project.

## Commands

Commands split by scope. **Top level spans the machine; `project` acts on one project.**

### Global

| Command | Does |
|---|---|
| `agent-utils list` | Every project on this machine, with each loop's ticks, live dispatches, cost and last tick |
| `agent-utils logs --project <p> --session <id>` | Log search across projects |
| `agent-utils forget <name\|id\|path>` | Drop a project from the registry, touching none of its files |
| `agent-utils migrate [--dry-run]` | Import state left by the old per-loop databases, and print a report. Not required |
| `agent-utils version` | Version and commit |
| `agent-utils config show [--reveal] \| get <key> \| set <key> <value> \| unset <key> \| webhook ...` | Read and write the machine-wide `~/.agent-utils/config.yaml`: the webhook daemon's URL, bind address and secret |
| `agent-utils listener start [--daemon] [--listen-addr <a>] [--listen-port <p>] \| stop \| status` | Run the webhook listener in the foreground, or install/remove/inspect it as a launchd agent |

### Project

| Command | Does |
|---|---|
| `agent-utils project init [<name>] [--dir <path>] [--no-loop]` | Create or re-register a project explicitly and, unless `--no-loop` or it already has a loop, walk the loop-configuration wizard for its first loop |
| `agent-utils project loop new` | Add another loop configuration to this project, via the same wizard |
| `agent-utils project status` | Identity, file locations, and every loop's state |
| `agent-utils project list` | This project's loop configurations |
| `agent-utils project sessions list` | Every claude session with its issue, runs, cost and state |
| `agent-utils project logs` | Watch a dispatched agent, live or after the fact |
| `agent-utils project loop tick --name <loop>` | One reconcile-and-dispatch pass, then exit. This is what cron (and the webhook daemon) runs |
| `agent-utils project loop status --name <loop>` | The reconciled view of one loop: issues, titles, dispatch state, retries |
| `agent-utils project loop reset --name <loop> --issue <n>` | Drop an issue's stored session and worktree so its next trigger starts fresh |
| `agent-utils project register-webhook [--name <loop>] [--yes]` | Register this project's repositories with GitHub as webhook delivery targets, and record each hook id |
| `agent-utils project deregister-webhook [--name <loop>] [--yes] [--force]` | Delete those webhooks at GitHub, by recorded hook id, and forget the records |

Every `project` command takes `--name <project>` to act from any directory, or uses the
project in the current directory when you omit it:

```bash
cd ~/Code/lawndominator && agent-utils project loop tick --name planning
agent-utils project --name lawndominator loop tick --name planning   # from anywhere
```

The outer `--name` is the project, the inner one is the loop. `--project` is an alias for the
outer one if you prefer it spelled out.

## Sessions

A session is one claude conversation. It survives resumes, so an issue keeps a single session
across a park and its answer, and several dispatches share it. That makes a session the unit
to follow when you want the whole story of an issue rather than one run.

```
$ agent-utils project sessions list
SESSION            LOOP       ISSUE  TITLE                  RUNS  COST     STATE      LAST RUN
cc33-tend-run      planning   57     Fix timezone bug       1     $0.30    succeeded  2026-08-18 21:17
bb22-session-two   planning   57     Fix timezone bug       1     $2.40    ORPHANED   2026-08-18 21:12
aa11-session-one   planning   42     Add zone lookup        3     $5.05    succeeded  2026-08-18 20:52
```

`ORPHANED` marks a session whose dispatch is still recorded as running but whose process is
gone. `--name <loop>` restricts the list to one loop.

## Watching a run

`loop tick` starts agents and exits, by design: it runs from cron and must not block. The
agents keep writing, and `logs` is how you watch them.

```bash
agent-utils project logs --list                 # recent dispatches and their ids
agent-utils project logs -f                     # follow the newest one live
agent-utils project logs --session aa11-...     # a whole session; no --name needed
agent-utils project logs --issue 42             # the newest dispatch for one issue
agent-utils project logs --dispatch 17          # one dispatch exactly
```

`--session` needs no `--name`: a session identifier already names its own loop.

The transcript is rendered rather than dumped — session start, the agent's text, each tool
call and its result, and a closing line with turns, cost and duration. Thinking blocks and
token counters are hidden.

| Flag | Effect |
|---|---|
| `-f`, `--follow` | Stream while the agent is alive, then stop. Following a finished run exits rather than hanging |
| `--thinking` | Include the agent's thinking blocks |
| `--raw` | Print the stream-json verbatim |
| `--stderr` | The agent's standard error instead |
| `--runner` | The runner's own log, for a dispatch that failed before the agent started |
| `--path` | Print the log file path and exit, for piping elsewhere |
| `--limit` | How many dispatches `--list` shows |

Each dispatch writes three files under `{state_dir}/logs/{loop}/`:

| File | Contents |
|---|---|
| `{kind}-{issue}-{timestamp}.jsonl` | The agent's stream-json transcript |
| `{kind}-{issue}-{timestamp}.jsonl.stderr` | The agent's standard error |
| `runner-{dispatch}.log` | The detached runner's own log |

All three are mode `0600`: a transcript records everything the agent read and ran.

## Configuration

One YAML file per loop, in `.agent-utils/configs/`. A loop's name is the `name:` field inside
the file, not the file name, and it must be unique within a directory.

Loop state lives in one database for the machine, at `~/.agent-utils/state.db`. Every row is
keyed by the project's UUID, so no project ever reads another's issue state, dispatches or
sessions. `state_dir` still holds each loop's tick lock and its log tree, under
`<project>/.agent-utils/state/<loop>/` by default. Only the database moved. `~/.agent-utils`
also holds `config.yaml` (the machine-wide settings `agent-utils config` edits — see
[Webhooks](#webhooks)), `env` (the `GITHUB_TOKEN` file `agent-utils config token` writes, read by
[Cron](#cron) and [Webhooks](#webhooks)), `listener.pid` and `listener.lock` (the
liveness source `listener stop` and `listener status` trust), and the webhook listener's own
logs.

`agent-utils project loop new` writes a loop file for you, by asking; **[`docs/configuration.md`](docs/configuration.md)
remains the reference for editing one by hand** — what each field means, what reads it, and
what happens if you get it wrong. `examples/planning.yaml` and `examples/execution.yaml` are
complete working files, ported from the reference planning and execution orchestrators.

## Migration

State used to live in one SQLite file per loop, at `{state_dir}/state.db`. Those files are
imported into the canonical database automatically, the first time any command touches the
project. There is nothing to run, and so nothing to forget.

The old files are never deleted. A `MIGRATED.txt` note is left beside one that has been read
for the last time. A runner started by the old binary keeps writing the old file, because an
upgrade does not change a running process, so that file stays open and is read again until it
is idle.

`agent-utils migrate` sweeps every registered project and prints what it did. Run it when you
want the report, not because anything waits on it. `--dry-run` writes no state and touches no
legacy file:

```
$ agent-utils migrate --dry-run
Dry run: no state was imported and no legacy file was touched.
Opening the canonical database still brought its schema up to date;
that part cannot be avoided.

Nothing left to import. Every registered project's state is already
in the canonical database.
```

## Security

A loop dispatches an agent that runs with permission prompts disabled, inside a git worktree,
on text written by other people. Issue bodies, issue comments, and pull request bodies are
UNTRUSTED input. An instruction hidden in a comment executes.

Point a loop only at a repository whose issue and pull request population you trust. The
engine reduces the blast radius in three ways, none of which is a substitute for that rule:

- The agent process gets a filtered environment and `GITHUB_TOKEN` is removed from it, at both
  hops (the detached runner and the agent itself).

  **This does not make the agent unprivileged.** It keeps `HOME` and `SSH_AUTH_SOCK`, because
  it has to push branches and use `gh`. Anything readable by your user is readable by the
  agent: `~/.config/gh/hosts.yml`, `~/.ssh`, git credential helpers, the `~/.agent-utils/env`
  file this README tells you to create, and `~/.agent-utils/config.yaml`, which holds the
  webhook HMAC secret. Treat the environment filter as
  defence in depth, not as a boundary. If you need a real boundary, run the loop as a separate
  user with its own `HOME` and a narrowly scoped token.
- Only a pull request opened by an OWNER, MEMBER, or COLLABORATOR, whose head branch lives in
  the target repository, is ever linked to an issue or tended.
- `bypassPermissions` requires `i_understand_bypass_permissions: true` in the loop config.

The webhook listener adds one more thing worth naming plainly: it accepts a request from the
internet that starts an agent. That is a stronger claim than "a cron job reads issues on a
timer," so it is verified accordingly. Every delivery is checked with HMAC-SHA256 over the raw
request body; a `sha1=` signature is refused no matter which header carries it, closing the
downgrade GitHub's own client library would otherwise allow; and an empty `webhook.secret`
makes the listener refuse to serve at all, rather than start up "verifying" deliveries against
a key an attacker also knows. What the listener changes is the delay: an issue comment used to
wait for the next cron tick before an agent read it, and now it does not. It does not change
the trust rule above — point a loop, webhook-driven or not, only at a repository whose issue
and pull request population you trust.

## Cron

Cron is optional. `loop tick` is what any driver runs — a cron entry, or the webhook listener
on a GitHub delivery — and the per-loop lock (`internal/lock`) makes it safe to run both at
once: an overlapping tick simply finds the lock held and exits rather than double-dispatching.
Keep the cron entry after the listener is running. It is not only a heartbeat for a quiet
repository or a proxy that is briefly down: the listener acts on the issue a delivery names,
so the full sweep is the only thing that notices a pull request that fell behind on someone
else's push, or a retry deadline on an issue nobody touched.

Do NOT put the token inline in the crontab. cron runs the whole line through `/bin/sh -c`, so
a `VAR=value command` prefix puts the token in the shell's argument list, where `ps` shows it
to every user on the machine.

Put it in a file instead:

```bash
agent-utils config token
```

That checks for a token you already have before asking for one: `$GITHUB_TOKEN` is used as it
stands, and otherwise a token from `gh auth token` is offered as the prompt's default, which
Enter accepts. Neither is ever echoed — you see a masked fingerprint like `ghp_…AB12` — and with
neither available it prompts for the token without echoing it. Either way it writes
`~/.agent-utils/env` at mode `0600`, creating the file if it is not there and leaving every
other line in it alone. Scripting a machine build? Pipe it in — `echo "$TOKEN" | agent-utils
config token`, which beats `$GITHUB_TOKEN` when both are there — or write the file by hand,
which is all the command is doing:

```bash
install -m 600 /dev/null ~/.agent-utils/env
echo 'export GITHUB_TOKEN=ghp_...' >> ~/.agent-utils/env
```

```cron
*/15 * * * * . $HOME/.agent-utils/env && /usr/local/bin/agent-utils project --name lawndominator loop tick --name planning >> $HOME/.agent-utils/planning.log 2>&1
```

Naming the project explicitly is what makes the entry independent of cron's working directory.
Passing `--config` with an absolute path works too and skips discovery entirely.

## Webhooks

The webhook listener turns a GitHub delivery — an issue labeled, a comment posted, a pull
request updated — directly into a `loop tick`, instead of waiting for the next cron interval.
A delivery acts on the issue it names, and on nothing else: it says "something about this
issue changed, figure out what and dispatch the right executor." The daemon fetches that one
issue, decides it, and stops — it does not read every open issue and every open pull request
in the repository, which is what a full reconcile costs in tokens and rate limit on every
delivery, per project watching that repository. Pull requests share the issue number space, so
a `pull_request` event is resolved to the issue its pull request closes (`Closes #N` in the
body); a pull request that closes no issue is a no-op. The `accepted delivery` line in the log
names the issue, so a delivery can be matched against the dispatch it caused.

The daemon is the fast path; cron remains the safety net. A `loop tick` is still a full sweep,
and it is what catches the work no event names — a pull request that fell behind because
someone pushed to the default branch (a `push` event, which this daemon does not subscribe to),
or a retry deadline on an issue nobody touched. Both can run at once: the per-loop lock makes
an overlapping tick harmless.

Set it up in this order:

```bash
agent-utils config token                 # uses $GITHUB_TOKEN or gh's token if there is one, else
                                         # prompts without echoing; writes ~/.agent-utils/env 0600
agent-utils config webhook --enable --url https://hooks.example.com/webhook
agent-utils project register-webhook
agent-utils listener start --daemon
```

`agent-utils listener start` speaks plain HTTP and never terminates TLS itself — it expects
nginx, cloudflared, or ngrok in front of it to do that. `webhook.url` is therefore the proxy's
public URL, not the listener's own bind address, and it must be `https`: over plain HTTP both
the delivery body and the `X-Hub-Signature-256` header that authorizes running an agent would
cross the internet in the clear, replayable by anyone who observed one. The only exception is
a loopback host (`http://localhost:...` or `http://127.0.0.1:...`), allowed so a local
end-to-end test needs no certificate. The listener itself binds `127.0.0.1:8787` by default —
`0.0.0.0` would accept deliveries from anything on the local network before you asked for
that — so the reverse proxy is also what makes it reachable from GitHub at all. Point the proxy
at `POST /webhook`; it also serves `GET /healthz`, unauthenticated, for the proxy's own health
check. Change the bind address or port with `agent-utils config set webhook.listen_addr` /
`webhook.listen_port`, or override either for a single run with `listener start --listen-addr`
/ `--listen-port` (the `--daemon` form writes its override into the launchd plist, not into
`config.yaml` — `config show` still reports the configured value, not what the installed agent
actually binds).

As it comes up, a foreground `listener start` prints the routing table it will use — every
repository it will accept deliveries for, and the loops each one dispatches. This is the
"did my setup work" check, and it is the first thing to read when a webhook seems to do
nothing:

```
listening for deliveries on 2 repositories (configured locally; GitHub is not asked):
  acme/widgets
    widgets/planning
  mcgarylabs/lawndominator-monorepo
    lawndominator/execution
    lawndominator/planning

skipped, and therefore not routed:
  ghost (/Users/you/old-project/.agent-utils): the project directory no longer exists
  lawndominator/broken.yaml: cannot load config: parse config ...: yaml: unmarshal errors: line 2: field this_key_does_not_exist not found in type config.Config
```

That table comes from the loop configurations on THIS machine, and nothing else: it does not
ask GitHub whether a webhook is really registered for those repositories, which would need an
API call per repository and a token on the startup path (`project register-webhook` is where
that happens). The `skipped` block is everything the same scan a delivery uses had to pass
over — a registered project whose directory is gone, a project with no `configs/` directory,
a loop file that does not load — so a silent misconfiguration shows up at startup rather
than never. `--daemon` prints nothing of the sort: it installs the launchd agent and returns
without serving; the agent it installs writes this table to
`~/.agent-utils/listener.stdout.log` at every login.

When the scan finds no loops at all, the banner says so loudly, because that daemon still
verifies signatures and returns 200 for every delivery before doing nothing with it:

```
NOT LISTENING FOR ANYTHING: no loop on this machine watches any repository.
This listener will still verify and accept GitHub deliveries, and then do
nothing with them. Either:
  * no project is registered on this host -- run `agent-utils project init`
    in each project, and `agent-utils list` to see what is registered; or
  * no loop configuration in .agent-utils/configs declares a `repo:`.
```

The listener needs the same `~/.agent-utils/env` file the [Cron](#cron) section has you
create, with `GITHUB_TOKEN` in it: `listener start` refuses to start without it, and once
running, the daemon re-reads it on every delivery so a rotated token needs no restart. If you
have not stored one yet:

```bash
agent-utils config token
```

`listener start` also offers that prompt itself when the file does not exist and you are at a
terminal, so the setup above works even if you skip this step — and it discovers a token the
same way, so with `$GITHUB_TOKEN` set or `gh` logged in there is nothing to type. It only
offers: with no terminal
(launchd, cron, CI) it fails with instructions instead, because a prompt nobody can answer
would hang the daemon forever. And it only offers for a MISSING file — a wrong mode, a symlink,
or a file owned by another account still fails outright, since something put a credential file
into that state and you should look at it rather than overwrite it.

`agent-utils project register-webhook` reads the repositories your project's loops watch and
registers (or updates) a GitHub webhook on each, pointed at `webhook.url` and signed with
`webhook.secret`. It asks for confirmation before it does — this grants GitHub the right to
trigger agent dispatch — unless you pass `--yes`, and it refuses to run unattended (no
terminal, no `--yes`) for the same reason a config wizard does. Run it again after
`config webhook --rotate-secret`: GitHub returns a hook's secret obfuscated, so there is no way
to detect that a hook's secret is stale short of always re-pushing it.

Each registration is recorded in the canonical state database, keyed by project and
repository, with the hook id GitHub assigned and the URL it was registered with.
`agent-utils project status` prints that under `WEBHOOKS`, one line per repository, so
"is a webhook actually registered for this repo, and which one" is answerable without
opening GitHub:

```
WEBHOOKS
  acme/widgets                         hook 512334891 (https://hooks.example.com/webhook)
  acme/quiet-repo                      not recorded
```

The id is what makes a changed `webhook.url` survivable. Registration matches the recorded
hook first and only then falls back to matching a delivery URL, so changing `webhook.url` and
re-running `register-webhook` MOVES the existing hook to the new endpoint instead of creating
a second one beside it — which is what used to happen, leaving the first hook delivering to a
dead address with nothing on this machine naming it.

`agent-utils project deregister-webhook` is the other half: it deletes each repository's hook
at GitHub **by the recorded id**, then forgets the record. It takes the same `--name <loop>`
selector and the same `--yes` rule as registration, and it needs neither `webhook.url` nor
`webhook.secret` — deleting addresses the hook by id and writes no secret — so it still works
after you have unset the webhook configuration. Three cases are worth knowing:

* **A hook you already deleted in GitHub's UI** answers 404. That is treated as success: the
  record is removed and the command exits zero, because failing would leave a row nothing
  could ever clear. (GitHub answers a token missing the `admin:repo_hook` scope with 404 too,
  so the message says as much — if deliveries continue, check the scope.)
* **A repository registered before ids were recorded** has no record, so the command falls
  back to finding a hook whose URL matches the current `webhook.url`, and says that is what
  it did. If neither exists, it reports "nothing to deregister" and exits zero.
* **A hook two projects share** — both watching one repository through one `webhook.url`, so
  the second registration found and edited the first's hook and recorded the same id — is
  REFUSED. Deleting it would silently stop the other project's deliveries, so the command
  names the other projects and their paths instead of guessing. `--force` overrides it and
  says plainly which projects just lost delivery; run `deregister-webhook` in each of them
  afterwards to clear their now-stale records.

`agent-utils listener start --daemon` installs the listener as a launchd user agent (macOS
only) instead of running it in your terminal — `RunAtLoad` and `KeepAlive`, so it starts at
login and restarts if it dies. Without `--daemon` it just runs in the foreground, useful for
watching its logs while you get the proxy working. `agent-utils listener status` reports
whether it is installed and running; `agent-utils listener stop` removes it, or signals a
foreground instance, or both.

**A launchd agent with `RunAtLoad` and `KeepAlive` is permanent login-time execution of
whatever binary path it names**, so `listener start --daemon` refuses to install itself when
that binary, or any parent directory of it, is group- or world-writable — another local
account could otherwise replace the binary launchd runs at every login. This is a real
operator gotcha on an Intel Mac with Homebrew, where `/usr/local/bin` is commonly
`drwxrwxr-x`, owned by group `admin`, and the refusal fires the first time you try `--daemon`.
The fix is the same either way: move the binary to a location only you can write to (for
example `~/bin`) and run `listener start --daemon` again.

Every other step in this section that widens what can reach the machine is gated — a typed
confirmation, an acknowledgement flag, a flat refusal. `config set webhook.listen_addr 0.0.0.0`
is the one that is not: it prints nothing and takes effect on the next `listener start`, and
the endpoint it widens is the one that starts agents. Treat it the way you would treat any
other change that opens a port to the LAN — deliberately, and behind your own firewall rule if
this machine is not already trusted network-wide.

## Upgrading

**`retry.backoff_ticks` was renamed to `retry.backoff`, and its values are now durations, not
tick counts.** A tick was a fixed interval only under cron; the webhook listener can tick a
loop at any moment, so a count of ticks no longer names a stable wait. A config that still sets
the old key fails to load, naming the new one as the replacement. Multiply the old tick count
by the cron interval you were running to get a starting duration:

```yaml
# before
retry:
  backoff_ticks: [0, 1, 2]

# after, on a 15-minute cron interval
retry:
  backoff: [0s, 15m, 30m]
```

**Implicit project onboarding is gone.** Before this branch, a directory with a hand-assembled
`.agent-utils/configs/` in it was a project the moment any command looked at it — the directory
itself was the only signal required. See [Quick start](#quick-start) for why that stopped being
enough: `~/.agent-utils` looks like an ordinary directory too, and nothing stood between a stray
`cd ~` and registering the machine-wide directory as a project. Now a project needs
`.agent-utils/config.yaml`, and only `project init` writes it. If you built loop configs by hand
and never ran `project init` or `project loop new` in that directory, commands that used to find
the project now fail — run `agent-utils project init --no-loop` there once to mint the missing
descriptor without touching the loop files already in `configs/`, and everything that pointed at
the directory before resumes working.

## Versioning and releases

The semantic version lives in the `VERSION` file at the repository root. It is the single
source of truth; nothing infers a version from a tag.

`scripts/version.sh <ref>` reconciles the file with the git ref:

| Ref | Behaviour |
|---|---|
| Not a tag | Rewrites `VERSION` to `<version>+<short-sha>` so the build identifies its commit |
| `refs/tags/vX.Y.Z` matching `VERSION` | Leaves the file alone; the release ships a bare semantic version |
| `refs/tags/vX.Y.Z` **not** matching | Exits non-zero and fails the build |

That last row is the whole point: a tag cannot produce a release whose binary reports a
different version than the tag says.

```bash
make build && ./bin/agent-utils version
# agent-utils v0.4.0 (d6e9df9)
```

A `go install` binary has no linker stamp, so it falls back to the module version and VCS
revision the Go toolchain embeds. A build from a dirty tree is marked `-dirty`.

### Cutting a release

1. Bump `VERSION` (e.g. `v0.4.0`) and merge it to the default branch.
2. Tag that commit with exactly the same string and push the tag:

   ```bash
   git tag v0.4.0 && git push origin v0.4.0
   ```

The release workflow verifies the tag is an ancestor of the default branch, verifies `VERSION`
equals the tag, builds four static binaries, and publishes a GitHub release with generated
notes.

| Artifact | Platform |
|---|---|
| `agent-utils-linux-amd64-<version>.tar.gz` | Linux x86-64 |
| `agent-utils-linux-arm64-<version>.tar.gz` | Linux ARM64 |
| `agent-utils-darwin-amd64-<version>.tar.gz` | macOS Intel |
| `agent-utils-darwin-arm64-<version>.tar.gz` | macOS Apple Silicon |

Every binary is built `CGO_ENABLED=0` and is fully static, which is possible because every
dependency is pure Go — `modernc.org/sqlite` was chosen partly for this. One Linux binary runs
on any distribution with no libc to match.

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

## Development

```bash
make deps        # install golangci-lint and staticcheck
make build       # build ./bin/agent-utils, version stamped in
make check       # fmtcheck + vet + lint + test, in that order
```

| Target | Does |
|---|---|
| `make all` | `deps` then `build` |
| `make build` | Build `./bin/agent-utils` with the version and commit stamped in |
| `make install` | Same, into `GOBIN` |
| `make test` | Full suite, cache disabled, one package at a time |
| `make test/race` | Same under `-race`. Roughly 3x slower; worth it before a release |
| `make test/verbose` | Full suite with per-test output |
| `make cover` | Coverage profile plus a total |
| `make lint` | `golangci-lint` |
| `make vet` | `go vet` |
| `make fmt` | `gofmt -w .` |
| `make fmtcheck` | Fail if anything is unformatted |
| `make check` | Everything that must pass before pushing |
| `make release` | Cross-compile and bundle all four platforms |
| `make clean` | Remove build output and coverage |

Tests run with `-p 1` and no cache on purpose: the `worktree` package shells out to real git
and `runner` spawns real processes, so package-level parallelism is not safe, and a cached
PASS is not evidence about the working tree.
