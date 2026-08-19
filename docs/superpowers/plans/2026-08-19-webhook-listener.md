# Implementation plan: webhook listener

**For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development
(recommended) or executing-plans to implement this plan task-by-task.

Design: [`docs/superpowers/specs/2026-08-19-webhook-listener-design.md`](../specs/2026-08-19-webhook-listener-design.md)

## Pipeline State

| Field   | Value                                                                 |
|---------|-----------------------------------------------------------------------|
| stage   | 2 (plan review complete, awaiting the human gate)                     |
| class   | large (new subsystem, schema migration, remote-triggered code execution) |
| profile | backend                                                               |
| branch  | feat/webhook-listener                                                 |
| pr      | #4                                                                    |
| gate    | pending                                                               |
| round   | 0                                                                     |

## Architecture

The daemon receives a GitHub webhook delivery. The daemon finds every loop that watches the
delivery's repository. The daemon runs the same tick the command runs.

```
GitHub
  |  POST /webhook  (X-Hub-Signature-256: sha256=...)
  v
internal/listener
  |  1. require the sha256= prefix, then verify HMAC-SHA256 over the raw body
  |  2. read repository.full_name
  |  3. answer 202
  |  4. hand the work to a BOUNDED pool, on a daemon-scoped context:
  v
internal/listener.Targets(repo)  ->  registry + config scan
  |
  v
loopcmd.Open(ref, path, Options{Token: ...})  ->  cfg, Deps, cleanup
loopcmd.RunTick(ctx, cfg, deps)               ->  lock + Tick
  |
  v
internal/engine  (unchanged decisions, wall-clock backoff)
internal/store   (issues.retry_after)
```

A second path wakes a loop with no repository activity:

```
store.DB.EarliestRetryAfter()  ->  RetryDue{ProjectID, Loop, Repo, Number, At}
  |
  v
listener.TargetFor(projectID, loop)   -- scoped to ONE loop, never fanned out by repo
  |
  v
loopcmd.RunTick
```

Four properties hold this together:

1. The daemon and the command call the same two functions. There is no second tick path.
2. The delivery carries no state the tick needs. The tick reads the truth from the GitHub API.
3. A dropped delivery is safe, because the tick that holds the lock reads the same state.
4. A retry deadline belongs to one project and one loop. Waking never crosses that boundary.

## Global Constraints

**This repository has no `AGENTS.md`, `CLAUDE.md`, `CONTRIBUTING.md`, `STANDARDS.md`, or
`STYLEGUIDE.md` at its root.** The binding conventions therefore come from `README.md`,
`Makefile`, `.golangci.yml`, `docs/configuration.md`, and codebase precedent. The text below is
copied word for word. Do not paraphrase it. Do not violate it.

From `README.md`, "Development":

> ```bash
> make deps        # install golangci-lint and staticcheck
> make build       # build ./bin/agent-utils, version stamped in
> make check       # fmtcheck + vet + lint + test, in that order
> ```

> | `make check` | Everything that must pass before pushing |

> Tests run with `-p 1` and no cache on purpose: the `worktree` package shells out to real git
> and `runner` spawns real processes, so package-level parallelism is not safe, and a cached
> PASS is not evidence about the working tree.

From `README.md`, "Continuous integration":

> | `test` | Full suite, then again under `-race` |

From `README.md`, "Security":

> A loop dispatches an agent that runs with permission prompts disabled, inside a git worktree,
> on text written by other people. Issue bodies, issue comments, and pull request bodies are
> UNTRUSTED input. An instruction hidden in a comment executes.

> - The agent process gets a filtered environment and `GITHUB_TOKEN` is removed from it, at both
>   hops (the detached runner and the agent itself).

From `README.md`, "Cron":

> Do NOT put the token inline in the crontab. cron runs the whole line through `/bin/sh -c`, so
> a `VAR=value command` prefix puts the token in the shell's argument list, where `ps` shows it
> to every user on the machine.

From `README.md`, "Versioning and releases":

> The semantic version lives in the `VERSION` file at the repository root. It is the single
> source of truth; nothing infers a version from a tag.

From `cmd/agent-utils/main.go`, the rule that governs every credential in this program:

> // The token must come from the environment, never a flag. A flag value
> // shows up in `ps` output and in the shell history of anyone who typed it.

From `cmd/agent-utils/main.go`, `selectedProject`, the flag-shadowing rule:

> // It cannot use c.String("name"): the loop subcommands define their OWN --name
> // for the loop, and urfave/cli lets a child shadow a parent's flag of the same
> // name. Reading the flag from the command that declares it is what keeps
> // `project --name web loop tick --name planning` unambiguous.

From `cmd/agent-utils/main.go`, `isInteractive`, the prompt rule:

> // isInteractive reports whether stdin is a terminal. This is what keeps a
> // prompt out of a cron job.

From `internal/config/docs_test.go`:

> // Documentation rots silently. This walks the Config struct and fails when a
> // yaml field is not mentioned in the reference, so adding a field without
> // documenting it breaks the build rather than shipping a lie.

From `internal/ghub/ghub.go`:

> // Client is the read surface the engine needs, plus the two writes reserved
> // for the retry-cap park. Every method is safe to fake in a test.

From `.golangci.yml`:

> ```yaml
> version: "2"
> linters:
>   enable:
>     # Unchecked errors. The loop's whole failure model depends on recording
>     # outcomes, so a silently dropped error is a real defect here.
>     - errcheck
>     # Comparing a sentinel with == instead of errors.Is. This repo already had
>     # one (lock.ErrHeld), which works only until something wraps it.
>     - errorlint
>     - govet
>     - ineffassign
>     - staticcheck
>     - unused
> ```

From `docs/configuration.md`:

> The parser is **strict**: an unknown key is an error, not a warning. A misspelled key fails
> the load rather than being silently ignored. Every validation error for a file is reported
> together, in a stable order.

House style observed in `internal/home/home.go`, `internal/registry/registry.go`,
`internal/proc/proc.go`, and `internal/loopcmd/tick.go`, and required of every file this plan
adds:

- A package comment states why the package exists, not only what it holds.
- A comment explains **why** the code is the way it is. It records the failure the code
  prevents. It does not restate the code.
- A mode of `0600` on a file that holds a secret carries a comment that says what leaks
  without it.
- Structured logging is `slog` with a short lowercase message and paired keys, `loop` first
  where a loop is in scope. Precedent: `slog.Info("clearing stale retry flag", "loop",
  cfg.Name, "issue", d.Issue, "reason", d.Reason)` at `internal/loopcmd/tick.go:223`.
- A package whose tests emit per-event logging carries a `TestMain` that silences `slog`.
  Precedent: `internal/loopcmd/main_test.go`.

Commit convention, inferred from `git log --oneline -20` (`feat(store):`, `fix(migrate):`,
`refactor(cli):`, `docs:`): Conventional Commits, lowercase imperative subject, optional scope.

User constraint, binding on every commit in this run:

- Never add a `Co-Authored-By` trailer or any other AI-attribution trailer to a commit message.

## Verified external API (do not re-derive)

Every signature below was read from source in the module cache at
`$(go env GOMODCACHE)/github.com/google/go-github/v77@v77.0.0/`. Use them as written.

### Repository hooks — `github/repos_hooks.go`

```go
func (s *RepositoriesService) CreateHook(ctx context.Context, owner, repo string, hook *Hook) (*Hook, *Response, error)
func (s *RepositoriesService) ListHooks(ctx context.Context, owner, repo string, opts *ListOptions) ([]*Hook, *Response, error)
func (s *RepositoriesService) EditHook(ctx context.Context, owner, repo string, id int64, hook *Hook) (*Hook, *Response, error)
```

```go
// github/repos_hooks.go:41
type Hook struct {
    ID     *int64      `json:"id,omitempty"`
    Name   *string     `json:"name,omitempty"`
    URL    *string     `json:"url,omitempty"`
    Config *HookConfig `json:"config,omitempty"`   // required on create
    Events []string    `json:"events,omitempty"`
    Active *bool       `json:"active,omitempty"`
}

// github/repos_hooks_configuration.go:14
type HookConfig struct {
    ContentType *string `json:"content_type,omitempty"`
    InsecureSSL *string `json:"insecure_ssl,omitempty"`
    URL         *string `json:"url,omitempty"`
    // Secret is returned obfuscated by GitHub, but it can be set for outgoing requests.
    Secret *string `json:"secret,omitempty"`
}
```

Two facts that change the code:

- `Config` is a typed `*HookConfig`. It is **not** `map[string]interface{}`. An older example
  found on the web will not compile.
- GitHub returns `Config.Secret` **obfuscated**. Never compare a listed hook's secret against
  the stored one. Match a hook by `Config.URL` only.

### Delivery validation — `github/messages.go`

```go
const (
    SHA1SignatureHeader   = "X-Hub-Signature"
    SHA256SignatureHeader = "X-Hub-Signature-256"
    EventTypeHeader       = "X-Github-Event"
    DeliveryIDHeader      = "X-Github-Delivery"
)

func ValidatePayloadFromBody(contentType string, readable io.Reader, signature string, secretToken []byte) (payload []byte, err error)
func ValidatePayload(r *http.Request, secretToken []byte) (payload []byte, err error)
func WebHookType(r *http.Request) string
func DeliveryID(r *http.Request) string
```

**Three traps, each verified in source. All three must be closed in E1.**

1. **`ValidatePayload` falls back to SHA-1 by header name.** At `messages.go:256-260`:

   ```go
   signature := r.Header.Get(SHA256SignatureHeader)
   if signature == "" {
       signature = r.Header.Get(SHA1SignatureHeader)
   }
   ```

   Do not call `ValidatePayload`.

2. **Reading the SHA-256 header yourself is NOT sufficient.** `messageMAC` at
   `messages.go:149-176` selects the hash function from the **signature string's own prefix**,
   not from the header name:

   ```go
   switch sigParts[0] {
   case sha1Prefix:   hashFunc = sha1.New
   case sha256Prefix: hashFunc = sha256.New
   case sha512Prefix: hashFunc = sha512.New
   ```

   So `X-Hub-Signature-256: sha1=<hmac-sha1-hex>` is verified with SHA-1. The handler MUST
   reject a signature that does not begin with `sha256=` before it calls the library.

3. **An empty secret disables verification.** At `messages.go:230-236` validation runs only
   `if len(secretToken) > 0 || len(signature) > 0`. With an empty secret the HMAC is computed
   with the empty key, which an attacker also knows. The listener MUST refuse to serve when
   the configured secret is empty.

`ValidatePayloadFromBody` switches on an **exact** media type at `messages.go:198-209`, so
`application/json; charset=utf-8` falls to `default` and errors. Parse the header with
`mime.ParseMediaType` first, the way `ValidatePayload` does at `messages.go:260-264`.

`ValidateSignature` compares with `hmac.Equal` (`messages.go:143-147`), so the comparison is
constant time.

### launchd — verified with `man launchctl` on Darwin 25.3.0, launchd 7.0.0

```
launchctl bootstrap gui/<uid> <plist-path>
launchctl bootout   gui/<uid>/<label>
launchctl print     gui/<uid>/<label>
```

`<uid>` is `os.Getuid()`. `bootout` returns a non-zero status when the service is not loaded;
treat that as success for an idempotent `stop`.

### Existing in-repo API this plan builds on

```go
// internal/home
func Dir() (string, error)              // honours $AGENT_UTILS_HOME
func EnsureDir() (string, error)        // creates it at 0700
func StateDBPath() (string, error)

// internal/lock
var ErrHeld = errors.New("lock is held by another process")
func Acquire(path string) (*Lock, error)   // LOCK_EX|LOCK_NB, never blocks
func (l *Lock) Release() error

// internal/registry
func List() ([]Project, error)          // most recently used first
func (p Project) Exists() bool          // the directory is still present

// internal/config
func List(dir string) ([]Entry, error)  // Entry{Name, File, Path, Repo, Err}
func Load(path string) (*Config, error) // strict: dec.KnownFields(true)
var ErrNoConfigs                        // internal/config/discover.go

// internal/store
func Open(path string) (*DB, error)
func (d *DB) Project(projectID string) *Store
func (d *DB) LoopStates() ([]LoopState, error)   // machine-wide read, the pattern to copy
func (s *Store) MarkNeedsRetry(loop, repo string, number int) error   // signature changes in C1
```

Callers of the retry-flag writers, confirmed by grep — C1 must update every one:

- `internal/runner/runner.go:242` `MarkNeedsRetry`, `:245` `MarkSucceeded`
- `internal/loopcmd/tick.go:162` `MarkNeedsRetry`, `:225` `ClearNeedsRetry`

## Tasks

### Phase A — the shared tick path

- [ ] **A1. Move `setup()` into `internal/loopcmd` as `Open`.**  `review: yes`

  Move `setup`, `projectRef`, and `migrationPolicy` from `cmd/agent-utils/main.go` into a new
  file `internal/loopcmd/open.go`. Export them as `Open`, `ProjectRef`, and `MigrationPolicy`
  with the constants `FailOnUnimported` and `WarnOnUnimported`. Update `refOf`
  (`cmd/agent-utils/main.go:113`), which returns the moved type.

  Change one thing about the body: the function must **not** call
  `os.Getenv("GITHUB_TOKEN")`. Add an options struct:

  ```go
  type Options struct {
      // Token authenticates the GitHub client.
      //
      // The caller reads it, because the daemon reads it from a file on each tick
      // and must not change its own environment to pass it.
      //
      // NOTHING may ever fill this field from a cli.Flag. A flag value shows up in
      // `ps` output and in the shell history of anyone who typed it. The command
      // reads the environment; the daemon reads ~/.agent-utils/env.
      Token           string
      RequireGitHub   bool
      MigrationPolicy MigrationPolicy
  }

  func Open(ref ProjectRef, configPath string, opts Options) (*config.Config, Deps, func(), error)
  ```

  Keep the existing error text for a missing token. Every call site in `main.go` passes
  `Token: os.Getenv("GITHUB_TOKEN")`, so behaviour does not change.

  Keep every existing comment. The comments on the token rule, on the migration policy, and on
  the write path are the record of why the code is shaped this way.

  **Acceptance:**
  - `make check` passes.
  - `grep -n "func setup" cmd/agent-utils/main.go` returns nothing.
  - `grep -rn 'os.Getenv("GITHUB_TOKEN")' internal/` returns nothing.
  - `grep -rn '"token"' cmd/agent-utils/` finds no `cli.Flag` definition. The credential rule
    moved from code-enforced to convention-enforced, so this grep is the replacement anchor.
  - A new `internal/loopcmd/open_test.go` covers the new surface directly: `RequireGitHub:
    true` with an empty `Token` returns the missing-token error; `RequireGitHub: false` with an
    empty `Token` succeeds; the returned cleanup closes the database.
  - No existing test file is edited except for an import path.

- [ ] **A2. Add `loopcmd.RunTick`.**  `review: yes`

  Move the lock-and-tick body of the `loop tick` action into `internal/loopcmd/open.go`:

  ```go
  // RunTick takes the loop's lock and runs one tick.
  //
  // It returns an error wrapping lock.ErrHeld when another tick already holds the
  // lock. The caller decides what that means: the command exits quietly, and the
  // daemon drops the delivery, because the running tick reads the same GitHub
  // state a moment later than the dropped one would have.
  func RunTick(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error)
  ```

  Callers match with `errors.Is(err, lock.ErrHeld)`, never `==`. The `loop tick` action becomes
  `Open`, `RunTick`, and an `errors.Is` check that logs and returns nil. Keep that log line and
  its wording.

  **Acceptance:** a test in `internal/loopcmd` proves that a second `RunTick` against a held
  lock returns an error matching `errors.Is(err, lock.ErrHeld)` and starts no dispatch.
  `make check` passes.

### Phase B — machine-wide configuration

- [ ] **B1. New package `internal/settings`.**  `review: yes`

  Create `internal/settings/settings.go`. The package comment states why this file is separate
  from `internal/config` (a loop) and `internal/project` (a project), and warns that
  `config.yaml` already names a project descriptor.

  ```go
  const FileName = "config.yaml"   // the same base name as a project descriptor; see Load

  type Settings struct {
      Webhook Webhook `yaml:"webhook"`
  }

  type Webhook struct {
      Enabled    bool   `yaml:"enabled"`
      URL        string `yaml:"url"`
      ListenAddr string `yaml:"listen_addr"`
      ListenPort int    `yaml:"listen_port"`
      Secret     string `yaml:"secret"`
  }

  func Path() (string, error)                 // through home.Dir
  func Load() (*Settings, error)              // a zero value when the file is absent
  func Save(s *Settings) error                // atomic, 0600, refuses a project descriptor
  func GenerateSecret() (string, error)       // 32 bytes from crypto/rand, hex
  ```

  This package also owns the **typed key table** that B2's command drives, so the validation
  logic is unit-testable without a CLI. `cmd/agent-utils` contains only `main.go` and every
  testable behaviour in this repo lives under `internal/`.

  ```go
  // Field is one settable key. The table is the single definition of what a key
  // is called, how its value parses, and whether printing it leaks a secret.
  type Field struct {
      Key    string
      Secret bool
      Get    func(*Settings) string
      Set    func(*Settings, string) error
      Unset  func(*Settings)
  }

  func Fields() []Field
  func FieldFor(key string) (Field, bool)
  func Render(s *Settings, reveal bool) (string, error)   // YAML, secret redacted unless reveal
  const Redacted = "***redacted***"
  ```

  Rules the code must enforce, each with a comment saying what breaks without it:

  - `Load` returns `&Settings{}` and no error when the file does not exist. Every existing
    command must keep working on a machine that has never run `config`.
  - `Load` decodes in **two passes**. The strict pass with `dec.KnownFields(true)` cannot
    produce the project-descriptor diagnostic on its own: `Settings` has no `id` or `name`
    field, so a descriptor fails with a bare unknown-field error, which sends the operator to
    the wrong file. Decode first into `struct{ ID, Name string }` with a lenient decoder; when
    both are non-empty, return a named error saying this path holds a project descriptor and
    that `$AGENT_UTILS_HOME` points at a project's `.agent-utils` directory. Otherwise decode
    strictly into `Settings`.
  - `Load` refuses a file whose mode allows group or other any access (`mode&0o077 != 0`). The
    file holds the sole authenticator for an endpoint that starts an agent, so a 0644 copy
    restored from a backup must not be used silently.
  - `Save` runs the same descriptor probe on the existing file **before** it writes, and
    refuses rather than overwriting. Without this, an `$AGENT_UTILS_HOME` pointed at a
    project's `.agent-utils` destroys the project UUID, which `internal/project` documents as
    never changing.
  - `Save` writes with `os.OpenFile(tmp, O_WRONLY|O_CREATE|O_EXCL|O_NOFOLLOW, 0o600)` using a
    random suffix, then renames. Do **not** copy `registry.write`'s `os.WriteFile(path+".tmp",
    ...)` verbatim: `os.WriteFile` ignores the mode argument when the file already exists and
    follows a symlink, so a leftover 0644 `config.yaml.tmp` would publish the secret
    world-readable. `registry.write` stores no secret, which is why the hole was harmless
    there. Say that in the comment.
  - `Save` calls `home.EnsureDir` first and writes a header comment above the YAML, the way
    `project.Save` does.
  - Defaults are applied by an explicit method, never inside `Load`:

    ```go
    // WithDefaults returns a copy with unset fields filled in.
    func (s Settings) WithDefaults() Settings
    ```

    `ListenAddr` defaults to `127.0.0.1` and `ListenPort` to `8787`. Bind to loopback by
    default, because `0.0.0.0` would accept deliveries from the local network before the
    operator asked for that. Keeping this out of `Load` is what lets `Load` return a true zero
    value for an absent file and lets `Save`/`Load` round-trip a stored value unchanged. The
    `listener` command calls `WithDefaults`; the `config` command does not, so `show` prints
    what is really in the file.

  Field validation, in the `Set` functions:
  - `webhook.url` must parse with `net/url`, must have a host, and must use scheme `http` or
    `https`. Do **not** reject `http`. The daemon is expected to sit behind nginx, cloudflared,
    or ngrok, which terminate TLS, so the public URL is normally `https` without this code
    saying anything; and a plain-`http` endpoint is legitimate on a private network or a
    tailnet. Print a warning when the scheme is `http` and the host is not loopback, naming
    what it costs: the delivery and its signature cross the network in the clear, so an
    observer can replay that one delivery. Do not print it for a loopback host.
  - `webhook.listen_port` must be in 1..65535.
  - `webhook.secret` has no `Set`. Direct the operator to `config webhook --rotate-secret`, so
    a weak hand-typed secret cannot reach the file.

  **Acceptance:** tests in `internal/settings/settings_test.go`, each pointing
  `$AGENT_UTILS_HOME` at `t.TempDir()`:
  - `Load` with no file returns a zero value and no error.
  - `Save` then `Load` round-trips every field.
  - The saved file's mode is exactly `0600` (`os.Stat` then `.Mode().Perm()`).
  - A pre-existing `config.yaml.tmp` at 0644 does not become the saved file's mode.
  - A file holding `id:` and `name:` gives the named project-descriptor error from `Load`, and
    `Save` refuses it and leaves the bytes untouched.
  - A file at 0644 gives the mode error from `Load`.
  - An unknown key gives an error.
  - `GenerateSecret` returns 64 hexadecimal characters, and two calls differ.
  - Table-driven tests on `FieldFor(...).Set`: `webhook.url` accepts `https://h/p`, rejects
    `http://example.com/p`, accepts `http://127.0.0.1:8787/p`, rejects `notaurl` and a
    schemeless value; `webhook.listen_port` rejects `0`, `70000`, and `abc`;
    `webhook.secret` has no setter.
  - `Render(s, false)` contains `***redacted***` and not the secret; `Render(s, true)`
    contains the secret.

- [ ] **B2. The `config` command group.**  `review: yes`

  Add a top-level `configCommand()` registered beside `listCommand()`. The action bodies are
  thin wiring over `internal/settings`; every parse and validation rule lives in B1's table.

  ```
  agent-utils config show [--reveal]
  agent-utils config get <key> [--reveal]
  agent-utils config set <key> <value>
  agent-utils config unset <key>
  agent-utils config webhook --enable|--disable [--url U] [--listen-port N] [--listen-addr A] [--rotate-secret]
  ```

  The command's `Usage` string must disambiguate it from loop configuration, which already owns
  the word "config" in this repo (`--config`, `docs/configuration.md`, `configs/`). Say:
  "machine-wide settings; loop configuration lives in `.agent-utils/configs/`".

  An unknown key is an error that lists `settings.Fields()`. `get` and `show` redact a
  `Field.Secret` value unless `--reveal` is given.

  `config webhook` applies every flag to one in-memory `Settings`, validates the result, and
  saves once:
  - `--enable` with no `--url` and no stored URL is an error, and writes nothing.
  - `--enable` mints a secret when the stored one is empty.
  - `--rotate-secret` mints a new secret and prints a line telling the operator to run
    `agent-utils project register-webhook` again for every repository.
  - `--enable` and `--disable` together is an error.

  Declare `listenPortFlag()` and `listenAddrFlag()` as shared constructors beside
  `configFlag()` and `nameFlag()`, and reuse them in E6 so the two commands cannot drift to
  different spellings. Precedent: `cmd/agent-utils/main.go` declares each shared flag once.

  **Acceptance:** `cmd/agent-utils` has no test file today; this task adds the first one
  (`cmd/agent-utils/config_test.go`). That is a deliberate new precedent, and it is justified
  only because these assertions are about flag wiring, which cannot be reached from
  `internal/`. Every parse and validation rule stays in B1's tests. The CLI tests assert:
  - `webhook --enable` with no URL exits non-zero and `settings.Load` still returns a zero
    value.
  - `webhook --enable --url https://x/y` writes a 64-character secret and the defaults.
  - `webhook --enable --disable` exits non-zero.
  - `show` on a populated file prints `***redacted***`; `show --reveal` prints the secret.
  - `set webhook.nope x` exits non-zero and names the known keys.

### Phase C — wall-clock retry backoff

**This phase changes an existing loop configuration key. See "Risks" below.**

- [ ] **C1. Store: the `retry_after` column and its lifecycle.**  `review: yes`

  In `internal/store/store.go`:
  - Add `retry_after INTEGER NOT NULL DEFAULT 0` to the `issues` `CREATE TABLE` at line 32.
  - Add `{"issues", "retry_after", "INTEGER NOT NULL DEFAULT 0"}` to `addedColumns` at line
    273, so an existing database gains the column on the next open.
  - Add `retry_after` to the `issues` column list in `rebuilt` (line 308). It is harmless
    today, because a pre-`project_id` database has only zeros there, but omitting it is a
    silent drop the next time that path runs.
  - Add `RetryAfter time.Time` to `store.IssueState` in `internal/store/types.go`. Comment: it
    is the deadline before which no retry runs, and the zero value means no deadline.
  - Read and write it in `IssueStates`, `IssueState`, and `PutIssueState`. Store Unix seconds;
    store 0 for the zero time and convert back on read.

    Comment the type choice honestly. Every other timestamp in this schema is a `TIMESTAMP`
    column (`issues.updated_at`, `cooldowns.until`, `ticks.started_at`, the `dispatches` time
    columns). This column is an `INTEGER` because `addedColumns` requires a literal `DEFAULT`
    and there is no natural default for a `TIMESTAMP` that reads back as the zero time. Do not
    claim INTEGER matches precedent; it does not.

  **The lifecycle is the part that matters.** A deadline that is never cleared is a permanent
  past deadline, and E4's `Wake` would then re-tick that loop forever, each iteration calling
  the GitHub API with a repository-write token.

  **`MarkNeedsRetry` is the ONE writer of `retry_after`.** Nothing else may write it. A second
  writer in `dispatch` would be overwritten on the very next failure — every `needs_retry`
  transition runs through `MarkNeedsRetry` — so the escalating list would collapse to its first
  entry forever. Recording the deadline where the failure is recorded also keeps the index
  where the old code read it: `BackoffTicks[state.RetryCount]` was evaluated against the
  failure, not against the dispatch.

  ```go
  // MarkNeedsRetry records that a dispatch for this issue failed, and stamps the
  // earliest time a retry may run.
  //
  // It reads retry_count inside the same transaction and indexes backoff with it,
  // clamped to the last entry. An empty list means no deadline: retry.max may be
  // 0, in which case retry.backoff is absent and no retry will ever be decided.
  func (s *Store) MarkNeedsRetry(loop, repo string, number int, now time.Time, backoff []time.Duration) error
  ```

  Threading the list to the call sites is a real refactor, so do it deliberately:

  - `internal/loopcmd/tick.go:162` (reaping a dead runner) has `cfg` and `now`. Pass them.
  - `internal/runner/runner.go:242` is inside **`finish(st, d, res)`** at `runner.go:226`,
    which has neither `cfg` nor `now`. Add a parameter to `finish` and thread it from its
    **seven** internal call sites (`runner.go:108, 116, 138, 148, 157, 213, 223`) and from the
    exported `Finish` at `runner.go:222`. `Supervise` has `cfg`; so does `Finish`'s caller at
    `internal/loopcmd/tick.go:472`. Pass `cfg.Retry.Backoff` converted to `[]time.Duration`.

  Clearing, so no stale deadline survives:
  - `ClearNeedsRetry` (line 461) also sets `retry_after = 0`.
  - `MarkSucceeded` (line 493) also sets `retry_after = 0`.
  - `parkRetryExhausted` in `internal/loopcmd/tick.go` clears `state.RetryAfter` where it sets
    `Parked = true`.

  Add the machine-wide read:

  ```go
  // RetryDue is the earliest pending retry deadline on this machine.
  type RetryDue struct {
      ProjectID string
      Loop      string
      Repo      string
      Number    int
      At        time.Time
  }

  // EarliestRetryAfter returns the soonest pending retry deadline, if there is one.
  //
  // It is scoped to rows that a retry can still act on. A parked issue, or one
  // whose failure flag was cleared, keeps its old deadline in the row, and
  // returning that value would give the daemon a deadline permanently in the past
  // to spin on.
  func (d *DB) EarliestRetryAfter() (RetryDue, bool, error)
  ```

  The predicate is `WHERE retry_after > 0 AND needs_retry = 1 AND parked = 0`, and it must also
  **exclude a loop whose circuit breaker is in cooldown**. `engine.Decide` returns with no
  decisions at all while `now.Before(st.CooldownUntil)` (`internal/engine/engine.go:18-20`),
  leaving `needs_retry = 1` and a past deadline untouched, so without this exclusion `Wake`
  re-ticks that loop every `minWakeInterval` for the whole cooldown — the exact spin the
  predicate exists to prevent. Left-join `cooldowns` and require `until IS NULL OR until <= ?`,
  passing `now`.

  Return the row
  with the smallest `retry_after`, with `ok=false` when there is none — a named struct plus a
  boolean, not a positional tuple of three bare strings, and following `DB.LoopStates` in
  shape. Select the column and order by it; do **not** use `MIN()`. `LoopStates` avoids
  aggregates for a stated reason: an aggregate has no declared type, so the driver hands back
  a value of a different type than every other read of that column.

  **Acceptance:**
  - A test opens a database, closes it, re-opens it, and confirms the column is added and
    existing rows survive with `retry_after = 0`.
  - A test round-trips a non-zero `RetryAfter` through `PutIssueState` and `IssueState`.
  - A test proves `EarliestRetryAfter` returns `ok=false` for an empty table, skips a row with
    `parked = 1`, skips a row with `needs_retry = 0`, skips a zero deadline, and returns the
    smallest of two live rows.
  - **A test proves a deadline belonging to project A is returned with project A's id, and
    that project B's row is not returned when B's deadline is later.** Isolation is the point.
  - Tests prove `ClearNeedsRetry` and `MarkSucceeded` zero the column.

- [ ] **C2. Config: `retry.backoff` as durations.**  `review: yes`

  In `internal/config/config.go`:
  - Add `Backoff []Duration \`yaml:"backoff"\`` to `Retry`. `Duration` already exists in
    `internal/config/duration.go`.
  - **Keep** `BackoffTicks []int \`yaml:"backoff_ticks"\`` as a rejection shim, and make
    `validate` reject a non-empty value with a migration message. Without the field,
    `KnownFields(true)` produces "field backoff_ticks not found in type config.Config", which
    does not tell the operator what to write. The `unused` linter does not fire: it is an
    exported field on an exported type and `validate` reads it.

    The message names the old key, the new key, and a value to copy:

    ```
    retry.backoff_ticks is no longer supported; a tick is no longer a fixed
    interval, because a webhook can tick a loop at any moment. Replace it with
    retry.backoff, a list of durations:

      backoff: [0s, 15m, 30m]
    ```

  - Keep the validation that the list has at least `retry.max` entries, now against `Backoff`.
  - `retry.max: 0` is legal and means "never retry" (`internal/config/config.go:193-200`), so
    `Backoff` may legitimately be **empty**. No code may index it without a length check. C1's
    `MarkNeedsRetry` treats an empty list as "no deadline"; add a test for a config with
    `max: 0` and no `backoff` key, and confirm it loads.

  **Acceptance:** tests in `internal/config/config_test.go`:
  - A file with `backoff: [0s, 15m, 30m]` loads, and `cfg.Retry.Backoff[1].Std()` is 15
    minutes.
  - A file with `backoff_ticks: [0, 1, 2]` fails, and the error text contains `retry.backoff`.
  - A file with `max: 3` and two `backoff` entries fails.
  - `make test` passes, which includes `TestEveryConfigFieldIsDocumented` — see C4.

- [ ] **C3. Engine and tick: decide on the clock, write the deadline.**  `review: yes`

  In `internal/engine/engine.go`:
  - `retryDecision` gains a `now time.Time` parameter and drops any use of `st.TickCount`.
    Declare the new signature explicitly: `retryDecision(cfg *config.Config, number int, state
    store.IssueState, st State, now time.Time) (*Decision, bool)`. Keep `st` only if a
    surviving reader needs it; if none does, remove the parameter rather than leaving it
    unused.
  - Replace `if wait > 0 && st.TickCount-state.LastRetryTick < int64(wait)` with
    `if !state.RetryAfter.IsZero() && now.Before(state.RetryAfter)`.
  - Keep the retry cap check, the park decision, and every existing comment.
  - `engine.State.TickCount` becomes unread. Line 161 is its only reader today, so **remove the
    field** from `engine.State` and its assignment in `tick.go`. Do not leave a dead field with
    a comment claiming other uses; the `unused` linter and the next reader both deserve the
    truth. `Store.TickCount` and `Store.RecordTick` are unaffected and stay.

  In `internal/loopcmd/tick.go`:
  - **`dispatch` must NOT write `state.RetryAfter`.** C1's `MarkNeedsRetry` is the only writer.
    Two writers would fight: `dispatch` stamps a deadline before the agent runs, the agent
    fails, and `MarkNeedsRetry` overwrites it — so the escalating list would collapse to one
    entry and never be observed.
  - Keep the existing `state.RetryCount++` and `state.LastRetryTick` assignment. `RetryCount`
    is what `MarkNeedsRetry` indexes the list with on the next failure, so the increment is
    load-bearing.
  - Clear `state.RetryAfter` where the code clears `NeedsRetry` on a human trigger.
  - `last_retry_tick` stays in the table and stops being read. Dropping a column costs a table
    rebuild and buys nothing.

  The resulting arithmetic, stated once so it can be tested:

  | Event | `retry_count` | `retry_after` written |
  |---|---|---|
  | First failure recorded | 0 | `t₀ + Backoff[0]` |
  | Retry 1 dispatches | 0 → 1 | untouched |
  | Second failure recorded | 1 | `t₁ + Backoff[1]` |
  | Retry 2 dispatches | 1 → 2 | untouched |
  | Success, park, or clear | — | 0 |

  Every entry of the list is reachable, and the index never exceeds `retry.max`, which C2
  guarantees is no longer than the list.

  **Acceptance:** tests in `internal/engine/engine_test.go` and `internal/loopcmd`:
  - An issue with `NeedsRetry`, the in-flight label, and `RetryAfter` in the future produces no
    decision.
  - The same issue with `RetryAfter` in the past produces a retry decision.
  - An issue with a zero `RetryAfter` and `NeedsRetry` retries at once.
  - The retry cap still parks, regardless of `RetryAfter`.
  - `grep -rn "TickCount" internal/engine/` shows no `State` field.

  Pin the arithmetic with **literal** expectations, not a formula recomputed from the same
  fields the implementation reads — a test that computes `Backoff[state.RetryCount]` passes
  against an off-by-one. With `backoff: [0s, 15m, 30m]` and a fixed `now`:
  - A first failure (`retry_count` 0) writes `retry_after == now`.
  - A second failure (`retry_count` 1) writes `retry_after == now + 15m`.
  - A third failure (`retry_count` 2) writes `retry_after == now + 30m`.
  - A failure at `retry_count` 5 with a three-entry list writes `now + 30m`, not a panic.
  - An empty `Backoff` writes a zero `retry_after`, not a panic.

  **One test must drive the whole sequence**, because that is the only thing that catches the
  two-writer bug: record a failure, run a tick that dispatches the retry, record a second
  failure, and assert the deadline is `Backoff[1]` from the second failure — not `Backoff[0]`,
  and not a value stamped by the dispatch.

- [ ] **C4. Update the examples and the configuration reference.**  `review: yes`

  `internal/config/docs_test.go` reflects over every yaml tag in `Config` and fails when the
  dotted name is absent from `docs/configuration.md`. C2 keeps `BackoffTicks`, so
  **`retry.backoff_ticks` must stay documented and `retry.backoff` must be added.** Deleting
  the old entry breaks `make test`. This task is `review: yes` for that reason alone.

  - `examples/planning.yaml:44` and `examples/execution.yaml:36`: replace
    `backoff_ticks: [0, 1, 2]` with `backoff: [0s, 15m, 30m]`.
  - `docs/configuration.md:150`: replace the `retry.backoff_ticks` row with a `retry.backoff`
    row, and add a second row for `retry.backoff_ticks` marked as removed, so the reflection
    test still finds the name.
  - `docs/configuration.md:535-548`: rewrite the section for durations. State that the deadline
    is stored on the issue row, that entry 0 is the wait before the first retry, and that both
    `loop tick` and the daemon read the same value. Add a short "removed" note for
    `retry.backoff_ticks` naming the replacement.
  - `docs/configuration.md:647`: update the validation-rules row
    `| len(retry.backoff_ticks) ≥ retry.max |` to name `retry.backoff`.

  **Acceptance:** `make test` passes, specifically `TestEveryConfigFieldIsDocumented` and
  `internal/config/examples_test.go`.

### Phase D — registering the webhook at GitHub

- [ ] **D1. Repository hooks in `internal/ghub`.**  `review: yes`

  Do **not** add these methods to `ghub.Client`. That interface is documented as "the read
  surface the engine needs", and `fakeGH` in `internal/loopcmd/tick_test.go:16` implements
  exactly its current five methods — growing it would break A1's acceptance that no existing
  test is edited. Declare a separate, narrow interface:

  ```go
  // HookAdmin administers repository webhooks. It is separate from Client because
  // no tick calls it: hook administration is an operator command that runs once.
  type HookAdmin interface {
      ListHooks(ctx context.Context, owner, repo string) ([]Hook, error)
      CreateHook(ctx context.Context, owner, repo string, h HookSpec) (int64, error)
      EditHook(ctx context.Context, owner, repo string, id int64, h HookSpec) error
  }
  ```

  `*GitHubClient` implements it, so `ghub.New(token)` satisfies both interfaces.

  Local types in `internal/ghub/types.go`, so no caller imports `go-github`:

  ```go
  type Hook struct {
      ID     int64
      URL    string   // Config.URL, the delivery target
      Events []string
      Active bool
  }

  type HookSpec struct {
      URL    string
      Secret string
      Events []string
  }

  // HookEvents is the event set a loop reacts to. It is declared once, here,
  // because two callers must agree: register-webhook subscribes to it, and the
  // listener drops any delivery outside it. Two independent lists would drift,
  // and the daemon would answer every delivery and do nothing.
  var HookEvents = []string{
      "issues",
      "issue_comment",
      "pull_request",
      "pull_request_review",
      "pull_request_review_comment",
  }

  func IsHookEvent(name string) bool
  ```

  Set `Config.ContentType` to `"json"`, `Config.InsecureSSL` to `"0"`, `Name` to `"web"`, and
  `Active` to true.

  Two comments the code must carry:
  - GitHub returns `Config.Secret` obfuscated, so a hook is matched by `Config.URL` alone.
    Comparing the secret would create a new hook on every run.
  - A token without the `admin:repo_hook` scope gets a 404 from this endpoint, not a 403.
    Translate a 404 into an error that names the scope, or the operator reads it as "the
    repository does not exist".

  `ListHooks` paginates, the way `ListOpenIssues` does.

  **Acceptance:** tests using `httptest` and a `github.Client` whose `BaseURL` points at it.
  `internal/ghub` has no such test today, so this task establishes the pattern.
  - A 404 from the hooks endpoint produces an error whose text contains `admin:repo_hook`.
  - `ListHooks` follows a second page.
  - `CreateHook` sends `content_type=json`, `insecure_ssl=0`, and the secret.
  - `IsHookEvent` accepts each of the five and rejects `push`.
  - `make check` passes with `fakeGH` unedited.

- [ ] **D2. `agent-utils project register-webhook`.**  `review: yes`

  Add the subcommand under `projectCommand()`.

  ```
  agent-utils project register-webhook [--name <loop>] [--yes]
  ```

  `--name` here selects a **loop**, and it shadows `project`'s own `--name`. Resolve the
  project with `openProject`/`selectedProject`, which walks the lineage, and read the loop with
  `c.String("name")`. Precedent: `sessionsCommand()` does exactly this. Say so in a comment;
  this is the one flag in the tree with a documented shadowing hazard.

  Steps:
  1. `openProject`, then `config.List(p.Dir)`. `--name` restricts to one loop.
  2. Collect the distinct `repo:` values. Skip a loop whose entry has a non-nil `Err`, and say
     so on stderr.
  3. `settings.Load`. An empty `webhook.url` or `webhook.secret` is an error naming
     `agent-utils config webhook --enable --url <url>`. Fail here, before any GitHub call.
  4. Require `GITHUB_TOKEN`, with the existing error text.
  5. Confirm before writing, because this grants GitHub the right to trigger agent dispatch.
     List the repositories and ask. `--yes` skips the prompt. Prompt **only** when stdin is a
     terminal: reuse `isInteractive()`, and in a non-interactive run with no `--yes` return an
     error listing the repositories and naming `--yes`. A prompt in a cron job hangs forever;
     that rule is already written into `resolveLoopConfig`.
  6. For each repository: `ListHooks`, find a hook whose `URL` equals `webhook.url`, then
     `EditHook` or `CreateHook`. Compare the found hook's `Events` against `ghub.HookEvents`
     and say `updated` when they differ, so re-running after this list grows re-subscribes an
     already-registered repository. This is what `Hook.Events` and `Hook.Active` are for.
  7. Print one line per repository saying `created` or `updated`, with the hook id.

  When `webhook.enabled` is false, do the work and warn that the listener will refuse to start.
  Registering before enabling is a reasonable order.

  **Acceptance:** tests with a fake `ghub.HookAdmin`:
  - A second run calls `EditHook` and not `CreateHook`.
  - Two loops naming one repository produce one call.
  - A missing `webhook.url` fails before any GitHub call.
  - A non-interactive run without `--yes` fails and names `--yes`, making no GitHub call.

### Phase E — the daemon

- [ ] **E1. `internal/listener`: the HTTP surface and signature verification.**  `review: yes`

  Create `internal/listener/listener.go` and `internal/listener/handler.go`. The package comment
  states that this is the one surface in the program a stranger can reach, that a valid request
  starts an agent with permission prompts disabled, and that every decision in the file exists
  to prove a request came from GitHub. Add `internal/listener/main_test.go` with a `TestMain`
  that silences `slog`, following `internal/loopcmd/main_test.go`.

  ```go
  type Server struct {
      Addr   string
      Port   int
      Secret string
      // Tick runs one loop for a repository. It is a seam so a test can drive the
      // handler without opening a database or starting an agent.
      Tick func(ctx context.Context, repo string)
      // MaxInFlight bounds concurrent ticks. Zero means the default.
      MaxInFlight int
  }

  func New(s *Server) (*Server, error)   // refuses an empty Secret
  func (s *Server) Handler(ctx context.Context) http.Handler
  func (s *Server) ListenAndServe(ctx context.Context) error
  ```

  `New` returns an error when `Secret` is empty. This is a **fail-closed requirement, not a
  convenience**: `ValidatePayloadFromBody` skips verification entirely when both the secret and
  the signature are empty, and computes the HMAC with the empty key otherwise — a key the
  attacker also knows. `settings.Load` returns a zero value when the file is absent, so an
  empty secret is a reachable state, and the refusal must live in this package rather than only
  in E6's command.

  `POST /webhook`:
  1. A method other than POST gives 405.
  2. `r.Body = http.MaxBytesReader(w, r.Body, 5<<20)`.
  3. `sig := r.Header.Get(github.SHA256SignatureHeader)`; empty gives 400.
  4. **`strings.HasPrefix(sig, "sha256=")` or 400.** Comment: `messageMAC`
     (`messages.go:149-176`) picks the hash function from the signature's own prefix, so
     `X-Hub-Signature-256: sha1=...` would otherwise be verified with SHA-1. Reading the
     SHA-256 header is not by itself enough to pin SHA-256.
  5. `mime.ParseMediaType(r.Header.Get("Content-Type"))`; require exactly `application/json`.
     Anything else gives 415. D1 pins the hook to `content_type: json`, so the form-encoded
     path is not needed, and accepting it would run `url.ParseQuery` over 5 MiB of attacker
     bytes and then verify the HMAC over a different string than it parses.
  6. `github.ValidatePayloadFromBody(mediaType, r.Body, sig, []byte(s.Secret))`. A
     `*http.MaxBytesError` gives 413; any other error gives 401. A size overflow is not an
     authentication failure, and the distinction matters when reading logs during an attack.
  7. `github.WebHookType(r)`; `!ghub.IsHookEvent(event)` gives 204.
  8. Decode `struct{ Repository struct{ FullName string \`json:"full_name"\` } }`. Require it
     to match `^[A-Za-z0-9._-]{1,100}/[A-Za-z0-9._-]{1,100}$`; anything else gives 400. This is
     the one attacker-controlled value the daemon logs, and an unbounded string with embedded
     control characters lands in the operator's log file.
  9. Drop a repeat `X-Github-Delivery` with 200, using a bounded LRU of the last 1024 ids.
     GitHub redelivers, and the plaintext hop behind the proxy makes a captured delivery
     replayable forever.
  10. Answer 202, then hand the work to the bounded pool.

  **Concurrency.** Do not start a bare goroutine per delivery. Before it ever reaches the
  per-loop lock that would shed it, each worker scans the registry and every project's configs,
  reads the token file, and calls `loopcmd.Open`, which opens a SQLite handle and runs the
  migration check. Use a buffered semaphore channel of `MaxInFlight` (default 8). When the
  semaphore is full, log and drop with 202 — the next delivery, or `Wake`, re-derives the same
  state.

  **Context.** The goroutine must NOT use `r.Context()`. `net/http` cancels it the moment the
  handler returns, which is before the tick makes its first GitHub call. `Handler` takes the
  daemon-scoped context explicitly and closes over it, and `ListenAndServe` passes its own
  context to `Handler`. Do not store a context in the `Server` struct.

  Rejections write a fixed generic body and never `err.Error()`. `messageMAC` interpolates the
  attacker's signature into its error text, and the stage that failed is itself information.
  Log the detail keyed by delivery id only, never the signature or the secret.

  `GET /healthz` returns 200 and the body `ok`. It carries **no** authentication, and that
  exemption is deliberate and must be stated in a comment: it reveals only that the daemon is
  live, it triggers no work, and a proxy needs it unauthenticated. Because `--listen-addr` can
  bind a routable address, the comment must also say that the operator, not this code, decides
  reachability, and the default stays loopback.

  `ListenAndServe` binds `net.JoinHostPort`, and sets `ReadHeaderTimeout`, `ReadTimeout`,
  `WriteTimeout`, and `IdleTimeout`. `MaxBytesReader` bounds body size but not read time, so a
  client that dribbles 5 MiB would otherwise hold a connection and a goroutine indefinitely.
  Shut down on context cancellation with `http.Server.Shutdown`.

  Handle the `errcheck` linter on every `w.Write` and `fmt.Fprintf(w, ...)`: `.golangci.yml`
  excludes only `Close` variants and `lock.Lock.Release`.

  **Acceptance:** tests using `httptest.NewServer(s.Handler(ctx))`, with the fake `Tick` closing a
  channel the test waits on. Never sleep and never read a plain counter — CI runs the suite
  again under `-race`.
  - A body signed with the correct secret gives 202 and calls `Tick` once with the repository
    name.
  - A wrong signature gives 401 and does not call `Tick`.
  - No signature header gives 400.
  - **`X-Hub-Signature-256: sha1=<valid HMAC-SHA1 over the body>` gives 400 and does not call
    `Tick`.** This is the test that proves the downgrade is closed; the header-name case alone
    would pass against a vulnerable handler.
  - `X-Hub-Signature: sha1=<valid>` with no SHA-256 header gives 400.
  - `New` with an empty secret returns an error.
  - `Content-Type: application/json; charset=utf-8` is accepted.
  - `Content-Type: application/x-www-form-urlencoded` gives 415.
  - A 6 MiB body gives 413, not 401.
  - An unsubscribed event gives 204.
  - A repeated `X-Github-Delivery` gives 200 and calls `Tick` once.
  - `repository.full_name` of `""`, `a/b/c`, and a 500-character string each give 400.
  - A GET gives 405; `/healthz` gives 200.
  - A rejection body contains neither the signature nor the word `sha1`.

- [ ] **E2. `internal/listener`: routing a repository to its loops.**  `review: yes`

  Add `internal/listener/route.go`:

  ```go
  // Target is one loop that watches a repository.
  type Target struct {
      ProjectID   string
      ProjectName string
      Dir         string // the project's .agent-utils directory
      ConfigPath  string
      LoopName    string
      Repo        string
  }

  // Targets returns every loop on this machine whose repo matches.
  func Targets(repo string) ([]Target, error)

  // TargetFor returns the one loop named by a project id and a loop name.
  //
  // Waking a retry deadline uses this, never Targets: a deadline belongs to one
  // project's issue, and routing it by repository would dispatch agents in every
  // other project that happens to watch the same repository.
  func TargetFor(projectID, loop string) (Target, bool, error)

  func (t Target) Ref() loopcmd.ProjectRef   // ID, Name, Dir
  ```

  `Targets` reads `registry.List()`, skips a project whose `Exists()` is false with a log line,
  calls `config.List(p.AgentUtilsDir)` for the rest, and keeps entries whose `Repo` matches with
  `strings.EqualFold`. The fold matches `ghub.ListOpenPullRequests`, which already folds because
  GitHub's casing and the configuration's casing need not agree.

  Error handling: a failure from `registry.List` is returned, because routing nothing silently
  turns every delivery into a no-op with no recorded outcome. A per-project `config.List`
  failure — including `config.ErrNoConfigs`, the normal state of a registered project with no
  `configs/` directory — is logged and skipped, not returned. One broken project must not stop
  every other project's loop from ticking.

  Comment why nothing is cached: an operator who adds a loop expects the next delivery to use
  it, and the scan reads a few small files, far less than the GitHub API call that follows.

  **Acceptance:** a test builds two temporary projects under a temporary `$AGENT_UTILS_HOME`,
  registers both, and proves:
  - A repository shared by two projects returns both loops.
  - A deleted project directory is skipped and does not fail the call.
  - A loop file that does not parse is skipped and does not fail the call.
  - A project with no `configs/` directory is skipped and does not fail the call.
  - The match ignores case.
  - `TargetFor` returns exactly one loop, and returns `ok=false` for an unknown pair.
  - `TargetFor(projectA, "planning")` does not return project B's loop of the same name on the
    same repository.

- [ ] **E3. `internal/listener`: the token file.**  `review: yes`

  The machine-wide `env` file is a machine-wide artifact, so its name belongs beside the other
  machine-wide names. Add to `internal/home`:

  ```go
  const EnvFile = "env"
  func EnvPath() (string, error)
  ```

  Precedent: `home.StateDBFile` + `home.StateDBPath`, and `registry.FileName` + `registry.Path`
  — "so the registry and the canonical state database can never disagree about where home is".

  Add `internal/listener/env.go`:

  ```go
  // Token reads GITHUB_TOKEN from ~/.agent-utils/env.
  //
  // It reads the file on every call, so a rotated token needs no restart.
  func Token() (string, error)
  ```

  Hardening, each with a comment naming what it prevents:
  - Open with `os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)`, then `Stat` the
    **descriptor**. Stat-then-open is a time-of-check-to-time-of-use gap, and `os.Stat` follows
    a symlink and reports the target's mode, leaving the link itself unchecked.
  - Require a regular file. A FIFO at this path makes `io.ReadAll` block forever, and the file
    is read once per tick, so that wedges the whole daemon.
  - Require the file to be owned by the current uid.
  - Require `mode&0o077 == 0`. The README tells the operator to create it with
    `install -m 600`.
  - Cap the read at 64 KiB.

  Parsing rules, stated so two implementations cannot differ:
  - Skip a blank line and a line whose first non-space character is `#`.
  - Allow leading whitespace and an optional `export ` prefix.
  - Split on the first `=` only.
  - Strip one layer of matching single or double quotes.
  - Strip a trailing `\r`, so a CRLF-written file does not yield a corrupt token.
  - Do **not** strip a trailing `# comment`; a `#` is legal in a token.
  - The **last** occurrence of a key wins, matching shell semantics.
  - Never log the value.

  **Acceptance:** tests with a temporary `$AGENT_UTILS_HOME`:
  - `export GITHUB_TOKEN=abc` and `GITHUB_TOKEN=abc` both read `abc`.
  - A quoted value loses its quotes; a CRLF line does not keep `\r`.
  - A comment line and a blank line are ignored.
  - A repeated key takes the last value.
  - A file at mode 0644 gives an error naming the mode.
  - A symlink at the path gives an error.
  - An absent file gives an error naming the path.
  - No test asserts on a logged token.

- [ ] **E4. `internal/listener`: the worker, its retries, and the wake loop.**  `review: yes`

  Add `internal/listener/work.go`. Every collaborator is a field, so the acceptance tests can
  be written: `internal/loopcmd/tick.go:22` states the rule this repo already follows — "Deps
  holds everything a tick needs. Each field is replaceable in a test."

  ```go
  type Worker struct {
      DB *store.DB
      // Targets, TargetFor, Open, and Run are seams. Production wires them to
      // listener.Targets, listener.TargetFor, loopcmd.Open, and loopcmd.RunTick.
      Targets   func(repo string) ([]Target, error)
      TargetFor func(projectID, loop string) (Target, bool, error)
      Token     func() (string, error)
      Open      func(ref loopcmd.ProjectRef, path string, o loopcmd.Options) (*config.Config, loopcmd.Deps, func(), error)
      Run       func(ctx context.Context, cfg *config.Config, deps loopcmd.Deps) (loopcmd.Summary, error)
      Now       func() time.Time
      // After schedules f. It is a seam: production wires it to time.AfterFunc,
      // and a test substitutes a controlled clock. Without it the retry tests
      // would have to sleep for the real delays, which the acceptance forbids.
      After func(d time.Duration, f func()) *time.Timer

      // Delays are fields, not constants, so a test can shrink them.
      OpenRetryDelay  time.Duration // default 1m
      MinRetryDelay   time.Duration // default 30s
      MinWakeInterval time.Duration // default 30s

      mu      sync.Mutex
      pending map[loopKey]*attempt   // guarded by mu
  }

  // NewWorker returns a Worker with production seams and defaults.
  //
  // A constructor is required, not optional: pending is unexported, so a caller
  // in package main cannot initialise it in a composite literal, and the first
  // failing tick would write to a nil map and panic the daemon. Worker also
  // holds a mutex, so it must never be copied.
  func NewWorker(db *store.DB) *Worker

  // Deliver ticks every loop that watches repo.
  func (w *Worker) Deliver(ctx context.Context, repo string)

  // Wake ticks the one loop whose retry deadline has passed, and returns when the
  // next deadline is due. ok is false when no deadline is pending.
  func (w *Worker) Wake(ctx context.Context) (next time.Time, ok bool)
  ```

  `Deliver`:
  1. `w.Targets(repo)`. On error, log and return.
  2. For each target, call `tickOne`.

  `tickOne(ctx, t Target)`:
  1. `w.Token()`. On error, log and **return without scheduling a retry**: a bad file mode or
     an absent file is an operator problem that retrying cannot fix, and retrying would log the
     same error `retry.max` times per delivery.
  2. `w.Open(t.Ref(), t.ConfigPath, loopcmd.Options{Token: tok, RequireGitHub: true,
     MigrationPolicy: loopcmd.FailOnUnimported})`. **`defer cleanup()` immediately.** `Open`
     opens a `*store.DB`; in a long-lived daemon a missed cleanup is one leaked handle per
     delivery per target. Every existing call site defers it.
     On error there is no `cfg`, so the backoff list is unknown: log and schedule a retry using
     `w.OpenRetryDelay` rather than an undefined value.
  3. `w.Run(ctx, cfg, deps)`.
  4. `errors.Is(err, lock.ErrHeld)`: log at info, clear any pending attempt, return. The
     delivery carries no state, so the tick that holds the lock reads the same GitHub state.
  5. Another error: log at error and schedule a retry.
  6. Success: clear the pending attempt.

  Retry scheduling, stated precisely because `-race` runs in CI:
  - `pending` is keyed by `loopKey{ProjectID, LoopName}` and is read and written only under
    `w.mu`.
  - An entry holds the attempt count and a `*time.Timer` created with `w.After`, never
    `time.AfterFunc` directly. The seam is what makes the retry acceptance tests writable
    without sleeping for the real delay.
  - The delay is `cfg.Retry.Backoff[attempt]`, clamped to the last entry and to zero for an
    empty list, with a floor of `w.MinRetryDelay`. The migrated first entry is `0s`, so an
    unfloored delay would retry a failing tick with no pause up to `retry.max` times.
  - Stop after `cfg.Retry.Max` attempts and log that the loop waits for the next delivery.
  - Scheduling again for a key that already has a timer stops the old timer first.
  - Every timer is stopped when the context is cancelled, so a shut-down daemon starts no work.
  - Every seam field is set once by `NewWorker`, before the worker is shared, and is never
    written afterwards. Only `pending` is mutated at run time, and only under `w.mu`.

  `Wake`:
  1. `w.DB.EarliestRetryAfter()`. `ok=false` returns `ok=false`.
  2. If `due.At` is in the future, return it — the caller sets its timer and does not tick.
  3. Otherwise log `slog.Info("waking a loop for a retry deadline", "loop", due.Loop,
     "issue", due.Number, "due", due.At)` — `Number` exists for this line, so the operator can
     see which issue woke the daemon — then `w.TargetFor(due.ProjectID, due.Loop)`, then
     `tickOne` on that one target.
     **Never route a deadline through `Targets(due.Repo)`**: the deadline belongs to one
     project's issue, and repository routing would dispatch agents in every other project that
     watches the same repository, on that project's token budget.
  4. Return the next deadline.

  The daemon loop lives here, not in the command, so it can be tested:

  ```go
  // Serve runs the wake loop until ctx is done.
  func (w *Worker) Serve(ctx context.Context)
  ```

  It selects over `ctx.Done()` and a single `time.Timer` reset from `Wake`'s return, with a
  floor of `w.MinWakeInterval` so a clock skew or a stale row cannot spin. A dynamic
  set of retry timers is not selected over; `time.AfterFunc` callbacks do their own work.

  **Acceptance:** tests with fake seams and a controlled clock, all synchronising on channels:
  - A tick returning `lock.ErrHeld` schedules no retry.
  - A tick returning another error is retried after the configured delay.
  - The retry stops after `retry.max` attempts.
  - A `Token` error schedules no retry.
  - An `Open` error schedules a retry at `OpenRetryDelay`.
  - A backoff entry of `0s` still waits `MinRetryDelay`.
  - `cleanup` is called exactly once per `tickOne`, including on the `Run`-error path.
  - Two targets both run when the first returns an error.
  - `Wake` with a past deadline ticks exactly the loop named by `RetryDue`, and a second loop
    on the same repository is **not** ticked.
  - `Wake` with a future deadline ticks nothing and returns that deadline.
  - `Wake` with no pending deadline returns `ok=false`.
  - `Serve` stops every pending timer when the context is cancelled.
  - The package passes `go test -race ./internal/listener/...`.

- [ ] **E5. `internal/service`: the service manager.**  `review: no`

  Name the package for the concept, not the tool it shells out to: systemd is meant to follow,
  and every other package here is named for what it does (`internal/proc` shells out to `ps`).

  ```go
  type Status struct {
      Installed bool
      Running   bool
      PID       int
  }

  type Manager interface {
      Install(binary string, args []string) error
      Uninstall() error
      Status() (Status, error)
      ServiceFilePath() (string, error)
  }

  func New() Manager   // launchd on darwin, an unsupported stub elsewhere
  ```

  `service_darwin.go` implements it behind a build tag; `service_other.go` returns a manager
  whose methods report that `--daemon` supports macOS only and that the foreground
  `listener start` works everywhere. Every file carries the package comment style this repo
  uses.

  The label is `com.seanmcgary.agent-utils.listener`. The plist lives at
  `<LaunchAgentsDir>/<label>.plist` at mode 0644, where `LaunchAgentsDir` resolves to
  `~/Library/LaunchAgents` but is **overridable by an environment variable** for tests. Every
  other machine-wide path in this repo is redirectable for exactly this reason
  (`internal/home`: "A test needs that").

  The plist holds `Label`, `ProgramArguments`, `RunAtLoad` true, `KeepAlive` true,
  `StandardOutPath` and `StandardErrorPath` under the machine-wide directory, and
  `WorkingDirectory`. It holds **no** `EnvironmentVariables` and **no** token; the daemon reads
  the token from `~/.agent-utils/env`, and this file is world readable.

  **Render the plist with `encoding/xml`, never string concatenation.** `ProgramArguments`
  carries the caller's `--listen-addr` and `--listen-port` values; an unescaped `<` or
  `</string>` would otherwise inject arbitrary keys — including the `EnvironmentVariables` key
  this design is careful to exclude — into a file launchd executes at every login.

  **Binary path.** `Install` resolves the binary with `os.Executable()` then
  `filepath.EvalSymlinks`, matching `cmd/agent-utils/main.go:741`. It **refuses** to install
  when the resolved path, or any parent directory, is writable by group or other. With
  `RunAtLoad` and `KeepAlive` both true the plist is a permanent login-time execution of that
  path, and the README states the agents this program dispatches run with permission prompts
  disabled on untrusted text and can write anything the user can. Installing a plist that
  points into a checkout's `./bin` would hand a prompt-injected agent persistence.

  `Install` writes the plist, then runs `launchctl bootstrap gui/<uid> <path>`. `Uninstall`
  runs `launchctl bootout gui/<uid>/<label>`, treats a non-zero status as success, then removes
  the plist. `Status` runs `launchctl print gui/<uid>/<label>` and reads the pid.

  **Acceptance:**
  - A test renders the plist to a string and asserts: the label is present, the arguments end
    with `listener start`, `RunAtLoad` is true, and the text contains neither
    `EnvironmentVariables` nor `GITHUB_TOKEN`.
  - A test passes `--listen-addr` with `<>&"` and asserts the output is well-formed XML that
    `encoding/xml` re-parses, with no injected key.
  - A test with the LaunchAgents override pointed at `t.TempDir()` proves `Install` writes
    there and never touches the real directory.
  - A test proves `Install` refuses a binary path in a world-writable directory.
  - `GOOS=darwin go build ./...` and `GOOS=darwin go vet ./...` are part of this task's gate.
    CI runs on `ubuntu-latest`, so `service_darwin.go` is otherwise never compiled.
  - No test runs `launchctl`.

- [ ] **E6. The `listener` command group.**  `review: yes`

  Add a top-level `listenerCommand()`, registered beside `listCommand()`.

  ```
  agent-utils listener start [--daemon] [--listen-port N] [--listen-addr A]
  agent-utils listener stop
  agent-utils listener status
  ```

  Reuse B2's `listenPortFlag()` and `listenAddrFlag()`, and validate an override through the
  same `settings.FieldFor(...).Set` path the `config` command uses. An override that bypasses
  validation reaches E5's plist unchecked.

  `start`:
  - `settings.Load`. `webhook.enabled` false is an error naming
    `agent-utils config webhook --enable`. An empty `webhook.secret` is an error; refuse to run
    an unauthenticated listener.
  - Check the token file up front by calling `listener.Token()` once and reporting its error.
    Without this a daemon with a 0644 env file starts happily and fails every tick after.
  - Without `--daemon`: open the state database, then `w := listener.NewWorker(db)`, then
    `listener.New(&listener.Server{Addr: ..., Port: ..., Secret: ..., Tick: w.Deliver})`.
    `NewWorker` already wires `Targets`, `TargetFor`, `Token`, `Open`, `Run`, `Now`, `After`,
    and the three delays; the command overrides none of them. Write the pidfile, then run
    `Server.ListenAndServe` and `Worker.Serve` on one daemon-scoped context until SIGINT or
    SIGTERM.

    **Shut down in this order**, and say why in a comment: stop accepting deliveries with
    `http.Server.Shutdown` first, then cancel the daemon context so timers stop, then wait for
    the in-flight semaphore to drain, then close the state database, then remove the pidfile.
    Closing the database or cancelling the context while a `tickOne` is mid-flight leaves
    `dispatches` rows stuck in `running`, which the next tick has to reap as orphans.
  - With `--daemon`: `service.New().Install(self, args)` where `self` is E5's resolved
    executable path and `args` carries any validated override. Print the plist path.

  **The pidfile is required**, at `<home>/listener.pid`, mode 0600. `proc.IsAlive` cannot be
  reused: it matches the command line against `--dispatch <id>` and the listener carries no
  such argument. `stop` and `status` must therefore work for a foreground listener too — that
  is the only mode available off macOS.

  `stop`: boot the launchd agent out when it is installed, and signal the pidfile's process
  when one is live. `status`: report the launchd state, the pidfile state, the pid, and the
  bound address.

  **Acceptance:**
  - `listener start` with `webhook.enabled` false exits non-zero and names the `config webhook`
    command.
  - `listener start` with an empty secret exits non-zero.
  - `listener --help` lists three subcommands.
  - The end-to-end serve test lives in `internal/listener`, not here, and constructs
    `listener.New(&Server{Addr: "127.0.0.1", Port: 0, ...})` directly. Port 0 means "any free
    port" to the kernel, but `settings` rejects it as a stored value and `WithDefaults` would
    turn it into 8787, so it must not travel through the settings path. The CLI override keeps
    the 1..65535 rule with no exception.
  - A `cmd/agent-utils` test asserts the wiring only: `listener --help` lists three
    subcommands, and `start` fails fast for a disabled webhook and for an empty secret.
  - `status` reports a live foreground listener through the pidfile.
  - A test proves shutdown drains an in-flight tick before the database is closed.

### Phase F — explicit project onboarding

This phase is **independent of the webhook feature** and can be reverted or shipped on its own.
It exists because `~/.agent-utils/config.yaml` (machine-wide settings, B1) and
`<project>/.agent-utils/config.yaml` (a project descriptor) can currently land on the same
path, and the cause is implicit onboarding rather than the file name.

- [ ] **F1. `FindDir` must never return the machine-wide directory.**  `review: yes`

  `internal/config/discover.go:85-96` walks from the working directory to the **filesystem
  root**. `~/.agent-utils` is a parent of everything under `~` and exists on any machine that
  has run this tool, so a command run from a directory that is not inside a project resolves
  the machine-wide directory as the project directory.

  The doc comment at `discover.go:64-70` says the function "deliberately does NOT fall back to
  `$HOME/.agent-utils`". That is true of an explicit fallback only; the walk-up reaches it as
  an ordinary parent. Correct the code, then correct the comment.

  Skip a candidate equal to `home.Dir()` and keep walking. Return `ErrNoDir` naming
  `agent-utils project init` when nothing else matches. `home` must not import `config`; check
  the import direction before wiring this, and invert it if needed.

  **Acceptance:** a test with a temporary tree proves `FindDir` called from
  `<home>/Downloads/scratch`, where `<home>/.agent-utils` exists and no nearer directory does,
  returns `ErrNoDir` rather than the machine-wide directory. A test proves a real project
  nested under the machine-wide directory's parent still resolves. `$AGENT_UTILS_DIR` still
  wins and is still allowed to name any directory, because it is the documented escape hatch.

- [ ] **F2. `agent-utils project init`.**  `review: yes`

  ```
  agent-utils project init [<name>] [--dir <path>]
  ```

  The name is a **positional argument**, following `forget <project>`. It must not be `--name`:
  `project` already declares `--name` as the project selector, and a child flag of the same
  name shadows it — the hazard `selectedProject` documents.

  Steps:
  1. Resolve the target directory: `--dir`, else the working directory. Refuse when it
     resolves to `home.Dir()`, naming why.
  2. Create `<dir>/.agent-utils/` at 0700 and `<dir>/.agent-utils/configs/` at 0700.
  3. Mint the descriptor with `project.Ensure`, which already uniquifies a taken name against
     `registry.NameTaken`. Use the positional name when given.
  4. Register with `registry.Register`.
  5. Print the name, the directory, and the next step: drop a loop configuration into
     `configs/`. Report a rename the way `openProject` already does.

  Re-running on an initialised project is **not** an error: report the existing identity and
  re-register, so a moved project is found again. Never mint a second id.

  **Acceptance:** tests with a temporary `$AGENT_UTILS_HOME`:
  - `init` in an empty directory creates `.agent-utils/configs/`, writes a descriptor with a
    uuid, and registers exactly one project.
  - The descriptor is mode 0600 and both directories are 0700.
  - Running `init` twice keeps the same id and registers once.
  - `init` with a positional name uses it; a taken name gets a suffix and says so.
  - `init` in the machine-wide directory exits non-zero and writes nothing.

- [ ] **F3. `ResolveProject` stops onboarding implicitly.**  `review: yes`

  `internal/loopcmd/resolve.go` currently calls `project.Ensure`, so the first `project` command
  in any directory mints a descriptor and registers it. That is what turns an accidental
  directory into a project.

  Change it to `project.Load`. On `project.ErrNoConfig`, and on `config.ErrNoDir`, return an
  error naming `agent-utils project init`, in the shape the existing `ErrNoDir` message uses.
  Keep `registry.Register` for a project that already has a descriptor, so a moved project is
  re-found.

  This is backward compatible: every already-onboarded project has a descriptor and keeps
  working. Only a directory that was never a project now needs one command.

  Delete `Project.Created` and `Project.RenamedFrom` if `init` is their only remaining writer,
  and move the reporting into F2 rather than leaving dead fields.

  **Acceptance:**
  - A directory with `.agent-utils/` but no descriptor gives an error naming `project init`,
    and writes nothing.
  - A directory with no `.agent-utils/` anywhere in its parents gives an error naming
    `project init`.
  - An initialised project resolves and is re-registered.
  - Existing tests that relied on implicit onboarding are updated to call `init` first; list
    them in the commit message.

### Phase G — documentation and version

- [ ] **G1. README, configuration reference, and VERSION.**  `review: no`

  `README.md`:
  - Add `config` and `listener` rows to the "Global" command table, and
    `project init` and `project register-webhook` rows to the "Project" table.
  - **Rewrite "Quick start".** It currently says "There is no init step" and describes
    `mkdir -p .agent-utils/configs` followed by any project command. After F2 the first step is
    `agent-utils project init`. The paragraph describing the automatic registration and the
    rename message moves to `project init`.
  - Add a "Webhooks" section after "Cron", giving the setup in order:
    `config webhook --enable --url ...`, `project register-webhook`,
    `listener start --daemon`. State that the daemon speaks plain HTTP and expects nginx,
    cloudflared, or ngrok in front of it to terminate TLS, and that the default bind address is
    `127.0.0.1`.
  - **The new section must carry the `install -m 600 /dev/null ~/.agent-utils/env` instruction**,
    or reference the Cron section that has it. That file is currently created only in the Cron
    section, and E3 makes it a hard daemon prerequisite.
  - Rewrite "Cron" to say cron is now optional, and that a cron entry and the daemon may both
    run, because the lock makes an overlapping tick harmless.
  - Update line 156, "Loop state lives in one database for the machine, at
    `~/.agent-utils/state.db`" — it is the only place the README enumerates `~/.agent-utils`
    contents, and it now also holds `config.yaml`, `listener.pid`, and the listener logs.
  - Add to "Security": the listener accepts a request from the internet that starts an agent.
    Say that every delivery is verified with HMAC-SHA256 over the raw body, that a `sha1=`
    signature is refused whichever header carries it, that an empty secret refuses to serve,
    and that the webhook shortens the delay before untrusted issue text reaches an agent but
    does not change the trust rule already stated there.
  - Add an "Upgrading" note for the `retry.backoff_ticks` → `retry.backoff` rename.

  `docs/configuration.md`:
  - Line 74-76 and the `AGENT_UTILS_HOME` row at line 82 describe the machine-wide directory as
    holding "the registry and the canonical state database". Add `config.yaml`.
  - Add a short section stating that the machine-wide `config.yaml` is a **different file** from
    a project's `config.yaml`, and pointing at `agent-utils config`.
  - The retry rows and section are handled by C4.

  `VERSION`: bump to `v0.4.0`. The README states it is "the single source of truth", and the
  prior two feature merges each carried the bump in-tree. This change adds a subsystem and
  breaks a configuration key, so it is a minor bump, not a patch.

  **Acceptance:** `make check` passes. Every command named in the README exists in
  `agent-utils --help`. `./bin/agent-utils version` reports `v0.4.0` after `make build`.

## Risks

1. **The `retry.backoff_ticks` rename breaks every existing loop configuration.** This is the
   one decision that needs approval before Phase C starts. Mitigations: a targeted migration
   error rather than an unknown-field error; both examples and the reference updated in the
   same commit; `retry.backoff_ticks` deliberately kept in the struct so
   `TestEveryConfigFieldIsDocumented` still passes and the operator gets the message. If the
   reader rejects the rename, Phase C reduces to C1 alone (the column stays, written but not
   read by `engine`), and E4's `Wake` uses a fixed interval.

2. **The daemon runs agents from an internet-reachable endpoint.** The controls are in E1 and
   the design's security table. Two tests in E1 are load-bearing and must not be dropped as
   redundant: the `X-Hub-Signature-256: sha1=...` case, and `New` refusing an empty secret.
   Reading the SHA-256 header alone does not pin SHA-256 — the library selects the hash from
   the signature's prefix.

3. **`Targets` reads the filesystem on every delivery.** Bounded by the number of loop files
   and far below the GitHub API call that follows. Revisit only with a measurement.

4. **`retry_after` semantics during the upgrade.** A database written by an old binary has
   `retry_after = 0` everywhere, which reads as "no deadline" and retries at once — the same
   behaviour the old `[0, ...]` first entry gave. No issue is stranded.

5. **launchd has no automated coverage beyond the rendered plist.** E5 gates on
   `GOOS=darwin go build`/`go vet` so the file at least compiles in CI. Verify
   `listener start --daemon`, `stop`, and `status` by hand once on this machine before the PR
   is marked ready, and record the result in the PR body.

6. **Phase F changes how a project is onboarded.** `agent-utils project init` becomes required
   for a directory that is not yet a project; every existing project keeps working, because it
   already has a descriptor. The phase is separable and can be reverted, or shipped as its own
   pull request ahead of this one. It is in scope because implicit onboarding is what lets the
   machine-wide `config.yaml` and a project descriptor land on the same path.

7. **`MarkNeedsRetry` changes signature**, touching `internal/runner`. That package spawns real
   processes in its tests, which is why the suite runs `-p 1`. Run
   `go test ./internal/runner/...` explicitly after C1.
