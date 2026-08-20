package listener

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// envHome points $AGENT_UTILS_HOME at a fresh temporary directory and
// returns it, so home.EnvPath() resolves inside a directory this test
// controls.
func envHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_UTILS_HOME", dir)
	return dir
}

// writeEnvFile writes body to the env file at the given mode.
func writeEnvFile(t *testing.T, home, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(home, "env")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTokenReadsAnUnexportedAssignment(t *testing.T) {
	home := envHome(t)
	writeEnvFile(t, home, "GITHUB_TOKEN=abc\n", 0o600)

	got, err := Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "abc" {
		t.Errorf("Token() = %q, want %q", got, "abc")
	}
}

func TestTokenReadsAnExportedAssignment(t *testing.T) {
	home := envHome(t)
	writeEnvFile(t, home, "export GITHUB_TOKEN=abc\n", 0o600)

	got, err := Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "abc" {
		t.Errorf("Token() = %q, want %q", got, "abc")
	}
}

func TestTokenStripsMatchingQuotes(t *testing.T) {
	home := envHome(t)
	writeEnvFile(t, home, `GITHUB_TOKEN="abc123"`+"\n", 0o600)

	got, err := Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "abc123" {
		t.Errorf("Token() = %q, want %q", got, "abc123")
	}
}

func TestTokenStripsSingleQuotes(t *testing.T) {
	home := envHome(t)
	writeEnvFile(t, home, "GITHUB_TOKEN='abc123'\n", 0o600)

	got, err := Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "abc123" {
		t.Errorf("Token() = %q, want %q", got, "abc123")
	}
}

// A CRLF-written file (common when a value is pasted from a Windows editor
// or copied through some clipboard paths) must not leave a \r stuck to the
// end of the token -- GitHub's API would then reject every request with a
// token that looks right in every printed rendering.
func TestTokenStripsATrailingCR(t *testing.T) {
	home := envHome(t)
	writeEnvFile(t, home, "GITHUB_TOKEN=abc\r\n", 0o600)

	got, err := Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "abc" {
		t.Errorf("Token() = %q, want %q (no trailing CR)", got, "abc")
	}
	if strings.Contains(got, "\r") {
		t.Errorf("Token() = %q, contains a stray carriage return", got)
	}
}

func TestTokenSkipsCommentsAndBlankLines(t *testing.T) {
	home := envHome(t)
	writeEnvFile(t, home, "\n# a comment\n   # an indented comment\n\nGITHUB_TOKEN=abc\n", 0o600)

	got, err := Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "abc" {
		t.Errorf("Token() = %q, want %q", got, "abc")
	}
}

func TestTokenRepeatedKeyTakesTheLastValue(t *testing.T) {
	home := envHome(t)
	writeEnvFile(t, home, "GITHUB_TOKEN=first\nGITHUB_TOKEN=second\n", 0o600)

	got, err := Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "second" {
		t.Errorf("Token() = %q, want %q (last occurrence wins, matching shell semantics)", got, "second")
	}
}

// A '#' is legal inside a token, so anything after an unquoted value must
// stay part of the value; only a line whose FIRST non-space character is
// '#' is a comment.
func TestTokenDoesNotTreatATrailingHashAsAComment(t *testing.T) {
	home := envHome(t)
	writeEnvFile(t, home, "GITHUB_TOKEN=abc#notacomment\n", 0o600)

	got, err := Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "abc#notacomment" {
		t.Errorf("Token() = %q, want the '#' preserved as part of the value", got)
	}
}

func TestTokenRejectsAWorldReadableFile(t *testing.T) {
	home := envHome(t)
	path := writeEnvFile(t, home, "GITHUB_TOKEN=abc\n", 0o644)
	// os.WriteFile's mode argument is filtered through the process umask,
	// so on a machine with a restrictive umask (e.g. 077) the file could
	// land at 0600 despite asking for 0644 -- and the body parses fine at
	// 0600, so the test would then fail for a reason that has nothing to do
	// with the mode check under test. Chmod sets the mode exactly, with no
	// umask involved.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Token()
	if err == nil {
		t.Fatal("Token() = nil error for a mode 0644 file")
	}
	if !strings.Contains(err.Error(), "0644") {
		t.Errorf("err = %q, want it to name the offending mode 0644", err)
	}
}

// The 64 KiB cap must actually be enforced, not merely documented: without
// this test env.go:119's LimitReader could silently regress into an
// unbounded read and nothing would catch it.
func TestTokenRejectsAFileOverTheSizeCap(t *testing.T) {
	home := envHome(t)
	const limit = 64 * 1024
	// One byte over the cap: enough to prove the boundary is enforced
	// without asserting on some arbitrary larger size.
	body := "GITHUB_TOKEN=" + strings.Repeat("a", limit+1) + "\n"
	writeEnvFile(t, home, body, 0o600)

	_, err := Token()
	if err == nil {
		t.Fatal("Token() = nil error for a file one byte over the 64 KiB cap")
	}
	if !strings.Contains(err.Error(), "65536") {
		t.Errorf("err = %q, want it to name the 65536-byte limit", err)
	}
}

// A FIFO at this path must be rejected, not read from -- and, more
// importantly, must not wedge Token() forever. open(2) on a FIFO blocks
// until a writer appears unless O_NONBLOCK is set; this test runs Token()
// in a goroutine with a timeout specifically so a regression that drops
// O_NONBLOCK fails this test instead of hanging the whole suite.
func TestTokenRejectsAFIFOWithoutBlockingForever(t *testing.T) {
	home := envHome(t)
	path := filepath.Join(home, "env")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, err := Token()
		done <- result{err: err}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("Token() = nil error for a FIFO at the env path")
		}
	case <-time.After(2 * time.Second):
		// This is exactly the wedge O_NONBLOCK exists to prevent: without
		// it, open(2) on this FIFO blocks forever waiting for a writer that
		// will never arrive, inside os.OpenFile, before IsRegular ever runs.
		t.Fatal("Token() did not return within 2s: it is blocked opening a FIFO, " +
			"the wedge O_NONBLOCK on the OpenFile call is supposed to prevent")
	}
}

func TestTokenRejectsASymlink(t *testing.T) {
	home := envHome(t)
	target := filepath.Join(home, "real-env")
	if err := os.WriteFile(target, []byte("GITHUB_TOKEN=abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, "env")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if _, err := Token(); err == nil {
		t.Fatal("Token() = nil error for a symlink at the env path")
	}
}

func TestTokenReportsAnAbsentFileByPath(t *testing.T) {
	home := envHome(t)
	wantPath := filepath.Join(home, "env")

	_, err := Token()
	if err == nil {
		t.Fatal("Token() = nil error for a missing env file")
	}
	if !strings.Contains(err.Error(), wantPath) {
		t.Errorf("err = %q, want it to name the path %q", err, wantPath)
	}
}
