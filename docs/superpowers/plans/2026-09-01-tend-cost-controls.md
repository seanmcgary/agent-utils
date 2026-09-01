# Tend cost controls — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop a tend dispatch that has nothing to do, stop a tend dispatch that repeats a
conflict it already lost, and let the tend dispatches that remain run a cheaper agent.

**Architecture:** Four changes on one path, in dependency order. A new `tend:` configuration
section gives a tend dispatch its own harness, model, and effort, and `runner.Effective` and
`engine.effectiveHarness` both learn the dispatch kind so the session-continuity rule still
holds. A new GitHub read reports the latest review activity on a pull request, a new store read
reports the last tend dispatch for it, and `engine.tendDecisions` treats "review activity is
newer" as a second reason to tend. A new `tend_conflicts` table fingerprints a rebase conflict,
and `loopcmd` refuses to dispatch the agent at a fingerprint that already defeated it.

**Tech Stack:** Go 1.25, `urfave/cli/v3`, `gopkg.in/yaml.v3`, `go-github/v77`, SQLite through
`internal/store`, git through `os/exec`.

**Spec:** `docs/superpowers/specs/2026-09-01-tend-cost-controls-design.md`

## Pipeline State

| Field   | Value |
|---------|-------|
| stage   | 2 (plan review) |
| class   | large (new config section, new table, new GitHub read, new gate before a dispatch) |
| profile | backend |
| branch  | feat/tend-cost-controls |
| pr      | #25 |
| gate    | pending |
| round   | 0 |
| decisions | 0 |

### Decisions

(none yet)

## What is already done

The request asks for four changes. Pull request #18 (`7184c75`) landed the clean-rebase fast
path after the request's log sample was taken.

- **(c) is complete.** `loopcmd.gitRebase` (`internal/loopcmd/rebase.go:126`) rebases with git,
  pushes with `--force-with-lease` pinned to the commit it fetched, and dispatches the agent only
  on a conflict. This plan changes it only to add the backoff outcome and the review-pending
  fall-through.
- **(b) is half complete.** `engine.tendDecisions` already refuses to decide a pull request that
  is not behind its base (`internal/engine/engine.go:381`). This plan adds the review-activity
  half.

## Global Constraints

The repository has no `AGENTS.md`, `CLAUDE.md`, or `CONTRIBUTING.md`. The binding rules for this
change come from the code and from `README.md`:

- **Session continuity per harness.** A session belongs to the harness that minted it. Enforced by
  `engine.resumable` (`internal/engine/engine.go:302`) and documented in
  `docs/configuration.md#tend_pr`. Changing the harness must START a session, never resume one.
- **Every `_test.go` file is in the SAME package as the code it tests.** Write unqualified names.
- **A test must not be run in parallel across packages.** `make test` passes `-p 1`; do not add
  `t.Parallel()` to a test that shells out to git or spawns a process.
- **A new NOT NULL column carries a literal DEFAULT.** Stated at
  `internal/store/store.go:30`. A wall-clock deadline is stored as Unix seconds in an INTEGER,
  because no literal `TIMESTAMP` default reads back as the zero time
  (`internal/store/store.go:53`).
- **A new column added after the first release must also be listed in `addedColumns`**
  (`internal/store/store.go:365`). `CREATE TABLE IF NOT EXISTS` does nothing to a database that
  already exists.
- **Every scoped read and write carries `project_id`.** `internal/store/scope_test.go` proves it.
- **Map iteration is never a log or an error order.** Sort the keys; `internal/config.Load` and
  `loopcmd.TendCheck` both record the rule.
- **`--force-with-lease`, never plain `--force`.** Enforced by `worktree.Manager.PushWithLease`.
- **Rebase a feature branch onto its base; never merge the base into it.** Repository owner's
  standing rule.
- **No `Co-Authored-By:` trailer on any commit.** Repository owner's standing rule; it overrides
  any skill template.
- **Comment style.** This codebase explains invariants and rejected alternatives in prose above the
  code. Read the neighbouring file before you write a comment, and say WHY, not what.

## Verified external API (do not re-derive)

Read from `github.com/google/go-github/v77@v77.0.0` in the module cache.

```go
// github/pulls_reviews.go:110
func (s *PullRequestsService) ListReviews(ctx context.Context, owner, repo string, number int,
    opts *ListOptions) ([]*PullRequestReview, *Response, error)

// github/pulls_comments.go:80
func (s *PullRequestsService) ListComments(ctx context.Context, owner, repo string, number int,
    opts *PullRequestListCommentsOptions) ([]*PullRequestComment, *Response, error)
```

- `PullRequestReview.SubmittedAt` is a `*Timestamp`. Read it with `GetSubmittedAt().Time`.
- `PullRequestComment.CreatedAt` and `.UpdatedAt` are `*Timestamp`. Read them the same way.
- `PullRequestListCommentsOptions` carries `Sort` (`"created"` or `"updated"`), `Direction`
  (`"asc"` or `"desc"`), and an embedded `ListOptions`. `ListReviews` takes a bare `ListOptions`
  and has NO sort option: its results are oldest first, so the last page holds the newest review.

Repository-internal facts this plan depends on:

- `runner.Effective` — `internal/runner/args.go:59`. Callers: `args.go:102,142`,
  `runner.go:145,365`, `loopcmd/sessions.go:472`, `loopcmd/describe.go:82`.
- `runner.Invocation` is built in exactly one production place: `internal/loopcmd/tick.go:786`.
- `engine.effectiveHarness` — `internal/engine/engine.go:317`. Callers: `engine.go:243,295,309`.
- `store.KindTend` / `store.KindRebase` — `internal/store/types.go:9,14`.
- `worktree.Manager.gitStdout` — `internal/worktree/worktree.go:412`, bounded by `gitTimeout`.

## Task 1 — the `tend:` configuration section

**review: yes** (it changes the session-continuity invariant)

- [ ] Add `TendAgent` to `internal/config/config.go`:

  ```go
  // TendAgent is the agent section for a tend dispatch. Every field is
  // optional and falls back to the same field of Agent.
  type TendAgent struct {
      Harness string `yaml:"harness"`
      Model   string `yaml:"model"`
      Effort  string `yaml:"effort"`
  }
  ```

  Add `Tend TendAgent \`yaml:"tend"\`` to `Config`, directly under `Agent`.

- [ ] Explain in a comment above `TendAgent` why only three fields are here, and not
  `permission_mode`, `worktree`, `max_budget_usd`, `timeout`, or `background_tasks`: those say how
  this program runs an agent, not which agent runs, and a tend has no reason to differ in them.
  Repeating them would give an operator two places to set one thing.

- [ ] Validate in `Config.validate`, beside the existing `agent.harness` and `agent.effort`
  switches, using the same enums and the same message shape. `tend.model` is NOT required: an
  empty value means "use `agent.model`", and requiring it would force every tending loop to repeat
  a value it already set.

- [ ] `Load` must NOT default `cfg.Tend.Harness`. `Load` defaults `cfg.Agent.Harness` to
  `HarnessClaude` because a harness must always resolve; an empty `tend.harness` is the signal to
  fall back, so defaulting it here would silently pin every tend to claude on a `harness: pi` loop.
  Say that in a comment beside the existing default.

**Acceptance:** `go test ./internal/config/...` passes with new tests that (1) load a config with
a full `tend:` section, (2) load one with a partial `tend:` section, (3) reject
`tend.harness: bogus` with a message naming `tend.harness`, (4) reject `tend.effort: turbo` with a
message naming `tend.effort`, and (5) confirm `Load` leaves an absent `tend.harness` empty. Check
`internal/config/docs_test.go` and `examples_test.go` — if either asserts over the documented field
set, extend it rather than working around it.

## Task 2 — `runner.Effective` learns the dispatch kind

**review: no**

- [ ] Change the signature to `Effective(cfg *config.Config, kind string, ov config.Overrides)
  Settings`. `kind` is a `store.Kind*` value.

- [ ] Build the base settings from `cfg.Agent`, then overlay each non-empty `cfg.Tend` field when
  `kind == store.KindTend`, then overlay the validated label overrides exactly as today. Do not
  re-order the existing override re-validation; it is the last line of defence before a value
  becomes an argv element, and its doc comment says so.

- [ ] Document the three layers and their order in the doc comment, and state why the label wins:
  `tend:` is a default for a class of dispatch, and a label is an instruction about one issue.

- [ ] Add `Kind string` to `runner.Invocation`, with a comment saying it selects the
  configuration layer and that an empty value means "not a tend", which is the correct reading for
  a hand-built `Invocation` in a test.

- [ ] Update every caller:
  - `args.go:102,142` — pass `inv.Kind`.
  - `runner.go:145` — pass `inv.Kind`.
  - `runner.go:365` — pass `d.Kind`. This one records the harness that minted a session, and the
    branch it sits in already excludes `store.KindTend`, so the value is a non-tend kind either
    way; pass it for the same single-source reason the rest do.
  - `loopcmd/tick.go:786` — set `Kind: d.Kind` on the `runner.Invocation`, where `d` is the
    dispatch row.
  - `loopcmd/sessions.go:472` and `loopcmd/describe.go:82` — these render what a dispatch RAN
    with. `describe.go` has `d.Kind` on the row; pass it. `sessions.go` aggregates a session, so
    pass the kind of the dispatch the model and harness were read from; if the aggregate does not
    carry one, pass `store.KindResume` and say in a comment that a session never aggregates a
    tend-only run.

**Acceptance:** `go test ./internal/runner/... ./internal/loopcmd/...` passes with new tests
proving: `KindTend` prefers `tend.model` over `agent.model`; an absent `tend.model` falls back to
`agent.model`; `KindStart` ignores `tend:` entirely; a `model:` label beats both for `KindTend`.

## Task 3 — the engine resolves the harness per dispatch kind

**review: yes** (this is the session-continuity invariant)

- [ ] Change `engine.effectiveHarness(cfg *config.Config, kind string, ov config.Overrides)
  string`. Resolve in the same order `runner.Effective` does: label override, then `cfg.Tend.Harness`
  when `kind == store.KindTend`, then `cfg.Agent.Harness`, then `config.HarnessClaude`.

- [ ] Change `engine.resumable(cfg *config.Config, kind string, state store.IssueState, ov
  config.Overrides) bool` to pass the kind through.

- [ ] Update the three call sites. The trigger path (`engine.go:243,295`) passes `store.KindStart`;
  the retry path (`engine.go:309`) passes the kind it is about to produce; `tendDecisions` passes
  `store.KindTend`.

- [ ] Extend `resumable`'s doc comment with the tend case: a loop whose `tend.harness` differs from
  its `agent.harness` must START the tend's session, never resume the issue's. pi does not refuse an
  identifier it has never seen — it creates a fresh session under it and carries on — so a resume
  across harnesses loses the conversation silently. That is what this gate exists to stop, and the
  `tend:` section is a new way to reach it.

**Acceptance:** `go test ./internal/engine/...` passes with new tests proving: with
`agent.harness: claude` and `tend.harness: pi`, a tend decision for an issue with a started claude
session carries an EMPTY `SessionID`; with `tend.harness` empty, it still inherits the session;
a `harness:` label on the issue still beats `tend.harness`.

## Task 4 — read the latest review activity from GitHub

**review: no**

- [ ] Add to `ghub.Client`:

  ```go
  LatestReviewActivity(ctx context.Context, owner, repo string, number int) (time.Time, error)
  ```

- [ ] Implement it on `GitHubClient` in `internal/ghub/ghub.go`, beside `BehindBy`. Read the
  newest review comment with `ListComments` and `PullRequestListCommentsOptions{Sort: "created",
  Direction: "desc", ListOptions: github.ListOptions{PerPage: 1}}`. Read the newest review by
  walking `ListReviews` — it has no sort option, so ask for `PerPage: 100` and take the maximum
  `SubmittedAt` over the pages, stopping at `Response.NextPage == 0`. Return the later of the two,
  and the zero time when there is neither.

- [ ] Say in the doc comment why the two reads differ: `ListComments` accepts a sort, so one page
  of one is enough; `ListReviews` does not, so the whole list is walked. Say why a review with no
  `submitted_at` (a pending review, which only its author can see) is skipped rather than counted
  as now.

- [ ] Update every fake `ghub.Client` in the test tree so the package still compiles. Grep for the
  existing method set rather than guessing which files hold one.

**Acceptance:** `go test ./internal/ghub/...` passes with a table test over a stubbed HTTP
transport (follow `internal/ghub/single_test.go`) proving: comments only; reviews only; both, with
the later one winning; neither, returning the zero time; a review with no `submitted_at` ignored.

## Task 5 — read the last tend dispatch from the store

**review: no**

- [ ] Add `func (s *Store) LastTendAt(loop, repo string, prNumber int) (time.Time, error)` to
  `internal/store/store.go`, beside `RunningDispatches`. Select `MAX(started_at)` over `dispatches`
  where `project_id`, `loop`, `repo` and `pr_number` match and `kind = 'tend'`. Return the zero time
  when there is no row; use a `sql.NullTime` so no row and a NULL both read as zero.

- [ ] Say in the doc comment why it counts `kind = 'tend'` only: a `kind = 'rebase'` row records a
  rebase git performed with no conversation, so it read no review and answered no comment. Counting
  it would suppress the first tend after every automatic rebase, which is exactly the feedback the
  new trigger exists to answer.

- [ ] Add `func (s *Store) LastTendByPR(loop, repo string) (map[int]time.Time, error)` for the
  passes that decide many issues at once, so a sweep does not issue one query per pull request.
  Return a map keyed by `pr_number`.

**Acceptance:** `go test ./internal/store/...` passes with tests proving: a `tend` row is
returned; a `rebase` row is ignored; another project's row is invisible; no row reads as the zero
time; `LastTendByPR` agrees with `LastTendAt` for every key.

## Task 6 — review activity becomes a tend trigger

**review: yes** (it adds a reason to spend money)

- [ ] Add `ReviewedAt map[int]time.Time` to `engine.Snapshot`, `LastTend map[int]time.Time` to
  `engine.State`, and `ReviewPending bool` to `engine.Decision`. Document each: the first two are
  keyed by PULL REQUEST number, not issue number, because review activity and a tend dispatch are
  both facts about a pull request.

- [ ] In `tendDecisions`, replace the `snap.BehindBy[pr.Number] <= 0` skip with the two-reason
  gate. A decision is produced when the pull request is behind, or when
  `snap.ReviewedAt[pr.Number].After(st.LastTend[pr.Number])`. Set `ReviewPending` on the decision
  when the second holds. Extend `Decision.Reason` so the log says which reason fired; keep the
  existing "N commits behind" wording for the first.

- [ ] Replace the skip reason with one that names both halves, so an operator reading "nothing
  happened" learns which of the two questions came back no.

- [ ] Fill the two maps in all three callers. Each one reads review activity only for a pull
  request it has already accepted as a tend candidate — the issue carries `labels.review` and
  `engine.LinkPR` trusted the pull request — so a pass with no candidates costs no extra call:
  - `internal/loopcmd/tickissue.go` — beside the existing `BehindBy` call, using `LastTendAt`.
  - `internal/loopcmd/tendsweep.go` (`tendSnapshot`) — beside the existing `BehindBy` call, using
    `LastTendByPR` read once. A failed read logs and skips that pull request, exactly as a failed
    compare does today; one unusable pull request must not abandon the pass.
  - `internal/loopcmd/tick.go` — the full tick's snapshot, the same way.

- [ ] Do NOT touch `internal/loopcmd/tendcheck.go`. Add a short comment there saying why: its
  contract is zero GitHub calls when nothing is behind, review activity is invisible to a local
  checkout, and a review already produces a `pull_request_review` delivery that reaches
  `tickIssue`. The delivery path is this trigger's fast path and cron's full `Tick` is its safety
  net.

- [ ] In `loopcmd.act`, make `doneRebased` fall through to the dispatch when
  `d.ReviewPending` is set. Count the rebase first, so the summary still reports it. Leave
  `doneNoRebase` returning without an agent, and say in a comment that the branch that pass
  reasoned about is gone, so its premise is stale whatever the feedback says.

**Acceptance:** `go test ./internal/engine/... ./internal/loopcmd/...` passes with tests proving:
a current pull request with review activity newer than the last tend produces a `KindTend`
decision with `ReviewPending` true; the same pull request with the last tend NEWER than the
activity produces no decision and the new skip reason; a behind pull request with no review
activity produces a decision with `ReviewPending` false; a clean rebase on a `ReviewPending`
decision counts the rebase AND dispatches the agent.

## Task 7 — read the conflicted paths from git

**review: no**

- [ ] Add `func (m *Manager) ConflictedPaths(ctx context.Context, path string) ([]string, error)`
  to `internal/worktree/worktree.go`, beside `AbortRebase`. Run
  `git diff --name-only --diff-filter=U` through `gitStdout`, split the output on newlines, drop
  empty lines, and sort the result.

- [ ] Sort inside this function, not at the call site. The fingerprint is a hash, so an unstable
  order would make one conflict look like a different conflict on the next pass and defeat the
  backoff completely. Say that in the doc comment.

- [ ] Return an empty slice and no error on a clean worktree. A rebase can fail for reasons that
  leave no conflicted path — a dead context, a bad ref — and the caller must be able to tell that
  from a real conflict.

**Acceptance:** `go test ./internal/worktree/...` passes with tests, built on the existing
`initRepo` helper (`internal/worktree/worktree_test.go:10`), proving: a real conflicted rebase
reports its conflicted files, sorted; a clean worktree reports an empty slice.

## Task 8 — the conflict table

**review: yes** (new schema)

- [ ] Add the `tend_conflicts` table to `schemaTables` in `internal/store/store.go`, in the shape
  the spec gives, with comments in the style of the tables around it. State on `retry_after` why it
  is Unix seconds in an INTEGER rather than a `TIMESTAMP`, citing the same reason
  `issues.retry_after` gives. State on the primary key why there is one row per pull request rather
  than one per fingerprint.

- [ ] `tend_conflicts` is a NEW table, so it needs no `addedColumns` entry and no rebuild.
  `CREATE TABLE IF NOT EXISTS` creates it on the next open of any existing database.

- [ ] Add `store.TendConflict` to `internal/store/types.go`, and three methods:
  - `TendConflict(loop, repo string, prNumber int) (TendConflict, bool, error)`
  - `RecordTendConflict(c TendConflict) error` — upsert. When the stored fingerprint differs, the
    row is REPLACED with `seen_count = 1`; when it matches, `seen_count` is incremented and
    `last_seen_at` and `retry_after` are updated. Spend the count in SQL, not by reading it back
    and writing it, for the reason `BeginDispatch` gives: another process can write between the
    read and the write.
  - `DeleteTendConflict(loop, repo string, prNumber int) error` — deleting a row that is not there
    is not an error.

- [ ] Call `DeleteTendConflict` where `TendCheck` deletes a `pr_links` row for a pull request that
  is no longer open (`internal/loopcmd/tendcheck.go`), so the two rows about one dead pull request
  disappear together.

**Acceptance:** `go test ./internal/store/...` passes with tests proving: a first record writes
`seen_count = 1`; a second record of the SAME fingerprint writes `seen_count = 2` and keeps
`first_seen_at`; a record of a DIFFERENT fingerprint resets `seen_count` to 1 and moves
`first_seen_at`; delete is idempotent; another project cannot see the row.

## Task 9 — the repeat-conflict backoff

**review: yes** (it decides not to act)

- [ ] Add `doneBackedOff` to the `rebaseOutcome` enum in `internal/loopcmd/rebase.go`, and extend
  the enum's comment: it now names four outcomes, three of which mean "dispatch no agent".

- [ ] Add the fingerprint function to `internal/loopcmd/rebase.go`:

  ```go
  func conflictFingerprint(headSHA string, paths []string) string
  ```

  SHA-256 over `headSHA` and the sorted paths, joined with `"\x00"` — a byte a path cannot hold.
  Return the hex digest. Document why the BASE commit is excluded: a tend sweep is armed by the
  base moving, so a fingerprint carrying the base would be new on every sweep and would suppress
  nothing. Name finding 5 — one pull request, the same `CLAUDE.md` conflict, four sweeps, five
  hours — as the case that requires it.

- [ ] Add the backoff schedule as a package constant beside `maxTendPerSweep`, with the same
  reasoning that comment gives for being a constant rather than a configuration field:

  ```go
  var conflictBackoff = []time.Duration{0, time.Hour, 6 * time.Hour, 24 * time.Hour}
  ```

  Index by `seen_count`, clamped to the last entry. Say that the first sighting always dispatches,
  because the agent is the right answer to a conflict it has not seen, and the backoff begins only
  once the agent has already failed at this exact conflict.

- [ ] In `gitRebase`, on the conflict branch and BEFORE the abort — the abort clears the conflicted
  paths — read `deps.Git.ConflictedPaths`, build the fingerprint from it and the lease, and record
  it. Then decide:
  - No paths, or the read failed: log and fall through to `notDone`. A rebase that failed with no
    conflicted path is not a conflict this gate understands, and refusing to dispatch on it would
    be a silent stall.
  - The recorded row's fingerprint matches and `now` is before its `retry_after`: return
    `doneBackedOff`. Log one line naming the loop, the issue, the pull request, the sighting count,
    the deadline, and the conflicted paths.
  - Otherwise: return `notDone` so the agent runs.

- [ ] Extend `RebaseGit` with `ConflictedPaths`. It is an interface so a test can drive this branch
  without a remote; keep that property.

- [ ] Delete the conflict row on the success path of `gitRebase`, beside `recordRebase`. A branch
  that replayed cleanly has no conflict left to remember. A failed delete is logged, not returned:
  the rebase HAPPENED, and reporting it as undone would send an agent at an already-current branch.

- [ ] Handle `doneBackedOff` in `loopcmd.act`: return with no agent and no count, like
  `doneNoRebase`. Add a `Backoff` counter to `Summary` so the tick summary an operator audits says
  a pull request was skipped rather than silently reporting nothing.

**Acceptance:** `go test ./internal/loopcmd/...` passes with tests proving: a first conflict
dispatches the agent and writes a row; the same fingerprint within the window dispatches nothing
and increments nothing beyond the recorded count; the same fingerprint after `retry_after`
dispatches; a moved head produces a new fingerprint and dispatches; a clean rebase deletes the
row; a rebase failure with no conflicted paths dispatches.

## Task 10 — documentation

**review: no**

- [ ] `docs/configuration.md`: add a `tend:` section after `agent`, with its own `## tend`
  heading, its three fields, the three-layer precedence, and the session-continuity warning. Add
  the three fields to the Quick reference table and `tend` to the Contents list.

- [ ] `docs/configuration.md`, `## tend_pr`: replace "Version 1 rebases only. It does not reply to
  review feedback." with the two tend triggers — behind its base, or review activity newer than the
  last tend dispatch — and add the repeat-conflict backoff and its schedule.

- [ ] `README.md`, `## Configuration`: one paragraph naming `tend:` and linking to the new
  reference section. `README.md`, the `tend_interval` subsection: one sentence saying the periodic
  check still gates on staleness alone, and that review activity reaches the loop through its own
  delivery.

- [ ] `examples/execution.yaml`: add a commented-out `tend:` block showing a cheaper model, so an
  operator can see the shape without reading the reference. Do NOT enable it — changing an example
  loop's behaviour is not this change's business.

**Acceptance:** `go test ./internal/config/...` passes — `docs_test.go` checks the documentation
against the config struct, so an undocumented field fails there. Every anchor link resolves.

## Task 11 — gates

**review: no**

- [ ] Run `make check` (`fmtcheck`, `vet` for both `GOOS`, `lint`, and the full `test` suite) and
  fix what it reports.

**Acceptance:** `make check` exits zero.
