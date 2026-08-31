package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/home"
)

// DirName is the directory that holds this tool's local files.
const DirName = ".agent-utils"

// ConfigsSubdir is the directory inside DirName that holds loop configurations.
const ConfigsSubdir = "configs"

// Discovery errors. Each is a distinct condition with a distinct fix, so the
// caller can say something useful rather than "not found".
var (
	// ErrNoDir reports that no .agent-utils directory was found.
	ErrNoDir = errors.New("no " + DirName + " directory found")
	// ErrNoConfigs reports that the directory exists but holds no configuration.
	ErrNoConfigs = errors.New("no loop configurations found")
	// ErrNotFound reports that a named configuration does not exist.
	ErrNotFound = errors.New("no such loop configuration")
	// ErrAmbiguous reports that several configurations exist and none was named.
	ErrAmbiguous = errors.New("several loop configurations found and none was named")
	// ErrDuplicateName reports that more than one file declares the same name.
	ErrDuplicateName = errors.New("duplicate loop name")
)

// Entry is one discovered configuration file.
type Entry struct {
	// Name is the loop's name: the `name` field INSIDE the file, not the file
	// name. It is how a configuration is selected on the command line, and it
	// is what the loop keys its database rows and its state directory on.
	//
	// When the file cannot be loaded there is no name to read, so this falls
	// back to the file's base name purely so the entry can be reported.
	Name string
	// File is the base file name. It is shown when it differs from Name, so a
	// configuration can be traced back to the file that declares it.
	File string
	// Path is the absolute path to the file.
	Path string
	// Repo is the repository the loop watches. It is empty when Err is set.
	Repo string
	// DefaultBranch and TendPR are copied off the same Load that fills in Repo.
	// The listener needs both to answer a push delivery without opening a
	// database: a push to a branch no loop tends must cost one field test, not
	// a token read, a SQLite handle, and a migration check. They are empty and
	// false when Err is set, like Repo.
	DefaultBranch string
	TendPR        bool
	// Err records why the file could not be loaded. Listing reports a broken
	// configuration rather than hiding it, because a file that silently does
	// not appear is harder to debug than one that appears with its error.
	Err error
}

// FindDir locates the project's .agent-utils directory.
//
// It looks in two places, in order:
//
//  1. $AGENT_UTILS_DIR, when set. This is the escape hatch for an unusual
//     layout, and it is trusted to name any directory -- including the
//     machine-wide one, if that is really what the caller wants.
//  2. A .agent-utils directory in startDir or any parent of it, the way git
//     finds .git. This is what makes the tool work from a subdirectory.
//
// Step 2 skips the machine-wide directory (internal/home.Dir()) when the
// walk reaches it. That directory is an ORDINARY ancestor of everything under
// $HOME, so an unguarded walk-up would silently adopt it as the project
// directory for any command run outside a project -- e.g. from
// ~/Downloads/scratch, once any project on the machine has been used and so
// created ~/.agent-utils. The caller would then write a project descriptor
// into the same directory the registry and the canonical state database live
// in. Configurations are project-local: running in an unrelated directory
// must say there is no project here, not silently adopt the machine-wide one.
//
// The comparison resolves symlinks on both sides (internal/home.Resolve).
// FindDir's only caller feeds it os.Getwd(), and on Darwin os.Getwd returns
// the fully resolved spelling (/private/var/...) whenever $PWD is unset --
// which is exactly the case for a launchd-started process such as the
// listener daemon. A raw string compare against home.Dir()'s unresolved
// /var/... spelling would then never match, and the guard above would fail
// open: it existed but silently let the machine-wide directory through.
//
// A cron entry should pass --config with an absolute path, which needs no
// discovery at all.
func FindDir(startDir string) (string, error) {
	if env := strings.TrimSpace(os.Getenv("AGENT_UTILS_DIR")); env != "" {
		if isDir(env) {
			return env, nil
		}
		return "", fmt.Errorf("%w: AGENT_UTILS_DIR is set to %q, which is not a directory",
			ErrNoDir, env)
	}

	// A raw string compare is not enough: home.Dir() and the walk candidate
	// can name one directory in two spellings, and the guard would then
	// silently pass the machine-wide directory through. An unresolvable
	// machineWide (no machine-wide directory at all, or home.Dir() erroring)
	// degrades to "", which cannot spuriously match an existing candidate,
	// so the walk-up is simply unguarded -- there is nothing to protect.
	machineWide := ""
	if dir, err := home.Dir(); err == nil {
		machineWide = home.Resolve(dir)
	}

	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", startDir, err)
	}
	for {
		candidate := filepath.Join(dir, DirName)
		// home.Resolve(candidate) is safe here because isDir(candidate) has
		// already established the path exists, so EvalSymlinks succeeds.
		if isDir(candidate) && (machineWide == "" || home.Resolve(candidate) != machineWide) {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // reached the filesystem root
		}
		dir = parent
	}

	// The message used to end "; run `agent-utils project init` to create
	// one" here too. loopcmd.ResolveProject wraps this error in a fuller
	// message that already names that command, so the same instruction
	// would otherwise print twice on the one path that actually reaches an
	// operator (a bare FindDir caller has no such wrapper today, but this is
	// a location, not a fix; the fix belongs to the caller that knows what a
	// project is).
	return "", fmt.Errorf("%w in %s or any parent directory", ErrNoDir, startDir)
}

// ConfigsDir returns the configurations directory inside a .agent-utils dir.
func ConfigsDir(agentUtilsDir string) string {
	return filepath.Join(agentUtilsDir, ConfigsSubdir)
}

// List returns every configuration in a .agent-utils directory, sorted by name.
//
// A file that fails to load is still listed, with its error in Entry.Err.
func List(agentUtilsDir string) ([]Entry, error) {
	configs := ConfigsDir(agentUtilsDir)
	if !isDir(configs) {
		return nil, fmt.Errorf("%w: %s does not exist", ErrNoConfigs, configs)
	}

	items, err := os.ReadDir(configs)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", configs, err)
	}

	var out []Entry
	for _, item := range items {
		if item.IsDir() {
			continue
		}
		ext := filepath.Ext(item.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(configs, item.Name())
		entry := Entry{
			// Placeholder until the file is read. A file that fails to load has
			// no name field to report, so the file name stands in for it.
			Name: strings.TrimSuffix(item.Name(), ext),
			File: item.Name(),
			Path: path,
		}
		if cfg, err := Load(path); err != nil {
			entry.Err = err
		} else {
			entry.Name = cfg.Name
			entry.Repo = cfg.Repo
			entry.DefaultBranch = cfg.DefaultBranch
			entry.TendPR = cfg.TendPR
		}
		out = append(out, entry)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s holds no .yaml or .yml file", ErrNoConfigs, configs)
	}
	return out, nil
}

// Resolve returns the path of the configuration whose name field is name.
func Resolve(agentUtilsDir, name string) (string, error) {
	entries, err := List(agentUtilsDir)
	if err != nil {
		return "", err
	}

	var matches []Entry
	for _, e := range entries {
		if e.Name == name {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].Path, nil
	case 0:
		return "", fmt.Errorf("%w %q in %s; available: %s",
			ErrNotFound, name, ConfigsDir(agentUtilsDir), strings.Join(Names(entries), ", "))
	default:
		files := make([]string, 0, len(matches))
		for _, m := range matches {
			files = append(files, m.File)
		}
		return "", fmt.Errorf("%w: %d files declare the name %q (%s)",
			ErrDuplicateName, len(matches), name, strings.Join(files, ", "))
	}
}

// Duplicates returns every name declared by more than one file, sorted.
//
// Two loops sharing a name is never benign. The name is half the key of every
// row a loop owns, together with the project, so both would read and write one
// another's issue state and dispatches. It also names the lock file and the log
// tree, so they would contend for one lock and write into one directory.
func Duplicates(entries []Entry) []string {
	seen := map[string]int{}
	for _, e := range entries {
		seen[e.Name]++
	}
	var out []string
	for name, n := range seen {
		if n > 1 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Names returns the name of each entry.
func Names(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

// StateSubdir is the directory inside DirName that holds each loop's tick lock
// and log tree. Loop state itself lives in the canonical database.
const StateSubdir = "state"

// DirFromPath returns the .agent-utils directory a file lives under, or "" when
// the file is not inside one. It walks up from the file, so it works for a
// config in .agent-utils/configs and for one nested deeper.
func DirFromPath(path string) string {
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return ""
	}
	for {
		if filepath.Base(dir) == DirName && isDir(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// ResolveStateDir returns where this loop keeps its tick lock and its logs.
//
// An explicit state_dir wins. Otherwise the directory is derived from the
// configuration file's own location: <project>/.agent-utils/state/<name>.
//
// It no longer decides where state is kept. Loop state lives in the one
// canonical database, and every row there is keyed by the project's identifier,
// so two loops that share a state_dir share a lock and a log tree, never state.
// They must still have different names: the name is the other half of that key.
func (c *Config) ResolveStateDir(configPath string) (string, error) {
	if strings.TrimSpace(c.StateDir) != "" {
		return expandHome(c.StateDir)
	}
	if dir := DirFromPath(configPath); dir != "" {
		return filepath.Join(dir, StateSubdir, c.Name), nil
	}
	return "", fmt.Errorf(
		"state_dir is required for %s: it is not inside a %s directory, so a "+
			"per-project state directory cannot be derived", configPath, DirName)
}

// ResolveWorkDirs returns checkout_base_dir and worktree_dir as absolute
// paths, resolved against the project root the way state_dir already is.
//
// agentUtilsDir is the project's .agent-utils directory, so the project root
// is its parent. configPath names the file only so an error can point at it.
//
// A relative value used raw resolves against the working directory of
// whichever process reads the configuration, and the three processes that read
// it do not share one:
//
//   - a CLI command run inside the project -- the project root, correct only
//     by luck;
//   - `--name <project>` run from anywhere else -- the operator's shell;
//   - the listener daemon -- ~/.agent-utils, because its launchd plist sets
//     WorkingDirectory to the machine-wide directory. A relative
//     checkout_base_dir there silently means ~/.agent-utils, and
//     checkout_base_dir becomes the agent's cmd.Dir, so the daemon would run
//     the agent in the directory holding the registry and the state database
//     rather than in the repository.
//
// Resolving here, from the project, makes every one of those contexts produce
// the same absolute path.
func (c *Config) ResolveWorkDirs(agentUtilsDir, configPath string) (checkout, worktrees string, err error) {
	checkout, err = resolveProjectPath("checkout_base_dir", c.CheckoutBaseDir, agentUtilsDir, configPath)
	if err != nil {
		return "", "", err
	}
	worktrees, err = resolveProjectPath("worktree_dir", c.WorktreeDir, agentUtilsDir, configPath)
	if err != nil {
		return "", "", err
	}
	return checkout, worktrees, nil
}

// resolveProjectPath is ResolveWorkDirs for one field. An absolute path is
// returned unchanged, so every configuration written before this existed is
// unaffected; a leading ~ expands as state_dir's does.
func resolveProjectPath(field, value, agentUtilsDir, configPath string) (string, error) {
	expanded, err := expandHome(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(expanded) {
		return expanded, nil
	}
	if agentUtilsDir == "" {
		return "", fmt.Errorf(
			"%s is relative (%q) in %s, which is not inside a %s directory, so there "+
				"is no project root to resolve it against; use an absolute path",
			field, expanded, configPath, DirName)
	}
	return filepath.Join(filepath.Dir(agentUtilsDir), expanded), nil
}

// expandHome expands a leading ~ so a configuration can be written portably.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", path, err)
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/")), nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
