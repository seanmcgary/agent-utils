package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration decodes a YAML string such as "3h" into a time.Duration.
type Duration time.Duration

// UnmarshalYAML parses the scalar with time.ParseDuration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string such as \"30m\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String renders the value for logs.
func (d Duration) String() string { return time.Duration(d).String() }
