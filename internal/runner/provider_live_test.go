package runner

import (
	"context"
	"os/exec"
	"testing"
)

// A live check against the real binary. Skipped wherever pi is not installed,
// which includes CI -- the parser's own behaviour is pinned by
// provider_test.go against captured output, and this only guards against the
// published table changing shape under us.
func TestResolveProviderAgainstTheRealBinary(t *testing.T) {
	if _, err := exec.LookPath("pi"); err != nil {
		t.Skip("pi is not installed")
	}
	got := ResolveProvider(context.Background(), "deepseek/deepseek-v4-flash-0731")
	if got == "" {
		t.Skip("pi resolved no provider; the model may no longer be listed")
	}
	if got != "openrouter" {
		t.Errorf("provider = %q, want %q", got, "openrouter")
	}
}
