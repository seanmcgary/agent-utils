# Webhook listener: event-driven loop ticks

## Purpose

Today a loop ticks only when cron runs `agent-utils project loop tick`. The interval sets the
latency: an issue labelled at 12:01 waits until 12:15. Cron also runs when nothing changed, so
most ticks do a GitHub API read and exit.

This design adds a daemon. The daemon receives GitHub webhook deliveries and ticks the loops
that watch the repository. A label change starts an agent in about one second. Cron is no
longer necessary.

The daemon does not replace the tick. It calls the same code that the command calls.

## Scope

In scope:

- A machine-wide configuration file at `~/.agent-utils/config.yaml`.
- A top-level `config` command that reads and writes that file.
- A `project register-webhook` command that creates the webhook at GitHub.
- A `listener` command that runs the daemon, with a `--daemon` flag for launchd.
- A `project init` command, and the end of implicit project onboarding.
- A refactor that lets the daemon and the command share one tick path.
- A change from tick-counted retry backoff to wall-clock retry backoff.

Out of scope:

- A GitHub App. The design uses a repository webhook and a personal access token.
- Linux service files. The launchd code goes behind an interface so systemd can follow.
- TLS. The daemon speaks plain HTTP behind a proxy that terminates TLS.
- Delivery replay or a durable event queue.

## Premise check

The findings below come from reading the code. Each one changed the design.

### Entry path

One path reaches a tick today:

1. `cmd/agent-utils/main.go` `loopCommand()` declares `loop tick`.
2. The action calls `openProject`, then `resolveLoopConfig`.
3. The action calls `setup()` at `cmd/agent-utils/main.go:686`.
4. The action takes a lock with `lock.Acquire`.
5. The action calls `loopcmd.Tick` at `internal/loopcmd/tick.go:66`.

`setup()` is in package `main`. No package outside `cmd/agent-utils` can build a
`loopcmd.Deps`. The daemon must therefore have this function moved to an importable package.
This is a precondition, not a preference.

The repository contains no HTTP server. A search for `webhook`, `listener`, and `launchd`
returns no result.

### Blast radius

The change touches one repository. No other program consumes this code.

| File | Change |
|---|---|
| `cmd/agent-utils/main.go` | `setup()` moves out. Three command groups are added. |
| `internal/loopcmd/` | New `Open` and `RunTick` functions. |
| `internal/engine/engine.go:146-165` | `retryDecision` reads a timestamp, not a tick count. |
| `internal/store/store.go:32-46` | The `issues` table gets a `retry_after` column. |
| `internal/store/store.go:271-298` | The `addedColumns` list gets one entry. |
| `internal/config/config.go` | `retry.backoff_ticks` becomes `retry.backoff`. |
| `internal/ghub/` | The client gets the repository hooks API. |
| `examples/planning.yaml:44` | `backoff_ticks: [0, 1, 2]` becomes durations. |
| `examples/execution.yaml:36` | The same change. |
| `docs/configuration.md:150,535-548` | The retry reference changes. |
| `README.md` | The cron section and the security section change. |
| `internal/settings/` | New package. |
| `internal/listener/` | New package. |
| `internal/service/` | New package. |

### Prior art

Reuse these. Do not write a second version of any of them.

- `registry.write` in `internal/registry/registry.go` writes a temporary file and renames it,
  at mode 0600. `settings.Save` uses the same method.
- `home.Dir` resolves the machine-wide directory and honours `$AGENT_UTILS_HOME`.
  `settings` resolves its path through `home`, so a test can redirect it.
- `lock.ErrHeld` in `internal/lock/lock.go` reports that another tick holds the lock. This is
  the behaviour the design wants when deliveries overlap.
- `store.addedColumns` at `internal/store/store.go:271` adds a column to an existing database.
- `registry.lockRegistry` shows the flock pattern for a read-modify-write on a shared file.

### Contradictions found

Six findings contradict the first shape of the design.

**1. `github.ValidatePayload` accepts a SHA-1 signature, in two different ways.**

At `messages.go:256-260` the function reads `X-Hub-Signature-256`. If that header is absent,
the function reads `X-Hub-Signature`, which is HMAC-SHA1.

Reading the SHA-256 header directly is **not** enough. At `messages.go:149-176` `messageMAC`
selects the hash function from the signature string's own prefix, not from the header name:

```go
switch sigParts[0] {
case sha1Prefix:   hashFunc = sha1.New
case sha256Prefix: hashFunc = sha256.New
case sha512Prefix: hashFunc = sha512.New
```

So `X-Hub-Signature-256: sha1=<hmac-sha1>` is verified with SHA-1.

The listener therefore does three things. It reads `github.SHA256SignatureHeader` itself and
rejects an empty value with 400. It rejects a signature that does not begin with `sha256=`
with 400. It then calls `github.ValidatePayloadFromBody`, which does the constant-time
comparison. The listener also wraps the body in `http.MaxBytesReader`.

A third trap sits beside these two: at `messages.go:230-236` the library validates only when
the secret or the signature is non-empty, and an empty secret makes the HMAC key empty. The
listener refuses to serve when the configured secret is empty.

**2. `proc.IsAlive` cannot report on the listener.**

At `internal/proc/proc.go:63` the function matches the command line against `--dispatch <id>`.
The listener carries no such argument. `listener status` and `listener stop` need a different
check: a pidfile, `syscall.Kill(pid, 0)`, and a command-line match on `listener`.

**3. `setup()` reads the token from the process environment.**

At `cmd/agent-utils/main.go:709` the function calls `os.Getenv("GITHUB_TOKEN")`. Requirement 7
says the daemon reads `~/.agent-utils/env` again on each tick, so a rotated token needs no
restart. That is not possible while the function reads the process environment.

`loopcmd.Open` therefore accepts the token as a field of its options. The command passes
`os.Getenv("GITHUB_TOKEN")`. The daemon passes the value it read from the file.

**4. The retry backoff is the only tick-counted policy.**

`engine.retryDecision` compares `st.TickCount - state.LastRetryTick` against
`cfg.Retry.BackoffTicks[state.RetryCount]`. The circuit breaker already uses a wall-clock
deadline in the `cooldowns` table. Only the retry backoff needs to change.

**5. `config.Load` is strict, so a renamed key breaks every existing file.**

`internal/config/config.go:97` calls `dec.KnownFields(true)`. A file that carries
`backoff_ticks` after the rename fails to load with an unknown-field error. Both example
files carry that key.

The design keeps the strict parser and adds a targeted error. See "Retry backoff" below.

**6. `config.yaml` names two different files.**

`internal/project/project.go:31` sets `project.FileName = "config.yaml"`, inside a project's
`.agent-utils` directory. The new machine-wide file has the same base name inside the
machine-wide `.agent-utils` directory. Only the directory tells them apart.

If an operator sets `$AGENT_UTILS_HOME` to a project's `.agent-utils` directory, one file is
read by two strict parsers. Each parser rejects the other's keys.

The two files collide because onboarding is implicit, not because the name is shared.
`config.FindDir` walks from the working directory to the filesystem root and does not stop at
the home directory, so `~/.agent-utils` — which exists on any machine that has run this tool —
is a parent of everything under `~`. A `project` command run from a directory that is not
inside a project therefore adopts the machine-wide directory as a project directory, and
`ResolveProject` writes a descriptor into it. This happens today, without this feature.

The design fixes the cause and keeps a guard for the symptom:

- `FindDir` skips a candidate equal to the machine-wide directory.
- A new `agent-utils project init` command creates a project's directory, mints its descriptor,
  and registers it. `ResolveProject` stops minting a descriptor on its own.
- `settings.Load` and `settings.Save` both refuse a file that carries the `id` and `name` keys
  of a project descriptor, which still covers an `$AGENT_UTILS_HOME` pointed at a project.

## Design

### Component map

```
GitHub  --delivery-->  internal/listener  --RunTick-->  internal/loopcmd
                              |                              |
                              | reads                        | reads
                              v                              v
                       internal/settings              internal/store
                       internal/registry              internal/engine
                       internal/config
```

### Machine-wide configuration

A new package `internal/settings` owns `~/.agent-utils/config.yaml`. No existing package fits.
`internal/config` describes a loop. `internal/project` describes a project. `internal/home`
resolves directories only.

The file:

```yaml
# ~/.agent-utils/config.yaml    mode 0600
webhook:
  enabled: true
  url: https://hooks.example.com/agent-utils
  listen_addr: 127.0.0.1
  listen_port: 8787
  secret: <64 hexadecimal characters>
```

| Field | Type | Meaning |
|---|---|---|
| `webhook.enabled` | bool | The listener refuses to start when this is false. |
| `webhook.url` | string | The public URL. GitHub sends deliveries here. |
| `webhook.listen_addr` | string | The address the daemon binds. Default `127.0.0.1`. |
| `webhook.listen_port` | int | The port the daemon binds. Default `8787`. |
| `webhook.secret` | string | The HMAC secret. One secret for the machine. |

Rules:

- `Load` returns a zero value when the file is absent. Every existing command keeps working.
- `Save` writes a temporary file and renames it, at mode 0600.
- `Save` keeps the header comment.
- The default `listen_addr` is `127.0.0.1`. A daemon that binds `0.0.0.0` by default would
  accept deliveries from the local network before the operator asks for that.

### The `config` command

```
agent-utils config show [--reveal]
agent-utils config get <key>
agent-utils config set <key> <value>
agent-utils config unset <key>
agent-utils config webhook --enable|--disable [--url U] [--port N] [--addr A] [--rotate-secret]
```

`show` prints the file. The secret prints as `***redacted***`. `--reveal` prints the true
value, which an operator needs to register a hook by hand.

`get`, `set`, and `unset` take a dotted key. The key set is fixed and typed. An unknown key is
an error. A value of the wrong type is an error. Nothing is written until every check passes.

`config webhook` is the shortcut. It writes every field the feature needs in one call, and it
validates them together. `--enable` with no URL, and no URL already stored, is an error: a
half-configured file is worse than a rejected command.

`--enable` mints the secret when the file has none. The secret is 32 bytes from `crypto/rand`,
hex encoded. `--rotate-secret` mints a new one and prints which repositories need
registration again.

### Registering the webhook

```
agent-utils project register-webhook [--name <loop>] [--yes]
```

The command resolves the project the same way every other `project` command does. It reads the
project's loop configurations and collects the distinct `repo:` values. `--name` limits the
work to one loop.

The command asks before it writes, because a webhook grants GitHub the right to trigger an
agent. `--yes` skips the question. The question appears only when stdin is a terminal; a
non-interactive run without `--yes` gets an error that lists the repositories. That rule is
already written into `resolveLoopConfig`: a prompt in a cron job hangs forever.

(The flag was named `--all` in an earlier draft of this design. It is `--yes`, because it
answers a confirmation rather than widening a selection.)

For each repository the command:

1. Reads `webhook.url` and `webhook.secret` from the machine-wide file. An empty value is an
   error that names the `config webhook` command to run.
2. Calls `Repositories.ListHooks`.
3. Finds a hook whose `Config.URL` equals `webhook.url`.
4. Calls `EditHook` when it finds one, and `CreateHook` when it does not.

The operation is idempotent. Running it twice creates one hook.

The hook subscribes to five events:

- `issues`
- `issue_comment`
- `pull_request`
- `pull_request_review`
- `pull_request_review_comment`

The hook sets `content_type: json` and `insecure_ssl: "0"`.

The token needs `admin:repo_hook` scope. The command reports a 404 from the hooks endpoint as
a missing scope, because GitHub returns 404 rather than 403 for a token without it.

### The listener

```
agent-utils listener start [--daemon] [--listen-port N] [--listen-addr A]
agent-utils listener stop
agent-utils listener status
```

`start` without `--daemon` runs the server in the foreground and logs to standard output.
`--daemon` writes a launchd agent and bootstraps it. `stop` boots the agent out. `status`
reports whether the daemon runs, its pid, and its address.

The port comes from the machine-wide file. `--listen-port` overrides it for one run.

**Request handling.** The server has two endpoints.

- `POST /webhook` receives a delivery.
- `GET /healthz` returns 200. A proxy uses it.

The delivery handler:

1. Rejects a request whose method is not POST, with 405.
2. Wraps the body in `http.MaxBytesReader` at 5 MiB.
3. Reads `X-Hub-Signature-256`. An empty value gives 400.
4. Calls `github.ValidatePayloadFromBody` with the machine-wide secret. A failure gives 401.
5. Reads `X-Github-Event`. An event outside the five subscribed events gives 204.
6. Decodes only the `repository.full_name` field from the payload. The handler needs nothing
   else: the payload says that something changed, and the tick reads the true state from the
   GitHub API.
7. Answers 202 and starts the work in a goroutine.

The handler answers before the work finishes. GitHub times a delivery out after 10 seconds. A
tick calls the GitHub API several times and can start an agent, so it does not fit in that
budget.

**Routing.** The worker finds the loops to tick:

1. `registry.List()` returns every project.
2. A project whose directory is absent is skipped, with a log line. `registry.Project.Exists`
   already reports this.
3. `config.List(project.AgentUtilsDir)` returns the loops. A loop that fails to load is
   skipped, with a log line.
4. A loop whose `repo:` equals the delivery's `repository.full_name`, compared without case,
   is selected.

The scan reads a few small YAML files for each delivery. That costs less than the GitHub API
call the tick then makes. A cached index would serve a stale answer after an operator adds a
loop, so the design reads the files each time.

**Serialization.** Each selected loop is ticked. `loopcmd.RunTick` takes the loop's lock. When
`lock.Acquire` returns `ErrHeld` the worker logs and returns. No event is queued and none is
retried for this reason.

This is safe because the delivery carries no information the tick needs. The tick already
running reads the same GitHub state a moment later than the dropped one would have.

**Retry of a failed tick.** A tick that returns an error is a different case. The daemon holds
an in-memory schedule keyed by project and loop. A failed tick is scheduled again after a
delay. The delay follows the same `retry.backoff` list the issue retry uses, and the daemon
gives up after `retry.max` attempts, at which point the next delivery or the retry timer for
an issue starts the work again.

**Waking for an issue retry.** An issue in backoff needs a tick when its deadline passes. The
daemon asks the store for the earliest `retry_after` across every project and loop, and sets a
timer for it. When the timer fires the daemon ticks that loop. Without this a backed-off issue
waits for unrelated repository activity.

**Token.** The daemon reads `~/.agent-utils/env` before each tick. It parses lines of the form
`export KEY=value` and `KEY=value`, and it takes `GITHUB_TOKEN`. It does not change its own
environment: it passes the value to `loopcmd.Open`. A rotated token therefore needs no
restart. The daemon refuses to start when the file is readable by group or other.

### The service manager

`listener start --daemon` writes
`~/Library/LaunchAgents/com.seanmcgary.agent-utils.listener.plist` and runs
`launchctl bootstrap gui/$UID <path>`. `listener stop` runs `launchctl bootout`.

The plist:

- `ProgramArguments` is the absolute path of this binary, then `listener`, then `start`.
- `RunAtLoad` is true.
- `KeepAlive` is true, so a crash restarts the daemon.
- `StandardOutPath` and `StandardErrorPath` are `~/.agent-utils/listener.log` and
  `listener.err.log`.
- `EnvironmentVariables` carries no token. The daemon reads the token from the file.

The plist is written at mode 0644, which is what launchd expects, and it holds no secret.

A `service.Manager` interface hides launchd:

```go
type ServiceManager interface {
    Install(binary string, args []string) error
    Uninstall() error
    Status() (Status, error)
}
```

The package is `internal/service`, named for the concept rather than the tool, because systemd is meant to follow. The launchd implementation goes in `service_darwin.go`. A build for another platform gets a
stub that reports that `--daemon` is not supported yet. This is what lets systemd follow
without a change to the command.

### Retry backoff: from ticks to wall clock

**This is a breaking change to the loop configuration. It needs the reader's decision.**

Today `retry.backoff_ticks` is a list of integers. `engine.retryDecision` compares
`st.TickCount - state.LastRetryTick` against the entry for the current retry count. Cron makes
a tick a fixed length of time, so `[0, 1, 2]` means "now, one interval later, two intervals
after that."

A webhook makes a tick an arbitrary length of time. Three comments in ten seconds are three
ticks. A backoff of two ticks then means twenty seconds, not thirty minutes. The policy exists
for an inference API outage, so a window measured in events is the wrong unit.

The change:

1. `retry.backoff_ticks` becomes `retry.backoff`, a list of durations.
   `backoff_ticks: [0, 1, 2]` becomes `backoff: [0s, 15m, 30m]`.
2. The `issues` table gets a `retry_after` column, of type `INTEGER`, holding Unix seconds.
   Zero means no deadline. The `addedColumns` list at `internal/store/store.go:271` carries
   the entry, so an existing database gains the column on the next open.
3. `engine.retryDecision` compares `now` against `state.RetryAfter`.
4. The tick writes `retry_after` when it records a retry, as `now + cfg.Retry.Backoff[n]`.
5. `last_retry_tick` stays in the table and stops being read. Dropping a column costs a table
   rebuild and buys nothing.

Compatibility: `config.Load` reports a specific error when it reads `backoff_ticks`. The error
names the new key and shows the value to write. An operator sees an instruction, not an
unknown-field error.

Both example files change. The reference in `docs/configuration.md` changes.

### Shared tick path

Two functions move the shared code into `internal/loopcmd`.

```go
// Options carries what Open cannot derive.
type Options struct {
    Token           string          // the GitHub token; empty when the caller needs no API
    RequireGitHub   bool
    MigrationPolicy MigrationPolicy
}

// Open builds the dependencies one loop needs.
func Open(ref ProjectRef, configPath string, opts Options) (*config.Config, Deps, func(), error)

// RunTick takes the loop's lock and runs one tick.
// It returns ErrLockHeld when another tick holds the lock.
func RunTick(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error)
```

`Open` holds the body of today's `setup()`. `RunTick` holds the lock-and-tick body of today's
`loop tick` action. The command action and the daemon worker both call these. There is no
second copy of the logic to keep in step.

`projectRef` and `migrationPolicy` move from `main.go` to `loopcmd` and become exported.

## Security

The listener accepts a request from the internet that starts an agent with permission prompts
disabled. This is the most sensitive surface in the program.

| Risk | Control |
|---|---|
| A forged delivery starts an agent. | HMAC-SHA256 over the raw body, constant-time compare. The SHA-1 header is never read. |
| A delivery with no signature. | Rejected with 400 before the body is parsed. |
| A large body exhausts memory. | `http.MaxBytesReader` at 5 MiB. |
| The secret leaks through a terminal. | `config show` redacts it. `--reveal` is explicit. |
| The secret leaks through a file. | Mode 0600, checked on read. |
| The token leaks through the plist. | The plist holds no token. |
| The token leaks through `ps`. | The token is never a flag. It comes from a file. |
| The daemon is reachable from the network. | The default bind address is `127.0.0.1`. |
| A timing attack on the secret. | `github.ValidatePayloadFromBody` uses `hmac.Equal`. |

The listener does not raise the trust level of the repository's contents. The README already
states that issue text is untrusted and that an instruction hidden in a comment executes. A
webhook shortens the delay before that happens; it does not change the rule. The README gains
a sentence that says so.

## Testing

| Area | Test |
|---|---|
| `settings` | Load with no file returns a zero value. Save then Load round-trips. Save writes 0600. A project descriptor in the path gives a clear error. |
| `config` command | `set` rejects an unknown key and a bad type. `show` redacts. `webhook --enable` with no URL fails and writes nothing. |
| signature | A valid SHA-256 signature passes. A wrong one gives 401. An absent one gives 400. A SHA-1 signature alone gives 400. |
| routing | A delivery selects the loops whose repo matches, across two projects. A missing project directory is skipped. An unparsable loop file is skipped. |
| serialization | A held lock drops the delivery and returns no error. |
| backoff | `retryDecision` waits until `retry_after`. A tick writes the deadline. |
| migration | An existing database gains `retry_after` on open and keeps its rows. |
| config compatibility | A file with `backoff_ticks` gives the migration error. |
| `register-webhook` | A second run edits rather than creates. A 404 reports the missing scope. |
| launchd | The plist renders with the expected keys and holds no token. |

The `listener` tests use `httptest.Server` and a fake tick function. They start no agent.

## Open decisions for the reader

1. **The backoff change is breaking.** Every loop configuration with `retry.max > 0` needs an
   edit. Approve the rename, or ask for the tick-counted behaviour to stay.
2. **The default bind address is `127.0.0.1`.** This needs a tunnel or a proxy on the same
   host. Say so if the daemon must bind a routable address.
3. **The launchd label is `com.seanmcgary.agent-utils.listener`.**
