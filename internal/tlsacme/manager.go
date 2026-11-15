package tlsacme

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"go.jacaudi.dev/hephaestus/internal/admin"
	"go.jacaudi.dev/hephaestus/internal/datakey"
)

const (
	// meterScope is the instrumentation scope of the service's own metrics.
	meterScope = "go.jacaudi.dev/hephaestus"

	minBackoff = 10 * time.Minute
	maxBackoff = 6 * time.Hour
	// maxBackoffShift is the shift at which minBackoff exceeds maxBackoff.
	maxBackoffShift = 6

	expiryWarn = 14.0 // days left below which tls:expiry warns

	checkCertificate = "tls:certificate"
	checkExpiry      = "tls:expiry"
	noCertificate    = "no certificate loaded"
)

// ErrNoCertificate is returned by GetCertificate until a certificate is loaded.
var ErrNoCertificate = errors.New("tlsacme: no certificate loaded yet")

// Certificate is an issued certificate: the PEM chain (leaf first) and the
// PEM private key.
type Certificate struct {
	ChainPEM, KeyPEM []byte
}

// Issuer obtains a certificate for domain. It is the Manager's boundary to
// the certificate authority: LegoIssuer is the real implementation,
// tlsacmetest.SelfSigned the test double.
type Issuer interface {
	Obtain(ctx context.Context, domain string) (Certificate, error)
}

// RenewAt is when a certificate valid from notBefore to notAfter is renewed:
// two thirds into its lifetime.
func RenewAt(notBefore, notAfter time.Time) time.Time {
	return notBefore.Add(notAfter.Sub(notBefore) * 2 / 3)
}

// Option configures a Manager.
type Option func(*Manager)

// WithClock sets the clock; the default is time.Now.
func WithClock(now func() time.Time) Option { return func(m *Manager) { m.now = now } }

// WithErrorHandler sets the function Reconcile hands every failure to; the
// default ignores them (Status still records the last one).
func WithErrorHandler(h func(context.Context, error)) Option {
	return func(m *Manager) { m.onError = h }
}

// WithMeter sets the meter the Manager's instruments are registered on; the
// default is a no-op meter.
func WithMeter(meter metric.Meter) Option { return func(m *Manager) { m.meter = meter } }

// Status is a snapshot of the Manager's state.
type Status struct {
	// Loaded reports whether a certificate is being served.
	Loaded bool
	// NotAfter is when the served certificate expires; zero when none is loaded.
	NotAfter time.Time
	// Failures is the number of consecutive failed renewals.
	Failures int
	// LastError is the last failure; empty after a successful renewal.
	LastError string
}

// Manager keeps one certificate for one domain current. GetCertificate,
// Status and Checks are safe for concurrent use with Run.
type Manager struct {
	repo   *Repository
	key    *datakey.Key
	iss    Issuer
	domain string

	now     func() time.Time
	onError func(context.Context, error)
	meter   metric.Meter

	renewalFailures metric.Int64Counter

	mu       sync.RWMutex
	cert     *tls.Certificate // nil until loaded; Leaf is always set
	failures int
	lastErr  string
}

// NewManager returns a Manager for domain that issues through iss and stores
// the certificate in repo, sealed under key. It starts nothing: call Load,
// then Run.
func NewManager(repo *Repository, key *datakey.Key, iss Issuer, domain string, opts ...Option) (*Manager, error) {
	m := &Manager{
		repo:    repo,
		key:     key,
		iss:     iss,
		domain:  domain,
		now:     time.Now,
		onError: func(context.Context, error) {},
		meter:   noop.NewMeterProvider().Meter(meterScope),
	}
	for _, opt := range opts {
		opt(m)
	}

	var err error
	m.renewalFailures, err = m.meter.Int64Counter("hephaestus.tls.renewal.failures",
		metric.WithDescription("Failed certificate renewals."))
	if err != nil {
		return nil, fmt.Errorf("tlsacme: renewal failures counter: %w", err)
	}
	_, err = m.meter.Float64ObservableGauge("hephaestus.tls.certificate.days_left",
		metric.WithDescription("Days until the served certificate expires; absent while none is loaded."),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			if c := m.current(); c != nil {
				o.Observe(daysLeft(c.Leaf.NotAfter, m.now()))
			}
			return nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("tlsacme: days left gauge: %w", err)
	}
	return m, nil
}

// Load serves the certificate stored for the domain, if there is one. A
// stored certificate that does not open under the data key is an error, never
// a reason to issue a new one: that would hide a wrong key and burn rate
// limits. A certificate that does not cover the domain (the FQDN changed) is
// ignored, so Run issues a new one.
func (m *Manager) Load(ctx context.Context) error {
	row, ok, err := m.repo.LoadCertificate(ctx, m.domain)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	chain, err := m.key.Open(row.ChainSealed, chainAAD(m.domain))
	if err != nil {
		return fmt.Errorf("stored TLS certificate for %s cannot be decrypted with the configured data key: %w", m.domain, err)
	}
	keyPEM, err := m.key.Open(row.KeySealed, keyAAD(m.domain))
	if err != nil {
		return fmt.Errorf("stored TLS certificate for %s cannot be decrypted with the configured data key: %w", m.domain, err)
	}
	pair, err := parsePair(chain, keyPEM)
	if err != nil {
		return fmt.Errorf("parse stored TLS certificate for %s: %w", m.domain, err)
	}
	if !slices.Contains(pair.Leaf.DNSNames, m.domain) {
		return nil
	}
	m.mu.Lock()
	m.cert = pair
	m.mu.Unlock()
	return nil
}

// Reconcile brings the certificate up to date and returns how long to wait
// before calling it again. A certificate that is loaded and not yet due costs
// nothing. Otherwise it obtains one. A failure at any step (the issuer, the
// parsing, the name check, the store, a certificate already due for renewal)
// is counted, handed to the error handler and returned with the backoff wait:
// 10 minutes, doubling per consecutive failure, capped at 6 hours.
//
// lego's Obtain takes no context, so Reconcile runs it in its own goroutine
// and abandons it when ctx is done, returning ctx.Err(); the abandoned call
// finishes on its own and its result is discarded. The next start retries.
func (m *Manager) Reconcile(ctx context.Context) (time.Duration, error) {
	if c := m.current(); c != nil {
		if wait := RenewAt(c.Leaf.NotBefore, c.Leaf.NotAfter).Sub(m.now()); wait > 0 {
			return wait, nil
		}
	}
	wait, err := m.renew(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return m.fail(ctx, err)
	}
	return wait, nil
}

// renew obtains, checks, stores and loads a new certificate and returns the
// wait until it is due. A certificate that is already due stays loaded (it is
// valid) but is reported as an error.
func (m *Manager) renew(ctx context.Context) (time.Duration, error) {
	issued, err := m.obtain(ctx)
	if err != nil {
		return 0, err
	}
	pair, err := parsePair(issued.ChainPEM, issued.KeyPEM)
	if err != nil {
		return 0, fmt.Errorf("parse issued certificate: %w", err)
	}
	if !slices.Contains(pair.Leaf.DNSNames, m.domain) {
		return 0, fmt.Errorf("issued certificate does not cover %s", m.domain)
	}
	now := m.now()
	err = m.repo.SaveCertificate(ctx, StoredCertificate{
		Domain:      m.domain,
		ChainSealed: m.key.Seal(issued.ChainPEM, chainAAD(m.domain)),
		KeySealed:   m.key.Seal(issued.KeyPEM, keyAAD(m.domain)),
		NotBefore:   pair.Leaf.NotBefore,
		NotAfter:    pair.Leaf.NotAfter,
		UpdatedAt:   now,
	})
	if err != nil {
		return 0, fmt.Errorf("store issued certificate: %w", err)
	}

	m.mu.Lock()
	m.cert = pair
	m.mu.Unlock()

	wait := RenewAt(pair.Leaf.NotBefore, pair.Leaf.NotAfter).Sub(now)
	if wait <= 0 {
		return 0, fmt.Errorf("issued certificate for %s is already due for renewal", m.domain)
	}
	m.mu.Lock()
	m.failures, m.lastErr = 0, ""
	m.mu.Unlock()
	return wait, nil
}

// obtain calls the issuer in its own goroutine so that ctx can abandon it.
// The channel is buffered, so the goroutine exits whenever the issuer returns.
func (m *Manager) obtain(ctx context.Context) (Certificate, error) {
	type result struct {
		cert Certificate
		err  error
	}
	done := make(chan result, 1)
	go func() {
		cert, err := m.iss.Obtain(ctx, m.domain)
		done <- result{cert, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			return Certificate{}, fmt.Errorf("obtain certificate for %s: %w", m.domain, r.err)
		}
		return r.cert, nil
	case <-ctx.Done():
		return Certificate{}, ctx.Err()
	}
}

// fail records err and returns the backoff wait with it.
func (m *Manager) fail(ctx context.Context, err error) (time.Duration, error) {
	m.mu.Lock()
	m.failures++
	m.lastErr = err.Error()
	wait := min(minBackoff<<min(m.failures-1, maxBackoffShift), maxBackoff)
	m.mu.Unlock()

	m.renewalFailures.Add(ctx, 1)
	m.onError(ctx, err)
	return wait, err
}

// Run calls Reconcile, waits as it says, and repeats until ctx is done, then
// returns nil. It does not wait for an abandoned Obtain.
func (m *Manager) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		wait, err := m.Reconcile(ctx)
		if err != nil && ctx.Err() != nil {
			return nil
		}
		// Any other error was recorded and handed to the error handler by
		// Reconcile; wait is its backoff.
		timer.Reset(wait)
	}
}

// GetCertificate serves the current certificate; it is the tls.Config
// callback. It returns ErrNoCertificate until one is loaded.
func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if c := m.current(); c != nil {
		return c, nil
	}
	return nil, ErrNoCertificate
}

// Status returns a snapshot of the Manager's state.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := Status{Loaded: m.cert != nil, Failures: m.failures, LastError: m.lastErr}
	if m.cert != nil {
		s.NotAfter = m.cert.Leaf.NotAfter
	}
	return s
}

// Checks returns the certificate health checks. Both read in-memory state, so
// they run every second and readiness follows a fresh issue within a second.
func (m *Manager) Checks() []admin.Check {
	return []admin.Check{
		{Name: checkCertificate, Critical: true, Interval: time.Second, Run: m.certificateCheck},
		{Name: checkExpiry, Interval: time.Second, Run: m.expiryCheck},
	}
}

func (m *Manager) certificateCheck(context.Context) admin.Result {
	c := m.current()
	if c == nil {
		return admin.Result{Status: admin.StatusFail, Output: noCertificate}
	}
	return admin.Result{Status: admin.StatusPass, Output: "valid until " + c.Leaf.NotAfter.UTC().Format(time.RFC3339)}
}

func (m *Manager) expiryCheck(context.Context) admin.Result {
	c := m.current()
	if c == nil {
		return admin.Result{Status: admin.StatusFail, Output: noCertificate}
	}
	days := daysLeft(c.Leaf.NotAfter, m.now())
	res := admin.Result{Status: admin.StatusPass, ObservedValue: days, ObservedUnit: "d"}
	switch {
	case days <= 0:
		res.Status, res.Output = admin.StatusFail, "certificate expired"
	case days < expiryWarn:
		res.Status, res.Output = admin.StatusWarn, fmt.Sprintf("certificate expires in %d days", int(days))
	}
	return res
}

// current returns the served certificate, or nil.
func (m *Manager) current() *tls.Certificate {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cert
}

func daysLeft(notAfter, now time.Time) float64 { return notAfter.Sub(now).Hours() / 24 }

// parsePair builds a tls.Certificate from PEM and parses its leaf.
func parsePair(chainPEM, keyPEM []byte) (*tls.Certificate, error) {
	pair, err := tls.X509KeyPair(chainPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse leaf: %w", err)
	}
	pair.Leaf = leaf
	return &pair, nil
}

// chainAAD and keyAAD bind a sealed value to its table, column and row, so a
// value copied to another row does not open.
func chainAAD(domain string) []byte { return []byte("tls_certificates/chain/" + domain) }
func keyAAD(domain string) []byte   { return []byte("tls_certificates/key/" + domain) }
