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

// Regression. The machine-wide directory ($AGENT_UTILS_HOME, or plain
// $HOME/.agent-utils) is a parent of everything under the home tree, so an
// unmodified walk-up reaches it as an ORDINARY ancestor for any command run
// from a directory under $HOME that is not inside a project -- e.g.
// ~/Downloads/scratch. FindDir must not treat that ancestor as a match: doing
// so would make the caller write a project descriptor into the machine-wide
// directory the registry and the canonical state database also live in.
//
// $AGENT_UTILS_HOME, not $HOME, is set here: $HOME also steers git and ssh,
// which a test must not disturb.
func TestFindDirDoesNotAdoptTheMachineWideDirectory(t *testing.T) {
	t.Setenv("AGENT_UTILS_DIR", "")

	homeDir := t.TempDir()
	machineWide := filepath.Join(homeDir, DirName)
	t.Setenv("AGENT_UTILS_HOME", machineWide)
	// The machine-wide directory itself, exactly as the registry creates it.
	if err := os.MkdirAll(filepath.Join(machineWide, ConfigsSubdir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(machineWide, ConfigsSubdir, "elsewhere.yaml"),
		[]byte(validYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	scratch := filepath.Join(homeDir, "Downloads", "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := FindDir(scratch)
	if err == nil {
		t.Fatalf("FindDir = %q, want an error; the machine-wide directory must never be adopted", got)
	}
	if !errors.Is(err, ErrNoDir) {
		t.Fatalf("err = %v, want ErrNoDir", err)
	}
	if !contains(err.Error(), "agent-utils project init") {
		t.Errorf("err = %q, want it to name the fix (`agent-utils project init`)", err)
	}
}

// Regression. home.Dir() and a walk candidate can name the same directory in
// two different spellings: macOS resolves /var to /private/var, and
// FindDir's only caller (internal/loopcmd/resolve.go) feeds it os.Getwd(),
// which returns the RESOLVED spelling whenever $PWD is unset -- exactly the
// case for a launchd-started process such as the listener daemon this
// feature adds. AGENT_UTILS_HOME is set here to the raw (unresolved)
// spelling, as an operator or a plist would write it, while the walk starts
// from the resolved spelling, as os.Getwd() would hand FindDir under
// launchd. A guard that compares raw strings never fires in this case and
// silently lets the machine-wide directory through.
func TestFindDirDoesNotAdoptTheMachineWideDirectoryAcrossASymlink(t *testing.T) {
	t.Setenv("AGENT_UTILS_DIR", "")

	homeDir := t.TempDir()
	resolvedHome, err := filepath.EvalSymlinks(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedHome == homeDir {
		t.Skip("temp dir is not reached through a symlink on this platform; the spellings cannot diverge")
	}

	// AGENT_UTILS_HOME holds the raw spelling, the way an operator would
	// write it or launchd's plist would name it.
	machineWide := filepath.Join(homeDir, DirName)
	t.Setenv("AGENT_UTILS_HOME", machineWide)
	if err := os.MkdirAll(filepath.Join(machineWide, ConfigsSubdir), 0o700); err != nil {
		t.Fatal(err)
	}

	// The walk starts from the RESOLVED spelling, the way os.Getwd() returns
	// it when $PWD is unset.
	scratch := filepath.Join(resolvedHome, "Downloads", "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := FindDir(scratch)
	if err == nil {
		t.Fatalf("FindDir = %q, want an error; the machine-wide directory must never be adopted, even spelled through a symlink", got)
	}
	if !errors.Is(err, ErrNoDir) {
		t.Fatalf("err = %v, want ErrNoDir", err)
	}
}

// A genuine project nested under the same tree as the machine-wide directory
// (e.g. ~/Downloads/myproject) must still resolve. Skipping the machine-wide
// directory must not turn into skipping every ancestor.
func TestFindDirStillResolvesAProjectNearTheMachineWideDirectory(t *testing.T) {
	t.Setenv("AGENT_UTILS_DIR", "")

	homeDir := t.TempDir()
	machineWide := filepath.Join(homeDir, DirName)
	t.Setenv("AGENT_UTILS_HOME", machineWide)
	if err := os.MkdirAll(filepath.Join(machineWide, ConfigsSubdir), 0o700); err != nil {
		t.Fatal(err)
	}

	project := filepath.Join(homeDir, "Downloads", "myproject")
	want := mkConfigs(t, project, nil)

	deep := filepath.Join(project, "sub", "dir")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := FindDir(deep)
	if err != nil {
		t.Fatalf("FindDir: %v", err)
	}
	if got != want {
		t.Errorf("FindDir = %q, want the project's own %q", got, want)
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
