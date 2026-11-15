package boot

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/telemetry"
)

// discoverForm is the report the iPXE discover script posts for the fixture
// machine, with the given key/value pairs replacing or adding to it.
func discoverForm(kv ...string) url.Values {
	form := url.Values{
		"uuid": {bootUUID}, "mac": {bootMAC}, "serial": {"S1"}, "product": {"P1"}, "manufacturer": {"ACME"},
		"nic0_mac": {bootMAC}, "nic0_chip": {"r8169"}, "nic1_mac": {"aa:bb:cc:00:00:02"}, "nic1_chip": {"igc"},
		"cpuvendor": {"GenuineIntel"}, "cpumodel": {"Xeon"}, "memsize": {"17179869184"}, "platform": {"efi"}, "arch": {"x86_64"},
	}
	for i := 0; i+1 < len(kv); i += 2 {
		form.Set(kv[i], kv[i+1])
	}
	return form
}

// nthMachineForm is the report of the n-th distinct machine, 1 to 255.
func nthMachineForm(n int) url.Values {
	mac := fmt.Sprintf("aa:bb:cc:00:00:%02x", n)
	return discoverForm("uuid", fmt.Sprintf("00000000-0000-4000-8000-%012d", n), "mac", mac, "nic0_mac", mac, "nic1_mac", "")
}

// discoverBodies are the raw bodies the tests post: every form the handler
// tests build. FuzzParseDiscoverForm seeds with them.
func discoverBodies() []string {
	forms := []url.Values{
		discoverForm(), nthMachineForm(1), nthMachineForm(9),
		discoverForm("serial", "S2"), discoverForm("memsize", "0"), discoverForm("memsize", ""),
		discoverForm("serial", "<script>alert(1)</script>"), discoverForm("serial", strings.Repeat("a", maxValueBytes+1)),
		discoverForm("nic0_mac", "zz"), discoverForm("nic1_mac", ""), discoverForm("nic16_mac", "aa:bb:cc:00:00:10"),
		discoverForm("manufacturer", "AC\x00ME"), discoverForm("manufacturer", "AC\xffME"),
		discoverForm("serial", "S\x001\xff"), discoverForm("uuid", "", "mac", ""), discoverForm("uuid", zeroUUID),
	}
	bodies := make([]string, 0, len(forms))
	for _, form := range forms {
		bodies = append(bodies, form.Encode())
	}
	return bodies
}

// newDiscoverRequest is a TLS POST /discover from the provisioning subnet.
func newDiscoverRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/discover", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = clientAddr
	req.TLS = &tls.ConnectionState{}
	return req
}

// discover posts form to /discover through TLS.
func (g *rig) discover(form url.Values) *httptest.ResponseRecorder {
	g.t.Helper()
	return g.discoverBody(form.Encode())
}

// discoverBody posts a raw body to /discover through TLS.
func (g *rig) discoverBody(body string) *httptest.ResponseRecorder {
	g.t.Helper()
	rec := httptest.NewRecorder()
	g.h.TLS().ServeHTTP(rec, newDiscoverRequest(g.t, body))
	return rec
}

// discoverOK posts form and requires a 200 "ok\n".
func (g *rig) discoverOK(form url.Values) {
	g.t.Helper()
	rec := g.discover(form)
	wantStatus(g.t, rec, http.StatusOK)
	if got, want := rec.Body.String(), "ok\n"; got != want {
		g.t.Fatalf("body = %q, want %q", got, want)
	}
}

// discoverWide posts form through WideEvents around TLS and returns the one log line.
func (g *rig) discoverWide(form url.Values) (*httptest.ResponseRecorder, map[string]any) {
	g.t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	rec := httptest.NewRecorder()
	telemetry.WideEvents(g.h.TLS(), logger, slog.LevelInfo).ServeHTTP(rec, newDiscoverRequest(g.t, form.Encode()))

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		g.t.Fatalf("log line is not one JSON object: %v\n%s", err, buf.String())
	}
	return rec, line
}

// storedFacts decodes the newest stored report of the fixture machine.
func (g *rig) storedFacts() facts {
	g.t.Helper()
	var raw string
	err := g.d.Bun().NewRaw("SELECT facts FROM inventory_reports ORDER BY id DESC LIMIT 1").Scan(g.t.Context(), &raw)
	if err != nil {
		g.t.Fatalf("read the newest report: %v", err)
	}
	var f facts
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		g.t.Fatalf("stored facts are not JSON: %v\n%s", err, raw)
	}
	return f
}

func (g *rig) reportCount() int { return g.count("SELECT count(*) FROM inventory_reports") }

func TestDiscover_StoresReport(t *testing.T) {
	g := newRig(t)

	g.discoverOK(discoverForm())

	m := g.machine()
	if len(m.MACs) != 2 || m.Manufacturer != "ACME" || m.State != fleet.StateDiscovered {
		t.Errorf("machine = MACs %v manufacturer %q state %q, want 2 MACs, ACME, discovered", m.MACs, m.Manufacturer, m.State)
	}
	if n := g.reportCount(); n != 1 {
		t.Fatalf("reports = %d, want 1", n)
	}
	if n := g.count("SELECT count(*) FROM inventory_reports WHERE source = 'ipxe'"); n != 1 {
		t.Errorf("reports with source ipxe = %d, want 1", n)
	}
	f := g.storedFacts()
	if f.Manufacturer != "ACME" || len(f.NICs) != 2 || f.NICs[1].Chip != "igc" || f.MemSize != "17179869184" {
		t.Errorf("stored facts = %+v, want manufacturer ACME, 2 NICs, nics[1].chip igc, memsize 17179869184", f)
	}
	if f.UUID != bootUUID || f.MAC != bootMAC || f.Serial != "S1" || f.Arch != "x86_64" || f.NICs[1].Index != 1 {
		t.Errorf("stored facts = %+v, want the normalized identity and NIC indexes", f)
	}
}

func TestDiscover_UnchangedNotCharged(t *testing.T) {
	g := newRig(t, WithLimiter(8, discoverWindow, limiterCapacity))

	for i := range 20 {
		rec := g.discover(discoverForm())
		if rec.Code != http.StatusOK {
			t.Fatalf("report #%d: status %d, want 200", i+1, rec.Code)
		}
	}

	if n := g.reportCount(); n != 1 {
		t.Errorf("reports = %d, want 1", n)
	}
}

func TestDiscover_RateLimited(t *testing.T) {
	g := newRig(t)

	for n := 1; n <= 8; n++ {
		if rec := g.discover(nthMachineForm(n)); rec.Code != http.StatusOK {
			t.Fatalf("machine %d: status %d, want 200", n, rec.Code)
		}
	}
	rec := g.discover(nthMachineForm(9))

	wantStatus(t, rec, http.StatusTooManyRequests)
	if got := rec.Header().Get("Retry-After"); got != "600" {
		t.Errorf("Retry-After = %q, want 600", got)
	}
	if n := g.count("SELECT count(*) FROM machines"); n != 8 {
		t.Errorf("machines = %d, want 8", n)
	}
}

func TestDiscover_Conflict(t *testing.T) {
	g := newRig(t)
	g.boot(bootQ)

	rec := g.discover(discoverForm("serial", "S2"))

	wantStatus(t, rec, http.StatusConflict)
	if n := g.reportCount(); n != 0 {
		t.Errorf("reports = %d, want 0", n)
	}
	if !g.machine().Junk {
		t.Error("machine is not marked Junk after the conflict")
	}
	if n := g.count("SELECT count(*) FROM audit_events WHERE action = 'alert.uuid_conflict'"); n != 1 {
		t.Errorf("alert.uuid_conflict audit rows = %d, want 1", n)
	}
}

func TestDiscover_NewMachineChargedOnce(t *testing.T) {
	g := newRig(t, WithLimiter(1, discoverWindow, limiterCapacity))

	g.discoverOK(discoverForm())
	rec := g.discover(discoverForm("memsize", "34359738368"))

	wantStatus(t, rec, http.StatusTooManyRequests)
	if got := rec.Header().Get("Retry-After"); got != "600" {
		t.Errorf("Retry-After = %q, want 600", got)
	}
	if n := g.reportCount(); n != 1 {
		t.Errorf("reports = %d, want 1: the changed report must have been refused", n)
	}
}

func TestDiscover_FirstContactAudited(t *testing.T) {
	g := newRig(t)

	g.discoverOK(discoverForm())

	if n := g.count("SELECT count(*) FROM audit_events WHERE action = 'transition.first_contact'"); n != 1 {
		t.Errorf("transition.first_contact audit rows = %d, want 1", n)
	}
}

func TestDiscover_MemsizeZeroIsUnknown(t *testing.T) {
	for _, memsize := range []string{"0", ""} {
		t.Run(fmt.Sprintf("memsize %q", memsize), func(t *testing.T) {
			g := newRig(t)

			g.discoverOK(discoverForm("memsize", memsize))

			if got := g.storedFacts().MemSize; got != "unknown" {
				t.Errorf("stored memsize = %q, want unknown", got)
			}
		})
	}
}

func TestDiscover_RepositoryErrorIsProblemJSON(t *testing.T) {
	g := newRig(t)
	if err := g.d.Close(); err != nil {
		t.Fatalf("close the database: %v", err)
	}

	rec, line := g.discoverWide(discoverForm())

	wantStatus(t, rec, http.StatusInternalServerError)
	if got, want := rec.Header().Get("Content-Type"), "application/problem+json"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := rec.Body.String(), `{"type":"about:blank","title":"Internal Server Error","status":500}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	var problem struct {
		Status int    `json:"status"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil || problem.Status != 500 || problem.Title != "Internal Server Error" {
		t.Errorf("body decodes to %+v (%v), want status 500 and title Internal Server Error", problem, err)
	}
	if _, ok := line["error"]; !ok {
		t.Errorf("log line = %v, want an error attribute", line)
	}
}

func TestDiscover_RejectsOversizeAndMalformed(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"serial of 257 bytes", discoverForm("serial", strings.Repeat("a", maxValueBytes+1)).Encode(), http.StatusBadRequest},
		{"chip of 257 bytes", discoverForm("nic0_chip", strings.Repeat("a", maxValueBytes+1)).Encode(), http.StatusBadRequest},
		{"17 KiB body", "serial=" + strings.Repeat("a", 17<<10), http.StatusRequestEntityTooLarge},
		{"nic MAC zz", discoverForm("nic0_mac", "zz").Encode(), http.StatusBadRequest},
		{"MAC zz", discoverForm("mac", "zz").Encode(), http.StatusBadRequest},
		{"no uuid and no mac", discoverForm("uuid", "", "mac", "").Encode(), http.StatusBadRequest},
		{"malformed uuid and no mac", discoverForm("uuid", "nope", "mac", "").Encode(), http.StatusBadRequest},
		{"malformed form", "%zz=1", http.StatusBadRequest},
	}
	g := newRig(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := g.discoverBody(tc.body)

			wantStatus(t, rec, tc.want)
		})
	}
	t.Run("GET", func(t *testing.T) {
		req := newDiscoverRequest(t, "")
		req.Method = http.MethodGet
		rec := httptest.NewRecorder()

		g.h.TLS().ServeHTTP(rec, req)

		wantStatus(t, rec, http.StatusMethodNotAllowed)
	})
	if n := g.reportCount(); n != 0 {
		t.Errorf("reports = %d, want 0 after every rejection", n)
	}
	if n := g.count("SELECT count(*) FROM machines"); n != 0 {
		t.Errorf("machines = %d, want 0 after every rejection", n)
	}
}

func TestDiscover_InvalidFactsTextRejected(t *testing.T) {
	cases := []struct{ name, key, value string }{
		{"NUL in manufacturer", "manufacturer", "AC\x00ME"},
		{"invalid UTF-8 in manufacturer", "manufacturer", "AC\xffME"},
		{"NUL in cpumodel", "cpumodel", "Xe\x00on"},
		{"invalid UTF-8 in chip", "nic1_chip", "i\xc3gc"},
	}
	g := newRig(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := g.discover(discoverForm(tc.key, tc.value))

			wantStatus(t, rec, http.StatusBadRequest)
		})
	}
	if n := g.reportCount(); n != 0 {
		t.Errorf("reports = %d, want 0", n)
	}
	if n := g.count("SELECT count(*) FROM machines"); n != 0 {
		t.Errorf("machines = %d, want 0: validation runs before any I/O", n)
	}
}

func TestDiscover_IdentityMatchesBoot(t *testing.T) {
	g := newRig(t)
	g.boot(bootQ)

	// The same serial as /boot's "S1", with the NUL and invalid UTF-8 /boot drops.
	rec := g.discover(discoverForm("serial", "S\x001\xff"))

	wantStatus(t, rec, http.StatusOK)
	if n := g.count("SELECT count(*) FROM machines"); n != 1 {
		t.Errorf("machines = %d, want 1: /discover must match the machine /boot created", n)
	}
	if got := g.storedFacts().Serial; got != "S1" {
		t.Errorf("stored serial = %q, want the cleaned S1", got)
	}
}

func TestDiscover_HostileTextStoredVerbatim(t *testing.T) {
	const serial = "<script>alert(1)</script>"
	g := newRig(t)

	rec := g.discover(discoverForm("serial", serial))

	wantStatus(t, rec, http.StatusOK)
	if got := g.storedFacts().Serial; got != serial {
		t.Errorf("stored serial = %q, want %q", got, serial)
	}
}

func TestDiscover_FleetFull(t *testing.T) {
	g := newRig(t)
	rows := make([]fleet.Machine, fleet.MaxMachines)
	for i := range rows {
		rows[i] = fleet.Machine{
			ID: fmt.Sprintf("m%04d", i), UUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1000),
			Serial: "S", Product: "P", Arch: fleet.ArchAMD64, State: fleet.StateDiscovered, CreatedAt: testNow(), LastSeenAt: testNow(),
		}
	}
	if _, err := g.d.Bun().NewInsert().Model(&rows).Exec(t.Context()); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}

	rec := g.discover(discoverForm())

	wantStatus(t, rec, http.StatusServiceUnavailable)
	if got := rec.Header().Get("Retry-After"); got != "600" {
		t.Errorf("Retry-After = %q, want 600", got)
	}
}

func TestDiscover_UnidentifiableIs400(t *testing.T) {
	g := newRig(t)

	// A junk-pattern UUID and no MAC cannot key a machine.
	rec := g.discover(discoverForm("uuid", zeroUUID, "mac", ""))

	wantStatus(t, rec, http.StatusBadRequest)
	if n := g.count("SELECT count(*) FROM machines"); n != 0 {
		t.Errorf("machines = %d, want 0", n)
	}
}

func TestDiscover_OffSubnetRefused(t *testing.T) {
	g := newRig(t)
	req := newDiscoverRequest(t, discoverForm().Encode())
	req.RemoteAddr = "10.10.30.5:1"
	rec := httptest.NewRecorder()

	g.h.TLS().ServeHTTP(rec, req)

	wantStatus(t, rec, http.StatusForbidden)
	if n := g.count("SELECT count(*) FROM machines"); n != 0 {
		t.Errorf("machines = %d, want 0", n)
	}
}

func TestDiscover_NotOnPlainHTTP(t *testing.T) {
	g := newRig(t)
	req := newDiscoverRequest(t, discoverForm().Encode())
	req.TLS = nil
	rec := httptest.NewRecorder()

	g.h.PlainHTTP().ServeHTTP(rec, req)

	wantStatus(t, rec, http.StatusNotFound)
	if n := g.count("SELECT count(*) FROM machines"); n != 0 {
		t.Errorf("machines = %d, want 0", n)
	}
}

func TestDiscover_WideEvent(t *testing.T) {
	g := newRig(t)

	_, line := g.discoverWide(discoverForm())

	m := g.machine()
	if line["outcome"] != "stored" || line["machine_id"] != m.ID {
		t.Errorf("log line = %v, want outcome stored and machine_id %s", line, m.ID)
	}

	_, line = g.discoverWide(discoverForm())

	if line["outcome"] != "unchanged" {
		t.Errorf("log line = %v, want outcome unchanged", line)
	}
}

// wantMetric requires a line of the exposition body with the metric name, the
// label and the value.
func wantMetric(t *testing.T, body, name, label, value string) {
	t.Helper()
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, name+"{") && strings.Contains(line, label) && strings.HasSuffix(line, " "+value) {
			return
		}
	}
	t.Errorf("metrics lack %s{%s} %s:\n%s", name, label, value, body)
}

func TestDiscover_MetricsAndAlertCountedOnce(t *testing.T) {
	metrics := newMetrics(t)
	g := newRig(t, WithMeter(metrics.Meter()))
	g.boot(bootQ)

	g.discover(discoverForm("serial", "S2"))
	g.discover(discoverForm("serial", "S2"))
	g.discover(discoverForm("serial", "S2", "mac", "zz"))

	body := scrapeMetrics(t, metrics)
	wantMetric(t, body, "hephaestus_alerts_total", `kind="uuid_conflict"`, "1")
	wantMetric(t, body, "hephaestus_discovery_reports_total", `outcome="conflict"`, "2")
	wantMetric(t, body, "hephaestus_discovery_reports_total", `outcome="invalid"`, "1")
}

func TestDiscover_MetricsStoredUnchangedRateLimited(t *testing.T) {
	metrics := newMetrics(t)
	g := newRig(t, WithMeter(metrics.Meter()), WithLimiter(1, discoverWindow, limiterCapacity))

	g.discover(discoverForm())
	g.discover(discoverForm())
	g.discover(discoverForm("memsize", "1"))

	body := scrapeMetrics(t, metrics)
	for _, outcome := range []string{"stored", "unchanged", "rate_limited"} {
		wantMetric(t, body, "hephaestus_discovery_reports_total", fmt.Sprintf("outcome=%q", outcome), "1")
	}
}

// discoverParseCase is one TestParseDiscoverForm case.
type discoverParseCase struct {
	name    string
	form    url.Values
	want    discoverInput
	wantErr error
}

func discoverParseCases() []discoverParseCase {
	wantFacts := facts{
		UUID: bootUUID, MAC: bootMAC, Serial: "S1", Manufacturer: "ACME", Product: "P1",
		CPUVendor: "GenuineIntel", CPUModel: "Xeon", MemSize: "17179869184", Platform: "efi", Arch: "x86_64",
		NICs: []nic{{0, bootMAC, "r8169"}, {1, "aa:bb:cc:00:00:02", "igc"}},
	}
	full := discoverInput{
		Identity: fleet.Identity{UUID: bootUUID, MAC: bootMAC, Serial: "S1", Product: "P1"},
		Facts:    wantFacts, MACs: []string{bootMAC, "aa:bb:cc:00:00:02"},
	}
	emptyNIC := full
	emptyNIC.Facts.NICs = []nic{{1, "aa:bb:cc:00:00:02", "igc"}}
	emptyNIC.MACs = []string{"aa:bb:cc:00:00:02", bootMAC}
	zeroMem := full
	zeroMem.Facts.MemSize = "unknown"
	macOnly := full
	macOnly.Identity.UUID, macOnly.Facts.UUID = "", ""
	return []discoverParseCase{
		{"fixture", discoverForm(), full, nil},
		{"normalized", discoverForm("uuid", strings.ToUpper(bootUUID), "mac", "AA-BB-CC-00-00-01", "nic0_mac", "AA-BB-CC-00-00-01"), full, nil},
		{"empty nic MAC dropped, mac kept", discoverForm("nic0_mac", "", "nic1_mac", "aa:bb:cc:00:00:02"), emptyNIC, nil},
		{"memsize 0", discoverForm("memsize", "0"), zeroMem, nil},
		{"NUL and invalid UTF-8 dropped from serial", discoverForm("serial", "S\x001\xff"), full, nil},
		{"mac only", discoverForm("uuid", ""), macOnly, nil},
		{"nic16 is not read", discoverForm("nic16_mac", "aa:bb:cc:00:00:10"), full, nil},
		{"invalid nic MAC", discoverForm("nic0_mac", "zz"), discoverInput{}, errInvalidMAC},
		{"invalid mac", discoverForm("mac", "zz"), discoverInput{}, errInvalidMAC},
		{"no identity", discoverForm("uuid", "x", "mac", ""), discoverInput{}, errNoIdentity},
		{"long serial", discoverForm("serial", strings.Repeat("a", maxValueBytes+1)), discoverInput{}, errValueTooLong},
		{"long unknown key", discoverForm("whatever", strings.Repeat("a", maxValueBytes+1)), discoverInput{}, errValueTooLong},
		{"NUL in manufacturer", discoverForm("manufacturer", "AC\x00ME"), discoverInput{}, errInvalidText},
		{"invalid UTF-8 in cpumodel", discoverForm("cpumodel", "\xff"), discoverInput{}, errInvalidText},
	}
}

func TestParseDiscoverForm(t *testing.T) {
	for _, tc := range discoverParseCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDiscoverForm(tc.form)

			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("parseDiscoverForm error = %v, want %v", err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseDiscoverForm = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func FuzzParseDiscoverForm(f *testing.F) {
	for _, body := range discoverBodies() {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		form, err := url.ParseQuery(raw)
		if err != nil {
			t.Skip()
		}
		got, err := parseDiscoverForm(form)
		if err != nil {
			return
		}
		strs := []string{
			got.Facts.UUID, got.Facts.MAC, got.Facts.Serial, got.Facts.Manufacturer, got.Facts.Product, got.Facts.CPUVendor,
			got.Facts.CPUModel, got.Facts.MemSize, got.Facts.Platform, got.Facts.Arch,
		}
		for _, n := range got.Facts.NICs {
			strs = append(strs, n.MAC, n.Chip)
		}
		for _, s := range strs {
			if len(s) > maxValueBytes || !utf8.ValidString(s) || strings.ContainsRune(s, 0) {
				t.Errorf("fact %q, want valid text of at most %d bytes without NUL", s, maxValueBytes)
			}
		}
		if len(got.Facts.NICs) > maxNICs {
			t.Errorf("NICs = %d, want at most %d", len(got.Facts.NICs), maxNICs)
		}
		for _, mac := range got.MACs {
			if len(mac) != 17 {
				t.Errorf("MAC %q has %d bytes, want 17", mac, len(mac))
			}
		}
		if got.Identity.UUID == "" && got.Identity.MAC == "" {
			t.Errorf("identity %+v has neither UUID nor MAC", got.Identity)
		}
	})
}
