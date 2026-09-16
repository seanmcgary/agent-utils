package wizard

import (
	"errors"
	"fmt"

	"github.com/seanmcgary/agent-utils/internal/config"
)

// scaffoldCheckoutBaseDir is the checkout_base_dir every scaffolded loop
// gets, for the reason Run's question 3 gives: a relative path is resolved
// against the project root, so "." is the project itself and the generated
// file survives being cloned to another machine. The templates carry an
// ABSOLUTE path to the repository they were written from, which is precisely
// the value that must not be copied.
const scaffoldCheckoutBaseDir = "."

// Scaffold returns a ready-to-write configuration for every embedded
// template, in the pipeline's own order.
//
// It exists because Run throws away nearly everything a template knows.
// TemplateNamed lifts only Labels, Prompt and ResumePrompt, so the agent and
// retry answers are re-asked with generic defaults — and an operator setting
// up a project answers two dozen questions per loop to arrive back at the
// values the template already held. Scaffold takes each template's
// configuration VERBATIM and overrides only the fields that describe this
// machine rather than the loop's behaviour:
//
//   - repo, because a template names the repository it was written from
//   - checkout_base_dir, for the same reason, and absolutely
//   - worktree_dir, pinned rather than trusted, so a template edit cannot
//     leak a path outside the project
//   - state_dir, cleared to the derived default
//
// default_branch is NOT overridden, and Detected.DefaultBranch is
// deliberately ignored: every template names master, and taking the detected
// origin/HEAD instead would hand a project a base branch its loops' prompts
// were not written for. scaffold_test.go pins that.
//
// The permission mode is verbatim too, which means bypassPermissions on all
// four. The caller is responsible for saying so — see the warning
// projectInitRun prints — because Scaffold has nowhere to print.
func Scaffold(d Detected) ([]*config.Config, error) {
	if d.Repo == "" {
		return nil, errors.New(
			"cannot scaffold loops without a repo: run this from a git checkout with an origin remote, " +
				"or use `agent-utils project loop new` to be asked for one")
	}

	out := make([]*config.Config, 0, len(templateNames))
	for _, name := range templateNames {
		cfg, err := loadEmbeddedTemplate(name)
		if err != nil {
			// loadEmbeddedTemplate is the same call TemplateNamed makes, and
			// templates_test.go loads every template through it, so a failure
			// here is a packaging defect rather than an operator's problem.
			return nil, fmt.Errorf("scaffold %s: %w", name, err)
		}

		cfg.Repo = d.Repo
		cfg.CheckoutBaseDir = scaffoldCheckoutBaseDir
		cfg.WorktreeDir = worktreeDirDefault
		cfg.StateDir = ""

		out = append(out, cfg)
	}
	return out, nil
}
