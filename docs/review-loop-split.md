# Propagating the review/remediation split to a live project

`examples/` now carries four loops instead of three: `pr-review` reviews and stops, and a new
`exec-pr-review-findings` applies what it found. This document is what to change in a project
already running the old three-loop chain, in what order, and what to do with work that is
in flight when you change it.

**Nothing here has been applied.** The four live configurations —
`~/Documents/Claude/Projects/{Koinos,LawnDominator,ProjectWrangler,SnootSnap}/code/*/.agent-utils/configs/`
— are untouched by the change that added this file. Read the examples first, then apply.

## What is different, in one table

| | Before | After |
|---|---|---|
| `pr-review` | reviews, then fixes what it finds, then applies `status:ready-for-review` | reviews, posts a findings comment, applies `status:ready-for-findings-exec`, stops |
| remediation | inside the review session, in a context that grew to 321k tokens | a separate dispatch, one fresh subagent per **file** group |
| findings | prose in a disposition table, re-located by whoever fixed them | anchored to `File:Line` with a prescribed fix, grouped by file |
| handoff | none — one session did both | a comment ID in Pipeline State's `findings comment` field |
| `status:ready-for-review` | applied by `pr-review` | applied by `exec-pr-review-findings` — same meaning, later in the chain |

The measurement behind it: over eight real sessions the reviewer fan-out cost $8.39 and the
remediation that followed cost $90.86. A quarter of remediation's tool calls were re-reading code
the reviewers had already read.

## The three new labels

Create these in **each** repository before touching any configuration file:

```bash
for r in koinos lawndominator projectwrangler snootsnap; do
  gh label create status:ready-for-findings-exec --repo "mcgarylabs/$r-monorepo" \
    --description "Reviewed; findings posted and waiting to be applied"
  gh label create status:fixing-findings --repo "mcgarylabs/$r-monorepo" \
    --description "A findings-execution agent is applying the review's findings"
  gh label create status:needs-findings-input --repo "mcgarylabs/$r-monorepo" \
    --description "Findings execution parked and needs a human answer"
done
```

Verify before continuing — three per repository:

```bash
gh label list --repo mcgarylabs/koinos-monorepo | grep -c findings   # want 3
```

**Do this first, and do not skip the verification.** A trigger label that does not exist in the
repository produces no error anywhere: the tick lists every open issue, finds nothing carrying the
label, records "no trigger label is present", and idles forever. The failure surfaces instead when
an *agent* tries to apply the label — and it does that after it has already removed the previous
status label, which can leave an issue carrying no status at all.

## The per-project changes

Every live `pr-review.yaml` is a machine-normalised copy of the old example: same prompts
byte-for-byte, differing only in `repo`, `checkout_base_dir: .`, and `timeout: 8h0m0s`. So the
propagation is: take the new example's bodies, keep the three local values.

### 1. `pr-review.yaml` — in all four projects

| Field | From | To |
|---|---|---|
| `labels.review` | `status:ready-for-review` | `status:ready-for-findings-exec` |
| `labels.terminal` | absent | `status:ready-for-findings-exec` |
| `labels.veto` | `blocked:*`, `status:ready-for-execution`, `status:executing` | add `status:ready-for-findings-exec` and `status:fixing-findings` |
| `agent.effort` | `high` | `medium` |
| `agent.max_budget_usd` | `50` | `10` |
| `prompt` | `reviewing-commits`, "YOU FIX WHAT YOU FIND" | `producing-review-findings`, "YOU DO NOT FIX WHAT YOU FIND" — copy the example's body verbatim |
| `resume_prompt` | repeats "you fix what you find" | copy the example's body verbatim |
| header comment | says the loop "reviews and FIXES" | rewrite; it describes the old behaviour |

`agent.model` stays `opus`. That is deliberate and it is the one setting the split makes *more*
important: this loop's output is now a specification a cheaper model executes, so strength saved
here is paid for twice downstream by an executor working out what a finding meant. Effort is the
axis that drops, because the expensive part of the old loop was never the reviewing.

`labels.review` and `labels.terminal` are the same value on purpose. For a machine handoff there
is no interval between "output ready to read" and "left this loop", and writing
`status:ready-for-review` here would make an issue queued for remediation look identical to one
already fixed and waiting on you. It is safe only because `tend_pr` is false.

### 2. `exec-pr-review-findings.yaml` — new file, all four projects

Copy `examples/exec-pr-review-findings.yaml` and change `repo`, `checkout_base_dir: .`, and
`timeout` to match the project's other loops. **Copy the whole file**, not the fields listed in a
summary somewhere: `config.Load` is strict and unconditionally requires `name`, `repo`,
`checkout_base_dir`, `worktree_dir`, `default_branch`, both prompts, the full `retry` block
including `breaker.orphan_threshold` and `breaker.cooldown`, and — because
`permission_mode: bypassPermissions` is set — `i_understand_bypass_permissions: true`.

A file that will not load is not a local failure. `EntryLoop` refuses for the **whole
repository** when any loop file fails to load, and the webhook router drops that loop from
routing. Validate before going further (see step 3 of the migration).

### 3. `execution.yaml` — in all four projects

| Field | From | To |
|---|---|---|
| `labels.terminal` | absent | `status:ready-for-pr-review` |
| `labels.veto` | `blocked:*`, `status:pr-reviewing` | add `status:fixing-findings` |
| `agent.model` | `opus` | `sonnet` |

`agent.effort` is already `medium` in all four live files; only the example needed changing.
`max_budget_usd` is `0` (unlimited) in all four and this change does not touch it.

**The `terminal` line fixes a bug that predates this work.** `EntryLoop` picks the front of the
pipeline as *the loop whose trigger is no OTHER loop's terminal or review label*. With
`execution.yaml` declaring no terminal, `pr-review`'s trigger is nobody's terminal, so `planning`
and `pr-review` both resolve as entry loops, the resolution is ambiguous, and **the epic sweep is
already disabled in all four projects** — its only symptom is a `WARN` reading
`epic sweep skipped: cannot name the pipeline's entry loop`. Adding the line makes `pr-review`
downstream of `execution` and leaves `planning` alone at the front. Confirm afterwards that the
warning has stopped.

`status:ready-for-pr-review` is declared as `terminal` but deliberately **not** added to
`execution.yaml`'s `veto`. `veto` is checked before tend decisions too, so vetoing it would stop
the execution loop rebasing a branch that is queued for review — which is exactly a branch that
should keep being rebased while it waits.

### 4. Cron

Cron entries are per loop, so the new loop needs its own line in each project's crontab or it
never gets the sweep backstop. Mirror the existing lines:

```cron
*/15 * * * * . $HOME/.agent-utils/env && /usr/local/bin/agent-utils project --name lawndominator loop tick --name exec-pr-review-findings >> $HOME/.agent-utils/exec-pr-review-findings.log 2>&1
```

Nothing else needs registering. The webhook listener re-reads the registry and every configuration
file on every delivery and caches nothing, so a new file is live to webhooks the moment it is
written. `agent-utils project loop new` is the interactive wizard and is not required — though it
now offers `exec-pr-review-findings` as a template if you prefer to scaffold.

## Migration — the order is not negotiable

1. **Create the three labels** in every repository, and verify (above).
2. **Copy `exec-pr-review-findings.yaml`** into each project's `.agent-utils/configs/`.
3. **Validate it loads**, per project, before going further:
   `agent-utils project --name <p> loop status --name exec-pr-review-findings`
   must print the loop rather than an error.
4. **Add the cron line** for the new loop.
5. **Only now, swap `pr-review.yaml`** and edit `execution.yaml`. Keep a `pr-review.yaml.bak`.

The reverse order strands work **silently**: a review that finishes before the new loop exists
applies `status:ready-for-findings-exec`, and no loop watches it, and nothing logs that.

### Where in-flight work lands

The engine reads every open issue on every tick and selects purely on labels, so the swap takes
effect immediately and carries no state of its own. Every live state and what it needs:

| State at the swap | Where it lands | Action |
|---|---|---|
| `status:pr-reviewing`, dispatch running | Untouched. The dispatch record wins over labels, so the next tick skips the issue; the running agent keeps its **old** prompt, fixes what it found, and applies `status:ready-for-review` — the correct end state under either chain. | None while it runs. See the warning below if it dies. |
| `status:ready-for-pr-review`, queued | Dispatched on the next tick under the new review-only prompt. | None, if the ordering above was followed. |
| `status:ready-for-review` from the old flow | Nothing. The label means the same thing it always did — reviewed, fixed, waiting on you — and it is still what the execution loop tends. It is `review` for three loops and `trigger` for none, so no loop picks it up. | **None. Do not relabel these.** |
| `status:needs-review-input` | Idles. `blocked:*` does not match it and it carries no trigger, so it waits for you to re-add `status:ready-for-pr-review`. | `loop reset` first — see below. |
| Parked at the retry cap, or operator-stopped | Unaffected; none of their labels move. | None. |
| A live *tend* dispatch on a `status:ready-for-review` pull request | Unaffected. | None. |

**The one real hazard is a resumed session.** Dispatch state is keyed by loop name and repository,
not by labels, and `pr-review`'s `trigger` and `in_flight` names do not change in this migration —
so a dispatch that dies is marked for retry and the next tick **resumes the same claude session**
under the new review-only `resume_prompt`. That session's context says "you fix what you find" and
its branch holds half-applied fixes. The two ways out:

- **Preferred: drain.** Swap when no issue carries `status:pr-reviewing`.
- **If you swap hot:** for each such issue, once its agent has ended, run
  `agent-utils project --name <p> loop reset --name pr-review --issue N`
  so any later dispatch starts a clean session. Do the same for any issue sitting at
  `status:needs-review-input` **before** you re-add its trigger.

### Verifying the swap took

Per repository, after the swap:

- `gh issue list --label status:ready-for-findings-exec --json number,updatedAt` — entries should
  drain within one tick interval. Anything sitting there across two intervals means the new loop
  is not running: check step 3 and step 4.
- `agent-utils project --name <p> loop status --name exec-pr-review-findings` — the tick count
  should rise.
- The `epic sweep skipped: cannot name the pipeline's entry loop` warning should have stopped.

### Rollback

Rolling back is not just restoring the file, because three labels would be left watched by
nothing and nothing would say so.

1. **Drain first.** For each of `status:ready-for-findings-exec`, `status:fixing-findings` and
   `status:needs-findings-input`: `gh issue list --label <l>` and relabel each issue back to
   `status:ready-for-pr-review`.
2. `agent-utils project --name <p> loop reset --name exec-pr-review-findings --issue N` for any
   issue that has a stored session with that loop.
3. **Only then** restore `pr-review.yaml.bak` and delete `exec-pr-review-findings.yaml`. Removing
   the file first makes the listener resolve the loop as gone and, after enough consecutive
   observations, permanently clear its retry rows.
4. Remove the cron line. Leave the three labels in the repository; they cost nothing and they are
   what a second attempt needs.

## One invariant to keep

Three loops now declare `review: status:ready-for-review`, and exactly one of them tends it.
`exec-pr-review-findings` must keep `tend_pr: false`; giving it `true` would put two loops
rebasing the same pull request. And `status:ready-for-review` must never be added to any loop's
`veto`, because `veto` silently disables tending as well as dispatching, and that label is the
only thing keeping a pull request in your queue rebased.
