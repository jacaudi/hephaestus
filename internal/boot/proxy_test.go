package boot

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"go.jacaudi.dev/hephaestus/internal/telemetry"
)

const (
	proxyPeer     = "127.0.0.1:50000"
	untrustedPeer = "192.0.2.1:50000"
)

func trustedProxies() []netip.Prefix { return []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")} }

// trustCase is one TrustProxies request: the peer, the X-Forwarded-For header
// lines, and the client address the next handler must see, or "" for a 403.
type trustCase struct {
	name       string
	peer       string
	xff        []string
	wantClient string
}

// trustCases are the TestTrustProxies rows; FuzzTrustProxies seeds with all of them.
func trustCases() []trustCase {
	return []trustCase{
		{"untrusted peer", untrustedPeer, []string{"10.0.0.5"}, ""},
		{"unparsable peer", "garbage", []string{"10.0.0.5"}, ""},
		{"single entry", proxyPeer, []string{"10.0.0.5"}, "10.0.0.5"},
		{"rightmost wins over a spoofed left entry", proxyPeer, []string{"1.2.3.4, 10.0.0.5"}, "10.0.0.5"},
		{"trusted hop skipped", proxyPeer, []string{"10.0.0.5, 127.0.0.2"}, "10.0.0.5"},
		{"two header lines", proxyPeer, []string{"1.2.3.4", "10.0.0.5"}, "10.0.0.5"},
		{"missing header", proxyPeer, nil, ""},
		{"empty header", proxyPeer, []string{""}, ""},
		{"only separators", proxyPeer, []string{" , "}, ""},
		{"unparsable entry", proxyPeer, []string{"nope"}, ""},
		{"unparsable rightmost entry", proxyPeer, []string{"10.0.0.5, nope"}, ""},
		{"trailing comma", proxyPeer, []string{"10.0.0.5,"}, ""},
		{"entry with a port", proxyPeer, []string{"10.0.0.5:4000"}, ""},
		{"unparsable entry left of the client is never read", proxyPeer, []string{"nope, 10.0.0.5"}, "10.0.0.5"},
		{"only a trusted entry", proxyPeer, []string{"127.0.0.2"}, ""},
		{"only trusted entries", proxyPeer, []string{"127.0.0.2, 127.0.0.3"}, ""},
		{"IPv4-mapped client", proxyPeer, []string{"::ffff:10.0.0.5"}, "10.0.0.5"},
		{"IPv4-mapped trusted hop is skipped", proxyPeer, []string{"10.0.0.5, ::ffff:127.0.0.2"}, "10.0.0.5"},
		{"only an IPv4-mapped trusted entry", proxyPeer, []string{"::ffff:127.0.0.2"}, ""},
		{"IPv4-mapped trusted peer", "[::ffff:127.0.0.1]:50000", []string{"10.0.0.5"}, "10.0.0.5"},
		{"IPv6 client", proxyPeer, []string{"2001:db8::1"}, "2001:db8::1"},
		{"zone is not part of the client", proxyPeer, []string{"fe80::1%eth0"}, "fe80::1"},
		{"IPv4-mapped trusted entry with a zone is skipped", proxyPeer, []string{"10.0.0.5, ::ffff:127.0.0.2%eth0"}, "10.0.0.5"},
	}
}

// newProxiedRequest is a GET /boot from peer with the given X-Forwarded-For lines.
func newProxiedRequest(tb testing.TB, peer string, xff []string) *http.Request {
	tb.Helper()
	req := httptest.NewRequestWithContext(tb.Context(), http.MethodGet, "/boot", nil)
	req.RemoteAddr = peer
	for _, line := range xff {
		req.Header.Add("X-Forwarded-For", line)
	}
	return req
}

// recordClient is a handler that records whether it ran and the client address it saw.
type recordClient struct {
	called bool
	client netip.Addr
	err    error
}

func (c *recordClient) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.called = true
	c.client, c.err = clientAddr(r)
	w.WriteHeader(http.StatusOK)
}

func TestTrustProxies(t *testing.T) {
	for _, tc := range trustCases() {
		t.Run(tc.name, func(t *testing.T) {
			var next recordClient
			rec := httptest.NewRecorder()

			TrustProxies(trustedProxies(), &next).ServeHTTP(rec, newProxiedRequest(t, tc.peer, tc.xff))

			if tc.wantClient == "" {
				wantProblem(t, rec, http.StatusForbidden)
				if next.called {
					t.Error("the next handler ran for a refused request")
				}
				return
			}
			wantStatus(t, rec, http.StatusOK)
			if next.err != nil {
				t.Fatalf("clientAddr in the next handler: %v", next.err)
			}
			if got := next.client.String(); got != tc.wantClient {
				t.Errorf("client = %s, want %s", got, tc.wantClient)
			}
		})
	}
}

// TestTrustProxies_OnlyXForwardedFor pins that the client address comes from
// X-Forwarded-For alone: a client controls the other headers just as well.
func TestTrustProxies_OnlyXForwardedFor(t *testing.T) {
	tests := []struct {
		name       string
		xff        []string
		wantClient string
	}{
		{"others without X-Forwarded-For", nil, ""},
		{"others beside X-Forwarded-For", []string{"10.0.0.9"}, "10.0.0.9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var next recordClient
			rec := httptest.NewRecorder()
			req := newProxiedRequest(t, proxyPeer, tt.xff)
			req.Header.Set("Forwarded", "for=10.0.0.5")
			req.Header.Set("X-Real-IP", "10.0.0.5")

			TrustProxies(trustedProxies(), &next).ServeHTTP(rec, req)

			if tt.wantClient == "" {
				wantProblem(t, rec, http.StatusForbidden)
				return
			}
			wantStatus(t, rec, http.StatusOK)
			if got := next.client.String(); got != tt.wantClient {
				t.Errorf("client = %s, want %s", got, tt.wantClient)
			}
		})
	}
}

// FuzzTrustProxies checks what must hold for any peer and header: the next
// handler runs only for a trusted peer, with a valid client that is not itself
// a trusted proxy, and every other request is a 403 problem document. Header
// lines are separated by newlines in the fuzzed string.
func FuzzTrustProxies(f *testing.F) {
	for _, tc := range trustCases() {
		f.Add(tc.peer, strings.Join(tc.xff, "\n"))
	}
	trusted := trustedProxies()
	f.Fuzz(func(t *testing.T, peer, xff string) {
		var next recordClient
		rec := httptest.NewRecorder()

		TrustProxies(trusted, &next).ServeHTTP(rec, newProxiedRequest(t, peer, strings.Split(xff, "\n")))

		if !next.called {
			if rec.Code != http.StatusForbidden || rec.Header().Get("Content-Type") != contentTypeProblem {
				t.Errorf("refused with status %d, Content-Type %q; want a 403 problem document", rec.Code, rec.Header().Get("Content-Type"))
			}
			return
		}
		if next.err != nil || !next.client.IsValid() {
			t.Fatalf("the next handler ran with client %v, err %v", next.client, next.err)
		}
		peerAddr, err := clientAddrOf(peer)
		if err != nil || !inAny(trusted, peerAddr) {
			t.Errorf("the next handler ran for peer %q, which is not a trusted proxy", peer)
		}
		if inAny(trusted, next.client) {
			t.Errorf("client %s is inside a trusted prefix", next.client)
		}
		if next.client.Is4In6() {
			t.Errorf("client %s is IPv4-mapped, want it unmapped", next.client)
		}
	})
}

// inAny reports whether addr is inside any of prefixes.
func inAny(prefixes []netip.Prefix, addr netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// clientAddrOf is clientAddr for a bare RemoteAddr.
func clientAddrOf(remoteAddr string) (netip.Addr, error) {
	return clientAddr(&http.Request{RemoteAddr: remoteAddr})
}

// proxiedRequest is a request that reached a Proxied handler: plain HTTP from
// the proxy, carrying the client in X-Forwarded-For.
func proxiedRequest(t *testing.T, method, target, body, client string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", client)
	req.RemoteAddr = proxyPeer
	return req
}

func TestProxied_GuardUsesClient(t *testing.T) {
	tests := []struct {
		name   string
		client string
		want   int
	}{
		{"client inside the subnet", "10.10.20.50", http.StatusOK},
		{"client outside the subnet", "10.10.30.5", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newRig(t)
			rec := httptest.NewRecorder()

			g.h.Proxied(trustedProxies()).ServeHTTP(rec, proxiedRequest(t, http.MethodGet, "/tftp/amd64/snp.efi", "", tt.client))

			wantStatus(t, rec, tt.want)
		})
	}
}

// TestProxied_ServesTheTLSRoutes pins that a request through the proxy is
// decided like one on the TLS listener although it arrives as plain HTTP: the
// proxy terminated the TLS, so /boot must not answer with the chain script.
func TestProxied_ServesTheTLSRoutes(t *testing.T) {
	g := newRig(t)
	rec := httptest.NewRecorder()

	g.h.Proxied(trustedProxies()).ServeHTTP(rec, proxiedRequest(t, http.MethodGet, "/boot?"+bootQ, "", "10.10.20.50"))

	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.HasPrefix(body, "#!ipxe\nparams\n") {
		t.Errorf("body = %q, want the discovery report script, not the chain script", body)
	}
	if n := g.count("SELECT count(*) FROM machines"); n != 1 {
		t.Errorf("machines = %d, want 1: the boot was not recorded", n)
	}
}

func TestProxied_DiscoverLimiterKeysOnClient(t *testing.T) {
	g := newRig(t, WithLimiter(1, discoverWindow, limiterCapacity))
	post := func(client string, n int) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		g.h.Proxied(trustedProxies()).ServeHTTP(rec, proxiedRequest(t, http.MethodPost, "/discover", nthMachineForm(n).Encode(), client))
		return rec
	}

	wantStatus(t, post("10.10.20.50", 1), http.StatusOK)
	wantProblem(t, post("10.10.20.50", 2), http.StatusTooManyRequests)
	rec := post("10.10.20.51", 3)

	wantStatus(t, rec, http.StatusOK)
	if n := g.count("SELECT count(*) FROM machines"); n != 2 {
		t.Errorf("machines = %d, want 2", n)
	}
}

// TestProxied_WideEventCarriesClient pins that the request log line names the
// client the proxy reported: its remote_ip is only the proxy.
func TestProxied_WideEventCarriesClient(t *testing.T) {
	g := newRig(t)
	var buf bytes.Buffer
	handler := telemetry.WideEvents(g.h.Proxied(trustedProxies()), slog.New(slog.NewJSONHandler(&buf, nil)), slog.LevelInfo)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, proxiedRequest(t, http.MethodGet, "/tftp/amd64/snp.efi", "", "10.10.20.50"))

	wantStatus(t, rec, http.StatusOK)
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line is not one JSON object: %v\n%s", err, buf.String())
	}
	if line["client_ip"] != "10.10.20.50" || line["remote_ip"] != "127.0.0.1" {
		t.Errorf("client_ip = %v, remote_ip = %v, want 10.10.20.50 and 127.0.0.1", line["client_ip"], line["remote_ip"])
	}
}

// TestProxied_UntrustedPeerNeverRecorded pins that a peer that is not a trusted
// proxy cannot make the service record a boot by naming a client in the subnet.
func TestProxied_UntrustedPeerNeverRecorded(t *testing.T) {
	g := newRig(t)
	rec := httptest.NewRecorder()
	req := proxiedRequest(t, http.MethodGet, "/boot?"+bootQ, "", "10.10.20.50")
	req.RemoteAddr = untrustedPeer

	g.h.Proxied(trustedProxies()).ServeHTTP(rec, req)

	wantProblem(t, rec, http.StatusForbidden)
	if n := g.count("SELECT count(*) FROM machines"); n != 0 {
		t.Errorf("machines = %d, want 0", n)
	}
}
