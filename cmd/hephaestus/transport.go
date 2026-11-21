package main

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"go.jacaudi.dev/hephaestus/internal/secrets"
)

const (
	// defaultDialTimeout, defaultTLSHandshakeTimeout and defaultResponseHeaderTimeout bound every connection and
	// request of the default transport. defaultKeepAlive is the standard library's own.
	defaultDialTimeout           = 10 * time.Second
	defaultKeepAlive             = 30 * time.Second
	defaultTLSHandshakeTimeout   = 10 * time.Second
	defaultResponseHeaderTimeout = 60 * time.Second
)

// setupDefaultTransport gives the process's default transport the TLS 1.3 floor and its timeouts, and sends
// http.DefaultClient through counter. It changes the *http.Transport behind http.DefaultTransport in place rather than
// replacing it: go-git's githttp.DefaultClient took that pointer when its package was initialised, and
// toolcache.NewHTTPClient clones it. The 1Password SDK sends through http.DefaultClient, so counter sees its
// requests. Calling it again sets the same values. It must run before anything sends a request.
func setupDefaultTransport(counter *secrets.Counter) {
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		panic("http.DefaultTransport is not *http.Transport")
	}
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	tr.DialContext = (&net.Dialer{Timeout: defaultDialTimeout, KeepAlive: defaultKeepAlive}).DialContext
	tr.TLSHandshakeTimeout = defaultTLSHandshakeTimeout
	tr.ResponseHeaderTimeout = defaultResponseHeaderTimeout
	http.DefaultClient.Transport = counter //nolint:forbidigo // the 1Password SDK sends through http.DefaultClient
}
