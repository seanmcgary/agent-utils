package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func mkConfigs(t *testing.T, root string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, DirName, ConfigsSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(root, DirName)
}

func TestFindDirWalksUpFromASubdirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_UTILS_DIR", "")
	want := mkConfigs(t, root, nil)

	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := FindDir(deep)
	if err != nil {
		t.Fatalf("FindDir: %v", err)
	}
	// Compare resolved paths; a temp dir on macOS is a /private symlink.
	gotResolved, _ := filepath.EvalSymlinks(got)
	wantResolved, _ := filepath.EvalSymlinks(want)
	if gotResolved != wantResolved {
		t.Errorf("FindDir = %q, want %q", gotResolved, wantResolved)
	}
}

func TestFindDirPrefersTheEnvironmentOverride(t *testing.T) {
	root := t.TempDir()
	override := mkConfigs(t, root, nil)
	t.Setenv("AGENT_UTILS_DIR", override)

	got, err := FindDir(t.TempDir())
	if err != nil {
		t.Fatalf("FindDir: %v", err)
	}
	if got != override {
		t.Errorf("FindDir = %q, want the override %q", got, override)
	}
}

func TestFindDirReportsAnUnusableOverride(t *testing.T) {
	t.Setenv("AGENT_UTILS_DIR", filepath.Join(t.TempDir(), "nope"))
	_, err := FindDir(t.TempDir())
	if !errors.Is(err, ErrNoDir) {
		t.Fatalf("err = %v, want ErrNoDir", err)
	}
}

func TestFindDirErrorsWhenNothingExists(t *testing.T) {
	t.Setenv("AGENT_UTILS_DIR", "")
	t.Setenv("HOME", t.TempDir())

	_, err := FindDir(t.TempDir())
	if !errors.Is(err, ErrNoDir) {
		t.Fatalf("err = %v, want ErrNoDir", err)
	}
}

// The name of a loop is the `name` field inside the file, never the file name.
func TestListTakesTheNameFromTheFileContents(t *testing.T) {
	t.Setenv("AGENT_UTILS_DIR", "")
	dir := mkConfigs(t, t.TempDir(), map[string]string{
		// The file is called zzz; the config declares name: planning.
		"zzz-unrelated-file-name.yaml": validYAML,
	})

	entries, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	if entries[0].Name != "planning" {
		t.Errorf("Name = %q, want %q from the name field", entries[0].Name, "planning")
	}
	if entries[0].File != "zzz-unrelated-file-name.yaml" {
		t.Errorf("File = %q, want the file name retained for display", entries[0].File)
	}
}

func TestListSortsAndReportsBrokenFiles(t *testing.T) {
	t.Setenv("AGENT_UTILS_DIR", "")
	dir := mkConfigs(t, t.TempDir(), map[string]string{
		"zebra.yaml":  replaceOnce(validYAML, "name: planning", "name: zebra"),
		"alpha.yml":   replaceOnce(validYAML, "name: planning", "name: alpha"),
		"broken.yaml": "name: x\nthis_key_does_not_exist: 1\n",
		"notes.txt":   "ignored",
	})

	entries, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// "broken" is the FILE name, standing in because the file has no readable
	// name field.
	if got := Names(entries); len(got) != 3 ||
		got[0] != "alpha" || got[1] != "broken" || got[2] != "zebra" {
		t.Fatalf("names = %v, want [alpha broken zebra]", got)
	}
	// A broken file is listed WITH its error, not hidden. A config that
	// silently does not appear is harder to debug than one that appears bad.
	for _, e := range entries {
		switch e.Name {
		case "broken":
			if e.Err == nil {
				t.Error("broken config must carry its load error")
			}
		default:
			if e.Err != nil {
				t.Errorf("%s: unexpected error %v", e.Name, e.Err)
			}
			if e.Repo == "" {
				t.Errorf("%s: repo not populated", e.Name)
			}
		}
	}
}

func TestListErrorsWhenThereAreNoConfigs(t *testing.T) {
	t.Setenv("AGENT_UTILS_DIR", "")
	dir := mkConfigs(t, t.TempDir(), nil)
	if _, err := List(dir); !errors.Is(err, ErrNoConfigs) {
		t.Fatalf("err = %v, want ErrNoConfigs", err)
	}
}

func TestResolveByName(t *testing.T) {
	t.Setenv("AGENT_UTILS_DIR", "")
	dir := mkConfigs(t, t.TempDir(), map[string]string{
		// Deliberately misleading file names: resolution must use the name
		// field, not the file.
		"b.yaml": validYAML,
		"a.yaml": replaceOnce(validYAML, "name: planning", "name: execution"),
	})

	path, err := Resolve(dir, "planning")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if filepath.Base(path) != "b.yaml" {
		t.Errorf("path = %q, want b.yaml, the file declaring name: planning", path)
	}

	_, err = Resolve(dir, "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// The error must name what IS available, so the fix is obvious.
	if got := err.Error(); !contains(got, "execution") || !contains(got, "planning") {
		t.Errorf("error %q should list the available names", got)
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

// Two loops sharing a name would share a state directory, a lock file and every
// database row while appearing to be separate loops. Resolution must refuse
// rather than silently pick one.
func TestResolveRefusesADuplicateName(t *testing.T) {
	t.Setenv("AGENT_UTILS_DIR", "")
	dir := mkConfigs(t, t.TempDir(), map[string]string{
		"one.yaml": validYAML,
		"two.yaml": validYAML,
	})

	_, err := Resolve(dir, "planning")
	if !errors.Is(err, ErrDuplicateName) {
		t.Fatalf("err = %v, want ErrDuplicateName", err)
	}
	for _, want := range []string{"one.yaml", "two.yaml"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q should name the conflicting file %q", err, want)
		}
	}
}

func TestDuplicatesReportsSharedNames(t *testing.T) {
	entries := []Entry{
		{Name: "planning", File: "a.yaml"},
		{Name: "planning", File: "b.yaml"},
		{Name: "execution", File: "c.yaml"},
	}
	got := Duplicates(entries)
	if len(got) != 1 || got[0] != "planning" {
		t.Errorf("Duplicates = %v, want [planning]", got)
	}
	if len(Duplicates(entries[1:])) != 0 {
		t.Error("Duplicates should be empty when every name is unique")
	}
}
