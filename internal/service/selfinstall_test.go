package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// privateDir returns a 0700 directory whose every ancestor is free of the
// group-write and other-write bits, plus a cleanup.
//
// t.TempDir() is NOT usable for a refuseIfWritableByOthers fixture. That
// function walks every parent up to the filesystem root, and on Linux
// t.TempDir() lands under /tmp, which is mode 1777. The existing darwin
// helper (service_darwin_test.go's writableSelf) gets away with t.TempDir()
// only because macOS TMPDIR is a private /var/folders/... tree that is 0700
// the whole way up; its "t.TempDir() defaults to 0700" comment is true of
// the leaf, not of the ancestry.
//
// The user's home directory is the portable answer: ~ and the directories
// above it are conventionally 0755 on both platforms, which has no group or
// other WRITE bit, so a 0700 directory created inside it passes the walk.
//
// If a future change makes these tests fail on a machine whose home
// directory is group-writable, the fix is this helper. It is NOT to relax
// refuseIfWritableByOthers -- read the sticky-bit paragraph in that
// function before touching it.
func privateDir(t *testing.T) string {
	t.Helper()
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("locate home directory: %v", err)
	}
	dir, err := os.MkdirTemp(userHome, ".agent-utils-test-")
	if err != nil {
		t.Fatalf("create a private fixture directory: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod the fixture directory: %v", err)
	}
	t.Cleanup(func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			t.Errorf("remove the fixture directory: %v", rmErr)
		}
	})
	return dir
}

// privateSelf creates a fake "agent-utils" binary at 0755 inside a private
// directory and returns its path.
func privateSelf(t *testing.T) string {
	t.Helper()
	path := filepath.Join(privateDir(t), "agent-utils")
	if err := os.WriteFile(path, []byte("fake binary"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

// stubExecutable points executablePath at path for the test. Install must
// never resolve the real `go test` binary: its location and permissions are
// outside this test's control.
func stubExecutable(t *testing.T, path string) {
	t.Helper()
	prev := executablePath
	executablePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { executablePath = prev })
}

// stubGeteuid makes the process look like it is running under the given
// effective user identifier. Install refuses euid 0 outright, and that
// refusal needs a test that does not require running the suite as root.
func stubGeteuid(t *testing.T, uid int) {
	t.Helper()
	prev := geteuid
	geteuid = func() int { return uid }
	t.Cleanup(func() { geteuid = prev })
}

// TestRefuseIfWritableByOthersRejectsAWritableParent pins the check that
// keeps a service definition from naming a binary another local account can
// replace. It lives in a file with no build tag because both the launchd
// backend and the systemd backend depend on it, and the systemd one is the
// stronger case: a system unit is started by root.
func TestRefuseIfWritableByOthersRejectsAWritableParent(t *testing.T) {
	loose := filepath.Join(privateDir(t), "loose")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Set explicitly: os.Mkdir applies the process umask, so the mode
	// argument alone does not produce a world-writable directory.
	if err := os.Chmod(loose, 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	bin := filepath.Join(loose, "agent-utils")
	if err := os.WriteFile(bin, []byte("fake binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := refuseIfWritableByOthers(bin)
	if err == nil {
		t.Fatal("refuseIfWritableByOthers accepted a world-writable parent directory")
	}
	// cmd/agent-utils/listener.go's containsWritableRefusal matches this
	// exact phrase. A wording change here breaks that match silently.
	if !strings.Contains(err.Error(), "writable by group or other") {
		t.Errorf("refusal %q does not carry the phrase containsWritableRefusal matches", err)
	}
}

// TestRefuseIfWritableByOthersAcceptsAPrivateDirectory proves the check is
// not simply always-refuse, and that privateDir produces a fixture it
// accepts on this machine.
func TestRefuseIfWritableByOthersAcceptsAPrivateDirectory(t *testing.T) {
	if err := refuseIfWritableByOthers(privateSelf(t)); err != nil {
		t.Errorf("refuseIfWritableByOthers rejected a private path: %v", err)
	}
}

// TestResolveSelfUsesExecutablePathVariable proves resolveSelf reads the
// running binary through the seam a test can replace, not through a path a
// caller supplies. That is the property that keeps a service definition from
// being pointed at an arbitrary path.
func TestResolveSelfUsesExecutablePathVariable(t *testing.T) {
	bin := privateSelf(t)
	stubExecutable(t, bin)

	got, err := resolveSelf()
	if err != nil {
		t.Fatalf("resolveSelf: %v", err)
	}
	want, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got != want {
		t.Errorf("resolveSelf = %q, want %q", got, want)
	}
}

// TestNewIsReplaceable pins the seam cmd/agent-utils's tests depend on. If
// New goes back to being a plain function, those tests start running a real
// launchctl or systemctl.
func TestNewIsReplaceable(t *testing.T) {
	prev := New
	t.Cleanup(func() { New = prev })

	called := false
	New = func() Manager {
		called = true
		return prev()
	}
	_ = New()
	if !called {
		t.Error("New is not a replaceable variable")
	}
}
