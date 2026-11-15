package telemetry

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNewLogger_JSON(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewLogger(&buf, "json", slog.LevelInfo, "v1.2.3")
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	logger.InfoContext(t.Context(), "hello", "user_id", "u1")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log output is not one JSON object: %v\n%s", err, buf.String())
	}
	want := map[string]string{
		"msg":     "hello",
		"level":   "INFO",
		"service": "hephaestus",
		"version": "v1.2.3",
		"user_id": "u1",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %q", k, got[k], v)
		}
	}
}

func TestNewLogger_RejectsUnknownFormat(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewLogger(&buf, "xml", slog.LevelInfo, "v1.2.3")
	if err == nil {
		t.Fatalf("NewLogger(xml) = %v, nil error; want error", logger)
	}
	if want := `unknown log format "xml"`; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}
