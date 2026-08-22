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
- **Contradiction scan.** One contradiction was found. `README.md:122` documents
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
cmd/agent-utils.allSessionsCommand           (new, top level)
  |  SessionFilter{Project, Loop, Running, Orphaned}
  v
loopcmd.AllSessions                          (new)
  |  registry.List()          -> project id -> display name
  |  openCanonical()          -> the one state database
  |  store.DB.Dispatches()    (new: every dispatch, every project)
  |  sessionsFrom(...)        (existing, re-keyed)
  |  filter, then sort by Last descending
  v
loopcmd.RenderAllSessions                    (new)
```

`loopcmd.Sessions` keeps its signature and its behavior. `project sessions list` is unchanged.

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
dispatch whose project is no longer registered still appears. Its `Project` field holds the
first eight characters of the project identifier, so the row remains identifiable. This follows
`RenderProjects`, which keeps a missing project in the report rather than hiding it.

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
  selector returns `ErrNoProject`.
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

## Out of scope

- `--limit`. The operator can pipe the output.
- A `--project` flag on the top-level `logs` command. `README.md:122` documents it and the code
  does not implement it. That gap is real and is recorded here, but this change does not close
  it.
- Any change to `project sessions list`.
- Any change to the database schema.

## Task order

Store method → session record and grouping key → aggregator with filters → renderer → command
wiring → README. Each step is testable on its own. The store method comes first because every
later step reads its output.
