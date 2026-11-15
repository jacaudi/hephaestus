package tlscert

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"go.jacaudi.dev/hephaestus/internal/admin"
)

const (
	defaultInterval = time.Minute

	// checkInterval is how often the checks run; both read in-memory state.
	checkInterval = time.Second

	expiryWarn = 14.0 // days left below which tls:expiry warns

	checkCertificate = "tls:certificate"
	checkExpiry      = "tls:expiry"
)

// Option configures a Loader.
type Option func(*Loader)

// WithClock sets the clock that judges expiry; the default is time.Now.
func WithClock(now func() time.Time) Option { return func(l *Loader) { l.now = now } }

// WithMeter sets the meter the Loader's instruments are registered on; the
// default is a no-op meter.
func WithMeter(m metric.Meter) Option { return func(l *Loader) { l.meter = m } }

// WithErrorHandler sets the function Run hands every failed reload to; the
// default discards them. The Loader never logs.
func WithErrorHandler(fn func(error)) Option { return func(l *Loader) { l.onError = fn } }

// WithInterval sets how often Run looks at the files; it must be positive. The
// default is one minute.
func WithInterval(d time.Duration) Option { return func(l *Loader) { l.interval = d } }

// stamp is what Run compares to notice that a file changed: its modification
// time and size.
type stamp struct{ modNano, size int64 }

// Loader serves one certificate and key read from files. GetCertificate and
// Checks are safe for concurrent use with Run.
type Loader struct {
	certFile, keyFile, fqdn string

	now      func() time.Time
	meter    metric.Meter
	onError  func(error)
	interval time.Duration

	reloadFailures metric.Int64Counter

	mu   sync.RWMutex
	cert *tls.Certificate // never nil after NewLoader; Leaf is always set

	// seen holds the stamps of the certificate and key file at the last load
	// attempt. Only NewLoader, before it returns, and then Run touch it.
	seen [2]stamp
}

// NewLoader reads certFile and keyFile and returns a Loader that serves the
// pair, or an error and nothing to serve when a file cannot be read, the key
// does not match the certificate, the leaf does not cover fqdn, the leaf is not
// yet valid or has expired, or the interval is not positive. It starts
// nothing: call Run to follow changes to the files.
func NewLoader(certFile, keyFile, fqdn string, opts ...Option) (*Loader, error) {
	l := &Loader{
		certFile: certFile,
		keyFile:  keyFile,
		fqdn:     fqdn,
		now:      time.Now,
		meter:    noop.NewMeterProvider().Meter(""),
		onError:  func(error) {},
		interval: defaultInterval,
	}
	for _, opt := range opts {
		opt(l)
	}
	if l.interval <= 0 {
		return nil, fmt.Errorf("tlscert: interval %s must be positive", l.interval)
	}

	// Stat before reading: a file that changes in between is then seen as
	// changed by the next look, never missed.
	seen, err := l.stamps()
	if err != nil {
		return nil, err
	}
	cert, err := l.read()
	if err != nil {
		return nil, err
	}
	l.seen, l.cert = seen, cert

	// The instruments come last: the gauge callback needs a certificate to read.
	l.reloadFailures, err = l.meter.Int64Counter("hephaestus.tls.reload.failures",
		metric.WithDescription("Failed reloads of the TLS certificate files."))
	if err != nil {
		return nil, fmt.Errorf("tlscert: reload failures counter: %w", err)
	}
	_, err = l.meter.Float64ObservableGauge("hephaestus.tls.certificate.days_left",
		metric.WithDescription("Days until the served certificate expires."),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			o.Observe(daysLeft(l.current().Leaf.NotAfter, l.now()))
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("tlscert: days left gauge: %w", err)
	}
	return l, nil
}

// Run looks at both files every interval. When either one's modification time
// or size differs from the last load attempt it reloads the pair with the
// checks NewLoader applies. A pair that passes replaces the one served; one that
// fails leaves the served pair in place, is counted, and is handed to the error
// handler once: the new stamps are recorded either way, so the same bad files
// are not retried until they change again. Run returns nil when ctx is done.
func (l *Loader) Run(ctx context.Context) error {
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			l.reloadIfChanged(ctx)
		}
	}
}

func (l *Loader) reloadIfChanged(ctx context.Context) {
	seen, err := l.stamps()
	if err != nil {
		// No stamp to remember: a file that stays missing is reported every interval.
		l.fail(ctx, err)
		return
	}
	if seen == l.seen {
		return
	}
	cert, err := l.read()
	l.seen = seen
	if err != nil {
		l.fail(ctx, err)
		return
	}
	l.mu.Lock()
	l.cert = cert
	l.mu.Unlock()
}

func (l *Loader) fail(ctx context.Context, err error) {
	l.reloadFailures.Add(ctx, 1)
	l.onError(err)
}

// stamps stats the certificate file and the key file, following symlinks, so a
// Kubernetes secret update that repoints them counts as a change.
func (l *Loader) stamps() ([2]stamp, error) {
	var out [2]stamp
	for i, path := range [2]string{l.certFile, l.keyFile} {
		fi, err := os.Stat(path)
		if err != nil {
			return [2]stamp{}, fmt.Errorf("tlscert: %w", err)
		}
		out[i] = stamp{modNano: fi.ModTime().UnixNano(), size: fi.Size()}
	}
	return out, nil
}

// read loads the pair from the files and applies the checks every pair must
// pass before it is served: it must be a matching pair, its leaf must cover the
// name, and the leaf must not have expired.
func (l *Loader) read() (*tls.Certificate, error) {
	certPEM, err := os.ReadFile(l.certFile)
	if err != nil {
		return nil, fmt.Errorf("tlscert: read certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(l.keyFile)
	if err != nil {
		return nil, fmt.Errorf("tlscert: read key: %w", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("tlscert: certificate %s and key %s: %w", l.certFile, l.keyFile, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("tlscert: parse leaf of %s: %w", l.certFile, err)
	}
	if err := leaf.VerifyHostname(l.fqdn); err != nil {
		return nil, fmt.Errorf("tlscert: certificate %s does not cover %s: %w", l.certFile, l.fqdn, err)
	}
	now := l.now()
	if now.Before(leaf.NotBefore) {
		return nil, fmt.Errorf("tlscert: certificate %s is not yet valid: valid from %s", l.certFile, leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	if now.After(leaf.NotAfter) {
		return nil, fmt.Errorf("tlscert: certificate %s expired at %s", l.certFile, leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	pair.Leaf = leaf
	return &pair, nil
}

// GetCertificate returns the pair being served; it is the tls.Config callback.
// The leaf is parsed (Leaf is set).
func (l *Loader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return l.current(), nil
}

func (l *Loader) current() *tls.Certificate {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.cert
}

// Checks returns the certificate health checks. tls:certificate is critical: it
// passes from the leaf's NotBefore until it expires. tls:expiry is
// informational: it warns when fewer than 14 days are left, and reports the days left either way. The
// served pair is never dropped when it expires; the check takes the service out
// of rotation.
func (l *Loader) Checks() []admin.Check {
	return []admin.Check{
		{Name: checkCertificate, Critical: true, Interval: checkInterval, Run: l.certificateCheck},
		{Name: checkExpiry, Interval: checkInterval, Run: l.expiryCheck},
	}
}

func (l *Loader) certificateCheck(context.Context) admin.Result {
	leaf := l.current().Leaf
	notAfter, now := leaf.NotAfter, l.now()
	if now.Before(leaf.NotBefore) {
		return admin.Result{Status: admin.StatusFail, Output: "certificate not yet valid until " + leaf.NotBefore.UTC().Format(time.RFC3339)}
	}
	if now.After(notAfter) {
		return admin.Result{Status: admin.StatusFail, Output: "certificate expired at " + notAfter.UTC().Format(time.RFC3339)}
	}
	return admin.Result{Status: admin.StatusPass, Output: "valid until " + notAfter.UTC().Format(time.RFC3339)}
}

func (l *Loader) expiryCheck(context.Context) admin.Result {
	days := daysLeft(l.current().Leaf.NotAfter, l.now())
	res := admin.Result{Status: admin.StatusPass, ObservedValue: days, ObservedUnit: "d"}
	switch {
	case days <= 0:
		res.Status, res.Output = admin.StatusWarn, "certificate expired"
	case days < expiryWarn:
		res.Status, res.Output = admin.StatusWarn, fmt.Sprintf("certificate expires in %d days", int(days))
	}
	return res
}

func daysLeft(notAfter, now time.Time) float64 { return notAfter.Sub(now).Hours() / 24 }
