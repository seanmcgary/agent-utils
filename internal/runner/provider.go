package runner

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"time"
)

// providerTimeout bounds one `pi --list-models` call. This runs inside a tick
// that holds the loop's flock, and a hung pi must not hold it: no answer and
// the retry cap standing is strictly better than a stalled loop.
const providerTimeout = 10 * time.Second

// ResolveProvider returns the pi provider serving model, or "" when it cannot
// be determined.
//
// Provider identity is what says whether two models share a balance.
// openrouter and openai-codex are separate accounts, so a 402 on one is no
// evidence about the other -- which is the entire reason engine.configRetired
// wants to know. pi owns that mapping, and `pi --list-models` is its published
// surface for it, so this shells out rather than reading pi's private
// models-store.json cache. If the published surface changes, resolution
// degrades to "unknown" and the cap keeps the behaviour it has today; a
// misread of a private file could do something worse.
//
// Every failure is silent and returns "". This is a repair path, not a
// correctness requirement: an unresolved provider means the cap stands, which
// is what happens without this function at all.
func ResolveProvider(ctx context.Context, model string) string {
	if strings.TrimSpace(model) == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()

	// The model is an argv element, never interpolated into a shell string, so
	// a model name cannot become another argument or a command.
	cmd := exec.CommandContext(ctx, "pi", "--list-models", model)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		// A MISS exits 0 and prints a sentence, so a non-zero status here means
		// pi is absent, broken, or timed out -- not that the model is unknown.
		// The answer is the same either way.
		return ""
	}
	return ParseModelTable(&out, model)
}

// ParseModelTable reads `pi --list-models` output and returns the provider of
// the row matching model exactly, or "" when there is no unambiguous match.
//
// The match must be EXACT because the search is fuzzy: asking for
// "deepseek/deepseek-v4-flash-0731" also returns the ":batch" variant, and
// asking for a bare id can return rows from several providers. Two shapes
// count, because both are in use as model: labels -- the bare id in the model
// column ("deepseek/deepseek-v4-flash-0731", an OpenRouter id whose first
// segment is the vendor, not the provider) and the provider-qualified form
// ("openai-codex/gpt-5.6-terra").
//
// Rows spanning more than one provider are unresolved rather than first-wins.
// Guessing here would retire a retry cap on a provider change that did not
// happen.
func ParseModelTable(r io.Reader, model string) string {
	sc := bufio.NewScanner(r)
	found := ""
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		provider, id := fields[0], fields[1]
		if provider == "provider" && id == "model" {
			// The header. It is not a row, and "provider" is not a provider.
			continue
		}
		if id != model && provider+"/"+id != model {
			continue
		}
		if found != "" && found != provider {
			// Two providers serve this name. Unresolved.
			return ""
		}
		found = provider
	}
	return found
}
