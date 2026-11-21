package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/storage/memory"

	"go.jacaudi.dev/hephaestus/internal/secrets"
	"go.jacaudi.dev/hephaestus/internal/toolcache"
)

// isolateDefaultTransport returns the *http.Transport behind http.DefaultTransport and restores, when the test ends,
// every setting setupDefaultTransport changes, so no other test sees them.
func isolateDefaultTransport(t *testing.T) *http.Transport {
	t.Helper()

	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is %T, want *http.Transport", http.DefaultTransport)
	}
	tlsConfig, dial, handshake, header := tr.TLSClientConfig, tr.DialContext, tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout
	client := http.DefaultClient.Transport
	t.Cleanup(func() {
		tr.CloseIdleConnections()
		tr.TLSClientConfig, tr.DialContext, tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout = tlsConfig, dial, handshake, header
		http.DefaultClient.Transport = client
	})
	return tr
}

// trustInDefaultTransport makes the default transport trust the certificate in caPEM, at the TLS 1.3 floor, until
// the test ends, and returns it: go-git fetches through it when no CA bundle is given, as serve's do.
func trustInDefaultTransport(t *testing.T, caPEM []byte) *http.Transport {
	t.Helper()

	tr := isolateDefaultTransport(t)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("the git server's certificate does not parse")
	}
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}
	return tr
}

func TestSetupDefaultTransport_FloorReachesClients(t *testing.T) {
	tr := isolateDefaultTransport(t)
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	// The one tls.Config below TLS 1.3: a server limited to TLS 1.2 is the negative case that proves the clients'
	// floor, which cannot be shown without a server under it.
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	setupDefaultTransport(secrets.NewCounter(http.DefaultTransport, time.Now))

	if got, ok := http.DefaultTransport.(*http.Transport); !ok || got != tr {
		t.Fatalf("http.DefaultTransport = %T %p after setup, want the original *http.Transport %p changed in place", http.DefaultTransport, got, tr)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Errorf("TLSClientConfig = %+v, want MinVersion TLS 1.3", tr.TLSClientConfig)
	}
	if tr.TLSHandshakeTimeout != 10*time.Second || tr.ResponseHeaderTimeout != time.Minute || tr.DialContext == nil {
		t.Errorf("handshake timeout %s, response header timeout %s, dialer set %t; want 10s, 1m0s and a dialer",
			tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout, tr.DialContext != nil)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("close body: %v", cerr)
		}
		t.Errorf("http.DefaultClient reached a TLS 1.2 server with status %d, want the handshake refused", resp.StatusCode)
	} else if !strings.Contains(err.Error(), "protocol version") {
		t.Errorf("http.DefaultClient error = %v, want a protocol version failure", err)
	}

	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: git.DefaultRemoteName, URLs: []string{srv.URL + "/repo.git"}})
	if _, err := remote.ListContext(t.Context(), &git.ListOptions{}); err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Errorf("go-git ls-remote error = %v, want a protocol version failure", err)
	}
}

func TestSetupDefaultTransport_CountsOnePassword(t *testing.T) {
	tr := isolateDefaultTransport(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	counter := secrets.NewCounter(http.DefaultTransport, time.Now)

	setupDefaultTransport(counter)
	setupDefaultTransport(counter) // a second call must not count a request twice

	addr := srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://my.1password.com/", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET through http.DefaultClient: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	if n := counter.Last24h(); n != 1 {
		t.Errorf("counted %d 1Password requests, want 1", n)
	}
}

func TestSetupDefaultTransport_ToolcacheClientStillBuilds(t *testing.T) {
	isolateDefaultTransport(t)

	setupDefaultTransport(secrets.NewCounter(http.DefaultTransport, time.Now))

	if c := toolcache.NewHTTPClient("hephaestus/test", nil); c == nil {
		t.Error("toolcache.NewHTTPClient = nil after setup")
	}
}
