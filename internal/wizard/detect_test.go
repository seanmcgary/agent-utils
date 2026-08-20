package wizard

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	if out, err := exec.Command("git", full...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestDetectNotAGitRepoReturnsEmpty(t *testing.T) {
	dir := t.TempDir()

	d := Detect(dir)
	if d.Repo != "" || d.DefaultBranch != "" || d.CheckoutBaseDir != "" {
		t.Fatalf("Detect in a non-git directory = %+v, want all three fields empty", d)
	}
}

func TestDetectParsesOwnerNameFromBothRemoteForms(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "remote", "add", "origin", "git@github.com:acme/example.git")

	d := Detect(dir)
	if d.Repo != "acme/example" {
		t.Fatalf("ssh remote: Repo = %q, want %q", d.Repo, "acme/example")
	}

	runGit(t, dir, "remote", "set-url", "origin", "https://github.com/acme/example.git")
	d = Detect(dir)
	if d.Repo != "acme/example" {
		t.Fatalf("https remote: Repo = %q, want %q", d.Repo, "acme/example")
	}
}

func TestDetectCheckoutBaseDirIsTheWorkTreeRoot(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")

	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %q: %v", dir, err)
	}

	d := Detect(dir)
	if d.CheckoutBaseDir == "" {
		t.Fatal("CheckoutBaseDir is empty in a real git work tree")
	}
	got, err := filepath.EvalSymlinks(d.CheckoutBaseDir)
	if err != nil {
		t.Fatalf("resolve detected %q: %v", d.CheckoutBaseDir, err)
	}
	if got != want {
		t.Fatalf("CheckoutBaseDir = %q, want %q", got, want)
	}
}

func TestDetectDefaultBranchStripsOriginPrefix(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init")
	// symbolic-ref only needs the ref to exist, not a real remote fetch, so
	// this exercises the origin/HEAD parsing without a network.
	runGit(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	d := Detect(dir)
	if d.DefaultBranch != "main" {
		t.Fatalf("DefaultBranch = %q, want %q", d.DefaultBranch, "main")
	}
}
