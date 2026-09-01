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
	"text/template"

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

// Reserved is the loop name the project's tend dispatcher owns.
//
// Tending is not a loop, but every row this program writes is keyed by
// (project, loop): dispatches, issue state, pr_links, tend_conflicts, ticks and
// cooldowns all carry a `loop TEXT NOT NULL` column, and the dispatch index is
// (project_id, loop, repo, status). A dispatcher with no name of its own would
// have to borrow a loop's, which is exactly the arrangement this design
// removed -- two writers each keeping half of one state. So the tend dispatcher
// HAS a name, that name is not a loop file, and no loop may take it:
// config.Load rejects a loop whose `name` is this one, naming the reason.
//
// A reserved name rather than a new column, deliberately. The alternative -- a
// `kind` column beside `loop`, or a nullable loop -- is a schema migration
// across six tables and every index over them, to express something the
// existing key already expresses: "the rows belonging to this dispatcher, in
// this project."
const Reserved = "tend"

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
//
// It is also the WHOLE description of the tend dispatcher's agent, and that is
// the second half of the same move. There was briefly a `tend.loop` naming a
// host loop, so that the agent fields below could fall through to that loop's
// `agent:` section and the tend prompt could live in its file. That setting
// existed only because tending still ran inside loop ticks; with a dispatcher
// of its own there is no host, nothing to fall through TO, and nothing left for
// a loop file to say about tending. What a tend does not declare here is
// DEFAULTED (see Load), never inherited: an inheritance rule would have to pick
// a loop, and picking one is the coupling this removed.
//
// What is NOT here is deliberate too. There is no `worktree` mode: a tend
// always runs in a worktree of its own for the pull request it is rebasing,
// under the dispatcher's own name, so the choice could only ever be set wrong.
// There is no `max_budget_usd` or `timeout`: both have safe defaults (no
// ceiling, and config.DefaultAgentTimeout), and a budget cap that lands
// mid-rebase leaves a half-resolved conflict, which is worse than the run it
// refused. There is no `veto` list: the loops' veto lists name one another's
// states, so a union of them vetoes every status label the pipeline has --
// including the tend label itself -- and the eligibility label, the draft
// check, and the project-wide live-dispatch and stopped guards are what gate a
// tend instead.
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

	// Prompt is the template the tend agent runs. It moved here out of the
	// host loop's `tend_prompt`, because a loop that does not tend has no
	// business carrying one, and because with no host loop there was no file
	// left that could claim it.
	//
	// It renders runner.PromptData like any other prompt, with one difference
	// that has teeth: there is no loop, so there are no loop LABELS. A
	// project-level prompt naming {{.Labels.Trigger}} would render an empty
	// string and instruct the agent to remove a label called "", so Load
	// REJECTS a prompt mentioning .Labels rather than letting it render blank.
	// Name the labels literally; {{.Tend.Label}} carries the eligibility label.
	Prompt string `yaml:"prompt"`

	// Harness, Model and Effort say WHICH agent runs a tend.
	//
	// There is no loop `agent:` section behind them any more. Harness defaults
	// to claude, exactly as agent.harness does; Effort is optional and means
	// "pass no effort level"; Model is REQUIRED when tending is enabled,
	// because which model a tend runs on is the one question this policy
	// exists to answer and there is no longer anything to inherit it from.
	Harness string `yaml:"harness"`
	Model   string `yaml:"model"`
	Effort  string `yaml:"effort"`

	// PermissionMode is claude's permission mode for a tend dispatch.
	//
	// Declared rather than defaulted, because a tend rebases, force-pushes and
	// replies on GitHub: it is the most destructive dispatch this program
	// makes, and the mode it runs under must be a sentence an operator wrote.
	// It is also the one agent setting with no safe default -- claude's own
	// default denies every prompt in a detached `-p` run, so a tend left
	// undeclared would fail at the first `git push` rather than do nothing.
	PermissionMode string `yaml:"permission_mode"`

	// AcknowledgeBypassPermissions must be true to select bypassPermissions,
	// for the reason the loop configuration's own acknowledgement exists: the
	// tend agent reads pull request review threads written by third parties,
	// and bypassPermissions runs whatever they contain with no gate.
	AcknowledgeBypassPermissions bool `yaml:"i_understand_bypass_permissions"`
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
	// The prompt and the model are required for the same reason the label is:
	// the tend dispatcher has no loop behind it, so there is no `tend_prompt`
	// and no `agent.model` left to fall back on. Enabled without either is a
	// dispatcher that would fail at its first dispatch, in a detached process,
	// with the reason a long way from the file that caused it.
	if c.Tend.Enabled && strings.TrimSpace(c.Tend.Prompt) == "" {
		return nil, fmt.Errorf("%s sets tend.enabled but no tend.prompt", Path(agentUtilsDir))
	}
	if c.Tend.Enabled && strings.TrimSpace(c.Tend.Model) == "" {
		return nil, fmt.Errorf("%s sets tend.enabled but no tend.model", Path(agentUtilsDir))
	}
	if err := validateTendAgent(c.Tend); err != nil {
		return nil, fmt.Errorf("%s: %w", Path(agentUtilsDir), err)
	}
	if err := validateTendPrompt(c.Tend.Prompt); err != nil {
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
	switch t.PermissionMode {
	case "", "acceptEdits", "auto", "manual", "dontAsk", "plan":
	case "bypassPermissions":
		if !t.AcknowledgeBypassPermissions {
			return errors.New(
				"tend.permission_mode is \"bypassPermissions\", which disables every " +
					"permission prompt on third-party pull request review text; set " +
					"tend.i_understand_bypass_permissions: true to confirm")
		}
	default:
		return fmt.Errorf(
			"tend.permission_mode %q is not a valid claude permission mode", t.PermissionMode)
	}
	// Enabled but with no permission mode is accepted here and reported by the
	// dispatcher instead: an operator parking a policy mid-edit should not be
	// stopped by a field whose absence only bites at dispatch time. See
	// config.LoadTend, which is the path that actually needs a usable value.
	return nil
}

// labelsRef is what a tend prompt may not contain.
//
// A tend prompt is rendered against runner.PromptData like any other, but a
// tend has no loop, so PromptData.Labels is the zero value. text/template
// renders a zero struct field as the empty string rather than failing, so a
// prompt carrying over the old host loop's "remove {{.Labels.Trigger}}" would
// silently instruct the agent to act on a label named "". Rejecting the
// reference at load time is what turns that into a sentence an operator reads
// once, in the file, instead of a park comment nobody can explain.
const labelsRef = ".Labels"

func validateTendPrompt(prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return nil
	}
	if _, err := template.New("tend.prompt").Parse(prompt); err != nil {
		return fmt.Errorf("tend.prompt: %w", err)
	}
	if strings.Contains(prompt, labelsRef) {
		return errors.New(
			"tend.prompt references " + labelsRef + ", and a tend has no loop, so there " +
				"are no loop labels to render: the reference would silently become an " +
				"empty string.\nName the labels literally, and use {{.Tend.Label}} for " +
				"the eligibility label")
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
		"# requests to keep rebased, and the agent and prompt that do it) and\n" +
		"# epic: (which loop may promote unblocked sub-issues). Neither is a\n" +
		"# loop's question, so no loop file can answer them -- tending is its\n" +
		"# own dispatcher, described entirely by the tend: block below.\n" +
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
