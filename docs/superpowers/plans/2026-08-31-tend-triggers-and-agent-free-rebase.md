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

## Global Constraints

This repository has no `CLAUDE.md`, `AGENTS.md`, or `STANDARDS.md`. The binding rules are the
gates and the conventions the code itself enforces:

- `make check` must pass: `fmtcheck`, `go vet` (host and `GOOS=darwin`), `golangci-lint run`,
  and the full test suite (`Makefile:173`).
- Every exported symbol carries a doc comment. This codebase states **why** a rule exists, not
  only what the code does. Match the density of the file you edit.
- A comment must not restate the code. Where a decision has a failure mode, name the failure.
- `ghub.HookEvents` is the single event list. `register-webhook` and the handler both read it
  (`internal/ghub/types.go:116-124`). Never add a second list.
- A value from a webhook payload is attacker-controlled. Shape-check it before it is stored,
  passed to git, or logged (`internal/listener/handler.go:519-528`).
- Never pass an unchecked ref to git. Use `ghub.SafeRef` or `worktree.SafeRef`
  (`internal/worktree/worktree.go:114`).
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
- `ghub.HookEvents` — `internal/ghub/types.go:118-124`. Five events today.
- `ghub.SafeRef(ref string) bool` — `internal/ghub/types.go:141-145`.
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
- `listener.Delivery` — `internal/listener/work.go:329-357`. `IsMergeInto` is at `work.go:366`.
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
- Modify: `internal/ghub/types.go:118-124`
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
	for _, t := range targets {
		// A push names no issue, so the only work it can start is the sweep.
		// Dropping it HERE, not in tickOne, is what keeps the cost at one
		// field test: tickOne runs after Open, which reads the token, opens a
		// SQLite handle and runs the migration check. A repository whose
		// feature branches are pushed all day would pay all three per push,
		// per loop, for a delivery no loop can act on.
		if d.Number == 0 && !(t.TendPR && d.IsPushTo(t.DefaultBranch)) {
			continue
		}
		w.tickOne(ctx, t, d, acc)
	}
```

Keep `Deliver`'s existing "evaluating this issue" log line for a delivery that names a number.
For a push, log the branch instead:

```go
	if d.Number == 0 {
		slog.Info("a push moved a branch; every loop that tends it will sweep",
			"repo", d.Repo, "pushed_to", d.PushedTo, "loops", loops)
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
- Modify: `internal/config/duration.go`
- Modify: `internal/settings/settings.go:56-90` and `settings.go:360` (`Fields`)
- Test: `internal/config/duration_test.go`, `internal/settings/settings_test.go`

**Interfaces:**
- Consumes: `config.Duration` (`internal/config/duration.go:11`).
- Produces: `settings.Settings.TendInterval config.Duration` (`yaml:"tend_interval"`),
  `settings.DefaultTendInterval = 15 * time.Minute`, and the `tend_interval` key in
  `settings.Fields()`.

**review: no**

- [ ] **Step 1: Write the failing tests**

In `internal/config/duration_test.go`:

```go
// Save writes the settings file back. Without MarshalYAML a Duration is
// marshalled as its integer nanoseconds, so a file written once would no
// longer parse the next time it is read.
func TestDurationRoundTripsThroughYAML(t *testing.T) {
	type holder struct {
		D config.Duration `yaml:"d"`
	}
	out, err := yaml.Marshal(holder{D: config.Duration(15 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	var back holder
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("marshalled form does not parse: %q: %v", out, err)
	}
	if back.D.Std() != 15*time.Minute {
		t.Errorf("round trip = %v, want 15m", back.D.Std())
	}
}
```

In `internal/settings/settings_test.go`:

```go
// The default is applied by WithDefaults, not by Load, exactly as the listen
// address is: Load must keep returning a true zero value for a machine that
// has never run `config`.
func TestWithDefaultsFillsTheTendInterval(t *testing.T) {
	got := settings.Settings{}.WithDefaults()
	if got.TendInterval.Std() != settings.DefaultTendInterval {
		t.Errorf("TendInterval = %v, want %v", got.TendInterval.Std(), settings.DefaultTendInterval)
	}
}

// Zero disables the periodic check, so WithDefaults must not overwrite a
// stored zero with the default. A stored value is a decision; an absent value
// is not.
func TestWithDefaultsKeepsAnExplicitZeroTendInterval(t *testing.T) {
	s := settings.Settings{TendIntervalSet: true}
	if got := s.WithDefaults(); got.TendInterval.Std() != 0 {
		t.Errorf("TendInterval = %v, want the stored 0", got.TendInterval.Std())
	}
}

func TestTendIntervalIsASettableKey(t *testing.T) {
	var found bool
	for _, f := range settings.Fields() {
		if f.Key == "tend_interval" {
			found = true
			s := &settings.Settings{}
			if err := f.Set(s, "30m"); err != nil {
				t.Fatal(err)
			}
			if s.TendInterval.Std() != 30*time.Minute {
				t.Errorf("after Set: %v, want 30m", s.TendInterval.Std())
			}
			if got := f.Get(s); got != "30m0s" {
				t.Errorf("Get = %q, want 30m0s", got)
			}
			if err := f.Set(s, "nonsense"); err == nil {
				t.Error("an unparsable duration must be refused")
			}
		}
	}
	if !found {
		t.Fatal("tend_interval is not a settable key")
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/config/ ./internal/settings/ -run 'Duration|Tend' -v`
Expected: FAIL — `MarshalYAML` and `TendInterval` do not exist.

- [ ] **Step 3: Add `MarshalYAML` to `config.Duration`**

```go
// MarshalYAML renders the duration as the string form UnmarshalYAML accepts.
//
// Without it, yaml.Marshal writes the underlying int64 nanoseconds, and a file
// written by settings.Save would fail to load on the next read. The canonical
// form is what time.Duration prints, so a stored "15m" comes back as "15m0s".
// That is the same value, and round-tripping correctly matters more than
// preserving the operator's spelling.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }
```

- [ ] **Step 4: Add the setting**

In `internal/settings/settings.go`:

```go
// DefaultTendInterval is how often the listener looks for a pull request that
// has fallen behind its base. It is applied by WithDefaults, not by Load.
//
// The check is nearly free when nothing is behind -- it reads local git refs
// the loop already fetched and makes no GitHub call -- so the interval is set
// by how soon an operator wants a stale branch noticed, not by what the check
// costs.
const DefaultTendInterval = 15 * time.Minute

// Settings is the machine-wide configuration.
type Settings struct {
	Webhook Webhook `yaml:"webhook"`
	// TendInterval is how often the listener runs its periodic tend check. A
	// value of 0 disables that check and leaves every other tend trigger
	// unchanged.
	//
	// It is machine-wide rather than per loop because it describes how
	// attentive the daemon is, not anything about a loop. It applies to every
	// loop with tend_pr: true, of every registered project.
	TendInterval config.Duration `yaml:"tend_interval"`
	// TendIntervalSet records that the file carried a tend_interval, so
	// WithDefaults can tell a stored 0 -- which disables the check -- from an
	// absent value, which takes DefaultTendInterval. Without it, an operator
	// who disables the check gets it back on the next load.
	TendIntervalSet bool `yaml:"-"`
}
```

Set `TendIntervalSet` where `Load` decodes the file. Decode into a shadow struct with
`*config.Duration`, or check the decoded YAML node for the key; either is acceptable, but the
flag must be true only when the key is present.

In `WithDefaults`:

```go
	if !s.TendIntervalSet && s.TendInterval == 0 {
		s.TendInterval = config.Duration(DefaultTendInterval)
	}
```

In `Fields()`, add:

```go
		{
			Key: "tend_interval",
			Get: func(s *Settings) string { return s.TendInterval.Std().String() },
			Set: func(s *Settings, v string) error {
				d, err := time.ParseDuration(v)
				if err != nil {
					return fmt.Errorf("tend_interval must be a duration such as \"15m\": %w", err)
				}
				if d < 0 {
					return fmt.Errorf("tend_interval must not be negative")
				}
				s.TendInterval = config.Duration(d)
				s.TendIntervalSet = true
				return nil
			},
			Unset: func(s *Settings) { s.TendInterval = 0; s.TendIntervalSet = false },
		},
```

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./internal/config/ ./internal/settings/ ./cmd/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config/duration.go internal/config/duration_test.go internal/settings/settings.go internal/settings/settings_test.go
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
supply a `Fetch` that records it ran and a worktree seam whose `BehindLocal` answers from the map.

**Note for the implementer:** `Deps.WT` is a concrete `*worktree.Manager`, so a test cannot
substitute it. Add one seam to `Deps` for this pass:

```go
	// Behind counts how far a pull request's head is behind its base, from the
	// local checkout. It is a seam so a test needs no git repository; production
	// wires it to worktree.Manager.BehindLocal.
	Behind func(headRef, baseRef string) (behind int, known bool, err error)
```

Wire it in `loopcmd.Open` beside `Fetch`, and fall back to `deps.WT.BehindLocal` when it is nil.

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
			slog.Error("delete a pr link whose pull request is closed",
				"loop", cfg.Name, "issue", number, "pr", l.PRNumber, "err", err)
			continue
		}
		slog.Debug("dropped a pr link whose pull request is closed",
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
		pr, ok := engine.LinkPR(number, prs)
		if !ok || pr.BaseRef != cfg.DefaultBranch || pr.HeadRef != links[number].HeadRef {
			continue
		}
		out.Stale++
	}
	return out, nil
}
```

Add the `fmt` and `ghub` imports. Confirm `cfg.RepoOwner()` and `cfg.RepoName()` are the
accessors `tendSnapshot` uses (`internal/loopcmd/tendsweep.go:101`).

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
  `settings.Settings.TendInterval` (Task 5), `Worker.armTend`.
- Produces:
  - `Worker.TendInterval time.Duration` — 0 disables the pass.
  - `Worker.RunTendCheck func(ctx, cfg, deps, force) (loopcmd.TendCheckResult, error)` — the seam.
  - `Worker.tendCheckPass(ctx)` — one sweep of every loop.

**review: yes** — it adds concurrent work to the daemon.

- [ ] **Step 1: Write the failing tests**

```go
// The pass walks the registry, not the deliveries. That is what makes it reach
// a project whose webhook is missing, which is the failure this feature fixes.
func TestTendCheckPassArmsASweepForEachStaleLoop(t *testing.T) {
	w := newTestWorker(t)
	w.TendInterval = time.Minute
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
func TestServeRunsNoTendCheckWhenTheIntervalIsZero(t *testing.T) {
	w := newTestWorker(t)
	w.TendInterval = 0
	w.RunTendCheck = func(context.Context, *config.Config, loopcmd.Deps, bool) (loopcmd.TendCheckResult, error) {
		t.Fatal("the pass must not run when tend_interval is 0")
		return loopcmd.TendCheckResult{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Serve(ctx) // returns at once on a cancelled context
}
```

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
	routes, err := Scan()
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
		if ctx.Err() != nil {
			return
		}
		w.tendCheckOne(ctx, t, acc)
	}
}
```

`tendCheckOne` opens the loop the way `tendFresh` does, decides `force` from `w.confirms` under
the mutex, calls `w.RunTendCheck`, records the confirm time when the result reports
`Confirmed`, and calls `w.armTend(ctx, t, cfg.DefaultBranch)` when `Stale > 0`. Log one line per
loop that finds something; say nothing for a loop that finds nothing, or the log fills with
noise every interval.

Add the constant:

```go
// tendConfirmInterval is how often the check calls GitHub even though nothing
// looks behind. It is what corrects a pr_links row that drifted with no
// delivery: a pull request that closed, or an issue that lost its review
// label, leaves a row the local gate would otherwise trust forever.
const tendConfirmInterval = 6 * time.Hour
```

- [ ] **Step 5: Add the ticker to `Serve`**

```go
	var tendC <-chan time.Time
	if w.TendInterval > 0 {
		tend := time.NewTicker(w.TendInterval)
		defer tend.Stop()
		tendC = tend.C
	}
```

Add `case <-tendC:` to the `select`, running `w.tendCheckPass(ctx)` and then continuing the loop.
A nil channel blocks forever, which is exactly what a disabled pass needs. Do **not** fold this
into `Wake`: `Wake` is driven by retry deadlines and floored at `MinWakeInterval`.

- [ ] **Step 6: Wire the setting**

In `cmd/agent-utils/listener.go`, where the worker is built, set
`w.TendInterval = s.TendInterval.Std()` from the loaded settings, after `WithDefaults`. Log the
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
func (m *Manager) HeadSHA(ctx context.Context, path string) (string, error)
func (m *Manager) Rebase(ctx context.Context, path, baseRef string) error
func (m *Manager) AbortRebase(ctx context.Context, path string) error
func (m *Manager) PushWithLease(ctx context.Context, path, headRef, lease string) error
```

**review: yes** — `PushWithLease` rewrites a remote branch.

- [ ] **Step 1: Write the failing tests**

Build a real repository with an `origin` remote in a temp directory, as the package's other git
tests do:

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

- `HeadSHA` runs `rev-parse HEAD` and trims the output.
- `Rebase` checks `SafeRef(baseRef)` and runs `rebase origin/<baseRef>`.
- `AbortRebase` runs `rebase --abort`. It returns nil when there is no rebase in progress; git
  exits non-zero in that case, and the caller aborts unconditionally.
- `PushWithLease` checks `SafeRef(headRef)` and runs
  `push --force-with-lease=<headRef>:<lease> origin HEAD:refs/heads/<headRef>`. Document that the
  lease is what makes an unattended force-push safe, and that git — not this program — enforces
  it.

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
- Modify: `internal/store/types.go:5-10` (the kinds)

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
	EnsurePR(number int, headRef string) (string, error)
	Dirty(path string) (bool, error)
	HeadSHA(ctx context.Context, path string) (string, error)
	Rebase(ctx context.Context, path, baseRef string) error
	AbortRebase(ctx context.Context, path string) error
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
func TestTryRebaseCleanReplayDispatchesNoAgent(t *testing.T) {
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
func TestTryRebaseConflictAbortsAndDispatchesTheAgent(t *testing.T) {
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
func TestTryRebaseRefusedPushDispatchesNoAgent(t *testing.T) {
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
func TestTryRebaseDirtyWorktreeDispatchesTheAgent(t *testing.T) {
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
func TestTryRebaseWithoutAPerIssueWorktreeDispatchesTheAgent(t *testing.T) {
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

- [ ] **Step 4: Implement `tryRebase` and the git path**

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
func gitRebase(ctx context.Context, cfg *config.Config, deps Deps, d engine.Decision) (bool, error) {
	// A loop with no per-issue worktree has no pull-request checkout to rebase
	// in, and this pass will not create one: the agent path already handles
	// that mode.
	if cfg.Agent.Worktree != config.WorktreePerIssue || deps.Git == nil {
		return false, nil
	}

	path, err := deps.Git.EnsurePR(d.PR, d.HeadRef)
	if err != nil {
		return false, err
	}

	// The lease. It is read AFTER EnsurePR, which fetches the head ref and
	// checks it out, so it is the commit the remote had a moment ago -- which
	// is exactly what the push must be pinned to.
	lease, err := deps.Git.HeadSHA(ctx, path)
	if err != nil {
		return false, err
	}

	// A dirty tend worktree holds work a rebase would destroy: uncommitted
	// changes, or commits that were never pushed. The agent is the right actor
	// for it.
	dirty, err := deps.Git.Dirty(path)
	if err != nil {
		return false, err
	}
	if dirty {
		slog.Info("tend worktree is dirty; leaving this rebase to the agent",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR)
		return false, nil
	}

	if err := deps.Git.Rebase(ctx, path, d.BaseRef); err != nil {
		// Unconditional, and its own error is logged rather than returned: a
		// worktree left mid-rebase fails every later pass for this pull
		// request, and the rebase failure below is the one worth reporting.
		if abortErr := deps.Git.AbortRebase(ctx, path); abortErr != nil {
			slog.Error("could not abort a failed rebase", "loop", cfg.Name,
				"issue", d.Issue, "pr", d.PR, "err", abortErr)
		}
		slog.Info("rebase did not replay cleanly; dispatching the tend agent",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", err)
		return false, nil
	}

	if err := deps.Git.PushWithLease(ctx, path, d.HeadRef, lease); err != nil {
		// The lease did its job, or the remote is unreachable. Either way this
		// pass acts no further and dispatches no agent; see the doc comment.
		slog.Warn("force-with-lease push refused; leaving this branch alone",
			"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "head", d.HeadRef, "err", err)
		return true, nil
	}

	if err := recordRebase(cfg, deps, d); err != nil {
		// The rebase HAPPENED. A failed record must not report it as
		// undone, or the next pass would rebase an already-current branch.
		slog.Error("could not record an automatic rebase", "loop", cfg.Name,
			"issue", d.Issue, "pr", d.PR, "err", err)
	}
	slog.Info("rebased a pull request with git; no agent was dispatched",
		"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "head", d.HeadRef, "base", d.BaseRef)
	return true, nil
}

// recordRebase writes the row that gives a force-push a cause.
//
// Without it the only evidence is a force-push in the pull request's timeline
// and a log line in a daemon an operator may not be watching. The row is
// written already finished: no process backs it, so nothing may reap it, and
// FinishDispatch only updates a row that is still running -- which is why the
// create and the finish are both here rather than split across the pass.
//
// The session identifier is empty on purpose. There is no conversation.
// sessionsFrom skips a dispatch with no session, so a rebase never appears in
// `sessions list` and never distorts a session's run count or cost, while
// `project logs --list` shows it like any other dispatch.
func recordRebase(cfg *config.Config, deps Deps, d engine.Decision) error {
	id, err := deps.Store.CreateDispatch(store.Dispatch{
		Loop: cfg.Name, Repo: cfg.Repo, Number: d.Issue, Kind: store.KindRebase,
		PRNumber: d.PR, Title: d.Title,
	})
	if err != nil {
		return err
	}
	return deps.Store.FinishDispatch(id, store.DispatchResult{
		Status: store.StatusSucceeded, ExitCode: 0, CostUSD: 0,
	})
}
```

Confirm `store.DispatchResult`'s field names against `internal/store/types.go` before writing the
literal, and add `FinishedAt` or `DurationMS` if the type requires them.

- [ ] **Step 5: Intercept in `act`**

Replace `case engine.KindTend:` at `tick.go:379`:

```go
	case engine.KindTend:
		// git first, the agent second. A rebase that replays cleanly needs no
		// conversation, and this is the common case: the agent exists for the
		// conflicts. tryRebase reports whether it settled the decision --
		// including the case where it settled it by declining to act, which is
		// what a refused lease means.
		{
			done, err := gitRebase(ctx, cfg, deps, d)
			if err != nil {
				// Logged, not returned: a git failure must not abandon the
				// rest of the sweep, and the agent is the fallback this whole
				// path is built around.
				slog.Warn("automatic rebase failed; falling back to the tend agent",
					"loop", cfg.Name, "issue", d.Issue, "pr", d.PR, "err", err)
			} else if done {
				sum.Rebased++
				return nil
			}
		}
		return count(&sum.Tended, dispatch(ctx, cfg, deps, d, now, store.KindTend))
```

- [ ] **Step 6: Run the tests and confirm they pass**

Run: `go test ./internal/loopcmd/ ./internal/store/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/loopcmd/rebase.go internal/loopcmd/rebase_test.go internal/loopcmd/tick.go internal/store/types.go
git commit -m "feat(loopcmd): rebase with git and escalate to an agent only on a conflict"
```

---

## Task 10: documentation

**Files:**
- Modify: `README.md` (the Webhooks section, around line 484; the tend paragraph around line 172)
- Modify: `docs/configuration.md`

**Interfaces:**
- Consumes: everything above.
- Produces: no code.

**review: no**

- [ ] **Step 1: Update the README's webhook section**

State the three tend triggers in one place: a merge into the default branch, a push to it, and
the periodic check. Say plainly that the periodic check runs only while the listener runs, and
that cron is still the safety net for a machine with no daemon. Say that the check makes no
GitHub call when nothing is behind.

- [ ] **Step 2: Update the README's tend paragraph**

Say that git attempts the rebase first, that a clean replay costs no agent, and that a conflict
dispatches the tend agent as before. Say that a clean rebase writes a `rebase` dispatch row and
appears in `project logs --list`, not in `sessions list`.

- [ ] **Step 3: Document `tend_interval` in `docs/configuration.md`**

Give the key, the default (`15m`), the meaning of `0`, and the fact that it is machine-wide.
Name the command that sets it: `agent-utils config set tend_interval 30m`.

- [ ] **Step 4: Add the deployment note**

Record that `push` is a new event, so every repository needs `register-webhook` re-run, and that
the step needs a token with admin on the repository. Say that until that is done, the merge
trigger and the periodic check still work.

- [ ] **Step 5: Commit**

```bash
git add README.md docs/configuration.md
git commit -m "docs: the three tend triggers, the agent-free rebase, and tend_interval"
```

---

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
