package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const bundle = `{"ref+op://lab/bundle/token": "s3cret-value"}`

func writeValues(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "values.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write values: %v", err)
	}

	return path
}

// TestRun pins the stub's exit statuses to what vals and Topf's internal/vals agree on: 0 for a resolved file, 1 for a
// failure, 2 for arguments other than `eval -f -`.
func TestRun(t *testing.T) {
	evalArgs := []string{"eval", "-f", "-"}

	tests := []struct {
		name       string
		args       []string
		stdin      string
		values     string
		noPath     bool
		wantStatus int
		wantOut    string
		wantMsg    string
	}{
		{
			name:    "substitutes a reference",
			args:    evalArgs,
			stdin:   "a: ref+op://lab/bundle/token\n",
			values:  bundle,
			wantOut: "a: s3cret-value\n",
		},
		{
			name:    "content without a reference passes through",
			args:    evalArgs,
			stdin:   "a: plain\n",
			values:  bundle,
			wantOut: "a: plain\n",
		},
		{
			name:       "unknown reference names the reference",
			args:       evalArgs,
			stdin:      "a: ref+op://lab/bundle/other\n",
			values:     bundle,
			wantStatus: 1,
			wantMsg:    "unknown reference ref+op://lab/bundle/other",
		},
		{
			name:       "other arguments are a usage error",
			args:       []string{"eval", "-f", "x.yaml"},
			values:     bundle,
			wantStatus: 2,
			wantMsg:    "only `eval -f -` is supported",
		},
		{
			name:       "no arguments are a usage error",
			values:     bundle,
			wantStatus: 2,
			wantMsg:    "only `eval -f -` is supported",
		},
		{
			name:       "values path not set",
			args:       evalArgs,
			noPath:     true,
			wantStatus: 1,
			wantMsg:    "VALSSTUB_VALUES is not set",
		},
		{
			name:       "values that are not a JSON object of strings",
			args:       evalArgs,
			values:     `{"ref": 1}`,
			wantStatus: 1,
			wantMsg:    "values are not a JSON object of strings",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			if !tt.noPath {
				path = writeValues(t, tt.values)
			}

			var out bytes.Buffer

			status, message := run(tt.args, strings.NewReader(tt.stdin), &out, path)

			if status != tt.wantStatus {
				t.Errorf("status = %d, want %d", status, tt.wantStatus)
			}

			if message != tt.wantMsg {
				t.Errorf("message = %q, want %q", message, tt.wantMsg)
			}

			if out.String() != tt.wantOut {
				t.Errorf("stdout = %q, want %q", out.String(), tt.wantOut)
			}
		})
	}
}
