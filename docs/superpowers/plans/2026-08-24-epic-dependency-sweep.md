# Epic dependency sweep Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a sub-issue of an epic closes, add the pipeline's entry label to every sibling its
closure unblocked.

**Architecture:** A new narrow interface, `ghub.EpicReader`, reads three GitHub endpoints. A new
pure package, `internal/epic`, holds the promotion rule. `loopcmd.EpicSweep` composes them and
writes labels under the loop lock. Two drivers call it: the webhook listener on an `issues`
delivery with action `closed`, and `loopcmd.Tick` for cron. No agent is dispatched.

**Tech Stack:** Go 1.24, `github.com/google/go-github/v77`, `log/slog`, standard `testing`.

**Spec:** `docs/superpowers/specs/2026-08-24-epic-dependency-sweep-design.md`

## Global Constraints

This repository has **no** root conventions document (no `AGENTS.md`, `CLAUDE.md`,
`CONTRIBUTING.md`, or `STANDARDS.md`). The binding rules below are read from the code and the
build. **Recommendation for a follow-up, not for this change:** capture these in a root
`STANDARDS.md`, because every plan in `docs/superpowers/plans/` restates them.

- **Gates:** `make check` runs `fmtcheck vet lint test`. It must pass before every commit.
- **Lint:** `.golangci.yml` governs. Do not add a `//nolint` without a comment saying why.
- **Comments carry the reason, not the restatement.** Every non-obvious decision in this
  codebase is commented with *why*, and often with the incident that caused it. See
  `internal/loopcmd/tendsweep.go:17-26` for the register to match. A comment that restates the
  code is noise; a comment that records why a safer-looking alternative is wrong is the house
  style.
- **One GitHub write authority.** The agent owns every GitHub write except the retry-cap park
  (`README.md`, Security). This change adds the second exception. Say so where it is added.
- **Untrusted input.** Issue bodies, comments, and labels are attacker-controlled
  (`README.md`, Security). Nothing read here is interpolated into a shell command or a prompt.
- **Commit trailer:** every commit ends with
  `Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>`.
- **Never merge.** This plan ends at a review-ready pull request.

## Verified external API (do not re-derive)

Read from source and probed live on 2026-08-24. Do not re-check these.

**GitHub REST.**

| Endpoint | Returns | Pagination |
|---|---|---|
| `GET /repos/{o}/{r}/issues/{n}/parent` | one issue object with `labels`; `404` when there is no parent | none |
| `GET /repos/{o}/{r}/issues/{n}/sub_issues` | array of issue objects with `number`, `state`, `labels` | `per_page` max 100, `page` |
| `GET /repos/{o}/{r}/issues/{n}/dependencies/blocked_by` | array of issue objects with `number`, `state`, `labels`, and a full `repository` | `per_page` max 100, `page` |

**Every one of these three relations can cross repositories.** GitHub allows a sub-issue, a
parent, and a `blocked_by` entry to live in a different repository from the issue that names it.
The documented limits are **100 sub-issues per parent** and **8 levels of nesting**.

This is the single most important fact in this block, and it splits three ways:

- A **blocker** in another repository is fine and is honored. Its `state` is in the same
  response, so no second call and no second client is needed, and the state is all the rule
  reads.
- A **sub-issue** in another repository is NOT fine. The sweep writes a label by number, against
  the loop's own `owner/repo`. A foreign child's number would label whichever LOCAL issue happens
  to carry it. Task 4 filters these out; see `Issue.Repo`.
- A **parent** in another repository is NOT fine, for the same reason: its children would be read
  as if they were this repository's.

Every issue object carries `repository_url` (`https://api.github.com/repos/{owner}/{repo}`), and
`blocked_by` additionally carries a full `repository` object. `repository_url` is the field to
use, because it is present on all three responses.

**go-github v77 coverage.**

- `SubIssueService.ListByIssue(...)` exists, but this plan does **not** use it: it returns
  `[]*SubIssue`, and `github.SubIssue` is a **defined type** (`type SubIssue Issue`,
  `sub_issue.go:27`), not an alias, so feeding it to `ConvertIssues` needs an element-by-element
  conversion. All three endpoints are therefore read the same way, by hand, which also means one
  pagination helper instead of two.
- There is **no** dependencies service and **no** parent accessor. Both need
  `g.c.NewRequest("GET", url, nil)` then `g.c.Do(ctx, req, &v)`.
- **`github.AddOptions` DOES NOT EXIST.** The helper is unexported (`addOptions`,
  `github/github.go:312`). Build the query string by hand. Getting this wrong does not compile.
- `github.Response.NextPage` is populated from the `Link` header for any request made through
  `Do`, including a hand-built one, so pagination works without the helper.
- `github.ErrorResponse` carries `Response.StatusCode`. Match a 404 with
  `errors.As(err, &ge) && ge.Response != nil && ge.Response.StatusCode == 404`, as
  `internal/ghub/hooks.go`'s `missingScopeErr` does. **Do not write a third copy of that
  predicate** — Task 1 extracts the one `missingScopeErr` already contains.
- `internal/ghub/ghub.go:111` — `ErrNotAnIssue` already exists, for "this number names a pull
  request, not an issue". It is the right sentinel for a parent that converts to nothing; it is
  NOT the same condition as a 404.

**This repository.**

- `internal/ghub/types.go:9` — `Issue` carries `Number`, `Title`, `Labels`, `UpdatedAt`. No
  `State`. Task 1 adds it.
- `internal/ghub/types.go:29` — `Issue.HasAnyLabel(names []string) bool` implements the
  `"prefix*"` rule. `"status:*"` is expressible with it. Comparison folds case.
- `internal/ghub/ghub.go:39` — `ConvertIssues` drops pull requests and maps labels.
- `internal/ghub/hooks.go:12` — `HookAdmin` is the precedent for a narrow interface beside
  `Client`.
- `internal/ghub/single_test.go` — client tests use `httptest` plus a `newTestClient(t, srv)`
  helper already in the package.
- `internal/config/discover.go:143` — `List(agentUtilsDir) ([]Entry, error)`. `Entry` has
  `Name`, `File`, `Path`, `Repo`, `Err`. It does **not** carry the loaded config.
- `internal/config/discover.go:254` — `DirFromPath(path) string` returns the `.agent-utils`
  directory a loop file lives under, or `""`.
- `internal/config/config.go:53` — `Labels{Trigger, InFlight, Blocked, Review, Terminal, Veto}`.
  `Terminal` is optional and the execution loop omits it.
- `internal/loopcmd/tick.go:22` — `Deps` holds `Store`, `ProjectID`, `GH`, `WT`, `SelfPath`,
  `ConfigPath`, `Now`, `Spawn`, `IsAlive`, `Fetch`.
- `internal/loopcmd/tick.go:87` — `Summary{Started, Resumed, Retried, Tended, Parked, Live,
  Orphans, BreakerTripped}`.
- `internal/loopcmd/tendsweep.go:70` — `TendSweep(ctx, cfg, deps, base) (Summary, error)` is the
  shape to copy: reads first, lock second, cap, then log what was deferred.
- `internal/lock/lock.go:31` — `Acquire` is non-blocking and returns `ErrHeld` at once.
- **`internal/loopcmd/open.go:205` — `RunTick` ALREADY HOLDS the loop lock before it calls
  `Tick`.** `flock` is per open-file-description, so a second `Acquire` on the same path in the
  same process returns `ErrHeld`. This is why `Tick` takes no lock of its own and `TendSweep`
  does: `TendSweep`'s caller does not hold one, and `Tick`'s does. Anything called from inside
  `Tick` must NOT acquire the loop lock. Getting this wrong makes the cron path fail silently and
  forever.
- **`internal/ghub/deliverycache.go` — `*DeliveryCache` implements `ghub.Client` ONLY**, and
  `internal/listener/work.go:395` wraps every delivery's client in it. So on the webhook path
  `Options.GH` is a `*DeliveryCache`, and a type assertion to `ghub.EpicReader` FAILS. The reader
  must be threaded through `Options` explicitly; see Task 5.
- `internal/listener/work_test.go:383` — `ranNumbers()` already reports the issue numbers the
  harness's `runIssue` seam saw. Do not add a second accessor for the same data.
- `Makefile` — `check: fmtcheck vet lint test`. **`-race` is NOT part of `make check`**; it is
  the separate `test/race` target, run before a release and in CI.
- `internal/config/discover.go:223` — `Duplicates` exists because "Two loops sharing a name is
  never benign." Loop name is therefore not a safe identity.
- `internal/listener/work.go:266` — `Delivery{Repo, Number, MergedInto, ClosedPR}`.
- `internal/listener/work.go:427` — `tickOne` runs `issuePass`, then arms tend, then cleanup.
- `internal/listener/handler.go:525` — where `mergedInto` and `closedPR` are derived from the
  decoded body. `body.Action` is already decoded.

## File Structure

**Create:**

- `internal/ghub/epic.go` — the `EpicReader` interface and its three `GitHubClient` methods.
- `internal/ghub/epic_test.go` — `httptest` coverage of the three methods.
- `internal/epic/epic.go` — the pure rule. No imports beyond `ghub` and `sort`.
- `internal/epic/epic_test.go` — the rule's table.
- `internal/config/entryloop.go` — `EntryLoop`, which names the one loop allowed to sweep.
- `internal/config/entryloop_test.go`
- `internal/loopcmd/epicsweep.go` — `EpicSweep` and `EpicSweepAll`.
- `internal/loopcmd/epicsweep_test.go`

**Modify:**

- `internal/ghub/types.go` — add `Issue.State` and `Issue.IsOpen`.
- `internal/ghub/ghub.go` — set `State` in `ConvertIssues`.
- `internal/loopcmd/tick.go` — add `Deps.Epic`, add `Summary.Promoted`, call `EpicSweepAll`.
- `internal/loopcmd/open.go` — populate `Deps.Epic`.
- `internal/listener/work.go` — add `Delivery.ClosedIssue`, the `RunEpic` seam, and `epicPass`.
- `internal/listener/handler.go` — derive `closedIssue`.
- `docs/configuration.md`, `README.md` — record the behavior.

---

### Task 1: `ghub.EpicReader`

**Files:**
- Create: `internal/ghub/epic.go`, `internal/ghub/epic_test.go`
- Modify: `internal/ghub/types.go`, `internal/ghub/ghub.go`

**Interfaces:**
- Consumes: `github.Client` from go-github v77; the package's existing `Issue` type,
  `ErrNotAnIssue`, `missingScopeErr`'s 404 predicate, and the `newTestClient` test helper
  (which lives in `internal/ghub/hooks_test.go:20`, not in `single_test.go`).
- Produces:
  - `ghub.Issue.State string` and `func (i Issue) IsOpen() bool`
  - `ghub.Issue.Repo string` and `func (i Issue) InRepo(owner, repo string) bool`
  - `var ghub.ErrNoParent error`
  - `type ghub.EpicReader interface { Parent; SubIssues; BlockedBy; EditLabels }`
  - `func (g *GitHubClient) Parent(ctx context.Context, owner, repo string, number int) (Issue, error)`
  - `func (g *GitHubClient) SubIssues(ctx context.Context, owner, repo string, number int) ([]Issue, error)`
  - `func (g *GitHubClient) BlockedBy(ctx context.Context, owner, repo string, number int) ([]Issue, error)`

`Issue` gains `State` and `Repo` rather than a second issue type being introduced. A second type
would need its own copy of `HasLabel` and `HasAnyLabel`, and two label matchers that could
disagree is the exact hazard `convertPR`'s comment warns about.

`Repo` is what makes the sweep safe across repositories, and it is the field this task exists to
add as much as the three methods are. See the Verified external API block: a sub-issue or a parent
in another repository, written back by NUMBER against this loop's `owner/repo`, labels an
unrelated local issue.

- [ ] **Step 1: Write the failing tests for `State`, `Repo`, and their accessors**

Add to `internal/ghub/ghub_test.go` — **except `TestListOpenIssuesCarriesTheRepository`**, which
uses `httptest` and belongs in the new `internal/ghub/epic_test.go` created in Step 5.
`ghub_test.go` imports only `testing` and `github`, and the other three tests here need nothing
more.

```go
func TestConvertIssuesCarriesState(t *testing.T) {
	got := ConvertIssues([]*github.Issue{
		{Number: github.Ptr(1), State: github.Ptr("open")},
		{Number: github.Ptr(2), State: github.Ptr("closed")},
	})
	if len(got) != 2 {
		t.Fatalf("ConvertIssues returned %d issues, want 2", len(got))
	}
	if !got[0].IsOpen() {
		t.Errorf("issue 1 State = %q, want it to read as open", got[0].State)
	}
	if got[1].IsOpen() {
		t.Errorf("issue 2 State = %q, want it to read as closed", got[1].State)
	}
}

// The repository an issue lives in decides whether the sweep may write to it by
// number. A sub-issue may live in ANOTHER repository, and its number means
// nothing here.
func TestConvertIssuesCarriesTheRepository(t *testing.T) {
	got := ConvertIssues([]*github.Issue{
		{Number: github.Ptr(1), RepositoryURL: github.Ptr("https://api.github.com/repos/o/r")},
		{Number: github.Ptr(2), Repository: &github.Repository{FullName: github.Ptr("other/repo")}},
		{Number: github.Ptr(3)},
	})
	if len(got) != 3 {
		t.Fatalf("ConvertIssues returned %d issues, want 3", len(got))
	}
	if got[0].Repo != "o/r" {
		t.Errorf("Repo from repository_url = %q, want o/r", got[0].Repo)
	}
	if got[1].Repo != "other/repo" {
		t.Errorf("Repo from repository object = %q, want other/repo", got[1].Repo)
	}
	// An issue that names no repository must NOT be assumed local. InRepo is
	// what the sweep gates its write on, and the safe answer to "unknown" is no.
	if got[2].Repo != "" {
		t.Errorf("Repo = %q for an issue naming none, want empty", got[2].Repo)
	}
	if got[2].InRepo("o", "r") {
		t.Error("an issue naming no repository must not read as local")
	}
}

// EpicSweepAll gates every epic on InRepo, so the LIST endpoint has to carry
// the repository too -- not only the three epic endpoints. GitHub populates
// repository_url there, and this pins it: if it ever stopped, the cron backstop
// would promote nothing and log nothing, which is the silent failure this
// design exists to avoid.
func TestListOpenIssuesCarriesTheRepository(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
		  {"number":69,"state":"open","labels":[{"name":"epic"}],
		   "repository_url":"https://api.github.com/repos/o/r"}
		]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).ListOpenIssues(context.Background(), "o", "r")
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListOpenIssues returned %d, want 1", len(got))
	}
	if !got[0].InRepo("o", "r") {
		t.Errorf("Repo = %q; the epic sweep's cron path gates every epic on this",
			got[0].Repo)
	}
}

func TestInRepoFoldsCase(t *testing.T) {
	i := Issue{Repo: "McGaryLabs/Koinos"}
	if !i.InRepo("mcgarylabs", "koinos") {
		t.Error("InRepo must fold case, as HasLabel does")
	}
	if i.InRepo("mcgarylabs", "other") {
		t.Error("InRepo matched the wrong repository")
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/ghub/ -run 'TestConvertIssuesCarries|TestInRepo'`
Expected: FAIL — `got[0].IsOpen undefined`.

- [ ] **Step 3: Add `State`, `Repo`, and the accessors**

In `internal/ghub/types.go`, add both fields to `Issue` after `Labels`:

```go
	// State is "open" or "closed", as GitHub spells it.
	//
	// Every caller before the epic sweep listed OPEN issues only, so this was
	// not needed and was not carried. The sweep reads sub-issues and blockers,
	// and both lists mix open and closed, so the state has to survive the
	// conversion. Compare it with IsOpen rather than by ==: GitHub's spelling
	// is stable, but nothing here forces the case.
	State string
	// Repo is the "owner/name" this issue lives in, or "" when the response did
	// not say.
	//
	// It exists because sub-issue, parent and dependency relations may CROSS
	// repositories, and the epic sweep writes a label by NUMBER against one
	// loop's owner/repo. A foreign sub-issue carried through without this field
	// would label whichever local issue happens to share its number -- the same
	// class of bug as answering a pull_request delivery as an issue, which
	// handler.go guards against by checking the event.
	//
	// Empty is NOT "local". It is "unknown", and InRepo answers false for it.
	Repo string
```

And the accessors, beside `HasLabel`:

```go
// IsOpen reports whether the issue is open. The comparison ignores case.
func (i Issue) IsOpen() bool { return strings.EqualFold(i.State, "open") }

// InRepo reports whether the issue lives in owner/repo.
//
// An issue whose Repo is empty answers false. The field is empty only when the
// response did not name a repository, which is "unknown", and the sweep's write
// is by number: guessing "local" there is how a foreign issue gets labelled.
func (i Issue) InRepo(owner, repo string) bool {
	return i.Repo != "" && strings.EqualFold(i.Repo, owner+"/"+repo)
}
```

In `internal/ghub/ghub.go`, inside `ConvertIssues`'s append, add `State: gi.GetState(),` and
`Repo: issueRepo(gi),`, plus the helper:

```go
// issueRepo returns the "owner/name" an issue belongs to.
//
// Two sources, because the three endpoints do not agree. blocked_by returns a
// full repository object; parent and sub_issues return repository_url. Reading
// only one of them would leave Repo empty for two endpoints out of three, and
// an empty Repo blocks every write -- a silent, total failure rather than a
// loud one.
func issueRepo(gi *github.Issue) string {
	if full := gi.GetRepository().GetFullName(); full != "" {
		return full
	}
	// repository_url is "https://api.github.com/repos/{owner}/{name}". Take the
	// last two path elements rather than trimming a hard-coded host prefix:
	// GitHub Enterprise serves a different base URL.
	u := gi.GetRepositoryURL()
	if u == "" {
		return ""
	}
	parts := strings.Split(strings.Trim(u, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}
```

- [ ] **Step 4: Run them and confirm they pass**

Run: `go test ./internal/ghub/ -run 'TestConvertIssuesCarries|TestInRepo'`
Expected: PASS.

- [ ] **Step 5: Write the failing tests for the three reads**

Create `internal/ghub/epic_test.go`:

```go
package ghub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-github/v77/github"
)

func TestParentReturnsTheEpic(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/73/parent", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&github.Issue{
			Number: github.Ptr(69),
			Title:  github.Ptr("epic(mobile): the ios app"),
			State:  github.Ptr("open"),
			Labels: []*github.Label{{Name: github.Ptr("epic")}},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).Parent(context.Background(), "o", "r", 73)
	if err != nil {
		t.Fatalf("Parent: %v", err)
	}
	if got.Number != 69 {
		t.Errorf("Number = %d, want 69", got.Number)
	}
	if !got.HasLabel("epic") {
		t.Errorf("labels not carried through: %v", got.Labels)
	}
}

// A 404 is the ORDINARY answer for an issue with no parent, which is most
// issues in most repositories. It must be a sentinel a caller can branch on,
// not an error that stops a sweep.
func TestParentReportsNoParentAsSentinel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/12/parent", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_, err := newTestClient(t, srv).Parent(context.Background(), "o", "r", 12)
	if !errors.Is(err, ErrNoParent) {
		t.Fatalf("Parent error = %v, want it to wrap ErrNoParent", err)
	}
}

func TestSubIssuesCarriesStateAndLabels(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/69/sub_issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]*github.Issue{
			{Number: github.Ptr(71), State: github.Ptr("closed")},
			{
				Number: github.Ptr(74),
				State:  github.Ptr("open"),
				Labels: []*github.Label{{Name: github.Ptr("status:plan-ready-for-review")}},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).SubIssues(context.Background(), "o", "r", 69)
	if err != nil {
		t.Fatalf("SubIssues: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("SubIssues returned %d, want 2", len(got))
	}
	if got[0].IsOpen() {
		t.Errorf("71 read as open; want closed")
	}
	if !got[1].HasLabel("status:plan-ready-for-review") {
		t.Errorf("74 lost its labels: %v", got[1].Labels)
	}
}

// A sub-issue may live in ANOTHER repository. Its number means nothing in this
// one, and the sweep writes by number, so the repository must survive the read.
func TestSubIssuesCarriesAForeignChildsRepository(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/69/sub_issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
		  {"number":73,"state":"open","labels":[],
		   "repository_url":"https://api.github.com/repos/o/r"},
		  {"number":74,"state":"open","labels":[],
		   "repository_url":"https://api.github.com/repos/other/repo"}
		]`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).SubIssues(context.Background(), "o", "r", 69)
	if err != nil {
		t.Fatalf("SubIssues: %v", err)
	}
	if !got[0].InRepo("o", "r") {
		t.Errorf("73 Repo = %q, want it to read as local", got[0].Repo)
	}
	if got[1].InRepo("o", "r") {
		t.Errorf("74 Repo = %q read as local; it lives in other/repo", got[1].Repo)
	}
}

// A server that always reports a next page must not spin a webhook handler
// forever.
func TestPagedIssuesRefusesAPaginationThatDoesNotAdvance(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/69/sub_issues", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// rel="next" pointing at page 1, forever.
		w.Header().Set("Link", `<`+srv.URL+`/repos/o/r/issues/69/sub_issues?page=1>; rel="next"`)
		_, _ = w.Write([]byte(`[{"number":71,"state":"closed","labels":[]}]`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	if _, err := newTestClient(t, srv).SubIssues(context.Background(), "o", "r", 69); err == nil {
		t.Fatal("want an error when pagination does not advance, got nil")
	}
}

// The state of a blocker is what decides a promotion, and a blocker may live in
// another repository. Its state is in THIS response, so the sweep never needs a
// second call -- pin that the field survives the conversion.
func TestBlockedByCarriesStateOfAForeignBlocker(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/74/dependencies/blocked_by",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
			  {"number":73,"state":"closed","labels":[],
			   "repository":{"full_name":"o/r"}},
			  {"number":9,"state":"open","labels":[],
			   "repository":{"full_name":"other/repo"}}
			]`))
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).BlockedBy(context.Background(), "o", "r", 74)
	if err != nil {
		t.Fatalf("BlockedBy: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("BlockedBy returned %d, want 2", len(got))
	}
	if got[0].IsOpen() {
		t.Errorf("73 read as open, want closed")
	}
	if !got[1].IsOpen() {
		t.Errorf("the foreign blocker read as closed, want open")
	}
}

func TestBlockedByReturnsEmptyForNoDependencies(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/78/dependencies/blocked_by",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).BlockedBy(context.Background(), "o", "r", 78)
	if err != nil {
		t.Fatalf("BlockedBy: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("BlockedBy returned %d, want 0", len(got))
	}
}

// Both list endpoints are paginated. A 30-child epic read one page deep would
// silently promote nothing for its tail, which is a wrong answer that looks
// like a correct one.
func TestSubIssuesFollowsPagination(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/69/sub_issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			_, _ = w.Write([]byte(`[{"number":72,"state":"open","labels":[]}]`))
			return
		}
		w.Header().Set("Link", `<`+srv.URL+`/repos/o/r/issues/69/sub_issues?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`[{"number":71,"state":"closed","labels":[]}]`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).SubIssues(context.Background(), "o", "r", 69)
	if err != nil {
		t.Fatalf("SubIssues: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("SubIssues returned %d, want both pages (2)", len(got))
	}
}
```

- [ ] **Step 6: Run them and confirm they fail**

Run: `go test ./internal/ghub/ -run 'TestParent|TestSubIssues|TestBlockedBy'`
Expected: FAIL — `newTestClient(...).Parent undefined`.

- [ ] **Step 7: Write `internal/ghub/epic.go`**

```go
package ghub

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/google/go-github/v77/github"
)

// EpicReader is the read surface the epic dependency sweep needs, plus the one
// write it performs.
//
// It is separate from Client for the reason HookAdmin is: no tick calls these,
// and a caller that must fake them should not have to fake the eight methods of
// Client as well. Four fakes of Client exist in this repository's tests, and
// none of them has anything to say about sub-issues.
//
// EditLabels is repeated here rather than referenced through Client so that the
// sweep depends on exactly one interface. It is the SECOND non-agent GitHub
// write in this program -- the first is the retry-cap park -- and the README's
// Security section names them both.
type EpicReader interface {
	Parent(ctx context.Context, owner, repo string, number int) (Issue, error)
	SubIssues(ctx context.Context, owner, repo string, number int) ([]Issue, error)
	BlockedBy(ctx context.Context, owner, repo string, number int) ([]Issue, error)
	EditLabels(ctx context.Context, owner, repo string, number int, add, remove []string) error
}

// ErrNoParent reports that an issue has no parent issue.
//
// GitHub answers the parent endpoint with 404 for that case, which is the
// ordinary answer for nearly every issue in nearly every repository. A caller
// must be able to tell it apart from a real failure without parsing a message:
// the sweep stops quietly on this one and logs loudly on any other.
var ErrNoParent = errors.New("issue has no parent")

// Parent returns the issue that holds number as a sub-issue.
//
// go-github v77 has no accessor for this endpoint, so the request is built by
// hand. hooks.go does the same for its own calls.
func (g *GitHubClient) Parent(ctx context.Context, owner, repo string, number int) (Issue, error) {
	u := fmt.Sprintf("repos/%s/%s/issues/%d/parent",
		url.PathEscape(owner), url.PathEscape(repo), number)
	req, err := g.c.NewRequest("GET", u, nil)
	if err != nil {
		return Issue{}, fmt.Errorf("parent %s/%s#%d: %w", owner, repo, number, err)
	}
	var gi github.Issue
	if _, err := g.c.Do(ctx, req, &gi); err != nil {
		if isNotFound(err) {
			return Issue{}, fmt.Errorf("parent %s/%s#%d: %w", owner, repo, number,
				errors.Join(ErrNoParent, err))
		}
		return Issue{}, fmt.Errorf("parent %s/%s#%d: %w", owner, repo, number, err)
	}
	// ConvertIssues drops pull requests. Passing through it keeps ONE mapping
	// from a GitHub issue to this type, so a field added there is carried by
	// every reader.
	out := ConvertIssues([]*github.Issue{&gi})
	if len(out) == 0 {
		// NOT ErrNoParent. A 404 means "this issue has no parent", which is
		// ordinary; landing here means a parent was returned and it was a pull
		// request. Those have different fixes, so they get different sentinels
		// -- the rule internal/config/discover.go:20 states for its own.
		return Issue{}, fmt.Errorf("parent %s/%s#%d: %w", owner, repo, number, ErrNotAnIssue)
	}
	return out[0], nil
}

// SubIssues returns every sub-issue of number, following pagination.
func (g *GitHubClient) SubIssues(ctx context.Context, owner, repo string, number int) ([]Issue, error) {
	return g.pagedIssues(ctx,
		fmt.Sprintf("repos/%s/%s/issues/%d/sub_issues",
			url.PathEscape(owner), url.PathEscape(repo), number),
		fmt.Sprintf("sub_issues %s/%s#%d", owner, repo, number))
}

// BlockedBy returns every issue number declares as a blocker, following
// pagination.
//
// A blocker may live in ANOTHER repository. Its state comes back in this same
// response, and the state is all the sweep reads, so the repository is
// deliberately not carried into Issue: nothing here would use it, and a field
// nothing uses invites a caller to filter on it and silently ignore a real
// cross-repository blocker.
func (g *GitHubClient) BlockedBy(ctx context.Context, owner, repo string, number int) ([]Issue, error) {
	return g.pagedIssues(ctx,
		fmt.Sprintf("repos/%s/%s/issues/%d/dependencies/blocked_by",
			url.PathEscape(owner), url.PathEscape(repo), number),
		fmt.Sprintf("blocked_by %s/%s#%d", owner, repo, number))
}

// pagedIssues reads every page of an endpoint that returns issue objects.
//
// One implementation, two callers: both endpoints are paginated the same way,
// and a second copy would be free to stop after page one for only one of them.
// That failure is invisible -- a 40-child epic would simply promote nothing for
// its tail.
func (g *GitHubClient) pagedIssues(ctx context.Context, path, what string) ([]Issue, error) {
	// The query is built by hand because go-github's addOptions is unexported.
	// Do not reach for github.AddOptions; there is no such symbol.
	page := 1
	var all []Issue
	for {
		u := fmt.Sprintf("%s?per_page=100&page=%d", path, page)
		req, err := g.c.NewRequest("GET", u, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		var got []*github.Issue
		resp, err := g.c.Do(ctx, req, &got)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		all = append(all, ConvertIssues(got)...)
		// NextPage is filled from the Link header for a hand-built request too,
		// so this needs no helper. A response with no Link header leaves it 0
		// and ends the loop.
		if resp == nil || resp.NextPage == 0 {
			return all, nil
		}
		// GitHub caps a parent at 100 sub-issues, so this loop is bounded in
		// practice at one or two pages. The guard is here anyway: this reads a
		// remote server's pagination cursor, and a server that always reports a
		// next page would otherwise spin forever inside a webhook handler.
		if resp.NextPage <= page {
			return all, fmt.Errorf("%s: pagination did not advance past page %d", what, page)
		}
		page = resp.NextPage
	}
}
```

`isNotFound` is NOT written here. `missingScopeErr` in `internal/ghub/hooks.go` already contains
that exact predicate, and this codebase single-sources a rule and says so — see `isLive` in
`internal/loopcmd/tick.go:62`, "the ONE liveness rule". Extract it from `missingScopeErr` into a
package-level helper and call it from both:

```go
// isNotFound reports whether err is GitHub's 404.
//
// One copy, two callers: missingScopeErr reads it as "no such hook, or no
// admin:repo_hook scope", and Parent reads it as "this issue has no parent".
// The two READINGS differ and that is fine; the test must not, or one of them
// silently stops recognising a 404.
func isNotFound(err error) bool {
	var ge *github.ErrorResponse
	return errors.As(err, &ge) && ge.Response != nil && ge.Response.StatusCode == 404
}
```

Then rewrite `missingScopeErr`'s condition to `if isNotFound(err) {`. Run
`go test ./internal/ghub/` afterwards — the hooks tests cover that path and must stay green.

- [ ] **Step 8: Run them and confirm they pass**

Run: `go test ./internal/ghub/`
Expected: PASS, including the pre-existing tests.

- [ ] **Step 9: Run the gates and commit**

```bash
make check
git add internal/ghub/
git commit -m "$(cat <<'EOF'
feat(ghub): read an issue's parent, sub-issues, and blockers

EpicReader is narrow, like HookAdmin, so the four existing Client fakes
stay untouched. go-github v77 covers none of the three endpoints, so the
requests are built by hand.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: `internal/epic`, the rule

**Files:**
- Create: `internal/epic/epic.go`, `internal/epic/epic_test.go`

**Interfaces:**
- Consumes: `ghub.Issue`, `ghub.Issue.IsOpen`, `ghub.Issue.HasAnyLabel` from Task 1.
- Produces:
  - `const epic.StatusPrefix = "status:*"`
  - `const epic.Label = "epic"`
  - `type epic.Child struct { Issue ghub.Issue; Blockers []ghub.Issue; BlockersUnknown bool }`
  - `func epic.Promote(children []Child, veto []string, owner, repo string) []int`
  - `func epic.NeedsBlockers(child ghub.Issue, veto []string) bool`

This package is pure. It opens no socket, reads no clock, and keeps no state. Every rule the
sweep applies lives here, and the sweep applies none of its own.

**Why a new package and not `internal/engine`.** `engine.Decide` is also pure and also consumes
`ghub.Issue` and `config.Labels`, so the question is fair. They are separated because they answer
different questions from different state. `Decide` answers "which agent must this tick dispatch",
and to do it, it reads stored issue state, running dispatches, retry deadlines and the circuit
breaker; its output is a `Plan` of `Decision`s that `act` turns into processes. `Promote` answers
"which issues did a closure unblock", reads nothing but the issues themselves, and its output is
a list of numbers to label. Putting it in `engine` would mean either a `Decision` kind that
dispatches nothing — which every consumer of `Plan.Decisions` would then have to learn to skip,
including `TendSweep`, which filters by kind — or a second exported function in `engine` that
shares none of `Decide`'s inputs. Neither is cheaper than a package whose whole surface is one
rule.

- [ ] **Step 1: Write the failing tests**

Create `internal/epic/epic_test.go`:

```go
package epic

import (
	"reflect"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/ghub"
)

// Every helper below lives in "o/r", the repository the rule is scoped to. A
// blocker with no Repo is FOREIGN, not local, so omitting it here would quietly
// turn a blocking case into an ignored one.
func open(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, State: "open", Repo: "o/r", Labels: labels}
}

func closed(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, State: "closed", Repo: "o/r", Labels: labels}
}

// The reference loop's veto list. blocked:* is a prefix rule.
var veto = []string{"blocked:*"}

func TestPromote(t *testing.T) {
	cases := []struct {
		name     string
		children []Child
		want     []int
	}{
		{
			name:     "no blockers at all is trivially unblocked",
			children: []Child{{Issue: open(78)}},
			want:     []int{78},
		},
		{
			name: "every blocker closed",
			children: []Child{{
				Issue:    open(73),
				Blockers: []ghub.Issue{closed(71), closed(70)},
			}},
			want: []int{73},
		},
		{
			name: "one open blocker holds it",
			children: []Child{{
				Issue:    open(74),
				Blockers: []ghub.Issue{closed(73), open(72)},
			}},
			want: nil,
		},
		{
			// The property that protects work in flight. Issue #74 of the
			// reference repository sits at plan-ready-for-review with its
			// blockers closed; pulling it back would discard a plan a human
			// is reading.
			name: "a child already in the pipeline is left alone",
			children: []Child{{
				Issue:    open(74, "status:plan-ready-for-review"),
				Blockers: []ghub.Issue{closed(73)},
			}},
			want: nil,
		},
		{
			name: "a child already promoted is not promoted twice",
			children: []Child{{
				Issue:    open(74, "status:ready-for-spec"),
				Blockers: []ghub.Issue{closed(73)},
			}},
			want: nil,
		},
		{
			name: "a veto label holds it",
			children: []Child{{
				Issue:    open(74, "blocked:legal"),
				Blockers: []ghub.Issue{closed(73)},
			}},
			want: nil,
		},
		{
			name:     "a closed child is never promoted",
			children: []Child{{Issue: closed(71)}},
			want:     nil,
		},
		{
			// Failing closed. A blocker list that could not be read says
			// nothing, and the alternative reading promotes an issue whose
			// blockers may be open.
			name: "an unreadable blocker list holds it",
			children: []Child{{
				Issue:           open(74),
				BlockersUnknown: true,
			}},
			want: nil,
		},
		{
			// The operator's decision: this sweep is scoped to one repository
			// entirely, so a blocker outside it cannot hold a child back. This
			// is fail-OPEN and is the one place in this package that is.
			name: "an OPEN blocker in another repository is ignored",
			children: []Child{{
				Issue:    open(74),
				Blockers: []ghub.Issue{{Number: 9, State: "open", Repo: "other/repo"}},
			}},
			want: []int{74},
		},
		{
			// The mixed case is the one that matters: the foreign blocker is
			// skipped, and the local one still decides.
			name: "a local open blocker still holds it when a foreign one is ignored",
			children: []Child{{
				Issue: open(74),
				Blockers: []ghub.Issue{
					{Number: 9, State: "closed", Repo: "other/repo"},
					open(73),
				},
			}},
			want: nil,
		},
		{
			// Repo empty means the response did not say. It is not this
			// repository, so it is ignored like any other foreign blocker.
			name: "a blocker naming no repository is ignored",
			children: []Child{{
				Issue:    open(74),
				Blockers: []ghub.Issue{{Number: 9, State: "open"}},
			}},
			want: []int{74},
		},
		{
			// The diamond. Two blockers of one child close in the same sweep,
			// and the child appears once, not twice.
			name: "a diamond promotes the child exactly once",
			children: []Child{{
				Issue:    open(76),
				Blockers: []ghub.Issue{closed(73), closed(75)},
			}},
			want: []int{76},
		},
		{
			// Ascending, so a capped sweep takes the low-numbered batch every
			// time and the next sweep takes the next one.
			name: "results are ascending by number",
			children: []Child{
				{Issue: open(77)},
				{Issue: open(74)},
				{Issue: open(76)},
			},
			want: []int{74, 76, 77},
		},
		{
			name:     "no children promotes nothing",
			children: nil,
			want:     nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Promote(c.children, veto, "o", "r")
			if len(got) == 0 && len(c.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Promote = %v, want %v", got, c.want)
			}
		})
	}
}

// The sweep filters before it spends a call per child. That filter must agree
// with the rule, or it saves a call by dropping a child that should have been
// promoted.
func TestNeedsBlockersAgreesWithPromote(t *testing.T) {
	cases := []ghub.Issue{
		open(78),
		open(74, "status:plan-ready-for-review"),
		open(74, "blocked:legal"),
		closed(71),
		open(70, "enhancement"),
	}
	for _, iss := range cases {
		// A child with no blockers is promoted exactly when the rule says it
		// may be. If NeedsBlockers says "skip the call" for a child Promote
		// would have promoted, the sweep loses a promotion.
		promoted := len(Promote([]Child{{Issue: iss}}, veto, "o", "r")) == 1
		if got := NeedsBlockers(iss, veto); got != promoted {
			t.Errorf("issue %d %v: NeedsBlockers = %v, but Promote %v it",
				iss.Number, iss.Labels, got,
				map[bool]string{true: "promoted", false: "declined"}[promoted])
		}
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/epic/`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Write `internal/epic/epic.go`**

```go
// Package epic decides which sub-issues of an epic a closure unblocked.
//
// Everything here is pure. It opens no socket, reads no clock, and keeps no
// state. The sweep in internal/loopcmd does the reading and the writing; the
// decision is only ever made here, so there is one place to read to know what
// the sweep will do.
package epic

import (
	"sort"

	"github.com/seanmcgary/agent-utils/internal/ghub"
)

// Label is the label a parent issue must carry for its children to be swept.
// It is the only switch: there is no configuration field, so removing this
// label from a parent is how an operator opts an epic out.
const Label = "epic"

// StatusPrefix matches every pipeline status label, using the "prefix*" rule
// ghub.Issue.HasAnyLabel implements.
//
// Carrying ANY status label is what makes a child ineligible. That is what
// makes this sweep idempotent without a single row of stored state: a promoted
// child now carries the trigger label, so the next sweep declines it, and a
// sweep that failed halfway is simply re-run.
//
// It is deliberately the whole namespace and not a list of known labels. A
// status label this program has never heard of still means "a human or an agent
// has this issue in hand", and the safe answer to an unknown state is to leave
// it alone.
const StatusPrefix = "status:*"

// Child is one sub-issue of an epic, with the blockers it declares.
type Child struct {
	// Issue is the sub-issue itself, as sub_issues returned it.
	Issue ghub.Issue
	// Blockers is what blocked_by returned for it. Empty means the issue
	// declares no dependency, which satisfies the rule.
	Blockers []ghub.Issue
	// BlockersUnknown reports that the blocker list could not be read.
	//
	// It is NOT the same as an empty list, and conflating the two is the one
	// mistake in this package that would be actively harmful: an empty list
	// means "nothing blocks this issue" and promotes it, while a failed read
	// means "this is unknown" and must not. The sweep sets this when GitHub
	// fails it, and the child is held until a later sweep can read it.
	BlockersUnknown bool
}

// Promote returns the numbers to promote, ascending.
//
// A child is promoted when all of these hold:
//   - it is open;
//   - its blocker list was read, and every blocker IN owner/repo is closed;
//   - it carries no status label;
//   - it carries none of the loop's veto labels.
//
// owner and repo scope the whole rule to one repository. See unblocked for what
// that means for a blocker outside it, and why it is the operator's decision
// rather than this package's.
//
// The result is ascending so that a capped sweep takes the low-numbered batch
// every time and the next sweep takes the next one. Without an order the batch
// identity would depend on GitHub's page order, and the same child could be
// deferred forever.
func Promote(children []Child, veto []string, owner, repo string) []int {
	var out []int
	for _, c := range children {
		if !NeedsBlockers(c.Issue, veto) {
			continue
		}
		if c.BlockersUnknown {
			continue
		}
		if !unblocked(c.Blockers, owner, repo) {
			continue
		}
		out = append(out, c.Issue.Number)
	}
	sort.Ints(out)
	return out
}

// NeedsBlockers reports whether child could be promoted if its blockers turned
// out to be closed.
//
// It is every part of the rule that can be decided WITHOUT reading the blocker
// list, and it exists so the sweep can skip a call it does not need. It is an
// optimization, never the decision: Promote tests the same conditions again, so
// a sweep that passed it a child this would have skipped still gets the right
// answer. TestNeedsBlockersAgreesWithPromote pins the two together.
func NeedsBlockers(child ghub.Issue, veto []string) bool {
	if !child.IsOpen() {
		return false
	}
	if child.HasAnyLabel([]string{StatusPrefix}) {
		return false
	}
	return !child.HasAnyLabel(veto)
}

// unblocked reports whether every blocker in owner/repo is closed. An empty
// list is unblocked: an issue that declares no dependency is waiting for
// nothing.
//
// # A blocker outside owner/repo is IGNORED, not honored
//
// GitHub lets an issue declare a blocker in another repository. This sweep
// scopes itself to one repository entirely, so such a blocker is skipped and
// cannot hold a child back.
//
// This is the operator's decision, and it is deliberately fail-OPEN, which is
// the opposite of what this package does everywhere else. The cost is real and
// worth stating plainly: a child whose only remaining blocker lives in another
// repository is promoted while that blocker is still open, and planning starts
// on work whose prerequisite is not done. The reasoning for accepting it is
// that a loop watches one repository, its labels mean nothing outside that
// repository, and honoring a dependency the loop can neither see change nor
// act on makes the sweep's behavior depend on a repository nobody here
// administers.
//
// It also removes a failure this design could not otherwise detect: a blocker
// in a repository the token cannot read may be OMITTED from the response
// rather than reported, and an omitted blocker is indistinguishable from one
// that was never declared. Every such blocker is foreign by definition, so
// ignoring foreign blockers makes that case decided rather than silent.
func unblocked(blockers []ghub.Issue, owner, repo string) bool {
	for _, b := range blockers {
		if !b.InRepo(owner, repo) {
			continue
		}
		if b.IsOpen() {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run them and confirm they pass**

Run: `go test ./internal/epic/ -v`
Expected: PASS, every subtest.

- [ ] **Step 5: Mutation-check the two properties that carry the safety**

Confirm the tests actually hold the line. Make each change, run the tests, see a FAIL, then
revert it.

1. In `NeedsBlockers`, delete the `HasAnyLabel([]string{StatusPrefix})` check.
   Expected: `a child already in the pipeline is left alone` FAILS.
2. In `Promote`, change `if c.BlockersUnknown { continue }` to `if false { continue }`.
   Expected: `an unreadable blocker list holds it` FAILS.
3. In `unblocked`, delete the `if !b.InRepo(owner, repo) { continue }` skip.
   Expected: `an OPEN blocker in another repository is ignored` and `a blocker naming no
   repository is ignored` both FAIL. This is the one fail-OPEN rule in the package, so its test
   must be the thing holding it in place.

Revert both. If either change leaves the tests green, the test is not pinning the property and
must be fixed before moving on.

- [ ] **Step 6: Run the gates and commit**

```bash
make check
git add internal/epic/
git commit -m "$(cat <<'EOF'
feat(epic): the promotion rule, as a pure package

A child is promoted when it is open, its blockers are all closed, and it
carries no status label. The status-label test is what makes the sweep
idempotent with no stored state, and what keeps a plan under review from
being pulled back to the start of the pipeline.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: `config.EntryLoop`

**Files:**
- Create: `internal/config/entryloop.go`, `internal/config/entryloop_test.go`

**Interfaces:**
- Consumes: `config.List`, `config.Load`, `config.Entry` from `internal/config/discover.go`.
- Produces:
  - `var config.ErrNoEntryLoop error`
  - `var config.ErrAmbiguousEntryLoop error`
  - `func config.EntryLoop(agentUtilsDir, repo string) (string, error)`

- [ ] **Step 1: Write the failing tests**

Create `internal/config/entryloop_test.go`:

```go
package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// loopYAML is a minimal valid loop file. The fields Load requires are all
// present; only the parts EntryLoop reads vary between cases.
func loopYAML(name, repo, trigger, review, terminal string) string {
	body := `
name: ` + name + `
repo: ` + repo + `
checkout_base_dir: /tmp/checkout
worktree_dir: /tmp/worktrees
state_dir: /tmp/state
default_branch: master
labels:
  trigger: ` + trigger + `
  in_flight: status:in-flight-` + name + `
  blocked: status:blocked-` + name + `
  review: ` + review + `
`
	if terminal != "" {
		body += "  terminal: " + terminal + "\n"
	}
	body += `agent: {model: opus, worktree: per_issue, timeout: 1h}
retry: {max: 1, backoff: [0s], breaker: {orphan_threshold: 2, cooldown: 1m}}
prompt: p
resume_prompt: rp
`
	return body
}

// writeLoops creates a .agent-utils/configs directory holding the given files,
// keyed by file name.
func writeLoops(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), DirName)
	cfgDir := ConfigsDir(dir)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(cfgDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The reference pair. planning.terminal IS execution.trigger, so execution is
// downstream and planning is the entry.
func TestEntryLoopResolvesTheReferencePair(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"planning.yaml": loopYAML("planning", "o/r",
			"status:ready-for-spec", "status:plan-ready-for-review", "status:ready-for-execution"),
		"execution.yaml": loopYAML("execution", "o/r",
			"status:ready-for-execution", "status:ready-for-review", ""),
	})

	got, err := EntryLoop(dir, "o/r")
	if err != nil {
		t.Fatalf("EntryLoop: %v", err)
	}
	if got != "planning" {
		t.Errorf("EntryLoop = %q, want planning", got)
	}
}

func TestEntryLoopResolvesASingleLoop(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"only.yaml": loopYAML("only", "o/r", "status:ready-for-spec", "status:in-review", ""),
	})

	got, err := EntryLoop(dir, "o/r")
	if err != nil {
		t.Fatalf("EntryLoop: %v", err)
	}
	if got != "only" {
		t.Errorf("EntryLoop = %q, want only", got)
	}
}

// Two loops neither of which is downstream of the other. Guessing here would
// promote issues into the wrong stage of the pipeline, silently.
func TestEntryLoopRefusesWhenAmbiguous(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"a.yaml": loopYAML("a", "o/r", "status:ready-for-a", "status:review-a", ""),
		"b.yaml": loopYAML("b", "o/r", "status:ready-for-b", "status:review-b", ""),
	})

	_, err := EntryLoop(dir, "o/r")
	if !errors.Is(err, ErrAmbiguousEntryLoop) {
		t.Fatalf("EntryLoop error = %v, want ErrAmbiguousEntryLoop", err)
	}
	// The loops must be NAMED. An operator cannot fix "it is ambiguous".
	for _, want := range []string{"a", "b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name loop %q: %v", want, err)
		}
	}
}

// A cycle: each loop's trigger is the other's terminal. No loop is the entry.
func TestEntryLoopRefusesWhenNoneIsTheEntry(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"a.yaml": loopYAML("a", "o/r", "status:x", "status:review-a", "status:y"),
		"b.yaml": loopYAML("b", "o/r", "status:y", "status:review-b", "status:x"),
	})

	_, err := EntryLoop(dir, "o/r")
	if !errors.Is(err, ErrNoEntryLoop) {
		t.Fatalf("EntryLoop error = %v, want ErrNoEntryLoop", err)
	}
}

// A loop file that does not load leaves the graph incomplete. The loop it
// declares might be the one that makes another downstream, so the derivation
// cannot be trusted and nothing sweeps.
func TestEntryLoopRefusesWhenALoopFileIsBroken(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"planning.yaml": loopYAML("planning", "o/r",
			"status:ready-for-spec", "status:plan-ready-for-review", "status:ready-for-execution"),
		"broken.yaml": "name: broken\nrepo: o/r\nthis is not: [valid",
	})

	if _, err := EntryLoop(dir, "o/r"); err == nil {
		t.Fatal("want an error when a loop file cannot be loaded, got nil")
	}
}

// Two files declaring one name. Each would exclude the other as "itself", so
// the graph would lose an edge and the message would name one loop twice.
func TestEntryLoopRefusesDuplicateLoopNames(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"a.yaml": loopYAML("planning", "o/r",
			"status:ready-for-spec", "status:plan-ready-for-review", "status:ready-for-execution"),
		"b.yaml": loopYAML("planning", "o/r",
			"status:ready-for-execution", "status:ready-for-review", ""),
	})

	_, err := EntryLoop(dir, "o/r")
	if !errors.Is(err, ErrAmbiguousEntryLoop) {
		t.Fatalf("EntryLoop error = %v, want ErrAmbiguousEntryLoop", err)
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error does not say the names are duplicated: %v", err)
	}
}

// A loop watching another repository is not part of this repository's pipeline
// and must not make its trigger look downstream.
func TestEntryLoopIgnoresAnotherRepositorysLoops(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"planning.yaml": loopYAML("planning", "o/r",
			"status:ready-for-spec", "status:plan-ready-for-review", "status:ready-for-execution"),
		"other.yaml": loopYAML("other", "other/repo",
			"status:whatever", "status:review-other", "status:ready-for-spec"),
	})

	got, err := EntryLoop(dir, "o/r")
	if err != nil {
		t.Fatalf("EntryLoop: %v", err)
	}
	if got != "planning" {
		t.Errorf("EntryLoop = %q, want planning", got)
	}
}

// The repository is spelled by hand in each loop file, so two files may differ
// in case while naming one repository.
func TestEntryLoopMatchesTheRepositoryCaseInsensitively(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"planning.yaml": loopYAML("planning", "O/R",
			"status:ready-for-spec", "status:plan-ready-for-review", "status:ready-for-execution"),
		"execution.yaml": loopYAML("execution", "o/r",
			"status:ready-for-execution", "status:ready-for-review", ""),
	})

	got, err := EntryLoop(dir, "o/r")
	if err != nil {
		t.Fatalf("EntryLoop: %v", err)
	}
	if got != "planning" {
		t.Errorf("EntryLoop = %q, want planning", got)
	}
}

func TestEntryLoopRefusesWhenNoLoopWatchesTheRepository(t *testing.T) {
	dir := writeLoops(t, map[string]string{
		"other.yaml": loopYAML("other", "other/repo", "status:x", "status:review-other", ""),
	})

	if _, err := EntryLoop(dir, "o/r"); !errors.Is(err, ErrNoEntryLoop) {
		t.Fatalf("EntryLoop error = %v, want ErrNoEntryLoop", err)
	}
}

```

The file's imports are `errors`, `os`, `path/filepath`, `strings`, and `testing`.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/config/ -run TestEntryLoop`
Expected: FAIL — `undefined: EntryLoop`.

- [ ] **Step 3: Write `internal/config/entryloop.go`**

```go
package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Entry-loop derivation errors. Each is a distinct condition with a distinct
// fix, so each is its own sentinel.
var (
	// ErrNoEntryLoop reports that no loop watching the repository is at the
	// front of its pipeline.
	ErrNoEntryLoop = errors.New("no entry loop")
	// ErrAmbiguousEntryLoop reports that more than one is.
	ErrAmbiguousEntryLoop = errors.New("more than one entry loop")
)

// EntryLoop returns the name of the one loop allowed to promote an issue into
// its trigger label, for the loops of agentUtilsDir that watch repo.
//
// # Why this is derived and not configured
//
// The epic sweep promotes a statusless issue by adding a loop's trigger label.
// If every loop did that for its OWN trigger, the execution loop would promote
// a fresh issue straight to status:ready-for-execution and the planning stage
// would be skipped entirely -- silently, and only for issues that happen to be
// swept. The operator's requirement is that the sweep needs no configuration,
// so the answer has to come from the loop files that already exist.
//
// # The rule
//
// A loop is the entry when its trigger label is not any OTHER loop's terminal
// or review label. A loop whose trigger is another's terminal is downstream of
// it, which is exactly how the reference pair is wired: planning ends at
// status:ready-for-execution, which is execution's trigger.
//
// # Why it fails closed
//
// Zero entry loops, two or more, or any loop file that will not load, all
// return an error and no loop sweeps. A guess would put issues into the wrong
// stage of the pipeline, and nothing downstream would report it: the issue
// would simply be picked up by an agent expecting a plan that was never
// written. A broken file counts because the loop it declares may be the very
// one that makes another downstream, so the graph cannot be trusted with a
// piece missing.
func EntryLoop(agentUtilsDir, repo string) (string, error) {
	entries, err := List(agentUtilsDir)
	if err != nil {
		return "", fmt.Errorf("entry loop for %s: %w", repo, err)
	}
	// Two loops sharing a name is never benign -- Duplicates exists to say so.
	// It matters more here than anywhere else: the derivation below asks "is
	// any OTHER loop's terminal label my trigger", and with a duplicated name
	// the two copies would each exclude the other as "itself". The graph would
	// be computed from a set that silently lost an edge, and the ambiguity
	// message would read "planning, planning are all at the front", which tells
	// an operator nothing.
	if dupes := Duplicates(entries); len(dupes) > 0 {
		return "", fmt.Errorf("entry loop for %s: duplicate loop names: %s: %w",
			repo, strings.Join(dupes, ", "), ErrAmbiguousEntryLoop)
	}

	type loop struct {
		name     string
		trigger  string
		terminal string
		review   string
	}
	var loops []loop
	for _, e := range entries {
		// A file that will not load is fatal even when it names another
		// repository: Entry.Repo is empty when Err is set, so it cannot be
		// filtered out honestly. Refusing is the conservative answer.
		if e.Err != nil {
			return "", fmt.Errorf("entry loop for %s: loop %q does not load: %w",
				repo, e.File, e.Err)
		}
		if !strings.EqualFold(e.Repo, repo) {
			continue
		}
		cfg, err := Load(e.Path)
		if err != nil {
			return "", fmt.Errorf("entry loop for %s: loop %q does not load: %w",
				repo, e.File, err)
		}
		loops = append(loops, loop{
			name:     cfg.Name,
			trigger:  cfg.Labels.Trigger,
			terminal: cfg.Labels.Terminal,
			review:   cfg.Labels.Review,
		})
	}

	var entry []string
	for i, l := range loops {
		downstream := false
		for j, other := range loops {
			// Compared by INDEX, not by name. Duplicates is rejected above, so
			// names are unique by the time this runs -- but identity that does
			// not depend on that invariant cannot be broken by relaxing it.
			if i == j {
				continue
			}
			// Terminal is optional -- the execution loop omits it -- and an
			// empty label matches nothing. Without this guard two loops that
			// both omit it would each look downstream of the other, and a
			// pipeline with no terminal labels at all would resolve to no
			// entry loop.
			if other.terminal != "" && strings.EqualFold(l.trigger, other.terminal) {
				downstream = true
				break
			}
			if other.review != "" && strings.EqualFold(l.trigger, other.review) {
				downstream = true
				break
			}
		}
		if !downstream {
			entry = append(entry, l.name)
		}
	}
	sort.Strings(entry)

	switch len(entry) {
	case 1:
		return entry[0], nil
	case 0:
		// Two quite different conditions, and an operator's fix differs for
		// each: there is no loop watching this repository at all, or there are
		// loops and every one of them is downstream of another. A single
		// message covering both would name neither fix.
		if len(loops) == 0 {
			return "", fmt.Errorf("entry loop for %s: no loop in %s watches this repository: %w",
				repo, agentUtilsDir, ErrNoEntryLoop)
		}
		return "", fmt.Errorf("entry loop for %s: every loop's trigger is another's terminal or review label: %w",
			repo, ErrNoEntryLoop)
	default:
		// NAMED, not counted. An operator cannot act on "it is ambiguous",
		// and this is a permanent misconfiguration rather than a transient
		// failure: it will be logged on every sweep until somebody fixes it.
		return "", fmt.Errorf("entry loop for %s: %s are all at the front of the pipeline: %w",
			repo, strings.Join(entry, ", "), ErrAmbiguousEntryLoop)
	}
}
```

- [ ] **Step 4: Run them and confirm they pass**

Run: `go test ./internal/config/ -run TestEntryLoop -v`
Expected: PASS, every case.

- [ ] **Step 5: Run the gates and commit**

```bash
make check
git add internal/config/
git commit -m "$(cat <<'EOF'
feat(config): derive the pipeline's entry loop

A loop is the entry when its trigger is no other loop's terminal or
review label. Deriving it is what lets the epic sweep be always-on with
no configuration field, without letting the execution loop promote a
fresh issue past the planning stage. It fails closed on zero, on more
than one, and on a loop file that does not load.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: `loopcmd.EpicSweep`

**Files:**
- Create: `internal/loopcmd/epicsweep.go`, `internal/loopcmd/epicsweep_test.go`
- Modify: `internal/loopcmd/tick.go`, `internal/loopcmd/open.go`

**Interfaces:**
- Consumes: `ghub.EpicReader`, `ghub.ErrNoParent` (Task 1); `epic.Child`, `epic.Promote`,
  `epic.NeedsBlockers`, `epic.Label` (Task 2); `config.EntryLoop`, `config.ErrNoEntryLoop`,
  `config.ErrAmbiguousEntryLoop`, `config.DirFromPath` (Task 3); `lock.Acquire`.
- Produces:
  - `Deps.Epic ghub.EpicReader`
  - `Summary.Promoted int`
  - `func loopcmd.EpicSweep(ctx context.Context, cfg *config.Config, deps Deps, closed int) (Summary, error)`
  - `func loopcmd.EpicSweepAll(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error)`

- [ ] **Step 1: Add the two struct fields**

In `internal/loopcmd/tick.go`, add to `Deps` after `GH`:

```go
	// Epic reads sub-issues and issue dependencies for the epic sweep. It is
	// narrow on purpose -- see ghub.EpicReader -- so a test of any OTHER pass
	// does not have to grow three methods it never calls.
	Epic ghub.EpicReader
```

And to `Summary`, after `Tended`:

```go
	Promoted int `json:"promoted"`
```

**Populating `Deps.Epic` — read this carefully, the obvious version does not work.**

`internal/loopcmd/open.go` has exactly ONE `gh` variable (line 129, typed `ghub.Client` from
`opts.GH`) and ONE `Deps` literal (lines 183-194). There are not two sites.

A type assertion on that variable is **not** sufficient, and this is the trap:

- On the CLI path, `opts.GH` is nil and `Open` builds a `*ghub.GitHubClient`, which does satisfy
  `EpicReader`. An assertion would work.
- On the WEBHOOK path — the sweep's primary driver — `listener.access` wraps the client in
  `ghub.NewDeliveryCache(...)` (`internal/listener/work.go:395`) and passes the
  `*ghub.DeliveryCache` as `opts.GH`. `DeliveryCache` implements the seven `ghub.Client` methods
  and nothing else. The assertion fails, `Deps.Epic` stays nil, and every close delivery logs
  `no EpicReader` at ERROR while promoting nothing.

So the reader is threaded through `Options` explicitly. Add to `Options`, after `GH`:

```go
	// Epic reads sub-issues and issue dependencies. Nil means "build one from
	// Token", like GH.
	//
	// It is a SEPARATE field from GH rather than an assertion on it, because
	// the daemon's GH is a *ghub.DeliveryCache: that type memoises single-issue
	// fetches for one delivery and implements ghub.Client only. Asserting
	// GH.(ghub.EpicReader) therefore succeeds for every CLI command and fails
	// for every webhook delivery -- which is the epic sweep's main driver, so
	// the feature would be dead exactly where it matters and nowhere a test
	// injecting its own reader would notice.
	Epic ghub.EpicReader
```

In `Open`, beside where `gh` is resolved:

```go
	// Falls back to the same client when the caller supplied none. The
	// concrete *ghub.GitHubClient satisfies both interfaces; only the
	// DeliveryCache wrapper does not, and the daemon passes Epic explicitly.
	epicReader := opts.Epic
	if epicReader == nil {
		if er, ok := gh.(ghub.EpicReader); ok {
			epicReader = er
		}
	}
```

and set `Epic: epicReader` in the `Deps` literal.

Then in `internal/listener/work.go`, carry the un-wrapped client on `access` beside `gh`:

```go
type access struct {
	token string
	gh    ghub.Client
	// epic is the SAME authenticated client as gh, before the DeliveryCache
	// wrapper. The cache exists to collapse the repeated single-issue fetch a
	// fan-out makes; the epic reads are made once per delivery and have nothing
	// to collapse, so they bypass it rather than teaching it three more methods
	// it would only ever pass through.
	epic ghub.EpicReader
}
```

In `Worker.access`, build it once:

```go
	c := w.NewClient(tok)
	acc := &access{token: tok, gh: ghub.NewDeliveryCache(c)}
	// A test's fake client may implement ghub.Client only. Leaving epic nil
	// there is correct: EpicSweep refuses rather than panicking.
	if er, ok := c.(ghub.EpicReader); ok {
		acc.epic = er
	}
	return acc, nil
```

And in `tickOne`, pass it through: `Epic: acc.epic,` in the `loopcmd.Options` literal.

- [ ] **Step 1b: Pin the wiring with a test that would have caught the nil**

The tests below inject `fakeEpic` directly into `Deps`, so none of them exercises the wiring
above. Add one that does, in `internal/loopcmd/open_test.go`:

```go
// The daemon's client is a *ghub.DeliveryCache, which implements ghub.Client
// and NOT ghub.EpicReader. Open must still produce a usable Deps.Epic, or the
// epic sweep is dead on the webhook path -- its primary driver -- while every
// test that injects its own reader stays green.
func TestOpenCarriesTheEpicReaderPastTheDeliveryCache(t *testing.T) {
	// The premise. If DeliveryCache ever grows the three methods, the explicit
	// Options.Epic field can go -- but until then, asserting on GH is a trap
	// that fails only in production.
	if _, ok := any((*ghub.DeliveryCache)(nil)).(ghub.EpicReader); ok {
		t.Fatal("DeliveryCache now implements EpicReader; this test's premise is stale")
	}

	t.Setenv(home.EnvVar, t.TempDir())
	path := writeOpenConfig(t)

	real := ghub.New("")
	_, deps, cleanup, err := Open(ProjectRef{}, path, Options{
		// Exactly what listener.access builds: the cache for GH, the
		// un-wrapped client for Epic.
		GH:   ghub.NewDeliveryCache(real),
		Epic: real,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cleanup()

	if deps.Epic == nil {
		t.Fatal("deps.Epic is nil; the epic sweep is dead on the webhook path")
	}
}

// The CLI path supplies neither, and must still get a reader.
func TestOpenBuildsAnEpicReaderWhenTheCallerSuppliesNone(t *testing.T) {
	t.Setenv(home.EnvVar, t.TempDir())
	path := writeOpenConfig(t)

	_, deps, cleanup, err := Open(ProjectRef{}, path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cleanup()

	if deps.Epic == nil {
		t.Fatal("deps.Epic is nil on the CLI path")
	}
}
```

- [ ] **Step 2: Write the failing tests**

Create `internal/loopcmd/epicsweep_test.go`:

```go
package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/ghub"
)

// fakeEpic is a ghub.EpicReader. It is narrow -- four methods -- which is the
// whole reason EpicReader exists apart from ghub.Client.
type fakeEpic struct {
	mu sync.Mutex

	// parents maps a child number to its parent. A number absent from the map
	// has no parent, which GitHub reports as 404.
	parents map[int]ghub.Issue
	// children maps a parent number to its sub-issues.
	children map[int][]ghub.Issue
	// blockers maps a child number to its blocked_by list.
	blockers map[int][]ghub.Issue
	// blockerErr maps a child number to an error BlockedBy must return.
	blockerErr map[int]error
	// labelErr maps an issue number to an error EditLabels must return.
	labelErr map[int]error

	// added records every (number, label) EditLabels was asked to add.
	added map[int][]string
	// blockedByCalls counts the lookups, so a test can prove a call was saved.
	blockedByCalls []int
	// subIssuesCalls counts the sub-issue listings. It is separate from
	// blockedByCalls because "the sweep stopped at the parent" and "the sweep
	// looked up no blockers" are different claims, and a test that asserts the
	// first against the second proves nothing.
	subIssuesCalls []int
	// subErr makes SubIssues fail for one parent, so the caller's
	// error-handling branch is reachable.
	subErr map[int]error
}

func newFakeEpic() *fakeEpic {
	return &fakeEpic{
		parents:    map[int]ghub.Issue{},
		children:   map[int][]ghub.Issue{},
		blockers:   map[int][]ghub.Issue{},
		blockerErr: map[int]error{},
		labelErr:   map[int]error{},
		added:      map[int][]string{},
		subErr:     map[int]error{},
	}
}

func (f *fakeEpic) Parent(_ context.Context, _, _ string, n int) (ghub.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.parents[n]
	if !ok {
		return ghub.Issue{}, fmt.Errorf("parent #%d: %w", n, ghub.ErrNoParent)
	}
	return p, nil
}

func (f *fakeEpic) SubIssues(_ context.Context, _, _ string, n int) ([]ghub.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subIssuesCalls = append(f.subIssuesCalls, n)
	if err := f.subErr[n]; err != nil {
		return nil, err
	}
	return f.children[n], nil
}

func (f *fakeEpic) BlockedBy(_ context.Context, _, _ string, n int) ([]ghub.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blockedByCalls = append(f.blockedByCalls, n)
	if err := f.blockerErr[n]; err != nil {
		return nil, err
	}
	return f.blockers[n], nil
}

func (f *fakeEpic) EditLabels(_ context.Context, _, _ string, n int, add, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.labelErr[n]; err != nil {
		return err
	}
	f.added[n] = append(f.added[n], add...)
	return nil
}

func (f *fakeEpic) promotedNumbers() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for n := range f.added {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// The fixtures below all live in "o/r", which is tickConfig's repo. An issue
// with no Repo would be skipped by the sweep's cross-repository guard, so
// omitting it here would make every promotion test pass for the wrong reason.
func openIssue(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, State: "open", Repo: "o/r", Labels: labels}
}

func closedIssue(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, State: "closed", Repo: "o/r", Labels: labels}
}

// foreignIssue is an open sub-issue that lives in ANOTHER repository.
func foreignIssue(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, State: "open", Repo: "other/repo", Labels: labels}
}

// epicParent is a parent issue carrying the epic label.
func epicParent(n int) ghub.Issue {
	return ghub.Issue{Number: n, State: "open", Repo: "o/r", Labels: []string{"epic"}}
}

// loopFile renders one loop configuration. The fields Load requires are all
// present; only the parts EntryLoop reads vary between cases.
func loopFile(name, trigger, review, terminal string) string {
	s := "name: " + name + "\nrepo: o/r\n" +
		"checkout_base_dir: /tmp/checkout\nworktree_dir: /tmp/worktrees\n" +
		"state_dir: /tmp/state\ndefault_branch: master\nlabels:\n" +
		"  trigger: " + trigger + "\n" +
		"  in_flight: status:in-flight-" + name + "\n" +
		"  blocked: status:blocked-" + name + "\n" +
		"  review: " + review + "\n"
	if terminal != "" {
		s += "  terminal: " + terminal + "\n"
	}
	return s + "agent: {model: opus, worktree: per_issue, timeout: 1h}\n" +
		"retry: {max: 1, backoff: [0s], breaker: {orphan_threshold: 2, cooldown: 1m}}\n" +
		"prompt: p\nresume_prompt: rp\n"
}

// referenceLoopFiles is the planning/execution pair. planning's terminal label
// IS execution's trigger, so execution is downstream and planning is the entry.
func referenceLoopFiles() map[string]string {
	return map[string]string{
		"planning.yaml": loopFile("planning",
			"status:ready-for-spec", "status:plan-ready-for-review", "status:ready-for-execution"),
		"execution.yaml": loopFile("execution",
			"status:ready-for-execution", "status:ready-for-review", ""),
	}
}

// writeLoopFiles creates a .agent-utils/configs directory holding files, and
// returns the path of the one named `loop`.
//
// The files are REAL, because EpicSweep derives the entry loop by loading them.
// A fixture that faked the derivation would not test the guard that stops the
// execution loop promoting past the planning stage.
func writeLoopFiles(t *testing.T, loop string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), config.DirName)
	cfgDir := config.ConfigsDir(dir)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(cfgDir, name), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(cfgDir, loop+".yaml")
}

// fixtureFor builds the config and deps for one loop of the reference pair.
func fixtureFor(t *testing.T, loop string) (*config.Config, Deps, *fakeEpic, *fakeGH) {
	t.Helper()
	return fixtureWithFiles(t, loop, referenceLoopFiles())
}

// fixtureWithFiles is fixtureFor with the loop files chosen by the caller, so a
// test can arrange an unresolvable derivation.
//
// It reuses tickConfig and newDeps from tick_test.go rather than inventing a
// second fixture shape for this package. Only the things the sweep reads are
// overridden: the labels, the config path (the entry-loop derivation walks up
// from it), and the reader.
func fixtureWithFiles(
	t *testing.T, loop string, files map[string]string,
) (*config.Config, Deps, *fakeEpic, *fakeGH) {
	t.Helper()
	cfg := tickConfig(t)
	cfg.Name = loop
	switch loop {
	case "planning":
		cfg.Labels.Trigger = "status:ready-for-spec"
		cfg.Labels.Review = "status:plan-ready-for-review"
		cfg.Labels.Terminal = "status:ready-for-execution"
	case "execution":
		cfg.Labels.Trigger = "status:ready-for-execution"
		cfg.Labels.Review = "status:ready-for-review"
		cfg.Labels.Terminal = ""
	default:
		t.Fatalf("fixtureFor: unknown loop %q", loop)
	}
	cfg.Labels.Veto = []string{"blocked:*"}

	gh := &fakeGH{}
	spawned := 0
	deps := newDeps(t, cfg, gh, &spawned)
	deps.ConfigPath = writeLoopFiles(t, loop, files)

	f := newFakeEpic()
	deps.Epic = f

	// The lock lives in StateDir, and tickConfig points it at a temp path that
	// does not exist yet. lock.Acquire does not create the directory.
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg, deps, f, gh
}

// sweepFixture is the entry loop: it sweeps.
func sweepFixture(t *testing.T) (*config.Config, Deps, *fakeEpic) {
	t.Helper()
	cfg, deps, f, _ := fixtureFor(t, "planning")
	return cfg, deps, f
}

// executionFixture is the downstream loop: it must not sweep.
func executionFixture(t *testing.T) (*config.Config, Deps, *fakeEpic) {
	t.Helper()
	cfg, deps, f, _ := fixtureFor(t, "execution")
	return cfg, deps, f
}

// sweepAllFixture also hands back the fakeGH, whose ListOpenIssues is what the
// cron path walks.
func sweepAllFixture(t *testing.T) (*config.Config, Deps, *fakeEpic, *fakeGH) {
	t.Helper()
	return fixtureFor(t, "planning")
}

func TestEpicSweepPromotesTheUnblockedSibling(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{
		closedIssue(71),
		openIssue(73),
		openIssue(74),
	}
	f.blockers[73] = []ghub.Issue{closedIssue(71)}
	f.blockers[74] = []ghub.Issue{closedIssue(71), openIssue(73)}

	sum, err := EpicSweep(context.Background(), cfg, deps, 71)
	if err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 1 || got[0] != 73 {
		t.Fatalf("promoted %v, want [73]", got)
	}
	if got := f.added[73]; len(got) != 1 || got[0] != "status:ready-for-spec" {
		t.Errorf("added %v to 73, want [status:ready-for-spec]", got)
	}
	if sum.Promoted != 1 {
		t.Errorf("Summary.Promoted = %d, want 1", sum.Promoted)
	}
}

// The common case for almost every delivery. It must cost ONE call and stop.
func TestEpicSweepStopsWhenTheIssueHasNoParent(t *testing.T) {
	cfg, deps, f := sweepFixture(t)

	if _, err := EpicSweep(context.Background(), cfg, deps, 12); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 0 {
		t.Errorf("promoted %v, want none", got)
	}
	// Asserted against subIssuesCalls, which is what "stopped at the parent"
	// actually means. blockedByCalls would be empty either way.
	if len(f.subIssuesCalls) != 0 {
		t.Errorf("listed sub-issues of %v; an issue with no parent must cost one call",
			f.subIssuesCalls)
	}
}

func TestEpicSweepStopsWhenTheParentIsNotAnEpic(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = ghub.Issue{Number: 69, State: "open", Labels: []string{"tracking"}}
	f.children[69] = []ghub.Issue{openIssue(73)}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 0 {
		t.Errorf("promoted %v, want none", got)
	}
	if len(f.subIssuesCalls) != 0 {
		t.Errorf("read sub-issues of a parent that is not an epic: %v", f.subIssuesCalls)
	}
}

// A parent in ANOTHER repository is not this loop's epic. Its children would be
// read from, and written to, the wrong repository by number.
func TestEpicSweepStopsWhenTheParentIsInAnotherRepository(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = ghub.Issue{
		Number: 69, State: "open", Repo: "other/repo", Labels: []string{"epic"},
	}
	f.children[69] = []ghub.Issue{openIssue(73)}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 0 {
		t.Errorf("promoted %v for a foreign epic, want none", got)
	}
	if len(f.subIssuesCalls) != 0 {
		t.Errorf("read sub-issues of a foreign parent: %v", f.subIssuesCalls)
	}
}

// The write is by NUMBER against this loop's owner/repo. A foreign child's
// number names a different issue here, so it must never be promoted.
func TestEpicSweepSkipsAChildInAnotherRepository(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{
		closedIssue(71),
		foreignIssue(73),
		openIssue(74),
	}
	f.blockers[74] = []ghub.Issue{closedIssue(71)}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	got := f.promotedNumbers()
	if len(got) != 1 || got[0] != 74 {
		t.Fatalf("promoted %v, want [74] only; 73 lives in another repository", got)
	}
	for _, n := range f.blockedByCalls {
		if n == 73 {
			t.Error("looked up blockers for a foreign sub-issue")
		}
	}
}

// Repo empty means "the response did not say", not "local".
func TestEpicSweepSkipsAChildWithNoRepository(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{
		closedIssue(71),
		{Number: 73, State: "open"},
	}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 0 {
		t.Errorf("promoted %v; an issue naming no repository must not read as local", got)
	}
}

// A child that cannot be promoted whatever its blockers say must not cost a
// call. This is the filter epic.NeedsBlockers exists for.
func TestEpicSweepSkipsTheBlockerLookupItDoesNotNeed(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{
		closedIssue(71),
		openIssue(74, "status:plan-ready-for-review"),
		openIssue(75, "blocked:legal"),
		closedIssue(76),
		openIssue(77),
	}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if len(f.blockedByCalls) != 1 || f.blockedByCalls[0] != 77 {
		t.Errorf("blocked_by calls = %v, want only [77]", f.blockedByCalls)
	}
}

// One child's failure must not cost the others their promotion, and the child
// that failed must NOT be promoted.
func TestEpicSweepContinuesPastAFailedBlockerRead(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73), openIssue(74)}
	f.blockerErr[73] = errors.New("502 bad gateway")
	f.blockers[74] = []ghub.Issue{closedIssue(71)}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	got := f.promotedNumbers()
	if len(got) != 1 || got[0] != 74 {
		t.Fatalf("promoted %v, want [74] only", got)
	}
	// The point of the test, stated as its own assertion: the child whose
	// blockers could not be read is HELD, not promoted. Without this line the
	// test passes even if BlockersUnknown is ignored entirely.
	if _, ok := f.added[73]; ok {
		t.Error("promoted 73 whose blocker list could not be read; it must be held")
	}
}

func TestEpicSweepContinuesPastAFailedLabelWrite(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73), openIssue(74)}
	f.blockers[73] = []ghub.Issue{closedIssue(71)}
	f.blockers[74] = []ghub.Issue{closedIssue(71)}
	f.labelErr[73] = errors.New("422 unprocessable")

	sum, err := EpicSweep(context.Background(), cfg, deps, 71)
	if err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 1 || got[0] != 74 {
		t.Fatalf("promoted %v, want [74]", got)
	}
	// The count must reflect what LANDED, not what was attempted.
	if sum.Promoted != 1 {
		t.Errorf("Summary.Promoted = %d, want 1", sum.Promoted)
	}
}

// The cap takes the low-numbered batch, so the next sweep takes the next one.
func TestEpicSweepCapsOneSweepAndTakesTheLowBatch(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	f.parents[1] = epicParent(69)
	kids := []ghub.Issue{closedIssue(1)}
	for n := 100; n < 100+maxPromotePerSweep+5; n++ {
		kids = append(kids, openIssue(n))
	}
	f.children[69] = kids

	sum, err := EpicSweep(context.Background(), cfg, deps, 1)
	if err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if sum.Promoted != maxPromotePerSweep {
		t.Fatalf("Promoted = %d, want the cap %d", sum.Promoted, maxPromotePerSweep)
	}
	got := f.promotedNumbers()
	if got[0] != 100 || got[len(got)-1] != 100+maxPromotePerSweep-1 {
		t.Errorf("capped batch = %v, want the low-numbered %d", got, maxPromotePerSweep)
	}
}

// Nothing sweeps when the derivation cannot name exactly one entry loop. This
// is the guard that stops the execution loop promoting past the planning stage.
func TestEpicSweepRefusesWhenThisLoopIsNotTheEntry(t *testing.T) {
	cfg, deps, f := executionFixture(t)
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73)}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep must not error when it simply does not sweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 0 {
		t.Fatalf("the execution loop promoted %v; only the entry loop may sweep", got)
	}
}

// An unresolvable derivation is a no-op, NOT an error. It is a permanent
// misconfiguration: returning an error would schedule retries of something no
// retry can fix. Three ways it can be unresolvable, all of them a quiet skip.
func TestEpicSweepIsANoOpWhenTheEntryLoopCannotBeResolved(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
	}{
		{
			// Every loop downstream of another: a cycle.
			name: "no entry loop",
			files: map[string]string{
				"planning.yaml":  loopFile("planning", "status:x", "status:review-a", "status:y"),
				"execution.yaml": loopFile("execution", "status:y", "status:review-b", "status:x"),
			},
		},
		{
			name: "two entry loops",
			files: map[string]string{
				"planning.yaml":  loopFile("planning", "status:ready-for-spec", "status:review-a", ""),
				"execution.yaml": loopFile("execution", "status:ready-for-other", "status:review-b", ""),
			},
		},
		{
			name: "a loop file that does not load",
			files: map[string]string{
				"planning.yaml": loopFile("planning",
					"status:ready-for-spec", "status:plan-ready-for-review", "status:ready-for-execution"),
				"broken.yaml": "name: broken\nrepo: o/r\nthis is not: [valid",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, deps, f, _ := fixtureWithFiles(t, "planning", tc.files)
			f.parents[71] = epicParent(69)
			f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73)}

			if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
				t.Fatalf("an unresolvable derivation must not be an error: %v", err)
			}
			if got := f.promotedNumbers(); len(got) != 0 {
				t.Fatalf("promoted %v with no resolvable entry loop", got)
			}
		})
	}
}

// A config path outside a .agent-utils directory leaves DirFromPath empty. This
// is live in production, not hypothetical: tick_test.go's newDeps defaults
// ConfigPath to /tmp/loop.yaml.
func TestEpicSweepIsANoOpWithNoProjectDirectory(t *testing.T) {
	cfg, deps, f := sweepFixture(t)
	deps.ConfigPath = "/tmp/loop.yaml"
	f.parents[71] = epicParent(69)
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73)}

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err != nil {
		t.Fatalf("EpicSweep: %v", err)
	}
	if got := f.promotedNumbers(); len(got) != 0 {
		t.Fatalf("promoted %v with no locatable project directory", got)
	}
}

// The cap bounds the whole PASS, not one epic. Anyone with triage can apply the
// epic label, so a per-epic cap would let the number of epics multiply the
// write authority.
func TestEpicSweepAllCapsTheWholePass(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	// Two epics, each with more unblocked children than the whole cap allows.
	gh.issues = []ghub.Issue{epicParent(10), epicParent(20)}
	for _, parent := range []int{10, 20} {
		var kids []ghub.Issue
		for i := 0; i < maxPromotePerSweep; i++ {
			kids = append(kids, openIssue(parent*1000+i))
		}
		f.children[parent] = kids
	}

	sum, err := EpicSweepAll(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("EpicSweepAll: %v", err)
	}
	if sum.Promoted != maxPromotePerSweep {
		t.Fatalf("Promoted = %d across two epics, want the pass cap %d",
			sum.Promoted, maxPromotePerSweep)
	}
}

// One unreadable epic must not abandon the rest of the pass.
func TestEpicSweepAllContinuesPastAFailedEpic(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{epicParent(10), epicParent(20)}
	f.subErr[10] = errors.New("502 bad gateway")
	f.children[20] = []ghub.Issue{closedIssue(71), openIssue(73)}
	f.blockers[73] = []ghub.Issue{closedIssue(71)}

	sum, err := EpicSweepAll(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("EpicSweepAll: %v", err)
	}
	if sum.Promoted != 1 {
		t.Fatalf("Promoted = %d, want 1 from the epic that could be read", sum.Promoted)
	}
}

// A nil reader is what a fake Client that does not implement EpicReader leaves
// behind. Refusing beats panicking inside a daemon.
func TestEpicSweepRefusesWithNoReader(t *testing.T) {
	cfg, deps, _ := sweepFixture(t)
	deps.Epic = nil

	if _, err := EpicSweep(context.Background(), cfg, deps, 71); err == nil {
		t.Fatal("want an error when Deps.Epic is nil, got nil")
	}
}

// The cron path. It enters at the epic rather than at a closed child, and every
// step after that is shared.
func TestEpicSweepAllWalksEveryOpenEpic(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{
		epicParent(69),
		openIssue(73),
		openIssue(90, "enhancement"),
	}
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73)}
	f.blockers[73] = []ghub.Issue{closedIssue(71)}

	sum, err := EpicSweepAll(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("EpicSweepAll: %v", err)
	}
	if sum.Promoted != 1 {
		t.Fatalf("Promoted = %d, want 1", sum.Promoted)
	}
	if got := f.promotedNumbers(); len(got) != 1 || got[0] != 73 {
		t.Errorf("promoted %v, want [73]", got)
	}
}
```

The file's imports are `context`, `errors`, `fmt`, `os`, `path/filepath`, `sort`, `sync`,
`testing`, `internal/config`, and `internal/ghub`.

- [ ] **Step 3: Run them and confirm they fail**

Run: `go test ./internal/loopcmd/ -run TestEpicSweep`
Expected: FAIL — `undefined: EpicSweep`.

- [ ] **Step 4: Write `internal/loopcmd/epicsweep.go`**

```go
package loopcmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/epic"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/lock"
)

// maxPromotePerSweep is how many issues one sweep may promote.
//
// It is higher than maxTendPerSweep, which is 10, because the two cap different
// things. A tend decision is an agent process in a git worktree with permission
// prompts disabled; a promotion is one label write. The cost of this batch is
// 25 API calls, and the cost of tending's is 10 agents.
//
// It bounds the whole PASS, not one epic. EpicSweepAll walks every open issue
// carrying the epic label, and anyone with triage can apply that label: a cap
// applied per epic would let N epics authorise 25 x N writes on one tick, which
// is the unbounded repository-wide fan-out this design exists to avoid. The
// budget is therefore threaded through, not re-read.
//
// It is a constant rather than a configuration field for the reason
// maxTendPerSweep is: no operator has needed a different value. What is left
// over is logged and NAMED, never dropped silently, and the next sweep takes
// the next batch.
const maxPromotePerSweep = 25

// EpicSweep promotes the sub-issues that the closure of one issue unblocked.
//
// closed is the issue a delivery reported closed. The sweep starts there, walks
// up to its parent, and considers that parent's children. It dispatches NO
// agent and spends no tokens: its whole output is label writes.
//
// # Why this may act on many issues when TickIssue may not
//
// Worker.RunIssue records that a full reconcile per delivery was removed
// because it burned a token budget on every open issue of every project
// watching the repository. This pass acts on many issues again, so it must not
// become that. Four things keep it apart, and the first is the one that
// matters:
//
//  1. It dispatches no agent. The removed pass was expensive because it started
//     agents. This one writes labels.
//  2. It runs for ONE event -- an issue closing -- and only when that issue's
//     parent carries the epic label. Opening an issue, moving a label and
//     commenting arm nothing.
//  3. Its fan-out is the epic's own children, not the repository.
//  4. It is capped at maxPromotePerSweep.
//
// # Where the lock is taken
//
// This function TAKES the loop lock, around the writes only. The reads happen
// before it, for the reason TendSweep documents at length: Worker.issuePass
// drops a delivery that finds the lock held, with no retry, so every second
// this pass holds it is a second in which a labelled issue can be dropped and
// never picked up.
//
// EpicSweepAll does NOT take it, because its caller already holds it. See that
// function.
//
// # Why a GitHub-only write needs the LOOP lock at all
//
// Nothing local is written here, so the lock is not protecting a file or a
// database row. It is protecting an ORDERING. The label this pass adds is the
// trigger label, and the next tick to see it dispatches an agent. A tick
// running concurrently with this pass would read the issue list either side of
// the write and, in the "before" case, decide nothing for an issue that is
// about to become dispatchable -- harmless -- or, worse, race a park that is
// removing the same label. Taking the lock makes the promotion and the
// dispatch decision serial, which is the property the rest of this package
// already relies on.
func EpicSweep(ctx context.Context, cfg *config.Config, deps Deps, closed int) (Summary, error) {
	var sum Summary
	if deps.Epic == nil {
		return sum, errors.New("epic sweep: no EpicReader; Deps.Epic is nil")
	}
	if !isEntryLoop(cfg, deps) {
		return sum, nil
	}

	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	// One call, and for almost every delivery it is the only one. Most issues
	// in most repositories have no parent, so this is the fast exit.
	parent, err := deps.Epic.Parent(ctx, owner, repo, closed)
	if err != nil {
		if errors.Is(err, ghub.ErrNoParent) {
			return sum, nil
		}
		// NOT treated as "no parent". A failure says nothing about whether the
		// issue belongs to an epic, and both readings are wrong to assume.
		return sum, fmt.Errorf("epic sweep: read parent of #%d: %w", closed, err)
	}
	if !parent.HasLabel(epic.Label) {
		return sum, nil
	}
	// A parent in ANOTHER repository is not this loop's epic. Its children are
	// read from, and written to, this loop's owner/repo, so a foreign parent
	// would expand whichever LOCAL issue shares its number. GitHub permits a
	// cross-repository parent, so this is reachable, not theoretical.
	if !parent.InRepo(owner, repo) {
		slog.Info("epic sweep skipped: the parent lives in another repository",
			"loop", cfg.Name, "issue", closed, "parent", parent.Number, "parent_repo", parent.Repo)
		return sum, nil
	}

	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		return sum, fmt.Errorf("epic sweep: lock loop %s: %w", cfg.Name, err)
	}
	defer l.Release()

	budget := maxPromotePerSweep
	return sweepEpic(ctx, cfg, deps, parent.Number, &budget)
}

// EpicSweepAll runs the sweep for every open epic of the repository.
//
// It is the CRON path. A webhook delivery can be missed -- the daemon can be
// down, the proxy can be down, GitHub can drop one -- and a missed close is a
// sub-issue that waits forever with no sign that anything is wrong. This pass
// finds it on the next tick. The daemon is the fast path; this is the backstop.
//
// It enters at the epic instead of at a closed child. Every step after the
// entry is shared with EpicSweep, so the two cannot decide differently.
//
// # It does NOT take the loop lock
//
// Its only caller is Tick, and Tick's production caller RunTick already holds
// that lock (internal/loopcmd/open.go:205). flock is per open-file-description,
// so acquiring it again in the same process returns ErrHeld -- which would make
// this backstop silently promote nothing, forever, which is precisely the
// failure it exists to prevent. This is the same reason Tick itself takes no
// lock and TendSweep does: TendSweep's caller does not hold one.
func EpicSweepAll(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error) {
	var sum Summary
	if deps.Epic == nil {
		return sum, errors.New("epic sweep: no EpicReader; Deps.Epic is nil")
	}
	if !isEntryLoop(cfg, deps) {
		return sum, nil
	}

	owner, repo := cfg.RepoOwner(), cfg.RepoName()
	issues, err := deps.GH.ListOpenIssues(ctx, owner, repo)
	if err != nil {
		return sum, fmt.Errorf("epic sweep: list open issues: %w", err)
	}

	// One budget for the whole pass. See maxPromotePerSweep: a per-epic cap
	// would let the number of epics multiply the write authority, and applying
	// the epic label costs an attacker one triage permission.
	budget := maxPromotePerSweep
	var swept int
	for _, iss := range issues {
		if !iss.HasLabel(epic.Label) {
			continue
		}
		// ListOpenIssues returns this repository's issues, so InRepo is
		// redundant here today. It is checked anyway, because the write below
		// is by number and the guard must not depend on which listing fed it.
		//
		// It does couple this path to ConvertIssues carrying Repo for the LIST
		// endpoint, not only the three epic ones. GitHub populates
		// repository_url on GET /repos/{o}/{r}/issues, so this holds -- and
		// TestListOpenIssuesCarriesTheRepository below pins it, because if it
		// ever stopped holding, this backstop would promote nothing and say
		// nothing, which is the failure this whole design is built to avoid.
		if !iss.InRepo(owner, repo) {
			continue
		}
		if budget <= 0 {
			slog.Warn("epic sweep hit its per-pass cap; the remaining epics wait for the next tick",
				"loop", cfg.Name, "promoted", sum.Promoted, "epics_swept", swept)
			break
		}
		swept++
		one, err := sweepEpic(ctx, cfg, deps, iss.Number, &budget)
		sum.Promoted += one.Promoted
		if err != nil {
			// One unreadable epic must not abandon the rest. If this returned,
			// anyone able to label an issue `epic` could stall every promotion
			// the loop would otherwise make.
			//
			// ErrHeld is not a failure, and must not be logged as one: the spec
			// states it is a skip. It cannot arise on this path today -- the
			// caller holds the lock and this function takes none -- but a bare
			// Warn here would be wrong the moment that changes.
			if errors.Is(err, lock.ErrHeld) {
				slog.Info("epic sweep skipped: another tick holds the loop lock",
					"loop", cfg.Name, "epic", iss.Number)
				continue
			}
			slog.Warn("epic sweep failed for one epic; continuing",
				"loop", cfg.Name, "epic", iss.Number, "err", err)
			continue
		}
	}
	return sum, nil
}

// sweepEpic considers the children of one epic and promotes what it may.
// Both drivers call it, so neither can decide differently from the other.
//
// The CALLER holds the loop lock. sweepEpic takes none: EpicSweep acquires it
// and Tick's caller already holds it, so acquiring here would deadlock the cron
// path against its own caller.
//
// budget is the number of promotions the whole PASS may still make. It is a
// pointer because EpicSweepAll spends one budget across many epics.
func sweepEpic(
	ctx context.Context, cfg *config.Config, deps Deps, parent int, budget *int,
) (Summary, error) {
	var sum Summary
	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	kids, err := deps.Epic.SubIssues(ctx, owner, repo, parent)
	if err != nil {
		return sum, fmt.Errorf("epic sweep: read sub-issues of #%d: %w", parent, err)
	}

	children := make([]epic.Child, 0, len(kids))
	for _, kid := range kids {
		// A child in ANOTHER repository is skipped before anything else. The
		// promotion below writes a label by NUMBER against this loop's
		// owner/repo, so a foreign child's number would label whichever LOCAL
		// issue happens to carry it -- an unrelated issue, moved into the
		// pipeline by a relation someone created in a repository this operator
		// may not control. GitHub permits a cross-repository sub-issue, so this
		// is reachable. An issue whose repository the response did not name is
		// also skipped: InRepo answers false for "unknown", which is the safe
		// direction when the alternative is labelling the wrong issue.
		if !kid.InRepo(owner, repo) {
			slog.Info("skipping a sub-issue outside this repository",
				"loop", cfg.Name, "epic", parent, "issue", kid.Number, "issue_repo", kid.Repo)
			continue
		}
		// A number GitHub could not have. The handler validates the identically
		// sourced value on the way in; this is the same check on the way out,
		// because this one is a WRITE target.
		if kid.Number <= 0 {
			slog.Warn("skipping a sub-issue with an impossible number",
				"loop", cfg.Name, "epic", parent, "number", kid.Number)
			continue
		}
		// The filter is an OPTIMIZATION, not the rule: it saves a call for a
		// child that cannot be promoted whatever its blockers say. Promote
		// tests the same conditions again and is the only place the decision is
		// made. See epic.NeedsBlockers.
		if !epic.NeedsBlockers(kid, cfg.Labels.Veto) {
			continue
		}
		c := epic.Child{Issue: kid}
		c.Blockers, err = deps.Epic.BlockedBy(ctx, owner, repo, kid.Number)
		if err != nil {
			// Held, not dropped and not promoted. An unreadable blocker list
			// says nothing, and the alternative reading promotes an issue whose
			// blockers may still be open. One unusable child must not abandon
			// the sweep, for the reason Tick gives about one unusable pull
			// request: anyone able to open an issue could otherwise stall the
			// loop.
			slog.Warn("cannot read blockers; holding this sub-issue",
				"loop", cfg.Name, "epic", parent, "issue", kid.Number, "err", err)
			c.BlockersUnknown = true
		}
		children = append(children, c)
	}

	promote := epic.Promote(children, cfg.Labels.Veto, owner, repo)
	if len(promote) == 0 {
		return sum, nil
	}

	var deferred []int
	if len(promote) > *budget {
		deferred = promote[*budget:]
		promote = promote[:*budget]
	}

	for _, n := range promote {
		if err := deps.Epic.EditLabels(ctx, owner, repo, n,
			[]string{cfg.Labels.Trigger}, nil); err != nil {
			// One failed write must not abandon the rest. The next close
			// delivery, or the next cron tick, promotes this one: nothing about
			// the issue changed, so the rule still selects it.
			slog.Error("cannot promote sub-issue",
				"loop", cfg.Name, "epic", parent, "issue", n, "err", err)
			continue
		}
		sum.Promoted++
		*budget--
		// One line per promotion, naming the label. This is a GitHub write made
		// with no human and no agent in the loop, and the log is the only
		// record of it that lives on this machine.
		slog.Info("promoted an unblocked sub-issue",
			"loop", cfg.Name, "epic", parent, "issue", n, "label", cfg.Labels.Trigger)
	}

	if len(deferred) > 0 {
		// Never silent. A capped sweep that said nothing would read as "every
		// unblocked sub-issue was promoted", which is the opposite of the
		// truth. NAMED, not counted, so an operator can see which work waits --
		// but TRUNCATED, because the length is the child count of an epic
		// anyone with triage can grow. handler.go's safeLabels sets the shape:
		// at most a few, and a count of what did not fit.
		slog.Warn("epic sweep hit its cap; the rest wait for the next sweep",
			"loop", cfg.Name, "epic", parent, "promoted", sum.Promoted,
			"deferred", loggedNumbers(deferred), "deferred_total", len(deferred))
	}
	return sum, nil
}

// maxLoggedNumbers bounds an issue-number list carried into a log line.
const maxLoggedNumbers = 10

// loggedNumbers returns at most maxLoggedNumbers of ns.
//
// The caller logs len(ns) beside it, so nothing is hidden by the truncation --
// only moved from the line to a count, which is what handler.go's safeLabels
// does for a label list of attacker-controlled length.
func loggedNumbers(ns []int) []int {
	if len(ns) <= maxLoggedNumbers {
		return ns
	}
	return ns[:maxLoggedNumbers]
}

// isEntryLoop reports whether cfg is the one loop allowed to promote.
//
// A derivation that cannot name exactly one entry loop returns false, and the
// reason is logged. It is logged at WARN and not returned as an error because
// it is a permanent misconfiguration rather than a failed pass: returning an
// error would schedule retries of something no retry can fix, and every retry
// would log the same line again.
func isEntryLoop(cfg *config.Config, deps Deps) bool {
	dir := config.DirFromPath(deps.ConfigPath)
	if dir == "" {
		slog.Warn("epic sweep skipped: cannot locate the project directory",
			"loop", cfg.Name, "path", deps.ConfigPath)
		return false
	}
	name, err := config.EntryLoop(dir, cfg.Repo)
	if err != nil {
		slog.Warn("epic sweep skipped: cannot name the pipeline's entry loop",
			"loop", cfg.Name, "repo", cfg.Repo, "err", err)
		return false
	}
	return name == cfg.Name
}
```

- [ ] **Step 5: Run them and confirm they pass**

Run: `go test ./internal/loopcmd/ -run TestEpicSweep -v`
Expected: PASS, every case.

- [ ] **Step 6: Mutation-check the three safety properties**

Make each change, run the tests, see a FAIL, revert.

1. In `EpicSweep`, delete the `if !isEntryLoop(cfg, deps) { return sum, nil }` guard.
   Expected: `TestEpicSweepRefusesWhenThisLoopIsNotTheEntry` FAILS.
2. In `sweepEpic`, delete the `BlockersUnknown = true` assignment, leaving the warning.
   Expected: `TestEpicSweepContinuesPastAFailedBlockerRead` FAILS on its "must be held"
   assertion. (That assertion is shipped in Step 2, not added here — a guard this plan does
   not pin before the mutation check is a guard the suite does not protect.)
3. In `sweepEpic`, move `sum.Promoted++` above the `EditLabels` error check.
   Expected: `TestEpicSweepContinuesPastAFailedLabelWrite` FAILS on the count.
4. In `sweepEpic`, delete the `if !kid.InRepo(owner, repo)` guard.
   Expected: `TestEpicSweepSkipsAChildInAnotherRepository` and
   `TestEpicSweepSkipsAChildWithNoRepository` FAIL.
5. In `EpicSweep`, delete the `if !parent.InRepo(owner, repo)` guard.
   Expected: `TestEpicSweepStopsWhenTheParentIsInAnotherRepository` FAILS.
6. In `sweepEpic`, change `*budget` back to `maxPromotePerSweep` in the cap comparison.
   Expected: `TestEpicSweepAllCapsTheWholePass` FAILS with 50 promotions instead of 25.

- [ ] **Step 7: Run the gates and commit**

```bash
make check
git add internal/loopcmd/
git commit -m "$(cat <<'EOF'
feat(loopcmd): the epic dependency sweep

EpicSweep enters at a closed sub-issue and EpicSweepAll enters at the
epic, for the webhook and the cron path. Both share every step after the
entry, so the two cannot decide differently. Only the derived entry loop
sweeps, the reads happen outside the loop lock, and no agent is
dispatched.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: Wire both drivers

**Files:**
- Modify: `internal/listener/work.go`, `internal/listener/handler.go`, `internal/loopcmd/tick.go`
- Test: `internal/listener/work_test.go`, `internal/listener/handler_test.go`,
  `internal/loopcmd/tick_test.go`

**Interfaces:**
- Consumes: `loopcmd.EpicSweep`, `loopcmd.EpicSweepAll` (Task 4).
- Produces:
  - `Delivery.ClosedIssue bool`
  - `Worker.RunEpic func(ctx, cfg, deps, closed int) (loopcmd.Summary, error)`

- [ ] **Step 1: Write the failing listener tests**

Add to `internal/listener/work_test.go`, following the harness the file already uses:

```go
// sweptNumbers wires RunEpic to record the issues it was asked to sweep. The
// slice is guarded because Deliver fans out per target.
func sweptNumbers(h *harness) (*[]int, *sync.Mutex) {
	var mu sync.Mutex
	var swept []int
	h.w.RunEpic = func(_ context.Context, _ *config.Config, _ loopcmd.Deps, closed int) (loopcmd.Summary, error) {
		mu.Lock()
		defer mu.Unlock()
		swept = append(swept, closed)
		return loopcmd.Summary{}, nil
	}
	return &swept, &mu
}

// An issue closing is what unblocks its siblings. It is the ONLY event that
// starts an epic sweep.
func TestClosedIssueRunsTheEpicSweep(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	swept, mu := sweptNumbers(h)

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 71, ClosedIssue: true})

	mu.Lock()
	defer mu.Unlock()
	if len(*swept) != 1 || (*swept)[0] != 71 {
		t.Fatalf("epic sweep ran for %v, want [71]", *swept)
	}
}

func TestANonCloseDeliveryRunsNoEpicSweep(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	swept, mu := sweptNumbers(h)

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 71})

	mu.Lock()
	defer mu.Unlock()
	if len(*swept) != 0 {
		t.Fatalf("epic sweep ran for %v; only a closed issue starts one", *swept)
	}
}

// A merged pull request is not an issue close. ClosedPR arms the worktree
// cleanup and the tend sweep; it must arm nothing here.
func TestAMergedPullRequestRunsNoEpicSweep(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	swept, mu := sweptNumbers(h)

	h.w.Deliver(context.Background(), Delivery{
		Repo: "o/r", Number: 71, MergedInto: "master", ClosedPR: true,
	})

	mu.Lock()
	defer mu.Unlock()
	if len(*swept) != 0 {
		t.Fatalf("epic sweep ran for %v on a pull request delivery", *swept)
	}
}

// The closed issue's own pass is what moves ITS labels. The sweep is extra
// work, not a replacement, and a failing sweep must not cost the issue its
// pass. The order matters too: the pass runs first.
func TestTheClosedIssuesOwnPassStillRuns(t *testing.T) {
	h := newHarness(nil)
	h.targets = []Target{h.target("planning")}
	h.w.RunEpic = func(context.Context, *config.Config, loopcmd.Deps, int) (loopcmd.Summary, error) {
		return loopcmd.Summary{}, errBoom
	}

	h.w.Deliver(context.Background(), Delivery{Repo: "o/r", Number: 71, ClosedIssue: true})

	if got := h.ranNumbers(); len(got) != 1 || got[0] != 71 {
		t.Fatalf("the issue pass ran for %v, want [71]", got)
	}
	// A failed sweep schedules NO retry: the cron sweep re-derives the whole
	// thing from scratch, so there is nothing here for a retry to recover.
	if n := h.timers.len(); n != 0 {
		t.Errorf("armed %d retry timers for a failed sweep, want 0", n)
	}
}
```

**Note:** `ranNumbers()` already exists (`internal/listener/work_test.go:383`) and reports the
issue numbers the harness's `runIssue` seam saw, mutex-guarded. Do not add a second accessor for
the same data, and do not read `h.ranIssues` directly — `Deliver` fans out on goroutines, and
although `-race` is not part of `make check` (it is the separate `test/race` target, run before a
release and in CI), an unguarded read still fails there.

In `internal/listener/handler_test.go`, first add the field to `tickCall` (line 80) and to the
`newServer` seam (line 96), so a test can see it:

```go
type tickCall struct {
	repo        string
	number      int
	mergedInto  string
	closedPR    bool
	closedIssue bool
}
```

```go
		Tick: func(_ context.Context, d Delivery) {
			tickCh <- tickCall{repo: d.Repo, number: d.Number, mergedInto: d.MergedInto,
				closedPR: d.ClosedPR, closedIssue: d.ClosedIssue}
		},
```

Then add the table, modeled on the merge-delivery table already in the file:

```go
// The handler decides only WHAT the delivery is. It holds no GitHub client, so
// it cannot look up a parent and does not try: it sets one boolean and the
// sweep decides the rest.
func TestClosedIssueIsDerivedFromTheEventAndAction(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		payload string
		want    bool
	}{
		{
			name:    "an issue closing",
			event:   "issues",
			payload: `{"action":"closed","repository":{"full_name":"o/r"},"issue":{"number":7}}`,
			want:    true,
		},
		{
			name:    "an issue opening",
			event:   "issues",
			payload: `{"action":"opened","repository":{"full_name":"o/r"},"issue":{"number":7}}`,
			want:    false,
		},
		{
			name:    "a label moving on an issue",
			event:   "issues",
			payload: `{"action":"labeled","repository":{"full_name":"o/r"},"issue":{"number":7}}`,
			want:    false,
		},
		{
			// Issues and pull requests share a number space. A pull_request
			// delivery answered as an issue close would sweep the epic of
			// whichever ISSUE happens to carry the pull request's number.
			name:    "a pull request closing",
			event:   "pull_request",
			payload: `{"action":"closed","repository":{"full_name":"o/r"},"pull_request":{"number":7,"merged":true,"base":{"ref":"master"}}}`,
			want:    false,
		},
		{
			// issue_comment carries an issue object, and its action is
			// attacker-shaped text. Only the EVENT check separates this from a
			// real close.
			name:    "a comment whose payload claims a closed action",
			event:   "issue_comment",
			payload: `{"action":"closed","repository":{"full_name":"o/r"},"issue":{"number":7}}`,
			want:    false,
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
			if got.closedIssue != tc.want {
				t.Errorf("closedIssue = %v, want %v", got.closedIssue, tc.want)
			}
			if got.number != 7 {
				t.Errorf("number = %d, want 7", got.number)
			}
		})
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/listener/ -run 'ClosedIssue|EpicSweep'`
Expected: FAIL — `unknown field ClosedIssue`.

- [ ] **Step 3: Add the field and the seam**

In `internal/listener/work.go`, add to `Delivery` after `ClosedPR`:

```go
	// ClosedIssue is true when this delivery closed an ISSUE, not a pull
	// request. It is what arms the epic sweep.
	//
	// It is deliberately narrower than ClosedPR's counterpart: issues and pull
	// requests share a number space, so a pull_request delivery that set this
	// would sweep the epic of whichever issue happens to carry that number.
	// The event is checked as well as the action.
	ClosedIssue bool
```

Add to `Worker`, beside `RunTend`:

```go
	// RunEpic promotes the sub-issues that a closed issue unblocked. Production
	// wires it to loopcmd.EpicSweep, which takes the loop's lock itself.
	//
	// It runs for ONE delivery -- an issue closing -- because that is the only
	// event that unblocks anything. It dispatches no agent: its whole output is
	// label writes, which is what makes it safe for a delivery to act on more
	// than the issue it names. See loopcmd.EpicSweep.
	RunEpic func(ctx context.Context, cfg *config.Config, deps loopcmd.Deps, closed int) (loopcmd.Summary, error)
```

Wire it in `NewWorker`, beside `RunTend`:

```go
		RunEpic: loopcmd.EpicSweep,
```

In `tickOne`, after the `armTend` block and before the `ClosedPR` block:

```go
	// Runs after the issue's own pass, and independently of whether that pass
	// succeeded: the closed issue's pass moves ITS labels, and that says
	// nothing about the siblings it unblocked.
	//
	// It is NOT armed on a timer the way tending is. A tend sweep is delayed to
	// collapse a merge train into one batch of agents; this writes labels, so a
	// burst costs a few more API calls and nothing else. A delay would only
	// postpone the promotion.
	if d.ClosedIssue {
		w.epicPass(ctx, t, d, cfg, deps)
	}
```

And the pass itself, beside `cleanupPass`:

```go
// epicPass promotes the sub-issues the delivery's closed issue unblocked.
//
// A failure is logged and dropped. It schedules NO retry, for the same reason
// the cleanup pass does not: the cron sweep re-derives this from scratch on its
// next tick, so a missed promotion is recovered without any state kept here.
func (w *Worker) epicPass(
	ctx context.Context, t Target, d Delivery, cfg *config.Config, deps loopcmd.Deps,
) {
	sum, err := w.RunEpic(ctx, cfg, deps, d.Number)
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			slog.Info("skipping epic sweep: another tick holds the loop lock",
				"loop", cfg.Name, "project", t.ProjectName, "issue", d.Number)
			return
		}
		slog.Error("epic sweep failed", "loop", cfg.Name, "project", t.ProjectName,
			"issue", d.Number, "err", err)
		return
	}
	if sum.Promoted > 0 {
		slog.Info("epic sweep promoted sub-issues", "loop", cfg.Name,
			"project", t.ProjectName, "issue", d.Number, "promoted", sum.Promoted)
	}
}
```

- [ ] **Step 4: Derive `closedIssue` in the handler**

In `internal/listener/handler.go`, beside where `closedPR` is derived:

```go
		// The counterpart of closedPR, and deliberately not merged with it. An
		// issue and a pull request share a number space, so a pull_request
		// delivery answered as an issue close would sweep the epic of whichever
		// issue carries the pull request's number. The event is what tells them
		// apart, and it is checked here rather than downstream because this is
		// the only layer that has it.
		closedIssue := event == "issues" && body.Action == "closed"
```

Pass it into the `Delivery` the handler builds, as `ClosedIssue: closedIssue`.

- [ ] **Step 5: Run the listener tests and confirm they pass**

Run: `go test ./internal/listener/`
Expected: PASS, including every pre-existing test.

- [ ] **Step 6: Wire the cron path**

In `internal/loopcmd/tick.go`, at the end of `Tick`, before the summary is recorded:

```go
	// The backstop. A webhook delivery can be missed -- the daemon down, the
	// proxy down, a delivery dropped -- and a missed close leaves a sub-issue
	// waiting forever with nothing to show that anything is wrong. This finds
	// it. A failure is logged and does not fail the tick: the tick's own work
	// is dispatch, and a sweep that could not read GitHub says nothing about
	// that.
	//
	// It runs AFTER the dispatch pass above, so an issue promoted here is
	// dispatched by the NEXT tick, not this one. That is deliberate: dispatch
	// decides from a snapshot read at the top of this function, and promoting
	// into that snapshot would mean deciding from a repository state that no
	// single read ever saw. One tick of latency on the backstop path costs
	// nothing -- the webhook path has none.
	//
	// EpicSweepAll takes NO lock. RunTick already holds it; see that function.
	if epicSum, err := EpicSweepAll(ctx, cfg, deps); err != nil {
		slog.Warn("epic sweep failed", "loop", cfg.Name, "err", err)
	} else {
		sum.Promoted += epicSum.Promoted
	}
```

- [ ] **Step 7: Add the cron-path test**

In `internal/loopcmd/tick_test.go`:

```go
// The cron tick is the backstop for a delivery the daemon never saw.
func TestTickRunsTheEpicSweep(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	// Built with the helpers, which set Repo. A bare ghub.Issue literal has an
	// empty Repo, and InRepo answers false for that, so the sweep would skip
	// every one of them and this test would fail for a reason that has nothing
	// to do with what it is testing.
	gh.issues = []ghub.Issue{epicParent(69), openIssue(73)}
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73)}
	f.blockers[73] = []ghub.Issue{closedIssue(71)}

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if sum.Promoted != 1 {
		t.Fatalf("Summary.Promoted = %d, want 1", sum.Promoted)
	}
	if got := f.promotedNumbers(); len(got) != 1 || got[0] != 73 {
		t.Errorf("promoted %v, want [73]", got)
	}
}

// The self-deadlock regression. RunTick holds the loop lock and then calls
// Tick, so anything Tick calls that acquires the SAME lock gets ErrHeld and
// promotes nothing, forever. Calling Tick directly cannot catch that; this
// calls RunTick, which is what cron runs.
func TestRunTickRunsTheEpicSweepWithoutDeadlocking(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{epicParent(69)}
	f.children[69] = []ghub.Issue{closedIssue(71), openIssue(73)}
	f.blockers[73] = []ghub.Issue{closedIssue(71)}

	sum, err := RunTick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("RunTick: %v", err)
	}
	if sum.Promoted != 1 {
		t.Fatalf("Promoted = %d, want 1; a sweep that self-deadlocks reports 0", sum.Promoted)
	}
}

// A sweep that cannot read GitHub must not cost the tick its dispatch work.
// The tick's job is dispatch; a failed sweep says nothing about that.
//
// The failure is injected at SubIssues, not at BlockedBy: a failed blocker read
// is handled inside sweepEpic (it holds the child and returns nil), so it never
// reaches Tick's error branch and would test nothing here.
func TestTickSurvivesAFailedEpicSweep(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{
		epicParent(69),
		openIssue(1, cfg.Labels.Trigger),
	}
	f.subErr[69] = errors.New("502 bad gateway")

	sum, err := Tick(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("Tick must not fail because a sweep did: %v", err)
	}
	if sum.Started != 1 {
		t.Errorf("Started = %d, want 1; the tick's own work must still happen", sum.Started)
	}
	if sum.Promoted != 0 {
		t.Errorf("Promoted = %d, want 0", sum.Promoted)
	}
}
```

Two notes for the implementer:

- `sweepAllFixture` sets `cfg.Labels.Trigger` to `status:ready-for-spec`, so the second test's
  issue 1 carries that label rather than `tickConfig`'s bare `trigger`.
- `internal/loopcmd/tick_test.go` currently imports `context`, `fmt`, `path/filepath`, `strings`,
  `testing`, `time`, plus `config`, `ghub`, `store`, and `worktree`. **Add `errors`.**

- [ ] **Step 8: Run the full suite**

Run: `make check`
Expected: PASS. Every pre-existing test in every package still passes; nothing in this task
changes an existing signature.

- [ ] **Step 9: Commit**

```bash
git add internal/listener/ internal/loopcmd/
git commit -m "$(cat <<'EOF'
feat: sweep an epic when one of its sub-issues closes

The handler sets ClosedIssue on an `issues` delivery with action closed,
checking the event as well as the action because issues and pull requests
share a number space. loopcmd.Tick runs the same sweep from the epic end,
as the backstop for a delivery the daemon never saw.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: Document the behavior

**Files:**
- Modify: `README.md`, `docs/configuration.md`

**Interfaces:**
- Consumes: everything above. Produces no code.

**This change makes three existing statements false.** Correcting them is the point of this task;
the new section is the smaller half. Commit `7c1ca40` exists because a previous change added a
second case to a documented "exactly one" and did not correct the sentence.

- [ ] **Step 1: Correct the three statements that this change falsifies**

`README.md:9` — "The agent owns every judgement and every GitHub write but one (see
[Security](#security))." There are now two. Replace with:

```markdown
retries, backoff, the circuit breaker. The agent owns every judgement and every GitHub write but
two: the retry-cap park, and the epic sweep's promotion (see [Epics](#epics)). Cron does the
scheduling; the engine has no timer.
```

`docs/configuration.md:370-371` — "**The loop writes a label exactly once**, in one situation:
when an issue exhausts its retry budget. Everything else is the agent's to apply. See
`retry.max`." Replace with:

```markdown
**The loop writes a label in two situations**, and never in any other. It applies `blocked` when
an issue exhausts its retry budget — see `retry.max`. And it applies `trigger` to a sub-issue of
an epic that a closing sibling unblocked — see [Epics](../README.md#epics), which also explains
why only one loop of a repository ever does that. Everything else is the agent's to apply.
```

`docs/configuration.md:374-380` — `### labels.trigger — required` says "The 'go' signal. **You**
apply it." Amend that line to:

```markdown
The "go" signal. **You** apply it — and so does the epic sweep, for the loop at the front of the
pipeline, when a sub-issue's blockers all close. It means both "start this" and "resume this",
and it never means "approved".
```

- [ ] **Step 2: Add a README section**

Add after the `Webhooks` section, before `Upgrading`:

```markdown
## Epics

An epic is an issue carrying the `epic` label whose sub-issues are the work. When a sub-issue
closes, the loop promotes every sibling that closure unblocked: it adds the pipeline's first
trigger label — `status:ready-for-spec` in the reference setup — and nothing else.

It reads GitHub's own relations, so there is nothing to write in an issue body. A sub-issue is a
sub-issue, and a dependency is `blocked_by`. A sibling is promoted when it is open, every issue in
its `blocked_by` list is closed, and it carries no `status:` label of its own. That last condition
is what leaves work already in flight alone, and it is why running the sweep twice promotes
nothing the second time.

**No agent runs.** The sweep is deterministic Go and its whole output is label writes. It is the
second GitHub write in this program that no agent makes; the first is the retry-cap park, which
`docs/configuration.md` documents under `retry.max`.

**It is scoped to one repository, end to end.** A sub-issue in another repository is skipped, and
so is an epic whose parent lives in another repository: the promotion is a label write addressed
by issue number, and a foreign number names a different issue here. A *blocker* in another
repository is **ignored** — it cannot hold a sub-issue back. That last one is a deliberate
trade: a sub-issue whose only remaining blocker is out-of-repo gets promoted while that blocker
is still open. Keep a dependency inside the repository if you want the sweep to wait for it.

**Who this trusts.** The sweep acts on relations a person with `triage` on the repository built:
the `epic` label, the sub-issue links, and the `blocked_by` edges. Anyone who can create those can
already apply `status:ready-for-spec` by hand, so the sweep adds no authority they did not have —
with one honest exception. The *author* of a sub-issue can close their own issue without holding
`triage`, and a blocker in another repository is closed by whoever controls that repository. In
both cases someone outside this repository chooses the *moment* a promotion happens. They do not
choose *which* issues are promoted: that was fixed when a maintainer put those issues in the epic
and declared the dependency. Point a loop only at a repository whose issue population you trust —
see [Security](#security).

**There is nothing to configure.** The `epic` label on the parent is the only switch. One loop per
repository does the promoting — the one at the front of the pipeline, which is the loop whose
trigger label is no other loop's terminal or review label. In the reference pair that is
`planning`, because `planning`'s terminal label is `execution`'s trigger. If that cannot be
resolved to exactly one loop, no loop sweeps and the reason is logged.

Two things drive it: an `issues` delivery reporting a close, and `loop tick` under cron, which
walks every open epic. The delivery is the fast path and cron is the backstop for a delivery that
never arrived.

The sweep never removes a label, never comments, never closes an issue, and never writes a
dependency. Entering an epic's graph is a human's job, or an agent's, and both use the GitHub UI
or its API.
```

- [ ] **Step 3: Run the docs test**

`internal/config/docs_test.go`'s `TestEveryConfigFieldIsDocumented` reflects over `Config`'s yaml
tags and fails when a dotted field path is missing from `docs/configuration.md` in backticks.
This change adds **no** yaml field, so the test is unaffected — but it must still be run, because
Step 1 edits that file. Run: `go test ./internal/config/`
Expected: PASS.

- [ ] **Step 4: Run the gates and commit**

```bash
make check
git add README.md docs/configuration.md
git commit -m "$(cat <<'EOF'
docs: record the epic sweep, and the second label the loop writes

README and docs/configuration.md both claimed the loop writes a label in
exactly one situation. The epic sweep is the second, so both statements
were false the moment it landed. Corrected alongside the new section.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Pipeline State

| Field   | Value                                                              |
|---------|--------------------------------------------------------------------|
| stage   | 2 (plan review)                                                    |
| class   | large (new subsystem + a second non-agent GitHub write authority)  |
| profile | backend                                                            |
| branch  | feat/epic-dependency-sweep                                         |
| pr      | #10                                                                |
| gate    | pending                                                            |
| round   | 0                                                                  |
