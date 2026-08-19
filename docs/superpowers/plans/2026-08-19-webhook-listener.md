# Implementation plan: webhook listener

**For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development
(recommended) or executing-plans to implement this plan task-by-task.

Design: [`docs/superpowers/specs/2026-08-19-webhook-listener-design.md`](../specs/2026-08-19-webhook-listener-design.md)

## Pipeline State

| Field   | Value                                                                 |
|---------|-----------------------------------------------------------------------|
| stage   | 2 (plan review)                                                       |
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
  |  POST /webhook  (X-Hub-Signature-256)
  v
internal/listener
  |  1. verify HMAC-SHA256 over the raw body
  |  2. read repository.full_name
  |  3. answer 202
  |  4. work in a goroutine:
  v
internal/registry  ->  every project
internal/config    ->  every loop, filtered by repo
  |
  v
loopcmd.Open(ref, path, Options{Token: ...})  ->  cfg, Deps
loopcmd.RunTick(ctx, cfg, deps)               ->  lock + Tick
  |
  v
internal/engine  (unchanged decisions, wall-clock backoff)
internal/store   (issues.retry_after)
```

Three properties hold this together:

1. The daemon and the command call the same two functions. There is no second tick path.
2. The delivery carries no state the tick needs. The tick reads the truth from the GitHub API.
3. A dropped delivery is safe, because the tick that holds the lock reads the same state.

## Global Constraints

**This repository has no `AGENTS.md`, `CLAUDE.md`, `CONTRIBUTING.md`, `STANDARDS.md`, or
`STYLEGUIDE.md` at its root.** The binding conventions therefore come from `README.md`,
`Makefile`, and `.golangci.yml`. The text below is copied word for word from those files. Do
not paraphrase it. Do not violate it.

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

From `cmd/agent-utils/main.go`, the rule that governs every credential in this program:

> // The token must come from the environment, never a flag. A flag value
> // shows up in `ps` output and in the shell history of anyone who typed it.

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

From `README.md`, "Configuration", on the strict parser (`docs/configuration.md`):

> The parser is **strict**: an unknown key is an error, not a warning. A misspelled key fails
> the load rather than being silently ignored. Every validation error for a file is reported
> together, in a stable order.

House style observed in `internal/home/home.go`, `internal/registry/registry.go`, and
`internal/proc/proc.go`, and required of every file this plan adds:

- A package comment states why the package exists, not only what it holds.
- A comment explains **why** the code is the way it is. It records the failure the code
  prevents. It does not restate the code.
- A mode of `0600` on a file that holds a secret carries a comment that says what leaks
  without it.

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
func (s *RepositoriesService) DeleteHook(ctx context.Context, owner, repo string, id int64) (*Response, error)
```

```go
// github/repos_hooks.go:41
type Hook struct {
    CreatedAt    *Timestamp     `json:"created_at,omitempty"`
    UpdatedAt    *Timestamp     `json:"updated_at,omitempty"`
    URL          *string        `json:"url,omitempty"`
    ID           *int64         `json:"id,omitempty"`
    Type         *string        `json:"type,omitempty"`
    Name         *string        `json:"name,omitempty"`
    TestURL      *string        `json:"test_url,omitempty"`
    PingURL      *string        `json:"ping_url,omitempty"`
    LastResponse map[string]any `json:"last_response,omitempty"`

    // Only the following fields are used when creating a hook.
    // Config is required.
    Config *HookConfig `json:"config,omitempty"`
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
func ValidateSignature(signature string, payload, secretToken []byte) error
func WebHookType(r *http.Request) string
func DeliveryID(r *http.Request) string
```

**Do not call `ValidatePayload`.** At `messages.go:256-260` it does this:

```go
signature := r.Header.Get(SHA256SignatureHeader)
if signature == "" {
    signature = r.Header.Get(SHA1SignatureHeader)
}
```

A delivery that carries only the SHA-1 header is accepted. Read
`r.Header.Get(github.SHA256SignatureHeader)` directly, reject an empty value, and call
`ValidatePayloadFromBody`.

`ValidatePayloadFromBody` accepts `application/json` and
`application/x-www-form-urlencoded`. It returns an error for any other content type. It uses
`hmac.Equal` internally, so the comparison is constant time.

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
func Acquire(path string) (*Lock, error)
func (l *Lock) Release() error

// internal/registry
func List() ([]Project, error)          // most recently used first
func (p Project) Exists() bool          // the directory is still present

// internal/config
func List(dir string) ([]Entry, error)  // Entry{Name, Path, Repo, Err}
func Load(path string) (*Config, error) // strict: dec.KnownFields(true)

// internal/store
func Open(path string) (*DB, error)
func (d *DB) Project(projectID string) *Store
func (d *DB) LoopStates() ([]LoopState, error)   // machine-wide read, the pattern to copy
```

## Tasks

### Phase A — the shared tick path

- [ ] **A1. Move `setup()` into `internal/loopcmd` as `Open`.**  `review: yes`

  Move `setup`, `projectRef`, and `migrationPolicy` from `cmd/agent-utils/main.go` into a new
  file `internal/loopcmd/open.go`. Export them as `Open`, `ProjectRef`, and `MigrationPolicy`
  with the constants `FailOnUnimported` and `WarnOnUnimported`.

  Change one thing about the body: the function must **not** call
  `os.Getenv("GITHUB_TOKEN")`. Add an options struct:

  ```go
  type Options struct {
      // Token authenticates the GitHub client. The caller reads it, because the
      // daemon reads it from a file on each tick and must not change its own
      // environment to pass it.
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

  **Acceptance:** `go build ./...` passes. `make test` passes with no test edited except for an
  import path. `grep -n "func setup" cmd/agent-utils/main.go` returns nothing. `grep -rn
  "os.Getenv(\"GITHUB_TOKEN\")" internal/` returns nothing.

- [ ] **A2. Add `loopcmd.RunTick`.**  `review: yes`

  Move the lock-and-tick body of the `loop tick` action into `internal/loopcmd/open.go`:

  ```go
  // RunTick takes the loop's lock and runs one tick.
  //
  // It returns lock.ErrHeld when another tick already holds the lock. The caller
  // decides what that means: the command exits quietly, and the daemon drops the
  // delivery, because the running tick reads the same GitHub state.
  func RunTick(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error)
  ```

  `RunTick` returns `lock.ErrHeld` unwrapped enough for `errors.Is` to match. The `loop tick`
  action becomes: `Open`, `RunTick`, and a check for `ErrHeld` that logs and returns nil. Keep
  that log line and its wording.

  **Acceptance:** a new test in `internal/loopcmd` proves that a second `RunTick` against a
  held lock returns an error matching `errors.Is(err, lock.ErrHeld)` and starts no dispatch.
  `make test` passes.

### Phase B — machine-wide configuration

- [ ] **B1. New package `internal/settings`.**  `review: yes`

  Create `internal/settings/settings.go`. The package comment states why this file is separate
  from `internal/config` (a loop) and `internal/project` (a project).

  ```go
  const FileName = "config.yaml"

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
  func Save(s *Settings) error                // temp file then rename, mode 0600
  func GenerateSecret() (string, error)       // 32 bytes from crypto/rand, hex
  ```

  Rules the code must enforce, each with a comment saying what breaks without it:

  - `Load` returns `&Settings{}` and no error when the file does not exist. Every existing
    command must keep working on a machine that has never run `config`.
  - `Load` uses `dec.KnownFields(true)`, matching the loop parser and the project parser.
  - `Load` reports a named error when the file carries a top-level `id` and `name` — that is a
    project descriptor, which means `$AGENT_UTILS_HOME` points at a project's `.agent-utils`
    directory. `project.FileName` is also `config.yaml`, so the two files differ only by
    directory. A plain unknown-field error would send the operator to the wrong file.
  - `Save` calls `home.EnsureDir` first, writes `<path>.tmp` at 0600, then renames. This copies
    `registry.write`.
  - `Save` writes a header comment above the YAML, the way `project.Save` does.
  - Applied defaults: `ListenAddr` defaults to `127.0.0.1` and `ListenPort` to `8787` when the
    field is empty or zero. Bind to loopback by default, because a default of `0.0.0.0` would
    accept deliveries from the local network before the operator asked for that.

  **Acceptance:** tests in `internal/settings/settings_test.go`, each pointing
  `$AGENT_UTILS_HOME` at `t.TempDir()`:
  - `Load` with no file returns a zero value and no error.
  - `Save` then `Load` round-trips every field.
  - The saved file's mode is exactly `0600` (`os.Stat` then `.Mode().Perm()`).
  - A file holding `id:` and `name:` gives the named project-descriptor error.
  - An unknown key gives an error.
  - `GenerateSecret` returns 64 hexadecimal characters, and two calls differ.

- [ ] **B2. The `config` command group.**  `review: yes`

  Add `cmd/agent-utils/config.go` (or extend `main.go`, matching the file layout the repo
  already uses) with a top-level `configCommand()` registered beside `listCommand()`.

  ```
  agent-utils config show [--reveal]
  agent-utils config get <key>
  agent-utils config set <key> <value>
  agent-utils config unset <key>
  agent-utils config webhook --enable|--disable [--url U] [--port N] [--addr A] [--rotate-secret]
  ```

  A typed key table drives `get`, `set`, and `unset`. Each entry holds the dotted name, a
  getter, a setter that parses and validates the string, and a `secret bool`. An unknown key is
  an error that lists the known keys. A value that does not parse is an error. Nothing is
  written unless every check passes.

  Validation rules:
  - `webhook.url` must parse with `net/url` and must use scheme `http` or `https`.
  - `webhook.listen_port` must be in 1..65535.
  - `webhook.secret` is not settable through `set`. Direct it to `config webhook
    --rotate-secret`, so a weak hand-typed secret cannot reach the file.

  `show` prints the YAML with `webhook.secret` replaced by `***redacted***`. `--reveal` prints
  the true value. `get webhook.secret` redacts unless `--reveal` is given.

  `config webhook` is the shortcut. It applies every flag to one in-memory `Settings`,
  validates the result, and saves once. Specific rules:
  - `--enable` with no `--url` and no stored URL is an error, and writes nothing.
  - `--enable` mints a secret when the stored one is empty.
  - `--rotate-secret` mints a new secret and prints a line telling the operator to run
    `agent-utils project register-webhook` again for every repository.
  - `--enable` and `--disable` together is an error.

  **Acceptance:** tests that run the command's action against a temporary
  `$AGENT_UTILS_HOME`:
  - `set webhook.listen_port 70000` fails and the file is unchanged.
  - `set webhook.nope x` fails and names the known keys.
  - `set webhook.secret abc` fails and directs the operator to `config webhook`.
  - `show` output contains `***redacted***` and does not contain the secret.
  - `show --reveal` contains the secret.
  - `webhook --enable` with no URL fails, and `settings.Load` still returns a zero value.
  - `webhook --enable --url https://x/y` writes a 64-character secret and sets the defaults.

### Phase C — wall-clock retry backoff

**This phase changes an existing loop configuration key. See "Risks" below.**

- [ ] **C1. Store: the `retry_after` column.**  `review: yes`

  In `internal/store/store.go`:
  - Add `retry_after INTEGER NOT NULL DEFAULT 0` to the `issues` `CREATE TABLE` at line 32.
  - Add `{"issues", "retry_after", "INTEGER NOT NULL DEFAULT 0"}` to `addedColumns` at line
    271, so an existing database gains the column on the next open.
  - Add `RetryAfter time.Time` to `store.IssueState` in `internal/store/types.go`, with a
    comment saying it is the deadline before which no retry runs, and that a zero value means
    no deadline.
  - Read and write it in `IssueStates`, `IssueState`, and `PutIssueState`. Store Unix seconds;
    store 0 for the zero time and convert back to the zero time on read. A `NULL`-free integer
    column matches every other timestamp in this schema.
  - Add `func (d *DB) EarliestRetryAfter() (time.Time, string, string, string, error)`
    returning the smallest non-zero `retry_after` across every project, with the owning project
    id, loop, and repo. Follow the shape of `DB.LoopStates` at line 871. The daemon sets its
    wake-up timer from this.

  Do not drop `last_retry_tick`. Dropping a column costs a table rebuild, and the column costs
  nothing where it is.

  **Acceptance:** a test opens a database, closes it, adds the column through a second `Open`,
  and confirms existing rows survive with `retry_after = 0`. A test round-trips a non-zero
  `RetryAfter` through `PutIssueState` and `IssueState`. A test proves `EarliestRetryAfter`
  ignores zero values and crosses project boundaries. `make test` passes.

- [ ] **C2. Config: `retry.backoff` as durations.**  `review: yes`

  In `internal/config/config.go`:
  - Replace `BackoffTicks []int \`yaml:"backoff_ticks"\`` with
    `Backoff []Duration \`yaml:"backoff"\``. `Duration` already exists in
    `internal/config/duration.go`.
  - Keep the validation that the list has at least `retry.max` entries. Update the message.
  - Add a field `BackoffTicks []int \`yaml:"backoff_ticks"\`` **only** so the strict parser
    accepts the old key, and make `validate` reject it with a migration message. Without this
    field, `KnownFields(true)` produces "field backoff_ticks not found in type config.Config",
    which does not tell the operator what to write.

    The message must name the file, the old key, the new key, and a value to copy. For example:

    ```
    retry.backoff_ticks is no longer supported; ticks are no longer a fixed
    interval. Replace it with retry.backoff, a list of durations:

      backoff: [0s, 15m, 30m]
    ```

  **Acceptance:** tests in `internal/config/config_test.go`:
  - A file with `backoff: [0s, 15m, 30m]` loads, and `cfg.Retry.Backoff[1].Std()` is 15
    minutes.
  - A file with `backoff_ticks: [0, 1, 2]` fails, and the error text contains `retry.backoff`.
  - A file with `max: 3` and two backoff entries fails.

- [ ] **C3. Engine and tick: decide on the clock, write the deadline.**  `review: yes`

  In `internal/engine/engine.go`, `retryDecision` at line 146:
  - Replace `if wait > 0 && st.TickCount-state.LastRetryTick < int64(wait)` with a comparison
    of `now` against `state.RetryAfter`. `Decide` already receives `now` and stays pure.
  - Keep the retry cap check, the park decision, and every existing comment.

  In `internal/loopcmd/tick.go`, `dispatch`:
  - Where the code sets `state.LastRetryTick = st.TickCount` for
    `KindRetryStart`/`KindRetryResume`, also set
    `state.RetryAfter = now.Add(cfg.Retry.Backoff[n].Std())`, where `n` is the retry count
    **after** the increment, clamped to the last entry. Add a comment: the deadline is written
    at dispatch, so a tick that declines to act loses nothing.
  - Clear `state.RetryAfter` where the code clears `NeedsRetry` on a human trigger.

  `st.TickCount` keeps its other uses. Do not remove `TickCount` from `engine.State`.

  **Acceptance:** tests in `internal/engine/engine_test.go`:
  - An issue with `NeedsRetry`, the in-flight label, and `RetryAfter` in the future produces no
    decision.
  - The same issue with `RetryAfter` in the past produces a retry decision.
  - The retry cap still parks, regardless of `RetryAfter`.
  - A test in `internal/loopcmd` proves a retry dispatch writes a `RetryAfter` equal to
    `now + backoff[n]`.

- [ ] **C4. Update the examples and the configuration reference.**  `review: no`

  - `examples/planning.yaml:44` and `examples/execution.yaml:36`: replace
    `backoff_ticks: [0, 1, 2]` with `backoff: [0s, 15m, 30m]`.
  - `docs/configuration.md`: replace the `retry.backoff_ticks` row at line 150 and the section
    at lines 535-548. Say that the value is a duration, that the deadline is stored on the
    issue, and that both the command and the daemon read it.

  **Acceptance:** `internal/config/examples_test.go` still passes; it loads both example files.

### Phase D — registering the webhook at GitHub

- [ ] **D1. Repository hooks in `internal/ghub`.**  `review: yes`

  Add to `ghub.Client` and `GitHubClient`:

  ```go
  ListHooks(ctx context.Context, owner, repo string) ([]Hook, error)
  CreateHook(ctx context.Context, owner, repo string, h HookSpec) (int64, error)
  EditHook(ctx context.Context, owner, repo string, id int64, h HookSpec) error
  ```

  Define local types in `internal/ghub/types.go`, so no caller imports `go-github`:

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
  ```

  Set `Config.ContentType` to `"json"` and `Config.InsecureSSL` to `"0"`. Set `Name` to
  `"web"`. Set `Active` to true.

  Two comments the code must carry:
  - GitHub returns `Config.Secret` obfuscated, so a hook is matched by `Config.URL` alone.
    Comparing the secret would create a new hook on every run.
  - A token without the `admin:repo_hook` scope gets a 404 from this endpoint, not a 403.
    Translate a 404 into an error that names the scope, or the operator reads it as "the
    repository does not exist".

  `ListHooks` paginates, the way `ListOpenIssues` does.

  **Acceptance:** tests using `httptest` and a `github.Client` pointed at it (the pattern
  `internal/ghub` already uses, if present; otherwise add one). A 404 from the hooks endpoint
  produces an error whose text contains `admin:repo_hook`. `ListHooks` follows a second page.

- [ ] **D2. `agent-utils project register-webhook`.**  `review: yes`

  Add the subcommand under `projectCommand()`.

  ```
  agent-utils project register-webhook [--name <loop>] [--all]
  ```

  Steps:
  1. `openProject`, then `config.List(p.Dir)`. `--name` restricts to one loop.
  2. Collect the distinct `repo:` values. Skip a loop whose file failed to load, and say so.
  3. `settings.Load`. An empty `webhook.url` or `webhook.secret` is an error naming
     `agent-utils config webhook --enable --url <url>`.
  4. Require `GITHUB_TOKEN`, with the existing error text.
  5. For each repository: `ListHooks`, find a hook whose `URL` equals `webhook.url`, then
     `EditHook` or `CreateHook`.
  6. Print one line per repository saying `created` or `updated`, with the hook id.

  When `webhook.enabled` is false, do the work and print a warning that the listener will
  refuse to start. Registering before enabling is a reasonable order.

  **Acceptance:** a test with a fake `ghub.Client` proves that a second run calls `EditHook`
  and not `CreateHook`, that two loops on one repository produce one call, and that a missing
  `webhook.url` fails before any GitHub call.

### Phase E — the daemon

- [ ] **E1. `internal/listener`: the HTTP surface and signature verification.**  `review: yes`

  Create `internal/listener/listener.go` and `internal/listener/handler.go`. The package
  comment states that this is the one surface in the program that a stranger can reach, and
  that every decision in it is about proving a request came from GitHub.

  ```go
  type Server struct {
      Settings *settings.Settings
      // Tick runs one loop. It is a seam so a test can drive the handler without
      // starting an agent.
      Tick func(ctx context.Context, repo string)
  }

  func (s *Server) Handler() http.Handler
  func (s *Server) ListenAndServe(ctx context.Context) error
  ```

  `POST /webhook`:
  1. A method other than POST gives 405.
  2. `r.Body = http.MaxBytesReader(w, r.Body, 5<<20)`.
  3. `sig := r.Header.Get(github.SHA256SignatureHeader)`; an empty value gives 400. Comment:
     `github.ValidatePayload` falls back to the SHA-1 header at `messages.go:256`, so this code
     reads the header itself and never accepts a SHA-1 signature.
  4. `github.ValidatePayloadFromBody(contentType, r.Body, sig, []byte(secret))`; an error gives
     401. Log the delivery id, never the signature or the secret.
  5. `github.WebHookType(r)`; an event outside the five subscribed events gives 204.
  6. Decode `struct{ Repository struct{ FullName string \`json:"full_name"\` } \`json:"repository"\` }`
     from the payload. An empty name gives 400.
  7. Answer 202, then call `s.Tick` in a goroutine. Comment: GitHub times a delivery out after
     10 seconds, and a tick makes several API calls and can start an agent.

  `GET /healthz` returns 200 and the body `ok`.

  `ListenAndServe` binds `net.JoinHostPort(addr, port)`, and shuts down on context
  cancellation with `http.Server.Shutdown`. Set `ReadHeaderTimeout` so a slow-header client
  cannot hold a connection open.

  **Acceptance:** tests using `httptest.NewServer(s.Handler())`:
  - A body signed with the correct secret gives 202 and calls `Tick` once with the repository
    name.
  - A wrong signature gives 401 and does not call `Tick`.
  - No signature header gives 400.
  - **A body signed with HMAC-SHA1 in `X-Hub-Signature`, with no SHA-256 header, gives 400 and
    does not call `Tick`.** This test is the point of the task.
  - An unsubscribed event gives 204.
  - A GET gives 405.
  - `/healthz` gives 200.

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
  func Targets(repo string) []Target
  ```

  It reads `registry.List()`, skips a project whose `Exists()` is false with a log line, calls
  `config.List(p.AgentUtilsDir)` for the rest, skips an entry whose `Err` is non-nil with a log
  line, and keeps an entry whose `Repo` matches with `strings.EqualFold`. Comment: the case
  fold matches `ghub.ListOpenPullRequests`, which already folds because GitHub's casing and the
  configuration's casing need not agree.

  Comment why nothing is cached: an operator who adds a loop expects the next delivery to use
  it, and the scan reads a few small files, which costs less than the GitHub API call the tick
  then makes.

  **Acceptance:** a test builds two temporary projects under a temporary `$AGENT_UTILS_HOME`,
  registers both, and proves:
  - A repository shared by two projects returns both loops.
  - A project directory that was deleted is skipped and does not fail the call.
  - A loop file that does not parse is skipped and does not fail the call.
  - The match ignores case.

- [ ] **E3. `internal/listener`: the token file.**  `review: yes`

  Add `internal/listener/env.go`:

  ```go
  // Token reads GITHUB_TOKEN from ~/.agent-utils/env.
  func Token() (string, error)
  ```

  It parses lines of the form `export KEY=value` and `KEY=value`, ignores a blank line and a
  `#` comment, and strips one layer of matching single or double quotes. It reads the file on
  every call, so a rotated token needs no restart.

  It returns an error when the file's mode allows group or other any access. Comment: the file
  holds a repository-write credential, and the README tells the operator to create it with
  `install -m 600`.

  **Acceptance:** tests with a temporary `$AGENT_UTILS_HOME`:
  - `export GITHUB_TOKEN=abc` is read.
  - `GITHUB_TOKEN=abc` is read.
  - A quoted value loses its quotes.
  - A comment line and a blank line are ignored.
  - A file at mode 0644 gives an error naming the mode.
  - An absent file gives an error naming the path.

- [ ] **E4. `internal/listener`: running ticks, with retry and a wake-up timer.**  `review: yes`

  Add `internal/listener/work.go`. This is the seam `Server.Tick` points at in production.

  ```go
  type Worker struct {
      DB    *store.DB
      Now   func() time.Time
      Sleep func(d time.Duration) <-chan time.Time
  }

  // Run ticks every loop that watches repo.
  func (w *Worker) Run(ctx context.Context, repo string)

  // Wake ticks the loop whose issue retry deadline has passed.
  func (w *Worker) Wake(ctx context.Context) (next time.Time)
  ```

  `Run`:
  1. `Targets(repo)`.
  2. For each target: `Token()`, then `loopcmd.Open(ref, target.ConfigPath, loopcmd.Options{
     Token: tok, RequireGitHub: true, MigrationPolicy: loopcmd.FailOnUnimported})`, then
     `loopcmd.RunTick`.
  3. `errors.Is(err, lock.ErrHeld)`: log at info and return. Comment: the delivery carries no
     state, so the tick that holds the lock reads the same GitHub state a moment later.
  4. Any other error: log at error and schedule a retry.

  The retry schedule is a map keyed by project id and loop name, held under a mutex. An entry
  holds the attempt count and a timer. The delay comes from the loop's own
  `cfg.Retry.Backoff`, clamped to the last entry, and the worker stops after `cfg.Retry.Max`
  attempts. Comment: the same list governs an issue retry and a tick retry, because both exist
  for the same failure — the platform, not the work.

  `Wake` calls `DB.EarliestRetryAfter`. When the deadline has passed it ticks that loop. It
  returns the next deadline so the caller can reset its timer. Comment: without this, an issue
  in backoff waits for unrelated repository activity, which may never come.

  The daemon's main loop selects over the context, the wake timer, and the retry timers.

  **Acceptance:** tests with a fake `Targets` result, a fake tick function, and a controlled
  clock:
  - A tick that returns `lock.ErrHeld` schedules no retry.
  - A tick that returns another error is retried after the configured delay.
  - The retry stops after `retry.max` attempts.
  - `Wake` ticks the loop whose `retry_after` has passed and returns the next deadline.
  - Two targets both run when one of them fails.

- [ ] **E5. `internal/launchd`: the service manager.**  `review: no`

  Create `internal/launchd`. Define the interface in a platform-neutral file:

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
      PlistPath() (string, error)
  }

  func New() Manager   // launchd on darwin, an unsupported stub elsewhere
  ```

  `service_darwin.go` implements it. `service_other.go` returns a manager whose methods report
  that `--daemon` supports macOS only, and that the foreground `listener start` works
  everywhere. Use build tags.

  The label is `com.seanmcgary.agent-utils.listener`. The plist goes at
  `~/Library/LaunchAgents/<label>.plist`, at mode 0644.

  The plist holds: `Label`, `ProgramArguments` (the absolute binary path, then `listener`, then
  `start`), `RunAtLoad` true, `KeepAlive` true, `StandardOutPath` and `StandardErrorPath` under
  the machine-wide directory, and `WorkingDirectory` set to the machine-wide directory.

  It holds **no** `EnvironmentVariables` and **no** token. Comment: the daemon reads the token
  from `~/.agent-utils/env`, so this file, which is world readable, never holds a credential.

  `Install` writes the plist, then runs `launchctl bootstrap gui/<uid> <path>`. `Uninstall`
  runs `launchctl bootout gui/<uid>/<label>` and treats a non-zero status as success, then
  removes the plist. `Status` runs `launchctl print gui/<uid>/<label>` and reads the pid.

  **Acceptance:** a test renders the plist to a string and asserts: the label is present, the
  argument list ends with `listener start`, `RunAtLoad` is true, and the text contains neither
  `EnvironmentVariables` nor `GITHUB_TOKEN`. A test proves `New()` on a non-darwin build tag
  returns the stub. No test runs `launchctl`.

- [ ] **E6. The `listener` command group.**  `review: no`

  Add a top-level `listenerCommand()`.

  ```
  agent-utils listener start [--daemon] [--listen-port N] [--listen-addr A]
  agent-utils listener stop
  agent-utils listener status
  ```

  `start`:
  - `settings.Load`. `webhook.enabled` false is an error naming
    `agent-utils config webhook --enable`.
  - An empty `webhook.secret` is an error. Refuse to run an unauthenticated listener.
  - `--listen-port` and `--listen-addr` override the stored values for this run only.
  - Without `--daemon`: open the state database, build the `Worker`, build the `Server`, and
    serve until SIGINT or SIGTERM, then shut down.
  - With `--daemon`: call `launchd.New().Install(self, args)` where `args` carries any port or
    address override, print the plist path, and return.

  `stop` calls `Uninstall`. `status` calls `Status` and prints the state, the pid, and the
  address the daemon binds.

  Register `listenerCommand()` in `main.go` beside `listCommand()`.

  **Acceptance:** `agent-utils listener start` with `webhook.enabled` false exits non-zero and
  names the `config webhook` command. `agent-utils listener --help` lists three subcommands.
  A test drives `start` with a temporary settings file, a port of 0, and a cancelled context,
  and proves the server starts and stops without an error.

### Phase F — documentation

- [ ] **F1. README and configuration reference.**  `review: no`

  `README.md`:
  - Add `config` and `listener` rows to the "Global" command table, and a
    `project register-webhook` row to the "Project" table.
  - Add a "Webhooks" section after "Cron". Give the whole setup in order:
    `config webhook --enable --url ...`, `project register-webhook`,
    `listener start --daemon`. Say that the daemon speaks plain HTTP and needs a proxy or a
    tunnel that terminates TLS. Say that the default bind address is `127.0.0.1`.
  - Rewrite the "Cron" section to say that cron is now optional, and that a cron entry and the
    daemon can both run, because the lock makes an overlapping tick harmless.
  - Add to "Security": the listener accepts a request from the internet that starts an agent.
    Say that every delivery is verified with HMAC-SHA256 over the raw body, that a SHA-1
    signature is refused, and that the webhook shortens the delay before untrusted issue text
    reaches an agent but does not change the trust rule already stated there.
  - Note the `retry.backoff_ticks` rename under a short "Upgrading" note.

  `docs/configuration.md`: already handled by C4. Add a pointer to the machine-wide
  `config.yaml`, and say clearly that it is a different file from a project's `config.yaml`.

  **Acceptance:** `make fmtcheck` passes. Every command named in the README exists in
  `agent-utils --help` output.

## Risks

1. **The `retry.backoff_ticks` rename breaks every existing loop configuration.** This is the
   one decision that needs approval before Phase C starts. The mitigation is a targeted
   migration error rather than an unknown-field error, plus both examples and the reference
   updated in the same commit. If the reader rejects the rename, Phase C reduces to C1 alone
   (the column stays, written but not read by `engine`), and the daemon's wake timer uses a
   fixed interval instead.

2. **The daemon runs agents from an internet-reachable endpoint.** The controls are in the
   design's security table. The SHA-1 downgrade test in E1 is the single most important test in
   this plan; do not let it be dropped as redundant.

3. **`Targets` reads the filesystem on every delivery.** A machine with many projects pays a
   cost per delivery. The cost is bounded by the number of loop files and is far below the
   GitHub API call that follows. Revisit only with a measurement.

4. **`retry_after` semantics differ between a cron run and a daemon run during the upgrade.**
   A database written by an old binary has `retry_after = 0` for every issue, which reads as
   "no deadline" and retries at once. That is the same behaviour the old `[0, ...]` first entry
   gives, so the upgrade does not strand an issue.

5. **launchd is not covered by an automated test.** E5 tests the rendered plist only. Verify
   `listener start --daemon`, `stop`, and `status` by hand once on this machine before the PR
   is marked ready, and record the result in the PR body.
