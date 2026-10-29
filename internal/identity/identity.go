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
	var kvs []gitKV
	var env []string
	if p.CoAuthor != "" {
		if err := os.MkdirAll(p.Dir, 0o700); err != nil {
			return nil, fmt.Errorf("create dispatch directory: %w", err)
		}
		hooksDir, err := writeHooks(ctx, p)
		if err != nil {
			return nil, err
		}
		kvs = append(kvs, gitKV{"core.hooksPath", hooksDir})
	}
	if len(kvs) == 0 {
		return env, nil
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
