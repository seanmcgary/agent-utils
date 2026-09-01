# Retry-cap retirement and a park comment that names the reason

## The incident this comes from

`mcgarylabs/koinos-monorepo#74`, execution loop, 2026-09-01.

Dispatch 187 ran under `harness:pi` with `model:deepseek/deepseek-v4-flash-0731`
and was reaped at the 8h timeout. The three retries that followed each died in
about two seconds with the same error, recorded verbatim in
`dispatches.api_error`:

```
402: {"message":"This request requires more credits, or fewer max_tokens.
You requested up to 873072 tokens, but can only afford 469825. ..."}
```

OpenRouter was out of credits. The retry cap engaged and
`parkRetryExhausted` posted its comment, which said only that the failures
"usually indicate a sustained platform-side issue". Nothing in the issue, and
nothing short of reading a run log, named the actual cause.

The operator then removed `model:` and `harness:pi` and re-applied
`status:ready-for-execution`, expecting the loop to run under the configured
claude/opus default. Four seconds later the loop stripped the trigger label
again and re-posted the cap comment. Two defects made that happen.

## Defect 1: the failure path eats the human's re-trigger

`engine.Decide` evaluates `state.NeedsRetry` *above* the trigger-label branch.
An issue at the cap with `needs_retry = 1` therefore reaches `retryDecision`,
which returns `KindParkRetryExhausted` before the trigger branch is ever
consulted. `retry_count` and `parked` are cleared in exactly one place --
`store.BeginDispatch` with `retry = false` -- and that is only reached from a
dispatch decision, so the re-trigger that was supposed to un-park the issue is
consumed by the park instead.

The comment at `internal/engine/engine.go:182` asserts that "a human who
re-applies that label deliberately un-parks the issue". On this path it does
not.

## Defect 2: the cap outlives the configuration it indicts

A retry cap is evidence about one configuration. Three OpenRouter 402s say
nothing about whether claude/opus can run the issue. When the operator changes
the harness -- or changes the model in a way that crosses to a different
provider, with a different account and a different balance -- the accumulated
failures stop being evidence and the budget should start over.

## Required behaviour

### R1 -- a harness change retires the retry history

When the harness the next dispatch will run under differs from the harness of
the most recent dispatch for that issue, `retryDecision` yields a fresh start
rather than a retry or a park. The dispatch runs as `KindStart`, so
`BeginDispatch(retry = false)` clears `parked`, `retry_count` and
`retry_after` as an ordinary consequence. There is no separate unpark.

The backoff window is skipped along with the cap. A wait sized to let the old
harness's platform recover buys nothing once that harness is not the one
running.

A retirement must not count toward the circuit breaker. It is a human's
deliberate reconfiguration, not evidence of a platform fault.

### R2 -- a provider-crossing model change retires the retry history

pi reaches several providers. `openrouter` and `openai-codex` are separate
accounts with separate balances, so a 402 on one is no evidence about the
other. A model change that crosses providers therefore retires the history on
the same terms as R1.

A model change *within* one provider does not. Swapping one OpenRouter model
for another while OpenRouter is out of credits changes nothing, and the cap
should still hold.

Provider is resolved by `pi --list-models <model>`, which prints an aligned
table whose first two columns are provider and model:

```
$ pi --list-models deepseek/deepseek-v4-flash-0731
provider    model                                  context  max-out  thinking  images
openrouter  deepseek/deepseek-v4-flash-0731        1.0M     943.7K   yes       no
openrouter  deepseek/deepseek-v4-flash-0731:batch  1.0M     943.7K   yes       no
```

Constraints observed on the real binary:

- The search is fuzzy, so the match must be pinned exactly against the `model`
  column or against `provider + "/" + model`. Both label shapes in use resolve
  under that rule: `openai-codex/gpt-5.6-terra` is provider/model, and
  `deepseek/deepseek-v4-flash-0731` is a bare OpenRouter id whose `deepseek/`
  prefix is the vendor, not the provider.
- A bare id can be ambiguous. `gpt-5.6-terra` returns three rows spanning two
  providers; rows spanning more than one provider mean unresolved.
- A miss prints `No models matching "..."` and **exits 0**, so the exit status
  carries nothing. The rows are the only signal.
- `--mode json` does not apply to `--list-models`; the output is the aligned
  table either way.

Unresolved means the cap stands. Fail closed.

### R3 -- retirement must terminate

A dispatch that dies before the harness emits a session identifier never
updates `issues.session_harness`, so a rule written against that column would
read "changed" forever and retire the cap on every tick -- an unbounded
dispatch loop with no human in it.

The comparison is therefore against what the loop most recently *attempted*,
not what most recently *succeeded* in creating a session. Both the effective
harness and the resolved provider are stamped on the issue row by
`BeginDispatch`, before the agent runs. After one retirement the stamped values
match the new configuration, the rule stops firing, and the ordinary cap
governs the new configuration's own failures.

`session_harness` keeps its existing meaning and its existing reader
(`resumable`). It is not repurposed: overwriting it on a dispatch that created
no session would make `resumable` claim a session that does not exist.

### R4 -- the park comment names the reason

`parkRetryExhausted` is the one place agent-utils writes to GitHub on its own
behalf, and it is the right place to say why. The comment carries the most
recent failed dispatch's `api_error`, plus the harness, model and provider that
produced it.

Every URL is stripped first. OpenRouter's 402 embeds
`https://openrouter.ai/workspaces/default/keys/<id>`, which names the key's
identifier -- credential-adjacent, and not something to publish to an issue.
The unredacted text stays in the dispatch row and the run log, where an
operator can still read it.

When no reason was recorded, the comment keeps today's wording rather than
asserting a cause it does not have.

## Out of scope

- The known circuit-breaker gap at `internal/engine/engine.go:237` (per-call
  `eligibleRetries` cannot reach a threshold above 1 on a webhook-scoped tick).
- Any `agent-utils` subcommand for un-parking by hand.
- Reading the provider back out of pi's event stream. pi reports
  `"provider"` on every `message_end`, but the comparison in R2 needs both
  sides to come from one resolver, and the resolver is what R3 stamps.
