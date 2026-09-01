package loopcmd

import (
	"context"
	"os/exec"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/config"
	"github.com/seanmcgary/agent-utils/internal/ghub"
)

// A claude loop resolves nothing. claude reaches one vendor one way, so a
// provider comparison has nothing to say about it, and resolving would spend a
// subprocess per model to learn "".
func TestResolveProvidersSkipsClaude(t *testing.T) {
	cfg := tickConfig(t)
	issues := []ghub.Issue{{Number: 1, Labels: []string{cfg.Labels.Trigger}}}

	got := resolveProviders(context.Background(), cfg, issues)

	if len(got) != 0 {
		t.Errorf("providers = %v, want empty for a claude loop", got)
	}
}

// An unparseable override is Decide's problem, not this function's: it becomes
// a KindStop, and no provider is wanted for an issue that will not dispatch.
func TestResolveProvidersSkipsUnparseableLabels(t *testing.T) {
	cfg := tickConfig(t)
	cfg.Agent.Harness = config.HarnessPi
	issues := []ghub.Issue{{Number: 1, Labels: []string{cfg.Labels.Trigger, "harness:nonsense"}}}

	got := resolveProviders(context.Background(), cfg, issues)

	if len(got) != 0 {
		t.Errorf("providers = %v, want empty for an unparseable label", got)
	}
}

// A model pi cannot resolve produces no entry rather than a wrong one. This
// holds whether or not pi is installed, which is what makes it a CI test.
func TestResolveProvidersOmitsUnresolvedModels(t *testing.T) {
	cfg := tickConfig(t)
	cfg.Agent.Harness = config.HarnessPi
	cfg.Agent.Model = "definitely-not-a-real-model-name"
	issues := []ghub.Issue{{Number: 1, Labels: []string{cfg.Labels.Trigger}}}

	got := resolveProviders(context.Background(), cfg, issues)

	if len(got) != 0 {
		t.Errorf("providers = %v, want empty for an unresolvable model", got)
	}
}

// The real mapping, when pi is available. Skipped in CI.
func TestResolveProvidersReadsTheRealMapping(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi is not installed")
	}
	cfg := tickConfig(t)
	cfg.Agent.Harness = config.HarnessPi
	cfg.Agent.Model = "deepseek/deepseek-v4-flash-0731"
	issues := []ghub.Issue{
		{Number: 1, Labels: []string{cfg.Labels.Trigger}},
		// A per-issue override reaches a different provider, and the two must
		// not share a cache entry.
		{Number: 2, Labels: []string{cfg.Labels.Trigger, "model:openai-codex/gpt-5.6-terra"}},
	}

	got := resolveProviders(context.Background(), cfg, issues)

	if got[1] == "" {
		t.Skip("pi resolved no provider; the model may no longer be listed")
	}
	if got[1] != "openrouter" {
		t.Errorf("issue 1 provider = %q, want %q", got[1], "openrouter")
	}
	if got[2] != "" && got[2] != "openai-codex" {
		t.Errorf("issue 2 provider = %q, want %q", got[2], "openai-codex")
	}
}
