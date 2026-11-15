package telemetry

import (
	"fmt"
	"io"
	"log/slog"
)

// NewLogger returns a logger that writes to w in the given format, "json" or
// "text", at the given level. Every record carries service=hephaestus and the
// build version.
func NewLogger(w io.Writer, format string, level slog.Level, version string) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	switch format {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("unknown log format %q", format)
	}
	return slog.New(h).With("service", "hephaestus", "version", version), nil
}
