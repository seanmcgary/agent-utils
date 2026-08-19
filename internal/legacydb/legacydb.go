// Package legacydb reads a per-loop state.db written by the layout that came
// before the canonical database, so its rows can be imported into that file.
//
// Nothing here writes to the file it opens. It applies no schema, adds no
// column and performs no key upgrade, because a runner process spawned by the
// OLD binary may still be writing that same file with the old code. A new
// schema applied underneath it would break the statements it has left to run:
// its issue write names a conflict target the project-keyed schema no longer
// has, so the durable state of a live agent would be lost. The legacy file is
// also the only copy of that state until the import commits, which is the
// second reason this package is read-only.
package legacydb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/store"

	// The driver is registered by importing it. This package never uses its
	// exported API directly, unlike internal/store, which inspects its errors.
	_ "modernc.org/sqlite"
)

// Legacy table names. They are constants, and they are the only strings ever
// interpolated into a PRAGMA statement, which takes no bind parameter.
const (
	tableIssues     = "issues"
	tableDispatches = "dispatches"
	tablePRLinks    = "pr_links"
	tableTicks      = "ticks"
	tableCooldowns  = "cooldowns"
)

// Every column this package knows how to read, per table, in scan order.
//
// These allowlists are the ONLY source of the names that reach a SELECT. A
// column named by the file but not listed here is never selected, so a
// hand-edited or unrelated database cannot steer the query.
var (
	issueColumns = []string{
		"loop", "repo", "number", "session_id", "worktree_path", "retry_count",
		"last_retry_tick", "needs_retry", "session_started", "parked", "updated_at",
	}
	dispatchColumns = []string{
		"id", "loop", "repo", "number", "kind", "session_id", "pid", "pid_start_at",
		"status", "started_at", "finished_at", "exit_code", "cost_usd", "duration_ms",
		"api_error", "log_path", "pr_number", "title",
	}
	prLinkColumns = []string{
		"loop", "repo", "number", "pr_number", "head_ref", "base_ref", "behind_by",
	}
	tickColumns     = []string{"loop", "started_at", "breaker_tripped", "summary_json"}
	cooldownColumns = []string{"loop", "until"}
)

// DB is an open handle on one legacy per-loop database.
type DB struct {
	db *sql.DB
}

// Open opens a legacy per-loop database for reading.
//
// busy_timeout is what makes a read wait for an old runner's write instead of
// failing the tick. It is per connection, so it only takes effect from the DSN.
//
// journal_mode is deliberately NOT set. Setting it rewrites the file header on
// any database not already in that mode, and this package promises to write
// nothing. Every legacy file was created by store.Open in WAL mode, so a reader
// has nothing to change anyway.
//
// No DDL, no added column, no key upgrade and no chmod run here -- see the
// package comment.
func Open(path string) (*DB, error) {
	// The driver CREATES a database that does not exist, on the first
	// connection. That would write a file this package promised never to touch,
	// and it would make a mistyped path look like an empty legacy database with
	// nothing to import, which is indistinguishable from a successful read.
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("stat legacy database %s: %w", path, err)
	}

	dsn := "file:" + path + "?_pragma=busy_timeout(30000)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open legacy database %s: %w", path, err)
	}
	// One connection, as in store.Open. The read transaction below must see the
	// same connection for every statement in it.
	db.SetMaxOpenConns(1)
	return &DB{db: db}, nil
}

// Close closes the handle. The legacy file is left exactly as it was found.
func (d *DB) Close() error {
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("close legacy database: %w", err)
	}
	return nil
}

// Data is one loop's rows, read from a legacy database in one transaction.
type Data struct {
	Issues []store.IssueState
	// Dispatches carry the LEGACY identifier in ID. That is the number the
	// running agent has on its command line and in its log file name, so it is
	// what a liveness check must match; the canonical database renumbers the row
	// on import and records this value in Dispatch.LegacyID.
	Dispatches []store.Dispatch
	PRLinks    []store.PRLink
	Ticks      []store.Tick
	// Cooldown is nil when the loop has none. A loop that never tripped its
	// breaker has no row, and a zero time would read as "cooled down in year 1".
	Cooldown *store.Cooldown
}

// Read returns every row belonging to one loop, in ONE read transaction.
//
// The loop filter is not an optimisation. Two loops may legitimately share one
// state_dir, so a single legacy file can hold the rows of more than one loop,
// and each loop is claimed by a different project. Importing the file whole
// would hand one project the other's state.
//
// The single transaction is what makes the caller's seal decision safe. Reading
// the dispatch rows and then asking separately whether any still run can observe
// a runner finishing in between: the source would be sealed while the rows the
// importer copied still say "running", and the next tick's reaper would rewrite
// a successful run as failed.
//
// A file holding none of the expected tables is not an error. It returns an
// empty Data, because there is nothing in it to lose and failing here would
// block every tick of the loop. A file that fails a real read -- corrupt, or
// not a database at all -- returns a wrapped error.
func (d *DB) Read(loop string) (Data, error) {
	// ReadOnly is a deferred BEGIN in this driver: the snapshot is taken by the
	// first SELECT and every later statement in the transaction sees it.
	tx, err := d.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Data{}, fmt.Errorf("begin legacy read transaction: %w", err)
	}
	// Rolled back on every path, never committed. This transaction only reads,
	// and the rollback is what guarantees the file is left as it was found.
	defer func() { _ = tx.Rollback() }()

	var out Data
	if out.Issues, err = readIssues(tx, loop); err != nil {
		return Data{}, err
	}
	if out.Dispatches, err = readDispatches(tx, loop); err != nil {
		return Data{}, err
	}
	if out.PRLinks, err = readPRLinks(tx, loop); err != nil {
		return Data{}, err
	}
	if out.Ticks, err = readTicks(tx, loop); err != nil {
		return Data{}, err
	}
	if out.Cooldown, err = readCooldown(tx, loop); err != nil {
		return Data{}, err
	}
	return out, nil
}

// HasLiveRunner reports whether any dispatch in this data is still being run by
// a live process.
//
// The caller seals a source on liveness, never on status alone: a row left
// "running" by a runner that crashed would otherwise pin the source open
// forever, so its marker file would never be written and every command on the
// machine would re-read the file for good.
//
// isAlive receives the LEGACY identifier, which is what the running process
// carries on its command line.
func (d Data) HasLiveRunner(isAlive func(pid int, dispatchID int64) bool) bool {
	for _, disp := range d.Dispatches {
		if disp.Status != store.StatusRunning {
			continue
		}
		if isAlive(disp.PID, disp.ID) {
			return true
		}
	}
	return false
}

// Rows is the number of rows this data holds. The migration report prints it,
// so an operator can see that a source was read and how much it carried.
func (d Data) Rows() int {
	n := len(d.Issues) + len(d.Dispatches) + len(d.PRLinks) + len(d.Ticks)
	if d.Cooldown != nil {
		n++
	}
	return n
}

// columns returns the names of the columns a legacy table actually has.
//
// PRAGMA table_info on a table that does not exist returns no rows and no
// error, so an empty result means "no such table". That is how a file with none
// of the expected tables reads as empty rather than failing.
func columns(tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, fmt.Errorf("inspect legacy table %s: %w", table, err)
	}
	defer rows.Close()

	have := make(map[string]bool)
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("scan the columns of legacy table %s: %w", table, err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the columns of legacy table %s: %w", table, err)
	}
	return have, nil
}

// present returns the allowlisted columns this file has, in allowlist order.
//
// A file written by an early binary has no title, no pr_number and no
// behind_by. Selecting a column it does not have fails the whole read, and a
// failed read blocks the loop, so every missing column is simply left at its
// zero value instead.
func present(allowed []string, have map[string]bool) []string {
	out := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if have[name] {
			out = append(out, name)
		}
	}
	return out
}

// scanRow scans one row into the destinations named by cols. dest maps a column
// name to the field it fills, so a file missing a column simply never fills it.
func scanRow(rows *sql.Rows, cols []string, dest map[string]any) error {
	into := make([]any, len(cols))
	for i, name := range cols {
		into[i] = dest[name]
	}
	return rows.Scan(into...)
}

func readIssues(tx *sql.Tx, loop string) ([]store.IssueState, error) {
	have, err := columns(tx, tableIssues)
	if err != nil {
		return nil, err
	}
	cols := present(issueColumns, have)
	if len(cols) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(
		`SELECT `+strings.Join(cols, ", ")+` FROM `+tableIssues+` WHERE loop = ?`, loop)
	if err != nil {
		return nil, fmt.Errorf("query legacy issues: %w", err)
	}
	defer rows.Close()

	var out []store.IssueState
	for rows.Next() {
		st := store.IssueState{Loop: loop}
		var updated sql.NullTime
		dest := map[string]any{
			"loop":            &st.Loop,
			"repo":            &st.Repo,
			"number":          &st.Number,
			"session_id":      &st.SessionID,
			"worktree_path":   &st.WorktreePath,
			"retry_count":     &st.RetryCount,
			"last_retry_tick": &st.LastRetryTick,
			"needs_retry":     &st.NeedsRetry,
			"session_started": &st.SessionStarted,
			"parked":          &st.Parked,
			"updated_at":      &updated,
		}
		if err := scanRow(rows, cols, dest); err != nil {
			return nil, fmt.Errorf("scan legacy issue: %w", err)
		}
		if updated.Valid {
			st.UpdatedAt = updated.Time
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read legacy issues: %w", err)
	}
	return out, nil
}

func readDispatches(tx *sql.Tx, loop string) ([]store.Dispatch, error) {
	have, err := columns(tx, tableDispatches)
	if err != nil {
		return nil, err
	}
	cols := present(dispatchColumns, have)
	if len(cols) == 0 {
		return nil, nil
	}
	// Oldest first, so the importer inserts them in the order they happened and
	// the canonical identifiers keep the same relative order as the legacy ones.
	rows, err := tx.Query(
		`SELECT `+strings.Join(cols, ", ")+` FROM `+tableDispatches+
			` WHERE loop = ? ORDER BY id`, loop)
	if err != nil {
		return nil, fmt.Errorf("query legacy dispatches: %w", err)
	}
	defer rows.Close()

	var out []store.Dispatch
	for rows.Next() {
		disp := store.Dispatch{Loop: loop}
		var pidStart, started, finished sql.NullTime
		dest := map[string]any{
			"id":           &disp.ID,
			"loop":         &disp.Loop,
			"repo":         &disp.Repo,
			"number":       &disp.Number,
			"kind":         &disp.Kind,
			"session_id":   &disp.SessionID,
			"pid":          &disp.PID,
			"pid_start_at": &pidStart,
			"status":       &disp.Status,
			"started_at":   &started,
			"finished_at":  &finished,
			"exit_code":    &disp.ExitCode,
			"cost_usd":     &disp.CostUSD,
			"duration_ms":  &disp.DurationMS,
			"api_error":    &disp.APIError,
			"log_path":     &disp.LogPath,
			"pr_number":    &disp.PRNumber,
			"title":        &disp.Title,
		}
		if err := scanRow(rows, cols, dest); err != nil {
			return nil, fmt.Errorf("scan legacy dispatch: %w", err)
		}
		if pidStart.Valid {
			disp.PIDStartAt = pidStart.Time
		}
		if started.Valid {
			disp.StartedAt = started.Time
		}
		if finished.Valid {
			disp.FinishedAt = finished.Time
		}
		out = append(out, disp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read legacy dispatches: %w", err)
	}
	return out, nil
}

func readPRLinks(tx *sql.Tx, loop string) ([]store.PRLink, error) {
	have, err := columns(tx, tablePRLinks)
	if err != nil {
		return nil, err
	}
	cols := present(prLinkColumns, have)
	if len(cols) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(
		`SELECT `+strings.Join(cols, ", ")+` FROM `+tablePRLinks+` WHERE loop = ?`, loop)
	if err != nil {
		return nil, fmt.Errorf("query legacy pull request links: %w", err)
	}
	defer rows.Close()

	var out []store.PRLink
	for rows.Next() {
		link := store.PRLink{Loop: loop}
		dest := map[string]any{
			"loop":      &link.Loop,
			"repo":      &link.Repo,
			"number":    &link.Number,
			"pr_number": &link.PRNumber,
			"head_ref":  &link.HeadRef,
			"base_ref":  &link.BaseRef,
			"behind_by": &link.BehindBy,
		}
		if err := scanRow(rows, cols, dest); err != nil {
			return nil, fmt.Errorf("scan legacy pull request link: %w", err)
		}
		out = append(out, link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read legacy pull request links: %w", err)
	}
	return out, nil
}

func readTicks(tx *sql.Tx, loop string) ([]store.Tick, error) {
	have, err := columns(tx, tableTicks)
	if err != nil {
		return nil, err
	}
	cols := present(tickColumns, have)
	if len(cols) == 0 {
		return nil, nil
	}
	// Oldest first: the tick counter a loop reports is the row count, and the
	// history reads in the order it happened.
	rows, err := tx.Query(
		`SELECT `+strings.Join(cols, ", ")+` FROM `+tableTicks+
			` WHERE loop = ? ORDER BY id`, loop)
	if err != nil {
		return nil, fmt.Errorf("query legacy ticks: %w", err)
	}
	defer rows.Close()

	var out []store.Tick
	for rows.Next() {
		tick := store.Tick{Loop: loop}
		var started sql.NullTime
		dest := map[string]any{
			"loop":            &tick.Loop,
			"started_at":      &started,
			"breaker_tripped": &tick.BreakerTripped,
			"summary_json":    &tick.SummaryJSON,
		}
		if err := scanRow(rows, cols, dest); err != nil {
			return nil, fmt.Errorf("scan legacy tick: %w", err)
		}
		if started.Valid {
			tick.StartedAt = started.Time
		}
		out = append(out, tick)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read legacy ticks: %w", err)
	}
	return out, nil
}

func readCooldown(tx *sql.Tx, loop string) (*store.Cooldown, error) {
	have, err := columns(tx, tableCooldowns)
	if err != nil {
		return nil, err
	}
	cols := present(cooldownColumns, have)
	if len(cols) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(
		`SELECT `+strings.Join(cols, ", ")+` FROM `+tableCooldowns+` WHERE loop = ?`, loop)
	if err != nil {
		return nil, fmt.Errorf("query legacy cooldown: %w", err)
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("read legacy cooldown: %w", err)
		}
		return nil, nil // the loop has no cooldown, which is not a missing value
	}
	cool := store.Cooldown{Loop: loop}
	var until sql.NullTime
	dest := map[string]any{"loop": &cool.Loop, "until": &until}
	if err := scanRow(rows, cols, dest); err != nil {
		return nil, fmt.Errorf("scan legacy cooldown: %w", err)
	}
	if until.Valid {
		cool.Until = until.Time
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read legacy cooldown: %w", err)
	}
	return &cool, nil
}
