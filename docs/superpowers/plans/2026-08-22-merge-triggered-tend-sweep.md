# Merge-triggered tend sweep Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a merge into a loop's default branch rebase every open pull request that the
merge left behind.

**Architecture:** The webhook handler decodes two more payload fields and passes a `Delivery`
value instead of a bare issue number. A delivery that merged a pull request into the loop's
`default_branch`, in a loop with `tend_pr: true`, arms a short per-loop timer. When the timer
fires, the worker runs `loopcmd.TendSweep` for that branch. `TendSweep` reads GitHub outside the
loop lock, takes the lock, and then builds the snapshot `Tick` builds, calls the same
`engine.Decide`, keeps only `KindTend` decisions, and dispatches at most `maxTendPerSweep` of
them. The merged pull request's own `TickIssue` pass is unchanged and still runs immediately.

**Tech Stack:** Go 1.25, `github.com/google/go-github` (webhook payload), SQLite via
`internal/store`, `log/slog`.

**Spec:** `docs/superpowers/specs/2026-08-22-merge-triggered-tend-sweep-design.md`

## What the plan review changed

Recorded because three findings reversed decisions the spec states, and an executor reading the
spec alone would build the earlier design.

- **The coalescing window is back.** The spec argues no debounce is needed because the loop lock
  is non-blocking and `engine.Decide` suppresses a pull request with a live tend dispatch. That
  covers merges that arrive *at the same time*. It does not cover a merge train: each merge is a
  separate sweep, and a tend agent that already finished no longer suppresses anything, so ten
  merges over ten minutes are ten sweeps. A per-loop timer collapses a train into one sweep.
- **The sweep is capped.** `dispatch` has no ceiling (`tick.go:348`). Without a cap, one merge in
  a repository with forty stale review pull requests spawns forty detached agents running with
  `--permission-mode bypassPermissions`, each in its own worktree.
- **The base branch is enforced, not asserted.** The spec's third limit — "the cause matches the
  effect" — was a claim with no code behind it. The gate checked where the *merge* landed while
  the staleness test used each pull request's own `pr.BaseRef`, so a merge into `master` could
  dispatch a rebase for a pull request targeting `release/1.0`. `TendSweep` now takes the merged
  branch and skips any pull request that does not target it.
- **The lock is held for less.** `BehindBy` is `CompareCommits`, a GitHub API call
  (`ghub.go:204`), not local git. The reads therefore move outside the lock and only the fetch
  and the dispatch stay inside it. See Task 2 for why that matters.

## Global Constraints

This repository has **no conventions document** — there is no `AGENTS.md`, `CLAUDE.md`,
`CONTRIBUTING.md`, or `STANDARDS.md` at the root. The rules below are read from the code itself.
Recommend a follow-up run of `identify-standards` to record them once, instead of restating them
in every plan.

Binding rules this change can touch:

- **`make check` must pass** — `fmtcheck` + `vet` + `golangci-lint` + the full suite (the
  `check` target in the `Makefile`). `make test/race` must also pass; the listener is
  concurrent and CI runs it.
- **Comments state the failure the code prevents**, and cite other code **by symbol, never by
  line number**. The repo has exactly six `file.go:NNN` citations and five point into vendored
  `go-github`. In-repo cross-references read "see `loopcmd.Tick`", "See `store.BeginDispatch`",
  "See `ghub.DeliveryCache`, which states the same rule where the memo lives". Follow that: line
  numbers rot, and two in the first draft of this plan were already wrong.
- **Seams are struct fields, not package functions.** `loopcmd.Deps` and `listener.Worker`
  declare every collaborator as a field so tests need no registry, database, token, or real
  clock. Any new collaborator follows this.
- **`Worker`'s fields are written once**, before the value is shared with the HTTP handler and
  the wake loop. Only `pending` and `orphans` are mutated at run time, and only under `mu`. New
  run-time state joins them under `mu` or does not exist.
- **Attacker-controlled payload text is bounded before it is logged** — see `safeText`,
  `safeLabels`, `safeAction`, `safeDeliveryID` in `handler.go`. The log file is not rotated.
- **Decision policy is single-sourced in `engine.Decide`.** `tickIssue` forbids a scoped copy of
  the retry, veto, and double-dispatch rules. The sweep calls `Decide` and filters its output; it
  must not re-implement any rule.
- **Commit messages** use `type(scope): subject` for single-package work — `feat(listener):`,
  `feat(loopcmd):`, `docs(config):`, `docs(readme):` — and bare `type:` only for multi-package
  commits. Commits authored by the agent end with the
  `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>` trailer. (The trailer is
  the harness's rule, not the repo's: no human-authored commit in this history carries one.)

## Verified external API (do not re-derive)

Read from source in this repository on 2026-08-22.

```go
// internal/loopcmd/tick.go — Deps
type Deps struct {
    Store      *store.Store
    ProjectID  string
    GH         ghub.Client
    WT         *worktree.Manager
    SelfPath   string
    ConfigPath string
    Now        func() time.Time
    Spawn      func(selfPath string, dispatchID int64, projectID, configPath, runnerLog string) (int, error)
    IsAlive    func(pid int, dispatchID int64) bool
    Fetch      func() error
}

type Summary struct {
    Started, Resumed, Retried, Tended, Parked, Live, Orphans int
    BreakerTripped                                           bool
}

// reapDead retires dead rows. It guards MarkNeedsRetry with
// `d.Kind != store.KindTend`, so retiring a tend row writes NO issue state.
func reapDead(cfg *config.Config, deps Deps, running []store.Dispatch,
    states map[int]store.IssueState, now time.Time, sum *Summary) ([]store.Dispatch, error)

// act dispatches one decision. KindTend spawns store.KindTend and counts sum.Tended.
// It applies NO ceiling of its own.
func act(ctx context.Context, cfg *config.Config, deps Deps, d engine.Decision,
    now time.Time, sum *Summary) error

// internal/ghub/ghub.go — BehindBy is CompareCommits, a GitHub API call. It does
// NOT read the local checkout, so it does not depend on Fetch having run.
ListOpenIssues(ctx context.Context, owner, repo string) ([]Issue, error)
ListOpenPullRequests(ctx context.Context, owner, repo string) ([]PullRequest, error)
BehindBy(ctx context.Context, owner, repo, base, head string) (int, error)

// internal/ghub/types.go — PullRequest. Trusted is decided at the API boundary
// (convertPR): head repo must equal this repo, author must be OWNER/MEMBER/
// COLLABORATOR, and both refs must pass SafeRef.
type PullRequest struct {
    Number            int
    HeadRef, BaseRef  string
    Body              string
    Draft             bool
    HeadRepo          string
    AuthorAssociation string
    Trusted           bool
}
type Issue struct { Number int; Title string; Labels []string; UpdatedAt time.Time }

// internal/engine — Decide is pure. LinkPR SKIPS any pull request with !pr.Trusted
// and requires a closingRef match ("Closes #N") in Body.
func Decide(cfg *config.Config, snap Snapshot, st State, now time.Time) Plan
func LinkPR(issueNumber int, prs []ghub.PullRequest) (ghub.PullRequest, bool)
type Snapshot struct { Issues []ghub.Issue; PRs []ghub.PullRequest; BehindBy map[int]int }
type State struct { Issues map[int]store.IssueState; Running []store.Dispatch; CooldownUntil time.Time }
const KindTend Kind = "tend"

// internal/lock — LOCK_EX|LOCK_NB. Returns ErrHeld at once; never waits.
func Acquire(path string) (*Lock, error)

// internal/listener/route.go — Target HAS a Repo field. Both producers populate it.
type Target struct {
    ProjectID, ProjectName, Dir, ConfigPath, LoopName, Repo string
}

// internal/listener/listener.go — the seam this plan widens.
Tick func(ctx context.Context, repo string, number int)
// internal/listener/handler.go — Handler TAKES A CONTEXT.
func (s *Server) Handler(ctx context.Context) http.Handler
```

**Test helpers that already exist. Do not write a second set.**

- `internal/loopcmd/tick_test.go` — `tickConfig(t)`, `newDeps(t, cfg, gh, &spawned)`, `fakeGH`
  (fields `issues`, `prs`, `behind map[int]int`, plus call counters `listedIssues`, `listedPRs`).
- `internal/loopcmd/tickreason_test.go` — `liveDispatch(t, cfg, deps, store.Dispatch)`, which
  registers pid 4242 so the `pidGracePeriod` branch is not taken.
- `internal/listener/work_test.go` — `newHarness(db)` returning `*harness` with `h.w`,
  `h.targets`, `h.runFn`, `h.timers`, and the locking accessors `h.ranLoops()`, `h.ranNumbers()`,
  `h.counts()`, `h.pendingLen()`, plus `timers.len()` and `timers.at(t, i)`.
- `internal/listener/handler_test.go` — `newServer(t, tickCh)`, `tickCall`, `doRequest`,
  `waitTick`, `assertNoTick`, `sha256Sig`. **`doRequest` does NOT sign the body**; every caller
  passes `github.SHA256SignatureHeader: sha256Sig(...)` itself.

---

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `internal/listener/work.go` | `Delivery`; the sweep timer; the `RunTend` seam | Modify |
| `internal/listener/handler.go` | Decode `merged` + `base.ref`; build a `Delivery` | Modify |
| `internal/listener/listener.go` | Widen the `Tick` seam to take a `Delivery` | Modify |
| `internal/loopcmd/tendsweep.go` | The tend-only sweep | Create |
| `internal/ghub/deliverycache.go` | Correct the `ListOpenIssues` comment | Modify |
| `cmd/agent-utils/listener.go` | Wire the widened seam | Modify |
| `docs/configuration.md`, `README.md` | Record when a sweep runs | Modify |

`Delivery` goes at the top of `work.go`, above `Deliver`, **not** in a new file. The listener
package splits by role, not by type, and `Target` — the other value the handler path passes
around — lives at the top of `route.go`, the file of its producer. Its test goes in the existing
`work_test.go`.

---

### Task 1: The `Delivery` value and the widened seam

Carry the merge facts from the handler to the worker. The handler cannot decide whether a base
ref matters — each loop has its own `default_branch`, and one repository can have several loops
— so it reports the fact and the worker judges it.

**Files:**
- Modify: `internal/listener/work.go` (add `Delivery` above `Deliver`; change `Deliver`,
  `tickOne`, `tickFresh`)
- Modify: `internal/listener/handler.go` (the payload struct; step 8, beside `number :=
  body.Issue.Number`)
- Modify: `internal/listener/listener.go` (the `Tick` field)
- Modify: `cmd/agent-utils/listener.go` (`wrapTick`, and the `Tick:` wiring)
- Test: `internal/listener/work_test.go`, `internal/listener/handler_test.go`,
  `internal/listener/listener_test.go`, `cmd/agent-utils/listener_test.go`

**Interfaces:**
- Produces: `listener.Delivery{Repo string; Number int; MergedInto string}` and
  `func (d Delivery) IsMergeInto(branch string) bool`. Task 3 consumes both.
- Produces: `Server.Tick func(ctx context.Context, d Delivery)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/listener/work_test.go`:

```go
// MergedInto is the ONE field that says "the default branch moved." An empty
// value must never match a branch name, or every ordinary delivery -- an
// opened issue, a moved label -- would start a repository-wide sweep. That is
// the regression Worker.RunIssue records.
func TestIsMergeIntoRequiresAMergedBaseRef(t *testing.T) {
	cases := []struct {
		name string
		d    Delivery
		arg  string
		want bool
	}{
		{"a merge into the branch", Delivery{Repo: "o/r", Number: 7, MergedInto: "master"}, "master", true},
		{"a merge into another branch", Delivery{Repo: "o/r", Number: 7, MergedInto: "feature"}, "master", false},
		{"not a merge", Delivery{Repo: "o/r", Number: 7}, "master", false},
		{"not a merge, and the loop names no branch", Delivery{Repo: "o/r", Number: 7}, "", false},
		{"a merge, but the loop names no branch", Delivery{Repo: "o/r", Number: 7, MergedInto: "master"}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.IsMergeInto(tc.arg); got != tc.want {
				t.Errorf("IsMergeInto(%q) = %v, want %v", tc.arg, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/listener/ -run TestIsMergeInto -v`
Expected: FAIL — `undefined: Delivery`.

- [ ] **Step 3: Add `Delivery` to `internal/listener/work.go`**

Place it directly above `Deliver`.

```go
// Delivery is what one accepted webhook delivery tells the worker.
//
// It replaced a bare (repo, number) pair because a merged pull request must
// start more work than an ordinary delivery, and the handler cannot judge
// that on its own: the decision needs a loop's default_branch, and one
// repository may be watched by several loops with different ones.
type Delivery struct {
	// Repo is the "owner/name" the delivery named. handleWebhook has already
	// matched it against repoFullName, so nothing downstream re-validates it.
	Repo string
	// Number is the issue or pull request the delivery named. Every accepted
	// delivery carries one; handleWebhook rejects a delivery without one
	// rather than answering 202 for work it cannot name.
	Number int
	// MergedInto is the base branch of a pull request this delivery reports as
	// MERGED, and is empty for every other delivery. Empty is the only safe
	// default: it is what keeps an opened issue or a moved label from arming a
	// repository-wide sweep. See Worker.Deliver for the regression that makes
	// this the important property of this type.
	MergedInto string
}

// IsMergeInto reports whether this delivery merged a pull request into branch.
//
// An empty branch never matches, even against an empty MergedInto. A loop with
// no default_branch names no branch to compare against, so "they are both
// empty" is not agreement, it is two absent values.
func (d Delivery) IsMergeInto(branch string) bool {
	return branch != "" && d.MergedInto == branch
}
```

- [ ] **Step 4: Run the test and confirm it passes**

Run: `go test ./internal/listener/ -run TestIsMergeInto -v`
Expected: PASS.

- [ ] **Step 5: Widen the `Tick` seam**

In `internal/listener/listener.go`, change the `Tick` field, keeping its existing comment and
adding the merge sentence:

```go
	// Tick acts on the delivery: the issue it named, and -- when it reports a
	// merge into the loop's default branch -- the tend sweep that merge calls
	// for. It is a seam so a test can drive the handler without opening a
	// database or starting an agent.
	//
	// The number is not decoration. A delivery says "something about this
	// issue changed"; reconciling the whole repository instead spends a token
	// budget on every open issue, per project watching that repository, on
	// every delivery -- which is how creating one unlabelled test issue
	// dispatched a tend agent for an unrelated one. Delivery.MergedInto is the
	// single, narrow exception, and it is empty on every other delivery.
	Tick func(ctx context.Context, d Delivery)
```

- [ ] **Step 6: Decode the two payload fields**

In `internal/listener/handler.go`, replace the `PullRequest` member of the anonymous `body`
struct:

```go
			// Merged and Base carry the one fact that arms a tend sweep: this
			// delivery merged a pull request, and into which branch. Both are
			// decoded here and judged per loop in Worker.tickOne, because
			// default_branch is loop configuration the handler does not hold.
			PullRequest struct {
				Number int        `json:"number"`
				Title  string     `json:"title"`
				Labels []nameOnly `json:"labels"`
				Merged bool       `json:"merged"`
				Base   struct {
					Ref string `json:"ref"`
				} `json:"base"`
			} `json:"pull_request"`
```

- [ ] **Step 7: Derive `mergedInto` in step 8, beside the number**

`handler.go` numbers its stages. The derivation belongs in **step 8**, immediately after the
`number <= 0` rejection — that is the analogue: decode, derive, validate, all before the dedup
cache and before the pool. Putting it in step 10 would also drop the field from the
`"dropping delivery: worker pool full"` line.

Extend step 8's leading comment to say there are now three attacker-controlled values, then add:

```go
		// A merged pull request is the only delivery that arms a sweep. The
		// ACTION is checked as well as the flag: GitHub sends merged: true on
		// later pull_request actions too (edited, unlabeled), and only the
		// close is the moment the base branch moved. The event is checked
		// because pull_request_review, pull_request_review_comment and
		// issue_comment all carry a pull_request object as well.
		var mergedInto string
		if event == "pull_request" && body.Action == "closed" && body.PullRequest.Merged {
			mergedInto = body.PullRequest.Base.Ref
		}
```

Add it to the accepted-delivery log line, only when present, matching how the label and title
fields are already handled:

```go
			if mergedInto != "" {
				attrs = append(attrs, "merged_into", safeText(mergedInto))
			}
```

Then change the pool goroutine's call and the pool-full drop line:

```go
				s.Tick(ctx, Delivery{Repo: repo, Number: number, MergedInto: mergedInto})
```

- [ ] **Step 8: Thread `Delivery` through the worker**

Change these signatures. Do not change behavior in this step — Task 3 adds the sweep.

```go
func (w *Worker) Deliver(ctx context.Context, d Delivery)
func (w *Worker) tickOne(ctx context.Context, t Target, d Delivery, acc *access)
```

Inside `Deliver`, replace `repo` with `d.Repo` and `number` with `d.Number`.

`schedule`'s signature does **not** change: it keeps taking `number int`. `Target` has a `Repo`
field, so `tickFresh` rebuilds a plain `Delivery`:

```go
	// A retry re-runs the ISSUE pass only. A retry may fire minutes after the
	// merge that caused it, and a sweep then is not what that merge asked for:
	// the base has moved again or has not, and the next merge arms a sweep
	// either way. MergedInto is left empty here on purpose.
	w.tickOne(ctx, t, Delivery{Repo: t.Repo, Number: number}, acc)
```

- [ ] **Step 9: Update every call site and stub**

This is the step that makes `make check` pass. Four files:

1. `internal/listener/work_test.go` — **~29 `h.w.Deliver(context.Background(), "o/r", N)` call
   sites**, at lines 374, 394, 400, 419, 447, 475, 490, 509, 536, 559, 580, 598, 627, 628, 651,
   835, 968, 969, 981, 1176, 1213, 1234, 1282, 1304, 1305, 1322, 1343, 1365, 1417. Each becomes
   `h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: N})`, preserving whatever
   number that site already passes.
2. `internal/listener/handler_test.go` — 9 `Tick:` stubs (lines 93, 318, 334, 357, 636, 690, 750,
   818, 871) and `internal/listener/listener_test.go` — 4 (lines 33, 51, 67, 99). A stub that
   ignores its argument becomes `func(context.Context, Delivery) {}`; one that reads
   `repo`/`number` reads `d.Repo`/`d.Number`.
3. `cmd/agent-utils/listener.go` — `wrapTick` is defined near line 674, not 391 (391 is only the
   `Tick: wrapTick(tickCtx, w.Deliver)` wiring). New signature:
   `func wrapTick(tickCtx context.Context, deliver func(context.Context, listener.Delivery)) func(context.Context, listener.Delivery)`,
   logging `d.Repo` / `d.Number`.
4. `cmd/agent-utils/listener_test.go` — 3 sites: line 510 (`Tick: func(context.Context, string,
   int) {}`), line 548 with its call at 532, and line 556 with its call at 558. Calls become
   `tick(handlerCtx, listener.Delivery{Repo: "owner/repo", Number: 7})`.

- [ ] **Step 10: Add a handler test proving the merge facts reach the seam**

First widen the `tickCall` fixture and make `newServer`'s fake `Tick` record the new field:

```go
type tickCall struct {
	repo       string
	number     int
	mergedInto string
}
```

Then append the test. `doRequest` does NOT sign — pass the signature yourself, as the existing
`pull_request` test near line 357 does.

```go
// The handler must report a merge, and must NOT report anything else as one.
// MergedInto is what arms a repository-wide sweep, so a false positive here is
// the regression Worker.RunIssue records, reintroduced at the front door.
func TestHandlerReportsOnlyAMergedPullRequestAsAMerge(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		payload string
		want    string
	}{
		{
			name:    "a merged pull request",
			event:   "pull_request",
			payload: `{"action":"closed","repository":{"full_name":"o/r"},"pull_request":{"number":7,"merged":true,"base":{"ref":"master"}}}`,
			want:    "master",
		},
		{
			name:    "a closed but unmerged pull request",
			event:   "pull_request",
			payload: `{"action":"closed","repository":{"full_name":"o/r"},"pull_request":{"number":7,"merged":false,"base":{"ref":"master"}}}`,
			want:    "",
		},
		{
			// GitHub sends merged: true on later pull_request actions too.
			// Only the close is the moment the base branch moved.
			name:    "an edited pull request that was already merged",
			event:   "pull_request",
			payload: `{"action":"edited","repository":{"full_name":"o/r"},"pull_request":{"number":7,"merged":true,"base":{"ref":"master"}}}`,
			want:    "",
		},
		{
			// pull_request_review carries a pull_request object as well, and
			// it is not a merge whatever that object says.
			name:    "a review on a merged pull request",
			event:   "pull_request_review",
			payload: `{"action":"submitted","repository":{"full_name":"o/r"},"pull_request":{"number":7,"merged":true,"base":{"ref":"master"}}}`,
			want:    "",
		},
		{
			name:    "an issue delivery",
			event:   "issues",
			payload: `{"action":"labeled","repository":{"full_name":"o/r"},"issue":{"number":7}}`,
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tickCh := make(chan tickCall, 1)
			s := newServer(t, tickCh)
			srv := httptest.NewServer(s.Handler(context.Background()))
			t.Cleanup(srv.Close)

			body := []byte(tc.payload)
			resp := doRequest(t, srv.URL+"/webhook", body, map[string]string{
				github.EventTypeHeader:       tc.event,
				github.SHA256SignatureHeader: sha256Sig(testSecret, body),
			})
			defer resp.Body.Close()

			got := waitTick(t, tickCh)
			if got.mergedInto != tc.want {
				t.Errorf("mergedInto = %q, want %q", got.mergedInto, tc.want)
			}
			if got.number != 7 {
				t.Errorf("number = %d, want 7", got.number)
			}
		})
	}
}
```

Read the existing test near line 357 first and copy its exact server-construction and secret
constant; use those where they differ from the sketch above.

- [ ] **Step 11: Run the gates**

Run: `make check && make test/race`
Expected: all green.

- [ ] **Step 12: Commit**

```bash
git add internal/listener cmd/agent-utils
git commit -m "$(cat <<'EOF'
feat(listener): carry merge facts from the webhook delivery to the worker

The handler reports whether a delivery merged a pull request and into
which branch. The comparison against a loop's default_branch happens in
the worker, because one repository may be watched by several loops with
different ones.

The action and the event are both checked, not just the merged flag:
GitHub sends merged: true on later pull_request actions, and three other
subscribed events carry a pull_request object of their own.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

**Acceptance criteria:** `TestIsMergeIntoRequiresAMergedBaseRef` and
`TestHandlerReportsOnlyAMergedPullRequestAsAMerge` pass. `make check` and `make test/race` green.
No behavior changed yet: no sweep runs.

**review: yes** — parses attacker-controlled payload fields and defines the flag that gates
repository-wide dispatch.

---

### Task 2: `loopcmd.TendSweep`

The tend-only sweep. It reuses `engine.Decide` and filters the result; it re-implements no rule.

**Files:**
- Create: `internal/loopcmd/tendsweep.go`
- Modify: `internal/ghub/deliverycache.go` (one comment — see Step 6)
- Test: `internal/loopcmd/tendsweep_test.go` (create), `internal/loopcmd/tick_test.go` (extend
  `fakeGH`)

**Interfaces:**
- Consumes: `loopcmd.Deps`, `config.Config`.
- Produces: `func TendSweep(ctx context.Context, cfg *config.Config, deps Deps, base string) (Summary, error)`.
  Task 3 consumes this exact signature. `base` is the branch the merge landed on.

- [ ] **Step 1: Extend `fakeGH` so a comparison can fail**

`fakeGH.BehindBy` cannot fail today, so the branch that survives a failed comparison would be
untested. Add one field and one branch in `internal/loopcmd/tick_test.go`:

```go
	// behindErr makes BehindBy fail for one pull request. A comparison CAN
	// fail in production -- a force-pushed head, a deleted branch -- and a
	// sweep must survive it, so a fake that cannot fail leaves the branch
	// that survives it untested.
	behindErr map[int]error
```

```go
func (f *fakeGH) BehindBy(_ context.Context, _, _, _, head string) (int, error) {
	for _, pr := range f.prs {
		if pr.HeadRef == head {
			if err := f.behindErr[pr.Number]; err != nil {
				return 0, err
			}
			return f.behind[pr.Number], nil
		}
	}
	return 0, nil
}
```

- [ ] **Step 2: Write the failing tests**

Create `internal/loopcmd/tendsweep_test.go`. Note the `config` import — `sweepConfig` returns
`*config.Config`.

```go
package loopcmd

import (
	"context"
	"errors"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// sweepConfig is tickConfig with tending on. tickConfig leaves TendPR false,
// and TendSweep must produce nothing for a loop that does not tend.
func sweepConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := tickConfig(t)
	cfg.TendPR = true
	cfg.TendPrompt = "rebase #{{.Issue.Number}}"
	return cfg
}

// reviewPRFixture is a trusted pull request that closes issue n. Trusted is
// load-bearing: engine.LinkPR skips !pr.Trusted, so a fixture without it links
// nothing and every assertion below reads zero for the wrong reason.
func reviewPRFixture(issue, pr int, base string) ghub.PullRequest {
	return ghub.PullRequest{
		Number:  pr,
		Body:    fmt.Sprintf("Closes #%d", issue),
		HeadRef: fmt.Sprintf("issue-%d", issue),
		BaseRef: base,
		Trusted: true,
	}
}

// The sweep dispatches a rebase for a review issue whose pull request is
// behind, and does nothing for an issue that merely carries the trigger label.
// A FULL tick would start an agent for that one. This pass must not: it
// answers a merge, and a merge calls for a rebase and nothing else.
func TestTendSweepDispatchesOnlyTendDecisions(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{
			{Number: 1, Labels: []string{"review"}},
			{Number: 2, Labels: []string{"trigger"}},
		},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "master")},
		behind: map[int]int{11: 3},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TendSweep(context.Background(), cfg, deps, "master")
	if err != nil {
		t.Fatalf("TendSweep: %v", err)
	}

	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1", sum.Tended)
	}
	if sum.Started != 0 {
		t.Errorf("Started = %d, want 0: a sweep must not start an agent for a triggered issue", sum.Started)
	}
	if spawned != 1 {
		t.Errorf("spawned = %d, want 1", spawned)
	}
}

// A merge into master says nothing about a pull request targeting another
// branch. Rebasing that branch would be a tend agent dispatched for an
// unrelated event -- the shape of the incident Worker.Deliver records.
func TestTendSweepIgnoresAPullRequestTargetingAnotherBranch(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "release/1.0")},
		behind: map[int]int{11: 5},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TendSweep(context.Background(), cfg, deps, "master")
	if err != nil {
		t.Fatalf("TendSweep: %v", err)
	}
	if sum.Tended != 0 || spawned != 0 {
		t.Errorf("Tended = %d, spawned = %d, want 0 and 0", sum.Tended, spawned)
	}
	// The skip happens before the comparison, so it costs no API call either.
	if len(gh.fetchedPRs) != 0 {
		t.Errorf("compared %d pull requests, want 0", len(gh.fetchedPRs))
	}
}

// A pull request level with its base produces nothing. Silence is correct.
func TestTendSweepIgnoresAnUpToDatePullRequest(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "master")},
		behind: map[int]int{11: 0},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TendSweep(context.Background(), cfg, deps, "master")
	if err != nil {
		t.Fatalf("TendSweep: %v", err)
	}
	if sum.Tended != 0 {
		t.Errorf("Tended = %d, want 0", sum.Tended)
	}
}

// A loop that does not tend produces nothing, whoever calls, and costs no API
// call. The caller checks this too; TendSweep is exported, so it checks itself.
func TestTendSweepDoesNothingWhenTendPRIsOff(t *testing.T) {
	cfg := sweepConfig(t)
	cfg.TendPR = false
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "master")},
		behind: map[int]int{11: 3},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TendSweep(context.Background(), cfg, deps, "master")
	if err != nil {
		t.Fatalf("TendSweep: %v", err)
	}
	if sum.Tended != 0 || spawned != 0 {
		t.Errorf("Tended = %d, spawned = %d, want 0 and 0", sum.Tended, spawned)
	}
	if gh.listedIssues != 0 {
		t.Errorf("listedIssues = %d, want 0: a loop that does not tend must cost no API call", gh.listedIssues)
	}
}

// One merge must not spawn an unbounded number of agents. Each dispatch is a
// detached process with permission prompts disabled, in its own worktree.
func TestTendSweepCapsDispatchesPerSweep(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{behind: map[int]int{}}
	for i := 1; i <= maxTendPerSweep+3; i++ {
		gh.issues = append(gh.issues, ghub.Issue{Number: i, Labels: []string{"review"}})
		gh.prs = append(gh.prs, reviewPRFixture(i, 100+i, "master"))
		gh.behind[100+i] = 2
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TendSweep(context.Background(), cfg, deps, "master")
	if err != nil {
		t.Fatalf("TendSweep: %v", err)
	}
	if sum.Tended != maxTendPerSweep {
		t.Errorf("Tended = %d, want %d", sum.Tended, maxTendPerSweep)
	}
	if spawned != maxTendPerSweep {
		t.Errorf("spawned = %d, want %d", spawned, maxTendPerSweep)
	}
}

// A dead TEND row is retired, or its pull request is never tended again. A
// dead row of any OTHER kind is left alone: retiring it would flag an issue
// this pass never examined for retry, the hazard tickIssue describes.
//
// behind is 0 so no NEW tend row is created. Reaping happens before Decide
// either way, so the property under test is unaffected -- and with a non-zero
// behind the fresh tend row would be indistinguishable from an unreaped one.
func TestTendSweepRetiresDeadTendRowsOnly(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{
			{Number: 1, Labels: []string{"review"}},
			{Number: 2, Labels: []string{"in-flight"}},
		},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "master")},
		behind: map[int]int{11: 0},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.IsAlive = func(int, int64) bool { return false }

	liveDispatch(t, cfg, deps, store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, Kind: store.KindTend, PRNumber: 11, SessionID: "t1",
	})
	liveDispatch(t, cfg, deps, store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 2, Kind: store.KindStart, SessionID: "s1",
	})

	if _, err := TendSweep(context.Background(), cfg, deps, "master"); err != nil {
		t.Fatalf("TendSweep: %v", err)
	}

	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	var sawTend, sawStart bool
	for _, d := range running {
		switch d.Kind {
		case store.KindTend:
			sawTend = true
		case store.KindStart:
			sawStart = true
		}
	}
	if sawTend {
		t.Error("the dead tend row was not retired")
	}
	if !sawStart {
		t.Error("a dead start row was retired by a tend sweep; only tend rows may be")
	}
	st, err := deps.Store.IssueState(cfg.Name, cfg.Repo, 2)
	if err != nil {
		t.Fatal(err)
	}
	if st.NeedsRetry {
		t.Error("issue 2 was flagged for retry by a pass that never examined it")
	}
}

// A stale checkout is the one the tend agent would rebase in. The sweep stops
// rather than dispatching an agent into it.
func TestTendSweepStopsWhenTheFetchFails(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "master")},
		behind: map[int]int{11: 3},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.Fetch = func() error { return errors.New("network down") }

	sum, err := TendSweep(context.Background(), cfg, deps, "master")
	if err == nil {
		t.Fatal("want an error when the fetch failed, got nil")
	}
	if sum.Tended != 0 || spawned != 0 {
		t.Errorf("Tended = %d, spawned = %d, want 0 and 0", sum.Tended, spawned)
	}
}

// One unusable pull request must not stop the pass. If it did, anyone able to
// open a pull request could stop every rebase this loop would otherwise do.
func TestTendSweepContinuesPastAFailedComparison(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{
			{Number: 1, Labels: []string{"review"}},
			{Number: 2, Labels: []string{"review"}},
		},
		prs: []ghub.PullRequest{
			reviewPRFixture(1, 11, "master"),
			reviewPRFixture(2, 12, "master"),
		},
		behind:    map[int]int{11: 3, 12: 3},
		behindErr: map[int]error{11: errors.New("no common ancestor")},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TendSweep(context.Background(), cfg, deps, "master")
	if err != nil {
		t.Fatalf("TendSweep: %v", err)
	}
	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1: the second pull request must still be tended", sum.Tended)
	}
}

// The breaker counts retry decisions, and this pass discards every one of
// them. A pass that will not act on that evidence must not stop the passes
// that would.
func TestTendSweepWritesNoCooldown(t *testing.T) {
	cfg := sweepConfig(t)
	cfg.Retry.Breaker.OrphanThreshold = 1
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{reviewPRFixture(1, 11, "master")},
		behind: map[int]int{11: 3},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	// One issue already needing a retry is enough to trip a threshold of 1.
	if err := deps.Store.MarkNeedsRetry(cfg.Name, cfg.Repo, 1, deps.Now(), nil); err != nil {
		t.Fatal(err)
	}

	if _, err := TendSweep(context.Background(), cfg, deps, "master"); err != nil {
		t.Fatalf("TendSweep: %v", err)
	}

	until, err := deps.Store.CooldownUntil(cfg.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !until.IsZero() {
		t.Errorf("CooldownUntil = %v, want zero: a tend sweep must not trip the breaker", until)
	}
}
```

Add `"fmt"` to the import block for `reviewPRFixture`. Before running, confirm `fakeGH` records
compared pull requests in a field usable by
`TestTendSweepIgnoresAPullRequestTargetingAnotherBranch`; if `fetchedPRs` counts single-PR
fetches rather than comparisons, add a `compared []int` counter to `fakeGH.BehindBy` in Step 1
and assert on that instead.

- [ ] **Step 3: Run the tests and confirm they fail**

Run: `go test ./internal/loopcmd/ -run TendSweep -v`
Expected: FAIL — `undefined: TendSweep`.

- [ ] **Step 4: Write `internal/loopcmd/tendsweep.go`**

```go
package loopcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// maxTendPerSweep is how many rebases one merge may dispatch.
//
// act applies no ceiling of its own: it calls dispatch once per decision, and
// each dispatch is a detached agent process with permission prompts disabled,
// in its own git worktree. A repository with forty stale review pull requests
// would answer one merge with forty of them. The cap is a constant rather than
// a configuration field because no operator has needed a different value yet;
// promote it if one does. What is left over is logged, never dropped silently,
// and the next merge takes the next batch.
const maxTendPerSweep = 10

// TendSweep rebases the stale pull requests of one loop, and does nothing else.
//
// base is the branch the merge landed on. It exists so the pass can enforce the
// thing that makes it safe rather than assert it: a merge into master says
// nothing about a pull request targeting release/1.0, and rebasing that branch
// would be a tend agent dispatched for an unrelated event -- the shape of the
// incident Worker.Deliver records.
//
// # Why this is not the reconcile that was removed
//
// Worker.RunIssue records that a full reconcile per delivery was removed: it
// burned a token budget on every open issue of every project watching the
// repository, and one unlabelled test issue dispatched a tend agent for an
// unrelated issue whose pull request was 16 commits behind. This pass acts on
// many issues again, so it must not become that. Four things keep it apart:
//
//  1. It runs for ONE event -- a pull request merged into the loop's default
//     branch. Opening an issue, moving a label and commenting arm no sweep.
//  2. It keeps TEND decisions only. Every other kind is dropped below, before
//     anything is dispatched.
//  3. It only considers pull requests targeting the branch that actually moved.
//  4. It dispatches at most maxTendPerSweep of them.
//
// Decisions come from engine.Decide, the same function the full tick calls. A
// scoped copy of the veto, live-dispatch and link rules would be a second
// implementation free to drift; see tickIssue, which states the same rule.
//
// # Where the lock is taken
//
// The GitHub reads happen BEFORE the lock and the dispatch happens under it.
// TickIssue holds the loop lock for one issue fetch; a sweep that held it for a
// paginated issue listing, a paginated pull request listing and a comparison
// per review issue would hold it for tens of seconds. That matters because of
// what the holder does to everyone else: Worker.issuePass drops a delivery that
// finds the lock held, with no retry, on the reasoning that "the tick already
// holding the lock reads the same GitHub state a moment later than this one
// would have". That reasoning is true of a TickIssue holder and FALSE of this
// one, which decides no issue but the ones it tends. Every second this pass
// holds the lock is a second in which a labelled issue can be dropped and never
// picked up. So it holds the lock only for the fetch and the dispatch.
//
// BehindBy is CompareCommits, a GitHub API call, so the comparisons do not
// depend on Fetch having run and lose nothing by preceding it.
func TendSweep(ctx context.Context, cfg *config.Config, deps Deps, base string) (Summary, error) {
	var sum Summary

	// Checked before anything is read. The caller checks it too; TendSweep is
	// exported, so a loop that does not tend must cost nothing whoever calls.
	if !cfg.TendPR {
		return sum, nil
	}

	snap, err := tendSnapshot(ctx, cfg, deps, base)
	if err != nil {
		return sum, err
	}

	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		return sum, err
	}
	defer l.Release()

	return tendDispatch(ctx, cfg, deps, snap, base, &sum)
}

// tendSnapshot reads GitHub and returns what engine.Decide needs. It takes no
// lock and touches no git.
func tendSnapshot(ctx context.Context, cfg *config.Config, deps Deps, base string) (engine.Snapshot, error) {
	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	issues, err := deps.GH.ListOpenIssues(ctx, owner, repo)
	if err != nil {
		return engine.Snapshot{}, err
	}
	prs, err := deps.GH.ListOpenPullRequests(ctx, owner, repo)
	if err != nil {
		return engine.Snapshot{}, err
	}

	snap := engine.Snapshot{Issues: issues, PRs: prs, BehindBy: map[int]int{}}
	for _, iss := range issues {
		if !iss.HasLabel(cfg.Labels.Review) {
			continue
		}
		pr, ok := engine.LinkPR(iss.Number, prs)
		if !ok {
			continue
		}
		// The branch that moved is the only reason this pass exists. A pull
		// request targeting anything else is behind for reasons this merge
		// knows nothing about. Skipping here also saves the comparison.
		if pr.BaseRef != base {
			continue
		}
		behind, err := deps.GH.BehindBy(ctx, owner, repo, pr.BaseRef, pr.HeadRef)
		if err != nil {
			// One unusable pull request must not abandon the sweep. If this
			// returned early, anyone able to open a pull request could stop
			// every rebase this loop would otherwise do.
			slog.Warn("compare failed; skipping this pull request",
				"loop", cfg.Name, "issue", iss.Number, "pr", pr.Number, "err", err)
			continue
		}
		snap.BehindBy[pr.Number] = behind
		if err := deps.Store.PutPRLink(store.PRLink{
			Loop: cfg.Name, Repo: cfg.Repo, Number: iss.Number,
			PRNumber: pr.Number, HeadRef: pr.HeadRef, BaseRef: pr.BaseRef,
			// Written, unlike in Tick and tickIssue, which both leave it zero.
			// RunAgent renders it into the tend prompt, and a zero there tells
			// the agent the opposite of why it was dispatched.
			BehindBy: behind,
		}); err != nil {
			slog.Error("store pr link", "loop", cfg.Name, "issue", iss.Number, "err", err)
		}
	}
	return snap, nil
}

// tendDispatch decides and acts. The caller holds the loop lock.
func tendDispatch(
	ctx context.Context, cfg *config.Config, deps Deps,
	snap engine.Snapshot, base string, sum *Summary,
) (Summary, error) {
	now := deps.Now()

	// Under the lock, and after the reads: the fetch prepares the checkout the
	// tend agent rebases in, so a failure here means there is nothing safe to
	// dispatch INTO. Unlike Tick, which suppresses tending and still reaps and
	// retries, this pass has only tending to do, so it stops.
	if deps.Fetch != nil {
		if err := deps.Fetch(); err != nil {
			return *sum, fmt.Errorf("fetch primary checkout: %w", err)
		}
	}

	states, err := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if err != nil {
		return *sum, err
	}
	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		return *sum, err
	}

	// Retire dead TEND rows only.
	//
	// Every row is still READ: engine.Decide builds liveIssues from them, and a
	// live start agent must keep suppressing a tend for its issue -- an agent
	// working a branch and a tend agent force-pushing it are the same hazard as
	// two agents. But only tend rows are RETIRED. tickIssue states why, where
	// the scoped reaping lives: retiring the loop's rows on a delivery flags
	// issues nobody touched for retry. A tend row cannot do that, because
	// reapDead guards MarkNeedsRetry with `d.Kind != store.KindTend`, so
	// retiring one writes no issue state at all.
	//
	// A dead row of another kind is therefore treated as live here. That is the
	// conservative direction: the cost is a rebase this sweep declines to do,
	// and the alternative cost is a second agent in a worktree that already
	// holds one.
	tendRows := make([]store.Dispatch, 0, len(running))
	live := make([]store.Dispatch, 0, len(running))
	for _, d := range running {
		if d.Kind == store.KindTend {
			tendRows = append(tendRows, d)
			continue
		}
		live = append(live, d)
	}
	liveTend, err := reapDead(cfg, deps, tendRows, states, now, sum)
	if err != nil {
		return *sum, err
	}
	live = append(live, liveTend...)
	sum.Live = len(live)

	st := engine.State{Issues: states, Running: live}
	if st.CooldownUntil, err = deps.Store.CooldownUntil(cfg.Name); err != nil {
		return *sum, err
	}

	plan := engine.Decide(cfg, snap, st, now)
	sum.BreakerTripped = plan.BreakerTripped

	// clearUnreachableDeadlines is deliberately NOT called, for the reason
	// tickIssue gives: this pass looked at review issues, so it holds no
	// evidence about any other stamped row. Tick still runs it.

	// A cooldown already set is OBEYED -- Decide halts on it -- but this pass
	// never WRITES one. The breaker counts retry decisions within one call, and
	// this pass discards every retry decision. A pass that will not act on that
	// evidence must not stop the passes that would.
	var tends []engine.Decision
	if !plan.BreakerTripped {
		for _, d := range plan.Decisions {
			// The boundary that bounds the blast radius of a merge. It is the
			// counterpart of tickIssue's per-issue check, and it is what keeps
			// this from being the per-delivery reconcile that was removed. It
			// must not depend on an invariant living in another package.
			if d.Kind == engine.KindTend {
				tends = append(tends, d)
			}
		}
		// Issue order, so a capped sweep takes the same batch every time
		// rather than one the map iteration happened to produce.
		sort.Slice(tends, func(i, j int) bool { return tends[i].Issue < tends[j].Issue })
	} else {
		slog.Warn("circuit breaker tripped; skipping all dispatch",
			"loop", cfg.Name, "cooldown_until", plan.CooldownUntil)
	}

	dropped := 0
	if len(tends) > maxTendPerSweep {
		dropped = len(tends) - maxTendPerSweep
		tends = tends[:maxTendPerSweep]
	}

	for _, d := range tends {
		if err := act(ctx, cfg, deps, d, now, sum); err != nil {
			// One failed decision must not abandon the rest of the sweep.
			slog.Error("decision failed", "loop", cfg.Name, "kind", d.Kind,
				"issue", d.Issue, "err", err)
		}
	}
	if dropped > 0 {
		// Never silent. A capped sweep that said nothing would read as "every
		// stale pull request was rebased", which is the opposite of the truth.
		slog.Warn("tend sweep hit its per-sweep cap; the rest wait for the next merge",
			"loop", cfg.Name, "dispatched", len(tends), "deferred", dropped)
	}

	// Recorded like any other tick -- including on the breaker path, where Tick
	// and tickIssue also record -- so the counter and the last-tick time keep
	// meaning something in `project loop status`.
	body, _ := json.Marshal(*sum)
	if _, err := deps.Store.RecordTick(cfg.Name, plan.BreakerTripped, string(body)); err != nil {
		return *sum, err
	}
	slog.Info("tend sweep complete", "loop", cfg.Name, "base", base, "summary", string(body))
	return *sum, nil
}
```

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./internal/loopcmd/ -run TendSweep -v`
Expected: PASS.

- [ ] **Step 6: Correct the `DeliveryCache` comment**

`internal/ghub/deliverycache.go` says of `ListOpenIssues`: *"Nothing on the delivery path calls
it."* This change makes that false. Amend it in this commit — a stale invariant here invites a
later reader to memoise the listing, which would make the second loop of a delivery decide from
labels read before the first loop's `EditLabels`, the exact failure the type's lifetime rule
exists to prevent.

```go
// ListOpenIssues is not memoised: it belongs to a pass that reconciles a whole
// repository and must read it as it is now. loopcmd.TendSweep calls it on the
// delivery path, once per loop watching the repository, and that repetition is
// deliberate. Memoising it would let the second loop of one delivery decide
// from labels read before the first loop's EditLabels.
```

- [ ] **Step 7: Mutation-check the four safety properties**

These are the properties the whole change rests on. Prove each test bites:

1. Delete the `d.Kind == engine.KindTend` filter (append every decision). Run
   `-run TendSweepDispatchesOnly`. MUST fail with `Started = 1, want 0`.
2. Delete the `pr.BaseRef != base` skip. Run `-run TargetingAnotherBranch`. MUST fail.
3. Delete the `maxTendPerSweep` truncation. Run `-run CapsDispatchesPerSweep`. MUST fail.
4. Send every row to `tendRows`. Run `-run RetiresDeadTendRowsOnly`. MUST fail.

Restore each after its run, and report which mutants were run and what each did. A test that
passes with its property removed is worse than no test.

- [ ] **Step 8: Run the gates**

Run: `make check && make test/race`
Expected: all green.

- [ ] **Step 9: Commit**

```bash
git add internal/loopcmd internal/ghub/deliverycache.go
git commit -m "$(cat <<'EOF'
feat(loopcmd): tend-only sweep for one loop

TendSweep builds the snapshot Tick builds, calls the same engine.Decide,
and keeps only tend decisions. It retires dead tend rows and no others,
writes no cooldown, and clears no deadlines, so a pass that answers a
merge cannot widen into the per-delivery reconcile that was removed.

It takes the branch the merge landed on and skips any pull request
targeting another one, so "the cause matches the effect" is enforced
rather than asserted, and it dispatches at most maxTendPerSweep rebases
because act applies no ceiling of its own.

The GitHub reads happen before the loop lock is taken. Worker.issuePass
drops a delivery that finds the lock held with no retry, reasoning that
the holder reads the same state a moment later -- true of a TickIssue
holder, false of this one.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

**Acceptance criteria:** all nine `TendSweep` tests pass; all four mutants in Step 7 fail their
test and are reported; `make check` and `make test/race` green.

**review: yes** — this is the blast-radius boundary and the lock-window change.

---

### Task 3: Arm the sweep from the worker

**Files:**
- Modify: `internal/listener/work.go` (`Worker` struct, `NewWorker`, `tickOne`, `stopAll`)
- Test: `internal/listener/work_test.go`

**Interfaces:**
- Consumes: `Delivery`, `Delivery.IsMergeInto` (Task 1); `loopcmd.TendSweep` (Task 2).
- Produces: `Worker.RunTend func(ctx context.Context, cfg *config.Config, deps loopcmd.Deps, base string) (loopcmd.Summary, error)`
  and `Worker.SweepDelay time.Duration`.

The seam is **`RunTend`, not `RunSweep`**. "Sweep" already means the full reconcile everywhere in
this codebase, and a `RunSweep` field beside `RunIssue` — whose own comment says *"It is
`loopcmd.TickIssue`, never `loopcmd.RunTick`"* — reads as precisely the thing that comment
forbids. For the same reason the helpers are `issuePass` and `tendPass`, not `runIssuePass`
(which would shadow the `RunIssue` field) and not `sweepOne`.

- [ ] **Step 1: Write the failing tests**

Extend `harness` first. Configuration fields go **above** `mu`, recorded output **below** it,
matching the existing split:

```go
	// above mu, with targets/runFn/gh:
	defaultBranch string
	tendPR        bool
	tendFn        func(cfg *config.Config) error

	// below mu, with ran/ranIssues:
	tends []string // guarded by mu
```

In `harness.open`, fold the two new reads into the existing guarded snapshot and put them on the
config:

```go
	openErr, backoff, max := h.openErr, h.backoff, h.max
	branch, tend := h.defaultBranch, h.tendPR
	h.mu.Unlock()
	...
	cfg := &config.Config{
		Name: loopFromPath(path), Repo: "o/r",
		DefaultBranch: branch, TendPR: tend,
	}
```

Add the seam method beside `runIssue`, a locking accessor beside `ranLoops`, and wire
`w.RunTend = h.runTend` in `newHarness`:

```go
func (h *harness) runTend(
	_ context.Context, cfg *config.Config, _ loopcmd.Deps, base string,
) (loopcmd.Summary, error) {
	h.mu.Lock()
	h.tends = append(h.tends, cfg.Name+"@"+base)
	fn := h.tendFn
	h.mu.Unlock()
	if fn != nil {
		return loopcmd.Summary{}, fn(cfg)
	}
	return loopcmd.Summary{}, nil
}

// tendedLoops returns "loop@base" for each sweep, in order, like ranLoops does
// for issue passes.
func (h *harness) tendedLoops() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.tends...)
}
```

Then the tests. **Every one sets `h.targets`** — `newHarness` leaves it nil, and `Deliver`
returns at `len(targets) == 0` before reaching `tickOne`, which would make these pass while
proving nothing.

```go
// The sweep is armed for exactly one case, and the issue pass always runs.
func TestDeliverArmsATendSweepOnlyOnAMergeIntoTheLoopsDefaultBranch(t *testing.T) {
	cases := []struct {
		name     string
		delivery Delivery
		tendPR   bool
		wantArm  bool
	}{
		{"a merge into the default branch", Delivery{Repo: "o/r", Number: 7, MergedInto: "master"}, true, true},
		{"a merge into a feature branch", Delivery{Repo: "o/r", Number: 7, MergedInto: "feature"}, true, false},
		{"not a merge", Delivery{Repo: "o/r", Number: 7}, true, false},
		{"a merge, but the loop does not tend", Delivery{Repo: "o/r", Number: 7, MergedInto: "master"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(nil)
			h.targets = []Target{h.target("planning")}
			h.defaultBranch = "master"
			h.tendPR = tc.tendPR

			h.w.Deliver(context.Background(), tc.delivery)

			// The merged pull request's own pass moves its issue to a terminal
			// state, and runs immediately whatever the sweep does.
			if got := h.ranLoops(); len(got) != 1 {
				t.Errorf("issue passes = %d, want 1", len(got))
			}
			want := 0
			if tc.wantArm {
				want = 1
			}
			if got := h.timers.len(); got != want {
				t.Fatalf("armed %d timers, want %d", got, want)
			}
			if want == 1 {
				h.timers.at(t, 0).fire()
				if got := h.tendedLoops(); len(got) != 1 || got[0] != "planning@master" {
					t.Errorf("tends = %v, want [planning@master]", got)
				}
			}
		})
	}
}

// A merge train is one sweep, not one per merge. Each sweep can dispatch up to
// maxTendPerSweep agents, and a tend agent that has already finished no longer
// suppresses anything, so an uncoalesced train multiplies.
func TestDeliverCoalescesAMergeTrainIntoOneSweep(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.defaultBranch = "master"
	h.tendPR = true

	for i := 0; i < 5; i++ {
		h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7 + i, MergedInto: "master"})
	}

	if got := h.timers.len(); got != 1 {
		t.Fatalf("armed %d timers, want 1: a train must ride the first timer", got)
	}
	h.timers.at(t, 0).fire()
	if got := h.tendedLoops(); len(got) != 1 {
		t.Errorf("tends = %v, want exactly one", got)
	}
	// Every merge still got its own issue pass.
	if got := h.ranLoops(); len(got) != 5 {
		t.Errorf("issue passes = %d, want 5", len(got))
	}
}

// A failing issue pass schedules its retry as before, and the sweep is still
// armed: the base branch moved whatever happened to that one issue.
func TestDeliverArmsATendSweepEvenWhenTheIssuePassFails(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.defaultBranch = "master"
	h.tendPR = true
	h.max = 1
	h.backoff = []time.Duration{0}
	h.runFn = func(*config.Config) error { return errors.New("boom") }

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7, MergedInto: "master"})

	// One retry timer and one sweep timer.
	if got := h.timers.len(); got != 2 {
		t.Fatalf("armed %d timers, want 2 (one retry, one sweep)", got)
	}
}

// A failed sweep is logged and dropped. It must not schedule a retry: the
// retry path re-runs the ISSUE pass, and re-running it for a sweep failure
// would spend that issue's retry budget on something the issue did not do.
func TestDeliverDoesNotRetryAFailedTendSweep(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.defaultBranch = "master"
	h.tendPR = true
	h.max = 3
	h.backoff = []time.Duration{0, 0, 0}
	h.tendFn = func(*config.Config) error { return errors.New("sweep failed") }

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7, MergedInto: "master"})
	if got := h.timers.len(); got != 1 {
		t.Fatalf("armed %d timers, want 1 (the sweep)", got)
	}
	h.timers.at(t, 0).fire()

	// The sweep failed. No SECOND timer may exist.
	if got := h.timers.len(); got != 1 {
		t.Errorf("armed %d timers after a failed sweep, want 1: a sweep must not schedule a retry", got)
	}
	if got := h.pendingLen(); got != 0 {
		t.Errorf("pending retries = %d, want 0", got)
	}
}
```

Read `timers.at`'s returned `armed` type before writing `.fire()` and use whatever method it
exposes to run the callback; if it has none, call `a.f()` directly as the existing retry tests do.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/listener/ -run TestDeliverArms -v`
Expected: FAIL — `h.w.RunTend undefined`.

- [ ] **Step 3: Add the seam, the delay, and the timer map**

In the `Worker` struct, beside `RunIssue`:

```go
	// RunTend rebases the stale pull requests of one loop, taking the loop's
	// lock itself. Production wires it to loopcmd.TendSweep. base is the branch
	// the merge landed on.
	//
	// It runs for ONE delivery -- a pull request merged into the loop's default
	// branch -- because that is the only event that makes many pull requests
	// stale at once and names none of them. Every other delivery gets RunIssue
	// and nothing more; see RunIssue for the reconcile that was removed and
	// must not come back.
	RunTend func(ctx context.Context, cfg *config.Config, deps loopcmd.Deps, base string) (loopcmd.Summary, error)
```

Beside the other delays:

```go
	SweepDelay      time.Duration // default 1m
```

with the constant beside the others:

```go
	// defaultSweepDelay is how long a merge waits before its tend sweep runs,
	// so a merge train produces one sweep rather than one per merge. The loop
	// lock only collapses sweeps that OVERLAP; merges a minute apart do not,
	// and a tend agent that has already finished suppresses nothing, so an
	// uncoalesced train multiplies by the number of merges in it.
	defaultSweepDelay = time.Minute
```

Beside `pending` and `orphans`:

```go
	// sweeps holds the armed tend timer of each loop, guarded by mu. A merge
	// arriving while one is armed rides it rather than arming a second.
	sweeps map[loopKey]*time.Timer // guarded by mu
```

Initialise it in `NewWorker` alongside `pending` and `orphans`, and set
`RunTend: loopcmd.TendSweep` and `SweepDelay: defaultSweepDelay`.

- [ ] **Step 4: Split `tickOne` and arm the sweep**

The issue pass keeps every existing branch **and every existing comment verbatim** — including
the `GH: acc.gh` rationale, the `MigrationPolicy` rationale, and the `defer cleanup()` rationale.
Do not drop them; the plan's own Global Constraints forbid it.

```go
func (w *Worker) tickOne(ctx context.Context, t Target, d Delivery, acc *access) {
	key := loopKey{ProjectID: t.ProjectID, LoopName: t.LoopName}

	cfg, deps, cleanup, err := w.Open(t.Ref(), t.ConfigPath, loopcmd.Options{
		Token: acc.token,
		// ... existing comments preserved verbatim ...
		GH:              acc.gh,
		RequireGitHub:   true,
		MigrationPolicy: loopcmd.FailOnUnimported,
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		// No cfg here, so the loop's backoff list is unknown -- and no sweep can
		// be armed either, because arming needs default_branch and tend_pr from
		// the file that could not be read.
		slog.Error("cannot open loop", "loop", t.LoopName, "project", t.ProjectName,
			"config", t.ConfigPath, "err", err)
		w.schedule(ctx, t, d.Number, kindOpen, openRetryMax, func(int) time.Duration { return w.OpenRetryDelay })
		return
	}

	w.issuePass(ctx, t, d, cfg, deps, key)

	// Armed on BOTH paths of the issue pass. The merged pull request's own pass
	// moves its issue to a terminal state; whether that succeeded says nothing
	// about the other branches, which are behind because the base moved.
	if cfg.TendPR && d.IsMergeInto(cfg.DefaultBranch) {
		w.armTend(ctx, t, cfg.DefaultBranch)
	}
}
```

`issuePass` is the current body of `tickOne` from `w.RunIssue(...)` onward, with `number`
replaced by `d.Number` and every comment kept.

```go
// armTend schedules the tend sweep a merge calls for, unless one is already
// armed for this loop.
//
// The wait exists because a merge train is normal: several pull requests merge
// within a few minutes, and each merge leaves every other branch further
// behind. Sweeping per merge would dispatch up to maxTendPerSweep agents each
// time, and the loop lock does not prevent it -- the lock only collapses
// sweeps that OVERLAP, and a sweep whose agents have finished suppresses
// nothing. Riding the armed timer rather than resetting it bounds the wait, so
// a long train still gets a sweep every SweepDelay rather than none until it
// stops.
func (w *Worker) armTend(ctx context.Context, t Target, base string) {
	key := loopKey{ProjectID: t.ProjectID, LoopName: t.LoopName}

	w.mu.Lock()
	if _, armed := w.sweeps[key]; armed {
		w.mu.Unlock()
		slog.Info("a tend sweep is already armed for this loop; riding it",
			"loop", t.LoopName, "project", t.ProjectName, "base", base)
		return
	}
	// Registered before the timer is built, so a second merge arriving between
	// these two statements rides this one instead of arming its own.
	w.sweeps[key] = nil
	w.mu.Unlock()

	timer := w.After(w.SweepDelay, func() {
		w.mu.Lock()
		delete(w.sweeps, key)
		w.mu.Unlock()
		// Same rule schedule states: a cancelled context here means the daemon
		// is shutting down, and a daemon told to stop starts no new agent.
		if ctx.Err() != nil {
			return
		}
		w.tendFresh(ctx, t, base)
	})

	w.mu.Lock()
	// Only if the entry is still ours: the timer may have fired and deleted it
	// already, and storing it then would leave an entry no one removes.
	if _, ok := w.sweeps[key]; ok {
		w.sweeps[key] = timer
	}
	w.mu.Unlock()

	slog.Info("armed a tend sweep", "loop", t.LoopName, "project", t.ProjectName,
		"base", base, "in", w.SweepDelay)
}

// tendFresh reads its own token and opens its own loop, like tickFresh: the
// access of the delivery that armed the timer is gone with Deliver's frame,
// and reusing one would decide from labels read a minute ago.
func (w *Worker) tendFresh(ctx context.Context, t Target, base string) {
	acc, err := w.access()
	if err != nil {
		slog.Error("cannot read the github token for a tend sweep",
			"loop", t.LoopName, "project", t.ProjectName, "err", err)
		return
	}
	cfg, deps, cleanup, err := w.Open(t.Ref(), t.ConfigPath, loopcmd.Options{
		Token: acc.token, GH: acc.gh, RequireGitHub: true,
		MigrationPolicy: loopcmd.FailOnUnimported,
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		slog.Error("cannot open loop for a tend sweep", "loop", t.LoopName,
			"project", t.ProjectName, "config", t.ConfigPath, "err", err)
		return
	}
	w.tendPass(ctx, t, cfg, deps, base)
}

// tendPass runs the sweep and decides what its outcome means.
//
// Nothing. A failure is logged and dropped, and schedules NO retry: the retry
// path re-runs the ISSUE pass, so a sweep failure would spend an issue's retry
// budget on something that issue did not do, and the work is not lost because
// the next merge arms another sweep.
func (w *Worker) tendPass(
	ctx context.Context, t Target, cfg *config.Config, deps loopcmd.Deps, base string,
) {
	if _, err := w.RunTend(ctx, cfg, deps, base); err != nil {
		if errors.Is(err, lock.ErrHeld) {
			slog.Info("skipping tend sweep: another tick holds the loop lock",
				"loop", cfg.Name, "project", t.ProjectName)
			return
		}
		slog.Error("tend sweep failed", "loop", cfg.Name, "project", t.ProjectName, "err", err)
	}
}
```

- [ ] **Step 5: Stop armed sweeps on shutdown**

`stopAll` stops every pending **retry** timer so no already-armed timer fires. A sweep timer it
does not know about would fire after the daemon was told to stop and dispatch up to
`maxTendPerSweep` agents. Extend it, and extend its comment to say it now covers both:

```go
	for key, timer := range w.sweeps {
		if timer != nil {
			timer.Stop()
		}
		delete(w.sweeps, key)
	}
```

`w.After` is wrapped in production by `cmd/agent-utils/listener.go`'s `instrumentRetries`, whose
`shuttingDown` gate covers a timer armed after `stopAll` ran. Confirm by reading it that the gate
is on `After` itself and so applies to sweep timers too; if it inspects the retry map instead,
extend it.

- [ ] **Step 6: Run the tests and confirm they pass**

Run: `go test ./internal/listener/ -run TestDeliver -v`
Expected: PASS.

- [ ] **Step 7: Mutation-check the gate and the coalescing**

1. Change `if cfg.TendPR && d.IsMergeInto(cfg.DefaultBranch)` to `if cfg.TendPR`. Run
   `-run TestDeliverArmsATendSweepOnly`. MUST fail on "not a merge" and "a merge into a feature
   branch". Restore.
2. Delete the `if _, armed := w.sweeps[key]; armed` early return. Run
   `-run CoalescesAMergeTrain`. MUST fail with `armed 5 timers, want 1`. Restore.

Report both.

- [ ] **Step 8: Run the gates**

Run: `make check && make test/race`
Expected: all green. The race detector matters here: `Deliver` runs from the handler's pool
goroutines and this task adds a map mutated from timer callbacks.

- [ ] **Step 9: Commit**

```bash
git add internal/listener
git commit -m "$(cat <<'EOF'
feat(listener): sweep stale pull requests when a merge lands on the default branch

A pull_request delivery that merged into the loop's default_branch arms a
tend sweep for loops with tend_pr: true. Every other delivery is
unchanged, and the merged pull request's own issue pass still runs at
once on both paths -- the base branch moved whatever happened to it.

The sweep waits SweepDelay and a second merge rides the armed timer, so a
merge train produces one sweep. The loop lock does not do this on its
own: it only collapses sweeps that overlap, and a tend agent that has
already finished suppresses nothing.

stopAll now stops armed sweep timers too, or a daemon told to stop would
still dispatch a batch of rebase agents a minute later.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

**Acceptance criteria:** the four `TestDeliver` tests pass; both mutants in Step 7 fail their
test and are reported; `make check` and `make test/race` green.

**review: yes** — this is the gate that decides when repository-wide dispatch happens, plus new
timer state and a shutdown path.

---

### Task 4: Document when a sweep runs

Both documents currently make statements this change falsifies. `README.md` has direct
precedent: commit `86a6e80 docs(readme): a delivery acts on the issue it names` exists only to
have corrected this same passage the last time delivery scope changed.

**Files:**
- Modify: `docs/configuration.md` (the `tend_pr` section)
- Modify: `README.md` (the webhook section; the cron section)

- [ ] **Step 1: Correct `docs/configuration.md`**

The `tend_pr` section opens "When true, **on every tick**, for each issue carrying
`labels.review`". That is now only half the story — fix that sentence, do not leave it standing.

Use **no new heading**. The reference half of this file uses `###` exclusively for a backticked
field name (`### name`, `### agent.harness — optional`), and `## tend_pr` structures itself with
bold lead-ins and lists. A `### When a sweep runs` would be the first non-field `###` in that
half and would read like a config key. Place this after the `Three safeguards apply:` list and
before `**Set this false for a planning loop.**`:

```markdown
**Two things dispatch a tend agent.** A delivery for one issue — the issue carries
`labels.review`, its linked pull request is behind its base, and the delivery named that issue.
And a merge into `default_branch`: GitHub sends a `pull_request` delivery with `merged: true`,
and because the merge is what made every other pull request stale while naming none of them,
that one delivery sweeps the loop. Every issue carrying `labels.review` whose linked pull
request targets `default_branch` and is now behind gets a tend dispatch.

A pull request targeting any other branch is left alone. A merge into `master` says nothing
about a branch based on `release/1.0`.

The sweep dispatches **tend agents only**. It never starts, resumes, or retries an issue agent,
and it never parks an issue. A merge is a reason to rebase; it is not a reason to start work.

A sweep waits about a minute before it runs, so a merge train produces one sweep rather than one
per merge, and it dispatches at most ten rebases. If more pull requests are behind than that,
the rest are named in the log and wait for the next merge.

**A sweep does not replace a periodic tick.** `agent-utils project loop tick` is still the only
full reconcile: it is what retires a dead runner for an issue no delivery names, and what finds
a pull request that fell behind for any reason other than a merge. Schedule it.
```

- [ ] **Step 2: Correct `README.md`**

Two passages. In the webhook section, "A delivery acts on the issue it names, and on nothing
else" is now false — amend to name the one exception:

```markdown
A delivery acts on the issue it names. The one exception is a pull request merged into a loop's
`default_branch`: that merge is what makes every other open pull request stale while naming none
of them, so it also sweeps the loop for rebases — tend agents only, capped, and only for loops
with `tend_pr: true`.
```

In the cron section, the safety-net example "a pull request that fell behind because someone
pushed to the default branch (a `push` event, which this daemon does not subscribe to)" is now
only half true: a merge through a pull request emits **both** a `push` and a `pull_request`
event, and the daemon now catches the second. Narrow it to the case that is still missed —
insert "**directly**" so it reads "someone pushed to the default branch directly".

- [ ] **Step 3: Run the gates**

Run: `make check`
Expected: green.

- [ ] **Step 4: Commit**

```bash
git add docs/configuration.md README.md
git commit -m "$(cat <<'EOF'
docs(config): record the merge-triggered tend sweep

The tend_pr section said a tend dispatch happens "on every tick, for each
issue carrying labels.review". That is still true of loop tick, but a
merge into default_branch now sweeps the loop from a single delivery, and
nothing said so.

The README said a delivery acts on the issue it names "and on nothing
else", which this change makes false, and offered "a pull request that
fell behind because someone pushed to the default branch" as work only
cron catches -- now true only of a DIRECT push, since a merge through a
pull request emits an event the daemon does subscribe to.

Both now say plainly what the sweep does not replace: no periodic tick
runs unless an operator schedules one.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

**Acceptance criteria:** `make check` green; neither document still claims a delivery acts only
on the issue it names; both state that a sweep does not replace the periodic tick.

**review: no** — prose only; the whole-diff fan-out still covers it.

---

---

### Task 5: Remove a closed pull request's worktrees

Added after the gate, at the operator's request. A worktree of this repository's
own monorepo is ~866MB (`node_modules`), and nothing has ever removed one: a
merged issue's checkout sits on disk forever.

**Decision, made by the operator with the risk stated:** on ANY close — merged or not — remove
**both** the `pr-<N>` and the `issue-<M>` worktree. The alternative considered was removing only
`pr-<N>` on an unmerged close, on the grounds that such a close often means the work continues
and the agent will push a replacement. That was declined in favour of reclaiming the disk. The
live-dispatch guard below is what keeps this from touching work in progress; what it does not
protect is uncommitted, unpushed work in an *idle* worktree, which is the accepted risk.

**Files:**
- Modify: `internal/listener/work.go` (`Delivery.ClosedPR`, the `RunCleanup` seam, `cleanupPass`)
- Modify: `internal/listener/handler.go` (set `ClosedPR`)
- Create: `internal/loopcmd/cleanup.go`
- Modify: `internal/loopcmd/tick.go` (extract the liveness predicate `reapDead` already uses)
- Modify: `internal/worktree/worktree.go` (`Dirty`)
- Test: `internal/loopcmd/cleanup_test.go`, `internal/listener/work_test.go`

**Interfaces:**
- Produces: `func CleanupClosedPR(ctx context.Context, cfg *config.Config, deps Deps, prNumber int) error`
- Produces: `func (m *Manager) Dirty(path string) (bool, error)`
- Produces: `Worker.RunCleanup func(ctx, cfg, deps, prNumber int) error`

**Three rules this task exists to get right:**

1. **The liveness check must use `reapDead`'s rule, not `Reset`'s.** `Reset` calls `isAlive`
   directly. `reapDead` additionally treats a row younger than `pidGracePeriod` (90s) carrying a
   non-positive pid as **live**, because the tick writes the pid just after the spawn. Cleanup
   runs on the delivery path where a dispatch can be seconds old, so `Reset`'s rule would delete a
   worktree out from under an agent that had just started. Extract the predicate from `reapDead`
   so both share one definition rather than copying it.
2. **A live dispatch cancels the WHOLE cleanup**, not just one path. If either the issue or the
   pull request has a live row, remove neither: the two checkouts belong to one piece of work.
3. **Removing is destructive and must leave a trace.** Log a warning naming the worktree if it
   had uncommitted changes when removed. This does not block the removal — the operator chose
   that — it makes the loss visible afterwards.

**Resolving the issue costs no API call.** `DeliveryCache.PullRequest` is memoised per delivery,
and the issue pass already fetched this pull request through `subject`. `engine.ClosesIssue`
turns it into the issue number.

- [ ] **Step 1: `worktree.Dirty`** — `git -C <path> status --porcelain`; non-empty output means
  dirty. A path that does not exist is not dirty and is not an error.
- [ ] **Step 2: Extract the liveness predicate** in `internal/loopcmd/tick.go` and make `reapDead`
  call it. Behavior must not change; the existing `reapDead` tests are the proof.
- [ ] **Step 3: Write the failing tests** in `internal/loopcmd/cleanup_test.go`: both worktrees
  removed on a merged close; both removed on an unmerged close; NEITHER removed when the issue has
  a live dispatch; neither removed when a row is younger than `pidGracePeriod` with pid 0; the
  `pr-` worktree still removed when the pull request closes no issue; a dirty worktree is removed
  AND warned about.
- [ ] **Step 4: Write `internal/loopcmd/cleanup.go`.**
- [ ] **Step 5: Wire it** — `Delivery.ClosedPR`, set in the handler beside `mergedInto`; the
  `RunCleanup` seam; `cleanupPass` called from `tickOne` after `issuePass`.
- [ ] **Step 6: Mutation-check** the liveness guard (delete it; the live-dispatch test must fail)
  and the grace-period rule (swap it for `Reset`'s bare `isAlive`; the young-row test must fail).
  Confirm each mutation applied before trusting a pass.
- [ ] **Step 7: `make check` and `make test/race` green; commit once.**

**review: yes** — this deletes user data on a webhook.

## Known limits

Carried from the plan review. Each is a real gap, none is a blocker, and all are recorded so a
later reader does not mistake a limit for an oversight.

- **A conflicted pull request is re-tended on every sweep.** Tend rows deliberately write no
  issue state (`reapDead` and `runner.finish` both guard on `store.KindTend`), so a tend agent
  that fails a genuine rebase conflict consumes no retry budget and can never be parked. The only
  suppression is a *live* tend row. `maxTendPerSweep` and the coalescing timer bound how often
  this repeats, but do not stop it. A real fix needs per-pull-request state that survives the
  agent's exit — record the head SHA at dispatch and skip a pull request whose head has not moved
  — which is a schema change and a separate piece of work.
- **A replayed merge delivery arms a real sweep.** `handler.go`'s delivery cache is a 1024-entry
  FIFO and its own comment notes that an id being actively replayed ages out. Before this change
  a replay re-ran one issue's pass, which is largely inert; now it can arm a sweep. The
  coalescing timer bounds the rate to one sweep per `SweepDelay` per loop. Making it idempotent
  on the merge itself — carrying `merge_commit_sha` and skipping a sweep for a SHA already swept
  — again needs stored state.
- **A direct push to the default branch still arms no sweep.** GitHub sends `push`, not
  `pull_request`, and this daemon does not subscribe to `push`. Subscribing would mean adding it
  to `ghub.HookEvents`, re-registering every webhook, and relaxing the handler's rule that every
  delivery names an issue number — a `push` payload names none.
- **No periodic sweep, and no scheduler in the daemon.** `loopcmd.Tick` never runs on the
  operator's machine: there is no cron entry and no launchd job. A pull request that falls behind
  for any reason other than a merge has nothing scheduled to notice it, and a dead non-tend
  runner is never retired unless a delivery arrives for its issue.

## Pipeline State

| Field   | Value                                                                        |
|---------|------------------------------------------------------------------------------|
| stage   | 3 (implementation)                                                           |
| class   | standard (new trigger path, a widened seam, and new timer state; no schema)   |
| profile | backend                                                                      |
| branch  | feat/merge-triggered-tend-sweep                                              |
| pr      | #9                                                                           |
| gate    | approved 2026-08-22                                                          |
| round   | 0                                                                            |
