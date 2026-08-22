# Implementation plan: top-level sessions list

**For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development
(recommended) or executing-plans to implement this plan task-by-task.

Design: [`docs/superpowers/specs/2026-08-22-top-level-sessions-design.md`](../specs/2026-08-22-top-level-sessions-design.md)

## Pipeline State

| Field   | Value                                                              |
|---------|--------------------------------------------------------------------|
| stage   | 3 (implementation)                                                 |
| class   | standard (adds a public CLI surface and a new read; no risky boundary) |
| profile | backend                                                            |
| branch  | feat/top-level-sessions                                            |
| pr      | #7                                                                 |
| gate    | approved 2026-08-22 (pre-approved in chat by @seanmcgary)           |
| round   | 0                                                                  |

## Architecture

One new query, one new aggregation, one new renderer, one new command. The per-project path
keeps its behavior.

```
cmd/agent-utils.sessionsCommand              (new, registered at the top level)
  |  loopcmd.SessionFilter{Project, Loop, Running, Orphaned}
  v
loopcmd.AllSessions                          (new, internal/loopcmd/sessions.go)
  |  registry.Find(filter.Project)  -> a project id, when --project is set
  |  registry.List()                -> project id -> display name
  |  openCanonical()                -> the one state database
  |  scoped:   store.DB.DispatchesForProject(id)   (existing)
  |  unscoped: store.DB.Dispatches()               (new)
  |  sessionsFrom(rows, loop)       (existing; keyed by project id AND session id)
  |  keepState(...)                 -> running / orphaned selection
  |  nameProjects(...)              -> display name, or a marker
  |  sort.SliceStable by Last, descending
  v
loopcmd.RenderAllSessions                    (new)
```

`loopcmd.Sessions` and `loopcmd.RenderSessions` keep their signatures. `agent-utils project
sessions list` prints what it prints today.

**Scoping stays in SQL.** When `--project` resolves, `AllSessions` calls the existing scoped
query. It does not read every row and drop the unwanted ones in Go. This repository enforces
project isolation at the query layer, and `internal/store/scope_test.go` is the proof. A Go-side
filter would make one `if` statement the only thing that separates two projects.

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
- **`dispatches.project_id` is `TEXT NOT NULL DEFAULT ''`** — `internal/store/store.go:34`, added
  by `addColumns` at `internal/store/store.go:324`. `upgradeKeys`
  (`internal/store/store.go:360`) states that rows from before the project key carry an empty
  `project_id`, and `stampInPlace` (`internal/store/legacy.go:363`) claims such rows only for a
  loop the sweep still discovers. A row for a deleted loop keeps `''` permanently.
  `internal/store/store_test.go:238` asserts this state. **`DB.Dispatches()` is the first read in
  this codebase with no project filter, so it is the first to return these rows.**
- `func List() ([]Project, error)` — `internal/registry/registry.go:131`. It returns every
  registered project, sorted by `LastSeen`, newest first.
- `func Find(selector string) (Project, error)` — `internal/registry/registry.go:176`. It
  matches a name (case-insensitive), then an id, then a path. It returns `ErrAmbiguousProject`
  for a name that matches two projects and `ErrNoProject` for no match. Return its error
  unwrapped; do not add a second layer of explanation.
- **`registry.Project.ID` can be empty.** `Register` (`internal/registry/registry.go:111`)
  appends an entry with whatever id it is given, and matches an existing entry by path when the
  id is empty. `RenderProjects` (`internal/loopcmd/projects.go`) guards with `if p.ID != ""`.
  The package doc (`internal/registry/registry.go:1`) calls the registry "a convenience index,
  never a source of truth."
- `type registry.Project struct` — `internal/registry/registry.go:31`. The fields this plan
  reads are `ID`, `Name`, and `Root`.
- `func openCanonical() (*store.DB, error)` — `internal/loopcmd/canonical.go`. It opens the one
  state database and sweeps legacy sources. The caller must `defer db.Close()`.
- `func truncate(s string, width int) string` — `internal/loopcmd/status.go:16`. It marks a cut
  string with a single-rune ellipsis.
- `func openDB(t *testing.T) *DB` — `internal/store/scope_test.go:242`. The store test helper
  that returns a `*DB`, not a project-scoped `*Store`. New store tests use it.
- `func (s *Store) CreateDispatch(d Dispatch) (int64, error)` — `internal/store/store.go:776`.
- `func isolate(t *testing.T)` — `internal/loopcmd/resolve_test.go:41`. It sets
  `AGENT_UTILS_HOME`, `HOME`, and `AGENT_UTILS_DIR` to fresh values, so `registry.Register` and
  `registry.List` cannot see another test's projects.
- Constants `testProject` (`internal/store/store_test.go:13`) and `otherProject`
  (`internal/store/scope_test.go:11`); constants `projectA` and `projectB`
  (`internal/loopcmd/canonical_test.go:10`). Reuse them.
- urfave/cli v3: a command declares `Name`, `Usage`, `Flags`, `Commands`, and
  `Action func(context.Context, *cli.Command) error`. Flags are `&cli.StringFlag{Name, Usage}`
  and `&cli.BoolFlag{Name, Usage}`. An action reads them with `c.String(name)` and
  `c.Bool(name)`. See `cmd/agent-utils/main.go:282` and `cmd/agent-utils/main.go:353`.

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

From `internal/loopcmd/projects.go`, on a project that cannot be read:

> Keep it in the report. A project whose directory has moved is
> something the operator should see, not something to hide.

From the user's global instructions, on chained calls in JavaScript and TypeScript: align a
chained `.method()` with the start of the line above. **This repository is Go. The rule does not
apply to any file in this plan.**

Additional binding conventions, observed from the codebase:

- Every exported identifier carries a doc comment. Non-trivial unexported identifiers carry one
  too (`sessionsFrom`, `summariseLoop`, `truncate`, `snapshot`). The comment says **why**, not
  only what.
- Constructor naming in `cmd/agent-utils`: the machine-wide command takes the bare name and the
  project-scoped twin takes the `project` prefix. `listCommand`
  (`cmd/agent-utils/main.go:452`) and `projectListCommand` (`cmd/agent-utils/main.go:242`);
  `projectStatusCommand` (`cmd/agent-utils/main.go:263`); `projectInitCommand`
  (`cmd/agent-utils/project.go:35`). No `all*` constructor exists.
- Errors wrap with `%w` and name the operation: `fmt.Errorf("query dispatches: %w", err)`.
- Table output uses `fmt.Fprintf` with explicit column widths into a `strings.Builder`, and each
  truncate width equals its column width.
- Tests are standard library only. No assertion library. Failure messages state the value and
  the want: `t.Errorf("Dispatches = %d, want 2", s.Dispatches)`.
- Test names are sentences: `TestSessionsFromKeepsTwoProjectsApart`.
- Every test in `internal/store/scope_test.go` and `internal/loopcmd/canonical_test.go` carries a
  leading comment that states what breaks if the behavior regresses. Tests in
  `internal/loopcmd/logs_test.go` do not follow this rule.

## Tasks

### Task 1: add the machine-wide dispatch read

**Files:** `internal/store/store.go`, `internal/store/scope_test.go`

**review: no** — one query, no new schema, no scoping decision. The whole-diff fan-out covers it.

Write the test first.

- [ ] Add `TestDispatchesSpansEveryProjectNewestFirst` to `internal/store/scope_test.go`, beside
      its sibling `TestRunningDispatchesSpansEveryProject` (`internal/store/scope_test.go:96`).
      Use `openDB(t)`. Create a dispatch under `testProject` and one under `otherProject` with
      `db.Project(id).CreateDispatch(...)`. Assert `db.Dispatches()` returns both rows, and that
      the higher `ID` comes first.
- [ ] Precede the test with a comment stating what it protects: the machine-wide sessions report
      cannot be built from a scoped read.
- [ ] Add `func (d *DB) Dispatches() ([]Dispatch, error)` to `internal/store/store.go`, next to
      `DispatchesForProject`. Select `dispatchColumns` from `dispatches`, order by `id DESC`,
      and return through `scanDispatches`. Wrap a query error with `%w`.
- [ ] Write a doc comment that states why the method exists, mirroring
      `internal/store/store.go:1037`: the sessions report spans the machine, and the per-project
      read cannot answer it. State also that this read returns rows with an empty `project_id`,
      which every scoped query hides.

**Acceptance:** `go test ./internal/store/ -run 'Dispatches' -count=1` passes. `make fmtcheck vet
lint` are clean.

### Task 2: key sessions by project and session, and carry the project on the record

**Files:** `internal/loopcmd/sessions.go`, `internal/loopcmd/canonical_test.go`

**review: no** — a contained change to one unexported function, covered by a test that proves
the existing behavior is intact.

Write the tests first.

- [ ] Add `TestSessionsFromKeepsTwoProjectsApart` to `internal/loopcmd/canonical_test.go`. Build
      two dispatches that share one session identifier and one loop name but differ in
      `ProjectID` (`projectA` and `projectB`). Assert `sessionsFrom` returns two sessions, and
      that each carries its own `ProjectID`. Precede it with a comment stating the regression it
      catches: one project's runs reported under another project's name.
- [ ] Confirm the existing tests `TestSessionsFromSummarisesNewestFirst`,
      `TestSessionsFromFiltersByLoop`, and `TestSessionsFromReportsAnOrphan` pass without an
      edit. They are the proof that the per-project path did not change.
- [ ] Add a `ProjectID string` field to `Session`. Comment it: the owning project, copied from
      the dispatch row, and half the grouping key.
- [ ] Add a `Project string` field to `Session`. Comment it: the display name, which only the
      machine-wide report fills in.
- [ ] Change the map in `sessionsFrom` to key on a struct of `ProjectID` and `SessionID`. Set
      `Session.ProjectID` from the dispatch. Keep the `order` slice so the newest-first order
      survives; its element type changes to the same key struct.
- [ ] Extend the doc comment on `sessionsFrom` to state why the key holds the project: a session
      identifier that repeated across projects would otherwise merge two projects into one row.
- [ ] Change `sort.Slice` to `sort.SliceStable` in `Sessions`. Comment why: `sessionsFrom`
      returns rows in `id DESC` order, and that order is the tiebreak worth keeping when two
      sessions share a `Last` timestamp.

**Acceptance:** `go test ./internal/loopcmd/ -run 'SessionsFrom' -count=1` passes, including the
three pre-existing tests, unedited. `make fmtcheck vet lint` are clean.

### Task 3: aggregate every project's sessions, with filters

**Files:** `internal/loopcmd/sessions.go`, `internal/loopcmd/canonical_test.go`

**review: yes** — this task owns the filter semantics, the scoped-query branch, and two fallback
cases. All three are behavior a reviewer must check against the design.

Write the tests first.

- [ ] Declare `SessionFilter` with the fields `Project string`, `Loop string`, `Running bool`,
      and `Orphaned bool`. Document that `Project` is a registry selector, and that neither state
      flag means every state while both mean the union.
- [ ] Add `func (f SessionFilter) filtered() bool`, true when any field is set. Document that the
      renderer branches its empty-list text on it, and that keeping the rule on the type is what
      stops the command layer from restating filter semantics.
- [ ] Add an unexported helper `keepState(s Session, running, orphaned bool) bool`. It returns
      true when neither flag is set, `s.Live` when only `running` is set, `s.Orphaned` when only
      `orphaned` is set, and `s.Live || s.Orphaned` when both are set. Write a why-comment: the
      rule is not inferable from the signature.
- [ ] Add `TestKeepStateSelectsTheRequestedStates`, a table test over the four flag combinations
      and the three session states (live, orphaned, finished). Precede it with a comment.
- [ ] Add an unexported helper `nameProjects(sessions []Session, names map[string]string)`. For
      each session it sets `Project` from the map. It falls back in two distinct cases:
      - `ProjectID` is empty: use the literal `(unclaimed)`. These are pre-project rows, which
        no scoped query returns and which `--project` can never select. Cite
        `internal/store/store.go:360`.
      - `ProjectID` is set but absent from the map (a forgotten project): use the first eight
        characters, or the whole string when it is shorter.
      Write a why-comment citing the `RenderProjects` precedent quoted in Global Constraints: a
      project the operator can no longer name stays in the report rather than disappearing.
- [ ] Add `TestNameProjectsMarksForgottenAndUnclaimedProjects`, covering the registered name, the
      forgotten short id, and the empty `(unclaimed)` case. Precede it with a comment.
- [ ] Add `func AllSessions(f SessionFilter) ([]Session, error)`:
      - When `f.Project` is not empty, call `registry.Find(f.Project)` and return its error
        unwrapped.
      - **Reject a resolved project whose `ID` is empty** with
        `fmt.Errorf("project %q has no identifier; run agent-utils project init in %s", f.Project, p.Root)`.
        Filtering on an empty id would silently report no sessions for a project that has many.
      - Read `registry.List()` for the name map.
      - Call `openCanonical()` and `defer db.Close()`.
      - Call `db.DispatchesForProject(id)` when a project resolved, and `db.Dispatches()`
        otherwise. Do not filter rows by project in Go.
      - Group with `sessionsFrom(rows, f.Loop)`, apply `keepState`, apply `nameProjects`, and
        sort with `sort.SliceStable` by `Last`, descending.
- [ ] Document on `AllSessions` why the registry is read before the database: an unknown or
      unusable `--project` must fail before the command opens and migrates anything. Document
      that it reads local state only, makes no GitHub call, and needs no token.
- [ ] Add `TestAllSessionsRestrictsToOneProject`. Call `isolate(t)` first, then `openCanonical()`
      directly — **do not** also call `openCanonicalForTest(t)`, because it re-sets
      `AGENT_UTILS_HOME` to a different directory and would discard the registry `isolate` set
      up. Register two projects with `registry.Register`, write a dispatch for each with
      `db.Project(id).CreateDispatch`, then assert that `AllSessions(SessionFilter{Project: name})`
      returns only that project's session and that its `Project` field holds the registered name.
      Precede it with a comment: this is the only test that proves the filter matches the
      resolved project id and not the raw selector.
- [ ] Add `TestAllSessionsRejectsAProjectWithNoIdentifier`: `isolate(t)`, register a project with
      an empty id, and assert `AllSessions` returns an error naming `project init`.

**Acceptance:** `go test ./internal/loopcmd/ -run 'KeepState|NameProjects|AllSessions' -count=1`
passes. `make fmtcheck vet lint` are clean.

### Task 4: render the machine-wide table

**Files:** `internal/loopcmd/sessions.go`, `internal/loopcmd/logs_test.go`

**review: no** — formatting, covered by its own tests and by the fan-out.

Write the tests first. Put them beside the existing `TestRenderSessions...` tests.

- [ ] Add `TestRenderAllSessionsShowsTheProjectColumnAndFlagsAnOrphan`. Assert the header
      contains `PROJECT`, that a project name appears, and that an orphaned session renders
      `ORPHANED`.
- [ ] Add `TestRenderAllSessionsExplainsAnEmptyList`, covering both the unfiltered text and the
      filtered text.
- [ ] Add `func RenderAllSessions(sessions []Session, f SessionFilter) string`. Write a doc
      comment stating that the table spans every project, and why it takes the filter: the empty
      result needs different text when a filter excluded everything.
- [ ] Use this header and row format. The existing columns stay byte-identical to
      `RenderSessions`; PROJECT is a new leading `%-16s`:

      header: `"%-16s %-38s %-12s %-6s %-30s %-5s %-9s %-10s %s\n"`
      row:    `"%-16s %-38s %-12s %-6d %-30s %-5d $%-8.2f %-10s %s\n"`

      Columns: `PROJECT SESSION LOOP ISSUE TITLE RUNS COST STATE LAST RUN`.
- [ ] Truncate with each column's own width: `truncate(s.Project, 16)`, `truncate(s.Loop, 12)`,
      `truncate(s.Title, 30)`. Do not truncate `SESSION`; a session identifier is a 36-character
      UUID and the `%-38s` column holds it whole, as `RenderSessions` already does.
- [ ] Compute the state exactly as `RenderSessions` does: `running` when `Live`, `ORPHANED` when
      `Orphaned`, otherwise `LastStatus`.
- [ ] Print no project header line.
- [ ] For an empty result with `f.filtered()` false, print that no session exists yet and point
      at `agent-utils list`. For an empty result with `f.filtered()` true, print that no session
      matched the filter.
- [ ] Print the footer `Follow one with: agent-utils project --name <PROJECT> logs --session
      <SESSION>`. Add a comment stating why the footer uses the `project --name` form: top-level
      `logs` resolves the project from the current directory, so the short form fails from
      elsewhere.
- [ ] Leave `RenderSessions` untouched.

**Acceptance:** `go test ./internal/loopcmd/ -run 'RenderAllSessions|RenderSessions' -count=1`
passes. `make fmtcheck vet lint` are clean.

### Task 5: wire the top-level command

**Files:** `cmd/agent-utils/main.go`

**review: yes** — this is the public surface. A reviewer must check the flag names, the usage
strings, the constructor rename, and that the command is registered in the machine-wide group.

- [ ] **Rename the existing `sessionsCommand()` (`cmd/agent-utils/main.go:282`) to
      `projectSessionsCommand()`**, and update its one reference inside `projectCommand()`
      (`cmd/agent-utils/main.go:322`). This is a rename with no behavior change. It follows the
      convention that the machine-wide constructor takes the bare name and the project-scoped
      twin takes the `project` prefix, as `listCommand` and `projectListCommand` do.
- [ ] Add a new `func sessionsCommand() *cli.Command`, named `sessions`, with one subcommand
      `list`. Give `list` the flags `project` (usage: restrict to one project, by name, id or
      path), `loop` (usage: restrict to loops with this name), `running`, and `orphaned`.
- [ ] Comment the flag block: this command spells the loop selector `--loop` while `project
      sessions list` spells it `--name`, because at the top level `--project` and the loop
      selector must coexist on one command, and two flags both called `name` cannot. Reference
      the `selectedProject` comment at `cmd/agent-utils/main.go:83`, which explains the shadowing
      that forced the older spelling.
- [ ] Do not add an alias. The two commands are deliberately different surfaces.
- [ ] The action builds a `loopcmd.SessionFilter` from the flags, calls `loopcmd.AllSessions`,
      and prints `loopcmd.RenderAllSessions(sessions, filter)`. The command layer holds no filter
      knowledge beyond building the struct.
- [ ] Register `sessionsCommand()` in the top-level `Commands` slice in `main`, inside the block
      commented `// Top level spans the machine.`, after `listCommand()`.
- [ ] Do not add it to `projectCommand()`.
- [ ] Write a doc comment on `sessionsCommand` that states why it is separate from
      `projectSessionsCommand`: the flags and the renderer differ, and the top level is the
      machine-wide scope.

**Acceptance:** `make build` succeeds. `./bin/agent-utils sessions list --help` shows the four
flags. `./bin/agent-utils sessions list` runs and prints a table or the empty-list text.
`./bin/agent-utils project sessions list --help` is unchanged. `make fmtcheck vet lint` are
clean.

### Task 6: document the command

**Files:** `README.md`

**review: no** — documentation, covered by the fan-out.

- [ ] Add a row to the "Global" command table (`README.md:116`):
      `agent-utils sessions list [--project <p>] [--loop <l>] [--running] [--orphaned]` —
      "Every session on this machine, with its project, issue, runs, cost and state". Put it
      after the `agent-utils list` row.
- [ ] Add a paragraph and an example to the "Sessions" section. **Capture the example by running
      `./bin/agent-utils sessions list` and pasting its real output.** Do not adapt the sample
      from the design document; that sample uses invented short session identifiers, and real
      ones are 36-character UUIDs.
- [ ] Rewrite `README.md:168`, which currently reads "`--name <loop>` restricts the list to one
      loop". Attribute each spelling to its command: `project sessions list` takes `--name`, and
      the machine-wide `sessions list` takes `--loop`.
- [ ] State that the follow command is `agent-utils project --name <p> logs --session <id>`.
- [ ] Do not change the `agent-utils logs --project <p> --session <id>` row at `README.md:119`.
      That gap is recorded in the design document and is out of scope here.

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
- A `--project` flag on the top-level `logs` command, which `README.md:119` documents and the
  code does not implement.
- Any change to what `agent-utils project sessions list` prints. Its constructor is renamed in
  task 5; its behavior is not touched.
- Any change to the database schema.
- Reporting unimportable legacy sources on stdout. `openCanonical` warns on stderr and
  continues, which every machine-wide report in this tool already relies on. The design's "Error
  handling" section records the accepted limitation.
