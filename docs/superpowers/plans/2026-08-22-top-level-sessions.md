# Implementation plan: top-level sessions list

**For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development
(recommended) or executing-plans to implement this plan task-by-task.

Design: [`docs/superpowers/specs/2026-08-22-top-level-sessions-design.md`](../specs/2026-08-22-top-level-sessions-design.md)

## Pipeline State

| Field   | Value                                                              |
|---------|--------------------------------------------------------------------|
| stage   | 2 (plan review)                                                    |
| class   | standard (adds a public CLI surface and a new read; no risky boundary) |
| profile | backend                                                            |
| branch  | feat/top-level-sessions                                            |
| pr      | #7                                                                 |
| gate    | pending                                                            |
| round   | 0                                                                  |

## Architecture

One new query, one new aggregation, one new renderer, one new command. The per-project path is
unchanged.

```
cmd/agent-utils.allSessionsCommand           (new, registered at the top level)
  |  loopcmd.SessionFilter{Project, Loop, Running, Orphaned}
  v
loopcmd.AllSessions                          (new, internal/loopcmd/sessions.go)
  |  registry.Find(filter.Project)  -> a project id, when --project is set
  |  registry.List()                -> project id -> display name
  |  openCanonical()                -> the one state database
  |  store.DB.Dispatches()          (new: every dispatch of every project)
  |  sessionsFrom(rows, loop)       (existing; keyed by project id AND session id)
  |  keepState(...)                 -> running / orphaned selection
  |  sort by Last, descending
  v
loopcmd.RenderAllSessions                    (new)
```

`loopcmd.Sessions` and `loopcmd.RenderSessions` keep their signatures. `agent-utils project
sessions list` prints exactly what it prints today.

## Verified external API (do not re-derive)

Every signature below was read from the source in this repository at the stated location. Do
not re-derive them. Do not change them.

- `func (d *DB) DispatchesForProject(projectID string) ([]Dispatch, error)` —
  `internal/store/store.go:1052`. It selects `dispatchColumns` and orders by `id DESC`. The new
  method copies this shape without the `WHERE` clause.
- `const dispatchColumns` — `internal/store/store.go:830`. Use this constant. Do not write a
  column list by hand.
- `func scanDispatches(rows *sql.Rows) ([]Dispatch, error)` — `internal/store/store.go:853`.
  Every dispatch query returns through it.
- `type Dispatch struct` — `internal/store/types.go:59`. The fields this plan reads are
  `ID`, `ProjectID`, `Loop`, `Number`, `SessionID`, `PID`, `Status`, `StartedAt`, `CostUSD`,
  `Title`.
- `func (d Dispatch) RunnerID() int64` — `internal/store/types.go:99`. Liveness checks use this,
  never `ID`.
- `func IsAlive(pid int, dispatchID int64) bool` — `internal/proc/proc.go:34`.
- `const StatusRunning = "running"` — `internal/store/types.go:14`.
- `func List() ([]Project, error)` — `internal/registry/registry.go:131`. It returns every
  registered project, sorted by `LastSeen`, newest first.
- `func Find(selector string) (Project, error)` — `internal/registry/registry.go:176`. It
  matches a name (case-insensitive), then an id, then a path. It returns `ErrAmbiguousProject`
  for a name that matches two projects and `ErrNoProject` for no match. Return its error
  unwrapped; do not add a second layer of explanation.
- `type registry.Project struct` — `internal/registry/registry.go:31`. The fields this plan
  reads are `ID` and `Name`.
- `func openCanonical() (*store.DB, error)` — `internal/loopcmd/canonical.go`. It opens the one
  state database and sweeps legacy sources. The caller must `defer db.Close()`.
- `func truncate(s string, width int) string` — `internal/loopcmd/status.go:16`. It marks a cut
  string with a single-rune ellipsis.
- `func openDB(t *testing.T) *DB` — `internal/store/scope_test.go:242`. The store test helper
  that returns a `*DB`, not a project-scoped `*Store`. New store tests use it.
- Constants `projectA` and `projectB` — `internal/loopcmd/canonical_test.go:10`. Existing test
  UUIDs. Reuse them.
- urfave/cli v3: a command declares `Name`, `Usage`, `Flags`, and `Action func(context.Context,
  *cli.Command) error`. Flags are `&cli.StringFlag{Name, Usage}` and `&cli.BoolFlag{Name,
  Usage}`. An action reads them with `c.String(name)` and `c.Bool(name)`. See
  `cmd/agent-utils/main.go:284` and `cmd/agent-utils/main.go:353`.

## Global Constraints

**This repository has no `AGENTS.md`, `CLAUDE.md`, `CONTRIBUTING.md`, `STANDARDS.md`, or
`STYLEGUIDE.md` at its root.** The binding conventions come from `README.md`, `Makefile`,
`.golangci.yml`, and codebase precedent. The text below is copied word for word. Do not
paraphrase it. Do not violate it.

From `README.md`, "Development":

> ```bash
> make deps        # install golangci-lint and staticcheck
> make build       # build ./bin/agent-utils, version stamped in
> make check       # fmtcheck + vet + lint + test, in that order
> ```

> Tests run with `-p 1` and no cache on purpose: the `worktree` package shells out to real git
> and `runner` spawns real processes, so package-level parallelism is not safe, and a cached
> PASS is not evidence about the working tree.

From `README.md`, "Commands":

> Commands split by scope. **Top level spans the machine; `project` acts on one project.**

From `internal/loopcmd/canonical.go`, on the read path:

> It is the READ path, so a source that cannot be imported is a warning and not
> a failure: one broken project must not stop a report about all the others.

> The warning goes to stderr. Everything these commands print to stdout is a
> table an operator may pipe somewhere.

From the user's global instructions, on chained calls in JavaScript and TypeScript: align a
chained `.method()` with the start of the line above. **This repository is Go. The rule does not
apply to any file in this plan.**

Additional binding conventions, observed from the codebase:

- Every exported identifier carries a doc comment. The comment says **why**, not only what.
  `internal/loopcmd/sessions.go` and `internal/store/store.go` are the models.
- Errors wrap with `%w` and name the operation: `fmt.Errorf("query dispatches: %w", err)`.
- Table output uses `fmt.Fprintf` with explicit column widths into a `strings.Builder`.
- Tests are standard library only. No assertion library. Failure messages state the value and
  the want: `t.Errorf("Dispatches = %d, want 2", s.Dispatches)`.
- Test names are sentences: `TestSessionsFromKeepsTwoProjectsApart`.

## Tasks

### Task 1: add the machine-wide dispatch read

**Files:** `internal/store/store.go`, `internal/store/scope_test.go`

**review: no** — one query, no new schema, no scoping decision. The whole-diff fan-out covers it.

Write the test first.

- [ ] Add `TestDispatchesReturnsEveryProjectNewestFirst` to `internal/store/scope_test.go`. Use
      `openDB(t)`. Create a dispatch under `testProject` and one under `otherProject` with
      `db.Project(id).CreateDispatch(...)`. Assert `db.Dispatches()` returns both rows, and that
      the higher `ID` comes first.
- [ ] Add `func (d *DB) Dispatches() ([]Dispatch, error)` to `internal/store/store.go`, next to
      `DispatchesForProject`. Select `dispatchColumns` from `dispatches`, order by `id DESC`,
      and return through `scanDispatches`. Wrap a query error with `%w`.
- [ ] Write a doc comment that states why the method exists: the sessions report spans the
      machine, and the per-project read cannot answer it.

**Acceptance:** `go test ./internal/store/ -run Dispatches -count=1` passes. `make fmtcheck vet
lint` are clean.

### Task 2: key sessions by project and session, and carry the project on the record

**Files:** `internal/loopcmd/sessions.go`, `internal/loopcmd/canonical_test.go`

**review: no** — a contained change to one unexported function, covered by a test that proves
the old behavior is intact.

Write the tests first.

- [ ] Add `TestSessionsFromKeepsTwoProjectsApart` to `internal/loopcmd/canonical_test.go`. Build
      two dispatches that share one session identifier and one loop name but differ in
      `ProjectID` (`projectA` and `projectB`). Assert `sessionsFrom` returns two sessions, and
      that each carries its own `ProjectID`.
- [ ] Confirm the existing tests `TestSessionsFromSummarisesNewestFirst`,
      `TestSessionsFromFiltersByLoop`, and `TestSessionsFromReportsAnOrphan` still pass without
      an edit. They are the proof that the per-project path did not change.
- [ ] Add a `ProjectID string` field and a `Project string` field to `Session`. Document
      `Project` as the display name, which only the machine-wide report fills in.
- [ ] Change the map in `sessionsFrom` to key on a struct of `ProjectID` and `SessionID`. Set
      `Session.ProjectID` from the dispatch. Keep the `order` slice so the newest-first order
      survives; its element type changes to the same key struct.
- [ ] Extend the doc comment on `sessionsFrom` to state why the key holds the project: a session
      identifier that repeated across projects would otherwise merge two projects into one row.

**Acceptance:** `go test ./internal/loopcmd/ -run SessionsFrom -count=1` passes, including the
three pre-existing tests, unedited. `make fmtcheck vet lint` are clean.

### Task 3: aggregate every project's sessions, with filters

**Files:** `internal/loopcmd/sessions.go`, `internal/loopcmd/canonical_test.go`

**review: yes** — this task owns the filter semantics and the fallback for an unregistered
project. Both are behavior a reviewer should check against the design.

Write the tests first. Test the filter through a pure helper so no test needs a database.

- [ ] Declare `SessionFilter` with the fields `Project string`, `Loop string`, `Running bool`,
      and `Orphaned bool`. Document that `Project` is a registry selector, and that neither
      state flag means every state while both mean the union.
- [ ] Add an unexported helper `keepState(s Session, running, orphaned bool) bool`. It returns
      true when neither flag is set, `s.Live` when only `running` is set, `s.Orphaned` when only
      `orphaned` is set, and `s.Live || s.Orphaned` when both are set.
- [ ] Add `TestKeepStateSelectsTheRequestedStates`, a table test over the four flag
      combinations and the three session states (live, orphaned, finished).
- [ ] Add an unexported helper `nameProjects(sessions []Session, names map[string]string)`. For
      each session it sets `Project` from the map, and falls back to the first eight characters
      of `ProjectID` when the map has no entry. A `ProjectID` shorter than eight characters is
      used whole.
- [ ] Add `TestNameProjectsFallsBackToTheShortIDForAForgottenProject`.
- [ ] Add `func AllSessions(f SessionFilter) ([]Session, error)`. It resolves `f.Project` through
      `registry.Find` when the field is not empty, and returns that error unwrapped. It reads
      `registry.List()` for the name map. It calls `openCanonical()` and defers `db.Close()`. It
      calls `db.Dispatches()`, filters the rows by project identifier when one was resolved,
      groups with `sessionsFrom(rows, f.Loop)`, applies `keepState`, applies `nameProjects`, and
      sorts by `Last` descending. Document why it reads the registry before the database: an
      unknown `--project` must fail before the command opens and migrates anything.
- [ ] Write a doc comment on `AllSessions` that states it reads local state only, makes no
      GitHub call, and needs no token.

**Acceptance:** `go test ./internal/loopcmd/ -run 'KeepState|NameProjects' -count=1` passes.
`make fmtcheck vet lint` are clean.

### Task 4: render the machine-wide table

**Files:** `internal/loopcmd/sessions.go`, `internal/loopcmd/logs_test.go`

**review: no** — formatting, covered by its own tests and by the fan-out.

Write the tests first. Put them beside the existing `TestRenderSessions...` tests.

- [ ] Add `TestRenderAllSessionsShowsTheProjectColumnAndFlagsAnOrphan`. Assert the header
      contains `PROJECT`, that a project name appears, and that an orphaned session renders
      `ORPHANED`.
- [ ] Add `TestRenderAllSessionsExplainsAnEmptyList`, covering both the unfiltered text and the
      filtered text.
- [ ] Add `func RenderAllSessions(sessions []Session, filtered bool) string`. Print the header
      `PROJECT SESSION LOOP ISSUE TITLE RUNS COST STATE LAST RUN` with explicit widths. Compute
      the state exactly as `RenderSessions` does: `running` when `Live`, `ORPHANED` when
      `Orphaned`, otherwise `LastStatus`. Truncate `Project`, `Loop`, and `Title` with
      `truncate`. Print no project header line.
- [ ] For an empty result with `filtered` false, print that no session exists yet and point at
      `agent-utils list`. For an empty result with `filtered` true, print that no session matched
      the filter.
- [ ] Print the footer `Follow one with: agent-utils project --name <PROJECT> logs --session
      <SESSION>`. Add a comment stating why the footer uses the `project --name` form: top-level
      `logs` resolves the project from the current directory, so the short form fails from
      elsewhere.
- [ ] Leave `RenderSessions` untouched.

**Acceptance:** `go test ./internal/loopcmd/ -run RenderAllSessions -count=1` passes, and
`go test ./internal/loopcmd/ -run RenderSessions -count=1` still passes. `make fmtcheck vet
lint` are clean.

### Task 5: wire the top-level command

**Files:** `cmd/agent-utils/main.go`

**review: yes** — this is the public surface. A reviewer should check the flag names, the usage
strings, and that the command is registered in the machine-wide group and not under `project`.

- [ ] Add `func allSessionsCommand() *cli.Command`, named `sessions`, with one subcommand
      `list`. Give `list` the flags `project` (usage: restrict to one project, by name, id or
      path), `loop` (usage: restrict to loops with this name), `running`, and `orphaned`.
- [ ] The action builds a `loopcmd.SessionFilter` from the flags, calls `loopcmd.AllSessions`,
      and prints `loopcmd.RenderAllSessions`. Pass `filtered` as true when any of the four flags
      is set.
- [ ] Register `allSessionsCommand()` in the top-level `Commands` slice in `main`, inside the
      block commented `// Top level spans the machine.`, after `listCommand()`.
- [ ] Do not add it to `projectCommand()`. Do not change `sessionsCommand()`.
- [ ] Write a doc comment on `allSessionsCommand` that states why it is separate from
      `sessionsCommand`: the flags and the renderer differ, and the top level is the
      machine-wide scope.

**Acceptance:** `make build` succeeds. `./bin/agent-utils sessions list --help` shows the four
flags. `./bin/agent-utils sessions list` runs and prints a table or the empty-list text.
`./bin/agent-utils project sessions list --help` is unchanged. `make fmtcheck vet lint` are
clean.

### Task 6: document the command

**Files:** `README.md`

**review: no** — documentation, covered by the fan-out.

- [ ] Add a row to the "Global" command table:
      `agent-utils sessions list [--project <p>] [--loop <l>] [--running] [--orphaned]` —
      "Every session on this machine, with its project, issue, runs, cost and state".
- [ ] Add a paragraph and an example to the "Sessions" section. Show the machine-wide table with
      the PROJECT column. State that the per-project form is `agent-utils project sessions list`.
- [ ] State that the follow command is `agent-utils project --name <p> logs --session <id>`.
- [ ] Do not change the existing `agent-utils logs --project <p> --session <id>` row. That gap is
      recorded in the design document and is out of scope here.

**Acceptance:** The table row and the example match the binary's real output. `make check`
passes.

## Verification

Run `make check` once, at commit review. It runs `fmtcheck`, `vet`, `lint`, and the full test
suite, in that order. Per-task runs are targeted, as each task's acceptance criteria state.

Manual check, after task 5:

```bash
make build
./bin/agent-utils sessions list
./bin/agent-utils sessions list --running
./bin/agent-utils sessions list --project <a real project>
./bin/agent-utils sessions list --project no-such-project   # expect the registry's error
./bin/agent-utils project sessions list                     # expect no change
```

## Out of scope

- `--limit`.
- A `--project` flag on the top-level `logs` command, which `README.md:122` documents and the
  code does not implement.
- Any change to `agent-utils project sessions list`.
- Any change to the database schema.
