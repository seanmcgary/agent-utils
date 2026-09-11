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

`unit.go` builds the file as text. A unit file is line-oriented and has no escape for a newline
inside a value, so a value containing a newline would end its directive and open a sibling
directive of the caller's choosing, in a file that root executes at every boot. The renderer
therefore refuses any value that contains a control character, which includes a carriage return
and a line feed, and returns an error instead of a document.

Each `ExecStart` argument is written inside double quotes, with a backslash and a double quote
escaped by a backslash. This is systemd's own quoting rule. It makes a path that contains a space
safe, and it removes any question about where one argument ends.

This is a narrower guarantee than `plist.go` gets from `encoding/xml`, which escapes everything.
The refusal is what closes the gap: a value that cannot be represented is rejected rather than
written.

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
5. Write it to a private temporary file, created with mode `0600` in the operator's own temporary
   directory.
6. Place it with `sudo install -m 0644 -o root -g root <temp> /etc/systemd/system/<unit>`.
7. Run `sudo systemctl daemon-reload`.
8. Run `sudo systemctl enable --now agent-utils-listener.service`.

Step 6 uses `install`, not a shell redirect and not `sudo tee`. No argument passes through a
shell, so there is no quoting to get wrong. The temporary file is removed after the placement,
whether or not the placement succeeded.

When the effective user identifier is already zero, every `sudo` prefix is dropped. The commands
are otherwise identical.

`Install` is idempotent. `install` overwrites an existing unit file, and `enable --now` on an
already-enabled unit is not an error.

### Uninstall

1. Run `sudo systemctl disable --now agent-utils-listener.service`. A non-zero exit is logged and
   ignored, because the unit may already be gone.
2. Run `sudo rm -f /etc/systemd/system/agent-utils-listener.service`.
3. Run `sudo systemctl daemon-reload`.

`Uninstall` is idempotent, for the same reason the darwin implementation is: the operator may
have removed the unit by hand.

### Status

`Status` runs `systemctl show agent-utils-listener.service --property=LoadState
--property=ActiveState --property=MainPID`. That form is machine-readable, needs no privilege,
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
password.

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
and tested natively on every push. The `GOOS=darwin go vet` line in the `vet` target stays, and a
`GOOS=linux go vet` line is added beside it so a developer on macOS still type-checks the new
file.

## Documentation

- `README.md` — the listener command table, the daemon section, and the security section are
  rewritten around the four verbs. A Linux walkthrough is added: `listener install`, the `sudo`
  prompt it produces, `journalctl -u agent-utils-listener` for the log, and `systemctl status`
  for the state. The macOS walkthrough keeps its two log files.
- `docs/configuration.md` — the lines that name `listener start` are updated to `listener run`.

There is no migration note and no compatibility shim. The user chose a hard removal. A macOS
operator who has a launchd agent from `--daemon` can replace it with `listener install`, which
overwrites the property list under the same label.
