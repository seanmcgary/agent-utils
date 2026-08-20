package wizard

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A broken embedded template must fail the build, not ship. This loads every
// file named by the go:embed directive in templates.go through config.Load —
// the same strict loader a real configuration file goes through — and fails
// if any of them does not validate.
func TestEmbeddedTemplatesLoadThroughConfigLoad(t *testing.T) {
	for _, name := range templateNames {
		t.Run(name, func(t *testing.T) {
			if _, err := loadEmbeddedTemplate(name); err != nil {
				t.Fatalf("embedded template %q does not validate: %v", name, err)
			}
		})
	}
}

func TestTemplateNamed(t *testing.T) {
	for _, name := range templateNames {
		tmpl, ok := TemplateNamed(name)
		if !ok {
			t.Fatalf("TemplateNamed(%q) = _, false", name)
		}
		if tmpl.Prompt == "" || tmpl.ResumePrompt == "" {
			t.Fatalf("template %q has an empty prompt or resume_prompt", name)
		}
		if tmpl.Labels.Trigger == "" {
			t.Fatalf("template %q has an empty labels.trigger", name)
		}
	}

	if _, ok := TemplateNamed("does-not-exist"); ok {
		t.Fatal(`TemplateNamed("does-not-exist") = _, true`)
	}
}

func TestTemplates(t *testing.T) {
	got := Templates()
	if len(got) != len(templateNames) {
		t.Fatalf("Templates() returned %d templates, want %d", len(got), len(templateNames))
	}
	for i, tmpl := range got {
		if tmpl.Name != templateNames[i] {
			t.Fatalf("Templates()[%d].Name = %q, want %q", i, tmpl.Name, templateNames[i])
		}
	}
}

// examples/*.yaml and internal/wizard/templates/*.yaml are two copies of the
// same files: go:embed cannot reach a parent directory, so the wizard's
// templates cannot simply BE the examples. Nothing pins them together, and
// only the embedded copies are validated (above) -- so an edit to one silently
// leaves the other stale, and the examples are what the README points a new
// operator at.
//
// Byte-identical, not merely equivalent: the header comments and field order
// are part of what an operator reads, and a comparison of loaded values would
// pass while the two files told different stories.
func TestExamplesMatchTheEmbeddedTemplates(t *testing.T) {
	for _, name := range templateNames {
		t.Run(name, func(t *testing.T) {
			embedded, err := templateFS.ReadFile("templates/" + name + ".yaml")
			if err != nil {
				t.Fatalf("read embedded template: %v", err)
			}
			example, err := os.ReadFile(filepath.Join("..", "..", "examples", name+".yaml"))
			if err != nil {
				t.Fatalf("read examples/%s.yaml: %v", name, err)
			}
			if !bytes.Equal(embedded, example) {
				t.Errorf("examples/%s.yaml and internal/wizard/templates/%s.yaml have drifted apart; copy one over the other",
					name, name)
			}
		})
	}
}
