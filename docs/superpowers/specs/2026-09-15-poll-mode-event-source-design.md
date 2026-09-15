# Polling as a second event source

A repository this machine has no ADMIN on cannot be given a webhook. Nothing
about the loops that watch it is different: they still want a tick when an
issue is labelled, a sweep when a pull request merges, a cleanup when one
closes. What they lack is the thing that tells them, because every one of
those facts reaches this daemon as a GitHub delivery today.

This design adds a second source of the same facts, derived by asking GitHub
what changed rather than being told.

## The shape

The `listener.Worker` is the runtime. It does not care how an event was
sourced, and must behave identically whichever source produced one. Two
sources feed it:

- the **HTTP handler**, which turns a verified GitHub delivery into a
  `listener.Delivery` and calls `Worker.Deliver`.
- the **poller**, which diffs GitHub's current state against a local snapshot,
  turns each difference into a `listener.Delivery`, and calls `Worker.Deliver`.

Everything downstream of `Deliver` -- routing to the loops that watch the
repository, the per-issue delivery window, the busy re-look, the issue pass,
the epic sweep, the tend arm, worktree cleanup, closure recording, the retry
schedule -- is reached through that one entry point and is not modified by this
work.

`Worker.Serve` is the runtime loop, and is likewise unchanged in character: the
retry wake, the periodic tend check, the orphan sweep and the startup closure
reconcile run exactly as they do today. It gains one `select` case, driven by a
poll ticker built the way `tendTicker` is -- a `PollInterval <= 0` returns a nil
channel, and a nil channel blocks forever, so a worker with polling off never
enters it.

Two commands, differing only in which source they start:

| command | HTTP server | poll ticker |
|---|---|---|
| `listener run` | yes | no (`PollInterval` stays 0) |
| `listener poll [interval]` | no | yes |

`listener poll` binds no port. It is the same daemon otherwise, which is the
property that makes a machine with no webhook behave like one with a webhook
rather than like a machine running cron.

## Where the code lives

New files in `internal/listener`: `poll.go` (the pass) and `pollsnap.go` (the
pure diff), beside `closures.go` and `orphansweep.go` -- every other source of
work is already a `Worker` method here. It cannot be its own package: it
consumes `listener.Routes` and produces `listener.Delivery`, and `Worker` calls
it, which is an import cycle.

## One pass

`w.pollPass(ctx)` walks `w.Scan()`'s `routes.ByRepo()`, taking one `access()`
(token and client) for the whole pass and checking `ctx.Err()` between
repositories, as `reconcileClosed` does -- a shutdown must not wait out every
repository on a many-project machine.

Per repository:

1. `GET /issues?state=all&sort=updated&direction=asc&since=<cursor>&per_page=100`,
   paginated. One call covers issues AND pull requests -- they share the
   endpoint as they share a number space -- and `updated_at` moves on a label
   edit, a comment, a close and a reopen.
2. Every subject whose state, labels or `updated_at` differ from its snapshot
   row is turned into a `Delivery` (see **Deriving the flags**) and delivered.
3. Any subject that is a pull request and has just become closed costs one
   `GET /pulls/{n}`, to learn merged-versus-closed and the base ref. Bounded by
   the closures inside one interval.
4. `GET /repos/{o}/{r}/commits/{default_branch}`. A head SHA that moved, and is
   not explained by a merge seen in step 3, produces one
   `Delivery{Repo, PushedTo: <default branch>}` with `Number: 0`. That is a
   valid delivery: `tickOne` guards every issue-shaped pass on `d.Number > 0`
   already, because the `push` event has always been numberless.

Roughly two requests per repository per pass, plus one per closed pull request.
At a one-minute interval that is about 120 requests per hour per repository
against a 5000-per-hour token. No conditional-request or ETag machinery: `since`
already reduces a quiet repository's response to an empty array, and the
saving would not pay for the transport.

## State

Two tables, added to the existing `CREATE TABLE IF NOT EXISTS` block in
`internal/store/store.go`:

```sql
CREATE TABLE IF NOT EXISTS poll_subjects (
  repo       TEXT    NOT NULL,
  number     INTEGER NOT NULL,
  is_pr      INTEGER NOT NULL,
  state      TEXT    NOT NULL,
  merged     INTEGER NOT NULL,
  labels     TEXT    NOT NULL,
  updated_at TIMESTAMP NOT NULL,
  PRIMARY KEY (repo, number));

CREATE TABLE IF NOT EXISTS poll_cursors (
  repo      TEXT PRIMARY KEY,
  since     TIMESTAMP NOT NULL,
  head_sha  TEXT NOT NULL,
  seeded_at TIMESTAMP NOT NULL);
```

Keyed by repository, NOT by project, so they are `*store.DB` methods rather
than `*store.Store` ones. A poll observes GitHub, which is a machine-wide fact
about a repository; `Deliver` fans out to the projects afterwards, exactly as
it does for a webhook. `closures` is keyed by project for the opposite reason:
its rows are joined against dispatch rows, which are a project's.

`repo` is stored lowercased. Two projects may spell one repository differently
in their yaml, and `ByRepo` already folds case for that reason; a snapshot that
did not would diff each spelling against its own empty history and deliver
everything twice.

`labels` is the issue's labels lowercased, sorted and comma-joined -- a value
that changes when the set changes and not when the order does.

### The cursor is inclusive

`since` is the greatest `updated_at` the last pass processed, and the boundary
is deliberately re-read on every pass. GitHub's `since` is
greater-than-or-equal, and a subject whose snapshot row is unchanged produces
no delivery, so re-reading costs a row comparison and closes the window where a
second subject updated inside the same second as the cursor would be skipped
forever.

This is the property that makes a state diff the right shape here rather than
an event feed: a replay is free, so the poller never has to be certain it read
something exactly once.

### The first pass seeds

A repository with no `poll_cursors` row has its snapshot written from the
current listing and delivers NOTHING, logging how many subjects it seeded. A
fresh install must not dispatch an agent for every issue in the repository's
history.

Once a cursor exists, every difference is delivered however old it is -- a
daemon that was down for a day catches up on its next pass. This is the same
division `reconcileClosed` draws: a catch-up pass establishes what is true, and
only changes after that point are work.

## Deriving the flags

`pollsnap.go` holds this as a pure function -- `(snapshot row, fresh subject)
-> (Delivery, bool)` -- so the table below is testable without a GitHub client,
a database or a worker.

| observed | delivery |
|---|---|
| number not in the snapshot, or `updated_at` moved, or labels differ | `{Repo, Number}`, the base tick |
| open -> closed, not a pull request | `ClosedIssue: true` |
| open -> closed, is a pull request | `ClosedPR: true` |
| closed -> open | `Reopened: true` |
| pull request closed with `merged_at` set | `MergedInto: <base ref>` |
| `epic.ReadyLabel` absent -> present, issues only | `EpicReady: true` |
| default branch SHA moved, unexplained by a merge this pass | `PushedTo: <default branch>` |

`ClosedIssue` and `EpicReady` are narrowed to issues for the reason the handler
narrows them: issues and pull requests share a number space, so setting either
from a pull request would sweep the epic of whichever issue carries that
number.

One `Delivery` per number, carrying every flag that applies. A webhook sends one
`issues` event per label, which is why the per-issue delivery window exists; a
poll sees the edit whole and arrives at the state that window is built to reach.
The window stays in the path unchanged -- it is harmless against a single
delivery, and it still collapses a poll against a concurrent webhook on a
repository that has both.

## Changes outside the poller

All four are in `internal/ghub`, and all four exist because every caller before
this listed OPEN issues and OPEN pull requests only:

- `PullRequest` gains `Merged bool` and `State string`, set in `convertPR`.
  Without `Merged` there is no `MergedInto`, and a merge arms no tend sweep.
- A new `Subject` type -- `{Number, IsPullRequest, State, Labels, UpdatedAt}`
  -- and `ListSubjectsUpdatedSince(ctx, owner, repo, since) ([]Subject, error)`:
  `state=all`, `sort=updated`, `direction=asc`, paginated.

  `Issue` is deliberately NOT reused and `ConvertIssues` is NOT touched.
  `ConvertIssues` DROPS pull requests (`gi.IsPullRequest()`), and every caller
  it has depends on that; teaching it to keep them, behind a new flag, would
  put a pull request into the epic sweep's issue lists. `ListOpenIssues` is
  unusable here for a second reason besides: it is open-only and unordered, and
  a close is exactly what a poll must see.
- `BranchHead(ctx, owner, repo, branch) (string, error)`, returning the head
  commit SHA. `BehindBy` compares two refs and answers a count, which is a
  different question.

None of this reaches the `ghub.Client` INTERFACE. The two new methods are on
`*GitHubClient` only, and the poller reaches them through its own narrow
interface:

```go
type PollSource interface {
    ListSubjectsUpdatedSince(ctx context.Context, owner, repo string, since time.Time) ([]ghub.Subject, error)
    BranchHead(ctx context.Context, owner, repo, branch string) (string, error)
    PullRequest(ctx context.Context, owner, repo string, number int) (ghub.PullRequest, error)
}
```

`Worker` gains `NewPollSource func(token string) PollSource`, wired to
`ghub.New` in `NewWorker`, exactly as the existing `NewClient` seam is. Widening
`ghub.Client` instead would force two methods onto every fake in `loopcmd` and
`listener` that implements it, for the benefit of one caller.

## Command surface

```
agent-utils listener poll [interval]
```

The interval is a positional Go duration, defaulting to `1m`. Below a 30s
floor it is REJECTED with a message naming the floor, not silently clamped --
`tend_interval` clamps because it is a stored setting a user may not be
watching when it loads, and an argument typed at a prompt has someone reading
the reply.

There is no `poll_interval` setting, no `config set` key and no generated yaml
line. The interval is the argument.

The command takes its own `listener.poll.lock` in `~/.agent-utils`, so a second
`listener poll` fails fast the way a second `listener run` does. It does NOT
contend for `listener.lock`: running both is a supported (if redundant)
configuration, the per-loop lock already makes overlapping ticks harmless, and
blocking here would turn "a daemon is up" into "the poller refuses to start".

On start it prints the same routing table `listener run` prints, with the poll
interval instead of a bind address. A poller that is running and watching
nothing is the failure this makes visible.

## Failure handling

A repository whose listing fails is logged and skipped, and its cursor is NOT
advanced; the next pass re-reads the same window. One unreachable repository
must not stop the others, and must not silently lose the interval it covered.

Within a repository, subjects are processed in ascending `updated_at`, and
neither the snapshot nor the cursor is written until the whole repository has
been read. A failure part way through therefore RESTARTS that repository's
window on the next pass rather than resuming inside it -- which costs one
re-read and is correct, because re-reading is free by construction: a subject
whose snapshot row is unchanged produces no delivery. Ascending order is what
makes the cursor a single high-water mark rather than a set.

A `Deliver` that fails is not the poller's problem: the retry schedule inside
the runtime owns it, identically to a webhook delivery.

## Testing

- `pollsnap.go`'s diff: table tests over every row of **Deriving the flags**,
  plus the cases that must produce nothing (an unchanged subject re-read at the
  inclusive boundary; a label set reordered).
- `pollPass` against a stub GitHub client and a stub `Deliver` sink, the way
  `work_test.go` already drives `Deliver` through stub hooks:
  - a first pass seeds and delivers nothing; the second delivers a real change
  - a merge plus the SHA move it causes arms ONE tend, not two
  - a failing repository does not stop the next one, and does not advance its
    own cursor
  - a closed pull request costs exactly one `GET /pulls/{n}`
- The command: the interval parses, the floor is rejected, the lock is
  exclusive, and `listener run` still leaves `PollInterval` at zero.

## Deliberately not built

- A one-shot `poll --once` for a machine with no daemon. The pass is a method;
  exposing it is cheap if that machine ever exists.
- ETag or conditional requests.
- Per-repository opt-in. Every repository any loop watches is polled.
- Bare review submissions carrying no comment body stay invisible to a poll:
  they move no `updated_at` this design reads. The periodic tend check already
  covers the staleness they would signal.
