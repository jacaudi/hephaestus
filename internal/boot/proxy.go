package boot

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"go.jacaudi.dev/hephaestus/internal/telemetry"
)

// The reasons TrustProxies refuses a request. They are fixed text, never the
// request's own, so they are safe on the wide event.
var (
	errUntrustedPeer   = errors.New("boot: peer is not a trusted proxy")
	errNoForwardedFor  = errors.New("boot: no X-Forwarded-For from a trusted proxy")
	errBadForwardedFor = errors.New("boot: unparsable X-Forwarded-For entry")
	errNoClient        = errors.New("boot: every X-Forwarded-For entry is a trusted proxy")
)

// forwardedFor is the one header the client address is read from. Forwarded and
// X-Real-IP are not: a client can send either, and only this one is documented
// to be appended to by every proxy on the path.
const forwardedFor = "X-Forwarded-For"

// clientKey is the context key under which TrustProxies stores the client address.
type clientKey struct{}

// TrustProxies serves next only for requests whose peer (RemoteAddr) lies inside
// one of the trusted prefixes, and makes the client address the one that
// X-Forwarded-For names instead of the peer's: RequireSubnet and the /discover
// limiter read it through clientAddr.
//
// All header lines are joined with commas and read from the right, the end the
// trusted proxies append to. Entries inside a trusted prefix are other proxies
// and are skipped; the first one that is not is the client. Everything else fails
// closed with a 403 problem document: a peer that is not trusted, a missing
// header, an entry that does not parse (only the entries walked are parsed, so
// whatever sits left of the client is never read), and a header of nothing but
// trusted entries. The peer's own address is never the client of a request that
// arrives here: a proxy that sends no header is misconfigured, not a client.
func TrustProxies(trusted []netip.Prefix, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, err := proxiedClient(r, trusted)
		if err != nil {
			telemetry.AddAttrs(r.Context(), slog.String("error", err.Error()))
			writeProblem(r.Context(), w, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientKey{}, client)))
	})
}

// proxiedClient is the client address of a request from a trusted proxy.
func proxiedClient(r *http.Request, trusted []netip.Prefix) (netip.Addr, error) {
	peer, err := remoteAddr(r)
	if err != nil {
		return netip.Addr{}, err
	}
	if !within(trusted, peer) {
		return netip.Addr{}, errUntrustedPeer
	}
	lines := r.Header.Values(forwardedFor)
	if len(lines) == 0 {
		return netip.Addr{}, errNoForwardedFor
	}
	entries := strings.Split(strings.Join(lines, ","), ",")
	for _, entry := range slices.Backward(entries) {
		addr, err := netip.ParseAddr(strings.TrimSpace(entry))
		if err != nil {
			return netip.Addr{}, errBadForwardedFor
		}
		// A zone names an interface of the proxy's host, not part of the address.
		addr = addr.WithZone("").Unmap()
		if !within(trusted, addr) {
			return addr, nil
		}
	}
	return netip.Addr{}, errNoClient
}

// within reports whether addr is inside any of prefixes.
func within(prefixes []netip.Prefix, addr netip.Addr) bool {
	return slices.ContainsFunc(prefixes, func(p netip.Prefix) bool { return p.Contains(addr) })
}
