package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"go.jacaudi.dev/hephaestus/internal/clusters/gittest"
	"go.jacaudi.dev/hephaestus/internal/factory/factorytest"
)

// The host names the program serves; run.sh points them at the program with --add-host, and gen-certs.sh issues
// their leaves. The overlay test/e2e/overlay/base.go.in makes factoryHost hephaestus's factory base.
const (
	factoryHost = "factory.e2e.test"
	// assetsHost is where the factory host redirects asset requests, as factory.talos.dev sends clients to
	// assets.factory.talos.dev, so a client must follow a 302 to another host name.
	assetsHost = "assets.factory.e2e.test"
	gitHost    = "git.e2e.test"
)

// The one image the fake factory serves: the plain schematic and the Talos version of the fixture cluster.
const (
	plainSchematic = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"
	talosVersion   = "v1.12.11"
)

// repoName is the repository built from the fixture tree and served as /lab.git.
const repoName = "lab"

// fakeKernel is an iPXE script in place of a kernel. iPXE's kernel command refuses an image it cannot recognise, which
// would end the install script before its initrd line; a script it runs, so the VM's serial console shows the install
// script reached its boot line.
const fakeKernel = "#!ipxe\necho HEPHAESTUS-E2E-TALOS-KERNEL booted\nsleep 600\n"

const (
	exitError = 1
	exitUsage = 2
)

// shutdownGrace bounds the drain of in-flight requests after a signal.
const shutdownGrace = 10 * time.Second

// config is what the program serves and where; loadConfig reads it from the environment.
type config struct {
	Addr                            string
	FactoryCertFile, FactoryKeyFile string
	AssetsCertFile, AssetsKeyFile   string
	GitCertFile, GitKeyFile         string
	FixtureDir                      string
	WorkDir                         string
	// Log receives the JSON log lines; main sets it to stdout.
	Log io.Writer
}

// loadConfig reads every UPSTREAMS_ variable through getenv. Each is required; all missing ones are reported together.
func loadConfig(getenv func(string) string) (config, error) {
	var cfg config
	var missing []error
	for _, v := range []struct {
		name string
		dst  *string
	}{
		{"UPSTREAMS_ADDR", &cfg.Addr},
		{"UPSTREAMS_FACTORY_CERT_FILE", &cfg.FactoryCertFile},
		{"UPSTREAMS_FACTORY_KEY_FILE", &cfg.FactoryKeyFile},
		{"UPSTREAMS_ASSETS_CERT_FILE", &cfg.AssetsCertFile},
		{"UPSTREAMS_ASSETS_KEY_FILE", &cfg.AssetsKeyFile},
		{"UPSTREAMS_GIT_CERT_FILE", &cfg.GitCertFile},
		{"UPSTREAMS_GIT_KEY_FILE", &cfg.GitKeyFile},
		{"UPSTREAMS_FIXTURE_DIR", &cfg.FixtureDir},
		{"UPSTREAMS_WORK_DIR", &cfg.WorkDir},
	} {
		*v.dst = getenv(v.name)
		if *v.dst == "" {
			missing = append(missing, fmt.Errorf("%s is required", v.name))
		}
	}
	return cfg, errors.Join(missing...)
}

// factoryAssets is what the fake factory serves, by the file names factory.FileName gives the amd64 and arm64 parts.
func factoryAssets() map[string][]byte {
	return map[string][]byte{
		"kernel-amd64":        []byte(fakeKernel),
		"initramfs-amd64.xz":  []byte("hephaestus e2e fake initramfs\n"),
		"cmdline-metal-amd64": []byte("talos.platform=metal console=tty0 console=ttyS0 init_on_alloc=1 slab_nomerge pti=on\n"),
		"kernel-arm64":        []byte(fakeKernel),
		"initramfs-arm64.xz":  []byte("hephaestus e2e fake arm64 initramfs\n"),
		"cmdline-metal-arm64": []byte("talos.platform=metal console=tty0 console=ttyAMA0 init_on_alloc=1 slab_nomerge\n"),
	}
}

// run builds the repository, then serves until ctx ends and drains in-flight requests.
func run(ctx context.Context, cfg config) error {
	logger := slog.New(slog.NewJSONHandler(cfg.Log, nil))

	var certs []tls.Certificate
	for _, pair := range [][2]string{
		{cfg.FactoryCertFile, cfg.FactoryKeyFile}, {cfg.AssetsCertFile, cfg.AssetsKeyFile}, {cfg.GitCertFile, cfg.GitKeyFile},
	} {
		cert, err := tls.LoadX509KeyPair(pair[0], pair[1])
		if err != nil {
			return fmt.Errorf("load the certificate %s: %w", pair[0], err)
		}
		certs = append(certs, cert)
	}
	if err := buildRepo(ctx, cfg.FixtureDir, cfg.WorkDir); err != nil {
		return err
	}

	fake := factorytest.New(factorytest.Options{Schematic: plainSchematic, Version: talosVersion, Assets: factoryAssets()})
	mux := http.NewServeMux()
	mux.Handle(factoryHost+"/image/", redirectTo("https://"+assetsHost, fake))
	mux.Handle(factoryHost+"/schematics", fake)
	mux.Handle(assetsHost+"/assets/", fake)
	mux.Handle(gitHost+"/", gittest.Handler(cfg.WorkDir))

	srv := &http.Server{
		Handler: logRequests(logger, mux),
		// With more than one certificate, the server picks the one whose name the client's SNI asks for.
		TLSConfig:         &tls.Config{Certificates: certs, MinVersion: tls.VersionTLS13},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      time.Minute,
		IdleTimeout:       time.Minute,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	logger.InfoContext(ctx, "listening", "addr", ln.Addr().String()) //nolint:forbidigo // a program, not library code: run.sh waits for this line

	served := make(chan error, 1)
	go func() { served <- srv.ServeTLS(ln, "", "") }()
	select {
	case err := <-served:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	// ctx is already done; the drain gets its own deadline.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down: %w", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// buildRepo commits a copy of the fixture tree to a new bare repository <work>/lab.git on the branch main, with the
// host git. The copy keeps git off the fixture, which the harness mounts read-only and owned by another user.
func buildRepo(ctx context.Context, fixture, work string) error {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("git is required: %w", err)
	}
	tree := filepath.Join(work, "tree")
	if err := os.CopyFS(tree, os.DirFS(fixture)); err != nil {
		return fmt.Errorf("copy the fixture: %w", err)
	}
	bare := filepath.Join(work, repoName+".git")
	repo := []string{"--git-dir=" + bare, "--work-tree=" + tree}
	for _, args := range [][]string{
		{"init", "--quiet", "--bare", "--initial-branch=main", bare},
		append(repo, "add", "--all"),
		append(repo, "-c", "user.name=upstreams", "-c", "user.email=upstreams@e2e.test", "-c", "commit.gpgsign=false",
			"commit", "--quiet", "-m", "e2e fixture cluster"),
	} {
		cmd := exec.CommandContext(ctx, gitPath, args...) //nolint:gosec // G204: the path is git's, and the arguments are this program's own
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
		}
	}
	return nil
}

// redirectTo makes next's relative redirects absolute on base, another host.
func redirectTo(base string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&redirectWriter{ResponseWriter: w, base: base}, r)
	})
}

// redirectWriter prefixes a relative Location with base when the status is written.
type redirectWriter struct {
	http.ResponseWriter

	base string
}

// WriteHeader rewrites a relative Location, then writes the status.
func (w *redirectWriter) WriteHeader(code int) {
	if loc := w.Header().Get("Location"); strings.HasPrefix(loc, "/") {
		w.Header().Set("Location", w.base+loc)
	}
	w.ResponseWriter.WriteHeader(code)
}

// logRequests writes one "request" line per request: the host it was sent to, the protocol, the method, the path
// without the query (an asset URL carries its expiry there) and the status.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		logger.InfoContext(r.Context(), "request", //nolint:forbidigo // a program, not library code: the harness reads these lines
			"host", r.Host, "proto", r.Proto, "method", r.Method, "path", r.URL.Path, "status", sw.status)
	})
}

// statusWriter records the status a handler wrote.
type statusWriter struct {
	http.ResponseWriter

	status int
}

// WriteHeader records code and writes it.
func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap returns the wrapped writer, for http.ResponseController.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// cli runs the program for args and the environment getenv reads, and returns its exit code. With no arguments it
// serves; validate-events checks an e2e event record instead.
func cli(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		if args[0] != "validate-events" {
			report(stderr, fmt.Errorf("unknown subcommand %q; the only one is validate-events", args[0]))
			return exitUsage
		}
		return validateEventsCLI(args[1:], stdout, stderr)
	}
	cfg, err := loadConfig(getenv)
	if err != nil {
		report(stderr, err)
		return exitUsage
	}
	cfg.Log = stdout
	if err := run(ctx, cfg); err != nil {
		report(stderr, err)
		return exitError
	}
	return 0
}

// validateEventsCLI runs validate-events <schema> <events.jsonl>: it prints one line and exits 0 when the record is
// valid, and prints every problem and exits 1 when it is not.
func validateEventsCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		report(stderr, errors.New("usage: upstreams validate-events <schema> <events.jsonl>"))
		return exitUsage
	}
	n, err := validateEvents(args[0], args[1])
	if err != nil {
		report(stderr, err)
		return exitError
	}
	if _, err := fmt.Fprintf(stdout, "%s matches %s (%d events)\n", args[1], args[0], n); err != nil {
		return exitError
	}
	return 0
}

// validateEvents checks the JSON Lines record at eventsPath: every line validates against the JSON Schema at
// schemaPath, and its seq is its line number, so the events count from 1 with none lost or repeated. It returns how
// many events the record holds, or every problem it found.
func validateEvents(schemaPath, eventsPath string) (int, error) {
	sch, err := compileSchema(schemaPath)
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(eventsPath) //nolint:gosec // G304: reading the file the caller names is the point
	if err != nil {
		return 0, fmt.Errorf("read the record: %w", err)
	}
	var problems []error
	n := 0
	for line := range bytes.Lines(data) {
		n++
		if err := validateEvent(sch, n, bytes.TrimSuffix(line, []byte("\n"))); err != nil {
			problems = append(problems, fmt.Errorf("%s line %d: %w", eventsPath, n, err))
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("%s: no events", eventsPath)
	}
	if err := errors.Join(problems...); err != nil {
		return 0, err
	}
	return n, nil
}

// validateEvent checks one line of a record, the seq-th.
func validateEvent(sch *jsonschema.Schema, seq int, line []byte) error {
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(line))
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if err := sch.Validate(inst); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	var ev struct {
		Seq int `json:"seq"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return fmt.Errorf("decode seq: %w", err)
	}
	if ev.Seq != seq {
		return fmt.Errorf("seq %d, want %d", ev.Seq, seq)
	}
	return nil
}

// compileSchema compiles the JSON Schema at path. Format is asserted too: draft 2020-12 treats it as an annotation
// unless the validator is told otherwise.
func compileSchema(path string) (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	sch, err := c.Compile(path)
	if err != nil {
		return nil, fmt.Errorf("compile the schema: %w", err)
	}
	return sch, nil
}

// report prints err to w.
func report(w io.Writer, err error) {
	if _, werr := fmt.Fprintf(w, "upstreams: %v\n", err); werr != nil {
		return // stderr is gone; there is no one left to tell
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	// After the first signal, restore default handling so a second one kills a stalled drain.
	context.AfterFunc(ctx, stop)
	code := cli(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
