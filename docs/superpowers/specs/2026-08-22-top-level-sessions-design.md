# Design: top-level sessions list

## Premise and blast-radius check (step-0 findings)

The tool shows sessions for one project only. `agent-utils project sessions list` reads one
project and prints its sessions. An operator who runs several projects has no single view. This
design adds `agent-utils sessions list`, which spans the machine.

- **Entry path.** `cmd/agent-utils/main.go:284` declares `sessionsCommand()`. That command is a
  child of `projectCommand()` (`cmd/agent-utils/main.go:323`), so it is project-scoped. Its
  action calls `openProject(c)`, then `loopcmd.Sessions(p, c.String("name"))`
  (`internal/loopcmd/sessions.go`), then `loopcmd.RenderSessions`. `loopcmd.Sessions` calls
  `store.DB.DispatchesForProject` (`internal/store/store.go:1052`), which filters on one
  `project_id`. There is no machine-wide equivalent for sessions.
- **Blast radius.** The change touches this repository only. The affected files are:
  - `internal/store/store.go` — one new read method on `DB`.
  - `internal/loopcmd/sessions.go` — the session type, the grouping function, a new aggregator,
    and a new renderer.
  - `cmd/agent-utils/main.go` — one new top-level command.
  - `README.md` — the global command table and the Sessions section.
  No other package reads `Session` or `sessionsFrom`. The database schema does not change.
- **Prior art.** Three patterns already exist and this design reuses all three:
  - `store.DB.RunningDispatches` (`internal/store/store.go:1038`) is a machine-wide dispatch
    read. The new method copies its shape.
  - `loopcmd.Projects` (`internal/loopcmd/projects.go`) is a machine-wide report. It reads the
    registry, opens the canonical database once, and renders a table. The new aggregator
    follows it.
  - `registry.Find` (`internal/registry/registry.go:176`) resolves a project by name, id, or
    path, and returns a listed error for an ambiguous name. `agent-utils forget` already uses
    it. The new `--project` flag uses the same function, so the errors match.
- **Contradiction scan.** One contradiction was found. `README.md:119` documents
  `agent-utils logs --project <p> --session <id>` as "Log search across projects".
  `logsCommand()` (`cmd/agent-utils/main.go:353`) declares no such flag. `openProject` reads the
  selector through `selectedProject` (`cmd/agent-utils/main.go:83`), which walks the command
  lineage for a command named `project`. At the top level there is no such parent, so top-level
  `logs` always resolves the project in the current directory. The documented flag does not
  exist. This design does not add it. The new command prints a follow hint that works today.
- **Profile.** `backend`. The change is Go, a CLI surface, and a database read. There is no UI.
- **Class.** `standard`. The change adds a public CLI surface and new read behavior. It touches
  no risky boundary: it is read-only, it makes no GitHub call, and it changes no schema.

## Requirements

The command answers one question: what has run on this machine, across every project, most
recent first.

1. `agent-utils sessions list` prints every session of every registered project.
2. Each row names its project.
3. Rows sort by last activity, newest first.
4. `--project <name|id|path>` restricts the output to one project.
5. `--loop <name>` restricts the output to loops with that name.
6. `--running` restricts the output to sessions whose most recent dispatch is alive.
7. `--orphaned` restricts the output to sessions whose most recent dispatch is marked running
   but whose process is gone.
8. `--running` and `--orphaned` together show both sets.
9. The command reads local state only. It needs no GitHub token and works offline.

## Architecture

The read path adds one query and one aggregation. Everything else is reuse.

```
cmd/agent-utils.sessionsCommand              (new, top level)
  |  SessionFilter{Project, Loop, Running, Orphaned}
  v
loopcmd.AllSessions                          (new)
  |  registry.Find(Project)   -> a project id, when --project is set
  |  registry.List()          -> project id -> display name
  |  openCanonical()          -> the one state database
  |  scoped:   DispatchesForProject(id)   (existing)
  |  unscoped: Dispatches()               (new: every dispatch, every project)
  |  sessionsFrom(...)        (existing, re-keyed)
  |  keepState, nameProjects, then a stable sort by Last descending
  v
loopcmd.RenderAllSessions                    (new)
```

`loopcmd.Sessions` keeps its signature and its behavior. `project sessions list` prints what it
prints today.

Both aggregators sort with `sort.SliceStable`, not `sort.Slice`. `sessionsFrom` returns its
sessions in `id DESC` order, which is total and deterministic. An unstable sort discards that
order for sessions that share a `Last` timestamp, and the row order then varies between two runs
of one command. Ties are more likely in the machine-wide report, because an imported dispatch
carries whatever timestamp the old file recorded.

### The store method

`store.DB.Dispatches()` returns every dispatch row on the machine, newest first. It is the
sibling of `DispatchesForProject` with the `WHERE` clause removed:

```go
func (d *DB) Dispatches() ([]Dispatch, error)
```

It selects `dispatchColumns` from `dispatches` and orders by `id DESC`. The order matters:
`sessionsFrom` depends on the newest row of a session arriving first.

### The grouping key

`sessionsFrom` groups dispatches into sessions. Today it keys its map on the session identifier
alone. This design changes the key to the pair of the project identifier and the session
identifier.

The change is safe for the existing caller. `loopcmd.Sessions` passes the dispatches of one
project, so the project identifier is constant and the grouping is identical. The change
matters for the new caller: a session identifier that appeared in two projects would otherwise
merge two projects' runs into one row, and the row would report the wrong project.

### The session record

`Session` gains two fields:

- `ProjectID` — the owning project's UUID, copied from the dispatch row.
- `Project` — the project's display name, resolved from the registry.

`RenderSessions` ignores both fields, so the per-project table does not change.

### Project names

`AllSessions` reads `registry.List()` once and builds a map from project identifier to name. A
session whose project the map cannot name still appears, in one of two forms. This follows
`RenderProjects`, which keeps a project it cannot read in the report rather than hiding it.

- **A forgotten project.** The identifier is set but the registry no longer holds it. `Project`
  holds the first eight characters of the identifier, so the row stays identifiable.
- **An unclaimed row.** The identifier is empty. `Project` holds the literal `(unclaimed)`.
  These rows are real: `dispatches.project_id` is `TEXT NOT NULL DEFAULT ''`, and `upgradeKeys`
  (`internal/store/store.go:360`) records that rows from before the project key carry an empty
  value. `stampInPlace` (`internal/store/legacy.go:363`) claims such a row only for a loop the
  sweep still discovers, so a row for a deleted loop keeps the empty value permanently.
  `DB.Dispatches()` is the first read in this tool with no project filter, so it is the first to
  return them. `--project` can never select such a row, because no selector resolves to an empty
  identifier.

`registry.Project.ID` is itself allowed to be empty: `Register`
(`internal/registry/registry.go:111`) matches an existing entry by path when the id is empty,
and `RenderProjects` guards with `if p.ID != ""`. A `--project` selector that resolves to such an
entry is rejected with an error naming `project init`. Filtering on an empty identifier would
report no sessions for a project that has many, and exit successfully.

### Filters

```go
type SessionFilter struct {
	Project  string // registry selector: a name, an id, or a path
	Loop     string // exact loop name
	Running  bool
	Orphaned bool
}
```

- `Project` resolves through `registry.Find`. A name that matches two projects returns the
  listed `ErrAmbiguousProject` error, the same error `agent-utils forget` returns. An unknown
  selector returns `ErrNoProject`. **A resolved project is applied as a query, not as a Go
  filter:** `AllSessions` calls the existing `DispatchesForProject(id)` when a project resolved,
  and `Dispatches()` only when none did. This repository enforces project isolation at the query
  layer, and `internal/store/scope_test.go` is the proof. A Go-side filter would make one `if`
  statement the only thing that separates two projects' rows.
- `Loop` matches the dispatch's loop name exactly, the same comparison `loopcmd.Sessions` makes.
- `Running` and `Orphaned` select on the session's computed state. Neither flag means every
  state. Both flags mean the union of the two.

### Output

```
PROJECT         SESSION           LOOP       ISSUE  TITLE              RUNS  COST    STATE      LAST RUN
lawndominator   cc33-tend-run     planning   57     Fix timezone bug   1     $0.30   succeeded  2026-08-18 21:17
my-repo         bb22-session-two  execution  57     Fix timezone bug   1     $2.40   ORPHANED   2026-08-18 21:12
```

`RenderAllSessions` prints the PROJECT column and then the columns `RenderSessions` prints. It
prints no project header line, because the table spans every project. It truncates long values
with the existing `truncate` helper (`internal/loopcmd/status.go:16`).

An empty result explains itself. With no filter, the text says no session exists yet and points
at `agent-utils list`. With a filter, the text says the filter matched nothing.

The footer names the command that follows one session:

```
Follow one with: agent-utils project --name <PROJECT> logs --session <SESSION>
```

The selector form is deliberate. Top-level `logs` resolves the project from the current
directory, so `agent-utils logs --session <id>` fails outside that project's tree. The
`project --name` form works from any directory today.

## Error handling

- A registry read failure fails the command. Without the registry there are no project names.
- A database open failure fails the command. `openCanonical` already reports an unimportable
  legacy source as a warning on stderr and continues, which is the behavior this report wants.
- An unknown or ambiguous `--project` selector fails the command with the registry's own error.
- A dispatch row with an empty session identifier is skipped, as `sessionsFrom` already does.
- **Accepted limitation.** `openCanonical` reports a legacy source it cannot import as a warning
  on stderr, then continues. A project whose legacy state failed to import therefore contributes
  no sessions, while the table on stdout looks complete. This is deliberate and is not new: it is
  the read path's stated contract, and `agent-utils list` already reports the machine under the
  same rule. Making the table say so on stdout would require `openCanonical` to return its sweep
  report to all four of its callers, which is a larger change than this feature. The warning on
  stderr stays the signal.

## Testing

Unit tests only. The functions under test take data, not a database handle, except the store
method, which the store package already tests against a temporary file.

- `store.DB.Dispatches` returns rows from every project, newest first.
- `sessionsFrom` keeps two projects apart when both define a loop with one name and one shared
  session identifier.
- `sessionsFrom` output for a single project is unchanged by the new key.
- Each filter selects the expected sessions: project, loop, running, orphaned, and the union of
  running and orphaned.
- A session whose project is not registered renders with the short project identifier.
- `RenderAllSessions` prints the PROJECT column, marks an orphan, and explains an empty result.
- `AllSessions` restricts to one project end to end, so the filter is proven to match the
  resolved project identifier and not the raw selector. A test of the pure helpers alone cannot
  catch that confusion: it compiles, returns nothing, and prints a legitimate-looking
  "no session matched the filter".
- `AllSessions` rejects a `--project` selector that resolves to an entry with no identifier.

## Out of scope

- `--limit`. The operator can pipe the output.
- A `--project` flag on the top-level `logs` command. `README.md:119` documents it and the code
  does not implement it. That gap is real and is recorded here, but this change does not close
  it.
- Any change to `project sessions list`.
- Any change to the database schema.

## Task order

Store method → session record and grouping key → aggregator with filters → renderer → command
wiring → README. Each step is testable on its own. The store method comes first because every
later step reads its output.
