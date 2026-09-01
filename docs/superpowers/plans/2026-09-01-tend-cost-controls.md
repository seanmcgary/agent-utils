# Tend cost controls — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop a tend dispatch that has nothing to do, stop a tend dispatch that repeats a
conflict it already lost, and let the tend dispatches that remain run a cheaper agent.

**Architecture:** Four changes on one path, in dependency order. A new `tend:` configuration
section gives a tend dispatch its own harness, model, and effort, and `runner.Effective` and
`engine.effectiveHarness` both learn the dispatch kind so the session-continuity rule still
holds. A new GitHub read reports the latest review activity on a pull request by somebody other
than the loop itself, a new store read reports the last finished tend dispatch for it, and
`engine.tendDecisions` treats "review activity is newer" as a second reason to tend. A new
`tend_conflicts` table fingerprints a rebase conflict, and `loopcmd` refuses to dispatch the agent
at a fingerprint that already defeated it.

**Tech Stack:** Go 1.25, `urfave/cli/v3`, `gopkg.in/yaml.v3`, `go-github/v77`, SQLite through
`internal/store`, git through `os/exec`.

**Spec:** `docs/superpowers/specs/2026-09-01-tend-cost-controls-design.md`

## Pipeline State

| Field   | Value |
|---------|-------|
| stage   | 5 (pr feedback loop) |
| class   | large (new config section, new table, new GitHub read, new gate before a dispatch) |
| profile | backend |
| branch  | feat/tend-cost-controls |
| pr      | #25 |
| gate    | approved 2026-09-01 |
| round   | 1 |
| decisions | 5 (see Decisions below) |

### Decisions

Every entry is rendered in the pull request body under "Deviations from the approved plan".

1. **Task 6 — `ReviewPending` transport.** The plan put it on `pr_links`. It travels on the
   DISPATCH row instead. Every `PutPRLink` call site runs before `engine.Decide` produces the
   value, so none could set it, and `PutPRLink`'s upsert rewrites every column, so the tend sweep
   would overwrite a set flag with a zero before the runner read it.
2. **Task 6 — where the review read happens.** The plan named `tickissue.go` and `tick.go`; an
   earlier draft also named `tendsweep.go`. The sweep is excluded, because `TendSweep`'s doc
   comment requires every trigger arming it to name one subject, the default branch moving.
   Review activity reaches the loop through its own delivery instead.
3. **Not in the plan — `maxTendPerSweep` now applies to the full `Tick`.** The review trigger
   removes the bound the staleness check provided, and on the first tick after install every
   review pull request qualifies at once because none has a finished tend row yet.
4. **Task 5 — the comparison point.** The plan compared review activity against the last tend's
   START time. It compares against the FINISH time: the agent's own reply is written during its
   own dispatch, and the identity filter cannot catch it because the agent runs with
   GITHUB_TOKEN stripped and authenticates as the machine's gh login.
5. **Not in the plan — `project logs` resolves the harness from the dispatch.** Out of scope, but
   this change is what makes the mismatch reachable from configuration alone.

## What is already done

The request asks for four changes. Pull request #18 (`7184c75`) landed the clean-rebase fast
path after the request's log sample was taken.

- **(c) is complete.** `loopcmd.gitRebase` (`internal/loopcmd/rebase.go:118`) rebases with git,
  pushes with `--force-with-lease` pinned to the commit it fetched, and dispatches the agent only
  on a conflict. This plan changes it only to add the backoff outcome and the review-pending
  fall-through.
- **(b) is half complete.** `engine.tendDecisions` already refuses to decide a pull request that
  is not behind its base (`internal/engine/engine.go:438`). This plan adds the review-activity
  half.

## Global Constraints

The repository has no `AGENTS.md`, `CLAUDE.md`, or `CONTRIBUTING.md`. The binding rules for this
change come from the code, from `.golangci.yml`, and from `README.md`:

- **Session continuity per harness.** A session belongs to the harness that minted it. Enforced by
  `engine.resumable` (`internal/engine/engine.go:354`) and documented in
  `docs/configuration.md#tend_pr`. Changing the harness must START a session, never resume one.
- **Every `_test.go` file is in the SAME package as the code it tests.** Write unqualified names.
- **Tests are not run in parallel across packages.** `make test` passes `-p 1` and `-count=1`
  (`Makefile:148-160`). Do not add `t.Parallel()` to a test that shells out to git or spawns a
  process.
- **A new NOT NULL column carries a literal DEFAULT** (`internal/store/store.go:30`). A
  wall-clock deadline is stored as Unix seconds in an INTEGER, because no literal `TIMESTAMP`
  default reads back as the zero time (`internal/store/store.go:53`).
- **A new COLUMN added after the first release must also be listed in `addedColumns`**
  (`internal/store/store.go:365`). A new TABLE must not: `CREATE TABLE IF NOT EXISTS` creates it,
  and it has no older rows.
- **Every scoped read and write carries `project_id`.** `internal/store/scope_test.go` proves it.
- **Map iteration is never a log or an error order.** Sort the keys; `internal/config.Load` and
  `loopcmd.TendCheck` both record the rule.
- **`--force-with-lease`, never plain `--force`.** Enforced by `worktree.Manager.PushWithLease`.
- **Rebase a feature branch onto its base; never merge the base into it.** Repository owner's
  standing rule.
- **Commit messages are Conventional Commits**: `type(scope): subject`, lowercase, imperative
  (`feat(sessions):`, `fix(listener):`, `docs(worktree,loopcmd):`, `test(loopcmd):`, `chore:`).
- **No `Co-Authored-By:` trailer on any commit.** Repository owner's standing rule; it overrides
  any skill template.
- **Do not bump `VERSION`.** It is bumped in standalone `chore: bump to vX.Y.0` commits on master,
  separately from a feature branch (`11a0224`, `23197f0`, `c08ebe6`).
- **Lint.** `.golangci.yml` enables `errcheck`, `errorlint`, `govet`, `ineffassign`,
  `staticcheck`, `unused`. Two consequences for this change: an error that is deliberately ignored
  must be handled explicitly (`if err := ...; err != nil { slog.Error(...) }`), never `_ =`,
  except for the `Close`/`Release` exclusions already listed; and a sentinel error is compared
  with `errors.Is`, never `==` (this includes `sql.ErrNoRows`).
- **Nothing logs at Debug.** No handler is configured for it, so a Debug line is a line nobody
  reads (`internal/loopcmd/tendcheck.go:172-174`).
- **Externally-influenced text is bounded before it is logged.** See
  `internal/listener/handler.go:69,76` (`maxLoggedTextRunes`, `maxLoggedLabels`) and
  `internal/loopcmd/rebase.go:211-215` (`truncate(d.HeadRef, 120)`, with the comment saying
  `SafeRef` bounds a ref's charset but not its length).
- **Comment idiom.** This codebase writes: the invariant, the alternative that was rejected, and
  the concrete failure that motivated the rule. Read the neighbouring file before writing a
  comment. Every step below that says "say why" means a comment of that shape.

## Verified external API (do not re-derive)

Read from `github.com/google/go-github/v77@v77.0.0` in the module cache.

```go
// github/pulls_reviews.go:110
func (s *PullRequestsService) ListReviews(ctx context.Context, owner, repo string, number int,
    opts *ListOptions) ([]*PullRequestReview, *Response, error)

// github/pulls_comments.go:80
func (s *PullRequestsService) ListComments(ctx context.Context, owner, repo string, number int,
    opts *PullRequestListCommentsOptions) ([]*PullRequestComment, *Response, error)

// github/users.go:95 — an empty user name returns the AUTHENTICATED user.
func (s *UsersService) Get(ctx context.Context, user string) (*User, *Response, error)
```

- `PullRequestReview` carries `SubmittedAt *Timestamp` (`pulls_reviews.go:22`),
  `AuthorAssociation *string` (`:33`), and `User *User`.
- `PullRequestComment` carries `CreatedAt *Timestamp` (`pulls_comments.go:36`),
  `AuthorAssociation *string` (`:44`), and `User *User` (`:34`).
- Read a `*Timestamp` with `GetSubmittedAt().Time` / `GetCreatedAt().Time`; read a login with
  `GetUser().GetLogin()`.
- `PullRequestListCommentsOptions` carries `Sort` (`"created"`/`"updated"`), `Direction`
  (`"asc"`/`"desc"`), and an embedded `ListOptions`. `ListReviews` takes a bare `ListOptions` and
  has NO sort option: its results are oldest first.

Repository-internal facts this plan depends on. Every line number below was re-verified against
the current tree.

- `runner.Effective` — `internal/runner/args.go:59`. Production callers: `args.go:102,142`,
  `runner.go:145,365`, `loopcmd/sessions.go:472`, `loopcmd/describe.go:82`. Test callers:
  `internal/runner/args_test.go:165,179,190,201,208,224,235`.
- `runner.Invocation` is built in exactly one production place: `internal/loopcmd/tick.go:786`.
- `engine.resumable` — `internal/engine/engine.go:354`. Callers: `engine.go:201,317,468`.
- `engine.effectiveHarness` — `internal/engine/engine.go:368`. Direct callers besides
  `resumable` (`:361`): `engine.go:220,331`.
- The tend staleness gate is `engine.go:438`; the tend session gate is `engine.go:468`.
- `store.KindTend` / `store.KindRebase` — `internal/store/types.go:9,14`.
- `worktree.Manager.gitStdout` — `internal/worktree/worktree.go:412`, bounded by `gitTimeout`.
- `loopcmd.gitRebase` — `internal/loopcmd/rebase.go:118`; `RebaseGit` — `rebase.go:19`; the
  detached `cleanupCtx` — `rebase.go:182`; `recordRebase` — `rebase.go:245`.
- `loopcmd.Summary` — `internal/loopcmd/tick.go:124`. Every field carries an explicit
  snake_case `json:` tag, because `Summary` is marshalled into `ticks.summary_json`.
- `loopcmd.Session.LastKind` — `internal/loopcmd/sessions.go:53`, set at `:539` from the newest
  dispatch of the session.
- `ghub.DeliveryCache` (`internal/ghub/deliverycache.go`) is a PRODUCTION implementation of
  `ghub.Client` that forwards every method by hand. `BehindBy` is passed through (`:130-136`);
  `PullRequest` is memoised (`:100-115`). The daemon passes it to `loopcmd`
  (`internal/listener/work.go:210,353`).
- Test fixtures to reuse rather than reinvent: `initRepo`
  (`internal/worktree/worktree_test.go:39`) and `initRepoWithOrigin` (`:45`); `openTemp`
  (`internal/store/store_test.go:15`), `openTempAt` (`:20`), `openTempDB`
  (`internal/store/closures_test.go:11`); `loopFile` (`internal/loopcmd/epicsweep_test.go:135`)
  and `writeLoopFiles` (`:168`).

## Task 1 — the `tend:` configuration section

**review: yes** (it changes the session-continuity invariant)

- [ ] Add `TendAgent` to `internal/config/config.go`:

  ```go
  type TendAgent struct {
      Harness string `yaml:"harness"`
      Model   string `yaml:"model"`
      Effort  string `yaml:"effort"`
  }
  ```

  Add `Tend TendAgent \`yaml:"tend"\`` to `Config`, directly under `Agent`.

- [ ] Comment above `TendAgent` in the house idiom. The invariant: only the three fields that say
  WHICH agent runs are here. The rejected alternative: repeating `permission_mode`, `worktree`,
  `max_budget_usd`, `timeout`, and `background_tasks`. The failure that rejects it: two places to
  set one thing, and an operator who sets a timeout in one of them gets the other's.

- [ ] Validate in `Config.validate`, beside the existing `agent.harness` and `agent.effort`
  switches, using the same enums and the same message shape (`tend.harness must be "claude" or
  "pi", got %q`; `tend.effort %q is not a valid effort level`). `tend.model` is NOT required: an
  empty value means "use `agent.model`", and requiring it would force every tending loop to repeat
  a value it already set.

- [ ] `Load` must NOT default `cfg.Tend.Harness`. `Load` defaults `cfg.Agent.Harness` to
  `HarnessClaude` (`internal/config/config.go:165-166`) because a harness must always resolve; an
  empty `tend.harness` is the signal to fall back, so defaulting it here would silently pin every
  tend to claude on a `harness: pi` loop. Say that in a comment beside the existing default.

**Acceptance:** `go test ./internal/config/...` passes with new tests that (1) load a config with
a full `tend:` section, (2) load one with a partial `tend:` section, (3) reject
`tend.harness: bogus` with a message naming `tend.harness`, (4) reject `tend.effort: turbo` with a
message naming `tend.effort`, and (5) confirm `Load` leaves an absent `tend.harness` empty.
`TestEveryConfigFieldIsDocumented` (`internal/config/docs_test.go:14-53`) walks nested structs, so
it will fail until Task 10 lands — that is expected; do not work around it.

## Task 2 — `runner.Effective` learns the dispatch kind

**review: no**

- [ ] Change the signature to `Effective(cfg *config.Config, kind string, ov config.Overrides)
  Settings`. `kind` is a `store.Kind*` value. Add `internal/store` to `args.go`'s OWN import
  block. Go imports are per file, and `args.go` imports only `bytes`, `fmt`, `strconv`,
  `text/template`, and `internal/config` today (`args.go:1-11`); the import at `runner.go:22`
  does not reach it.

- [ ] Build the base settings from `cfg.Agent`, then overlay each non-empty `cfg.Tend` field when
  `kind == store.KindTend`, then overlay the validated label overrides exactly as today. Do not
  re-order the existing override re-validation; it is the last line of defence before a value
  becomes an argv element, and its doc comment says so.

- [ ] Document the three layers and their order, and say why the label wins: `tend:` is a default
  for a class of dispatch, and a label is an instruction about one issue.

- [ ] Add `Kind string` to `runner.Invocation`, with a comment saying it selects the configuration
  layer and that an empty value means "not a tend", which is the correct reading for a hand-built
  `Invocation` in a test.

- [ ] Update every caller:
  - `args.go:102,142` and `runner.go:145` — pass `inv.Kind`.
  - `runner.go:365` — pass `d.Kind`.
  - `loopcmd/tick.go:786` — set `Kind: d.Kind` on the `runner.Invocation`, where `d` is the
    dispatch row.
  - `loopcmd/describe.go:82` — pass `d.Kind` from the dispatch row.
  - `loopcmd/sessions.go:472` — pass `sessions[i].LastKind` (`sessions.go:53`, set at `:539`). A
    tend that inherits the issue's session shares its `session_id`, so `sessionsFrom` DOES group a
    tend into a session, and `describe.go:35-37` describes exactly that case. Hard-coding a
    non-tend kind here would make `sessions list` report `agent.model` for a session whose last
    run used `tend.model`.
  - `internal/runner/args_test.go:165,179,190,201,208,224,235` — the package does not compile
    until these are updated.

**Acceptance:** `go test ./internal/runner/... ./internal/loopcmd/...` passes with new tests
proving: `KindTend` prefers `tend.model` over `agent.model`; an absent `tend.model` falls back to
`agent.model`; `KindStart` ignores `tend:` entirely; a `model:` label beats both for `KindTend`.

## Task 3 — the engine resolves the harness per dispatch kind

**review: yes** (this is the session-continuity invariant)

- [ ] Change `engine.effectiveHarness(cfg *config.Config, kind string, ov config.Overrides)
  string`. Resolve in the same order `runner.Effective` does: label override, then
  `cfg.Tend.Harness` when `kind == store.KindTend`, then `cfg.Agent.Harness`, then
  `config.HarnessClaude`.

- [ ] Change `engine.resumable(cfg *config.Config, kind string, state store.IssueState, ov
  config.Overrides) bool` to pass the kind through.

- [ ] Update all five call sites. `kind` is a `store.Kind*` STRING, not an `engine.Kind`; the two
  are different types and there is no `store` counterpart for `KindRetryStart` or
  `KindRetryResume` — `act` maps both onto `store.KindStart`/`store.KindResume`
  (`internal/loopcmd/tick.go:498-505`). Only `store.KindTend` changes behaviour, so:
  - `engine.go:201` (`resumable`, trigger path) — `store.KindStart`.
  - `engine.go:220` (`effectiveHarness`, the trigger path's "why" string) — `store.KindStart`.
  - `engine.go:317` (`resumable`, retry path) — `store.KindStart`.
  - `engine.go:331` (`effectiveHarness`, the retry path's "why" string) — `store.KindStart`.
  - `engine.go:468` (`resumable`, `tendDecisions`) — `store.KindTend`.

- [ ] Extend `resumable`'s doc comment with the tend case: a loop whose `tend.harness` differs from
  its `agent.harness` must START the tend's session, never resume the issue's. pi does not refuse
  an identifier it has never seen — it creates a fresh session under it and carries on — so a
  resume across harnesses loses the conversation silently.

**Acceptance:** `go test ./internal/engine/...` passes with new tests proving: with
`agent.harness: claude` and `tend.harness: pi`, a tend decision for an issue with a started claude
session carries an EMPTY `SessionID`; with `tend.harness` empty, it still inherits the session;
a `harness:` label on the issue still beats `tend.harness`.

## Task 4 — read the latest review activity from GitHub

**review: yes** (it decides whether to spend money, from attacker-writable input)

- [ ] Add two methods to `ghub.Client`:

  ```go
  // AuthenticatedLogin returns the login of the account this client's token
  // belongs to.
  AuthenticatedLogin(ctx context.Context) (string, error)

  // LatestReviewActivity returns the time of the most recent review or review
  // comment on a pull request that this loop did not write itself.
  LatestReviewActivity(ctx context.Context, owner, repo string, number int) (time.Time, error)
  ```

- [ ] Implement both on `GitHubClient` in `internal/ghub/ghub.go`, beside `BehindBy`.
  `AuthenticatedLogin` calls `Users.Get(ctx, "")` and MEMOISES the answer on the client behind a
  `sync.Once`-style guard: the token does not change while the process runs, and a call per tend
  candidate would spend a request to learn a constant.

- [ ] `LatestReviewActivity` applies two filters, and BOTH are load-bearing:

  1. **Skip activity written by this loop itself** — an author login equal to
     `AuthenticatedLogin`. Without it the feature is a money loop: the tend prompt tells the agent
     to comment (`examples/execution.yaml:118,124`), the agent's comment is newer than the
     dispatch that produced it, and every later pass sees pending review activity and dispatches
     again, forever, at about $0.75 a turn.
  2. **Skip an author whose `AuthorAssociation` is not `OWNER`, `MEMBER`, or `COLLABORATOR`** —
     the same three values `convertPR` requires before it will trust a pull request
     (`internal/ghub/ghub.go:113`). A review comment can be written by anyone with read access, so
     without this filter a stranger can spend the loop's budget at will and put chosen text in
     front of an agent that holds push rights on the branch.

  Say both, and the failure each prevents, in the doc comment.

- [ ] Read the newest review comment with `ListComments` and
  `PullRequestListCommentsOptions{Sort: "created", Direction: "desc", ListOptions:
  github.ListOptions{PerPage: 100}}`. Read reviews by walking `ListReviews` at `PerPage: 100` and
  taking the maximum `SubmittedAt` over the pages. Cap the walk at a package constant
  (`maxReviewPages = 10`) and stop there, treating what was seen as the answer: any user who can
  review can post thousands of reviews, and an unbounded walk holds the loop lock while it
  exhausts the daemon's rate limit for every project on the machine. Say why the two reads differ
  — `ListComments` accepts a sort, `ListReviews` does not.

  A page of comments is read rather than a single row because the newest comment may be one this
  loop wrote; the filters above then have candidates left to consider.

- [ ] Skip a review with no `SubmittedAt` — a pending review, which only its author can see —
  rather than counting it as now. Say so.

- [ ] Add both methods to `ghub.DeliveryCache` (`internal/ghub/deliverycache.go`). It is a
  PRODUCTION implementation, not a test fake, and the daemon build breaks without it
  (`internal/listener/work.go:210,353`). MEMOISE both, the way `PullRequest` is memoised at
  `deliverycache.go:100-115` and unlike `BehindBy`'s pass-through at `:130-136`: several loops of
  several projects answer one delivery, the answer is the same instant for all of them, and the
  cache's lifetime is one delivery. State that reasoning in the comment, as
  `deliverycache.go:14-48` requires.

- [ ] Update every fake `ghub.Client` in the test tree. Grep for an existing method name rather
  than guessing which files hold one.

**Acceptance:** `go test ./internal/ghub/... ./internal/listener/...` passes with a table test over
a stubbed HTTP transport (follow `internal/ghub/single_test.go`) proving: comments only; reviews
only; both, with the later one winning; neither, returning the zero time; a review with no
`submitted_at` ignored; activity by the authenticated login ignored; activity by a
`CONTRIBUTOR`/`NONE` author ignored; the review walk stops at the page cap.

## Task 5 — read the last tend dispatch from the store

**review: no**

- [ ] Add to `internal/store/store.go`, beside `RunningDispatches`:

  ```go
  func (s *Store) LastTendAt(loop, repo string, prNumber int) (time.Time, error)
  func (s *Store) LastTendByPR(loop, repo string) (map[int]time.Time, error)
  ```

  Both select `MAX(started_at)` over `dispatches` where `project_id`, `loop`, `repo` match,
  `kind = 'tend'`, and `finished_at IS NOT NULL`. `LastTendByPR` groups by `pr_number` so a pass
  deciding many issues issues one query, not one per pull request. Use `sql.NullTime` so no row
  and a NULL both read as the zero time; compare any sentinel with `errors.Is`, never `==`
  (`errorlint` is enabled).

- [ ] Document three choices in the house idiom:
  - **`kind = 'tend'` only.** A `kind = 'rebase'` row records a rebase git performed with no
    conversation, so it read no review and answered no comment. Counting it would suppress the
    first tend after every automatic rebase, which is the feedback the new trigger exists to
    answer.
  - **`finished_at IS NOT NULL`.** A running tend is excluded because `dispatch` writes the row
    before the agent starts (`internal/loopcmd/tick.go:562`); `engine.Decide`'s `liveTendPRs`
    already suppresses a second pass while one runs (`engine.go:26-30,434`), so counting a running
    row here would be a second, weaker copy of that guard.
  - **A FAILED tend still counts.** The alternative — counting only a succeeded tend, so a crashed
    agent gets another turn at the same feedback — was rejected: `runner.finish` deliberately
    writes no retry state for a tend (`internal/runner/runner.go:353`), so nothing bounds how many
    times a persistently failing tend is redispatched, and unbounded unattended spend is the
    failure this whole change exists to remove. The cost is that feedback which met a crashed
    agent waits for the next review comment. The dispatch row records the failure, and
    `project logs --list` shows it.

- [ ] Note in the doc comment that `dispatches` is indexed only on
  `(project_id, loop, repo, status)` (`internal/store/store.go:199`), so both reads scan. That is
  accepted at current volumes; add an index when a loop's dispatch history makes it matter.

**Acceptance:** `go test ./internal/store/...` passes with tests proving: a finished `tend` row is
returned; a `rebase` row is ignored; a running `tend` row (`finished_at` NULL) is ignored; a
failed but finished `tend` row IS returned; another project's row is invisible to BOTH methods
(assert `project_id` scoping explicitly for `LastTendByPR`, not only that it agrees with
`LastTendAt`); no row reads as the zero time.

## Task 6 — review activity becomes a tend trigger

**review: yes** (it adds a reason to spend money)

- [ ] Add `ReviewedAt map[int]time.Time` to `engine.Snapshot`, `LastTend map[int]time.Time` to
  `engine.State`, and `ReviewPending bool` to `engine.Decision`. Document that the two maps are
  keyed by PULL REQUEST number, not issue number, because review activity and a tend dispatch are
  both facts about a pull request.

- [ ] Give `tendDecisions` the new map. It does NOT receive `engine.State`; it takes
  `states map[int]store.IssueState` and its one caller (`engine.go:266`) passes `st.Issues`
  (`engine.go:405-413`). Add a `lastTend map[int]time.Time` parameter beside `states` and pass
  `st.LastTend` at the call site. Without this the new field on `State` is not reachable from the
  function that reads it, and the package does not compile.

- [ ] In `tendDecisions`, replace the `snap.BehindBy[pr.Number] <= 0` skip (`engine.go:438`) with a
  two-reason gate. Produce a decision when the pull request is behind, or when
  `snap.ReviewedAt[pr.Number].After(lastTend[pr.Number])`. Set `ReviewPending` when the second
  holds. Extend `Decision.Reason` so the log says which reason fired; keep the existing
  "N commits behind" wording for the first.

- [ ] Replace the skip reason with one naming both halves, so an operator reading "nothing
  happened" learns which of the two questions came back no.

- [ ] Fill the two maps in TWO callers only:
  - `internal/loopcmd/tickissue.go` — beside the existing `BehindBy` call, using `LastTendAt`.
  - `internal/loopcmd/tick.go` — the full tick's snapshot, using `LastTendByPR` read once.

  In both, ask GitHub only for a pull request already accepted as a tend candidate: the issue
  carries `labels.review` and `engine.LinkPR` trusted the pull request. A pass with no candidates
  costs nothing.

- [ ] Do NOT add the review read to `internal/loopcmd/tendsweep.go`, and say why in a comment
  beside `TendSweep`'s existing property list (`tendsweep.go:50-58`). That list states the sweep's
  first invariant: everything arming it names one subject, the loop's default branch moving, and
  a fourth trigger is acceptable only while it keeps that property. Review activity is not that
  subject, so a merge to master must not dispatch agents at pull requests that are current and
  merely carry comments. Review activity reaches the loop by its own route — a
  `pull_request_review` or `pull_request_review_comment` delivery lands in `tickIssue` — and cron's
  full `Tick` is its safety net. Keeping it out of the sweep also keeps the sweep's GitHub cost
  where it is.

- [ ] Do NOT add any GitHub call to `internal/loopcmd/tendcheck.go`. Add a comment there saying
  why: its contract is zero GitHub calls when nothing is behind, and review activity is invisible
  to a local checkout. (Task 8 does add a store DELETE to that file; that is not a GitHub call and
  does not break the contract.)

- [ ] **Error direction, stated per call site.** A trigger that spends money fails CLOSED. A failed
  `LatestReviewActivity` or `LastTendAt`/`LastTendByPR` logs and leaves the entry unset, so the
  pull request is judged on staleness alone. Do not abandon the pass and do not proceed with an
  empty map treated as "everything is pending" — that would answer one failed read with a burst of
  dispatches, the opposite of the goal. In `tickissue.go` keep the pull request in the snapshot
  with the entry unset, matching how a failed `BehindBy` behaves there (`tickissue.go:83-87`); in
  `tick.go` do the same.

- [ ] Carry `ReviewPending` to the detached runner, which never sees the tick's snapshot, on the
  DISPATCH row — not on `pr_links`. Add `review_pending INTEGER NOT NULL DEFAULT 0` to
  `dispatches` in `schemaTables`, add `{"dispatches", "review_pending", "INTEGER NOT NULL DEFAULT
  0"}` to `addedColumns` (`internal/store/store.go:365`), add `ReviewPending bool` to
  `store.Dispatch`, and set it in `loopcmd.dispatch` from `d.ReviewPending` beside `Model`,
  `Harness`, and `Effort` (`internal/loopcmd/tick.go:562-571`).

  `pr_links` was considered and REJECTED for two concrete failures. First, no `PutPRLink` call
  site could set it: all three run BEFORE `engine.Decide`, so no `Decision.ReviewPending` exists
  yet at any of them (`tickissue.go:91`, `tick.go:206`, `tendsweep.go:189`). Second, `PutPRLink`
  is an upsert whose `DO UPDATE SET` writes every column (`store.go:1168-1180`), and the tend
  sweep deliberately does not read review activity — so a sweep armed by any merge would
  overwrite a `1` with a `0` in the window between the decision and the detached runner reading
  the row, and the agent would silently take the pure-rebase branch. The dispatch row has neither
  problem: it is written once, from the decision, by the code that made the decision, which is
  exactly how `Model`, `Harness`, and `Effort` already travel.

- [ ] Add `ReviewPending bool` to `runner.PromptPR` and render it from the DISPATCH row
  (`d.ReviewPending`) where `RunAgent` builds `PromptData` (`internal/loopcmd/tick.go:748-762`),
  beside `BehindBy`, which comes from the link row. Without this the feature buys nothing: the
  shipped tend prompt is a pure rebase instruction, and a `ReviewPending` dispatch on a current
  pull request would render "It is 0 commits behind" and tell the agent to rebase and stop.

- [ ] In `loopcmd.act`, make `doneRebased` fall through to the dispatch when `d.ReviewPending` is
  set. Count the rebase first, so the summary still reports it. Leave `doneNoRebase` returning
  without an agent, and say in a comment that the branch that pass reasoned about is gone, so its
  premise is stale whatever the feedback says.

**Acceptance:** `go test ./internal/engine/... ./internal/loopcmd/... ./internal/store/...` passes
with tests proving: a current pull request with review activity newer than the last tend produces
a `KindTend` decision with `ReviewPending` true; the same pull request with the last tend NEWER
than the activity produces no decision and the new skip reason; a behind pull request with no
review activity produces a decision with `ReviewPending` false; a failed review read leaves the
decision on staleness alone and dispatches nothing for a current pull request; a clean rebase on a
`ReviewPending` decision counts the rebase AND dispatches the agent; `CreateDispatch` and the
dispatch reads round-trip `ReviewPending`; a `tend_prompt` containing `{{.PR.ReviewPending}}`
renders true for a review-pending dispatch and false otherwise.

## Task 7 — read the conflicted paths from git

**review: no**

- [ ] Add `func (m *Manager) ConflictedPaths(ctx context.Context, path string) ([]string, error)`
  to `internal/worktree/worktree.go`, beside `AbortRebase`. Run
  `git diff -z --name-only --diff-filter=U` through `gitStdout`, split on NUL, drop empty entries,
  and sort.

- [ ] Split on NUL, not on newline, and say why: `--name-only` C-quotes an unusual path only while
  `core.quotePath` is true, so a repository that turned it off makes a path containing a newline
  split into two entries — which changes the fingerprint's shape and defeats the backoff. NUL is
  the one byte a path cannot hold, which is why the fingerprint joins on it too.

- [ ] Sort inside this function, not at the call site. The fingerprint is a hash, so an unstable
  order would make one conflict look like a different conflict on the next pass and defeat the
  backoff completely.

- [ ] Return an empty slice and no error on a clean worktree. A rebase can fail for reasons that
  leave no conflicted path — a dead context, a bad ref — and the caller must be able to tell that
  from a real conflict.

- [ ] Say in the doc comment that the returned paths are never passed back to git and never
  interpolated into a command, so a later change does not quietly do it.

**Acceptance:** `go test ./internal/worktree/...` passes with tests, built on `initRepoWithOrigin`
(`internal/worktree/worktree_test.go:45`), proving: a real conflicted rebase reports its
conflicted files, sorted; a clean worktree reports an empty slice and no error.

## Task 8 — the conflict table

**review: yes** (new schema)

- [ ] Add the `tend_conflicts` table to `schemaTables` in `internal/store/store.go`, in the shape
  the spec gives, with comments in the style of the tables around it.
  - On the PRIMARY KEY: one row per pull request, not one per fingerprint. A new fingerprint
    replaces the row, so the table cannot grow without a bound and a changed conflict cannot
    inherit an old conflict's backoff.
  - On `retry_after`: Unix seconds in an INTEGER, because no literal `TIMESTAMP` default reads
    back as the zero time (`internal/store/store.go:53`), and because it matches
    `issues.retry_after`, the other wall-clock deadline in this schema. Do NOT cite the
    `addedColumns` reason `issues.retry_after` gives — this is a new table, and that reason does
    not apply here.
  - `project_id TEXT NOT NULL DEFAULT ''` first in the key.

- [ ] `tend_conflicts` is a NEW table, so it needs no `addedColumns` entry, no `rebuilt` entry, no
  `schemaIndexes` entry (every read is by the full primary key), and no work in
  `internal/store/legacy.go` — `stampInPlace` (`legacy.go:372`) claims pre-project rows in the
  five tables that predate the project era, and this table cannot hold one.

- [ ] Add `store.TendConflict` to `internal/store/types.go` and three methods:
  - `TendConflict(loop, repo string, prNumber int) (TendConflict, bool, error)` — the second
    result reports whether a row exists.
  - `PutTendConflict(c TendConflict) error` — a plain upsert that writes exactly the row it is
    given, including `seen_count` and `retry_after`. It does NOT compute either. The backoff
    schedule is a `loopcmd` constant the store cannot see, so a count spent in SQL could not
    derive the deadline that goes with it; every caller holds the loop lock (`act` runs under it
    in all three passes), so read-then-write here is not the racy pattern `BeginDispatch` exists
    to avoid. Say that in the doc comment.
  - `DeleteTendConflict(loop, repo string, prNumber int) error` — deleting a row that is not there
    is not an error.

- [ ] Delete the row wherever a pull request stops being tendable, so the table follows the pull
  request out. Two places, both logging and continuing on failure rather than returning — one
  failed row must not abandon a pass (`internal/loopcmd/tendcheck.go:166-177`):
  - beside the `pr_links` delete in `internal/loopcmd/tendcheck.go`;
  - in the closed-pull-request cleanup in `internal/loopcmd/cleanup.go`, which is the only path a
    cron-only machine reaches.

**Acceptance:** `go test ./internal/store/... ./internal/loopcmd/...` passes with tests proving:
`PutTendConflict` then `TendConflict` round-trips every field; a second `PutTendConflict` for the
same pull request replaces the row; `TendConflict` reports `false` for an absent row;
`DeleteTendConflict` is idempotent; another project cannot see the row; closing a pull request
deletes it on both cleanup paths.

## Task 9 — the repeat-conflict backoff

**review: yes** (it decides not to act)

- [ ] Add `doneBackedOff` to the `rebaseOutcome` enum in `internal/loopcmd/rebase.go`, and extend
  the enum's comment: it now names four outcomes, three of which mean "dispatch no agent".

- [ ] Add the fingerprint function to `internal/loopcmd/rebase.go`:

  ```go
  func conflictFingerprint(headSHA string, paths []string) string
  ```

  SHA-256 over `headSHA` and the sorted paths, joined with `"\x00"`. Return the hex digest.
  Document why the BASE commit is excluded: a tend sweep is armed by the base moving, so a
  fingerprint carrying the base would be new on every sweep and would suppress nothing. Name
  finding 5 — one pull request, the same `CLAUDE.md` conflict, four sweeps, five hours — as the
  case that requires it. `headSHA` is the value `gitRebase` already reads as the push lease
  (`rebase.go:145-152`): it is read after `EnsurePRCtx` checked out `FETCH_HEAD`, so it is the
  remote head of the branch, and it is read before `Rebase`, so a mid-rebase HEAD does not pollute
  it.

- [ ] Add the schedule as a package variable beside `maxTendPerSweep`, with the same reasoning that
  comment gives for being a constant rather than a configuration field:

  ```go
  // conflictBackoff[n-1] is the wait after the nth agent dispatch at one
  // fingerprint. Index n-1, not n: a pull request with no row has never had an
  // agent sent at this conflict, and the agent is the right answer to a
  // conflict it has not seen, so the first sighting dispatches with no wait and
  // consults no entry here.
  var conflictBackoff = []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour}
  ```

  Clamp the index to the last entry.

- [ ] Extend `RebaseGit` with `ConflictedPaths(ctx context.Context, path string) ([]string, error)`
  — the same signature `worktree.Manager` gets in Task 7. It is an interface so a test can drive
  this branch without a remote; keep that property.

- [ ] Change `gitRebase`'s signature to take the clock:
  `gitRebase(ctx context.Context, cfg *config.Config, deps Deps, d engine.Decision, now
  time.Time)`. `act` already holds a `now` (`internal/loopcmd/tick.go:452-457`) and passes it to
  `dispatch` and `reapDead`; pass the same value here so one pass reads one clock. Do NOT reach
  for `deps.Now()` inside `gitRebase` — a test that pins the tick's clock would then see two
  different times in one pass.

- [ ] In `gitRebase`, on the conflict branch, in this exact order:

  1. Read `deps.Git.ConflictedPaths` on the DETACHED `cleanupCtx`, not on `ctx`, and before the
     abort. Before the abort because the abort clears the conflicted paths. On the detached
     context because the commonest way to reach this branch is `rebaseBudget` expiring, and
     `exec.CommandContext` on a dead context fails without running git — the same reasoning the
     abort's own comment gives (`rebase.go:158-180`). Build `cleanupCtx` above this read so the
     abort can share it.
  2. If the read failed, or returned no paths, log and fall through to `notDone`. A rebase that
     failed with no conflicted path is not a conflict this gate understands, and refusing to
     dispatch on it would be a silent stall.
  3. Compute the fingerprint. Read the stored row with `deps.Store.TendConflict`. If that read
     failed, log and fall through to `notDone` — a gate that declines to spend money must fail OPEN
     on unreadable state, or an unreadable row would strand the pull request.
  4. If a row exists, its fingerprint matches, `now` is before its `retry_after`, AND the
     decision does NOT carry `ReviewPending`: return `doneBackedOff` WITHOUT writing anything. Do
     not advance `seen_count` and do not move `retry_after`. A sweep is armed by every merge and
     every push, so a write here would push the deadline forward more often than it arrives and
     the agent would never be dispatched again; `seen_count` is a count of agent dispatches that
     failed at this conflict, not of passes that saw it.

     `d.ReviewPending` is checked HERE rather than in `act` so the count stays honest. The backoff
     is evidence about a repeated rebase conflict and says nothing about whether a reviewer's
     comment has been answered, so a review-pending decision must still reach the agent — and
     because it does, that pass IS an agent dispatch at this fingerprint and must advance the
     count like any other. Letting `act` override the outcome instead would dispatch the agent
     without ever advancing `seen_count`, so a stuck conflict with a talkative reviewer would be
     bounded by nothing.
  5. Otherwise the agent is about to be dispatched. Compute the new count — `1` when no row exists
     or the fingerprint changed, else `seen_count + 1` — and write the row with
     `retry_after = now + conflictBackoff[min(count, len)-1]`. A failed write is logged, not
     returned: the agent still runs, and losing one backoff round is better than abandoning the
     pass. Return `notDone`.

  Note that the failed-abort path below still returns `doneNoRebase` and writes nothing, which is
  consistent: no agent was dispatched, so no sighting is counted.

- [ ] Log one Info line on the backoff naming the loop, the issue, the pull request, the sighting
  count, the deadline, and the conflict. Log the NUMBER of conflicted paths, plus the joined list
  through `truncate(..., 200)`: a conflicted rebase can list thousands of paths of arbitrary UTF-8,
  and every other externally-influenced string on this path is bounded before it is logged
  (`rebase.go:211-215`). Not Debug — nothing in this program logs at Debug.

- [ ] Delete the conflict row on the success path of `gitRebase`, beside `recordRebase`
  (`rebase.go:245`). A branch that replayed cleanly has no conflict left to remember. Handle the
  error explicitly and log it — `errcheck` is enabled and a bare `_ =` is not an option — but do
  not return it: the rebase HAPPENED, and reporting it as undone would send an agent at an
  already-current branch.

- [ ] Handle `doneBackedOff` in `loopcmd.act`: return with no agent and no dispatch count, like
  `doneNoRebase`. No `ReviewPending` special case belongs here — `gitRebase` already declined to
  back off such a decision, so a `doneBackedOff` reaching `act` is always one with no feedback to
  answer. Say that, so a later reader does not add the case back and double-dispatch.

- [ ] Add `Backoff int \`json:"backoff"\`` to `loopcmd.Summary` (`tick.go:124`) — every field there
  carries an explicit snake_case tag because `Summary` is marshalled into `ticks.summary_json` —
  and count it on the backed-off path, so the summary an operator audits says a pull request was
  skipped rather than reporting nothing. Add it to the sweep's cap warning line
  (`tendsweep.go:306`) beside `dispatched` and `rebased`.

**Acceptance:** `go test ./internal/loopcmd/...` passes with tests proving: a first conflict
dispatches the agent and writes a row with `seen_count = 1` and a one-hour deadline; the same
fingerprint within the window returns `doneBackedOff`, dispatches nothing, and leaves
`seen_count` and `retry_after` UNCHANGED; the same fingerprint after `retry_after` dispatches and
writes `seen_count = 2` with a six-hour deadline; a moved head produces a new fingerprint and
writes `seen_count = 1`; a clean rebase deletes the row; a rebase failure with no conflicted paths
dispatches; an unreadable conflict row dispatches; a `ReviewPending` decision inside the backoff
window dispatches AND advances `seen_count`.

## Task 10 — documentation

**review: no**

- [ ] `docs/configuration.md`: add a `## tend` section after `## agent`, with its three fields,
  the three-layer precedence, and the session-continuity warning. `TestEveryConfigFieldIsDocumented`
  (`internal/config/docs_test.go:20-29,34-56`) asserts only that the DOTTED path appears
  BACKTICKED somewhere in the file — so `` `tend.harness` ``, `` `tend.model` ``, and
  `` `tend.effort` `` must each appear in exactly that form. A section writing bare `` `harness` ``
  satisfies nothing. Add all three to the Quick reference table
  (`docs/configuration.md:176-211`) in that form, and add `tend` to the Contents list
  (`:163-175`).

- [ ] `docs/configuration.md`, the Validation rules table (`:1015-1035`): add the two new rules,
  `tend.harness` enum and `tend.effort` enum, beside `agent.effort` and `agent.permission_mode`.

- [ ] `docs/configuration.md`, `## tend_pr` (`:762`): replace "Version 1 rebases only. It does not
  reply to review feedback." with the two tend triggers — behind its base, or review activity newer
  than the last finished tend dispatch — the fact that only a trusted repository member's activity
  counts and the loop's own comments never do, the repeat-conflict backoff and its schedule, and
  the note that answering feedback needs a `tend_prompt` that branches on `{{.PR.ReviewPending}}`.
  State that the tend SWEEP still triggers on staleness alone.

- [ ] `docs/configuration.md`, Template variables (`:988`): add `PR.ReviewPending`.

- [ ] `README.md`, `## Configuration`: one paragraph naming `tend:` and linking to the new
  reference section. `README.md`, the `tend_interval` subsection (`:664`): one sentence saying the
  periodic check still gates on staleness alone, and that review activity reaches the loop through
  its own delivery.

- [ ] `examples/execution.yaml`: add a commented-out `tend:` block showing a cheaper model, and
  extend the `tend_prompt` to branch on `{{.PR.ReviewPending}}` — when it is set, read the
  unresolved review threads and answer them; when it is not, the existing rebase instruction is
  unchanged. Do NOT enable the `tend:` block.

- [ ] Make the same edit to `internal/wizard/templates/execution.yaml`.
  `TestExamplesMatchTheEmbeddedTemplates` (`internal/wizard/templates_test.go:65-81`) requires
  `examples/<name>.yaml` to be BYTE-IDENTICAL to `internal/wizard/templates/<name>.yaml` for every
  name in `templateNames` (`internal/wizard/templates.go:23`). Check whether `pi.yaml` and the
  other templates carry a `tend_prompt` that needs the same treatment.

**Acceptance:** `go test ./internal/config/... ./internal/wizard/...` passes. Every anchor link
resolves.

## Task 11 — gates

**review: no**

- [ ] Run `make check` (`fmtcheck`, `vet` for both `GOOS`, `lint`, and the full `test` suite) and
  fix what it reports.

**Acceptance:** `make check` exits zero.
