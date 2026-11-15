package tlsacme_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"go.jacaudi.dev/hephaestus/internal/admin"
	"go.jacaudi.dev/hephaestus/internal/datakey"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
	"go.jacaudi.dev/hephaestus/internal/secret"
	"go.jacaudi.dev/hephaestus/internal/telemetry"
	"go.jacaudi.dev/hephaestus/internal/tlsacme"
	"go.jacaudi.dev/hephaestus/internal/tlsacme/tlsacmetest"
)

const (
	domain      = "hephaestus.example.test"
	fixtureKey  = secret.Secret("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	otherKey    = secret.Secret("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	ninetyDays  = 90 * 24 * time.Hour
	failurePath = "obtain certificate for " + domain

	stagingDirectory    = "https://acme-staging.example.test/directory"
	productionDirectory = "https://acme.example.test/directory"
)

func start() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }

func mustKey(t *testing.T, s secret.Secret) *datakey.Key {
	t.Helper()
	k, err := datakey.Parse(s)
	if err != nil {
		t.Fatalf("datakey.Parse: %v", err)
	}
	return k
}

// fakeClock is a settable clock safe for the goroutines Run starts.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// errLog collects what the error handler receives.
type errLog struct {
	mu   sync.Mutex
	errs []error
}

func (l *errLog) handle(_ context.Context, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, err)
}

func (l *errLog) all() []error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]error(nil), l.errs...)
}

// harness is one Manager over a fresh SQLite database.
type harness struct {
	db    *db.DB
	repo  *tlsacme.Repository
	key   *datakey.Key
	iss   *tlsacmetest.SelfSigned
	clock *fakeClock
	errs  *errLog
	mgr   *tlsacme.Manager
}

func newHarness(t *testing.T, iss *tlsacmetest.SelfSigned, opts ...tlsacme.Option) *harness {
	t.Helper()
	d := dbtest.SQLite(t)
	h := &harness{
		db:    d,
		repo:  tlsacme.NewRepository(d),
		key:   mustKey(t, fixtureKey),
		clock: &fakeClock{now: start()},
		errs:  &errLog{},
	}
	h.mgr = h.manager(t, h.key, iss, opts...)
	h.iss = iss
	return h
}

// manager builds another Manager over the harness's repository.
func (h *harness) manager(t *testing.T, key *datakey.Key, iss *tlsacmetest.SelfSigned, opts ...tlsacme.Option) *tlsacme.Manager {
	t.Helper()
	opts = append([]tlsacme.Option{tlsacme.WithClock(h.clock.Now), tlsacme.WithErrorHandler(h.errs.handle)}, opts...)
	m, err := tlsacme.NewManager(h.repo, key, iss, domain, opts...)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func ninety() *tlsacmetest.SelfSigned {
	return &tlsacmetest.SelfSigned{NotBefore: start(), NotAfter: start().Add(ninetyDays)}
}

func TestRenewAt(t *testing.T) {
	if got, want := tlsacme.RenewAt(start(), start().Add(ninetyDays)), start().Add(60*24*time.Hour); !got.Equal(want) {
		t.Errorf("RenewAt = %s, want %s", got, want)
	}
}

func TestReconcile_IssuesWhenEmpty(t *testing.T) {
	h := newHarness(t, ninety())

	wait, err := h.mgr.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if want := 60 * 24 * time.Hour; wait != want {
		t.Errorf("wait = %s, want %s", wait, want)
	}
	if got := h.iss.Calls.Load(); got != 1 {
		t.Errorf("issuer calls = %d, want 1", got)
	}
	if !h.mgr.Status().Loaded {
		t.Error("Status().Loaded = false, want true")
	}
	cert, err := h.mgr.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if got := cert.Leaf.DNSNames; len(got) != 1 || got[0] != domain {
		t.Errorf("Leaf.DNSNames = %v, want [%s]", got, domain)
	}

	stored, ok, err := h.repo.LoadCertificate(t.Context(), domain)
	if err != nil || !ok {
		t.Fatalf("LoadCertificate = ok %v, err %v; want true, nil", ok, err)
	}
	if strings.Contains(string(stored.ChainSealed), "BEGIN CERTIFICATE") {
		t.Error("stored chain is plaintext PEM, want sealed")
	}
}

func TestReconcile_NoReissueBeforeRenewAt(t *testing.T) {
	h := newHarness(t, ninety())
	if _, err := h.mgr.Reconcile(t.Context()); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	h.clock.Set(start().Add(59 * 24 * time.Hour))
	wait, err := h.mgr.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if got := h.iss.Calls.Load(); got != 1 {
		t.Errorf("issuer calls = %d, want 1", got)
	}
	if want := 24 * time.Hour; wait != want {
		t.Errorf("wait = %s, want %s", wait, want)
	}
}

func TestReconcile_RenewsAtTwoThirds(t *testing.T) {
	h := newHarness(t, ninety())
	if _, err := h.mgr.Reconcile(t.Context()); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}

	// The renewal is a fresh certificate, valid from the renewal time.
	renewal := start().Add(60 * 24 * time.Hour)
	h.clock.Set(renewal)
	h.iss.NotBefore, h.iss.NotAfter = renewal, renewal.Add(ninetyDays)
	wait, err := h.mgr.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if got := h.iss.Calls.Load(); got != 2 {
		t.Errorf("issuer calls = %d, want 2", got)
	}
	if want := 60 * 24 * time.Hour; wait != want {
		t.Errorf("wait = %s, want %s", wait, want)
	}
}

func TestLoad_ReusesStoredCertificate(t *testing.T) {
	h := newHarness(t, ninety())
	if _, err := h.mgr.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	second := ninety()
	m2 := h.manager(t, h.key, second)
	if err := m2.Load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !m2.Status().Loaded {
		t.Fatal("Status().Loaded = false after Load, want true")
	}
	h.clock.Set(start().Add(24 * time.Hour))
	if _, err := m2.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := second.Calls.Load(); got != 0 {
		t.Errorf("second issuer calls = %d, want 0", got)
	}
}

func TestLoad_WrongDataKeyFailsLoudly(t *testing.T) {
	h := newHarness(t, ninety())
	if _, err := h.mgr.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	m2 := h.manager(t, mustKey(t, otherKey), ninety())
	err := m2.Load(t.Context())
	if !errors.Is(err, datakey.ErrDecrypt) {
		t.Fatalf("Load error = %v, want one wrapping datakey.ErrDecrypt", err)
	}
	if want := "cannot be decrypted with the configured data key"; !strings.Contains(err.Error(), want) {
		t.Errorf("Load error = %q, want it to contain %q", err, want)
	}
}

func TestLoad_OtherDomainIgnored(t *testing.T) {
	h := newHarness(t, ninety())
	other, err := tlsacme.NewManager(h.repo, h.key, ninety(), "other.example.test", tlsacme.WithClock(h.clock.Now))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := other.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile for other domain: %v", err)
	}

	if err := h.mgr.Load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if h.mgr.Status().Loaded {
		t.Error("Status().Loaded = true for a certificate stored under another domain, want false")
	}
}

// TestLoad_CertForOtherNameIgnored stores a certificate for another name
// under this domain's row, sealed with the right key, so Load opens it and
// reaches the "certificate is for another name" check.
func TestLoad_CertForOtherNameIgnored(t *testing.T) {
	h := newHarness(t, ninety())
	cert, err := (&tlsacmetest.SelfSigned{Domain: "other.example.test", NotBefore: start(), NotAfter: start().Add(ninetyDays)}).
		Obtain(t.Context(), domain)
	if err != nil {
		t.Fatalf("Obtain: %v", err)
	}
	err = h.repo.SaveCertificate(t.Context(), tlsacme.StoredCertificate{
		Domain:      domain,
		ChainSealed: h.key.Seal(cert.ChainPEM, []byte("tls_certificates/chain/"+domain)),
		KeySealed:   h.key.Seal(cert.KeyPEM, []byte("tls_certificates/key/"+domain)),
		NotBefore:   start(), NotAfter: start().Add(ninetyDays), UpdatedAt: start(),
	})
	if err != nil {
		t.Fatalf("SaveCertificate: %v", err)
	}

	if err := h.mgr.Load(t.Context()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if h.mgr.Status().Loaded {
		t.Error("Status().Loaded = true for a certificate that does not cover the domain, want false")
	}
}

// TestLoad_OtherDirectoryIgnored pins that a certificate obtained from one ACME
// directory is not served once another directory is configured: Load skips it
// and Reconcile obtains and stores a new one from the configured directory.
func TestLoad_OtherDirectoryIgnored(t *testing.T) {
	h := newHarness(t, ninety(), tlsacme.WithDirectoryURL(stagingDirectory))
	if _, err := h.mgr.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile on the first directory: %v", err)
	}

	same := h.manager(t, h.key, ninety(), tlsacme.WithDirectoryURL(stagingDirectory))
	if err := same.Load(t.Context()); err != nil {
		t.Fatalf("Load on the same directory: %v", err)
	}
	if !same.Status().Loaded {
		t.Error("Status().Loaded = false on the directory the certificate came from, want true")
	}

	iss := ninety()
	other := h.manager(t, h.key, iss, tlsacme.WithDirectoryURL(productionDirectory))
	if err := other.Load(t.Context()); err != nil {
		t.Fatalf("Load on another directory: %v", err)
	}
	if other.Status().Loaded {
		t.Error("Status().Loaded = true for a certificate from another directory, want false")
	}
	if _, err := other.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile on another directory: %v", err)
	}
	if got := iss.Calls.Load(); got != 1 {
		t.Errorf("issuer calls = %d, want 1", got)
	}
	row, ok, err := h.repo.LoadCertificate(t.Context(), domain)
	if err != nil || !ok {
		t.Fatalf("LoadCertificate = ok %v, err %v; want true, nil", ok, err)
	}
	if row.DirectoryURL != productionDirectory {
		t.Errorf("stored DirectoryURL = %q, want %q", row.DirectoryURL, productionDirectory)
	}
}

// TestLoad_RestartKeepsReissueFloor pins that the 24h floor between issuer
// calls survives a restart: a stored certificate that is already due is not
// replaced until 24h after it was stored, however often the service restarts.
func TestLoad_RestartKeepsReissueFloor(t *testing.T) {
	// Already due the moment it is issued: RenewAt is 30 days in the past.
	due := func() *tlsacmetest.SelfSigned {
		return &tlsacmetest.SelfSigned{NotBefore: start().Add(-ninetyDays), NotAfter: start().Add(time.Hour)}
	}
	h := newHarness(t, due())
	if _, err := h.mgr.Reconcile(t.Context()); err == nil || !strings.Contains(err.Error(), "already due for renewal") {
		t.Fatalf("Reconcile error = %v, want one containing %q", err, "already due for renewal")
	}

	for _, elapsed := range []time.Duration{time.Minute, 2 * time.Minute, 23 * time.Hour} {
		h.clock.Set(start().Add(elapsed))
		iss := due()
		restarted := h.manager(t, h.key, iss)
		if err := restarted.Load(t.Context()); err != nil {
			t.Fatalf("Load at +%s: %v", elapsed, err)
		}
		wait, err := restarted.Reconcile(t.Context())
		if err != nil {
			t.Fatalf("Reconcile at +%s: %v", elapsed, err)
		}
		if want := 24*time.Hour - elapsed; wait != want {
			t.Errorf("wait at +%s = %s, want %s", elapsed, wait, want)
		}
		if got := iss.Calls.Load(); got != 0 {
			t.Fatalf("issuer calls at +%s = %d, want 0", elapsed, got)
		}
	}

	h.clock.Set(start().Add(24 * time.Hour))
	iss := due()
	restarted := h.manager(t, h.key, iss)
	if err := restarted.Load(t.Context()); err != nil {
		t.Fatalf("Load at +24h: %v", err)
	}
	if _, err := restarted.Reconcile(t.Context()); err == nil {
		t.Error("Reconcile at +24h error = nil, want the already-due error again")
	}
	if got := iss.Calls.Load(); got != 1 {
		t.Errorf("issuer calls at +24h = %d, want 1", got)
	}
}

func TestReconcile_FailureBacksOff(t *testing.T) {
	h := newHarness(t, &tlsacmetest.SelfSigned{Err: errors.New("dns-01 timed out")})

	var got []time.Duration
	for range 7 {
		wait, err := h.mgr.Reconcile(t.Context())
		if err == nil {
			t.Fatal("Reconcile error = nil, want the issuer's error")
		}
		got = append(got, wait)
	}
	want := []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 80 * time.Minute, 160 * time.Minute, 320 * time.Minute, 360 * time.Minute}
	if !equalDurations(got, want) {
		t.Errorf("waits = %v, want %v", got, want)
	}
	handled := h.errs.all()
	if len(handled) != 7 {
		t.Fatalf("handler got %d errors, want 7", len(handled))
	}
	for _, err := range handled {
		if !strings.Contains(err.Error(), failurePath) {
			t.Errorf("handler error = %q, want it to contain %q", err, failurePath)
		}
	}
	if s := h.mgr.Status(); s.Failures != 7 || s.LastError == "" {
		t.Errorf("Status() = %+v, want Failures 7 and a LastError", s)
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReconcile_FailureCountsMetric(t *testing.T) {
	m := newMetrics(t)
	h := newHarness(t, &tlsacmetest.SelfSigned{Err: errors.New("dns-01 timed out")}, tlsacme.WithMeter(m.Meter()))
	if _, err := h.mgr.Reconcile(t.Context()); err == nil {
		t.Fatal("Reconcile error = nil, want the issuer's error")
	}
	if body := scrape(t, m); !strings.Contains(body, "hephaestus_tls_renewal_failures_total{") {
		t.Errorf("metrics body missing hephaestus_tls_renewal_failures_total:\n%s", body)
	}
}

func TestReconcile_MalformedPEMBacksOff(t *testing.T) {
	h := newHarness(t, &tlsacmetest.SelfSigned{Malformed: true})

	wait, err := h.mgr.Reconcile(t.Context())
	if err == nil {
		t.Fatal("Reconcile error = nil, want a parse error")
	}
	if want := 10 * time.Minute; wait != want {
		t.Errorf("wait = %s, want %s", wait, want)
	}
	s := h.mgr.Status()
	if s.Failures != 1 || s.Loaded {
		t.Errorf("Status() = %+v, want Failures 1 and not loaded", s)
	}
	if _, ok, err := h.repo.LoadCertificate(t.Context(), domain); err != nil || ok {
		t.Errorf("LoadCertificate = ok %v, err %v; want no stored row", ok, err)
	}
}

func TestReconcile_ShortLivedCertBacksOff(t *testing.T) {
	h := newHarness(t, &tlsacmetest.SelfSigned{NotBefore: start().Add(-ninetyDays), NotAfter: start().Add(time.Hour)})

	wait, err := h.mgr.Reconcile(t.Context())
	if err == nil || !strings.Contains(err.Error(), "already due for renewal") {
		t.Fatalf("Reconcile error = %v, want one containing %q", err, "already due for renewal")
	}
	// The first-failure backoff is 10m, but the issuer is not called again within
	// 24h of a successful issuance, so the wait is floored at that.
	if want := 24 * time.Hour; wait != want {
		t.Errorf("wait = %s, want %s", wait, want)
	}
	if s := h.mgr.Status(); !s.Loaded || s.Failures != 1 {
		t.Errorf("Status() = %+v, want loaded with Failures 1", s)
	}
}

func TestReconcile_RejectsCertForWrongName(t *testing.T) {
	h := newHarness(t, &tlsacmetest.SelfSigned{Domain: "other.example.test", NotBefore: start(), NotAfter: start().Add(ninetyDays)})

	_, err := h.mgr.Reconcile(t.Context())
	if want := "does not cover " + domain; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Reconcile error = %v, want one containing %q", err, want)
	}
	if h.mgr.Status().Loaded {
		t.Error("Status().Loaded = true, want false")
	}
}

func TestGetCertificate_NoneYet(t *testing.T) {
	h := newHarness(t, ninety())
	if _, err := h.mgr.GetCertificate(nil); !errors.Is(err, tlsacme.ErrNoCertificate) {
		t.Errorf("GetCertificate error = %v, want ErrNoCertificate", err)
	}
}

func checkByName(t *testing.T, m *tlsacme.Manager, name string) admin.Check {
	t.Helper()
	for _, c := range m.Checks() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %q", name)
	return admin.Check{}
}

func TestChecks(t *testing.T) {
	h := newHarness(t, ninety())
	cert := checkByName(t, h.mgr, "tls:certificate")
	expiry := checkByName(t, h.mgr, "tls:expiry")
	if !cert.Critical || expiry.Critical {
		t.Errorf("Critical = %v/%v, want tls:certificate true and tls:expiry false", cert.Critical, expiry.Critical)
	}
	if cert.Interval != time.Second || expiry.Interval != time.Second {
		t.Errorf("Interval = %s/%s, want 1s for both", cert.Interval, expiry.Interval)
	}

	if got := cert.Run(t.Context()); got.Status != admin.StatusFail || got.Output != "no certificate loaded" {
		t.Errorf("tls:certificate before load = %+v, want fail %q", got, "no certificate loaded")
	}
	if got := expiry.Run(t.Context()); got.Status != admin.StatusFail || got.Output != "no certificate loaded" {
		t.Errorf("tls:expiry before load = %+v, want fail %q", got, "no certificate loaded")
	}

	if _, err := h.mgr.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	h.clock.Set(start().Add(60 * 24 * time.Hour))
	if got, want := cert.Run(t.Context()), (admin.Result{Status: admin.StatusPass, Output: "valid until " + start().Add(ninetyDays).Format(time.RFC3339)}); got != want {
		t.Errorf("tls:certificate loaded = %+v, want %+v", got, want)
	}
	if got := expiry.Run(t.Context()); got.Status != admin.StatusPass || got.ObservedValue != 30.0 || got.ObservedUnit != "d" {
		t.Errorf("tls:expiry at 30 days = %+v, want pass, 30.0 d", got)
	}

	h.clock.Set(start().Add(80 * 24 * time.Hour))
	if got := expiry.Run(t.Context()); got.Status != admin.StatusWarn || got.Output != "certificate expires in 10 days" {
		t.Errorf("tls:expiry at 10 days = %+v, want warn %q", got, "certificate expires in 10 days")
	}

	h.clock.Set(start().Add(91 * 24 * time.Hour))
	if got := expiry.Run(t.Context()); got.Status != admin.StatusFail || got.Output != "certificate expired" {
		t.Errorf("tls:expiry past NotAfter = %+v, want fail %q", got, "certificate expired")
	}
}

func newMetrics(t *testing.T) *telemetry.Metrics {
	t.Helper()
	m, err := telemetry.NewMetrics("v1.2.3", "abc1234")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return m
}

func scrape(t *testing.T, m *telemetry.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestDaysLeftMetric(t *testing.T) {
	m := newMetrics(t)
	h := newHarness(t, ninety(), tlsacme.WithMeter(m.Meter()))

	if body := scrape(t, m); strings.Contains(body, "hephaestus_tls_certificate_days_left{") {
		t.Errorf("metrics body reports days left before a certificate is loaded:\n%s", body)
	}
	if _, err := h.mgr.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	body := scrape(t, m)
	if !strings.Contains(body, "hephaestus_tls_certificate_days_left") {
		t.Errorf("metrics body missing hephaestus_tls_certificate_days_left:\n%s", body)
	}
	if !strings.Contains(body, "} 90") {
		t.Errorf("metrics body: days left is not 90:\n%s", body)
	}
}

func TestRun_StopsOnCancel(t *testing.T) {
	h := newHarness(t, ninety())
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.mgr.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for !h.mgr.Status().Loaded {
		if time.Now().After(deadline) {
			t.Fatal("no certificate issued within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRun_AbandonsBlockedObtain(t *testing.T) {
	block := make(chan struct{})
	h := newHarness(t, &tlsacmetest.SelfSigned{NotBefore: start(), NotAfter: start().Add(ninetyDays), Block: block})
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.mgr.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return while Obtain was blocked")
	}
	close(block)
}

// failSaves makes every certificate save fail until the returned function
// lifts the fault. A trigger fails the upsert inside the database, so the
// Manager still talks to its real Repository.
func failSaves(t *testing.T, h *harness) (lift func()) {
	t.Helper()
	if _, err := h.db.Bun().ExecContext(t.Context(),
		"CREATE TRIGGER fail_save BEFORE INSERT ON tls_certificates BEGIN SELECT RAISE(ABORT, 'disk full'); END"); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	return func() {
		t.Helper()
		if _, err := h.db.Bun().ExecContext(t.Context(), "DROP TRIGGER fail_save"); err != nil {
			t.Fatalf("drop trigger: %v", err)
		}
	}
}

func TestReconcile_StoreFailureKeepsCertAndRetriesSaveOnly(t *testing.T) {
	h := newHarness(t, ninety())
	lift := failSaves(t, h)

	for i, wantWait := range []time.Duration{10 * time.Minute, 20 * time.Minute} {
		wait, err := h.mgr.Reconcile(t.Context())
		if err == nil || !strings.Contains(err.Error(), "store issued certificate") {
			t.Fatalf("Reconcile #%d error = %v, want one containing %q", i+1, err, "store issued certificate")
		}
		// The next step is a save, not an Obtain, so no 24h floor applies.
		if wait != wantWait {
			t.Errorf("Reconcile #%d wait = %s, want %s", i+1, wait, wantWait)
		}
		cert, err := h.mgr.GetCertificate(nil)
		if err != nil || cert.Leaf.DNSNames[0] != domain {
			t.Errorf("GetCertificate after failed save = %v, %v; want the issued certificate", cert, err)
		}
		h.clock.Set(h.clock.Now().Add(wait))
	}
	if _, ok, err := h.repo.LoadCertificate(t.Context(), domain); err != nil || ok {
		t.Fatalf("LoadCertificate = ok %v, err %v; want no stored row while saves fail", ok, err)
	}

	lift()
	wait, err := h.mgr.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile after the fault is lifted: %v", err)
	}
	if want := renewIn(h.clock.Now()); wait != want {
		t.Errorf("wait = %s, want the normal renewal wait %s", wait, want)
	}
	if got := h.iss.Calls.Load(); got != 1 {
		t.Errorf("issuer calls = %d, want 1: a failed save must not re-issue", got)
	}
	if _, ok, err := h.repo.LoadCertificate(t.Context(), domain); err != nil || !ok {
		t.Errorf("LoadCertificate = ok %v, err %v; want the retried save stored", ok, err)
	}
	if s := h.mgr.Status(); s.Failures != 0 || s.LastError != "" {
		t.Errorf("Status() = %+v, want failures reset", s)
	}

	// Saved and not due: nothing more to do.
	if _, err := h.mgr.Reconcile(t.Context()); err != nil {
		t.Errorf("final Reconcile: %v", err)
	}
	if got := h.iss.Calls.Load(); got != 1 {
		t.Errorf("issuer calls = %d, want 1", got)
	}
}

// renewIn is the wait the harness's 90-day certificate (valid from start())
// has left at now.
func renewIn(now time.Time) time.Duration {
	return tlsacme.RenewAt(start(), start().Add(ninetyDays)).Sub(now)
}

func TestReconcile_NoReissueWithin24h(t *testing.T) {
	// Already due the moment it is issued: RenewAt is 30 days in the past.
	h := newHarness(t, &tlsacmetest.SelfSigned{NotBefore: start().Add(-ninetyDays), NotAfter: start().Add(time.Hour)})

	wait, err := h.mgr.Reconcile(t.Context())
	if err == nil || !strings.Contains(err.Error(), "already due for renewal") {
		t.Fatalf("Reconcile error = %v, want one containing %q", err, "already due for renewal")
	}
	if want := 24 * time.Hour; wait != want {
		t.Errorf("wait = %s, want %s", wait, want)
	}

	for _, elapsed := range []time.Duration{10 * time.Minute, 5 * time.Hour, 23 * time.Hour} {
		h.clock.Set(start().Add(elapsed))
		wait, err := h.mgr.Reconcile(t.Context())
		if err != nil {
			t.Fatalf("Reconcile at +%s: %v", elapsed, err)
		}
		if want := 24*time.Hour - elapsed; wait != want {
			t.Errorf("wait at +%s = %s, want %s", elapsed, wait, want)
		}
		if got := h.iss.Calls.Load(); got != 1 {
			t.Fatalf("issuer calls at +%s = %d, want 1", elapsed, got)
		}
	}

	h.clock.Set(start().Add(24 * time.Hour))
	if _, err := h.mgr.Reconcile(t.Context()); err == nil {
		t.Error("Reconcile at +24h error = nil, want the already-due error again")
	}
	if got := h.iss.Calls.Load(); got != 2 {
		t.Errorf("issuer calls at +24h = %d, want 2", got)
	}
}
