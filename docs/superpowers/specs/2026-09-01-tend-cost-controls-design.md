# Design: tend cost controls

Four changes to the tend path. Each one removes an agent dispatch that the program can answer
itself, or makes the dispatch that remains cost less.

## Premise and blast-radius check (step-0 findings)

The request comes from a measurement of 42 real tend dispatch logs and the `dispatches` table of
`~/.agent-utils/state.db`. It reports six findings. Two of the four requested changes are already
in the code, because pull request #18 (`7184c75`) landed after the logs were recorded. This
section states what is present and what is absent, with evidence.

### Entry path

1. GitHub sends a webhook. `internal/listener/handler.go` validates it and builds a `Delivery`.
2. `Worker.tickOne` (`internal/listener/work.go:687`) runs the issue pass and arms a tend sweep
   when the delivery merged into, or pushed to, `cfg.DefaultBranch`.
3. `Worker.tendCheckPass` (`work.go:1144`) runs `loopcmd.TendCheck` on a timer.
4. `loopcmd.TickIssue` (`internal/loopcmd/tickissue.go:38`) decides one issue.
   `loopcmd.TendSweep` (`internal/loopcmd/tendsweep.go:88`) decides the loop's review issues.
   `loopcmd.Tick` decides all of them.
5. All three call `engine.Decide` (`internal/engine/engine.go:16`), which calls `tendDecisions`
   (`engine.go:335`) to produce a `KindTend` decision.
6. `loopcmd.act` (`internal/loopcmd/tick.go:452`) answers a `KindTend` decision. It calls
   `gitRebase` (`internal/loopcmd/rebase.go:126`) first, and `dispatch` only when the rebase did
   not settle the decision.

### What the four requests find in the code today

- **(a) A `tend:` config section — ABSENT.** `config.Agent` (`internal/config/config.go:62`) is
  one flat struct for the loop. `runner.Effective` (`internal/runner/args.go:57`) layers only the
  per-issue label overrides on top of it. No dispatch kind can carry its own harness, model, or
  effort.
- **(b) A precheck before a tend dispatch — HALF PRESENT.** `tendDecisions` produces no decision
  when `snap.BehindBy[pr.Number] <= 0` (`engine.go:381`), so the "is the pull request behind its
  base" half is enforced today. The "are there review comments newer than the last tend dispatch"
  half does not exist: nothing in the program reads a review, a review comment, or the time of the
  last tend dispatch. Review activity is therefore not a tend trigger at all.
- **(c) A clean-rebase fast path — PRESENT.** `gitRebase` (`rebase.go:126`) fetches, checks the
  worktree is clean, rebases, and force-pushes with a lease. It dispatches the agent only on a
  conflict. `--force-with-lease` is used, and the lease is pinned to the commit the pass fetched
  (`rebase.go:170`).
- **(d) A repeat-conflict backoff — ABSENT.** `gitRebase` aborts a conflicted rebase and returns
  `notDone`, and `act` then dispatches the agent (`tick.go:485`). Nothing records the conflict, so
  every later sweep repeats it. Finding 5 in the request is this behaviour: issue 51 met the same
  `CLAUDE.md` conflict on four sweeps in five hours and spent about $0.75 each time.

### Blast radius

All of it is in this repository.

- `internal/config` — the new `tend:` section and its validation.
- `internal/runner` — `Effective` gains the dispatch kind.
- `internal/engine` — the review-activity trigger and the harness the tend will run.
- `internal/ghub` — one new read method.
- `internal/store` — one new table, and the reads and writes for it.
- `internal/loopcmd` — the conflict fingerprint, the backoff gate, and the review-activity read.
- `internal/worktree` — one new git command.
- `README.md` and `docs/configuration.md` — the new section and the new trigger.

### Prior art to reuse, not reinvent

- `config.ParseOverrides` (`internal/config/overrides.go:52`) already validates a harness, a
  model, and an effort value. The `tend:` section reuses the same enums.
- `runner.Effective` is already the one place a configured value and an override meet.
- `engine.resumable` and `engine.effectiveHarness` (`engine.go:302,317`) already hold the
  session-continuity rule.
- `store.IssueState.RetryAfter` is already a wall-clock deadline stored as Unix seconds with a
  literal default, which is the pattern the new backoff deadline copies.
- `worktree.Manager.gitStdout` (`internal/worktree/worktree.go:412`) already runs a bounded git
  command and returns its output.

**Outcome: the premise holds, and two details are stale.** (c) is done. (b) is half done. The
plan implements (a), the missing half of (b), and (d), and states in one line that (c) needs no
change. This is outcome 2 of the premise check, not a block.

- **Class: Large.** The change adds a configuration section, a database table, a GitHub read, and
  a new gate in front of an agent dispatch.
- **Profile: backend.** The repository has no user interface.

## Goals

1. A tend dispatch can run a cheaper harness, model, and effort than the loop's own work.
2. New review activity on a pull request is a tend trigger, so the agent answers feedback instead
   of only rebasing.
3. A tend that has nothing to do dispatches no agent, and the pass still records itself.
4. A conflict that already defeated the agent does not dispatch the agent again until something
   about it changes.

## Non-goals

- The clean-rebase fast path. It exists (`rebase.go`), and this change does not alter it, except
  where goal 2 requires the agent to run after a clean rebase.
- A per-dispatch-kind section for `start` or `resume`. Only `tend:` is added. The request asks for
  the structure that makes more kinds possible, not for more kinds.
- Making the conflict backoff configurable. The schedule is a constant, like `maxTendPerSweep`.
- Replying to a review comment without a pull request. The trigger is a pull request's review
  activity, and an issue with no linked pull request is not tended.

## Section 1: the `tend:` configuration section

### The shape

```yaml
agent:
  harness: claude
  model: opus
  effort: high

tend:
  harness: claude
  model: sonnet
  effort: medium
```

`tend:` is optional. Each of its three fields is optional. An absent field falls back to the same
field of `agent:`. The other `agent` fields — `permission_mode`, `worktree`, `max_budget_usd`,
`timeout`, `background_tasks` — are NOT repeated in `tend:`. They are properties of how this
program runs an agent, not of which agent runs, and a tend has no reason to differ in them.

### The precedence

Three layers, most specific last:

1. `agent.<field>` — the loop's value.
2. `tend.<field>` — the value for a tend dispatch, when the field is set.
3. The issue's `model:`, `harness:`, or `effort:` label — the value for this issue.

The label stays on top. `docs/configuration.md` already states that a label "can override this
setting for that issue's dispatch", and an operator who writes `model:opus` on one issue means
it. The `tend:` section is a default for a class of dispatch; a label is an instruction about one
issue.

### The API

`runner.Effective` gains a dispatch kind:

```go
func Effective(cfg *config.Config, kind string, ov config.Overrides) Settings
```

`kind` is a `store.Kind*` value. `store.KindTend` selects the `tend:` layer; every other value
selects `agent:` alone. The kind is a string that the store already writes on the dispatch row, so
the detached runner reads it from the row it already loads and needs no new column.

### The invariant this must not break

A session belongs to the harness that created it. `engine.resumable` refuses to resume a session
whose recorded harness differs from the harness the dispatch will run, and `engine.tendDecisions`
uses that to decide whether the tend inherits the issue's session (`engine.go:409`).

`engine.effectiveHarness` today reads `cfg.Agent.Harness`. It must read the harness the TEND will
run, which is `tend.harness` when set. Without that change, a loop with `agent.harness: claude`
and `tend.harness: pi` would hand the pi tend a claude session identifier. pi does not refuse an
unknown identifier — it creates a fresh session under it and carries on — so the conversation
would be silently lost, which is the exact failure `resumable` exists to prevent.

`engine.effectiveHarness` therefore also gains the dispatch kind, and `tendDecisions` passes
`store.KindTend`. The retry and trigger paths pass their own kinds and are unchanged in
behaviour.

### Validation

`config.validate` checks `tend.harness` against the same two names as `agent.harness`, and
`tend.effort` against the same five levels as `agent.effort`. `tend.model` is free text, exactly
as `agent.model` is, and is NOT required: an absent value means "use `agent.model`".

## Section 2: review activity as a tend trigger

### Why this is the missing half of the precheck

`tendDecisions` already refuses to dispatch when the pull request is not behind its base. That is
the first half of "is there anything to do?", and it is enforced. The second half is absent, and
its absence became a real gap when the clean-rebase fast path landed: a pull request that is
behind gets rebased by git and no agent runs, so review feedback on that pull request is never
answered. A pull request that is NOT behind produces no decision at all.

### The read

`ghub.Client` gains one method:

```go
LatestReviewActivity(ctx context.Context, owner, repo string, number int) (time.Time, error)
```

It returns the most recent of the pull request's submitted reviews and its review comments, and
the zero time when there are none. It is implemented with `PullRequests.ListReviews` and
`PullRequests.ListComments`, both sorted so the newest page is read first.

It is called once per tend candidate — an issue carrying `labels.review` whose trusted linked pull
request targets the branch in question. A candidate is already the subject of a `BehindBy` call,
so this doubles the per-candidate cost of a pass and adds nothing to a pass with no candidates.
One REST call costs no tokens, and the dispatch it can prevent cost $0.75 on average.

### Where it is NOT called

`loopcmd.TendCheck` (`tendcheck.go:52`) stays exactly as it is. Its whole purpose is to cost zero
GitHub calls when nothing is behind, and review activity cannot be seen from a local checkout.
Review activity does not need a timer: `pull_request_review` and `pull_request_review_comment` are
both in `ghub.HookEvents`, so a review already produces a delivery, and that delivery already
reaches `loopcmd.tickIssue`. The delivery path is the fast path for this trigger, and cron's full
`Tick` is its safety net.

### The comparison

The decision needs two times:

- the latest review activity on the pull request, from the read above;
- the start time of the last tend dispatch for that pull request, from a new store read
  `LastTendAt(loop, repo string, prNumber int) (time.Time, error)`.

`LastTendAt` reads `MAX(started_at)` over `dispatches` rows with `kind = 'tend'` and the given
`pr_number`. It counts a tend AGENT only. A `kind = 'rebase'` row records a rebase git performed,
which read no review and answered no comment, so counting it would suppress the first tend after
every automatic rebase.

Review activity is pending when the activity time is after the last tend time. A pull request that
has never been tended and carries any review activity is therefore pending, which is correct: no
agent has read that feedback.

### The decision

`engine.Snapshot` gains `ReviewedAt map[int]time.Time`, keyed by pull request number.
`engine.State` gains `LastTend map[int]time.Time`, keyed by pull request number.
`engine.Decision` gains `ReviewPending bool`.

`tendDecisions` produces a `KindTend` decision when the pull request is behind its base OR review
activity is pending. Its skip reason, when it produces neither, becomes "the linked pull request
is up to date with its base and carries no review activity since the last tend".

### The interaction with the clean rebase

`loopcmd.act` today returns as soon as `gitRebase` reports `doneRebased`. When the decision
carries `ReviewPending`, it must not: the rebase answered the staleness, and the feedback is still
unanswered. So `act` counts the rebase and then falls through to the dispatch.

`doneNoRebase` still returns without an agent. That outcome means the branch this pass reasoned
about is gone, so an agent sent at it now would work from a stale premise — the reasoning in
`gitRebase`'s doc comment is unchanged by this section.

## Section 3: the repeat-conflict backoff

### The fingerprint

A conflict is fingerprinted by:

- the sorted list of conflicted paths, read with `git diff --name-only --diff-filter=U` in the
  worktree, before the abort; and
- the head commit the rebase was attempted from, which `gitRebase` already reads as the push
  lease.

The fingerprint is the SHA-256 of those, joined with a separator that cannot appear in a path.

**The base commit is deliberately EXCLUDED.** Including it looks safer and defeats the whole
feature. A tend sweep is armed by the base branch moving, so the base is different on every sweep
by construction. Finding 5 is exactly this shape: one pull request met the same `CLAUDE.md`
conflict on four sweeps in five hours, and every one of those sweeps had a new base. A fingerprint
carrying the base would have been new every time and would have suppressed nothing.

The head IS included, and it is what makes the backoff release. A head that moved is an agent, or
a human, that changed the branch. Whatever it changed, the conflict this pass is looking at is not
the one that was already tried.

### The state

One new table, one row per pull request:

```sql
CREATE TABLE IF NOT EXISTS tend_conflicts (
  project_id    TEXT NOT NULL DEFAULT '',
  loop          TEXT NOT NULL,
  repo          TEXT NOT NULL,
  pr_number     INTEGER NOT NULL,
  fingerprint   TEXT NOT NULL,
  seen_count    INTEGER NOT NULL DEFAULT 0,
  first_seen_at TIMESTAMP NOT NULL,
  last_seen_at  TIMESTAMP NOT NULL,
  retry_after   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (project_id, loop, repo, pr_number)
);
```

One row per pull request, not one per fingerprint. A new fingerprint REPLACES the row, so the
table cannot grow without a bound and a changed conflict cannot inherit an old conflict's backoff.
`retry_after` is Unix seconds with a literal default `0` meaning "no deadline", copying
`issues.retry_after` for the reason that column's comment gives: `addedColumns` needs a literal
default, and no literal `TIMESTAMP` default reads back as the zero time.

The row is deleted when the pull request rebases cleanly, and when `TendCheck` deletes the
`pr_links` row of a pull request that is no longer open.

### The schedule

A constant, like `maxTendPerSweep`:

| Sighting | Wait before the next tend agent |
|---|---|
| 1st | none — the agent runs |
| 2nd | 1 hour |
| 3rd | 6 hours |
| 4th and later | 24 hours |

The first sighting always dispatches. The agent is the right answer to a conflict it has not seen.
The backoff starts only once the agent has already failed to get past this exact conflict.

The wait is a floor, not a schedule: nothing wakes to retry it. The next sweep that reaches this
pull request after the deadline dispatches the agent again, and the sighting count advances.

### Where the gate sits

In `loopcmd`, between `gitRebase` reporting a conflict and `act` dispatching. It is NOT in
`engine.Decide`, and that is deliberate. `Decide` is pure and knows nothing about git; the
conflicted paths are only knowable by running the rebase. Putting the gate in `loopcmd` also keeps
the decision itself unchanged, so `loop status` still reports the pull request as one this loop
would tend.

`gitRebase` gains a fourth outcome, `doneBackedOff`: the rebase conflicted, the conflict is one
that already defeated the agent, and the deadline has not passed. It dispatches nothing, counts
nothing, and logs one line naming the pull request, the sighting count, and the deadline.

### What it does not do

It does not park the issue and it does not write a label. A backoff is this program declining to
spend money on a repeat, not a judgement about the work. The operator sees it in the log and in
`project logs --list`, and the pull request is still tended the moment its head moves.

## Testing

Every `_test.go` file in this repository is in the SAME package as the code it tests. The tests
below name unqualified symbols for that reason.

- `internal/config`: a `tend:` section loads; each field falls back to `agent:` when absent; an
  invalid `tend.harness` and an invalid `tend.effort` each fail the load with a message naming the
  field.
- `internal/runner`: `Effective` with `KindTend` prefers `tend:` over `agent:`; a label beats both;
  `Effective` with any other kind ignores `tend:` entirely.
- `internal/engine`: a tend runs the tend harness, so a session minted by the loop harness is NOT
  resumed when `tend.harness` differs; a pull request that is current but carries review activity
  newer than the last tend produces a `KindTend` decision with `ReviewPending` set; one with
  neither produces the new skip reason.
- `internal/store`: `LastTendAt` ignores a `rebase` row; the conflict table upserts, replaces on a
  new fingerprint, and is scoped by project.
- `internal/loopcmd`: a first conflict dispatches; the same fingerprint inside the window
  dispatches nothing; the same fingerprint after the deadline dispatches; a moved head dispatches;
  a clean rebase deletes the row; a `ReviewPending` decision dispatches the agent after a clean
  rebase.
- `internal/worktree`: the conflicted-path read returns the conflicted files of a real conflicted
  rebase, and an empty list on a clean worktree.

## Deployment

No operator action is required. The `tend:` section is optional and absent from every existing
configuration, so every loop keeps the behaviour it has. The new table is created by
`CREATE TABLE IF NOT EXISTS` on the next open, and it has no rows to backfill.
