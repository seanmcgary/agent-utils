# Design: merge-triggered tend sweep

## Premise and blast-radius check (step-0 findings)

The daemon rebases a stale pull request with a tend agent. Today a tend decision is reachable
only for the one issue a delivery names, or from the full sweep in `loopcmd.Tick`. This change
makes a merge into the default branch start a tend-only sweep across the loop.

- **Entry path.** GitHub sends a webhook. `internal/listener/handler.go:421` drops any event
  outside `ghub.HookEvents`. The handler takes one number from the payload
  (`handler.go:501`) and hands the delivery to a bounded pool. `Worker.Deliver`
  (`internal/listener/work.go:200`) fans out to each loop that watches the repository and calls
  `tickOne`, which calls `w.RunIssue`. Production wires `RunIssue` to `loopcmd.TickIssue`
  (`work.go:182`).
- **Blast radius.** `internal/listener` (the payload struct, the `Worker` seams, the fan-out)
  and `internal/loopcmd` (a new sweep beside `Tick` and `TickIssue`). The sweep reads
  `internal/engine` and `internal/ghub` and adds nothing to either. `docs/configuration.md`
  records the new behavior. There is no schema change, no new configuration field, and no new
  dependency. All of it is in this repository.
- **Prior art.** Every part exists. `ghub.Client` supplies `ListOpenPullRequests` and
  `BehindBy` (`internal/ghub/ghub.go:23-24`). `engine.LinkPR` links an issue to its pull
  request. `engine.Decide` already makes the tend decision (`internal/engine/engine.go:284`).
  `loopcmd.act` dispatches it. The sweep composes these; it invents no policy.
- **Contradiction scan.** The code agrees with the operator's description. A merge does reach
  the daemon today, and it does start a tick — but that tick is scoped to the merged pull
  request's own issue, which is the one issue tending cannot help.

### The finding that shapes this design

`internal/listener/work.go:140-144` records a past regression:

> RunIssue acts on ONE issue, taking the loop's lock first. It is `loopcmd.TickIssue`, never
> `loopcmd.RunTick`: the daemon answers events, and an event names an issue. The full
> reconcile is the cron sweep's job — see `loopcmd.Tick` — and running it per delivery burned a
> token budget on every open issue of every project watching the repository.

`Deliver`'s own comment (`work.go:205`) names the symptom: opening one unlabelled test issue
dispatched a tend agent for an unrelated issue whose pull request was 16 commits behind.

This change makes a delivery act on more than one issue again. That is the thing the earlier
fix removed, so the design must not undo it. Three limits keep the two apart:

1. The sweep runs for **one event only** — a merged pull request whose base is the loop's
   default branch. Opening an issue, moving a label, or commenting starts no sweep.
2. The sweep keeps **tend decisions only**. Every other decision kind is dropped before
   anything is dispatched.
3. Tending an out-of-date pull request is the **correct** response to the default branch
   moving. The earlier incident was a tend agent dispatched because an unrelated issue was
   opened. Here the cause and the effect match: the base moved, so the branches behind it are
   rebased.

### Second finding: no periodic sweep runs on this machine

`loopcmd.Tick` is the full sweep. It runs from `agent-utils project loop tick`, which an
operator schedules. On the machine in question there is no `crontab` entry, no launchd job, and
no other scheduler. The only driver is `agent-utils listener start`, which calls `TickIssue`
only. `loopcmd.Tick` therefore never runs, and `reviewPR`'s comment — "The cron sweep still
lists open pull requests and finds a new one" — describes a fallback that is not present.

A periodic sweep is **out of scope for this change**, by the operator's decision. This section
records the gap so a later reader does not read the merge trigger as a complete answer to pull
request staleness.

## Verified external API (do not re-derive)

Read from source in this repository on 2026-08-22.

`internal/ghub/ghub.go:15-27` — the client interface the sweep uses:

```go
ListOpenIssues(ctx context.Context, owner, repo string) ([]Issue, error)
ListOpenPullRequests(ctx context.Context, owner, repo string) ([]PullRequest, error)
BehindBy(ctx context.Context, owner, repo, base, head string) (int, error)
```

`internal/lock/lock.go:31` — `Acquire` uses `syscall.LOCK_EX|syscall.LOCK_NB` and returns
`ErrHeld` at once when another holder has the lock. It never waits.

`internal/engine/engine.go:17` — `Decide(cfg, snap, st, now) Plan` is pure. It reads
`st.Running` to build two guards: `liveIssues`, keyed by issue number, and `liveTendPRs`, keyed
by pull request number (`engine.go:26-34`). A live tend dispatch for a pull request suppresses a
second tend decision for it (`engine.go:277`).

`internal/runner/runner.go` — `finish` writes issue state only when `d.Kind != store.KindTend`.
A tend dispatch holds no issue state, so retiring a tend row writes no retry flag.

GitHub's `pull_request` payload carries `action`, `pull_request.merged` (bool) and
`pull_request.base.ref` (string). A merge arrives as `action: "closed"` with `merged: true`.

## Behavior

### The trigger

The handler decodes two more fields from the payload: `pull_request.merged` and
`pull_request.base.ref`. A delivery is a **merge delivery** when all three are true:

- the event is `pull_request`;
- the action is `closed`;
- `merged` is `true`.

The handler does **not** compare the base ref to a branch name. Each loop carries its own
`default_branch`, and one repository can have several loops. The handler has no loop
configuration, so it passes the base ref down and the comparison happens per loop.

The base ref is attacker-controlled text. The handler bounds it before it is logged, exactly as
it bounds the label name and the title today.

### The fan-out

`Deliver` keeps its present behavior. The merged pull request's own issue still gets its
`TickIssue`. That pass is what moves the issue to its terminal state; the sweep does not replace
it.

After that pass, and for the same target, the worker runs the sweep when both are true:

- `cfg.TendPR` is `true`;
- the delivery's base ref equals `cfg.DefaultBranch`.

The `execution` loop has `tend_pr: true` and sweeps. The `planning` loop has `tend_pr: false`
and never does.

### The sweep

`loopcmd.TendSweep` takes the loop's lock, then:

1. Fetches the primary checkout. A failed fetch makes branch comparison stale, so the sweep
   stops. It has nothing else to do.
2. Lists open issues and open pull requests. For each issue that carries `labels.review` and
   links to a trusted pull request, it asks `BehindBy` and records the result.
3. Reads issue states and running dispatches.
4. Retires dead **tend** rows only (see Scope limits).
5. Calls `engine.Decide` — the same function the full tick calls.
6. **Drops every decision whose kind is not `KindTend`.**
7. Acts on what remains.
8. Records the tick, so `project loop status` does not read the loop as idle.

### Scope limits

Each limit exists because a wider pass would repeat a failure the codebase already documents.

- **Retire tend rows only.** `tickissue.go:112` states that retiring the loop's rows on a
  delivery would flag issues nobody touched for retry, and would let the next pass start a
  second agent in a worktree that already holds one. A tend row does not carry that hazard:
  `runner.finish` writes no issue state for `store.KindTend`. Rows of every other kind are still
  **read**, because a live start agent must keep suppressing tend for its issue. They are never
  retired.
- **Do not write a cooldown.** The circuit breaker counts retry decisions within one call
  (`tickissue.go:311`). This pass discards retry decisions. A pass that will not act on that
  evidence must not stop the passes that would.
- **Do not clear unreachable deadlines.** `tickissue.go:146` gives the reason: the sweep
  examines review-labeled issues, so it holds no evidence about any other row.

### Repeat deliveries

No debounce is added. Two mechanisms already make a burst of merges safe:

- The loop lock is non-blocking. A second sweep that arrives while the first still holds the
  lock gets `lock.ErrHeld`, which `tickOne` logs and drops with no retry (`work.go:378`).
- `engine.Decide` suppresses a tend decision for any pull request that already has a live tend
  dispatch (`engine.go:277`).

A burst therefore costs a few more GitHub reads and no more agents. A timer would add mutable
state to `Worker`, whose fields are otherwise written once before it is shared across goroutines
(`work.go:118-122`), and would delay the first rebase for no gain.

### Error handling

- A failed `BehindBy` for one pull request logs a warning and the sweep continues. `Tick` gives
  the reason (`tick.go:124`): a single unusable pull request must not stop the pass, or anyone
  able to open a pull request could stall the loop.
- A failed sweep is logged. It does not fail the delivery and schedules no retry. The delivery's
  own `TickIssue` pass keeps its present retry behavior, and the next merge sweeps again.
- `lock.ErrHeld` is not a failure. It is logged as a skip.

## Testing

`Worker`'s collaborators are fields, so the listener tests need no registry, database, GitHub
token, or real clock. `RunSweep` becomes a seam beside `RunIssue`.

Cases to pin:

- A merged pull request whose base is the loop's default branch runs a sweep.
- A merged pull request whose base is another branch runs no sweep.
- A closed but unmerged pull request runs no sweep.
- An `issues` or `issue_comment` delivery runs no sweep.
- A loop with `tend_pr: false` runs no sweep.
- The merged pull request's own `TickIssue` still runs, in every case above.
- The sweep drops a non-tend decision that `engine.Decide` returns.
- The sweep retires a dead tend row and leaves a dead start row alone.
- The sweep writes no cooldown when the breaker trips.
- A second delivery that finds the lock held is skipped without a retry.

## Out of scope

- A periodic tend sweep, or any scheduler inside the daemon.
- Restoring a cron or launchd entry for `loopcmd.Tick`.
- Retiring dead non-tend runners loop-wide, which no pass does today when no delivery arrives
  for their issue.
