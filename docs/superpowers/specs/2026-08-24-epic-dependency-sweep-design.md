# Design: epic dependency sweep

## Premise and blast-radius check (step-0 findings)

An epic is a GitHub issue that carries the `epic` label and holds other issues as sub-issues.
The sub-issues declare their order with GitHub issue dependencies: issue #73 records that #71
blocks it. Today nothing reads either relation. A human applies the entry label to each
sub-issue by hand, after the issue that blocked it closes.

This change makes the closure of one sub-issue promote the sub-issues it unblocked. The promotion
is one label write. It dispatches no agent.

- **Entry path.** There are two drivers, and both already exist. GitHub sends a webhook;
  `internal/listener/handler.go:421` drops any event outside `ghub.HookEvents`; the handler takes
  one number from the payload (`handler.go:501`) and hands the delivery to a bounded pool;
  `Worker.Deliver` (`internal/listener/work.go:297`) fans out to each loop that watches the
  repository. The second driver is `loopcmd.Tick`, the full sweep an operator schedules with
  cron.
- **Blast radius.** Four packages, all in this repository. `internal/ghub` gets a new read
  interface. `internal/epic` is new and holds the rule. `internal/loopcmd` gets the sweep and its
  cron entry point. `internal/listener` gets one payload field and one seam. `docs/configuration.md`
  and `README.md` record the new behavior. There is no schema change, no new configuration field,
  and no new dependency.
- **Prior art.** `loopcmd.TendSweep` (`internal/loopcmd/tendsweep.go`) is the same shape: one
  delivery that names no issue, a bounded fan-out, a cap, and a lock held only for the write.
  `ghub.HookAdmin` (`internal/ghub/hooks.go`) is the precedent for a narrow interface that a test
  can fake without faking all of `ghub.Client`. `ghub.Client.EditLabels` already exists and is
  already used by a non-agent write (`KindParkRetryExhausted`).
- **Contradiction scan.** The code agrees with the operator's description. Epic #69 of
  `mcgarylabs/koinos-monorepo` carries the `epic` label and holds nine sub-issues. Its dependency
  graph is prose in the issue body today, and both dependency endpoints return `[]` for its
  children, so the graph must be entered through the API before the sweep does anything. That is
  data entry, not code.

### The finding that shapes this design

`internal/listener/work.go:150-157` records a past regression:

> RunIssue acts on ONE issue, taking the loop's lock first. It is `loopcmd.TickIssue`, never
> `loopcmd.RunTick`: the daemon answers events, and an event names an issue. The full reconcile
> is the cron sweep's job — see `loopcmd.Tick` — and running it per delivery burned a token budget
> on every open issue of every project watching the repository.

This change makes a delivery act on more than one issue again. Four limits keep it apart from the
pass that was removed:

1. **It dispatches no agent.** This is the strongest of the four. The removed reconcile was
   expensive because it started agents and spent tokens. This sweep writes labels. Its cost is a
   small, bounded number of GitHub API calls and no tokens at all.
2. It runs for **one event only** — an `issues` delivery with action `closed`, for an issue whose
   parent carries the `epic` label. An opened issue, a moved label, and a comment start no sweep.
3. Its fan-out is **the epic's own children**, not the repository. An epic with nine sub-issues
   costs at most eleven reads.
4. It writes **one label, to a statelessly-selected issue**, and removes none.

### Second finding: the sweep grants no new authority

The sweep adds `status:ready-for-spec` to an issue with no human in the loop. That label starts a
planning agent on issue text this repository treats as untrusted (`README.md`, Security).

The sweep does **not** widen that exposure. Anyone who can make the sweep act — by setting the
`epic` label on a parent, or by adding a sub-issue, or by closing an issue — already holds the
`triage` permission that lets them apply `status:ready-for-spec` to the issue directly, in one
click, with no sweep involved. The sweep is a slower path to a thing its actor can already do.

The rule that governs both remains the one in `README.md`: point a loop only at a repository whose
issue population you trust.

### Third finding: a naive rule would skip the planning stage

The operator's requirement is that the sweep is always on, with no configuration flag. A rule of
"every loop promotes an unblocked issue into its own `labels.trigger`" satisfies that and is
wrong. The `execution` loop's trigger is `status:ready-for-execution`. A statusless sub-issue is
visible to both loops on the same tick, so the execution loop would promote it directly to
execution and skip planning entirely.

The design therefore derives **one** entry loop per repository and lets only that loop sweep. See
*The entry loop*.

## Verified external API (do not re-derive)

Read from GitHub's documentation and probed live against `mcgarylabs/koinos-monorepo` on
2026-08-24.

**`GET /repos/{owner}/{repo}/issues/{number}/parent`**

- Returns one issue object, with `labels`. Returns `404` when the issue has no parent.
- Takes no pagination parameters.
- The same fact is on the issue object as `parent_issue_url` (string or null), which gives the URL
  but not the parent's labels.

**`GET /repos/{owner}/{repo}/issues/{number}/sub_issues`**

- Returns an array of issue objects, each with `number`, `state`, and `labels`.
- Paginated: `per_page` max 100, default 30; `page` default 1.
- Returns `[]` for an issue with no sub-issues.

**`GET /repos/{owner}/{repo}/issues/{number}/dependencies/blocked_by`**

- Returns an array of issue objects, each with `number`, `state`, `state_reason`, `labels`, and a
  full `repository` object.
- Paginated: `per_page` max 100, default 30; `page` default 1.
- Returns `[]` for an issue with no declared blockers.
- **A blocker can live in a different repository.** Its `state` is in the same response, so the
  sweep needs no second call and no cross-repository client to read it.

**go-github v77 coverage.** `SubIssueService.ListByIssue(ctx, owner, repo, issueNumber int64, opts *IssueListOptions)`
exists and covers `sub_issues`. There is **no** dependencies service and **no** parent accessor in
v77. Those two endpoints need `client.NewRequest` plus `client.Do`, which is what
`internal/ghub/hooks.go` already does for its own calls.

**Repository facts.**

- `internal/config/discover.go:143` — `List(agentUtilsDir)` returns one `Entry` per loop file.
  `Entry` carries `Name`, `File`, `Path`, `Repo`, and `Err`. It does **not** carry the loaded
  configuration, so the entry-loop derivation must call `config.Load(entry.Path)` per file.
- `internal/config/config.go:53` — `Labels` carries `Trigger`, `InFlight`, `Blocked`, `Review`,
  `Terminal`, and `Veto`. `Terminal` is optional; the execution loop omits it.
- `internal/ghub/types.go:9` — `ghub.Issue` carries `Number`, `Title`, `Labels`, and `UpdatedAt`.
  It has **no** `State` field, because every present caller lists open issues only.
- `internal/ghub/types.go:29` — `Issue.HasAnyLabel` implements the `"prefix*"` rule the veto lists
  use. `status:*` is expressible with it.
- `internal/lock/lock.go:31` — `Acquire` is non-blocking and returns `ErrHeld` at once.

## Behavior

### The trigger

The handler decodes no new payload field. It already decodes `Action`. A delivery is a **close
delivery** when both are true:

- the event is `issues`;
- the action is `closed`.

`issues` is already in `ghub.HookEvents`, so no webhook re-registration is needed and an existing
installation gains the behavior on upgrade.

The handler does not look up the parent and does not decide whether the issue belongs to an epic.
It has no GitHub client. It sets one boolean on `Delivery` and the sweep decides the rest, exactly
as `MergedInto` is judged per loop rather than in the handler.

### The entry loop

Only one loop per repository sweeps. It is derived, never configured.

For the loops of one project that watch the same repository:

1. Load each loop's configuration.
2. Collect the set of every loop's `labels.terminal` and `labels.review`, excluding the loop being
   tested.
3. A loop is the **entry loop** when its `labels.trigger` is in neither set.

For the reference pair this resolves as intended. `planning.labels.trigger` is
`status:ready-for-spec`, which is no other loop's terminal or review label, so planning is the
entry loop. `execution.labels.trigger` is `status:ready-for-execution`, which is
`planning.labels.terminal`, so execution is downstream and never sweeps.

The comparison folds case, as `Issue.HasLabel` does.

**Failing closed.** The sweep runs only when the derivation names exactly one entry loop. Zero
entry loops, two or more entry loops, or any loop file in the group that fails to load, all mean
no loop sweeps. The tick logs which of those it found, and names the loops. A guess here would
promote issues into the wrong stage of the pipeline, and the failure would be silent.

### The rule

`internal/epic` holds one pure function. It takes the epic's children, and each open child's
blocker list, and returns the numbers to promote. It reads no clock, opens no socket, and keeps
no state.

A child is promoted when **all** of these are true:

- the child's state is `open`;
- every issue in the child's `blocked_by` list has state `closed`, where an empty list satisfies
  this;
- the child carries **no** label matching `status:*`;
- the child carries no label matching the entry loop's `labels.veto` rules.

Promotion is: add the entry loop's `labels.trigger`. Nothing is removed. No comment is posted. No
agent is dispatched, no session is opened, and no worktree is created.

**Idempotence is structural.** The third condition stops being true the moment the promotion
lands, so the sweep keeps no record of what it promoted and needs none. A sweep that fails halfway
is re-run with no cleanup, and a sweep that runs twice writes once.

**The `status:*` test is what protects work in flight.** Issue #74 of the reference repository
sits at `status:plan-ready-for-review` while its blockers are closed. The rule must leave it
alone; pulling it back to `status:ready-for-spec` would discard a plan a human is reading.

### The sweep

`loopcmd.EpicSweep` takes the closed issue's number, and:

1. Reads the closed issue's parent. On `404`, or a parent without the `epic` label, it stops and
   reads nothing more. This is the common case for most deliveries and it costs one call.
2. Reads the parent's sub-issues.
3. Selects the children that are open, carry no `status:*` label, and carry no veto label. Only
   these need a blocker lookup, so the filter runs before the calls, not after.

   This filter is an **optimization, not the rule**. It saves a call for a child that cannot be
   promoted whatever its blockers say. `epic.Promote` tests the same three conditions again, and
   it is the only place the decision is made. The sweep may pass it a child the filter would have
   dropped, and the answer must not change.
4. Reads `blocked_by` for each selected child.
5. Calls `epic.Promote`, which returns the numbers to promote.
6. Takes the loop's lock, then adds the entry loop's `labels.trigger` to each of them.

The lock is taken for the writes only, not for the reads. `TendSweep` documents why: every second
the lock is held is a second in which a labelled issue can be dropped by a concurrent delivery,
and holding it across a paginated listing would hold it for tens of seconds.

The cron driver enters at step 2 instead: `loopcmd.Tick` lists open issues, keeps those with the
`epic` label, and runs steps 2 to 6 for each. This is the safety net for a delivery the daemon
never saw. The daemon is the fast path; cron is the backstop. Both share every step after the
entry.

### Caps

`maxPromotePerSweep` bounds one sweep, in the same way and for the same reason as
`maxTendPerSweep`. Promotions are ordered by issue number, so a capped sweep takes the
low-numbered batch and the next sweep takes the next. What is left over is logged and named, never
dropped silently.

The value is **25**. It is higher than `maxTendPerSweep`, which is 10, because a promotion is a
label write and not an agent: the cost of the batch is 25 API calls, where tending's is 10 agent
processes. It is a constant, not a configuration field, and it is promoted to a field only when an
operator needs a different value.

### Error handling

- A failed `blocked_by` read for one child logs a warning and the sweep continues with the rest.
  A child whose blockers could not be read is **not** promoted. Failing closed is correct here:
  the alternative promotes an issue whose blockers may be open.
- A failed `EditLabels` for one child logs an error and the sweep continues. The next close
  delivery, or the next cron tick, promotes it.
- A failed parent read that is not a `404` is logged and stops this sweep. It says nothing about
  whether the issue belongs to an epic, and both answers are wrong to assume.
- `lock.ErrHeld` is not a failure. It is logged as a skip, and the next driver sweeps.
- A failed sweep never fails the delivery. The delivery's own `TickIssue` pass keeps its present
  behavior and its present retry.

## Testing

`internal/epic` is pure, so its tests are a table with no fakes and no clock.

`ghub.EpicReader` is a narrow interface, so the sweep's tests fake three methods rather than all
of `ghub.Client`. The four existing `ghub.Client` fakes are untouched by this change.

Cases to pin, in `internal/epic`:

- A child with an empty `blocked_by` list is promoted.
- A child whose blockers are all closed is promoted.
- A child with one open blocker is not promoted.
- A child carrying `status:plan-ready-for-review` is not promoted, even with every blocker closed.
- A child carrying a veto label is not promoted.
- A closed child is not promoted.
- A diamond: two blockers of one child close in the same sweep, and the child is promoted once.
- A blocker in another repository is honored by its state, not by its repository.

Cases to pin, in `internal/loopcmd`:

- An issue with no parent stops the sweep after one call.
- A parent without the `epic` label stops the sweep after one call.
- A child that needs no blocker lookup does not get one.
- A failed `blocked_by` read for one child leaves the other children promoted.
- A failed `EditLabels` for one child leaves the other children promoted.
- A sweep beyond the cap promotes the low-numbered batch and names the rest.
- Zero entry loops resolved: nothing sweeps, and the reason is logged.
- Two entry loops resolved: nothing sweeps, and both are named.
- A loop file that fails to load: nothing sweeps.
- Only the entry loop sweeps when both loops watch one repository.

Cases to pin, in `internal/listener`:

- An `issues` delivery with action `closed` runs the sweep.
- An `issues` delivery with any other action runs no sweep.
- A `pull_request` delivery runs no sweep.
- The closed issue's own `TickIssue` still runs in every case above.

## Out of scope

- Writing the dependency graph. The sweep reads `blocked_by`; it never creates one. Entering epic
  #69's prose graph into the API is data entry for a human or an agent, not work for this code.
- Any promotion other than into the entry loop's trigger label. The sweep never moves an issue
  between stages, never removes a label, and never closes an issue.
- Reacting to a dependency that is added or removed. Only a close starts a sweep, and cron catches
  the rest.
- A configuration flag to disable the sweep. The operator's decision is that it is always on. The
  `epic` label on the parent is the only switch.
