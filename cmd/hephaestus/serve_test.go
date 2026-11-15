package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"go.jacaudi.dev/hephaestus/internal/datakey"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/secret"
	"go.jacaudi.dev/hephaestus/internal/tlsacme"
	"go.jacaudi.dev/hephaestus/internal/tlsacme/tlsacmetest"
)

const (
	// otherDataKey is a valid data key that is not testDataKey.
	otherDataKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	// bootQuery is a well-formed /boot query for a machine in the fleet.
	bootQuery = "uuid=4c4c4544-0042-3510-8052-b4c04f4e4d32&mac=aa-bb-cc-00-00-01&arch=x86_64&serial=S1&product=P1"
)

// testServeConfig is a serveConfig for a loopback run in a fresh data dir.
func testServeConfig(t *testing.T) serveConfig {
	t.Helper()

	key, err := datakey.Parse(secret.Secret(testDataKey))
	if err != nil {
		t.Fatalf("datakey.Parse: %v", err)
	}
	dir := t.TempDir()
	return serveConfig{
		FQDN:          "hephaestus.example.test",
		Interface:     "lo0",
		Address:       netip.MustParseAddr("127.0.0.1"),
		Subnet:        netip.MustParsePrefix("127.0.0.0/8"),
		DataDir:       dir,
		DatabaseURL:   secret.Secret("sqlite:" + filepath.Join(dir, "h.db")),
		DataKey:       key,
		ShutdownGrace: 5 * time.Second,
		LogFormat:     "json",
		LogLevel:      slog.LevelInfo,
	}
}

// selfSignedIssuer is a serveDeps.newIssuer that issues self-signed
// certificates; with a non-nil block it never issues until block is closed.
func selfSignedIssuer(block <-chan struct{}) func(context.Context, *tlsacme.Repository, *datakey.Key) (tlsacme.Issuer, error) {
	return func(context.Context, *tlsacme.Repository, *datakey.Key) (tlsacme.Issuer, error) {
		return &tlsacmetest.SelfSigned{
			NotBefore: time.Now().Add(-time.Hour),
			NotAfter:  time.Now().Add(90 * 24 * time.Hour),
			Block:     block,
		}, nil
	}
}

// testDeps listens on three loopback ports; runServe takes ownership of them.
func testDeps(t *testing.T, newIssuer func(context.Context, *tlsacme.Repository, *datakey.Key) (tlsacme.Issuer, error)) serveDeps {
	t.Helper()

	var lc net.ListenConfig
	var lns [3]net.Listener
	for i := range lns {
		ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		lns[i] = ln
	}
	return serveDeps{
		newIssuer: newIssuer,
		httpLn:    lns[0], httpsLn: lns[1], adminLn: lns[2],
		files: map[string][]byte{"amd64/snp.efi": []byte("amd64-efi"), "arm64/snp.efi": []byte("arm64-efi")},
	}
}

func (d serveDeps) httpURL() string  { return "http://" + d.httpLn.Addr().String() }
func (d serveDeps) httpsURL() string { return "https://" + d.httpsLn.Addr().String() }
func (d serveDeps) adminURL() string { return "http://" + d.adminLn.Addr().String() }

// discardLogger drops every record.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// startServe runs runServe in a goroutine. Cancelling stop begins shutdown;
// done delivers runServe's result.
func startServe(t *testing.T, cfg serveConfig, logger *slog.Logger, deps serveDeps) (stop context.CancelFunc, done <-chan error) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() { result <- runServe(ctx, cfg, logger, deps) }()
	return cancel, result
}

// waitDone returns the result of runServe, failing the test after within.
func waitDone(t *testing.T, done <-chan error, within time.Duration) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatalf("runServe did not return within %s", within)
		return nil
	}
}

// testClient is an HTTP client that keeps no connection open, so goleak sees
// no transport goroutine afterwards.
func testClient(tlsConfig *tls.Config) *http.Client {
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true, TLSClientConfig: tlsConfig},
	}
}

// reply is what a test needs of a response whose body was read and closed.
type reply struct {
	status     int
	tlsVersion uint16 // zero on a plain connection
	body       string
}

// get fetches url and fails the test on a transport error.
func get(t *testing.T, client *http.Client, url string) reply {
	t.Helper()

	resp, err := getE(t.Context(), client, url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close %s: %v", url, err)
	}
	r := reply{status: resp.StatusCode, body: string(body)}
	if resp.TLS != nil {
		r.tlsVersion = resp.TLS.Version
	}
	return r
}

// getE is a GET that returns the transport error instead of failing the test.
func getE(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// waitStatus polls url every 100ms until it answers 200, failing the test
// after 10s or when runServe returns first.
func waitStatus(t *testing.T, url string, done <-chan error) {
	t.Helper()

	client := testClient(nil)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("runServe returned before %s answered 200: %v", url, err)
		default:
		}
		resp, err := getE(t.Context(), client, url)
		if err == nil {
			if cerr := resp.Body.Close(); cerr != nil {
				t.Fatalf("close %s: %v", url, cerr)
			}
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not answer 200 within 10s", url)
}

// requireNeverServed fails if url still answers; call it after runServe returned.
func requireNeverServed(t *testing.T, url string) {
	t.Helper()

	resp, err := getE(t.Context(), testClient(nil), url)
	if err == nil {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("close %s: %v", url, cerr)
		}
		t.Errorf("%s answered %d, want a refused connection", url, resp.StatusCode)
	}
}

// waitStored polls until the certificate for cfg.FQDN is in the database, so
// stopping the service cannot race the Manager's store.
func waitStored(t *testing.T, cfg serveConfig) {
	t.Helper()

	handle, err := db.Open(t.Context(), cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() {
		if err := handle.Close(); err != nil {
			t.Errorf("close db: %v", err)
		}
	}()
	repo := tlsacme.NewRepository(handle)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, ok, err := repo.LoadCertificate(t.Context(), cfg.FQDN)
		if err != nil {
			t.Fatalf("LoadCertificate: %v", err)
		}
		if ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the issued certificate was not stored within 10s")
}

func TestRunServe_ServesAndDrains(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg := testServeConfig(t)
	deps := testDeps(t, selfSignedIssuer(nil))
	stop, done := startServe(t, cfg, discardLogger(), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)

	start := get(t, testClient(nil), deps.httpURL()+"/start")
	if start.status != http.StatusOK {
		t.Errorf("GET /start status = %d, want 200", start.status)
	}
	if want := "chain https://hephaestus.example.test/boot?"; !strings.Contains(start.body, want) {
		t.Errorf("GET /start body = %q, want it to contain %q", start.body, want)
	}

	tlsClient := testClient(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	boot := get(t, tlsClient, deps.httpsURL()+"/boot?"+bootQuery)
	if boot.status != http.StatusOK {
		t.Errorf("GET /boot status = %d, want 200", boot.status)
	}
	if !strings.HasPrefix(boot.body, "#!ipxe\nparams\n") {
		t.Errorf("GET /boot body = %q, want an iPXE script starting with params", boot.body)
	}
	if boot.tlsVersion != tls.VersionTLS13 {
		t.Errorf("GET /boot TLS version = %#x, want TLS 1.3 (%#x)", boot.tlsVersion, tls.VersionTLS13)
	}

	metrics := get(t, testClient(nil), deps.adminURL()+"/metrics")
	if !strings.Contains(metrics.body, "hephaestus_boot_decisions_total") {
		t.Errorf("GET /metrics lacks hephaestus_boot_decisions_total:\n%s", metrics.body)
	}

	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Errorf("runServe after cancel = %v, want nil", err)
	}
}

func TestRunServe_TLS12Refused(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg := testServeConfig(t)
	deps := testDeps(t, selfSignedIssuer(nil))
	stop, done := startServe(t, cfg, discardLogger(), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)

	tlsClient := testClient(&tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12})
	resp, err := getE(t.Context(), tlsClient, deps.httpsURL()+"/boot?"+bootQuery)
	if err == nil {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("close body: %v", cerr)
		}
		t.Fatalf("TLS 1.2 handshake succeeded with status %d, want it refused", resp.StatusCode)
	}
	if !strings.Contains(err.Error(), "protocol version") {
		t.Errorf("TLS 1.2 error = %v, want a protocol version failure", err)
	}

	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Errorf("runServe after cancel = %v, want nil", err)
	}
}

func TestRunServe_WrongDataKeyFails(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg := testServeConfig(t)
	deps := testDeps(t, selfSignedIssuer(nil))
	stop, done := startServe(t, cfg, discardLogger(), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)
	waitStored(t, cfg)
	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Fatalf("first run after cancel = %v, want nil", err)
	}

	other, err := datakey.Parse(secret.Secret(otherDataKey))
	if err != nil {
		t.Fatalf("datakey.Parse: %v", err)
	}
	cfg.DataKey = other
	deps = testDeps(t, selfSignedIssuer(nil))
	_, done = startServe(t, cfg, discardLogger(), deps)

	err = waitDone(t, done, 10*time.Second)
	if err == nil {
		t.Fatal("second run with the other data key returned nil, want an error")
	}
	if want := "cannot be decrypted with the configured data key"; !strings.Contains(err.Error(), want) {
		t.Errorf("second run error = %v, want it to contain %q", err, want)
	}
	requireNeverServed(t, deps.adminURL()+"/readyz")
}

func TestServeCmd_InvalidConfig(t *testing.T) {
	clearEnv(t)

	_, err := execute(t, "serve")
	if err == nil {
		t.Fatal("serve with no configuration returned nil, want an error")
	}
	if got := exitCode(err); got != 1 {
		t.Errorf("exitCode = %d, want 1", got)
	}
	if want := "HEPHAESTUS_FQDN is required"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to contain %q", err, want)
	}
}

// TestServeCmd_InheritedPersistentFlag proves serve hands loadConfig a flag set
// that includes the root's persistent --log-format: a bad value must reach the
// validation, which a missing flag would not.
func TestServeCmd_InheritedPersistentFlag(t *testing.T) {
	setMinimalEnv(t)

	_, err := execute(t, "serve", "--log-format=bogus")
	if err == nil {
		t.Fatal("serve --log-format=bogus returned nil, want an error")
	}
	if want := `HEPHAESTUS_LOG_FORMAT "bogus": must be json or text`; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to contain %q", err, want)
	}
}

func TestRunServe_IssuerFactoryErrorStops(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg := testServeConfig(t)
	deps := testDeps(t, func(context.Context, *tlsacme.Repository, *datakey.Key) (tlsacme.Issuer, error) {
		return nil, errors.New("account check failed")
	})
	_, done := startServe(t, cfg, discardLogger(), deps)

	err := waitDone(t, done, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "account check failed") {
		t.Errorf("runServe = %v, want an error containing %q", err, "account check failed")
	}
	requireNeverServed(t, deps.adminURL()+"/readyz")
}

func TestRunServe_ShutdownAbandonsObtain(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg := testServeConfig(t)
	cfg.ShutdownGrace = 2 * time.Second
	block := make(chan struct{})
	defer close(block) // runs before goleak: lets the abandoned Obtain finish

	deps := testDeps(t, selfSignedIssuer(block))
	stop, done := startServe(t, cfg, discardLogger(), deps)
	waitStatus(t, deps.adminURL()+"/healthz", done)

	stop()
	if err := waitDone(t, done, cfg.ShutdownGrace+time.Second); err != nil {
		t.Errorf("runServe after cancel = %v, want nil", err)
	}
}

func TestRunServe_AdminWideEvents(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	var logs bytes.Buffer
	cfg := testServeConfig(t)
	deps := testDeps(t, selfSignedIssuer(nil))
	stop, done := startServe(t, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), deps)
	waitStatus(t, deps.adminURL()+"/healthz", done)

	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Fatalf("runServe after cancel = %v, want nil", err)
	}

	// runServe has returned, so nothing writes to logs any more.
	for line := range bytes.SplitSeq(logs.Bytes(), []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec["msg"] == "request" && rec["path"] == "/healthz" {
			return
		}
	}
	t.Errorf("no \"request\" log line with path /healthz in:\n%s", logs.String())
}

func TestRunServe_ComponentFailureStopsEverything(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg := testServeConfig(t)
	deps := testDeps(t, selfSignedIssuer(nil))
	if err := deps.httpLn.Close(); err != nil { // Serve on a closed listener fails at once
		t.Fatalf("close listener: %v", err)
	}
	_, done := startServe(t, cfg, discardLogger(), deps)

	err := waitDone(t, done, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "boot http server") {
		t.Errorf("runServe = %v, want an error from the boot http server", err)
	}
	requireNeverServed(t, deps.adminURL()+"/healthz")
}
