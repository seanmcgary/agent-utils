// Package store holds the durable loop state in SQLite.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// The pragmas live in the DSN, not in this schema string. journal_mode is
// persisted in the file, but busy_timeout and foreign_keys are PER CONNECTION.
// The tick process and every detached runner open this file at the same time,
// so a pragma applied to one pooled connection does not protect the others.
const schema = `
CREATE TABLE IF NOT EXISTS issues (
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
  PRIMARY KEY (loop, repo, number)
);

CREATE TABLE IF NOT EXISTS dispatches (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  loop         TEXT NOT NULL,
  repo         TEXT NOT NULL,
  number       INTEGER NOT NULL,
  kind         TEXT NOT NULL,
  session_id   TEXT NOT NULL DEFAULT '',
  pid          INTEGER NOT NULL DEFAULT 0,
  pid_start_at TIMESTAMP,
  status       TEXT NOT NULL,
  started_at   TIMESTAMP NOT NULL,
  finished_at  TIMESTAMP,
  exit_code    INTEGER NOT NULL DEFAULT 0,
  cost_usd     REAL NOT NULL DEFAULT 0,
  duration_ms  INTEGER NOT NULL DEFAULT 0,
  api_error    TEXT NOT NULL DEFAULT '',
  log_path     TEXT NOT NULL DEFAULT '',
  pr_number    INTEGER NOT NULL DEFAULT 0,
  title        TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS dispatches_running
  ON dispatches (loop, repo, status);

CREATE TABLE IF NOT EXISTS pr_links (
  loop       TEXT NOT NULL,
  repo       TEXT NOT NULL,
  number     INTEGER NOT NULL,
  pr_number  INTEGER NOT NULL,
  head_ref   TEXT NOT NULL,
  base_ref   TEXT NOT NULL,
  behind_by  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (loop, repo, number)
);

CREATE TABLE IF NOT EXISTS ticks (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  loop            TEXT NOT NULL,
  started_at      TIMESTAMP NOT NULL,
  breaker_tripped INTEGER NOT NULL DEFAULT 0,
  summary_json    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS cooldowns (
  loop  TEXT PRIMARY KEY,
  until TIMESTAMP NOT NULL
);
`

// Store is the durable loop state.
type Store struct {
	db *sql.DB
}

// Open opens the database at path and applies the schema.
func Open(path string) (*Store, error) {
	// Every connection must carry busy_timeout, because several processes write
	// this file. Passing the pragmas in the DSN is the only way to guarantee it.
	dsn := "file:" + path +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	// The database holds session identifiers. Keep it private to this user.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Chmod(path+suffix, 0o600)
	}
	return &Store{db: db}, nil
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
}

// migrate adds any column missing from an existing database. Each column has a
// default, so an added column needs no backfill.
func migrate(db *sql.DB) error {
	for _, c := range addedColumns {
		has, err := hasColumn(db, c.table, c.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", c.table, c.column, c.def)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("add column %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
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

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// IssueStates returns every issue record for one loop and repository.
func (s *Store) IssueStates(loop, repo string) (map[int]IssueState, error) {
	rows, err := s.db.Query(`
		SELECT number, session_id, worktree_path, retry_count, last_retry_tick,
		       needs_retry, session_started, parked, updated_at
		FROM issues WHERE loop = ? AND repo = ?`, loop, repo)
	if err != nil {
		return nil, fmt.Errorf("query issues: %w", err)
	}
	defer rows.Close()

	out := make(map[int]IssueState)
	for rows.Next() {
		st := IssueState{Loop: loop, Repo: repo}
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
		INSERT INTO issues (loop, repo, number, session_id, worktree_path,
		                    retry_count, last_retry_tick, needs_retry,
		                    session_started, parked, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(loop, repo, number) DO UPDATE SET
		  session_id      = excluded.session_id,
		  worktree_path   = excluded.worktree_path,
		  retry_count     = excluded.retry_count,
		  last_retry_tick = excluded.last_retry_tick,
		  needs_retry     = excluded.needs_retry,
		  session_started = excluded.session_started,
		  parked          = excluded.parked,
		  updated_at      = excluded.updated_at`,
		st.Loop, st.Repo, st.Number, st.SessionID, st.WorktreePath,
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
		INSERT INTO issues (loop, repo, number, needs_retry, updated_at)
		VALUES (?, ?, ?, 1, ?)
		ON CONFLICT(loop, repo, number) DO UPDATE SET
		  needs_retry = 1, updated_at = excluded.updated_at`,
		loop, repo, number, time.Now().UTC())
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
		WHERE loop = ? AND repo = ? AND number = ?`,
		time.Now().UTC(), loop, repo, number)
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
		WHERE loop = ? AND repo = ? AND number = ?`,
		time.Now().UTC(), loop, repo, number)
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
		WHERE loop = ? AND repo = ? AND number = ?`,
		time.Now().UTC(), loop, repo, number)
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
	return IssueState{Loop: loop, Repo: repo, Number: number}, nil
}

// LastTick returns the time of the most recent recorded tick.
func (s *Store) LastTick(loop string) (time.Time, error) {
	var t time.Time
	err := s.db.QueryRow(
		`SELECT started_at FROM ticks WHERE loop = ? ORDER BY id DESC LIMIT 1`,
		loop).Scan(&t)
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
		 WHERE loop = ? AND repo = ? GROUP BY number`, loop, repo)
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
	_, err := s.db.Exec(`DELETE FROM issues WHERE loop = ? AND repo = ? AND number = ?`,
		loop, repo, number)
	if err != nil {
		return fmt.Errorf("delete issue state: %w", err)
	}
	return nil
}

// CreateDispatch inserts a running dispatch row and returns its identifier.
func (s *Store) CreateDispatch(d Dispatch) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO dispatches (loop, repo, number, kind, session_id,
		                        status, started_at, log_path, pr_number, title)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Loop, d.Repo, d.Number, d.Kind, d.SessionID,
		StatusRunning, time.Now().UTC(), d.LogPath, d.PRNumber, d.Title)
	if err != nil {
		return 0, fmt.Errorf("create dispatch: %w", err)
	}
	return res.LastInsertId()
}

// SetDispatchProcess records the operating system process for a dispatch.
func (s *Store) SetDispatchProcess(id int64, pid int, startedAt time.Time) error {
	_, err := s.db.Exec(`UPDATE dispatches SET pid = ?, pid_start_at = ? WHERE id = ?`,
		pid, startedAt.UTC(), id)
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
		WHERE id = ? AND status = ?`,
		r.Status, r.ExitCode, r.CostUSD, r.DurationMS, r.APIError, time.Now().UTC(),
		id, StatusRunning)
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

const dispatchColumns = `id, loop, repo, number, kind, session_id, pid, pid_start_at,
	status, started_at, finished_at, exit_code, cost_usd, duration_ms, api_error,
	log_path, pr_number, title`

func scanDispatch(sc interface{ Scan(...any) error }) (Dispatch, error) {
	var d Dispatch
	var pidStart, finished sql.NullTime
	err := sc.Scan(&d.ID, &d.Loop, &d.Repo, &d.Number, &d.Kind, &d.SessionID,
		&d.PID, &pidStart, &d.Status, &d.StartedAt, &finished, &d.ExitCode,
		&d.CostUSD, &d.DurationMS, &d.APIError, &d.LogPath, &d.PRNumber, &d.Title)
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

// RunningDispatches returns every dispatch still marked running.
func (s *Store) RunningDispatches(loop, repo string) ([]Dispatch, error) {
	rows, err := s.db.Query(
		`SELECT `+dispatchColumns+` FROM dispatches
		 WHERE loop = ? AND repo = ? AND status = ?`, loop, repo, StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("query running dispatches: %w", err)
	}
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

// GetDispatch returns one dispatch by identifier.
func (s *Store) GetDispatch(id int64) (Dispatch, error) {
	row := s.db.QueryRow(`SELECT `+dispatchColumns+` FROM dispatches WHERE id = ?`, id)
	d, err := scanDispatch(row)
	if err != nil {
		return Dispatch{}, fmt.Errorf("get dispatch %d: %w", id, err)
	}
	return d, nil
}

// PutPRLink inserts or replaces one issue-to-pull-request mapping.
func (s *Store) PutPRLink(l PRLink) error {
	_, err := s.db.Exec(`
		INSERT INTO pr_links (loop, repo, number, pr_number, head_ref, base_ref, behind_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(loop, repo, number) DO UPDATE SET
		  pr_number = excluded.pr_number,
		  head_ref  = excluded.head_ref,
		  base_ref  = excluded.base_ref,
		  behind_by = excluded.behind_by`,
		l.Loop, l.Repo, l.Number, l.PRNumber, l.HeadRef, l.BaseRef, l.BehindBy)
	if err != nil {
		return fmt.Errorf("put pr link: %w", err)
	}
	return nil
}

// PRLinks returns every issue-to-pull-request mapping for one loop.
func (s *Store) PRLinks(loop, repo string) (map[int]PRLink, error) {
	rows, err := s.db.Query(
		`SELECT number, pr_number, head_ref, base_ref, behind_by FROM pr_links
		 WHERE loop = ? AND repo = ?`, loop, repo)
	if err != nil {
		return nil, fmt.Errorf("query pr links: %w", err)
	}
	defer rows.Close()

	out := make(map[int]PRLink)
	for rows.Next() {
		l := PRLink{Loop: loop, Repo: repo}
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
		`INSERT INTO ticks (loop, started_at, breaker_tripped, summary_json)
		 VALUES (?, ?, ?, ?)`, loop, time.Now().UTC(), tripped, summary)
	if err != nil {
		return 0, fmt.Errorf("record tick: %w", err)
	}
	return res.LastInsertId()
}

// TickCount returns how many ticks this loop has recorded.
func (s *Store) TickCount(loop string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM ticks WHERE loop = ?`, loop).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("tick count: %w", err)
	}
	return n, nil
}

// SetCooldown records the time before which the loop must not dispatch.
func (s *Store) SetCooldown(loop string, until time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO cooldowns (loop, until) VALUES (?, ?)
		ON CONFLICT(loop) DO UPDATE SET until = excluded.until`, loop, until.UTC())
	if err != nil {
		return fmt.Errorf("set cooldown: %w", err)
	}
	return nil
}

// CooldownUntil returns the recorded cooldown, or the zero time when none is set.
func (s *Store) CooldownUntil(loop string) (time.Time, error) {
	var t time.Time
	err := s.db.QueryRow(`SELECT until FROM cooldowns WHERE loop = ?`, loop).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("cooldown until: %w", err)
	}
	return t.UTC(), nil
}
