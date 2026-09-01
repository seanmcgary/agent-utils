package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/project"
)

// Epic-loop resolution errors. Each is a distinct condition with a distinct
// fix, so each is its own sentinel.
var (
	// ErrNoEpicLoop reports that the project names no loop to promote epic
	// siblings, or names one that does not watch the repository.
	ErrNoEpicLoop = errors.New("no epic loop")
	// ErrAmbiguousEpicLoop reports that the name it does give cannot be
	// resolved to one loop.
	ErrAmbiguousEpicLoop = errors.New("ambiguous epic loop")
)

// EpicLoop returns the name of the one loop allowed to promote an issue into
// its trigger label, for the loops of agentUtilsDir that watch repo.
//
// # Why exactly one loop
//
// The epic sweep promotes a statusless issue by adding a loop's trigger label.
// If every loop did that for its OWN trigger, the execution loop would promote
// a fresh issue straight to status:ready-for-execution and the planning stage
// would be skipped entirely -- silently, and only for issues that happen to be
// swept.
//
// # Why this is declared and not derived
//
// It used to be derived: a loop was the entry when its trigger label was no
// OTHER loop's terminal or review label. That worked, and it was the wrong
// shape. It made the answer a property of every loop file at once, so renaming
// one label could move the front of the pipeline, or produce two candidates and
// disable promotion for the whole project with nothing but a log line to say
// so. It also required labels.review to exist purely to be read by a different
// loop -- the one cross-loop dependency in a design whose loops are otherwise
// self-contained.
//
// Declaring it in the project descriptor puts a cross-loop question where the
// whole project is visible, and leaves each loop knowing only its own labels.
// The pipeline still chains, but only because an operator chose one loop's
// terminal to be another's trigger; no loop file asserts that, and nothing
// reads it back.
//
// # Why it fails closed
//
// No declaration, a declaration naming a loop that does not exist or watches a
// different repository, duplicate loop names, or any loop file that will not
// load, all return an error and no loop sweeps. A guess would put issues into
// the wrong stage of the pipeline, and nothing downstream would report it: the
// issue would simply be picked up by an agent expecting a plan that was never
// written. A broken file counts because it may be the very loop that was
// named.
func EpicLoop(agentUtilsDir, repo string) (string, error) {
	pc, err := project.Load(agentUtilsDir)
	if err != nil {
		return "", fmt.Errorf("epic loop for %s: %w", repo, err)
	}
	declared := strings.TrimSpace(pc.Epic.Loop)
	if declared == "" {
		return "", fmt.Errorf(
			"epic loop for %s: %s names no epic.loop: %w",
			repo, project.Path(agentUtilsDir), ErrNoEpicLoop)
	}

	entries, err := List(agentUtilsDir)
	if err != nil {
		return "", fmt.Errorf("epic loop for %s: %w", repo, err)
	}
	// Two loops sharing a name is never benign -- Duplicates exists to say so.
	// It matters more here than anywhere else: the declaration is a NAME, so a
	// duplicated one names two different loops with two different trigger
	// labels, and promoting into either would be a guess.
	//
	// This rejection is GLOBAL, not scoped to repo: entries spans every loop
	// List finds in agentUtilsDir, and Duplicates is asked about all of them
	// before the repo filter below ever runs. A duplicate name among another
	// repository's loops therefore stops THIS repository's sweep too, even
	// though its own configuration may be fine. That is fail-closed and
	// deliberate, the same shape of decision as the unloadable-file case below.
	if dupes := Duplicates(entries); len(dupes) > 0 {
		return "", fmt.Errorf("epic loop for %s: duplicate loop names: %s: %w",
			repo, strings.Join(dupes, ", "), ErrAmbiguousEpicLoop)
	}

	for _, e := range entries {
		// A file that will not load is fatal even when it names another
		// repository: Entry.Repo is empty when Err is set, so it cannot be
		// filtered out honestly, and it may be the loop that was declared.
		// Refusing is the conservative answer.
		if e.Err != nil {
			return "", fmt.Errorf("epic loop for %s: loop %q does not load: %w",
				repo, e.File, e.Err)
		}
		if !strings.EqualFold(e.Name, declared) {
			continue
		}
		// Named, but watching a different repository. Promotion writes labels
		// by issue number against a repository, so a foreign loop's number
		// would label whichever local issue happened to carry it.
		if !strings.EqualFold(e.Repo, repo) {
			return "", fmt.Errorf(
				"epic loop for %s: %s names epic.loop %q, which watches %s: %w",
				repo, project.Path(agentUtilsDir), declared, e.Repo, ErrNoEpicLoop)
		}
		return e.Name, nil
	}

	return "", fmt.Errorf(
		"epic loop for %s: %s names epic.loop %q, and no loop by that name exists: %w",
		repo, project.Path(agentUtilsDir), declared, ErrNoEpicLoop)
}
