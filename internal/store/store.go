// Package store holds the durable loop state in SQLite.
//
// There is ONE database for the machine, at ~/.agent-utils/state.db. Every row
// carries the identifier of the project that owns it, so one file can hold every
// project without any of them seeing another's state. Open returns the file
// handle; Project returns a view scoped to one project, and that view is what
// the loop code uses.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/seanmcgary/agent-utils/internal/home"
	"modernc.org/sqlite"
)

// The pragmas live in the DSN, not in this schema string. journal_mode is
// persisted in the file, but busy_timeout and foreign_keys are PER CONNECTION.
// The tick process and every detached runner open this file at the same time,
// so a pragma applied to one pooled connection does not protect the others.
//
// project_id is the first column of every key. It is a project's UUID, minted
// once in its descriptor, so it survives a rename and a move.
//
// Every new NOT NULL column carries DEFAULT ”. A binary from before this schema
// may still hold this file open, and its INSERT statements name no project.
const schemaTables = `
CREATE TABLE IF NOT EXISTS issues (
  project_id      TEXT NOT NULL DEFAULT '',
  loop            TEXT NOT NULL,
  repo            TEXT NOT NULL,
  number          INTEGER NOT NULL,
  session_id      TEXT NOT NULL DEFAULT '',
  worktree_path   TEXT NOT NULL DEFAULT '',
  retry_count     INTEGER NOT NULL DEFAULT 0,
  last_retry_tick INTEGER NOT NULL DEFAULT 0,
  needs_retry     INTEGER NOT NULL DEFAULT 0,
  session_started INTEGER NOT NULL DEFAULT 0,
  -- session_harness is the harness that CREATED the session, so a later
  -- dispatch can tell whether it may resume it. Each harness keeps its own
  -- session store, so an identifier minted by one means nothing to the other:
  -- claude refuses outright ("No conversation found with session ID"), and pi
  -- silently creates a new session under that id, losing the conversation
  -- without saying so. Empty means "recorded before this column existed".
  session_harness TEXT NOT NULL DEFAULT '',
  -- dispatch_harness and dispatch_provider record what the loop most recently
  -- ATTEMPTED, which is not the question session_harness answers.
  -- session_harness is the harness that CREATED the session, so it stays empty
  -- when a dispatch dies before the harness emits a session identifier -- which
  -- is exactly what a misconfigured harness does. The retirement rule in
  -- engine.configRetired reads "did the configuration change", and reading it
  -- from session_harness would answer yes on every tick and redispatch forever
  -- with no human in the loop. BeginDispatch stamps these BEFORE the agent
  -- runs, so one configuration change buys exactly one retirement.
  --
  -- Empty means "recorded before this column existed", which the engine reads
  -- as unknown rather than as a change.
  dispatch_harness  TEXT NOT NULL DEFAULT '',
  dispatch_provider TEXT NOT NULL DEFAULT '',
  parked          INTEGER NOT NULL DEFAULT 0,
  -- retry_after is Unix seconds, and 0 means "no deadline". It is an INTEGER
  -- where every other timestamp in this schema is a TIMESTAMP
  -- (issues.updated_at, cooldowns.until, ticks.started_at, the dispatches time
  -- columns). It does NOT match that precedent: addedColumns needs a literal
  -- DEFAULT so an existing database gains the column without a backfill, and no
  -- literal TIMESTAMP default reads back as the zero time.
  retry_after     INTEGER NOT NULL DEFAULT 0,
  -- stopped and stopped_reason are set by an operator's "sessions kill" or by
  -- a KindStop decision (an invalid label), and cleared only by
  -- "sessions resume". PutIssueState must never write them: it is a
  -- read-modify-write, and a state read before a kill and written after would
  -- silently un-stop the issue.
  stopped         INTEGER NOT NULL DEFAULT 0,
  stopped_reason  TEXT NOT NULL DEFAULT '',
  updated_at      TIMESTAMP NOT NULL,
  PRIMARY KEY (project_id, loop, repo, number)
);

CREATE TABLE IF NOT EXISTS dispatches (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id    TEXT NOT NULL DEFAULT '',
  loop          TEXT NOT NULL,
  repo          TEXT NOT NULL,
  number        INTEGER NOT NULL,
  kind          TEXT NOT NULL,
  session_id    TEXT NOT NULL DEFAULT '',
  pid           INTEGER NOT NULL DEFAULT 0,
  pid_start_at  TIMESTAMP,
  status        TEXT NOT NULL,
  started_at    TIMESTAMP NOT NULL,
  finished_at   TIMESTAMP,
  exit_code     INTEGER NOT NULL DEFAULT 0,
  cost_usd      REAL NOT NULL DEFAULT 0,
  duration_ms   INTEGER NOT NULL DEFAULT 0,
  api_error     TEXT NOT NULL DEFAULT '',
  log_path      TEXT NOT NULL DEFAULT '',
  pr_number     INTEGER NOT NULL DEFAULT 0,
  title         TEXT NOT NULL DEFAULT '',
  legacy_source TEXT NOT NULL DEFAULT '',
  legacy_id     INTEGER NOT NULL DEFAULT 0,
  -- agent_pid is the agent CHILD's pid, distinct from pid above (the
  -- runner's). It is never cleared once set, so it is stale on any row whose
  -- runner has already died.
  agent_pid     INTEGER NOT NULL DEFAULT 0,
  -- model, harness, and effort are the label overrides in effect for this
  -- dispatch. Empty means "no override", never "the empty model".
  model         TEXT NOT NULL DEFAULT '',
  harness       TEXT NOT NULL DEFAULT '',
  effort        TEXT NOT NULL DEFAULT '',
  -- provider is the pi provider serving this dispatch's model, resolved when
  -- the dispatch was decided. Unlike the three columns above it is the
  -- EFFECTIVE value rather than an override, because a provider has no label
  -- to override: it is derived from whichever model ends up in play. Empty
  -- means claude, or a resolution that failed.
  provider      TEXT NOT NULL DEFAULT '',
  -- review_pending carries engine.Decision.ReviewPending to the detached
  -- runner, which never sees the tick's Decision. It lives here, not on
  -- pr_links, because every PutPRLink call site runs before engine.Decide
  -- produces this value, and PutPRLink's upsert rewrites every column -- so a
  -- tend sweep, which deliberately never sets this, would overwrite a set
  -- flag with 0 before the runner read the row. See store.Dispatch.
  review_pending INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS pr_links (
  project_id TEXT NOT NULL DEFAULT '',
  loop       TEXT NOT NULL,
  repo       TEXT NOT NULL,
  number     INTEGER NOT NULL,
  pr_number  INTEGER NOT NULL,
  head_ref   TEXT NOT NULL,
  base_ref   TEXT NOT NULL,
  behind_by  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (project_id, loop, repo, number)
);

CREATE TABLE IF NOT EXISTS ticks (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id      TEXT NOT NULL DEFAULT '',
  loop            TEXT NOT NULL,
  started_at      TIMESTAMP NOT NULL,
  breaker_tripped INTEGER NOT NULL DEFAULT 0,
  summary_json    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS cooldowns (
  project_id TEXT NOT NULL DEFAULT '',
  loop       TEXT NOT NULL,
  until      TIMESTAMP NOT NULL,
  PRIMARY KEY (project_id, loop)
);

-- One row per repository this project has registered a webhook for.
--
-- The row exists because registration used to leave NOTHING on this machine.
-- register-webhook found an existing hook by matching Config.URL against
-- webhook.url, so changing webhook.url and re-running it created a SECOND hook
-- at GitHub while the first kept delivering to a dead endpoint -- orphaned,
-- invisible here, and removable only by hand in GitHub's UI. Recording what was
-- registered is what makes that recoverable.
CREATE TABLE IF NOT EXISTS webhooks (
  project_id    TEXT NOT NULL,
  repo          TEXT NOT NULL,
  -- hook_id is GitHub's identifier for the hook, and it is the column this
  -- table exists for. deregister-webhook deletes by it rather than by matching
  -- a URL, which is the only way to remove the hook a project actually
  -- registered AFTER webhook.url has been changed -- the exact case that
  -- otherwise leaves an orphaned hook delivering to a dead endpoint forever.
  hook_id       INTEGER NOT NULL,
  -- url is the delivery target the hook carried when it was recorded. It is
  -- kept for the operator, not for matching: after a webhook.url change it is
  -- the only local record of where the hook still points.
  url           TEXT NOT NULL,
  registered_at TIMESTAMP NOT NULL,
  PRIMARY KEY (project_id, repo)
);

-- One row per issue or pull request this machine knows to be CLOSED.
--
-- A row's PRESENCE is the fact; a reopen deletes it. There is no closed=0 row,
-- because "not closed" and "never heard of" must read the same way: the
-- sessions report hides a session only when a row says its issue is closed, so
-- an issue nothing has ever reported on stays visible rather than vanishing on
-- a default value.
--
-- The key omits the loop, unlike every other table here. Closure is a fact
-- about an issue in a REPOSITORY, and two loops watching one repo see the same
-- closure; keying it per loop would record the same fact twice and let the
-- copies disagree. It is also why this is not a column on issues: that table's
-- rows are per loop, and are absent entirely for issues whose loop was renamed
-- or whose state was cleaned up, which is exactly the old work the report most
-- wants to hide.
CREATE TABLE IF NOT EXISTS closures (
  project_id TEXT NOT NULL DEFAULT '',
  repo       TEXT NOT NULL,
  number     INTEGER NOT NULL,
  closed_at  TIMESTAMP NOT NULL,
  PRIMARY KEY (project_id, repo, number)
);

-- One row per pull request currently backed off from repeated rebase
-- conflicts.
--
-- The key is one row per PULL REQUEST, not one row per fingerprint. A new
-- fingerprint REPLACES the row rather than adding one: the table cannot grow
-- without a bound, and a changed conflict cannot inherit an old conflict's
-- backoff -- the count and the deadline both belong to the conflict a rebase
-- is meeting right now.
--
-- retry_after is Unix seconds with a literal DEFAULT 0 meaning "no deadline",
-- matching issues.retry_after: no literal TIMESTAMP default reads back as the
-- zero time. This is a NEW table, so it needs no addedColumns entry -- that
-- mechanism exists to add a column to a database that already has the table
-- without it, and no such database exists here.
CREATE TABLE IF NOT EXISTS tend_conflicts (
  project_id    TEXT NOT NULL DEFAULT '',
  loop          TEXT NOT NULL,
  repo          TEXT NOT NULL,
  pr_number     INTEGER NOT NULL,
  fingerprint   TEXT NOT NULL,
  seen_count    INTEGER NOT NULL DEFAULT 0,
  first_seen_at TIMESTAMP NOT NULL,
  last_seen_at  TIMESTAMP NOT NULL,
  retry_after   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (project_id, loop, repo, pr_number)
);

-- One row per legacy per-loop database this canonical file has imported.
--
-- The key is a triple, not a path. Two loops may share one state_dir, so one
-- file can hold two loops, and each loop is claimed by its own project.
CREATE TABLE IF NOT EXISTS legacy_sources (
  path              TEXT NOT NULL,
  project_id        TEXT NOT NULL,
  loop              TEXT NOT NULL,
  repo              TEXT NOT NULL DEFAULT '',
  state             TEXT NOT NULL,
  first_imported_at TIMESTAMP NOT NULL,
  last_imported_at  TIMESTAMP NOT NULL,
  PRIMARY KEY (path, project_id, loop)
);
`

// The indexes are separate from the table DDL because they name columns that an
// older database only gains during the upgrade. Creating them before that pass
// fails on "no such column".
const schemaIndexes = `
CREATE INDEX IF NOT EXISTS dispatches_running_project
  ON dispatches (project_id, loop, repo, status);

-- An imported row is identified by where it came from. The import is idempotent
-- because of this index: a second pass updates rather than duplicates.
CREATE UNIQUE INDEX IF NOT EXISTS dispatches_legacy
  ON dispatches (legacy_source, legacy_id, project_id, loop)
  WHERE legacy_source <> '';

CREATE INDEX IF NOT EXISTS ticks_loop ON ticks (project_id, loop);
`

// DB is the canonical state database. One file holds every project.
type DB struct {
	db *sql.DB
	// path is kept so the importer can recognise a legacy source that IS this
	// file. A loop whose state_dir is the home directory has exactly that.
	//
	// It is stored resolved, in the same spelling the importer resolves its
	// sources into. Comparing a raw path against a resolved one silently takes
	// the wrong branch on any machine whose home traverses a symlink.
	path string
}

// Store is a view of the database scoped to one project. Every read and every
// write it issues carries that project's identifier, so a scoped caller can
// neither see nor touch another project's rows.
type Store struct {
	db        *sql.DB
	projectID string
}

// Open opens the canonical database at path and brings its schema up to date.
func Open(path string) (*DB, error) {
	// Every connection must carry busy_timeout, because several processes write
	// this file. Passing the pragmas in the DSN is the only way to guarantee it.
	//
	// 30s, not 10s: this one file now takes the writes of every tick and every
	// detached runner on the machine. Almost every write is a single small
	// statement, and the transactions that are not (the schema pass, the legacy
	// import, MarkNeedsRetry) hold the lock only for the few statements inside
	// them, so a wait this long only ever covers a queue.
	//
	// _txlock=immediate takes the write lock when a transaction BEGINS.
	// MarkNeedsRetry reads retry_count and then writes it back, and a deferred
	// transaction would take a read snapshot first and try to upgrade at the
	// write. SQLite answers that upgrade with SQLITE_BUSY_SNAPSHOT and does NOT
	// invoke the busy handler, so busy_timeout would not cover it: the failure
	// flag would be lost and the issue stranded holding the in-flight label.
	dsn := "file:" + path +
		"?_pragma=busy_timeout(30000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_txlock=immediate"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := openSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	// The database holds session identifiers. Keep it private to this user.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Chmod(path+suffix, 0o600)
	}
	return &DB{db: db, path: home.Resolve(path)}, nil
}

// Project returns a view of the database scoped to one project.
func (d *DB) Project(projectID string) *Store {
	return &Store{db: d.db, projectID: projectID}
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// openSchema applies the schema, retrying while the database is busy.
//
// busy_timeout does not cover everything here. A connection applies
// "PRAGMA journal_mode=WAL" as it opens, and on a database another process is
// writing that pragma can return SQLITE_BUSY immediately rather than waiting out
// the busy handler. Before this change each loop had its own file and the window
// never opened; now every tick and every runner on the machine opens one file, so
// a cron minute that starts two loops together would fail one of them.
func openSchema(db *sql.DB) error {
	const attempts = 12
	var err error
	for i := 0; i < attempts; i++ {
		if err = applySchema(db); err == nil {
			return nil
		}
		if !isBusy(err) {
			return err
		}
		time.Sleep(time.Duration(i+1) * 50 * time.Millisecond)
	}
	return err
}

// isBusy reports whether the database refused because another connection held
// it. Nothing else is worth retrying.
func isBusy(err error) bool {
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return false
	}
	// The low byte is the primary result code. SQLITE_BUSY is 5 and
	// SQLITE_LOCKED is 6; both mean "someone else has it, try again".
	switch e.Code() & 0xff {
	case 5, 6:
		return true
	}
	return false
}

// applySchema creates, upgrades and indexes the tables, in ONE transaction
// whose first statement writes.
//
// The single transaction is what makes this safe between processes. Every tick
// and every runner on the machine opens this file, so two of them can enter here
// at the same time. A write transaction takes the write lock on its first
// statement, so the second process waits out busy_timeout and then finds the
// work already done. Splitting the check from the rebuild would let the second
// process drop the table the first one just filled.
func applySchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin schema transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// First statement, and a write: this takes the lock for the whole function.
	if _, err := tx.Exec(schemaTables); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	// Columns before keys. The rebuild below selects columns that a database
	// from an early release only has after this pass.
	if err := addColumns(tx); err != nil {
		return err
	}
	if err := upgradeKeys(tx); err != nil {
		return err
	}
	if err := backfillSessionHarness(tx); err != nil {
		return err
	}
	// The indexes come last: every column they name exists by now, and the key
	// upgrade above drops tables, which takes their indexes with them.
	if _, err := tx.Exec(schemaIndexes); err != nil {
		return fmt.Errorf("create indexes: %w", err)
	}
	// The pre-project index is a prefix of the new one and only costs writes.
	if _, err := tx.Exec(`DROP INDEX IF EXISTS dispatches_running`); err != nil {
		return fmt.Errorf("drop the superseded dispatch index: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit schema transaction: %w", err)
	}
	return nil
}

// addedColumns lists every column added after the first release. CREATE TABLE
// IF NOT EXISTS does nothing to a database that already exists, so a new column
// must be added explicitly or every query naming it fails on an older file.
var addedColumns = []struct{ table, column, def string }{
	{"issues", "needs_retry", "INTEGER NOT NULL DEFAULT 0"},
	{"issues", "session_started", "INTEGER NOT NULL DEFAULT 0"},
	{"issues", "session_harness", "TEXT NOT NULL DEFAULT ''"},
	{"issues", "parked", "INTEGER NOT NULL DEFAULT 0"},
	{"dispatches", "pr_number", "INTEGER NOT NULL DEFAULT 0"},
	{"dispatches", "title", "TEXT NOT NULL DEFAULT ''"},
	{"pr_links", "behind_by", "INTEGER NOT NULL DEFAULT 0"},
	{"dispatches", "project_id", "TEXT NOT NULL DEFAULT ''"},
	{"dispatches", "legacy_source", "TEXT NOT NULL DEFAULT ''"},
	{"dispatches", "legacy_id", "INTEGER NOT NULL DEFAULT 0"},
	{"ticks", "project_id", "TEXT NOT NULL DEFAULT ''"},
	{"issues", "retry_after", "INTEGER NOT NULL DEFAULT 0"},
	{"issues", "stopped", "INTEGER NOT NULL DEFAULT 0"},
	{"issues", "stopped_reason", "TEXT NOT NULL DEFAULT ''"},
	{"dispatches", "agent_pid", "INTEGER NOT NULL DEFAULT 0"},
	{"dispatches", "model", "TEXT NOT NULL DEFAULT ''"},
	{"dispatches", "harness", "TEXT NOT NULL DEFAULT ''"},
	{"dispatches", "effort", "TEXT NOT NULL DEFAULT ''"},
	{"issues", "dispatch_harness", "TEXT NOT NULL DEFAULT ''"},
	{"issues", "dispatch_provider", "TEXT NOT NULL DEFAULT ''"},
	{"dispatches", "provider", "TEXT NOT NULL DEFAULT ''"},
	{"dispatches", "review_pending", "INTEGER NOT NULL DEFAULT 0"},
}

// backfillSessionHarness fills issues.session_harness for rows whose session was
// created before the column existed, reading the harness from the dispatch that
// created it.
//
// Only an EXPLICIT harness override is recovered: dispatches.harness records the
// issue's harness: label, and is empty whenever the loop's configured harness
// was used. An empty value therefore stays empty, which is what the engine reads
// as "unknown" and treats as resumable. That is the safe direction -- it can
// miss a mismatch, but it can never invent one and restart a healthy session.
//
// It runs once in effect: after this pass the rows it can fill are filled, and
// MarkSessionStarted records the resolved harness on every session from here on.
func backfillSessionHarness(tx *sql.Tx) error {
	_, err := tx.Exec(`
		UPDATE issues SET session_harness = (
		  SELECT d.harness FROM dispatches d
		  WHERE d.project_id = issues.project_id AND d.loop = issues.loop
		    AND d.repo = issues.repo AND d.number = issues.number
		    AND d.harness <> '' AND d.kind <> 'tend'
		  ORDER BY d.id DESC LIMIT 1
		)
		WHERE session_harness = '' AND session_started = 1
		  AND EXISTS (
		    SELECT 1 FROM dispatches d
		    WHERE d.project_id = issues.project_id AND d.loop = issues.loop
		      AND d.repo = issues.repo AND d.number = issues.number
		      AND d.harness <> '' AND d.kind <> 'tend'
		  )`)
	if err != nil {
		return fmt.Errorf("backfill session harness: %w", err)
	}
	return nil
}

// addColumns adds any column missing from an existing database. Each column has
// a default, so an added column needs no backfill.
func addColumns(tx *sql.Tx) error {
	for _, c := range addedColumns {
		has, err := hasColumn(tx, c.table, c.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.def)
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("add column %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

// rebuilt names the three tables whose PRIMARY KEY gained project_id, with the
// columns to carry over. SQLite cannot ALTER a key, so each is rebuilt.
var rebuilt = []struct{ table, columns string }{
	{"issues", `loop, repo, number, session_id, worktree_path, retry_count,
		last_retry_tick, needs_retry, session_started, session_harness, parked,
		retry_after, stopped, stopped_reason, updated_at`},
	{"pr_links", `loop, repo, number, pr_number, head_ref, base_ref, behind_by`},
	{"cooldowns", `loop, until`},
}

// upgradeKeys rebuilds the tables whose primary key gained project_id.
//
// Rows from before this change belong to no project yet, so they are copied with
// an empty project_id. That makes them invisible to every scoped query, because
// a project identifier is a UUID and is never empty. The importer stamps them
// with a real identifier when the file it is importing IS this file.
//
// KNOWN LIMITATION: when a loop's state_dir is the home directory, this file is
// also the state file of any runner still alive from the old binary. After the
// rebuild that runner's issue write, which names ON CONFLICT(loop, repo, number),
// has no matching unique index and fails; its dispatch write still succeeds. The
// tick's reaper retires the row on the next pass, so the loop recovers. Keeping a
// second unique index on (loop, repo, number) would fix that write and defeat
// project keying, which is the whole point of this schema.
func upgradeKeys(tx *sql.Tx) error {
	has, err := hasColumn(tx, "issues", "project_id")
	if err != nil {
		return err
	}
	if has {
		return nil // already the new shape
	}

	for _, t := range rebuilt {
		stmt := fmt.Sprintf(`ALTER TABLE %s RENAME TO %s_old`, t.table, t.table)
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("rename %s: %w", t.table, err)
		}
	}
	// Recreate the three tables in their new shape, then copy and drop.
	if _, err := tx.Exec(schemaTables); err != nil {
		return fmt.Errorf("create the project-keyed tables: %w", err)
	}
	for _, t := range rebuilt {
		copyStmt := fmt.Sprintf(
			`INSERT INTO %s (project_id, %s) SELECT '', %s FROM %s_old`,
			t.table, t.columns, t.columns, t.table)
		if _, err := tx.Exec(copyStmt); err != nil {
			return fmt.Errorf("copy %s into its project-keyed table: %w", t.table, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`DROP TABLE %s_old`, t.table)); err != nil {
			return fmt.Errorf("drop the superseded %s table: %w", t.table, err)
		}
	}
	return nil
}

// querier is what hasColumn needs: a transaction here, and nothing else.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func hasColumn(q querier, table, column string) (bool, error) {
	rows, err := q.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("scan %s columns: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// IssueStates returns every issue record for one loop and repository.
func (s *Store) IssueStates(loop, repo string) (map[int]IssueState, error) {
	rows, err := s.db.Query(`
		SELECT number, session_id, worktree_path, retry_count, last_retry_tick,
		       needs_retry, session_started, session_harness,
		       dispatch_harness, dispatch_provider, parked, retry_after,
		       stopped, stopped_reason, updated_at
		FROM issues WHERE project_id = ? AND loop = ? AND repo = ?`,
		s.projectID, loop, repo)
	if err != nil {
		return nil, fmt.Errorf("query issues: %w", err)
	}
	defer rows.Close()

	out := make(map[int]IssueState)
	for rows.Next() {
		st := IssueState{ProjectID: s.projectID, Loop: loop, Repo: repo}
		var retryAfter int64
		if err := rows.Scan(&st.Number, &st.SessionID, &st.WorktreePath,
			&st.RetryCount, &st.LastRetryTick, &st.NeedsRetry, &st.SessionStarted,
			&st.SessionHarness, &st.DispatchHarness, &st.DispatchProvider,
			&st.Parked, &retryAfter, &st.Stopped, &st.StoppedReason,
			&st.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan issue: %w", err)
		}
		st.RetryAfter = retryAfterTime(retryAfter)
		out[st.Number] = st
	}
	return out, rows.Err()
}

// retryAfterSeconds encodes a deadline for the retry_after column. The zero
// time is stored as 0, which is also the column's default, so a row that never
// carried a deadline and a row whose deadline was cleared read back the same.
func retryAfterSeconds(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// retryAfterTime decodes the retry_after column back into a deadline.
func retryAfterTime(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}

// PutIssueState inserts or replaces one issue record.
//
// It reads dispatch_harness and dispatch_provider back but never writes them,
// for the reason it never writes stopped: its one remaining caller is the park
// path, a read-modify-write over state read before the dispatch that stamped
// those columns. BeginDispatch is their only writer, and a stale value written
// back here would report a configuration change that had already been acted
// on -- which is a retirement the engine would grant again on the next tick.
func (s *Store) PutIssueState(st IssueState) error {
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(`
		INSERT INTO issues (project_id, loop, repo, number, session_id, worktree_path,
		                    retry_count, last_retry_tick, needs_retry,
		                    session_started, session_harness, parked, retry_after,
		                    updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, loop, repo, number) DO UPDATE SET
		  session_id      = excluded.session_id,
		  worktree_path   = excluded.worktree_path,
		  retry_count     = excluded.retry_count,
		  last_retry_tick = excluded.last_retry_tick,
		  needs_retry     = excluded.needs_retry,
		  session_started = excluded.session_started,
		  session_harness = excluded.session_harness,
		  parked          = excluded.parked,
		  retry_after     = excluded.retry_after,
		  updated_at      = excluded.updated_at`,
		s.projectID, st.Loop, st.Repo, st.Number, st.SessionID, st.WorktreePath,
		st.RetryCount, st.LastRetryTick, st.NeedsRetry, st.SessionStarted,
		st.SessionHarness, st.Parked, retryAfterSeconds(st.RetryAfter),
		st.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("put issue state: %w", err)
	}
	return nil
}

// MarkNeedsRetry records that a dispatch for this issue failed, and stamps the
// earliest time a retry may run. It is durable, so a tick that declines to act
// on the failure (backoff or circuit breaker) does not lose it.
//
// It is the only writer of a NON-ZERO retry_after. Four other statements write
// that column, and every one of them only ever clears it: ClearNeedsRetry,
// ClearRetryAfter, BeginDispatch on a human trigger, and PutIssueState, whose
// one remaining caller is the park path in internal/loopcmd, which zeroes the
// deadline with the flag it is retiring. Every needs-retry
// transition runs through here, so a second writer of a real deadline -- one
// stamped by the dispatch, say -- would be overwritten by the very next
// failure, and the escalating list would collapse to its first entry forever.
//
// It reads retry_count inside the same transaction and indexes backoff with it,
// clamped to the last entry. An empty list means no deadline: retry.max may be
// 0, in which case retry.backoff is absent and no retry will ever be decided.
func (s *Store) MarkNeedsRetry(loop, repo string, number int, now time.Time, backoff []time.Duration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("mark needs retry: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The count is read here rather than taken from the caller because it is
	// what the index must agree with: the row on disk is the only thing every
	// failure path shares.
	var retryCount int
	err = tx.QueryRow(
		`SELECT retry_count FROM issues
		 WHERE project_id = ? AND loop = ? AND repo = ? AND number = ?`,
		s.projectID, loop, repo, number).Scan(&retryCount)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read retry count: %w", err)
	}

	var deadline int64
	if len(backoff) > 0 {
		i := retryCount
		if i >= len(backoff) {
			i = len(backoff) - 1
		}
		if i < 0 {
			i = 0
		}
		deadline = retryAfterSeconds(now.Add(backoff[i]))
	}

	if _, err := tx.Exec(`
		INSERT INTO issues (project_id, loop, repo, number, needs_retry, retry_after, updated_at)
		VALUES (?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(project_id, loop, repo, number) DO UPDATE SET
		  needs_retry = 1,
		  retry_after = excluded.retry_after,
		  updated_at  = excluded.updated_at`,
		s.projectID, loop, repo, number, deadline, time.Now().UTC()); err != nil {
		return fmt.Errorf("mark needs retry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark needs retry: %w", err)
	}
	return nil
}

// ClearNeedsRetry clears a failure flag that no retry can act on. Without it an
// issue whose failure was recorded while it was not in flight is stranded
// permanently.
//
// The deadline goes with the flag. A deadline that is never cleared is a
// permanent past deadline, and the daemon's wake query would then re-tick this
// loop forever, each pass reading the GitHub API with a repository-write token.
func (s *Store) ClearNeedsRetry(loop, repo string, number int) error {
	_, err := s.db.Exec(`
		UPDATE issues SET needs_retry = 0, retry_after = 0, updated_at = ?
		WHERE project_id = ? AND loop = ? AND repo = ? AND number = ?`,
		time.Now().UTC(), s.projectID, loop, repo, number)
	if err != nil {
		return fmt.Errorf("clear needs retry: %w", err)
	}
	return nil
}

// ClearRetryAfter drops a retry DEADLINE while leaving the failure flag alone.
//
// It exists for an issue the loop can no longer see: closed, transferred, or
// carrying a veto label. engine.Decide iterates the open, non-vetoed issues
// only, so such a row can never reach KindClearRetry, while the daemon's wake
// query (EarliestRetryAfterAt) selects on retry_after alone and would hand the
// same permanently-past deadline back every MinWakeInterval forever -- a full
// tick each time, GitHub reads included, with a repository-write token.
//
// Only the deadline goes. needs_retry stays, so the failure is not destroyed:
// reopening the issue, or removing the veto label, puts it back in front of
// engine.Decide with a zero deadline, which retryDecision treats as due now.
// Clearing the flag as well would be irreversible -- nothing re-derives it --
// and would strand the issue holding an in-flight label with no agent.
func (s *Store) ClearRetryAfter(loop, repo string, number int) error {
	_, err := s.db.Exec(`
		UPDATE issues SET retry_after = 0, updated_at = ?
		WHERE project_id = ? AND loop = ? AND repo = ? AND number = ?`,
		time.Now().UTC(), s.projectID, loop, repo, number)
	if err != nil {
		return fmt.Errorf("clear retry after: %w", err)
	}
	return nil
}

// MarkStopped sets the stopped flag and its reason for one issue.
//
// It is an UPSERT, ON CONFLICT(project_id, loop, repo, number), rather than an
// UPDATE, because an invalid label can be the very FIRST thing Decide ever
// sees for an issue -- no row exists yet, and an UPDATE that matched nothing
// would silently fail to record the stop. It is a targeted write, not a
// read-modify-write, for the reason BeginDispatch gives above: a killed
// dispatch's own MarkNeedsRetry can land between a read and this write, and a
// stale write-back would lose that failure.
func (s *Store) MarkStopped(loop, repo string, number int, reason string, now time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO issues (project_id, loop, repo, number, stopped, stopped_reason, updated_at)
		VALUES (?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(project_id, loop, repo, number) DO UPDATE SET
		  stopped        = 1,
		  stopped_reason = excluded.stopped_reason,
		  updated_at     = excluded.updated_at`,
		s.projectID, loop, repo, number, reason, now.UTC())
	if err != nil {
		return fmt.Errorf("mark stopped: %w", err)
	}
	return nil
}

// ClearStopped resumes a stopped issue. It reports whether the issue was
// actually stopped: a target `--issue`/`--session` resolves from config or
// the registry, not from the stopped table, so a resume can legitimately
// name an issue that merely sits in retry backoff. Without the `stopped = 1`
// predicate this would clear needs_retry/retry_after on such an issue
// unconditionally, discarding the escalating backoff MarkNeedsRetry exists
// to enforce; the caller uses the return value to report that case as a
// no-op instead of a silent mutation.
//
// It also clears needs_retry and retry_after, but leaves parked alone. A
// killed runner's dispatch is recorded FAILED, and the runner's own finish
// call marks the issue for retry (internal/runner/runner.go:320) -- that
// happens whether or not an operator meant to resume it, so a resumed issue
// must not carry a failure it did not earn. parked is a fact about the
// retry budget, unrelated to why the issue was stopped, so it is untouched.
func (s *Store) ClearStopped(loop, repo string, number int, now time.Time) (bool, error) {
	res, err := s.db.Exec(`
		UPDATE issues
		SET stopped = 0, stopped_reason = '', needs_retry = 0, retry_after = 0,
		    updated_at = ?
		WHERE project_id = ? AND loop = ? AND repo = ? AND number = ? AND stopped = 1`,
		now.UTC(), s.projectID, loop, repo, number)
	if err != nil {
		return false, fmt.Errorf("clear stopped: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("clear stopped: %w", err)
	}
	return n > 0, nil
}

// BeginDispatch records the issue state a dispatch owns, just before the agent
// is spawned: the session it will run under, a cleared park and failure flag,
// and the retry budget this attempt spends.
//
// It writes named columns rather than a whole IssueState read a moment earlier
// (PutIssueState) because the tick is NOT the only writer of this row. A
// detached runner process finishing a failed dispatch calls MarkNeedsRetry from
// outside the loop flock the tick holds, so a read-modify-write spanning the
// spawn can land on top of a failure recorded in between: the flag, the
// deadline and the retry budget would all be lost, which is exactly the
// uncapped redispatch needs_retry exists to prevent. The window is reachable --
// runner.finish writes FinishDispatch and MarkNeedsRetry as two statements, and
// a webhook tick between them sees the issue as neither live nor failed.
//
// retry is what the failure path costs: on a retry the budget is spent in SQL
// (retry_count + 1) rather than incremented from a value read before the gap,
// for the same reason. A human trigger begins a new episode, so it resets the
// budget and drops any deadline left over from the previous one.
// The harness and provider arguments are the configuration this dispatch is
// about to run, stamped BEFORE the agent starts. engine.configRetired compares
// them against what the NEXT dispatch would use to decide whether the issue's
// accumulated failures still describe the configuration in play. Stamping them
// here, rather than when a run reports its session, is what makes a retirement
// terminate: a dispatch that dies before the harness says anything still
// records what it tried.
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

// SetWorktreePath records where a dispatch's agent is working.
//
// Separate from BeginDispatch, and a targeted UPDATE, for the reason given
// there: the worktree is created between the two writes, and re-persisting a
// whole IssueState read before that could clobber a failure another process
// recorded while git was working.
func (s *Store) SetWorktreePath(loop, repo string, number int, path string, now time.Time) error {
	_, err := s.db.Exec(`
		UPDATE issues SET worktree_path = ?, updated_at = ?
		WHERE project_id = ? AND loop = ? AND repo = ? AND number = ?`,
		path, now.UTC(), s.projectID, loop, repo, number)
	if err != nil {
		return fmt.Errorf("set worktree path: %w", err)
	}
	return nil
}

// MarkSessionStarted records that claude created the session for this issue.
//
// It is written on ANY dispatch whose stream reported a session identifier,
// success or failure alike. If it were written only on success, a run that
// created a session and then crashed would leave the flag false, and every
// retry would start a NEW run against the already-used identifier, which claude
// refuses outright.
func (s *Store) MarkSessionStarted(loop, repo string, number int, harness string) error {
	_, err := s.db.Exec(`
		UPDATE issues SET session_started = 1, session_harness = ?, updated_at = ?
		WHERE project_id = ? AND loop = ? AND repo = ? AND number = ?`,
		harness, time.Now().UTC(), s.projectID, loop, repo, number)
	if err != nil {
		return fmt.Errorf("mark session started: %w", err)
	}
	return nil
}

// MarkSucceeded clears the failure state after a clean dispatch. It resets the
// retry budget, so an issue that fails three times over its lifetime with
// successful runs in between is not parked on its next single failure.
func (s *Store) MarkSucceeded(loop, repo string, number int) error {
	_, err := s.db.Exec(`
		UPDATE issues
		SET needs_retry = 0, parked = 0, retry_count = 0, session_started = 1,
		    retry_after = 0, updated_at = ?
		WHERE project_id = ? AND loop = ? AND repo = ? AND number = ?`,
		time.Now().UTC(), s.projectID, loop, repo, number)
	if err != nil {
		return fmt.Errorf("mark succeeded: %w", err)
	}
	return nil
}

// IssueState returns the stored state for one issue. When no row exists it
// returns a zero value with the keys filled in.
//
// It reports a read error rather than hiding one. A caller that persisted a
// zero value returned from a failed read would wipe the session identifier and
// the retry counter of a live issue.
func (s *Store) IssueState(loop, repo string, number int) (IssueState, error) {
	states, err := s.IssueStates(loop, repo)
	if err != nil {
		return IssueState{}, err
	}
	if st, ok := states[number]; ok {
		return st, nil
	}
	return IssueState{ProjectID: s.projectID, Loop: loop, Repo: repo, Number: number}, nil
}

// LastTick returns the time of the most recent recorded tick.
func (s *Store) LastTick(loop string) (time.Time, error) {
	var t time.Time
	err := s.db.QueryRow(
		`SELECT started_at FROM ticks WHERE project_id = ? AND loop = ?
		 ORDER BY id DESC LIMIT 1`, s.projectID, loop).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("last tick: %w", err)
	}
	return t.UTC(), nil
}

// CostByIssue returns the total recorded cost for each issue.
func (s *Store) CostByIssue(loop, repo string) (map[int]float64, error) {
	rows, err := s.db.Query(
		`SELECT number, SUM(cost_usd) FROM dispatches
		 WHERE project_id = ? AND loop = ? AND repo = ? GROUP BY number`,
		s.projectID, loop, repo)
	if err != nil {
		return nil, fmt.Errorf("cost by issue: %w", err)
	}
	defer rows.Close()

	out := make(map[int]float64)
	for rows.Next() {
		var number int
		var cost float64
		if err := rows.Scan(&number, &cost); err != nil {
			return nil, fmt.Errorf("scan cost: %w", err)
		}
		out[number] = cost
	}
	return out, rows.Err()
}

// DeleteIssueState removes one issue record.
func (s *Store) DeleteIssueState(loop, repo string, number int) error {
	_, err := s.db.Exec(
		`DELETE FROM issues WHERE project_id = ? AND loop = ? AND repo = ? AND number = ?`,
		s.projectID, loop, repo, number)
	if err != nil {
		return fmt.Errorf("delete issue state: %w", err)
	}
	return nil
}

// CreateDispatch inserts a running dispatch row and returns its identifier.
func (s *Store) CreateDispatch(d Dispatch) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO dispatches (project_id, loop, repo, number, kind, session_id,
		                        status, started_at, log_path, pr_number, title,
		                        model, harness, effort, provider, review_pending)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.projectID, d.Loop, d.Repo, d.Number, d.Kind, d.SessionID,
		StatusRunning, time.Now().UTC(), d.LogPath, d.PRNumber, d.Title,
		d.Model, d.Harness, d.Effort, d.Provider, d.ReviewPending)
	if err != nil {
		return 0, fmt.Errorf("create dispatch: %w", err)
	}
	return res.LastInsertId()
}

// RecordFinishedDispatch inserts one dispatch row that is already complete.
//
// Every other dispatch row is born running and finished later, because a
// process backs it. This one has none: git did the work synchronously, in this
// process, and it is over before the row exists. Two statements would leave a
// window -- and a PERMANENT stuck row if the second failed -- in which the row
// reads as a live agent to engine.Decide, to reapDead, and to tendDispatch's
// reap partition, none of which can reap a kind they do not know about. A
// single already-finished INSERT makes that state unreachable, which is worth
// more than reusing CreateDispatch.
//
// Only the columns that carry meaning are named. The rest -- pid, exit code,
// cost, duration, log path -- default to their zero value in the schema, which
// is the truth for a row no process ever ran. The session identifier is left
// empty on purpose; see KindRebase.
func (s *Store) RecordFinishedDispatch(d Dispatch) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`
		INSERT INTO dispatches (project_id, loop, repo, number, kind,
		                        status, started_at, finished_at, pr_number, title)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.projectID, d.Loop, d.Repo, d.Number, d.Kind,
		StatusSucceeded, now, now, d.PRNumber, d.Title)
	if err != nil {
		return fmt.Errorf("record finished dispatch: %w", err)
	}
	return nil
}

// SetDispatchProcess records the operating system process for a dispatch.
func (s *Store) SetDispatchProcess(id int64, pid int, startedAt time.Time) error {
	_, err := s.db.Exec(
		`UPDATE dispatches SET pid = ?, pid_start_at = ?
		 WHERE id = ? AND project_id = ?`,
		pid, startedAt.UTC(), id, s.projectID)
	if err != nil {
		return fmt.Errorf("set dispatch process: %w", err)
	}
	return nil
}

// SetDispatchAgentPID records the agent CHILD's own process identifier, once
// Supervise has started it. It is separate from SetDispatchProcess (the
// runner's own pid) because the runner is spawned Setsid and the agent child
// Setpgid into its own group -- a signal to one does not reach the other, and
// killing the agent needs this pid, verified against the runner independently
// by internal/proc.
func (s *Store) SetDispatchAgentPID(id int64, pid int) error {
	_, err := s.db.Exec(
		`UPDATE dispatches SET agent_pid = ? WHERE id = ? AND project_id = ?`,
		pid, id, s.projectID)
	if err != nil {
		return fmt.Errorf("set dispatch agent pid: %w", err)
	}
	return nil
}

// FinishDispatch records the outcome of a dispatch.
func (s *Store) FinishDispatch(id int64, r DispatchResult) error {
	// Only a row that is still running may be finished. If a tick already reaped
	// this row as dead and dispatched a replacement, the original runner must not
	// come back and overwrite the replacement's state.
	res, err := s.db.Exec(`
		UPDATE dispatches
		SET status = ?, exit_code = ?, cost_usd = ?, duration_ms = ?,
		    api_error = ?, finished_at = ?
		WHERE id = ? AND project_id = ? AND status = ?`,
		r.Status, r.ExitCode, r.CostUSD, r.DurationMS, r.APIError, time.Now().UTC(),
		id, s.projectID, StatusRunning)
	if err != nil {
		return fmt.Errorf("finish dispatch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish dispatch rows: %w", err)
	}
	if n == 0 {
		return ErrDispatchNotRunning
	}
	return nil
}

// ErrDispatchNotRunning reports that a dispatch was already finished, so this
// caller lost the race and must not write any further state for it.
var ErrDispatchNotRunning = errors.New("dispatch is no longer running")

const dispatchColumns = `id, project_id, loop, repo, number, kind, session_id, pid,
	pid_start_at, status, started_at, finished_at, exit_code, cost_usd, duration_ms,
	api_error, log_path, pr_number, title, legacy_source, legacy_id,
	agent_pid, model, harness, effort, provider, review_pending`

func scanDispatch(sc interface{ Scan(...any) error }) (Dispatch, error) {
	var d Dispatch
	var pidStart, finished sql.NullTime
	err := sc.Scan(&d.ID, &d.ProjectID, &d.Loop, &d.Repo, &d.Number, &d.Kind,
		&d.SessionID, &d.PID, &pidStart, &d.Status, &d.StartedAt, &finished,
		&d.ExitCode, &d.CostUSD, &d.DurationMS, &d.APIError, &d.LogPath,
		&d.PRNumber, &d.Title, &d.LegacySource, &d.LegacyID,
		&d.AgentPID, &d.Model, &d.Harness, &d.Effort, &d.Provider, &d.ReviewPending)
	if err != nil {
		return Dispatch{}, err
	}
	if pidStart.Valid {
		d.PIDStartAt = pidStart.Time
	}
	if finished.Valid {
		d.FinishedAt = finished.Time
	}
	return d, nil
}

func scanDispatches(rows *sql.Rows) ([]Dispatch, error) {
	defer rows.Close()
	var out []Dispatch
	for rows.Next() {
		d, err := scanDispatch(rows)
		if err != nil {
			return nil, fmt.Errorf("scan dispatch: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RunningDispatches returns every dispatch still marked running.
func (s *Store) RunningDispatches(loop, repo string) ([]Dispatch, error) {
	rows, err := s.db.Query(
		`SELECT `+dispatchColumns+` FROM dispatches
		 WHERE project_id = ? AND loop = ? AND repo = ? AND status = ?`,
		s.projectID, loop, repo, StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("query running dispatches: %w", err)
	}
	return scanDispatches(rows)
}

// RunningDispatchesForRepo returns every running dispatch this project holds
// against one repository, across ALL of its loops.
//
// It is the project-wide twin of RunningDispatches, and it exists for exactly
// one caller: the tend dispatcher. Every other reader is a loop deciding its
// own issues, and a loop must not see its neighbours' rows. Tending is the one
// pass whose safety question spans loops -- "is any agent, anywhere in this
// project, already working this issue's branch?" -- and it cannot be answered
// from the reserved name's own rows, which hold only the project's tends.
//
// It is a new QUERY, not a new column: dispatches is already keyed
// (project_id, loop, repo, status), so dropping `loop` from the WHERE clause is
// the whole of the change. The index leads with project_id, so this is the same
// lookup with one fewer equality on the end of the prefix.
func (s *Store) RunningDispatchesForRepo(repo string) ([]Dispatch, error) {
	rows, err := s.db.Query(
		`SELECT `+dispatchColumns+` FROM dispatches
		 WHERE project_id = ? AND repo = ? AND status = ?`,
		s.projectID, repo, StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("query running dispatches for repo: %w", err)
	}
	return scanDispatches(rows)
}

// StoppedIssuesForRepo returns every issue an operator has stopped in this
// project's repository, in ANY loop, mapped to the reason.
//
// The tend dispatcher's counterpart to the per-loop Stopped flag Decide reads.
// `sessions kill` stops an issue in the loop that was working it, and the
// operator meant "run no more agents at this issue" -- a tend is one of that
// issue's agents, and it would otherwise force-push the branch of the session
// they just killed, because the tend dispatcher keeps no issue state of its own
// and its own scope is always clean.
//
// An issue stopped in more than one loop yields whichever reason SQLite returns
// last; the reasons are advisory text in a skip line, and reporting one of two
// true reasons is not a failure worth a second query to order.
func (s *Store) StoppedIssuesForRepo(repo string) (map[int]string, error) {
	rows, err := s.db.Query(
		`SELECT number, stopped_reason FROM issues
		 WHERE project_id = ? AND repo = ? AND stopped = 1`,
		s.projectID, repo)
	if err != nil {
		return nil, fmt.Errorf("query stopped issues for repo: %w", err)
	}
	defer rows.Close()

	out := map[int]string{}
	for rows.Next() {
		var (
			number int
			reason sql.NullString
		)
		if err := rows.Scan(&number, &reason); err != nil {
			return nil, fmt.Errorf("scan stopped issue: %w", err)
		}
		out[number] = reason.String
	}
	return out, rows.Err()
}

// LastTendAt returns the FINISH time of the most recent finished tend dispatch
// for one pull request, and the zero time when it has never been tended.
//
// The finish time, not the start time, and this is the whole reason the
// review-activity trigger cannot become a dispatch loop. The tend prompt tells
// the agent to reply on the review threads it answers, so the agent's own
// comment is created DURING its own dispatch. Compared against the start time
// that comment is newer, so the next pass reads it as unanswered feedback and
// dispatches again -- forever, at roughly $0.75 a turn. Compared against the
// finish time it is older, and the loop cannot start.
//
// ghub.LatestReviewActivity also filters out activity written by the loop's own
// token identity, and that filter is NOT what makes this safe. The agent runs
// with GITHUB_TOKEN stripped from its environment (runner.agentEnv), so its gh
// calls authenticate as whatever ~/.config/gh holds -- on the ordinary
// deployment, a human's login rather than the daemon's bot. The identity filter
// is defence in depth for the case where the two DO match; this comparison is
// what holds when they do not.
//
// The cost is that a review comment written while a tend is running is not seen
// as pending afterwards. That is the conservative direction: the next comment
// re-arms the trigger, and a bounded loss of one round beats an unbounded spend.
//
// Three further choices are deliberate:
//
//   - kind = 'tend' only. A kind = 'rebase' row records a rebase git performed
//     with no conversation, so it read no review and answered no comment.
//     Counting it would suppress the first tend after every automatic rebase,
//     which is exactly the feedback the review-activity trigger exists to
//     answer.
//   - finished_at IS NOT NULL. A running tend has no finish time to compare
//     against, and engine.Decide's liveTendPRs already suppresses a second pass
//     while one runs, so counting a running row here would be a second, weaker
//     copy of that guard.
//   - A FAILED tend still counts. The alternative -- counting only a succeeded
//     tend, so a crashed agent gets another turn at the same feedback -- was
//     rejected: runner.finish deliberately writes no retry state for a tend, so
//     nothing would bound how many times a persistently failing tend is
//     redispatched, and unbounded unattended spend is the failure this whole
//     change exists to remove. The cost is that feedback which met a crashed
//     agent waits for the next review comment; the dispatch row records the
//     failure, and `project logs --list` shows it.
//
// A plain SELECT of finished_at, not a SELECT of MAX(finished_at). Both
// LastTendAt and LastTendByPR use a MAX only inside a WHERE clause, as a text
// comparison against the stored ISO-8601 value, and select the finished_at
// COLUMN itself as the output. modernc.org/sqlite carries a column's declared type
// (TIMESTAMP) to the scan converter only for a bare column reference; an
// aggregate expression's result column carries none, and Scan then either
// fails outright (into time.Time or sql.NullTime) or -- worse -- silently
// hands back the driver's own %v-formatted string ("2026-09-01 14:58:35 +0000
// UTC") rather than the stored ISO-8601 text, which no further parsing here
// recovers. Comparing MAX(finished_at) against finished_at in a WHERE clause
// sidesteps the conversion entirely: SQLite compares the two as text, which
// sorts and compares correctly because ISO-8601 timestamps do.
//
// A row that matches is returned like LastTick's ORDER BY ... LIMIT 1: no row
// means the zero time via sql.ErrNoRows, compared with errors.Is.
//
// dispatches is indexed only on (project_id, loop, repo, status) (see
// schemaIndexes), and pr_number is not part of that index, so this scans.
// That is accepted at current volumes; add an index when a loop's dispatch
// history makes it matter.
func (s *Store) LastTendAt(loop, repo string, prNumber int) (time.Time, error) {
	var t time.Time
	err := s.db.QueryRow(
		`SELECT finished_at FROM dispatches d
		 WHERE project_id = ? AND loop = ? AND repo = ? AND pr_number = ?
		   AND kind = ? AND finished_at IS NOT NULL
		   AND finished_at = (
		     SELECT MAX(finished_at) FROM dispatches
		     WHERE project_id = d.project_id AND loop = d.loop AND repo = d.repo
		       AND pr_number = d.pr_number AND kind = d.kind AND finished_at IS NOT NULL
		   )`,
		s.projectID, loop, repo, prNumber, KindTend).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("last tend at: %w", err)
	}
	return t.UTC(), nil
}

// LastTendByPR returns the finish time of the most recently finished tend
// dispatch for every pull request this loop has tended, keyed by pull request
// number.
//
// It exists so a pass deciding many issues issues one query instead of one
// per pull request -- the same reasoning CostByIssue groups by number for. The
// reasoning documented on LastTendAt applies here identically. The
// self-join replaces GROUP BY for the reason given above LastTendAt: a
// GROUP BY pr_number, MAX(started_at) would select the aggregate as an output
// column, and lose the declared type the same way.
func (s *Store) LastTendByPR(loop, repo string) (map[int]time.Time, error) {
	rows, err := s.db.Query(
		`SELECT d.pr_number, d.finished_at FROM dispatches d
		 WHERE project_id = ? AND loop = ? AND repo = ?
		   AND kind = ? AND finished_at IS NOT NULL
		   AND finished_at = (
		     SELECT MAX(finished_at) FROM dispatches
		     WHERE project_id = d.project_id AND loop = d.loop AND repo = d.repo
		       AND pr_number = d.pr_number AND kind = d.kind AND finished_at IS NOT NULL
		   )`,
		s.projectID, loop, repo, KindTend)
	if err != nil {
		return nil, fmt.Errorf("query last tend by pr: %w", err)
	}
	defer rows.Close()

	out := make(map[int]time.Time)
	for rows.Next() {
		var prNumber int
		var t time.Time
		if err := rows.Scan(&prNumber, &t); err != nil {
			return nil, fmt.Errorf("scan last tend by pr: %w", err)
		}
		out[prNumber] = t.UTC()
	}
	return out, rows.Err()
}

// RecentDispatches returns the most recent dispatches for a loop, newest first.
// A non-zero issue restricts the result to that issue.
func (s *Store) RecentDispatches(loop, repo string, issue, limit int) ([]Dispatch, error) {
	return s.recentDispatches(loop, repo, issue, limit, false)
}

// RecentDispatchesWithLogs is RecentDispatches restricted to the rows that
// have a log file, newest first.
//
// The restriction is made in SQL rather than by filtering a page of rows in
// Go, and the difference is not performance. A caller that wants "the newest
// dispatch an operator can actually read" needs LIMIT 1 against a filtered
// query; filtering a fixed page afterwards gives it a horizon instead, and a
// loop whose tend work is mostly agent-free rebases can push its last agent
// past that horizon and be told it has no dispatches at all.
//
// Only a rebase row lacks a log path today -- every dispatch that spawns a
// process is given one when the row is created -- but the condition is written
// on the column rather than on the kind, because "has something to show" is
// the question being asked.
func (s *Store) RecentDispatchesWithLogs(loop, repo string, issue, limit int) ([]Dispatch, error) {
	return s.recentDispatches(loop, repo, issue, limit, true)
}

func (s *Store) recentDispatches(loop, repo string, issue, limit int, withLogs bool) ([]Dispatch, error) {
	if limit <= 0 {
		limit = 20
	}
	query := `SELECT ` + dispatchColumns + ` FROM dispatches
		WHERE project_id = ? AND loop = ? AND repo = ?`
	args := []any{s.projectID, loop, repo}
	if issue > 0 {
		query += ` AND number = ?`
		args = append(args, issue)
	}
	if withLogs {
		query += ` AND log_path <> ''`
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query recent dispatches: %w", err)
	}
	return scanDispatches(rows)
}

// DispatchesBySession returns every dispatch that used a claude session,
// newest first. A session survives resumes, so this is how one issue's whole
// conversation is found across several runs.
//
// It is scoped like every other read. A session identifier is unique, but a
// scoped caller must not be able to reach another project's row with one.
func (s *Store) DispatchesBySession(sessionID string) ([]Dispatch, error) {
	rows, err := s.db.Query(
		`SELECT `+dispatchColumns+` FROM dispatches
		 WHERE project_id = ? AND session_id = ? ORDER BY id DESC`,
		s.projectID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query dispatches by session: %w", err)
	}
	return scanDispatches(rows)
}

// DispatchesForLoop returns every dispatch a loop has recorded, newest first.
// Session summaries aggregate over the whole history, so this is unpaged.
func (s *Store) DispatchesForLoop(loop, repo string) ([]Dispatch, error) {
	rows, err := s.db.Query(
		`SELECT `+dispatchColumns+` FROM dispatches
		 WHERE project_id = ? AND loop = ? AND repo = ? ORDER BY id DESC`,
		s.projectID, loop, repo)
	if err != nil {
		return nil, fmt.Errorf("query dispatches for loop: %w", err)
	}
	return scanDispatches(rows)
}

// GetDispatch returns one dispatch by identifier.
func (s *Store) GetDispatch(id int64) (Dispatch, error) {
	row := s.db.QueryRow(
		`SELECT `+dispatchColumns+` FROM dispatches WHERE id = ? AND project_id = ?`,
		id, s.projectID)
	d, err := scanDispatch(row)
	if err != nil {
		return Dispatch{}, fmt.Errorf("get dispatch %d: %w", id, err)
	}
	return d, nil
}

// PutPRLink inserts or replaces one issue-to-pull-request mapping.
func (s *Store) PutPRLink(l PRLink) error {
	_, err := s.db.Exec(`
		INSERT INTO pr_links (project_id, loop, repo, number, pr_number, head_ref,
		                      base_ref, behind_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, loop, repo, number) DO UPDATE SET
		  pr_number = excluded.pr_number,
		  head_ref  = excluded.head_ref,
		  base_ref  = excluded.base_ref,
		  behind_by = excluded.behind_by`,
		s.projectID, l.Loop, l.Repo, l.Number, l.PRNumber, l.HeadRef, l.BaseRef, l.BehindBy)
	if err != nil {
		return fmt.Errorf("put pr link: %w", err)
	}
	return nil
}

// PRLinks returns every issue-to-pull-request mapping for one loop.
func (s *Store) PRLinks(loop, repo string) (map[int]PRLink, error) {
	rows, err := s.db.Query(
		`SELECT number, pr_number, head_ref, base_ref, behind_by FROM pr_links
		 WHERE project_id = ? AND loop = ? AND repo = ?`, s.projectID, loop, repo)
	if err != nil {
		return nil, fmt.Errorf("query pr links: %w", err)
	}
	defer rows.Close()

	out := make(map[int]PRLink)
	for rows.Next() {
		l := PRLink{ProjectID: s.projectID, Loop: loop, Repo: repo}
		if err := rows.Scan(&l.Number, &l.PRNumber, &l.HeadRef, &l.BaseRef, &l.BehindBy); err != nil {
			return nil, fmt.Errorf("scan pr link: %w", err)
		}
		out[l.Number] = l
	}
	return out, rows.Err()
}

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

// TendConflict returns the backoff state for one pull request's rebase
// conflict. The second result reports whether a row exists at all, because
// the zero SeenCount of a never-seen pull request and the zero SeenCount of a
// row that was somehow written with it must not be confused by a caller that
// only checked the value.
func (s *Store) TendConflict(loop, repo string, prNumber int) (TendConflict, bool, error) {
	var c TendConflict
	var firstSeen, lastSeen time.Time
	var retryAfter int64
	err := s.db.QueryRow(
		`SELECT fingerprint, seen_count, first_seen_at, last_seen_at, retry_after
		 FROM tend_conflicts
		 WHERE project_id = ? AND loop = ? AND repo = ? AND pr_number = ?`,
		s.projectID, loop, repo, prNumber).
		Scan(&c.Fingerprint, &c.SeenCount, &firstSeen, &lastSeen, &retryAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return TendConflict{}, false, nil
	}
	if err != nil {
		return TendConflict{}, false, fmt.Errorf("get tend conflict: %w", err)
	}
	c.ProjectID = s.projectID
	c.Loop = loop
	c.Repo = repo
	c.PRNumber = prNumber
	c.FirstSeenAt = firstSeen.UTC()
	c.LastSeenAt = lastSeen.UTC()
	c.RetryAfter = retryAfterTime(retryAfter)
	return c, true, nil
}

// PutTendConflict inserts or replaces the backoff row for one pull request,
// writing exactly the row it is given -- SeenCount and RetryAfter included.
//
// It does NOT compute either. The backoff schedule (conflictBackoff in
// loopcmd) is a loopcmd constant the store cannot see, so a count advanced in
// SQL could not derive the deadline that goes with it. Every caller holds the
// loop lock -- act runs under it on all three passes that can reach a rebase
// -- so the read-then-write this implies is not the racy pattern
// BeginDispatch exists to avoid: nothing else can write this row between a
// caller's TendConflict read and its PutTendConflict write.
func (s *Store) PutTendConflict(c TendConflict) error {
	_, err := s.db.Exec(`
		INSERT INTO tend_conflicts (project_id, loop, repo, pr_number, fingerprint,
		                            seen_count, first_seen_at, last_seen_at, retry_after)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, loop, repo, pr_number) DO UPDATE SET
		  fingerprint   = excluded.fingerprint,
		  seen_count    = excluded.seen_count,
		  first_seen_at = excluded.first_seen_at,
		  last_seen_at  = excluded.last_seen_at,
		  retry_after   = excluded.retry_after`,
		s.projectID, c.Loop, c.Repo, c.PRNumber, c.Fingerprint, c.SeenCount,
		c.FirstSeenAt.UTC(), c.LastSeenAt.UTC(), retryAfterSeconds(c.RetryAfter))
	if err != nil {
		return fmt.Errorf("put tend conflict: %w", err)
	}
	return nil
}

// DeleteTendConflict removes one pull request's backoff row, following the
// pull request out wherever it stops being tendable: a clean rebase, a
// pr_links delete in tendcheck, or the closed-pull-request cleanup. Deleting a
// row that is not there is not an error, the same as DeletePRLink -- more than
// one of those paths may reach the same row.
func (s *Store) DeleteTendConflict(loop, repo string, prNumber int) error {
	_, err := s.db.Exec(
		`DELETE FROM tend_conflicts WHERE project_id = ? AND loop = ? AND repo = ? AND pr_number = ?`,
		s.projectID, loop, repo, prNumber)
	if err != nil {
		return fmt.Errorf("delete tend conflict: %w", err)
	}
	return nil
}

// RecordTick appends one tick row and returns its identifier.
func (s *Store) RecordTick(loop string, breakerTripped bool, summary string) (int64, error) {
	tripped := 0
	if breakerTripped {
		tripped = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO ticks (project_id, loop, started_at, breaker_tripped, summary_json)
		 VALUES (?, ?, ?, ?, ?)`, s.projectID, loop, time.Now().UTC(), tripped, summary)
	if err != nil {
		return 0, fmt.Errorf("record tick: %w", err)
	}
	return res.LastInsertId()
}

// TickCount returns how many ticks this loop has recorded.
func (s *Store) TickCount(loop string) (int64, error) {
	var n int64
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM ticks WHERE project_id = ? AND loop = ?`,
		s.projectID, loop).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("tick count: %w", err)
	}
	return n, nil
}

// SetCooldown records the time before which the loop must not dispatch.
func (s *Store) SetCooldown(loop string, until time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO cooldowns (project_id, loop, until) VALUES (?, ?, ?)
		ON CONFLICT(project_id, loop) DO UPDATE SET until = excluded.until`,
		s.projectID, loop, until.UTC())
	if err != nil {
		return fmt.Errorf("set cooldown: %w", err)
	}
	return nil
}

// CooldownUntil returns the recorded cooldown, or the zero time when none is set.
func (s *Store) CooldownUntil(loop string) (time.Time, error) {
	var t time.Time
	err := s.db.QueryRow(
		`SELECT until FROM cooldowns WHERE project_id = ? AND loop = ?`,
		s.projectID, loop).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("cooldown until: %w", err)
	}
	return t.UTC(), nil
}

// RunningDispatches returns every dispatch still marked running, in every
// project. It is the machine-wide read the per-project view cannot answer.
func (d *DB) RunningDispatches() ([]Dispatch, error) {
	rows, err := d.db.Query(
		`SELECT `+dispatchColumns+` FROM dispatches WHERE status = ? ORDER BY id DESC`,
		StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("query running dispatches: %w", err)
	}
	return scanDispatches(rows)
}

// StoppedIssues returns every stopped issue on the machine, in every project.
// It is the machine-wide read the per-project view cannot answer -- the same
// reason DB.RunningDispatches exists beside Store.RunningDispatches.
func (d *DB) StoppedIssues() ([]StoppedIssue, error) {
	rows, err := d.db.Query(`
		SELECT project_id, loop, repo, number, stopped_reason
		FROM issues WHERE stopped = 1
		ORDER BY project_id, loop, repo, number`)
	if err != nil {
		return nil, fmt.Errorf("query stopped issues: %w", err)
	}
	defer rows.Close()

	var out []StoppedIssue
	for rows.Next() {
		var si StoppedIssue
		if err := rows.Scan(&si.ProjectID, &si.Loop, &si.Repo, &si.Number, &si.Reason); err != nil {
			return nil, fmt.Errorf("scan stopped issue: %w", err)
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// DispatchesForProject returns every dispatch of one project, newest first.
//
// Session summaries aggregate a whole project at once, so reading it in one
// query is what replaced opening one database per loop.
func (d *DB) DispatchesForProject(projectID string) ([]Dispatch, error) {
	rows, err := d.db.Query(
		`SELECT `+dispatchColumns+` FROM dispatches
		 WHERE project_id = ? ORDER BY id DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query dispatches for project: %w", err)
	}
	return scanDispatches(rows)
}

// Dispatches returns every dispatch on the machine, newest first.
//
// The sessions report spans the machine, so the per-project read cannot answer
// it: naming one project at a time would need the caller to know every project
// up front, and the report exists to tell it what is there.
//
// It is also the only read that returns rows with an empty project_id. Those
// are pre-project rows the sweep could not claim (see upgradeKeys), and every
// scoped query hides them because no project selector can ever match an empty
// string. A machine-wide report has to show them anyway, so the caller must be
// ready to label a dispatch that belongs to no project it can name.
func (d *DB) Dispatches() ([]Dispatch, error) {
	rows, err := d.db.Query(
		`SELECT ` + dispatchColumns + ` FROM dispatches ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("query dispatches: %w", err)
	}
	return scanDispatches(rows)
}

// LoopStates returns the tick count, the last tick and the total cost of every
// loop on the machine.
//
// Cost is keyed by repository as well as by loop, because a loop whose repo was
// changed in its configuration still holds the old repository's dispatches. The
// per-loop reads this replaced filtered on repo, and a report that silently
// added the two together would overstate what the loop has spent.
//
// It runs two queries and merges them, rather than joining. The ticks table has
// no repo column, and a loop that has ticked but never dispatched must still
// appear: a join would drop it, and a report of "0 ticks" for a running loop is
// worse than no report at all.
func (d *DB) LoopStates() ([]LoopState, error) {
	byKey := map[LoopKey]*LoopState{}
	at := func(projectID, loop string) *LoopState {
		k := LoopKey{ProjectID: projectID, Loop: loop}
		if cur, ok := byKey[k]; ok {
			return cur
		}
		cur := &LoopState{ProjectID: projectID, Loop: loop}
		byKey[k] = cur
		return cur
	}

	if err := d.eachRow(
		`SELECT project_id, loop, COUNT(*) FROM ticks GROUP BY project_id, loop`,
		func(rows *sql.Rows) error {
			var projectID, loop string
			var count int64
			if err := rows.Scan(&projectID, &loop, &count); err != nil {
				return err
			}
			at(projectID, loop).Ticks = count
			return nil
		}); err != nil {
		return nil, fmt.Errorf("count ticks: %w", err)
	}

	// The newest tick is read as a column of the ticks table, not as MAX(). An
	// aggregate has no declared type, so the driver would hand back a string
	// where every other read of this column yields a time.
	if err := d.eachRow(
		`SELECT t.project_id, t.loop, t.started_at FROM ticks t
		 JOIN (SELECT project_id, loop, MAX(id) AS newest FROM ticks
		       GROUP BY project_id, loop) m ON t.id = m.newest`,
		func(rows *sql.Rows) error {
			var projectID, loop string
			var started time.Time
			if err := rows.Scan(&projectID, &loop, &started); err != nil {
				return err
			}
			at(projectID, loop).LastTick = started.UTC()
			return nil
		}); err != nil {
		return nil, fmt.Errorf("read the newest tick: %w", err)
	}

	if err := d.eachRow(
		`SELECT project_id, loop, repo, SUM(cost_usd) FROM dispatches
		 GROUP BY project_id, loop, repo`,
		func(rows *sql.Rows) error {
			var projectID, loop, repo string
			var cost sql.NullFloat64
			if err := rows.Scan(&projectID, &loop, &repo, &cost); err != nil {
				return err
			}
			st := at(projectID, loop)
			if st.CostByRepo == nil {
				st.CostByRepo = map[string]float64{}
			}
			st.CostByRepo[repo] = cost.Float64
			st.Cost += cost.Float64
			return nil
		}); err != nil {
		return nil, fmt.Errorf("sum dispatch cost: %w", err)
	}

	out := make([]LoopState, 0, len(byKey))
	for _, st := range byKey {
		out = append(out, *st)
	}
	return out, nil
}

// EarliestRetryAfterAt returns the soonest pending retry deadline, if there is
// one, with the cooldown boundary judged against the supplied clock.
//
// The clock is a parameter rather than time.Now() read inside: the daemon
// carries its own Now seam and has to be able to freeze this boundary against
// it in a test, and MarkNeedsRetry already takes its time from the caller for
// the same reason. There is deliberately no time.Now() convenience wrapper
// beside this: one existed, no production code ever called it, and a second
// entry point that reads a clock this package cannot control is exactly what
// the seam exists to avoid.
//
// It is scoped to rows that a retry can still act on. A parked issue, or one
// whose failure flag was cleared, keeps its old deadline in the row, and
// returning that value would give the daemon a deadline permanently in the past
// to spin on. A loop whose circuit breaker is in cooldown is excluded for the
// same reason: Decide returns with no decisions at all while the cooldown runs,
// so needs_retry stays set and the deadline stays in the past for its whole
// length.
//
// The deadline is selected as a column and ordered by, not read with MIN().
// An aggregate has no declared type, so the driver hands back a value of a
// different type than every other read of that column.
//
// The cooldown comparison is done in SQL. Every timestamp in this database is
// written through time.Time.UTC(), which the driver stores as text with a fixed
// "+0000 UTC" suffix, so a text comparison orders them correctly. A writer that
// omitted .UTC() would break this and legacy.go's refresh comparison together.
//
// skip names loops whose rows this call must step over, and it exists because
// exactly one row is returned. A caller that cannot act on the earliest row --
// the daemon, when the loop it names cannot be routed right now -- would
// otherwise be handed that same row on every call and would never see any other
// loop's due deadline: one stuck loop starves the whole machine. The set is the
// CALLER's, and deliberately not a column here: nothing about being unservable
// belongs in the durable state, and the caller re-establishes it every pass.
func (d *DB) EarliestRetryAfterAt(now time.Time, skip []LoopKey) (RetryDue, bool, error) {
	var (
		due        RetryDue
		retryAfter int64
	)
	// Placeholders, never the loop names interpolated into the SQL: a project
	// id and a loop name both come from files on disk, and one apostrophe in a
	// loop name would otherwise be a syntax error at best.
	args := make([]any, 0, 1+2*len(skip))
	args = append(args, now.UTC())
	var excluded strings.Builder
	for _, k := range skip {
		excluded.WriteString(" AND NOT (i.project_id = ? AND i.loop = ?)")
		args = append(args, k.ProjectID, k.Loop)
	}
	err := d.db.QueryRow(`
		SELECT i.project_id, i.loop, i.repo, i.number, i.retry_after
		FROM issues i
		LEFT JOIN cooldowns c
		  ON c.project_id = i.project_id AND c.loop = i.loop
		WHERE i.retry_after > 0 AND i.needs_retry = 1 AND i.parked = 0
		  AND (c.until IS NULL OR c.until <= ?)`+excluded.String()+`
		ORDER BY i.retry_after ASC
		LIMIT 1`, args...).
		Scan(&due.ProjectID, &due.Loop, &due.Repo, &due.Number, &retryAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return RetryDue{}, false, nil
	}
	if err != nil {
		return RetryDue{}, false, fmt.Errorf("earliest retry after: %w", err)
	}
	due.At = retryAfterTime(retryAfter)
	return due, true, nil
}

// eachRow runs a query and calls scan for every row. It exists so the three
// aggregates above do not each repeat the same close-and-check dance.
func (d *DB) eachRow(query string, scan func(*sql.Rows) error) error {
	rows, err := d.db.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
