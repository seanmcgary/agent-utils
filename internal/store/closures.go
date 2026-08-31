package store

import (
	"fmt"
	"time"
)

// Closure records that one issue or pull request is closed. See the closures
// table for why the key carries no loop.
type Closure struct {
	ProjectID string
	Repo      string
	Number    int
	ClosedAt  time.Time
}

// IssueRef names one issue in one project's repository. It carries no loop,
// matching Closure and for the same reason: two loops watching a repository ask
// GitHub the same question about the same number.
type IssueRef struct {
	ProjectID string
	Repo      string
	Number    int
}

// MarkClosed records that repo#number is closed.
//
// It is an upsert that leaves the original closed_at alone. A close delivery
// can arrive twice -- GitHub redelivers, and the startup reconcile re-checks
// issues the listener already marked -- and the first time the issue closed is
// the more useful of the two timestamps.
func (s *Store) MarkClosed(repo string, number int, now time.Time) error {
	_, err := s.db.Exec(`
		INSERT INTO closures (project_id, repo, number, closed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(project_id, repo, number) DO NOTHING`,
		s.projectID, repo, number, now.UTC())
	if err != nil {
		return fmt.Errorf("mark closed: %w", err)
	}
	return nil
}

// ClearClosed forgets that repo#number was closed, which is what a reopen
// means.
//
// It deletes rather than writing a "not closed" row, for the reason the table
// gives: an issue with no row and an issue known to be open are the same state
// to every reader, and both stay visible.
func (s *Store) ClearClosed(repo string, number int) error {
	_, err := s.db.Exec(
		`DELETE FROM closures WHERE project_id = ? AND repo = ? AND number = ?`,
		s.projectID, repo, number)
	if err != nil {
		return fmt.Errorf("clear closed: %w", err)
	}
	return nil
}

// Closures returns every closed issue on the machine, in every project. It is
// the machine-wide read the per-project view cannot answer -- the same reason
// DB.StoppedIssues exists.
func (d *DB) Closures() ([]Closure, error) {
	rows, err := d.db.Query(`
		SELECT project_id, repo, number, closed_at
		FROM closures ORDER BY project_id, repo, number`)
	if err != nil {
		return nil, fmt.Errorf("query closures: %w", err)
	}
	defer rows.Close()

	var out []Closure
	for rows.Next() {
		var c Closure
		if err := rows.Scan(&c.ProjectID, &c.Repo, &c.Number, &c.ClosedAt); err != nil {
			return nil, fmt.Errorf("scan closure: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// BelievedOpen returns every issue this machine has dispatched an agent for and
// does NOT know to be closed.
//
// It is the reconcile's candidate set, and it is deliberately the complement of
// the closures table rather than a list of everything ever dispatched: an issue
// already marked closed cannot become "more closed", so re-checking it would
// grow the startup cost of every restart forever. The price is that a REOPEN
// that happened while the daemon was down is not noticed here -- the live
// delivery is what clears a closure, and a reopened issue that gets any further
// work dispatched reappears anyway.
//
// number > 0 and repo <> ” drop the rows that name no issue. A tend dispatch
// can carry number 0, and asking GitHub about issue 0 is a guaranteed 404.
//
// One row per {project, repo, number}, however many loops and however many
// dispatches produced it: the reconcile asks GitHub about a NUMBER, and the
// answer does not depend on which loop dispatched the agent.
func (d *DB) BelievedOpen() ([]IssueRef, error) {
	rows, err := d.db.Query(`
		SELECT DISTINCT d.project_id, d.repo, d.number
		FROM dispatches d
		WHERE d.number > 0 AND d.repo <> ''
		  AND NOT EXISTS (
		    SELECT 1 FROM closures c
		    WHERE c.project_id = d.project_id AND c.repo = d.repo
		      AND c.number = d.number)
		ORDER BY d.project_id, d.repo, d.number`)
	if err != nil {
		return nil, fmt.Errorf("query believed-open issues: %w", err)
	}
	defer rows.Close()

	var out []IssueRef
	for rows.Next() {
		var r IssueRef
		if err := rows.Scan(&r.ProjectID, &r.Repo, &r.Number); err != nil {
			return nil, fmt.Errorf("scan believed-open issue: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
