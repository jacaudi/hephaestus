package tlscert_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"go.jacaudi.dev/hephaestus/internal/admin"
	"go.jacaudi.dev/hephaestus/internal/telemetry"
	"go.jacaudi.dev/hephaestus/internal/tlscert"
	"go.jacaudi.dev/hephaestus/internal/tlscert/tlscerttest"
)

const (
	fqdn      = "hephaestus.example.test"
	otherName = "other.example.test"
	day       = 24 * time.Hour
	tick      = 10 * time.Millisecond
)

func t0() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }

// clock is a settable clock safe for the goroutine Run starts.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// errLog collects what the error handler receives.
type errLog struct {
	mu   sync.Mutex
	errs []error
}

func (l *errLog) handle(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, err)
}

func (l *errLog) all() []error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]error(nil), l.errs...)
}

// files writes a certificate for name valid from t0-1h for lifetime, in a new directory.
func files(t *testing.T, name string, lifetime time.Duration) (certFile, keyFile string) {
	t.Helper()
	return tlscerttest.WriteSelfSigned(t, t.TempDir(), name, t0().Add(-time.Hour), t0().Add(-time.Hour+lifetime))
}

// rewrite replaces the certificate pair in the directory of certFile with a new
// one for name, and moves both files' modification times to mtime so the change
// is visible however coarse the file system's clock is.
func rewrite(t *testing.T, certFile, name string, mtime time.Time) {
	t.Helper()
	tlscerttest.WriteSelfSigned(t, filepath.Dir(certFile), name, t0().Add(-time.Hour), t0().Add(90*day))
	for _, f := range []string{certFile, filepath.Join(filepath.Dir(certFile), "tls.key")} {
		if err := os.Chtimes(f, mtime, mtime); err != nil {
			t.Fatalf("chtimes %s: %v", f, err)
		}
	}
}

// replaceFile puts content at path through a temporary file and a rename, so a
// reader never sees a truncated file.
func replaceFile(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename %s: %v", tmp, err)
	}
}

// serial is the serial number of the leaf the loader serves.
func serial(t *testing.T, l *tlscert.Loader) *big.Int {
	t.Helper()
	c, err := l.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	return c.Leaf.SerialNumber
}

// fileSerial is the serial number of the leaf in a PEM file.
func fileSerial(t *testing.T, certFile string) *big.Int {
	t.Helper()
	b, err := os.ReadFile(certFile)
	if err != nil {
		t.Fatalf("read %s: %v", certFile, err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		t.Fatalf("%s holds no PEM block", certFile)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse %s: %v", certFile, err)
	}
	return leaf.SerialNumber
}

func newLoader(t *testing.T, certFile, keyFile string, opts ...tlscert.Option) *tlscert.Loader {
	t.Helper()
	opts = append([]tlscert.Option{tlscert.WithClock(t0)}, opts...)
	l, err := tlscert.NewLoader(certFile, keyFile, fqdn, opts...)
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	return l
}

// startRun runs l until the returned stop is called, which waits for Run to return nil.
func startRun(t *testing.T, l *tlscert.Loader) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	return func() {
		t.Helper()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run = %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	}
}

// eventually polls cond until it holds, failing the test after 2s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within 2s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestNewLoader_ServesPair(t *testing.T) {
	certFile, keyFile := files(t, fqdn, 90*day)

	l := newLoader(t, certFile, keyFile)

	c, err := l.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if got, want := c.Leaf.SerialNumber, fileSerial(t, certFile); got.Cmp(want) != 0 {
		t.Errorf("served serial = %v, want %v", got, want)
	}
	if err := c.Leaf.VerifyHostname(fqdn); err != nil {
		t.Errorf("served leaf does not cover %s: %v", fqdn, err)
	}
	if c.PrivateKey == nil {
		t.Error("served certificate has no private key")
	}
}

func TestNewLoader_Unreadable(t *testing.T) {
	certFile, keyFile := files(t, fqdn, 90*day)
	missing := filepath.Join(t.TempDir(), "absent")

	tests := []struct {
		name            string
		certFile, kFile string
	}{
		{"certificate", missing, keyFile},
		{"key", certFile, missing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, err := tlscert.NewLoader(tt.certFile, tt.kFile, fqdn, tlscert.WithClock(t0))
			if err == nil {
				t.Fatal("NewLoader succeeded with an unreadable file, want an error")
			}
			if l != nil {
				t.Errorf("NewLoader returned a loader along with %v", err)
			}
			if !strings.Contains(err.Error(), "absent") {
				t.Errorf("error = %q, want it to name the missing file", err)
			}
		})
	}
}

func TestNewLoader_KeyMismatch(t *testing.T) {
	certFile, _ := files(t, fqdn, 90*day)
	_, otherKey := files(t, fqdn, 90*day)

	l, err := tlscert.NewLoader(certFile, otherKey, fqdn, tlscert.WithClock(t0))

	if err == nil {
		t.Fatal("NewLoader succeeded with a key that does not match, want an error")
	}
	if l != nil {
		t.Errorf("NewLoader returned a loader along with %v", err)
	}
	if want := "does not match"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

func TestNewLoader_WrongName(t *testing.T) {
	certFile, keyFile := files(t, otherName, 90*day)

	l, err := tlscert.NewLoader(certFile, keyFile, fqdn, tlscert.WithClock(t0))

	if err == nil {
		t.Fatal("NewLoader succeeded with a certificate for another name, want an error")
	}
	if l != nil {
		t.Errorf("NewLoader returned a loader along with %v", err)
	}
	if want := "does not cover " + fqdn; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

func TestNewLoader_Expired(t *testing.T) {
	certFile, keyFile := files(t, fqdn, 30*day)
	late := t0().Add(31 * day)

	l, err := tlscert.NewLoader(certFile, keyFile, fqdn, tlscert.WithClock(func() time.Time { return late }))

	if err == nil {
		t.Fatal("NewLoader succeeded with an expired certificate, want an error")
	}
	if l != nil {
		t.Errorf("NewLoader returned a loader along with %v", err)
	}
	if want := "expired"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

func TestNewLoader_NotYetValid(t *testing.T) {
	certFile, keyFile := tlscerttest.WriteSelfSigned(t, t.TempDir(), fqdn, t0().Add(time.Hour), t0().Add(30*day))

	l, err := tlscert.NewLoader(certFile, keyFile, fqdn, tlscert.WithClock(t0))

	if err == nil {
		t.Fatal("NewLoader succeeded with a certificate that is not yet valid, want an error")
	}
	if l != nil {
		t.Errorf("NewLoader returned a loader along with %v", err)
	}
	if want := "not yet valid"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

func TestNewLoader_RejectsNonPositiveInterval(t *testing.T) {
	certFile, keyFile := files(t, fqdn, 90*day)

	for _, d := range []time.Duration{0, -time.Second} {
		t.Run(d.String(), func(t *testing.T) {
			l, err := tlscert.NewLoader(certFile, keyFile, fqdn, tlscert.WithClock(t0), tlscert.WithInterval(d))

			if err == nil {
				t.Fatal("NewLoader succeeded with a non-positive interval, want an error")
			}
			if l != nil {
				t.Errorf("NewLoader returned a loader along with %v", err)
			}
			if want := "interval"; !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
		})
	}
}

func TestRun_ReloadsOnChange(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	certFile, keyFile := files(t, fqdn, 90*day)
	l := newLoader(t, certFile, keyFile, tlscert.WithInterval(tick))
	old := serial(t, l)
	stop := startRun(t, l)
	defer stop()

	rewrite(t, certFile, fqdn, time.Now().Add(time.Hour))

	want := fileSerial(t, certFile)
	eventually(t, "reload of the rewritten pair", func() bool { return serial(t, l).Cmp(want) == 0 })
	if want.Cmp(old) == 0 {
		t.Fatal("test bug: the rewritten pair has the old serial")
	}
}

// TestRun_ReloadsOnSymlinkSwap pins the Kubernetes secret layout: the
// configured paths are symlinks, and an update repoints them.
func TestRun_ReloadsOnSymlinkSwap(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	v1, v2, live := t.TempDir(), t.TempDir(), t.TempDir()
	tlscerttest.WriteSelfSigned(t, v1, fqdn, t0().Add(-time.Hour), t0().Add(90*day))
	tlscerttest.WriteSelfSigned(t, v2, fqdn, t0().Add(-time.Hour), t0().Add(90*day))
	later := time.Now().Add(time.Hour)
	for _, f := range []string{"tls.crt", "tls.key"} {
		if err := os.Chtimes(filepath.Join(v2, f), later, later); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
		if err := os.Symlink(filepath.Join(v1, f), filepath.Join(live, f)); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}
	certFile, keyFile := filepath.Join(live, "tls.crt"), filepath.Join(live, "tls.key")
	l := newLoader(t, certFile, keyFile, tlscert.WithInterval(tick))
	stop := startRun(t, l)
	defer stop()

	for _, f := range []string{"tls.crt", "tls.key"} {
		if err := os.Remove(filepath.Join(live, f)); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if err := os.Symlink(filepath.Join(v2, f), filepath.Join(live, f)); err != nil {
			t.Fatalf("symlink: %v", err)
		}
	}

	want := fileSerial(t, filepath.Join(v2, "tls.crt"))
	eventually(t, "reload after the symlinks were repointed", func() bool { return serial(t, l).Cmp(want) == 0 })
}

func TestRun_BadReloadKeepsCurrent(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	certFile, keyFile := files(t, fqdn, 90*day)
	var errs errLog
	l := newLoader(t, certFile, keyFile, tlscert.WithInterval(tick), tlscert.WithErrorHandler(errs.handle))
	old := serial(t, l)
	stop := startRun(t, l)
	defer stop()

	replaceFile(t, keyFile, "not a key")

	eventually(t, "the error handler call", func() bool { return len(errs.all()) > 0 })
	time.Sleep(15 * tick) // several more ticks: the same bad file must not be retried
	if got := errs.all(); len(got) != 1 {
		t.Errorf("error handler calls = %d, want 1; the first was %v", len(got), got[0])
	}
	if got := serial(t, l); got.Cmp(old) != 0 {
		t.Errorf("served serial = %v after a bad reload, want the old %v", got, old)
	}
}

func TestRun_RecoversAfterBadReload(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	certFile, keyFile := files(t, fqdn, 90*day)
	var errs errLog
	l := newLoader(t, certFile, keyFile, tlscert.WithInterval(tick), tlscert.WithErrorHandler(errs.handle))
	stop := startRun(t, l)
	defer stop()
	replaceFile(t, keyFile, "not a key")
	eventually(t, "the error handler call", func() bool { return len(errs.all()) > 0 })

	rewrite(t, certFile, fqdn, time.Now().Add(time.Hour))

	want := fileSerial(t, certFile)
	eventually(t, "reload of the repaired pair", func() bool { return serial(t, l).Cmp(want) == 0 })
}

// TestRun_RejectsReplacementThatFailsTheChecks pins that a reload applies the
// NewLoader checks: a replacement for another name, or an expired one, is
// reported and never served.
func TestRun_RejectsReplacementThatFailsTheChecks(t *testing.T) {
	tests := []struct {
		name       string
		certName   string
		lifetime   time.Duration // from t0-1h
		wantReason string
	}{
		{"other name", otherName, 90 * day, "does not cover " + fqdn},
		{"expired", fqdn, 30 * time.Minute, "expired"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
			certFile, keyFile := files(t, fqdn, 90*day)
			var errs errLog
			l := newLoader(t, certFile, keyFile, tlscert.WithInterval(tick), tlscert.WithErrorHandler(errs.handle))
			old := serial(t, l)
			defer startRun(t, l)()

			tlscerttest.WriteSelfSigned(t, filepath.Dir(certFile), tt.certName, t0().Add(-time.Hour), t0().Add(-time.Hour+tt.lifetime))
			later := time.Now().Add(time.Hour)
			for _, f := range []string{certFile, keyFile} {
				if err := os.Chtimes(f, later, later); err != nil {
					t.Fatalf("chtimes: %v", err)
				}
			}

			// The cert and key land one after the other, so a tick between them
			// may report a mismatch first; the reason must still be reported.
			eventually(t, "an error naming "+tt.wantReason, func() bool {
				return slices.ContainsFunc(errs.all(), func(err error) bool { return strings.Contains(err.Error(), tt.wantReason) })
			})
			if got := serial(t, l); got.Cmp(old) != 0 {
				t.Errorf("served serial = %v, want the old %v", got, old)
			}
		})
	}
}

func checkByName(t *testing.T, l *tlscert.Loader, name string) admin.Check {
	t.Helper()
	for _, c := range l.Checks() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("Checks() has no %q", name)
	return admin.Check{}
}

func TestChecks_ExpiryWarnsUnder14Days(t *testing.T) {
	certFile, keyFile := files(t, fqdn, 90*day) // expires t0 - 1h + 90d
	clk := &clock{now: t0()}
	l := newLoader(t, certFile, keyFile, tlscert.WithClock(clk.Now))
	check := checkByName(t, l, "tls:expiry")
	if check.Critical {
		t.Error("tls:expiry is critical, want informational")
	}

	tests := []struct {
		name       string
		at         time.Time
		wantStatus admin.Status
	}{
		{"fresh", t0(), admin.StatusPass},
		{"15 days left", t0().Add(90*day - time.Hour - 15*day), admin.StatusPass},
		{"13 days left", t0().Add(90*day - time.Hour - 13*day), admin.StatusWarn},
		{"1 day left", t0().Add(90*day - time.Hour - day), admin.StatusWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clk.Set(tt.at)

			got := check.Run(t.Context())

			if got.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q (%+v)", got.Status, tt.wantStatus, got)
			}
			days, ok := got.ObservedValue.(float64)
			if !ok || got.ObservedUnit != "d" {
				t.Fatalf("observed = %v (%T) %q, want days as a float64 in %q", got.ObservedValue, got.ObservedValue, got.ObservedUnit, "d")
			}
			if want := (90*day - time.Hour - tt.at.Sub(t0())).Hours() / 24; days != want {
				t.Errorf("observed days = %v, want %v", days, want)
			}
		})
	}
}

func TestChecks_FailsAfterExpiry(t *testing.T) {
	certFile, keyFile := files(t, fqdn, 30*day) // expires t0 - 1h + 30d
	clk := &clock{now: t0()}
	l := newLoader(t, certFile, keyFile, tlscert.WithClock(clk.Now))
	check := checkByName(t, l, "tls:certificate")
	if !check.Critical {
		t.Error("tls:certificate is not critical, want critical")
	}

	if got := check.Run(t.Context()); got.Status != admin.StatusPass {
		t.Errorf("before expiry: status = %q, want pass (%+v)", got.Status, got)
	}

	clk.Set(t0().Add(30 * day))

	if got := check.Run(t.Context()); got.Status != admin.StatusFail {
		t.Errorf("after expiry: status = %q, want fail (%+v)", got.Status, got)
	}
}

func TestChecks_FailsBeforeNotBefore(t *testing.T) {
	certFile, keyFile := files(t, fqdn, 30*day) // valid from t0 - 1h
	clk := &clock{now: t0()}
	l := newLoader(t, certFile, keyFile, tlscert.WithClock(clk.Now))
	check := checkByName(t, l, "tls:certificate")

	clk.Set(t0().Add(-2 * time.Hour))

	if got := check.Run(t.Context()); got.Status != admin.StatusFail {
		t.Errorf("before NotBefore: status = %q, want fail (%+v)", got.Status, got)
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

func TestMetrics_DaysLeftAndReloadFailures(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	m := newMetrics(t)
	certFile, keyFile := tlscerttest.WriteSelfSigned(t, t.TempDir(), fqdn, t0().Add(-time.Hour), t0().Add(90*day))
	var errs errLog
	l := newLoader(t, certFile, keyFile, tlscert.WithMeter(m.Meter()), tlscert.WithInterval(tick), tlscert.WithErrorHandler(errs.handle))
	stop := startRun(t, l)
	defer stop()

	body := scrape(t, m)
	if !strings.Contains(body, "hephaestus_tls_certificate_days_left") || !strings.Contains(body, "} 90") {
		t.Errorf("metrics lack hephaestus_tls_certificate_days_left of 90:\n%s", body)
	}
	if strings.Contains(body, "hephaestus_tls_reload_failures_total{") {
		t.Errorf("metrics count a reload failure before any happened:\n%s", body)
	}

	replaceFile(t, keyFile, "not a key")
	eventually(t, "the error handler call", func() bool { return len(errs.all()) > 0 })

	if body := scrape(t, m); !strings.Contains(body, "hephaestus_tls_reload_failures_total{") {
		t.Errorf("metrics lack hephaestus_tls_reload_failures_total after a bad reload:\n%s", body)
	}
}

// chain writes a chain for fqdn valid from t0-1h for 90 days, in a new
// directory, and returns its files and the root a client must trust.
func chain(t *testing.T, withIntermediate, includeIntermediate bool) (certFile, keyFile string, root *x509.Certificate) {
	t.Helper()
	return tlscerttest.WriteChain(t, t.TempDir(), fqdn, t0().Add(-time.Hour), t0().Add(90*day), withIntermediate, includeIntermediate)
}

func TestNewLoader_ChainReachesRoot(t *testing.T) {
	certFile, keyFile, root := chain(t, false, false)

	l := newLoader(t, certFile, keyFile, tlscert.WithRoots([]*x509.Certificate{root}))

	if got, want := serial(t, l), fileSerial(t, certFile); got.Cmp(want) != 0 {
		t.Errorf("served serial = %v, want %v", got, want)
	}
}

func TestNewLoader_ChainWithIntermediate(t *testing.T) {
	certFile, keyFile, root := chain(t, true, true)

	l := newLoader(t, certFile, keyFile, tlscert.WithRoots([]*x509.Certificate{root}))

	if got, want := serial(t, l), fileSerial(t, certFile); got.Cmp(want) != 0 {
		t.Errorf("served serial = %v, want %v", got, want)
	}
}

const untrustedChain = "does not reach a root the embedded iPXE trusts"

func TestNewLoader_MissingIntermediate(t *testing.T) {
	certFile, keyFile, root := chain(t, true, false)

	l, err := tlscert.NewLoader(certFile, keyFile, fqdn, tlscert.WithClock(t0), tlscert.WithRoots([]*x509.Certificate{root}))

	if err == nil {
		t.Fatal("NewLoader succeeded with a leaf-only file whose issuer is an intermediate, want an error")
	}
	if l != nil {
		t.Errorf("NewLoader returned a loader along with %v", err)
	}
	if !strings.Contains(err.Error(), untrustedChain) {
		t.Errorf("error = %q, want it to contain %q", err, untrustedChain)
	}
}

func TestNewLoader_OtherCA(t *testing.T) {
	certFile, keyFile := files(t, fqdn, 90*day) // self-signed: no CA issued it
	_, _, root := chain(t, false, false)

	l, err := tlscert.NewLoader(certFile, keyFile, fqdn, tlscert.WithClock(t0), tlscert.WithRoots([]*x509.Certificate{root}))

	if err == nil {
		t.Fatal("NewLoader succeeded with a certificate from another CA, want an error")
	}
	if l != nil {
		t.Errorf("NewLoader returned a loader along with %v", err)
	}
	if !strings.Contains(err.Error(), untrustedChain) {
		t.Errorf("error = %q, want it to contain %q", err, untrustedChain)
	}
}

// TestRun_ReloadRejectsUntrustedChain pins that a reload applies the root check:
// a replacement from a CA the roots do not hold is reported and never served.
func TestRun_ReloadRejectsUntrustedChain(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	certFile, keyFile, root := chain(t, true, true)
	var errs errLog
	l := newLoader(t, certFile, keyFile, tlscert.WithRoots([]*x509.Certificate{root}), tlscert.WithInterval(tick), tlscert.WithErrorHandler(errs.handle))
	old := serial(t, l)
	stop := startRun(t, l)
	defer stop()

	// The replacement is written and stamped in a sibling directory, then each
	// file is renamed into place (a rename keeps the modification time).
	next := t.TempDir()
	nextCert, nextKey, _ := tlscerttest.WriteChain(t, next, fqdn, t0().Add(-time.Hour), t0().Add(90*day), true, true)
	later := time.Now().Add(time.Hour)
	for _, f := range []string{nextCert, nextKey} {
		if err := os.Chtimes(f, later, later); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	for _, p := range [][2]string{{nextCert, certFile}, {nextKey, keyFile}} {
		if err := os.Rename(p[0], p[1]); err != nil {
			t.Fatalf("rename %s: %v", p[0], err)
		}
	}

	// The two renames land one after the other, so a tick between them may see
	// the new certificate with the old key: a mismatch. That and the untrusted
	// chain, reported once, are the only errors there can be.
	untrusted := func() (n int) {
		for _, err := range errs.all() {
			if strings.Contains(err.Error(), untrustedChain) {
				n++
			}
		}
		return n
	}
	eventually(t, "an error naming the untrusted chain", func() bool { return untrusted() > 0 })
	time.Sleep(15 * tick) // several more ticks: the same bad files must not be retried
	if n := untrusted(); n != 1 {
		t.Errorf("handler calls naming the untrusted chain = %d, want 1", n)
	}
	for _, err := range errs.all() {
		if !strings.Contains(err.Error(), untrustedChain) && !strings.Contains(err.Error(), "does not match") {
			t.Errorf("unexpected reload error: %v", err)
		}
	}
	if got := serial(t, l); got.Cmp(old) != 0 {
		t.Errorf("served serial = %v after an untrusted reload, want the old %v", got, old)
	}
}
