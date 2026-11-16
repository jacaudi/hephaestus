package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"go.jacaudi.dev/hephaestus/internal/secret"
	"go.jacaudi.dev/hephaestus/internal/tlscert/tlscerttest"
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

// testDeps is the file-mode wiring: boot HTTP, boot HTTPS and admin listeners,
// and root as the one root the iPXE binaries trust.
func testDeps(t *testing.T, root *x509.Certificate) serveDeps {
	t.Helper()

	lns := listenLoopback(t, 3)
	return serveDeps{httpLn: lns[0], httpsLn: lns[1], adminLn: lns[2], files: testFiles(), roots: []*x509.Certificate{root}}
}

// testProxyDeps is the proxy-mode wiring: boot HTTP, proxy and admin listeners.
func testProxyDeps(t *testing.T) serveDeps {
	t.Helper()

	lns := listenLoopback(t, 3)
	return serveDeps{httpLn: lns[0], proxyLn: lns[1], adminLn: lns[2], files: testFiles()}
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
		boot := get(t, tlsClient, deps.httpsURL()+"/boot?"+bootQuery)
		if boot.status != http.StatusOK {
			t.Errorf("connection %d: GET /boot status = %d, want 200", i+1, boot.status)
		}
		if boot.resumed {
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
