# Tend triggers and the agent-free rebase — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Find a stale pull request without a merge and without a delivery, and rebase it with git
instead of an agent when the replay is clean.

**Architecture:** Three changes to one path. A push to the default branch arms the existing tend
sweep. A ticker in the listener finds stale pull requests with local git, and spends a GitHub call
only when a branch is behind. `loopcmd.act` tries a git rebase before it dispatches a tend agent,
and escalates to the agent on a conflict.

**Tech Stack:** Go 1.25, `urfave/cli/v3`, `gopkg.in/yaml.v3`, SQLite through `internal/store`,
`go-github` webhook validation, git through `os/exec`.

**Spec:** `docs/superpowers/specs/2026-08-31-tend-triggers-and-agent-free-rebase-design.md`

## A note on the test code below

Every `_test.go` file in this repository is in the **same package** as the code it tests
(`package config`, `package store`, `package settings`, `package loopcmd`, `package listener`,
`package worktree`) — not an external `_test` package. Several test snippets in this plan are
written with qualified names (`config.List`, `store.PRLink`, `settings.Fields`). Drop the package
qualifier when you write them, or they will not compile.

The helper names in the snippets are illustrative. The real ones are: `initRepo`
(`internal/worktree/worktree_test.go:10`), `openTemp` (`internal/store/store_test.go:15`),
`newServer` / `doRequest` / `subjectPayload` / `fixedSecret` (`internal/listener/handler_test.go:91,
135, 66, 29`), `newHarness` plus the `timers` seam (`internal/listener/work_test.go:162, 49`), and
`writeLoopFiles` / `loopFile` (`internal/loopcmd/epicsweep_test.go:168, 135` — `package loopcmd`
only, NOT reachable from `internal/config`). Use those; write the ones that genuinely do not
exist.

## Global Constraints

This repository has no `CLAUDE.md`, `AGENTS.md`, or `STANDARDS.md`. The binding rules are the
gates and the conventions the code itself enforces:

- `make check` must pass: `fmtcheck`, `go vet` (host and `GOOS=darwin`), `golangci-lint run`,
  and the full test suite (`Makefile:173`).
- Every exported symbol carries a doc comment. This codebase states **why** a rule exists, not
  only what the code does. Match the density of the file you edit.
- A comment must not restate the code. Where a decision has a failure mode, name the failure.
- `ghub.HookEvents` is the single event list. `register-webhook` and the handler both read it
  (`internal/ghub/types.go:123-129`). Never add a second list.
- A value from a webhook payload is attacker-controlled. Shape-check it before it is stored,
  passed to git, or logged (`internal/listener/handler.go:519-528`).
- Never pass an unchecked ref to git. Use `ghub.SafeRef` or `worktree.SafeRef`
  (`internal/worktree/worktree.go:107`).
- No `Co-Authored-By:` trailer in any commit message.
- Commit messages use Conventional Commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`).

## Verified external API (do not re-derive)

Read from source in this repository at the stated lines.

- `config.Entry` — `internal/config/discover.go:36-55`. Fields: `Name`, `File`, `Path`, `Repo`,
  `Err`. `config.List` already calls `config.Load` for each file and keeps two fields
  (`discover.go:171-176`).
- `config.Config` — `internal/config/config.go`. Relevant fields: `Name`, `Repo`,
  `DefaultBranch`, `TendPR bool` (`tend_pr`), `StateDir`, `CheckoutBaseDir`, `Agent.Worktree`,
  `Labels.Review`.
- `config.Duration` — `internal/config/duration.go:11-31`. It has `UnmarshalYAML` and `Std()`.
  It has **no** `MarshalYAML`; Task 5 adds one.
- `config.WorktreePerIssue` — the `per_issue` worktree mode constant.
- `ghub.HookEvents` — `internal/ghub/types.go:123-129`. Five events today.
- `ghub.SafeRef(ref string) bool` — `internal/ghub/types.go:146`.
- `ghub.Client` — `ListOpenIssues(ctx, owner, repo)`, `ListOpenPullRequests(ctx, owner, repo)`,
  `BehindBy(ctx, owner, repo, base, head string) (int, error)`.
- `ghub.Issue.HasLabel(name string) bool`; `ghub.PullRequest` fields `Number`, `HeadRef`,
  `BaseRef`.
- `engine.LinkPR(issue int, prs []ghub.PullRequest) (ghub.PullRequest, bool)`.
- `engine.Decision` — `internal/engine/types.go:60-75`. Fields used here: `Kind`, `Issue`,
  `Title`, `PR`, `SessionID`, `HeadRef`, `BaseRef`, `Reason`, `Overrides`.
- `engine.KindTend` — the tend decision kind.
- `loopcmd.Deps` — `internal/loopcmd/tick.go:23-43`. Fields used here: `Store`, `GH`, `WT`,
  `Now`, `Fetch`.
- `loopcmd.Summary` — `internal/loopcmd/tick.go:91-105`.
- `loopcmd.act` — `internal/loopcmd/tick.go:355-395`. `case engine.KindTend` is at line 379.
- `loopcmd.TendSweep(ctx, cfg, deps, base)` — `internal/loopcmd/tendsweep.go:71`.
- `listener.Target` — `internal/listener/route.go:16-23`.
- `listener.Scan() (Routes, error)` — `internal/listener/route.go:120`.
- `listener.Delivery` — `internal/listener/work.go:329-357`. `IsMergeInto` is at `work.go:365`.
- `Worker.armTend(ctx, t Target, base string)` — `internal/listener/work.go:855`.
- `Worker.tickOne` — `internal/listener/work.go:532`. The tend arm is at `work.go:577-579`.
- `Worker.Serve(ctx)` — `internal/listener/work.go:1398`.
- `Worker.After` — the `time.AfterFunc` seam (`work.go:241`).
- `settings.Settings` / `settings.Webhook` — `internal/settings/settings.go:56-67`.
  `WithDefaults` is at `settings.go:79`. `Fields()` is at `settings.go:360`.
- `store.Store.PRLinks(loop, repo string) (map[int]PRLink, error)` — `internal/store/store.go:1110`.
- `store.Store.PutPRLink(l PRLink) error` — `store.go:1092`.
- `store.Store.CreateDispatch(d Dispatch) (int64, error)` — `store.go:904`. It always writes
  status `running`.
- `store.Store.FinishDispatch(id int64, r DispatchResult) error` — `store.go:948`. It updates
  only a row that is still `running`.
- `store.KindStart` / `KindResume` / `KindTend` — `internal/store/types.go:5-10`.
- `worktree.Manager.EnsurePR(number int, headRef string) (string, error)` —
  `internal/worktree/worktree.go:75`. It fetches the head ref and checks it out detached.
- `worktree.Manager.Dirty(path string) (bool, error)` — `worktree.go:140`.
- `worktree.Manager.Fetch() error` — `worktree.go:43`. It runs `git fetch origin --prune`.
- `worktree.Manager.git` / `gitOutput` — `worktree.go:166-179`. Both are unexported and use
  `exec.Command` with **no** deadline.

---

## Task 1: carry `default_branch` and `tend_pr` on a routing target

`config.List` loads each loop file in full and keeps two of its fields. The push filter and the
periodic pass both need two more. Taking them from the config already in hand costs no file read.

**Files:**
- Modify: `internal/config/discover.go:36-55` (the `Entry` type), `internal/config/discover.go:171-176`
- Modify: `internal/listener/route.go:16-23` (the `Target` type), `internal/listener/route.go:179-186`
- Test: `internal/config/discover_test.go`, `internal/listener/route_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `config.Entry.DefaultBranch string`, `config.Entry.TendPR bool`,
  `listener.Target.DefaultBranch string`, `listener.Target.TendPR bool`.

**review: no**

- [ ] **Step 1: Write the failing tests**

In `internal/config/discover_test.go`:

```go
// List already loads each file in full. The two fields below are read off that
// same load, so the push filter and the periodic tend pass never open a
// database to learn which branch a loop tends.
func TestListCarriesTheTendFacts(t *testing.T) {
	dir := writeLoopFiles(t, "planning", map[string]string{
		"planning.yaml": loopFile("planning", "status:go", "status:review", "status:done"),
	})
	entries, err := config.List(filepath.Dir(filepath.Dir(dir)))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].DefaultBranch != "master" {
		t.Errorf("DefaultBranch = %q, want master", entries[0].DefaultBranch)
	}
	if entries[0].TendPR {
		t.Errorf("TendPR = true, want the file's false")
	}
}
```

Use the file helpers the package's own tests already provide. If `discover_test.go` has no
writer helper, write the yaml inline with `os.WriteFile` under
`config.ConfigsDir(t.TempDir()+"/.agent-utils")`, and include every field `config.Load` requires:
`name`, `repo`, `checkout_base_dir`, `worktree_dir`, `state_dir`, `default_branch`, the four
`labels` fields, `agent`, `retry`, `prompt`, `resume_prompt`.

In `internal/listener/route_test.go`, add a case to the existing scan test that asserts the two
fields reach `Target`.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/config/ ./internal/listener/ -run 'TendFacts|Scan' -v`
Expected: FAIL — `entries[0].DefaultBranch undefined`.

- [ ] **Step 3: Add the fields**

In `internal/config/discover.go`, inside `Entry`:

```go
	// DefaultBranch and TendPR are copied off the same Load that fills in Repo.
	// The listener needs both to answer a push delivery without opening a
	// database: a push to a branch no loop tends must cost one field test, not
	// a token read, a SQLite handle, and a migration check. They are empty and
	// false when Err is set, like Repo.
	DefaultBranch string
	TendPR        bool
```

In `List`, beside `entry.Repo = cfg.Repo`:

```go
			entry.DefaultBranch = cfg.DefaultBranch
			entry.TendPR = cfg.TendPR
```

In `internal/listener/route.go`, inside `Target`:

```go
	// DefaultBranch and TendPR are what let Deliver drop a push delivery
	// before it opens anything. Open reads the token, opens a SQLite handle
	// and runs the migration check; a busy feature branch would pay all three
	// on every push, once per loop, for a delivery no loop can act on.
	DefaultBranch string
	TendPR        bool
```

And in the `Target` literal inside `Scan`:

```go
				DefaultBranch: e.DefaultBranch,
				TendPR:        e.TendPR,
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/config/ ./internal/listener/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/discover.go internal/config/discover_test.go internal/listener/route.go internal/listener/route_test.go
git commit -m "feat(listener): carry default_branch and tend_pr on a routing target"
```

---

## Task 2: accept a push delivery

The handler drops any event outside `ghub.HookEvents` and rejects a delivery with no issue
number. A push payload has no number, so both rules change together. This task touches the
authenticated request path, so it is reviewed.

**Files:**
- Modify: `internal/ghub/types.go:123-129`
- Modify: `internal/listener/handler.go:495-530`, `internal/listener/handler.go:611-660`
- Test: `internal/listener/handler_test.go`

**Interfaces:**
- Consumes: `ghub.SafeRef` (`internal/ghub/types.go:141`).
- Produces: `"push"` in `ghub.HookEvents`; `Delivery.PushedTo string` filled by the handler.

**review: yes** — it changes what an unauthenticated body may reach.

- [ ] **Step 1: Write the failing tests**

In `internal/listener/handler_test.go`, follow the file's existing helper for a signed request:

```go
// A push carries no issue number. The rule that rejects a numberless delivery
// stays for every other event: it is what stops a subscription to an event
// this daemon cannot act on from being accepted in silence.
func TestPushIsAcceptedWithoutANumber(t *testing.T) {
	var got Delivery
	srv := testServer(t, func(_ context.Context, d Delivery) { got = d })

	body := `{"ref":"refs/heads/master","repository":{"full_name":"o/r"}}`
	res := postSigned(t, srv, "push", body)

	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 200 or 202", res.StatusCode)
	}
	if got.PushedTo != "master" {
		t.Errorf("PushedTo = %q, want master", got.PushedTo)
	}
	if got.Number != 0 {
		t.Errorf("Number = %d, want 0", got.Number)
	}
}

// The old rule still holds everywhere else.
func TestIssuesDeliveryWithoutANumberIsStillRejected(t *testing.T) {
	srv := testServer(t, func(context.Context, Delivery) { t.Fatal("must not tick") })
	res := postSigned(t, srv, "issues", `{"action":"opened","repository":{"full_name":"o/r"}}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

// A ref that is not a branch, or whose name git would read as an option,
// arms nothing. The value reaches git through the sweep's base comparison.
func TestPushRefsThatAreNotSafeBranchesLeavePushedToEmpty(t *testing.T) {
	for _, ref := range []string{"refs/tags/v1", "refs/heads/-oops", "", "refs/heads/"} {
		var got Delivery
		srv := testServer(t, func(_ context.Context, d Delivery) { got = d })
		postSigned(t, srv, "push", `{"ref":"`+ref+`","repository":{"full_name":"o/r"}}`)
		if got.PushedTo != "" {
			t.Errorf("ref %q gave PushedTo %q, want empty", ref, got.PushedTo)
		}
	}
}
```

Match the names of the helpers the file already uses. If it builds requests inline, build them
the same way rather than adding helpers.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/listener/ -run Push -v`
Expected: FAIL — the push event is dropped with 204, and `PushedTo` does not exist.

- [ ] **Step 3: Add the event**

In `internal/ghub/types.go`:

```go
var HookEvents = []string{
	"issues",
	"issue_comment",
	"pull_request",
	"pull_request_review",
	"pull_request_review_comment",
	// A push to a loop's default branch makes every open pull request of that
	// loop stale and names none of them. A merge produces a push too, so the
	// two overlap -- but a direct push produces no pull_request delivery at
	// all, and that is the case no other event covers.
	"push",
}
```

- [ ] **Step 4: Make the number requirement conditional**

In `internal/listener/handler.go`, decode the ref beside the other payload fields:

```go
			// Ref is the branch a push delivery moved. It is the only subject
			// a push has: the payload carries no issue and no pull request.
			Ref string `json:"ref"`
```

Replace the number gate (`handler.go:502-509`) with:

```go
		number := body.Issue.Number
		if number == 0 {
			number = body.PullRequest.Number
		}
		// A push names a branch, not an issue, so it is the one accepted event
		// with no number. Every other event keeps the rule: a delivery this
		// daemon cannot attribute to a number is one it cannot act on, and
		// answering 202 would hide a malformed delivery -- or a subscription
		// to an event that carries no number -- behind a success the operator
		// never sees.
		if number <= 0 && event != "push" {
			s.rejected(w, deliveryID, "issue number", http.StatusBadRequest)
			return
		}
		if number < 0 {
			number = 0
		}
```

- [ ] **Step 5: Fill in `PushedTo`**

Below the `mergedInto` block:

```go
		// pushedTo is the branch a push delivery moved, and it arms the same
		// sweep a merge does. It is shape-checked here, exactly as mergedInto
		// is: the value reaches git as a branch name, and a ref failing
		// SafeRef could never have been tended anyway. A ref that is not a
		// branch (a tag, for example) leaves this empty, and an empty value
		// arms nothing.
		var pushedTo string
		if event == "push" {
			if ref, ok := strings.CutPrefix(body.Ref, "refs/heads/"); ok && ghub.SafeRef(ref) {
				pushedTo = ref
			}
		}
```

Add `pushedTo` to the `Delivery` literal at `handler.go:654-657`, and to the accepted-delivery
log line beside `merged_into`:

```go
			if pushedTo != "" {
				attrs = append(attrs, "pushed_to", safeText(pushedTo))
			}
```

Do the same in the dropped-delivery branch, where `merged_into` is already appended.

- [ ] **Step 6: Name the subject of a numberless delivery in the log**

The accepted line carries `"number", number`, which is 0 for a push and says nothing. Build the
attributes so a push prints its ref instead:

```go
			attrs := []any{"delivery", safeDeliveryID(deliveryID),
				"event", event, "action", safeAction(body.Action), "repo", repo}
			// The subject of the work, which is a number for every event but
			// one. A push has no number, and "number=0" reads as a bug rather
			// than as "this delivery names a branch".
			if number > 0 {
				attrs = append(attrs, "number", number)
			}
```

- [ ] **Step 7: Add `PushedTo` to `Delivery`**

In `internal/listener/work.go`, inside `Delivery`:

```go
	// PushedTo is the branch a push delivery moved, and is empty for every
	// other delivery. It arms the same tend sweep MergedInto does, for the
	// case a merge cannot cover: a direct push to the default branch produces
	// no pull_request delivery, so nothing else tells this daemon that every
	// open pull request just fell behind.
	PushedTo string
```

- [ ] **Step 8: Run the tests and confirm they pass**

Run: `go test ./internal/listener/ ./internal/ghub/`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/ghub/types.go internal/listener/handler.go internal/listener/work.go internal/listener/handler_test.go
git commit -m "feat(listener): accept a push delivery and carry the branch it moved"
```

---

## Task 3: arm the tend sweep on a push

**Files:**
- Modify: `internal/listener/work.go:362-368` (beside `IsMergeInto`), `work.go:389-440` (`Deliver`),
  `work.go:532-604` (`tickOne`)
- Test: `internal/listener/work_test.go`

**Interfaces:**
- Consumes: `Delivery.PushedTo` (Task 2), `Target.DefaultBranch`, `Target.TendPR` (Task 1).
- Produces: `Delivery.IsPushTo(branch string) bool`.

**review: yes** — it decides which deliveries reach the dispatch path.

- [ ] **Step 1: Write the failing tests**

In `internal/listener/work_test.go`:

```go
// The same rule IsMergeInto states: an empty branch never matches, even
// against an empty PushedTo. A loop with no default_branch names no branch,
// and two absent values are not agreement.
func TestIsPushTo(t *testing.T) {
	d := Delivery{PushedTo: "master"}
	if !d.IsPushTo("master") {
		t.Error("a push to master must match master")
	}
	if d.IsPushTo("main") {
		t.Error("a push to master must not match main")
	}
	if (Delivery{}).IsPushTo("") {
		t.Error("two empty values are not a match")
	}
}

// A push names no issue, so the three passes that act on one issue must not
// run. Only the sweep is armed.
func TestPushArmsTheSweepAndRunsNoIssuePass(t *testing.T) {
	w := newTestWorker(t)
	var issuePasses, tends int
	w.RunIssue = func(context.Context, *config.Config, loopcmd.Deps, int) (loopcmd.Summary, error) {
		issuePasses++
		return loopcmd.Summary{}, nil
	}
	w.RunTend = func(context.Context, *config.Config, loopcmd.Deps, string) (loopcmd.Summary, error) {
		tends++
		return loopcmd.Summary{}, nil
	}

	w.Deliver(context.Background(), Delivery{Repo: "o/r", PushedTo: "master"})
	fireTendTimer(t, w)

	if issuePasses != 0 {
		t.Errorf("issue passes = %d, want 0 for a push", issuePasses)
	}
	if tends != 1 {
		t.Errorf("tend sweeps = %d, want 1", tends)
	}
}

// A merge and the push it produces arrive together. armTend already collapses
// a burst, and this proves the two triggers ride one timer rather than arming
// two sweeps.
func TestAMergeAndItsPushProduceOneSweep(t *testing.T) {
	w := newTestWorker(t)
	var tends int
	w.RunTend = func(context.Context, *config.Config, loopcmd.Deps, string) (loopcmd.Summary, error) {
		tends++
		return loopcmd.Summary{}, nil
	}

	w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 7, MergedInto: "master"})
	w.Deliver(context.Background(), Delivery{Repo: "o/r", PushedTo: "master"})
	fireTendTimer(t, w)

	if tends != 1 {
		t.Errorf("tend sweeps = %d, want 1", tends)
	}
}

// A push to a feature branch must cost nothing: no token read, no SQLite
// handle, no migration check. Open is the seam that proves it.
func TestPushToAnotherBranchOpensNothing(t *testing.T) {
	w := newTestWorker(t)
	var opens int
	inner := w.Open
	w.Open = func(r loopcmd.ProjectRef, p string, o loopcmd.Options) (*config.Config, loopcmd.Deps, func(), error) {
		opens++
		return inner(r, p, o)
	}

	w.Deliver(context.Background(), Delivery{Repo: "o/r", PushedTo: "feat/x"})

	if opens != 0 {
		t.Errorf("Open calls = %d, want 0 for a push to a branch no loop tends", opens)
	}
}
```

Use the package's existing worker fixture and timer control. `work_test.go` already builds a
`Worker` with fake seams and a controlled `After`; reuse those rather than writing new ones. If no
`fireTendTimer` helper exists, drive the recorded timer function the way the existing tend tests
do.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/listener/ -run 'Push|Sweep' -v`
Expected: FAIL — `IsPushTo` is undefined.

- [ ] **Step 3: Add `IsPushTo`**

In `internal/listener/work.go`, below `IsMergeInto`:

```go
// IsPushTo reports whether this delivery pushed to branch.
//
// It follows IsMergeInto's rule exactly: an empty branch never matches, even
// against an empty PushedTo, because a loop with no default_branch names no
// branch to compare against.
func (d Delivery) IsPushTo(branch string) bool {
	return branch != "" && d.PushedTo == branch
}
```

- [ ] **Step 4: Filter a push in `Deliver`**

At the top of `Deliver`'s target loop, before `w.tickOne`:

```go
	// The push filter runs BEFORE w.access(), not only before tickOne.
	// access() reads the token file and builds a client; subscribing to push
	// multiplies delivery volume by every branch of every watched repository,
	// so a filter placed after it would read a secret from disk on every
	// feature-branch push. Build the kept list first, and return when it is
	// empty.
	kept := make([]Target, 0, len(targets))
	for _, t := range targets {
		// A push names no issue, so the only work it can start is the sweep.
		if d.Number == 0 && !(t.TendPR && d.IsPushTo(t.DefaultBranch)) {
			continue
		}
		kept = append(kept, t)
	}
	if len(kept) == 0 {
		slog.Info("no loop tends the branch this push moved",
			"repo", d.Repo, "pushed_to", safeText(d.PushedTo))
		return
	}

	acc, err := w.access()
	// ... existing error handling ...

	for _, t := range kept {
		w.tickOne(ctx, t, d, acc)
	}
```

Keep `Deliver`'s existing "evaluating this issue" log line for a delivery that names a number.
For a push, log the branch instead:

```go
	if d.Number == 0 {
		// safeText, because PushedTo is attacker-written. SafeRef bounds its
		// charset but not its length, so a multi-kilobyte branch name would go
		// verbatim into an unrotated log -- the failure handler.go:185-196
		// documents. work.go is the same package and gets the same treatment.
		slog.Info("a push moved a branch; every loop that tends it will sweep",
			"repo", d.Repo, "pushed_to", safeText(d.PushedTo), "loops", loops)
	} else {
		slog.Info("evaluating this issue in every loop that watches the repository",
			"repo", d.Repo, "number", d.Number, "loops", loops)
	}
```

- [ ] **Step 5: Arm the sweep in `tickOne`**

Replace the arm at `work.go:577-579`:

```go
	// Armed by a merge OR by a push. A merge into the default branch produces
	// a push event too, so the two overlap; armTend collapses them onto one
	// timer, which is why the overlap costs nothing and why the merge trigger
	// stays. It has to stay: a hook that nobody re-registers after this change
	// still carries the old event list, and the merge path is what keeps
	// working for it.
	if cfg.TendPR && (d.IsMergeInto(cfg.DefaultBranch) || d.IsPushTo(cfg.DefaultBranch)) {
		w.armTend(ctx, t, cfg.DefaultBranch)
	}
```

Guard the three issue-scoped passes on the number:

```go
	if d.Number > 0 && w.openIssueWindow(ctx, t, d.Number) {
		w.issuePass(ctx, t, d, cfg, deps, key)
	}
```

`epicPass` and `cleanupPass` are already gated on `d.ClosedIssue` and `d.ClosedPR`, which a push
never sets, so they need no change. Confirm that by reading the two branches; do not add a
redundant condition.

**The `Open` failure path also needs the guard, and this one is a real bug if you miss it.**
`tickOne` schedules a retry when `Open` fails (`internal/listener/work.go:559`):

```go
	w.schedule(ctx, t, d.Number, kindOpen, openRetryMax, ...)
```

`pending` is keyed by `loopKey` — **per loop, not per issue** (`work.go:1044-1065`) — and
`schedule` stops whatever timer is already armed for that key. So a push whose `Open` failed
would cancel a real issue's pending retry and spend the loop's `kindOpen` budget on issue 0.
The retry that eventually fires calls `tickFresh(ctx, t, 0)`, which builds a `Delivery` carrying
no `PushedTo` (`work.go:511-522` drops it deliberately), so it is then gated out by the
`d.Number > 0` guard above and accomplishes nothing — while the sweep the push should have armed
is lost.

Guard it:

```go
	if err != nil {
		slog.Error("cannot open loop", "loop", t.LoopName, "project", t.ProjectName,
			"config", t.ConfigPath, "err", err)
		// A numberless delivery names no issue, so it must not enter the
		// issue retry schedule: that schedule is keyed per loop and would
		// cancel a real issue's pending retry. The sweep is not lost -- the
		// periodic check finds the same stale branches on its next tick.
		if d.Number > 0 {
			w.schedule(ctx, t, d.Number, kindOpen, openRetryMax,
				func(int) time.Duration { return w.OpenRetryDelay })
		}
		return
	}
```

Add a test: a push delivery whose `Open` fails must leave an existing issue's pending retry
armed.

- [ ] **Step 6: Run the tests and confirm they pass**

Run: `go test ./internal/listener/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/listener/work.go internal/listener/work_test.go
git commit -m "feat(listener): arm the tend sweep on a push to the default branch"
```

---

## Task 4: local behind-count and pr_links deletion

The periodic pass needs two primitives: a behind count from local git, and a way to delete a row
for a pull request that has closed. Nothing deletes a `pr_links` row today.

**Files:**
- Modify: `internal/worktree/worktree.go`
- Modify: `internal/store/store.go` (beside `PRLinks`, `store.go:1110`)
- Test: `internal/worktree/worktree_test.go`, `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func (m *Manager) BehindLocal(headRef, baseRef string) (behind int, known bool, err error)`
  - `func (s *Store) DeletePRLink(loop, repo string, number int) error`

**review: no**

- [ ] **Step 1: Write the failing tests**

In `internal/worktree/worktree_test.go`, build a real repository with `git init`, following the
package's existing test setup:

```go
// The periodic tend pass counts what the base has and the head does not, with
// no GitHub call. A branch the prune removed is not an error and not behind:
// it is a pull request whose branch is gone, and the pass must skip it.
func TestBehindLocal(t *testing.T) {
	m := newTestManagerWithOrigin(t) // origin/master and origin/feature exist

	behind, known, err := m.BehindLocal("feature", "master")
	if err != nil {
		t.Fatal(err)
	}
	if !known {
		t.Fatal("known = false, want true for a branch that resolves")
	}
	if behind != 2 {
		t.Errorf("behind = %d, want 2", behind)
	}

	_, known, err = m.BehindLocal("branch-that-was-pruned", "master")
	if err != nil {
		t.Fatalf("a missing ref must not be an error: %v", err)
	}
	if known {
		t.Error("known = true for a ref that does not resolve")
	}
}

// An unsafe ref never reaches git.
func TestBehindLocalRejectsAnUnsafeRef(t *testing.T) {
	m := newTestManagerWithOrigin(t)
	if _, _, err := m.BehindLocal("-oops", "master"); err == nil {
		t.Error("an unsafe ref must be refused")
	}
}
```

In `internal/store/store_test.go`:

```go
// Nothing deleted a pr_links row before this. A row for a pull request that
// merged days ago is still read by every later pass, and the local gate would
// count its branch as behind forever.
func TestDeletePRLink(t *testing.T) {
	s := newTestStore(t)
	if err := s.PutPRLink(store.PRLink{
		Loop: "execution", Repo: "o/r", Number: 7, PRNumber: 9,
		HeadRef: "feat/x", BaseRef: "master", BehindBy: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePRLink("execution", "o/r", 7); err != nil {
		t.Fatal(err)
	}
	links, err := s.PRLinks("execution", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := links[7]; ok {
		t.Error("the row is still present after DeletePRLink")
	}
	// Deleting a row that is not there is not an error: the confirm pass
	// deletes what it believes is gone, and two passes can agree.
	if err := s.DeletePRLink("execution", "o/r", 7); err != nil {
		t.Errorf("a second delete must be a no-op: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/worktree/ ./internal/store/ -run 'BehindLocal|DeletePRLink' -v`
Expected: FAIL — both symbols are undefined.

- [ ] **Step 3: Implement `BehindLocal`**

In `internal/worktree/worktree.go`:

```go
// BehindLocal counts the commits origin/baseRef has that origin/headRef does
// not, using only the local checkout.
//
// It is the gate of the periodic tend pass. The equivalent GitHub call
// (ghub.BehindBy) costs one request per pull request, per loop, per project,
// on every interval; this costs a rev-list against refs the loop's fetch
// already updated. It answers only "is this branch behind", never "should this
// branch be rebased" -- loopcmd.TendSweep stays the one place that decides
// that, so a stale local ref can never cause a dispatch on its own.
//
// known is false when origin/headRef does not resolve. That is not an error:
// Fetch prunes, so a pull request whose branch was deleted loses its remote
// ref, and the caller must skip the row rather than fail the pass.
func (m *Manager) BehindLocal(headRef, baseRef string) (behind int, known bool, err error) {
	if !SafeRef(headRef) {
		return 0, false, fmt.Errorf("unsafe branch name %q", headRef)
	}
	if !SafeRef(baseRef) {
		return 0, false, fmt.Errorf("unsafe branch name %q", baseRef)
	}
	head, base := "origin/"+headRef, "origin/"+baseRef
	if _, err := m.gitOutput(m.checkoutBaseDir, "rev-parse", "--verify", "--quiet", head+"^{commit}"); err != nil {
		return 0, false, nil
	}
	if _, err := m.gitOutput(m.checkoutBaseDir, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		return 0, false, nil
	}
	out, err := m.gitOutput(m.checkoutBaseDir, "rev-list", "--count", head+".."+base)
	if err != nil {
		return 0, false, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, false, fmt.Errorf("parse rev-list count %q: %w", strings.TrimSpace(out), err)
	}
	return n, true, nil
}
```

Add `strconv` to the imports.

- [ ] **Step 4: Implement `DeletePRLink`**

In `internal/store/store.go`, below `PRLinks`:

```go
// DeletePRLink removes one issue-to-pull-request mapping.
//
// A row outlives the pull request it names: nothing removed one before this,
// so a database accumulates a row for every pull request it ever linked. The
// periodic tend pass counts a merged branch as behind its base forever, so the
// dead rows would defeat the gate that exists to avoid GitHub calls.
//
// Deleting a row that is not there is not an error. The confirm pass deletes
// what GitHub says is gone, and two passes may agree about the same row.
func (s *Store) DeletePRLink(loop, repo string, number int) error {
	_, err := s.db.Exec(
		`DELETE FROM pr_links WHERE project_id = ? AND loop = ? AND repo = ? AND number = ?`,
		s.projectID, loop, repo, number)
	if err != nil {
		return fmt.Errorf("delete pr link: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./internal/worktree/ ./internal/store/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/worktree/worktree.go internal/worktree/worktree_test.go internal/store/store.go internal/store/store_test.go
git commit -m "feat: local behind count and pr_links deletion"
```

---

## Task 5: the `tend_interval` setting

**Files:**
- Modify: `internal/settings/settings.go:56-90` and `settings.go:360` (`Fields`)
- Modify: `internal/settings/settings_test.go:38-59` (the round-trip test)
- Test: `internal/settings/settings_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `settings.Settings.TendInterval string` (`yaml:"tend_interval,omitempty"`),
  `settings.DefaultTendInterval = 15 * time.Minute`,
  `func (s Settings) TendEvery() time.Duration`, and the `tend_interval` key in
  `settings.Fields()`.

**review: no**

**The field is a string, not a `config.Duration`, and that is deliberate.** Three reasons, each
of which was a defect in the first draft of this plan:

1. `settings.Save` marshals the whole struct (`internal/settings/settings.go:274`). A typed
   duration with no `omitempty` writes `tend_interval: 0s` for a machine that never set one. The
   next `Load` then sees the key, and any "was it set?" flag reads true — so the periodic pass
   would silently disable itself on every machine that ever ran `agent-utils config set`. A
   string with `omitempty` is absent when it is empty, so "never set" and "set to zero" stay
   distinguishable with no second field.
2. `internal/settings`'s package doc opens by distancing this package from `internal/config`
   ("It is neither internal/config, which loads one LOOP's configuration file"). Importing
   `config` for a scalar contradicts it.
3. `config.Duration` has no `MarshalYAML`, and `internal/wizard/write.go:13-23` carries a
   ten-line comment that depends on it not having one. Adding it would make that comment false
   and drag the wizard into this change.

- [ ] **Step 1: Write the failing tests**

In `internal/settings/settings_test.go` — the file is `package settings`, so refer to the
symbols unqualified, as the tests around it do:

```go
// The default is applied by WithDefaults, not by Load, exactly as the listen
// address is: Load must keep returning a true zero value for a machine that
// has never run `config`.
func TestTendEveryDefaultsWhenUnset(t *testing.T) {
	if got := (Settings{}).TendEvery(); got != DefaultTendInterval {
		t.Errorf("TendEvery() = %v, want %v", got, DefaultTendInterval)
	}
}

// Zero disables the periodic check. It must survive, or an operator who turns
// the pass off gets it back on the next load.
func TestTendEveryHonoursAnExplicitZero(t *testing.T) {
	for _, v := range []string{"0", "0s"} {
		if got := (Settings{TendInterval: v}).TendEvery(); got != 0 {
			t.Errorf("TendEvery(%q) = %v, want 0", v, got)
		}
	}
}

// An unparsable value falls back to the default rather than disabling the
// pass. Set already refuses a bad value, so a bad one in the file was
// hand-written, and silently turning the check off is the worse failure.
func TestTendEveryFallsBackOnGarbage(t *testing.T) {
	if got := (Settings{TendInterval: "later"}).TendEvery(); got != DefaultTendInterval {
		t.Errorf("TendEvery() = %v, want the default", got)
	}
}

// The bug this field shape exists to prevent: a settings file saved with no
// tend_interval must come back with no tend_interval, so the default still
// applies.
func TestSaveThenLoadLeavesAnUnsetTendIntervalUnset(t *testing.T) {
	withHome(t)
	if err := Save(&Settings{Webhook: Webhook{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.TendInterval != "" {
		t.Errorf("TendInterval = %q, want empty so the default applies", got.TendInterval)
	}
	if got.TendEvery() != DefaultTendInterval {
		t.Errorf("TendEvery() = %v, want the default", got.TendEvery())
	}
}

func TestTendIntervalIsASettableKey(t *testing.T) {
	var found bool
	for _, f := range Fields() {
		if f.Key != "tend_interval" {
			continue
		}
		found = true
		s := &Settings{}
		if err := f.Set(s, "30m"); err != nil {
			t.Fatal(err)
		}
		if s.TendInterval != "30m" {
			t.Errorf("after Set: %q, want 30m", s.TendInterval)
		}
		if got := f.Get(s); got != "30m" {
			t.Errorf("Get = %q, want 30m", got)
		}
		if err := f.Set(s, "nonsense"); err == nil {
			t.Error("an unparsable duration must be refused")
		}
		if err := f.Set(s, "-5m"); err == nil {
			t.Error("a negative duration must be refused")
		}
		f.Unset(s)
		if s.TendInterval != "" {
			t.Errorf("after Unset: %q, want empty", s.TendInterval)
		}
	}
	if !found {
		t.Fatal("tend_interval is not a settable key")
	}
}
```

**Also extend the existing round-trip test.** `TestSaveThenLoadRoundTripsEveryField`
(`internal/settings/settings_test.go:38-59`) compares whole structs with `*got != *want`. It is
this repo's check that a new settings field survives Save/Load, so the fixture must gain the new
field:

```go
	want := &Settings{
		Webhook: Webhook{
			Enabled:    true,
			URL:        "https://example.com/hooks/agent-utils",
			ListenAddr: "0.0.0.0",
			ListenPort: 9999,
			Secret:     "supersecretvalue",
		},
		TendInterval: "30m",
	}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/settings/ -run Tend -v`
Expected: FAIL — `TendInterval` and `TendEvery` do not exist.

- [ ] **Step 3: Add the setting**

In `internal/settings/settings.go`:

```go
// DefaultTendInterval is how often the listener looks for a pull request that
// has fallen behind its base. It is applied by TendEvery, not by Load, for the
// reason WithDefaults states: Load must return a true zero value for a machine
// that has never run `config`.
//
// The check is nearly free when nothing is behind -- it compares local refs
// the loop already fetched and makes no GitHub call -- so this value is set by
// how soon an operator wants a stale branch noticed, not by what the check
// costs.
const DefaultTendInterval = 15 * time.Minute

// Settings is the machine-wide configuration.
type Settings struct {
	Webhook Webhook `yaml:"webhook"`
	// TendInterval is how often the listener runs its periodic tend check.
	// "0" disables that check and leaves every other tend trigger unchanged;
	// an ABSENT value takes DefaultTendInterval.
	//
	// It is a string, and it carries omitempty, so those two states stay
	// distinguishable through a Save. Save marshals the whole struct, so a
	// typed duration would write "tend_interval: 0s" for a machine that never
	// set one, and the next Load could not tell that from an operator who
	// disabled the pass on purpose.
	//
	// It is machine-wide rather than per loop because it describes how
	// attentive the daemon is, not anything about a loop. It applies to every
	// loop with tend_pr: true, of every registered project.
	TendInterval string `yaml:"tend_interval,omitempty"`
}

// TendEvery returns the periodic tend check's interval.
//
// An absent value takes the default. An explicit "0" disables the check. A
// value that does not parse ALSO takes the default rather than disabling the
// check: Fields refuses a bad value, so a bad one in the file was written by
// hand, and silently turning off the pass that keeps branches current is the
// worse of the two failures.
func (s Settings) TendEvery() time.Duration {
	if strings.TrimSpace(s.TendInterval) == "" {
		return DefaultTendInterval
	}
	d, err := time.ParseDuration(s.TendInterval)
	if err != nil || d < 0 {
		return DefaultTendInterval
	}
	return d
}
```

Add `time` to the imports. `WithDefaults` is **not** changed: `TendEvery` owns the default, so
`Load` and `Save` keep round-tripping the stored text unchanged.

In `Fields()`, add:

```go
		{
			Key: "tend_interval",
			Get: func(s *Settings) string { return s.TendInterval },
			Set: func(s *Settings, v string) error {
				d, err := time.ParseDuration(v)
				if err != nil {
					return fmt.Errorf("tend_interval must be a duration such as \"15m\": %w", err)
				}
				if d < 0 {
					return fmt.Errorf("tend_interval must not be negative")
				}
				// Stored as the operator typed it, not as the parsed value
				// re-rendered: `config get` should read back what `config set`
				// was given.
				s.TendInterval = v
				return nil
			},
			Unset: func(s *Settings) { s.TendInterval = "" },
		},
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/settings/ ./cmd/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/settings/settings.go internal/settings/settings_test.go
git commit -m "feat(settings): add the machine-wide tend_interval"
```

---

## Task 6: the periodic tend candidate pass

This is the gate. It answers one question — is any of this loop's pull requests behind — and
spends a GitHub call only when the local answer is yes.

**Files:**
- Create: `internal/loopcmd/tendcheck.go`
- Create: `internal/loopcmd/tendcheck_test.go`

**Interfaces:**
- Consumes: `Manager.BehindLocal`, `Store.DeletePRLink` (Task 4), `Deps`, `ghub.Client`.
- Produces:
  - `Deps.Behind func(headRef, baseRef string) (behind int, known bool, err error)` — a new
    field on `loopcmd.Deps`, wired in `loopcmd.Open` to `deps.WT.BehindLocal`. It is a seam
    because `Deps.WT` is a concrete `*worktree.Manager` that a test cannot substitute. `Deps` is
    also built by hand at several call sites, so a nil value must be treated as "no candidates",
    the way `Deps.Fetch` is nil-guarded at `internal/loopcmd/tendsweep.go:164`.
  - `loopcmd.TendCheck` and `loopcmd.TendCheckResult`:

```go
// TendCheckResult is what one candidate pass found.
type TendCheckResult struct {
	// Stale is the number of pull requests confirmed behind their base.
	Stale int
	// Confirmed reports that the pass called GitHub.
	Confirmed bool
}

func TendCheck(ctx context.Context, cfg *config.Config, deps Deps, force bool) (TendCheckResult, error)
```

**review: yes** — it is the new decision path.

- [ ] **Step 1: Write the failing tests**

In `internal/loopcmd/tendcheck_test.go`, use the package's existing fake GitHub client and a fake
worktree seam. Count the client's calls:

```go
// The whole point of the gate. A loop whose branches are all current must cost
// no GitHub request at all.
func TestTendCheckMakesNoGitHubCallWhenNothingIsBehind(t *testing.T) {
	gh := &countingGH{}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/x": 0})
	seedPRLink(t, deps, 7, 9, "feat/x", "master")

	got, err := TendCheck(context.Background(), tendCheckConfig(), deps, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Confirmed {
		t.Error("Confirmed = true; the pass must not call GitHub when nothing is behind")
	}
	if gh.calls != 0 {
		t.Errorf("GitHub calls = %d, want 0", gh.calls)
	}
	if got.Stale != 0 {
		t.Errorf("Stale = %d, want 0", got.Stale)
	}
}

// One behind branch buys exactly two calls: the open pull requests and the
// open issues. Not one per pull request.
func TestTendCheckConfirmsWithTwoCallsWhenSomethingIsBehind(t *testing.T) {
	gh := &countingGH{
		prs:    []ghub.PullRequest{{Number: 9, HeadRef: "feat/x", BaseRef: "master"}},
		issues: []ghub.Issue{{Number: 7, Repo: "o/r", Labels: []string{"status:review"}}},
	}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/x": 3})
	seedPRLink(t, deps, 7, 9, "feat/x", "master")

	got, err := TendCheck(context.Background(), tendCheckConfig(), deps, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stale != 1 {
		t.Errorf("Stale = %d, want 1", got.Stale)
	}
	if gh.calls != 2 {
		t.Errorf("GitHub calls = %d, want 2", gh.calls)
	}
}

// A branch the prune removed is a pull request whose branch is gone. It is not
// a candidate, and it is not an error.
func TestTendCheckSkipsARowWhoseBranchIsGone(t *testing.T) {
	gh := &countingGH{}
	deps := tendCheckDeps(t, gh, nil) // BehindLocal reports known=false for everything
	seedPRLink(t, deps, 7, 9, "feat/gone", "master")

	got, err := TendCheck(context.Background(), tendCheckConfig(), deps, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stale != 0 || gh.calls != 0 {
		t.Errorf("Stale = %d, calls = %d; want 0 and 0", got.Stale, gh.calls)
	}
}

// The cold cache. A loop with no rows has nothing to gate on, so a forced pass
// is the only thing that can ever populate it.
func TestTendCheckForcedRunsTheConfirmWithNoRows(t *testing.T) {
	gh := &countingGH{
		prs:    []ghub.PullRequest{{Number: 9, HeadRef: "feat/x", BaseRef: "master"}},
		issues: []ghub.Issue{{Number: 7, Repo: "o/r", Labels: []string{"status:review"}}},
	}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/x": 2})

	got, err := TendCheck(context.Background(), tendCheckConfig(), deps, true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Confirmed {
		t.Error("Confirmed = false; a forced pass must call GitHub")
	}
	if got.Stale != 1 {
		t.Errorf("Stale = %d, want 1", got.Stale)
	}
}

// The drifted cache. A row whose pull request is no longer open is deleted, so
// the gate stops counting a merged branch as behind forever.
func TestTendCheckDeletesARowWhosePullRequestIsClosed(t *testing.T) {
	gh := &countingGH{
		prs:    nil, // nothing open
		issues: nil,
	}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/merged": 4})
	seedPRLink(t, deps, 7, 9, "feat/merged", "master")

	if _, err := TendCheck(context.Background(), tendCheckConfig(), deps, true); err != nil {
		t.Fatal(err)
	}
	links, err := deps.Store.PRLinks("execution", "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := links[7]; ok {
		t.Error("the row for a closed pull request was not deleted")
	}
}

// An issue that lost its review label is not tended, even though its branch is
// behind. The gate never decides on the local cache alone.
func TestTendCheckDropsACandidateWhoseIssueLostTheReviewLabel(t *testing.T) {
	gh := &countingGH{
		prs:    []ghub.PullRequest{{Number: 9, HeadRef: "feat/x", BaseRef: "master"}},
		issues: []ghub.Issue{{Number: 7, Repo: "o/r", Labels: []string{"status:doing"}}},
	}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/x": 3})
	seedPRLink(t, deps, 7, 9, "feat/x", "master")

	got, err := TendCheck(context.Background(), tendCheckConfig(), deps, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stale != 0 {
		t.Errorf("Stale = %d, want 0", got.Stale)
	}
}
```

Write `countingGH`, `tendCheckDeps`, `tendCheckConfig`, and `seedPRLink` in the test file. Model
them on the fakes the package already uses (`fakeGH` in `epicsweep_test.go`). `tendCheckDeps` must
supply a `Fetch` that records it ran and a `Behind` that answers from the map.

**The pull request fixtures above are incomplete as written. Fix them before you start.**
`engine.LinkPR` (`internal/engine/prlink.go:20-39`) links an issue to a pull request only when
BOTH hold:

- `pr.Trusted` is true. `ghub.convertPR` sets it only for a head branch in the SAME repository,
  authored by an OWNER, MEMBER, or COLLABORATOR, with both refs passing `SafeRef`
  (`internal/ghub/ghub.go:107-118`). It is the guard that stops a fork's pull request from
  running an agent — or now, from being force-pushed.
- `pr.Body` carries a closing reference to the issue (`Closes #7` and the other spellings
  `closingRef` matches).

Every fixture must therefore read:

```go
ghub.PullRequest{
	Number: 9, HeadRef: "feat/x", BaseRef: "master",
	Trusted: true, Body: "Closes #7",
}
```

Add one test that proves the trust boundary survives this pass, because it is the property a
force-push makes expensive to get wrong:

```go
// An untrusted pull request -- a fork's branch, or an outside contributor's --
// is not linked, not counted, and never rebased. ghub.convertPR is what sets
// Trusted, and engine.LinkPR is what enforces it; this test is here so a
// future refactor of the confirm step cannot quietly drop the check.
func TestTendCheckIgnoresAnUntrustedPullRequest(t *testing.T) {
	gh := &countingGH{
		prs:    []ghub.PullRequest{{Number: 9, HeadRef: "feat/x", BaseRef: "master", Body: "Closes #7"}},
		issues: []ghub.Issue{{Number: 7, Repo: "o/r", Labels: []string{"status:review"}}},
	}
	deps := tendCheckDeps(t, gh, map[string]int{"feat/x": 3})
	seedPRLink(t, deps, 7, 9, "feat/x", "master")

	got, err := TendCheck(context.Background(), tendCheckConfig(), deps, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stale != 0 {
		t.Errorf("Stale = %d, want 0 for an untrusted pull request", got.Stale)
	}
}
```

Two more tests the task needs, both trivial and both currently missing:

```go
// A loop that does not tend must not fetch, must not read rows, and must not
// call GitHub.
func TestTendCheckDoesNothingWhenTheLoopDoesNotTend(t *testing.T) {
	gh := &countingGH{}
	deps := tendCheckDeps(t, gh, nil)
	cfg := tendCheckConfig()
	cfg.TendPR = false

	got, err := TendCheck(context.Background(), cfg, deps, true)
	if err != nil || got.Confirmed || gh.calls != 0 {
		t.Errorf("got %+v, err %v, calls %d; want a no-op", got, err, gh.calls)
	}
}

// A failed fetch makes every comparison below it stale, so the pass stops
// rather than reporting a branch as current after the base moved.
func TestTendCheckFailsWhenTheFetchFails(t *testing.T) {
	deps := tendCheckDeps(t, &countingGH{}, nil)
	deps.Fetch = func() error { return errors.New("no route to host") }

	if _, err := TendCheck(context.Background(), tendCheckConfig(), deps, false); err == nil {
		t.Error("a failed fetch must fail the pass")
	}
}
```

**Test file placement:** `internal/loopcmd/tendcheck_test.go` is `package loopcmd`, like every
other test in that directory, so refer to `TendCheck` and `TendCheckResult` unqualified.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/loopcmd/ -run TendCheck -v`
Expected: FAIL — `TendCheck` is undefined.

- [ ] **Step 3: Implement `TendCheck`**

Create `internal/loopcmd/tendcheck.go`:

```go
package loopcmd

import (
	"context"
	"log/slog"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
)

// TendCheckResult is what one candidate pass found.
type TendCheckResult struct {
	// Stale is the number of pull requests confirmed behind their base.
	Stale int
	// Confirmed reports that the pass called GitHub. A pass that gated
	// locally and found nothing reports false, and that is the case the
	// caller's log line is worth staying quiet about.
	Confirmed bool
}

// TendCheck answers one question: has any of this loop's pull requests fallen
// behind its base?
//
// It exists so the daemon can ask that question on a timer without paying for
// it. The GitHub equivalent costs two list calls plus one comparison per pull
// request, per loop, per project, on every interval. This reads refs the
// loop's own fetch already updated, so the common case -- nothing is behind --
// costs no request at all.
//
// Three properties are load-bearing. Do not break them.
//
//  1. The local step is a GATE, never a decision. It decides only whether to
//     spend the API calls. A pr_links row can be stale -- a pull request that
//     merged, an issue that lost its review label -- so a dispatch made on the
//     local answer alone would rebase work that is already done.
//  2. Zero GitHub calls when nothing is behind and force is false. This is the
//     whole reason the pass can run every fifteen minutes.
//  3. loopcmd.TendSweep stays the only code that decides what to dispatch.
//     This function reports a count; it dispatches nothing and writes no issue
//     state.
//
// force runs the confirm step whether or not anything looks behind. The caller
// sets it on the first pass after the daemon starts and every few hours after
// that, because the gate can only skip the calls when it has rows to trust: a
// loop with no rows would otherwise stay silent forever, and a row that
// drifted would stay wrong forever.
func TendCheck(ctx context.Context, cfg *config.Config, deps Deps, force bool) (TendCheckResult, error) {
	var out TendCheckResult
	if !cfg.TendPR {
		return out, nil
	}

	// A seam that a hand-built Deps may leave nil. Deps.Fetch is nil-guarded
	// the same way at tendsweep.go:164; without this the daemon panics on the
	// Serve goroutine and takes every project down with it.
	if deps.Behind == nil {
		return out, nil
	}

	// The loop lock, for the same reason every other Fetch in this package is
	// under one. This pass fetches (which moves the refs a concurrent rebase
	// resolves) and deletes pr_links rows (which races PutPRLink), and
	// tendSnapshot's comment records that this package keeps a single writer
	// under the lock deliberately -- see tendsweep.go:92-98.
	//
	// It does NOT wait. A held lock means a tick is already running for this
	// loop, and that tick does this pass's work as part of its own: the sweep
	// it performs is the thing this pass would have armed. Waiting would pin
	// the Serve goroutine behind an agent dispatch.
	l, err := lock.TryAcquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if errors.Is(err, lock.ErrHeld) {
		slog.Info("another tick holds the loop lock; skipping this tend check",
			"loop", cfg.Name)
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer l.Release()

	// Before every comparison below, because each of them reads a remote
	// tracking ref this updates. A stale answer is worse than no answer: it
	// reports a branch as current after the base moved.
	if deps.Fetch != nil {
		if err := deps.Fetch(); err != nil {
			return out, fmt.Errorf("fetch primary checkout: %w", err)
		}
	}

	links, err := deps.Store.PRLinks(cfg.Name, cfg.Repo)
	if err != nil {
		return out, err
	}

	behind := make(map[int]bool, len(links))
	for number, l := range links {
		n, known, err := deps.Behind(l.HeadRef, l.BaseRef)
		if err != nil {
			// One unusable row must not abandon the pass, for the reason
			// tendSnapshot gives: anyone able to open a pull request could
			// otherwise stop every rebase this loop would do.
			slog.Warn("local compare failed; skipping this pull request",
				"loop", cfg.Name, "issue", number, "pr", l.PRNumber, "err", err)
			continue
		}
		// An unknown ref is a branch the prune removed, which is a pull
		// request whose branch is gone. It is not a candidate and not an
		// error.
		if !known || n <= 0 {
			continue
		}
		behind[number] = true
	}

	if len(behind) == 0 && !force {
		return out, nil
	}

	owner, repo := cfg.RepoOwner(), cfg.RepoName()
	prs, err := deps.GH.ListOpenPullRequests(ctx, owner, repo)
	if err != nil {
		return out, err
	}
	issues, err := deps.GH.ListOpenIssues(ctx, owner, repo)
	if err != nil {
		return out, err
	}
	out.Confirmed = true

	open := make(map[int]bool, len(prs))
	for _, pr := range prs {
		open[pr.Number] = true
	}

	// The rows for pull requests that are no longer open. Nothing deleted one
	// before this pass existed, so a database accumulates them -- and the gate
	// above would count a merged branch as behind its base on every interval,
	// forever.
	for number, l := range links {
		if open[l.PRNumber] {
			continue
		}
		if err := deps.Store.DeletePRLink(cfg.Name, cfg.Repo, number); err != nil {
			// Named for the failure, like every other Error line in this
			// package ("decision failed", "retire dead dispatch").
			slog.Error("could not delete a pr link whose pull request is closed",
				"loop", cfg.Name, "issue", number, "pr", l.PRNumber, "err", err)
			continue
		}
		// Info, not Debug: nothing in this program logs at Debug, no handler
		// is configured for it, and a row disappearing from the database is a
		// state change an operator should be able to find afterwards.
		slog.Info("dropped a pr link whose pull request is closed",
			"loop", cfg.Name, "issue", number, "pr", l.PRNumber)
	}

	byNumber := make(map[int]ghub.Issue, len(issues))
	for _, iss := range issues {
		byNumber[iss.Number] = iss
	}

	for number := range behind {
		iss, ok := byNumber[number]
		if !ok || !iss.HasLabel(cfg.Labels.Review) {
			// The issue closed, or lost the review label, since the row was
			// written. Either way this loop no longer tends it.
			continue
		}
		// LinkPR, not a lookup by the row's pr_number, and that is the point.
		// It links only a TRUSTED pull request -- same repository, an owner,
		// member or collaborator, both refs safe (internal/ghub/ghub.go:107-118)
		// -- that names this issue in a closing reference. The stored row
		// carries none of that, so trusting it would let a fork's branch reach
		// the rebase path this feature adds.
		pr, ok := engine.LinkPR(number, prs)
		if !ok || pr.BaseRef != cfg.DefaultBranch || pr.HeadRef != links[number].HeadRef {
			continue
		}
		out.Stale++
	}
	return out, nil
}
```

Imports: `context`, `errors`, `fmt`, `log/slog`, `path/filepath`, plus `config`, `engine`,
`ghub`, and `lock` from this module. Confirm `cfg.RepoOwner()` and `cfg.RepoName()` are the
accessors `tendSnapshot` uses (`internal/loopcmd/tendsweep.go:101`).

**`lock.TryAcquire` may not exist.** Read `internal/lock/` first. `lock.Acquire`
(`internal/loopcmd/tendsweep.go:83`) is what the package uses today, and `lock.ErrHeld` is
already referenced by `internal/listener/work.go`. If the package offers only a blocking
`Acquire`, add a non-blocking variant there, with its own test, as part of this task — a
blocking acquire on the `Serve` goroutine is exactly the stall this pass must not cause.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/loopcmd/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/loopcmd/tendcheck.go internal/loopcmd/tendcheck_test.go internal/loopcmd/tick.go internal/loopcmd/open.go
git commit -m "feat(loopcmd): find a stale pull request with local git before calling github"
```

---

## Task 7: run the check on a ticker

**Files:**
- Modify: `internal/listener/work.go` (the `Worker` type, `NewWorker`, `Serve`)
- Modify: `cmd/agent-utils/listener.go` (pass the setting to the worker)
- Test: `internal/listener/work_test.go`

**Interfaces:**
- Consumes: `loopcmd.TendCheck` (Task 6), `Target.TendPR` (Task 1),
  `settings.Settings.TendEvery()` (Task 5), `Worker.armTend`.
- Produces:
  - `Worker.TendInterval time.Duration` — 0 disables the pass.
  - `Worker.RunTendCheck func(ctx context.Context, cfg *config.Config, deps loopcmd.Deps, force bool) (loopcmd.TendCheckResult, error)` — the seam.
  - `Worker.ScanTargets func() (Routes, error)` — the routing seam for this pass, defaulted to
    the package-level `Scan` in `NewWorker`. **It is required, not optional:** `Scan` reads the
    real registry, and the `Worker`'s existing routing seams are `Targets` and `TargetFor`
    (`internal/listener/work.go:191-192`), neither of which this pass can use. Without a seam
    every test below finds zero loops and asserts nothing.
  - `Worker.tendCheckPass(ctx)` — one pass over every tending loop.
  - `Worker.tendCheckOne(ctx, t Target, acc *access)` — one loop.
  - `Worker.confirms map[loopKey]time.Time` — guarded by `w.mu`.
  - `tendConfirmInterval = 6 * time.Hour`.
  - `tendPassTimeout = 10 * time.Minute` — the whole pass's deadline.

**review: yes** — it adds concurrent work to the daemon.

- [ ] **Step 1: Write the failing tests**

**Helper reality check before you write these.** `internal/listener/work_test.go` builds its
worker with `newHarness(db)` (`work_test.go:162`) and controls time through a `timers` seam
(`work_test.go:49`), not with `newTestWorker`/`fireTendTimer`. Use the real helpers, and set
`w.ScanTargets` to return a fixed `Routes` rather than building a registry on disk. The names
below are written against that: replace `newTestWorker(t)` with the harness the file already has.

```go
// The pass walks the registry, not the deliveries. That is what makes it reach
// a project whose webhook is missing, which is the failure this feature fixes.
func TestTendCheckPassArmsASweepForEachStaleLoop(t *testing.T) {
	w := newTestWorker(t)
	w.TendInterval = time.Minute
	w.ScanTargets = func() (Routes, error) {
		return Routes{Targets: []Target{{
			ProjectID: "p1", ProjectName: "weather", LoopName: "execution",
			Repo: "o/r", ConfigPath: cfgPath, DefaultBranch: "master", TendPR: true,
		}}}, nil
	}
	var checked []string
	w.RunTendCheck = func(_ context.Context, cfg *config.Config, _ loopcmd.Deps, _ bool) (loopcmd.TendCheckResult, error) {
		checked = append(checked, cfg.Name)
		return loopcmd.TendCheckResult{Stale: 1, Confirmed: true}, nil
	}
	var tends int
	w.RunTend = func(context.Context, *config.Config, loopcmd.Deps, string) (loopcmd.Summary, error) {
		tends++
		return loopcmd.Summary{}, nil
	}

	w.tendCheckPass(context.Background())
	fireTendTimer(t, w)

	if len(checked) == 0 {
		t.Fatal("no loop was checked")
	}
	if tends != 1 {
		t.Errorf("tend sweeps = %d, want 1", tends)
	}
}

// A loop that finds nothing arms nothing. A pass that armed a sweep anyway
// would spend the API calls the gate exists to save.
func TestTendCheckPassArmsNothingWhenNothingIsStale(t *testing.T) {
	w := newTestWorker(t)
	w.TendInterval = time.Minute
	w.RunTendCheck = func(context.Context, *config.Config, loopcmd.Deps, bool) (loopcmd.TendCheckResult, error) {
		return loopcmd.TendCheckResult{}, nil
	}
	var tends int
	w.RunTend = func(context.Context, *config.Config, loopcmd.Deps, string) (loopcmd.Summary, error) {
		tends++
		return loopcmd.Summary{}, nil
	}

	w.tendCheckPass(context.Background())

	if tends != 0 {
		t.Errorf("tend sweeps = %d, want 0", tends)
	}
}

// The first pass after start forces the confirm, so a cold pr_links cache is
// populated instead of gating forever on rows that do not exist. The second
// pass does not force it.
func TestTendCheckPassForcesTheFirstPassOnly(t *testing.T) {
	w := newTestWorker(t)
	w.TendInterval = time.Minute
	var forced []bool
	w.RunTendCheck = func(_ context.Context, _ *config.Config, _ loopcmd.Deps, force bool) (loopcmd.TendCheckResult, error) {
		forced = append(forced, force)
		return loopcmd.TendCheckResult{}, nil
	}

	w.tendCheckPass(context.Background())
	w.tendCheckPass(context.Background())

	if len(forced) != 2 || !forced[0] || forced[1] {
		t.Errorf("force flags = %v, want [true false]", forced)
	}
}

// Six hours later the confirm runs again, so a row that drifted with no
// delivery is corrected.
func TestTendCheckPassForcesTheConfirmEverySixHours(t *testing.T) {
	w := newTestWorker(t)
	w.TendInterval = time.Minute
	now := time.Now()
	w.Now = func() time.Time { return now }
	var forced []bool
	w.RunTendCheck = func(_ context.Context, _ *config.Config, _ loopcmd.Deps, force bool) (loopcmd.TendCheckResult, error) {
		forced = append(forced, force)
		return loopcmd.TendCheckResult{}, nil
	}

	w.tendCheckPass(context.Background())
	now = now.Add(7 * time.Hour)
	w.tendCheckPass(context.Background())

	if len(forced) != 2 || !forced[1] {
		t.Errorf("force flags = %v, want the second pass forced", forced)
	}
}

// Zero disables the pass outright.
//
// The cancelled-context version of this test proves nothing: Serve returns at
// the ctx.Err() guard (work.go:1412-1415) before it reaches the select, so it
// passes identically with a non-zero interval. Assert on the ticker channel
// the constructor builds instead.
func TestServeBuildsNoTendTickerWhenTheIntervalIsZero(t *testing.T) {
	w := newTestWorker(t)
	w.TendInterval = 0
	if ch := w.tendTicker(); ch != nil {
		t.Error("a zero interval must build no ticker; a nil channel blocks forever, which is the disable")
	}

	w.TendInterval = time.Minute
	if ch := w.tendTicker(); ch == nil {
		t.Error("a non-zero interval must build a ticker")
	}
}
```

Split the ticker construction into a small `tendTicker()` method so `Serve`'s wiring is testable
at all. `Serve` itself stays untested — it is a loop around seams — but the branch that decides
whether the pass can ever run must not be.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/listener/ -run TendCheck -v`
Expected: FAIL — `tendCheckPass` is undefined.

- [ ] **Step 3: Add the fields and the seam**

In the `Worker` type:

```go
	// RunTendCheck answers "is any pull request of this loop behind its base",
	// as cheaply as it can. Production wires it to loopcmd.TendCheck. It is a
	// seam because the real one reads local git refs and a database, and the
	// scheduling this file owns is what the tests here are about.
	RunTendCheck func(ctx context.Context, cfg *config.Config, deps loopcmd.Deps, force bool) (loopcmd.TendCheckResult, error)
	// TendInterval is how often tendCheckPass runs. A value of 0 disables it,
	// which leaves the merge and push triggers untouched. It comes from the
	// machine-wide settings file.
	TendInterval time.Duration
	// confirms holds, per loop, when the pass last called GitHub. It is
	// memory only: a restart costs one forced pass per loop, which is two API
	// calls, and that is cheaper than the column and the migration a durable
	// version would need.
	confirms map[loopKey]time.Time // guarded by mu
```

Initialise `confirms` in `NewWorker` and set `RunTendCheck: loopcmd.TendCheck`.

- [ ] **Step 4: Implement `tendCheckPass`**

```go
// tendCheckPass looks for a pull request that has fallen behind its base, in
// every loop on this machine that tends.
//
// It walks listener.Scan -- the REGISTRY -- rather than reacting to a
// delivery. That is the whole point: a repository whose webhook is missing,
// broken, or simply quiet receives no delivery, and before this pass its stale
// pull requests waited for a merge that might never come.
//
// It dispatches nothing itself. A loop with a stale pull request gets an
// armTend, so the periodic trigger and the merge trigger end in the same
// sweep, on the same timer, holding the same loop lock.
func (w *Worker) tendCheckPass(ctx context.Context) {
	// A deadline on the WHOLE pass. This runs on the Serve goroutine, which is
	// the single wake loop for every project on this machine: it is what fires
	// retry deadlines. The pass shells out to git, and git talks to a network
	// it does not control, so an unreachable remote with no deadline here
	// stops every retry, for every loop, until the daemon is restarted.
	ctx, cancel := context.WithTimeout(ctx, tendPassTimeout)
	defer cancel()

	routes, err := w.ScanTargets()
	if err != nil {
		slog.Error("cannot scan projects for the tend check", "err", err)
		return
	}
	acc, err := w.access()
	if err != nil {
		slog.Error("cannot read the github token for the tend check", "err", err)
		return
	}

	for _, t := range routes.Targets {
		if !t.TendPR {
			continue
		}
		// Checked between loops, not only at the top: the deadline above and a
		// daemon shutdown both land here, and a pass that ignored them would
		// keep opening databases through the stop.
		if ctx.Err() != nil {
			return
		}
		w.tendCheckOne(ctx, t, acc)
	}
}

// tendCheckOne runs the check for one loop and arms a sweep when it finds
// something.
func (w *Worker) tendCheckOne(ctx context.Context, t Target, acc *access) {
	cfg, deps, cleanup, err := w.Open(t.Ref(), t.ConfigPath, loopcmd.Options{
		Token: acc.token, GH: acc.gh, RequireGitHub: true,
		MigrationPolicy: loopcmd.FailOnUnimported,
	})
	// Deferred BEFORE any branch below can return. Open holds a SQLite handle,
	// and this pass calls Open once per loop per interval for the life of the
	// daemon, so a single missed cleanup is not a leak that stops -- it is one
	// handle every fifteen minutes, forever. tickOne records the same rule for
	// the delivery path (work.go:548-553). The nil check is for the error
	// path, where Open returns a nil cleanup.
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		// Logged and dropped, with NO retry scheduled. The retry schedule is
		// keyed per loop and spends an issue's budget; this pass names no
		// issue, and the next interval re-runs it in full.
		slog.Error("cannot open loop for the tend check", "loop", t.LoopName,
			"project", t.ProjectName, "config", t.ConfigPath, "err", err)
		return
	}

	key := loopKey{ProjectID: t.ProjectID, LoopName: t.LoopName}
	w.mu.Lock()
	last, seen := w.confirms[key]
	w.mu.Unlock()
	force := !seen || w.Now().Sub(last) >= tendConfirmInterval

	res, err := w.RunTendCheck(ctx, cfg, deps, force)
	if err != nil {
		// Logged and dropped, for the reason tendPass gives: this pass decides
		// no issue, so there is no issue whose retry budget could pay for it,
		// and the next interval tries again.
		slog.Error("tend check failed", "loop", cfg.Name, "project", t.ProjectName, "err", err)
		return
	}
	if res.Confirmed {
		w.mu.Lock()
		w.confirms[key] = w.Now()
		w.mu.Unlock()
	}
	if res.Stale == 0 {
		// Deliberately silent. This is the common case, once every interval,
		// for every loop on the machine; a line here would bury every line
		// that means something.
		return
	}
	slog.Info("the tend check found a pull request behind its base",
		"loop", cfg.Name, "project", t.ProjectName, "stale", res.Stale)
	w.armTend(ctx, t, cfg.DefaultBranch)
}
```

Add the constants:

```go
// tendConfirmInterval is how often the check calls GitHub even though nothing
// looks behind. It is what corrects a pr_links row that drifted with no
// delivery: a pull request that closed, or an issue that lost its review
// label, leaves a row the local gate would otherwise trust forever.
const tendConfirmInterval = 6 * time.Hour

// tendPassTimeout bounds one whole tend check across every loop on the
// machine. See tendCheckPass: the pass runs on the single wake loop, and it
// shells out to a network git cannot be trusted to return from.
const tendPassTimeout = 10 * time.Minute
```

**`armTend` is called with the pass's context**, which carries `tendPassTimeout`. Check what
`armTend` does with it (`internal/listener/work.go:855-899`): the timer's callback tests
`ctx.Err()` before running the sweep, so a context that expires with the pass would cancel the
sweep it just armed. Pass the **daemon's** context to `armTend`, not the timeout-bounded one —
keep both in scope in `tendCheckPass` and hand the right one to each callee. This is a real bug
if you miss it, and no test above catches it; add one that arms a sweep and then lets the pass
context expire before the timer fires.

- [ ] **Step 5: Add the ticker to `Serve`**

```go
	// tendTicker returns the channel the periodic tend check fires on, or nil
	// when it is disabled. A nil channel blocks forever in a select, which is
	// exactly what "disabled" means here -- no branch, no flag, nothing for a
	// later reader to get subtly wrong.
	func (w *Worker) tendTicker() <-chan time.Time {
		if w.TendInterval <= 0 {
			return nil
		}
		return time.NewTicker(w.TendInterval).C
	}
```

In `Serve`, build it once before the loop (keeping the `*time.Ticker` so it can be stopped on
return), and add `case <-tendC:` to the `select`, which runs `w.tendCheckPass(ctx)` and
continues.

Add `case <-tendC:` to the `select`, running `w.tendCheckPass(ctx)` and then continuing the loop.
A nil channel blocks forever, which is exactly what a disabled pass needs. Do **not** fold this
into `Wake`: `Wake` is driven by retry deadlines and floored at `MinWakeInterval`.

- [ ] **Step 6: Wire the setting**

In `cmd/agent-utils/listener.go`, where the worker is built, set
`w.TendInterval = s.TendEvery()` from the loaded settings. Log the
resolved interval in the startup banner, beside the routing table, so an operator can see whether
the pass is on.

- [ ] **Step 7: Run the tests and confirm they pass**

Run: `go test ./internal/listener/ ./cmd/...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/listener/work.go internal/listener/work_test.go cmd/agent-utils/listener.go
git commit -m "feat(listener): run the tend check on a ticker"
```

---

## Task 8: git primitives for the rebase

The rebase runs in the daemon, so every git command needs a deadline. A hung `git push` would
hold the loop lock and stall the ticker.

**Files:**
- Modify: `internal/worktree/worktree.go`
- Test: `internal/worktree/worktree_test.go`

**Interfaces:**
- Consumes: `Manager.EnsurePR`, `Manager.Dirty`.
- Produces:

```go
func (m *Manager) EnsurePRCtx(ctx context.Context, number int, headRef string) (string, error)
func (m *Manager) DirtyCtx(ctx context.Context, path string) (bool, error)
func (m *Manager) HeadSHA(ctx context.Context, path string) (string, error)
func (m *Manager) Rebase(ctx context.Context, path, baseRef string) error
func (m *Manager) AbortRebase(ctx context.Context, path string) error
func (m *Manager) PushWithLease(ctx context.Context, path, headRef, lease string) error
func (m *Manager) RemoveCtx(ctx context.Context, path string) error
```

**`EnsurePRCtx` and `DirtyCtx` are not optional additions.** The first draft of this plan reused
the existing `EnsurePR` and `Dirty`, which run through `m.git` → `exec.Command` with **no
deadline** (`internal/worktree/worktree.go:81, 166-179`). `EnsurePR` runs `git fetch origin
<headRef>` — the command most likely to block on a network stall — and it runs first on the
rebase path, while `TendSweep` holds the loop lock. Bounding the push and the rebase while
leaving the fetch unbounded defeats the whole reason this task exists.

Implement each as the context-taking body, and leave the existing method as a thin wrapper that
calls it with `context.Background()`, so every current caller keeps compiling and behaving
exactly as before.

**review: yes** — `PushWithLease` rewrites a remote branch.

- [ ] **Step 1: Write the failing tests**

`internal/worktree/worktree_test.go` has `initRepo` (`worktree_test.go:10`), which already
creates a bare origin — model the new fixtures on it. `newTestManagerWithRemote`,
`newTestManagerWithConflict`, `gitOut`, and `commitOnBranch` do **not** exist and are part of
this task's work; the conflict fixture (two branches editing the same line) is the fiddly one.
Add `context`, `time`, and `regexp` to the file's imports — none of the three is there today
(`worktree.go:3-10`).

Build a real repository with an `origin` remote in a temp directory:

```go
// A clean replay pushes, and the remote then holds the rebased commit.
func TestRebaseAndPushWithLease(t *testing.T) {
	m, origin := newTestManagerWithRemote(t)
	path, err := m.EnsurePR(9, "feature")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := m.HeadSHA(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Rebase(context.Background(), path, "master"); err != nil {
		t.Fatal(err)
	}
	if err := m.PushWithLease(context.Background(), path, "feature", lease); err != nil {
		t.Fatal(err)
	}
	if gitOut(t, origin, "rev-parse", "feature") == lease {
		t.Error("the remote branch did not move")
	}
}

// The lease is the guard. A remote that moved after the fetch refuses the
// push, and the branch keeps the other writer's commit.
func TestPushWithLeaseRefusesWhenTheRemoteMoved(t *testing.T) {
	m, origin := newTestManagerWithRemote(t)
	path, _ := m.EnsurePR(9, "feature")
	lease, _ := m.HeadSHA(context.Background(), path)
	if err := m.Rebase(context.Background(), path, "master"); err != nil {
		t.Fatal(err)
	}

	// Somebody else pushes to the branch while this pass runs.
	commitOnBranch(t, origin, "feature", "someone else's work")
	before := gitOut(t, origin, "rev-parse", "feature")

	err := m.PushWithLease(context.Background(), path, "feature", lease)
	if err == nil {
		t.Fatal("the push must be refused when the remote moved")
	}
	if got := gitOut(t, origin, "rev-parse", "feature"); got != before {
		t.Error("the refused push still changed the branch")
	}
}

// A conflict leaves no rebase in progress. A worktree stuck mid-rebase would
// fail every later pass for that pull request.
func TestAbortRebaseLeavesACleanWorktree(t *testing.T) {
	m, _ := newTestManagerWithConflict(t)
	path, _ := m.EnsurePR(9, "feature")
	if err := m.Rebase(context.Background(), path, "master"); err == nil {
		t.Fatal("this fixture must conflict")
	}
	if err := m.AbortRebase(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if out := gitOut(t, path, "status", "--porcelain"); out != "" {
		t.Errorf("worktree is not clean after the abort: %q", out)
	}
}

// A deadline that has passed must stop the command rather than hang the
// daemon's loop lock.
func TestRebaseRespectsTheContext(t *testing.T) {
	m, _ := newTestManagerWithRemote(t)
	path, _ := m.EnsurePR(9, "feature")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Rebase(ctx, path, "master"); err == nil {
		t.Error("a cancelled context must fail the rebase")
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/worktree/ -run 'Rebase|Lease' -v`
Expected: FAIL — the four methods do not exist.

- [ ] **Step 3: Implement the primitives**

Add a context-aware runner beside `gitOutput`:

```go
// gitCtx runs one git command with a deadline.
//
// The existing git helpers have none, which is correct for a command-line
// tick: a human sees it hang and stops it. The daemon has no such reader. A
// hung push holds the loop lock and stalls the tend ticker for every project
// on the machine, so every command on the automatic rebase path is bounded.
func (m *Manager) gitCtx(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %w: %s",
			strings.Join(args, " "), dir, err, redact(strings.TrimSpace(string(out))))
	}
	return string(out), nil
}

// gitTimeout bounds one git command on the automatic rebase path.
const gitTimeout = 2 * time.Minute
```

Then:

- `HeadSHA` runs `rev-parse HEAD` and returns the trimmed object id. **It must read stdout
  only** — `cmd.Output()`, not the `CombinedOutput` every other helper here uses. `gitCtx`
  returns stdout and stderr together, so any advice, hook, or fsmonitor warning git prints on
  success is prepended to the id. A polluted lease makes every push fail forever; the failure is
  silent, because `gitRebase` treats a refused push as "leave this branch alone". Give `HeadSHA`
  its own runner, or have `gitCtx` take a flag.
- `Rebase` checks `SafeRef(baseRef)` and runs `rebase origin/<baseRef>`.
- `AbortRebase` runs `rebase --abort`. It returns nil when there is no rebase in progress; git
  exits non-zero in that case, and the caller aborts unconditionally.
- `RemoveCtx` is `Remove` with a deadline. It is what `gitRebase` uses when an abort fails, so a
  worktree left mid-rebase is destroyed rather than handed to an agent.
- `PushWithLease` checks `SafeRef(headRef)`, **validates the lease**, and runs
  `push --force-with-lease=<headRef>:<lease> origin HEAD:refs/heads/<headRef>`.

  The lease validation is the one guard the whole feature rests on, so it is checked here rather
  than trusted from the caller:

```go
// leaseSHA matches a full git object id, in either hash size. A short or
// abbreviated id is refused: --force-with-lease with an EMPTY value silently
// degrades to remote-tracking semantics, and a detached tend worktree has no
// useful remote-tracking ref -- EnsurePR fetches into FETCH_HEAD, not
// refs/remotes/origin/<head> (worktree.go:81-88). That degraded form is a
// materially weaker guard than the fetched id, and it would arrive looking
// exactly like the strong one.
var leaseSHA = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)
```

  `PushWithLease` returns an error, and pushes nothing, when the lease fails that pattern. Add a
  test for the empty lease specifically — it is the case that degrades rather than fails.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/worktree/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/worktree/worktree.go internal/worktree/worktree_test.go
git commit -m "feat(worktree): bounded rebase, abort, and leased force-push"
```

---

## Task 9: try git before dispatching a tend agent

**Files:**
- Create: `internal/loopcmd/rebase.go`
- Create: `internal/loopcmd/rebase_test.go`
- Modify: `internal/loopcmd/tick.go:91-105` (`Summary`), `tick.go:379` (`act`)
- Modify: `internal/loopcmd/open.go:205-216` — **wire `Deps.Git`**. Without this the shipped
  binary has `Deps.Git == nil`, which `gitRebase` treats as "disabled". The feature would be
  dead on arrival and every test would still pass.
- Modify: `internal/loopcmd/logs.go:60-99` (`SelectDispatch`) — see Step 6.
- Modify: `internal/store/types.go:5-10` (the kinds)
- Modify: `internal/store/store.go` — add `RecordFinishedDispatch` (Step 4)

**Interfaces:**
- Consumes: the Task 8 primitives, `engine.Decision`, `Deps`.
- Produces:
  - `store.KindRebase = "rebase"`
  - `Summary.Rebased int` (`json:"rebased"`)
  - `func gitRebase(ctx context.Context, cfg *config.Config, deps Deps, d engine.Decision) (done bool, err error)`
  - `loopcmd.RebaseGit` — the interface `Deps.Git` holds:

```go
// RebaseGit is the git the automatic rebase needs, and nothing else.
//
// It is an interface, not the concrete *worktree.Manager, for one reason: the
// rebase path is the only code in this program that force-pushes, and a test
// of its branching must be able to make a push fail without a remote to fail
// against. *worktree.Manager satisfies it, and Open wires it in.
type RebaseGit interface {
	PathForPR(number int) string
	EnsurePRCtx(ctx context.Context, number int, headRef string) (string, error)
	DirtyCtx(ctx context.Context, path string) (bool, error)
	HeadSHA(ctx context.Context, path string) (string, error)
	Rebase(ctx context.Context, path, baseRef string) error
	AbortRebase(ctx context.Context, path string) error
	RemoveCtx(ctx context.Context, path string) error
	PushWithLease(ctx context.Context, path, headRef, lease string) error
}
```

  Add `Git RebaseGit` to `Deps`, wired to `deps.WT` in `loopcmd.Open`. `gitRebase` reads
  `deps.Git`, never `deps.WT`, so the fake in the tests above is the whole of what it touches.
  A nil `Deps.Git` disables the automatic rebase and falls through to the agent, which is what
  keeps every existing caller that builds a `Deps` by hand working unchanged.

**review: yes** — it force-pushes without a human.

- [ ] **Step 1: Write the failing tests**

```go
// The point of the feature: a clean replay costs no agent and no tokens.
func TestGitRebaseCleanReplayDispatchesNoAgent(t *testing.T) {
	deps, git := rebaseDeps(t) // git is a fake recording the calls
	git.rebaseErr = nil
	var sum Summary

	if err := act(context.Background(), rebaseConfig(), deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Rebased != 1 {
		t.Errorf("Rebased = %d, want 1", sum.Rebased)
	}
	if sum.Tended != 0 {
		t.Errorf("Tended = %d, want 0: no agent may be dispatched for a clean replay", sum.Tended)
	}
	if git.pushes != 1 {
		t.Errorf("pushes = %d, want 1", git.pushes)
	}
}

// A conflict is what an agent is for. The abort must run first, so the next
// pass does not meet a worktree stuck mid-rebase.
func TestGitRebaseConflictAbortsAndDispatchesTheAgent(t *testing.T) {
	deps, git := rebaseDeps(t)
	git.rebaseErr = errors.New("CONFLICT (content): Merge conflict in a.go")
	var sum Summary

	if err := act(context.Background(), rebaseConfig(), deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if git.aborts != 1 {
		t.Errorf("aborts = %d, want 1", git.aborts)
	}
	if git.pushes != 0 {
		t.Errorf("pushes = %d, want 0 after a conflict", git.pushes)
	}
	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1: a conflict escalates to the agent", sum.Tended)
	}
	if sum.Rebased != 0 {
		t.Errorf("Rebased = %d, want 0", sum.Rebased)
	}
}

// A refused push means somebody else moved the branch while this pass ran.
// Sending an agent at a branch under active work is the more dangerous answer,
// so this pass does nothing and lets the next one read the new state.
func TestGitRebaseRefusedPushDispatchesNoAgent(t *testing.T) {
	deps, git := rebaseDeps(t)
	git.pushErr = errors.New("stale info")
	var sum Summary

	if err := act(context.Background(), rebaseConfig(), deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.Tended != 0 || sum.Rebased != 0 {
		t.Errorf("Tended = %d, Rebased = %d; want 0 and 0", sum.Tended, sum.Rebased)
	}
}

// A dirty worktree holds work a rebase would destroy. The agent is the right
// actor for it.
func TestGitRebaseDirtyWorktreeDispatchesTheAgent(t *testing.T) {
	deps, git := rebaseDeps(t)
	git.dirty = true
	var sum Summary

	if err := act(context.Background(), rebaseConfig(), deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if git.rebases != 0 {
		t.Errorf("rebases = %d, want 0 in a dirty worktree", git.rebases)
	}
	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1", sum.Tended)
	}
}

// A loop with worktree: none has no pull-request worktree to rebase in.
func TestGitRebaseWithoutAPerIssueWorktreeDispatchesTheAgent(t *testing.T) {
	deps, git := rebaseDeps(t)
	cfg := rebaseConfig()
	cfg.Agent.Worktree = config.WorktreeNone
	var sum Summary

	if err := act(context.Background(), cfg, deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}
	if git.rebases != 0 {
		t.Errorf("rebases = %d, want 0", git.rebases)
	}
	if sum.Tended != 1 {
		t.Errorf("Tended = %d, want 1", sum.Tended)
	}
}

// The record. A force-push with no local cause is what an operator would find
// otherwise. The row must NOT appear as a session: it has no conversation.
func TestACleanRebaseWritesADispatchRowWithNoSession(t *testing.T) {
	deps, _ := rebaseDeps(t)
	var sum Summary
	if err := act(context.Background(), rebaseConfig(), deps, tendDecision(), time.Now(), &sum); err != nil {
		t.Fatal(err)
	}

	ds, err := deps.Store.RecentDispatches("execution", "o/r", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(ds))
	}
	if ds[0].Kind != store.KindRebase {
		t.Errorf("Kind = %q, want %q", ds[0].Kind, store.KindRebase)
	}
	if ds[0].SessionID != "" {
		t.Errorf("SessionID = %q, want empty", ds[0].SessionID)
	}
	if ds[0].Status != store.StatusSucceeded {
		t.Errorf("Status = %q, want succeeded", ds[0].Status)
	}
	if got := sessionsFrom(ds, ""); len(got) != 0 {
		t.Errorf("sessions = %d, want 0: a rebase is not a session", len(got))
	}
}
```

The fake the tests above call `git` implements `RebaseGit` (see Interfaces). It records
`rebases`, `aborts`, and `pushes`, and returns `rebaseErr`, `pushErr`, and `dirty` from its
fields. `rebaseDeps` builds a `Deps` whose `Git` is that fake and whose `Store` is a real
temporary database, so the record assertions read what was actually written.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/loopcmd/ -run Rebase -v`
Expected: FAIL — `store.KindRebase` and `Summary.Rebased` do not exist.

- [ ] **Step 3: Add the kind and the counter**

In `internal/store/types.go`:

```go
	// KindRebase is a rebase git performed with no agent. The row carries no
	// session identifier, because there is no conversation: it exists so a
	// force-push has a cause an operator can read in `project logs --list`.
	// sessionsFrom skips a dispatch with no session, so a rebase never appears
	// in `sessions list` and never distorts a session's cost.
	KindRebase = "rebase"
```

In `Summary`, beside `Tended`:

```go
	// Rebased counts the pull requests git replayed with no agent. It is
	// separate from Tended so a sweep's line says which of the two happened:
	// how many rebases cost nothing, and how many needed an agent.
	Rebased int `json:"rebased"`
```

- [ ] **Step 4: Implement `gitRebase` and the git path**

Create `internal/loopcmd/rebase.go`:

```go
package loopcmd

import (
	"context"
	"log/slog"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/engine"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// gitRebase replays a pull request's branch on its base with git alone, and
// reports whether it settled the decision.
//
// A tend agent exists for the rebases that need judgment. Most do not: the
// base moved, the branch replays cleanly, and the result is a force-push that
// no conversation improves. This function does that case for nothing, and
// hands the rest to the agent unchanged.
//
// done reports that the caller must NOT dispatch an agent. It is true in two
// cases, and the second is the one worth reading twice:
//
//   - the rebase replayed and the push landed;
//   - the push was REFUSED because the remote moved. The branch this pass
//     reasoned about is gone, so an agent sent at it now would work from the
//     same stale premise. The next tick reads the new state and decides again.
//
// # Guards
//
// Two, and only two:
//
//  1. --force-with-lease, pinned to the commit this pass fetched. Git refuses
//     the push when the remote moved, so a commit somebody else pushed is
//     never overwritten. This is enforced by git, not by this program.
//  2. No live dispatch for the issue or the pull request. engine.Decide
//     already suppresses a tend decision while an agent works that issue, so a
//     decision reaching this function has passed it. A rebase under a running
//     agent is the same hazard as two agents.
//
// Two more were considered and deliberately REJECTED by the operator. Do not
// add them back without asking:
//
//   - Commit authorship is not inspected. A branch carrying a human's commits
//     is rebased like any other, because the lease already refuses the push
//     that would lose work.
//   - A veto label, a stopped session, and a parked issue do NOT stop a clean
//     replay. A rebase spends no token and writes no label, so a paused issue
//     still gets a current branch. Those refusals continue to stop every AGENT
//     dispatch, including this function's own escalation, because Decide
//     applies them before act runs.
// rebaseOutcome is what gitRebase settled, and it is three values rather than
// a bool because two different outcomes both mean "dispatch no agent" while
// only one of them rebased anything. A bool made the caller count a REFUSED
// push as a completed rebase in the tick summary.
type rebaseOutcome int

const (
	// notDone: the caller must dispatch the tend agent.
	notDone rebaseOutcome = iota
	// doneRebased: git replayed the branch and pushed it.
	doneRebased
	// doneNoRebase: this pass settled the decision by declining to act. No
	// agent, and nothing to count.
	doneNoRebase
)

func gitRebase(ctx context.Context, cfg *config.Config, deps Deps, d engine.Decision) (rebaseOutcome, error) {
	// A loop with no per-issue worktree has no pull-request checkout to rebase
	// in, and this pass will not create one: the agent path already handles
	// that mode.
	if cfg.Agent.Worktree != config.WorktreePerIssue || deps.Git == nil {
		return notDone, nil
	}

	// The dirty check comes FIRST, before the worktree is refreshed, and that
	// ordering is the whole guard. EnsurePR runs `checkout --detach
	// FETCH_HEAD` on an existing worktree (worktree.go:81-88), which orphans
	// any local commits an agent left behind -- so a check made afterwards
	// asks about a tree the refresh already flattened, and Dirty's
	// ahead-of-upstream test returns false for a detached worktree by
	// construction (worktree.go:152-160). Checking the stale path before the
	// refresh is what lets a crashed agent's unpushed work stop this pass.
	dirty, err := deps.Git.DirtyCtx(ctx, deps.Git.PathForPR(d.PR))
	if err != nil {
		return notDone, err
	}
	if dirty {
		slog.Info("tend worktree holds uncommitted or unpushed work; leaving this rebase to the agent",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR)
		return notDone, nil
	}

	path, err := deps.Git.EnsurePRCtx(ctx, d.PR, d.HeadRef)
	if err != nil {
		return notDone, err
	}

	// The lease. It is read AFTER EnsurePRCtx, which fetches the head ref and
	// checks it out, so it is the commit the remote had a moment ago -- which
	// is exactly what the push must be pinned to. PushWithLease refuses an id
	// that is not a full object hash, so a polluted read fails loudly here
	// rather than silently degrading the guard.
	lease, err := deps.Git.HeadSHA(ctx, path)
	if err != nil {
		return notDone, err
	}

	if err := deps.Git.Rebase(ctx, path, d.BaseRef); err != nil {
		// Unconditional, and its own error is logged rather than returned: a
		// worktree left mid-rebase fails every later pass for this pull
		// request, and the rebase failure below is the one worth reporting.
		if abortErr := deps.Git.AbortRebase(ctx, path); abortErr != nil {
			// The abort itself failed, so the worktree still holds
			// .git/rebase-merge. This is reachable, not theoretical: gitCtx
			// uses exec.CommandContext, which KILLS the rebase at the deadline
			// or at shutdown, and the abort then inherits the same dead
			// context and fails immediately. Worktrees are stable across ticks
			// (worktree.go:75-101), so the broken state would persist -- and
			// an agent started in it can force-push a half-replayed tree.
			//
			// Destroy it and dispatch nobody. The next pass builds it fresh.
			slog.Error("could not abort a failed rebase; removing the worktree and dispatching nothing",
				"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", abortErr)
			if rmErr := deps.Git.RemoveCtx(ctx, path); rmErr != nil {
				slog.Error("could not remove a worktree left mid-rebase",
					"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", rmErr)
			}
			return doneNoRebase, nil
		}
		slog.Info("rebase did not replay cleanly; dispatching the tend agent",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", err)
		return notDone, nil
	}

	if err := deps.Git.PushWithLease(ctx, path, d.HeadRef, lease); err != nil {
		// The lease did its job, or the remote is unreachable. Either way this
		// pass acts no further and dispatches no agent; see the doc comment.
		//
		// safeText, because the branch name reaches this line from a webhook
		// payload by way of a database row, and SafeRef bounds its CHARSET but
		// not its LENGTH. handler.go:185-196 records the unrotated-log-file
		// failure that requires it.
		slog.Warn("force-with-lease push refused; leaving this branch alone",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR,
			"head", safeText(d.HeadRef), "err", err)
		// done, but NOT rebased. The caller must not count this: the summary
		// is the surface an operator audits unattended force-pushes with, and
		// nothing was pushed.
		return doneNoRebase, nil
	}

	if err := recordRebase(cfg, deps, d); err != nil {
		// The rebase HAPPENED. A failed record must not report it as
		// undone, or the next pass would rebase an already-current branch.
		slog.Error("could not record an automatic rebase", "loop", cfg.Name,
			"issue", d.Issue, "pr", d.PR, "err", err)
	}
	slog.Info("rebased a pull request with git; no agent was dispatched",
		"loop", cfg.Name, "issue", d.Issue, "pr", d.PR,
		"head", safeText(d.HeadRef), "base", safeText(d.BaseRef))
	return doneRebased, nil
}

// recordRebase writes the row that gives a force-push a cause.
//
// Without it the only evidence is a force-push in the pull request's timeline
// and a log line in a daemon an operator may not be watching.
//
// The session identifier is empty on purpose. There is no conversation.
// sessionsFrom skips a dispatch with no session (internal/loopcmd/sessions.go),
// so a rebase never appears in `sessions list` and never distorts a session's
// run count or cost, while `project logs --list` shows it like any other
// dispatch.
func recordRebase(cfg *config.Config, deps Deps, d engine.Decision) error {
	return deps.Store.RecordFinishedDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: d.Issue, Kind: store.KindRebase,
		PRNumber: d.PR, Title: d.Title,
	})
}
```

**Why a new store method rather than `CreateDispatch` + `FinishDispatch`.** `CreateDispatch`
always inserts `status = running` (`internal/store/store.go:904-916`). A `rebase` row left
running is not inert -- three separate places branch on "tend or not", and a rebase row falls on
the wrong side of all three:

- `internal/engine/engine.go:29-45` puts every running row whose `Kind != store.KindTend` into
  `liveIssues`, which suppresses **every** decision for that issue: start, resume, retry and
  tend. The issue would be frozen, with no reaper able to free it.
- `internal/loopcmd/tick.go:281` guards `MarkNeedsRetry` with the same `!= KindTend` test, so the
  full `Tick` would reap the orphan and stamp a retry deadline and backoff on an issue whose
  agent never ran.
- `internal/loopcmd/tendsweep.go:196-215` partitions running rows into "tend rows, which are
  reaped" and "everything else, treated as live" -- and says so in as many words.

The first draft of this plan created the row running and then logged-and-continued when the
finish failed, which produces exactly that stuck row. One INSERT that is already finished when
it lands makes the bad state unreachable, which is better than teaching three guards about a
fourth kind:

```go
// RecordFinishedDispatch inserts one dispatch row that is already complete.
//
// Every other dispatch row is born running and finished later, because a
// process backs it. This one has none: git did the work synchronously, and it
// is over before the row exists. Two statements would leave a window -- and a
// PERMANENT stuck row if the second failed -- in which the row reads as a live
// agent to engine.Decide (engine.go:29-45), to reapDead (tick.go:281), and to
// tendDispatch's reap partition (tendsweep.go:196-215), none of which can reap
// a kind they do not know about.
func (s *Store) RecordFinishedDispatch(d Dispatch) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`
		INSERT INTO dispatches (project_id, loop, repo, number, kind, session_id,
		                        status, started_at, finished_at, exit_code,
		                        cost_usd, duration_ms, log_path, pr_number, title,
		                        model, harness, effort)
		VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, 0, 0, 0, '', ?, ?, '', '', '')`,
		s.projectID, d.Loop, d.Repo, d.Number, d.Kind,
		StatusSucceeded, now, now, d.PRNumber, d.Title)
	if err != nil {
		return fmt.Errorf("record finished dispatch: %w", err)
	}
	return nil
}
```

Check the column names against `dispatchColumns` and the existing `CreateDispatch` INSERT before
writing this, and add a store test asserting the row lands with `status = succeeded`, an empty
session, and a non-zero `finished_at`.

- [ ] **Step 5: Intercept in `act`**

Replace `case engine.KindTend:` at `tick.go:379`:

```go
	case engine.KindTend:
		// git first, the agent second. A rebase that replays cleanly needs no
		// conversation, and this is the common case: the agent exists for the
		// conflicts. gitRebase reports whether it settled the decision --
		// including the case where it settled it by declining to act, which is
		// what a refused lease means.
		switch outcome, err := gitRebase(ctx, cfg, deps, d); {
		case err != nil:
			// Logged, not returned: a git failure must not abandon the rest of
			// the sweep, and the agent is the fallback this whole path is
			// built around.
			slog.Warn("automatic rebase failed; falling back to the tend agent",
				"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", err)
		case outcome == doneRebased:
			sum.Rebased++
			return nil
		case outcome == doneNoRebase:
			// Settled by declining to act. No agent, and nothing counted.
			return nil
		}
		return count(&sum.Tended, dispatch(ctx, cfg, deps, d, now, store.KindTend))
```

- [ ] **Step 6: Keep `project logs` working**

`SelectDispatch` with no `--dispatch`, `--session` or `--issue` returns the NEWEST dispatch
(`internal/loopcmd/logs.go:60-85`). After a clean rebase that is the `rebase` row, whose
`LogPath` is empty — so a bare `agent-utils project logs` would fail with "no log at  yet".

A rebase row has no log because no process wrote one. Skip it when selecting a default:

```go
	// A rebase row is a record, not a run: git did the work in this process
	// and wrote no transcript. Selecting it as "the newest dispatch" would
	// answer `project logs` with an empty path. It is still listed by --list,
	// and still selectable by --dispatch, which is where an operator who wants
	// to see it looks.
```

Write the test with the rebase row newest and an agent dispatch behind it, and assert the agent's
log is the one selected. `--list` must still show both.

- [ ] **Step 7: Run the tests and confirm they pass**

Run: `go test ./internal/loopcmd/ ./internal/store/`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/loopcmd/rebase.go internal/loopcmd/rebase_test.go internal/loopcmd/tick.go internal/loopcmd/open.go internal/loopcmd/logs.go internal/store/types.go internal/store/store.go
git commit -m "feat(loopcmd): rebase with git and escalate to an agent only on a conflict"
```

---

## Task 10: documentation

The stale-doc set is larger than "the README's webhook section". Each item below is a sentence
that this change makes false, found by reading rather than guessing.

**Files:**
- Modify: `internal/loopcmd/tick.go:107-113` — the `Tick` doc comment says GitHub sends no
  delivery when a pull request falls behind "because someone pushed to master (that is a push
  event, which this daemon does not subscribe to)". This change subscribes to it. It is CODE
  documentation, so no other task touches it and no reviewer of the README will catch it.
- Modify: `README.md:449-450` (the cron section makes the same claim), `README.md:490`
  ("dispatching tend agents only"), `README.md:531-533` ("a `push` event, which this daemon does
  not subscribe to"), and the machine-wide settings list around `README.md:555-560`.
- Modify: `docs/configuration.md:761-767, 780-788, 813-816` — the `tend_pr` section says
  "**Two things dispatch a tend agent**" (now three), "Dispatch a tend agent **only** if it is
  behind" (git now goes first), "each tend run gets a fresh session" (a rebase has none), and
  "**A sweep does not replace a periodic tick**" (there is now a periodic check).

**Interfaces:** none.

**review: no**

- [ ] **Step 1: Fix the code comment first**

Update `internal/loopcmd/tick.go:107-113`. `Tick` is still the full reconcile and cron is still
the safety net — the only false clause is the parenthetical about not subscribing to push. Say
what is now true: the daemon subscribes to push, and `Tick` remains the sweep that runs where no
daemon does.

- [ ] **Step 2: The three tend triggers, in one place**

In the README's webhook section, replace the merge-only description with the three triggers: a
merge into the default branch, a push to it, and the periodic check. State that the periodic
check runs only while the listener runs, that it makes no GitHub call when nothing is behind, and
that cron is still the safety net for a machine with no daemon. Fix `README.md:449-450` and
`README.md:531-533`, which both assert the opposite today.

- [ ] **Step 3: The agent-free rebase**

In the README's tend paragraph (around `README.md:172`) and in `docs/configuration.md:761-816`:
git attempts the rebase first; a clean replay costs no agent and no tokens; a conflict dispatches
the tend agent as before. Correct the three specific sentences named in Files. Say that a clean
rebase writes a `rebase` dispatch row, visible in `project logs --list` and deliberately absent
from `sessions list`.

- [ ] **Step 4: Document `tend_interval` where machine-wide settings live**

**Not in `docs/configuration.md`.** That document is the *loop* configuration reference and says
in its own text (`docs/configuration.md:127-134`) that the machine-wide file "is never described
by this document". The machine-wide settings are documented in the README beside
`webhook.listen_addr` and `webhook.listen_port` (`README.md:555-560`); `tend_interval` goes
there. Give the default (`15m`), the meaning of `0`, and the command:
`agent-utils config set tend_interval 30m`.

- [ ] **Step 5: The deployment note**

Record that `push` is a new event, so every repository needs `register-webhook` re-run, and that
the step needs a token with **admin** on the repository — the token in `~/.agent-utils/env` today
holds `maintain` and gets a 404 reading its own hooks. Say that until that is done, the merge
trigger and the periodic check still work.

- [ ] **Step 6: Commit**

```bash
git add README.md docs/configuration.md internal/loopcmd/tick.go
git commit -m "docs: the three tend triggers, the agent-free rebase, and tend_interval"
```

---

## Known limits

Recorded here rather than discovered later, following the predecessor plan's precedent.

- **The periodic check runs only while the listener runs.** A machine with no daemon has no
  periodic tend. Cron remains the safety net, and this change does not add one.
- **`confirms` is memory only.** A daemon restart forces one confirm per tending loop: two API
  calls each, once. That is the price of adding no column and no migration.
- **The cold-cache window.** A loop whose `pr_links` rows are empty finds nothing until the first
  forced confirm, which is the first pass after start. A loop whose rows are stale can be up to
  six hours out of date on the *link* facts — never on the behind counts, which are read from git
  every pass.
- **`maxTendPerSweep` (10) still caps the agent-free rebases.** The cap exists to bound how many
  agents one merge can start, and a git rebase costs no agent, so applying it to both is
  conservative. A repository with forty stale pull requests takes four passes — an hour at the
  default interval. Its log line ("the rest wait for the next merge") is now wrong in a second
  way and should read "the next sweep".
- **The loop lock is held longer than before.** `act` runs under it, and the rebase adds a fetch,
  a rebase, and a push per stale pull request, each bounded at two minutes.
  `internal/loopcmd/tendsweep.go:53-67` records why that matters: a delivery arriving while the
  lock is held is dropped by `issuePass` with no retry. The mitigation here is the per-command
  deadline and the cap above; moving the git work outside the lock is a larger change and is
  **deliberately not attempted in this plan**. See the gate note.
- **No separate off-switch for the agent-free rebase.** `tend_interval: 0` disables *detection*,
  not the rebase, which also runs from the merge trigger, the push trigger, and cron's `Tick`.
  Turning the rebase off means turning `tend_pr` off for the loop.

## Pipeline State

| Field     | Value                                                                    |
|-----------|--------------------------------------------------------------------------|
| stage     | 2 (plan review)                                                          |
| class     | large (new subsystem, new config surface, automatic force-push)          |
| profile   | backend                                                                  |
| branch    | feat/tend-triggers-and-agent-free-rebase                                 |
| pr        | #18                                                                      |
| gate      | pending                                                                  |
| round     | 0                                                                        |
| decisions | 0                                                                        |

### Decisions

_None yet._
