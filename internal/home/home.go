// Package home resolves the machine-wide agent-utils directory.
//
// It exists so the registry and the canonical state database can never disagree
// about where "home" is. They are both read by every command and by every
// detached runner, and a mismatch would split one machine's state across two
// directories.
//
// $AGENT_UTILS_HOME overrides the location. A test needs that: pointing $HOME at
// a temporary directory would also move the home git and ssh use, which the
// agent still needs.
package home

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvVar names the override. It is the machine-wide directory ITSELF, not a
// replacement for $HOME.
//
// Do not confuse it with AGENT_UTILS_DIR, which names ONE PROJECT's
// .agent-utils directory.
const EnvVar = "AGENT_UTILS_HOME"

// DirName is the directory inside the user's home directory.
const DirName = ".agent-utils"

// StateDBFile is the canonical database, holding every project's loop state.
const StateDBFile = "state.db"

// Dir returns the machine-wide agent-utils directory.
//
// An override that names something which exists and is not a directory is an
// error rather than a silent fallback. Falling back would write this machine's
// state somewhere the operator did not ask for, and the mistake would surface
// much later as missing state.
func Dir() (string, error) {
	if env := strings.TrimSpace(os.Getenv(EnvVar)); env != "" {
		info, err := os.Stat(env)
		if err == nil && !info.IsDir() {
			return "", fmt.Errorf("%s is set to %q, which is not a directory", EnvVar, env)
		}
		return env, nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(h, DirName), nil
}

// EnsureDir creates the directory and returns it.
//
// 0700: the directory holds the state database, which carries claude session
// identifiers and the paths of agent transcripts.
func EnsureDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	return dir, nil
}

// StateDBPath returns the canonical state database path.
func StateDBPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, StateDBFile), nil
}

// Resolve returns a path in the one spelling everything compares against:
// absolute, with symlinks resolved.
//
// One file reached by two spellings is otherwise two files to this tool. That
// matters most for the canonical database: the importer decides whether a legacy
// source IS the canonical file by comparing paths, and on a machine whose home
// traverses a symlink (macOS resolves /var to /private/var) the raw and resolved
// spellings differ. The importer would then take the wrong branch and seal a
// source without importing a row.
//
// A path that cannot be resolved is returned as absolute, which is still better
// than the raw string. A file that does not exist yet cannot be resolved at all.
func Resolve(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return real
}
