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

func TestEnsureCreatesADescriptorNamedAfterTheDirectory(t *testing.T) {
	dir := mkDir(t, t.TempDir(), "lawndominator")

	c, created, err := Ensure(dir, func(string) bool { return false })
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !created {
		t.Error("created = false on a fresh project")
	}
	if c.Name != "lawndominator" {
		t.Errorf("Name = %q, want the directory name", c.Name)
	}
	if c.ID == "" {
		t.Error("ID must be minted")
	}

	// A second call must load, not re-create: the id has to be stable.
	again, created, err := Ensure(dir, func(string) bool { return false })
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

// Two projects can easily share a directory name. The name is the human handle
// and must be unique, so the second one gets a suffix.
func TestEnsureUniquifiesATakenName(t *testing.T) {
	dir := mkDir(t, t.TempDir(), "web")

	c, _, err := Ensure(dir, func(n string) bool { return n == "web" || n == "web-2" })
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "web-3" {
		t.Errorf("Name = %q, want web-3 when web and web-2 are taken", c.Name)
	}
}

func TestEnsureSlugsAnAwkwardDirectoryName(t *testing.T) {
	dir := mkDir(t, t.TempDir(), "My Project (v2)!")

	c, _, err := Ensure(dir, func(string) bool { return false })
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
