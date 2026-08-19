package registry

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func mkProject(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, ".agent-utils")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRegisterIsIdempotentAndRecordsTheRoot(t *testing.T) {
	t.Setenv("AGENT_UTILS_HOME", "")
	t.Setenv("HOME", t.TempDir())
	dir := mkProject(t, t.TempDir())

	for i := 0; i < 3; i++ {
		if err := Register(dir, "id-"+filepath.Base(filepath.Dir(dir)), filepath.Base(filepath.Dir(dir))); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}

	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 after three registrations", len(got))
	}
	if got[0].AgentUtilsDir != dir {
		t.Errorf("AgentUtilsDir = %q, want %q", got[0].AgentUtilsDir, dir)
	}
	if got[0].Root != filepath.Dir(dir) {
		t.Errorf("Root = %q, want %q", got[0].Root, filepath.Dir(dir))
	}
	if got[0].LastSeen.Before(got[0].FirstSeen) {
		t.Error("LastSeen must not precede FirstSeen")
	}
}

func TestListIsEmptyBeforeAnythingIsRegistered(t *testing.T) {
	t.Setenv("AGENT_UTILS_HOME", "")
	t.Setenv("HOME", t.TempDir())
	got, err := List()
	if err != nil {
		t.Fatalf("List on a fresh home must not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestListOrdersMostRecentFirst(t *testing.T) {
	t.Setenv("AGENT_UTILS_HOME", "")
	t.Setenv("HOME", t.TempDir())
	a := mkProject(t, t.TempDir())
	b := mkProject(t, t.TempDir())

	if err := Register(a, "id-"+filepath.Base(filepath.Dir(a)), filepath.Base(filepath.Dir(a))); err != nil {
		t.Fatal(err)
	}
	if err := Register(b, "id-"+filepath.Base(filepath.Dir(b)), filepath.Base(filepath.Dir(b))); err != nil {
		t.Fatal(err)
	}
	// Touch a again so it becomes the most recent.
	if err := Register(a, "id-"+filepath.Base(filepath.Dir(a)), filepath.Base(filepath.Dir(a))); err != nil {
		t.Fatal(err)
	}

	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].AgentUtilsDir != a {
		t.Errorf("order = %v, want %q first", got, a)
	}
}

func TestForgetRemovesOnlyTheNamedProject(t *testing.T) {
	t.Setenv("AGENT_UTILS_HOME", "")
	t.Setenv("HOME", t.TempDir())
	a := mkProject(t, t.TempDir())
	b := mkProject(t, t.TempDir())
	if err := Register(a, "id-"+filepath.Base(filepath.Dir(a)), filepath.Base(filepath.Dir(a))); err != nil {
		t.Fatal(err)
	}
	if err := Register(b, "id-"+filepath.Base(filepath.Dir(b)), filepath.Base(filepath.Dir(b))); err != nil {
		t.Fatal(err)
	}

	if err := Forget(a); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AgentUtilsDir != b {
		t.Errorf("after Forget: %v, want only %q", got, b)
	}
	// Forget must not touch the project's own files.
	if _, err := os.Stat(a); err != nil {
		t.Errorf("Forget deleted project files: %v", err)
	}
}

func TestExistsReportsAMovedProject(t *testing.T) {
	t.Setenv("AGENT_UTILS_HOME", "")
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	dir := mkProject(t, root)
	if err := Register(dir, "id-"+filepath.Base(filepath.Dir(dir)), filepath.Base(filepath.Dir(dir))); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want the entry retained", len(got))
	}
	if got[0].Exists() {
		t.Error("Exists = true for a directory that was removed")
	}
}

// Several loops tick at once, each in its own process. The read-modify-write
// has to be exclusive or a registration is lost.
func TestConcurrentRegistrationsAllSurvive(t *testing.T) {
	t.Setenv("AGENT_UTILS_HOME", "")
	t.Setenv("HOME", t.TempDir())

	const n = 8
	dirs := make([]string, n)
	for i := range dirs {
		dirs[i] = mkProject(t, t.TempDir())
	}

	var wg sync.WaitGroup
	for _, d := range dirs {
		wg.Add(1)
		go func(dir string) {
			defer wg.Done()
			if err := Register(dir, "id-"+filepath.Base(filepath.Dir(dir)), filepath.Base(filepath.Dir(dir))); err != nil {
				t.Errorf("Register(%s): %v", dir, err)
			}
		}(d)
	}
	wg.Wait()

	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Errorf("len = %d, want %d; a concurrent registration was lost", len(got), n)
	}
}

func TestReadToleratesAnEmptyFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_UTILS_HOME", "")
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := List(); err != nil {
		t.Errorf("an empty registry must not be an error: %v", err)
	}
}
