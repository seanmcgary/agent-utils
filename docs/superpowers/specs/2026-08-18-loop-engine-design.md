# Loop engine design

Date: 2026-08-18
Status: approved (design agreed in session, 2026-08-18)

## 1. Purpose

`agent-utils` is a Go command line tool. It holds utilities for agent workflows.

The first utility is the **loop engine**. The loop engine reads GitHub issues for one repository.
It filters the issues by label. It then dispatches a Claude Code agent to work on each selected
issue.

Today an LLM orchestrator does this work. The orchestrator reads labels, counts slots, compares
timestamps, and dispatches agents. All of that work is deterministic. An LLM does it slowly and
at a token cost that repeats on every tick. The loop engine moves that work into Go.

Two reference documents describe the current LLM orchestrators:

- `/Users/seanmcgary/Code/lawndominator/docs/planning-loop.md`
- `/Users/seanmcgary/Code/lawndominator/docs/execution-loop.md`

The loop engine must model both loops from one code path and two configuration files.

## 2. Scope

### In scope

- One generic loop engine. Each loop is a YAML configuration file.
- Selection of issues by label role.
- Dispatch of `claude -p` as a detached process.
- Session continuity for each issue, so a resumed issue continues its original conversation.
- Durable state in SQLite across cron invocations.
- Git worktree lifecycle for each dispatched agent.
- Retry policy for failed dispatches.
- Rebase of a stale pull request through a dispatched agent.
- Observability: log files, structured logs, a status command, and cost records.

### Out of scope

- Any GitHub write from Go, with one stated exception (see section 8.3). The agent owns
  comments and label transitions.
- Automatic resume from new comments. A human must apply the trigger label.
- Concurrency caps and priority ordering. The operator controls load through labels.
- Merging a pull request. The engine never merges.

## 3. Premise check findings

The repository holds only `go.mod`. There is no prior art to reuse. The following facts come
from direct inspection, not from memory.

### 3.1 Session continuity works across processes

The design depends on one behavior: a Claude Code session must survive between two separate
processes. A test confirmed this behavior.

- Process 1: `claude -p --session-id <uuid> --model haiku "Remember this number: 4271."`
  The result was `OK`.
- Process 2: `claude -p -r <uuid> --model haiku "What number did I ask you to remember?"`
  The result was `4271`.

The caller assigns the session identifier. The engine does not need to parse a generated
identifier out of the first response.

### 3.2 Session resume does not depend on the working directory

A third process ran the same resume from a different working directory. The result was again
`4271`. Session continuity is therefore independent of the working directory.

This finding relaxes a constraint. A stable worktree path is necessary for **branch** state. It
is not necessary for **session** state.

### 3.3 The result JSON schema

`claude -p --output-format json` prints one JSON object. The object holds these fields, among
others:

| Field | Type | Use |
|---|---|---|
| `session_id` | string | Confirms the session identifier. |
| `total_cost_usd` | number | Cost record for the dispatch. |
| `duration_ms` | number | Duration record for the dispatch. |
| `num_turns` | number | Diagnostic record. |
| `is_error` | boolean | Marks a failed run. |
| `subtype` | string | `success` on a clean run. |
| `api_error_status` | string or null | Identifies a platform error, such as a 529. |
| `result` | string | The final agent text. |
| `permission_denials` | array | Diagnostic record. |

### 3.4 The label filter in go-github is an AND filter

`github.IssueListByRepoOptions.Labels` selects issues that carry **all** the listed labels. It
does not select issues that carry **any** of them.

The engine therefore makes one call to `Issues.ListByRepo` with `State: "open"` and no label
filter. The engine then filters the issues in Go. This approach costs fewer API calls than one
call for each label role. It also gives the engine the complete label set for each issue, which
the veto rule needs.

### 3.5 The issue list includes pull requests

`Issues.ListByRepo` returns pull requests together with issues. The engine must discard every
item for which `Issue.IsPullRequest()` is true. If the engine does not do this, it treats a pull
request as an issue.

### 3.6 Blast radius

The blast radius is one repository: `seanmcgary/agent-utils`. The engine reads the GitHub API of
a **target** repository and creates worktrees from a **target** checkout. The engine changes no
code in a target repository. The two reference documents are input only.

## 4. Architecture

Go owns every deterministic decision. The agent owns every judgment and every GitHub write.

```
cron ──► agent-utils loop tick --config planning.yaml
           │
           ├─ 1. LOCK      acquire a per-loop file lock; exit if another tick holds it
           ├─ 2. RECONCILE read GitHub issues; read SQLite dispatch rows; check process liveness
           ├─ 3. DECIDE    pure function: (snapshot, state, now) -> []Decision
           ├─ 4. ACT       spawn detached `agent-utils internal run-agent --dispatch <id>`
           └─ 5. exit      fast; no daemon

agent-utils internal run-agent --dispatch <id>   (detached; one process per dispatch)
           ├─ ensure the git worktree
           ├─ exec `claude -p …`; tee stream-json to a log file
           └─ record exit code, cost, and duration in SQLite
```

### 4.1 Why a detached self-invocation

The tick process must exit quickly, because cron starts it. The agent process runs for a long
time. Some process must therefore outlive the tick to record how the agent ended.

The engine solves this with a self-invocation. The tick spawns `agent-utils internal run-agent`.
That process runs `claude` as its child and waits. It then writes the exit code, the cost, and
the duration into SQLite.

This design makes orphan detection a fact instead of a guess. An orphan is a dispatch row with
`status = 'running'` whose process identifier is dead. The LLM orchestrator could not know this,
which is why its reference document contains a large amount of retry guesswork.

## 5. Components

| Package | Responsibility |
|---|---|
| `internal/config` | Reads and validates the YAML file. Rejects unknown keys. |
| `internal/ghub` | Wraps go-github behind an interface. Lists issues and pull requests. Compares branches. |
| `internal/store` | SQLite access. Holds the schema, the migrations, and the queries. |
| `internal/engine` | Pure decision function. Holds no I/O. |
| `internal/runner` | Builds the `claude` command line. Spawns the detached process. Parses the result. |
| `internal/worktree` | Creates, lists, and removes git worktrees. |
| `cmd/agent-utils` | Command wiring with `urfave/cli` v3. |

### 5.1 Commands

| Command | Purpose |
|---|---|
| `loop tick --config <file>` | Runs one tick and exits. This is the cron entry point. |
| `loop status --config <file>` | Prints the reconciled view. Makes no change. |
| `loop reset --config <file> --issue <n>` | Drops the stored session and worktree for one issue. |
| `internal run-agent --dispatch <id>` | Hidden. Runs one dispatch and records the result. |

## 6. Configuration

Each loop is one YAML file. The label roles are fixed. Both reference loops fit these roles.

```yaml
name: planning
repo: mcgarylabs/lawndominator-monorepo

checkout_base_dir: /Users/seanmcgary/Code/lawndominator
worktree_dir: /Users/seanmcgary/.agent-utils/worktrees

labels:
  trigger:   status:ready-for-spec
  in_flight: status:speccing
  blocked:   status:needs-spec-input
  review:    status:plan-ready-for-review
  terminal:  status:ready-for-execution
  veto:
    - blocked:design
    - status:ready-for-execution

agent:
  model: opus
  effort: high
  permission_mode: bypassPermissions
  worktree: per_issue
  max_budget_usd: 25
  timeout: 3h

tend_pr: true

retry:
  max: 3
  backoff_ticks: [0, 1, 2]
  breaker:
    orphan_threshold: 2
    cooldown: 30m

prompt: |
  Run the /plan-feature skill for GitHub issue #{{.Issue.Number}} in {{.Repo}}.
  …

resume_prompt: |
  Issue #{{.Issue.Number}} carries {{.Labels.Trigger}} again. Read the new comments …

tend_prompt: |
  PR #{{.PR.Number}} for issue #{{.Issue.Number}} is {{.PR.BehindBy}} commits behind
  {{.PR.BaseRef}}. Rebase it and force-push with --force-with-lease …
```

### 6.1 Label roles

| Role | Meaning | Applied by |
|---|---|---|
| `trigger` | The issue is cleared to run. This covers a first start and every resume. | Human |
| `in_flight` | An agent works on the issue now. | Agent |
| `blocked` | The agent parked. It waits for a human answer. | Agent |
| `review` | The agent finished. The output waits for a human read. | Agent |
| `terminal` | The issue left this loop. | Human |
| `veto` | A list. The engine skips the issue even when `trigger` is present. | Human |

The `veto` list is not the same as "no trigger label". An issue without the `trigger` label is
never selected. That is the default. The `veto` list handles the different case of an issue that
**does** carry the trigger label but must still be skipped. The reference documents need this
rule for `blocked:design`, and for an issue that a human approved while a run was in flight.

### 6.2 Paths

- `checkout_base_dir` is the primary checkout of the target repository. The engine runs
  `git fetch` in it. The engine never changes its branch and never edits its files.
- `worktree_dir` is the parent directory for every worktree the engine creates.

Worktree paths are deterministic:

- An issue dispatch uses `{worktree_dir}/{loop}/issue-{n}`.
- A tend dispatch uses `{worktree_dir}/{loop}/pr-{n}`.

A stable path lets a resumed run find the branch state it left. Section 3.2 shows that session
state does not need this, but branch state does.

### 6.3 Prompt templates

The three prompts are Go `text/template` strings inside the YAML file. The template data holds
the repository, the issue, the label names, the session identifier, and, for a tend run, the
pull request.

The engine renders `prompt` for a first start. It renders `resume_prompt` for a resume. It
renders `tend_prompt` for a tend run.

## 7. The tick

The engine runs these steps in order.

1. **Lock.** Take an exclusive file lock for this loop. If another tick holds the lock, exit
   with success and log the fact. This stops two cron ticks from dispatching the same issue.
2. **Fetch.** Run `git fetch` in `checkout_base_dir`.
3. **Reconcile.** Read all open issues. Discard pull requests. Read the dispatch rows. Check
   whether each running process is alive.
4. **Decide.** Call the pure decision function.
5. **Act.** Perform each decision.
6. **Exit.**

### 7.1 The four issue cases

| Case | Condition | Action |
|---|---|---|
| Start | `trigger` present, no session row | Create a session identifier. Create the worktree. Dispatch with `prompt`. |
| Resume | `trigger` present, session row exists | Dispatch with `resume_prompt` and `-r <session-id>`. |
| Healthy | `in_flight` present, process alive | Do nothing. Spend no tokens and make no API write. |
| Orphan | `in_flight` present, dispatch is `running`, process dead | Apply the retry policy. |

The **Resume** case is the reason this project exists. The reference documents make a human
re-apply a label and then make an LLM work out whether the issue was seen before. Go answers
that question from one SQLite row.

### 7.2 Tend a pull request

The engine runs these steps for each issue that carries the `review` label, when `tend_pr` is
true.

1. **Link the pull request.** List the open pull requests once for each tick. Match a pull
   request to an issue when the body contains `Closes #<n>`. Cache the mapping in SQLite.
2. **Measure the gap.** Call `Repositories.CompareCommits(ctx, owner, repo, base, head, nil)`.
   Read `BehindBy`.
3. **Decide.** Dispatch a tend agent only when `BehindBy > 0` and no live tend dispatch exists
   for the pull request.

A current pull request costs one API call and produces nothing. Silence is the correct output.

A tend run differs from an issue run in three ways:

- The engine never changes a label for a tend run.
- The engine starts a **new** session for each tend run. A rebase is idempotent, so it needs no
  memory of an earlier rebase.
- The worktree checks out the pull request head branch.

For version 1, `tend_pr` covers a stale branch only. It does not answer review feedback. The
condition list in section 7.2 step 3 is the extension point for that later work.

## 8. Error handling

### 8.1 Failure classes

| Failure | Detection | Response |
|---|---|---|
| GitHub API error | Error from go-github | Log the error. Abort the tick. Exit non-zero. |
| Agent exits non-zero | Exit code from the child | Mark the dispatch failed. The next tick retries. |
| Agent reports an API error | `api_error_status` is not null | Mark the dispatch failed. The next tick retries. |
| Runner process killed | Row is `running` and the process is dead | Treat as an orphan. Retry. |
| Worktree creation fails | Error from git | Mark the dispatch failed. Do not dispatch. |

### 8.2 Retry policy

The policy follows the reference documents, but it reads facts instead of comments.

- **Cap.** Retry a dispatch at most `retry.max` times. The counter lives in SQLite. The
  reference documents count marker comments on the issue, which is fragile.
- **Backoff.** `retry.backoff_ticks` gives the number of ticks to wait before each retry. The
  default `[0, 1, 2]` matches the reference documents.
- **Circuit breaker.** When `retry.breaker.orphan_threshold` or more issues are orphaned and
  past their backoff in the same tick, treat the event as a platform failure. Skip every
  dispatch for that tick. Record a cooldown of `retry.breaker.cooldown`.

### 8.3 The one GitHub write

Go performs one GitHub write, and only one. When an issue reaches the retry cap, Go posts a
comment and moves the issue from `in_flight` to `blocked`.

This is a deliberate exception to the rule in section 2. The reference documents make the same
exception, and for the same reason: the failing action is the dispatch itself. To dispatch an
agent whose task is to report that dispatch is broken would fail for the same cause.

## 9. Data model

SQLite, through `modernc.org/sqlite`. The driver name is `sqlite`. The driver needs no CGO.

```sql
CREATE TABLE issues (
  loop           TEXT NOT NULL,
  repo           TEXT NOT NULL,
  number         INTEGER NOT NULL,
  session_id     TEXT,
  worktree_path  TEXT,
  retry_count    INTEGER NOT NULL DEFAULT 0,
  last_retry_tick INTEGER,
  updated_at     TIMESTAMP NOT NULL,
  PRIMARY KEY (loop, repo, number)
);

CREATE TABLE dispatches (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  loop         TEXT NOT NULL,
  repo         TEXT NOT NULL,
  number       INTEGER NOT NULL,
  kind         TEXT NOT NULL,          -- 'start' | 'resume' | 'tend'
  session_id   TEXT,
  pid          INTEGER,
  pid_start_at TIMESTAMP,
  status       TEXT NOT NULL,          -- 'running' | 'succeeded' | 'failed'
  started_at   TIMESTAMP NOT NULL,
  finished_at  TIMESTAMP,
  exit_code    INTEGER,
  cost_usd     REAL,
  duration_ms  INTEGER,
  api_error    TEXT,
  log_path     TEXT
);

CREATE TABLE pr_links (
  loop      TEXT NOT NULL,
  repo      TEXT NOT NULL,
  number    INTEGER NOT NULL,          -- the issue number
  pr_number INTEGER NOT NULL,
  head_ref  TEXT NOT NULL,
  base_ref  TEXT NOT NULL,
  PRIMARY KEY (loop, repo, number)
);

CREATE TABLE ticks (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  loop            TEXT NOT NULL,
  started_at      TIMESTAMP NOT NULL,
  finished_at     TIMESTAMP,
  breaker_tripped INTEGER NOT NULL DEFAULT 0,
  summary_json    TEXT
);
```

`dispatches.pid_start_at` holds the process start time. A process identifier alone is not
sufficient, because the operating system reuses identifiers. The engine compares the recorded
start time with the live process start time before it decides the process is the same one.

## 10. Observability

- **Log files.** Each dispatch writes its `stream-json` output to
  `{log_dir}/{loop}/{issue}-{timestamp}.jsonl`.
- **Structured logs.** The tick writes `log/slog` JSON lines to standard output. Cron captures
  them.
- **Status command.** `loop status` prints the in-flight set, the parked set, the review set,
  the retry counters, the session identifiers, the worktree paths, and the last tick time.
- **Cost.** The runner records `total_cost_usd` and `duration_ms` for each dispatch. `loop
  status` reports the total for each issue.

## 11. Testing

`engine.Decide` is a pure function. It takes a snapshot, the stored state, and the current time.
It returns a list of decisions. It performs no I/O.

This design puts the whole state machine under table-driven unit tests, with no GitHub and no
process spawning:

- Selection: trigger present, trigger absent, veto label present.
- Start against resume, chosen by the presence of a session row.
- Orphan detection, the retry cap, and the backoff schedule.
- The circuit breaker threshold.
- Tend selection: behind, current, and a live tend dispatch.
- Pull request filtering, per section 3.5.

Other layers use fakes behind the `ghub` and `runner` interfaces. One integration test puts a
stub `claude` binary on `PATH`. The stub prints a fixture result JSON. That test covers the
runner end to end, including the result parse and the SQLite record.

## 12. Verified external API (do not re-derive)

Each signature below comes from `go doc` against the pinned version.

```go
// github.com/urfave/cli/v3 v3.11.0
type Command struct {
    Name     string
    Usage    string
    Version  string
    Commands []*Command
    Flags    []Flag
    Action   ActionFunc
    Hidden   bool
    Arguments []Argument
}
func (cmd *Command) Run(ctx context.Context, osArgs []string) (deferErr error)
type StringFlag = FlagBase[string, StringConfig, stringValue]

// github.com/google/go-github/v77 v77.0.0
func NewClient(httpClient *http.Client) *Client
func (c *Client) WithAuthToken(token string) *Client
func (i Issue) IsPullRequest() bool
func (s *RepositoriesService) CompareCommits(
    ctx context.Context, owner, repo, base, head string, opts *ListOptions,
) (*CommitsComparison, *Response, error)

type CommitsComparison struct {
    Status       *string // "behind" or "ahead"
    AheadBy      *int
    BehindBy     *int
    TotalCommits *int
    // …
}

type IssueListByRepoOptions struct {
    State  string   // "open", "closed", "all"
    Labels []string // AND filter; see section 3.4
    Sort   string
    // …
}

// modernc.org/sqlite v1.56.0
// driverName = "sqlite"; no CGO required.

// gopkg.in/yaml.v3 v3.0.1
```

Claude Code command line facts, confirmed by running the binary:

```
claude -p --session-id <uuid> --model <m> --output-format json "<prompt>"
claude -p -r <uuid> --model <m> --output-format json "<prompt>"
claude -p --output-format stream-json …    # for the log file
--permission-mode bypassPermissions
--max-budget-usd <amount>
--effort <low|medium|high|xhigh|max>
```

## 13. Open items

None. The design is agreed.
