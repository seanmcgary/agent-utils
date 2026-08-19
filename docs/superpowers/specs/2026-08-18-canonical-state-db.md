# Canonical State Database — Design

**Status:** design for implementation
**Date:** 2026-08-18
**Author:** agent-utils maintainer

## 1. Goal

Move all durable loop state into one SQLite database at `~/.agent-utils/state.db`. Key every
row by the project that owns it. Import the existing per-loop databases automatically.

Two results are wanted:

1. **Cross-project coordination.** One database can answer a machine-wide question in one
   query. It can also hold a machine-wide policy later, such as a global limit on concurrent
   dispatches. Separate files cannot do either.
2. **One canonical location.** State lives in one place. A human, a backup script, and a
   future command all look at the same file.

## 2. What exists today

The tool keeps one database per loop.

- `store.Open` is called with `<StateDir>/state.db` only (`cmd/agent-utils/main.go:619`).
- `StateDir` resolves to `<project>/.agent-utils/state/<loop>`
  (`internal/config/discover.go:244`). An explicit `state_dir` in the loop configuration
  overrides it.
- Every table already carries a `loop TEXT` column (`internal/store/store.go:18-79`). The
  schema is already multi-tenant inside one file. It has no project dimension.
- `~/.agent-utils/registry.json` lists the projects (`internal/registry/registry.go:22`). It is
  an index, not a source of truth.
- Cross-project commands open every project database in turn
  (`internal/loopcmd/projects.go:101`, `internal/loopcmd/sessions.go:71`,
  `internal/loopcmd/sessions.go:196`).

## 3. Premise check

Each finding below comes from the code, with the evidence.

### 3.1 Entry path

Every command that touches state calls `setup()` (`cmd/agent-utils/main.go:585`). `setup()`
resolves the state directory and opens the database. Two callers exist:

- The command line, which always resolves a project first through `openProject()`
  (`cmd/agent-utils/main.go:89`). The project descriptor holds a stable UUID
  (`internal/project/project.go:32`).
- The detached runner, `internal run-agent` (`cmd/agent-utils/main.go:568`). It receives
  `--config` and `--dispatch` only. It never resolves a project.

**Consequence:** the runner needs the project identifier passed to it. A new `--project` flag
on `internal run-agent` supplies it.

### 3.2 Blast radius

The change touches this repository only. Inside it:

- `internal/store` — schema, every query, the open path.
- `internal/loopcmd` — `tick.go`, `status.go`, `logs.go`, `projects.go`, `sessions.go`,
  `projectstatus.go`.
- `cmd/agent-utils/main.go` — `setup()`, the runner flags, a new `migrate` command.
- `internal/runner` — `Spawn` must pass the project identifier.
- `internal/registry` — shares the home directory helper.
- `README.md` and `docs/configuration.md` — both document the old location.

### 3.3 The dispatch identifier is an external key

This is the most important finding. The dispatch identifier is not private to the database.

- The runner carries it on its command line: `--dispatch <id>` (`internal/runner/runner.go:35`).
- Liveness matches on that token (`internal/proc/proc.go`, `matchesDispatch`).
- The runner log file name embeds it: `runner-<id>.log` (`internal/runner/runner.go:21-24`).

Each legacy database numbers dispatches from 1. Two projects therefore hold the same
identifiers. A merge must give every imported row a new identifier. A renumbered row loses the
match with its live runner process.

**Consequence:** an imported dispatch keeps its old identifier in a `legacy_id` column.
Liveness uses `legacy_id` when it is set. The old identifier also keeps the runner log path
correct.

### 3.4 A live runner writes to the database it was started with

A runner is a detached copy of the binary that spawned it. An upgrade does not change a running
process. A runner started by the old binary therefore writes its outcome to the legacy file,
after the migration has copied that row.

**Consequence:** the migration cannot assume a source file is final. A source that holds
running dispatches stays **open**. An open source is read again on each later command, until
none of its dispatches run. Then it is **sealed**.

**Consequence:** the migration must never refuse to run because a dispatch is alive. A tick
runs every few minutes while agents run for hours. A migration that waits for an idle moment
would block the normal state of the system.

### 3.5 Prior art to reuse

- `registry.lockRegistry` (`internal/registry/registry.go:268`) already locks a home-directory
  file with `flock`. The migration uses the same method.
- `project.Config.ID` already gives a stable key. No new identifier is needed.
- `store.migrate` (`internal/store/store.go:125`) already adds columns to an existing file. The
  new schema work reuses it.

### 3.6 Contradiction scan

Nothing in the request contradicts the code. One comment does contradict the new design, and
must be rewritten rather than deleted: `ResolveStateDir`'s comment says a shared `state_dir`
would make two projects share one database (`internal/config/discover.go:240-243`). After this
change the database is always shared and the project identifier is what separates the rows.
`state_dir` keeps the logs and the lock file.

### 3.7 Profile and class

- **Profile:** backend. The change is schema, data, and process coordination.
- **Class:** Large. It changes a schema, adds a data migration, and touches concurrency and
  data integrity.

## 4. Design

### 4.1 Location

The canonical database is `<home>/state.db`, where `<home>` is `$AGENT_UTILS_HOME` when set,
and `~/.agent-utils` otherwise.

A new package `internal/home` returns that directory. `internal/registry` uses it too, so the
registry and the database always agree. The environment variable makes an end-to-end test
hermetic. Without it, a test of the migration would write to the developer's real home
directory.

### 4.2 Schema

Every state table gains `project_id TEXT NOT NULL`. It becomes the first column of each key.

| Table | New primary key |
|---|---|
| `issues` | `(project_id, loop, repo, number)` |
| `pr_links` | `(project_id, loop, repo, number)` |
| `cooldowns` | `(project_id, loop)` |
| `dispatches` | `id` (unchanged), plus an index on `(project_id, loop, repo, status)` |
| `ticks` | `id` (unchanged), plus an index on `(project_id, loop)` |

`dispatches` gains three more columns:

- `legacy_id INTEGER NOT NULL DEFAULT 0` — the identifier the row had in its source file.
- `legacy_source TEXT NOT NULL DEFAULT ''` — the absolute path of the source file.
- A unique index on `(legacy_source, legacy_id)` where both are set. It makes the import
  idempotent.

SQLite cannot add a column to a primary key. The tables that change key are therefore rebuilt:
create the new table, copy the rows, drop the old table, rename. The rebuild runs inside one
transaction, and only on a file that still carries the old shape.

### 4.3 Store API

`store.Store` keeps its current method set and its current signatures. It gains a project
scope.

```go
type DB struct { ... }                       // the canonical database handle
func Open(path string) (*DB, error)
func (d *DB) Project(projectID string) *Store  // a scoped view; methods take (loop, repo, ...)
func (d *DB) Close() error
```

Global reads live on `DB`:

```go
func (d *DB) RunningDispatches() ([]Dispatch, error)      // every project
func (d *DB) LoopStates() ([]LoopState, error)            // ticks, last tick and cost per loop
func (d *DB) DispatchesForProject(projectID string) ([]Dispatch, error)
```

This shape keeps the diff in `tick.go` and `status.go` small. `Deps.Store` stays a
`*store.Store`; the caller binds the project once, in `setup()`.

`Dispatch`, `IssueState`, and `PRLink` gain a `ProjectID` field.

### 4.4 Liveness of an imported dispatch

`proc.IsAlive(pid, id)` matches `--dispatch <id>` on the process command line. An imported row
therefore reports the identifier its runner actually carries:

```go
func (d Dispatch) RunnerID() int64 {   // the identifier the live process carries
    if d.LegacyID != 0 { return d.LegacyID }
    return d.ID
}
```

Every liveness call and every runner log path uses `RunnerID()`.

### 4.5 Migration

**Discovery.** A source is `<state_dir>/state.db` for one loop of one project. The migration
finds sources in two ways:

- *Targeted*, for the project a command acts on. It lists that project's loop configurations
  and resolves each state directory.
- *Sweep*, for every project in the registry. Global commands and `agent-utils migrate` use it.

**Record.** A new table records each source:

```sql
CREATE TABLE legacy_sources (
  path              TEXT NOT NULL,      -- absolute, symlink-resolved source path
  project_id        TEXT NOT NULL,
  loop              TEXT NOT NULL,
  repo              TEXT NOT NULL DEFAULT '',
  state             TEXT NOT NULL,      -- 'open' or 'sealed'
  first_imported_at TIMESTAMP NOT NULL,
  last_imported_at  TIMESTAMP NOT NULL,
  PRIMARY KEY (path, project_id, loop)
);
```

The key is a triple, not a path. Two loops may share one `state_dir`
(`docs/configuration.md:146`), so one file can hold two loops. The import therefore reads and
copies the rows of ONE loop, and the loop name is what separates them.

**Import.** For one source, inside one transaction on the canonical database:

1. Copy `issues`, `pr_links`, `ticks`, and `cooldowns`, stamped with the project identifier.
2. Copy `dispatches`, stamped with the project identifier, `legacy_source`, and `legacy_id`.
   The new `id` is assigned by SQLite.
3. Set the source state. It is `open` when the source still holds a `running` dispatch whose
   process is alive. It is `sealed` otherwise. Both facts come from ONE read transaction on the
   source, so a runner that finishes mid-import cannot leave a stale `running` row behind a
   seal.
4. Write `MIGRATED.txt` next to a sealed source. The note says where the state went and that
   the file is now a backup.

**Re-import.** A source in state `open` is imported again on each later command that reaches
it. Only two tables can change in a source after the first import, because only two are written
by a runner (`internal/runner/runner.go:212-236`, `internal/loopcmd/tick.go:413`):

- **`dispatches`.** A row matches on `(legacy_source, legacy_id)`. The refresh copies the
  outcome columns, and only into a row this database still marks `running`.
- **`issues`.** A row matches on its key. The refresh applies only when the source row's
  `updated_at` is newer than the canonical row's `updated_at`. This is what stops a stale
  source from overwriting a fresh write by the new binary.

`ticks`, `pr_links`, and `cooldowns` are written by a tick only. A tick after the upgrade
writes to the canonical database, so these three tables are copied on the first import and are
never read again.

A source is sealed when no dispatch row in it is `running` with a live process. The test is
liveness, not status: a row left `running` by a crashed runner would otherwise pin the source
open forever.

**The file is never deleted.** A sealed source stays on disk as a backup.

**Concurrency.** The whole migration takes an exclusive `flock` on `<home>/migrate.lock`. Two
ticks that start together therefore serialize. The per-source record makes a second run a
no-op.

**Failure policy.** The policy differs by caller, because the cost of a wrong answer differs:

- **Targeted (a loop command, which writes).** A source that cannot be imported is a hard
  error. A tick that ran against an empty database would re-dispatch every issue and start a
  second agent in a worktree that already has one.
- **Sweep (a global read).** A source that cannot be imported is skipped and reported. One
  broken project must not stop `agent-utils list`.

### 4.6 The `migrate` command

```
agent-utils migrate [--dry-run]
```

It sweeps every registered project, prints one line per source, and reports what it imported,
what it skipped, and why. `--dry-run` reports without writing. The command is not required:
normal use migrates a project the first time a command touches it. The command exists so a
human can see the whole picture and so a report can be attached to a bug.

### 4.7 What the loop keeps in `state_dir`

`state_dir` keeps the per-loop tick lock and the log tree. Only the database moves. The lock
must stay per loop: it serializes ticks of one loop, and a machine-wide lock would serialize
unrelated loops.

### 4.8 Contention

One file now takes the writes of every tick and every runner on the machine. Three properties
keep this safe:

- WAL allows one writer and many readers at the same time.
- Every write is a single small statement. No transaction is held across a network call or an
  agent run.
- `busy_timeout` rises from 10s to 30s. The value is per connection and is passed in the DSN,
  as it is today (`internal/store/store.go:88-92`).

## 5. Alternatives considered

| Option | Why not |
|---|---|
| Keep per-loop files; `ATTACH` them for reads | Read-only. It gives no place for a machine-wide policy, and SQLite attaches ten databases by default. |
| One file per project | Still N files. The cross-project question stays a fan-out. |
| Refuse to migrate while a dispatch runs | Blocks the normal state of the system. Agents run for hours. |
| Renumber dispatches with no `legacy_id` | Breaks liveness and the runner log path of every in-flight dispatch. |
| Delete the source file after import | No way back if the import is wrong. |

## 6. Out of scope

A machine-wide limit on concurrent dispatches is the coordination policy this change enables.
It needs a home for machine-wide configuration and a rule for which loop wins. It is a separate
change, and it is much easier to review on its own. This design adds the global queries it
needs.

## 7. Risks

| Risk | Control |
|---|---|
| An import loses state | The source file is kept. The import is one transaction. |
| An import runs twice | `legacy_sources` records each source. Dispatch rows are unique on `(legacy_source, legacy_id)`. |
| An in-flight dispatch is orphaned | `legacy_id` keeps liveness correct. An open source is read again until it is idle. |
| A tick runs against an unmigrated project | The targeted path fails loudly rather than dispatching. |
| Write contention on one file | WAL, small writes, 30s `busy_timeout`. |

## 8. Process note

The operator waived the interactive brainstorm and the human gate for this run, and asked for
best judgment instead. Section 5 records the design alternatives that the brainstorm would have
produced. Every decision above is recorded so a reviewer can disagree with a named choice.
