package boot

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestRequireSubnet(t *testing.T) {
	subnet := netip.MustParsePrefix("10.10.20.0/24")
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name       string
		remoteAddr string
		want       int
	}{
		{"inside", "10.10.20.50:1", http.StatusOK},
		{"outside", "10.10.30.5:1", http.StatusForbidden},
		{"IPv4-mapped inside", "[::ffff:10.10.20.50]:1", http.StatusOK},
		{"IPv6", "[2001:db8::1]:1", http.StatusForbidden},
		{"unparsable", "garbage", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/boot", nil)
			req.RemoteAddr = tc.remoteAddr
			rec := httptest.NewRecorder()

			RequireSubnet(subnet, next).ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if tc.want == http.StatusForbidden && rec.Body.Len() != 0 {
				t.Errorf("403 body = %q, want empty", rec.Body.String())
			}
		})
	}
}
