# Retry-Cap Retirement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A harness change, or a model change that crosses pi providers, retires an issue's accumulated retry failures instead of re-parking it — and when the cap does hold, the park comment names the real reason.

**Architecture:** `BeginDispatch` stamps the effective harness and resolved provider of every dispatch onto the issue row, before the agent runs. `retryDecision` compares those stamps against what the next dispatch would use; a difference yields `KindStart`, whose `BeginDispatch(retry = false)` already clears `parked`, `retry_count` and `retry_after`. Provider resolution shells out to `pi --list-models` in the caller, so `engine.Decide` stays pure and receives a per-issue map.

**Tech Stack:** Go 1.x, modernc.org/sqlite, `make check` (gofmt, vet, golangci-lint, go test) as the gate.

**Spec:** `docs/superpowers/specs/2026-09-01-retry-cap-retirement-design.md`

## Global Constraints

- Every new NOT NULL column carries a literal `DEFAULT ''` and an entry in `addedColumns`. A binary from before this schema may still hold the file open. (`internal/store/store.go:30`)
- `engine.Decide` is pure: it reads only its arguments, performs no I/O, and reads no clock. Provider resolution must happen in the caller.
- An empty recorded value means **unknown**, and unknown never counts as a change. Fail closed — the cap stands.
- Comments explain *why*, in the voice of the surrounding code. This codebase's comments argue; match that.
- Run `make check` before every commit. Never `git stash` in this worktree.
- No `Co-Authored-By:` trailers on any commit.

---

### Task 1: Stamp the attempted harness and provider on the issue row

The rule in Task 2 compares against what the loop most recently *attempted*. Recording it in `BeginDispatch` — before the agent runs — is what makes retirement terminate (spec R3).

**Files:**
- Modify: `internal/store/store.go` (schema string, `addedColumns`, `IssueStates`, `PutIssueState`, `BeginDispatch`)
- Modify: `internal/store/types.go` (`IssueState`)
- Modify: `internal/loopcmd/tick.go:555` (the one `BeginDispatch` call site)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `store.IssueState.DispatchHarness string`, `store.IssueState.DispatchProvider string`, and
  `func (s *Store) BeginDispatch(loop, repo string, number int, sessionID, harness, provider string, retry bool, now time.Time) error`

- [ ] **Step 1: Write the failing test**

In `internal/store/store_test.go`:

```go
func TestBeginDispatchStampsHarnessAndProvider(t *testing.T) {
	s := testStore(t)

	if err := s.BeginDispatch("execution", "o/r", 7, "sess-1", "pi", "openrouter", false, time.Now()); err != nil {
		t.Fatalf("begin dispatch: %v", err)
	}

	st, err := s.IssueState("execution", "o/r", 7)
	if err != nil {
		t.Fatalf("issue state: %v", err)
	}
	if st.DispatchHarness != "pi" {
		t.Errorf("DispatchHarness = %q, want %q", st.DispatchHarness, "pi")
	}
	if st.DispatchProvider != "openrouter" {
		t.Errorf("DispatchProvider = %q, want %q", st.DispatchProvider, "openrouter")
	}
}

// A second dispatch under a different configuration must REPLACE the stamps,
// not accumulate them. The rule reads "what was attempted last", so a stale
// value would keep reporting a change that has already been acted on and
// retire the cap forever.
func TestBeginDispatchReplacesStamps(t *testing.T) {
	s := testStore(t)
	now := time.Now()

	if err := s.BeginDispatch("execution", "o/r", 7, "sess-1", "pi", "openrouter", false, now); err != nil {
		t.Fatalf("begin dispatch: %v", err)
	}
	if err := s.BeginDispatch("execution", "o/r", 7, "sess-2", "claude", "", true, now); err != nil {
		t.Fatalf("begin dispatch: %v", err)
	}

	st, err := s.IssueState("execution", "o/r", 7)
	if err != nil {
		t.Fatalf("issue state: %v", err)
	}
	if st.DispatchHarness != "claude" {
		t.Errorf("DispatchHarness = %q, want %q", st.DispatchHarness, "claude")
	}
	if st.DispatchProvider != "" {
		t.Errorf("DispatchProvider = %q, want empty", st.DispatchProvider)
	}
}
```

If `testStore` does not exist in this package under that name, use whatever helper the neighbouring tests already use to open a temporary store; do not invent a second one.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/ -run 'TestBeginDispatchStamps|TestBeginDispatchReplaces' -count=1`
Expected: FAIL — compile error, `BeginDispatch` takes 6 arguments and `IssueState` has no field `DispatchHarness`.

- [ ] **Step 3: Add the columns and the fields**

In `internal/store/store.go`, inside `schemaTables`, after the `session_harness` block:

```sql
  -- dispatch_harness and dispatch_provider record what the loop most recently
  -- ATTEMPTED, which is not the same question session_harness answers.
  -- session_harness is the harness that CREATED the session, so it stays empty
  -- when a dispatch dies before the harness emits a session identifier -- which
  -- is exactly what a misconfigured harness does. A retirement rule reading
  -- that column would see "changed" on every tick and redispatch forever with
  -- no human in the loop. These are stamped by BeginDispatch, before the agent
  -- runs, so one retirement is all any single change can buy.
  --
  -- Empty means "recorded before this column existed", and the engine reads
  -- that as unknown rather than as a change.
  dispatch_harness  TEXT NOT NULL DEFAULT '',
  dispatch_provider TEXT NOT NULL DEFAULT '',
```

Append to `addedColumns`:

```go
	{"issues", "dispatch_harness", "TEXT NOT NULL DEFAULT ''"},
	{"issues", "dispatch_provider", "TEXT NOT NULL DEFAULT ''"},
```

In `internal/store/types.go`, add to `IssueState` immediately after `SessionHarness`:

```go
	// DispatchHarness is the effective harness of the most recent dispatch,
	// whether or not that dispatch created a session. SessionHarness answers
	// "may this session be resumed"; this answers "what did the loop last
	// try", and only the second question can safely retire a retry budget.
	//
	// Empty means unknown: the row predates the column, or no dispatch has run
	// since it was added. Unknown is never a change.
	DispatchHarness string
	// DispatchProvider is the provider serving the most recent dispatch's
	// model, as resolved by `pi --list-models`. It is empty for claude, and
	// empty whenever resolution failed -- both of which read as unknown.
	DispatchProvider string
```

Add both columns to the `SELECT` and scan lists in `IssueStates` and `IssueState`, and to the `INSERT`/`ON CONFLICT` in `PutIssueState`, in the same order they appear in the schema. Follow the existing column ordering exactly; the scan list is positional.

- [ ] **Step 4: Stamp them in `BeginDispatch`**

Replace the signature and the statement in `internal/store/store.go`:

```go
func (s *Store) BeginDispatch(loop, repo string, number int, sessionID, harness, provider string, retry bool, now time.Time) error {
	// A retry deliberately leaves retry_after alone: MarkNeedsRetry is the only
	// writer of a non-zero deadline, and a deadline stamped before the agent
	// runs would be overwritten by the failure that follows, collapsing the
	// escalating backoff list to its first entry forever.
	count, update := 1, "retry_count = retry_count + 1"
	if !retry {
		count, update = 0, "retry_count = 0, retry_after = 0"
	}
	_, err := s.db.Exec(`
		INSERT INTO issues (project_id, loop, repo, number, session_id,
		                    dispatch_harness, dispatch_provider,
		                    needs_retry, parked, retry_count, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)
		ON CONFLICT(project_id, loop, repo, number) DO UPDATE SET
		  session_id        = excluded.session_id,
		  dispatch_harness  = excluded.dispatch_harness,
		  dispatch_provider = excluded.dispatch_provider,
		  needs_retry       = 0,
		  parked            = 0,
		  `+update+`,
		  updated_at        = excluded.updated_at`,
		s.projectID, loop, repo, number, sessionID, harness, provider, count, now.UTC())
	if err != nil {
		return fmt.Errorf("begin dispatch: %w", err)
	}
	return nil
}
```

Extend the doc comment above it with:

```go
// The harness and provider arguments are the configuration this dispatch is
// about to run, stamped BEFORE the agent starts. They are what
// engine.retryDecision compares against to decide whether the accumulated
// failures still describe the configuration in play.
```

- [ ] **Step 5: Update the call site**

In `internal/loopcmd/tick.go`, in `dispatch`, replace the `BeginDispatch` call:

```go
	isRetry := d.Kind == engine.KindRetryStart || d.Kind == engine.KindRetryResume
	if kind != store.KindTend {
		// The stamps are the EFFECTIVE configuration, not the override alone.
		// dispatches.model and dispatches.harness record only the label, and
		// are empty whenever the loop default was used -- an ambiguity the
		// retirement rule cannot carry, because "empty" there would mean both
		// "claude by default" and "not recorded".
		eff := runner.Effective(cfg, d.Overrides)
		harness := eff.Harness
		if harness == "" {
			harness = config.HarnessClaude
		}
		if err := deps.Store.BeginDispatch(cfg.Name, cfg.Repo, d.Issue, sessionID,
			harness, d.Provider, isRetry, now); err != nil {
			return err
		}
	}
```

`d.Provider` does not exist yet — it arrives in Task 4. For this task, pass `""` and leave a `// Provider is wired in Task 4.` line; Task 4 replaces it. Add the `config` and `runner` imports if the file does not already have them (it imports `runner` already for `runner.LogPath`).

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/store/ ./internal/loopcmd/ ./internal/engine/ -count=1`
Expected: PASS. Other `BeginDispatch` callers in tests will need the two new arguments; update them to `"claude", ""` unless the test is specifically about a harness.

- [ ] **Step 7: Commit**

```bash
make check
git add internal/store/store.go internal/store/types.go internal/store/store_test.go internal/loopcmd/tick.go
git commit -m "feat(store): stamp the attempted harness and provider on the issue row"
```

---

### Task 2: A harness change retires the retry history

**Files:**
- Modify: `internal/engine/engine.go` (`retryDecision`, `effectiveHarness` → `EffectiveHarness`)
- Test: `internal/engine/engine_test.go`

**Interfaces:**
- Consumes: `store.IssueState.DispatchHarness` (Task 1).
- Produces: `func EffectiveHarness(cfg *config.Config, ov config.Overrides) string` (exported rename of `effectiveHarness`), and `func configRetired(cfg *config.Config, state store.IssueState, ov config.Overrides, provider string) string` returning a non-empty reason when the history is retired.

- [ ] **Step 1: Write the failing test**

In `internal/engine/engine_test.go`:

```go
// The incident this exists for: three pi dispatches failed on an OpenRouter
// 402, the cap parked the issue, and the operator removed harness:pi to fall
// back to the configured claude default. The re-applied trigger label must
// dispatch, not meet the cap a second time.
func TestHarnessChangeRetiresTheRetryCap(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{Issues: map[int]store.IssueState{
		1: {
			SessionID:       "sess-1",
			SessionStarted:  true,
			SessionHarness:  "pi",
			DispatchHarness: "pi",
			NeedsRetry:      true,
			RetryCount:      3,
		},
	}}

	p := Decide(cfg, snap, st, time.Now())

	if got := kinds(p); len(got) != 1 || got[0] != KindStart {
		t.Fatalf("kinds = %v, want [%v]", got, KindStart)
	}
	if p.Decisions[0].SessionID != "" {
		t.Errorf("SessionID = %q, want empty: a new harness must mint its own",
			p.Decisions[0].SessionID)
	}
	if !strings.Contains(p.Decisions[0].Reason, "retiring") {
		t.Errorf("Reason = %q, want it to say the history was retired", p.Decisions[0].Reason)
	}
}

// The backoff window goes with the cap. A wait sized to let pi's provider
// recover buys nothing once claude is the one running.
func TestHarnessChangeSkipsTheBackoffWindow(t *testing.T) {
	cfg := testConfig()
	now := time.Now()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{Issues: map[int]store.IssueState{
		1: {
			SessionStarted:  true,
			DispatchHarness: "pi",
			NeedsRetry:      true,
			RetryCount:      1,
			RetryAfter:      now.Add(30 * time.Minute),
		},
	}}

	p := Decide(cfg, snap, st, now)

	if got := kinds(p); len(got) != 1 || got[0] != KindStart {
		t.Fatalf("kinds = %v, want [%v]", got, KindStart)
	}
}

// A retirement is a human reconfiguring the loop, not a platform fault, so it
// must not push the circuit breaker toward its threshold and drop every other
// issue's dispatch for the cooldown.
func TestHarnessChangeDoesNotCountTowardTheBreaker(t *testing.T) {
	cfg := testConfig()
	cfg.Retry.Breaker.OrphanThreshold = 1
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{Issues: map[int]store.IssueState{
		1: {SessionStarted: true, DispatchHarness: "pi", NeedsRetry: true, RetryCount: 3},
	}}

	p := Decide(cfg, snap, st, time.Now())

	if p.BreakerTripped {
		t.Error("BreakerTripped = true, want false for a retirement")
	}
}

// Unknown is not a change. Rows written before dispatch_harness existed all
// have an empty value, and retiring on that would wipe the retry budget of
// every failing issue the moment this version is installed.
func TestUnknownDispatchHarnessDoesNotRetire(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{Issues: map[int]store.IssueState{
		1: {SessionStarted: true, DispatchHarness: "", NeedsRetry: true, RetryCount: 3},
	}}

	p := Decide(cfg, snap, st, time.Now())

	if got := kinds(p); len(got) != 1 || got[0] != KindParkRetryExhausted {
		t.Fatalf("kinds = %v, want [%v]", got, KindParkRetryExhausted)
	}
}

// The same harness failing again is the case the cap was written for.
func TestSameHarnessStillParksAtTheCap(t *testing.T) {
	cfg := testConfig()
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{Issues: map[int]store.IssueState{
		1: {SessionStarted: true, DispatchHarness: "claude", NeedsRetry: true, RetryCount: 3},
	}}

	p := Decide(cfg, snap, st, time.Now())

	if got := kinds(p); len(got) != 1 || got[0] != KindParkRetryExhausted {
		t.Fatalf("kinds = %v, want [%v]", got, KindParkRetryExhausted)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/engine/ -run 'Harness|Retire|Park' -count=1`
Expected: FAIL — `DispatchHarness` is set but ignored, so the retirement tests get `KindParkRetryExhausted` where they want `KindStart`.

- [ ] **Step 3: Export `EffectiveHarness` and add `configRetired`**

In `internal/engine/engine.go`, rename `effectiveHarness` to `EffectiveHarness` (exported — `loopcmd` needs the same definition, and two copies of "which harness will actually run" is how they drift apart). Update its three existing call sites in this file.

Add below `resumable`:

```go
// configRetired reports whether the issue's accumulated retry failures still
// describe the configuration the next dispatch will run under. It returns the
// reason when they do not, and "" when they still do.
//
// A retry cap is evidence about ONE configuration. Three OpenRouter 402s say
// nothing about whether claude/opus can build the issue, and holding the cap
// across that change makes the change unusable: the park removes the trigger
// label, so the operator's only move is to re-apply it, and the failure path
// above the trigger branch meets the cap again and re-parks before the new
// configuration has run once.
//
// It compares against what was last ATTEMPTED (dispatch_harness,
// dispatch_provider), not what last succeeded in creating a session. A
// dispatch that dies before the harness emits a session identifier -- exactly
// what a misconfigured harness does -- never updates session_harness, so a
// rule written against that column would read "changed" on every tick and
// redispatch forever with no human in the loop. BeginDispatch stamps these
// before the agent runs, so a single change buys a single retirement.
//
// Both comparisons treat an empty recorded value as UNKNOWN rather than as a
// change, which is the same reading resumable applies and for the same reason:
// rows predating the column would otherwise all retire at once on upgrade.
//
// provider is the provider serving the model this dispatch would use, resolved
// by the caller (Decide is pure). It is empty for claude and empty whenever
// resolution failed; either way the provider comparison is skipped and the cap
// stands.
func configRetired(cfg *config.Config, state store.IssueState, ov config.Overrides, provider string) string {
	if h := EffectiveHarness(cfg, ov); state.DispatchHarness != "" && state.DispatchHarness != h {
		return fmt.Sprintf(
			"retiring the retry history: it belongs to %s and this dispatch runs %s",
			state.DispatchHarness, h)
	}
	// A model change WITHIN one provider is not a retirement. Swapping one
	// OpenRouter model for another while OpenRouter is out of credits changes
	// nothing the failures were about, so the cap must still hold. Crossing to
	// another provider is a different account with its own balance, and the
	// failures stop being evidence.
	if state.DispatchProvider != "" && provider != "" && state.DispatchProvider != provider {
		return fmt.Sprintf(
			"retiring the retry history: it belongs to provider %s and this dispatch runs %s",
			state.DispatchProvider, provider)
	}
	return ""
}
```

- [ ] **Step 4: Apply it in `retryDecision`**

Change the signature to accept the provider, and insert the retirement above the cap check:

```go
func retryDecision(cfg *config.Config, number int, state store.IssueState,
	ov config.Overrides, provider string, now time.Time, force bool) (*Decision, bool, string) {
	// ABOVE the cap and above the backoff window, because it retires both. The
	// window was sized to let the old configuration's platform recover, and
	// that is not the platform about to be used.
	//
	// KindStart, not KindRetryStart: loopcmd dispatches a start with
	// isRetry=false, and store.BeginDispatch then clears parked, retry_count
	// and retry_after in one statement. That reset IS the retirement; there is
	// no separate unpark. The second result is false so a retirement never
	// counts toward the circuit breaker -- a human reconfiguring the loop is
	// not evidence of a platform fault, and counting it would drop every other
	// issue's dispatch for the cooldown.
	if reason := configRetired(cfg, state, ov, provider); reason != "" {
		return &Decision{
			Kind:      KindStart,
			Issue:     number,
			SessionID: "",
			Reason:    reason,
		}, false, ""
	}

	if state.RetryCount >= cfg.Retry.Max {
		// ... unchanged
```

Update the call in `Decide` to pass the provider — for this task pass `st.Providers[iss.Number]`, which does not exist yet, so pass `""` and leave `// Providers arrives in Task 4.` Task 4 replaces it.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/engine/ -count=1`
Expected: PASS, all tests in the package.

- [ ] **Step 6: Commit**

```bash
make check
git add internal/engine/engine.go internal/engine/engine_test.go
git commit -m "feat(engine): a harness change retires the retry history"
```

---

### Task 3: Resolve a model's pi provider

**Files:**
- Create: `internal/runner/provider.go`
- Test: `internal/runner/provider_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func ParseModelTable(r io.Reader, model string) string` and `func ResolveProvider(ctx context.Context, model string) string`.

- [ ] **Step 1: Write the failing test**

Create `internal/runner/provider_test.go`:

```go
package runner

import (
	"strings"
	"testing"
)

const listModelsOutput = `provider      model                                  context  max-out  thinking  images
openai-codex  gpt-5.6-terra                          272K     128K     yes       yes
openrouter    deepseek/deepseek-v4-flash-0731        1.0M     943.7K   yes       no
openrouter    deepseek/deepseek-v4-flash-0731:batch  1.0M     943.7K   yes       no
openrouter    openai/gpt-5.6-terra                   1.1M     128K     yes       yes
`

// A bare OpenRouter id. The "deepseek/" prefix is the VENDOR, not the
// provider -- reading the first path segment as a provider is the mistake this
// pins against.
func TestParseModelTableResolvesBareOpenRouterID(t *testing.T) {
	got := ParseModelTable(strings.NewReader(listModelsOutput), "deepseek/deepseek-v4-flash-0731")
	if got != "openrouter" {
		t.Errorf("provider = %q, want %q", got, "openrouter")
	}
}

// The provider/model shape, which is how an openai-codex model is labelled.
func TestParseModelTableResolvesProviderPrefixedID(t *testing.T) {
	got := ParseModelTable(strings.NewReader(listModelsOutput), "openai-codex/gpt-5.6-terra")
	if got != "openai-codex" {
		t.Errorf("provider = %q, want %q", got, "openai-codex")
	}
}

// The search is fuzzy, so a prefix must not resolve to its longer neighbour.
func TestParseModelTableIgnoresSuffixMatches(t *testing.T) {
	got := ParseModelTable(strings.NewReader(listModelsOutput), "deepseek/deepseek-v4-flash")
	if got != "" {
		t.Errorf("provider = %q, want empty for a non-exact match", got)
	}
}

// "gpt-5.6-terra" is served by openai-codex as a bare id AND by openrouter as
// openai/gpt-5.6-terra. Ambiguity is unresolved, not a coin flip.
func TestParseModelTableRejectsAmbiguity(t *testing.T) {
	ambiguous := `provider      model          context
openai-codex  gpt-5.6-terra  272K
openrouter    gpt-5.6-terra  1.1M
`
	got := ParseModelTable(strings.NewReader(ambiguous), "gpt-5.6-terra")
	if got != "" {
		t.Errorf("provider = %q, want empty when two providers serve the id", got)
	}
}

// A miss prints a sentence and exits 0, so the rows are the only signal.
func TestParseModelTableHandlesAMiss(t *testing.T) {
	got := ParseModelTable(strings.NewReader("No models matching \"zzz\"\n"), "zzz")
	if got != "" {
		t.Errorf("provider = %q, want empty", got)
	}
}

func TestParseModelTableHandlesEmptyInput(t *testing.T) {
	if got := ParseModelTable(strings.NewReader(""), "anything"); got != "" {
		t.Errorf("provider = %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/runner/ -run ParseModelTable -count=1`
Expected: FAIL — `undefined: ParseModelTable`.

- [ ] **Step 3: Write the implementation**

Create `internal/runner/provider.go`:

```go
package runner

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"time"
)

// ResolveProvider returns the pi provider serving model, or "" when it cannot
// be determined.
//
// Provider identity is what says whether two models share a balance.
// openrouter and openai-codex are separate accounts, so a 402 on one is no
// evidence about the other -- which is the whole reason the engine wants to
// know. pi owns the mapping and `pi --list-models` is its published surface
// for it, so this shells out rather than reading pi's private
// models-store.json cache: if that file moves or changes shape, resolution
// degrades to "unknown" and the retry cap simply keeps its current behaviour,
// where a broken parse of a private file could do something worse.
//
// Every failure is silent and returns "". This is an optimisation on a repair
// path, not a correctness requirement: an unresolved provider means the cap
// stands, which is what happens today.
func ResolveProvider(ctx context.Context, model string) string {
	if strings.TrimSpace(model) == "" {
		return ""
	}
	// A short deadline of its own. This runs inside a tick that holds the
	// loop's flock, and a hung `pi` must not hold it: no answer and the cap
	// standing is strictly better than a stalled loop.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// The model is passed as the search argument, not interpolated into a
	// shell string: exec.CommandContext takes an argv, so a model name can
	// never become another argument or a command.
	cmd := exec.CommandContext(ctx, "pi", "--list-models", model)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		// A miss EXITS 0 and prints a sentence, so a non-zero status here means
		// pi is absent or broken, not that the model is unknown. Either way the
		// answer is the same.
		return ""
	}
	return ParseModelTable(&out, model)
}

// ParseModelTable reads `pi --list-models` output and returns the provider of
// the row matching model exactly, or "" when there is no unambiguous match.
//
// The match must be EXACT because the search is fuzzy: asking for
// "deepseek/deepseek-v4-flash-0731" also returns the ":batch" variant, and
// asking for a bare id can return rows from several providers. Two shapes
// count as a match, because both are in use as model: labels -- the bare id in
// the model column ("deepseek/deepseek-v4-flash-0731", an OpenRouter id whose
// first segment is the vendor) and provider-qualified
// ("openai-codex/gpt-5.6-terra").
//
// Rows spanning more than one provider are unresolved rather than
// first-wins. Guessing here would retire a retry cap on a provider change that
// did not happen.
func ParseModelTable(r io.Reader, model string) string {
	sc := bufio.NewScanner(r)
	found := ""
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		provider, id := fields[0], fields[1]
		if provider == "provider" && id == "model" {
			// The header. It is not a row, and "provider" is not a provider.
			continue
		}
		if id != model && provider+"/"+id != model {
			continue
		}
		if found != "" && found != provider {
			// Two providers serve this name. Unresolved.
			return ""
		}
		found = provider
	}
	return found
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/runner/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
make check
git add internal/runner/provider.go internal/runner/provider_test.go
git commit -m "feat(runner): resolve a model's pi provider from pi --list-models"
```

---

### Task 4: Wire the resolved provider through the tick

**Files:**
- Modify: `internal/engine/types.go` (`State.Providers`, `Decision.Provider`)
- Modify: `internal/engine/engine.go` (pass `st.Providers[iss.Number]` into `retryDecision`; carry the provider onto every dispatch decision)
- Modify: `internal/loopcmd/tick.go` (build the map, pass `d.Provider` to `BeginDispatch`)
- Modify: `internal/loopcmd/tickissue.go` (build the map for the scoped tick)
- Test: `internal/engine/engine_test.go`, `internal/loopcmd/tick_test.go`

**Interfaces:**
- Consumes: `configRetired` (Task 2), `runner.ResolveProvider` (Task 3), `BeginDispatch(..., harness, provider, ...)` (Task 1).
- Produces: `engine.State.Providers map[int]string`, `engine.Decision.Provider string`, and `func resolveProviders(ctx context.Context, cfg *config.Config, issues []ghub.Issue) map[int]string` in `loopcmd`.

- [ ] **Step 1: Write the failing test**

In `internal/engine/engine_test.go`:

```go
// openrouter is out of credits; openai-codex is a different account with its
// own balance, so the failures stop being evidence.
func TestProviderChangeRetiresTheRetryCap(t *testing.T) {
	cfg := testConfig()
	cfg.Agent.Harness = config.HarnessPi
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{
		Issues: map[int]store.IssueState{
			1: {
				SessionStarted:   true,
				DispatchHarness:  config.HarnessPi,
				DispatchProvider: "openrouter",
				NeedsRetry:       true,
				RetryCount:       3,
			},
		},
		Providers: map[int]string{1: "openai-codex"},
	}

	p := Decide(cfg, snap, st, time.Now())

	if got := kinds(p); len(got) != 1 || got[0] != KindStart {
		t.Fatalf("kinds = %v, want [%v]", got, KindStart)
	}
}

// Swapping one OpenRouter model for another while OpenRouter is out of credits
// changes nothing the failures were about.
func TestSameProviderStillParksAtTheCap(t *testing.T) {
	cfg := testConfig()
	cfg.Agent.Harness = config.HarnessPi
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{
		Issues: map[int]store.IssueState{
			1: {
				SessionStarted:   true,
				DispatchHarness:  config.HarnessPi,
				DispatchProvider: "openrouter",
				NeedsRetry:       true,
				RetryCount:       3,
			},
		},
		Providers: map[int]string{1: "openrouter"},
	}

	p := Decide(cfg, snap, st, time.Now())

	if got := kinds(p); len(got) != 1 || got[0] != KindParkRetryExhausted {
		t.Fatalf("kinds = %v, want [%v]", got, KindParkRetryExhausted)
	}
}

// An unresolved provider must not retire anything. Fail closed.
func TestUnresolvedProviderDoesNotRetire(t *testing.T) {
	cfg := testConfig()
	cfg.Agent.Harness = config.HarnessPi
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{
		Issues: map[int]store.IssueState{
			1: {
				SessionStarted:   true,
				DispatchHarness:  config.HarnessPi,
				DispatchProvider: "openrouter",
				NeedsRetry:       true,
				RetryCount:       3,
			},
		},
		Providers: map[int]string{},
	}

	p := Decide(cfg, snap, st, time.Now())

	if got := kinds(p); len(got) != 1 || got[0] != KindParkRetryExhausted {
		t.Fatalf("kinds = %v, want [%v]", got, KindParkRetryExhausted)
	}
}

// Every dispatch decision carries the provider it resolved to, so the tick can
// stamp it without resolving a second time.
func TestStartCarriesTheResolvedProvider(t *testing.T) {
	cfg := testConfig()
	cfg.Agent.Harness = config.HarnessPi
	snap := Snapshot{Issues: []ghub.Issue{issue(1, cfg.Labels.Trigger)}}
	st := State{
		Issues:    map[int]store.IssueState{},
		Providers: map[int]string{1: "openrouter"},
	}

	p := Decide(cfg, snap, st, time.Now())

	if len(p.Decisions) != 1 || p.Decisions[0].Provider != "openrouter" {
		t.Fatalf("Provider = %q, want %q", p.Decisions[0].Provider, "openrouter")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/engine/ -run 'Provider' -count=1`
Expected: FAIL — `unknown field Providers in struct literal of type State`.

- [ ] **Step 3: Add the fields**

In `internal/engine/types.go`, add to `State`:

```go
	// Providers maps an issue number to the pi provider that would serve the
	// model its next dispatch runs. The CALLER resolves it -- resolution shells
	// out to `pi --list-models`, and Decide performs no I/O.
	//
	// A missing entry means unresolved, which is never treated as a provider
	// change. Callers that can never retire a cap (the tend sweep, which makes
	// no retry decisions) leave it nil.
	Providers map[int]string
```

and to `Decision`:

```go
	// Provider is the pi provider serving this dispatch's model, copied from
	// State.Providers. It travels on the decision so the tick can stamp it in
	// BeginDispatch without resolving a second time -- and so the value the
	// engine COMPARED is the value the store records, which is what keeps the
	// comparison stable across ticks.
	Provider string
```

- [ ] **Step 4: Carry it through `Decide`**

In `internal/engine/engine.go`, inside the issue loop, take `provider := st.Providers[iss.Number]` next to the `ov, ovErr := config.ParseOverrides(...)` line. Pass it to `retryDecision`. Set `Provider: provider` on the `KindResume` and `KindStart` decisions in the trigger branch, on the retirement decision, and on both retry decisions inside `retryDecision` (which already receives `provider`).

Leave `tendDecisions` alone — a `KindTend` decision keeps an empty `Provider`. Add this to its doc comment so the omission is a decision rather than an oversight:

```go
// A tend carries no resolved Provider. It makes no retry decision and can
// never retire a cap, and BeginDispatch is not called for a tend at all
// (loopcmd skips it for store.KindTend), so there is nothing for the value to
// reach.
```

- [ ] **Step 5: Build the map in the tick**

In `internal/loopcmd/tick.go`, add:

```go
// resolveProviders returns the pi provider serving each issue's next dispatch,
// keyed by issue number.
//
// It is built here rather than in the engine because resolution shells out and
// engine.Decide is pure. Only a pi dispatch has a provider: claude is one
// vendor reached one way, so there is nothing for a provider comparison to
// say about it, and resolving would spend a subprocess to learn "".
//
// One resolution per distinct MODEL, not per issue. A loop of thirty issues
// almost always runs one model, and `pi --list-models` is a process spawn.
func resolveProviders(ctx context.Context, cfg *config.Config, issues []ghub.Issue) map[int]string {
	out := make(map[int]string, len(issues))
	byModel := map[string]string{}
	for _, iss := range issues {
		// A label this loop cannot parse is not this function's problem: Decide
		// turns it into a KindStop, and an unresolved provider there changes
		// nothing.
		ov, err := config.ParseOverrides(iss.Labels)
		if err != nil {
			continue
		}
		eff := runner.Effective(cfg, ov)
		if engine.EffectiveHarness(cfg, ov) != config.HarnessPi || eff.Model == "" {
			continue
		}
		provider, seen := byModel[eff.Model]
		if !seen {
			provider = runner.ResolveProvider(ctx, eff.Model)
			byModel[eff.Model] = provider
		}
		if provider != "" {
			out[iss.Number] = provider
		}
	}
	return out
}
```

Call it where `st` is built (`internal/loopcmd/tick.go:231`):

```go
	st := engine.State{
		// ... existing fields
		Providers: resolveProviders(ctx, cfg, snap.Issues),
	}
```

Do the same at `internal/loopcmd/tickissue.go:133`. Leave `internal/loopcmd/tendsweep.go:234` alone: the sweep only tends, makes no retry decision, and cannot retire a cap.

- [ ] **Step 6: Stamp it**

In `internal/loopcmd/tick.go`, replace the `""` placeholder from Task 1 Step 5 with `d.Provider`.

- [ ] **Step 7: Run the tests**

Run: `go test ./... -count=1 -p 1`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
make check
git add internal/engine internal/loopcmd
git commit -m "feat(engine): a provider-crossing model change retires the retry history"
```

---

### Task 5: The park comment names the reason

**Files:**
- Modify: `internal/loopcmd/tick.go` (`retryCapComment`, `parkRetryExhausted`)
- Test: `internal/loopcmd/tick_test.go`

**Interfaces:**
- Consumes: `store.RecentDispatches(loop, repo string, issue, limit int) ([]store.Dispatch, error)` (already exists).
- Produces: `func failureSentence(apiError string) string` and `func redactForComment(s string) string`.

- [ ] **Step 1: Write the failing test**

In `internal/loopcmd/tick_test.go`:

```go
// The real OpenRouter 402, trimmed. The key-management URL names the key's
// identifier and must never reach a GitHub comment.
const openRouter402 = `402: {"message":"This request requires more credits, or fewer max_tokens. You requested up to 873072 tokens, but can only afford 469825. To increase, visit https://openrouter.ai/workspaces/default/keys/15a8e996fc9ffff0cd339779332daf18263705193a275ad56eda0caf49e30d10 and adjust the key's daily limit","code":402}`

func TestFailureSentenceExtractsTheProviderMessage(t *testing.T) {
	got := failureSentence(openRouter402)

	if !strings.Contains(got, "requires more credits") {
		t.Errorf("sentence = %q, want the provider's own message", got)
	}
	if strings.Contains(got, "openrouter.ai/workspaces") {
		t.Errorf("sentence = %q, must not carry the key URL", got)
	}
	if strings.Contains(got, "15a8e996") {
		t.Errorf("sentence = %q, must not carry the key identifier", got)
	}
}

func TestRedactForCommentDropsEveryURL(t *testing.T) {
	got := redactForComment("see https://example.com/secret/abc and http://x.y/z now")
	if strings.Contains(got, "http") {
		t.Errorf("redacted = %q, want no URLs", got)
	}
	if !strings.Contains(got, "see") || !strings.Contains(got, "now") {
		t.Errorf("redacted = %q, want the surrounding prose kept", got)
	}
}

func TestRedactForCommentCapsLength(t *testing.T) {
	got := redactForComment(strings.Repeat("a", 900))
	if len([]rune(got)) > 301 {
		t.Errorf("len = %d runes, want at most 301 including the ellipsis", len([]rune(got)))
	}
}

// Truncation must not split a multi-byte rune into mojibake.
func TestRedactForCommentTruncatesOnRuneBoundaries(t *testing.T) {
	got := redactForComment(strings.Repeat("é", 900))
	if !utf8.ValidString(got) {
		t.Errorf("redacted = %q, want valid UTF-8", got)
	}
}

// A failure the runner could not describe must not become an invented cause.
func TestFailureSentenceEmptyForNoRecordedError(t *testing.T) {
	if got := failureSentence(""); got != "" {
		t.Errorf("sentence = %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/loopcmd/ -run 'FailureSentence|RedactForComment' -count=1`
Expected: FAIL — `undefined: failureSentence`.

- [ ] **Step 3: Write the helpers**

In `internal/loopcmd/tick.go`, near `retryCapComment`:

```go
// urlPattern matches any absolute URL. Deliberately greedy to the next space:
// a partially redacted URL is worse than none, because it still shows the
// prefix that identifies the resource.
var urlPattern = regexp.MustCompile(`https?://\S+`)

// failureSentence renders a dispatch's api_error for a PUBLIC GitHub comment,
// or "" when nothing was recorded.
//
// api_error is often a bare status followed by the provider's JSON body --
// `402: {"message":"...","code":402,...}`. The message field is the sentence a
// human needs; the rest is envelope. Extracting it is best-effort: anything
// that does not parse is redacted and truncated as-is, which still beats
// saying nothing.
func failureSentence(apiError string) string {
	s := strings.TrimSpace(apiError)
	if s == "" {
		return ""
	}
	if i := strings.Index(s, "{"); i >= 0 {
		var body struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(s[i:]), &body); err == nil && body.Message != "" {
			prefix := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s[:i]), ":"))
			if prefix != "" {
				return redactForComment(prefix + ": " + body.Message)
			}
			return redactForComment(body.Message)
		}
	}
	return redactForComment(s)
}

// redactForComment strips what must not be published and caps the length.
//
// A provider is free to put anything in an error string, and OpenRouter's 402
// puts a key-management URL in it:
// https://openrouter.ai/workspaces/default/keys/<id>. That names the key and is
// credential-adjacent, so every URL goes. The unredacted text stays on the
// dispatch row and in the run log, where `agent-utils project logs` can still
// reach it.
func redactForComment(s string) string {
	s = urlPattern.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	const max = 300
	r := []rune(s)
	if len(r) > max {
		// Cut on a rune boundary. Slicing the byte string would split a
		// multi-byte rune and put mojibake in a GitHub comment.
		s = strings.TrimSpace(string(r[:max])) + "…"
	}
	return s
}
```

Add `encoding/json` and `regexp` to the file's imports.

- [ ] **Step 4: Record the provider on the dispatch row**

`capCause` names the provider that failed, and `store.Dispatch` has no such field yet. Add it alongside the `model`/`harness`/`effort` columns it sits with:

- `internal/store/store.go`: add `provider TEXT NOT NULL DEFAULT ''` to the `dispatches` block of `schemaTables`, an `{"dispatches", "provider", "TEXT NOT NULL DEFAULT ''"}` entry in `addedColumns`, and the column to the `CreateDispatch` insert plus every `SELECT`/scan list over `dispatches` (`recentDispatches`, `RunningDispatches`, `DispatchesBySession`, `DispatchesForLoop`, `GetDispatch`).
- `internal/store/types.go`: add to `Dispatch`:

```go
	// Provider is the pi provider that served Model, as resolved when the
	// dispatch was decided. It is the same value stamped on the issue row, so
	// a park comment naming it names what the engine actually compared.
	Provider string
```

- `internal/loopcmd/tick.go`: set `Provider: d.Provider` in the `CreateDispatch` call, beside `Model`/`Harness`/`Effort`.

- [ ] **Step 5: Use it in the comment**

Replace the constant and the body construction:

```go
const retryCapComment = `🔁 **Orphan retry cap reached (%d/%d)** — %d consecutive agent dispatches for this issue failed to complete.%s

To proceed: re-add the ` + "`%s`" + ` label once the underlying issue has cleared, and this resumes normally.`

// capCause renders the sentence between the cap line and the instruction: what
// actually went wrong, when the loop recorded it.
//
// The generic "sustained platform-side issue" wording is the FALLBACK, not the
// default. It was the whole comment once, and an operator reading it learned
// nothing they could act on -- the 402 that caused it was sitting in the
// dispatch row the entire time. When the reason is known, say it; when it is
// not, do not assert a cause.
func capCause(d store.Dispatch) string {
	reason := failureSentence(d.APIError)
	if reason == "" {
		return " This usually indicates a sustained platform-side issue rather than a problem with the issue itself. Parking here rather than retrying indefinitely."
	}
	where := ""
	if parts := nonEmpty(d.Harness, d.Model, d.Provider); len(parts) > 0 {
		where = fmt.Sprintf(" under %s", strings.Join(parts, ", "))
	}
	return fmt.Sprintf(
		"\n\nThe last failure reported:\n\n> %s\n\nThat ran%s. Parking here rather than retrying indefinitely.",
		reason, where)
}

// nonEmpty drops the empty strings so a claude dispatch, which records no
// provider, does not render an empty slot in the parenthetical.
func nonEmpty(vals ...string) []string {
	var out []string
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, s)
		}
	}
	return out
}
```

In `parkRetryExhausted`, after the `state.Parked` early return and before building the body:

```go
	// Best-effort. The park is the point of this function; a database read that
	// fails must not stop the comment or the label edits, so the cause is
	// simply omitted and the fallback wording stands.
	var last store.Dispatch
	if runs, err := deps.Store.RecentDispatches(cfg.Name, cfg.Repo, d.Issue, 5); err == nil {
		for _, r := range runs {
			if r.Status == store.StatusFailed {
				last = r
				break
			}
		}
	}

	body := fmt.Sprintf(retryCapComment,
		cfg.Retry.Max, cfg.Retry.Max, cfg.Retry.Max, capCause(last), cfg.Labels.Trigger)
```

`RecentDispatches` returns newest first; confirm that against its query before relying on the `break`, and reverse the scan if it does not.

- [ ] **Step 6: Run the tests**

Run: `go test ./... -count=1 -p 1`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
make check
git add internal/loopcmd internal/store
git commit -m "feat(tick): the retry-cap comment names the failure it parked on"
```

---

### Task 6: Correct the stale comment and document the behaviour

**Files:**
- Modify: `internal/engine/engine.go:182` (the comment that asserts a re-trigger un-parks)
- Modify: `docs/configuration.md`

- [ ] **Step 1: Fix the comment**

The claim at `internal/engine/engine.go:182` — "a human who re-applies that label deliberately un-parks the issue" — is false on the failure path, which is the defect that started this. Replace it:

```go
		// A parked issue needs no separate guard here. parkRetryExhausted removes
		// the trigger label, so the check below already skips it.
		//
		// Re-applying that label reaches this branch only when needs_retry is
		// already clear -- parkRetryExhausted clears it with the park. While it
		// is still set, the failure path above owns the issue, and it un-parks
		// only when the configuration itself changed (see configRetired). That
		// asymmetry is deliberate: an unchanged configuration that failed its
		// whole budget will fail again, and a label edit is not new evidence.
```

- [ ] **Step 2: Document it**

Add to `docs/configuration.md`, under the retry section, matching the file's existing heading style:

```markdown
### Retiring a retry cap

An issue parked at `retry.max` un-parks when the configuration the failures
describe is no longer the one in play:

- Changing the harness (`harness:` label, or the loop's `agent.harness`)
  retires the accumulated failures and skips the remaining backoff.
- Changing the model to one served by a **different pi provider** does the
  same. `openrouter` and `openai-codex` are separate accounts, so a credit
  failure on one is no evidence about the other. A model change within a
  provider does not retire anything.

Re-applying the trigger label alone does not un-park an issue whose
configuration has not changed. The cap comment names the failure it parked on,
so the reason to change is in the issue.
```

- [ ] **Step 3: Commit**

```bash
make check
git add internal/engine/engine.go docs/configuration.md
git commit -m "docs: describe when a retry cap is retired"
```

---

## Verification

After Task 6, verify against the incident that produced this:

```bash
go build -o bin/agent-utils ./cmd/agent-utils
sqlite3 ~/.agent-utils/state.db \
  "select number, retry_count, parked, session_harness, dispatch_harness, dispatch_provider
   from issues where number = 74 and repo like '%koinos%';"
```

`dispatch_harness` and `dispatch_provider` are empty on that row — it predates the columns — so the harness rule reads unknown and the cap stands, which is correct and expected. The row's `parked = 1` with `needs_retry = 0` means re-applying `status:ready-for-execution` already reaches the trigger branch and starts a fresh claude session. Confirm that reading before touching the live issue, and do not edit labels on `koinos#74` as part of executing this plan.
