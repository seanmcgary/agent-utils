package loopcmd

import (
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
	// Created reports that this call minted the project's descriptor.
	Created bool
	// RenamedFrom is set when the directory's own name was already taken by
	// another project and a suffix had to be added.
	RenamedFrom string
}

// ResolveProject finds the project to act on.
//
// With a selector it looks the project up in the registry, so a command works
// from any directory. Without one it uses the current directory, walking up the
// way git finds .git.
//
// A project found by directory is registered on the way out, and gets a
// descriptor minted if it has none. That is what onboards a project: there is
// no separate init step.
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
		return nil, fmt.Errorf(
			"%w\n\nThis command acts on the project you are in. To set this directory up:\n"+
				"  mkdir -p %s/%s\n\n"+
				"Or name a project from anywhere:\n"+
				"  agent-utils project --name <project> ...\n"+
				"  agent-utils list        # what is registered",
			err, config.DirName, config.ConfigsSubdir)
	}

	before := project.Slug(baseOf(dir))
	cfg, created, err := project.Ensure(dir, registry.NameTaken)
	if err != nil {
		return nil, err
	}

	out := &Project{Config: cfg, Dir: dir, Root: parentOf(dir), Created: created}
	if created && cfg.Name != before {
		out.RenamedFrom = before
	}
	if err := registry.Register(dir, cfg.ID, cfg.Name); err != nil {
		// The registry is an index. Losing an update costs the project a line in
		// `agent-utils list`, never a dispatch.
		return out, nil
	}
	return out, nil
}

func baseOf(agentUtilsDir string) string   { return filepath.Base(filepath.Dir(agentUtilsDir)) }
func parentOf(agentUtilsDir string) string { return filepath.Dir(agentUtilsDir) }
