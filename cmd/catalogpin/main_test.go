package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"go/format"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.jacaudi.dev/hephaestus/internal/catalog"
)

func TestParseFlags(t *testing.T) {
	all := []string{"-out", "o.go", "-keys", "k", "-provenance", "P.md", "-work", "/work", "-report", "r.txt"}
	tests := []struct {
		name    string
		args    []string
		want    config
		wantErr string
	}{
		{
			name: "every flag",
			args: all,
			want: config{out: "o.go", keys: "k", provenance: "P.md", work: "/work", report: "r.txt"},
		},
		{
			name: "only is split, trimmed and stripped of empties",
			args: append(slices.Clone(all), "-only", "grml, shredos,,"),
			want: config{out: "o.go", keys: "k", provenance: "P.md", work: "/work", report: "r.txt", only: []string{"grml", "shredos"}},
		},
		{name: "no flags names every missing one", args: nil, wantErr: "-out"},
		{name: "one missing", args: all[2:], wantErr: "-out"},
		{name: "a stray argument", args: append(slices.Clone(all), "extra"), wantErr: `"extra"`},
		{name: "an unknown flag", args: append(slices.Clone(all), "-nope"), wantErr: "nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFlags(tt.args, io.Discard)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseFlags error = %v, want one holding %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			if got.out != tt.want.out || got.keys != tt.want.keys || got.provenance != tt.want.provenance ||
				got.work != tt.want.work || got.report != tt.want.report || !slices.Equal(got.only, tt.want.only) {
				t.Errorf("parseFlags = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseFlags_Help(t *testing.T) {
	if _, err := parseFlags([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("parseFlags(-h) error = %v, want flag.ErrHelp", err)
	}
}

func TestUserAgent(t *testing.T) {
	if got := userAgent(); !strings.HasPrefix(got, "hephaestus-catalogpin/") || strings.HasSuffix(got, "/") {
		t.Errorf("userAgent = %q, want hephaestus-catalogpin/<revision or dev>", got)
	}
}

// runConfig returns a config over fresh directories, with a PROVENANCE.md
// that lists testFPR.
func runConfig(t *testing.T) config {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"keys", "work"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	prov := filepath.Join(dir, "PROVENANCE.md")
	if err := os.WriteFile(prov, []byte("| key | "+testFPR+" |\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return config{
		out: filepath.Join(dir, "catalog_pins.go"), keys: filepath.Join(dir, "keys"), provenance: prov,
		work: filepath.Join(dir, "work"), report: filepath.Join(dir, "report.txt"),
	}
}

func TestRun_WritesPinsReportAndSummary(t *testing.T) {
	good := makeZip(t, member{"f.bin", []byte("payload")})
	srv := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ok.zip" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(good)
	}), false)
	cfg := runConfig(t)
	in := pinRun{Specs: []catalog.Spec{
		zipSpec("ok", srv.URL+"/ok.zip", "f.bin"),
		zipSpec("bad", srv.URL+"/bad.zip", "f.bin"),
	}}
	var stdout bytes.Buffer

	if err := run(t.Context(), cfg, in, testClient(t, srv), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}

	if got, want := stdout.String(), "PINNED 1 ABSENT 1\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	out, err := os.ReadFile(cfg.out)
	if err != nil {
		t.Fatal(err)
	}
	if formatted, err := format.Source(out); err != nil || !bytes.Equal(formatted, out) {
		t.Errorf("the pins file is not gofmt-clean (%v):\n%s", err, out)
	}
	for _, want := range []string{`Entry: "ok"`, sha256Hex(good), `"http-404: `} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the pins file lacks %s:\n%s", want, out)
		}
	}
	report, err := os.ReadFile(cfg.report)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ok/amd64", "PINNED ", "bad/amd64", "ABSENT http-404: "} {
		if !strings.Contains(string(report), want) {
			t.Errorf("the report lacks %q:\n%s", want, report)
		}
	}
}

func TestRun_OnlyKeepsTheOthers(t *testing.T) {
	good := makeZip(t, member{"f.bin", []byte("payload")})
	h, reqs := counted(serveBytes(good))
	srv := startServer(t, h, false)
	cfg := runConfig(t)
	cfg.only = []string{"ok"}
	in := pinRun{
		Specs: []catalog.Spec{
			zipSpec("ok", srv.URL+"/ok.zip", "f.bin"),
			zipSpec("held", srv.URL+"/held.zip", "f.bin"),
		},
		Absent: map[catalog.Key]string{{Entry: "held", Arch: "amd64"}: "signature: key expired"},
	}

	if err := run(t.Context(), cfg, in, testClient(t, srv), io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}

	out, err := os.ReadFile(cfg.out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"signature: key expired"`) {
		t.Errorf("the held entry's reason was not kept:\n%s", out)
	}
	if n := reqs.Load(); n != 1 {
		t.Errorf("server saw %d requests, want 1", n)
	}
}

func TestRun_RejectsUnknownOnly(t *testing.T) {
	cfg := runConfig(t)
	cfg.only = []string{"nope"}
	in := pinRun{Specs: []catalog.Spec{zipSpec("ok", "https://upstream.example.test/ok.zip", "f.bin")}}

	err := run(t.Context(), cfg, in, nil, io.Discard)

	if err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("run error = %v, want one naming %q", err, "nope")
	}
	if _, statErr := os.Stat(cfg.out); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("the pins file was written for a refused run (stat: %v)", statErr)
	}
}

func TestRun_CancelledWritesNothing(t *testing.T) {
	cfg := runConfig(t)
	in := pinRun{Specs: []catalog.Spec{zipSpec("ok", "https://upstream.example.test/ok.zip", "f.bin")}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := run(ctx, cfg, in, nil, io.Discard)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want context.Canceled", err)
	}
	for _, p := range []string{cfg.out, cfg.report} {
		if _, statErr := os.Stat(p); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("%s was written for an interrupted run (stat: %v)", p, statErr)
		}
	}
}

func TestRun_UnwritableOutputFails(t *testing.T) {
	cfg := runConfig(t)
	cfg.out = filepath.Join(t.TempDir(), "no-such-dir", "catalog_pins.go")

	err := run(t.Context(), cfg, pinRun{}, nil, io.Discard)

	if err == nil {
		t.Fatal("run succeeded with an output directory that does not exist")
	}
}
