package version_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/version.sh is the contract between the VERSION file, the Makefile,
// and the release workflow. These tests pin its three behaviours so a change to
// it cannot silently alter what a released binary reports.
func runVersionScript(t *testing.T, versionFile, ref string) (string, string, int) {
	t.Helper()

	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "version.sh"))
	if err != nil {
		t.Fatal(err)
	}

	// The script reads ./VERSION and shells out to git, so give it a throwaway
	// repository of its own rather than letting it rewrite the real file.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte(versionFile), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "master"},
		{"-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	cmd := exec.Command(script, ref)
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()

	after, err := os.ReadFile(filepath.Join(dir, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	return string(out), string(after), cmd.ProcessState.ExitCode()
}

func TestVersionScriptAppendsCommitOnANonTagRef(t *testing.T) {
	out, after, code := runVersionScript(t, "v0.1.0\n", "refs/heads/main")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	if !strings.HasPrefix(after, "v0.1.0+") {
		t.Errorf("VERSION = %q, want it rewritten to v0.1.0+<sha>", after)
	}
	if strings.ContainsAny(after, " \n\t") {
		t.Errorf("VERSION = %q, want no surrounding whitespace", after)
	}
}

func TestVersionScriptAcceptsAMatchingTag(t *testing.T) {
	out, after, code := runVersionScript(t, "v0.1.0\n", "refs/tags/v0.1.0")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, out)
	}
	if !strings.Contains(out, "Version correctly matches tag") {
		t.Errorf("output = %q, want the match confirmation", out)
	}
	// A release must ship the bare semantic version, with no commit suffix.
	if strings.TrimSpace(after) != "v0.1.0" {
		t.Errorf("VERSION = %q, want it left as v0.1.0 on a tagged release", after)
	}
}

func TestVersionScriptRejectsAMismatchedTag(t *testing.T) {
	out, _, code := runVersionScript(t, "v0.1.0\n", "refs/tags/v9.9.9")
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero when the tag and VERSION disagree\n%s", out)
	}
	if !strings.Contains(out, "does not match the tag") {
		t.Errorf("output = %q, want the mismatch message", out)
	}
}

// The VERSION file in the repository must be a bare semantic version. CI
// rewrites it in place, so a committed suffix would make every tag mismatch.
func TestRepositoryVersionFileIsCleanSemver(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	v := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(v, "v") {
		t.Errorf("VERSION = %q, want a leading v", v)
	}
	if strings.Contains(v, "+") {
		t.Errorf("VERSION = %q, want no build suffix committed", v)
	}
	if strings.Count(v, ".") != 2 {
		t.Errorf("VERSION = %q, want major.minor.patch", v)
	}
}
