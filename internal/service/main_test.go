package service

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences this package's structured logging (Install and Uninstall
// each log one line). Unconstrained by a GOOS build tag so it links into the
// test binary on every platform, matching internal/loopcmd/main_test.go.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
