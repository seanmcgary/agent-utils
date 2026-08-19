package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The import lives in this package because this package owns the schema. The
// alternative, handing raw transactional access to the migration package, would
// put SQL for these tables in two places and let them drift.

// LegacyKey identifies one legacy per-loop database, from one project's point of
// view.
//
// The path alone is not an identity. Two loops may share one state_dir
// (docs/configuration.md), so one file can hold two loops, and each loop is
// claimed by the project that configures it.
type LegacyKey struct {
	// Path is the source file, absolute and with symlinks resolved.
	Path string
	// ProjectID is the project that claims this loop's rows.
	ProjectID string
	// Loop is the loop whose rows are imported. Rows of any other loop in the
	// same file belong to a different key.
	Loop string
	// Repo is recorded for the report only.
	Repo string
}

// LegacyData is one loop's rows, read out of a legacy database.
type LegacyData struct {
	Issues     []IssueState
	Dispatches []Dispatch // ID holds the identifier the row had in the source
	PRLinks    []PRLink
	Ticks      []Tick
	Cooldown   *Cooldown
}

// LegacySourceRow is what this database remembers about one source.
type LegacySourceRow struct {
	Key            LegacyKey
	State          string
	FirstImported  time.Time
	LastImported   time.Time
	ExistsInRecord bool
}

// ErrSourceClaimed reports that another project already imported this file's
// loop. Two projects that both claim one (path, loop) cannot both be right, and
// silently letting the second one win would attribute a project's whole history
// to its neighbour.
var ErrSourceClaimed = errors.New("another project already imported this source")

// reaperVerdict is the api_error the tick writes when it retires a dispatch
// whose process is gone (internal/loopcmd/tick.go).
//
// A refresh is allowed to overwrite that verdict on an imported row. The reaper
// guessed from a process that had already exited; the source file holds what the
// runner actually recorded.
const reaperVerdict = "runner process died"

// LegacySource returns what this database remembers about one source.
func (d *DB) LegacySource(k LegacyKey) (LegacySourceRow, error) {
	row := LegacySourceRow{Key: k}
	err := d.db.QueryRow(
		`SELECT state, first_imported_at, last_imported_at FROM legacy_sources
		 WHERE path = ? AND project_id = ? AND loop = ?`,
		k.Path, k.ProjectID, k.Loop,
	).Scan(&row.State, &row.FirstImported, &row.LastImported)
	if errors.Is(err, sql.ErrNoRows) {
		return row, nil
	}
	if err != nil {
		return row, fmt.Errorf("read legacy source %s: %w", k.Path, err)
	}
	row.ExistsInRecord = true
	return row, nil
}

// ClaimedBy returns the project that already imported this file's loop, or an
// empty string when none has.
func (d *DB) ClaimedBy(path, loop string) (string, error) {
	var projectID string
	err := d.db.QueryRow(
		`SELECT project_id FROM legacy_sources WHERE path = ? AND loop = ?`,
		path, loop).Scan(&projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the claim on %s: %w", path, err)
	}
	return projectID, nil
}

// ImportLegacy copies one loop's rows out of a legacy database, in one
// transaction, and records what it did.
//
// seal reports that nothing will write the source again. An unsealed source is
// read once more on a later command, because a runner from the old binary may
// still be recording an outcome in it.
//
// It returns the number of rows it wrote.
func (d *DB) ImportLegacy(k LegacyKey, data LegacyData, seal bool) (int, error) {
	claimant, err := d.ClaimedBy(k.Path, k.Loop)
	if err != nil {
		return 0, err
	}
	if claimant != "" && claimant != k.ProjectID {
		return 0, fmt.Errorf("%w: %s loop %q was imported by project %s",
			ErrSourceClaimed, k.Path, k.Loop, claimant)
	}

	prior, err := d.LegacySource(k)
	if err != nil {
		return 0, err
	}
	if prior.State == SourceSealed {
		return 0, nil // nothing here can change again
	}

	tx, err := d.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin import of %s: %w", k.Path, err)
	}
	defer func() { _ = tx.Rollback() }()

	var rows int
	switch {
	case k.Path == d.path:
		// The source IS this file. Its rows are already here, left without a
		// project by the schema upgrade. Stamp them instead of copying them.
		rows, err = stampInPlace(tx, k)
	case prior.ExistsInRecord:
		rows, err = refresh(tx, k, data)
	default:
		rows, err = importAll(tx, k, data)
	}
	if err != nil {
		return 0, err
	}

	state := SourceOpen
	if seal {
		state = SourceSealed
	}
	if err := recordSource(tx, k, state, prior); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit import of %s: %w", k.Path, err)
	}
	return rows, nil
}

// importAll copies every table. It runs once per source, on the first import.
func importAll(tx *sql.Tx, k LegacyKey, data LegacyData) (int, error) {
	var n int

	for _, st := range data.Issues {
		_, err := tx.Exec(`
			INSERT INTO issues (project_id, loop, repo, number, session_id, worktree_path,
			                    retry_count, last_retry_tick, needs_retry, session_started,
			                    parked, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(project_id, loop, repo, number) DO NOTHING`,
			k.ProjectID, k.Loop, st.Repo, st.Number, st.SessionID, st.WorktreePath,
			st.RetryCount, st.LastRetryTick, st.NeedsRetry, st.SessionStarted,
			st.Parked, st.UpdatedAt.UTC())
		if err != nil {
			return n, fmt.Errorf("import issue %d of %s: %w", st.Number, k.Path, err)
		}
		n++
	}

	for _, dp := range data.Dispatches {
		// legacy_source and legacy_id are what keep the row identifiable after
		// SQLite renumbers it, so a live runner is still recognised and its log
		// file is still found.
		//
		// The ON CONFLICT target repeats the index predicate. SQLite matches a
		// partial unique index only when the statement names it.
		_, err := tx.Exec(`
			INSERT INTO dispatches (project_id, loop, repo, number, kind, session_id,
			                        pid, pid_start_at, status, started_at, finished_at,
			                        exit_code, cost_usd, duration_ms, api_error, log_path,
			                        pr_number, title, legacy_source, legacy_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(legacy_source, legacy_id, project_id, loop)
			  WHERE legacy_source <> '' DO NOTHING`,
			k.ProjectID, k.Loop, dp.Repo, dp.Number, dp.Kind, dp.SessionID,
			dp.PID, nullTime(dp.PIDStartAt), dp.Status, dp.StartedAt.UTC(),
			nullTime(dp.FinishedAt), dp.ExitCode, dp.CostUSD, dp.DurationMS,
			dp.APIError, dp.LogPath, dp.PRNumber, dp.Title, k.Path, dp.ID)
		if err != nil {
			return n, fmt.Errorf("import dispatch %d of %s: %w", dp.ID, k.Path, err)
		}
		n++
	}

	for _, l := range data.PRLinks {
		_, err := tx.Exec(`
			INSERT INTO pr_links (project_id, loop, repo, number, pr_number, head_ref,
			                      base_ref, behind_by)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(project_id, loop, repo, number) DO NOTHING`,
			k.ProjectID, k.Loop, l.Repo, l.Number, l.PRNumber, l.HeadRef, l.BaseRef, l.BehindBy)
		if err != nil {
			return n, fmt.Errorf("import pr link %d of %s: %w", l.Number, k.Path, err)
		}
		n++
	}

	for _, tk := range data.Ticks {
		tripped := 0
		if tk.BreakerTripped {
			tripped = 1
		}
		_, err := tx.Exec(`
			INSERT INTO ticks (project_id, loop, started_at, breaker_tripped, summary_json)
			VALUES (?, ?, ?, ?, ?)`,
			k.ProjectID, k.Loop, tk.StartedAt.UTC(), tripped, tk.SummaryJSON)
		if err != nil {
			return n, fmt.Errorf("import a tick of %s: %w", k.Path, err)
		}
		n++
	}

	if c := data.Cooldown; c != nil {
		_, err := tx.Exec(`
			INSERT INTO cooldowns (project_id, loop, until) VALUES (?, ?, ?)
			ON CONFLICT(project_id, loop) DO NOTHING`,
			k.ProjectID, k.Loop, c.Until.UTC())
		if err != nil {
			return n, fmt.Errorf("import the cooldown of %s: %w", k.Path, err)
		}
		n++
	}
	return n, nil
}

// refresh re-reads a source that is still open.
//
// Only two tables can change in a source after the first import, because only
// two are written by a runner: dispatches and issues. A tick after the upgrade
// writes here, not there, so ticks, pr_links and cooldowns cannot hold anything
// newer and are left alone.
func refresh(tx *sql.Tx, k LegacyKey, data LegacyData) (int, error) {
	var n int

	for _, dp := range data.Dispatches {
		// An outcome lands on a row this database still calls running, or on one
		// the reaper retired on a guess. It must NOT overwrite a row that was
		// finished here, which is a fact and not a guess.
		res, err := tx.Exec(`
			UPDATE dispatches
			SET status = ?, exit_code = ?, cost_usd = ?, duration_ms = ?, api_error = ?,
			    finished_at = ?, pid = ?, pid_start_at = ?, session_id = ?
			WHERE legacy_source = ? AND legacy_id = ? AND project_id = ? AND loop = ?
			  AND (status = ? OR api_error = ?)`,
			dp.Status, dp.ExitCode, dp.CostUSD, dp.DurationMS, dp.APIError,
			nullTime(dp.FinishedAt), dp.PID, nullTime(dp.PIDStartAt), dp.SessionID,
			k.Path, dp.ID, k.ProjectID, k.Loop, StatusRunning, reaperVerdict)
		if err != nil {
			return n, fmt.Errorf("refresh dispatch %d of %s: %w", dp.ID, k.Path, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return n, fmt.Errorf("refresh dispatch %d of %s: %w", dp.ID, k.Path, err)
		}
		n += int(affected)
	}

	for _, st := range data.Issues {
		// Only the four columns the legacy runner writes. A whole-row copy would
		// drag the source's frozen session_id and worktree_path over values this
		// binary wrote after the import.
		//
		// The timestamp comparison is done in SQL. Every timestamp in this
		// database is written through time.Time.UTC(), which the driver stores as
		// text with a fixed "+0000 UTC" suffix, so a text comparison orders them
		// correctly. A row this binary touched more recently is left alone.
		res, err := tx.Exec(`
			UPDATE issues
			SET needs_retry = ?, session_started = ?, parked = ?, retry_count = ?,
			    updated_at = ?
			WHERE project_id = ? AND loop = ? AND repo = ? AND number = ?
			  AND updated_at < ?`, // see the note on time comparison below
			st.NeedsRetry, st.SessionStarted, st.Parked, st.RetryCount,
			st.UpdatedAt.UTC(), k.ProjectID, k.Loop, st.Repo, st.Number,
			st.UpdatedAt.UTC())
		if err != nil {
			return n, fmt.Errorf("refresh issue %d of %s: %w", st.Number, k.Path, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return n, fmt.Errorf("refresh issue %d of %s: %w", st.Number, k.Path, err)
		}
		n += int(affected)
	}
	return n, nil
}

// stampInPlace claims the rows of a source that IS this database.
//
// A loop whose state_dir is the home directory already wrote into this file. The
// schema upgrade carried those rows over with no project, so there is nothing to
// copy: they only need an owner. The loop guard is what keeps two loops that
// share the directory apart.
func stampInPlace(tx *sql.Tx, k LegacyKey) (int, error) {
	var n int
	for _, table := range []string{"issues", "dispatches", "pr_links", "ticks", "cooldowns"} {
		res, err := tx.Exec(fmt.Sprintf(
			`UPDATE %s SET project_id = ? WHERE project_id = '' AND loop = ?`, table),
			k.ProjectID, k.Loop)
		if err != nil {
			return n, fmt.Errorf("claim the %s rows of %s: %w", table, k.Path, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return n, fmt.Errorf("claim the %s rows of %s: %w", table, k.Path, err)
		}
		n += int(affected)
	}
	return n, nil
}

func recordSource(tx *sql.Tx, k LegacyKey, state string, prior LegacySourceRow) error {
	now := time.Now().UTC()
	first := now
	if prior.ExistsInRecord {
		first = prior.FirstImported
	}
	_, err := tx.Exec(`
		INSERT INTO legacy_sources (path, project_id, loop, repo, state,
		                            first_imported_at, last_imported_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path, project_id, loop) DO UPDATE SET
		  repo             = excluded.repo,
		  state            = excluded.state,
		  last_imported_at = excluded.last_imported_at`,
		k.Path, k.ProjectID, k.Loop, k.Repo, state, first, now)
	if err != nil {
		return fmt.Errorf("record legacy source %s: %w", k.Path, err)
	}
	return nil
}

// LegacySources returns every recorded source, for the report.
func (d *DB) LegacySources() ([]LegacySourceRow, error) {
	rows, err := d.db.Query(`
		SELECT path, project_id, loop, repo, state, first_imported_at, last_imported_at
		FROM legacy_sources ORDER BY path, loop`)
	if err != nil {
		return nil, fmt.Errorf("query legacy sources: %w", err)
	}
	defer rows.Close()

	var out []LegacySourceRow
	for rows.Next() {
		var r LegacySourceRow
		if err := rows.Scan(&r.Key.Path, &r.Key.ProjectID, &r.Key.Loop, &r.Key.Repo,
			&r.State, &r.FirstImported, &r.LastImported); err != nil {
			return nil, fmt.Errorf("scan legacy source: %w", err)
		}
		r.ExistsInRecord = true
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}
