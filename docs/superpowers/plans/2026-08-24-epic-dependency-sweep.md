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

A `blocked_by` entry can live in **another repository**. Its `state` is in the same response, so
no second call and no second client is needed.

**go-github v77 coverage.**

- `SubIssueService.ListByIssue(ctx, owner, repo string, issueNumber int64, opts *IssueListOptions) ([]*SubIssue, *Response, error)`
  exists. `github.SubIssue` is an alias of `github.Issue`.
- There is **no** dependencies service and **no** parent accessor. Both need
  `g.c.NewRequest("GET", url, nil)` then `g.c.Do(ctx, req, &v)`.
- `github.ErrorResponse` carries `Response.StatusCode`. Match a 404 with
  `errors.As(err, &ge) && ge.Response != nil && ge.Response.StatusCode == 404`, as
  `internal/ghub/hooks.go:129` does.

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
- Consumes: `github.Client` from go-github v77; the package's existing `Issue` type and
  `newTestClient` test helper.
- Produces:
  - `ghub.Issue.State string` and `func (i Issue) IsOpen() bool`
  - `var ghub.ErrNoParent error`
  - `type ghub.EpicReader interface { Parent; SubIssues; BlockedBy; EditLabels }`
  - `func (g *GitHubClient) Parent(ctx context.Context, owner, repo string, number int) (Issue, error)`
  - `func (g *GitHubClient) SubIssues(ctx context.Context, owner, repo string, number int) ([]Issue, error)`
  - `func (g *GitHubClient) BlockedBy(ctx context.Context, owner, repo string, number int) ([]Issue, error)`

`Issue` gains `State` rather than a second issue type being introduced. A second type would need
its own copy of `HasLabel` and `HasAnyLabel`, and two label matchers that could disagree is the
exact hazard `convertPR`'s comment warns about.

- [ ] **Step 1: Write the failing test for `State` and `IsOpen`**

Add to `internal/ghub/ghub_test.go`:

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
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/ghub/ -run TestConvertIssuesCarriesState`
Expected: FAIL — `got[0].IsOpen undefined`.

- [ ] **Step 3: Add `State` and `IsOpen`**

In `internal/ghub/types.go`, add the field to `Issue` after `Labels`:

```go
	// State is "open" or "closed", as GitHub spells it.
	//
	// Every caller before the epic sweep listed OPEN issues only, so this was
	// not needed and was not carried. The sweep reads sub-issues and blockers,
	// and both lists mix open and closed, so the state has to survive the
	// conversion. Compare it with IsOpen rather than by ==: GitHub's spelling
	// is stable, but nothing here forces the case.
	State string
```

And the accessor, beside `HasLabel`:

```go
// IsOpen reports whether the issue is open. The comparison ignores case.
func (i Issue) IsOpen() bool { return strings.EqualFold(i.State, "open") }
```

In `internal/ghub/ghub.go`, inside `ConvertIssues`'s append, add `State: gi.GetState(),`.

- [ ] **Step 4: Run it and confirm it passes**

Run: `go test ./internal/ghub/ -run TestConvertIssuesCarriesState`
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
	// ConvertIssues drops pull requests, and a parent is never one. Passing
	// through it anyway keeps ONE mapping from a GitHub issue to this type, so
	// a field added there is carried by every reader.
	out := ConvertIssues([]*github.Issue{&gi})
	if len(out) == 0 {
		return Issue{}, fmt.Errorf("parent %s/%s#%d: %w", owner, repo, number, ErrNoParent)
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
	opts := &github.ListOptions{PerPage: 100}
	var all []Issue
	for {
		u, err := github.AddOptions(path, opts)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		req, err := g.c.NewRequest("GET", u, nil)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		var page []*github.Issue
		resp, err := g.c.Do(ctx, req, &page)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		all = append(all, ConvertIssues(page)...)
		if resp == nil || resp.NextPage == 0 {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
}

// isNotFound reports whether err is GitHub's 404.
func isNotFound(err error) bool {
	var ge *github.ErrorResponse
	return errors.As(err, &ge) && ge.Response != nil && ge.Response.StatusCode == 404
}
```

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
  - `func epic.Promote(children []Child, veto []string) []int`
  - `func epic.NeedsBlockers(child ghub.Issue, veto []string) bool`

This package is pure. It opens no socket, reads no clock, and keeps no state. Every rule the
sweep applies lives here, and the sweep applies none of its own.

- [ ] **Step 1: Write the failing tests**

Create `internal/epic/epic_test.go`:

```go
package epic

import (
	"reflect"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/ghub"
)

func open(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, State: "open", Labels: labels}
}

func closed(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, State: "closed", Labels: labels}
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
			// A foreign blocker is honored by its STATE. Nothing about the
			// repository it lives in reaches this function, by design.
			name: "a blocker in another repository is honored by state",
			children: []Child{{
				Issue:    open(74),
				Blockers: []ghub.Issue{{Number: 9, State: "open"}},
			}},
			want: nil,
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
			got := Promote(c.children, veto)
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
		promoted := len(Promote([]Child{{Issue: iss}}, veto)) == 1
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
//   - its blocker list was read, and every blocker in it is closed;
//   - it carries no status label;
//   - it carries none of the loop's veto labels.
//
// The result is ascending so that a capped sweep takes the low-numbered batch
// every time and the next sweep takes the next one. Without an order the batch
// identity would depend on GitHub's page order, and the same child could be
// deferred forever.
func Promote(children []Child, veto []string) []int {
	var out []int
	for _, c := range children {
		if !NeedsBlockers(c.Issue, veto) {
			continue
		}
		if c.BlockersUnknown {
			continue
		}
		if !unblocked(c.Blockers) {
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

// unblocked reports whether every blocker is closed. An empty list is
// unblocked: an issue that declares no dependency is waiting for nothing.
func unblocked(blockers []ghub.Issue) bool {
	for _, b := range blockers {
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
		return "", err
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
	for _, l := range loops {
		downstream := false
		for _, other := range loops {
			if other.name == l.name {
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

In `internal/loopcmd/open.go`, wherever `Deps.GH` is populated from the real client, populate
`Epic` from the same value. `*ghub.GitHubClient` satisfies both interfaces, so this is one extra
assignment and no new construction. Where `Options.GH` is supplied by the caller (the listener's
shared client), set `Epic` from it with a type assertion that leaves `Epic` nil when the value
does not implement `EpicReader`:

```go
	// The listener hands in one shared client per delivery. It is a
	// *ghub.GitHubClient in production and satisfies EpicReader, but Options.GH
	// is typed as the narrower ghub.Client, so the assertion is what carries it
	// through. A fake that does not implement EpicReader leaves this nil, and
	// EpicSweep refuses to run rather than panicking.
	if er, ok := deps.GH.(ghub.EpicReader); ok {
		deps.Epic = er
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
}

func newFakeEpic() *fakeEpic {
	return &fakeEpic{
		parents:    map[int]ghub.Issue{},
		children:   map[int][]ghub.Issue{},
		blockers:   map[int][]ghub.Issue{},
		blockerErr: map[int]error{},
		labelErr:   map[int]error{},
		added:      map[int][]string{},
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

func openIssue(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, State: "open", Labels: labels}
}

func closedIssue(n int, labels ...string) ghub.Issue {
	return ghub.Issue{Number: n, State: "closed", Labels: labels}
}

// epicParent is a parent issue carrying the epic label.
func epicParent(n int) ghub.Issue {
	return ghub.Issue{Number: n, State: "open", Labels: []string{"epic"}}
}

// writeLoopFiles creates a .agent-utils/configs directory holding a planning
// and an execution loop file, and returns the path of the one named `loop`.
//
// The files are REAL, because EpicSweep derives the entry loop by loading them.
// A fixture that faked the derivation would not test the guard that stops the
// execution loop promoting past the planning stage.
func writeLoopFiles(t *testing.T, loop string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), config.DirName)
	cfgDir := config.ConfigsDir(dir)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := func(name, trigger, review, terminal string) string {
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
	files := map[string]string{
		"planning.yaml": body("planning",
			"status:ready-for-spec", "status:plan-ready-for-review", "status:ready-for-execution"),
		"execution.yaml": body("execution",
			"status:ready-for-execution", "status:ready-for-review", ""),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(cfgDir, name), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(cfgDir, loop+".yaml")
}

// fixtureFor builds the config and deps for one loop of the reference pair.
//
// It reuses tickConfig and newDeps from tick_test.go rather than inventing a
// second fixture shape for this package. Only the three things the sweep reads
// are overridden: the labels, the config path (the entry-loop derivation walks
// up from it), and the reader.
func fixtureFor(t *testing.T, loop string) (*config.Config, Deps, *fakeEpic, *fakeGH) {
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
	deps.ConfigPath = writeLoopFiles(t, loop)

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
	if len(f.blockedByCalls) != 0 {
		t.Errorf("looked up blockers %v; an issue with no parent must cost one call",
			f.blockedByCalls)
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
	if len(f.blockedByCalls) != 0 {
		t.Errorf("read sub-issues of a parent that is not an epic: %v", f.blockedByCalls)
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
	"encoding/json"
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
// The GitHub reads happen BEFORE the lock and the writes happen under it, for
// the reason TendSweep documents at length: Worker.issuePass drops a delivery
// that finds the lock held, with no retry, so every second this pass holds it
// is a second in which a labelled issue can be dropped and never picked up.
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

	return sweepEpic(ctx, cfg, deps, parent.Number)
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
func EpicSweepAll(ctx context.Context, cfg *config.Config, deps Deps) (Summary, error) {
	var sum Summary
	if deps.Epic == nil {
		return sum, errors.New("epic sweep: no EpicReader; Deps.Epic is nil")
	}
	if !isEntryLoop(cfg, deps) {
		return sum, nil
	}

	issues, err := deps.GH.ListOpenIssues(ctx, cfg.RepoOwner(), cfg.RepoName())
	if err != nil {
		return sum, fmt.Errorf("epic sweep: list open issues: %w", err)
	}
	for _, iss := range issues {
		if !iss.HasLabel(epic.Label) {
			continue
		}
		one, err := sweepEpic(ctx, cfg, deps, iss.Number)
		if err != nil {
			// One unreadable epic must not abandon the rest. If this returned,
			// anyone able to label an issue `epic` could stall every promotion
			// the loop would otherwise make.
			slog.Warn("epic sweep failed for one epic; continuing",
				"loop", cfg.Name, "epic", iss.Number, "err", err)
			continue
		}
		sum.Promoted += one.Promoted
	}
	return sum, nil
}

// sweepEpic considers the children of one epic and promotes what it may.
// Both drivers call it, so neither can decide differently from the other.
func sweepEpic(ctx context.Context, cfg *config.Config, deps Deps, parent int) (Summary, error) {
	var sum Summary
	owner, repo := cfg.RepoOwner(), cfg.RepoName()

	kids, err := deps.Epic.SubIssues(ctx, owner, repo, parent)
	if err != nil {
		return sum, fmt.Errorf("epic sweep: read sub-issues of #%d: %w", parent, err)
	}

	children := make([]epic.Child, 0, len(kids))
	for _, kid := range kids {
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

	promote := epic.Promote(children, cfg.Labels.Veto)
	if len(promote) == 0 {
		return sum, nil
	}

	var deferred []int
	if len(promote) > maxPromotePerSweep {
		deferred = promote[maxPromotePerSweep:]
		promote = promote[:maxPromotePerSweep]
	}

	// The lock covers the WRITES only. See the function comment above.
	l, err := lock.Acquire(filepath.Join(cfg.StateDir, cfg.Name+".lock"))
	if err != nil {
		return sum, err
	}
	defer l.Release()

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
		// One line per promotion, naming the label. This is a GitHub write made
		// with no human and no agent in the loop, and the log is the only
		// record of it that lives on this machine.
		slog.Info("promoted an unblocked sub-issue",
			"loop", cfg.Name, "epic", parent, "issue", n, "label", cfg.Labels.Trigger)
	}

	if len(deferred) > 0 {
		// Never silent. A capped sweep that said nothing would read as "every
		// unblocked sub-issue was promoted", which is the opposite of the
		// truth. NAMED, not counted, so an operator can see which work waits.
		slog.Warn("epic sweep hit its per-sweep cap; the rest wait for the next sweep",
			"loop", cfg.Name, "epic", parent, "promoted", sum.Promoted, "deferred", deferred)
	}

	body, _ := json.Marshal(sum)
	slog.Info("epic sweep complete", "loop", cfg.Name, "epic", parent, "summary", string(body))
	return sum, nil
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
			"loop", cfg.Name, "config", deps.ConfigPath)
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
2. In `sweepEpic`, change the failed-`BlockedBy` branch to `continue` instead of setting
   `BlockersUnknown`.
   Expected: `TestEpicSweepContinuesPastAFailedBlockerRead` still passes — so ALSO add a case
   asserting the failed child is absent from `promotedNumbers()`, and confirm that one fails.
3. In `sweepEpic`, move `sum.Promoted++` above the `EditLabels` error check.
   Expected: `TestEpicSweepContinuesPastAFailedLabelWrite` FAILS on the count.

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

	if got := h.runCalls(); len(got) != 1 || got[0] != 71 {
		t.Fatalf("the issue pass ran for %v, want [71]", got)
	}
	// A failed sweep schedules NO retry: the cron sweep re-derives the whole
	// thing from scratch, so there is nothing here for a retry to recover.
	if n := h.timers.len(); n != 0 {
		t.Errorf("armed %d retry timers for a failed sweep, want 0", n)
	}
}
```

**Note:** `h.runCalls()` is named here for the issue numbers the harness's `runIssue` seam saw.
If the harness has no such accessor, add one beside `pendingLen()` — a mutex-guarded slice
appended to in `h.runIssue`. Do not read the field directly; `Deliver` fans out on goroutines
and `-race` is part of `make check`.

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
	gh.issues = []ghub.Issue{
		{Number: 69, State: "open", Labels: []string{"epic"}},
		{Number: 73, State: "open"},
	}
	f.children[69] = []ghub.Issue{
		{Number: 71, State: "closed"},
		{Number: 73, State: "open"},
	}
	f.blockers[73] = []ghub.Issue{{Number: 71, State: "closed"}}

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

// A sweep that cannot read GitHub must not cost the tick its dispatch work.
// The tick's job is dispatch; a failed sweep says nothing about that.
func TestTickSurvivesAFailedEpicSweep(t *testing.T) {
	cfg, deps, f, gh := sweepAllFixture(t)
	gh.issues = []ghub.Issue{
		{Number: 69, State: "open", Labels: []string{"epic"}},
		{Number: 1, State: "open", Labels: []string{cfg.Labels.Trigger}},
	}
	f.children[69] = []ghub.Issue{{Number: 73, State: "open"}}
	f.blockerErr[73] = errors.New("502 bad gateway")

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

`sweepAllFixture` sets `cfg.Labels.Trigger` to `status:ready-for-spec`, so the second test's
issue 1 carries that label rather than `tickConfig`'s bare `trigger`.

- [ ] **Step 8: Run the full suite**

Run: `make check`
Expected: PASS. Every pre-existing test in every package still passes; nothing in this task
changes an existing signature.

- [ ] **Step 9: Commit**

```bash
git add internal/listener/ internal/loopcmd/
git commit -m "$(cat <<'EOF'
feat(listener): sweep an epic when one of its sub-issues closes

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

- [ ] **Step 1: Add a README section**

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
second GitHub write in this program that no agent makes; the first is the retry-cap park (see
[Security](#security)).

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

- [ ] **Step 2: Add the `docs/configuration.md` note**

Under the `labels` reference, add:

```markdown
`labels.trigger` has a second reader. The epic sweep promotes an unblocked sub-issue by adding
this label, but only for the loop at the FRONT of the pipeline — the one whose `trigger` is no
other loop's `terminal` or `review`. Nothing enables this and nothing disables it; the `epic`
label on a parent issue is the only switch. See [Epics](../README.md#epics).
```

- [ ] **Step 3: Run the docs test**

`internal/config/docs_test.go` checks that documented fields exist. Run:
`go test ./internal/config/`
Expected: PASS.

- [ ] **Step 4: Run the gates and commit**

```bash
make check
git add README.md docs/configuration.md
git commit -m "$(cat <<'EOF'
docs: record when an epic sweep runs and what it writes

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
