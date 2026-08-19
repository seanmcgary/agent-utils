// Package project describes a project: the directory that holds a
// .agent-utils directory, plus the identity recorded inside it.
//
// A project has both a name and an identifier. The name is for humans and is
// unique across the machine, so `--project lawndominator` is unambiguous. The
// identifier is a UUID minted once and never changed, so a project keeps its
// identity when it is renamed or moved.
package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// FileName is the project descriptor inside a .agent-utils directory.
//
// Note the singular: config.yaml describes the PROJECT, while configs/ holds
// the loop configurations.
const FileName = "config.yaml"

// Config is the project descriptor.
type Config struct {
	// Name identifies the project to a human. It is unique across the machine.
	Name string `yaml:"name"`
	// ID is a UUID minted at first use. It never changes, so renaming or moving
	// a project does not make it a different one.
	ID string `yaml:"id"`
}

// ErrNoConfig reports that a project has no descriptor yet.
var ErrNoConfig = errors.New("project has no " + FileName)

// Path returns the descriptor's location inside a .agent-utils directory.
func Path(agentUtilsDir string) string {
	return filepath.Join(agentUtilsDir, FileName)
}

// Load reads a project descriptor.
func Load(agentUtilsDir string) (*Config, error) {
	raw, err := os.ReadFile(Path(agentUtilsDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w at %s", ErrNoConfig, Path(agentUtilsDir))
	}
	if err != nil {
		return nil, fmt.Errorf("read project config: %w", err)
	}

	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", Path(agentUtilsDir), err)
	}
	if strings.TrimSpace(c.Name) == "" {
		return nil, fmt.Errorf("%s has no name", Path(agentUtilsDir))
	}
	if strings.TrimSpace(c.ID) == "" {
		return nil, fmt.Errorf("%s has no id", Path(agentUtilsDir))
	}
	return &c, nil
}

// Save writes a project descriptor.
func Save(agentUtilsDir string, c *Config) error {
	raw, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode project config: %w", err)
	}
	header := "# Identifies this project to agent-utils.\n" +
		"# The name is unique across your machine; the id never changes, so\n" +
		"# renaming or moving the project does not make it a different one.\n"
	if err := os.WriteFile(Path(agentUtilsDir), append([]byte(header), raw...), 0o600); err != nil {
		return fmt.Errorf("write project config: %w", err)
	}
	return nil
}

// EnsureNamed loads a project's descriptor, creating one when it does not
// exist. A new project is named base; when base is already taken by another
// project, a numeric suffix is added until it is unique, starting at 2, and
// renamedFrom is set to base so the caller can report what happened. taken
// reports whether a name is already in use elsewhere.
//
// It returns the descriptor and whether it had to be created; renamedFrom is
// only ever set when created is true.
func EnsureNamed(agentUtilsDir, base string, taken func(name string) bool) (c *Config, created bool, renamedFrom string, err error) {
	c, err = Load(agentUtilsDir)
	if err == nil {
		return c, false, "", nil
	}
	if !errors.Is(err, ErrNoConfig) {
		return nil, false, "", err
	}

	if base == "" {
		base = "project"
	}
	name := base
	for i := 2; taken(name); i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}

	c = &Config{Name: name, ID: uuid.NewString()}
	if err := Save(agentUtilsDir, c); err != nil {
		return nil, false, "", err
	}
	if name != base {
		renamedFrom = base
	}
	return c, true, renamedFrom, nil
}

// unsafeChars matches everything a project name may not contain. A name is
// typed on a command line, so it stays to characters that need no quoting.
var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// Slug turns a directory name into a usable project name.
func Slug(s string) string {
	s = unsafeChars.ReplaceAllString(strings.TrimSpace(s), "-")
	s = strings.Trim(s, "-._")
	return s
}
