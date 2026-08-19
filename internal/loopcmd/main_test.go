package loopcmd

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences the tick's structured logging. The loop logs every decision
// by design, which buries an actual test failure in hundreds of INFO lines.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
