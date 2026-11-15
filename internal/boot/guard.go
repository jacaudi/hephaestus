package boot

import (
	"fmt"
	"net/http"
	"net/netip"
)

// RequireSubnet serves next only to clients whose source address is inside
// subnet, and answers everyone else 403 with an empty body. An IPv4-mapped
// IPv6 address is unmapped first, so a dual-stack listener still matches an
// IPv4 subnet; an address that does not parse is refused.
func RequireSubnet(subnet netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr, err := sourceAddr(r.RemoteAddr)
		if err != nil || !subnet.Contains(addr) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sourceAddr is the client address of a connection's RemoteAddr, with an
// IPv4-mapped IPv6 address unmapped. The guard and the /discover rate limiter
// both see a client as this one address.
func sourceAddr(remoteAddr string) (netip.Addr, error) {
	ap, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("boot: source address: %w", err)
	}
	return ap.Addr().Unmap(), nil
}
