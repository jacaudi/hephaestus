package boot

import (
	"fmt"
	"net/http"
	"net/netip"
)

// RequireSubnet serves next only to clients whose address is inside subnet, and
// answers everyone else 403 with an empty body. The client is the connection's
// peer, or the address TrustProxies took from X-Forwarded-For. An IPv4-mapped
// IPv6 address is unmapped first, so a dual-stack listener still matches an
// IPv4 subnet; an address that does not parse is refused.
func RequireSubnet(subnet netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !onSubnet(subnet, r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// onSubnet reports whether r's client address (clientAddr) parses and is inside subnet.
func onSubnet(subnet netip.Prefix, r *http.Request) bool {
	addr, err := clientAddr(r)
	return err == nil && subnet.Contains(addr)
}

// clientAddr is the client address of r, with an IPv4-mapped IPv6 address
// unmapped: the one TrustProxies set when the request came through a trusted
// proxy, otherwise the connection's. The guard and the /discover rate limiter
// both see a client as this one address.
func clientAddr(r *http.Request) (netip.Addr, error) {
	if addr, ok := r.Context().Value(clientKey{}).(netip.Addr); ok {
		return addr, nil
	}
	return remoteAddr(r)
}

// remoteAddr is the address of the connection's peer, unmapped.
func remoteAddr(r *http.Request) (netip.Addr, error) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("boot: source address: %w", err)
	}
	return ap.Addr().Unmap(), nil
}
