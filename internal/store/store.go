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
  parked          INTEGER NOT NULL DEFAULT 0,
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
  legacy_id     INTEGER NOT NULL DEFAULT 0
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
	// detached runner on the machine. Each write is a single small statement, so
	// a wait this long only ever covers a queue, never a slow transaction.
	dsn := "file:" + path +
		"?_pragma=busy_timeout(30000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)"

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
	{"issues", "parked", "INTEGER NOT NULL DEFAULT 0"},
	{"dispatches", "pr_number", "INTEGER NOT NULL DEFAULT 0"},
	{"dispatches", "title", "TEXT NOT NULL DEFAULT ''"},
	{"pr_links", "behind_by", "INTEGER NOT NULL DEFAULT 0"},
	{"dispatches", "project_id", "TEXT NOT NULL DEFAULT ''"},
	{"dispatches", "legacy_source", "TEXT NOT NULL DEFAULT ''"},
	{"dispatches", "legacy_id", "INTEGER NOT NULL DEFAULT 0"},
	{"ticks", "project_id", "TEXT NOT NULL DEFAULT ''"},
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
		last_retry_tick, needs_retry, session_started, parked, updated_at`},
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
		       needs_retry, session_started, parked, updated_at
		FROM issues WHERE project_id = ? AND loop = ? AND repo = ?`,
		s.projectID, loop, repo)
	if err != nil {
		return nil, fmt.Errorf("query issues: %w", err)
	}
	defer rows.Close()

	out := make(map[int]IssueState)
	for rows.Next() {
		st := IssueState{ProjectID: s.projectID, Loop: loop, Repo: repo}
		if err := rows.Scan(&st.Number, &st.SessionID, &st.WorktreePath,
			&st.RetryCount, &st.LastRetryTick, &st.NeedsRetry, &st.SessionStarted,
			&st.Parked, &st.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan issue: %w", err)
		}
		out[st.Number] = st
	}
	return out, rows.Err()
}

// PutIssueState inserts or replaces one issue record.
func (s *Store) PutIssueState(st IssueState) error {
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(`
		INSERT INTO issues (project_id, loop, repo, number, session_id, worktree_path,
		                    retry_count, last_retry_tick, needs_retry,
		                    session_started, parked, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project_id, loop, repo, number) DO UPDATE SET
		  session_id      = excluded.session_id,
		  worktree_path   = excluded.worktree_path,
		  retry_count     = excluded.retry_count,
		  last_retry_tick = excluded.last_retry_tick,
		  needs_retry     = excluded.needs_retry,
		  session_started = excluded.session_started,
		  parked          = excluded.parked,
		  updated_at      = excluded.updated_at`,
		s.projectID, st.Loop, st.Repo, st.Number, st.SessionID, st.WorktreePath,
		st.RetryCount, st.LastRetryTick, st.NeedsRetry, st.SessionStarted,
		st.Parked, st.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("put issue state: %w", err)
	}
	return nil
}

// MarkNeedsRetry records that a dispatch for this issue failed. It is durable,
// so a tick that declines to act on the failure (backoff or circuit breaker)
// does not lose it.
func (s *Store) MarkNeedsRetry(loop, repo string, number int) error {
	_, err := s.db.Exec(`
		INSERT INTO issues (project_id, loop, repo, number, needs_retry, updated_at)
		VALUES (?, ?, ?, ?, 1, ?)
		ON CONFLICT(project_id, loop, repo, number) DO UPDATE SET
		  needs_retry = 1, updated_at = excluded.updated_at`,
		s.projectID, loop, repo, number, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("mark needs retry: %w", err)
	}
	return nil
}

// ClearNeedsRetry clears a failure flag that no retry can act on. Without it an
// issue whose failure was recorded while it was not in flight is stranded
// permanently.
func (s *Store) ClearNeedsRetry(loop, repo string, number int) error {
	_, err := s.db.Exec(`
		UPDATE issues SET needs_retry = 0, updated_at = ?
		WHERE project_id = ? AND loop = ? AND repo = ? AND number = ?`,
		time.Now().UTC(), s.projectID, loop, repo, number)
	if err != nil {
		return fmt.Errorf("clear needs retry: %w", err)
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
func (s *Store) MarkSessionStarted(loop, repo string, number int) error {
	_, err := s.db.Exec(`
		UPDATE issues SET session_started = 1, updated_at = ?
		WHERE project_id = ? AND loop = ? AND repo = ? AND number = ?`,
		time.Now().UTC(), s.projectID, loop, repo, number)
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
		    updated_at = ?
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
		                        status, started_at, log_path, pr_number, title)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.projectID, d.Loop, d.Repo, d.Number, d.Kind, d.SessionID,
		StatusRunning, time.Now().UTC(), d.LogPath, d.PRNumber, d.Title)
	if err != nil {
		return 0, fmt.Errorf("create dispatch: %w", err)
	}
	return res.LastInsertId()
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
	api_error, log_path, pr_number, title, legacy_source, legacy_id`

func scanDispatch(sc interface{ Scan(...any) error }) (Dispatch, error) {
	var d Dispatch
	var pidStart, finished sql.NullTime
	err := sc.Scan(&d.ID, &d.ProjectID, &d.Loop, &d.Repo, &d.Number, &d.Kind,
		&d.SessionID, &d.PID, &pidStart, &d.Status, &d.StartedAt, &finished,
		&d.ExitCode, &d.CostUSD, &d.DurationMS, &d.APIError, &d.LogPath,
		&d.PRNumber, &d.Title, &d.LegacySource, &d.LegacyID)
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

// RecentDispatches returns the most recent dispatches for a loop, newest first.
// A non-zero issue restricts the result to that issue.
func (s *Store) RecentDispatches(loop, repo string, issue, limit int) ([]Dispatch, error) {
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
