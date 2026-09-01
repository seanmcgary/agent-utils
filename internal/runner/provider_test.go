package runner

import (
	"strings"
	"testing"
)

const listModelsOutput = `provider      model                                  context  max-out  thinking  images
openai-codex  gpt-5.6-terra                          272K     128K     yes       yes
openrouter    deepseek/deepseek-v4-flash-0731        1.0M     943.7K   yes       no
openrouter    deepseek/deepseek-v4-flash-0731:batch  1.0M     943.7K   yes       no
openrouter    openai/gpt-5.6-terra                   1.1M     128K     yes       yes
`

// A bare OpenRouter id. The "deepseek/" prefix is the VENDOR, not the
// provider -- reading the first path segment as a provider is the mistake this
// pins against.
func TestParseModelTableResolvesBareOpenRouterID(t *testing.T) {
	got := ParseModelTable(strings.NewReader(listModelsOutput), "deepseek/deepseek-v4-flash-0731")
	if got != "openrouter" {
		t.Errorf("provider = %q, want %q", got, "openrouter")
	}
}

// The provider/model shape, which is how an openai-codex model is labelled.
func TestParseModelTableResolvesProviderPrefixedID(t *testing.T) {
	got := ParseModelTable(strings.NewReader(listModelsOutput), "openai-codex/gpt-5.6-terra")
	if got != "openai-codex" {
		t.Errorf("provider = %q, want %q", got, "openai-codex")
	}
}

// The search is fuzzy, so a prefix must not resolve to its longer neighbour.
func TestParseModelTableIgnoresSuffixMatches(t *testing.T) {
	got := ParseModelTable(strings.NewReader(listModelsOutput), "deepseek/deepseek-v4-flash")
	if got != "" {
		t.Errorf("provider = %q, want empty for a non-exact match", got)
	}
}

// "gpt-5.6-terra" is served by openai-codex as a bare id AND by openrouter as
// openai/gpt-5.6-terra. Ambiguity is unresolved, not a coin flip.
func TestParseModelTableRejectsAmbiguity(t *testing.T) {
	ambiguous := `provider      model          context
openai-codex  gpt-5.6-terra  272K
openrouter    gpt-5.6-terra  1.1M
`
	got := ParseModelTable(strings.NewReader(ambiguous), "gpt-5.6-terra")
	if got != "" {
		t.Errorf("provider = %q, want empty when two providers serve the id", got)
	}
}

// The same provider listing one id twice is not ambiguous. pi does exactly
// this when a model has a ":batch" twin, and both rows agree on the answer.
func TestParseModelTableAllowsRepeatsFromOneProvider(t *testing.T) {
	repeated := `provider    model        context
openrouter  a/b          1.0M
openrouter  a/b          1.0M
`
	if got := ParseModelTable(strings.NewReader(repeated), "a/b"); got != "openrouter" {
		t.Errorf("provider = %q, want %q", got, "openrouter")
	}
}

// A miss prints a sentence and exits 0, so the rows are the only signal.
func TestParseModelTableHandlesAMiss(t *testing.T) {
	got := ParseModelTable(strings.NewReader("No models matching \"zzz\"\n"), "zzz")
	if got != "" {
		t.Errorf("provider = %q, want empty", got)
	}
}

func TestParseModelTableHandlesEmptyInput(t *testing.T) {
	if got := ParseModelTable(strings.NewReader(""), "anything"); got != "" {
		t.Errorf("provider = %q, want empty", got)
	}
}

// The header is not a row, and "provider" is not a provider. A model literally
// named "model" must not resolve to it.
func TestParseModelTableSkipsTheHeader(t *testing.T) {
	if got := ParseModelTable(strings.NewReader(listModelsOutput), "model"); got != "" {
		t.Errorf("provider = %q, want empty", got)
	}
}
