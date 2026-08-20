package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPutWebhookRoundTrip(t *testing.T) {
	s := openTemp(t)
	when := time.Now().UTC().Truncate(time.Second)
	if err := s.PutWebhook(Webhook{
		Repo: "o/r", HookID: 123, URL: "https://hooks.example/webhook", RegisteredAt: when,
	}); err != nil {
		t.Fatalf("PutWebhook: %v", err)
	}

	got, ok, err := s.Webhook("o/r")
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	if !ok {
		t.Fatal("Webhook reported no row for a repository just registered")
	}
	if got.ProjectID != testProject || got.Repo != "o/r" || got.HookID != 123 ||
		got.URL != "https://hooks.example/webhook" || !got.RegisteredAt.Equal(when) {
		t.Errorf("round trip mismatch: %+v", got)
	}
}

// TestPutWebhookIsUpsert is the whole reason the row is keyed by
// (project_id, repo): re-registering after `config set webhook.url` must
// REPLACE what is recorded, not leave a second row naming the dead endpoint.
func TestPutWebhookIsUpsert(t *testing.T) {
	s := openTemp(t)
	first := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := s.PutWebhook(Webhook{Repo: "o/r", HookID: 1, URL: "https://old/hook", RegisteredAt: first}); err != nil {
		t.Fatalf("first PutWebhook: %v", err)
	}
	second := time.Now().UTC().Truncate(time.Second)
	if err := s.PutWebhook(Webhook{Repo: "o/r", HookID: 2, URL: "https://new/hook", RegisteredAt: second}); err != nil {
		t.Fatalf("second PutWebhook: %v", err)
	}

	all, err := s.Webhooks()
	if err != nil {
		t.Fatalf("Webhooks: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("len = %d, want 1: a re-registration must replace the row, not add one", len(all))
	}
	if all[0].HookID != 2 || all[0].URL != "https://new/hook" || !all[0].RegisteredAt.Equal(second) {
		t.Errorf("row was not replaced: %+v", all[0])
	}
}

func TestWebhookMissingIsNotAnError(t *testing.T) {
	s := openTemp(t)
	_, ok, err := s.Webhook("o/never-registered")
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	if ok {
		t.Error("Webhook reported a row for a repository that was never registered")
	}
}

func TestDeleteWebhookRemovesOnlyThatRepo(t *testing.T) {
	s := openTemp(t)
	for _, repo := range []string{"o/one", "o/two"} {
		if err := s.PutWebhook(Webhook{Repo: repo, HookID: 7, URL: "https://x/y", RegisteredAt: time.Now()}); err != nil {
			t.Fatalf("PutWebhook %s: %v", repo, err)
		}
	}
	if err := s.DeleteWebhook("o/one"); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}

	if _, ok, err := s.Webhook("o/one"); err != nil || ok {
		t.Errorf("Webhook(o/one) = ok %v, err %v; want the row gone", ok, err)
	}
	if _, ok, err := s.Webhook("o/two"); err != nil || !ok {
		t.Errorf("Webhook(o/two) = ok %v, err %v; want it untouched", ok, err)
	}
}

// TestWebhookRowsAreScopedToTheProject guards the invariant every other table
// in this database holds: one file holds every project, and a scoped read must
// not see another project's rows.
func TestWebhookRowsAreScopedToTheProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.Project(otherProject).PutWebhook(Webhook{
		Repo: "o/r", HookID: 123, URL: "https://x/y", RegisteredAt: time.Now(),
	}); err != nil {
		t.Fatalf("PutWebhook: %v", err)
	}

	if _, ok, err := db.Project(testProject).Webhook("o/r"); err != nil || ok {
		t.Errorf("a scoped read saw another project's row: ok %v, err %v", ok, err)
	}
	all, err := db.Project(testProject).Webhooks()
	if err != nil {
		t.Fatalf("Webhooks: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("Webhooks = %+v, want none: the rows belong to another project", all)
	}
}

// TestWebhooksForHookNamesEveryHolder covers the shared-hook hazard:
// deregistering from one project must be able to discover that another project
// records the same hook, because deleting it at GitHub stops that project's
// deliveries too.
func TestWebhooksForHookNamesEveryHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	for _, id := range []string{testProject, otherProject} {
		if err := db.Project(id).PutWebhook(Webhook{
			Repo: "o/r", HookID: 123, URL: "https://x/y", RegisteredAt: time.Now(),
		}); err != nil {
			t.Fatalf("PutWebhook for %s: %v", id, err)
		}
	}
	// A different hook on the same repository, and the same hook id on a
	// different repository: neither is the same hook, and neither may be
	// reported as sharing one. GitHub's hook ids are per repository.
	if err := db.Project(otherProject).PutWebhook(Webhook{
		Repo: "o/elsewhere", HookID: 123, URL: "https://x/y", RegisteredAt: time.Now(),
	}); err != nil {
		t.Fatalf("PutWebhook for the decoy repository: %v", err)
	}

	holders, err := db.WebhooksForHook("o/r", 123)
	if err != nil {
		t.Fatalf("WebhooksForHook: %v", err)
	}
	if len(holders) != 2 {
		t.Fatalf("WebhooksForHook = %+v, want both projects", holders)
	}
	seen := map[string]bool{}
	for _, h := range holders {
		seen[h.ProjectID] = true
		if h.Repo != "o/r" || h.HookID != 123 {
			t.Errorf("holder %+v is not the hook that was asked for", h)
		}
	}
	if !seen[testProject] || !seen[otherProject] {
		t.Errorf("holders = %+v, want one row per project", holders)
	}
}
