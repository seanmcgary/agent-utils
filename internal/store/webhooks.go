package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// webhookColumns is the select list every webhook read shares, in the order
// scanWebhook expects.
const webhookColumns = `project_id, repo, hook_id, url, registered_at`

// PutWebhook records that this project registered a webhook on a repository.
//
// It is an upsert on (project_id, repo) rather than an insert: re-running
// register-webhook after `config set webhook.url` must REPLACE what is
// recorded. Appending instead would leave the row that named the previous
// endpoint sitting beside the new one, and deregistration would then have two
// hook ids for one repository and no way to tell which is live.
//
// Callers must call it only after GitHub confirms. A row written ahead of the
// API call would survive a failed create, and the next deregistration would
// try to delete a hook that never existed.
func (s *Store) PutWebhook(w Webhook) error {
	if w.RegisteredAt.IsZero() {
		w.RegisteredAt = time.Now().UTC()
	}
	_, err := s.db.Exec(`
		INSERT INTO webhooks (project_id, repo, hook_id, url, registered_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(project_id, repo) DO UPDATE SET
		  hook_id       = excluded.hook_id,
		  url           = excluded.url,
		  registered_at = excluded.registered_at`,
		s.projectID, w.Repo, w.HookID, w.URL, w.RegisteredAt.UTC())
	if err != nil {
		return fmt.Errorf("put webhook for %s: %w", w.Repo, err)
	}
	return nil
}

// Webhook returns this project's recorded registration for one repository.
//
// A missing row is reported by the boolean, not by an error: a repository that
// was never registered, and one registered before this table existed, are both
// ordinary states that deregistration handles by falling back to a URL match.
func (s *Store) Webhook(repo string) (Webhook, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+webhookColumns+` FROM webhooks WHERE project_id = ? AND repo = ?`,
		s.projectID, repo)
	w, err := scanWebhook(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Webhook{}, false, nil
	}
	if err != nil {
		return Webhook{}, false, fmt.Errorf("read webhook for %s: %w", repo, err)
	}
	return w, true, nil
}

// Webhooks returns every registration this project has recorded, so `project
// status` can report the recorded state of each repository in one read.
func (s *Store) Webhooks() ([]Webhook, error) {
	rows, err := s.db.Query(
		`SELECT `+webhookColumns+` FROM webhooks WHERE project_id = ? ORDER BY repo`,
		s.projectID)
	if err != nil {
		return nil, fmt.Errorf("query webhooks: %w", err)
	}
	return scanWebhooks(rows)
}

// DeleteWebhook forgets this project's registration for one repository.
//
// Deleting a row that is not there is not an error: deregistration reaches
// here after GitHub has confirmed the hook is gone, and refusing at that point
// would report a failure for work that is already done.
func (s *Store) DeleteWebhook(repo string) error {
	_, err := s.db.Exec(
		`DELETE FROM webhooks WHERE project_id = ? AND repo = ?`, s.projectID, repo)
	if err != nil {
		return fmt.Errorf("delete webhook for %s: %w", repo, err)
	}
	return nil
}

// WebhooksForHook returns every project that records one repository's hook.
//
// It is deliberately machine-wide, which is why it hangs off DB and not the
// project-scoped Store. Two projects can watch the same repository with the
// same webhook.url: registering from the first creates the hook, registering
// from the second FINDS it by URL and edits it, and both then record the same
// id. Deleting it on behalf of one silently stops deliveries for the other, so
// deregistration has to be able to see across the project boundary before it
// decides.
//
// The hook id is matched together with the repository because GitHub numbers
// hooks per repository: id 123 on one repository is unrelated to id 123 on
// another.
func (d *DB) WebhooksForHook(repo string, hookID int64) ([]Webhook, error) {
	rows, err := d.db.Query(
		`SELECT `+webhookColumns+` FROM webhooks
		 WHERE repo = ? AND hook_id = ? ORDER BY project_id`, repo, hookID)
	if err != nil {
		return nil, fmt.Errorf("query webhook holders: %w", err)
	}
	return scanWebhooks(rows)
}

// scanner is what scanWebhook needs: a *sql.Row here, or a *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanWebhook(s scanner) (Webhook, error) {
	var w Webhook
	if err := s.Scan(&w.ProjectID, &w.Repo, &w.HookID, &w.URL, &w.RegisteredAt); err != nil {
		return Webhook{}, err
	}
	w.RegisteredAt = w.RegisteredAt.UTC()
	return w, nil
}

func scanWebhooks(rows *sql.Rows) ([]Webhook, error) {
	defer rows.Close()
	var out []Webhook
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, fmt.Errorf("scan webhook: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
