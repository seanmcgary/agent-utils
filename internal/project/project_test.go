package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mkDir(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name, ".agent-utils")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestEnsureNamedCreatesADescriptorNamedAfterTheBase covers what Ensure used
// to cover for the directory-derived path (Ensure was deleted: it was a thin
// wrapper around EnsureNamed with the directory's own slugged basename as
// base, and after F5 removed its only production caller -- loopcmd's former
// ResolveProject -- EnsureNamed had exactly one remaining caller,
// mintProjectDescriptor, which always computes its own base first).
func TestEnsureNamedCreatesADescriptorNamedAfterTheBase(t *testing.T) {
	dir := mkDir(t, t.TempDir(), "lawndominator")

	c, created, _, err := EnsureNamed(dir, "lawndominator", func(string) bool { return false })
	if err != nil {
		t.Fatalf("EnsureNamed: %v", err)
	}
	if !created {
		t.Error("created = false on a fresh project")
	}
	if c.Name != "lawndominator" {
		t.Errorf("Name = %q, want the given base", c.Name)
	}
	if c.ID == "" {
		t.Error("ID must be minted")
	}

	// A second call must load, not re-create: the id has to be stable.
	again, created, _, err := EnsureNamed(dir, "lawndominator", func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Error("created = true on an existing project")
	}
	if again.ID != c.ID {
		t.Errorf("id changed across calls: %q -> %q", c.ID, again.ID)
	}
}

// Two projects can easily share a base name. The name is the human handle
// and must be unique, so the second one gets a suffix.
func TestEnsureNamedUniquifiesATakenName(t *testing.T) {
	dir := mkDir(t, t.TempDir(), "web")

	c, _, _, err := EnsureNamed(dir, "web", func(n string) bool { return n == "web" || n == "web-2" })
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "web-3" {
		t.Errorf("Name = %q, want web-3 when web and web-2 are taken", c.Name)
	}
}

func TestEnsureNamedSlugsAnAwkwardBase(t *testing.T) {
	dir := mkDir(t, t.TempDir(), "x")

	c, _, _, err := EnsureNamed(dir, Slug("My Project (v2)!"), func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	// A name is typed on a command line, so it must need no quoting.
	for _, bad := range []string{" ", "(", ")", "!"} {
		if contains(c.Name, bad) {
			t.Errorf("Name = %q, must not contain %q", c.Name, bad)
		}
	}
	if c.Name == "" {
		t.Error("Name must not be empty")
	}
}

func TestLoadRejectsADescriptorMissingItsFields(t *testing.T) {
	dir := mkDir(t, t.TempDir(), "x")
	if err := os.WriteFile(Path(dir), []byte("name: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("a descriptor with no id must be rejected")
	}
}

func TestLoadReportsAMissingDescriptor(t *testing.T) {
	dir := mkDir(t, t.TempDir(), "x")
	_, err := Load(dir)
	if !errors.Is(err, ErrNoConfig) {
		t.Fatalf("err = %v, want ErrNoConfig", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestEnsureNamedUsesAnExplicitBaseAndReportsARename covers the entry point
// `project init <name>` needs (EnsureNamed cannot be reached through Ensure,
// which always derives its base from the directory): an explicit base name
// is used verbatim when free, and a taken one is suffixed with renamedFrom
// naming what was actually asked for.
func TestEnsureNamedUsesAnExplicitBaseAndReportsARename(t *testing.T) {
	dir := mkDir(t, t.TempDir(), "irrelevant-directory-name")

	c, created, renamedFrom, err := EnsureNamed(dir, "web", func(string) bool { return false })
	if err != nil {
		t.Fatalf("EnsureNamed: %v", err)
	}
	if !created {
		t.Error("created = false on a fresh project")
	}
	if c.Name != "web" {
		t.Errorf("Name = %q, want the explicit base %q, not the directory name", c.Name, "web")
	}
	if renamedFrom != "" {
		t.Errorf("renamedFrom = %q, want empty: the base name was free", renamedFrom)
	}
}

func TestEnsureNamedUniquifiesATakenExplicitBase(t *testing.T) {
	dir := mkDir(t, t.TempDir(), "irrelevant-directory-name")

	c, created, renamedFrom, err := EnsureNamed(dir, "web",
		func(n string) bool { return n == "web" })
	if err != nil {
		t.Fatalf("EnsureNamed: %v", err)
	}
	if !created {
		t.Error("created = false on a fresh project")
	}
	if c.Name != "web-2" {
		t.Errorf("Name = %q, want web-2 when web is taken", c.Name)
	}
	if renamedFrom != "web" {
		t.Errorf("renamedFrom = %q, want %q", renamedFrom, "web")
	}
}

// TestEnsureNamedOnAnExistingProjectIgnoresBaseAndReportsNoRename covers the
// idempotent re-run `project init` relies on: base is not even consulted
// once a descriptor already exists, and no id is minted twice.
func TestEnsureNamedOnAnExistingProjectIgnoresBaseAndReportsNoRename(t *testing.T) {
	dir := mkDir(t, t.TempDir(), "x")

	first, _, _, err := EnsureNamed(dir, "original", func(string) bool { return false })
	if err != nil {
		t.Fatalf("EnsureNamed: %v", err)
	}

	again, created, renamedFrom, err := EnsureNamed(dir, "different-name", func(string) bool { return false })
	if err != nil {
		t.Fatalf("EnsureNamed: %v", err)
	}
	if created {
		t.Error("created = true on an existing project")
	}
	if renamedFrom != "" {
		t.Errorf("renamedFrom = %q, want empty on an existing project", renamedFrom)
	}
	if again.ID != first.ID {
		t.Errorf("id changed across calls: %q -> %q", first.ID, again.ID)
	}
	if again.Name != "original" {
		t.Errorf("Name = %q, want the existing identity kept", again.Name)
	}
}

// writeDescriptor saves a descriptor with the given tend policy and returns
// its .agent-utils directory.
func writeDescriptor(t *testing.T, tend Tend) string {
	t.Helper()
	dir := mkDir(t, t.TempDir(), "p")
	c := &Config{Name: "p", ID: "00000000-0000-0000-0000-000000000001", Tend: tend}
	if err := Save(dir, c); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return dir
}

// goodTend is the smallest tend policy Load accepts.
func goodTend() Tend {
	return Tend{
		Enabled:        true,
		Label:          "status:ready-for-review",
		Model:          "sonnet",
		PermissionMode: "acceptEdits",
		Prompt:         "rebase PR {{.PR.Number}}",
	}
}

// Every field the dispatcher cannot run without is required when tending is
// enabled, and each is reported by name. There is no loop behind the policy
// any more, so there is nothing left for any of them to fall back on.
func TestLoadRequiresTheFieldsATendCannotRunWithout(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Tend)
		want string
	}{
		{"label", func(x *Tend) { x.Label = "" }, "no tend.label"},
		{"prompt", func(x *Tend) { x.Prompt = "" }, "no tend.prompt"},
		{"model", func(x *Tend) { x.Model = "" }, "no tend.model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tend := goodTend()
			tc.edit(&tend)
			_, err := Load(writeDescriptor(t, tend))
			if err == nil {
				t.Fatalf("want an error for a policy with no tend.%s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// The same fields are NOT required while tending is switched off. That is how
// an operator parks a policy without deleting it, and the descriptor is read by
// every project command, so refusing here would break commands that have
// nothing to do with tending.
func TestLoadAcceptsAParkedTendPolicy(t *testing.T) {
	tend := goodTend()
	tend.Enabled = false
	tend.Prompt, tend.Model, tend.Label = "", "", ""

	if _, err := Load(writeDescriptor(t, tend)); err != nil {
		t.Fatalf("a parked policy must load: %v", err)
	}
}

// A tend prompt has no loop, so it has no loop labels. text/template renders a
// zero struct field as the empty string rather than failing, so a prompt
// carrying over the old host loop's "remove {{.Labels.Trigger}}" would silently
// instruct the agent to act on a label named "". The reference is refused at
// load time, where an operator reads it once.
func TestLoadRejectsATendPromptThatReferencesLoopLabels(t *testing.T) {
	tend := goodTend()
	tend.Prompt = "park by removing {{.Labels.Terminal}} and adding {{.Labels.Blocked}}"

	_, err := Load(writeDescriptor(t, tend))
	if err == nil {
		t.Fatal("want an error for a tend prompt naming .Labels, got nil")
	}
	// The message must offer the replacement. "rejected" alone leaves the
	// operator with a prompt they cannot rewrite.
	if !strings.Contains(err.Error(), "{{.Tend.Label}}") {
		t.Errorf("error must name the replacement, got: %v", err)
	}
}

// A tend prompt that will not parse fails the descriptor, not a detached runner
// three hours later.
func TestLoadRejectsAnUnparsableTendPrompt(t *testing.T) {
	tend := goodTend()
	tend.Prompt = "rebase {{.PR.Number"

	if _, err := Load(writeDescriptor(t, tend)); err == nil {
		t.Fatal("want an error for an unparsable tend prompt, got nil")
	}
}

// bypassPermissions disables every permission prompt on pull request review
// text written by third parties, so it needs the same acknowledgement a loop's
// own permission mode does.
func TestLoadRequiresTheBypassAcknowledgement(t *testing.T) {
	tend := goodTend()
	tend.PermissionMode = "bypassPermissions"

	if _, err := Load(writeDescriptor(t, tend)); err == nil {
		t.Fatal("want an error for unacknowledged bypassPermissions, got nil")
	}

	tend.AcknowledgeBypassPermissions = true
	if _, err := Load(writeDescriptor(t, tend)); err != nil {
		t.Fatalf("an acknowledged bypassPermissions must load: %v", err)
	}
}

// An invalid permission mode is rejected by name rather than reaching a
// dispatch claude would refuse.
func TestLoadRejectsAnInvalidTendPermissionMode(t *testing.T) {
	tend := goodTend()
	tend.PermissionMode = "nonsense"

	if _, err := Load(writeDescriptor(t, tend)); err == nil {
		t.Fatal("want an error for an invalid tend.permission_mode, got nil")
	}
}
