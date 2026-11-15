package boot

import (
	"net/http"
	"net/netip"
)

// RequireSubnet serves next only to clients whose source address is inside
// subnet, and answers everyone else 403 with an empty body. An IPv4-mapped
// IPv6 address is unmapped first, so a dual-stack listener still matches an
// IPv4 subnet; an address that does not parse is refused.
func RequireSubnet(subnet netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr, err := netip.ParseAddrPort(r.RemoteAddr)
		if err != nil || !subnet.Contains(addr.Addr().Unmap()) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
