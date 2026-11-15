package secret

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestSecret_Redacts(t *testing.T) {
	s := Secret("hunter2")

	for _, verb := range []string{"%s", "%v", "%+v", "%#v", "%d", "%x"} {
		if got := fmt.Sprintf(verb, s); got != "****" {
			t.Errorf("Sprintf(%q) = %q, want %q", verb, got, "****")
		}
	}

	gotJSON, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(gotJSON) != `"****"` {
		t.Errorf("json.Marshal = %s, want %q", gotJSON, `"****"`)
	}

	gotText, err := s.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	if string(gotText) != "****" {
		t.Errorf("MarshalText = %q, want %q", gotText, "****")
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("msg", slog.Any("k", s))
	line := buf.String()
	if !strings.Contains(line, `"k":"****"`) {
		t.Errorf("slog line %q lacks %q", line, `"k":"****"`)
	}
	if strings.Contains(line, "hunter2") {
		t.Errorf("slog line %q leaks the secret", line)
	}

	if got := s.Expose(); got != "hunter2" {
		t.Errorf("Expose() = %q, want %q", got, "hunter2")
	}
}
