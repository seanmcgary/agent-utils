package wizard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/seanmcgary/agent-utils/internal/config"
	"gopkg.in/yaml.v3"
)

// yamlDoc and its nested types mirror config.Config's yaml shape exactly,
// field for field, so config.Load's strict decoder (dec.KnownFields(true),
// which errors on an unknown key) accepts what this package writes.
//
// config.Duration implements UnmarshalYAML but not MarshalYAML, so
// yaml.Marshal(cfg) directly would encode a duration as the bare integer
// nanosecond count instead of a string. config.Load would then reject its own
// output: UnmarshalYAML requires a scalar string such as "30m". Rendering
// every duration field as a string here is what makes the reload at the end
// of Write actually pass instead of failing on the file this package just
// wrote.
type yamlDoc struct {
	Name                         string     `yaml:"name"`
	Repo                         string     `yaml:"repo"`
	CheckoutBaseDir              string     `yaml:"checkout_base_dir"`
	WorktreeDir                  string     `yaml:"worktree_dir"`
	StateDir                     string     `yaml:"state_dir,omitempty"`
	DefaultBranch                string     `yaml:"default_branch"`
	Labels                       yamlLabels `yaml:"labels"`
	Agent                        yamlAgent  `yaml:"agent"`
	Retry                        yamlRetry  `yaml:"retry"`
	AcknowledgeBypassPermissions bool       `yaml:"i_understand_bypass_permissions,omitempty"`
	Prompt                       string     `yaml:"prompt"`
	ResumePrompt                 string     `yaml:"resume_prompt"`
}

type yamlLabels struct {
	Trigger  string   `yaml:"trigger"`
	InFlight string   `yaml:"in_flight"`
	Blocked  string   `yaml:"blocked"`
	Terminal string   `yaml:"terminal,omitempty"`
	Veto     []string `yaml:"veto"`
}

type yamlAgent struct {
	Harness        string  `yaml:"harness"`
	Model          string  `yaml:"model"`
	Effort         string  `yaml:"effort"`
	PermissionMode string  `yaml:"permission_mode"`
	Worktree       string  `yaml:"worktree"`
	MaxBudgetUSD   float64 `yaml:"max_budget_usd"`
	Timeout        string  `yaml:"timeout"`
}

type yamlBreaker struct {
	OrphanThreshold int    `yaml:"orphan_threshold"`
	Cooldown        string `yaml:"cooldown"`
}

type yamlRetry struct {
	Max     int         `yaml:"max"`
	Backoff []string    `yaml:"backoff"`
	Breaker yamlBreaker `yaml:"breaker"`
}

func toYAMLDoc(cfg *config.Config) yamlDoc {
	backoff := make([]string, 0, len(cfg.Retry.Backoff))
	for _, d := range cfg.Retry.Backoff {
		backoff = append(backoff, d.String())
	}
	return yamlDoc{
		Name:            cfg.Name,
		Repo:            cfg.Repo,
		CheckoutBaseDir: cfg.CheckoutBaseDir,
		WorktreeDir:     cfg.WorktreeDir,
		StateDir:        cfg.StateDir,
		DefaultBranch:   cfg.DefaultBranch,
		Labels: yamlLabels{
			Trigger:  cfg.Labels.Trigger,
			InFlight: cfg.Labels.InFlight,
			Blocked:  cfg.Labels.Blocked,
			Terminal: cfg.Labels.Terminal,
			Veto:     cfg.Labels.Veto,
		},
		Agent: yamlAgent{
			Harness:        cfg.Agent.Harness,
			Model:          cfg.Agent.Model,
			Effort:         cfg.Agent.Effort,
			PermissionMode: cfg.Agent.PermissionMode,
			Worktree:       cfg.Agent.Worktree,
			MaxBudgetUSD:   cfg.Agent.MaxBudgetUSD,
			Timeout:        cfg.Agent.Timeout.String(),
		},
		Retry: yamlRetry{
			Max:     cfg.Retry.Max,
			Backoff: backoff,
			Breaker: yamlBreaker{
				OrphanThreshold: cfg.Retry.Breaker.OrphanThreshold,
				Cooldown:        cfg.Retry.Breaker.Cooldown.String(),
			},
		},
		AcknowledgeBypassPermissions: cfg.AcknowledgeBypassPermissions,
		Prompt:                       cfg.Prompt,
		ResumePrompt:                 cfg.ResumePrompt,
	}
}

// Write marshals a configuration to a loop file, with a header comment.
//
// It refuses to overwrite an existing file of that name: a second run of the
// wizard in the same project must not silently clobber a configuration the
// operator has since hand-tuned.
//
// 0600, and the temp-file-and-rename pattern internal/registry/registry.go
// already uses: a crash mid-write cannot leave a truncated configuration
// behind for a later `agent-utils` invocation to load, and the file starts
// out readable only by its owner. This file holds no secret today — tokens
// are environment-only, never configuration, per cmd/agent-utils/main.go —
// but a freshly generated file is unreviewed, and restricting it until the
// operator has read it costs nothing.
func Write(dir string, cfg *config.Config) (path string, err error) {
	configsDir := filepath.Join(dir, config.ConfigsSubdir)
	if err := os.MkdirAll(configsDir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", configsDir, err)
	}

	target := filepath.Join(configsDir, cfg.Name+".yaml")
	if _, statErr := os.Stat(target); statErr == nil {
		return "", fmt.Errorf("%s already exists; refusing to overwrite it", target)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("stat %s: %w", target, statErr)
	}

	raw, err := yaml.Marshal(toYAMLDoc(cfg))
	if err != nil {
		return "", fmt.Errorf("encode %s: %w", target, err)
	}
	header := fmt.Sprintf(
		"# Generated by the agent-utils setup wizard on %s.\n"+
			"# Review every value below before this loop starts acting on real issues.\n\n",
		time.Now().UTC().Format(time.RFC3339))
	body := append([]byte(header), raw...)

	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup; the rename error above is what matters
		return "", fmt.Errorf("replace %s: %w", target, err)
	}

	// The proof: reload what was just written through the same strict loader
	// every other command uses. A wizard that writes a file config.Load
	// rejects is worse than no wizard, so the file is kept for inspection
	// (the path is still returned) and the loader's own error is reported
	// rather than a generic "write failed".
	if _, err := config.Load(target); err != nil {
		return target, fmt.Errorf("wrote %s but it failed to reload: %w", target, err)
	}
	return target, nil
}
