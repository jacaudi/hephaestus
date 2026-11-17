package toolcache

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// MaxRedirects is how many redirects the upstream client follows: the sixth
// is refused.
const MaxRedirects = 5

// responseHeaderTimeout bounds the wait for an upstream's response headers.
// The body has no total deadline; the fetcher applies an idle one.
const responseHeaderTimeout = 60 * time.Second

// userAgentTransport sets the User-Agent on every request it sends, redirect
// hops included.
type userAgentTransport struct {
	base      *http.Transport
	userAgent string
}

// RoundTrip implements http.RoundTripper. It sends a clone, because a
// RoundTripper must not modify the request it is given.
func (t *userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", t.userAgent)
	return t.base.RoundTrip(req)
}

// CloseIdleConnections lets http.Client.CloseIdleConnections reach the real
// transport, whose pooled connections the wrapper would otherwise hide.
func (t *userAgentTransport) CloseIdleConnections() { t.base.CloseIdleConnections() }

// NewHTTPClient returns the client for every upstream fetch: TLS 1.3 at
// least, trusting roots (the system roots when nil), no total timeout (the
// objects are large; the fetcher applies an idle deadline), no transparent
// decompression (the pinned hash is over the bytes as served), and the proxy
// from the environment. It sends userAgent, follows at most MaxRedirects
// redirects, and follows only https ones. Go adds a Referer on a redirect;
// the client removes it, because SourceForge answers a request carrying one
// with an HTML interstitial and status 200. It panics if http.DefaultTransport
// has been replaced by something other than an *http.Transport.
func NewHTTPClient(userAgent string, roots *x509.CertPool) *http.Client {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		panic("toolcache: http.DefaultTransport is not *http.Transport")
	}
	tr := base.Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}
	tr.ResponseHeaderTimeout = responseHeaderTimeout
	tr.DisableCompression = true
	tr.Proxy = http.ProxyFromEnvironment
	return &http.Client{
		Transport:     &userAgentTransport{base: tr, userAgent: userAgent},
		CheckRedirect: checkRedirect,
	}
}

// checkRedirect is the client's redirect policy. A redirect off https is an
// IntegrityError; its message names the scheme and host only, never the URL,
// which may carry a query string.
func checkRedirect(req *http.Request, via []*http.Request) error {
	req.Header.Del("Referer")
	if req.URL.Scheme != "https" {
		return &IntegrityError{Err: fmt.Errorf("redirect to %q scheme on host %q: https only", req.URL.Scheme, req.URL.Host)}
	}
	if len(via) > MaxRedirects {
		return errors.New("toolcache: stopped after too many redirects")
	}
	return nil
}
