# Polling Event Source Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `agent-utils` a second source of GitHub events -- a poller -- so a
repository nobody has ADMIN on, and therefore cannot be given a webhook, drives
the same loops in the same way a hooked repository does.

**Architecture:** `listener.Worker` is the runtime and does not care how an
event was sourced. Today one source feeds it: the HTTP handler, which turns a
verified GitHub delivery into a `listener.Delivery` and calls `Worker.Deliver`.
This adds a second: a poller that diffs GitHub's current state against a local
snapshot, turns each difference into the same `listener.Delivery`, and calls the
same `Worker.Deliver`. Nothing downstream of `Deliver` changes. A new
`listener poll [interval]` command runs the runtime with the poll source and no
HTTP server; `listener run` is untouched.

**Tech Stack:** Go 1.x, `urfave/cli/v3`, `google/go-github/v77`, SQLite via the
existing `internal/store`, standard-library testing with `httptest`.

**Spec:** `docs/superpowers/specs/2026-09-15-poll-mode-event-source-design.md`

## Global Constraints

- Every exported identifier gets a doc comment, and comments explain WHY a
  decision was made, not what the line does. This repo's existing comments are
  the standard to match -- see `internal/listener/closures.go`.
- Tests are standard-library Go. No testify, no mocks framework. Fakes are
  hand-written structs in `_test.go` files.
- TDD: write the failing test, watch it fail for the right reason, then
  implement. Commit at the end of each task.
- `gofmt` clean. `go build ./... && go test ./...` passes before every commit.
- The `ghub.Client` INTERFACE must not grow. New GitHub calls go on
  `*ghub.GitHubClient` and are reached through the poller's own narrow
  `PollSource` interface.
- `ghub.ConvertIssues` must not be modified. It drops pull requests and every
  caller depends on that.
- Repository names are stored and compared LOWERCASED. Two projects may spell
  one repository differently in their yaml.
- Timestamps are stored UTC.

---

### Task 1: The GitHub calls a poll needs

**Files:**
- Modify: `internal/ghub/types.go` (add `Subject`; add `Merged`/`State` to `PullRequest`)
- Modify: `internal/ghub/ghub.go` (add `ListSubjectsUpdatedSince`, `BranchHead`; set the two new `PullRequest` fields in `convertPR`)
- Test: `internal/ghub/poll_test.go` (new)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `ghub.Subject{Number int; IsPullRequest bool; State string; Labels []string; UpdatedAt time.Time}`
  - `func (g *GitHubClient) ListSubjectsUpdatedSince(ctx context.Context, owner, repo string, since time.Time) ([]Subject, error)`
  - `func (g *GitHubClient) BranchHead(ctx context.Context, owner, repo, branch string) (string, error)`
  - `ghub.PullRequest` gains `Merged bool` and `State string`

- [ ] **Step 1: Write the failing tests**

Create `internal/ghub/poll_test.go`:

```go
package ghub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-github/v77/github"
)

// A poll must see pull requests as well as issues: they share the endpoint
// because they share a number space, and three of the delivery flags the
// poller derives turn on which kind a subject is. ConvertIssues drops them,
// which is why this path does not use it.
func TestListSubjectsUpdatedSinceKeepsPullRequests(t *testing.T) {
	var gotQuery url.Values
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]*github.Issue{
			{
				Number:    github.Ptr(51),
				State:     github.Ptr("open"),
				Labels:    []*github.Label{{Name: github.Ptr("status:ready")}},
				UpdatedAt: &github.Timestamp{Time: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)},
			},
			{
				Number:      github.Ptr(52),
				State:       github.Ptr("closed"),
				UpdatedAt:   &github.Timestamp{Time: time.Date(2026, 9, 15, 11, 0, 0, 0, time.UTC)},
				PullRequestLinks: &github.PullRequestLinks{URL: github.Ptr("https://api.github.com/repos/o/r/pulls/52")},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	since := time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)
	got, err := newTestClient(t, srv).ListSubjectsUpdatedSince(context.Background(), "o", "r", since)
	if err != nil {
		t.Fatalf("ListSubjectsUpdatedSince: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (the pull request must not be dropped): %+v", len(got), got)
	}
	if got[0].Number != 51 || got[0].IsPullRequest {
		t.Errorf("subject[0] = %+v, want issue 51", got[0])
	}
	if got[1].Number != 52 || !got[1].IsPullRequest {
		t.Errorf("subject[1] = %+v, want pull request 52", got[1])
	}
	if got[0].State != "open" || got[1].State != "closed" {
		t.Errorf("states = %q, %q; want open, closed", got[0].State, got[1].State)
	}
	if len(got[0].Labels) != 1 || got[0].Labels[0] != "status:ready" {
		t.Errorf("labels = %v, want [status:ready]", got[0].Labels)
	}

	// The query is load-bearing: state=all is what makes a close visible at
	// all, and direction=asc is what lets the caller advance its cursor to the
	// last subject it fully processed after a mid-page failure.
	for key, want := range map[string]string{
		"state": "all", "sort": "updated", "direction": "asc",
		"since": "2026-09-15T09:00:00Z",
	} {
		if gotQuery.Get(key) != want {
			t.Errorf("query %s = %q, want %q", key, gotQuery.Get(key), want)
		}
	}
}

// A merge arms the tend sweep. Without this field the poller cannot tell a
// merged pull request from an abandoned one, and a merge would arm nothing.
func TestPullRequestCarriesMergedAndState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls/108", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		pr := insiderPR()
		pr.State = github.Ptr("closed")
		pr.Merged = github.Ptr(true)
		_ = json.NewEncoder(w).Encode(pr)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).PullRequest(context.Background(), "o", "r", 108)
	if err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
	if !got.Merged {
		t.Error("Merged = false, want true")
	}
	if got.State != "closed" {
		t.Errorf("State = %q, want closed", got.State)
	}
	if got.BaseRef != "master" {
		t.Errorf("BaseRef = %q, want master", got.BaseRef)
	}
}

// A direct push to the default branch produces no pull request event at all,
// so the poller watches the branch itself. BehindBy answers a different
// question -- how far one ref trails another -- and cannot report identity.
func TestBranchHeadReturnsTheCommitSHA(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/commits/master", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&github.RepositoryCommit{SHA: github.Ptr("deadbeef")})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	got, err := newTestClient(t, srv).BranchHead(context.Background(), "o", "r", "master")
	if err != nil {
		t.Fatalf("BranchHead: %v", err)
	}
	if got != "deadbeef" {
		t.Errorf("BranchHead = %q, want deadbeef", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/ghub/ -run 'Subjects|Merged|BranchHead' -v`
Expected: compile failure -- `ListSubjectsUpdatedSince`, `BranchHead`, `Merged` and `State` are undefined.

- [ ] **Step 3: Add the types**

In `internal/ghub/types.go`, add to the `PullRequest` struct:

```go
	// State is "open" or "closed", as GitHub spells it, and Merged says which
	// kind of close. Both are here for the poller: a delivery TELLS this
	// daemon a pull request merged, and a poll has to ask. Compare State with
	// EqualFold rather than ==, as Issue.IsOpen does.
	State  string
	Merged bool
```

And add the new type:

```go
// Subject is one issue OR pull request as a poll sees it: the fields that say
// whether something happened to it, and nothing else.
//
// It is not Issue. Issue is built by ConvertIssues, which DROPS pull requests,
// and every caller of that function depends on it -- a pull request carried
// into the epic sweep's lists would be swept as the issue that shares its
// number. The poll listing returns both kinds mixed on purpose, because both
// kinds are things a loop reacts to, so it gets a type that can say which is
// which.
type Subject struct {
	Number        int
	IsPullRequest bool
	// State is "open" or "closed", as GitHub spells it.
	State     string
	Labels    []string
	UpdatedAt time.Time
}
```

- [ ] **Step 4: Implement the two calls and the conversion**

In `internal/ghub/ghub.go`, inside `convertPR`, add to the returned literal:

```go
		State:  pr.GetState(),
		Merged: pr.GetMerged(),
```

Then add the two methods:

```go
// ListSubjectsUpdatedSince returns every issue AND pull request in the
// repository whose updated_at is at or after since, oldest first.
//
// state=all is the point of it: ListOpenIssues cannot answer this, because a
// close is exactly the change a poll must see and an open-only listing reports
// a closed issue by omitting it, which is indistinguishable from "unchanged".
//
// direction=asc is load-bearing too. The caller advances a cursor to the last
// subject it fully processed, so a failure part way through a page resumes
// rather than restarts; descending order would make every partial pass lose
// its oldest work.
//
// since is INCLUSIVE at the API, and the caller relies on that -- see the
// poller's cursor, which deliberately re-reads its own boundary.
func (g *GitHubClient) ListSubjectsUpdatedSince(
	ctx context.Context, owner, repo string, since time.Time,
) ([]Subject, error) {
	opts := &github.IssueListByRepoOptions{
		State:       "all",
		Sort:        "updated",
		Direction:   "asc",
		Since:       since.UTC(),
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var all []Subject
	for {
		page, resp, err := g.c.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list subjects %s/%s: %w", owner, repo, err)
		}
		for _, gi := range page {
			if gi == nil {
				continue
			}
			labels := make([]string, 0, len(gi.Labels))
			for _, l := range gi.Labels {
				if l.GetName() != "" {
					labels = append(labels, l.GetName())
				}
			}
			all = append(all, Subject{
				Number:        gi.GetNumber(),
				IsPullRequest: gi.IsPullRequest(),
				State:         gi.GetState(),
				Labels:        labels,
				UpdatedAt:     gi.GetUpdatedAt().Time,
			})
		}
		if resp.NextPage == 0 {
			return all, nil
		}
		// IssueListByRepoOptions embeds BOTH ListCursorOptions (Page string)
		// and ListOptions (Page int) at the same depth, so a bare opts.Page is
		// an ambiguous selector and does not compile. Qualify it.
		opts.ListOptions.Page = resp.NextPage
	}
}

// BranchHead returns the SHA at the tip of branch.
//
// It exists for the one event a poll cannot see any other way: a direct push
// to the default branch produces no pull request and no issue update, and it
// makes every open pull request of that repository stale exactly as a merge
// does. BehindBy is not a substitute -- it answers how far one ref trails
// another, which cannot report that a ref moved.
func (g *GitHubClient) BranchHead(ctx context.Context, owner, repo, branch string) (string, error) {
	c, _, err := g.c.Repositories.GetCommit(ctx, owner, repo, branch, nil)
	if err != nil {
		return "", fmt.Errorf("head of %s/%s@%s: %w", owner, repo, branch, err)
	}
	return c.GetSHA(), nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/ghub/ -v`
Expected: PASS, including every pre-existing test in the package.

- [ ] **Step 6: Commit**

```bash
git add internal/ghub/
git commit -m "feat(ghub): add the listing, branch head, and merge state a poll needs"
```

---

### Task 2: The snapshot and cursor tables

**Files:**
- Modify: `internal/store/store.go` (two `CREATE TABLE IF NOT EXISTS` statements in the existing schema block)
- Create: `internal/store/poll.go`
- Test: `internal/store/poll_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `store.PollSubject{Repo string; Number int; IsPR bool; State string; Merged bool; Labels string; UpdatedAt time.Time}`
  - `store.PollCursor{Repo string; Since time.Time; HeadSHA string; SeededAt time.Time}`
  - `func (d *DB) PollSnapshot(repo string) (map[int]PollSubject, error)`
  - `func (d *DB) SavePollSubjects(repo string, subjects []PollSubject) error`
  - `func (d *DB) PollCursor(repo string) (PollCursor, bool, error)`
  - `func (d *DB) SavePollCursor(c PollCursor) error`

- [ ] **Step 1: Write the failing tests**

Create `internal/store/poll_test.go`:

```go
package store

import (
	"testing"
	"time"
)

func TestPollSnapshotRoundTripsAndUpserts(t *testing.T) {
	db, _ := openTempDB(t)
	at := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)

	first := []PollSubject{
		{Repo: "o/r", Number: 51, State: "open", Labels: "status:ready", UpdatedAt: at},
		{Repo: "o/r", Number: 52, IsPR: true, State: "open", UpdatedAt: at},
	}
	if err := db.SavePollSubjects("o/r", first); err != nil {
		t.Fatalf("SavePollSubjects: %v", err)
	}

	later := at.Add(time.Hour)
	if err := db.SavePollSubjects("o/r", []PollSubject{
		{Repo: "o/r", Number: 51, State: "closed", Labels: "status:ready", UpdatedAt: later},
	}); err != nil {
		t.Fatalf("SavePollSubjects (update): %v", err)
	}

	got, err := db.PollSnapshot("o/r")
	if err != nil {
		t.Fatalf("PollSnapshot: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[51].State != "closed" || !got[51].UpdatedAt.Equal(later) {
		t.Errorf("51 = %+v, want the UPDATED row", got[51])
	}
	if !got[52].IsPR {
		t.Errorf("52 = %+v, want IsPR", got[52])
	}
}

// Two projects may spell one repository differently in their yaml, and
// ByRepo already folds case for that reason. A snapshot that did not would
// diff each spelling against its own empty history and deliver everything
// twice -- which, on a fresh poll, means dispatching an agent per issue.
func TestPollSnapshotFoldsRepositoryCase(t *testing.T) {
	db, _ := openTempDB(t)
	if err := db.SavePollSubjects("O/R", []PollSubject{
		{Repo: "O/R", Number: 7, State: "open", UpdatedAt: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("SavePollSubjects: %v", err)
	}

	got, err := db.PollSnapshot("o/r")
	if err != nil {
		t.Fatalf("PollSnapshot: %v", err)
	}
	if _, ok := got[7]; !ok {
		t.Fatalf("snapshot for o/r = %+v, want the row written as O/R", got)
	}
}

// "No cursor" and "a cursor at the zero time" mean opposite things: the first
// seeds and delivers nothing, the second delivers the whole history. They
// cannot be told apart by value, so the read reports presence separately.
func TestPollCursorReportsAbsence(t *testing.T) {
	db, _ := openTempDB(t)

	_, ok, err := db.PollCursor("o/r")
	if err != nil {
		t.Fatalf("PollCursor: %v", err)
	}
	if ok {
		t.Fatal("a repository never polled must report ok=false")
	}

	want := PollCursor{
		Repo:     "o/r",
		Since:    time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC),
		HeadSHA:  "deadbeef",
		SeededAt: time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC),
	}
	if err := db.SavePollCursor(want); err != nil {
		t.Fatalf("SavePollCursor: %v", err)
	}

	got, ok, err := db.PollCursor("O/R")
	if err != nil {
		t.Fatalf("PollCursor: %v", err)
	}
	if !ok {
		t.Fatal("ok = false after SavePollCursor")
	}
	if got.HeadSHA != want.HeadSHA || !got.Since.Equal(want.Since) || !got.SeededAt.Equal(want.SeededAt) {
		t.Errorf("cursor = %+v, want %+v", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run Poll -v`
Expected: compile failure -- `PollSubject`, `PollCursor` and the four methods are undefined.

- [ ] **Step 3: Add the tables**

In `internal/store/store.go`, append to the existing schema string, after the
`tend_conflicts` table:

```sql
-- poll_subjects is what the poller last saw of one issue or pull request, and
-- exists so a poll can answer "what CHANGED" from a listing that only reports
-- what IS. It is keyed by repository and NOT by project: a poll observes
-- GitHub, which is a machine-wide fact, and the fan-out to the projects
-- watching that repository happens after, in Worker.Deliver, exactly as it
-- does for a webhook delivery. Contrast closures, which is keyed by project
-- because its rows are joined against dispatch rows.
CREATE TABLE IF NOT EXISTS poll_subjects (
  repo       TEXT    NOT NULL,
  number     INTEGER NOT NULL,
  is_pr      INTEGER NOT NULL,
  state      TEXT    NOT NULL,
  merged     INTEGER NOT NULL,
  labels     TEXT    NOT NULL,
  updated_at TIMESTAMP NOT NULL,
  PRIMARY KEY (repo, number)
);

-- poll_cursors is where each repository's poll resumes. The ABSENCE of a row
-- is meaningful: it is what makes the first pass seed the snapshot silently
-- instead of dispatching an agent for every issue in the repository's history.
CREATE TABLE IF NOT EXISTS poll_cursors (
  repo      TEXT PRIMARY KEY,
  since     TIMESTAMP NOT NULL,
  head_sha  TEXT NOT NULL,
  seeded_at TIMESTAMP NOT NULL
);
```

- [ ] **Step 4: Implement the four methods**

Create `internal/store/poll.go`:

```go
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// PollSubject is what the poller last saw of one issue or pull request.
//
// Labels is a single string, not a slice: nothing reads the individual labels
// back out. The only question asked of it is "is this the same set as last
// time", and the poller writes it already lowercased, sorted and joined, so a
// reordered label list is not a change.
type PollSubject struct {
	Repo      string
	Number    int
	IsPR      bool
	State     string
	Merged    bool
	Labels    string
	UpdatedAt time.Time
}

// PollCursor is where one repository's poll resumes.
type PollCursor struct {
	Repo string
	// Since is the greatest updated_at the last pass processed. It is used as
	// an INCLUSIVE lower bound, so the boundary subject is deliberately
	// re-read; see the poller for why that costs nothing.
	Since time.Time
	// HeadSHA is the default branch tip the last pass saw, and is empty when
	// the pass could not name a default branch.
	HeadSHA string
	// SeededAt is when this repository's snapshot was first written. Its only
	// consumer is an operator reading the table.
	SeededAt time.Time
}

// foldRepo is where every repository key in this file is normalised. Two
// projects may spell one repository differently in their yaml, and the
// listener's routing already folds case for that reason.
func foldRepo(repo string) string { return strings.ToLower(repo) }

// PollSnapshot returns what the poller last saw of repo, keyed by number.
//
// It does not use eachRow, which takes no bind arguments: this is the one
// machine-wide read in the package that is SCOPED, and scoping it by string
// interpolation would put a repository name out of a yaml file into SQL.
func (d *DB) PollSnapshot(repo string) (map[int]PollSubject, error) {
	rows, err := d.db.Query(`
		SELECT repo, number, is_pr, state, merged, labels, updated_at
		FROM poll_subjects WHERE repo = ?`, foldRepo(repo))
	if err != nil {
		return nil, fmt.Errorf("read poll snapshot: %w", err)
	}
	defer rows.Close()

	out := map[int]PollSubject{}
	for rows.Next() {
		var s PollSubject
		if err := rows.Scan(&s.Repo, &s.Number, &s.IsPR, &s.State, &s.Merged, &s.Labels, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("read poll snapshot: %w", err)
		}
		out[s.Number] = s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read poll snapshot: %w", err)
	}
	return out, nil
}

// SavePollSubjects upserts each subject, in one transaction.
//
// One transaction because a pass writes a page at a time: a crash part way
// through must leave the snapshot consistent with the cursor that was not yet
// advanced, so the next pass re-reads the same window and reaches the same
// answer.
func (d *DB) SavePollSubjects(repo string, subjects []PollSubject) error {
	if len(subjects) == 0 {
		return nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return fmt.Errorf("save poll subjects: %w", err)
	}
	defer tx.Rollback()

	for _, s := range subjects {
		if _, err := tx.Exec(`
			INSERT INTO poll_subjects (repo, number, is_pr, state, merged, labels, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(repo, number) DO UPDATE SET
				is_pr = excluded.is_pr, state = excluded.state,
				merged = excluded.merged, labels = excluded.labels,
				updated_at = excluded.updated_at`,
			foldRepo(repo), s.Number, s.IsPR, s.State, s.Merged, s.Labels, s.UpdatedAt.UTC()); err != nil {
			return fmt.Errorf("save poll subject %s#%d: %w", repo, s.Number, err)
		}
	}
	return tx.Commit()
}

// PollCursor returns where repo's poll resumes, and whether it has ever run.
//
// The bool is not redundant with the zero value. "Never polled" makes the next
// pass SEED -- write the snapshot and deliver nothing -- and a cursor at the
// zero time would make it deliver the repository's entire history instead.
func (d *DB) PollCursor(repo string) (PollCursor, bool, error) {
	var c PollCursor
	err := d.db.QueryRow(`
		SELECT repo, since, head_sha, seeded_at FROM poll_cursors WHERE repo = ?`,
		foldRepo(repo)).Scan(&c.Repo, &c.Since, &c.HeadSHA, &c.SeededAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PollCursor{}, false, nil
	}
	if err != nil {
		return PollCursor{}, false, fmt.Errorf("read poll cursor for %s: %w", repo, err)
	}
	return c, true, nil
}

// SavePollCursor writes where repo's poll resumes.
func (d *DB) SavePollCursor(c PollCursor) error {
	_, err := d.db.Exec(`
		INSERT INTO poll_cursors (repo, since, head_sha, seeded_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(repo) DO UPDATE SET
			since = excluded.since, head_sha = excluded.head_sha`,
		foldRepo(c.Repo), c.Since.UTC(), c.HeadSHA, c.SeededAt.UTC())
	if err != nil {
		return fmt.Errorf("save poll cursor for %s: %w", c.Repo, err)
	}
	return nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/store/ -v`
Expected: PASS, including every pre-existing store test (the schema block is
shared, so a malformed statement fails the whole package).

- [ ] **Step 6: Commit**

```bash
git add internal/store/
git commit -m "feat(store): record what a poll last saw of each repository"
```

---

### Task 3: The diff, as a pure function

**Files:**
- Create: `internal/listener/pollsnap.go`
- Test: `internal/listener/pollsnap_test.go`

**Interfaces:**
- Consumes: `store.PollSubject` (Task 2), `ghub.Subject` (Task 1).
- Produces:
  - `type pollSubject struct{ Number int; IsPR bool; State string; Merged bool; MergedBase string; Labels []string; UpdatedAt time.Time }`
  - `func labelKey(labels []string) string`
  - `func pollDelivery(repo string, prev store.PollSubject, known bool, cur pollSubject) (Delivery, bool)`
  - `func (p pollSubject) row(repo string) store.PollSubject`

This is the whole flag table from the spec, with no GitHub client, no database
and no worker in sight. Everything else in the poller is plumbing around it.

- [ ] **Step 1: Write the failing tests**

Create `internal/listener/pollsnap_test.go`:

```go
package listener

import (
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/epic"
	"github.com/seanmcgary/agent-utils/internal/store"
)

func at(h int) time.Time { return time.Date(2026, 9, 15, h, 0, 0, 0, time.UTC) }

func TestPollDelivery(t *testing.T) {
	openIssue := store.PollSubject{
		Repo: "o/r", Number: 51, State: "open", Labels: "status:ready", UpdatedAt: at(10),
	}
	openPR := store.PollSubject{
		Repo: "o/r", Number: 52, IsPR: true, State: "open", UpdatedAt: at(10),
	}

	cases := []struct {
		name  string
		prev  store.PollSubject
		known bool
		cur   pollSubject
		want  Delivery
		none  bool
	}{
		{
			name:  "a number never seen before is a plain tick",
			known: false,
			cur:   pollSubject{Number: 51, State: "open", UpdatedAt: at(10)},
			want:  Delivery{Repo: "o/r", Number: 51},
		},
		{
			name:  "an unchanged subject re-read at the inclusive boundary delivers nothing",
			prev:  openIssue, known: true,
			cur:   pollSubject{Number: 51, State: "open", Labels: []string{"status:ready"}, UpdatedAt: at(10)},
			none:  true,
		},
		{
			name:  "a reordered label set is not a change",
			prev:  store.PollSubject{Repo: "o/r", Number: 51, State: "open", Labels: "a,b", UpdatedAt: at(10)},
			known: true,
			cur:   pollSubject{Number: 51, State: "open", Labels: []string{"B", "A"}, UpdatedAt: at(10)},
			none:  true,
		},
		{
			name:  "a moved updated_at is a plain tick",
			prev:  openIssue, known: true,
			cur:   pollSubject{Number: 51, State: "open", Labels: []string{"status:ready"}, UpdatedAt: at(11)},
			want:  Delivery{Repo: "o/r", Number: 51},
		},
		{
			name:  "a label change with no updated_at move is still a tick",
			prev:  openIssue, known: true,
			cur:   pollSubject{Number: 51, State: "open", Labels: []string{"status:blocked"}, UpdatedAt: at(10)},
			want:  Delivery{Repo: "o/r", Number: 51},
		},
		{
			name:  "an issue that closed arms the epic sweep",
			prev:  openIssue, known: true,
			cur:   pollSubject{Number: 51, State: "closed", Labels: []string{"status:ready"}, UpdatedAt: at(11)},
			want:  Delivery{Repo: "o/r", Number: 51, ClosedIssue: true},
		},
		{
			name:  "a pull request that closed arms worktree cleanup, not the epic sweep",
			prev:  openPR, known: true,
			cur:   pollSubject{Number: 52, IsPR: true, State: "closed", UpdatedAt: at(11)},
			want:  Delivery{Repo: "o/r", Number: 52, ClosedPR: true},
		},
		{
			name:  "a merged pull request also names the branch it landed on",
			prev:  openPR, known: true,
			cur: pollSubject{
				Number: 52, IsPR: true, State: "closed", Merged: true,
				MergedBase: "master", UpdatedAt: at(11),
			},
			want: Delivery{Repo: "o/r", Number: 52, ClosedPR: true, MergedInto: "master"},
		},
		{
			name: "a reopen erases the recorded closure",
			prev: store.PollSubject{Repo: "o/r", Number: 51, State: "closed", UpdatedAt: at(10)},
			known: true,
			cur:   pollSubject{Number: 51, State: "open", UpdatedAt: at(11)},
			want:  Delivery{Repo: "o/r", Number: 51, Reopened: true},
		},
		{
			name:  "the epic ready label appearing on an issue arms the requested sweep",
			prev:  openIssue, known: true,
			cur: pollSubject{
				Number: 51, State: "open",
				Labels: []string{"status:ready", epic.ReadyLabel}, UpdatedAt: at(11),
			},
			want: Delivery{Repo: "o/r", Number: 51, EpicReady: true},
		},
		{
			name: "the epic ready label REMOVED presses no button",
			prev: store.PollSubject{
				Repo: "o/r", Number: 51, State: "open", Labels: epic.ReadyLabel, UpdatedAt: at(10),
			},
			known: true,
			cur:   pollSubject{Number: 51, State: "open", UpdatedAt: at(11)},
			want:  Delivery{Repo: "o/r", Number: 51},
		},
		{
			// Issues and pull requests share a number space. EpicReady set from
			// a pull request would sweep the epic of whichever ISSUE carries
			// that number -- the bug the webhook handler narrows by event.
			name:  "the epic ready label on a pull request presses no button",
			prev:  openPR, known: true,
			cur: pollSubject{
				Number: 52, IsPR: true, State: "open",
				Labels: []string{epic.ReadyLabel}, UpdatedAt: at(11),
			},
			want: Delivery{Repo: "o/r", Number: 52},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := pollDelivery("o/r", c.prev, c.known, c.cur)
			if c.none {
				if ok {
					t.Fatalf("delivered %+v, want nothing", got)
				}
				return
			}
			if !ok {
				t.Fatal("delivered nothing, want a delivery")
			}
			if got != c.want {
				t.Errorf("delivery = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestLabelKeyIsOrderAndCaseIndependent(t *testing.T) {
	if a, b := labelKey([]string{"B", "a"}), labelKey([]string{"A", "b"}); a != b {
		t.Errorf("labelKey disagrees on the same set: %q vs %q", a, b)
	}
	if labelKey(nil) != "" {
		t.Errorf("labelKey(nil) = %q, want empty", labelKey(nil))
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/listener/ -run 'PollDelivery|LabelKey' -v`
Expected: compile failure -- `pollSubject`, `pollDelivery` and `labelKey` are undefined.

- [ ] **Step 3: Implement the diff**

Create `internal/listener/pollsnap.go`:

```go
package listener

import (
	"sort"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/epic"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// pollSubject is one issue or pull request as a pass sees it NOW, after the
// listing and, for a pull request that just closed, the single fetch that says
// whether it merged.
type pollSubject struct {
	Number int
	IsPR   bool
	// State is "open" or "closed", as GitHub spells it.
	State  string
	Merged bool
	// MergedBase is the branch a merged pull request landed on, and is empty
	// for everything else.
	MergedBase string
	Labels     []string
	UpdatedAt  time.Time
}

// labelKey reduces a label set to a value that changes when the SET changes and
// not when its order or casing does. GitHub returns labels in an order nothing
// promises to keep stable, and a poll that treated a reshuffle as a change
// would dispatch an agent for it.
func labelKey(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, strings.ToLower(l))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// row is what this subject should be remembered as.
func (p pollSubject) row(repo string) store.PollSubject {
	return store.PollSubject{
		Repo: repo, Number: p.Number, IsPR: p.IsPR, State: p.State,
		Merged: p.Merged, Labels: labelKey(p.Labels), UpdatedAt: p.UpdatedAt,
	}
}

// isOpen reports whether a GitHub state string says open, ignoring case, as
// ghub.Issue.IsOpen does.
func isOpen(state string) bool { return strings.EqualFold(state, "open") }

// pollDelivery turns "here is what this subject was, here is what it is now"
// into the delivery a webhook would have sent, or reports that nothing
// happened.
//
// It is pure, and it is the whole of what this feature decides. Everything
// around it -- the listing, the cursor, the snapshot writes, the ticker, the
// command -- is plumbing that exists to call this function with the right two
// arguments.
//
// known is not derivable from prev. A zero PollSubject is a real possible
// state, and "we have never seen this number" must produce a tick while "we
// saw it and nothing moved" must produce nothing.
func pollDelivery(repo string, prev store.PollSubject, known bool, cur pollSubject) (Delivery, bool) {
	d := Delivery{Repo: repo, Number: cur.Number}

	if !known {
		// A first sighting is a plain tick and nothing more. The transition
		// flags below all describe a CHANGE, and there is no previous state
		// to have changed from -- a number first seen already closed says
		// nothing about when it closed, so arming an epic sweep off it would
		// sweep on the poller's schedule rather than the issue's.
		return d, true
	}

	changed := !cur.UpdatedAt.Equal(prev.UpdatedAt) || labelKey(cur.Labels) != prev.Labels

	switch {
	case isOpen(prev.State) && !isOpen(cur.State):
		changed = true
		// Narrowed by kind, exactly as the webhook handler narrows by event:
		// issues and pull requests share a number space, so a closed pull
		// request setting ClosedIssue would sweep the epic of whichever ISSUE
		// carries its number.
		if cur.IsPR {
			d.ClosedPR = true
			if cur.Merged {
				d.MergedInto = cur.MergedBase
			}
		} else {
			d.ClosedIssue = true
		}
	case !isOpen(prev.State) && isOpen(cur.State):
		changed = true
		d.Reopened = true
	}

	// APPLYING the label presses the button; removing it does not. Issues
	// only, for the number-space reason above.
	//
	// The previous set is tested by MEMBERSHIP, not with strings.Contains
	// against the joined key: "status:epic-ready" is a substring of
	// "status:epic-ready-later", and a substring test would silently swallow
	// the sweep an operator had just asked for.
	if !cur.IsPR && hasLabel(cur.Labels, epic.ReadyLabel) && !keyHasLabel(prev.Labels, epic.ReadyLabel) {
		changed = true
		d.EpicReady = true
	}

	if !changed {
		return Delivery{}, false
	}
	return d, true
}

// hasLabel reports whether labels carries name, ignoring case.
func hasLabel(labels []string, name string) bool {
	for _, l := range labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

// keyHasLabel reports whether a stored label key -- the lowercased, sorted,
// comma-joined form labelKey produces -- carries name.
func keyHasLabel(key, name string) bool {
	if key == "" {
		return false
	}
	return hasLabel(strings.Split(key, ","), name)
}
```

- [ ] **Step 4: Add the test that pins membership**

Append to `internal/listener/pollsnap_test.go`:

```go
// The previous label set is stored joined; a membership test done by substring
// would see "status:epic-ready" inside "status:epic-ready-later" and suppress
// the sweep the operator just asked for.
func TestEpicReadyIsMembershipNotSubstring(t *testing.T) {
	prev := store.PollSubject{
		Repo: "o/r", Number: 51, State: "open",
		Labels: "status:epic-ready-later", UpdatedAt: at(10),
	}
	cur := pollSubject{
		Number: 51, State: "open",
		Labels:    []string{"status:epic-ready-later", epic.ReadyLabel},
		UpdatedAt: at(11),
	}
	got, ok := pollDelivery("o/r", prev, true, cur)
	if !ok || !got.EpicReady {
		t.Fatalf("delivery = %+v (ok=%v), want EpicReady", got, ok)
	}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/listener/ -run 'PollDelivery|LabelKey|EpicReady' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/listener/pollsnap.go internal/listener/pollsnap_test.go
git commit -m "feat(listener): derive a delivery from what changed about a subject"
```

---

### Task 4: The pass

**Files:**
- Create: `internal/listener/poll.go`
- Modify: `internal/listener/work.go` (two `Worker` fields; two lines in `NewWorker`)
- Test: `internal/listener/poll_test.go`

**Interfaces:**
- Consumes: `pollDelivery`, `pollSubject`, `labelKey` (Task 3); the four
  `store` methods (Task 2); `ghub.Subject`, `ListSubjectsUpdatedSince`,
  `BranchHead`, `PullRequest.Merged` (Task 1).
- Produces:
  - `type PollSource interface{ ListSubjectsUpdatedSince(...); BranchHead(...); PullRequest(...) }`
  - `Worker.PollInterval time.Duration`
  - `Worker.NewPollSource func(token string) PollSource`
  - `func (w *Worker) pollPass(ctx context.Context)`
  - `func (w *Worker) pollRepo(ctx context.Context, src PollSource, r RepoRoute) error`
  - `func (w *Worker) pollTicker() (<-chan time.Time, func())`

- [ ] **Step 1: Write the failing tests**

Create `internal/listener/poll_test.go`:

```go
package listener

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// fakeSource is a PollSource that answers from tables and counts what it was
// asked. The counts are the point of two of the tests below: a pass that reads
// more than it must is the failure this whole design is shaped to avoid.
type fakeSource struct {
	subjects map[string][]ghub.Subject // keyed "owner/repo"
	heads    map[string]string
	prs      map[int]ghub.PullRequest
	err      error

	listed  int
	headed  int
	fetched []int
	since   time.Time
}

func (f *fakeSource) ListSubjectsUpdatedSince(
	_ context.Context, owner, repo string, since time.Time,
) ([]ghub.Subject, error) {
	f.listed++
	f.since = since
	if f.err != nil {
		return nil, f.err
	}
	return f.subjects[owner+"/"+repo], nil
}

func (f *fakeSource) BranchHead(_ context.Context, owner, repo, branch string) (string, error) {
	f.headed++
	return f.heads[owner+"/"+repo+"@"+branch], nil
}

func (f *fakeSource) PullRequest(
	_ context.Context, _, _ string, number int,
) (ghub.PullRequest, error) {
	f.fetched = append(f.fetched, number)
	return f.prs[number], nil
}

// pollHarness wires a Worker whose deliveries are recorded instead of acted
// on, over a real temporary database.
type pollHarness struct {
	w   *Worker
	db  *store.DB
	src *fakeSource
	got []Delivery
}

func newPollHarness(t *testing.T, src *fakeSource, targets []Target) *pollHarness {
	t.Helper()
	db, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	h := &pollHarness{db: db, src: src}
	h.w = NewWorker(db)
	h.w.Token = func() (string, error) { return "t", nil }
	h.w.NewPollSource = func(string) PollSource { return src }
	h.w.ScanTargets = func() (Routes, error) { return Routes{Targets: targets}, nil }
	h.w.deliver = func(_ context.Context, d Delivery) { h.got = append(h.got, d) }
	return h
}

func repoTarget() Target {
	return Target{
		ProjectID: "p1", ProjectName: "proj", LoopName: "planning",
		Repo: "o/r", DefaultBranch: "master",
	}
}

func issueSubject(n int, state string, labels []string, h int) ghub.Subject {
	return ghub.Subject{Number: n, State: state, Labels: labels, UpdatedAt: at(h)}
}

// A fresh install must not dispatch an agent for every issue in a
// repository's history. The first pass establishes what is true; only changes
// after that point are work.
func TestFirstPassSeedsAndDeliversNothing(t *testing.T) {
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{
			"o/r": {issueSubject(51, "open", []string{"status:ready"}, 10)},
		},
		heads: map[string]string{"o/r@master": "sha1"},
	}
	h := newPollHarness(t, src, []Target{repoTarget()})

	h.w.pollPass(context.Background())

	if len(h.got) != 0 {
		t.Fatalf("seeded pass delivered %+v, want nothing", h.got)
	}
	snap, err := h.db.PollSnapshot("o/r")
	if err != nil {
		t.Fatalf("PollSnapshot: %v", err)
	}
	if _, ok := snap[51]; !ok {
		t.Fatalf("snapshot = %+v, want issue 51 seeded", snap)
	}
	if _, ok, _ := h.db.PollCursor("o/r"); !ok {
		t.Fatal("no cursor written; the next pass would seed again")
	}
}

// The second pass is where work starts, and the flags must survive the trip
// from the diff to the runtime.
func TestSecondPassDeliversTheChange(t *testing.T) {
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{
			"o/r": {issueSubject(51, "open", []string{"status:ready"}, 10)},
		},
		heads: map[string]string{"o/r@master": "sha1"},
	}
	h := newPollHarness(t, src, []Target{repoTarget()})
	h.w.pollPass(context.Background())

	src.subjects["o/r"] = []ghub.Subject{issueSubject(51, "closed", []string{"status:ready"}, 11)}
	h.w.pollPass(context.Background())

	if len(h.got) != 1 {
		t.Fatalf("deliveries = %+v, want exactly one", h.got)
	}
	want := Delivery{Repo: "o/r", Number: 51, ClosedIssue: true}
	if h.got[0] != want {
		t.Errorf("delivery = %+v, want %+v", h.got[0], want)
	}
}

// The cursor is used as an inclusive lower bound, so the boundary subject is
// re-read on every pass. Re-reading must be free.
func TestRereadingTheBoundaryDeliversNothing(t *testing.T) {
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{
			"o/r": {issueSubject(51, "open", []string{"status:ready"}, 10)},
		},
		heads: map[string]string{"o/r@master": "sha1"},
	}
	h := newPollHarness(t, src, []Target{repoTarget()})
	h.w.pollPass(context.Background())
	h.w.pollPass(context.Background())
	h.w.pollPass(context.Background())

	if len(h.got) != 0 {
		t.Fatalf("re-reads delivered %+v, want nothing", h.got)
	}
	if !src.since.Equal(at(10)) {
		t.Errorf("since = %v, want the boundary %v re-read", src.since, at(10))
	}
}

// A merge moves the default branch too. Both arm the same sweep, and arming it
// twice would dispatch two tend agents for one merge.
func TestAMergeAndItsPushArmOneTend(t *testing.T) {
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{
			"o/r": {{Number: 52, IsPullRequest: true, State: "open", UpdatedAt: at(10)}},
		},
		heads: map[string]string{"o/r@master": "sha1"},
		prs:   map[int]ghub.PullRequest{52: {Number: 52, State: "closed", Merged: true, BaseRef: "master"}},
	}
	h := newPollHarness(t, src, []Target{repoTarget()})
	h.w.pollPass(context.Background())

	src.subjects["o/r"] = []ghub.Subject{{Number: 52, IsPullRequest: true, State: "closed", UpdatedAt: at(11)}}
	src.heads["o/r@master"] = "sha2"
	h.w.pollPass(context.Background())

	if len(h.got) != 1 {
		t.Fatalf("deliveries = %+v, want one (the merge), not a push beside it", h.got)
	}
	if h.got[0].MergedInto != "master" || !h.got[0].ClosedPR {
		t.Errorf("delivery = %+v, want the merge of 52 into master", h.got[0])
	}
	if len(src.fetched) != 1 || src.fetched[0] != 52 {
		t.Errorf("pull request fetches = %v, want exactly [52]", src.fetched)
	}
}

// Somebody pushing straight to the base branch makes every open pull request
// stale and names none of them. It is the one event with no number.
func TestAnUnexplainedBranchMoveDeliversAPush(t *testing.T) {
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{"o/r": nil},
		heads:    map[string]string{"o/r@master": "sha1"},
	}
	h := newPollHarness(t, src, []Target{repoTarget()})
	h.w.pollPass(context.Background())

	src.heads["o/r@master"] = "sha2"
	h.w.pollPass(context.Background())

	if len(h.got) != 1 {
		t.Fatalf("deliveries = %+v, want one push", h.got)
	}
	if h.got[0].PushedTo != "master" || h.got[0].Number != 0 {
		t.Errorf("delivery = %+v, want a numberless push to master", h.got[0])
	}
}

// One unreachable repository must not stop the others, and must not silently
// lose the window it covered.
func TestAFailingRepositoryDoesNotAdvanceItsCursorOrStopTheNext(t *testing.T) {
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{
			"o/r":  {issueSubject(51, "open", nil, 10)},
			"o/r2": {issueSubject(9, "open", nil, 10)},
		},
		heads: map[string]string{"o/r@master": "sha1", "o/r2@master": "sha1"},
	}
	second := repoTarget()
	second.Repo = "o/r2"
	h := newPollHarness(t, src, []Target{repoTarget(), second})
	h.w.pollPass(context.Background()) // seed both

	src.err = errors.New("github is down")
	h.w.pollPass(context.Background())
	src.err = nil

	if _, ok, _ := h.db.PollCursor("o/r"); !ok {
		t.Fatal("the seeded cursor vanished")
	}
	// Both repositories were attempted: two listings on the failing pass.
	if src.listed != 4 {
		t.Errorf("listings = %d, want 4 (two repositories, two passes)", src.listed)
	}
}

// PollInterval <= 0 must produce a nil channel: a nil channel blocks forever,
// which is how Serve's select case simply never fires for a worker that does
// not poll. It is the same shape tendTicker uses.
func TestPollTickerIsNilWhenDisabled(t *testing.T) {
	w := NewWorker(nil)
	c, stop := w.pollTicker()
	defer stop()
	if c != nil {
		t.Error("a zero PollInterval must produce a nil channel")
	}

	w.PollInterval = time.Minute
	c2, stop2 := w.pollTicker()
	defer stop2()
	if c2 == nil {
		t.Error("a positive PollInterval must produce a ticker")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/listener/ -run Poll -v`
Expected: compile failure -- `PollSource`, `NewPollSource`, `pollPass`,
`pollTicker` and the `deliver` seam are undefined.

- [ ] **Step 3: Add the worker fields and the deliver seam**

In `internal/listener/work.go`, add to the `Worker` struct, beside `NewClient`:

```go
	// PollInterval is how often pollPass runs, and zero means never. `listener
	// run` leaves it zero and `listener poll` sets it: the two commands differ
	// in which SOURCE they start, and in nothing else.
	PollInterval time.Duration
	// NewPollSource builds the GitHub client one poll pass shares across every
	// repository it reads. It is a seam for the same reason NewClient is, and
	// it is a SEPARATE seam because its interface is narrower: widening
	// ghub.Client to carry two methods only the poller calls would force them
	// onto every fake in this tree that implements it.
	NewPollSource func(token string) PollSource
	// deliver is Deliver, indirected so a test can record what a pass produced
	// without wiring a whole runtime behind it. Production never replaces it.
	deliver func(ctx context.Context, d Delivery)
```

In `NewWorker`, add to the literal:

```go
		NewPollSource: func(token string) PollSource { return ghub.New(token) },
```

and after the literal, before `return w`:

```go
	// Assigned after the literal because it refers to w itself.
	w.deliver = w.Deliver
```

- [ ] **Step 4: Implement the pass**

Create `internal/listener/poll.go`:

```go
package listener

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/store"
)

// PollSource is the GitHub surface a poll needs.
//
// It is declared here, narrow, rather than added to ghub.Client: only the
// poller calls these three, and widening that interface would cost every fake
// in this tree two methods it never answers.
type PollSource interface {
	ListSubjectsUpdatedSince(ctx context.Context, owner, repo string, since time.Time) ([]ghub.Subject, error)
	BranchHead(ctx context.Context, owner, repo, branch string) (string, error)
	PullRequest(ctx context.Context, owner, repo string, number int) (ghub.PullRequest, error)
}

// pollTicker returns the channel pollPass runs on, and a stop function.
//
// A PollInterval of zero returns a NIL channel, and a nil channel blocks
// forever, so Serve's select case simply never fires for a worker that does
// not poll. tendTicker is built the same way and for the same reason.
func (w *Worker) pollTicker() (<-chan time.Time, func()) {
	if w.PollInterval <= 0 {
		return nil, func() {}
	}
	tk := time.NewTicker(w.PollInterval)
	return tk.C, tk.Stop
}

// pollPass asks GitHub what changed in every repository this machine routes,
// and turns each change into the delivery a webhook would have sent.
//
// It is the second source of events for this runtime, beside the HTTP handler,
// and it ends where that one does: at Deliver. Nothing downstream can tell
// which source produced a delivery, which is the property that makes a
// repository nobody has ADMIN on behave like one with a webhook.
func (w *Worker) pollPass(ctx context.Context) {
	if w.DB == nil {
		return
	}
	routes, err := w.ScanTargets()
	if err != nil {
		slog.Error("cannot route a poll", "err", err)
		return
	}
	byRepo := routes.ByRepo()
	if len(byRepo) == 0 {
		return
	}

	token, err := w.Token()
	if err != nil {
		slog.Error("cannot read the github token to poll", "err", err)
		return
	}
	src := w.NewPollSource(token)

	for _, r := range byRepo {
		// Checked per repository, not only on entry: a shutdown must not wait
		// out every repository on a many-project machine. reconcileClosed
		// makes the same check for the same reason.
		if ctx.Err() != nil {
			return
		}
		if err := w.pollRepo(ctx, src, r); err != nil {
			// Logged and skipped, never fatal to the pass. One unreachable
			// repository must not stop the others -- and pollRepo leaves the
			// cursor where it was, so the window this pass failed to cover is
			// re-read by the next one rather than lost.
			slog.Error("poll failed", "repo", r.Repo, "err", err)
		}
	}
}

// pollRepo polls one repository. It returns an error rather than logging one
// so its caller owns the "skip and continue" decision in one place.
func (w *Worker) pollRepo(ctx context.Context, src PollSource, r RepoRoute) error {
	owner, name, ok := splitRepo(r.Repo)
	if !ok {
		return fmt.Errorf("not an owner/name repository: %q", r.Repo)
	}

	cursor, seeded, err := w.DB.PollCursor(r.Repo)
	if err != nil {
		return err
	}
	since := cursor.Since
	if !seeded {
		// A repository never polled is SEEDED: its snapshot is written and
		// nothing is delivered. Otherwise a fresh install dispatches an agent
		// for every issue in the repository's history. The listing still has
		// to be made -- the snapshot is what it produces -- so the zero time
		// asks for everything, once.
		since = time.Time{}
	}

	subjects, err := src.ListSubjectsUpdatedSince(ctx, owner, name, since)
	if err != nil {
		return err
	}
	// Ascending, so the cursor can be advanced to the last subject fully
	// processed. The API is asked for this order; sorting here makes the
	// property the code relies on independent of that.
	sort.Slice(subjects, func(i, j int) bool {
		return subjects[i].UpdatedAt.Before(subjects[j].UpdatedAt)
	})

	snap, err := w.DB.PollSnapshot(r.Repo)
	if err != nil {
		return err
	}

	var (
		rows       []store.PollSubject
		deliveries []Delivery
		high       = cursor.Since
		merged     bool
	)
	for _, s := range subjects {
		cur := pollSubject{
			Number: s.Number, IsPR: s.IsPullRequest, State: s.State,
			Labels: s.Labels, UpdatedAt: s.UpdatedAt,
		}

		prev, known := snap[s.Number]
		// The one extra fetch a pass can make, and only for a pull request
		// that JUST closed: the listing carries no merged flag and no base
		// ref, and without them a merge arms no tend sweep.
		if known && cur.IsPR && isOpen(prev.State) && !isOpen(cur.State) {
			pr, err := src.PullRequest(ctx, owner, name, s.Number)
			if err != nil {
				return err
			}
			cur.Merged = pr.Merged
			cur.MergedBase = pr.BaseRef
		}

		rows = append(rows, cur.row(r.Repo))
		if s.UpdatedAt.After(high) {
			high = s.UpdatedAt
		}
		if !seeded {
			continue
		}
		if d, ok := pollDelivery(r.Repo, prev, known, cur); ok {
			deliveries = append(deliveries, d)
			if d.MergedInto != "" {
				merged = true
			}
		}
	}

	// The default branch, for the event nothing else can report. The first
	// non-empty branch among this repository's targets: they are loops of
	// possibly different projects, and a loop that names none has nothing to
	// compare against.
	head, branch := "", defaultBranchOf(r)
	if branch != "" {
		if head, err = src.BranchHead(ctx, owner, name, branch); err != nil {
			return err
		}
		// Suppressed when a merge in this same pass explains the move: both
		// arm the same sweep, and arming it twice dispatches two tend agents
		// for one merge.
		if seeded && head != "" && head != cursor.HeadSHA && !merged {
			deliveries = append(deliveries, Delivery{Repo: r.Repo, PushedTo: branch})
		}
	}

	// Written BEFORE the deliveries go out. A delivery can take a long time --
	// it opens loops and may dispatch agents -- and a crash in the middle of
	// that must not leave a snapshot claiming the old state, which would
	// deliver everything again on the next pass.
	if err := w.DB.SavePollSubjects(r.Repo, rows); err != nil {
		return err
	}
	if err := w.DB.SavePollCursor(store.PollCursor{
		Repo: r.Repo, Since: high, HeadSHA: head, SeededAt: seededAt(cursor, seeded, w.Now()),
	}); err != nil {
		return err
	}

	if !seeded {
		slog.Info("seeded a repository for polling; delivering nothing for it",
			"repo", r.Repo, "subjects", len(rows))
		return nil
	}
	for _, d := range deliveries {
		if ctx.Err() != nil {
			return nil
		}
		slog.Info("polled change", "repo", d.Repo, "number", d.Number,
			"closed_issue", d.ClosedIssue, "closed_pr", d.ClosedPR,
			"reopened", d.Reopened, "merged_into", d.MergedInto,
			"pushed_to", d.PushedTo, "epic_ready", d.EpicReady)
		w.deliver(ctx, d)
	}
	return nil
}

// seededAt keeps the original seeding time across every later write.
func seededAt(c store.PollCursor, seeded bool, now time.Time) time.Time {
	if seeded {
		return c.SeededAt
	}
	return now.UTC()
}

// defaultBranchOf returns the first default branch this repository's targets
// name, or "" when none does.
func defaultBranchOf(r RepoRoute) string {
	for _, t := range r.Targets {
		if t.DefaultBranch != "" {
			return t.DefaultBranch
		}
	}
	return ""
}

// splitRepo splits "owner/name".
func splitRepo(repo string) (string, string, bool) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}
```

If `splitRepo` already exists in this package, use the existing one and delete
this copy.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/listener/ -v`
Expected: PASS, including every pre-existing listener test.

- [ ] **Step 6: Commit**

```bash
git add internal/listener/poll.go internal/listener/poll_test.go internal/listener/work.go
git commit -m "feat(listener): poll github and deliver what changed"
```

---

### Task 5: Run the poll source inside the runtime loop

**Files:**
- Modify: `internal/listener/work.go` (`Serve`: one ticker, one select case)
- Test: `internal/listener/poll_test.go` (append)

**Interfaces:**
- Consumes: `pollTicker`, `pollPass` (Task 4).
- Produces: no new names. `Serve` now drives the poll source when
  `PollInterval > 0`.

- [ ] **Step 1: Write the failing test**

Append to `internal/listener/poll_test.go`:

```go
// The runtime must not care which source produced an event. Serve is what
// makes that true in practice: a worker with a poll interval runs the retry
// wake, the tend check and the orphan sweep exactly as a worker serving HTTP
// does, and polls in addition.
func TestServeRunsThePollSource(t *testing.T) {
	src := &fakeSource{
		subjects: map[string][]ghub.Subject{"o/r": {issueSubject(51, "open", nil, 10)}},
		heads:    map[string]string{"o/r@master": "sha1"},
	}
	h := newPollHarness(t, src, []Target{repoTarget()})
	h.w.PollInterval = time.Millisecond
	// Keep the rest of the loop quiet: this test is about the poll case
	// firing, and a real wake would try to open loops that do not exist here.
	h.w.MinWakeInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.w.Serve(ctx); close(done) }()

	deadline := time.After(2 * time.Second)
	for {
		if _, ok, _ := h.db.PollCursor("o/r"); ok {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("Serve never ran a poll pass")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/listener/ -run ServeRunsThePollSource -v`
Expected: FAIL with "Serve never ran a poll pass" -- `Serve` has no poll case.

- [ ] **Step 3: Wire the ticker into Serve**

In `internal/listener/work.go`, inside `Serve`, beside the `tendC` construction:

```go
	// The second event source, on its own interval. It is built here rather
	// than folded into the wake timer for the reason the tend ticker is: the
	// wake serves deadlines this daemon wrote, and a poll asks GitHub a
	// question nobody wrote a deadline for.
	pollC, stopPoll := w.pollTicker()
	defer stopPoll()
```

And in the `select`, beside the `tendC` case:

```go
		case <-pollC:
			// nil when polling is off, and a nil channel blocks forever, so
			// this case simply never fires for `listener run`.
			w.pollPass(ctx)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/listener/ -v`
Expected: PASS. Then `go test ./... -count=1` to confirm nothing else moved.

- [ ] **Step 5: Commit**

```bash
git add internal/listener/work.go internal/listener/poll_test.go
git commit -m "feat(listener): run the poll source from the runtime loop"
```

---

### Task 6: The `listener poll` command

**Files:**
- Modify: `cmd/agent-utils/listener.go` (add `listenerPollCommand`, register it, extend `CommandNotFound`'s list)
- Test: `cmd/agent-utils/listener_test.go` (append)

**Interfaces:**
- Consumes: `Worker.PollInterval` (Task 4), `Worker.Serve` (Task 5).
- Produces:
  - `func listenerPollCommand() *cli.Command`
  - `func parsePollInterval(arg string) (time.Duration, error)`
  - `const pollLockFileName = "listener.poll.lock"`
  - `const defaultPollInterval = time.Minute`
  - `const minPollInterval = 30 * time.Second`

- [ ] **Step 1: Write the failing tests**

Append to `cmd/agent-utils/listener_test.go`:

```go
func TestParsePollInterval(t *testing.T) {
	cases := []struct {
		arg  string
		want time.Duration
		bad  bool
	}{
		{arg: "", want: defaultPollInterval},
		{arg: "1m", want: time.Minute},
		{arg: "5m", want: 5 * time.Minute},
		{arg: "30s", want: 30 * time.Second},
		{arg: "10s", bad: true},
		{arg: "0", bad: true},
		{arg: "-1m", bad: true},
		{arg: "soon", bad: true},
	}
	for _, c := range cases {
		got, err := parsePollInterval(c.arg)
		if c.bad {
			if err == nil {
				t.Errorf("parsePollInterval(%q) = %v, want an error", c.arg, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePollInterval(%q): %v", c.arg, err)
			continue
		}
		if got != c.want {
			t.Errorf("parsePollInterval(%q) = %v, want %v", c.arg, got, c.want)
		}
	}
}

// The floor is REJECTED, not clamped. tend_interval clamps because it is a
// stored setting whose owner may be nowhere near the machine when it loads; an
// argument typed at a prompt has somebody reading the reply.
func TestPollIntervalBelowTheFloorNamesTheFloor(t *testing.T) {
	_, err := parsePollInterval("5s")
	if err == nil {
		t.Fatal("5s was accepted")
	}
	if !strings.Contains(err.Error(), minPollInterval.String()) {
		t.Errorf("error %q does not name the floor %s", err, minPollInterval)
	}
}

// A second poller on one machine would double every dispatch. It must fail
// fast, the way a second `listener run` does.
func TestPollLockIsExclusive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_UTILS_HOME", home)

	first, err := lock.Acquire(filepath.Join(home, pollLockFileName))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer first.Release()

	if _, err := lock.Acquire(filepath.Join(home, pollLockFileName)); err == nil {
		t.Fatal("a second poller acquired the lock")
	}
}
```

Add `"strings"`, `"time"`, `"path/filepath"` and the `lock` import to the test
file if they are not already there.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/agent-utils/ -run Poll -v`
Expected: compile failure -- `parsePollInterval`, `pollLockFileName`,
`defaultPollInterval` and `minPollInterval` are undefined.

- [ ] **Step 3: Implement the command**

In `cmd/agent-utils/listener.go`, add the constants beside `lockFileName`:

```go
// pollLockFileName is the poller's single-instance lock, at
// <home>/listener.poll.lock.
//
// It is a SEPARATE lock from lockFileName on purpose. Running a poller beside
// an HTTP listener is redundant but supported -- the per-loop lock already
// makes an overlapping tick harmless -- and contending for one lock would turn
// "a daemon is up" into "the poller refuses to start".
const pollLockFileName = "listener.poll.lock"

// defaultPollInterval is how often `listener poll` asks GitHub what changed
// when the operator names no interval.
const defaultPollInterval = time.Minute

// minPollInterval is the floor under the interval argument. Below it, a poll
// spends its rate limit re-reading a repository faster than an agent can act
// on what the last one found.
const minPollInterval = 30 * time.Second
```

Register the command in `listenerCommand`'s `Commands` list, after
`listenerRunCommand()`:

```go
			listenerPollCommand(),
```

and extend the `CommandNotFound` fallback message to name it:

```go
					"available commands: run, poll, install, uninstall, status\n",
```

Then add:

```go
// parsePollInterval turns the command's optional positional argument into an
// interval.
//
// An out-of-range value is REJECTED rather than clamped, unlike
// settings.TendEvery: that one reads a stored file whose author may be nowhere
// near the machine when it loads, and this one reads an argument somebody just
// typed, with the reply in front of them.
func parsePollInterval(arg string) (time.Duration, error) {
	if strings.TrimSpace(arg) == "" {
		return defaultPollInterval, nil
	}
	d, err := time.ParseDuration(arg)
	if err != nil {
		return 0, fmt.Errorf("interval must be a duration such as %q: %w", "1m", err)
	}
	if d < minPollInterval {
		return 0, fmt.Errorf(
			"interval must be at least %s; got %s", minPollInterval, d)
	}
	return d, nil
}

// listenerPollCommand runs this machine's loops from a POLL instead of from
// webhook deliveries.
//
// It exists for a repository nobody has ADMIN on: a hook cannot be created
// there, and without one none of the events a loop reacts to ever reach this
// machine. Polling derives the same events by asking.
//
// It binds no port. `listener run` and this command start DIFFERENT SOURCES
// for the same runtime and differ in nothing else -- the retry wake, the
// periodic tend check, the orphan sweep and the startup closure reconcile all
// run here exactly as they do there.
func listenerPollCommand() *cli.Command {
	return &cli.Command{
		Name:      "poll",
		Usage:     "run the loops from a github poll instead of webhook deliveries",
		ArgsUsage: "[interval]",
		Description: "The interval is a Go duration such as 1m or 5m, and defaults to " +
			defaultPollInterval.String() + ". It binds no port and needs no webhook.",
		Action: func(ctx context.Context, c *cli.Command) error {
			interval, err := parsePollInterval(c.Args().First())
			if err != nil {
				return err
			}

			// The token half of listenerPreflight, and deliberately not
			// the whole of it: that function also requires webhook.enabled
			// and a non-empty webhook.secret, and a poller has neither a
			// delivery to verify nor a port to serve it on. Refusing to start
			// on a missing or world-readable env file is the half that
			// applies, for the reason it applies to `run` -- Worker reads the
			// token fresh on every pass, so without this the daemon comes up
			// looking healthy and then fails every one of them.
			if err := ensureToken(os.Stdin, os.Stderr, isInteractive()); err != nil {
				return err
			}

			dir, err := home.Dir()
			if err != nil {
				return err
			}
			lockPath := filepath.Join(dir, pollLockFileName)
			lk, err := lock.Acquire(lockPath)
			if errors.Is(err, lock.ErrHeld) {
				return errors.New(
					"another `agent-utils listener poll` is already running (its lock is held); " +
						"stop it with Ctrl-C in its own terminal")
			}
			if err != nil {
				return fmt.Errorf("acquire poll lock: %w", err)
			}
			defer func() {
				if err := lk.Release(); err != nil {
					slog.Warn("release poll lock", "path", lockPath, "err", err)
				}
			}()

			dbPath, err := home.StateDBPath()
			if err != nil {
				return err
			}
			db, err := store.Open(dbPath)
			if err != nil {
				return err
			}
			defer func() {
				if err := db.Close(); err != nil {
					slog.Warn("close state database", "err", err)
				}
			}()

			w := listener.NewWorker(db)
			w.PollInterval = interval
			// Read once at startup, exactly as runListener reads it, and for
			// the same reason: a Worker's delays are set before it is shared
			// with the wake loop and never written afterwards. The runtime is
			// the runtime -- a poller that skipped this would tend on a
			// different schedule from a listener, which is precisely the
			// difference this command exists to avoid.
			w.TendInterval = settings.DefaultTendInterval
			if st, err := settings.Load(); err == nil {
				w.TendInterval = st.TendEvery()
			} else {
				slog.Warn("cannot read settings; using the default tend interval", "err", err)
			}

			routes, err := listener.Scan()
			if err != nil {
				return err
			}
			fmt.Printf("polling every %s; no port is bound\n", interval)
			fmt.Print(routingTable(routes))

			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			w.Serve(ctx)
			return nil
		},
	}
}
```

Every helper above already exists in `cmd/agent-utils/listener.go` or its
neighbours -- `ensureToken`, `isInteractive`, `home.Dir`, `home.StateDBPath`,
`store.Open`, `routingTable`, `settings.Load` -- and is used here exactly as
`runListener` uses it. Read `runListener` before writing this; if its lock,
database or settings handling has moved on, follow what is there rather than
what is written above.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./cmd/agent-utils/ -v`
Expected: PASS.

- [ ] **Step 5: Verify it runs**

Run: `go run ./cmd/agent-utils listener poll 10s`
Expected: refused, naming the 30s floor.

Run: `go run ./cmd/agent-utils listener poll --help`
Expected: usage naming `[interval]` and the default.

- [ ] **Step 6: Commit**

```bash
git add cmd/agent-utils/
git commit -m "feat(cli): add listener poll, the event source for a repo with no webhook"
```

---

### Task 7: Document it

**Files:**
- Modify: `README.md` (the `Webhooks` section gains a `Polling` subsection; the command table gains a row)

**Interfaces:**
- Consumes: everything above.
- Produces: no code.

- [ ] **Step 1: Add the command table row**

In the command table (around `README.md:138`), after the `listener run` row:

```markdown
| `agent-utils listener poll [interval]` | Run the loops from a GitHub poll instead of webhook deliveries, for a repository you cannot add a hook to |
```

- [ ] **Step 2: Add the section**

After the `Webhooks` section, add:

```markdown
## Polling

Creating a webhook needs ADMIN on the repository. On a repository you do not
have it on, `agent-utils listener poll` derives the same events by asking
GitHub what changed, on an interval:

```bash
agent-utils listener poll 1m
```

The interval is a Go duration and defaults to one minute; below thirty seconds
it is refused. It binds no port, and it needs no `webhook.url` and no secret --
only the same `~/.agent-utils/env` with `GITHUB_TOKEN` in it that the [Cron](#cron)
section has you create.

It is the same daemon `listener run` is, with a different event source. The
runtime does not know which source produced an event: the retry wake, the
periodic tend check, the orphan sweep and the startup reconcile of what closed
while it was down all run here exactly as they do there, and a polled change
reaches a loop through the same path a delivery does.

Each pass asks each watched repository for the issues and pull requests updated
since the last pass, and for the tip of the default branch. What it finds is
compared against what the last pass saw, and each difference becomes the
delivery a webhook would have sent -- a label edit or a comment becomes a tick,
a close arms the epic sweep or worktree cleanup, a merge or a direct push to
the default branch arms a tend sweep. Roughly two API calls per repository per
pass.

**The first pass for a repository delivers nothing.** It records what is
currently true and stops; otherwise starting the poller on an established
repository would dispatch an agent for every issue in its history. Changes after
that point are delivered however old they are, so a poller that was off for a
day catches up on its next pass.

One thing a poll cannot see: a review submitted with no comment body. The
periodic tend check already covers the staleness that would signal.

Cron remains worth keeping beside it, for the same reason it is worth keeping
beside the listener.
```

- [ ] **Step 3: Verify the links and the table render**

Run: `grep -n "listener poll" README.md`
Expected: the table row and the section, and no broken `#polling` anchor
elsewhere.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: document listener poll"
```
