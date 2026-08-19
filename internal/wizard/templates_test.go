package wizard

import "testing"

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
