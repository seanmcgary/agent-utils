# Review/remediation split Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Split the reference `pr-review` loop into two loops — one that reviews and stops, one
that fixes what the review found — so remediation runs in a fresh context at a cheaper tier
instead of inside the reviewing session.

**Architecture:** Configuration, documentation, and the small amount of Go that pins them.
`pr-review` becomes review-only and declares `labels.terminal: status:ready-for-findings-exec`,
which is the new `exec-pr-review-findings` loop's trigger. `internal/config/entryloop.go` derives
the pipeline graph from exactly that relation.

Two consequences the first draft of this plan got wrong, both found by the plan review:

- **`examples/` is not free-standing.** `internal/wizard/templates/*.yaml` are byte-identical
  copies enforced by `TestExamplesMatchTheEmbeddedTemplates`, so every example edit is two edits,
  and adding a fourth loop is also a `go:embed` line and a `templateNames` entry.
- **The terminal edge does more than document the chain.** `examples/execution.yaml` declared no
  terminal, so `pr-review`'s trigger was no other loop's terminal and `planning` and `pr-review`
  BOTH resolved as entry loops — ambiguous, which disables the epic sweep for the whole project
  with nothing but a log warning. That was already true before this change. Declaring
  `terminal: status:ready-for-pr-review` on the execution loop is what actually makes the chain
  single-entry, and a new test pins it.

**Tech Stack:** YAML loop files under `examples/` and `internal/wizard/templates/`, two small Go
edits in `internal/wizard/templates.go`, tests in `internal/config/examples_test.go`, Markdown
under `docs/` and `README.md`.

**Spec:** none. The design was fixed by the operator before this plan; the measurements below are
his, and the plan implements the agreed shape rather than re-deriving it.

## The measurement this exists to fix

Across eight real `pr-review` sessions:

| | Cost | Share |
|---|---|---|
| Reviewer fan-out | $8.39 | 8% |
| Remediation after it | $90.86 | **92%** |

Reviewers are dispatched by about turn 47 with the parent at 66k context. In the worst session,
turns 47 → 262 drove context from 66k to 321k and cost $23.86 of a $38 dispatch. Of the 584 Bash
calls after review, 28% were gates/tests/builds, 20% edits, 10% git — and **25% read/search**,
because findings came back as prose and the parent re-located every issue itself, in the most
expensive context in the run.

Two independent fixes follow, and both are needed:

1. **A fresh context per fix.** A separate dispatch, and within it one subagent per file group.
2. **A prescriptive artifact.** Findings anchored to `File:Line` *and* carrying a prescribed fix
   specific enough to act on without re-deriving it. That is what removes the 25%.

## Global Constraints

This repository has **no** root conventions document (no `AGENTS.md`, `CLAUDE.md`,
`CONTRIBUTING.md`, or `STANDARDS.md`). The binding rules below are read from the code and the
build.

- **Gates:** `make check` runs `fmtcheck vet lint test`. It must pass before every commit.
- **Comments carry the reason, not the restatement.** Every non-obvious decision in this
  codebase is commented with *why*, and often with the incident that caused it. The example
  loop files carry the same register — see `examples/pr-review.yaml`'s block on why `tend_pr` is
  false, which states the trade rather than the setting. A comment that restates the config is
  noise.
- **Untrusted input.** Issue bodies, comments, and labels are attacker-controlled
  (`README.md`, Security). This change adds no new interpolation of any of them.
- **Never add `Co-Authored-By:` trailers.** The repository owner's standing instruction. It
  overrides the trailer rule stated in older plans under `docs/superpowers/plans/`, which are
  historical records and are not edited by this change.
- **Never merge.** This plan ends at a review-ready pull request.

## Verified external API (do not re-derive)

Probed live against the GitHub REST API on 2026-09-01, against
`POST /repos/seanmcgary/agent-utils/issues/11/comments`:

| Body length | Result |
|---|---|
| 70,000 characters | accepted, stored intact |
| 262,144 characters | accepted, stored intact |
| 262,145 characters | `422` |

The `422` reads `Body is too long (maximum is 65536 characters)`. **That message is wrong.** The
enforced cap is 2^18 = 262,144. Do not size the findings artifact against 65,536; do not trust
the error text as documentation. All probe comments were deleted.

Measured reviewer output is 35–70KB, so one round fits with a wide margin. The artifact format
budgets 200,000 characters and defines an overflow split anyway, because the cap counts bytes and
a five-dimension review of a large branch is not hypothetical.

## Facts read from this codebase (do not re-derive)

- **`labels.terminal` drives no engine behaviour.** It exists so prompts can name the gate via
  `{{.Labels.Terminal}}` (`docs/configuration.md` § `labels.terminal`). What makes an issue
  actually leave a loop is listing the same label under `veto`.
- **`internal/config/entryloop.go`** derives the entry loop as "the loop whose trigger is no
  OTHER loop's terminal or review label". Declaring `pr-review`'s terminal is therefore not
  decoration: it is the edge that keeps `planning` unambiguously at the front once a third loop
  exists.
- **Terminal is prompt-enforced, not gated.** `examples/planning.yaml` forbids its agent from
  ever applying `{{.Labels.Terminal}}`, because there the terminal is the human gate.
  `pr-review`'s terminal is a machine handoff, so its prompt must say the opposite — apply it,
  and only as the strictly final action. Applying a downstream trigger before the last phase puts
  a second agent on a branch the first is still writing to.
- **`agent.background_tasks` defaults false** (`internal/config/config.go`;
  `docs/configuration.md`). A subagent is therefore an ordinary blocking tool call: several
  dispatched in one turn still run concurrently, one per turn runs serially. Every fan-out this
  change describes must go out in a single message.
- **`agent.effort`** is validated against `low|medium|high|xhigh|max`. `agent.model` is free-form
  and only checked non-empty, and existing files use short aliases (`opus`).

## Tasks

### Task 1 — `examples/pr-review.yaml` becomes review-only

- [ ] Add `terminal: status:ready-for-findings-exec` under `labels`, and add
      `status:fixing-findings` to `veto` so an issue being remediated is not re-reviewed
      underneath the agent fixing it.
- [ ] Replace the "YOU FIX WHAT YOU FIND" block with its inverse: triage into Fix / Reject /
      Defer, post the findings artifact, apply nothing.
- [ ] Drop the fold/`--fixup`/`--autosquash`/re-gate language. **Keep** the worktree, fetch,
      checkout and `--force-with-lease` rules: gate fixes (format, lint) still produce commits
      that must be pushed.
- [ ] Add the terminal-label instruction as the strictly final action, with the reason.
- [ ] Add the findings-artifact instruction: post one comment, record its ID in Pipeline State's
      `findings comment` field, name the format contract.
- [ ] Lower `max_budget_usd` from 50 to 10. Measured review phase is ~$1.05/session.
- [ ] **Keep `model: opus`; drop `effort: high` → `medium`.** The existing comment's rationale —
      the reviewer must be stronger than whatever wrote the code — gets stronger, not weaker,
      once the reviewer's output is a specification a cheaper executor consumes, so the MODEL
      does not move. Effort does: the expensive part of the old loop was the remediation, and
      reading a diff against a rubric is bounded work. The comment claiming this loop runs "the
      strongest model at the highest effort, always" is now wrong and must be reworded.
- [ ] Update `resume_prompt` to match: it currently repeats "you fix what you find".

`review: yes` — this file is the contract the other two hang off.

### Task 2 — add `examples/exec-pr-review-findings.yaml`

- [ ] Labels: `trigger: status:ready-for-findings-exec`, `in_flight: status:fixing-findings`,
      `blocked: status:needs-findings-input`, `review: status:ready-for-review`,
      `veto: [blocked:*, status:ready-for-execution, status:executing, status:pr-reviewing]`.
- [ ] Agent: claude harness (default), `model: sonnet`, `effort: medium`,
      `permission_mode: bypassPermissions`, `worktree: per_issue`, `max_budget_usd`, `timeout`.
- [ ] `tend_pr: false`, with the reason stated in the same register as `pr-review`'s.
- [ ] Prompt: fetch the findings comment by ID, group by file, one subagent per group in ONE
      message, targeted tests per group, full suite once, push with `--force-with-lease`, then
      `labels.review` last. No terminal label — the human merges.
- [ ] `resume_prompt` and a `tend_prompt` stub matching house style.

`review: yes`

### Task 3 — retune `examples/execution.yaml`

- [ ] `model: opus` → `sonnet`; `effort: high` → `medium`.
- [ ] Add `status:pr-reviewing` AND `status:fixing-findings` to `veto`, so tend does not rebase a
      branch while another agent is rewriting it. The brief said `status:pr-reviewing` was already
      there; it is not — only the four LIVE `execution.yaml` files have it, and the example has
      only `blocked:*`. So the hazard exists unguarded in the example as shipped.
- [ ] Add `terminal: status:ready-for-pr-review`, and comment why it must NOT also go in `veto`.
- [ ] Comment every veto entry with its reason.

`review: yes` — the terminal edge changes entry-loop resolution for the whole project.

### Task 4 — the propagation and migration document

- [ ] Add `docs/review-loop-split.md`: what changes in each of the four live configs
      (Koinos, LawnDominator, ProjectWrangler, SnootSnap), the label set to create, and the
      migration for in-flight work.
- [ ] **Do not touch any file under `~/Documents/Claude/Projects/`.** The operator applies it.
- [ ] The migration must give every currently-live state a landing spot: an issue holding
      `status:ready-for-review` from the OLD flow means "reviewed and fixed, waiting on a human",
      which under the new flow is the same label with a different history. Say so explicitly, and
      say what to do with an issue mid-`status:pr-reviewing` when the configs are swapped.

`review: yes` — a wrong migration strands live work.

### Task 4b — the wizard copies and the tests that pin them

- [ ] Copy every edited example verbatim over `internal/wizard/templates/<name>.yaml`.
- [ ] Add `templates/exec-pr-review-findings.yaml` to the `go:embed` directive and
      `"exec-pr-review-findings"` to `templateNames` in `internal/wizard/templates.go`.
- [ ] Extend `internal/config/examples_test.go`'s render list to every file in `examples/`, not a
      subset — `pr-review.yaml` was never covered there.
- [ ] Add a test asserting the example set resolves to exactly ONE entry loop, and that it is
      `planning`. Verify it FAILS without the terminal from Task 3.

`review: yes` — this is the task that makes the rest verifiable.

### Task 5 — docs

- [ ] `README.md`: the paragraph describing the three example files still says the third
      "fix[es] what it finds rather than reporting it". Rewrite for four files and the new chain.
- [ ] `docs/configuration.md`: its opening line names three example files. Update, and add the
      new chain where the label sections describe `terminal`.
- [ ] Check every other mention of `pr-review` in both files.

`review: no` — mechanical, gated by grep.

## Pipeline State

| Field   | Value                                                        |
|---------|--------------------------------------------------------------|
| stage   | 5 (pr feedback loop)                                         |
| class   | standard (new pipeline contract, contained to config + docs) |
| profile | backend                                                      |
| branch  | feat/split-review-and-remediation-loops                      |
| pr      | #26                                                          |
| gate    | approved 2026-09-01 (design fixed by the operator's brief)   |
| round   | 0                                                            |
| decisions | 7 (see Decisions below)                                    |

### Decisions

- **Task 1 — Pipeline State field name.** Brief: call the field `findings_comment`. Did: called
  it `findings comment`. Why: the block's other multi-word fields are `design assets` and
  `plan comment`, spaced and unquoted; an underscored field would be the only one of its kind in
  a table a human reads. The name is prose in a Markdown table, not a parsed key — nothing in Go
  reads it.
- **Task 1 — `agent.effort`.** Brief: keep `high`. Did: `medium`. Why: the operator corrected the
  brief mid-run. `model: opus` is unchanged; only the effort axis moved. The config comment
  claiming this loop runs "the strongest model at the highest effort, always" was reworded rather
  than deleted, because the model half of it is still the loop's whole point.
- **Task 1 — `labels.review`.** Brief: silent on it. Did: set it equal to `labels.terminal`
  (`status:ready-for-findings-exec`). Why: the plan review pointed out that the alternative is a
  field the agent never applies, and that leaving it at `status:ready-for-review` makes an issue
  *queued for remediation* indistinguishable from one *already fixed and waiting on the human* —
  to the execution loop's tend and to `agent-utils status` alike. Safe only because `tend_pr` is
  false here, which the config comment says.
- **Task 3 — the terminal on the execution loop.** Brief: silent on it; the brief's scope was
  pr-review, the new loop, and execution's model settings. Did: added
  `terminal: status:ready-for-pr-review`. Why: this is scope the brief did not ask for, and it is
  here because the plan review found the shipped example set already resolves to TWO entry loops,
  which disables the epic sweep for the whole project behind nothing but a log warning. The new
  loop does not cause that and does not fix it; the missing terminal on `execution` is the cause.
  Leaving it would have shipped a four-loop chain whose graph does not resolve, in the same change
  that documents the graph.
- **Policy change mid-run — two human touches, not four.** The operator changed the design while
  this was in flight: the execution agent now applies `status:ready-for-pr-review` itself, so an
  issue takes exactly two human touches (approve the plan, merge at the end) instead of four.
  That forced a collision to be resolved. `execution.labels.review` was `status:ready-for-review`
  and was applied mid-run, which under an automated chain summons the human before the branch has
  been reviewed or fixed at all. Did: gave execution `review: status:pr-open`, a new label.
  Why: `labels.review` does two jobs — "go read this" and "tend this" — and automating the chain
  pulls them apart. Execution has no human-facing output state any more, but it is still the only
  loop that tends, so the field is chosen for the tending job and named for what the agent
  actually asserts when it applies it. Nothing removes `status:pr-open`, so tending now covers
  every wait in the chain including the human gate, which is wider coverage than before, and
  `status:ready-for-review` is left meaning one thing applied by one loop. Rejected the
  alternative of moving `tend_pr: true` to the remediation loop: it leaves the review and
  remediation queues, and every park, untended.
  The hazard the old comment named is real and is solved rather than re-documented — the execution
  prompt now has a numbered six-step completion order with the terminal as its strictly last
  action, after the final push, and the comment says why terminal is safe to chain when review is
  not: it is a property of WHEN a label is applied, not of which field it is.
- **Policy change mid-run — no cost ceilings, 24h timeouts.** `agent.max_budget_usd` is `0` in
  every example, and `agent.timeout` gained a `24h` default and stopped being required. Did:
  wrote both out explicitly in the examples rather than omitting them. Why: the operator's lean,
  and it matches the house preference for documented configuration over relying on a zero value —
  a reader should see that "no ceiling" was decided, not forgotten. The timeout default is a real
  Go change in `Load()`, and `validate()` now rejects only a NEGATIVE duration, because YAML
  cannot distinguish an omitted field from an explicit zero.
- **Task 4b — new Go and a new test.** Brief: "Your work should be YAML/skills/docs only." Did:
  two lines in `internal/wizard/templates.go` and two tests in
  `internal/config/examples_test.go`. Why: `examples/*.yaml` is byte-pinned to
  `internal/wizard/templates/*.yaml` by an existing test, so a YAML-only change does not exist for
  three of these files; and the entry-loop breakage above is invisible without a test that asserts
  it. Neither edit touches `internal/config`'s non-test source, so there is no collision with the
  open `tend:` pull request.
