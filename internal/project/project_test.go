package project

import (
	"errors"
	"os"
	"path/filepath"
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
