# Canonical State Database — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep all durable loop state in one SQLite database at `~/.agent-utils/state.db`, keyed
by project UUID. Import every existing per-loop database automatically, and lose nothing.

**Architecture:** `store.DB` is the one canonical handle. `DB.Project(id)` returns a `*store.Store`
scoped to one project; that scoped type keeps today's method set, so the loop code barely
changes. Global queries live on `DB`. A new `internal/migrate` package finds legacy
`state.db` files, copies their rows into the canonical database under a transaction, and records
each source it has imported. A source whose runner processes still run stays open and is read
again until it is idle.

**Tech stack:** Go 1.25.9, `modernc.org/sqlite` (no CGO), `urfave/cli` v3, `gopkg.in/yaml.v3`.

**Spec:** `docs/superpowers/specs/2026-08-18-canonical-state-db.md`

## Pipeline State

| Field   | Value                                                              |
|---------|--------------------------------------------------------------------|
| stage   | 2 (plan review)                                                     |
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
  `errcheck`, `errorlint`, `staticcheck`, `ineffassign`, and `unused` are enabled
  (`.golangci.yml`).
- Wrap every returned error with `fmt.Errorf("<what failed>: %w", err)`. Compare sentinels with
  `errors.Is`.
- Every exported symbol has a doc comment. A comment says **why**, not what. This repository's
  comments record the failure a line prevents; match that voice.
- Tests are table-free, plain `func TestX(t *testing.T)`, in the package under test.
- `engine.Decide` stays pure. No task in this plan touches it.
- Go must not write to GitHub. No task in this plan makes a GitHub call.
- Never delete or rewrite a legacy `state.db`. It is the only copy of the state until the import
  succeeds.

## Verified external API (do not re-derive)

Each signature below was read from this repository or from the Go standard library, not recalled.

```go
// internal/store/store.go:88-104 — the DSN form the driver accepts. Pragmas MUST be in the DSN;
// busy_timeout and foreign_keys are per connection.
dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
db, err := sql.Open("sqlite", dsn)   // driver name is "sqlite" (modernc.org/sqlite)
db.SetMaxOpenConns(1)

// internal/registry/registry.go:268-282 — the file-lock form already used in the home directory.
lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
err = syscall.Flock(int(lf.Fd()), syscall.LOCK_EX)   // blocking; LOCK_UN to release

// internal/proc/proc.go — liveness matches the runner's own command line.
const DispatchFlag = "--dispatch"
func IsAlive(pid int, dispatchID int64) bool

// internal/runner/runner.go:21-24, 33-36
func RunnerLogPath(stateDir, loop string, dispatchID int64) string
func Spawn(selfPath string, dispatchID int64, configPath, runnerLog string) (int, error)

// internal/project/project.go:29-35 — the project key. ID is a UUID, minted once.
type Config struct { Name string; ID string }
func Load(agentUtilsDir string) (*Config, error)

// internal/registry/registry.go
func List() ([]Project, error)     // Project{ID, Name, Root, AgentUtilsDir, ...}
func Path() (string, error)        // $HOME/.agent-utils/registry.json

// internal/config/discover.go:244
func (c *Config) ResolveStateDir(configPath string) (string, error)
func List(agentUtilsDir string) ([]Entry, error)   // Entry{Name, Path, Repo, Err}

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
- `PRAGMA user_version` stores an integer in the file header. It needs no table.
- A partial unique index (`... WHERE legacy_source <> ''`) is supported.

## File structure

| Path | Responsibility |
|---|---|
| `internal/home/home.go` | The one place that resolves `~/.agent-utils`. **New.** |
| `internal/store/store.go` | Schema, schema upgrade, `DB`, scoped `Store`, global queries. |
| `internal/store/types.go` | `ProjectID` and `legacy` fields; `Dispatch.RunnerID()`. |
| `internal/store/legacy/legacy.go` | Reads a legacy database. Never writes it. **New.** |
| `internal/migrate/migrate.go` | Discovery, locking, per-source import, report. **New.** |
| `internal/registry/registry.go` | Uses `internal/home`. |
| `internal/loopcmd/*.go` | Scoped store; cross-project reads from one database. |
| `cmd/agent-utils/main.go` | Opens the canonical database; `--project` on the runner; `migrate`. |
| `README.md`, `docs/configuration.md` | Document the new location and the migration. |

---

## Task 1 — `internal/home`, the one home-directory resolver

**review: no**

- [ ] Create `internal/home/home.go` with:
  - `func Dir() (string, error)` — returns `$AGENT_UTILS_HOME` when it is set and not blank,
    and `<user home>/.agent-utils` otherwise. Wrap a `os.UserHomeDir` failure.
  - `func StateDBPath() (string, error)` — `<Dir()>/state.db`.
  - `func LockPath(name string) (string, error)` — `<Dir()>/<name>`.
  - `func EnsureDir() (string, error)` — creates `Dir()` with mode `0o700` and returns it. The
    directory holds session identifiers, so it must not be group readable.
- [ ] Document why the override exists: a test must be able to move the home directory without
  changing `$HOME`, which git and the agent environment still need.
- [ ] Tests in `internal/home/home_test.go`: the override wins; the default ends in
  `.agent-utils`; `EnsureDir` creates the directory with mode `0700`.

**Acceptance:** `go test ./internal/home/...` passes. `gofmt`, `go vet`, and `golangci-lint`
are clean.

## Task 2 — Registry uses `internal/home`

**review: no**

- [ ] Change `registry.Path()` to call `home.Dir()`. Keep the exported signature.
- [ ] Keep the existing behavior when `AGENT_UTILS_HOME` is not set. `registry_test.go` sets
  `HOME` and must keep passing unchanged.

**Acceptance:** `go test ./internal/registry/...` passes with no test edit.

## Task 3 — Schema version 2: project keying and the legacy columns

**review: yes** — this task changes a schema and can lose rows.

- [ ] In `internal/store/store.go`, rewrite the `schema` constant to the new shape:
  - `issues`: add `project_id TEXT NOT NULL` as the first column. Primary key becomes
    `(project_id, loop, repo, number)`.
  - `pr_links`: the same. Primary key `(project_id, loop, repo, number)`.
  - `cooldowns`: add `project_id`. Primary key `(project_id, loop)`.
  - `dispatches`: add `project_id TEXT NOT NULL DEFAULT ''`,
    `legacy_source TEXT NOT NULL DEFAULT ''`, `legacy_id INTEGER NOT NULL DEFAULT 0`. The
    primary key stays `id`.
  - `ticks`: add `project_id TEXT NOT NULL DEFAULT ''`. The primary key stays `id`.
  - Indexes: `dispatches_running_project` on `(project_id, loop, repo, status)`;
    `ticks_loop` on `(project_id, loop)`; a unique index `dispatches_legacy` on
    `(legacy_source, legacy_id)` `WHERE legacy_source <> ''`.
  - New table `legacy_sources(path TEXT PRIMARY KEY, project_id TEXT NOT NULL, loop TEXT NOT
    NULL, repo TEXT NOT NULL, state TEXT NOT NULL, first_imported_at TIMESTAMP NOT NULL,
    last_imported_at TIMESTAMP NOT NULL)`.
- [ ] Extend the existing column-adding migration (`addedColumns`) with the five new columns on
  `dispatches` and `ticks`. That path already handles a file created by an older binary.
- [ ] Add `upgradeKeys(db *sql.DB) error`, which runs after the DDL:
  - Return early when `issues` already has a `project_id` column.
  - Otherwise rebuild `issues`, `pr_links`, and `cooldowns` inside ONE transaction: create the
    new table under a temporary name, `INSERT ... SELECT` with `''` for `project_id`, drop the
    old table, rename the new one.
  - Re-run the DDL afterwards so the dropped indexes come back.
  - Set `PRAGMA user_version = 2`.
  - A row copied with an empty `project_id` is invisible to every scoped query, because a
    project identifier is a UUID and is never empty. Task 6 stamps such rows when the source
    file IS the canonical file. Say this in a comment.
- [ ] Drop the old `dispatches_running` index if it exists.
- [ ] Tests in `internal/store/store_test.go`:
  - A database created by the current code has `project_id` on all five tables.
  - A database built with the OLD DDL (write the old `CREATE TABLE` statements in the test),
    filled with one row per table, is upgraded on open: every row survives, and `project_id`
    is `''`.
  - The upgrade is idempotent: opening twice changes nothing.

**Acceptance:** the three tests above pass. No existing store test is deleted.

## Task 4 — Project scope on the store API

**review: yes** — a wrong scope reads or writes another project's rows.

- [ ] Split the type in `internal/store/store.go`:
  - `type DB struct { db *sql.DB }`, returned by `Open(path string) (*DB, error)`.
  - `func (d *DB) Project(projectID string) *Store` returns `&Store{db: d.db, projectID:
    projectID}`.
  - `func (d *DB) Close() error`.
  - Keep `type Store struct { db *sql.DB; projectID string }` with **every current method
    signature unchanged**.
- [ ] Add `project_id = ?` to every `WHERE` clause and every `INSERT` in the scoped methods.
  Every `ON CONFLICT` target gains `project_id`.
- [ ] `Store.DispatchesBySession` and `Store.GetDispatch` also filter by `project_id`. A
  session identifier is unique, but a scoped read must not be able to return another project's
  row.
- [ ] Add `ProjectID` to `IssueState`, `Dispatch`, and `PRLink` in `internal/store/types.go`,
  and populate it on every scan.
- [ ] Add the global reads on `DB`:
  - `func (d *DB) RunningDispatches() ([]Dispatch, error)` — every project.
  - `func (d *DB) DispatchesForProject(projectID string) ([]Dispatch, error)` — newest first.
  - `func (d *DB) LoopStates() ([]LoopState, error)` — one row per
    `(project_id, loop, repo)`, with the tick count, the last tick time, and the total cost.
    Define `LoopState` in `types.go`.
- [ ] Tests: two projects write the same loop name, the same repository, and the same issue
  number; each scoped read returns only its own rows; a delete in one project leaves the other
  untouched; `RunningDispatches` returns both; `LoopStates` aggregates per project.

**Acceptance:** the isolation tests above pass. Every existing store test passes after being
re-pointed through `Open(...).Project("p1")`.

## Task 5 — `Dispatch.RunnerID()` and the liveness call sites

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

- [ ] Add `LegacyID int64` and `LegacySource string` to `Dispatch`, and scan them.
- [ ] Replace `d.ID` with `d.RunnerID()` at every liveness call and every runner-log path:
  `internal/loopcmd/tick.go` (the reaper and `Reset`), `internal/loopcmd/sessions.go`,
  `internal/loopcmd/projects.go`, `internal/loopcmd/logs.go`, `cmd/agent-utils/main.go`.
  Search for `IsAlive(` and `RunnerLogPath(` and change every hit.
- [ ] Test: a dispatch with `LegacyID` set reports the legacy identifier; one without reports
  `ID`.

**Acceptance:** `grep -rn 'IsAlive(d.PID, d.ID)'` returns nothing. Tests pass.

## Task 6 — The legacy reader

**review: yes** — it reads a file another process may write at the same time.

- [ ] Create `internal/store/legacy/legacy.go`.
- [ ] `func Open(path string) (*DB, error)` opens the file with the same DSN pragmas as
  `store.Open`, **and applies no schema and no migration**. Say why in a comment: an old runner
  process is still writing this file with the old code, and rewriting its schema under it would
  break it.
- [ ] Read helpers that return plain structs: `Issues()`, `Dispatches()`, `PRLinks()`,
  `Ticks()`, `Cooldowns()`, and `HasRunningDispatch() (bool, error)`.
- [ ] Every read is column-defensive. Call `PRAGMA table_info(<table>)` first and select only
  the columns that exist. A file written by an early binary has no `title`, no `pr_number`, and
  no `behind_by`. A missing column takes its zero value.
- [ ] Tests: build a database with the old DDL, including one missing-column variant, and read
  it back. `HasRunningDispatch` is true only when a row has status `running`.

**Acceptance:** the reader returns every row from both an old-shape and a current-shape legacy
file. It never issues a write statement — assert this by opening the file read-only in the test
(`&mode=ro`) and running the same reads.

## Task 7 — The importer

**review: yes** — this is the task that can lose a user's state.

- [ ] Create `internal/migrate/migrate.go`.
- [ ] Types:

```go
// Source is one legacy database file and the loop it belongs to.
type Source struct { Path, ProjectID, Loop, Repo string }

// Result is what happened to one source.
type Result struct {
    Source  Source
    State   string // "imported", "refreshed", "sealed", "skipped", "failed"
    Rows    int
    Err     error
}

// Report is the outcome of a whole run.
type Report struct { Results []Result }
```

- [ ] `func Import(db *store.DB, dbPath string, sources []Source) (Report, error)`:
  - Take an exclusive `flock` on `<home>/migrate.lock` for the whole call. Release it on every
    path.
  - For each source, in its own transaction on the canonical database:
    1. Skip a source already recorded `sealed`.
    2. **First import** (no `legacy_sources` row): copy `issues`, `pr_links`, `cooldowns`,
       `ticks`, and `dispatches`, stamping `project_id`. Stamp each dispatch with
       `legacy_source` (the absolute source path) and `legacy_id` (its old `id`).
    3. **Refresh** (`legacy_sources` row is `open`): update only `dispatches` and `issues`.
       - A dispatch matches on `(legacy_source, legacy_id)`. Copy `status`, `exit_code`,
         `cost_usd`, `duration_ms`, `api_error`, `finished_at`, `pid`, `pid_start_at`, and
         `session_id`, and only into a row whose current status is `running`.
       - An issue matches on its key, and is updated only when the source row's `updated_at`
         is strictly newer than the canonical row's `updated_at`.
       - Never touch `ticks`, `pr_links`, or `cooldowns` on a refresh. A tick after the
         upgrade writes to the canonical database, so the source cannot have newer rows there.
    4. Write the `legacy_sources` row. Its state is `open` when the source still has a running
       dispatch, and `sealed` otherwise.
    5. Commit. Roll back on any error and record the source as `failed` with the error.
  - **Special case:** when the source path equals the canonical database path, copy nothing.
    Instead stamp the rows the schema upgrade left with `project_id = ''` with this source's
    project identifier, then seal it. Guard the update by `loop = ?` so two loops that share
    that directory each claim their own rows.
- [ ] After a source is sealed, write `MIGRATED.txt` next to it. It names the canonical database
  and says the file is now a backup that nothing reads. Never delete the source.
- [ ] `func Discover(agentUtilsDir, projectID string) ([]Source, error)` lists one project's
  loop configurations, resolves each state directory, and returns the sources whose `state.db`
  exists.
- [ ] `func DiscoverAll() ([]Source, error)` walks `registry.List()`, loads each project
  descriptor, and calls `Discover`. A project whose directory is gone, or whose descriptor is
  missing, is skipped and reported, not an error.
- [ ] `func EnsureProject(db *store.DB, dbPath, agentUtilsDir, projectID string) error` — the
  targeted path. It discovers, imports, and returns an error when any source failed. Document
  why it is strict: a tick against an empty database would re-dispatch every issue and start a
  second agent in a worktree that already holds one.
- [ ] `func Sweep(db *store.DB, dbPath string) (Report, error)` — the global path. It never
  returns an error for one bad source. It records it in the report.
- [ ] Tests in `internal/migrate/migrate_test.go`, each building real legacy files with the old
  DDL under a temporary `AGENT_UTILS_HOME`:
  1. A single source imports every row, and the rows read back through the scoped store.
  2. Two projects with the same loop name, repository, and issue numbers do not collide.
  3. A second `Import` of a sealed source is a no-op. Row counts do not change.
  4. A source with a running dispatch is left `open`. After the legacy row is finished by hand
     and `Import` runs again, the canonical row shows the outcome and the source is `sealed`.
  5. A refresh does NOT overwrite an issue row the canonical database updated more recently.
  6. Dispatch identifiers are renumbered, and `RunnerID()` still returns the legacy identifier.
  7. `MIGRATED.txt` appears only next to a sealed source.
  8. The source file still exists, and its row count is unchanged, after every case above.

**Acceptance:** all eight tests pass. `go test ./internal/migrate/...` is green.

## Task 8 — Wire the command line to the canonical database

**review: yes** — this is where a project identifier can be lost.

- [ ] `setup()` in `cmd/agent-utils/main.go` takes the project identifier and the
  `.agent-utils` directory as parameters. It:
  1. Resolves the state directory as it does today. The lock and the logs stay there.
  2. Opens the canonical database from `home.StateDBPath()`, after `home.EnsureDir()`.
  3. Calls `migrate.EnsureProject(...)` before the first read.
  4. Sets `deps.Store = db.Project(projectID)`.
  5. Returns a cleanup that closes the `DB`.
- [ ] Add `--project` (required) to `internal run-agent`. `runner.Spawn` gains a `projectID`
  parameter and passes it. `Deps.Spawn`'s function type changes with it; update
  `internal/loopcmd/tick.go` and its tests.
- [ ] Keep the `state_dir` doc comment on `ResolveStateDir` accurate: rewrite the paragraph that
  says a shared `state_dir` makes two projects share one database. State is separated by project
  identifier now; `state_dir` holds the lock and the logs.
- [ ] Test: a tick spawns a runner whose argument list carries `--project <uuid>`.

**Acceptance:** `go build ./...` succeeds, `go test ./...` passes, and
`bin/agent-utils internal run-agent --help` lists `--project`.

## Task 9 — Cross-project reads from one database

**review: no**

- [ ] `loopcmd.Projects()` opens the canonical database once. It calls `migrate.Sweep` first,
  then reads `LoopStates()` and `RunningDispatches()` once each, and joins them onto the
  configurations it lists per project. A source the sweep could not import is reported on the
  project's line.
- [ ] `loopcmd.Sessions(p, loopFilter)` and `loopcmd.FindSession(p, id)` read
  `DispatchesForProject(p.Config.ID)` once, instead of opening one file per loop.
- [ ] `loopcmd.Describe` (project status) uses the same single handle.
- [ ] Remove the now-dead per-loop `state.db` path building from `projects.go`, `sessions.go`,
  and `projectstatus.go`.
- [ ] Tests: the existing `loopcmd` tests keep passing; add one that two loops of one project
  are summarised from a single canonical database.

**Acceptance:** `grep -rn '"state.db"' internal/loopcmd` returns nothing.

## Task 10 — The `migrate` command

**review: no**

- [ ] Add a top-level `agent-utils migrate` command with `--dry-run`. It sweeps every registered
  project and prints one line per source: the project, the loop, the source path, the state, and
  the row count. `--dry-run` reports what it would do and writes nothing.
- [ ] The command exits non-zero when a source failed, and names each failure.
- [ ] Document it in `--help` as "not required; a project is migrated the first time a command
  touches it".

**Acceptance:** running it twice on the same machine reports `sealed` the second time and
changes nothing.

## Task 11 — Documentation and version

**review: no**

- [ ] `README.md`: replace the "state is per-project" paragraph. Say that state lives in
  `~/.agent-utils/state.db`, that it is keyed by project, and that logs and the tick lock stay
  under `state_dir`. Add a short "Migration" note: it happens automatically, the old files are
  kept, and `agent-utils migrate` shows the report.
- [ ] `docs/configuration.md`: update the `state_dir` section and the file table
  (`{state_dir}/state.db` no longer exists). Keep the example that points `state_dir` at a
  shared directory, and correct what it implies.
- [ ] `VERSION`: `v0.3.0`. The state location changes, so it is not a patch.

**Acceptance:** `grep -rn 'state.db' README.md docs/` describes the canonical file only.

## Task 12 — Whole-branch verification

**review: yes**

- [ ] `make check` passes.
- [ ] `make test/race` passes. Two processes write one file now, so the race detector is worth
  the extra minute.
- [ ] Manual end-to-end check with a scratch home directory:
  1. Build the current `master` binary. Point `AGENT_UTILS_HOME` at a temporary directory and
     create two projects with one loop each. Write rows with `loop tick` against a fake
     repository, or seed the databases directly.
  2. Build the branch binary. Run `agent-utils list`, `agent-utils migrate --dry-run`, and
     `agent-utils migrate`.
  3. Confirm: every row appears under the right project, the legacy files still exist, and a
     second `migrate` reports `sealed`.

**Acceptance:** the three steps above behave as described, and the output is pasted into the PR.
