package listener

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countAssignments counts the lines in body that assign key, so a test can
// prove SetToken left exactly one GITHUB_TOKEN line behind. Token() alone
// cannot prove that: it takes the LAST assignment, so a stale earlier line
// would go unnoticed.
func countAssignments(body, key string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if k, _, ok := envAssignment(line); ok && k == key {
			n++
		}
	}
	return n
}

func readBack(t *testing.T, path string) (string, os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(data), info.Mode().Perm()
}

func TestSetTokenCreatesTheFileAt0600(t *testing.T) {
	dir := envHome(t)

	path, err := SetToken("ghp_new")
	if err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	if want := filepath.Join(dir, "env"); path != want {
		t.Errorf("SetToken returned %q, want %q", path, want)
	}

	body, mode := readBack(t, path)
	if mode != 0o600 {
		t.Errorf("mode = %04o, want 0600: the file holds a repository-write credential", mode)
	}
	if !strings.HasPrefix(body, "export "+tokenKey+"=") {
		t.Errorf("body = %q, want an `export %s=` assignment so the file stays shell-sourceable", body, tokenKey)
	}

	got, err := Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "ghp_new" {
		t.Errorf("Token() = %q, want %q", got, "ghp_new")
	}
}

// SetToken must create the machine-wide directory itself. Requiring the
// operator to mkdir it first is the out-of-band step this command exists to
// remove.
func TestSetTokenCreatesTheHomeDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not", "created", "yet")
	t.Setenv("AGENT_UTILS_HOME", dir)

	if _, err := SetToken("ghp_new"); err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "env")); err != nil {
		t.Fatalf("stat env file: %v", err)
	}
}

func TestSetTokenReplacesAnExistingAssignment(t *testing.T) {
	home := envHome(t)
	writeEnvFile(t, home, "export "+tokenKey+"=ghp_old\n", 0o600)

	path, err := SetToken("ghp_new")
	if err != nil {
		t.Fatalf("SetToken: %v", err)
	}

	body, _ := readBack(t, path)
	if strings.Contains(body, "ghp_old") {
		t.Errorf("body = %q, still carries the replaced token", body)
	}
	if n := countAssignments(body, tokenKey); n != 1 {
		t.Errorf("%d %s assignments in %q, want exactly 1", n, tokenKey, body)
	}
	got, err := Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "ghp_new" {
		t.Errorf("Token() = %q, want %q", got, "ghp_new")
	}
}

// The README tells operators this file is sourced by cron, so it may hold
// unrelated exports and comments. Losing one of those would break a cron
// entry that has nothing to do with the token.
func TestSetTokenPreservesEveryOtherLine(t *testing.T) {
	home := envHome(t)
	before := "# the token for the webhook daemon\n" +
		"export PATH=/opt/homebrew/bin:$PATH\n" +
		tokenKey + "=ghp_old\n" +
		"# trailing note\n" +
		"export OTHER=1\n"
	writeEnvFile(t, home, before, 0o600)

	path, err := SetToken("ghp_new")
	if err != nil {
		t.Fatalf("SetToken: %v", err)
	}

	body, _ := readBack(t, path)
	for _, want := range []string{
		"# the token for the webhook daemon",
		"export PATH=/opt/homebrew/bin:$PATH",
		"# trailing note",
		"export OTHER=1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %q, lost the line %q", body, want)
		}
	}
	// Replaced in place, not moved to the end: an operator's file keeps the
	// shape they gave it.
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("body = %q, want 5 lines", body)
	}
	if k, _, ok := envAssignment(lines[2]); !ok || k != tokenKey {
		t.Errorf("line 3 = %q, want the %s assignment in its original position", lines[2], tokenKey)
	}
	got, err := Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "ghp_new" {
		t.Errorf("Token() = %q, want %q", got, "ghp_new")
	}
}

// A second assignment left in the file would win, because a sourced file and
// parseEnvValue both take the LAST one -- so the token just written would be
// silently overridden by a stale one.
func TestSetTokenDropsADuplicateAssignment(t *testing.T) {
	home := envHome(t)
	writeEnvFile(t, home, tokenKey+"=first\nexport OTHER=1\nexport "+tokenKey+"=second\n", 0o600)

	path, err := SetToken("ghp_new")
	if err != nil {
		t.Fatalf("SetToken: %v", err)
	}

	body, _ := readBack(t, path)
	if n := countAssignments(body, tokenKey); n != 1 {
		t.Errorf("%d %s assignments in %q, want exactly 1", n, tokenKey, body)
	}
	if !strings.Contains(body, "export OTHER=1") {
		t.Errorf("body = %q, lost an unrelated line while dropping the duplicate", body)
	}
	got, err := Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "ghp_new" {
		t.Errorf("Token() = %q, want %q", got, "ghp_new")
	}
}

// A file left group- or world-readable by an earlier hand-run `echo >>` is
// exactly what Token refuses to read. Rewriting it at 0600 repairs that
// instead of sending the operator back to chmod.
func TestSetTokenTightensAWideMode(t *testing.T) {
	home := envHome(t)
	writeEnvFile(t, home, "export OTHER=1\n", 0o644)

	path, err := SetToken("ghp_new")
	if err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	if _, mode := readBack(t, path); mode != 0o600 {
		t.Errorf("mode = %04o, want 0600", mode)
	}
	if _, err := Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}
}

// A file with no trailing newline must not have the assignment glued onto its
// last line, which would corrupt both lines.
func TestSetTokenAppendsAfterAFileWithNoTrailingNewline(t *testing.T) {
	home := envHome(t)
	writeEnvFile(t, home, "export OTHER=1", 0o600)

	path, err := SetToken("ghp_new")
	if err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	body, _ := readBack(t, path)
	if !strings.Contains(body, "export OTHER=1\n") {
		t.Errorf("body = %q, want the existing line intact and newline-terminated", body)
	}
	got, err := Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "ghp_new" {
		t.Errorf("Token() = %q, want %q", got, "ghp_new")
	}
}

// Refusing a symlink here matches readTokenFile: writing through one would
// put a repository-write credential into a file this process does not own.
func TestSetTokenRefusesASymlink(t *testing.T) {
	home := envHome(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, "env")); err != nil {
		t.Fatal(err)
	}

	if _, err := SetToken("ghp_new"); err == nil {
		t.Fatal("SetToken through a symlink: want an error, got nil")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "untouched\n" {
		t.Errorf("symlink target = %q, want it untouched", data)
	}
}

func TestSetTokenRejectsUnwritableValues(t *testing.T) {
	envHome(t)

	for name, token := range map[string]string{
		"empty":      "",
		"whitespace": "   \t\n",
		"newline":    "ghp_a\nexport EVIL=1",
		"quote":      "ghp_a'b",
		"nul":        "ghp_a\x00b",
		"control":    "ghp_a\x1bb",
	} {
		if _, err := SetToken(token); err == nil {
			t.Errorf("SetToken(%s): want an error, got nil", name)
		} else if strings.Contains(err.Error(), "ghp_a") {
			t.Errorf("SetToken(%s) error repeats the token: %v", name, err)
		}
	}
}

// A pasted token usually arrives with the newline the operator typed after
// it, and often with a stray leading space.
func TestSetTokenTrimsSurroundingWhitespace(t *testing.T) {
	envHome(t)

	if _, err := SetToken("  ghp_new\n"); err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	got, err := Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != "ghp_new" {
		t.Errorf("Token() = %q, want %q", got, "ghp_new")
	}
}

// `listener start` prompts only when the file is ABSENT; a wrong mode, a
// symlink or a bad owner must still fail. That distinction is made with
// errors.Is against this sentinel, so the wrapping has to survive.
func TestTokenReportsAMissingFileAsErrEnvFileMissing(t *testing.T) {
	envHome(t)

	_, err := Token()
	if err == nil {
		t.Fatal("Token with no env file: want an error, got nil")
	}
	if !errors.Is(err, ErrEnvFileMissing) {
		t.Errorf("Token error = %v, want it to wrap ErrEnvFileMissing", err)
	}
}

// The other failure modes must NOT report ErrEnvFileMissing: each is a
// condition the operator has to look at, not one a prompt can answer.
func TestTokenReportsOtherFailuresAsSomethingElse(t *testing.T) {
	home := envHome(t)
	writeEnvFile(t, home, "export "+tokenKey+"=abc\n", 0o644)

	_, err := Token()
	if err == nil {
		t.Fatal("Token on a 0644 file: want an error, got nil")
	}
	if errors.Is(err, ErrEnvFileMissing) {
		t.Errorf("Token error = %v, want a mode failure, not ErrEnvFileMissing", err)
	}
}
