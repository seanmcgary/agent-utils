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
	// Tend is the project's pull-request maintenance policy. omitempty so a
	// descriptor that sets no policy stays the two lines it has always been,
	// rather than growing a block of zero values `project init` never chose.
	Tend Tend `yaml:"tend,omitempty"`
	// Epic is the project's epic-promotion policy.
	Epic Epic `yaml:"epic,omitempty"`
}

// Epic is the project's epic-promotion policy.
//
// It exists for one reason: when a sub-issue closes, exactly ONE loop may add
// the pipeline's first trigger label to the siblings that closure unblocked. If
// every loop did it, the execution loop would promote a fresh issue straight to
// status:ready-for-execution and planning would be skipped -- silently, and only
// for issues that happen to be swept.
//
// That loop used to be DERIVED, by asking which loop's trigger label was no
// other loop's terminal or review label. The derivation worked, and it was the
// wrong shape: it made every loop's label choices depend on every other loop's,
// so renaming one label could silently move the front of the pipeline, or
// produce two candidates and disable promotion entirely with nothing but a log
// line. Loops are meant to be self-contained -- a loop declares its own labels
// and knows nothing about its neighbours, and the chain exists only because an
// operator chose one loop's terminal to be another's trigger.
//
// A cross-loop question therefore cannot be answered inside a loop file. It is
// declared here instead, where the project can see all of its loops.
type Epic struct {
	// Loop names the loop allowed to promote unblocked siblings. Empty means
	// no loop promotes, which is a legitimate configuration for a project that
	// does not use epics -- but it is reported when an epic sweep is attempted,
	// rather than passing silently.
	Loop string `yaml:"loop"`
}

// Tend is the pull-request maintenance policy for a whole project.
//
// It lives HERE, and not in a loop configuration, because that is what it
// actually describes. Tending keeps a repository's open pull requests rebased
// and answers review activity on them; it is a property of the repository's
// pull requests, not of any one loop's issue lifecycle. It used to be a
// per-loop `tend_pr: bool` gated on that loop's labels.review -- which worked
// only because that label happened to mark the right issues, and which meant an
// operator reasoning about "does this repository keep its pull requests fresh"
// had to read every loop file to find out.
//
// Moving it up also removes a class of misconfiguration that had no error
// message: two loops in one project could both set tend_pr, and both would then
// rebase the same branch.
type Tend struct {
	// Enabled turns tending on for the project. Off by default: tending
	// force-pushes branches, and a policy that destructive is opt-in.
	Enabled bool `yaml:"enabled"`

	// Label is the issue label that makes a pull request eligible. It is the
	// explicit statement of what used to be implicit in labels.review: "an
	// issue in THIS state has a pull request worth keeping fresh."
	//
	// Naming it here rather than deriving it is the point. The old gate read
	// whichever label a loop happened to call its review label, so changing a
	// loop's label silently changed what got tended.
	Label string `yaml:"label"`

	// Loop names the loop whose rows host the tend dispatches.
	//
	// The policy above says WHETHER to tend and WHICH pull requests. Something
	// must also say WHERE the resulting dispatches are recorded, because
	// `dispatches`, the live-dispatch guard, the per-pull-request last-tend time
	// and the repeat-conflict rows are all keyed by loop. Two loops answering
	// one project-level policy would each keep half of that state and both
	// force-push the same branch, and nothing downstream would catch it:
	// engine.Decide's liveTendPRs only ever sees the loop it was called for.
	//
	// It is DECLARED and not derived, for the same reason Epic.Loop is. The
	// obvious derivation -- the loop whose labels.review is the tend label --
	// died with labels.review, and every other derivation reintroduces the
	// cross-loop coupling this design removed.
	//
	// Name the loop that last touches the branch. Its worktree, its session and
	// its tend_prompt are the ones that describe the work being maintained.
	Loop string `yaml:"loop"`

	// Harness, Model and Effort say WHICH agent runs a tend, overriding the
	// dispatching loop's `agent:` section for tend dispatches only. Empty
	// means "use the loop's agent setting for this too".
	//
	// These carry ONLY the three fields that say which agent runs. The
	// rejected alternative was repeating every Agent field --
	// permission_mode, worktree, max_budget_usd, timeout, background_tasks --
	// so a tend could diverge from the trigger dispatch in those too. That
	// gives an operator two places to set one thing, and the concrete failure
	// is an operator who sets a timeout in one of them and gets the other's: a
	// tend that quietly inherits the trigger dispatch's stale worktree mode or
	// an unset budget cap because they only remembered to edit `agent:`. Every
	// field this struct does NOT have is deliberately shared with `agent:` for
	// that reason.
	Harness string `yaml:"harness"`
	Model   string `yaml:"model"`
	Effort  string `yaml:"effort"`
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
	// tend.enabled with no tend.label would tend nothing and say nothing about
	// why. There is no default to fall back on: the label names issue states
	// this program did not invent, and guessing one would tend whatever
	// happened to match.
	if c.Tend.Enabled && strings.TrimSpace(c.Tend.Label) == "" {
		return nil, fmt.Errorf("%s sets tend.enabled but no tend.label", Path(agentUtilsDir))
	}
	// Enabled with no host loop would tend nothing and log nothing. The other
	// order -- a loop named while disabled -- is fine and stays fine: it is how
	// an operator parks the policy without deleting it.
	if c.Tend.Enabled && strings.TrimSpace(c.Tend.Loop) == "" {
		return nil, fmt.Errorf("%s sets tend.enabled but no tend.loop", Path(agentUtilsDir))
	}
	if err := validateTendAgent(c.Tend); err != nil {
		return nil, fmt.Errorf("%s: %w", Path(agentUtilsDir), err)
	}
	return &c, nil
}

// validateTendAgent rejects a tend agent setting that names something no
// dispatch could run. The values are the same enumerations the loop
// configuration validates for `agent:`, restated here rather than imported
// because a project descriptor must not depend on the loop-configuration
// package: config already reads project descriptors through the discovery
// path, and the reverse edge would be a cycle.
func validateTendAgent(t Tend) error {
	switch t.Harness {
	case "", "claude", "pi":
	default:
		return fmt.Errorf("tend.harness must be claude or pi, got %q", t.Harness)
	}
	switch t.Effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return fmt.Errorf("tend.effort must be low, medium, high, xhigh or max, got %q", t.Effort)
	}
	return nil
}

// Save writes a project descriptor.
func Save(agentUtilsDir string, c *Config) error {
	raw, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("encode project config: %w", err)
	}
	header := "# Identifies this project to agent-utils, and holds the settings that\n" +
		"# belong to the PROJECT rather than to any one loop: tend: (which pull\n" +
		"# requests to keep rebased, and which loop hosts the dispatches) and\n" +
		"# epic: (which loop may promote unblocked sub-issues). Both are\n" +
		"# cross-loop questions, so no loop file can answer them.\n" +
		"# The name is unique across your machine; the id never changes, so\n" +
		"# renaming or moving the project does not make it a different one.\n" +
		"# See docs/configuration.md for the tend: and epic: fields.\n"
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
