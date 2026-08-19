package loopcmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/project"
	"github.com/seanmcgary/agent-utils/internal/registry"
)

// Project is a resolved project: where it lives and who it is.
type Project struct {
	Config *project.Config
	// Dir is the project's .agent-utils directory.
	Dir string
	// Root is the directory containing Dir.
	Root string
}

// ResolveProject finds the project to act on.
//
// With a selector it looks the project up in the registry, so a command works
// from any directory. Without one it uses the current directory, walking up the
// way git finds .git.
//
// A project found by directory is registered on the way out, so a project
// that moves is re-found rather than recorded twice. It is NOT minted here:
// this used to call project.Ensure, which wrote a fresh descriptor for any
// directory that happened to contain a .agent-utils -- including, before
// config.FindDir learned to skip it, the machine-wide ~/.agent-utils itself.
// `agent-utils project init` is now the only path that creates a project, so
// a directory with no descriptor is reported as an error rather than
// silently adopted. This is backward compatible for every real user: a
// project that was already onboarded has a descriptor on disk and resolves
// exactly as before. Only a directory that was never a project now needs one
// command run in it first.
func ResolveProject(selector string) (*Project, error) {
	if selector != "" {
		p, err := registry.Find(selector)
		if err != nil {
			return nil, err
		}
		cfg, err := project.Load(p.AgentUtilsDir)
		if err != nil {
			return nil, err
		}
		return &Project{Config: cfg, Dir: p.AgentUtilsDir, Root: p.Root}, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	dir, err := config.FindDir(cwd)
	if err != nil {
		return nil, noProjectErr(err)
	}

	cfg, err := project.Load(dir)
	if err != nil {
		if errors.Is(err, project.ErrNoConfig) {
			return nil, noProjectErr(err)
		}
		return nil, err
	}

	out := &Project{Config: cfg, Dir: dir, Root: parentOf(dir)}
	if err := registry.Register(dir, cfg.ID, cfg.Name); err != nil {
		// The registry is an index. Losing an update costs the project a line in
		// `agent-utils list`, never a dispatch.
		return out, nil
	}
	return out, nil
}

// noProjectErr reports that the current directory is not an initialised
// project, in the same shape config.ErrNoDir's own message uses: what went
// wrong, then the exact command that fixes it. Both config.FindDir (no
// .agent-utils anywhere in the parents) and project.Load (a .agent-utils
// with no descriptor) land here, because from the operator's chair they are
// the same problem -- this directory has not been set up -- and deserve the
// same one-line fix rather than two different error shapes for what is
// functionally one gap.
func noProjectErr(err error) error {
	return fmt.Errorf(
		"%w\n\nThis command acts on the project you are in. To set this directory up:\n"+
			"  agent-utils project init\n\n"+
			"Or name a project from anywhere:\n"+
			"  agent-utils project --name <project> ...\n"+
			"  agent-utils list        # what is registered",
		err)
}

func parentOf(agentUtilsDir string) string { return filepath.Dir(agentUtilsDir) }
