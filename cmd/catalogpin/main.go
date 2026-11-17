// Command catalogpin pins the utility catalog: for every spec it downloads each
// upstream object over TLS 1.3, checks its size and signature before parsing
// it, extracts the files the catalog boots, and writes their sizes, hashes and
// pin ids to internal/catalog/catalog_pins.go and a report. An entry that
// cannot be pinned is written to the file's absent ledger with a class-prefixed
// reason. It is the developer tool behind `task catalog:pin` and runs inside a
// pinned container, with gpg.
//
// Exit codes: 0 once the file is written, 1 when it cannot be (or the run was
// interrupted), 2 on a usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/toolcache"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// config is the parsed command line.
type config struct {
	out        string // path of catalog_pins.go
	keys       string // directory of signer keys
	provenance string // path of PROVENANCE.md
	work       string // scratch directory for downloads
	report     string // path of the report
	only       []string
}

// parseFlags parses args. Every flag but -only is required; every missing one
// is reported together. -h prints the flags to stderr and returns flag.ErrHelp.
func parseFlags(args []string, stderr io.Writer) (config, error) {
	var cfg config
	var only string
	fs := flag.NewFlagSet("catalogpin", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.out, "out", "", "path of the generated catalog_pins.go")
	fs.StringVar(&cfg.keys, "keys", "", "directory of the signer key files")
	fs.StringVar(&cfg.provenance, "provenance", "", "path of keys/PROVENANCE.md")
	fs.StringVar(&cfg.work, "work", "", "scratch directory for downloads")
	fs.StringVar(&cfg.report, "report", "", "path of the report")
	fs.StringVar(&only, "only", "", "comma-separated entries to re-pin; every other entry keeps its compiled-in pin or absent reason")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stderr)
			fs.PrintDefaults()
		}
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	var missing []error
	for _, f := range []struct{ name, value string }{
		{"out", cfg.out}, {"keys", cfg.keys}, {"provenance", cfg.provenance}, {"work", cfg.work}, {"report", cfg.report},
	} {
		if f.value == "" {
			missing = append(missing, fmt.Errorf("-%s is required", f.name))
		}
	}
	if err := errors.Join(missing...); err != nil {
		return config{}, err
	}
	for entry := range strings.SplitSeq(only, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			cfg.only = append(cfg.only, entry)
		}
	}
	return cfg, nil
}

// userAgent is "hephaestus-catalogpin/<vcs.revision>", or "dev" when the
// binary carries no revision.
func userAgent() string {
	rev := "dev"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				rev = s.Value
			}
		}
	}
	return "hephaestus-catalogpin/" + rev
}

// checkOnly refuses an -only entry that names no spec: a typo would otherwise
// re-pin nothing and still rewrite the file.
func checkOnly(in pinRun) error {
	var errs []error
	for _, entry := range in.Only {
		if !slices.ContainsFunc(in.Specs, func(s catalog.Spec) bool { return s.Entry == entry }) {
			errs = append(errs, fmt.Errorf("-only: %q is not an entry of the catalog specs", entry))
		}
	}
	return errors.Join(errs...)
}

// readFile reads path through a root on its directory.
func readFile(path string) (data []byte, err error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open the directory of %s: %w", path, err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	data, err = root.ReadFile(filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}

// writeFile writes data to path, mode 0o600, through a root on its directory.
func writeFile(path string, data []byte) (err error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open the directory of %s: %w", path, err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	if err := root.WriteFile(filepath.Base(path), data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// run pins in with client, writes the pins file and the report, and prints
// "PINNED <n> ABSENT <m>" to stdout. It writes nothing when ctx is cancelled
// before the pinning ends: a partial result must not replace the file.
func run(ctx context.Context, cfg config, in pinRun, client *http.Client, stdout io.Writer) (err error) {
	in.Only = cfg.only
	if err := checkOnly(in); err != nil {
		return err
	}
	work, err := os.OpenRoot(cfg.work)
	if err != nil {
		return fmt.Errorf("open the work directory: %w", err)
	}
	defer func() { err = errors.Join(err, work.Close()) }()
	keys, err := os.OpenRoot(cfg.keys)
	if err != nil {
		return fmt.Errorf("open the keys directory: %w", err)
	}
	defer func() { err = errors.Join(err, keys.Close()) }()
	md, err := readFile(cfg.provenance)
	if err != nil {
		return err
	}
	p := &pinner{
		client: client, work: work, keys: keys, provenance: provenanceFingerprints(md),
		verify: gpgVerify, keyPrimary: gpgKeyPrimary, retryWait: defaultRetryWait,
	}

	pins, absent, lines := p.pinAll(ctx, in)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("interrupted, nothing written: %w", err)
	}
	src, err := render(pins, absent)
	if err != nil {
		return fmt.Errorf("render the pins file: %w", err)
	}
	summary := fmt.Sprintf("PINNED %d ABSENT %d", len(pins), len(absent))
	if err := writeFile(cfg.out, src); err != nil {
		return err
	}
	if err := writeFile(cfg.report, []byte(strings.Join(append(lines, summary), "\n")+"\n")); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, summary)
	return err
}

// cli runs the command line args and returns the exit code.
func cli(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := parseFlags(args, stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return exitOK
	case err != nil:
		report(stderr, err)
		return exitUsage
	}
	in := pinRun{Specs: catalog.Specs(), Existing: catalog.Pins(), Absent: catalog.Absent()}
	if err := run(ctx, cfg, in, toolcache.NewHTTPClient(userAgent(), nil), stdout); err != nil {
		report(stderr, err)
		return exitError
	}
	return exitOK
}

// report prints err to w.
func report(w io.Writer, err error) {
	if _, werr := fmt.Fprintf(w, "catalogpin: %v\n", err); werr != nil {
		return // stderr is gone; there is no one left to tell
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	// After the first signal, restore default handling so a second one kills a
	// stalled download.
	context.AfterFunc(ctx, stop)
	code := cli(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
