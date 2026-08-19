package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
//     layout.
//  2. A .agent-utils directory in startDir or any parent of it, the way git
//     finds .git. This is what makes the tool work from a subdirectory.
//
// It deliberately does NOT fall back to $HOME/.agent-utils. Configurations are
// project-local: running in an unrelated directory must say there is no project
// here, not silently adopt some other project's loops. The home directory holds
// the cross-project registry, which is why it exists at all and why falling
// back to it produced a confusing "configs does not exist" error rather than an
// honest "no project here".
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

	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", startDir, err)
	}
	for {
		candidate := filepath.Join(dir, DirName)
		if isDir(candidate) {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break // reached the filesystem root
		}
		dir = parent
	}

	return "", fmt.Errorf(
		"%w in %s or any parent directory", ErrNoDir, startDir)
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
// Two loops sharing a name is never benign: the name keys the state directory,
// the lock file and every database row, so both would write one database and
// contend for one lock while appearing to be separate loops.
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

// StateSubdir is the directory inside DirName that holds per-loop state.
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

// ResolveStateDir returns where this loop keeps its database, lock and logs.
//
// An explicit state_dir wins. Otherwise the directory is derived from the
// configuration file's own location: <project>/.agent-utils/state/<name>.
//
// Deriving it is what keeps state distinct per project. A shared absolute
// state_dir copied between two projects would point both of them at one
// database, so each would see the other's dispatches and issue state.
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
