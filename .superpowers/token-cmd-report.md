# `agent-utils config token` — what was built

The complaint: `listener start` refused to run without `~/.agent-utils/env` and told the
operator to run `install -m 600 /dev/null ...` plus an `echo ... >>` by hand. The CLI knows
everything in those two commands except the token, so it now asks for that one thing and does
the rest.

## What was built

### `internal/listener` — the writer

- **`SetToken(token) (path, error)`** (`internal/listener/env_write.go`). Trims the value,
  validates it, creates `~/.agent-utils` via `home.EnsureDir()`, reads any existing file,
  upserts the `GITHUB_TOKEN` assignment, and writes atomically at 0600. Returns the path so the
  caller can report where it went.
- **`upsertEnvAssignment`** rewrites the existing assignment *where it stands* and leaves every
  other line — comments, unrelated exports — untouched. A *duplicate* assignment further down
  is dropped: both a sourced shell file and `parseEnvValue` take the last one, so a stale
  duplicate would silently override the token just written and `config token` would report
  success while the daemon kept using a possibly-revoked credential.
- **`writeTokenFile`** is `internal/settings`' `writeSecretFile`: random temp name,
  `O_WRONLY|O_CREATE|O_EXCL|O_NOFOLLOW` at 0600, then rename. `os.WriteFile` ignores its mode
  argument when the target exists (so a 0644 file stays 0644 and keeps leaking the token) and
  follows a symlink at the path.
- **`ErrEnvFileMissing`** — a sentinel wrapped into the existing "does not exist" message, so
  the message renders **byte-for-byte as before** while a caller can now tell the absent-file
  case from a wrong mode / symlink / bad owner.
- **`readTokenFile` was split into `readEnvFile(path, requireOwnerOnlyMode bool)`.** `SetToken`
  passes `false`: it is about to rewrite the file at 0600, so refusing to *read* a 0644 one
  would send the operator away to `chmod` a file this program is already replacing. Every other
  check (no symlink, regular file, owned by us, size cap) still applies on the write path,
  because the content read there is preserved into a credential file cron sources.
- **`parseEnvValue` was refactored onto a shared `envAssignment(line)`**, which now owns the
  line-form rules. Its own comment claimed the rules were pinned "in one place so two future
  call sites cannot silently diverge" — adding a writer that re-implemented them would have
  broken exactly that promise, and the concrete failure is a writer that fails to recognise an
  assignment and appends a second one beside it.

### `cmd/agent-utils` — the command

- **`configTokenCommand()`** in the new `cmd/agent-utils/config_token.go`, registered under the
  existing `config` group. No flags. An argument is rejected with an explanation rather than
  ignored, per main.go's rule (a value on the command line shows up in `ps` and shell history).
- **`readToken(in, out)`** — `term.ReadPassword` when `in` is a terminal (an echoed token
  survives in scrollback and is captured whole by a screen share or a recorded session; it is a
  repository-write credential, so that exposure lasts as long as the token does). Otherwise it
  takes the first line from `in`, which is the `echo "$TOKEN" | agent-utils config token`
  scripting path. Neither a terminal nor piped input is a refusal naming the command, because
  prompting under launchd/cron would hang forever.
- **`storeToken(in, out)`** writes the file and prints the path and the mode — never the token.
- **`ensureToken(in, out, interactive)`** in `listener.go` replaces the bare
  `listener.Token()` check in `listener start`. It keeps the fail-fast property (the check still
  happens before the database is opened or a socket bound), and it re-reads the file after
  writing rather than trusting the write. It prompts **only** for `ErrEnvFileMissing` **and**
  only when interactive; every other failure returns `github token: %w` exactly as before.
  `interactive` is a parameter, not an `isInteractive()` call inside, so it is testable.

### Docs

- **README**: the Cron section and the Webhooks section now lead with `agent-utils config
  token`; the hand-run `install -m 600` form is kept below it for scripted machine builds. The
  Webhooks setup block gained `config token` as its first line, and the Webhooks prose explains
  that `listener start` offers the prompt inline, that it does so only at a terminal, and only
  for a missing file. The `~/.agent-utils` layout list now names the command that writes `env`.
- **docs/configuration.md**: a paragraph under *The machine-wide directory* documents `env`,
  the command, the 0600 atomic write, the preserve-other-lines guarantee, the no-flag rule, the
  pipe form, and the refusal — with the manual form kept. The `project loop status` line that
  mentions `GITHUB_TOKEN` now points at it.
- `internal/config/docs_test.go` still passes (no yaml field was added).

## Test evidence

`make check` (fmtcheck, vet, `GOOS=darwin` vet, golangci-lint "0 issues", full suite) and
`make test/race` both pass with no failures.

New tests, none of which reads `os.Stdin`:

`internal/listener/env_write_test.go` — creates the file at 0600; creates the home directory;
replaces an existing assignment; preserves every other line **and** keeps the assignment in its
original position; drops a duplicate assignment; tightens a 0644 file to 0600; appends safely
after a file with no trailing newline; refuses a symlink and leaves its target untouched;
rejects empty / whitespace / newline / quote / NUL / control values without repeating the token
in the error; trims surrounding whitespace; `ErrEnvFileMissing` is reported for an absent file
and *not* for a wrong mode.

`cmd/agent-utils/config_token_test.go` — piped stdin writes the file and the output names the
path and the mode but never the token; refusal with no terminal and no input names `config
token` and writes nothing; an unwritable value writes nothing; only the first piped line is
taken; `config token` is registered and carries no flags; `ensureToken` prompts and continues
for an absent file, keeps the unchanged `install -m 600` error with no terminal, and refuses to
prompt (leaving the file byte-for-byte as found) for a 0644 file.

Manual verification against a built binary, in a throwaway `$AGENT_UTILS_HOME`:

- `echo 'ghp_x' | ./au config token` → `Wrote GITHUB_TOKEN to <path> (mode 0600).`, file is
  `-rw-------`, contains `export GITHUB_TOKEN='ghp_x'`.
- Run again with `export OTHER=1` already in the file → token replaced in place, `OTHER`
  preserved.
- `./au config token < /dev/null` → refusal, exit 1. `./au config token ghp_x` → argument
  rejected, exit 1.
- Under a real pty (`pty.fork`): the prompt appears and the typed token is **not** echoed back.
- Under a real pty, `./au listener start` with no env file: prompts, writes the file, and goes
  on to bind and log `listener started`. The same command with `</dev/null` prints the original
  `install -m 600` error verbatim and exits 1.

## Judged differently from the brief

1. **The assignment is written single-quoted: `export GITHUB_TOKEN='<value>'`, not
   `export GITHUB_TOKEN=<value>`.** The brief specified the unquoted form "so it stays
   sourceable"; single quoting serves that goal strictly better. The file is sourced by cron, so
   an unquoted value containing `$`, a space, or `#` would be expanded or truncated by the
   shell. `parseEnvValue` already strips exactly one layer of matching quotes, so the daemon
   reads back the identical bytes, and the hand-written unquoted form in the docs still parses.
   `validateToken` rejects the one character single quoting cannot survive (`'`), along with
   newlines (which would let a pasted value append further assignments to a file cron sources),
   NUL, and other control characters.
2. **`SetToken` reads an existing file with the mode check disabled**, unlike the daemon's read
   path. Refusing to read a 0644 env file would block the command in exactly the situation an
   operator reaches for it — an earlier hand-run `echo >>` that got the mode wrong — when the
   0600 atomic write is about to repair it. The symlink, regular-file, and owner checks all
   still apply, since that content is preserved into the new file.
3. **A duplicate `GITHUB_TOKEN` line is removed**, not left in place. The brief said "replace
   the assignment if present, append if not, leave every other line untouched"; a second
   assignment of the *same* key is not an unrelated line, and leaving it would let it win under
   last-occurrence-wins and silently override the token just written.
4. **The non-terminal `listener start` error is unchanged, exactly as instructed** — including
   the two shell commands. Under launchd it would arguably be more useful to point at
   `agent-utils config token`, but the brief was explicit, and `ErrEnvFileMissing` is wrapped
   so the rendered text is identical to before. Flagging it as the one place the old
   instructions survive in an error message.
5. **`config token` rejects a positional argument** rather than ignoring it. The brief said
   never accept the token as a flag or an argument; silently ignoring one would leave the
   operator at a prompt for a value they believe they already supplied.
