package config

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Entry-loop derivation errors. Each is a distinct condition with a distinct
// fix, so each is its own sentinel.
var (
	// ErrNoEntryLoop reports that no loop watching the repository is at the
	// front of its pipeline.
	ErrNoEntryLoop = errors.New("no entry loop")
	// ErrAmbiguousEntryLoop reports that more than one is.
	ErrAmbiguousEntryLoop = errors.New("more than one entry loop")
)

// EntryLoop returns the name of the one loop allowed to promote an issue into
// its trigger label, for the loops of agentUtilsDir that watch repo.
//
// # Why this is derived and not configured
//
// The epic sweep promotes a statusless issue by adding a loop's trigger label.
// If every loop did that for its OWN trigger, the execution loop would promote
// a fresh issue straight to status:ready-for-execution and the planning stage
// would be skipped entirely -- silently, and only for issues that happen to be
// swept. The operator's requirement is that the sweep needs no configuration,
// so the answer has to come from the loop files that already exist.
//
// # The rule
//
// A loop is the entry when its trigger label is not any OTHER loop's terminal
// or review label. A loop whose trigger is another's terminal is downstream of
// it, which is exactly how the reference pair is wired: planning ends at
// status:ready-for-execution, which is execution's trigger.
//
// # Why it fails closed
//
// Zero entry loops, two or more, or any loop file that will not load, all
// return an error and no loop sweeps. A guess would put issues into the wrong
// stage of the pipeline, and nothing downstream would report it: the issue
// would simply be picked up by an agent expecting a plan that was never
// written. A broken file counts because the loop it declares may be the very
// one that makes another downstream, so the graph cannot be trusted with a
// piece missing.
func EntryLoop(agentUtilsDir, repo string) (string, error) {
	entries, err := List(agentUtilsDir)
	if err != nil {
		return "", fmt.Errorf("entry loop for %s: %w", repo, err)
	}
	// Two loops sharing a name is never benign -- Duplicates exists to say so.
	// It matters more here than anywhere else: the derivation below asks "is
	// any OTHER loop's terminal label my trigger", and with a duplicated name
	// the two copies would each exclude the other as "itself". The graph would
	// be computed from a set that silently lost an edge, and the ambiguity
	// message would read "planning, planning are all at the front", which tells
	// an operator nothing.
	if dupes := Duplicates(entries); len(dupes) > 0 {
		return "", fmt.Errorf("entry loop for %s: duplicate loop names: %s: %w",
			repo, strings.Join(dupes, ", "), ErrAmbiguousEntryLoop)
	}

	type loop struct {
		name     string
		trigger  string
		terminal string
		review   string
	}
	var loops []loop
	for _, e := range entries {
		// A file that will not load is fatal even when it names another
		// repository: Entry.Repo is empty when Err is set, so it cannot be
		// filtered out honestly. Refusing is the conservative answer.
		if e.Err != nil {
			return "", fmt.Errorf("entry loop for %s: loop %q does not load: %w",
				repo, e.File, e.Err)
		}
		if !strings.EqualFold(e.Repo, repo) {
			continue
		}
		cfg, err := Load(e.Path)
		if err != nil {
			return "", fmt.Errorf("entry loop for %s: loop %q does not load: %w",
				repo, e.File, err)
		}
		loops = append(loops, loop{
			name:     cfg.Name,
			trigger:  cfg.Labels.Trigger,
			terminal: cfg.Labels.Terminal,
			review:   cfg.Labels.Review,
		})
	}

	var entry []string
	for i, l := range loops {
		downstream := false
		for j, other := range loops {
			// Compared by INDEX, not by name. Duplicates is rejected above, so
			// names are unique by the time this runs -- but identity that does
			// not depend on that invariant cannot be broken by relaxing it.
			if i == j {
				continue
			}
			// Terminal is optional -- the execution loop omits it -- and an
			// empty label matches nothing. Without this guard two loops that
			// both omit it would each look downstream of the other, and a
			// pipeline with no terminal labels at all would resolve to no
			// entry loop.
			if other.terminal != "" && strings.EqualFold(l.trigger, other.terminal) {
				downstream = true
				break
			}
			if other.review != "" && strings.EqualFold(l.trigger, other.review) {
				downstream = true
				break
			}
		}
		if !downstream {
			entry = append(entry, l.name)
		}
	}
	sort.Strings(entry)

	switch len(entry) {
	case 1:
		return entry[0], nil
	case 0:
		// Two quite different conditions, and an operator's fix differs for
		// each: there is no loop watching this repository at all, or there are
		// loops and every one of them is downstream of another. A single
		// message covering both would name neither fix.
		if len(loops) == 0 {
			return "", fmt.Errorf("entry loop for %s: no loop in %s watches this repository: %w",
				repo, agentUtilsDir, ErrNoEntryLoop)
		}
		return "", fmt.Errorf("entry loop for %s: every loop's trigger is another's terminal or review label: %w",
			repo, ErrNoEntryLoop)
	default:
		// NAMED, not counted. An operator cannot act on "it is ambiguous",
		// and this is a permanent misconfiguration rather than a transient
		// failure: it will be logged on every sweep until somebody fixes it.
		return "", fmt.Errorf("entry loop for %s: %s are all at the front of the pipeline: %w",
			repo, strings.Join(entry, ", "), ErrAmbiguousEntryLoop)
	}
}
