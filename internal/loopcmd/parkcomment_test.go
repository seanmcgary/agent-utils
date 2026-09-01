package loopcmd

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/seanmcgary/agent-utils/internal/store"
)

// The real OpenRouter 402, trimmed. The key-management URL names the key's
// identifier and must never reach a GitHub comment.
const openRouter402 = `402: {"message":"This request requires more credits, or fewer max_tokens. You requested up to 873072 tokens, but can only afford 469825. To increase, visit https://openrouter.ai/workspaces/default/keys/15a8e996fc9ffff0cd339779332daf18263705193a275ad56eda0caf49e30d10 and adjust the key's daily limit","code":402}`

func TestFailureSentenceExtractsTheProviderMessage(t *testing.T) {
	got := failureSentence(openRouter402)

	if !strings.Contains(got, "requires more credits") {
		t.Errorf("sentence = %q, want the provider's own message", got)
	}
	if !strings.HasPrefix(got, "402:") {
		t.Errorf("sentence = %q, want the status kept as the prefix", got)
	}
	if strings.Contains(got, "\"code\"") {
		t.Errorf("sentence = %q, want the JSON envelope dropped", got)
	}
	if strings.Contains(got, "openrouter.ai/workspaces") || strings.Contains(got, "15a8e996") {
		t.Errorf("sentence = %q, must not carry the key URL or its identifier", got)
	}
}

// Not every harness reports JSON. A plain sentence must survive intact.
func TestFailureSentenceKeepsPlainText(t *testing.T) {
	got := failureSentence("No conversation found with session ID: abc-123")
	if got != "No conversation found with session ID: abc-123" {
		t.Errorf("sentence = %q, want it unchanged", got)
	}
}

func TestRedactForCommentDropsEveryURL(t *testing.T) {
	got := redactForComment("see https://example.com/secret/abc and http://x.y/z now")
	if strings.Contains(got, "http") {
		t.Errorf("redacted = %q, want no URLs", got)
	}
	if !strings.Contains(got, "see") || !strings.Contains(got, "now") {
		t.Errorf("redacted = %q, want the surrounding prose kept", got)
	}
}

func TestRedactForCommentCapsLength(t *testing.T) {
	got := redactForComment(strings.Repeat("a", 900))
	if utf8.RuneCountInString(got) > 301 {
		t.Errorf("len = %d runes, want at most 301 including the ellipsis",
			utf8.RuneCountInString(got))
	}
}

// Truncation must not split a multi-byte rune into mojibake.
func TestRedactForCommentTruncatesOnRuneBoundaries(t *testing.T) {
	got := redactForComment(strings.Repeat("é", 900))
	if !utf8.ValidString(got) {
		t.Errorf("redacted = %q, want valid UTF-8", got)
	}
}

// A failure the runner could not describe must not become an invented cause.
func TestFailureSentenceEmptyForNoRecordedError(t *testing.T) {
	if got := failureSentence(""); got != "" {
		t.Errorf("sentence = %q, want empty", got)
	}
}

func TestCapCauseNamesTheFailureAndWhatRanIt(t *testing.T) {
	got := capCause(store.Dispatch{
		APIError: openRouter402,
		Harness:  "pi",
		Model:    "deepseek/deepseek-v4-flash-0731",
		Provider: "openrouter",
	})

	for _, want := range []string{"requires more credits", "pi", "deepseek/deepseek-v4-flash-0731", "openrouter"} {
		if !strings.Contains(got, want) {
			t.Errorf("cause = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "usually indicates") {
		t.Errorf("cause = %q, want the known reason instead of the fallback", got)
	}
}

// With nothing recorded, the comment keeps its old wording rather than
// asserting a cause it does not have.
func TestCapCauseFallsBackWhenNothingWasRecorded(t *testing.T) {
	got := capCause(store.Dispatch{})
	if !strings.Contains(got, "usually indicates a sustained platform-side issue") {
		t.Errorf("cause = %q, want the fallback wording", got)
	}
}

// A claude dispatch records no provider, and the parenthetical must not show
// an empty slot for it.
func TestCapCauseOmitsAnUnrecordedProvider(t *testing.T) {
	got := capCause(store.Dispatch{
		APIError: "No conversation found with session ID: abc-123",
		Harness:  "claude",
		Model:    "opus",
	})
	if strings.Contains(got, ", ,") || strings.Contains(got, "claude, opus,") {
		t.Errorf("cause = %q, want no empty slot for the provider", got)
	}
	if !strings.Contains(got, "claude, opus") {
		t.Errorf("cause = %q, want the harness and model named", got)
	}
}
