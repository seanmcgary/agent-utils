package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/seanmcgary/agent-utils/internal/listener"
	"github.com/seanmcgary/agent-utils/internal/metrics"
	"github.com/seanmcgary/agent-utils/internal/settings"
	"github.com/urfave/cli/v3"
)

// runWithMetricsRoot runs args under a root that carries the real statsd
// flags and Before, which runListenerCLI's bare root deliberately does not.
func runWithMetricsRoot(t *testing.T, args ...string) error {
	t.Helper()
	t.Cleanup(metrics.Shutdown)
	root := &cli.Command{
		Name:     "agent-utils",
		Flags:    statsdFlags(),
		Before:   configureMetrics,
		Commands: []*cli.Command{listenerCommand()},
	}
	return root.Run(context.Background(), append([]string{"agent-utils"}, args...))
}

func readyForInstall(t *testing.T) *fakeManager {
	t.Helper()
	withHome(t)
	isolateService(t)
	if err := settings.Save(&settings.Settings{
		Webhook: settings.Webhook{Enabled: true, URL: "https://x/y", Secret: "a-secret"},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	t.Setenv("GITHUB_TOKEN", "")
	if _, err := listener.SetToken("ghp_testtoken"); err != nil {
		t.Fatalf("SetToken: %v", err)
	}
	fake := &fakeManager{}
	fakeService(t, fake)
	return fake
}

// TestListenerInstallCarriesMetricsFromTheEnvironment is the service half of
// the propagation problem. A service started at login inherits none of the
// operator's shell, so an AGENT_UTILS_STATSD_* variable exported when they
// ran install has to be written into the service definition, or metrics stop
// at the install.
func TestListenerInstallCarriesMetricsFromTheEnvironment(t *testing.T) {
	fake := readyForInstall(t)
	t.Setenv(metrics.EnvAddr, "127.0.0.1:8125")
	t.Setenv(metrics.EnvTags, "env:prod,host:mini")

	if err := runWithMetricsRoot(t, "listener", "install"); err != nil {
		t.Fatalf("listener install: %v", err)
	}
	want := []string{"listener", "run",
		"--statsd-addr", "127.0.0.1:8125", "--statsd-prefix", metrics.DefaultPrefix,
		"--statsd-tags", "env:prod,host:mini"}
	if !slices.Equal(fake.installArgs, want) {
		t.Errorf("installed args = %q, want %q", fake.installArgs, want)
	}
}

// The flag spelling reaches the service the same way, and a flag given after
// the subcommand counts: urfave/cli v3 root flags are persistent.
func TestListenerInstallCarriesMetricsFromAFlag(t *testing.T) {
	fake := readyForInstall(t)
	t.Setenv(metrics.EnvAddr, "")

	if err := runWithMetricsRoot(t, "listener", "install",
		"--statsd-addr", "127.0.0.1:9125", "--statsd-prefix", "au"); err != nil {
		t.Fatalf("listener install: %v", err)
	}
	got := strings.Join(fake.installArgs, " ")
	if !strings.Contains(got, "--statsd-addr 127.0.0.1:9125 --statsd-prefix au") {
		t.Errorf("installed args = %q, want the statsd flags", got)
	}
	if strings.Contains(got, "--statsd-tags") {
		t.Errorf("installed args = %q, want no --statsd-tags when none were given", got)
	}
}

// With metrics off, install writes exactly what it always wrote.
func TestListenerInstallWithoutMetricsAddsNothing(t *testing.T) {
	fake := readyForInstall(t)
	t.Setenv(metrics.EnvAddr, "")

	if err := runWithMetricsRoot(t, "listener", "install"); err != nil {
		t.Fatalf("listener install: %v", err)
	}
	if want := []string{"listener", "run"}; !slices.Equal(fake.installArgs, want) {
		t.Errorf("installed args = %q, want %q", fake.installArgs, want)
	}
}

// A malformed address fails the command at the terminal instead of producing
// a service that silently sends nothing.
func TestAnInvalidStatsdAddressFailsTheCommand(t *testing.T) {
	fake := readyForInstall(t)
	err := runWithMetricsRoot(t, "--statsd-addr", "no-port-here", "listener", "install")
	if err == nil {
		t.Fatal("an invalid --statsd-addr was accepted")
	}
	if fake.installArgs != nil {
		t.Errorf("the service was installed anyway: %q", fake.installArgs)
	}
}
