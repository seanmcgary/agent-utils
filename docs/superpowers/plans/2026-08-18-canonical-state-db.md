# Canonical State Database — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep all durable loop state in one SQLite database at `~/.agent-utils/state.db`, keyed
by project UUID. Import every existing per-loop database automatically, and lose nothing.

**Architecture:** `store.DB` is the one canonical handle. `DB.Project(id)` returns a
`*store.Store` scoped to one project; the scoped type keeps today's method set, so the loop code
barely changes. Global reads live on `DB`. All import SQL lives in `internal/store`, because that
package owns the schema. `internal/legacydb` reads an old file and writes nothing to it.
`internal/migrate` finds the old files, decides what to do, and calls the two packages above. A
source whose runner processes still run stays open, and is read again until it is idle.

**Tech stack:** Go 1.25.9, `modernc.org/sqlite` (no CGO), `urfave/cli` v3, `gopkg.in/yaml.v3`.

**Spec:** `docs/superpowers/specs/2026-08-18-canonical-state-db.md`

## Pipeline State

| Field   | Value                                                              |
|---------|--------------------------------------------------------------------|
| stage   | 4 (commit review)                                                   |
| class   | large (schema change, data migration, concurrency, data integrity)  |
| profile | backend                                                             |
| branch  | feat/canonical-state-db                                             |
| pr      | #3                                                                  |
| gate    | waived by the operator on 2026-08-18 ("build it and open the PR")   |
| round   | 0                                                                   |

## Global Constraints

This repository has **no** conventions document at its root. There is no `AGENTS.md`,
`CLAUDE.md`, `CONTRIBUTING.md`, `STANDARDS.md`, or `STYLEGUIDE.md`. The only binding convention
file is the operator's global instruction file at `~/.claude/CLAUDE.md`. It is copied here word
for word:

> - when indenting javascript/typescript chained function calls, always align the .functionName()
> with the start of the line above. e.g.
> Promise.resolve()
> .then(() => 'stuff');
>
> NOT
>
> Promsie.resolve()
>     .then(() => 'stuff');

That rule governs JavaScript and TypeScript. This project is Go, so no task changes because of
it.

The remaining constraints come from the repository itself. Every task must respect them:

- Module path is `github.com/seanmcgary/agent-utils`. Use `modernc.org/sqlite`. Never add a
  dependency that needs CGO.
- `make check` must pass: `gofmt -l`, `go vet ./...`, `golangci-lint run`, `go test -p 1 ./...`.
  `errcheck`, `errorlint`, `govet`, `ineffassign`, `staticcheck`, and `unused` are enabled
  (`.golangci.yml`). CI runs the same gates (`.github/workflows/main.yml`).
- Wrap every returned error with `fmt.Errorf("<what failed>: %w", err)`. Compare sentinels with
  `errors.Is`. Name a sentinel `Err<Thing>`.
- Every package has a package doc comment that says why the package exists. Every exported
  symbol has a doc comment. A comment says **why**, and names the failure the line prevents.
  Match that voice (`internal/store/store.go:13-17`, `internal/registry/registry.go:1-6`).
- Packages under `internal/` are flat, one responsibility each. Split a package into files by
  concern, as `internal/runner` and `internal/engine` do. Do not nest a package inside another.
- Name a repeated string literal as a constant (`internal/store/types.go:13-16`).
- Tests are plain `func TestX(t *testing.T)` in the package under test, with `t.TempDir()` and
  `t.Setenv` for isolation. No table tests. Name a test for the behavior it protects.
- `internal/config/docs_test.go` fails the build when a config field is undocumented. Keep
  `docs/configuration.md` true.
- Commit messages use conventional-commit prefixes (`feat:`, `fix:`, `docs:`, `refactor(cli):`).
  **Never** add a `Co-Authored-By` or any other AI-attribution trailer.
- `engine.Decide` stays pure. No task in this plan touches it.
- Go must not write to GitHub. No task in this plan makes a GitHub call.
- **Never delete, rename, or rewrite a legacy `state.db`.** It is the only copy of that state
  until the import succeeds, and an old runner process may still be writing it.

## Verified external API (do not re-derive)

Each signature below was read from this repository or from the Go standard library.

```go
// internal/store/store.go:88-108 — the DSN form the driver accepts. Pragmas MUST be in the DSN;
// busy_timeout and foreign_keys are per connection. Open also chmods the file and its sidecars.
dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
db, err := sql.Open("sqlite", dsn)   // driver name is "sqlite" (modernc.org/sqlite)
db.SetMaxOpenConns(1)
for _, suffix := range []string{"", "-wal", "-shm"} { _ = os.Chmod(path+suffix, 0o600) }

// internal/registry/registry.go:268-282 — the file-lock form already used in the home directory.
lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
err = syscall.Flock(int(lf.Fd()), syscall.LOCK_EX)   // blocking; LOCK_UN to release

// internal/proc/proc.go — liveness matches the runner's own command line.
const DispatchFlag = "--dispatch"
func IsAlive(pid int, dispatchID int64) bool

// internal/runner/runner.go:21-24, 33-49 — Spawn builds the argv AND sets cmd.Env = agentEnv().
func RunnerLogPath(stateDir, loop string, dispatchID int64) string
func Spawn(selfPath string, dispatchID int64, configPath, runnerLog string) (int, error)
// internal/runner/runner.go:247-259 — agentEnv() is an ALLOWLIST. A variable not in `keep`
// does not reach the runner or the agent.

// internal/loopcmd/tick.go:153 — the exact text the reaper writes. Task 7 matches on it.
APIError: "runner process died"

// internal/project/project.go:29-35 — the project key. ID is a UUID, minted once.
type Config struct { Name string; ID string }
func Load(agentUtilsDir string) (*Config, error)

// internal/registry/registry.go
func List() ([]Project, error)     // Project{ID, Name, Root, AgentUtilsDir, ...}; sorted
func Path() (string, error)

// internal/config/discover.go
func (c *Config) ResolveStateDir(configPath string) (string, error)  // :244; may be relative
func DirFromPath(path string) string                                 // :216; "" when not found
func List(agentUtilsDir string) ([]Entry, error)                     // :110; ErrNoConfigs when empty
var ErrNoConfigs = ...                                               // :21-30
// Entry{Name, File, Path, Repo, Err} — a broken file is RETURNED with Err set, not dropped.

// database/sql — the transaction API used by the importer.
func (db *sql.DB) Begin() (*sql.Tx, error)
func (tx *sql.Tx) Exec(query string, args ...any) (sql.Result, error)
func (tx *sql.Tx) Query(query string, args ...any) (*sql.Rows, error)
func (tx *sql.Tx) Rollback() error
func (tx *sql.Tx) Commit() error

// os — home resolution. os.UserHomeDir reads $HOME on unix, so a test can set it.
func os.UserHomeDir() (string, error)
```

SQLite facts this plan depends on:

- `ALTER TABLE` cannot change a primary key. A table whose key changes must be rebuilt:
  create, copy, drop, rename.
- `DROP TABLE` also drops that table's indexes. Re-run the `CREATE INDEX IF NOT EXISTS`
  statements after a rebuild.
- A transaction whose FIRST statement writes takes the write lock immediately. There is no
  deferred-to-write upgrade to lose. This is how Task 3 serializes the rebuild between
  processes without a new lock file.
- A partial unique index (`... WHERE legacy_source <> ''`) is supported.
- `ON CONFLICT (cols)` requires a unique index on exactly those columns.

## File structure

| Path | Responsibility |
|---|---|
| `internal/home/home.go` | The one place that resolves `~/.agent-utils`. **New.** |
| `internal/store/store.go` | Schema, schema upgrade, `DB`, scoped `Store`, global reads. |
| `internal/store/types.go` | `ProjectID`, legacy fields, `Dispatch.RunnerID()`, `Tick`, `Cooldown`. |
| `internal/store/legacy.go` | The import transaction. All import SQL lives here. **New.** |
| `internal/legacydb/legacydb.go` | Reads an old `state.db`. Writes nothing. **New.** |
| `internal/migrate/migrate.go` | Locking, per-source orchestration, dry run. **New.** |
| `internal/migrate/discover.go` | Source discovery, targeted and machine-wide. **New.** |
| `internal/migrate/types.go` | `Source`, `Result`, `Report`, state constants. **New.** |
| `internal/registry/registry.go` | Uses `internal/home`. |
| `internal/loopcmd/*.go` | Scoped store; cross-project reads from one database. |
| `cmd/agent-utils/main.go` | Opens the canonical database; `--project`; `migrate`. |
| `.golangci.yml` | errcheck exclusion follows the `DB`/`Store` split. |
| `README.md`, `docs/configuration.md`, `examples/*.yaml` | Documented state layout. |

---

## Task 1 — `internal/home`, the one home-directory resolver

**review: no**

- [ ] Create `internal/home/home.go` with a package doc comment that says why it exists: the
  registry and the canonical database must always agree on one directory, and a test must be
  able to move that directory without changing `$HOME`, which git and the agent environment
  still need.
- [ ] API:
  - `func Dir() (string, error)` — returns `$AGENT_UTILS_HOME` when it is set and not blank,
    and `<user home>/.agent-utils` otherwise. `$AGENT_UTILS_HOME` names the `.agent-utils`
    directory ITSELF, not a `$HOME` replacement. When it is set and names something that
    exists and is not a directory, return an error. That matches `AGENT_UTILS_DIR`
    (`internal/config/discover.go:73-79`), which errors rather than falling back silently.
  - `func StateDBPath() (string, error)` — `<Dir()>/state.db`.
  - `func LockPath(name string) (string, error)` — `<Dir()>/<name>`.
  - `func EnsureDir() (string, error)` — creates `Dir()` with mode `0o700` and returns it. The
    directory holds session identifiers, so it must not be group readable.
- [ ] Tests: the override wins; the override that names a file is an error; the default ends in
  `.agent-utils`; `EnsureDir` creates the directory with mode `0700`.

**Acceptance:** `go test ./internal/home/...` passes. `gofmt`, `go vet`, `golangci-lint` clean.

## Task 2 — Registry uses `internal/home`

**review: no**

- [ ] Change `registry.Path()` to call `home.Dir()`. Keep the exported signature.
- [ ] Update the doc comments the change falsifies: `registry.Path` (`registry.go:52-57`) says
  `$HOME/.agent-utils/registry.json`; the package comment (`registry.go:1-6`) says deleting the
  registry "loses nothing but the list", which stops being true once `migrate.DiscoverAll` uses
  it to find unimported state. Say what is lost: the machine-wide sweep can no longer find a
  project, so that project is imported the next time a command runs inside it.
- [ ] Add `t.Setenv("AGENT_UTILS_HOME", "")` to every registry test that sets only `HOME`
  (`registry_test.go:20,48,59,84,111,136,167`). An exported override in the developer's shell
  would otherwise point the tests at the real home directory. This is the precedent
  `discover_test.go:26` sets for `AGENT_UTILS_DIR`.

**Acceptance:** `go test ./internal/registry/...` passes with `AGENT_UTILS_HOME` both unset and
set to a temporary directory.

## Task 3 — Schema version 2: project keying and the legacy columns

**review: yes** — this task rebuilds tables and can lose rows.

- [ ] In `internal/store/store.go`, rewrite the `schema` constant:
  - `issues`: add `project_id TEXT NOT NULL DEFAULT ''` as the first column. Primary key
    `(project_id, loop, repo, number)`.
  - `pr_links`: the same. Primary key `(project_id, loop, repo, number)`.
  - `cooldowns`: add `project_id TEXT NOT NULL DEFAULT ''`. Primary key `(project_id, loop)`.
  - `dispatches`: add `project_id TEXT NOT NULL DEFAULT ''`,
    `legacy_source TEXT NOT NULL DEFAULT ''`, `legacy_id INTEGER NOT NULL DEFAULT 0`. Primary
    key stays `id`.
  - `ticks`: add `project_id TEXT NOT NULL DEFAULT ''`. Primary key stays `id`.
  - Indexes: `dispatches_running_project` on `(project_id, loop, repo, status)`; `ticks_loop`
    on `(project_id, loop)`; unique `dispatches_legacy` on
    `(legacy_source, legacy_id, project_id, loop)` `WHERE legacy_source <> ''`.
  - New table:

```sql
CREATE TABLE IF NOT EXISTS legacy_sources (
  path              TEXT NOT NULL,      -- absolute, symlink-resolved source path
  project_id        TEXT NOT NULL,
  loop              TEXT NOT NULL,
  repo              TEXT NOT NULL DEFAULT '',
  state             TEXT NOT NULL,      -- store.SourceOpen or store.SourceSealed
  first_imported_at TIMESTAMP NOT NULL,
  last_imported_at  TIMESTAMP NOT NULL,
  PRIMARY KEY (path, project_id, loop)
);
```

  `DEFAULT ''` on every new NOT NULL column is deliberate. It keeps an `INSERT` written by an
  older binary working when the canonical file happens to BE that binary's state file.

- [ ] Explain the key in a comment: two loops may share one `state_dir`
  (`docs/configuration.md:146`), so a source is identified by its path AND its loop AND the
  project that claims it. Nothing else separates them.
- [ ] Extend `addedColumns` with the **four** new columns: `dispatches.project_id`,
  `dispatches.legacy_source`, `dispatches.legacy_id`, `ticks.project_id`. That path already
  handles a file created by an older binary.
- [ ] Raise `busy_timeout` in the DSN from `10000` to `30000`. One file now takes the writes of
  every tick and every runner on the machine. Say that in the comment above the DSN.
- [ ] Restructure `Open` so the DDL, `addedColumns`, and the new key upgrade run in **one
  transaction whose first statement is the DDL**:
  1. `tx, err := db.Begin()`.
  2. `tx.Exec(schema)` — a write, so the transaction holds the write lock from here on. A
     second process entering `Open` at the same time blocks for `busy_timeout` and then sees
     the finished work.
  3. `addedColumns` — must run BEFORE the key upgrade. The rebuild's `INSERT ... SELECT` names
     `needs_retry`, `session_started`, `parked`, and `behind_by`, which exist on a
     pre-release file only after this pass.
  4. `upgradeKeys(tx)`.
  5. `tx.Exec(schema)` again, to recreate the indexes that `DROP TABLE` removed.
  6. `tx.Commit()`. `defer func() { _ = tx.Rollback() }()` covers every early return.
- [ ] `upgradeKeys(tx *sql.Tx) error`:
  - Re-read column presence INSIDE the transaction. Return early when `issues` already has
    `project_id`. The check and the rebuild must be in the same transaction, or two processes
    both rebuild and the second drops the first's stamped rows.
  - Rebuild `issues`, `pr_links`, and `cooldowns`: create `<name>_v2` with the new shape,
    `INSERT ... SELECT` with `''` for `project_id`, `DROP TABLE <name>`, rename.
  - Drop the old `dispatches_running` index if it exists.
  - A row copied with an empty `project_id` is invisible to every scoped query, because a
    project identifier is a UUID and is never empty. Task 7 stamps such rows when the source
    file IS the canonical file. Say this in a comment.
- [ ] Keep the `0600` chmod of the database and its `-wal`/`-shm` sidecars
  (`store.go:110-113`). It matters more now: one file holds every project's session
  identifiers.
- [ ] Known limitation, recorded in a comment on `upgradeKeys`: when a loop's `state_dir` IS
  the home directory, a runner from the OLD binary that is still alive writes `issues` with
  `ON CONFLICT(loop, repo, number)`. That conflict target no longer has a matching unique
  index, so its issue-state write fails while its dispatch write succeeds. The window is one
  upgrade with one in-flight runner on one unusual configuration. The tick's reaper still
  retires the row, so the loop recovers on the next tick. Do not attempt to keep a second
  unique index for it: that index would defeat project keying.
- [ ] Tests in `internal/store/store_test.go`:
  - A database created by the current code has `project_id` on all five tables.
  - A database built with the OLD DDL (write the old `CREATE TABLE` statements in the test),
    holding one row per table, is upgraded on open: every row survives with `project_id = ''`.
  - The upgrade is idempotent: two `Open` calls leave identical row counts.
  - Two goroutines calling `Open` on the same fresh path concurrently both succeed, and the
    row counts are unchanged. Run it under `-race`.

**Acceptance:** the four tests above pass. No existing store test is deleted.

## Task 4 — Project scope on the store API

**review: yes** — a wrong scope reads or writes another project's rows.

- [ ] Split the type in `internal/store/store.go`:
  - `type DB struct { db *sql.DB }`, returned by `Open(path string) (*DB, error)`.
  - `func (d *DB) Project(projectID string) *Store`.
  - `func (d *DB) Close() error`.
  - `type Store struct { db *sql.DB; projectID string }` keeps **every current method
    signature**, with one exception: **remove `Store.Close`**. A scoped view must not close the
    handle every other scope in the process shares.
- [ ] Add `project_id = ?` to every `WHERE` and every `INSERT` in the scoped methods. Every
  `ON CONFLICT` target gains `project_id`.
- [ ] `Store.DispatchesBySession` and `Store.GetDispatch` also filter by `project_id`. A session
  identifier is unique, but a scoped read must not be able to return another project's row.
- [ ] Add `ProjectID` to `IssueState`, `Dispatch`, and `PRLink`; populate it on every scan. Add
  `LegacyID int64` and `LegacySource string` to `Dispatch`.
- [ ] Add `Tick` and `Cooldown` structs to `types.go`. The importer needs them, and no type
  exists today.
- [ ] Add the global reads on `DB`:
  - `func (d *DB) RunningDispatches() ([]Dispatch, error)` — every project.
  - `func (d *DB) DispatchesForProject(projectID string) ([]Dispatch, error)` — newest first.
  - `func (d *DB) LoopStates() ([]LoopState, error)` where
    `type LoopState struct { ProjectID, Loop string; Ticks int64; LastTick time.Time; Cost float64 }`.
    Build it from TWO queries and merge in Go: one over `ticks` grouped by
    `(project_id, loop)`, one over `dispatches` grouped by `(project_id, loop)` summing
    `cost_usd`. `ticks` has no `repo` column (`store.go:69-75`), so a single grouped query
    cannot produce this, and a loop that ticked but never dispatched must still appear.
- [ ] Update `.golangci.yml`: replace the errcheck exclusion
  `(*github.com/seanmcgary/agent-utils/internal/store.Store).Close` with
  `(*github.com/seanmcgary/agent-utils/internal/store.DB).Close`. Without this, every
  `defer db.Close()` fails `make lint` and the CI lint job.
- [ ] Update every existing caller of `store.Open`, including
  `internal/runner/runner_test.go:30`, `internal/loopcmd/tick_test.go:79`,
  `internal/store/store_test.go`, and `internal/loopcmd/regression_test.go`. Each becomes
  `Open(path)` then `.Project("<some uuid>")`.
- [ ] Tests: two projects write the same loop name, repository, and issue number; each scoped
  read returns only its own rows; a delete in one project leaves the other untouched;
  `RunningDispatches` returns both; `LoopStates` reports a loop that has ticks and no
  dispatches.

**Acceptance:** the isolation tests pass and every existing store test passes after being
re-pointed through `Open(...).Project(...)`.

## Task 5 — `Dispatch.RunnerID()` and every liveness call site

**review: yes** — a wrong identifier here reaps a live agent or double-dispatches one.

- [ ] Add to `internal/store/types.go`:

```go
// RunnerID is the dispatch identifier the runner process actually carries.
//
// An imported dispatch was renumbered by the canonical database, but its live
// runner still carries the identifier from the file it was started with, both
// on its command line and in its log file name. Liveness and log paths must use
// this, never ID.
func (d Dispatch) RunnerID() int64 {
    if d.LegacyID != 0 {
        return d.LegacyID
    }
    return d.ID
}
```

- [ ] Replace the identifier at **every** liveness call site. The complete list, verified by
  `grep -rn 'IsAlive(' --include='*.go'`:
  `internal/loopcmd/tick.go:145`, `internal/loopcmd/tick.go:492` (`Reset`),
  `internal/loopcmd/status.go:61`, `internal/loopcmd/sessions.go:119`,
  `internal/loopcmd/projects.go:128`, `internal/loopcmd/logs.go:109`,
  `internal/loopcmd/logs.go:167`.
- [ ] Replace the identifier at every runner-log path site:
  `internal/loopcmd/tick.go:331`, `internal/loopcmd/logs.go:92`.
- [ ] Test: a dispatch with `LegacyID` set reports the legacy identifier; one without reports
  `ID`.

**Acceptance:** `grep -rn 'IsAlive(d.PID, d.ID)' internal cmd` returns nothing, and
`grep -rn 'RunnerLogPath(.*d.ID' internal cmd` returns nothing.

## Task 6 — `internal/legacydb`, the read-only reader

**review: yes** — it reads a file another process may write at the same time.

- [ ] Create `internal/legacydb/legacydb.go` with a package doc comment: it reads a per-loop
  database written by an older layout, and it applies no schema and no migration, because an
  old runner process may still be writing that file with the old code.
- [ ] `func Open(path string) (*DB, error)` — same DSN pragmas as `store.Open`, **no DDL, no
  `addedColumns`, no key upgrade**. Add `func (d *DB) Close() error`.
- [ ] `func (d *DB) Read(loop string) (Data, error)` returns everything for ONE loop in ONE
  read transaction:

```go
// Data is one loop's rows, read from a legacy database in one transaction.
type Data struct {
    Issues     []store.IssueState
    Dispatches []store.Dispatch   // ID holds the LEGACY identifier
    PRLinks    []store.PRLink
    Ticks      []store.Tick
    Cooldown   *store.Cooldown    // nil when the loop has none
}
```

  One transaction is required: reading the dispatch rows and then asking a second time whether
  any still run can see a runner finish in between. The source would then be sealed while
  holding a stale `running` row, and the tick would rewrite a successful run as failed.
- [ ] `func (d Data) HasLiveRunner(isAlive func(pid int, id int64) bool) bool` — true when a
  dispatch row has status `running` AND its process is alive. Seal on liveness, not on status:
  a row left `running` by a crashed runner would otherwise pin the source open forever, so
  `MIGRATED.txt` would never be written and every command would re-read the file.
- [ ] Every read is column-defensive. Call `PRAGMA table_info(<table>)` once per table and
  select only the columns that exist. A file written by an early binary has no `title`, no
  `pr_number`, and no `behind_by`. A missing column takes its zero value.
- [ ] A file with none of the expected tables is not an error. `Read` returns empty `Data`, and
  Task 8 seals it. There is nothing to lose in an empty file, and failing would block a tick.
- [ ] A file that fails a read (corrupt, unreadable, wrong format) returns a wrapped error. Task
  8 decides what that means.
- [ ] Tests: build a database with the old DDL and read it back; a missing-column variant reads
  with zero values; a file with no tables returns empty `Data`; `HasLiveRunner` is true only
  for a running row whose fake `isAlive` says yes; reading a source with two loops in it
  returns only the requested loop's rows.

**Acceptance:** the reader returns every row of both an old-shape and a current-shape file. A
test asserts the source file's row counts and size are unchanged after a read.

## Task 7 — The import transaction, in `internal/store`

**review: yes** — this is the task that can lose a user's state.

- [ ] Create `internal/store/legacy.go`. All import SQL lives here, because this package owns
  the schema. `internal/migrate` must not need raw access to `DB.db`.
- [ ] Types and constants:

```go
// SourceOpen means a legacy file may still be written by a runner from the old
// binary, so it must be read again. SourceSealed means it never will be.
const (
    SourceOpen   = "open"
    SourceSealed = "sealed"
)

// LegacyKey identifies one legacy source. Two loops may share one state_dir, so
// the path alone is not an identity.
type LegacyKey struct { Path, ProjectID, Loop, Repo string }
```

- [ ] `func (d *DB) LegacySourceState(k LegacyKey) (string, bool, error)` — the recorded state
  and whether a row exists.
- [ ] `func (d *DB) ClaimedBy(path, loop string) (projectID string, err error)` — the project
  that already recorded this `(path, loop)`, or `""`. Task 8 uses it to refuse a second claim.
- [ ] `func (d *DB) ImportLegacy(k LegacyKey, data legacyData, seal bool) (int, error)` — one
  transaction, returning the number of rows written. Take `data` as an interface or a struct
  defined HERE so `store` does not import `legacydb` (the dependency runs the other way).
  1. `tx, err := d.db.Begin()`, with `defer func() { _ = tx.Rollback() }()`.
  2. **First import** (no `legacy_sources` row): insert `issues`, `pr_links`, `cooldowns`,
     `ticks`, and `dispatches`, stamping `project_id`. Stamp each dispatch with
     `legacy_source = k.Path` and `legacy_id = <its old id>`. Let SQLite assign the new `id`.
  3. **Refresh** (row exists and is `open`): update only `dispatches` and `issues`.
     - A dispatch matches on `(legacy_source, legacy_id, project_id, loop)`. Copy `status`,
       `exit_code`, `cost_usd`, `duration_ms`, `api_error`, `finished_at`, `pid`,
       `pid_start_at`, and `session_id`. Apply it when the canonical row is still `running`,
       **or** when the reaper retired it (`api_error = 'runner process died'`, the exact text
       at `internal/loopcmd/tick.go:153`). The reaper's verdict on an imported row is a guess
       made from a process that had already exited; the source file holds the truth.
     - An issue matches on its key, and is updated only when the source row's `updated_at` is
       strictly newer. Copy **only** the four columns the legacy runner writes:
       `needs_retry`, `session_started`, `parked`, `retry_count` (plus `updated_at`). A
       whole-row copy would drag the frozen `session_id` and `worktree_path` over values the
       new binary wrote after the import (`internal/runner/runner.go:212-234`).
     - Never touch `ticks`, `pr_links`, or `cooldowns` on a refresh. A tick after the upgrade
       writes to the canonical database, so a source cannot hold newer rows there.
  4. **Canonical special case** — `k.Path` is the canonical database path. Copy nothing.
     `UPDATE <table> SET project_id = ? WHERE project_id = '' AND loop = ?` on each of the five
     tables, then seal. The `loop` guard is what keeps two loops in that directory apart.
  5. Upsert the `legacy_sources` row with the state the caller passed.
  6. `tx.Commit()`.
- [ ] Tests in `internal/store/legacy_test.go`:
  - A first import writes every row and the rows read back through the scoped store.
  - Two projects import sources holding the same loop name, repository, and issue numbers; the
    scoped reads stay separate.
  - A second import of the same key writes nothing new (row counts unchanged).
  - A refresh applies a finished outcome to a row that is still `running`.
  - A refresh applies a finished outcome to a row the reaper marked failed with
    `runner process died`.
  - A refresh does NOT overwrite an issue row the canonical database updated more recently, and
    does NOT overwrite `session_id` when it does apply.
  - The canonical special case stamps `project_id = ''` rows for the named loop only.

**Acceptance:** all seven tests pass.

## Task 8 — `internal/migrate`, discovery and orchestration

**review: yes** — this task decides when a tick is allowed to proceed.

- [ ] `internal/migrate/types.go`:

```go
// Source is one legacy database file and the loop inside it.
type Source struct { Path, ProjectID, ProjectName, Loop, Repo string }

// Result is what happened to one source.
type Result struct {
    Source Source
    State  string  // Imported, Refreshed, Sealed, Skipped, Failed
    Rows   int     // rows written into the canonical database; 0 on a skip
    Reason string  // why it was skipped, in one sentence
    Err    error
}

// Report is the outcome of one run.
type Report struct { Results []Result }
func (r Report) Failed() []Result
```

  Name each state as a constant.
- [ ] `internal/migrate/discover.go`:
  - `func Discover(agentUtilsDir, projectID, projectName string) ([]Source, []Result)` — for one
    project. It finds sources in two ways, and returns their union, de-duplicated on
    `(path, loop)`:
    1. `config.List(agentUtilsDir)` and `ResolveStateDir` per entry. An entry with `Err != nil`
       becomes a `Failed` result, NOT a silent skip: a loop whose YAML broke still has state,
       and skipping it would let a tick run against nothing. `ErrNoConfigs` is a normal empty
       case, not an error.
    2. A direct scan of `<agentUtilsDir>/state/*/state.db`, the derived layout. The directory
       name is the loop name. This is what finds the state of a loop whose configuration was
       deleted or renamed.
  - Resolve every path with `filepath.Abs` and then `filepath.EvalSymlinks` (falling back to
    the `Abs` result when `EvalSymlinks` fails). One source recorded under two spellings would
    import every row twice.
  - `func DiscoverAll() ([]Source, []Result, error)` — walks `registry.List()`, loads each
    project descriptor, and calls `Discover`. A project whose directory is gone or whose
    descriptor is missing becomes a `Skipped` result with a reason. Only a failure to read the
    registry itself returns an error.
- [ ] `internal/migrate/migrate.go`:
  - `func Pending(sources []Source) []Source` — the fast path. A source is not pending when
    `MIGRATED.txt` sits beside it. This runs before any lock and before the canonical database
    is opened for migration work, so a machine whose sources are all sealed pays only a few
    `stat` calls per command, forever. A machine-wide lock on every tick and every runner spawn
    would otherwise serialize unrelated loops.
  - `func Run(db *store.DB, dbPath string, sources []Source, opts Options) (Report, error)`
    where `type Options struct { DryRun bool; IsAlive func(pid int, id int64) bool }`.
    1. Return an empty report immediately when `Pending(sources)` is empty.
    2. Take an exclusive `flock` on `home.LockPath("migrate.lock")`, opened `0o600`. Release it
       on every return path. A failure to take the lock is a returned error.
    3. For each pending source: read the recorded state; skip a `sealed` one; refuse a source
       whose `(path, loop)` is already claimed by another project (`DB.ClaimedBy`) with a
       `Failed` result naming both projects; open it with `legacydb`, `Read(loop)`, decide
       `seal := !data.HasLiveRunner(opts.IsAlive)`, and call `DB.ImportLegacy`.
    4. On `DryRun`, do everything except `ImportLegacy` and the marker file. Report the action
       it would take and the row count it would write. State in the doc comment that a dry run
       still opens the canonical database, which applies the schema upgrade — the report cannot
       be produced without it.
    5. After a source is sealed, write `MIGRATED.txt` beside it with mode `0o600`, naming the
       canonical database and saying the file is now an unread backup. A failure to write the
       marker is logged with `slog.Warn` and does not fail the import: the rows are already
       committed, and the `legacy_sources` row is the real record.
  - `func EnsureProject(db *store.DB, dbPath string, sources []Source) error` — the WRITE path
    (`loop tick`, `loop reset`, `internal run-agent`). It runs `Run` and returns an error when
    any result failed. Document why it is strict: a tick against a database missing this loop's
    rows would re-dispatch every open issue and start a second agent in a worktree that already
    holds one.
  - `func Sweep(db *store.DB, dbPath string) (Report, error)` — the READ path
    (`list`, `project status`, `sessions`, `logs`). It discovers everything, never fails for one
    bad source, and returns the report so the caller can print a warning naming it.
- [ ] Tests in `internal/migrate`, each with `t.Setenv("AGENT_UTILS_HOME", t.TempDir())` and
  real legacy files built with the old DDL:
  1. A single source imports every row, readable through the scoped store.
  2. Two projects with the same loop name, repository, and issue numbers do not collide.
  3. A second `Run` is a no-op: row counts unchanged, and the fast path takes no lock (assert
     by removing the lock file and confirming the run still succeeds without recreating it).
  4. A source with a live dispatch (fake `IsAlive`) stays `open` and gets NO `MIGRATED.txt`.
     After the legacy row is finished by hand and `Run` repeats, the canonical row shows the
     outcome and the source is sealed.
  5. A source with a `running` row whose process is dead is sealed on the first pass.
  6. Two loops sharing one `state_dir` both import, and each sees only its own rows.
  7. The same `(path, loop)` claimed by a second project is reported `Failed`, and
     `EnsureProject` returns an error for it while `Sweep` does not.
  8. A loop whose configuration file was deleted is still discovered through the state
     directory scan.
  9. A broken configuration file makes `EnsureProject` fail rather than silently skip.
  10. `DryRun` writes nothing: the canonical row counts and the source's row counts are
      unchanged, and no `MIGRATED.txt` appears.
  11. Every case above leaves the source file present, with its row count unchanged.

**Acceptance:** all eleven tests pass. `go test ./internal/migrate/...` is green.

## Task 9 — Wire the command line to the canonical database

**review: yes** — this is where a project identifier or an environment can be lost.

- [ ] `setup()` in `cmd/agent-utils/main.go` takes the project identifier, the project name, and
  the `.agent-utils` directory. It:
  1. Resolves the state directory as it does today. The lock and the logs stay there.
  2. Calls `home.EnsureDir()` and opens `home.StateDBPath()`.
  3. Builds the source list: `migrate.Discover(...)` PLUS the caller's own resolved
     `<cfg.StateDir>/state.db` when it exists. The explicit entry is required: `--config` takes
     an arbitrary path (`main.go:105-108`), and such a loop's state directory is not always
     inside the directory `Discover` scans.
  4. Calls `migrate.EnsureProject(...)` before the first read.
  5. Sets `deps.Store = db.Project(projectID)` and `deps.ProjectID = projectID`.
  6. Returns a cleanup that closes the `DB`.
- [ ] For the runner, derive the `.agent-utils` directory with `config.DirFromPath(configPath)`.
  When it returns `""`, the source list is the explicit `<cfg.StateDir>/state.db` entry alone.
  Say why in a comment: the runner must not depend on discovery it cannot perform.
- [ ] Add `--project` (required) to `internal run-agent`. Add `ProjectID` to `loopcmd.Deps`.
  `runner.Spawn` gains a `projectID` parameter and passes `--project`. Update the `Deps.Spawn`
  function type, `internal/loopcmd/tick.go:332`, and every test closure that implements it
  (`internal/loopcmd/tick_test.go`, `internal/loopcmd/regression_test.go:62,73`).
- [ ] **Pass the home override across the spawn boundary.** `runner.Spawn` sets
  `cmd.Env = agentEnv()`, an allowlist that does not include `AGENT_UTILS_HOME`
  (`internal/runner/runner.go:49,247-259`). A tick started with the override would spawn a
  runner that resolves a DIFFERENT canonical database and finishes a dispatch id belonging to
  another project. Append `AGENT_UTILS_HOME=<resolved home>` to the RUNNER's environment only.
  Do not widen `agentEnv`'s `keep` list, which also feeds the claude child.
- [ ] Rewrite the comments the change falsifies: `ResolveStateDir`
  (`internal/config/discover.go:238-243`) and `setup()` (`cmd/agent-utils/main.go:594-598`) both
  say the derived directory is what keeps two projects from sharing one database. State is
  separated by project identifier now; `state_dir` holds the tick lock and the logs.
- [ ] Tests: a tick spawns a runner whose argument list carries `--project <uuid>` and whose
  environment carries `AGENT_UTILS_HOME` when it is set.

**Acceptance:** `go build ./...` succeeds, `go test ./...` passes, and
`bin/agent-utils internal run-agent --help` lists `--project`.

## Task 10 — Cross-project reads from one database

**review: no**

- [ ] `loopcmd.Projects()` opens the canonical database once, calls `migrate.Sweep`, then reads
  `LoopStates()` and `RunningDispatches()` once each and joins them onto the configurations it
  lists per project. A source the sweep could not import is reported on that project's line.
- [ ] `loopcmd.Describe` (`project status`), `loopcmd.Sessions`, and `loopcmd.FindSession` take
  the same handle and read `DispatchesForProject(p.Config.ID)` once, instead of opening one file
  per loop. Each of these commands also runs the migration first — the lenient `Sweep`, not the
  strict `EnsureProject`, because a read must not fail on an unrelated broken project. Print a
  one-line warning to stderr naming any failed source.
- [ ] `summariseLoop` changes signature to take the pre-aggregated state instead of a path. It
  is shared by `Projects` and `Describe` (`internal/loopcmd/projects.go:82`,
  `internal/loopcmd/projectstatus.go:32`), so both callers change together.
- [ ] Remove the per-loop `state.db` path building from `internal/loopcmd/projects.go:101` and
  `internal/loopcmd/sessions.go:71,196`.
- [ ] Tests: the existing `loopcmd` tests keep passing; add one that two loops of one project
  are summarised from a single canonical database, and one that a project with no rows renders
  without error.

**Acceptance:** `grep -rn '"state.db"' internal/loopcmd` returns nothing.

## Task 11 — The `migrate` command

**review: no**

- [ ] Add `migrateCommand()` to the `// Top level spans the machine.` group in
  `cmd/agent-utils/main.go:34-46`. It sweeps every registered project, so it is machine-wide.
- [ ] `agent-utils migrate [--dry-run]`. Give `--dry-run` a `Usage` string in the existing style:
  "report what would be imported and write nothing".
- [ ] Print one line per source: project, loop, source path, state, rows. Print a trailer with
  the totals. Exit non-zero when any source failed, naming each failure and its reason.
- [ ] Document in `Usage` that the command is not required: a project is migrated the first time
  a command touches it.
- [ ] Tests: a dry run against a seeded temporary home writes nothing; a real run reports
  `sealed` on the second invocation; a failed source produces a non-zero exit.

**Acceptance:** running it twice on the same machine reports `sealed` the second time and
changes nothing.

## Task 12 — Documentation and version

**review: no**

Every target below states something the change falsifies. Fix all of them.

- [ ] `README.md:156` — state lives in `~/.agent-utils/state.db`, keyed by project; logs and the
  tick lock stay under `state_dir`.
- [ ] `README.md:64-70` — add a `migrate` row to the global command table.
- [ ] `README.md:222-225` — the worked example prints `agent-utils v0.2.0`; bump it with
  `VERSION`, as commit `a9b1481` did.
- [ ] `README.md` — a short "Migration" note: it happens automatically, the old files are kept,
  and `agent-utils migrate` prints the report.
- [ ] `docs/configuration.md:53-70` — document `AGENT_UTILS_HOME`, and state the difference from
  `AGENT_UTILS_DIR` in one sentence: `AGENT_UTILS_DIR` names a PROJECT's `.agent-utils`
  directory, `AGENT_UTILS_HOME` names the machine-wide one. Correct the paragraph that explains
  what the home directory holds.
- [ ] `docs/configuration.md:200-232` — the `state_dir` section and the file table:
  `{state_dir}/state.db` no longer exists; the lock and the logs stay. Correct
  "every project's real configuration and state stay in its own directory", the `0600` database
  sentence, and the "give each loop its own `state_dir`" guidance.
- [ ] `docs/configuration.md:146` — two loops sharing a `state_dir` must still have different
  names, and now for a second reason: the loop name is what separates their imported rows.
- [ ] `examples/planning.yaml:6-8` and `examples/execution.yaml:6-8` — the comments say
  `state_dir` keeps every project's database separate.
- [ ] `VERSION` → `v0.3.0`. The state location changes, so it is not a patch.

**Acceptance:** `go test ./internal/config/...` passes (the docs test), and
`grep -rn 'state.db' README.md docs/ examples/` describes the canonical file only.

## Task 13 — Whole-branch verification

**review: yes**

- [ ] `make check` passes.
- [ ] `make test/race` passes. Two processes write one file now, and Task 3 has a concurrency
  test.
- [ ] Manual end-to-end check with a scratch home directory:
  1. Build `master`'s binary. Point `AGENT_UTILS_HOME` at a temporary directory. Create two
     projects with one loop each and seed their per-loop databases.
  2. Build the branch binary. Run `agent-utils list`, `agent-utils migrate --dry-run`, and
     `agent-utils migrate`.
  3. Confirm: every row appears under the right project, the legacy files still exist with the
     same row counts, `MIGRATED.txt` is beside each sealed source, and a second `migrate`
     reports `sealed`.
- [ ] Paste the output into the pull request.

**Acceptance:** the three steps behave as described.

---

## Review findings folded in

Three fresh-context reviewers (security, quality, standards) reviewed the first draft of this
plan. The high-severity findings they raised, and where each is handled:

| Finding | Handled in |
|---|---|
| `AGENT_UTILS_HOME` does not survive the spawn boundary; the runner would open a different database | Task 9 |
| The schema rebuild ran with no cross-process serialization | Task 3 |
| Discovery missed a loop run with `--config <path outside configs/>` | Tasks 8, 9 |
| `legacy_sources` keyed on path alone; two loops or two projects may share a `state_dir` | Tasks 3, 7, 8 |
| `migrate` had no transactional surface on `store.DB` | Task 7 |
| `tick.go` had no project identifier to pass to `Spawn` | Task 9 |
| `--dry-run` consumed a capability no task created | Tasks 8, 11 |
| `LoopStates` could not be grouped as specified; `ticks` has no `repo` | Task 4 |
| The liveness call-site list was wrong in both directions | Task 5 |
| `.golangci.yml` names `store.Store.Close`, which the split removes | Task 4 |
| Doc-sync covered about half the places that state the old layout | Task 12 |
| `Store.Close` on a scoped view would close the shared handle | Task 4 |
| The seal condition would pin a source open forever after a crashed runner | Tasks 6, 8 |
| A non-atomic legacy read could seal a source holding a stale `running` row | Task 6 |
| The issue refresh was a whole-row last-write-wins | Task 7 |
| The reaper could permanently discard an imported dispatch's real outcome | Task 7 |
| `migrate()` and `upgradeKeys` had no defined order | Task 3 |
| The machine-wide lock would be taken on every command forever | Task 8 |
| `busy_timeout` was raised in the spec and by no task | Task 3 |

Accepted limitations, recorded rather than fixed:

- When a loop's `state_dir` IS the home directory, a still-running old-binary runner loses its
  issue-state write during the upgrade (Task 3). Keeping a compatible unique index would defeat
  project keying, and the tick's reaper recovers the issue on the next pass.

## Commit-review findings, and what was done

Three fresh-context reviewers (security, quality, standards) read the whole diff. The findings
that changed code:

| Finding | Fix |
|---|---|
| The canonical-source test compared a resolved path against an unresolved one, so a home directory reached through a symlink silently sealed a source with zero rows imported | One resolver, `home.Resolve`, used by both `store.Open` and `migrate`. Regression test proves the old code fails |
| `MIGRATED.txt` was written beside the canonical database, telling the operator it was a deletable backup | A canonical source never gets a marker, and the note no longer advises deleting anything |
| The seal decision read the source and then checked liveness, so a runner that finished in between was sealed away behind a stale `running` row | The source is read again after the liveness check, before it is sealed. Regression test proves the old code fails |
| A refresh copied a source row still marked `running`, resurrecting a dispatch this database had already retired | A running source row is skipped; there is no outcome in it to carry |
| A refresh only updated, so a row an old-binary tick created after the first import was dropped when the source sealed | A refresh inserts a row that has no counterpart here |
| `EnsureProject`'s doc claimed discovery problems were fatal; they are skips, and were dropped in silence | The comment says what the code does, and a skip is logged rather than dropped. The test that asserted the old contract is replaced by one that covers a real failure |
| `logs` failed outright when an unrelated loop's old file was broken | `setup` takes a migration policy: a command that writes fails, a command that only reads warns |
| The snapshot dropped the repository dimension the per-loop reads had, so a loop whose repo changed reported both repositories added together | `LoopState.CostByRepo`, and live/orphan counts keyed by repository |
| A dry run reported the whole file's row count for a source that would only be refreshed | It reports the rows a refresh can write |
| An unreadable state directory was passed over in silence | It is reported as a skip |
| The migrate renderer and the branch's only `cmd/` test sat outside `internal/loopcmd`, where every other renderer lives | Moved to `internal/loopcmd/migratereport.go` |
| Three copies of a "duplicate loop name" comment still argued from the old one-database-per-loop world | Rewritten to name the failure that exists now |
| `legacydb` set `journal_mode=WAL`, which rewrites the header of a file the package promises never to write | The pragma is gone; only `busy_timeout` remains |
| `ErrSourceClaimed` left the write path wedged with no stated remedy | The error names the claiming project and what to change |

## Deviations from this plan, decided during execution

- **A loop configuration that does not load is reported and skipped, not fatal** (Task 8). The
  plan made it fatal on the write path. That would stop a tick of loop A because loop B's YAML
  is broken, which is a new failure this change has no reason to introduce. State is per loop,
  so a broken sibling hides nothing loop A needs, and `setup()` always adds loop A's own state
  directory through `migrate.SourceFor`. A source that exists and cannot be READ is still fatal
  on the write path, which is the case the rule was written for.
- **`PRAGMA user_version` was dropped** (Task 3). The upgrade decides by column presence, and a
  second version mechanism that nothing reads invites a later reader to trust it.
- **`store.Open` retries while the database is busy** (Task 3, not in the plan). A connection
  applies `journal_mode=WAL` as it opens, and on a database another process is writing that
  pragma can return SQLITE_BUSY without waiting out the busy handler. Per-loop files never
  opened that window; one shared file does.
- **The schema DDL is split into tables and indexes** (Task 3, not in the plan). The new indexes
  name columns an older database only gains during the upgrade, so creating them first fails.
