// Package registry records which projects this tool has been used against.
//
// It exists so `agent-utils list` can report every onboarded project without
// being told where they are. It is a convenience index, never a source of
// truth: a project's real configuration lives in its own .agent-utils
// directory, and its loop state lives in the canonical database.
//
// Deleting the registry costs the machine-wide sweep its list of projects. A
// forgotten project keeps every row it already has, and is found again the next
// time a command runs inside it.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/seanmcgary/agent-utils/internal/home"
)

// FileName is the registry file inside the user's home .agent-utils directory.
const FileName = "registry.json"

// Project is one recorded project.
type Project struct {
	// ID is the project's stable identifier, minted once in its descriptor. It
	// is the key: a project keeps its identity when renamed or moved.
	ID string `json:"id"`
	// Name identifies the project to a human and is unique across the machine.
	Name string `json:"name"`
	// Root is the directory that contains the .agent-utils directory.
	Root string `json:"root"`
	// AgentUtilsDir is the .agent-utils directory itself.
	AgentUtilsDir string `json:"agent_utils_dir"`
	// FirstSeen is when this project was first used.
	FirstSeen time.Time `json:"first_seen"`
	// LastSeen is when a command last ran against it.
	LastSeen time.Time `json:"last_seen"`
}

// Exists reports whether the project's directory is still present. A project
// that has been moved or deleted stays in the registry until it is pruned, so
// status can say so rather than silently omitting it.
func (p Project) Exists() bool {
	info, err := os.Stat(p.AgentUtilsDir)
	return err == nil && info.IsDir()
}

type file struct {
	Projects []Project `json:"projects"`
}

// Path returns the registry location inside the machine-wide directory.
//
// The registry is always in that directory even when the project's own
// .agent-utils lives elsewhere. A per-project registry could not list the other
// projects, which is the whole point of it.
//
// It resolves the directory through internal/home, so the registry and the
// canonical state database can never disagree about where home is.
func Path() (string, error) {
	dir, err := home.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Register records that a command ran against this project.
//
// It is best effort by contract: the caller should log a failure and carry on.
// Failing a tick because an index could not be updated would trade a real
// operation for a cosmetic one.
//
// Matching is by ID, not by path, so a project that moves is updated in place
// rather than recorded twice.
func Register(agentUtilsDir, id, name string) error {
	abs, err := filepath.Abs(agentUtilsDir)
	if err != nil {
		return err
	}
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create registry directory: %w", err)
	}

	// Several loops can tick at once, each in its own process, so the
	// read-modify-write below has to be exclusive.
	unlock, err := lockRegistry(path)
	if err != nil {
		return err
	}
	defer unlock()

	f, err := read(path)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for i := range f.Projects {
		if f.Projects[i].ID == id || (id == "" && f.Projects[i].AgentUtilsDir == abs) {
			f.Projects[i].Name = name
			f.Projects[i].Root = filepath.Dir(abs)
			f.Projects[i].AgentUtilsDir = abs
			f.Projects[i].LastSeen = now
			return write(path, f)
		}
	}
	f.Projects = append(f.Projects, Project{
		ID:            id,
		Name:          name,
		Root:          filepath.Dir(abs),
		AgentUtilsDir: abs,
		FirstSeen:     now,
		LastSeen:      now,
	})
	return write(path, f)
}

// List returns every recorded project, most recently used first.
func List() ([]Project, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	f, err := read(path)
	if err != nil {
		return nil, err
	}
	sort.Slice(f.Projects, func(i, j int) bool {
		return f.Projects[i].LastSeen.After(f.Projects[j].LastSeen)
	})
	return f.Projects, nil
}

// NameTaken reports whether another project already uses a name. It is what
// keeps project names unique across the machine.
func NameTaken(name string) bool {
	projects, err := List()
	if err != nil {
		return false
	}
	for _, p := range projects {
		if strings.EqualFold(p.Name, name) {
			return true
		}
	}
	return false
}

// Find returns the project matching a name, an id, or a path.
//
// A name is matched first and case-insensitively, because that is what an
// operator types. An unambiguous path is accepted too, so a cron entry can name
// a directory without knowing the project name.
//
// A name matching MORE than one project is an error, never a pick. This used
// to return the first match while List() sorts by LastSeen descending, so a
// registry holding two projects called "lawndominator" answered `project
// --name lawndominator loop reset` with whichever one happened to have ticked
// last -- acting on the wrong repository with nothing in the output saying so.
// `project init` refuses to create such a duplicate, but a registry written
// before that check existed can still hold one, so the read side has to
// refuse too. An id or a path selector stays unambiguous by definition and is
// what the error tells the operator to use.
func Find(selector string) (Project, error) {
	projects, err := List()
	if err != nil {
		return Project{}, err
	}
	if len(projects) == 0 {
		return Project{}, fmt.Errorf("%w: no projects are registered yet", ErrNoProject)
	}

	var names []string
	var byName []Project
	for _, p := range projects {
		names = append(names, p.Name)
		if strings.EqualFold(p.Name, selector) {
			byName = append(byName, p)
		}
	}
	if len(byName) > 1 {
		return Project{}, ambiguousNameErr(selector, byName)
	}
	if len(byName) == 1 {
		return byName[0], nil
	}
	for _, p := range projects {
		if p.ID == selector {
			return p, nil
		}
	}

	// Fall back to a path, exact or as a suffix of the root.
	if abs, err := filepath.Abs(selector); err == nil {
		for _, p := range projects {
			if p.Root == abs || p.AgentUtilsDir == abs {
				return p, nil
			}
		}
	}

	return Project{}, fmt.Errorf("%w %q; known projects: %s",
		ErrNoProject, selector, strings.Join(names, ", "))
}

// ErrNoProject reports that no registered project matched.
var ErrNoProject = errors.New("no such project")

// ErrAmbiguousProject reports that a name matched more than one registered
// project, so the command refused to guess which one was meant.
var ErrAmbiguousProject = errors.New("ambiguous project name")

// ambiguousNameErr lists every candidate with its id and root, because the
// only way out is to re-run the command with one of them: the operator cannot
// choose between two projects they cannot tell apart.
func ambiguousNameErr(selector string, matches []Project) error {
	var lines []string
	for _, p := range matches {
		lines = append(lines, fmt.Sprintf("\n  %s (%s)", p.ID, p.Root))
	}
	return fmt.Errorf("%w %q; it matches %d projects:%s\nselect one by id or path instead",
		ErrAmbiguousProject, selector, len(matches), strings.Join(lines, ""))
}

// Forget removes a project from the registry by name, id, or path. It does not
// touch the project's own files.
func ForgetSelector(selector string) error {
	p, err := Find(selector)
	if err != nil {
		return err
	}
	return Forget(p.AgentUtilsDir)
}

// Forget removes a project from the registry. It does not touch the project's
// own files.
func Forget(agentUtilsDir string) error {
	abs, err := filepath.Abs(agentUtilsDir)
	if err != nil {
		return err
	}
	path, err := Path()
	if err != nil {
		return err
	}
	unlock, err := lockRegistry(path)
	if err != nil {
		return err
	}
	defer unlock()

	f, err := read(path)
	if err != nil {
		return err
	}
	kept := f.Projects[:0]
	for _, p := range f.Projects {
		if p.AgentUtilsDir != abs {
			kept = append(kept, p)
		}
	}
	f.Projects = kept
	return write(path, f)
}

func read(path string) (*file, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &file{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	if len(raw) == 0 {
		return &file{}, nil
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse registry %s: %w", path, err)
	}
	return &f, nil
}

func write(path string, f *file) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode registry: %w", err)
	}
	raw = append(raw, '\n')

	// Write to a temporary file and rename, so a crash mid-write cannot leave a
	// truncated registry behind.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write registry: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace registry: %w", err)
	}
	return nil
}

// lockRegistry takes an exclusive lock on a sidecar file. It blocks rather than
// failing: an update is quick, and a caller that gave up would silently skip
// recording the project.
func lockRegistry(path string) (func(), error) {
	lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open registry lock: %w", err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		lf.Close()
		return nil, fmt.Errorf("lock registry: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		_ = lf.Close()
	}, nil
}
