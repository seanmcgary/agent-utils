package listener

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences per-delivery and per-skip logging. This package logs on
// every rejection and every skipped project by design, which buries an
// actual test failure in the noise. Precedent: internal/loopcmd/main_test.go.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
