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

// ValidateHarnessSafety refuses a harness: override whose EFFECTIVE harness
// would be pi when the configuration sets a safety setting pi enforces
// neither of: agent.permission_mode or a non-zero agent.max_budget_usd.
// BuildArgs (claude) emits both; PiBuildArgs emits NEITHER (args.go) -- so
// switching TO pi on a loop that configured either would silently run the
// dispatch with no permission mode and no cost ceiling, the exact two bounds
// that exist because the agent reads third-party issue text.
//
// The hazard is directional. Switching FROM pi TO claude only ever ADDS a
// bound PiBuildArgs never enforced in the first place, so it is never
// refused, however the configuration is set — the same is true when the
// override equals the configured harness (a no-op) or is empty (no
// override at all).
//
// It lives here, not in internal/engine, so BOTH callers that can put a row
// in front of RunAgent enforce it: engine.Decide, on the ordinary tick path,
// and runner.Effective, the last line of defence before a value becomes an
// argv element, for a row reaching RunAgent by any other path (legacy
// import, an older binary, a hand-edited database).
func (ov Overrides) ValidateHarnessSafety(cfg *Config) error {
	if ov.Harness == "" {
		return nil
	}
	configured := cfg.Agent.Harness
	if configured == "" {
		configured = HarnessClaude
	}
	if ov.Harness == configured {
		return nil
	}
	if ov.Harness != HarnessPi {
		// Switching to claude never drops a bound: claude enforces both.
		return nil
	}
	if cfg.Agent.PermissionMode != "" {
		return fmt.Errorf(
			"harness override %q would drop agent.permission_mode %q: the pi harness enforces no permission mode",
			ov.Harness, cfg.Agent.PermissionMode)
	}
	if cfg.Agent.MaxBudgetUSD != 0 {
		return fmt.Errorf(
			"harness override %q would drop the agent.max_budget_usd %.2f ceiling: the pi harness enforces no cost ceiling",
			ov.Harness, cfg.Agent.MaxBudgetUSD)
	}
	return nil
}

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
