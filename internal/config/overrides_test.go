package config

import (
	"strings"
	"testing"
)

// mustErr calls ParseOverrides and fails the test if it does not return an
// error. It returns that error so the caller can inspect it.
func mustErr(t *testing.T, labels []string) error {
	t.Helper()
	_, err := ParseOverrides(labels)
	if err == nil {
		t.Fatalf("ParseOverrides(%v) = nil error, want an error", labels)
	}
	return err
}

func TestParseOverrides(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   Overrides
	}{
		{
			name:   "no override labels",
			labels: []string{"needs-triage", "priority:high-ish-but-not-a-prefix-we-know"},
			want:   Overrides{},
		},
		{
			name:   "all three at once",
			labels: []string{"model:claude-opus-5", "harness:pi", "effort:high"},
			want:   Overrides{Model: "claude-opus-5", Harness: "pi", Effort: "high"},
		},
		{
			name:   "prefix folds case, model value does not",
			labels: []string{"Model:Claude-Opus-5"},
			want:   Overrides{Model: "Claude-Opus-5"},
		},
		{
			name:   "harness enum value is lowered",
			labels: []string{"harness:PI"},
			want:   Overrides{Harness: "pi"},
		},
		{
			name:   "effort enum value is lowered",
			labels: []string{"effort:HIGH"},
			want:   Overrides{Effort: "high"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseOverrides(c.labels)
			if err != nil {
				t.Fatalf("ParseOverrides(%v) unexpected error: %v", c.labels, err)
			}
			if got != c.want {
				t.Fatalf("ParseOverrides(%v) = %+v, want %+v", c.labels, got, c.want)
			}
		})
	}
}

func TestParseOverridesRejectsBadValues(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
	}{
		{"empty value", []string{"model:"}},
		{"whitespace", []string{"model:claude opus"}},
		{"leading dash", []string{"model:-opus"}},
		{"zero-width space", []string{"model:claude" + "\u200b" + "opus"}},
		{"duplicate prefix", []string{"model:a", "model:b"}},
		{"bad harness", []string{"harness:gpt"}},
		{"bad effort", []string{"effort:bogus"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseOverrides(c.labels)
			if err == nil {
				t.Fatalf("ParseOverrides(%v) = nil error, want an error", c.labels)
			}
			if got != (Overrides{}) {
				t.Fatalf("ParseOverrides(%v) returned non-zero Overrides %+v alongside an error", c.labels, got)
			}
		})
	}
}

// The security rule. A rejected value must never reach an argument list.
func TestParseOverridesRejectsEveryFlagShapedValue(t *testing.T) {
	for _, v := range []string{"-p", "--model", "-", "--", "-x"} {
		if _, err := ParseOverrides([]string{"model:" + v}); err == nil {
			t.Fatalf("ParseOverrides accepted the flag-shaped value %q", v)
		}
	}
}

// The reason text is persisted as stopped_reason, logged, and printed to a
// terminal. A label carrying a newline or an escape must not travel raw.
func TestParseOverridesQuotesTheLabelInEveryError(t *testing.T) {
	for _, labels := range [][]string{
		{"model:a\nb"},
		{"model:a", "model:b\nc"}, // the duplicate error interpolates BOTH labels
	} {
		err := mustErr(t, labels)
		if strings.Contains(err.Error(), "\n") {
			t.Fatalf("error %q carries a raw newline; quote the label with %%q", err)
		}
	}
}
