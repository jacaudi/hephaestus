package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// execute runs a fresh root command with args and returns its stdout and error.
func execute(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out, errOut bytes.Buffer
	cmd := newRootCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)

	err := cmd.ExecuteContext(t.Context())
	return out.String(), err
}

func TestVersionCmd_JSON(t *testing.T) {
	out, err := execute(t, "version", "--json")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	want := map[string]string{"version": "dev", "commit": "unknown", "date": "unknown"}
	if len(got) != len(want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}

func TestVersionCmd_Text(t *testing.T) {
	got, err := execute(t, "version")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := "hephaestus dev (commit unknown, built unknown)\n"
	if got != want {
		t.Errorf("out = %q, want %q", got, want)
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"usage", usageError{errors.New("x")}, 2},
		{"other", errors.New("x"), 1},
		{"wrapped usage", fmt.Errorf("w: %w", usageError{errors.New("x")}), 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCode(tt.err); got != tt.want {
				t.Errorf("exitCode(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestRoot_UnknownCommandIsUsageError(t *testing.T) {
	_, err := execute(t, "bogus")
	if err == nil {
		t.Fatal("expected an error for an unknown command")
	}
	if got := exitCode(err); got != 2 {
		t.Errorf("exitCode(%v) = %d, want 2", err, got)
	}
}

func TestRoot_UnknownFlagIsUsageError(t *testing.T) {
	_, err := execute(t, "--bogus")
	if err == nil {
		t.Fatal("expected an error for an unknown flag")
	}
	if got := exitCode(err); got != 2 {
		t.Errorf("exitCode(%v) = %d, want 2", err, got)
	}
}

func TestVersionCmd_StrayArgIsUsageError(t *testing.T) {
	_, err := execute(t, "version", "bogus")
	if err == nil {
		t.Fatal("expected an error for a stray argument")
	}
	if got := exitCode(err); got != 2 {
		t.Errorf("exitCode(%v) = %d, want 2", err, got)
	}
}

func TestRoot_LogFormatFlagIsPersistent(t *testing.T) {
	flag := newRootCmd().PersistentFlags().Lookup("log-format")
	if flag == nil {
		t.Fatal("root has no persistent --log-format flag")
	}
	if flag.DefValue != "" {
		t.Errorf("default = %q, want empty (the env or json default applies)", flag.DefValue)
	}

	// A subcommand accepts it too: it is persistent, not local to the root.
	if _, err := execute(t, "version", "--log-format=text"); err != nil {
		t.Errorf("version --log-format=text: %v", err)
	}
}
