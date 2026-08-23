# Merge-triggered tend sweep Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a merge into a loop's default branch rebase every open pull request that the
merge left behind.

**Architecture:** The webhook handler decodes two more payload fields and passes a `Delivery`
value instead of a bare issue number. When the delivery is a merge into the loop's
`default_branch` and the loop sets `tend_pr: true`, the worker runs a new `loopcmd.TendSweep`
after the delivery's own issue pass. `TendSweep` builds the same snapshot `Tick` builds, calls
the same `engine.Decide`, and then keeps only `KindTend` decisions. The merged pull request's
own `TickIssue` pass is unchanged.

**Tech Stack:** Go 1.25, `github.com/google/go-github` (webhook payload), SQLite via
`internal/store`, `log/slog`.

**Spec:** `docs/superpowers/specs/2026-08-22-merge-triggered-tend-sweep-design.md`

## Global Constraints

This repository has **no conventions document** — there is no `AGENTS.md`, `CLAUDE.md`,
`CONTRIBUTING.md`, or `STANDARDS.md` at the root. The rules below are read from the code itself.
Recommend a follow-up run of `identify-standards` to record them once, instead of restating them
in every plan.

Binding rules this change can touch:

- **`make check` must pass** — `fmtcheck` + `vet` + `golangci-lint` + the full suite
  (`Makefile:173`). `make test/race` must also pass; the listener is concurrent and CI runs it.
- **Comments state the failure the code prevents.** Every non-obvious branch in
  `internal/listener` and `internal/loopcmd` carries a comment naming the bug it stops. Match
  that density. A comment that only restates the code is not the house style.
- **Seams are struct fields, not package functions.** `loopcmd.Deps` and `listener.Worker`
  declare every collaborator as a field so tests need no registry, database, token, or real
  clock (`work.go:116`). Any new collaborator follows this.
- **`Worker`'s fields are written once**, before the value is shared with the HTTP handler and
  the wake loop. Only `pending` and `orphans` are mutated at run time, and only under `mu`
  (`work.go:118-122`). Do not add mutable state outside that rule.
- **Attacker-controlled payload text is bounded before it is logged** — see `safeText`,
  `safeLabels`, `safeAction`, `safeDeliveryID` in `handler.go`. The log file is not rotated.
- **Decision policy is single-sourced in `engine.Decide`.** `tickissue.go:138` forbids a scoped
  copy of the retry, veto, and double-dispatch rules. The sweep calls `Decide` and filters its
  output; it must not re-implement any rule.
- **Commit messages** follow `type: subject` (`feat:`, `fix:`, `docs:`, `chore:`) and end with
  the `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>` trailer.

## Verified external API (do not re-derive)

Read from source in this repository on 2026-08-22.

```go
// internal/loopcmd/tick.go:23
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

// internal/loopcmd/tick.go:63
type Summary struct {
    Started, Resumed, Retried, Tended, Parked, Live, Orphans int
    BreakerTripped                                           bool
}

// internal/loopcmd/tick.go:204 — retires dead rows; MarkNeedsRetry is guarded by
// `if d.Kind != store.KindTend` at tick.go:241, so retiring a tend row writes no issue state.
func reapDead(cfg *config.Config, deps Deps, running []store.Dispatch,
    states map[int]store.IssueState, now time.Time, sum *Summary) ([]store.Dispatch, error)

// internal/loopcmd/tick.go:313 — KindTend dispatches store.KindTend and counts sum.Tended.
func act(ctx context.Context, cfg *config.Config, deps Deps, d engine.Decision,
    now time.Time, sum *Summary) error

// internal/ghub/ghub.go:15
ListOpenIssues(ctx context.Context, owner, repo string) ([]Issue, error)
ListOpenPullRequests(ctx context.Context, owner, repo string) ([]PullRequest, error)
BehindBy(ctx context.Context, owner, repo, base, head string) (int, error)

// internal/engine — Decide is pure. LinkPR links an issue to its trusted pull request.
func Decide(cfg *config.Config, snap Snapshot, st State, now time.Time) Plan
func LinkPR(issueNumber int, prs []ghub.PullRequest) (ghub.PullRequest, bool)
type Snapshot struct { Issues []ghub.Issue; PRs []ghub.PullRequest; BehindBy map[int]int }
type State struct { Issues map[int]store.IssueState; Running []store.Dispatch; CooldownUntil time.Time }
const KindTend Kind = "tend"   // internal/engine/types.go:25

// internal/lock/lock.go:23 — LOCK_EX|LOCK_NB. Returns ErrHeld at once; never waits.
func Acquire(path string) (*Lock, error)

// internal/listener/listener.go:72 — the seam this plan widens.
Tick func(ctx context.Context, repo string, number int)
```

GitHub `pull_request` payload: a merge arrives as `action: "closed"` with
`pull_request.merged: true` and `pull_request.base.ref` naming the branch merged into.

---

## File Structure

| File | Responsibility | Change |
|---|---|---|
| `internal/listener/delivery.go` | The `Delivery` value the handler hands the worker | Create |
| `internal/listener/handler.go` | Decode `merged` + `base.ref`; build a `Delivery` | Modify |
| `internal/listener/listener.go` | Widen the `Tick` seam to take a `Delivery` | Modify |
| `internal/listener/work.go` | Run the sweep after the issue pass, per loop | Modify |
| `internal/loopcmd/tendsweep.go` | The tend-only sweep | Create |
| `cmd/agent-utils/listener.go` | Wire the widened seam | Modify |
| `docs/configuration.md` | Record when a sweep runs | Modify |

---

### Task 1: The `Delivery` value and the widened seam

Carry the merge facts from the handler to the worker. The handler cannot decide whether a base
ref matters — each loop has its own `default_branch`, and one repository can have several loops
— so it reports the fact and the worker judges it.

**Files:**
- Create: `internal/listener/delivery.go`
- Modify: `internal/listener/handler.go` (payload struct near line 436; the `s.Tick` call near
  line 604)
- Modify: `internal/listener/listener.go:72` (the `Tick` field)
- Modify: `cmd/agent-utils/listener.go:391` (`wrapTick`)
- Modify: `internal/listener/work.go` (`Deliver`, `tickOne`, `tickFresh`, `schedule` signatures)
- Test: `internal/listener/delivery_test.go` (create),
  `internal/listener/handler_test.go` (14 stub sites listed below)

**Interfaces:**
- Produces: `listener.Delivery{Repo string; Number int; MergedInto string}` and
  `func (d Delivery) IsMergeInto(branch string) bool`. Task 3 consumes both.
- Produces: `Server.Tick func(ctx context.Context, d Delivery)`.

- [ ] **Step 1: Write the failing test**

Create `internal/listener/delivery_test.go`:

```go
package listener

import "testing"

// MergedInto is the ONE field that says "the default branch moved." An empty
// value must never match a branch name, or every ordinary delivery -- an
// opened issue, a moved label -- would start a repository-wide sweep. That is
// the regression work.go:140 records.
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

- [ ] **Step 3: Write `internal/listener/delivery.go`**

```go
package listener

// Delivery is what one accepted webhook delivery tells the worker.
//
// It replaced a bare (repo, number) pair because a merged pull request must
// start more work than an ordinary delivery, and the handler cannot judge
// that on its own: the decision needs a loop's default_branch, and one
// repository may be watched by several loops with different ones.
type Delivery struct {
	// Repo is the "owner/name" the delivery named.
	Repo string
	// Number is the issue or pull request the delivery named. Every accepted
	// delivery carries one; handler.go rejects a delivery without it.
	Number int
	// MergedInto is the base branch of a pull request this delivery reports as
	// MERGED, and is empty for every other delivery. Empty is the only safe
	// default: it is what keeps an opened issue or a moved label from starting
	// a repository-wide sweep. See work.go's Deliver for the regression that
	// makes this the important property of this type.
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

In `internal/listener/listener.go`, change the field at line 72 and keep its comment, adding the
merge sentence:

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

In `internal/listener/handler.go`, inside the anonymous `body` struct, replace the
`PullRequest` member with:

```go
			// Merged and Base carry the one fact that starts a tend sweep: this
			// delivery merged a pull request, and into which branch. Both are
			// decoded here and judged per loop in work.go, because default_branch
			// is loop configuration the handler does not hold.
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

- [ ] **Step 7: Build the `Delivery` and log the base ref**

In `handler.go`, immediately before the `attrs := []any{...}` line, add:

```go
			// A merged pull request is the only delivery that starts more than
			// an issue pass. The action is checked as well as the flag: GitHub
			// sends merged: true on later pull_request actions too, and only
			// the close is the moment the base branch moved.
			var mergedInto string
			if event == "pull_request" && body.Action == "closed" && body.PullRequest.Merged {
				mergedInto = body.PullRequest.Base.Ref
			}
```

Add the field to the log line, only when present — an empty one would say nothing while looking
like it said something, which is the rule the label and title fields already follow:

```go
			if mergedInto != "" {
				attrs = append(attrs, "merged_into", safeText(mergedInto))
			}
```

Then change the pool goroutine's call:

```go
				s.Tick(ctx, Delivery{Repo: repo, Number: number, MergedInto: mergedInto})
```

- [ ] **Step 8: Thread `Delivery` through the worker**

In `internal/listener/work.go`, change these signatures. Do not change any behavior in this
step — Task 3 adds the sweep.

```go
func (w *Worker) Deliver(ctx context.Context, d Delivery)      // was (ctx, repo string, number int)
func (w *Worker) tickOne(ctx context.Context, t Target, d Delivery, acc *access)
```

Inside `Deliver`, replace `repo` with `d.Repo` and `number` with `d.Number`.

The retry path keeps carrying a number, not a `Delivery`, and `tickFresh` rebuilds a plain one:

```go
	// A retry re-runs the ISSUE pass only. A retry may fire minutes after the
	// merge that caused it, and a sweep then is not what that merge asked for:
	// the base has moved again or has not, and the next merge sweeps either
	// way. MergedInto is therefore left empty here on purpose.
	w.tickOne(ctx, t, Delivery{Repo: t.Repo, Number: number}, acc)
```

If `Target` carries no `Repo` field, pass the repository the retry was scheduled for through
`schedule` alongside `number`, and keep the same comment.

- [ ] **Step 9: Update `wrapTick` and every test stub**

In `cmd/agent-utils/listener.go:391`, change `wrapTick` so its inner function takes a
`listener.Delivery` and calls `w.Deliver(ctx, d)`.

Update all 14 stub sites. They are at `internal/listener/listener_test.go` lines 33, 51, 67, 99
and `internal/listener/handler_test.go` lines 93, 318, 334, 357, 636, 690, 750, 818, 871. A stub
that ignores its argument becomes `func(context.Context, Delivery) {}`; a stub that reads
`repo`/`number` reads `d.Repo`/`d.Number`.

- [ ] **Step 10: Add a handler test proving the merge facts reach the seam**

First widen the existing `tickCall` fixture at `internal/listener/handler_test.go:80`, and make
`newServer`'s fake `Tick` (line 88) record the new field:

```go
type tickCall struct {
	repo       string
	number     int
	mergedInto string
}
```

Then append the test. It uses the file's existing helpers — `newServer`, `doRequest`, `waitTick`
— and its existing signing path. Do not write a second one.

```go
// The handler must report a merge, and must NOT report anything else as one.
// MergedInto is what starts a repository-wide sweep, so a false positive here
// is the regression work.go:140 records, reintroduced at the front door.
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
			// GitHub sends merged: true on later pull_request actions too. Only
			// the close is the moment the base branch moved.
			name:    "an edited pull request that was already merged",
			event:   "pull_request",
			payload: `{"action":"edited","repository":{"full_name":"o/r"},"pull_request":{"number":7,"merged":true,"base":{"ref":"master"}}}`,
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
			srv := httptest.NewServer(s.Handler())
			t.Cleanup(srv.Close)

			body := []byte(tc.payload)
			resp := doRequest(t, srv.URL+"/webhook", body, map[string]string{
				"X-GitHub-Event": tc.event,
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

`doRequest` already signs the body and sets the delivery id; read its definition at line 131 and
match how the nearest existing `pull_request` test (line 357) sets `X-GitHub-Event` and the
server URL. Follow that test's exact shape rather than the sketch above where the two differ.

- [ ] **Step 11: Run the gates**

Run: `make check && make test/race`
Expected: all green.

- [ ] **Step 12: Commit**

```bash
git add internal/listener cmd/agent-utils/listener.go
git commit -m "$(cat <<'EOF'
feat: carry merge facts from the webhook delivery to the worker

The handler reports whether a delivery merged a pull request and into
which branch. The comparison against a loop's default_branch happens in
the worker, because one repository may be watched by several loops with
different ones.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

**Acceptance criteria:** `TestIsMergeIntoRequiresAMergedBaseRef` and
`TestHandlerReportsOnlyAMergedPullRequestAsAMerge` pass. `make check` and `make test/race` are
green. No behavior changed yet: no sweep runs.

**review: yes** — this parses attacker-controlled payload fields and defines the flag that gates
repository-wide dispatch.

---

### Task 2: `loopcmd.TendSweep`

The tend-only sweep. It reuses `engine.Decide` and filters the result; it re-implements no rule.

**Files:**
- Create: `internal/loopcmd/tendsweep.go`
- Test: `internal/loopcmd/tendsweep_test.go` (create)

**Interfaces:**
- Consumes: `loopcmd.Deps`, `config.Config` (both already exist).
- Produces: `func TendSweep(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error)`.
  Task 3 consumes this exact signature.

- [ ] **Step 1: Write the failing test**

This package's fakes already exist. Use `tickConfig(t)` (`tick_test.go:88`), `newDeps(t, cfg, gh,
&spawned)` (`tick_test.go:127`) and `fakeGH` (`tick_test.go:17`). Do not add a second set.

`fakeGH.BehindBy` (`tick_test.go:70`) cannot fail today. Add one field and one branch to it, so
the "one bad pull request must not stop the sweep" case is reachable:

```go
type fakeGH struct {
	// ... existing fields ...

	// behindErr makes BehindBy fail for one pull request. A comparison CAN
	// fail in production -- a force-pushed head, a deleted branch -- and a
	// sweep must survive it, so a fake that cannot fail leaves the branch
	// that survives it untested.
	behindErr map[int]error
}

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

Then create `internal/loopcmd/tendsweep_test.go`:

```go
package loopcmd

import (
	"context"
	"errors"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// sweepConfig is tickConfig with tending on. tickConfig leaves TendPR false,
// and a sweep on a loop that does not tend must produce nothing.
func sweepConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := tickConfig(t)
	cfg.TendPR = true
	cfg.TendPrompt = "rebase #{{.Issue.Number}}"
	cfg.DefaultBranch = "master"
	return cfg
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
		prs: []ghub.PullRequest{
			{Number: 11, Body: "Closes #1", HeadRef: "issue-1", BaseRef: "master", Trusted: true},
		},
		behind: map[int]int{11: 3},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TendSweep(context.Background(), cfg, deps)
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

// A pull request level with its base produces nothing. Silence is correct.
func TestTendSweepIgnoresAnUpToDatePullRequest(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{{Number: 11, Body: "Closes #1", HeadRef: "issue-1", BaseRef: "master", Trusted: true}},
		behind: map[int]int{11: 0},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TendSweep(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("TendSweep: %v", err)
	}
	if sum.Tended != 0 {
		t.Errorf("Tended = %d, want 0", sum.Tended)
	}
}

// A loop that does not tend produces nothing, whoever calls. The caller checks
// this too; TendSweep is exported, so it checks for itself.
func TestTendSweepDoesNothingWhenTendPRIsOff(t *testing.T) {
	cfg := sweepConfig(t)
	cfg.TendPR = false
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{{Number: 11, Body: "Closes #1", HeadRef: "issue-1", BaseRef: "master", Trusted: true}},
		behind: map[int]int{11: 3},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TendSweep(context.Background(), cfg, deps)
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

// A dead TEND row is retired, or its pull request is never tended again. A
// dead row of any OTHER kind is left alone: retiring it would flag an issue
// this pass never examined for retry, the hazard tickissue.go:112 describes.
func TestTendSweepRetiresDeadTendRowsOnly(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{
			{Number: 1, Labels: []string{"review"}},
			{Number: 2, Labels: []string{"in-flight"}},
		},
		prs:    []ghub.PullRequest{{Number: 11, Body: "Closes #1", HeadRef: "issue-1", BaseRef: "master", Trusted: true}},
		behind: map[int]int{11: 3},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.IsAlive = func(int, int64) bool { return false }

	// Both rows are dead. Use liveDispatch (tickreason_test.go:31) to insert
	// them with a registered pid, so the pidGracePeriod branch does not keep
	// them alive.
	liveDispatch(t, cfg, deps, store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 1, Kind: store.KindTend, PRNumber: 11, SessionID: "t1",
	})
	liveDispatch(t, cfg, deps, store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: 2, Kind: store.KindStart, SessionID: "s1",
	})

	if _, err := TendSweep(context.Background(), cfg, deps); err != nil {
		t.Fatalf("TendSweep: %v", err)
	}

	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range running {
		if d.Kind == store.KindTend {
			t.Error("the dead tend row was not retired")
		}
	}
	var sawStart bool
	for _, d := range running {
		if d.Kind == store.KindStart {
			sawStart = true
		}
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

// A stale checkout cannot answer "how far behind is this branch", which is the
// whole pass. It stops rather than deciding from a stale comparison.
func TestTendSweepStopsWhenTheFetchFails(t *testing.T) {
	cfg := sweepConfig(t)
	gh := &fakeGH{
		issues: []ghub.Issue{{Number: 1, Labels: []string{"review"}}},
		prs:    []ghub.PullRequest{{Number: 11, Body: "Closes #1", HeadRef: "issue-1", BaseRef: "master", Trusted: true}},
		behind: map[int]int{11: 3},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.Fetch = func() error { return errors.New("network down") }

	sum, err := TendSweep(context.Background(), cfg, deps)
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
			{Number: 11, Body: "Closes #1", HeadRef: "issue-1", BaseRef: "master", Trusted: true},
			{Number: 12, Body: "Closes #2", HeadRef: "issue-2", BaseRef: "master", Trusted: true},
		},
		behind:    map[int]int{11: 3, 12: 3},
		behindErr: map[int]error{11: errors.New("no common ancestor")},
	}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)

	sum, err := TendSweep(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("TendSweep: %v", err)
	}
	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1: the second pull request must still be tended", sum.Tended)
	}
}
```

`Trusted: true` is load-bearing. `engine.LinkPR` (`internal/engine/prlink.go:26`) skips any pull
request with `!pr.Trusted`, so a fixture that omits it links nothing and every assertion reads
zero for the wrong reason — a test that passes while proving nothing. `Body` must carry a real
closing reference (`Closes #N`); `closingRef` at `prlink.go:13` is what matches it.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/loopcmd/ -run TendSweep -v`
Expected: FAIL — `undefined: TendSweep`.

- [ ] **Step 3: Write `internal/loopcmd/tendsweep.go`**

```go
package loopcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/lock"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// TendSweep rebases every stale pull request of one loop, and does nothing
// else.
//
// It exists because the default branch moving is the one event that makes many
// pull requests stale at once, and no webhook delivery names them. GitHub sends
// no delivery to their issues; the merged pull request's own delivery names
// only itself, and that is the single pull request tending cannot help.
//
// # Why this is not the reconcile that was removed
//
// work.go:140 records that a full reconcile per delivery was removed: it burned
// a token budget on every open issue of every project watching the repository,
// and one unlabelled test issue dispatched a tend agent for an unrelated issue
// whose pull request was 16 commits behind. This pass acts on many issues
// again, so it must not become that. Three things keep it apart:
//
//  1. It runs for ONE event -- a pull request merged into the loop's default
//     branch. Opening an issue, moving a label and commenting start no sweep.
//  2. It keeps TEND decisions only. Every other kind is dropped below, before
//     anything is dispatched.
//  3. The cause matches the effect. The earlier incident dispatched a rebase
//     because an unrelated issue was opened. Here the base branch moved, so the
//     branches behind it are rebased. That is the correct answer to the event.
//
// Decisions come from engine.Decide, the same function the full tick calls. A
// scoped copy of the veto, live-dispatch and link rules would be a second
// implementation free to drift; see tickissue.go:138.
func TendSweep(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error) {
	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		return Summary{}, err
	}
	defer l.Release()
	return tendSweep(ctx, cfg, deps)
}

func tendSweep(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error) {
	var sum Summary
	now := deps.Now()

	// The caller checks this too. It is checked again here because TendSweep is
	// exported: a loop that does not tend must produce nothing, whoever calls.
	if !cfg.TendPR {
		return sum, nil
	}

	// Unlike Tick, a failed fetch ENDS this pass rather than narrowing it. Tick
	// suppresses tending and still reaps, retries and parks; this pass has
	// nothing but tending to do, and "how far behind is this branch" cannot be
	// answered from a stale checkout.
	if deps.Fetch != nil {
		if err := deps.Fetch(); err != nil {
			return sum, fmt.Errorf("fetch primary checkout: %w", err)
		}
	}

	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	issues, err := deps.GH.ListOpenIssues(ctx, owner, repo)
	if err != nil {
		return sum, err
	}
	prs, err := deps.GH.ListOpenPullRequests(ctx, owner, repo)
	if err != nil {
		return sum, err
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
		}); err != nil {
			slog.Error("store pr link", "loop", cfg.Name, "issue", iss.Number, "err", err)
		}
	}

	states, err := deps.Store.IssueStates(cfg.Name, cfg.Repo)
	if err != nil {
		return sum, err
	}
	running, err := deps.Store.RunningDispatches(cfg.Name, cfg.Repo)
	if err != nil {
		return sum, err
	}

	// Retire dead TEND rows only.
	//
	// Every row is still READ: engine.Decide builds liveIssues from them, and a
	// live start agent must keep suppressing a tend for its issue -- an agent
	// working a branch and a tend agent force-pushing it are the same hazard as
	// two agents. But only tend rows are RETIRED. tickissue.go:112 states why:
	// retiring the loop's rows on a delivery flags issues nobody touched for
	// retry. A tend row cannot do that -- reapDead guards MarkNeedsRetry with
	// `d.Kind != store.KindTend` at tick.go:241 -- so retiring one writes no
	// issue state at all.
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
	liveTend, err := reapDead(cfg, deps, tendRows, states, now, &sum)
	if err != nil {
		return sum, err
	}
	live = append(live, liveTend...)
	sum.Live = len(live)

	st := engine.State{Issues: states, Running: live}
	if st.CooldownUntil, err = deps.Store.CooldownUntil(cfg.Name); err != nil {
		return sum, err
	}

	plan := engine.Decide(cfg, snap, st, now)
	sum.BreakerTripped = plan.BreakerTripped

	// A cooldown already set is OBEYED -- Decide halts on it above -- but this
	// pass never WRITES one. The breaker counts retry decisions within one call
	// (tickissue.go:311), and this pass discards every retry decision. A pass
	// that will not act on that evidence must not stop the passes that would.
	if plan.BreakerTripped {
		slog.Warn("circuit breaker tripped; skipping all dispatch",
			"loop", cfg.Name, "cooldown_until", plan.CooldownUntil)
		return sum, nil
	}

	// clearUnreachableDeadlines is deliberately NOT called, for the reason
	// tickissue.go:146 gives: this pass looked at review issues, so it holds no
	// evidence about any other stamped row. Tick still runs it.

	for _, d := range plan.Decisions {
		// The boundary that bounds the blast radius of a merge. It is the
		// counterpart of tickissue.go:158's per-issue check, and it is what
		// keeps this from being the per-delivery reconcile that was removed. It
		// must not depend on an invariant living in another package.
		if d.Kind != engine.KindTend {
			slog.Info("dropping a non-tend decision in a tend sweep",
				"loop", cfg.Name, "issue", d.Issue, "kind", d.Kind, "reason", d.Reason)
			continue
		}
		if err := act(ctx, cfg, deps, d, now, &sum); err != nil {
			// One failed decision must not abandon the rest of the sweep.
			slog.Error("decision failed", "loop", cfg.Name, "kind", d.Kind,
				"issue", d.Issue, "err", err)
		}
	}

	// Recorded like any other tick, so the counter and the last-tick time keep
	// meaning something in `project loop status`.
	body, _ := json.Marshal(sum)
	if _, err := deps.Store.RecordTick(cfg.Name, plan.BreakerTripped, string(body)); err != nil {
		return sum, err
	}
	slog.Info("tend sweep complete", "loop", cfg.Name, "summary", string(body))
	return sum, nil
}
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/loopcmd/ -run TendSweep -v`
Expected: PASS.

- [ ] **Step 5: Mutation-check the two tests that matter**

The kind filter and the tend-only reaping are the safety properties. Prove their tests bite:

1. Delete the `if d.Kind != engine.KindTend { ... continue }` block. Run
   `go test ./internal/loopcmd/ -run TendSweepDispatchesOnly`. It MUST fail with
   `Started = 1, want 0`. Restore the block.
2. Change the row split so every row goes to `tendRows`. Run
   `go test ./internal/loopcmd/ -run RetiresDeadTendRowsOnly`. It MUST fail. Restore the split.

Report which mutants were run and what each did. A test that passes with its property removed is
worse than no test.

- [ ] **Step 6: Run the gates**

Run: `make check && make test/race`
Expected: all green.

- [ ] **Step 7: Commit**

```bash
git add internal/loopcmd/tendsweep.go internal/loopcmd/tendsweep_test.go
git commit -m "$(cat <<'EOF'
feat: tend-only sweep for one loop

TendSweep builds the snapshot Tick builds, calls the same engine.Decide,
and keeps only tend decisions. It retires dead tend rows and no others,
writes no cooldown, and clears no deadlines, so a pass that answers a
merge cannot widen into the per-delivery reconcile that was removed.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

**Acceptance criteria:** all five `TendSweep` tests pass; both mutants in Step 5 fail their test
and are reported; `make check` and `make test/race` green.

**review: yes** — this is the blast-radius boundary and the concurrency-sensitive half.

---

### Task 3: Run the sweep from the worker

**Files:**
- Modify: `internal/listener/work.go` (`Worker` struct, `NewWorker`, `tickOne`)
- Test: `internal/listener/work_test.go`

**Interfaces:**
- Consumes: `listener.Delivery` and `Delivery.IsMergeInto` (Task 1); `loopcmd.TendSweep` (Task 2).
- Produces: `Worker.RunSweep func(ctx context.Context, cfg *config.Config, deps loopcmd.Deps) (loopcmd.Summary, error)`.

- [ ] **Step 1: Write the failing test**

Extend the existing `harness` (`work_test.go:135`) rather than building a second one. Add three
fields and one seam:

```go
type harness struct {
	// ... existing fields ...

	// defaultBranch and tendPR are what harness.open puts on the config it
	// returns. The sweep gate reads both, and the fake config had neither.
	defaultBranch string
	tendPR        bool
	// sweepFn decides what the RunSweep seam returns. Nil means it succeeds.
	sweepFn func(cfg *config.Config) error
	// sweeps records the loop name of each sweep, like ran does for passes.
	sweeps []string
}
```

In `harness.open` (line 222), set them on the config it builds:

```go
	cfg := &config.Config{
		Name:          loopFromPath(path),
		Repo:          "o/r",
		DefaultBranch: h.defaultBranch,
		TendPR:        h.tendPR,
	}
```

Add the seam method beside `runIssue`, and wire `w.RunSweep = h.runSweep` in `newHarness`:

```go
func (h *harness) runSweep(
	_ context.Context, cfg *config.Config, _ loopcmd.Deps,
) (loopcmd.Summary, error) {
	h.mu.Lock()
	h.sweeps = append(h.sweeps, cfg.Name)
	fn := h.sweepFn
	h.mu.Unlock()
	if fn != nil {
		return loopcmd.Summary{}, fn(cfg)
	}
	return loopcmd.Summary{}, nil
}
```

Then append the tests:

```go
// The sweep runs for exactly one case, and the issue pass always runs.
func TestDeliverSweepsOnlyOnAMergeIntoTheLoopsDefaultBranch(t *testing.T) {
	cases := []struct {
		name      string
		delivery  Delivery
		tendPR    bool
		wantSweep bool
	}{
		{"a merge into the default branch", Delivery{Repo: "o/r", Number: 7, MergedInto: "master"}, true, true},
		{"a merge into a feature branch", Delivery{Repo: "o/r", Number: 7, MergedInto: "feature"}, true, false},
		{"not a merge", Delivery{Repo: "o/r", Number: 7}, true, false},
		{"a merge, but the loop does not tend", Delivery{Repo: "o/r", Number: 7, MergedInto: "master"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(nil)
			h.defaultBranch = "master"
			h.tendPR = tc.tendPR

			h.w.Deliver(context.Background(), tc.delivery)

			// The merged pull request's own pass moves its issue to a terminal
			// state. The sweep never replaces it.
			if len(h.ran) != 1 {
				t.Errorf("issue passes = %d, want 1", len(h.ran))
			}
			want := 0
			if tc.wantSweep {
				want = 1
			}
			if len(h.sweeps) != want {
				t.Errorf("sweeps = %d, want %d", len(h.sweeps), want)
			}
		})
	}
}

// A failing issue pass schedules its retry as before, and the sweep still
// runs: the base branch moved whatever happened to that one issue.
func TestDeliverSweepsEvenWhenTheIssuePassFails(t *testing.T) {
	h := newHarness(nil)
	h.defaultBranch = "master"
	h.tendPR = true
	h.runFn = func(*config.Config) error { return errors.New("boom") }

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7, MergedInto: "master"})

	if len(h.sweeps) != 1 {
		t.Errorf("sweeps = %d, want 1", len(h.sweeps))
	}
}

// A sweep failure is logged and dropped. It must not schedule a retry: the
// retry path re-runs the ISSUE pass, and re-running it for a sweep failure
// would spend that issue's retry budget on something the issue did not do.
func TestDeliverDoesNotRetryAFailedSweep(t *testing.T) {
	h := newHarness(nil)
	h.defaultBranch = "master"
	h.tendPR = true
	h.max = 3
	h.backoff = []time.Duration{0, 0, 0}
	h.sweepFn = func(*config.Config) error { return errors.New("sweep failed") }

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7, MergedInto: "master"})

	if n := h.timers.len(); n != 0 {
		t.Errorf("scheduled %d retries, want 0", n)
	}
}
```

`timers.len()` (`work_test.go:62`) is the accessor this file already uses for "how many timers
were armed".

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/listener/ -run TestDeliverSweeps -v`
Expected: FAIL — `w.RunSweep undefined`.

- [ ] **Step 3: Add the seam**

In `internal/listener/work.go`, add to the `Worker` struct, beside `RunIssue`:

```go
	// RunSweep rebases every stale pull request of one loop, taking the loop's
	// lock first. Production wires it to loopcmd.TendSweep.
	//
	// It runs for ONE delivery -- a pull request merged into the loop's
	// default branch -- because that is the only event that makes many pull
	// requests stale at once and names none of them. Every other delivery gets
	// RunIssue and nothing more; see RunIssue for the reconcile that was
	// removed and must not come back.
	RunSweep func(ctx context.Context, cfg *config.Config, deps loopcmd.Deps) (loopcmd.Summary, error)
```

In `NewWorker`, add `RunSweep: loopcmd.TendSweep,`.

- [ ] **Step 4: Split the issue pass out of `tickOne` and add the sweep**

Replace `tickOne`'s body. The issue pass keeps every existing branch and comment verbatim; it
just moves into a helper so the sweep can follow it on both the success and the failure path.

```go
func (w *Worker) tickOne(ctx context.Context, t Target, d Delivery, acc *access) {
	key := loopKey{ProjectID: t.ProjectID, LoopName: t.LoopName}

	cfg, deps, cleanup, err := w.Open(t.Ref(), t.ConfigPath, loopcmd.Options{
		Token:           acc.token,
		GH:              acc.gh,
		RequireGitHub:   true,
		MigrationPolicy: loopcmd.FailOnUnimported,
	})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		// No cfg here, so the loop's backoff list is unknown, and no sweep is
		// possible either: the sweep needs default_branch and tend_pr from the
		// file that could not be read.
		slog.Error("cannot open loop", "loop", t.LoopName, "project", t.ProjectName,
			"config", t.ConfigPath, "err", err)
		w.schedule(ctx, t, d.Number, kindOpen, openRetryMax, func(int) time.Duration { return w.OpenRetryDelay })
		return
	}

	w.runIssuePass(ctx, t, d, cfg, deps, key)

	// The sweep follows the issue pass on BOTH paths. The merged pull request's
	// own pass is what moves its issue to a terminal state; whether that
	// succeeded says nothing about the other branches, which are behind because
	// the base moved.
	if cfg.TendPR && d.IsMergeInto(cfg.DefaultBranch) {
		w.sweepOne(ctx, t, cfg, deps)
	}
}

// runIssuePass acts on the one issue the delivery named and decides what the
// outcome means for the retry schedule.
//
// d.Number is carried through every retry this schedules, so a retry re-runs
// the SAME scoped pass rather than widening into a reconcile: a delivery that
// failed and is retried a minute later must still be about the issue the
// delivery named.
func (w *Worker) runIssuePass(
	ctx context.Context, t Target, d Delivery,
	cfg *config.Config, deps loopcmd.Deps, key loopKey,
) {
	if _, err := w.RunIssue(ctx, cfg, deps, d.Number); err != nil {
		if errors.Is(err, lock.ErrHeld) {
			// No retry. The delivery carries no state of its own, so the tick
			// already holding the lock reads the same GitHub state a moment
			// later than this one would have. The pending attempt is cleared
			// too, or the next real failure would resume a backoff list part
			// way through and give up early.
			slog.Info("skipping tick: another tick holds the loop lock",
				"loop", cfg.Name, "project", t.ProjectName, "issue", d.Number)
			w.clear(key)
			return
		}
		slog.Error("tick failed", "loop", cfg.Name, "project", t.ProjectName,
			"issue", d.Number, "err", err)
		w.schedule(ctx, t, d.Number, kindTick, cfg.Retry.Max, func(n int) time.Duration {
			return w.backoffFor(cfg, n)
		})
		return
	}
	w.clear(key)
}

// sweepOne rebases the loop's stale pull requests after a merge.
//
// A failure here is logged and dropped. It schedules NO retry, for two
// reasons: the retry path re-runs the issue pass, so a sweep failure would
// spend an issue's retry budget on something that issue did not do; and the
// work is not lost, because the next merge sweeps again.
//
// A held lock is not a failure. A burst of merges collapses to one sweep here,
// which is why no debounce exists: lock.Acquire is non-blocking, so the second
// sweep of a burst is skipped rather than queued.
func (w *Worker) sweepOne(ctx context.Context, t Target, cfg *config.Config, deps loopcmd.Deps) {
	if _, err := w.RunSweep(ctx, cfg, deps); err != nil {
		if errors.Is(err, lock.ErrHeld) {
			slog.Info("skipping tend sweep: another tick holds the loop lock",
				"loop", cfg.Name, "project", t.ProjectName)
			return
		}
		slog.Error("tend sweep failed", "loop", cfg.Name, "project", t.ProjectName, "err", err)
		return
	}
}
```

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./internal/listener/ -run TestDeliver -v`
Expected: PASS.

- [ ] **Step 6: Mutation-check the gate**

Change `if cfg.TendPR && d.IsMergeInto(cfg.DefaultBranch)` to `if cfg.TendPR`. Run
`go test ./internal/listener/ -run TestDeliverSweepsOnly`. It MUST fail on the "not a merge" and
"a merge into a feature branch" cases. Restore the condition and report the result.

- [ ] **Step 7: Run the gates**

Run: `make check && make test/race`
Expected: all green. The race detector matters here: `Deliver` runs from the handler's pool
goroutines, and this task adds a read of `cfg` on a new path.

- [ ] **Step 8: Commit**

```bash
git add internal/listener
git commit -m "$(cat <<'EOF'
feat: sweep stale pull requests when a merge lands on the default branch

A pull_request delivery that merged into the loop's default_branch now
runs loopcmd.TendSweep after the delivery's own issue pass, for loops
with tend_pr: true. Every other delivery is unchanged.

The sweep runs on both the success and failure path of the issue pass:
the base branch moved whatever happened to that one issue. A failed
sweep schedules no retry, because the retry path re-runs the issue pass.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

**Acceptance criteria:** the three `TestDeliver` tests pass; the Step 6 mutant fails its test and
is reported; `make check` and `make test/race` green.

**review: yes** — this is the gate that decides when repository-wide dispatch happens.

---

### Task 4: Document when a sweep runs

**Files:**
- Modify: `docs/configuration.md` (the `tend_pr` section)

- [ ] **Step 1: Extend the `tend_pr` section**

Find the `## tend_pr` section. After the sentence describing the per-tick behavior, add:

```markdown
### When a sweep runs

A tend dispatch now has two triggers.

- **A delivery for one issue.** The issue carries `labels.review`, its linked pull request is
  behind its base, and the delivery named that issue. This is the narrow path, and it is the
  only one for every delivery that is not a merge.
- **A merge into `default_branch`.** GitHub sends a `pull_request` delivery with
  `merged: true`. The merge is what made the other pull requests stale, and no delivery names
  them, so this one starts a sweep across the loop: every issue carrying `labels.review` whose
  linked pull request is now behind gets a tend dispatch.

The sweep dispatches **tend agents only**. It never starts, resumes, or retries an issue agent,
and it never parks an issue. A merge is a reason to rebase; it is not a reason to start work.

A loop with `tend_pr: false` never sweeps.

Several merges close together cost one sweep, not several. The loop lock is not held while
waiting, so a second sweep that arrives during the first is skipped, and a sweep that runs after
one finished dispatches nothing for a pull request whose tend agent is still live.

**A sweep does not replace a periodic tick.** `agent-utils project loop tick` is still the only
full reconcile: it is what retires a dead runner for an issue no delivery names, and what finds
a pull request that fell behind for any reason other than a merge. Schedule it.
```

- [ ] **Step 2: Run the gates**

Run: `make check`
Expected: green.

- [ ] **Step 3: Commit**

```bash
git add docs/configuration.md
git commit -m "$(cat <<'EOF'
docs: record the merge-triggered tend sweep

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

**Acceptance criteria:** `make check` green; the section names both triggers and states that a
sweep does not replace the periodic tick.

**review: no** — prose only; the whole-diff fan-out still covers it.

---

## Out of scope

Recorded so a later reader does not mistake a limit for an oversight.

- **A periodic sweep, and any scheduler in the daemon.** The operator chose merge-only for this
  change. `loopcmd.Tick` never runs on the machine in question — there is no cron entry and no
  launchd job — so a pull request that falls behind for any other reason still has nothing
  scheduled to notice it.
- **A `push` trigger.** `tick.go:78` names the gap: GitHub sends a `push` event, not a
  `pull_request` event, when someone pushes to the default branch directly, and this daemon does
  not subscribe to `push`. A direct push to `master` therefore starts no sweep. Subscribing would
  mean adding `push` to `ghub.HookEvents`, re-registering every webhook, and relaxing the
  handler's rule that every delivery names an issue number (`handler.go:501`) — a `push` payload
  names none. That is a larger change than this one.
- **Retiring dead non-tend runners loop-wide.** The sweep leaves them alone by design (Task 2).
  Nothing else retires them either when no delivery arrives for their issue; that is a gap in
  the daemon, not one this change creates.

## Pipeline State

| Field   | Value                                                                 |
|---------|-----------------------------------------------------------------------|
| stage   | 2 (plan review)                                                       |
| class   | standard (new trigger path and a widened seam; no schema, no new dependency) |
| profile | backend                                                               |
| branch  | feat/merge-triggered-tend-sweep                                       |
| pr      | #9                                                                    |
| gate    | pending                                                               |
| round   | 0                                                                     |
