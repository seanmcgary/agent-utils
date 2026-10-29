package identity

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const sean = "Sean McGary <sean@seanmcgary.com>"

func prepare(t *testing.T, base []string, workDir, coAuthor string) []string {
	t.Helper()
	extra, err := Prepare(context.Background(), Plan{
		Dir: filepath.Join(t.TempDir(), "run"), WorkDir: workDir, Repo: "acme/widgets",
		SelfPath: "/bin/false", CoAuthor: coAuthor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(append([]string{}, base...), extra...)
}

func TestCoAuthorTrailerAddedOnce(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	env := prepare(t, base, repo, sean)

	msg := commit(t, repo, env, "b", "feat: b")
	if got := trailers(msg); len(got) != 1 || got[0] != "Co-authored-by: "+sean {
		t.Fatalf("trailers = %q", got)
	}
	run(t, repo, env, "git", "commit", "-q", "--amend", "--no-edit")
	run(t, repo, env, "git", "commit", "-q", "--amend", "-m", run(t, repo, env, "git", "log", "-1", "--format=%B"))
	if got := trailers(run(t, repo, env, "git", "log", "-1", "--format=%B")); len(got) != 1 {
		t.Errorf("after amends, trailers = %q", got)
	}
}

func TestCoAuthorWithApostrophe(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	v := "Seán O'Brien <7+sob@users.noreply.github.com>"
	msg := commit(t, repo, prepare(t, base, repo, v), "b", "feat: b")
	if got := trailers(msg); len(got) != 1 || got[0] != "Co-authored-by: "+v {
		t.Errorf("trailers = %q", got)
	}
}

func TestCoAuthorFromGrandchildProcess(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	env := prepare(t, base, repo, sean)
	os.WriteFile(filepath.Join(repo, "c"), []byte("c"), 0o644)
	run(t, repo, env, "sh", "-c", `sh -c "git add c && git commit -qm 'feat: c'"`)
	if got := trailers(run(t, repo, env, "git", "log", "-1", "--format=%B")); len(got) != 1 {
		t.Errorf("trailers = %q", got)
	}
}

func TestHooksChainToRepositoryHooks(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	hooks := filepath.Join(repo, ".git", "hooks")
	marker := filepath.Join(t.TempDir(), "ran")
	os.WriteFile(filepath.Join(hooks, "prepare-commit-msg"),
		[]byte("#!/bin/sh\necho repo-hook >> \""+marker+"\"\necho 'Ticket: X-1' >> \"$1\"\n"), 0o755)
	os.WriteFile(filepath.Join(hooks, "pre-commit"),
		[]byte("#!/bin/sh\n[ -f BLOCK ] && exit 1\necho pre-commit >> \""+marker+"\"\n"), 0o755)
	env := prepare(t, base, repo, sean)

	msg := commit(t, repo, env, "b", "feat: b")
	if !strings.Contains(msg, "Ticket: X-1") || len(trailers(msg)) != 1 {
		t.Errorf("message = %q; want the repo hook's line and our trailer", msg)
	}
	ran, _ := os.ReadFile(marker)
	if string(ran) != "pre-commit\nrepo-hook\n" {
		t.Errorf("hooks ran = %q", ran)
	}

	os.WriteFile(filepath.Join(repo, "BLOCK"), nil, 0o644)
	run(t, repo, env, "git", "add", "BLOCK")
	cmd := exec.Command("git", "commit", "-qm", "blocked")
	cmd.Dir, cmd.Env = repo, env
	if err := cmd.Run(); err == nil {
		t.Error("a failing repository pre-commit must still refuse the commit")
	}
}

func TestHooksHonourRepositoryHooksPath(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	custom := filepath.Join(repo, ".husky")
	os.MkdirAll(custom, 0o755)
	marker := filepath.Join(t.TempDir(), "ran")
	os.WriteFile(filepath.Join(custom, "commit-msg"), []byte("#!/bin/sh\necho husky >> \""+marker+"\"\n"), 0o755)
	run(t, repo, base, "git", "config", "core.hooksPath", ".husky")
	commit(t, repo, prepare(t, base, repo, sean), "b", "feat: b")
	if ran, _ := os.ReadFile(marker); string(ran) != "husky\n" {
		t.Errorf("husky commit-msg ran = %q", ran)
	}
}

func TestNoCoAuthorMeansNoHooks(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	dir := filepath.Join(t.TempDir(), "run")
	extra, err := Prepare(context.Background(), Plan{Dir: dir, WorkDir: repo, Repo: "acme/widgets", SelfPath: "/bin/false"})
	if err != nil {
		t.Fatal(err)
	}
	if len(extra) != 0 {
		t.Errorf("env = %q, want none", extra)
	}
	if _, err := os.Stat(filepath.Join(dir, "hooks")); !os.IsNotExist(err) {
		t.Error("hooks directory written with no co-author")
	}
}

func TestConcurrentDispatchesInSiblingWorktrees(t *testing.T) {
	base := isolatedGit(t)
	repo := newRepo(t, base)
	authors := []string{sean, "Ada Lovelace <1+ada@users.noreply.github.com>"}
	var wts []string
	for i := range authors {
		wt := filepath.Join(t.TempDir(), "wt")
		run(t, repo, base, "git", "worktree", "add", "-q", "-b", "issue-"+string(rune('a'+i)), wt)
		wts = append(wts, wt)
	}
	envs := make([][]string, len(authors))
	for i := range authors {
		envs[i] = prepare(t, base, wts[i], authors[i])
	}
	var wg sync.WaitGroup
	errs := make([]error, len(authors))
	for i := range authors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range 5 {
				if _, err := tryCommit(wts[i], envs[i], "f"+string(rune('0'+n)), "feat: step"); err != nil {
					errs[i] = err
					return
				}
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worktree %d: %v", i, err)
		}
	}
	for i, wt := range wts {
		log := run(t, wt, base, "git", "log", "-5", "--format=%(trailers:key=Co-authored-by,valueonly)")
		for _, l := range strings.Split(log, "\n") {
			if l != "" && l != authors[i] {
				t.Errorf("worktree %d has trailer %q, want only %q", i, l, authors[i])
			}
		}
		if strings.Count(log, authors[i]) != 5 {
			t.Errorf("worktree %d: %d of 5 commits credited", i, strings.Count(log, authors[i]))
		}
	}
}
