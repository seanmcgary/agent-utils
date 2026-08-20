package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/settings"
)

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what
// was written. collectRepos' skip-a-broken-loop warning and
// registerWebhookRun's disabled-daemon warning both write directly to
// os.Stderr rather than through an injectable io.Writer (matching this
// program's existing prompt/warning convention elsewhere in cmd/agent-utils),
// so asserting on them needs this rather than a Deps field.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = old
	w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read captured stderr: %v", err)
	}
	return buf.String()
}

// fakeHookAdmin is the narrow fake register-webhook's tests use in place of a
// real GitHub client, per B1/D1 review note 4: ghub.HookAdmin is a separate,
// deliberately small interface exactly so a caller does not have to fake the
// whole of ghub.Client.
type fakeHookAdmin struct {
	// hooks is keyed "owner/repo", mirroring how the real API scopes hooks.
	hooks map[string][]ghub.Hook
	// nextID hands out ascending hook ids, distinct from a repository's index,
	// so a test can tell which call created which hook.
	nextID int64

	listCalls   int
	createCalls int
	editCalls   int
	deleteCalls int
}

func newFakeHookAdmin() *fakeHookAdmin {
	return &fakeHookAdmin{hooks: map[string][]ghub.Hook{}}
}

var _ ghub.HookAdmin = (*fakeHookAdmin)(nil)

func (f *fakeHookAdmin) ListHooks(_ context.Context, owner, repo string) ([]ghub.Hook, error) {
	f.listCalls++
	// Return a copy: the real API never hands back a slice the caller could
	// mutate to change what a later List sees.
	existing := f.hooks[owner+"/"+repo]
	out := make([]ghub.Hook, len(existing))
	copy(out, existing)
	return out, nil
}

func (f *fakeHookAdmin) CreateHook(_ context.Context, owner, repo string, h ghub.HookSpec) (int64, error) {
	f.createCalls++
	f.nextID++
	id := f.nextID
	key := owner + "/" + repo
	f.hooks[key] = append(f.hooks[key], ghub.Hook{ID: id, URL: h.URL, Events: h.Events, Active: true})
	return id, nil
}

func (f *fakeHookAdmin) EditHook(_ context.Context, owner, repo string, id int64, h ghub.HookSpec) error {
	f.editCalls++
	key := owner + "/" + repo
	for i := range f.hooks[key] {
		if f.hooks[key][i].ID == id {
			f.hooks[key][i].Events = h.Events
			f.hooks[key][i].Active = true
			return nil
		}
	}
	return errors.New("no such hook")
}

func (f *fakeHookAdmin) DeleteHook(_ context.Context, owner, repo string, id int64) error {
	f.deleteCalls++
	key := owner + "/" + repo
	for i := range f.hooks[key] {
		if f.hooks[key][i].ID == id {
			f.hooks[key] = append(f.hooks[key][:i], f.hooks[key][i+1:]...)
			return nil
		}
	}
	// The real client reports GitHub's 404 this way, and a caller tidying up a
	// hook that is already gone branches on it rather than on the message.
	return fmt.Errorf("hooks %s/%s: 404: %w", owner, repo, ghub.ErrHookNotFound)
}

// TestCollectReposDedupsAcrossLoops covers: "Two loops naming one repository
// produce one call" at the collection stage, before any GitHub call is made.
func TestCollectReposDedupsAcrossLoops(t *testing.T) {
	entries := []config.Entry{
		{Name: "planning", Repo: "acme/widgets"},
		{Name: "execution", Repo: "acme/widgets"},
	}
	repos := collectRepos(entries, "")
	if len(repos) != 1 || repos[0] != "acme/widgets" {
		t.Fatalf("collectRepos = %v, want exactly [acme/widgets]", repos)
	}
}

// TestCollectReposSkipsBrokenEntries covers step 2: "Skip a loop whose entry
// has a non-nil Err, and say so on stderr."
func TestCollectReposSkipsBrokenEntries(t *testing.T) {
	entries := []config.Entry{
		{Name: "broken", Err: errors.New("boom")},
		{Name: "ok", Repo: "acme/widgets"},
	}
	var repos []string
	stderr := captureStderr(t, func() {
		repos = collectRepos(entries, "")
	})
	if len(repos) != 1 || repos[0] != "acme/widgets" {
		t.Fatalf("collectRepos = %v, want exactly [acme/widgets]", repos)
	}
	if !strings.Contains(stderr, "broken") || !strings.Contains(stderr, "boom") {
		t.Errorf("stderr = %q, want it to name the skipped loop %q and its error", stderr, "broken")
	}
}

// TestCollectReposFiltersByName covers: "--name restricts to one loop."
func TestCollectReposFiltersByName(t *testing.T) {
	entries := []config.Entry{
		{Name: "planning", Repo: "acme/widgets"},
		{Name: "execution", Repo: "acme/other"},
	}
	repos := collectRepos(entries, "execution")
	if len(repos) != 1 || repos[0] != "acme/other" {
		t.Fatalf("collectRepos with --name execution = %v, want exactly [acme/other]", repos)
	}
}

// TestRegisterWebhookSecondRunEditsNotCreates covers: "A second run calls
// EditHook and not CreateHook."
func TestRegisterWebhookSecondRunEditsNotCreates(t *testing.T) {
	fake := newFakeHookAdmin()
	s := &settings.Settings{Webhook: settings.Webhook{
		Enabled: true, URL: "https://x/y", Secret: "sekrit",
	}}
	deps := registerWebhookDeps{Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: io.Discard}

	if err := registerWebhookRun(context.Background(), []string{"acme/widgets"}, deps); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if fake.createCalls != 1 || fake.editCalls != 0 {
		t.Fatalf("after first run: create=%d edit=%d, want create=1 edit=0", fake.createCalls, fake.editCalls)
	}

	if err := registerWebhookRun(context.Background(), []string{"acme/widgets"}, deps); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if fake.createCalls != 1 || fake.editCalls != 1 {
		t.Fatalf("after second run: create=%d edit=%d, want create=1 edit=1", fake.createCalls, fake.editCalls)
	}
}

// TestRegisterWebhookTwoLoopsOneRepoProducesOneCall covers the same
// acceptance line end to end, through registerWebhookRun rather than only
// collectRepos: "Two loops naming one repository produce one call."
func TestRegisterWebhookTwoLoopsOneRepoProducesOneCall(t *testing.T) {
	fake := newFakeHookAdmin()
	s := &settings.Settings{Webhook: settings.Webhook{
		Enabled: true, URL: "https://x/y", Secret: "sekrit",
	}}
	entries := []config.Entry{
		{Name: "planning", Repo: "acme/widgets"},
		{Name: "execution", Repo: "acme/widgets"},
	}
	repos := collectRepos(entries, "")
	deps := registerWebhookDeps{Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: io.Discard}

	if err := registerWebhookRun(context.Background(), repos, deps); err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1", fake.createCalls)
	}
}

// TestRegisterWebhookMissingURLFailsBeforeGitHubCall covers: "A missing
// webhook.url fails before any GitHub call."
func TestRegisterWebhookMissingURLFailsBeforeGitHubCall(t *testing.T) {
	fake := newFakeHookAdmin()
	s := &settings.Settings{} // webhook.url and webhook.secret both empty
	deps := registerWebhookDeps{Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: io.Discard}

	err := registerWebhookRun(context.Background(), []string{"acme/widgets"}, deps)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "config webhook --enable") {
		t.Errorf("error = %q, want it to name `agent-utils config webhook --enable --url <url>`", err.Error())
	}
	if fake.listCalls != 0 || fake.createCalls != 0 || fake.editCalls != 0 {
		t.Fatalf("GitHub was called: list=%d create=%d edit=%d, want all zero",
			fake.listCalls, fake.createCalls, fake.editCalls)
	}
}

// TestRegisterWebhookNonInteractiveWithoutYesFails covers: "A non-interactive
// run without --yes fails and names --yes, making no GitHub call." It also
// asserts Confirm is never invoked: "Prompt only when stdin is a terminal...
// A prompt in a cron job would hang forever."
func TestRegisterWebhookNonInteractiveWithoutYesFails(t *testing.T) {
	fake := newFakeHookAdmin()
	s := &settings.Settings{Webhook: settings.Webhook{URL: "https://x/y", Secret: "sekrit"}}
	deps := registerWebhookDeps{
		Hooks: fake, Settings: s, Token: "tok",
		Yes: false, Interactive: false,
		Confirm: func([]string) (bool, error) {
			t.Fatal("must not prompt when stdin is not a terminal")
			return false, nil
		},
		Out: io.Discard,
	}

	err := registerWebhookRun(context.Background(), []string{"acme/widgets"}, deps)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("error = %q, want it to name --yes", err.Error())
	}
	if !strings.Contains(err.Error(), "acme/widgets") {
		t.Errorf("error = %q, want it to list the repositories", err.Error())
	}
	if fake.listCalls != 0 || fake.createCalls != 0 || fake.editCalls != 0 {
		t.Fatalf("GitHub was called: list=%d create=%d edit=%d, want all zero",
			fake.listCalls, fake.createCalls, fake.editCalls)
	}
}

// TestRegisterWebhookInteractiveDeclinedMakesNoGitHubCall guards the other
// half of the confirmation: a "no" answer must not proceed.
func TestRegisterWebhookInteractiveDeclinedMakesNoGitHubCall(t *testing.T) {
	fake := newFakeHookAdmin()
	s := &settings.Settings{Webhook: settings.Webhook{URL: "https://x/y", Secret: "sekrit"}}
	deps := registerWebhookDeps{
		Hooks: fake, Settings: s, Token: "tok",
		Yes: false, Interactive: true,
		Confirm: func([]string) (bool, error) { return false, nil },
		Out:     io.Discard,
	}

	if err := registerWebhookRun(context.Background(), []string{"acme/widgets"}, deps); err == nil {
		t.Fatal("expected an error when the operator declines")
	}
	if fake.createCalls != 0 || fake.editCalls != 0 {
		t.Fatalf("GitHub was called after a decline: create=%d edit=%d", fake.createCalls, fake.editCalls)
	}
}

// TestRegisterWebhookMissingTokenFailsBeforeGitHubCall covers step 4: require
// GITHUB_TOKEN, before any GitHub call.
func TestRegisterWebhookMissingTokenFailsBeforeGitHubCall(t *testing.T) {
	fake := newFakeHookAdmin()
	s := &settings.Settings{Webhook: settings.Webhook{URL: "https://x/y", Secret: "sekrit"}}
	deps := registerWebhookDeps{Hooks: fake, Settings: s, Token: "", Yes: true, Out: io.Discard}

	if err := registerWebhookRun(context.Background(), []string{"acme/widgets"}, deps); err == nil {
		t.Fatal("expected an error")
	}
	if fake.listCalls != 0 || fake.createCalls != 0 || fake.editCalls != 0 {
		t.Fatalf("GitHub was called without a token: list=%d create=%d edit=%d",
			fake.listCalls, fake.createCalls, fake.editCalls)
	}
}

// TestRegisterWebhookWarnsWhenDisabled covers: "When webhook.enabled is
// false, do the work and warn that the listener will refuse to start."
func TestRegisterWebhookWarnsWhenDisabled(t *testing.T) {
	fake := newFakeHookAdmin()
	s := &settings.Settings{Webhook: settings.Webhook{
		Enabled: false, URL: "https://x/y", Secret: "sekrit",
	}}
	deps := registerWebhookDeps{Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: io.Discard}

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = registerWebhookRun(context.Background(), []string{"acme/widgets"}, deps)
	})
	if runErr != nil {
		t.Fatalf("run with webhook disabled: %v", runErr)
	}
	if fake.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1: disabled must still do the work", fake.createCalls)
	}
	if !strings.Contains(stderr, "webhook.enabled is false") {
		t.Errorf("stderr = %q, want the disabled-daemon warning", stderr)
	}
}

// TestNoReposErrNamesFlagOnlyWhenGiven covers MINOR 6 from the B2+D2 review:
// pointing an operator at --name only when they actually passed it, since
// with no --name at all the real cause is every loop configuration failing
// to load (already reported by collectRepos on stderr), not a bad flag.
func TestNoReposErrNamesFlagOnlyWhenGiven(t *testing.T) {
	err := noReposErr("planning")
	if !strings.Contains(err.Error(), "--name") || !strings.Contains(err.Error(), "planning") {
		t.Errorf("noReposErr(%q) = %q, want it to name --name and the loop", "planning", err.Error())
	}

	err = noReposErr("")
	if strings.Contains(err.Error(), "--name") {
		t.Errorf("noReposErr(\"\") = %q, want it not to mention --name: none was passed", err.Error())
	}
	if !strings.Contains(err.Error(), "every loop configuration failed to load") {
		t.Errorf("noReposErr(\"\") = %q, want it to say every loop failed to load", err.Error())
	}
}

// TestMissingWebhookFieldsNamesOnlyWhatIsEmpty covers MINOR 5: `config set
// webhook.enabled true` followed by `config set webhook.url <url>` sets a
// URL without ever minting a secret, so the error must name only
// webhook.secret in that case, not both fields.
func TestMissingWebhookFieldsNamesOnlyWhatIsEmpty(t *testing.T) {
	s := &settings.Settings{Webhook: settings.Webhook{URL: "https://x/y", Secret: ""}}
	deps := registerWebhookDeps{Hooks: newFakeHookAdmin(), Settings: s, Token: "tok", Yes: true, Out: io.Discard}

	err := registerWebhookRun(context.Background(), []string{"acme/widgets"}, deps)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "webhook.secret") {
		t.Errorf("error = %q, want it to name webhook.secret", err.Error())
	}
	if strings.Contains(err.Error(), "webhook.url") {
		t.Errorf("error = %q, want it NOT to name webhook.url, which was set", err.Error())
	}
}
