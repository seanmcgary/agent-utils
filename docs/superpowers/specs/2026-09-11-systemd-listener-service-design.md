# Design: systemd listener service and the listener verb split

Two changes that land together. The first adds a systemd backend to `internal/service`, so a
Linux machine can register the webhook listener as a managed service. The second replaces the
`listener start --daemon` and `listener stop` verbs with `listener run`, `listener install`, and
`listener uninstall`, so that service registration and process control are separate commands.

The two changes are one unit of work. The new backend needs an install verb that does not also
run the listener, and the verb split is not worth doing for launchd alone.

## Premise and blast-radius check (step-0 findings)

### Entry path

1. `listenerCommand` (`cmd/agent-utils/listener.go:62`) registers three subcommands: `start`,
   `stop`, and `status`.
2. `listenerStartCommand` (`listener.go:75`) validates the settings, the webhook secret, and the
   GitHub token. It then branches on the `--daemon` flag (`listener.go:138`).
3. With `--daemon`, it calls `installDaemon` (`listener.go:209`). Without it, it calls
   `runListener` (`listener.go:147`) and serves in the foreground.
4. `installDaemon` calls `service.New()` (`internal/service/service.go:69`), which calls
   `newManager()`.
5. `newManager` resolves to `darwinManager` on darwin (`service_darwin.go:63`) and to
   `otherManager` on every other platform (`service_other.go:16`).
6. `otherManager` returns `errUnsupported` from all four methods (`service_other.go:13`). The
   text of that error says service management is supported on macOS only.

### What the request finds in the code today

- **A systemd backend — ABSENT.** `service_other.go` is a stub. Its build tag is `!darwin`, so
  it is what a Linux build compiles.
- **The `Manager` seam — PRESENT and sufficient.** `Manager` (`service.go:44`) declares
  `Install`, `Uninstall`, `Status`, and `ServiceFilePath`. No method is launchd-specific. The
  package doc (`service.go:1`) states that systemd is meant to follow launchd.
- **The self-install security check — PRESENT, but darwin-only.** `resolveSelf`
  (`service_darwin.go:99`) resolves the running binary through `os.Executable` and
  `filepath.EvalSymlinks`. `refuseIfWritableByOthers` (`service_darwin.go:117`) then refuses any
  path with a group-writable or world-writable component. Both live behind the darwin build tag.
- **A service-definition renderer — PRESENT for launchd only.** `plist.go` renders the launchd
  property list through `encoding/xml`, which escapes every caller value.
- **No PR review bot.** The three most recent merged pull requests carry no bot review and no bot
  comment. Continuous integration status and human comments are the only review signals.

The premise holds. Nothing in the request already exists.

### Blast radius

All of it is in this repository.

- `internal/service` — the new systemd backend, the new unit renderer, and the move of the
  self-install check into shared code.
- `cmd/agent-utils/listener.go` and its tests — the verb split.
- `Makefile` — a `GOOS=linux go vet` line, so a darwin developer still type-checks the Linux file.
- `README.md` and `docs/configuration.md` — the new verbs and the Linux walkthrough.

### Profile and class

- **Profile:** backend. The change touches process supervision, privilege, and the command-line
  surface. No user interface is involved.
- **Class:** Large. The change installs a root-owned unit that runs at every boot, and it calls
  `sudo`. That is a security boundary. It also removes two published commands, which is a
  contract change for every existing user.

## Decisions taken in design

The user made these five decisions during the design discussion. They are recorded here because
each one closes an alternative that a reader would otherwise expect.

1. **A system unit, not a user unit.** The unit goes in `/etc/systemd/system`. A user unit would
   need `loginctl enable-linger` to survive a logout, which is one more step to explain.
2. **`agent-utils` calls `sudo` itself.** The program does not refuse and ask the operator to
   re-run under `sudo`. It runs the privileged steps through `sudo` and lets `sudo` prompt.
3. **`install` is its own verb.** The `--daemon` flag is removed.
4. **`start` and `stop` are removed.** After an install, the operator uses `systemctl` or
   `launchctl`. Before an install, `listener run` is a foreground process that Ctrl-C stops.
5. **Output goes to journald.** The Linux unit sets no `StandardOutput` and no `StandardError`.
   The launchd property list keeps its two log files, which does not change.

Decision 4 removes the only supported way to send `SIGTERM` to a foreground listener from a
second terminal. The user accepted that loss. It also removes the code that deleted a stale
pidfile after an unclean exit, so this design moves that cleanup into `listener status`.

## The command surface

`listener` keeps four subcommands. Two are new, one is renamed, one is unchanged in purpose.

| Command | What it does |
|---------|--------------|
| `listener run [--listen-addr <a>] [--listen-port <p>]` | Runs the listener in this terminal. This is the old `start` without `--daemon`. |
| `listener install [--listen-addr <a>] [--listen-port <p>]` | Registers the listener with the platform service manager and starts it. |
| `listener uninstall` | Stops the service and removes the registration. |
| `listener status` | Reports the registration state and the pidfile state. |

`start` and `stop` are deleted. The command-line library reports an unknown command for each,
and lists the four commands above.

### `listener run`

The body is today's `listenerStartCommand` with the `--daemon` flag and its branch removed. Every
check it makes today stays, and the order stays:

1. The webhook must be enabled in the settings.
2. `--listen-addr` and `--listen-port` must pass `setField`, the same validator `config set` uses.
3. `webhook.secret` must not be empty.
4. `ensureToken` must find a readable GitHub token.

### `listener install`

`install` makes the same four checks, in the same order, and for the same reason: a service that
cannot read its token fails on every delivery instead of failing now at a terminal. It then calls
`Manager.Install` with the validated override arguments, and prints the path of the service file
it wrote.

The validated overrides are the only caller-supplied values that reach the service definition.
That is unchanged from today.

### `listener uninstall`

`uninstall` calls `Manager.Uninstall`, which is idempotent. It prints what it removed, or reports
that nothing was installed.

It separates two reasons for "nothing to remove" from a third case that is not one. A platform
with no service manager, and a machine with no registration, both print that nothing is installed.
Any other failure to read the registration is surfaced as an error. That failure means a
root-owned, boot-persistent unit may still be installed, so reporting success would be a lie at
the one moment it matters. `Manager` gains an exported `ErrUnsupported` sentinel so the command
can tell the cases apart.

### `listener status`

`status` prints two lines, as it does today. The first line changes its label from `launchd:` to
`service:`, because the backend is no longer always launchd.

`status` also takes over the stale-pidfile cleanup that `stop` performed. When the listener lock
is free and a pidfile is still on disk, the pidfile is from a process that died without running
its own shutdown. `status` removes it and says so. This keeps `status` from reporting a dead
process forever.

## The systemd backend

### Files

- `internal/service/service_linux.go` — build tag `linux`. Implements `Manager`.
- `internal/service/unit.go` — no build tag. Renders the unit file as text.
- `internal/service/service_other.go` — build tag becomes `!darwin && !linux`. Its error text
  names both supported platforms.
- `internal/service/service.go` — gains the `UnitName` constant, the directory override variable
  for tests, and the self-install check moved out of the darwin file.

### The unit file

The unit is named `agent-utils-listener.service`. It is written to
`/etc/systemd/system/agent-utils-listener.service`. The name must stay stable across releases for
the same reason the launchd label must: a rename orphans every registration made by an earlier
version, and a later `uninstall` cannot find it.

```ini
[Unit]
Description=agent-utils webhook listener
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart="/home/sean/bin/agent-utils" "listener" "run"
User=sean
Group=sean
WorkingDirectory=/home/sean/.agent-utils
Environment="HOME=/home/sean"
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Each directive has a reason:

- `Type=simple` — the listener does not fork and does not signal readiness.
- `Restart=always` with `RestartSec=5` — this is the launchd `KeepAlive` equivalent. The delay
  keeps a listener that cannot bind its port from spinning.
- `WantedBy=multi-user.target` — this is the launchd `RunAtLoad` equivalent for a system unit.
  The listener starts at boot, with no login and no lingering.
- `User=` and `Group=` — the daemon runs as the operator, not as root. It reads the same
  `~/.agent-utils/env` file, the same state database, and the same project registry that the
  foreground process reads.
- `Environment="HOME=..."` — systemd sets `HOME` for a `User=` unit, but this program resolves
  `~/.agent-utils` from `HOME` on every path. The unit states it rather than depends on it.
- `WorkingDirectory=` — the agent-utils home directory, which matches what the launchd property
  list already sets.
- No `StandardOutput` and no `StandardError` — journald is the default sink. `journalctl -u
  agent-utils-listener` reads the log.

`AGENT_UTILS_HOME` is carried into the unit as a second `Environment=` line when, and only when,
it is set in the environment that runs `listener install`. Without that line a machine with an
overridden home would install a service that reads a different directory than the operator does.

### Rendering the unit

`unit.go` builds the file as text. A unit file is line-oriented and metacharacter-rich, and four
different constructs can carry an injection. None of them can be escaped the same way, and some
cannot be escaped at all:

- A newline ends a directive. A value containing one opens a sibling directive of the caller's
  choosing, in a file that root executes at every boot.
- A percent sign is systemd's specifier escape. It is expanded inside `ExecStart`, `Environment`,
  `WorkingDirectory`, and `Description`, whether or not the value is quoted.
- A dollar sign is expanded inside `ExecStart`.
- A line that ends in a backslash is joined with the next line. That deletes the following
  directive rather than adding one, and the directives it can delete include the `User=` line that
  keeps the service from running as root.

So the renderer refuses. Any value that contains a control character, a percent sign, a dollar
sign, or a backslash produces an error and no document. The error names the offending field.

The double quote is the single exception. It is escaped with a backslash rather than refused,
because it is the delimiter of the quoting the renderer itself applies.

Each `ExecStart` argument and each `Environment` assignment is written inside double quotes. That
makes a path containing a space safe and removes any question about where one argument ends.
`User`, `Group`, and `WorkingDirectory` are written raw, because systemd does not read a quoted
value as an account name. That is safe only because the refusal above has already removed every
rune that could end one of those lines early or run past it.

This is a narrower guarantee than `plist.go` gets from `encoding/xml`, which escapes everything.
A unit file has no total escape scheme to delegate to. The refusal is what closes the gap: a value
that cannot be represented is rejected rather than written, and a refusal is auditable in a way
that an escape scheme invented here would not be.

The renderer does not rely on the caller having validated anything. The `webhook.listen_addr`
setting's validator is a non-empty check and a loopback warning, not an address parser, so
`--listen-addr` arrives as an arbitrary string. The renderer is the control, not a second line of
defense behind one.

### Install

`Install` runs as the operator, not as root. Only the three privileged steps go through `sudo`.
This matters: a program running under `sudo` for its whole life would resolve `HOME` to `/root`
and would write a unit that names root's directories.

1. Resolve the running binary with `resolveSelf`, and refuse a group-writable or world-writable
   path. See "The self-install check" below.
2. Verify the caller's `binary` argument against that result, as the darwin implementation does.
3. Resolve the agent-utils home directory with `home.EnsureDir`, so `WorkingDirectory` exists
   before systemd starts the service.
4. Render the unit.
5. Write it to root's standard input with `sudo tee /etc/systemd/system/<unit>`.
6. Run `sudo chmod 0644 /etc/systemd/system/<unit>`.
7. Run `sudo systemctl daemon-reload`.
8. Run `sudo systemctl enable --now agent-utils-listener.service`.

The rendered unit goes from memory to root's standard input. It is never staged in a file first.

Staging would open a window. Between the moment this process closes a staged file and the moment
root reads it, anything that runs as the operator can rewrite those bytes or replace the path with
a symbolic link. That is not a remote threat for this program: it dispatches coding agents that
run with permission prompts disabled on untrusted text. Root would then place content the renderer
never produced, and every guarantee the renderer makes is bypassed without the renderer being
wrong about anything.

No shell is involved either. The command runner builds an explicit argument list, so `tee` gets
the destination as one argument. There is no redirect, no quoting, and no word splitting.

`chmod` is a separate step because `tee` creates the file with root's umask applied. The mode is
normalized rather than assumed. No `chown` step is needed: `tee` ran as root, so the file is
already root's.

`Install` refuses one case before it does anything else: an effective user identifier of zero.
That means the operator ran the whole program as root. Every directory the program resolves would
then be root's, and the installed service would run as root and read a state directory the
operator has never written to.

The test is the identifier alone, not the identifier together with a set `SUDO_USER`. A root login
shell, `su -`, `doas`, and `sudo env -u SUDO_USER agent-utils listener install` all arrive with an
effective user identifier of zero and no `SUDO_USER`. Each one would otherwise install a service
that runs the agent dispatcher as root at every boot. There is no case in which running the whole
program as root is correct, because it calls `sudo` itself for the three steps that need it.

The unit directory override has the same shape of hazard, and the same answer. That environment
variable exists only so the test suite does not touch `/etc/systemd/system`. It steers the
destination of a root-owned write and a root-owned delete, so honoring it while still escalating
would turn an environment variable into "create or delete a root-owned file at a path of my
choosing". So the `sudo` prefix is dropped whenever the override is set. A test does not need
root, and setting the variable outside a test makes the install fail rather than redirect it.

`Install` is idempotent. `install` overwrites an existing unit file, and `enable --now` on an
already-enabled unit is not an error.

### Uninstall

0. Return with no action when the unit file does not exist. Nothing is registered, so there is
   nothing to remove, and an operator who asks to uninstall on a machine with no service must not
   get a `sudo` password prompt for it.
1. Run `sudo systemctl disable --now agent-utils-listener.service`. A non-zero exit is logged and
   ignored, because the unit may already be gone.
2. Run `sudo rm -f /etc/systemd/system/agent-utils-listener.service`.
3. Run `sudo systemctl daemon-reload`.

`Uninstall` is idempotent, for the same reason the darwin implementation is: the operator may
have removed the unit by hand.

### Status

`Status` runs `systemctl show agent-utils-listener.service --property=ActiveState
--property=MainPID`. That form is machine-readable, needs no privilege,
and needs no `sudo` prompt from a command an operator runs to ask a question.

- `Installed` is true when the unit file exists on disk. This matches what the darwin
  implementation reports and does not depend on systemd having loaded it.
- `Running` is true when `ActiveState` is `active`.
- `PID` is `MainPID`, when it parses to a positive number.

A failure to run `systemctl` is not an error from `Status`. It reports not installed and not
running, exactly as the darwin implementation treats a failed `launchctl print`.

### The self-install check

`resolveSelf` and `refuseIfWritableByOthers` move from `service_darwin.go` into `service.go`,
with no change to their behavior and no change to the wording of the refusal. Both are written in
terms of `os` and `path/filepath` only, so they compile everywhere.

The check matters more on Linux than on macOS. A launchd user agent runs as the operator, so a
writable binary path is persistence. A systemd system unit is started by root, so a writable
binary path is persistence AND a path to the operator's own account from any local account that
can write that directory. The refusal is the same, and the reason to keep it is stronger.

`explainInstallErr` (`cmd/agent-utils/listener.go:249`) matches the refusal by its text. It keeps
working, because the text does not change. Its message names launchd, so it gains a platform-
neutral opening sentence and keeps the Homebrew explanation as the macOS example it is.

## Testing

The systemd backend is tested the way the launchd backend is tested. The command runner is a
package-level variable, so no test ever runs a real `systemctl` and no test ever prompts for a
password. Two more seams are added for the same reason. `service.New` becomes a variable, so a
test in `cmd/agent-utils` can substitute a fake `Manager` instead of running a real `launchctl
print` or `systemctl show`. `os.Geteuid` becomes a variable, so the refusal to install as root can
be tested without running the suite as root.

Test fixtures for the self-install check are rooted under the operator's home directory, not under
`t.TempDir()`. The check walks every parent to the filesystem root, and on Linux `t.TempDir()`
lands under `/tmp`, which is mode 1777. The existing launchd test passes only because the macOS
temporary directory is a private per-user tree. A home directory is conventionally mode 0755 on
both platforms, which carries no group-write or other-write bit, so a 0700 directory inside it
passes the walk.

- `unit_test.go` — the rendered document for a plain install, for an install with listen
  overrides, and for an install with `AGENT_UTILS_HOME` set. One test per refused value: a line
  feed, a carriage return, and another control character. One test that a path containing a space
  survives the quoting.
- `service_linux_test.go` — the exact command sequence `Install` runs, in order; the mode, owner,
  and destination of the placement; the removal of the temporary file on both the success and the
  failure path; the dropped `sudo` prefix when the effective user identifier is zero; the
  idempotence of `Install` and `Uninstall`; the parse of a `systemctl show` output into a
  `Status`; and the refusal of a group-writable binary path.
- `service_other_test.go` — the updated error text.
- `listener_test.go` — the renamed and new verbs. The tests that today drive `start` drive `run`.
  The tests that drive `stop` are replaced by tests for `uninstall` and for the stale-pidfile
  cleanup in `status`.

Continuous integration runs on `ubuntu-latest`, so the Linux backend is compiled, vetted, linted,
and tested natively on every push. The `vet` target gains two lines beside the existing
`GOOS=darwin` one.

`GOOS=linux go vet` lets a developer on macOS type-check the new file. `GOOS=windows go vet`
covers the stub, and it closes a real hole: once the stub's build tag is narrowed to
`!darwin && !linux`, nothing else selects it. Not the host build, not the two other `GOOS` lines,
not `make test`, not `make lint`, and not the release targets, which build Linux and macOS only.
Without that line the stub and its test compile for nobody and rot in place. `go vet` analyzes
test files, so the line type-checks both. Nothing ever runs the stub's test, and that is the
honest state of a stub for platforms this project ships no binary for.

## Documentation

- `README.md` — the listener command table, the daemon section, and the security section are
  rewritten around the four verbs. A Linux walkthrough is added: `listener install`, the `sudo`
  prompt it produces, `journalctl -u agent-utils-listener` for the log, and `systemctl status`
  for the state. The macOS walkthrough keeps its two log files.
- `docs/configuration.md` — the lines that name `listener start` are updated to `listener run`.

There is no migration note and no compatibility shim. The user chose a hard removal. A macOS
operator who has a launchd agent from `--daemon` can replace it with `listener install`, which
overwrites the property list under the same label.
