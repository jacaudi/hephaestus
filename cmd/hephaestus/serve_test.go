package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/goleak"

	"go.jacaudi.dev/hephaestus/internal/boot"
	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/catalog/catalogtest"
	"go.jacaudi.dev/hephaestus/internal/clusters"
	"go.jacaudi.dev/hephaestus/internal/clusters/gittest"
	"go.jacaudi.dev/hephaestus/internal/datakey"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/secret"
	"go.jacaudi.dev/hephaestus/internal/secrets"
	"go.jacaudi.dev/hephaestus/internal/store"
	"go.jacaudi.dev/hephaestus/internal/tlscert/tlscerttest"
	"go.jacaudi.dev/hephaestus/internal/toolcache"
)

const (
	// bootQuery is a well-formed /boot query for a machine in the fleet.
	bootQuery = "uuid=4c4c4544-0042-3510-8052-b4c04f4e4d32&mac=aa-bb-cc-00-00-01&arch=x86_64&serial=S1&product=P1"
	// testFQDN is the name the test certificates cover.
	testFQDN = "hephaestus.example.test"
)

// testServeConfig is a serveConfig for a loopback run in a fresh data dir, in
// file mode with a full chain for testFQDN. It also returns the chain's root,
// which testDeps passes to runServe as the root the iPXE binaries trust.
func testServeConfig(t *testing.T) (serveConfig, *x509.Certificate) {
	t.Helper()

	dir := t.TempDir()
	certFile, keyFile, root := writeTestChain(t, testFQDN)
	return serveConfig{
		FQDN:          testFQDN,
		Interface:     "lo0",
		Address:       netip.MustParseAddr("127.0.0.1"),
		Subnet:        netip.MustParsePrefix("127.0.0.0/8"),
		DataDir:       dir,
		DatabaseURL:   secret.Secret("sqlite:" + filepath.Join(dir, "h.db")),
		TLSMode:       tlsModeFile,
		TLSCertFile:   certFile,
		TLSKeyFile:    keyFile,
		DataKey:       parseKey(t, testDataKey),
		ShutdownGrace: 5 * time.Second,
		LogFormat:     "json",
		LogLevel:      slog.LevelInfo,
	}, root
}

// testProxyConfig is testServeConfig in proxy mode. The one trusted proxy is the
// test client's own address, 127.0.0.1: trusting all of 127.0.0.0/8 would make
// the 127.0.0.9 client the tests name another trusted proxy, which is refused.
func testProxyConfig(t *testing.T) serveConfig {
	t.Helper()

	cfg, _ := testServeConfig(t)
	cfg.TLSMode, cfg.TLSCertFile, cfg.TLSKeyFile = tlsModeProxy, "", ""
	cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	return cfg
}

// writeTestChain writes a leaf for name under a fresh root and intermediate,
// valid now for 90 days, with the intermediate in the certificate file as a
// fullchain.pem has it. It returns the files and the root.
func writeTestChain(t *testing.T, name string) (certFile, keyFile string, root *x509.Certificate) {
	t.Helper()
	return tlscerttest.WriteChain(t, t.TempDir(), name, time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour), true, true)
}

// parseKey parses an encoded data key.
func parseKey(t *testing.T, encoded string) *datakey.Key {
	t.Helper()
	k, err := datakey.Parse(secret.Secret(encoded))
	if err != nil {
		t.Fatalf("datakey.Parse: %v", err)
	}
	return k
}

// testCounter is a 1Password request counter that no client sends through.
func testCounter() *secrets.Counter { return secrets.NewCounter(http.DefaultTransport, time.Now) }

// listenLoopback opens n loopback listeners; runServe takes ownership of the ones it is given.
func listenLoopback(t *testing.T, n int) []net.Listener {
	t.Helper()

	var lc net.ListenConfig
	lns := make([]net.Listener, n)
	for i := range lns {
		ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		lns[i] = ln
	}
	return lns
}

// testFiles are the iPXE binaries the test servers serve.
func testFiles() map[string][]byte {
	return map[string][]byte{"amd64/snp.efi": []byte("amd64-efi"), "arm64/snp.efi": []byte("arm64-efi")}
}

// testCatalog is the fixture catalog the test servers boot utilities from.
func testCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	return catalogtest.New(t, "https://upstream.invalid").Catalog
}

// testDeps is the file-mode wiring: boot HTTP, boot HTTPS and admin listeners,
// and root as the one root the iPXE binaries trust.
func testDeps(t *testing.T, root *x509.Certificate) serveDeps {
	t.Helper()

	lns := listenLoopback(t, 3)
	return serveDeps{httpLn: lns[0], httpsLn: lns[1], adminLn: lns[2], files: testFiles(), roots: []*x509.Certificate{root}, catalog: testCatalog(t), client: testUpstreamClient(), counter: testCounter()}
}

// testProxyDeps is the proxy-mode wiring: boot HTTP, proxy and admin listeners.
func testProxyDeps(t *testing.T) serveDeps {
	t.Helper()

	lns := listenLoopback(t, 3)
	return serveDeps{httpLn: lns[0], proxyLn: lns[1], adminLn: lns[2], files: testFiles(), catalog: testCatalog(t), client: testUpstreamClient(), counter: testCounter()}
}

func (d serveDeps) httpURL() string  { return "http://" + d.httpLn.Addr().String() }
func (d serveDeps) httpsURL() string { return "https://" + d.httpsLn.Addr().String() }
func (d serveDeps) proxyURL() string { return "http://" + d.proxyLn.Addr().String() }
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
	resumed    bool   // the TLS handshake resumed an earlier session
	body       string
}

// get fetches url and fails the test on a transport error.
func get(t *testing.T, client *http.Client, url string) reply {
	t.Helper()
	return getWith(t, client, url, nil)
}

// getWith is get with extra request headers.
func getWith(t *testing.T, client *http.Client, url string, header http.Header) reply {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build GET %s: %v", url, err)
	}
	maps.Copy(req.Header, header)
	resp, err := client.Do(req)
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
		r.resumed = resp.TLS.DidResume
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

// probeResult is how the service met a probe: err is nil with the HTTP status
// it answered, or the error that ended the connection unanswered.
type probeResult struct {
	status int
	err    error
}

// probeReadyz connects to the admin listener and sends GET /readyz before
// runServe starts, so the request waits in the open listener's backlog. If the
// admin server ever starts serving it answers the probe; if runServe returns
// without serving, closing the listener ends the connection unanswered.
// (A request sent after runServe returned would be refused whatever the
// service did, which proves nothing.)
func probeReadyz(t *testing.T, deps serveDeps) <-chan probeResult {
	t.Helper()

	ctx := t.Context()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", deps.adminLn.Addr().String())
	if err != nil {
		t.Fatalf("dial admin listener: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, deps.adminURL()+"/readyz", http.NoBody)
	if err != nil {
		t.Fatalf("build probe: %v", err)
	}
	req.Close = true
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set probe deadline: %v", err)
	}
	if err := req.Write(conn); err != nil {
		t.Fatalf("send probe: %v", err)
	}

	out := make(chan probeResult, 1)
	go func() {
		var res probeResult
		resp, err := http.ReadResponse(bufio.NewReader(conn), req)
		if err != nil {
			res.err = err
		} else {
			res.status = resp.StatusCode
			if cerr := resp.Body.Close(); cerr != nil {
				t.Errorf("close probe body: %v", cerr)
			}
		}
		if cerr := conn.Close(); cerr != nil {
			t.Errorf("close probe connection: %v", cerr)
		}
		out <- res
	}()
	return out
}

// requireUnanswered fails if the service answered the probe; call it after runServe returned.
func requireUnanswered(t *testing.T, probe <-chan probeResult) {
	t.Helper()

	select {
	case res := <-probe:
		if res.err == nil {
			t.Errorf("the admin listener answered /readyz with status %d, want the connection dropped unanswered", res.status)
		}
	case <-time.After(5 * time.Second):
		t.Error("the /readyz probe was still pending 5s after runServe returned")
	}
}

func TestRunServe_ServesAndDrains(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg, root := testServeConfig(t)
	deps := testDeps(t, root)
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
	bootResp := get(t, tlsClient, deps.httpsURL()+"/boot?"+bootQuery)
	if bootResp.status != http.StatusOK {
		t.Errorf("GET /boot status = %d, want 200", bootResp.status)
	}
	if !strings.HasPrefix(bootResp.body, "#!ipxe\nparams\n") {
		t.Errorf("GET /boot body = %q, want an iPXE script starting with params", bootResp.body)
	}
	if bootResp.tlsVersion != tls.VersionTLS13 {
		t.Errorf("GET /boot TLS version = %#x, want TLS 1.3 (%#x)", bootResp.tlsVersion, tls.VersionTLS13)
	}

	metrics := get(t, testClient(nil), deps.adminURL()+"/metrics")
	if !strings.Contains(metrics.body, "hephaestus_boot_decisions_total") {
		t.Errorf("GET /metrics lacks hephaestus_boot_decisions_total:\n%s", metrics.body)
	}
	if !strings.Contains(metrics.body, "hephaestus_tls_certificate_days_left") {
		t.Errorf("GET /metrics lacks hephaestus_tls_certificate_days_left:\n%s", metrics.body)
	}
	health := get(t, testClient(nil), deps.adminURL()+"/health")
	for _, check := range []string{"tls:certificate", "tls:expiry"} {
		if !strings.Contains(health.body, check) {
			t.Errorf("GET /health lacks the %s check:\n%s", check, health.body)
		}
	}

	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Errorf("runServe after cancel = %v, want nil", err)
	}
}

// ticketCounter is a client session cache that counts the sessions stored in it.
// A TLS 1.3 client stores one for every NewSessionTicket it receives; a nil
// state is how the cache is told to drop an entry, so it is not counted.
type ticketCounter struct {
	tls.ClientSessionCache

	stored atomic.Int32
}

func (c *ticketCounter) Put(key string, cs *tls.ClientSessionState) {
	if cs != nil {
		c.stored.Add(1)
	}
	c.ClientSessionCache.Put(key, cs)
}

// TestRunServe_NoSessionTickets pins that the boot HTTPS listener issues no TLS
// session tickets. iPXE 9261ebe hashes a NewSessionTicket that arrives before
// its own Finished into the handshake transcript, so the handshake fails against
// a server that sends one. The client counts the tickets it receives, and a
// second connection that resumes would show one was issued.
func TestRunServe_NoSessionTickets(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg, root := testServeConfig(t)
	deps := testDeps(t, root)
	stop, done := startServe(t, cfg, discardLogger(), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)

	// One client, so both connections share the session cache. Its transport
	// keeps no connection alive, so the second request dials afresh.
	cache := &ticketCounter{ClientSessionCache: tls.NewLRUClientSessionCache(4)}
	tlsClient := testClient(&tls.Config{
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
		ClientSessionCache: cache,
	})
	for i := range 2 {
		bootResp := get(t, tlsClient, deps.httpsURL()+"/boot?"+bootQuery)
		if bootResp.status != http.StatusOK {
			t.Errorf("connection %d: GET /boot status = %d, want 200", i+1, bootResp.status)
		}
		if bootResp.resumed {
			t.Errorf("connection %d: the handshake resumed a session, so the server issued a ticket", i+1)
		}
		if n := cache.stored.Load(); n != 0 {
			t.Errorf("after connection %d: the client received %d session tickets, want 0", i+1, n)
		}
	}

	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Errorf("runServe after cancel = %v, want nil", err)
	}
}

// TestRunServe_MetricsIgnoreHostHeader pins that the boot listeners' request
// metrics carry the listener's address, not whatever Host a client sends.
func TestRunServe_MetricsIgnoreHostHeader(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg, root := testServeConfig(t)
	deps := testDeps(t, root)
	stop, done := startServe(t, cfg, discardLogger(), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, deps.httpURL()+"/start", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Host = "random-host-1.example:8123"
	resp, err := testClient(nil).Do(req)
	if err != nil {
		t.Fatalf("GET /start: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	metrics := get(t, testClient(nil), deps.adminURL()+"/metrics")
	if !strings.Contains(metrics.body, "http_server_request_duration_seconds") {
		t.Fatalf("GET /metrics lacks the request duration:\n%s", metrics.body)
	}
	for _, unwanted := range []string{"random-host-1", "8123"} {
		if strings.Contains(metrics.body, unwanted) {
			t.Errorf("GET /metrics contains the client-supplied %q", unwanted)
		}
	}

	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Errorf("runServe after cancel = %v, want nil", err)
	}
}

func TestRunServe_TLS12Refused(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg, root := testServeConfig(t)
	deps := testDeps(t, root)
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

// TestRunServe_CertificateForOtherNameFails pins that a certificate that does
// not cover HEPHAESTUS_FQDN stops startup before anything serves: the admin
// listener never answers the probe that waits in its backlog.
func TestRunServe_CertificateForOtherNameFails(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg, root := testServeConfig(t)
	cfg.TLSCertFile, cfg.TLSKeyFile, _ = writeTestChain(t, "other.example.test")
	deps := testDeps(t, root)
	probe := probeReadyz(t, deps)
	_, done := startServe(t, cfg, discardLogger(), deps)

	err := waitDone(t, done, 10*time.Second)

	if want := "does not cover " + testFQDN; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("runServe = %v, want an error containing %q", err, want)
	}
	requireUnanswered(t, probe)
}

// TestRunServe_UntrustedChainFails pins that a certificate whose chain does not
// reach a root the iPXE binaries trust stops startup before anything serves: the
// admin listener never answers the probe that waits in its backlog.
func TestRunServe_UntrustedChainFails(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg, root := testServeConfig(t)
	cfg.TLSCertFile, cfg.TLSKeyFile, _ = writeTestChain(t, testFQDN) // another root issued it
	deps := testDeps(t, root)
	probe := probeReadyz(t, deps)
	_, done := startServe(t, cfg, discardLogger(), deps)

	err := waitDone(t, done, 10*time.Second)

	if want := "does not reach a root the embedded iPXE trusts"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("runServe = %v, want an error containing %q", err, want)
	}
	requireUnanswered(t, probe)
}

// TestRunServe_FileModeWithoutRootsFails pins that file mode fails closed: with no
// iPXE roots to check the chain against, startup is refused rather than the check
// skipped, and nothing serves.
func TestRunServe_FileModeWithoutRootsFails(t *testing.T) {
	for name, roots := range map[string][]*x509.Certificate{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

			cfg, root := testServeConfig(t)
			deps := testDeps(t, root)
			deps.roots = roots
			probe := probeReadyz(t, deps)
			_, done := startServe(t, cfg, discardLogger(), deps)

			err := waitDone(t, done, 10*time.Second)

			if want := "file mode needs the embedded iPXE roots"; err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("runServe = %v, want an error containing %q", err, want)
			}
			requireUnanswered(t, probe)
		})
	}
}

// TestRunServe_ProxyMode pins proxy mode end to end: the proxy listener serves
// the TLS routes over plain HTTP to a trusted proxy that names a client, refuses
// a request that names none, and the service is ready with no TLS check.
func TestRunServe_ProxyMode(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg := testProxyConfig(t)
	deps := testProxyDeps(t)
	stop, done := startServe(t, cfg, discardLogger(), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)
	client := testClient(nil)

	named := getWith(t, client, deps.proxyURL()+"/boot?"+bootQuery, http.Header{"X-Forwarded-For": {"127.0.0.9"}})
	if named.status != http.StatusOK {
		t.Errorf("GET /boot with X-Forwarded-For status = %d, want 200", named.status)
	}
	if !strings.HasPrefix(named.body, "#!ipxe\nparams\n") {
		t.Errorf("GET /boot with X-Forwarded-For body = %q, want the boot decision script starting with params", named.body)
	}
	if anonymous := get(t, client, deps.proxyURL()+"/boot?"+bootQuery); anonymous.status != http.StatusForbidden {
		t.Errorf("GET /boot without X-Forwarded-For status = %d, want 403", anonymous.status)
	}

	health := get(t, client, deps.adminURL()+"/health")
	if strings.Contains(health.body, "tls:") {
		t.Errorf("GET /health lists a TLS check in proxy mode:\n%s", health.body)
	}

	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Errorf("runServe after cancel = %v, want nil", err)
	}
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

// holdOpen connects to addr and sends the start of a request without finishing
// it, so the server counts the connection as in flight until it is cut. The
// connection is closed when the test ends.
func holdOpen(t *testing.T, addr string) {
	t.Helper()

	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close %s: %v", addr, err)
		}
	})
	if _, err := io.WriteString(conn, "GET /start HTTP/1.1\r\nHost: held\r\n"); err != nil {
		t.Fatalf("write to %s: %v", addr, err)
	}
}

// TestRunServe_DrainSharesOneGrace pins that the boot servers and the admin
// server drain against one HEPHAESTUS_SHUTDOWN_GRACE deadline, not one each:
// with both stuck on an unfinished request, the process must still stop within
// roughly one grace, or a stop timeout equal to the grace ends in SIGKILL.
func TestRunServe_DrainSharesOneGrace(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg, root := testServeConfig(t)
	cfg.ShutdownGrace = 2 * time.Second
	deps := testDeps(t, root)
	stop, done := startServe(t, cfg, discardLogger(), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)
	holdOpen(t, deps.httpLn.Addr().String())
	holdOpen(t, deps.adminLn.Addr().String())

	start := time.Now()
	stop()
	err := waitDone(t, done, cfg.ShutdownGrace+time.Second)

	if elapsed := time.Since(start); elapsed < cfg.ShutdownGrace {
		t.Errorf("runServe returned after %s, before the %s grace: the held connections were not draining", elapsed, cfg.ShutdownGrace)
	}
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("runServe after a drain that hit the grace = %v, want a deadline error", err)
	}
}

func TestRunServe_AdminWideEvents(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	var logs bytes.Buffer
	cfg, root := testServeConfig(t)
	deps := testDeps(t, root)
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

// TestRunServe_LogsDrainAndStop pins that the shutdown reaches the JSON log
// stream: "draining" when the context is done, "stopped" after the last stage.
func TestRunServe_LogsDrainAndStop(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	var logs bytes.Buffer
	cfg, root := testServeConfig(t)
	deps := testDeps(t, root)
	stop, done := startServe(t, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)

	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Fatalf("runServe after cancel = %v, want nil", err)
	}

	// runServe has returned, so nothing writes to logs any more.
	var msgs []string
	for line := range bytes.SplitSeq(logs.Bytes(), []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if msg, _ := rec["msg"].(string); msg == "draining" || msg == "stopped" {
			if rec["level"] != "INFO" {
				t.Errorf("%q logged at level %v, want INFO", msg, rec["level"])
			}
			msgs = append(msgs, msg)
		}
	}
	if want := []string{"draining", "stopped"}; !slices.Equal(msgs, want) {
		t.Errorf("shutdown log messages = %v, want %v\n%s", msgs, want, logs.String())
	}
}

// TestFatalLogged pins that a fatal error after the logger exists is logged at
// Error level under the error key, once: the returned error is marked so main
// does not print it a second time, and still exits 1.
func TestFatalLogged(t *testing.T) {
	var logs, stderr bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	cause := errors.New("listen on 10.10.20.5:80: address in use")

	err := fatalLogged(t.Context(), logger, cause)

	var rec map[string]any
	if jerr := json.Unmarshal(logs.Bytes(), &rec); jerr != nil {
		t.Fatalf("log is not one JSON record: %v\n%s", jerr, logs.String())
	}
	if rec["level"] != "ERROR" || rec["error"] != cause.Error() {
		t.Errorf("record = %v, want level ERROR and error %q", rec, cause)
	}
	if !errors.Is(err, cause) {
		t.Errorf("returned error = %v, want one wrapping %v", err, cause)
	}
	if got := exitCode(err); got != 1 {
		t.Errorf("exitCode = %d, want 1", got)
	}
	reportError(&stderr, err)
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing for an error already logged", stderr.String())
	}
}

func TestReportError_PrintsUnloggedErrors(t *testing.T) {
	var stderr bytes.Buffer

	reportError(&stderr, errors.New("HEPHAESTUS_FQDN is required"))

	if want := "hephaestus: HEPHAESTUS_FQDN is required\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

func TestRunServe_ComponentFailureStopsEverything(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg, root := testServeConfig(t)
	deps := testDeps(t, root)
	if err := deps.httpLn.Close(); err != nil { // Serve on a closed listener fails at once
		t.Fatalf("close listener: %v", err)
	}
	_, done := startServe(t, cfg, discardLogger(), deps)

	err := waitDone(t, done, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "boot http server") {
		t.Errorf("runServe = %v, want an error from the boot http server", err)
	}
}

// TestRunServe_CorruptStoreIndexMovedAside pins that a store whose index.json
// cannot be decoded does not stop serve: the layout is set aside, one Error
// line says where, and the service starts with an empty store and drains.
func TestRunServe_CorruptStoreIndexMovedAside(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	var logs bytes.Buffer
	cfg, root := testServeConfig(t)
	storeDir := filepath.Join(cfg.DataDir, "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatalf("create store dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "index.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("write corrupt index: %v", err)
	}
	deps := testDeps(t, root)
	stop, done := startServe(t, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)

	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Fatalf("runServe after cancel = %v, want nil", err)
	}

	moved, err := filepath.Glob(filepath.Join(cfg.DataDir, "store.corrupt-*"))
	if err != nil || len(moved) != 1 {
		t.Fatalf("set-aside directories = %v, %v; want exactly one store.corrupt-*", moved, err)
	}
	// runServe has returned, so nothing writes to logs any more.
	var lines []map[string]any
	for line := range bytes.SplitSeq(logs.Bytes(), []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec["msg"] == "store was unusable; moved aside" {
			lines = append(lines, rec)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("%d \"store was unusable; moved aside\" lines, want 1\n%s", len(lines), logs.String())
	}
	if got := lines[0]["level"]; got != "ERROR" {
		t.Errorf("level = %v, want ERROR", got)
	}
	if got := lines[0]["path"]; got != moved[0] {
		t.Errorf("path = %v, want %v", got, moved[0])
	}
}

// testUpstreamClient is the upstream client the serve tests that fetch nothing
// run with: the production client, trusting the system roots.
func testUpstreamClient() *http.Client { return toolcache.NewHTTPClient("hephaestus/test", nil) }

// utilityUpstream is a TLS 1.3 fake object server. Its handler is swapped
// atomically, so the fixture catalog can be built from the server's own URL.
type utilityUpstream struct {
	*httptest.Server

	handler atomic.Pointer[http.Handler]
}

// newUtilityUpstream starts the server. The caller closes it with defer, after
// its goleak defer, so the upstream's connections close before goleak looks:
// the production client keeps its connections alive and exposes no transport.
func newUtilityUpstream() *utilityUpstream {
	u := &utilityUpstream{}
	u.serve(http.NotFoundHandler())
	u.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*u.handler.Load()).ServeHTTP(w, r)
	}))
	u.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	u.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	u.StartTLS()
	return u
}

func (u *utilityUpstream) serve(h http.Handler) { u.handler.Store(&h) }

// client is the production upstream client trusting this server.
func (u *utilityUpstream) client() *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(u.Certificate())
	return toolcache.NewHTTPClient("hephaestus/test", pool)
}

// serveObjects serves each object at its URL's path, with a strong ETag and range support.
func serveObjects(base string, objects map[string][]byte) http.Handler {
	mux := http.NewServeMux()
	for url, body := range objects {
		mux.HandleFunc("GET "+strings.TrimPrefix(url, base), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"v1"`)
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
		})
	}
	return mux
}

// openCache opens and migrates the database cfg names, as serve will find it.
// The returned function closes it; call it with defer, after the goleak defer,
// because an open database holds a goroutine.
func openCache(t *testing.T, cfg serveConfig) (*fleet.Repository, func()) {
	t.Helper()
	d, closeDB := openDB(t, cfg)
	return fleet.NewRepository(d), closeDB
}

// openDB opens and migrates the database cfg names; the returned function closes it, as openCache's does.
func openDB(t *testing.T, cfg serveConfig) (*db.DB, func()) {
	t.Helper()
	d, err := db.Open(t.Context(), cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := d.Migrate(t.Context()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return d, func() {
		if err := d.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	}
}

// waitCacheState polls the row at key until it is in state, failing the test after within.
func waitCacheState(t *testing.T, repo *fleet.Repository, key fleet.CacheKey, state fleet.CacheState, within time.Duration) fleet.CacheRow {
	t.Helper()
	var row fleet.CacheRow
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		var ok bool
		var err error
		row, ok, err = repo.CacheRow(t.Context(), key)
		if err != nil {
			t.Fatalf("CacheRow(%v): %v", key, err)
		}
		if ok && row.State == state {
			return row
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("row %v = %s (%q) after %s, want %s", key, row.State, row.LastError, within, state)
	return row
}

// logRecords decodes the JSON log lines whose msg is msg.
func logRecords(logs []byte, msg string) []map[string]any {
	var out []map[string]any
	for line := range bytes.SplitSeq(logs, []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func tshellAMD64Key() fleet.CacheKey { return fleet.CacheKey{Entry: "tshell", Arch: fleet.ArchAMD64} }

func TestRunServe_FetchesWantedUtility(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	up := newUtilityUpstream()
	defer up.Close()
	fx := catalogtest.New(t, up.URL)
	up.serve(serveObjects(up.URL, fx.Objects))

	var logs bytes.Buffer
	cfg, root := testServeConfig(t)
	deps := testDeps(t, root)
	deps.catalog, deps.client = fx.Catalog, up.client()
	repo, closeDB := openCache(t, cfg)
	defer closeDB()
	entry, _ := fx.Catalog.Lookup("tshell", "amd64")
	if _, err := repo.RequestFetch(t.Context(), tshellAMD64Key(), entry.Pin.ID, 0, time.Now()); err != nil {
		t.Fatalf("RequestFetch: %v", err)
	}
	stop, done := startServe(t, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)

	waitCacheState(t, repo, tshellAMD64Key(), fleet.CacheReady, 5*time.Second)

	health := get(t, testClient(nil), deps.adminURL()+"/health")
	var doc struct {
		Checks map[string][]struct {
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(health.body), &doc); err != nil {
		t.Fatalf("decode /health: %v\n%s", err, health.body)
	}
	if got := doc.Checks["store:readable"]; len(got) != 1 || got[0].Status != "pass" {
		t.Errorf("GET /health store:readable = %v, want one passing check:\n%s", got, health.body)
	}
	if _, ok := doc.Checks["store:bytes"]; !ok {
		t.Errorf("GET /health lacks the store:bytes check:\n%s", health.body)
	}
	if metrics := get(t, testClient(nil), deps.adminURL()+"/metrics"); !strings.Contains(metrics.body, "hephaestus_utility_fetch_bytes_total") {
		t.Errorf("GET /metrics lacks hephaestus_utility_fetch_bytes_total:\n%s", metrics.body)
	}

	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Fatalf("runServe after cancel = %v, want nil", err)
	}

	// runServe has returned, so nothing writes to logs any more.
	var ready int
	for _, rec := range logRecords(logs.Bytes(), "utility fetch") {
		if rec["to"] == "ready" {
			ready++
			if rec["entry"] != "tshell" || rec["arch"] != "amd64" || rec["pin"] != entry.Pin.ID || rec["from"] != "fetching" {
				t.Errorf("ready record = %v, want tshell/amd64 at %s from fetching", rec, entry.Pin.ID)
			}
		}
	}
	if ready != 1 {
		t.Errorf("%d \"utility fetch\" lines with to ready, want 1\n%s", ready, logs.String())
	}

	// The check is called directly: /health shows the value of its last
	// scheduled run, which may predate the fetch.
	st, _, err := store.Open(t.Context(), filepath.Join(cfg.DataDir, "store"), time.Now)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	var found bool
	for _, check := range st.Checks() {
		if check.Name != "store:bytes" {
			continue
		}
		found = true
		res := check.Run(t.Context())
		if n, ok := res.ObservedValue.(int64); !ok || n <= 0 {
			t.Errorf("store:bytes = %+v, want a positive byte count", res)
		}
	}
	if !found {
		t.Error("the store has no store:bytes check")
	}
}

func TestRunServe_ReconcilesBeforeServing(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	var logs bytes.Buffer
	cfg, root := testServeConfig(t)
	deps := testDeps(t, root)
	deps.catalog = catalogtest.New(t, "https://127.0.0.1:1").Catalog // nothing listens: a fetch fails at once
	repo, closeDB := openCache(t, cfg)
	defer closeDB()
	entry, _ := deps.catalog.Lookup("tshell", "amd64")
	if _, err := repo.RequestFetch(t.Context(), tshellAMD64Key(), entry.Pin.ID, 0, time.Now()); err != nil {
		t.Fatalf("RequestFetch: %v", err)
	}
	if _, claimed, err := repo.ClaimFetch(t.Context(), time.Now()); err != nil || !claimed { // wanted to fetching: the process that fetched it died
		t.Fatalf("ClaimFetch = %v, %v; want true, nil", claimed, err)
	}
	spool := filepath.Join(cfg.DataDir, "spool")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatalf("create spool dir: %v", err)
	}
	junk := filepath.Join(spool, "junk")
	if err := os.WriteFile(junk, []byte("partial"), 0o600); err != nil {
		t.Fatalf("write spool file: %v", err)
	}
	stop, done := startServe(t, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), deps)

	waitStatus(t, deps.adminURL()+"/readyz", done)

	if _, err := os.Stat(junk); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat the junk spool file = %v, want it removed", err)
	}
	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Fatalf("runServe after cancel = %v, want nil", err)
	}

	// runServe has returned, so nothing writes to logs any more.
	reconciled := logRecords(logs.Bytes(), "utility reconciled")
	if len(reconciled) != 1 {
		t.Fatalf("%d \"utility reconciled\" lines, want 1\n%s", len(reconciled), logs.String())
	}
	if rec := reconciled[0]; rec["interrupted"] != float64(1) || rec["spool_removed"] != float64(1) {
		t.Errorf("record = %v, want interrupted 1 and spool_removed 1", rec)
	}
	var order []string
	for line := range bytes.SplitSeq(logs.Bytes(), []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if msg, _ := rec["msg"].(string); msg == "utility reconciled" || msg == "request" {
			order = append(order, msg)
		}
	}
	if len(order) < 2 || order[0] != "utility reconciled" {
		t.Errorf("log order = %v, want \"utility reconciled\" before the first \"request\"", order)
	}
	if gc := logRecords(logs.Bytes(), "utility garbage collected"); len(gc) != 1 || gc[0]["untagged"] != float64(0) {
		t.Errorf("\"utility garbage collected\" records = %v, want one with untagged 0", gc)
	}
}

func TestRunServe_FetchFailureDoesNotStopServe(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	up := newUtilityUpstream()
	defer up.Close()
	up.serve(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) }))
	fx := catalogtest.New(t, up.URL)

	cfg, root := testServeConfig(t)
	deps := testDeps(t, root)
	deps.catalog, deps.client = fx.Catalog, up.client()
	repo, closeDB := openCache(t, cfg)
	defer closeDB()
	entry, _ := fx.Catalog.Lookup("tshell", "amd64")
	if _, err := repo.RequestFetch(t.Context(), tshellAMD64Key(), entry.Pin.ID, 0, time.Now()); err != nil {
		t.Fatalf("RequestFetch: %v", err)
	}
	stop, done := startServe(t, cfg, discardLogger(), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)

	waitCacheState(t, repo, tshellAMD64Key(), fleet.CacheFailed, 5*time.Second)

	client := testClient(nil)
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if probe := get(t, client, deps.adminURL()+"/readyz"); probe.status != http.StatusOK {
			t.Fatalf("GET /readyz after the failed fetch = %d, want 200", probe.status)
		}
	}
	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Errorf("runServe after cancel = %v, want nil", err)
	}
}

// TestRunServe_FetchWorkersBoundsConcurrentDownloads pins that serve builds the
// fetcher with cfg.FetchWorkers. serveDeps has no fetcher seam, so it is seen
// through the upstream: with 2 workers and 3 wanted rows against upstreams that
// block, exactly 2 downloads start and the third row waits until one is done.
func TestRunServe_FetchWorkersBoundsConcurrentDownloads(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	up := newUtilityUpstream()
	defer up.Close()
	fx := catalogtest.New(t, up.URL)
	objects := serveObjects(up.URL, fx.Objects)
	arrived, release := make(chan string, 4), make(chan struct{})
	var releaseOnce sync.Once
	open := func() { releaseOnce.Do(func() { close(release) }) }
	defer open() // runs before up.Close, which waits for the blocked handlers
	up.serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- r.URL.Path
		select {
		case <-release:
			objects.ServeHTTP(w, r)
		case <-r.Context().Done():
		}
	}))

	cfg, root := testServeConfig(t)
	cfg.FetchWorkers = 2
	deps := testDeps(t, root)
	deps.catalog, deps.client = fx.Catalog, up.client()
	repo, closeDB := openCache(t, cfg)
	defer closeDB()
	keys := []fleet.CacheKey{
		tshellAMD64Key(),
		{Entry: "tlive", Arch: fleet.ArchAMD64},
		{Entry: "tlive", Arch: fleet.ArchARM64},
	}
	for _, key := range keys {
		entry, _ := fx.Catalog.Lookup(key.Entry, string(key.Arch))
		if _, err := repo.RequestFetch(t.Context(), key, entry.Pin.ID, 0, time.Now()); err != nil {
			t.Fatalf("RequestFetch(%v): %v", key, err)
		}
	}
	stop, done := startServe(t, cfg, discardLogger(), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)

	for range cfg.FetchWorkers {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("fewer than %d downloads reached the upstream, want one per worker", cfg.FetchWorkers)
		}
	}
	select {
	case path := <-arrived:
		t.Fatalf("a third download (%s) reached the upstream with %d workers busy", path, cfg.FetchWorkers)
	case <-time.After(300 * time.Millisecond):
	}
	states := map[fleet.CacheState]int{}
	for _, key := range keys {
		row, _, err := repo.CacheRow(t.Context(), key)
		if err != nil {
			t.Fatalf("CacheRow(%v): %v", key, err)
		}
		states[row.State]++
	}
	if states[fleet.CacheFetching] != 2 || states[fleet.CacheWanted] != 1 {
		t.Errorf("row states = %v, want 2 fetching and 1 wanted", states)
	}

	open()
	for _, key := range keys {
		waitCacheState(t, repo, key, fleet.CacheReady, 10*time.Second)
	}
	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Errorf("runServe after cancel = %v, want nil", err)
	}
}

// TestRunServe_SpoolSweepFailureDoesNotStopServe pins that a spool that cannot
// be swept is a Warn and not a stop, like a failed garbage collection: stray
// files are only disk, and serve still comes ready.
func TestRunServe_SpoolSweepFailureDoesNotStopServe(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	var logs bytes.Buffer
	cfg, root := testServeConfig(t)
	spool := filepath.Join(cfg.DataDir, "spool")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatalf("create spool dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(spool, "stray"), []byte("partial"), 0o600); err != nil {
		t.Fatalf("write stray spool file: %v", err)
	}
	if err := os.Chmod(spool, 0o500); err != nil {
		t.Fatalf("chmod spool dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(spool, 0o700); err != nil {
			t.Errorf("restore spool dir mode: %v", err)
		}
	})
	deps := testDeps(t, root)
	stop, done := startServe(t, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)

	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Fatalf("runServe after cancel = %v, want nil", err)
	}

	// runServe has returned, so nothing writes to logs any more.
	warned := logRecords(logs.Bytes(), "utility spool sweep failed")
	if len(warned) != 1 {
		t.Fatalf("%d \"utility spool sweep failed\" lines, want 1\n%s", len(warned), logs.String())
	}
	if rec := warned[0]; rec["level"] != "WARN" || rec["error"] == "" || rec["error"] == nil {
		t.Errorf("record = %v, want level WARN and an error", rec)
	}
	if reconciled := logRecords(logs.Bytes(), "utility reconciled"); len(reconciled) != 1 {
		t.Errorf("%d \"utility reconciled\" lines, want 1: the summary is still logged", len(reconciled))
	}
}

// TestRunServe_GarbageCollectionFailureDoesNotStopServe pins that a failed
// garbage collection is a Warn and not a stop: the garbage is only disk, and
// serve still comes ready. A store directory nobody may write to makes the
// store's GC fail to save the index, which it replaces atomically.
func TestRunServe_GarbageCollectionFailureDoesNotStopServe(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	var logs bytes.Buffer
	cfg, root := testServeConfig(t)
	storeDir := filepath.Join(cfg.DataDir, "store")
	if _, _, err := store.Open(t.Context(), storeDir, time.Now); err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := os.Chmod(storeDir, 0o500); err != nil {
		t.Fatalf("chmod store dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(storeDir, 0o700); err != nil {
			t.Errorf("restore store dir mode: %v", err)
		}
	})
	deps := testDeps(t, root)
	stop, done := startServe(t, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)

	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Fatalf("runServe after cancel = %v, want nil", err)
	}

	// runServe has returned, so nothing writes to logs any more.
	warned := logRecords(logs.Bytes(), "utility garbage collection failed")
	if len(warned) != 1 {
		t.Fatalf("%d \"utility garbage collection failed\" lines, want 1\n%s", len(warned), logs.String())
	}
	if rec := warned[0]; rec["level"] != "WARN" || rec["untagged"] != float64(0) || rec["error"] == "" || rec["error"] == nil {
		t.Errorf("record = %v, want level WARN, untagged 0 and an error", rec)
	}
	if collected := logRecords(logs.Bytes(), "utility garbage collected"); len(collected) != 0 {
		t.Errorf("%d \"utility garbage collected\" lines after a failure, want none", len(collected))
	}
}

// TestBootErrorHandler_Level pins that a lost race with the CLI, which is
// expected now and then, logs at Warn, and every other boot error, lost
// content included, at Error.
func TestBootErrorHandler_Level(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		level string
	}{
		{name: "boot-next race", err: boot.ErrBootNextRace, level: "WARN"},
		{name: "wrapped boot-next race", err: fmt.Errorf("decide: %w", boot.ErrBootNextRace), level: "WARN"},
		{name: "lost content", err: boot.ErrUtilityMissing, level: "ERROR"},
		{name: "any other error", err: errors.New("disk on fire"), level: "ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer

			bootErrorHandler(slog.New(slog.NewJSONHandler(&logs, nil)))(t.Context(), tt.err)

			got := logRecords(logs.Bytes(), "boot error")
			if len(got) != 1 || got[0]["level"] != tt.level {
				t.Errorf("records = %v, want one \"boot error\" at %s", got, tt.level)
			}
		})
	}
}

// TestLogFetchEvent_ErrorIsPrintable logs an event whose error holds control
// and format runes; the logged error carries printable text only.
func TestLogFetchEvent_ErrorIsPrintable(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	logFetchEvent(t.Context(), logger, toolcache.FetchEvent{Err: errors.New("reset\u009b by\u202e peer\x1b[2J")})

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line %q is not JSON: %v", buf.Bytes(), err)
	}
	if got, want := rec["error"], "reset by peer[2J"; got != want {
		t.Errorf("logged error = %q, want %q", got, want)
	}
}

// TestServeHelp_ListsFetchWorkers: serve's help names every variable it reads,
// and HEPHAESTUS_FETCH_WORKERS is read by loadConfig.
func TestServeHelp_ListsFetchWorkers(t *testing.T) {
	_, optional, ok := strings.Cut(newServeCmd().Long, "Optional:")
	if !ok {
		t.Fatal("serve's help has no Optional paragraph")
	}
	optional, _, _ = strings.Cut(optional, "\n\n")

	if !strings.Contains(optional, "HEPHAESTUS_FETCH_WORKERS") {
		t.Errorf("serve's optional variables are %q, want HEPHAESTUS_FETCH_WORKERS among them", optional)
	}
}

// otherDataKey is base64 of 32 bytes of 0x02: a key the fixture database was not sealed with.
const otherDataKey = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="

// TestRunServe_WrongDataKeyStops pins that a data key other than the one the
// database's canary was sealed with stops startup, naming the variable, before
// anything serves.
func TestRunServe_WrongDataKeyStops(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg, root := testServeConfig(t)
	d, closeDB := openDB(t, cfg)
	if err := datakey.EnsureCanary(t.Context(), d.Bun(), cfg.DataKey); err != nil {
		t.Fatalf("EnsureCanary: %v", err)
	}
	closeDB()
	cfg.DataKey = parseKey(t, otherDataKey)
	deps := testDeps(t, root)
	probe := probeReadyz(t, deps)
	_, done := startServe(t, cfg, discardLogger(), deps)

	err := waitDone(t, done, 10*time.Second)

	if err == nil || !strings.Contains(err.Error(), "HEPHAESTUS_DATA_KEY") || !errors.Is(err, datakey.ErrCanary) {
		t.Errorf("runServe = %v, want datakey.ErrCanary naming HEPHAESTUS_DATA_KEY", err)
	}
	requireUnanswered(t, probe)
}

// healthEntry is one result of a check in the /health document.
type healthEntry struct {
	Status string `json:"status"`
	Output string `json:"output"`
}

// health fetches and decodes the admin listener's /health document, by check name.
func health(t *testing.T, deps serveDeps) map[string][]healthEntry {
	t.Helper()
	body := get(t, testClient(nil), deps.adminURL()+"/health").body
	var doc struct {
		Checks map[string][]healthEntry `json:"checks"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode /health: %v\n%s", err, body)
	}
	return doc.Checks
}

// TestRunServe_StartsWithoutOPToken pins that serve starts and comes ready with
// no 1Password token, and that secrets:token warns, naming the variable, only
// once a cluster exists. A refresh requested meanwhile stays pending.
func TestRunServe_StartsWithoutOPToken(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfg, root := testServeConfig(t)
	cfg.OPToken = ""
	deps := testDeps(t, root)
	stop, done := startServe(t, cfg, discardLogger(), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)
	checks := health(t, deps)
	if got := checks["secrets:token"]; len(got) != 1 || got[0].Status != "pass" {
		t.Errorf("secrets:token with no cluster = %+v, want one pass", got)
	}
	for _, name := range []string{"secrets:cache", "secrets:budget", "secrets:client", "git:fetch", "render:approved", "talos:cache", "config:pending"} {
		if _, ok := checks[name]; !ok {
			t.Errorf("GET /health lacks the %s check", name)
		}
	}
	if metrics := get(t, testClient(nil), deps.adminURL()+"/metrics"); !strings.Contains(metrics.body, "hephaestus_secrets_requests") {
		t.Errorf("GET /metrics lacks hephaestus_secrets_requests:\n%s", metrics.body)
	}
	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Fatalf("runServe after cancel = %v, want nil", err)
	}

	d, closeDB := openDB(t, cfg)
	defer closeDB()
	clusterRepo := clusters.NewRepository(d, cfg.DataKey)
	if _, err := clusterRepo.Import(t.Context(), clusters.ImportRequest{
		Name: "lab", RepoURL: "https://127.0.0.1:1/lab.git", Ref: "main", Subpath: "lab", At: time.Now(),
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if err := clusterRepo.RequestRefresh(t.Context(), "lab", fleet.ActorCLI, time.Now()); err != nil {
		t.Fatalf("RequestRefresh: %v", err)
	}
	deps = testDeps(t, root)
	stop, done = startServe(t, cfg, discardLogger(), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)
	got := health(t, deps)["secrets:token"]
	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Fatalf("runServe after cancel = %v, want nil", err)
	}

	if len(got) != 1 || got[0].Status != "warn" || !strings.Contains(got[0].Output, "HEPHAESTUS_OP_TOKEN") {
		t.Errorf("secrets:token with a cluster = %+v, want one warn naming HEPHAESTUS_OP_TOKEN", got)
	}
	if c, err := clusterRepo.Get(t.Context(), "lab"); err != nil || !c.RefreshRequested {
		t.Errorf("cluster lab refresh requested = %t, %v; want the request still pending", c.RefreshRequested, err)
	}
}

// labDefinition is a definition of cluster lab under lab/ that loads, with the
// shared secrets bundle, whose references a resolver is asked for.
func labDefinition(t *testing.T) map[string]string {
	t.Helper()
	bundle, err := os.ReadFile("../../internal/third_party/topf/render/testdata/bundle/secrets.yaml")
	if err != nil {
		t.Fatalf("read the secrets bundle: %v", err)
	}
	return map[string]string{
		"lab/topf.yaml": `clusterName: lab
clusterEndpoint: https://10.10.20.11:6443
kubernetesVersion: v1.35.10
talosVersion: v1.12.11
nodes:
  - host: cp-01
    role: control-plane
    ip: 10.10.20.11
`,
		"lab/secrets.yaml": string(bundle),
	}
}

// resolveUnwind is how long blockingResolver takes to return once its context
// ends, as a real call cut short takes a while to unwind.
const resolveUnwind = 300 * time.Millisecond

// blockingResolver waits for the end of its context on every call, then for
// resolveUnwind more; entered is closed when the first call starts.
type blockingResolver struct {
	once    sync.Once
	entered chan struct{}
}

func (b *blockingResolver) ResolveAll(ctx context.Context, _ []string) (map[string]secrets.Result, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	time.Sleep(resolveUnwind)
	return nil, &secrets.WholeCallError{Err: ctx.Err()}
}

// TestRunServe_ShutdownOrderClearsProcessing pins that the clusters service
// stops before the database closes: a processing mark it holds when serve is
// told to stop, here on a resolve cut short, is cleared rather than left for the
// next start to turn into a failed tree. The resolve unwinds for resolveUnwind
// after its context ends, so a service that serve does not wait for releases
// its mark only after the database is closed, and the mark stays.
func TestRunServe_ShutdownOrderClearsProcessing(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	srv.Commit(t, labDefinition(t), nil)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	tr := trustInDefaultTransport(t, srv.CAPEM())
	defer tr.CloseIdleConnections() // before goleak looks; the restore runs after it

	var logs bytes.Buffer
	cfg, root := testServeConfig(t)
	d, closeDB := openDB(t, cfg)
	defer closeDB()
	clusterRepo := clusters.NewRepository(d, cfg.DataKey)
	if _, err := clusterRepo.Import(t.Context(), clusters.ImportRequest{
		Name: "lab", RepoURL: srv.URL(gittest.DefaultRepo), Ref: "main", Subpath: "lab", At: time.Now(),
	}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	res := &blockingResolver{entered: make(chan struct{})}
	deps := testDeps(t, root)
	deps.resolver = res
	stop, done := startServe(t, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), deps)

	select {
	case <-res.entered:
	case <-time.After(10 * time.Second):
		stop()
		err := waitDone(t, done, 10*time.Second)
		t.Fatalf("the head tree never reached the resolver (runServe = %v):\n%s", err, logs.String())
	}
	if c, err := clusterRepo.Get(t.Context(), "lab"); err != nil || c.ProcessingTree == "" {
		t.Fatalf("processing tree while resolving = %q, %v; want the head tree marked", c.ProcessingTree, err)
	}
	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Fatalf("runServe after cancel = %v, want nil", err)
	}

	c, err := clusterRepo.Get(t.Context(), "lab")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if c.ProcessingTree != "" || c.ProcessingRevision != nil || c.FailedTree != "" {
		t.Errorf("after shutdown: processing tree %q, revision %v, failed tree %q; want no mark and no failed tree\n%s",
			c.ProcessingTree, c.ProcessingRevision, c.FailedTree, logs.String())
	}
}

// TestServeHelp_ExampleStartsServe: the example sets every variable serve requires, the data key included, and the
// help says what keeps working without the 1Password token.
func TestServeHelp_ExampleStartsServe(t *testing.T) {
	cmd := newServeCmd()
	if !strings.Contains(cmd.Example, "HEPHAESTUS_DATA_KEY_FILE=") {
		t.Errorf("serve's example does not set the data key:\n%s", cmd.Example)
	}
	if strings.Contains(cmd.Long, "fails closed") || !strings.Contains(cmd.Long, "approved configs") {
		t.Errorf("serve's help does not say that approved configs keep serving without the token:\n%s", cmd.Long)
	}
}

// TestServeHelp_ListsSecretVariables: serve's help names the data key and the
// 1Password token variables loadConfig reads, with their _FILE forms.
func TestServeHelp_ListsSecretVariables(t *testing.T) {
	help := newServeCmd().Long
	for _, name := range []string{"HEPHAESTUS_DATA_KEY", "HEPHAESTUS_DATA_KEY_FILE", "HEPHAESTUS_OP_TOKEN", "HEPHAESTUS_OP_TOKEN_FILE"} {
		if !regexp.MustCompile(`\b` + name + `\b`).MatchString(help) {
			t.Errorf("serve's help does not list %s:\n%s", name, help)
		}
	}
}

// TestLogFetchEvent_Source pins that a fetch event's line carries its source and
// names its row: a utility row by entry, arch and pin, a Talos row by its tuple.
func TestLogFetchEvent_Source(t *testing.T) {
	talos := fleet.TalosKey{Schematic: plainSchematic, Version: "v1.12.11", Arch: fleet.ArchAMD64}
	tests := []struct {
		name string
		ev   toolcache.FetchEvent
		msg  string
		want map[string]any
	}{
		{
			name: "utility",
			ev:   toolcache.FetchEvent{Source: toolcache.SourceUtility, Key: tshellAMD64Key(), Pin: "p1", From: fleet.CacheFetching, To: fleet.CacheReady},
			msg:  "utility fetch",
			want: map[string]any{"source": "utility", "entry": "tshell", "arch": "amd64", "pin": "p1", "to": "ready"},
		},
		{
			name: "talos",
			ev:   toolcache.FetchEvent{Source: toolcache.SourceTalos, Talos: talos, From: fleet.CacheFetching, To: fleet.CacheBroken, Class: "integrity"},
			msg:  "talos fetch",
			want: map[string]any{"source": "talos", "schematic": plainSchematic, "version": "v1.12.11", "arch": "amd64", "to": "broken", "class": "integrity"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer

			logFetchEvent(t.Context(), slog.New(slog.NewJSONHandler(&buf, nil)), tt.ev)

			recs := logRecords(buf.Bytes(), tt.msg)
			if len(recs) != 1 {
				t.Fatalf("%d %q lines, want 1:\n%s", len(recs), tt.msg, buf.String())
			}
			for k, want := range tt.want {
				if got := recs[0][k]; got != want {
					t.Errorf("%s = %v, want %v", k, got, want)
				}
			}
		})
	}
}

// alertCounts reads the hephaestus.alerts sums by kind.
func alertCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if m.Name != "hephaestus.alerts" || !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				kind, _ := dp.Attributes.Value(attribute.Key("kind"))
				out[kind.AsString()] += dp.Value
			}
		}
	}
	return out
}

// TestFetchEvents_TalosBrokenAlerts pins that a Talos row going broken raises the
// talos_broken alert, and that no other change does.
func TestFetchEvents_TalosBrokenAlerts(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	alerts, err := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test").
		Int64Counter("hephaestus.alerts", metric.WithDescription("Operator alerts raised, by kind."))
	if err != nil {
		t.Fatalf("alerts counter: %v", err)
	}
	handle := fetchEventHandler(discardLogger(), alerts)

	handle(t.Context(), toolcache.FetchEvent{Source: toolcache.SourceTalos, From: fleet.CacheFetching, To: fleet.CacheBroken})
	handle(t.Context(), toolcache.FetchEvent{Source: toolcache.SourceTalos, From: fleet.CacheFetching, To: fleet.CacheFailed})
	handle(t.Context(), toolcache.FetchEvent{Source: toolcache.SourceUtility, From: fleet.CacheFetching, To: fleet.CacheBroken})

	if got, want := alertCounts(t, reader), map[string]int64{"talos_broken": 1}; !maps.Equal(got, want) {
		t.Errorf("alerts = %v, want %v", got, want)
	}
}

// TestStoreErrorHandler_Level pins that a filesystem refusing a directory fsync,
// a known limit of some volumes, logs at Warn, and anything else at Error.
func TestStoreErrorHandler_Level(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		level string
	}{
		{name: "directory fsync unsupported", err: fmt.Errorf("%w: fsync blobs: invalid argument", store.ErrDirSyncUnsupported), level: "WARN"},
		{name: "any other error", err: errors.New("disk on fire"), level: "ERROR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer

			storeErrorHandler(slog.New(slog.NewJSONHandler(&logs, nil)))(t.Context(), tt.err)

			got := logRecords(logs.Bytes(), "store error")
			if len(got) != 1 || got[0]["level"] != tt.level {
				t.Errorf("records = %v, want one \"store error\" at %s", got, tt.level)
			}
		})
	}
}

// TestRunTicketPruning pins that pruning deletes the tickets consumed, revoked or
// expired more than a week ago, at once and not only after the first hour, and
// keeps the rest: one consumed six days ago, and a live one whose config
// recording is still being retried after its expiry.
func TestRunTicketPruning(t *testing.T) {
	d := dbtest.SQLite(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	repo := fleet.NewRepository(d)
	m, _, err := repo.RecordBoot(t.Context(), fleet.BootRecord{
		Identity: fleet.Identity{UUID: "4c4c4544-0042-3510-8052-b4c04f4e4d32", MAC: "aa:bb:cc:00:00:01", Serial: "S", Product: "P"},
		Arch:     fleet.ArchAMD64, Trigger: fleet.TriggerFirstContact,
		Event: fleet.BootEvent{Row: 7, Action: "hold", Reason: "awaiting approval"}, At: checkT0,
	})
	if err != nil {
		t.Fatalf("RecordBoot: %v", err)
	}
	ticket := func(id string, minted time.Time, consumed bool) fleet.Ticket {
		tk := fleet.Ticket{
			ID: id, MachineID: m.ID, TokenSHA256: "hash-" + id, ClusterID: "cluster-1", HostName: "cp-01",
			PairID: "pair-1", SecretVersionID: "version-1", MintedAt: minted, ExpiresAt: minted.Add(fleet.TicketTTL),
		}
		if consumed {
			tk.ConsumedAt = minted.Add(time.Minute)
		}
		return tk
	}
	tickets := []fleet.Ticket{
		ticket("old", checkT0.Add(-8*24*time.Hour), true),
		ticket("recent", checkT0.Add(-6*24*time.Hour), true),
		ticket("retrying", checkT0.Add(-fleet.TicketTTL-time.Hour), false),
	}
	if _, err := d.Bun().NewInsert().Model(&tickets).Exec(t.Context()); err != nil {
		t.Fatalf("insert tickets: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runTicketPruning(ctx, repo, discardLogger(), func() time.Time { return checkT0 }) }()

	var ids []string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		ids = nil
		if err := d.Bun().NewSelect().Model((*fleet.Ticket)(nil)).Column("id").Order("id").Scan(t.Context(), &ids); err != nil {
			t.Fatalf("read tickets: %v", err)
		}
		if !slices.Contains(ids, "old") {
			break
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("runTicketPruning = %v, want nil", err)
	}

	if want := []string{"recent", "retrying"}; !slices.Equal(ids, want) {
		t.Errorf("tickets after pruning = %v, want %v", ids, want)
	}
}

// refusingTransport fails every request without sending it.
type refusingTransport struct{}

func (refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no upstream in this test")
}

// TestRunServe_ReconcilesTalos pins that serve reconciles the Talos rows after
// the utility garbage collection and before anything serves: a row a dead
// process left fetching is counted as interrupted. The upstream client refuses
// every request, so the fetch that follows reaches no image factory.
func TestRunServe_ReconcilesTalos(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	var logs bytes.Buffer
	cfg, root := testServeConfig(t)
	deps := testDeps(t, root)
	deps.client = &http.Client{Transport: refusingTransport{}}
	d, closeDB := openDB(t, cfg)
	defer closeDB()
	key := fleet.TalosKey{Schematic: plainSchematic, Version: "v1.12.11", Arch: fleet.ArchAMD64}
	if _, err := fleet.WantTalosTx(t.Context(), d.Bun(), fleet.TalosWant{Key: key}, time.Now()); err != nil {
		t.Fatalf("WantTalosTx: %v", err)
	}
	if _, ok, err := fleet.NewRepository(d).ClaimTalosRow(t.Context(), key, time.Now()); err != nil || !ok {
		t.Fatalf("ClaimTalosRow = %v, %v; want the row fetching", ok, err)
	}
	stop, done := startServe(t, cfg, slog.New(slog.NewJSONHandler(&logs, nil)), deps)
	waitStatus(t, deps.adminURL()+"/readyz", done)
	stop()
	if err := waitDone(t, done, 10*time.Second); err != nil {
		t.Fatalf("runServe after cancel = %v, want nil", err)
	}

	// runServe has returned, so nothing writes to logs any more.
	if rec := logRecords(logs.Bytes(), "talos reconciled"); len(rec) != 1 || rec[0]["interrupted"] != float64(1) {
		t.Errorf("\"talos reconciled\" records = %v, want one with interrupted 1", rec)
	}
	var steps []string
	for line := range bytes.SplitSeq(logs.Bytes(), []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		msg, _ := rec["msg"].(string)
		if msg == "utility garbage collected" || msg == "talos reconciled" || (msg == "request" && !slices.Contains(steps, msg)) {
			steps = append(steps, msg)
		}
	}
	if want := []string{"utility garbage collected", "talos reconciled", "request"}; !slices.Equal(steps, want) {
		t.Errorf("startup log order = %v, want %v", steps, want)
	}
}
