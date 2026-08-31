# Design: tend triggers and the agent-free rebase

## Premise and blast-radius check (step-0 findings)

Today the daemon rebases a stale pull request in one case only. A merged pull request whose base
is the loop's default branch arms a tend sweep. Every rebase is done by a tend agent. This change
adds two more triggers and one cheaper action.

- **Entry path.** GitHub sends a webhook. `internal/listener/handler.go:421` drops any event
  outside `ghub.HookEvents`. The handler reads one issue number from the payload and rejects a
  delivery that has none (`handler.go:502-509`). `Worker.Deliver` fans the delivery out to each
  loop that watches the repository. `Worker.tickOne` (`internal/listener/work.go:532`) runs the
  issue pass, and arms the tend sweep when `cfg.TendPR` is true and the delivery merged into
  `cfg.DefaultBranch` (`work.go:577-579`). `Worker.armTend` holds the sweep for one minute
  (`work.go:855`). `loopcmd.TendSweep` (`internal/loopcmd/tendsweep.go:71`) then reads GitHub,
  takes the loop lock, and dispatches tend agents through `loopcmd.act`.
- **Blast radius.** All of it is in this repository:
  - `internal/ghub` — the shared event list (`types.go:118-124`).
  - `internal/listener` — the payload struct, the `Delivery` type, `tickOne`, and `Serve`.
  - `internal/settings` — one new machine-wide field.
  - `internal/loopcmd` — the periodic pass, the agent-free rebase, and the link refresh.
  - `internal/worktree` — git commands with a deadline.
  - `internal/store` — one new dispatch kind, and the deletion of dead `pr_links` rows.
  - `README.md` and `docs/configuration.md` — the new trigger, the new field, the new behavior.
- **Prior art.** Every part exists.
  - `worktree.Manager.EnsurePR` (`internal/worktree/worktree.go:75`) fetches the head ref and
    checks it out detached. This is the worktree the rebase runs in.
  - `worktree.Manager.Dirty` (`worktree.go:140`) reports uncommitted work and unpushed commits.
  - `store.Store.PRLinks` (`internal/store/store.go:1110`) returns each issue-to-pull-request
    row with its head ref, base ref, and behind count.
  - `listener.Scan` (`internal/listener/route.go:120`) walks every registered project and
    returns every loop.
  - `Worker.armTend` collapses a burst of triggers into one sweep.
- **Contradiction scan.** The code agrees with the operator's report. The koinos project shows
  the failure directly. Its three open pull requests (`mcgarylabs/koinos-monorepo` #92, #93,
  #94) were last touched on 2026-08-24. Pull requests #93 and #94 are three commits behind
  `master`. Their issues (#76 and #91) carry the review label. The project last ticked on
  2026-08-25, because the repository has had no delivery since, and this machine has no
  `crontab` entry. The merge sweep is the only tend trigger the daemon has, and no merge has
  happened.
- **Prior decision, now reversed.** The merge-sweep spec
  (`docs/superpowers/specs/2026-08-22-merge-triggered-tend-sweep-design.md`) recorded that a
  periodic sweep was out of scope "by the operator's decision". The operator has now asked for
  it. This design supplies it.

**Outcome: the premise holds.** The change is additive. It invents no policy that
`internal/engine` does not already own.

- **Class: Large.** It adds a subsystem (the periodic tender), a machine-wide configuration
  field, a dispatch kind, and an automatic force-push.
- **Profile: backend.** The repository has no user interface.

## Goals

1. A push to a loop's default branch must arm the tend sweep. A merge is not the only event that
   makes the other pull requests stale.
2. The daemon must find a stale pull request without a delivery. A quiet repository must not
   strand its pull requests.
3. The detection must cost no GitHub API call when nothing is behind.
4. A clean rebase must cost no agent and no tokens.

## Non-goals

- This change does not replace `agent-utils project loop tick`. The periodic pass runs only
  while the listener runs. Cron stays the safety net for a machine with no daemon.
- This change does not make the periodic pass do a full reconcile. It decides tends only.
- This change does not resolve a rebase conflict without an agent.

## Section 1: sweep on a push to the default branch

### The change

`ghub.HookEvents` gets a sixth event, `"push"`. The list is declared once, and both
`register-webhook` and the handler read it, so the subscription and the drop check cannot
diverge.

The handler rejects a delivery that carries no issue number (`handler.go:502-509`). A push
payload carries none. The rule becomes conditional:

- A `push` delivery is valid with number 0.
- Every other event keeps the current rule and gets a 400 when the number is absent.

`listener.Delivery` gets one field, `PushedTo string`. The handler fills it from the payload's
`ref` field. It removes the `refs/heads/` prefix and keeps the branch name. A ref that is not a
branch (a tag, for example) leaves the field empty. The value passes `ghub.SafeRef` before it is
kept, exactly as `MergedInto` does (`handler.go:524-528`). A value that fails the check leaves
the field empty, and an empty field arms nothing.

`Delivery` gets one method, `IsPushTo(branch string) bool`. It mirrors `IsMergeInto`
(`work.go:366`): an empty branch never matches, even against an empty `PushedTo`.

In `Worker.tickOne` the arm becomes:

```go
if cfg.TendPR && (d.IsMergeInto(cfg.DefaultBranch) || d.IsPushTo(cfg.DefaultBranch)) {
    w.armTend(ctx, t, cfg.DefaultBranch)
}
```

A push delivery has no subject issue. `tickOne` therefore skips the issue pass, the epic pass,
and the cleanup pass when `d.Number` is 0. Each of those passes acts on one issue, and a push
names none.

### Why the merge trigger stays

A merge into the default branch also produces a push event, so the two triggers overlap. The
merge trigger stays for two reasons. A hook that nobody re-registers keeps the old event list,
and the merge path keeps working for it. `armTend` collapses the two triggers into one sweep,
so the overlap costs nothing.

### Cheap rejection

A push to a branch that is not any loop's default branch must cost nothing. Today the only place
that knows `cfg.DefaultBranch` is `tickOne`, which runs after `w.Open`. `Open` reads the token,
opens a SQLite handle, and runs the migration check. A busy feature branch would pay that on
every push, for every loop of every project that watches the repository.

`listener.Target` therefore carries the two facts the filter needs: `DefaultBranch string` and
`TendPR bool`. This costs no extra file read. `config.List` already calls `config.Load` on each
loop file and keeps only two of its fields (`internal/config/discover.go:171-176`); `config.Entry`
and `Target` each gain the two fields from the config that is already in hand.

`Worker.Deliver` then drops a push delivery for a target whose `TendPR` is false, or whose
`DefaultBranch` is not the pushed branch, **before** it calls `Open`. The same two fields let the
periodic pass in Section 2 select its loops without opening anything.

### Logging

The "accepted delivery" line names the issue number, because the number is the whole scope of
the work (`handler.go:611-616`). A push has no number. The line prints `ref` in place of
`number` for a push delivery. An operator must be able to read why a sweep started.

## Section 2: the periodic tend check

### Where it runs

`Worker.Serve` (`work.go:1398`) already has a timer loop. That timer is driven by retry
deadlines through `Worker.Wake`, and it has a 30-second floor. The tend check needs a different
cadence and has a different reason to fire, so it gets its own ticker in the same `select`.

### Configuration

`settings.Settings` gets one field:

```yaml
tend_interval: 15m   # 0 disables the periodic check
```

The field is machine-wide, beside the webhook block, because it describes how attentive the
daemon is. It does not describe a loop. `settings.WithDefaults` fills in `15m` when the field is
absent. A value of `0` disables the periodic check and leaves every other trigger unchanged.

### What one tick does

The pass iterates `listener.Scan()`. That function walks the registry, not the deliveries, so it
reaches a project whose webhook is missing or broken. This is what covers the koinos failure. It
selects its loops from `Target.TendPR`, which Section 1 adds, so a loop that does not tend costs
one field test and nothing else.

For each loop with `tend_pr: true`:

1. Call `deps.Fetch()`. This is the existing `git fetch origin --prune` on the primary checkout.
   It is local and costs no API call.
2. Read the loop's rows with `deps.Store.PRLinks(cfg.Name, cfg.Repo)`.
3. For each row, resolve `origin/<head_ref>`. A ref that no longer resolves is skipped: the
   prune in step 1 removed it, so the branch is gone.
4. For each row that resolves, count the commits the base has and the head does not:
   `git rev-list --count origin/<head_ref>..origin/<base_ref>`.
5. **If no row is behind, stop.** The pass makes no GitHub call. This is the common case.
6. If any row is behind, call `ListOpenPullRequests` and `ListOpenIssues`. Confirm each candidate
   against the answer:
   - the pull request is still open;
   - its base ref is still the loop's default branch;
   - its head ref still matches the row;
   - the issue still carries the review label.
7. Call `w.armTend` with the loop's default branch. The sweep then runs the existing
   `loopcmd.TendSweep`, which takes the loop lock and re-reads what it needs.

The local step is a **gate**, not a decision. It decides only whether to spend the API calls.
`TendSweep` stays the one place that decides what to dispatch, so the local cache can never cause
a dispatch on its own.

### The cold cache and the drifted cache

The gate can skip the API calls only when it has rows to trust. Two states break that:

- A loop with no rows has nothing to check, and would stay silent forever.
- A row can be stale. A pull request can close, or an issue can lose its review label, with no
  delivery to record it.

The pass therefore runs the confirm step unconditionally in two more cases:

- on the first tick after the daemon starts, for each loop;
- every six hours for each loop.

The `Worker` holds the last confirm time for each loop in memory. This adds no column and no
table. A daemon restart costs one refresh for each loop, which is two API calls.

The confirm step also **deletes** the rows for pull requests that are no longer open. Nothing
deletes a `pr_links` row today. The koinos project carries six rows for pull requests that
merged days ago. A new `store.Store.DeletePRLink(loop, repo, number)` method does the deletion.

### Interaction with the other triggers

The pass ends by calling `armTend`, so a periodic detection and a merge that lands ten seconds
later collapse onto one timer and produce one sweep. `TendSweep` takes the loop lock itself, so a
periodic pass and a delivery-driven pass cannot interleave.

## Section 3: the agent-free rebase

This section was not presented in the design conversation. It needs the operator's review.

### The change

`loopcmd.act` dispatches every decision. For a `KindTend` decision it now tries git first:

1. Try the rebase with git.
2. A clean rebase ends the work. No agent runs, and no token is spent.
3. A conflict aborts the rebase and dispatches the tend agent exactly as today.

The behavior belongs to the tend **action**, not to the trigger, so both the merge sweep and the
periodic pass get it.

### The steps

A new file, `internal/loopcmd/rebase.go`, holds one function:

```go
func tryRebase(ctx context.Context, cfg *config.Config, deps Deps, d engine.Decision) (done bool, err error)
```

It runs these steps in order:

1. **Worktree mode.** Return `false` at once when `cfg.Agent.Worktree` is not `per_issue`. A
   shared checkout has no pull-request worktree to rebase in.
2. **Worktree.** Call `deps.WT.EnsurePR(d.PR, d.HeadRef)`. The method fetches the head ref and
   checks it out detached, so the worktree holds exactly what the remote holds.
3. **Lease.** Record the commit the worktree now points at. This value is the lease for step 7.
4. **Dirty check, BEFORE the worktree is refreshed.** `EnsurePR` runs `checkout --detach
   FETCH_HEAD` on an existing worktree, which orphans any commits an agent left there, and
   `Dirty`'s unpushed-commit test cannot see anything in a detached worktree anyway. The check
   therefore runs on the worktree path first, and steps 2 and 3 follow it. A dirty worktree
   dispatches the agent.
5. **Rebase.** Run `git rebase origin/<base_ref>`.
6. **Conflict.** On any failure, run `git rebase --abort` and return `false`. The caller then
   dispatches the agent. The abort is unconditional, so the worktree is never left mid-rebase.
7. **Push.** Run
   `git push --force-with-lease=<head_ref>:<lease> origin HEAD:refs/heads/<head_ref>`.
   The lease pins the push to the commit this pass fetched. Git refuses the push when the remote
   has moved, so a branch that somebody pushed to in the meantime is never overwritten.
8. **Refused push.** A refused push settles the decision and **dispatches no agent**, but counts
   as no rebase. The remote moved while this pass ran, so the branch state this pass reasoned
   about is gone. The next tick reads the new state and decides again. Sending an agent at a
   branch somebody is actively pushing to is the more dangerous answer.
9. **Record.** Write the outcome (see below) and report that git rebased the branch.

The plan returns a three-valued outcome rather than a bool, because "no agent" and "rebased" are
not the same fact: a refused push means the first and not the second, and reporting it as a
rebase would overstate what happened in the tick summary an operator audits.

The remotes are SSH (`git@github.com:...`), so the push uses the operator's existing SSH key.
This adds no credential handling.

### Guards

Two guards apply, and they are the two the operator chose.

- **`--force-with-lease` pinned to the fetched ref.** This is the guard that stops the pass from
  destroying work. It is checked by git, not by this program.
- **No live dispatch for the issue or the pull request.** `engine.Decide` already suppresses a
  tend decision while a dispatch for that issue is live, and `tendDispatch` builds that liveness
  from the running rows (`tendsweep.go:196-215`). `tryRebase` runs after the decision, so the
  guard already holds. The rebase must never rewrite a branch under a running agent.

Two guards were considered and **rejected by the operator**:

- **No "human commits" check.** The pass does not inspect commit authorship.
  `--force-with-lease` already refuses the push that would lose work.
- **The clean rebase does not honour a veto label, a stopped session, or a parked issue.** A
  rebase spends no token and writes no label, so a paused issue still gets a current branch.
  Those refusals continue to stop every **agent** dispatch, including the conflict escalation in
  step 6, because `engine.Decide` applies them before `act` ever runs.

### The record an agent-free rebase leaves

A rebase that runs no agent creates no dispatch row today, so it would appear in neither
`agent-utils project logs --list` nor `agent-utils sessions list`. An operator would see a
force-push with no local record of what caused it.

**The decision: write a dispatch row with a new kind, `store.KindRebase = "rebase"`, in a single
INSERT that is already finished.** Two statements would leave a window, and a permanent stuck row
if the second failed. That row would not be inert: three separate places treat a running row of
any kind other than `tend` as a live agent (`internal/engine/engine.go:29-45`,
`internal/loopcmd/tick.go:281`, `internal/loopcmd/tendsweep.go:196-215`), so a stuck one would
freeze its issue against every future decision. The row carries:

- the issue number, the pull request number, and the title, as a tend row does;
- an **empty** session identifier;
- status `succeeded`, exit code 0, cost 0;
- `started_at` and `finished_at` set to the same pass.

This choice reuses the surfaces that exist:

- `agent-utils project logs --list` shows the row, so the force-push has a cause an operator can
  read.
- `agent-utils sessions list` is unaffected. `sessionsFrom` skips a dispatch with an empty
  session identifier (`internal/loopcmd/sessions.go`), so a rebase never appears as a session
  and never distorts a session's cost.
- Nothing reaps the row, because it is written finished.

`loopcmd.Summary` gets one counter, `Rebased int`, beside `Tended`. The sweep's completion line
then separates the two: how many pull requests git rebased, and how many needed an agent.

### Timeouts

`worktree.Manager` runs git with `exec.Command` and no deadline (`worktree.go:171`). A hung
`git push` in the daemon would hold the loop lock and stall the tend ticker. The new git
operations therefore run through `exec.CommandContext` with a deadline of two minutes for each
command. The existing methods keep their current behavior; only the rebase path takes the
context.

## Testing

The repository's tests use fakes for GitHub and seams for git and for time. This change follows
that pattern.

- **Handler.** A push delivery with no issue number is accepted. A delivery of any other event
  with no number is still a 400. A push to a branch that is not the default branch arms nothing.
  A `ref` that fails `SafeRef` leaves `PushedTo` empty.
- **Worker.** `IsPushTo` matches only an equal, non-empty branch. A push delivery runs no issue
  pass, no epic pass, and no cleanup pass. A merge and a push that arrive together produce one
  sweep, not two.
- **Periodic pass.** A loop whose rows are all current makes no GitHub call — the fake client
  counts its calls, and the count must be zero. A loop with one row behind makes exactly two
  calls. A row whose branch no longer resolves is skipped. The first tick after start confirms
  even when nothing is behind. The six-hour refresh deletes a row whose pull request is closed.
- **The rebase.** A clean rebase pushes once and dispatches no agent. A conflicting rebase runs
  `--abort` and dispatches the agent. A refused push dispatches no agent. A dirty worktree
  dispatches the agent. A shared-worktree loop dispatches the agent.
- **The record.** A clean rebase writes a `rebase` dispatch row with an empty session
  identifier, and `sessionsFrom` ignores it.

## Deployment

1. Re-run `agent-utils project register-webhook` for every project. The event list gains `push`,
   and an existing hook keeps the old list until it is updated.
2. That step needs a token with `admin:repo_hook` **and admin on the repository**. The token in
   `~/.agent-utils/env` currently gets a 404 when it reads a hook it registered, because it holds
   `maintain`, not `admin`.
3. Restart the listener, so it picks up the new binary and the new `tend_interval`.

Until step 1 is done for a repository, the merge trigger and the periodic pass still work there.
Only the push trigger waits on the hook.
