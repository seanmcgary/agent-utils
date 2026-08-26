package config

import (
	"fmt"
	"regexp"
	"strings"
)

// The three label prefixes that override an agent setting for one issue.
// They are fixed and not configurable: the feature is always active, so
// there is no config field an operator could use to turn it off by mistake.
const (
	OverrideModelPrefix   = "model:"
	OverrideHarnessPrefix = "harness:"
	OverrideEffortPrefix  = "effort:"
)

// Overrides holds the per-issue agent settings read from labels. A zero
// field means "no override for this setting", not "the empty value" — an
// empty Model, for example, must never be mistaken for a model named "".
type Overrides struct {
	Model   string
	Harness string
	Effort  string
}

// overrideValue is an ALLOWLIST, not a denylist. The value becomes one
// element of the argv exec receives (internal/runner/args.go:26); Go passes
// a list, not a shell string, so quoting is not the hazard here — a leading
// "-" is, because the agent reads it as a FLAG
// (model:--dangerously-skip-permissions). internal/ghub/types.go:141's
// SafeRef rejects a leading dash for the same reason. An allowlist also
// excludes U+200B (zero-width space) and U+2060 (word joiner), which
// unicode.IsSpace does not match, so those cannot slip a whitespace-shaped
// value past a naive space check.
var overrideValue = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)

// validEfforts is the same closed list internal/config/config.go:263
// enforces on agent.effort. A label must not reopen a rule the configuration
// closes.
var validEfforts = map[string]bool{
	"low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

// ParseOverrides reads the agent overrides from an issue's labels. Nothing
// else in this program parses these labels — internal/config owns the rules
// beside the config fields they mirror.
//
// It returns the ZERO Overrides alongside any error, so a caller that
// ignores the error (there should be none, but the type must not invite one)
// never gets half an override.
func ParseOverrides(labels []string) (Overrides, error) {
	var (
		out                              Overrides
		modelLabel, harnessLabel, effLbl string
	)

	for _, label := range labels {
		prefix, value, ok := cutPrefixFold(label, OverrideModelPrefix)
		if ok {
			// Validate BEFORE reporting a duplicate: the duplicate error
			// below interpolates both labels, and this one is unvalidated
			// until this line runs.
			if err := validateOverrideValue(prefix, value); err != nil {
				return Overrides{}, err
			}
			if modelLabel != "" {
				return Overrides{}, fmt.Errorf(
					"duplicate model override: %q and %q", modelLabel, label)
			}
			modelLabel = label
			// model values are NOT lowered: a model identifier is
			// case-sensitive, unlike harness and effort, which are enums.
			out.Model = value
			continue
		}

		prefix, value, ok = cutPrefixFold(label, OverrideHarnessPrefix)
		if ok {
			// Validate BEFORE reporting a duplicate — both the allowlist
			// and the enum check are validation of THIS value, and must run
			// before the duplicate error interpolates this label.
			if err := validateOverrideValue(prefix, value); err != nil {
				return Overrides{}, err
			}
			lowered := strings.ToLower(value)
			if lowered != HarnessClaude && lowered != HarnessPi {
				return Overrides{}, fmt.Errorf(
					"invalid harness override %q: must be %q or %q",
					label, HarnessClaude, HarnessPi)
			}
			if harnessLabel != "" {
				return Overrides{}, fmt.Errorf(
					"duplicate harness override: %q and %q", harnessLabel, label)
			}
			harnessLabel = label
			out.Harness = lowered
			continue
		}

		prefix, value, ok = cutPrefixFold(label, OverrideEffortPrefix)
		if ok {
			// Validate BEFORE reporting a duplicate, for the same reason.
			if err := validateOverrideValue(prefix, value); err != nil {
				return Overrides{}, err
			}
			lowered := strings.ToLower(value)
			if !validEfforts[lowered] {
				return Overrides{}, fmt.Errorf(
					"invalid effort override %q: must be one of low, medium, high, xhigh, max",
					label)
			}
			if effLbl != "" {
				return Overrides{}, fmt.Errorf(
					"duplicate effort override: %q and %q", effLbl, label)
			}
			effLbl = label
			out.Effort = lowered
			continue
		}
	}

	return out, nil
}

// cutPrefixFold reports whether label starts with prefix, ignoring case —
// every other label comparison in this program folds case (ghub.HasLabel
// uses EqualFold) — and returns the prefix (in the label's own casing, for
// error messages) and the remainder.
func cutPrefixFold(label, prefix string) (matchedPrefix, rest string, ok bool) {
	if len(label) < len(prefix) {
		return "", "", false
	}
	if !strings.EqualFold(label[:len(prefix)], prefix) {
		return "", "", false
	}
	return label[:len(prefix)], label[len(prefix):], true
}

// A harness: override carries no cross-harness safety rule. claude and pi
// support different settings, and the ones only claude has --
// agent.permission_mode, agent.max_budget_usd, agent.background_tasks --
// are simply not emitted when the effective harness is pi
// (internal/runner/args.go PiBuildArgs). An override that lands on a
// harness which does not implement a setting IGNORES that setting; it is
// never a reason to refuse the override, in either direction.

// validateOverrideValue applies the argument-injection rule: the value is
// checked against an ALLOWLIST regex, not a denylist, so no enumeration of
// "dangerous" characters can be incomplete. An empty value or one starting
// with "-" fails the allowlist already; a leading dash gets its own message
// because it names the exact hazard (the agent reads it as a flag) rather
// than a bare "invalid" report.
func validateOverrideValue(matchedPrefix, value string) error {
	label := matchedPrefix + value
	if value == "" {
		return fmt.Errorf("empty value in label %q", label)
	}
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf(
			"label %q starts with '-'; the value becomes an argument and would be read as a flag",
			label)
	}
	if !overrideValue.MatchString(value) {
		return fmt.Errorf("label %q has an invalid value %q", label, value)
	}
	return nil
}
