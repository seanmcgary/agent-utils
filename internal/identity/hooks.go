package identity

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// stubHooks is every client-side hook git runs from core.hooksPath, except the
// two commit-message hooks writeHooks generates itself. Setting
// core.hooksPath replaces the repository's hook directory outright, so each
// one gets a stub that runs the repository's own; without them husky,
// lefthook and plain .git/hooks checks would silently stop running.
var stubHooks = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch",
	"pre-commit", "pre-merge-commit", "post-commit",
	"pre-rebase", "post-checkout", "post-merge", "pre-push",
	"pre-auto-gc", "post-rewrite", "reference-transaction",
	"post-index-change", "sendemail-validate",
}

// writeHooks writes <Dir>/hooks and returns its path.
//
// The repository's hook directory is resolved here, in the runner, before
// the agent's environment overrides core.hooksPath -- git rev-parse
// --git-path hooks answers with the repository's own core.hooksPath when it
// has one, else <common dir>/hooks.
//
// The co-author is written to a file the hook reads, not into the script, so
// a name with a quote in it is data and never shell.
func writeHooks(ctx context.Context, p Plan) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	cmd.Dir = p.WorkDir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("find the repository's hooks in %s: %w", p.WorkDir, err)
	}
	orig := strings.TrimSpace(string(out))

	dir := filepath.Join(p.Dir, "hooks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create hooks directory: %w", err)
	}
	valueFile := filepath.Join(p.Dir, "co-author")
	if err := os.WriteFile(valueFile, []byte(p.CoAuthor), 0o600); err != nil {
		return "", fmt.Errorf("write co-author: %w", err)
	}

	for _, name := range stubHooks {
		stub := "#!/bin/sh\n" +
			"h=" + shellQuote(filepath.Join(orig, name)) + "\n" +
			"[ -x \"$h\" ] && exec \"$h\" \"$@\"\n" +
			"exit 0\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0o700); err != nil {
			return "", fmt.Errorf("write %s hook: %w", name, err)
		}
	}

	// Both message hooks add the trailer. prepare-commit-msg is the primary
	// path because --no-verify skips commit-msg but not it; but it runs before
	// the editor, so a message written in the editor has no trailer yet and
	// commit-msg adds it. addIfDifferent keeps it to one copy when both run,
	// and across --amend. A rebase is left alone; see trailerHook.
	for _, name := range []string{"prepare-commit-msg", "commit-msg"} {
		script := trailerHook(filepath.Join(orig, name), valueFile)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
			return "", fmt.Errorf("write %s hook: %w", name, err)
		}
	}
	return dir, nil
}

// trailerHook renders a message hook that runs the repository's hook of the
// same name (which can refuse the commit), then adds the co-author trailer.
//
// A message with nothing but comments is left alone: adding the trailer to it
// would make it non-empty and defeat git's "Aborting commit due to empty
// commit message". stripspace reads core.commentChar itself.
//
// Nothing is added while a rebase, cherry-pick or revert is replaying
// commits. The sequencer runs the message hooks for every pick, so a rebase
// the agent runs on a PR branch would otherwise credit the issue author on a
// human's commits too. The agent's own commits already carry the trailer from
// when they were made. --git-path answers for this worktree, not the main one.
func trailerHook(repoHook, valueFile string) string {
	return "#!/bin/sh\n" +
		"h=" + shellQuote(repoHook) + "\n" +
		"if [ -x \"$h\" ]; then \"$h\" \"$@\" || exit $?; fi\n" +
		"[ -d \"$(git rev-parse --git-path rebase-merge)\" ] && exit 0\n" +
		"[ -d \"$(git rev-parse --git-path rebase-apply)\" ] && exit 0\n" +
		"[ -f \"$(git rev-parse --git-path CHERRY_PICK_HEAD)\" ] && exit 0\n" +
		"[ -f \"$(git rev-parse --git-path REVERT_HEAD)\" ] && exit 0\n" +
		"git stripspace --strip-comments <\"$1\" | grep -q . || exit 0\n" +
		"v=$(cat " + shellQuote(valueFile) + ") || exit 1\n" +
		"exec git interpret-trailers --in-place --if-exists addIfDifferent --trailer \"Co-authored-by: $v\" \"$1\"\n"
}
