package home

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOverrideWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvVar, dir)

	got, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if got != dir {
		t.Errorf("Dir() = %q, want the override %q", got, dir)
	}
}

// An override pointing at a file must fail. Silently using the home directory
// instead would write this machine's state somewhere nobody asked for, and the
// mistake would only surface later as missing state.
func TestOverrideNamingAFileIsAnError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvVar, file)

	if _, err := Dir(); err == nil {
		t.Fatal("Dir() = nil error for an override that names a file")
	}
}

// An override that does not exist yet is fine: EnsureDir creates it.
func TestOverrideMayNotExistYet(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "made", "later")
	t.Setenv(EnvVar, dir)

	got, err := EnsureDir()
	if err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if got != dir {
		t.Errorf("EnsureDir() = %q, want %q", got, dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode = %o, want 700: the directory holds session identifiers", perm)
	}
}

func TestDefaultIsUnderTheHomeDirectory(t *testing.T) {
	t.Setenv(EnvVar, "")
	t.Setenv("HOME", t.TempDir())

	got, err := Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if !strings.HasSuffix(got, string(os.PathSeparator)+DirName) {
		t.Errorf("Dir() = %q, want a path ending in %q", got, DirName)
	}
}

func TestStateDBPathIsInsideTheDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvVar, dir)

	got, err := StateDBPath()
	if err != nil {
		t.Fatalf("StateDBPath: %v", err)
	}
	if want := filepath.Join(dir, StateDBFile); got != want {
		t.Errorf("StateDBPath() = %q, want %q", got, want)
	}
}

// EnvPath must resolve through Dir() exactly like StateDBPath does, so the
// registry, the canonical database, and the token file can never disagree
// about where home is.
func TestEnvPathIsInsideTheDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvVar, dir)

	got, err := EnvPath()
	if err != nil {
		t.Fatalf("EnvPath: %v", err)
	}
	if want := filepath.Join(dir, EnvFile); got != want {
		t.Errorf("EnvPath() = %q, want %q", got, want)
	}
}
