package wizard

import (
	"embed"
	"fmt"
	"os"

	"github.com/seanmcgary/agent-utils/internal/config"
)

// templateFS embeds the loop configuration templates. A binary installed
// with `go install` has no examples/ directory beside it, so reading
// examples/*.yaml at run time would fail for every real user; go:embed bakes
// the content into the binary at build time instead.
//
//go:embed templates/planning.yaml templates/execution.yaml templates/pr-review.yaml templates/exec-pr-review-findings.yaml
var templateFS embed.FS

// templateNames lists the embedded templates, in the order Templates and the
// wizard's own template question offer them. It is kept in sync with the
// file list embedded into templateFS above by templates_test.go, which loads
// every one of them through config.Load.
//
// The order is the pipeline's own order, and it is the only useful one: each
// loop's trigger is the previous loop's terminal, so an operator scaffolding a
// project reads the list as the chain it is building.
var templateNames = []string{"planning", "execution", "pr-review", "exec-pr-review-findings"}

// Template supplies the prompt bodies and the label and tend defaults that go
// with them.
type Template struct {
	Name         string
	Labels       config.Labels
	TendPR       bool
	Prompt       string
	ResumePrompt string
	TendPrompt   string
}

// Templates returns every embedded template, in the order the wizard offers
// them.
func Templates() []Template {
	out := make([]Template, 0, len(templateNames))
	for _, name := range templateNames {
		t, ok := TemplateNamed(name)
		if !ok {
			// templateNames and the go:embed directive are maintained
			// together in this file; a mismatch is a packaging bug that
			// templates_test.go exists to catch before it ships, not a
			// condition an operator can hit at run time.
			panic(fmt.Sprintf("wizard: embedded template %q is missing", name))
		}
		out = append(out, t)
	}
	return out
}

// TemplateNamed returns the named embedded template.
func TemplateNamed(name string) (Template, bool) {
	known := false
	for _, n := range templateNames {
		if n == name {
			known = true
			break
		}
	}
	if !known {
		return Template{}, false
	}

	cfg, err := loadEmbeddedTemplate(name)
	if err != nil {
		// templates_test.go loads every embedded template through
		// config.Load and fails the build if one does not validate, so
		// reaching this at run time would mean that test did not run.
		panic(fmt.Sprintf("wizard: embedded template %q failed to load: %v", name, err))
	}
	return Template{
		Name:         name,
		Labels:       cfg.Labels,
		TendPR:       cfg.TendPR,
		Prompt:       cfg.Prompt,
		ResumePrompt: cfg.ResumePrompt,
		TendPrompt:   cfg.TendPrompt,
	}, true
}

// loadEmbeddedTemplate decodes an embedded template through config.Load, the
// same strict loader every real configuration file goes through.
// config.Load takes a path, not bytes, so the embedded content is staged to a
// temp file first. Decoding it any other way (a bare yaml.Unmarshal, say)
// would let a template drift from what config.Load actually accepts in
// production — exactly the failure templates_test.go exists to catch.
func loadEmbeddedTemplate(name string) (*config.Config, error) {
	raw, err := templateFS.ReadFile("templates/" + name + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("read embedded template %s: %w", name, err)
	}

	tmp, err := os.CreateTemp("", "wizard-template-*.yaml")
	if err != nil {
		return nil, fmt.Errorf("stage embedded template %s: %w", name, err)
	}
	defer func() {
		_ = os.Remove(tmp.Name()) // best-effort cleanup; a stray temp file costs nothing this function's caller cares about
	}()

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("stage embedded template %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("stage embedded template %s: %w", name, err)
	}

	return config.Load(tmp.Name())
}
