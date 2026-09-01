package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/seanmcgary/agent-utils/internal/project"
)

// ErrNoTend reports that a project does not tend its pull requests.
//
// It is a distinct error because the two callers want opposite things from it.
// The listener SKIPS a project that does not tend, silently and on every scan,
// so it must be able to tell "this project chose not to" from "this project is
// misconfigured". An operator who typed `loop tick --name tend` wants a
// sentence saying tending is off and where to switch it on.
var ErrNoTend = errors.New("tending is not enabled for this project")

// TendPath returns the file that stands in for the tend dispatcher's
// configuration: the project descriptor itself.
//
// The tend dispatcher has no configuration FILE -- that is the whole point --
// but every path into a tick takes one: loopcmd.Open is given a path, the
// detached runner is spawned with --config <path>, and `logs`, `status` and
// `tick` all resolve one before they open anything. Rather than teach each of
// those a second shape, the descriptor's own path is what they carry, and
// loopcmd.Open recognises it (IsTendPath) and builds the tend configuration
// instead of parsing a loop file. One seam, in one function, instead of a
// second argument threaded through four commands and a detached process.
func TendPath(agentUtilsDir string) string { return project.Path(agentUtilsDir) }

// IsTendPath reports whether a --config path names the tend dispatcher.
//
// It tests the base name, not the directory, because the descriptor is the only
// file called config.yaml this program ever passes as a --config: loop files
// live in configs/ and are named for their loops, and config.Load's own error
// covers anything else that finds its way here.
func IsTendPath(path string) bool {
	return filepath.Base(path) == project.FileName
}

// LoadTend builds the configuration for a project's tend dispatcher.
//
// # Why this is synthesised rather than read
//
// Tending is project policy, so there is no file to read it from, and inventing
// one would put the policy back in the same shape it just left: a file per
// tender, with a name, that a loop could be pointed at. What a tend needs
// divides cleanly in two, and each half comes from where it belongs.
//
// The AGENT and the WORK -- the eligibility label, the prompt, the harness,
// model, effort and permission mode -- come from the project descriptor's
// tend: block, which is the whole of the policy and the only place it is
// written.
//
// The REPOSITORY FACTS -- which repository, which default branch, where the
// primary checkout is and where worktrees go -- come from the project's loop
// files, which must agree on them. That is not policy leaking back into a loop
// file: those four fields describe the project's checkout, not any loop's
// behaviour, and a project whose loops disagree about which repository they are
// watching is broken for reasons that have nothing to do with tending. So
// disagreement is an ERROR that names the two loops, rather than a silent pick
// of whichever file the directory listing returned first.
//
// Everything else is DEFAULTED here and never inherited from a loop:
//
//   - worktree mode is always per_issue, so a tend gets a worktree of its own
//     for the pull request it rebases. It is namespaced under the dispatcher's
//     reserved name, so <worktree_dir>/tend/pr-N can never collide with a
//     loop's <worktree_dir>/<loop>/issue-N.
//   - retry is off. A tend is never retried: store.KindTend rows are exempt
//     from MarkNeedsRetry, so a retry budget here would be a number nothing
//     reads. The breaker fields carry the minimum the validator accepts for
//     the same reason -- the tend passes discard every retry decision and
//     write no cooldown.
//   - timeout is DefaultAgentTimeout and there is no budget ceiling. See
//     project.Tend for why neither is a field.
//
// The result is validated through the same validate() every loop file goes
// through, so a policy that cannot produce a runnable dispatch is reported
// here, once, rather than in a detached runner an hour later.
func LoadTend(agentUtilsDir string) (*Config, error) {
	pc, err := project.Load(agentUtilsDir)
	if err != nil {
		return nil, err
	}
	if !pc.Tend.Enabled {
		return nil, fmt.Errorf("%w (%s): set tend.enabled to switch it on",
			ErrNoTend, project.Path(agentUtilsDir))
	}

	repoFacts, err := tendRepoFacts(agentUtilsDir)
	if err != nil {
		return nil, err
	}

	// bypassPermissions is not defaulted to, and the empty case is refused
	// here rather than in project.Load: an operator mid-edit may legitimately
	// have a descriptor with tending parked and no mode chosen, and only the
	// dispatcher actually needs the value. claude's own default denies every
	// prompt in a detached `-p` run, so an undeclared mode is not "the safe
	// choice" -- it is a tend that fails at its first `git push` with no
	// explanation anywhere near the file that caused it.
	if strings.TrimSpace(pc.Tend.PermissionMode) == "" {
		return nil, fmt.Errorf(
			"%s enables tending but sets no tend.permission_mode; a tend rebases and "+
				"force-pushes, and claude denies every prompt in a detached run, so a "+
				"tend with no mode fails at its first push",
			project.Path(agentUtilsDir))
	}

	harness := strings.TrimSpace(pc.Tend.Harness)
	if harness == "" {
		harness = HarnessClaude
	}

	cfg := &Config{
		Name:            project.Reserved,
		Repo:            repoFacts.repo,
		CheckoutBaseDir: repoFacts.checkoutBaseDir,
		WorktreeDir:     repoFacts.worktreeDir,
		DefaultBranch:   repoFacts.defaultBranch,
		Tend:            pc.Tend,
		// The tend prompt is the dispatcher's ONLY prompt, so it lands in the
		// field the runner already renders. There is no resume prompt, and
		// nothing asks for one: a tend never resumes -- it gets a fresh
		// session per dispatch -- and validate() below is the thing that would
		// have demanded one, so it is given the same text rather than a stub
		// that could ever be reached and read as instructions.
		Prompt:       pc.Tend.Prompt,
		ResumePrompt: pc.Tend.Prompt,
		Agent: Agent{
			Harness:        harness,
			Model:          pc.Tend.Model,
			Effort:         pc.Tend.Effort,
			PermissionMode: pc.Tend.PermissionMode,
			Worktree:       WorktreePerIssue,
			Timeout:        Duration(DefaultAgentTimeout),
		},
		AcknowledgeBypassPermissions: pc.Tend.AcknowledgeBypassPermissions,
		Retry: Retry{
			Max: 0,
			Breaker: Breaker{
				// The minimum validate() accepts. Neither value is ever read:
				// the tend passes keep only tend decisions, so they count no
				// eligible retries and write no cooldown.
				OrphanThreshold: 1,
				Cooldown:        Duration(DefaultAgentTimeout),
			},
		},
	}
	// labels.trigger, in_flight and blocked are required by validate() and a
	// tend has none of them: it is triggered by a branch moving, it moves no
	// label of its own, and it parks by posting a comment and applying labels
	// its PROMPT names literally. They are filled with the eligibility label so
	// the shared validator has something non-empty to check, and nothing reads
	// them back -- runner.PromptData's Labels are deliberately left empty for a
	// tend, which is why project.Load refuses a tend prompt that mentions them.
	cfg.Labels = Labels{
		Trigger:  pc.Tend.Label,
		InFlight: pc.Tend.Label,
		Blocked:  pc.Tend.Label,
		Terminal: pc.Tend.Label,
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid tend policy in %s: %w", project.Path(agentUtilsDir), err)
	}
	return cfg, nil
}

// repoFacts is where the project's repository lives, as every loop of the
// project agrees it lives.
type repoFacts struct {
	repo            string
	defaultBranch   string
	checkoutBaseDir string
	worktreeDir     string
}

// tendRepoFacts reads the four repository fields off the project's loop files
// and requires them to agree.
//
// A loop that does not LOAD is skipped rather than fatal, matching
// listener.Scan: one unparsable file must not stop the project's tending, and
// the file's own error is already reported wherever loops are listed. A project
// with no loadable loop at all is an error, because there is then nothing that
// says which repository the project even watches.
func tendRepoFacts(agentUtilsDir string) (repoFacts, error) {
	entries, err := List(agentUtilsDir)
	if err != nil {
		return repoFacts{}, err
	}

	var out repoFacts
	// The name of the loop each field was first taken from, so a disagreement
	// can name both sides. An error saying only "the loops disagree about
	// default_branch" leaves the operator to diff every file themselves.
	var from string
	for _, e := range entries {
		if e.Err != nil {
			continue
		}
		cfg, err := Load(e.Path)
		if err != nil {
			continue
		}
		// Resolved against the project the same way loopcmd.Open resolves a
		// loop's, so two files spelling one directory as "." and as an
		// absolute path are not reported as a disagreement.
		checkout, worktrees, err := cfg.ResolveWorkDirs(agentUtilsDir, e.Path)
		if err != nil {
			continue
		}
		cur := repoFacts{
			repo:            cfg.Repo,
			defaultBranch:   cfg.DefaultBranch,
			checkoutBaseDir: checkout,
			worktreeDir:     worktrees,
		}
		if from == "" {
			out, from = cur, cfg.Name
			continue
		}
		for _, d := range []struct{ field, a, b string }{
			{"repo", out.repo, cur.repo},
			{"default_branch", out.defaultBranch, cur.defaultBranch},
			{"checkout_base_dir", out.checkoutBaseDir, cur.checkoutBaseDir},
			{"worktree_dir", out.worktreeDir, cur.worktreeDir},
		} {
			if d.a == d.b {
				continue
			}
			return repoFacts{}, fmt.Errorf(
				"this project's loops disagree about %s, so its tend dispatcher cannot "+
					"tell which repository it maintains: %s says %q and %s says %q",
				d.field, from, d.a, cfg.Name, d.b)
		}
	}
	if from == "" {
		names := Names(entries)
		sort.Strings(names)
		return repoFacts{}, fmt.Errorf(
			"this project enables tending but has no loop configuration that loads, so "+
				"there is nothing that says which repository it watches (found: %s)",
			strings.Join(names, ", "))
	}
	return out, nil
}
