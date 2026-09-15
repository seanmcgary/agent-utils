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
