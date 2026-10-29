package identity

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// clientHooks is every client-side hook git runs from core.hooksPath. Setting
// core.hooksPath replaces the repository's hook directory outright, so each
// one gets a stub that runs the repository's own; without them husky,
// lefthook and plain .git/hooks checks would silently stop running.
var clientHooks = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch",
	"pre-commit", "pre-merge-commit", "commit-msg", "post-commit",
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

	for _, name := range clientHooks {
		stub := "#!/bin/sh\n" +
			"h=" + shellQuote(filepath.Join(orig, name)) + "\n" +
			"[ -x \"$h\" ] && exec \"$h\" \"$@\"\n" +
			"exit 0\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0o700); err != nil {
			return "", fmt.Errorf("write %s hook: %w", name, err)
		}
	}

	// The repository's prepare-commit-msg runs first and can refuse the
	// commit. addIfDifferent keeps the trailer to one copy across --amend,
	// rebase rewords and squashes.
	pcm := "#!/bin/sh\n" +
		"h=" + shellQuote(filepath.Join(orig, "prepare-commit-msg")) + "\n" +
		"if [ -x \"$h\" ]; then \"$h\" \"$@\" || exit $?; fi\n" +
		"v=$(cat " + shellQuote(valueFile) + ") || exit 1\n" +
		"exec git interpret-trailers --in-place --if-exists addIfDifferent --trailer \"Co-authored-by: $v\" \"$1\"\n"
	if err := os.WriteFile(filepath.Join(dir, "prepare-commit-msg"), []byte(pcm), 0o700); err != nil {
		return "", fmt.Errorf("write prepare-commit-msg hook: %w", err)
	}
	return dir, nil
}
