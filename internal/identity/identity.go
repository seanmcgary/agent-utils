// Package identity prepares the git and gh identity one agent dispatch runs
// under.
//
// Everything here is per dispatch and travels in the agent's environment or
// in a directory only that dispatch uses. Nothing is written to the
// repository's .git/config or .git/hooks: every worktree of a repository
// shares those, so two agents working issues by different authors would
// overwrite each other's settings.
package identity

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/ghapp"
)

// RepoEnv names the repository the token helpers serve. It is the only way
// they learn it; see cmd/agent-utils internal gh-token.
const RepoEnv = "AGENT_UTILS_DISPATCH_REPO"

// Plan is what Prepare needs to know about one dispatch.
type Plan struct {
	Dir      string          // per-dispatch directory; Prepare creates it, the caller removes it
	WorkDir  string          // the agent's working directory (a git checkout)
	Repo     string          // owner/name
	SelfPath string          // absolute path of this agent-utils binary
	Home     string          // AGENT_UTILS_HOME to bake into helper commands, "" when unset
	Bot      *ghapp.Identity // nil in token mode
	CoAuthor string          // "Name <email>", "" for none
}

// Prepare writes the dispatch's directory and returns the environment to add
// to the agent's. With no app and no co-author it writes nothing and returns
// nothing, so the agent runs exactly as it did before this package existed.
func Prepare(ctx context.Context, p Plan) ([]string, error) {
	if p.Bot == nil && p.CoAuthor == "" {
		return nil, nil
	}
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create dispatch directory: %w", err)
	}
	var kvs []gitKV
	var env []string
	if p.Bot != nil {
		kvs = append(kvs, appGitConfig(p.SelfPath, p.Home, *p.Bot)...)
		ghConfig := filepath.Join(p.Dir, "gh")
		if err := os.MkdirAll(ghConfig, 0o700); err != nil {
			return nil, err
		}
		binDir, err := writeGhShim(p)
		if err != nil {
			return nil, fmt.Errorf("write gh shim: %w", err)
		}
		env = append(env,
			RepoEnv+"="+p.Repo,
			// An empty config dir, so a gh that somehow bypasses the shim
			// finds no stored login rather than the host user's.
			"GH_CONFIG_DIR="+ghConfig,
			"GH_PROMPT_DISABLED=1",
		)
		if binDir != "" {
			// Appended after agentEnv's PATH; exec keeps the last value of a
			// repeated key.
			env = append(env, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		}
	}
	if p.CoAuthor != "" {
		hooksDir, err := writeHooks(ctx, p)
		if err != nil {
			return nil, err
		}
		kvs = append(kvs, gitKV{"core.hooksPath", hooksDir})
	}
	return append(env, renderGitConfig(kvs)...), nil
}

type gitKV struct{ key, value string }

// renderGitConfig turns kvs into GIT_CONFIG_COUNT and its numbered pairs.
// git applies these above every config file, including the repository's, and
// a repeated key adds a value rather than replacing it, which is what lets
// one key carry two insteadOf rewrites.
func renderGitConfig(kvs []gitKV) []string {
	out := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(len(kvs))}
	for i, kv := range kvs {
		out = append(out,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv.key),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv.value))
	}
	return out
}

// shellQuote quotes s for /bin/sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// helperCommand is the shell prefix that runs this binary's token helpers.
// AGENT_UTILS_HOME is written into it rather than passed in the agent's
// environment: agentEnv withholds that variable from the agent on purpose
// (see runner.Spawn), but the helpers must open the same settings and token
// cache as the daemon that spawned it.
func helperCommand(selfPath, home string) string {
	cmd := shellQuote(selfPath)
	if home != "" {
		cmd = "AGENT_UTILS_HOME=" + shellQuote(home) + " " + cmd
	}
	return cmd
}

// appGitConfig points git at the app: its credential, HTTPS for SSH-style
// remotes, and the bot as the default author and committer.
//
// The empty helper value comes FIRST and is load-bearing: it clears every
// helper inherited from the host's config files for github.com, so an
// osxkeychain or store helper holding a personal token is never asked.
//
// The identity is user.name/user.email, deliberately not GIT_AUTHOR_* in the
// environment: those would override the author of an existing commit on
// --amend and during a rebase, rewriting a human's authorship as the bot's.
func appGitConfig(selfPath, home string, bot ghapp.Identity) []gitKV {
	return []gitKV{
		{"credential.https://github.com.helper", ""},
		{"credential.https://github.com.helper", "!" + helperCommand(selfPath, home) + " internal git-credential"},
		{"url.https://github.com/.insteadOf", "git@github.com:"},
		{"url.https://github.com/.insteadOf", "ssh://git@github.com/"},
		{"user.name", bot.Name},
		{"user.email", bot.Email},
	}
}

// GitEnv is the environment Go-side git runs with in app mode: fetches and
// the automatic rebase's push. It carries no hooks; those commands make no
// new commits.
func GitEnv(selfPath, home, repo string, bot ghapp.Identity) []string {
	return append([]string{RepoEnv + "=" + repo},
		renderGitConfig(appGitConfig(selfPath, home, bot))...)
}

// writeGhShim writes <Dir>/bin/gh. It fetches a token on EVERY invocation:
// a dispatch outlives a token, and the helper returns the cached one unless
// it is within ghapp.RefreshBefore of expiring, so the call is cheap.
func writeGhShim(p Plan) (binDir string, err error) {
	realGh, err := exec.LookPath("gh")
	if err != nil {
		// The runner's stderr goes to its log.
		fmt.Fprintln(os.Stderr, "warning: gh is not on PATH; the agent's gh calls will fail as they would without agent-utils")
		return "", nil
	}
	binDir = filepath.Join(p.Dir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", err
	}
	shim := "#!/bin/sh\n" +
		"GH_TOKEN=$(" + helperCommand(p.SelfPath, p.Home) + " internal gh-token) || exit 1\n" +
		"export GH_TOKEN\n" +
		"exec " + shellQuote(realGh) + " \"$@\"\n"
	return binDir, os.WriteFile(filepath.Join(binDir, "gh"), []byte(shim), 0o700)
}
