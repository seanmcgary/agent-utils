package identity

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// isolatedGit returns an environment that hides the developer's own git
// config, so a test sees only what it sets up.
func isolatedGit(t *testing.T) []string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(cfg, []byte("[user]\n\tname = Host User\n\temail = host@example.com\n[init]\n\tdefaultBranch = main\n"), 0o600)
	return []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=" + cfg, "GIT_CONFIG_NOSYSTEM=1",
	}
}

func run(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	out, err := tryRun(dir, env, name, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// tryRun is run without the test handle, for goroutines: FailNow may only be
// called from the test's own goroutine.
func tryRun(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %v: %w\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// newRepo makes a repository with one commit and returns its path.
func newRepo(t *testing.T, env []string) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, env, "git", "init", "-q")
	os.WriteFile(filepath.Join(dir, "a"), []byte("a"), 0o644)
	run(t, dir, env, "git", "add", "a")
	run(t, dir, env, "git", "commit", "-qm", "init")
	return dir
}

func commit(t *testing.T, dir string, env []string, file, msg string) string {
	t.Helper()
	out, err := tryCommit(dir, env, file, msg)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func tryCommit(dir string, env []string, file, msg string) (string, error) {
	os.WriteFile(filepath.Join(dir, file), []byte(file), 0o644)
	if _, err := tryRun(dir, env, "git", "add", file); err != nil {
		return "", err
	}
	if _, err := tryRun(dir, env, "git", "commit", "-qm", msg); err != nil {
		return "", err
	}
	return tryRun(dir, env, "git", "log", "-1", "--format=%B")
}

func trailers(msg string) []string {
	var out []string
	for _, l := range strings.Split(msg, "\n") {
		if strings.HasPrefix(l, "Co-authored-by:") {
			out = append(out, l)
		}
	}
	return out
}
