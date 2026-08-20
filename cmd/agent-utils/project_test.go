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
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
	"github.com/seanmcgary/agent-utils/internal/registry"
	"github.com/seanmcgary/agent-utils/internal/settings"
	"github.com/seanmcgary/agent-utils/internal/store"
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
	// deletedIDs records what DeleteHook was asked to remove, which is the
	// assertion that matters after webhook.url changes: the RECORDED id must
	// be deleted, never one rediscovered by matching the new URL.
	deletedIDs []int64
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
			f.hooks[key][i].URL = h.URL
			f.hooks[key][i].Active = true
			return nil
		}
	}
	return errors.New("no such hook")
}

func (f *fakeHookAdmin) DeleteHook(_ context.Context, owner, repo string, id int64) error {
	f.deleteCalls++
	f.deletedIDs = append(f.deletedIDs, id)
	key := owner + "/" + repo
	for i := range f.hooks[key] {
		if f.hooks[key][i].ID == id {
			f.hooks[key] = append(f.hooks[key][:i], f.hooks[key][i+1:]...)
			return nil
		}
	}
	// The real client reports GitHub's 404 this way, and deregister-webhook
	// branches on it: a hook an operator already deleted in the UI must read
	// as "already done", not as a failure.
	return fmt.Errorf("hooks %s/%s: 404: %w", owner, repo, ghub.ErrHookNotFound)
}

// fakeWebhookRecords stands in for the canonical state database. It is a map
// rather than a real store so a cmd test needs no $AGENT_UTILS_HOME and no
// sqlite file, matching how fakeHookAdmin stands in for GitHub.
type fakeWebhookRecords struct {
	rows map[string]store.Webhook
	// others is what OtherHolders reports, keyed "repo#hookID". It models rows
	// belonging to OTHER projects, which this project's scoped view can never
	// hold itself.
	others map[string][]store.Webhook
	// putErr fails the recording write, so a test can prove a hook registered
	// at GitHub but unrecorded here is reported rather than silently accepted.
	putErr error

	putCalls    int
	deleteCalls int
}

func newFakeWebhookRecords() *fakeWebhookRecords {
	return &fakeWebhookRecords{rows: map[string]store.Webhook{}, others: map[string][]store.Webhook{}}
}

var _ webhookRecords = (*fakeWebhookRecords)(nil)

func (f *fakeWebhookRecords) PutWebhook(w store.Webhook) error {
	f.putCalls++
	if f.putErr != nil {
		return f.putErr
	}
	f.rows[w.Repo] = w
	return nil
}

func (f *fakeWebhookRecords) Webhook(repo string) (store.Webhook, bool, error) {
	w, ok := f.rows[repo]
	return w, ok, nil
}

func (f *fakeWebhookRecords) DeleteWebhook(repo string) error {
	f.deleteCalls++
	delete(f.rows, repo)
	return nil
}

func (f *fakeWebhookRecords) OtherHolders(repo string, hookID int64) ([]store.Webhook, error) {
	return f.others[fmt.Sprintf("%s#%d", repo, hookID)], nil
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
	deps := registerWebhookDeps{Records: newFakeWebhookRecords(), Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: io.Discard}

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
	deps := registerWebhookDeps{Records: newFakeWebhookRecords(), Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: io.Discard}

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
	deps := registerWebhookDeps{Records: newFakeWebhookRecords(), Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: io.Discard}

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
		Records: newFakeWebhookRecords(),
		Hooks:   fake, Settings: s, Token: "tok",
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
		Records: newFakeWebhookRecords(),
		Hooks:   fake, Settings: s, Token: "tok",
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
	deps := registerWebhookDeps{Records: newFakeWebhookRecords(), Hooks: fake, Settings: s, Token: "", Yes: true, Out: io.Discard}

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
	deps := registerWebhookDeps{Records: newFakeWebhookRecords(), Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: io.Discard}

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
	deps := registerWebhookDeps{Records: newFakeWebhookRecords(), Hooks: newFakeHookAdmin(), Settings: s, Token: "tok", Yes: true, Out: io.Discard}

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

// TestRegisterWebhookRecordsTheHook covers the whole point of recording: before
// this, registration left nothing on this machine naming the hook it created,
// so a later webhook.url change orphaned it with no way to find it again.
func TestRegisterWebhookRecordsTheHook(t *testing.T) {
	fake := newFakeHookAdmin()
	records := newFakeWebhookRecords()
	s := &settings.Settings{Webhook: settings.Webhook{
		Enabled: true, URL: "https://x/y", Secret: "sekrit",
	}}
	deps := registerWebhookDeps{Records: records, Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: io.Discard}

	if err := registerWebhookRun(context.Background(), []string{"acme/widgets"}, deps); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, ok, _ := records.Webhook("acme/widgets")
	if !ok {
		t.Fatal("nothing was recorded for acme/widgets after a successful registration")
	}
	if got.HookID != 1 {
		t.Errorf("HookID = %d, want the id GitHub returned (1)", got.HookID)
	}
	if got.URL != "https://x/y" {
		t.Errorf("URL = %q, want the URL it was registered with", got.URL)
	}
	if got.RegisteredAt.IsZero() {
		t.Error("RegisteredAt is zero; the row must record when registration happened")
	}

	// A second run edits rather than creates, and must still leave the row
	// current: an EditHook that pushed a rotated secret but left a stale row
	// would record a registration that no longer describes the live hook.
	s.Webhook.URL = "https://moved/z"
	deps.Settings = s
	if err := registerWebhookRun(context.Background(), []string{"acme/widgets"}, deps); err != nil {
		t.Fatalf("second run: %v", err)
	}
	got, _, _ = records.Webhook("acme/widgets")
	if got.HookID != 1 || got.URL != "https://moved/z" {
		t.Errorf("after re-registering: %+v, want hook 1 recorded at the new URL", got)
	}
}

// TestRegisterWebhookRecordsNothingWhenGitHubFails covers: "A failed GitHub
// call must record nothing." A row written ahead of the API call would name a
// hook that does not exist, and the next deregistration would try to delete it.
func TestRegisterWebhookRecordsNothingWhenGitHubFails(t *testing.T) {
	fake := &failingHookAdmin{fakeHookAdmin: newFakeHookAdmin()}
	records := newFakeWebhookRecords()
	s := &settings.Settings{Webhook: settings.Webhook{
		Enabled: true, URL: "https://x/y", Secret: "sekrit",
	}}
	deps := registerWebhookDeps{Records: records, Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: io.Discard}

	if err := registerWebhookRun(context.Background(), []string{"acme/widgets"}, deps); err == nil {
		t.Fatal("expected the GitHub failure to surface")
	}
	if records.putCalls != 0 {
		t.Errorf("putCalls = %d, want 0: a failed registration must record nothing", records.putCalls)
	}
	if _, ok, _ := records.Webhook("acme/widgets"); ok {
		t.Error("a row was recorded for a registration GitHub refused")
	}
}

// failingHookAdmin fails the create, leaving list and edit alone, so a test can
// separate "GitHub refused" from "nothing was called".
type failingHookAdmin struct {
	*fakeHookAdmin
}

func (f *failingHookAdmin) CreateHook(_ context.Context, _, _ string, _ ghub.HookSpec) (int64, error) {
	f.createCalls++
	return 0, errors.New("github said no")
}

// TestDeregisterWebhookDeletesTheRecordedID is the acceptance test for the
// failure this feature exists to fix: after webhook.url changed, the live hook
// points at the OLD endpoint, so a URL match finds nothing and the orphan
// survives. Deleting by the recorded id still removes it.
func TestDeregisterWebhookDeletesTheRecordedID(t *testing.T) {
	fake := newFakeHookAdmin()
	fake.hooks["acme/widgets"] = []ghub.Hook{{ID: 77, URL: "https://old/hook", Active: true}}
	records := newFakeWebhookRecords()
	records.rows["acme/widgets"] = store.Webhook{
		Repo: "acme/widgets", HookID: 77, URL: "https://old/hook", RegisteredAt: time.Now(),
	}
	s := &settings.Settings{Webhook: settings.Webhook{Enabled: true, URL: "https://new/hook", Secret: "sekrit"}}

	var out bytes.Buffer
	err := deregisterWebhookRun(context.Background(), []string{"acme/widgets"}, deregisterWebhookDeps{
		Records: records, Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: &out,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(fake.deletedIDs) != 1 || fake.deletedIDs[0] != 77 {
		t.Fatalf("deletedIDs = %v, want [77]: the RECORDED id, not one matched by the new URL", fake.deletedIDs)
	}
	if fake.listCalls != 0 {
		t.Errorf("listCalls = %d, want 0: a recorded hook needs no URL search", fake.listCalls)
	}
	if _, ok, _ := records.Webhook("acme/widgets"); ok {
		t.Error("the row survived a confirmed delete")
	}
	if !strings.Contains(out.String(), "77") {
		t.Errorf("output = %q, want it to name the hook it deleted", out.String())
	}
}

// TestDeregisterWebhookRemovesTheRowOnlyAfterGitHubConfirms covers the ordering
// rule: a delete GitHub refused for any reason other than "already gone" must
// leave the record in place, or the operator loses the id and the hook keeps
// delivering with nothing naming it.
func TestDeregisterWebhookRemovesTheRowOnlyAfterGitHubConfirms(t *testing.T) {
	fake := &refusingHookAdmin{fakeHookAdmin: newFakeHookAdmin()}
	records := newFakeWebhookRecords()
	records.rows["acme/widgets"] = store.Webhook{Repo: "acme/widgets", HookID: 77, URL: "https://x/y"}
	s := &settings.Settings{Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: "sekrit"}}

	err := deregisterWebhookRun(context.Background(), []string{"acme/widgets"}, deregisterWebhookDeps{
		Records: records, Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: io.Discard,
	})
	if err == nil {
		t.Fatal("expected the GitHub failure to surface")
	}
	if records.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0: the row must outlive a failed delete", records.deleteCalls)
	}
}

// refusingHookAdmin fails DeleteHook with something that is NOT a 404, which
// must never be read as "the hook is already gone".
type refusingHookAdmin struct {
	*fakeHookAdmin
}

func (f *refusingHookAdmin) DeleteHook(_ context.Context, _, _ string, id int64) error {
	f.deleteCalls++
	f.deletedIDs = append(f.deletedIDs, id)
	return errors.New("500 from github")
}

// TestDeregisterWebhookTreatsAMissingHookAsDone covers: a recorded hook that is
// already gone at GitHub is a success. The operator deleted it in the UI and is
// tidying up; failing would leave a row nothing on this machine can clear.
func TestDeregisterWebhookTreatsAMissingHookAsDone(t *testing.T) {
	fake := newFakeHookAdmin() // holds no hooks at all, so DeleteHook 404s
	records := newFakeWebhookRecords()
	records.rows["acme/widgets"] = store.Webhook{Repo: "acme/widgets", HookID: 77, URL: "https://x/y"}
	s := &settings.Settings{Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: "sekrit"}}

	var out bytes.Buffer
	err := deregisterWebhookRun(context.Background(), []string{"acme/widgets"}, deregisterWebhookDeps{
		Records: records, Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: &out,
	})
	if err != nil {
		t.Fatalf("a hook already gone at GitHub must exit zero, got: %v", err)
	}
	if _, ok, _ := records.Webhook("acme/widgets"); ok {
		t.Error("the stale row survived; nothing else can ever clear it")
	}
	if !strings.Contains(out.String(), "already") {
		t.Errorf("output = %q, want it to say the hook was already gone", out.String())
	}
}

// TestDeregisterWebhookFallsBackToTheURL covers a repository registered before
// this table existed: with no row, the only handle left is the delivery URL.
func TestDeregisterWebhookFallsBackToTheURL(t *testing.T) {
	fake := newFakeHookAdmin()
	fake.hooks["acme/widgets"] = []ghub.Hook{
		{ID: 5, URL: "https://someone-else/hook", Active: true},
		{ID: 9, URL: "https://x/y", Active: true},
	}
	records := newFakeWebhookRecords() // no rows at all
	s := &settings.Settings{Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: "sekrit"}}

	var out bytes.Buffer
	err := deregisterWebhookRun(context.Background(), []string{"acme/widgets"}, deregisterWebhookDeps{
		Records: records, Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: &out,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(fake.deletedIDs) != 1 || fake.deletedIDs[0] != 9 {
		t.Fatalf("deletedIDs = %v, want [9]: the hook matching webhook.url", fake.deletedIDs)
	}
	if !strings.Contains(out.String(), "webhook.url") {
		t.Errorf("output = %q, want it to say the hook was found by matching webhook.url", out.String())
	}
}

// TestDeregisterWebhookWithNothingToDoExitsZero: no record and no hook at
// GitHub is the tidy state this command is trying to reach, not a failure.
func TestDeregisterWebhookWithNothingToDoExitsZero(t *testing.T) {
	fake := newFakeHookAdmin()
	s := &settings.Settings{Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: "sekrit"}}

	var out bytes.Buffer
	err := deregisterWebhookRun(context.Background(), []string{"acme/widgets"}, deregisterWebhookDeps{
		Records: newFakeWebhookRecords(), Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: &out,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if fake.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0", fake.deleteCalls)
	}
	if !strings.Contains(out.String(), "nothing to deregister") {
		t.Errorf("output = %q, want it to say there was nothing to deregister", out.String())
	}
}

// TestDeregisterWebhookRefusesASharedHook covers the shared-hook hazard: two
// projects watching one repository through one webhook.url end up recording the
// SAME hook id, and deleting it on behalf of one silently stops deliveries for
// the other. Refusing and naming them mirrors registry.Find's ambiguity rule.
func TestDeregisterWebhookRefusesASharedHook(t *testing.T) {
	t.Setenv("AGENT_UTILS_HOME", t.TempDir())
	const otherID = "22222222-2222-2222-2222-222222222222"
	if err := registry.Register("/work/other-project/.agent-utils", otherID, "other-project"); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	fake := newFakeHookAdmin()
	fake.hooks["acme/widgets"] = []ghub.Hook{{ID: 77, URL: "https://x/y", Active: true}}
	records := newFakeWebhookRecords()
	records.rows["acme/widgets"] = store.Webhook{Repo: "acme/widgets", HookID: 77, URL: "https://x/y"}
	records.others["acme/widgets#77"] = []store.Webhook{
		{ProjectID: otherID, Repo: "acme/widgets", HookID: 77, URL: "https://x/y"},
	}
	s := &settings.Settings{Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: "sekrit"}}
	deps := deregisterWebhookDeps{
		Records: records, Hooks: fake, Settings: s, Token: "tok", Yes: true, Out: io.Discard,
	}

	err := deregisterWebhookRun(context.Background(), []string{"acme/widgets"}, deps)
	if err == nil {
		t.Fatal("expected a refusal: another project records the same hook")
	}
	for _, want := range []string{"other-project", "/work/other-project", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err.Error(), want)
		}
	}
	if fake.deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0: the refusal must come before the delete", fake.deleteCalls)
	}
	if records.deleteCalls != 0 {
		t.Errorf("the row was cleared despite the refusal")
	}

	// --force overrides, and must say plainly who just lost delivery.
	deps.Force = true
	var out bytes.Buffer
	deps.Out = &out
	if err := deregisterWebhookRun(context.Background(), []string{"acme/widgets"}, deps); err != nil {
		t.Fatalf("--force run: %v", err)
	}
	if len(fake.deletedIDs) != 1 || fake.deletedIDs[0] != 77 {
		t.Fatalf("deletedIDs = %v, want [77] once --force is passed", fake.deletedIDs)
	}
	if !strings.Contains(out.String(), "other-project") {
		t.Errorf("output = %q, want the forced delete to name the project that lost delivery", out.String())
	}
}

// TestDeregisterWebhookNonInteractiveWithoutYesFails mirrors register-webhook's
// own rule: a prompt in a cron job would hang forever, and this command deletes
// state at GitHub, so nothing may be called before the operator has agreed.
func TestDeregisterWebhookNonInteractiveWithoutYesFails(t *testing.T) {
	fake := newFakeHookAdmin()
	records := newFakeWebhookRecords()
	records.rows["acme/widgets"] = store.Webhook{Repo: "acme/widgets", HookID: 77, URL: "https://x/y"}
	s := &settings.Settings{Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: "sekrit"}}

	err := deregisterWebhookRun(context.Background(), []string{"acme/widgets"}, deregisterWebhookDeps{
		Records: records, Hooks: fake, Settings: s, Token: "tok",
		Yes: false, Interactive: false,
		Confirm: func([]string) (bool, error) {
			t.Fatal("must not prompt when stdin is not a terminal")
			return false, nil
		},
		Out: io.Discard,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Errorf("error = %q, want it to name --yes", err.Error())
	}
	if !strings.Contains(err.Error(), "acme/widgets") {
		t.Errorf("error = %q, want it to list the repositories", err.Error())
	}
	if fake.listCalls != 0 || fake.deleteCalls != 0 {
		t.Fatalf("GitHub was called: list=%d delete=%d, want all zero", fake.listCalls, fake.deleteCalls)
	}
	if records.deleteCalls != 0 {
		t.Fatalf("the record was cleared without confirmation")
	}
}

// TestDeregisterWebhookMissingTokenFailsBeforeGitHubCall mirrors
// register-webhook: without a token every call would fail anyway, and failing
// first keeps a half-deregistered set of repositories out of the picture.
func TestDeregisterWebhookMissingTokenFailsBeforeGitHubCall(t *testing.T) {
	fake := newFakeHookAdmin()
	s := &settings.Settings{Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: "sekrit"}}

	err := deregisterWebhookRun(context.Background(), []string{"acme/widgets"}, deregisterWebhookDeps{
		Records: newFakeWebhookRecords(), Hooks: fake, Settings: s, Token: "", Yes: true, Out: io.Discard,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if fake.listCalls != 0 || fake.deleteCalls != 0 {
		t.Fatalf("GitHub was called without a token: list=%d delete=%d", fake.listCalls, fake.deleteCalls)
	}
}
