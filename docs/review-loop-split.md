# Propagating the review/remediation split to a live project

`examples/` now carries four loops instead of three: `pr-review` reviews and stops, and a new
`exec-pr-review-findings` applies what it found. The chain between them is also **automated** —
the execution and review agents apply the next loop's trigger themselves, so an issue takes
exactly two human touches instead of four. This document is what to change in a project already
running the old three-loop chain, in what order, and what to do with work that is in flight when
you change it.

**This is a behaviour change, not a label rename.** The execution loop's prompt changes: it now
applies `status:ready-for-pr-review` itself. Read that section before you decide when to swap.

**Nothing here has been applied.** The four live configurations —
`~/Documents/Claude/Projects/{Koinos,LawnDominator,ProjectWrangler,SnootSnap}/code/*/.agent-utils/configs/`
— are untouched by the change that added this file. Read the examples first, then apply.

Every "From" value below was read from the live files on 2026-09-01. Nothing in this repository
pins them, so `diff` each file against `examples/` before you edit it rather than trusting this
document to still be current.

## What is different, in one table

| | Before | After |
|---|---|---|
| human touches | four: approve plan, hand the branch to review, and read the result | **two**: approve the plan, merge at the end |
| `execution` | opens the PR, applies `status:ready-for-review`, stops; **you** apply `status:ready-for-pr-review` when you want it reviewed | opens the PR, applies `status:pr-open`, finishes, then applies `status:ready-for-pr-review` itself as its strictly last action |
| `pr-review` | reviews, then fixes what it finds, then applies `status:ready-for-review` | reviews, posts a findings comment, applies `status:ready-for-findings-exec`, stops |
| remediation | inside the review session, in a context that grew to 321k tokens | a separate dispatch, one fresh subagent per **file** group |
| findings | prose in a disposition table, re-located by whoever fixed them | anchored to `File:Line` with a prescribed fix, grouped by file |
| handoff | none — one session did both | a comment ID in Pipeline State's `findings comment` field |
| `status:ready-for-review` | applied by `execution` when it opened the PR, then again by `pr-review` — it meant both "a PR exists" and "your turn" | applied only by `exec-pr-review-findings`, at the very end. It means one thing: the pipeline is finished and it is your turn to merge |
| tending | keyed to `status:ready-for-review`, which happened to be present from execution onward | keyed to `status:pr-open`, which says exactly that and is applied for exactly that reason |

The measurement behind it: over eight real sessions the reviewer fan-out cost $8.39 and the
remediation that followed cost $90.86. A quarter of remediation's tool calls were re-reading code
the reviewers had already read.

## The four new labels

Create these in **each** repository before touching any configuration file:

```bash
for r in koinos lawndominator projectwrangler snootsnap; do
  gh label create status:ready-for-findings-exec --repo "mcgarylabs/$r-monorepo" \
    --description "Reviewed; findings posted and waiting to be applied"
  gh label create status:fixing-findings --repo "mcgarylabs/$r-monorepo" \
    --description "A findings-execution agent is applying the review's findings"
  gh label create status:needs-findings-input --repo "mcgarylabs/$r-monorepo" \
    --description "Findings execution parked and needs a human answer"
  gh label create status:pr-open --repo "mcgarylabs/$r-monorepo" \
    --description "A pull request exists for this issue; keep it rebased"
done
```

Verify before continuing — three `findings` labels plus `status:pr-open` per repository:

```bash
gh label list --repo mcgarylabs/koinos-monorepo --limit 200 | grep -c findings     # want 3
gh label list --repo mcgarylabs/koinos-monorepo --limit 200 | grep -c 'pr-open'    # want 1
```

**`status:pr-open` is the one that is easy to underestimate.** It is not cosmetic and it is not
optional: it is the execution loop's `labels.review`, which is what makes an issue eligible for
tending. If it does not exist when the swap lands, the execution agent's attempt to apply it fails
and **nothing rebases any pull request in the project** — silently, because an untended pull
request looks exactly like a fresh one until it is behind.

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
| `agent.max_budget_usd` | `50` | `0` — see the budget/timeout section below |
| `agent.timeout` | `8h0m0s` | `24h` (or delete the line) |
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

**This is the file whose behaviour changes, not just its labels.** Copy the example's `prompt` and
`resume_prompt` bodies verbatim, the same way you do for `pr-review.yaml`.

| Field | From | To |
|---|---|---|
| `labels.review` | `status:ready-for-review` | `status:pr-open` |
| `labels.terminal` | absent | `status:ready-for-pr-review` |
| `labels.veto` | `blocked:*`, `status:pr-reviewing` | add `status:fixing-findings` |
| `agent.model` | `opus` | `sonnet` |
| `prompt` | "ON COMPLETION. Open the pull request … add `{{.Labels.Review}}`" | a numbered six-step completion order ending with `{{.Labels.Terminal}}` as the strictly last action — copy the example's body verbatim |
| `resume_prompt` | silent on completion order | names the numbered order and the strictly-last terminal — copy verbatim |
| header comment | says the human applies `status:ready-for-pr-review` by hand | rewrite; that is now the agent's job |

`agent.effort` is already `medium` in all four live files; only the example needed changing.
`max_budget_usd` is already `0` in all four `execution.yaml` files, so only the `pr-review.yaml`
caps need clearing. `agent.timeout` is `8h0m0s` everywhere — see below.

**Why `labels.review` moves off `status:ready-for-review`.** The field does two jobs — "the agent
finished, go read it" and "this issue is eligible for tending" — and automating the chain pulls
them apart. Execution no longer has a human-facing output state: its work goes straight to
pr-review. But it is still the only loop that tends. Leaving `status:ready-for-review` here would
summon you the moment execution finished, before the branch had been reviewed or fixed at all,
which defeats the whole point of the change. `status:pr-open` keeps the tending and drops the
claim on your attention, and it is honest about what the agent is actually asserting when it
applies it: a pull request now exists.

The consequence to hold on to is that **nothing removes `status:pr-open`**. It is on the issue
from the moment the pull request opens until the issue closes, so tending covers every wait in the
chain — the review queue, the remediation queue, your gate at the end, and any park in between.
That is a wider tending window than the old configuration had, not a narrower one.

**The `terminal` line fixes a bug that predates this work.** `EntryLoop` picks the front of the
pipeline as *the loop whose trigger is no OTHER loop's terminal or review label*. With
`execution.yaml` declaring no terminal, `pr-review`'s trigger is nobody's terminal, so `planning`
and `pr-review` both resolve as entry loops, the resolution is ambiguous, and **the epic sweep is
already disabled in all four projects** — its only symptom is a `WARN` reading
`epic sweep skipped: cannot name the pipeline's entry loop`. Adding the line makes `pr-review`
downstream of `execution` and leaves `planning` alone at the front. Confirm afterwards that the
warning has stopped.

**This supersedes the warning currently in your `execution.yaml`.** That comment says
`status:ready-for-pr-review` "must NOT be" the execution loop's handoff, because the agent applies
its label before its last phase and would start the review on a branch it is still writing to. The
hazard is real and the comment was right about it. What has changed is the resolution: instead of
avoiding the chain, the prompt now defines *when* the label is applied. `labels.review` stays
early, because tending should start as soon as the pull request exists and that label is nobody's
trigger. `labels.terminal` is applied only after the final push, as the numbered last step, and
nothing follows it. Delete the old comment when you copy the new prompt in — leaving it beside a
prompt that does the opposite is worse than either.

**The prompt is the guard here, and there is no mechanical backstop for it.** If a project's
`execution.yaml` gets the new `terminal` but keeps the old prompt, the agent never applies it and
issues simply stop at the end of execution — visible, recoverable, and the safer failure. If it
gets the new prompt but the agent applies the terminal early anyway, two agents land on one branch.
That is why the prompt bodies are copied verbatim rather than summarised.

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

## Budgets and timeouts, in every loop file

Separate from the split, and applying to `planning.yaml` as well:

| Field | Live value today | New value |
|---|---|---|
| `agent.max_budget_usd` | `50` in `pr-review.yaml`; `0` already in `execution.yaml` | `0` everywhere |
| `agent.timeout` | `8h0m0s` in every file | `24h` |

**`max_budget_usd: 0` means no cost ceiling, and that is now the recommendation.** A cap does not
prevent an expensive run; it interrupts one, wherever it happens to be — often mid-edit and after
the last push — and the loop then retries from a resumed session and spends again on work the cap
just threw away. Cost is controlled by `model` and `effort`, which shape the whole run. The
examples write the `0` out rather than omitting the field, so it reads as a decision.

**`agent.timeout` is no longer required.** It now defaults to `24h`, so you may delete the line
instead of setting it. Either is fine; the examples set it explicitly for the same
documented-config reason. The timeout is a last resort on a wedged process, not a hang detector —
`retry.breaker.orphan_threshold` and `loop kill` handle a stuck dispatch on evidence — and
guessing low is the expensive mistake, because a killed dispatch is recorded failed and retried, so
a real long run reads as a flaky agent.

The four live files all carry explicit caps and `8h0m0s` timeouts written by the setup wizard.
None of that clears itself; you have to edit each one. Nothing breaks if you do not — the old
values remain valid — but a `pr-review.yaml` still capped at `50` will keep interrupting the runs
you were trying to make cheaper by other means.

## Migration — the order is not negotiable

1. **Create the four labels** in every repository, and verify (above).
2. **Copy `exec-pr-review-findings.yaml`** into each project's `.agent-utils/configs/`.
3. **Validate it loads**, per project, before going further:
   `agent-utils project --name <p> loop status --name exec-pr-review-findings`
   must print the loop rather than an error.
4. **Add the cron line** for the new loop.
5. **Swap `pr-review.yaml`.** Keep a `pr-review.yaml.bak`.
6. **Last, swap `execution.yaml`.** Keep an `execution.yaml.bak`.

Every step is downstream-first, and that is the whole rule: **never create a producer of a label
before its consumer exists.** Step 5 before step 2 strands a finished review at
`status:ready-for-findings-exec` with no loop watching it. Step 6 before step 5 hands a branch to
a `pr-review` that still fixes what it finds, which is not wrong but is not the pipeline you are
migrating to. Nothing logs either case — a label no loop watches produces silence, not an error.

Step 6 last has a second benefit worth taking deliberately: between step 5 and step 6 the new
review and remediation loops are live but nothing feeds them automatically, so you can promote one
issue by hand with `gh issue edit N --add-label status:ready-for-pr-review` and watch the whole new
chain run end to end before the execution loop starts feeding it on its own. Do that once per
project.

### Where in-flight work lands

The engine reads every open issue on every tick and selects purely on labels, so the swap takes
effect immediately and carries no state of its own. Every live state and what it needs:

| State at the swap | Where it lands | Action |
|---|---|---|
| `status:executing`, dispatch running | Untouched — the dispatch record wins over labels. The running agent keeps its **old** prompt, so it finishes by applying `status:ready-for-review` and stops, exactly as before. It does **not** hand on to review. | Once it finishes, promote by hand: `gh issue edit N --add-label status:ready-for-pr-review`. This is the last issue in the project that will ever need that. |
| `status:ready-for-execution`, queued | Dispatched on the next tick under the **new** execution prompt, and hands on to review by itself at the end. | None. |
| `status:pr-reviewing`, dispatch running | Untouched, same reason. The running agent keeps its **old** prompt, fixes what it found, and applies `status:ready-for-review`. That is still a correct end state — it just skips the new remediation loop for this one issue. | None while it runs. See the resumed-session warning below if it dies. |
| `status:ready-for-pr-review`, queued | Dispatched on the next tick under the new review-only prompt. | None, if the ordering above was followed. |
| `status:ready-for-review` from the old flow | Nothing. The label means what it always did — the work is done and it is your turn — and no loop triggers on it. | **None. Do not relabel these.** |
| `status:needs-review-input` or `status:needs-execution-input` | Idles. `blocked:*` does not match either and neither carries a trigger, so it waits for you to re-add the loop's trigger. | `loop reset` first — see below. |
| Parked at the retry cap, or operator-stopped | Unaffected; none of their labels move. | None. |
| **Any open pull request, in any of the above states** | It has no `status:pr-open` label, because that label did not exist when its execution agent ran. Execution's `labels.review` is now `status:pr-open`, so **none of these are tended any more.** They were tended before the swap and they silently stop being tended after it. | Backfill it once, per repository — see below. |

**The one real hazard is a resumed session.** Dispatch state is keyed by loop name and repository,
not by labels, and `pr-review`'s `trigger` and `in_flight` names do not change in this migration —
so a dispatch that dies is marked for retry and the next tick **resumes the same claude session**
under the new review-only `resume_prompt`. That session's context says "you fix what you find" and
its branch holds half-applied fixes. The two ways out:

- **Preferred: drain.** Swap when no issue carries `status:pr-reviewing` or `status:executing`.
- **If you swap hot:** for each such issue, once its agent has ended, run
  `agent-utils project --name <p> loop reset --name pr-review --issue N`
  (or `--name execution`) so any later dispatch starts a clean session. Do the same for any issue
  sitting at `status:needs-review-input` or `status:needs-execution-input` **before** you re-add
  its trigger.

**The tending backfill is not optional, and it is easy to forget** because nothing reports it. Every
pull request that already exists was opened before `status:pr-open` did, so after the swap the
execution loop tends none of them. Backfill once per repository, right after step 6:

```bash
gh issue list --repo mcgarylabs/koinos-monorepo --state open \
  --json number,labels \
  --jq '.[] | select([.labels[].name] | any(startswith("status:"))) | .number' \
| while read -r n; do
    gh issue edit "$n" --repo mcgarylabs/koinos-monorepo --add-label status:pr-open
  done
```

That is deliberately wider than "issues with a pull request": adding the label to an issue that has
no pull request is harmless, because tending also requires an open pull request that is behind. The
opposite mistake — missing one — is a branch that quietly rots.

### Verifying the swap took

Per repository, after the swap:

- `gh issue list --label status:ready-for-findings-exec --json number,updatedAt` — entries should
  drain within one tick interval. Anything sitting there across two intervals means the new loop
  is not running: check step 3 and step 4.
- `agent-utils project --name <p> loop status --name exec-pr-review-findings` — the tick count
  should rise.
- The `epic sweep skipped: cannot name the pipeline's entry loop` warning should have stopped.

### If an issue stops moving in both loops

An issue carrying **both** `status:ready-for-pr-review` and `status:ready-for-findings-exec` is
vetoed by both loops: `pr-review` vetoes its own terminal so a stale trigger cannot pull back work
already handed on, and `exec-pr-review-findings` vetoes `status:ready-for-pr-review` so it does not
remediate under a reviewer. Each veto is right on its own and together they deadlock. The engine
logs only `a veto label is present`, and
`agent-utils project --name <p> loop status --name <loop>` prints `veto` for the issue under both
loops — that pair is the signature.

It is reachable by the ordinary operator action of re-adding `status:ready-for-pr-review` for
another review round while the previous round's handoff label is still on the issue. Remove
`status:ready-for-findings-exec` first, then add the trigger. The new `pr-review` resume prompt
also clears the stale terminal itself on re-dispatch, so this only bites when the issue never gets
dispatched at all.

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
rebasing the same pull request.

And **`status:ready-for-review` must never enter the veto list of the loop that tends** — the
execution loop. `veto` silently disables tending as well as dispatching, and because the execution
agent applies that label when it opens the pull request and nothing ever removes it, that one
entry would switch off tending for the entire pipeline downstream of execution. (`planning.yaml`
vetoes it and that is fine: planning has `tend_pr: false`, so the field has only its dispatch
job there.)
