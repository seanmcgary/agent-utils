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
| `execution` | opens the PR, applies `status:ready-for-review`, stops; **you** apply `status:ready-for-pr-review` when you want it reviewed | opens the PR, finishes, then applies `status:ready-for-pr-review` itself as its strictly last action |
| `pr-review` | reviews, then fixes what it finds, then applies `status:ready-for-review` | reviews, posts a findings comment, applies `status:ready-for-findings-exec`, stops |
| remediation | inside the review session, in a context that grew to 321k tokens | a separate dispatch, one fresh subagent per **file** group |
| findings | prose in a disposition table, re-located by whoever fixed them | anchored to `File:Line` with a prescribed fix, grouped by file |
| handoff | none — one session did both | a comment ID in Pipeline State's `findings comment` field |
| `status:ready-for-review` | applied by `execution` when it opened the PR, then again by `pr-review` — it meant both "a PR exists" and "your turn" | applied only by `exec-pr-review-findings`, at the very end. It means one thing: the pipeline is finished and it is your turn to merge |
| tending | per-loop `tend_pr: true`, keyed to that loop's `labels.review` | **its own dispatcher**, described entirely by a project-level `tend:` block — no loop file mentions it at all |
| `labels.review` | required on every loop | **removed**. A loop declares only its own four labels and ends by applying its terminal |
| epic sweep | derived the entry loop from the label graph | a project-level `epic.loop` declaration |

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

Plus one more, for planning's new terminal:

```bash
for r in koinos lawndominator projectwrangler snootsnap; do
  gh label create status:ready-for-plan-review --repo "mcgarylabs/$r-monorepo" \
    --description "Planning finished; read the plan and approve it"
done
```

Verify before continuing:

```bash
gh label list --repo mcgarylabs/koinos-monorepo --limit 200 | grep -c findings          # want 3
gh label list --repo mcgarylabs/koinos-monorepo --limit 200 | grep -c plan-review       # want 1
```

Every other label in the new chain already exists — `status:ready-for-pr-review`,
`status:ready-for-execution` and `status:ready-for-review` are all in use today, and their
meanings do not change.

**Do this first, and do not skip the verification.** A trigger label that does not exist in the
repository produces no error anywhere: the tick lists every open issue, finds nothing carrying the
label, records "no trigger label is present", and idles forever. The failure surfaces instead when
an *agent* tries to apply the label — and it does that after it has already removed the previous
status label, which can leave an issue carrying no status at all.

## The per-project changes

Every live `pr-review.yaml` is a machine-normalised copy of the old example: same prompts
byte-for-byte, differing only in `repo`, `checkout_base_dir: .`, and `timeout: 8h0m0s`. So the
propagation is: take the new example's bodies, keep the three local values.

**Two removals apply to every loop file in every project**, and they are the reason this is not a
label rename:

- **`labels.review` is gone from the config format.** A file that still sets it fails to load.
- **`tend_pr` is gone from the config format.** Same: a file that still sets it fails to load.
- **`tend_prompt` is gone from the config format.** Same again. A loop that does not tend — which
  is now every loop — has no business carrying the instructions for a dispatch it never makes.

All three moved to the project descriptor, `.agent-utils/config.yaml`, which is a different file —
see step 0.

**One name is now reserved.** A loop called `tend` fails to load, naming the reason: that is the
tend dispatcher's own name, and every row this program writes is keyed by `(project, loop)`, so a
loop sharing it would read and write the dispatcher's dispatches, pull request links, conflict
rows, lock file and worktrees. None of the four projects has such a loop.

### 0. The project descriptor — in all four projects

New, and it comes first because two of the loop edits below depend on it. Add to
`<project>/.agent-utils/config.yaml`, keeping the existing `name` and `id` lines untouched:

```yaml
tend:
  enabled: true
  label:   status:ready-for-review
  model:   sonnet
  effort:  medium
  permission_mode: bypassPermissions
  i_understand_bypass_permissions: true
  prompt: |
    # the rebase-and-review-reply template that used to be execution.yaml's
    # tend_prompt. Copy it from examples/project/config.yaml.

epic:
  loop: planning
```

**`tend:` replaces `tend_pr` on the loops, and it is now the WHOLE of tending.** Tending keeps a
repository's open pull requests rebased and answers review activity on them; that describes the
repository, not any one loop's issue lifecycle. The old per-loop flag was gated on that loop's
`labels.review`, which worked only because the review label happened to mark the right issues —
and it had a misconfiguration with no error message, since two loops in one project could both set
`tend_pr` and both would rebase the same branch.

**There is no `tend.loop`.** An earlier draft of this change had one, naming which loop's rows
hosted the dispatches, because the tend work still ran inside loop ticks and every row it wrote had
to be keyed by some loop's name. That setting existed only because of the arrangement it was
working around. Tending is its own **dispatcher** now: it keys its rows by the reserved name
`tend`, runs its own agent, gets a fresh session per dispatch, and uses worktrees of its own at
`<worktree_dir>/tend/pr-N`. No loop file says anything about it.

**So the agent fields are the whole agent, not an overlay.** There is no loop `agent:` block behind
them: `model` is REQUIRED when `enabled` is true, `permission_mode` is required by the dispatcher
(a tend rebases and force-pushes, and claude denies every prompt in a detached run, so a tend with
no mode fails at its first push), `harness` defaults to `claude`, and `effort` is optional. There
is no `worktree` mode (a tend always gets its own worktree for the pull request), no
`max_budget_usd` (a cap that lands mid-rebase leaves a half-resolved conflict) and no `timeout`
(24h, the same default a loop gets by omitting it).

**`tend.prompt` is the tend prompt**, moved out of the host loop's `tend_prompt`. It renders the
same template context a loop prompt does with **one difference that has teeth**: a tend has no
loop, so there are no loop labels. `{{.Labels.Trigger}}` and friends would render as empty strings,
so a `tend.prompt` that mentions `.Labels` is **rejected at load time**. Name the labels literally,
and use `{{.Tend.Label}}` for the eligibility label. The template in
`examples/project/config.yaml` is the old one with exactly that edit applied.

**`tend.label` is what makes a pull request eligible.** It stays `status:ready-for-review` — the
final loop's terminal, and the state where a pull request waits longest: every earlier wait is a
machine that picks the issue up within a tick, and only the human gate lasts long enough for the
base branch to move.

**Where the dispatcher shows up.** It answers to `tend` everywhere a loop name is accepted, which
is deliberate — it dispatches agents and holds sessions, so it must not be invisible:

```
agent-utils project loop status --name tend    # the pull request queue it maintains
agent-utils project loop tick   --name tend    # the full pass, for a cron entry
agent-utils project logs        --name tend --list
agent-utils project sessions list --name tend
```

`loop status --name tend` prints a different table from a loop's: the dispatcher moves no label and
keeps no issue state, so what it reports is which pull request each eligible issue links to, how
far behind it is, when it was last tended, and whether an agent is in it now. `sessions list` with
no filter shows tend sessions alongside the loops', under the loop name `tend`.

**`epic:` replaces a derivation, and fixes a bug that predates this work.** The epic sweep used to
find its entry loop by asking which loop's trigger was no other loop's terminal or review label.
Because the live `execution.yaml` declares no terminal, `pr-review`'s trigger is nobody's terminal,
so `planning` and `pr-review` both resolve as entry loops — `ErrAmbiguousEntryLoop`, which means
**the epic sweep is already disabled in all four projects today.** Its only symptom is a `WARN`.
Declaring `epic.loop: planning` resolves it. Confirm afterwards that the warning has stopped:

```
epic sweep skipped: the project names no usable epic.loop     # new message
epic sweep skipped: cannot name the pipeline's entry loop     # old message; must stop appearing
```

A descriptor that will not parse fails **every loop in the project**, deliberately: the policy it
carries might have said "enabled", and running a loop under a policy nobody can read is the
outcome worth refusing. Validate before going further (step 3 of the migration).

### 1. `pr-review.yaml` — in all four projects

| Field | From | To |
|---|---|---|
| `labels.review` | `status:ready-for-review` | **delete the line** |
| `labels.terminal` | absent | `status:ready-for-findings-exec` |
| `labels.veto` | `blocked:*`, `status:ready-for-execution`, `status:executing` | add `status:ready-for-findings-exec` and `status:fixing-findings` |
| `agent.effort` | `high` | `medium` |
| `agent.max_budget_usd` | `50` | `0` — see the budget/timeout section below |
| `agent.timeout` | `8h0m0s` | `24h` (or delete the line) |
| `prompt` | `reviewing-commits`, "YOU FIX WHAT YOU FIND" | `producing-review-findings`, "YOU DO NOT FIX WHAT YOU FIND" — copy the example's body verbatim |
| `resume_prompt` | repeats "you fix what you find" | copy the example's body verbatim |
| `tend_prompt` | a "never rendered" stub | delete it; the field no longer exists |
| header comment | says the loop "reviews and FIXES" | rewrite; it describes the old behaviour |

`agent.model` stays `opus`. That is deliberate and it is the one setting the split makes *more*
important: this loop's output is now a specification a cheaper model executes, so strength saved
here is paid for twice downstream by an executor working out what a finding meant. Effort is the
axis that drops, because the expensive part of the old loop was never the reviewing.

### 2. `exec-pr-review-findings.yaml` — new file, all four projects

Copy `examples/exec-pr-review-findings.yaml` and change `repo`, `checkout_base_dir: .`, and
`timeout` to match the project's other loops. **Copy the whole file**, not the fields listed in a
summary somewhere: `config.Load` is strict and unconditionally requires `name`, `repo`,
`checkout_base_dir`, `worktree_dir`, `default_branch`, both prompts, the full `retry` block
including `breaker.orphan_threshold` and `breaker.cooldown`, and — because
`permission_mode: bypassPermissions` is set — `i_understand_bypass_permissions: true`.

**It carries no `tend_prompt`, and neither does any other loop file.** The rebase-and-review-reply
template that used to live on `execution.yaml` is now `tend.prompt` in the project descriptor
(step 0), with two changes: it tells the agent it is in a FRESH session and holds no memory of the
branch, and it names its labels literally instead of through `{{.Labels.*}}`, which a
project-level prompt has no loop to fill in.

A file that will not load is not a local failure. `EpicLoop` refuses for the **whole repository**
when any loop file fails to load, and the webhook router drops that loop from routing. Validate
before going further (see step 3 of the migration).

### 3. `execution.yaml` — in all four projects

**This is the file whose behaviour changes, not just its labels.** Copy the example's `prompt` and
`resume_prompt` bodies verbatim, the same way you do for `pr-review.yaml`.

| Field | From | To |
|---|---|---|
| `labels.review` | `status:ready-for-review` | **delete the line** |
| `labels.terminal` | absent | `status:ready-for-pr-review` |
| `labels.veto` | `blocked:*`, `status:pr-reviewing` | add `status:ready-for-pr-review` and `status:fixing-findings` |
| `tend_pr` | `true` | **delete the line**; the policy is the descriptor's now |
| `tend_prompt` | the real rebase template | delete it; it moves to `tend.prompt` in the project descriptor (step 0) |
| `agent.model` | `opus` | `sonnet` |
| `prompt` | "ON COMPLETION. Open the pull request … add `{{.Labels.Review}}`" | a numbered five-step completion order ending with `{{.Labels.Terminal}}` as the strictly last action — copy the example's body verbatim |
| `resume_prompt` | silent on completion order | names the numbered order and the strictly-last terminal — copy verbatim |
| header comment | says the human applies `status:ready-for-pr-review` by hand | rewrite; that is now the agent's job |

`agent.effort` is already `medium` in all four live files; only the example needed changing.
`max_budget_usd` is already `0` in all four `execution.yaml` files, so only the `pr-review.yaml`
caps need clearing. `agent.timeout` is `8h0m0s` everywhere — see below.

**This supersedes the warning currently in your `execution.yaml`.** That comment says
`status:ready-for-pr-review` "must NOT be" the execution loop's handoff, because the agent applies
its label before its last phase and would start the review on a branch it is still writing to. The
hazard is real and the comment was right about it. What changed is the resolution: instead of
avoiding the chain, the prompt now defines *when* the label is applied — after the final push, as a
numbered last step, with nothing following it. Delete the old comment when you copy the new prompt
in; leaving it beside a prompt that does the opposite is worse than either.

**The prompt is the guard here, and there is no mechanical backstop for it.** If a project's
`execution.yaml` gets the new `terminal` but keeps the old prompt, the agent never applies it and
issues simply stop at the end of execution — visible, recoverable, and the safer failure. If it
gets the new prompt but the agent applies the terminal early anyway, two agents land on one branch.
That is why the prompt bodies are copied verbatim rather than summarised.

### 4. `planning.yaml` — in all four projects

| Field | From | To |
|---|---|---|
| `labels.review` | `status:plan-ready-for-review` | **delete the line** |
| `labels.terminal` | `status:ready-for-execution` | `status:ready-for-plan-review` |
| `labels.veto` | `blocked:*`, `status:ready-for-execution`, `status:executing`, `status:ready-for-review` | add `status:ready-for-plan-review` |
| `tend_pr` | `false` | **delete the line** |
| `tend_prompt` | a "never rendered" stub | delete it; the field no longer exists |
| `prompt` | PARK-FOR-REVIEW applies `{{.Labels.Review}}`; "You NEVER apply `{{.Labels.Terminal}}`" | PARK-FOR-REVIEW applies `{{.Labels.Terminal}}` as its last action; "you NEVER apply `status:ready-for-execution`" — copy the example's body verbatim |
| `resume_prompt` | ends "you never apply `{{.Labels.Terminal}}`" | copy the example's body verbatim |

**The approval gate does not move; it stops being planning's terminal.** Before, planning ended by
applying its review label and the human applied its terminal. Now planning ends by applying its own
terminal — `status:ready-for-plan-review`, meaning "planning is finished" — and you apply
`status:ready-for-execution`, which is execution's trigger and which the prompt forbids the agent
from ever applying. You do exactly what you did before: read the plan comment, apply
`status:ready-for-execution`. Only the label the agent leaves behind has changed name.

`status:plan-ready-for-review` is retired by this. Leave the label in the repository; it costs
nothing and it is what a rollback needs.

### 5. Cron

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
3. **Add the `tend:` and `epic:` blocks** to each project's `.agent-utils/config.yaml`.
4. **Validate**, per project, before going further:
   `agent-utils project --name <p> loop status --name exec-pr-review-findings`
   must print the loop rather than an error. A bad descriptor fails here too, and it fails every
   loop in the project, so this one command covers steps 2 and 3 together.
5. **Add the cron line** for the new loop.
6. **Swap `pr-review.yaml`.** Keep a `pr-review.yaml.bak`.
7. **Swap `execution.yaml`.** Keep an `execution.yaml.bak`.
8. **Last, swap `planning.yaml`.** Keep a `planning.yaml.bak`.

Every step is downstream-first, and that is the whole rule: **never create a producer of a label
before its consumer exists.** Step 6 before step 2 strands a finished review at
`status:ready-for-findings-exec` with no loop watching it. Step 7 before step 6 hands a branch to
a `pr-review` that still fixes what it finds, which is not wrong but is not the pipeline you are
migrating to. Step 8 before step 7 leaves plans parked at a terminal nothing consumes — harmless,
but you will be relabelling by hand until you finish. Nothing logs any of it: a label no loop
watches produces silence, not an error.

Step 3 must come before steps 6–8 for a second reason: `tend_pr`, `labels.review` and
`tend_prompt` are all removed from the loop format, so a swapped loop file no longer carries the
tend policy OR the tend prompt. Between the swap and the descriptor edit, **nothing in that project
tends**.

Steps 6 and 7 have a benefit worth taking deliberately: after step 6 the new review and remediation
loops are live but nothing feeds them automatically, so you can promote one issue by hand with
`gh issue edit N --add-label status:ready-for-pr-review` and watch the whole new chain run end to
end before the execution loop starts feeding it. Do that once per project.

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
| `status:speccing`, dispatch running | Untouched. The running agent keeps its **old** prompt, so it finishes by applying `status:plan-ready-for-review` and stops. | Read the plan and apply `status:ready-for-execution`, exactly as you always did. The label the agent left is the old one; nothing triggers on either, so nothing is stuck. |
| `status:plan-ready-for-review` from the old flow | Nothing. No loop ever triggered on it and none does now. | Read and apply `status:ready-for-execution`. Relabelling to `status:ready-for-plan-review` is cosmetic. |
| **Any open pull request, in any state** | Still tended, provided step 3 was done. Tending moves off `execution` and onto the project's own dispatcher; the eligibility label stays `status:ready-for-review`, so the set of tended pull requests is unchanged. Its dispatches, logs and sessions appear under the loop name `tend` rather than `execution`, and its worktrees move from `<worktree_dir>/execution/pr-N` to `<worktree_dir>/tend/pr-N` — the old ones are left on disk and can be deleted by hand. | None. |

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

**No label backfill is needed.** An earlier draft of this migration introduced a `status:pr-open`
label to carry tend eligibility and required backfilling it onto every existing pull request; that
is gone. Eligibility is `tend.label`, and it points at `status:ready-for-review` — the same label
those pull requests already carry. Nothing to relabel.

**One tend behaviour changes, though, and it changes for in-flight work too.** A tend agent no
longer resumes the issue's session; it gets its own, every time. Nothing needs doing about that,
but expect the first tend after the swap on any given pull request to read the branch cold rather
than remember writing it. That is the intended trade: a clean rebase runs no agent at all now, so
what is left for one is a conflict or a review reply, both fully described by the branch and the
pull request thread. Resuming also used to block the issue's own dispatches for as long as the tend
ran.

### Verifying the swap took

Per repository, after the swap:

- `gh issue list --label status:ready-for-findings-exec --json number,updatedAt` — entries should
  drain within one tick interval. Anything sitting there across two intervals means the new loop
  is not running: check steps 4 and 5.
- `agent-utils project --name <p> loop status --name exec-pr-review-findings` — the tick count
  should rise.
- The `epic sweep skipped: cannot name the pipeline's entry loop` warning should have stopped, and
  not been replaced by `epic sweep skipped: the project names no usable epic.loop`.
- A pull request behind its base at `status:ready-for-review` should still get rebased. That is
  the one behaviour that moved between loops, and the failure is silent: check the
  `exec-pr-review-findings` logs for a tend, not the `execution` ones.

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

### Stale `pr_links` rows under a loop's name

Loops no longer write `pr_links`. Tending does, under the reserved `tend` name, and `TendCheck` —
the only thing that prunes the table — deletes only rows filed under that name. So whatever
`pr_links` rows your loops wrote before the upgrade stay in the database forever, and nothing after
the upgrade updates them.

That matters in exactly one place. When the detached runner renders a prompt for a **loop**
dispatch it looks the issue up in `pr_links`, so `.PR.HeadRef`, `.PR.BaseRef` and `.PR.BehindBy`
render from a row that stopped being maintained at the upgrade — a plausible-looking branch name
and commit count describing a pull request that may since have merged, closed, or been force-pushed
somewhere else. `.PR.Number` is read from the dispatch row instead and renders `0` for a loop
dispatch, which at least reads as obviously wrong; the other three do not.

The shipped loop prompts do not read `.PR.*` at all, and the ones in this migration tell the agent
not to trust it, so a project on those prompts is unaffected. **If you carry an operator-written
prompt that renders `.PR.HeadRef`, `.PR.BaseRef` or `.PR.BehindBy` in a loop, stop rendering
it** — the pull request the agent needs is the one it can read from the issue with `gh`, which is
current by construction. There is no automatic prune: deleting a row a downgrade would want back is
not something a migration should do on its own. To clear them by hand once you are certain you are
not rolling back:

```sql
-- ~/.agent-utils/state.db; <project-id> is the id in the project descriptor.
DELETE FROM pr_links WHERE project_id = '<project-id>' AND loop <> 'tend';
```

### Rollback

Rolling back is not just restoring the files, because labels would be left watched by nothing and
nothing would say so.

1. **Drain first.** For each of `status:ready-for-findings-exec`, `status:fixing-findings` and
   `status:needs-findings-input`: `gh issue list --label <l>` and relabel each issue back to
   `status:ready-for-pr-review`. Do the same for `status:ready-for-plan-review`, relabelling to
   `status:plan-ready-for-review`.
2. `agent-utils project --name <p> loop reset --name exec-pr-review-findings --issue N` for any
   issue that has a stored session with that loop.
3. **Restore the loop files** — `planning.yaml.bak`, `execution.yaml.bak`, `pr-review.yaml.bak` —
   and delete `exec-pr-review-findings.yaml`. Removing the loop file first makes the listener
   resolve the loop as gone and, after enough consecutive observations, permanently clear its retry
   rows.
4. **Remove the `tend:` and `epic:` blocks** from the project descriptor, in the same edit. The
   restored loop files carry `tend_pr` and `tend_prompt` again, and a descriptor that still enabled
   tending would run a second dispatcher against the same branches.
5. Remove the cron line. Leave every label in the repository; they cost nothing and they are what a
   second attempt needs.

**A rollback is only possible while the loop format still accepts `tend_pr` and `labels.review`.**
Both are removed from the config format by the same release that introduces this migration, so
rolling the *configuration* back also means running the older `agent-utils` binary. Keep the one
you were on until the new chain has run a full feature end to end.

## Two invariants to keep

**`status:ready-for-review` must never be anything but `tend.label`.** The loops' veto lists no
longer affect tending at all: the dispatcher has no veto list, because the loops' lists name one
another's states and a union of them would veto every status label the pipeline has, including the
eligibility label itself. What gates a tend now is the eligibility label, the draft check, and two
PROJECT-WIDE guards — an issue with a live dispatch in any loop, and an issue an operator stopped
in any loop, are both skipped. Every loop may still veto `status:ready-for-review` for its own
purposes, freely, and `planning.yaml` does.

**Exactly one loop may end at `status:ready-for-review`.** It is the human's merge queue, and the
whole two-touch design rests on nothing else putting an issue there. The temptation is a loop
reaching for it to obtain something else — which is exactly what the old `labels.review` did for
tend eligibility, and why an issue used to arrive in your queue the moment execution finished.
Tending is `tend.label`'s job now, and it is set once, in the project descriptor.
