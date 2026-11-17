package toolcache

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testUserAgent = "hephaestus/test"

// startTLS starts an httptest server that speaks TLS 1.3 only, or TLS 1.2 only
// when tls12 is set (the one carve-out: proving the client refuses it).
func startTLS(tb testing.TB, h http.Handler, tls12 bool) *httptest.Server {
	tb.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	if tls12 {
		srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}
	}
	srv.StartTLS()
	tb.Cleanup(srv.Close)
	return srv
}

// poolOf trusts the certificates of servers.
func poolOf(servers ...*httptest.Server) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, s := range servers {
		pool.AddCert(s.Certificate())
	}
	return pool
}

// get issues a GET through c and closes the body of whatever comes back.
func get(tb testing.TB, c *http.Client, url string) (int, error) {
	tb.Helper()
	req, err := http.NewRequestWithContext(tb.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		tb.Fatalf("NewRequest: %v", err)
	}
	resp, err := c.Do(req)
	if resp != nil {
		if cerr := resp.Body.Close(); cerr != nil {
			tb.Errorf("close body: %v", cerr)
		}
		return resp.StatusCode, err
	}
	return 0, err
}

func TestHTTPClient_StripsRefererOnRedirect(t *testing.T) {
	var mu sync.Mutex
	var gotReferer, gotUA string
	var reached bool
	b := startTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		reached = true
		gotReferer, gotUA = r.Header.Get("Referer"), r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}), false)
	a := startTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+"/final", http.StatusFound)
	}), false)

	status, err := get(t, NewHTTPClient(testUserAgent, poolOf(a, b)), a.URL+"/start")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reached {
		t.Fatal("redirect target was never reached")
	}
	if gotReferer != "" {
		t.Errorf("Referer = %q, want none", gotReferer)
	}
	if gotUA != testUserAgent {
		t.Errorf("User-Agent = %q, want %q", gotUA, testUserAgent)
	}
}

func TestHTTPClient_RefusesTLS12(t *testing.T) {
	srv := startTLS(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), true)

	_, err := get(t, NewHTTPClient(testUserAgent, poolOf(srv)), srv.URL)
	if err == nil {
		t.Fatal("Do succeeded against a TLS 1.2-only server")
	}
	if !strings.Contains(err.Error(), "protocol version") {
		t.Errorf("error = %v, want a protocol version refusal", err)
	}
	if _, ok := errors.AsType[*IntegrityError](err); ok {
		t.Errorf("a refused TLS version is a transport failure, not an IntegrityError: %v", err)
	}
}

func TestHTTPClient_Redirects(t *testing.T) {
	tests := []struct {
		name    string
		hops    int
		wantErr bool
	}{
		{name: "none", hops: 0},
		{name: "five succeed", hops: MaxRedirects},
		{name: "sixth fails", hops: MaxRedirects + 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var srv *httptest.Server
			srv = startTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var n int
				if _, err := fmt.Sscanf(r.URL.Path, "/hop/%d", &n); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if n < tt.hops {
					http.Redirect(w, r, fmt.Sprintf("%s/hop/%d", srv.URL, n+1), http.StatusFound)
					return
				}
				w.WriteHeader(http.StatusOK)
			}), false)

			status, err := get(t, NewHTTPClient(testUserAgent, poolOf(srv)), srv.URL+"/hop/0")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Do followed %d redirects without error (status %d)", tt.hops, status)
				}
				if _, ok := errors.AsType[*IntegrityError](err); ok {
					t.Errorf("too many redirects is an ordinary error, got IntegrityError: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			if status != http.StatusOK {
				t.Errorf("status = %d, want 200", status)
			}
		})
	}
}

func TestHTTPClient_NonHTTPSRedirectIsIntegrity(t *testing.T) {
	srv := startTLS(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/plain?token=s3cret", http.StatusFound)
	}), false)

	_, err := get(t, NewHTTPClient(testUserAgent, poolOf(srv)), srv.URL)
	ie, ok := errors.AsType[*IntegrityError](err)
	if !ok {
		t.Fatalf("error = %v, want an IntegrityError", err)
	}
	// net/url prefixes the whole URL to what Do returns; the IntegrityError is ours.
	if strings.Contains(ie.Error(), "s3cret") {
		t.Errorf("IntegrityError leaks the redirect query string: %v", ie)
	}
}

func TestHTTPClient_NoTotalTimeout(t *testing.T) {
	c := NewHTTPClient(testUserAgent, nil)
	if c.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (a total deadline would cut a large download)", c.Timeout)
	}
	ua, ok := c.Transport.(*userAgentTransport)
	if !ok {
		t.Fatalf("Transport = %T, want *userAgentTransport", c.Transport)
	}
	if got := ua.base.TLSClientConfig.MinVersion; got != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#x, want TLS 1.3 (%#x)", got, tls.VersionTLS13)
	}
	if !ua.base.DisableCompression {
		t.Error("DisableCompression = false, want true (the sha256 is over the encoded bytes)")
	}
	if got, want := ua.base.ResponseHeaderTimeout, time.Minute; got != want {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", got, want)
	}
	if ua.base.Proxy == nil {
		t.Error("Proxy is nil, want http.ProxyFromEnvironment")
	}
}

func TestHTTPClient_UserAgentOnEveryRequest(t *testing.T) {
	var got string
	srv := startTLS(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
	}), false)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("User-Agent", "caller/1")

	resp, err := NewHTTPClient(testUserAgent, poolOf(srv)).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	if got != testUserAgent {
		t.Errorf("server saw User-Agent %q, want %q", got, testUserAgent)
	}
	if h := req.Header.Get("User-Agent"); h != "caller/1" {
		t.Errorf("caller's request was mutated: User-Agent = %q", h)
	}
}

func TestIntegrityError(t *testing.T) {
	cause := errors.New("hash mismatch")
	err := error(&IntegrityError{Err: cause})
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is(%v, cause) = false, want true", err)
	}
	if !strings.Contains(err.Error(), "hash mismatch") {
		t.Errorf("Error() = %q, want the cause in it", err.Error())
	}
	if got := (&IntegrityError{}).Error(); got == "" {
		t.Error("Error() of a zero IntegrityError is empty")
	}
}
