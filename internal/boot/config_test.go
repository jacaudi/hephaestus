package boot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"go.jacaudi.dev/hephaestus/internal/clusters"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/telemetry"
)

const (
	// configYAML stands in for a rendered machine config.
	configYAML = "version: v1alpha1\nmachine:\n  token: hunter2-machine-token\n"
	otherUUID  = "4c4c4544-0042-3510-8052-b4c04f4e4d33"
	offSubnet  = "10.10.30.5:1"
	// secretInRenderError is what a failed render's error quotes.
	secretInRenderError = "hunter2"
)

// errDBDown is a recording failure the database causes.
var errDBDown = errors.New("database is down")

// configQ is the /config target for uuid and token.
func configQ(uuid, token string) string { return "/config?u=" + uuid + "&t=" + token }

// lockedClock is a settable clock the recorder's goroutine can read.
type lockedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *lockedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *lockedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// configRig is the fixture machine assigned to the fixture host of cluster
// lab, whose approved pair is pair-1 in the config source and the database,
// with a live ticket for it.
type configRig struct {
	*rig

	src     *fakeConfigs
	clock   *lockedClock
	metrics *telemetry.Metrics
	token   string
	ticket  fleet.Ticket
}

func newConfigRig(t *testing.T) *configRig {
	t.Helper()
	metrics := newMetrics(t)
	src := newFakeConfigs()
	src.render = []byte(configYAML)
	clock := &lockedClock{t: testNow()}
	g := newInstallRig(t, src, WithMeter(metrics.Meter()), WithClock(clock.now))
	g.assign()
	const stamp = "'2026-11-14T12:00:00Z'"
	g.exec("INSERT INTO clusters (id, name, repo_url, ref, subpath, approved_pair_id, created_at, updated_at) "+
		"VALUES (?, 'lab', 'https://git.example.test/lab.git', 'main', '.', 'pair-1', "+stamp+", "+stamp+")", testCluster)
	token, tk, err := g.repo.MintTicket(t.Context(), fleet.MintRequest{
		MachineID: g.machine().ID, ClusterID: testCluster, HostName: testHost, PairID: "pair-1", SecretVersionID: "version-1",
		TalosSchematic: plainSchematic, TalosVersion: talosVersion, Arch: fleet.ArchAMD64, At: testNow(),
	})
	if err != nil {
		t.Fatalf("MintTicket: %v", err)
	}
	return &configRig{rig: g, src: src, clock: clock, metrics: metrics, token: token, ticket: tk}
}

// config serves a TLS GET of target.
func (c *configRig) config(target string) *httptest.ResponseRecorder {
	c.t.Helper()
	return c.secure(target)
}

// fetch is a TLS GET of the fixture machine's config with its ticket.
func (c *configRig) fetch() *httptest.ResponseRecorder {
	c.t.Helper()
	return c.config(configQ(bootUUID, c.token))
}

// consumed reports whether the fixture ticket is consumed.
func (c *configRig) consumed() bool {
	c.t.Helper()
	return c.count("SELECT count(*) FROM tickets WHERE id = ? AND consumed_at IS NOT NULL", c.ticket.ID) == 1
}

// audits counts the audit events with action.
func (c *configRig) audits(action string) int {
	c.t.Helper()
	return c.count("SELECT count(*) FROM audit_events WHERE action = ?", action)
}

// pending is how many recordings wait for a retry.
func (c *configRig) pending() int {
	c.h.rec.mu.Lock()
	defer c.h.rec.mu.Unlock()
	return len(c.h.rec.entries)
}

// runRecorder runs the recorder until the returned stop is called; stop waits
// for it to return and may be called more than once.
func (c *configRig) runRecorder() (stop func()) {
	ctx, cancel := context.WithCancel(c.t.Context())
	done := make(chan error, 1)
	go func() { done <- c.h.RunRecorder(ctx) }()
	return sync.OnceFunc(func() {
		cancel()
		if err := <-done; err != nil {
			c.t.Errorf("RunRecorder = %v, want nil", err)
		}
	})
}

// scriptedRecord stands in for RecordConfigServed: call n, counted from 0,
// answers with answer(n). It is safe for the recorder's goroutine.
type scriptedRecord struct {
	mu     sync.Mutex
	calls  int
	answer func(ctx context.Context, req fleet.RecordConfigRequest, n int) (fleet.RecordConfigResult, error)
}

func (s *scriptedRecord) record(ctx context.Context, req fleet.RecordConfigRequest) (fleet.RecordConfigResult, error) {
	s.mu.Lock()
	n := s.calls
	s.calls++
	s.mu.Unlock()
	return s.answer(ctx, req, n)
}

func (s *scriptedRecord) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// failing is a recording that always fails with errDBDown.
func failing(context.Context, fleet.RecordConfigRequest, int) (fleet.RecordConfigResult, error) {
	return fleet.RecordConfigResult{}, errDBDown
}

// eventually waits up to 5 seconds for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// reported returns the reported errors whose text contains s.
func reported(errs []error, s string) []error {
	var out []error
	for _, err := range errs {
		if strings.Contains(err.Error(), s) {
			out = append(out, err)
		}
	}
	return out
}

func TestConfig_Served(t *testing.T) {
	c := newConfigRig(t)

	rec, line := c.wide(configQ(bootUUID, c.token))

	wantStatus(t, rec, http.StatusOK)
	if got := rec.Body.String(); got != configYAML {
		t.Errorf("body = %q, want the render %q", got, configYAML)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/yaml" {
		t.Errorf("Content-Type = %q, want application/yaml", got)
	}
	if !rec.Flushed {
		t.Error("the body was not flushed")
	}
	if !c.consumed() {
		t.Error("the ticket is not consumed")
	}
	if m := c.machine(); m.State != fleet.StateInstalling {
		t.Errorf("state = %s, want installing", m.State)
	}
	if n := c.audits("config.served"); n != 1 {
		t.Errorf("config.served audits = %d, want 1", n)
	}
	if n := c.audits("config.older_pair"); n != 0 {
		t.Errorf("config.older_pair audits = %d, want 0", n)
	}
	wantMetric(t, scrapeMetrics(t, c.metrics), "hephaestus_config_requests_total", `outcome="served"`, "1")
	if line["outcome"] != "served" || line["machine_id"] != c.machine().ID {
		t.Errorf("log line = %v, want outcome served and the machine id", line)
	}
	if text := fmt.Sprint(line); strings.Contains(text, c.token) || strings.Contains(text, c.ticket.ID) || strings.Contains(text, secretInRenderError) {
		t.Errorf("log line carries the ticket or the config: %v", line)
	}
	if len(c.errs) != 0 {
		t.Errorf("reported errors = %v, want none", c.errs)
	}
}

func TestConfig_Refusals(t *testing.T) {
	cases := []struct {
		name string
		// setup changes the rig before the request; do sends it (default fetch).
		setup  func(c *configRig)
		do     func(c *configRig) *httptest.ResponseRecorder
		reason string
	}{
		{name: "plain HTTP (no route)", do: func(c *configRig) *httptest.ResponseRecorder {
			return c.plain(configQ(bootUUID, c.token))
		}},
		{name: "off-subnet", reason: "off_subnet", do: func(c *configRig) *httptest.ResponseRecorder {
			req := newRequest(c.t, configQ(bootUUID, c.token), true)
			req.RemoteAddr = offSubnet
			rec := httptest.NewRecorder()
			c.h.TLS().ServeHTTP(rec, req)
			return rec
		}},
		{name: "missing t", reason: "bad_query", do: func(c *configRig) *httptest.ResponseRecorder {
			return c.config("/config?u=" + bootUUID)
		}},
		{name: "missing u", reason: "bad_query", do: func(c *configRig) *httptest.ResponseRecorder {
			return c.config("/config?t=" + c.token)
		}},
		{name: "oversize t", reason: "bad_query", do: func(c *configRig) *httptest.ResponseRecorder {
			return c.config(configQ(bootUUID, strings.Repeat("A", maxValueBytes+1)))
		}},
		{name: "forged t", reason: "unknown_ticket", do: func(c *configRig) *httptest.ResponseRecorder {
			return c.config(configQ(bootUUID, strings.Repeat("A", ticketChars)))
		}},
		{name: "stored hash as t", reason: "unknown_ticket", do: func(c *configRig) *httptest.ResponseRecorder {
			return c.config(configQ(bootUUID, c.ticket.TokenSHA256))
		}},
		{name: "expired", reason: "invalid_ticket", setup: func(c *configRig) { c.clock.advance(fleet.TicketTTL) }},
		{name: "reused (consumed)", reason: "invalid_ticket", setup: func(c *configRig) {
			c.exec("UPDATE tickets SET consumed_at = minted_at WHERE id = ?", c.ticket.ID)
		}},
		{name: "revoked", reason: "invalid_ticket", setup: func(c *configRig) {
			c.exec("UPDATE tickets SET revoked_at = minted_at WHERE id = ?", c.ticket.ID)
		}},
		{name: "other UUID", reason: "other_machine", do: func(c *configRig) *httptest.ResponseRecorder {
			return c.config(configQ(otherUUID, c.token))
		}},
		{name: "unassigned machine", reason: "not_assigned", setup: func(c *configRig) { c.setState(fleet.StateDiscovered) }},
		{name: "junk machine", reason: "junk", setup: func(c *configRig) {
			c.exec("UPDATE machines SET junk = ? WHERE smbios_uuid = ?", true, bootUUID)
		}},
		{name: "other host", reason: "other_host", setup: func(c *configRig) {
			c.exec("UPDATE machines SET host_name = 'wrk-01' WHERE smbios_uuid = ?", bootUUID)
		}},
		{name: "stale pair", reason: "stale_pair", setup: func(c *configRig) {
			c.src.pair = clusters.Pair{ID: "pair-2", ClusterID: testCluster, SecretVersionID: "version-2", TalosVersion: talosVersion}
		}},
		{name: "no approved pair", reason: "stale_pair", setup: func(c *configRig) { c.src.pair.ClusterID = "another" }},
		{name: "drift", reason: "drift", setup: func(c *configRig) {
			c.src.render, c.src.renderErr = nil, fmt.Errorf("render lab/cp-01: %w", clusters.ErrDrift)
		}},
		{name: "cache miss", reason: "no_render", setup: func(c *configRig) {
			c.src.render, c.src.renderErr = nil, clusters.ErrNoRender
		}},
		{name: "render error", reason: "render_error", setup: func(c *configRig) {
			c.src.render, c.src.renderErr = nil, errors.New("render: patch quotes "+secretInRenderError)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newConfigRig(t)
			if tc.setup != nil {
				tc.setup(c)
			}
			do := tc.do
			if do == nil {
				do = (*configRig).fetch
			}

			rec := do(c)

			wantStatus(t, rec, http.StatusNotFound)
			if strings.Contains(rec.Body.String(), "version:") {
				t.Errorf("body = %q, want no config", rec.Body.String())
			}
			// The reused case consumed it at minted_at in setup.
			if n := c.count("SELECT count(*) FROM tickets WHERE id = ? AND (consumed_at IS NULL OR consumed_at = minted_at)", c.ticket.ID); n != 1 {
				t.Error("the request consumed the ticket, want it left alone")
			}
			if c.machine().State == fleet.StateInstalling {
				t.Error("the machine is installing, want it left alone")
			}
			if n := c.audits("config.served"); n != 0 {
				t.Errorf("config.served audits = %d, want 0", n)
			}
			body := scrapeMetrics(t, c.metrics)
			if tc.reason == "" {
				if strings.Contains(body, "hephaestus_config_requests_total") {
					t.Errorf("a request with no route was counted:\n%s", body)
				}
			} else {
				wantMetric(t, body, "hephaestus_config_requests_total", fmt.Sprintf("outcome=%q", "refused_"+tc.reason), "1")
			}
			if leaked := reported(c.errs, secretInRenderError); len(leaked) != 0 {
				t.Errorf("reported errors quote the render error: %v", leaked)
			}
		})
	}
}

// TestConfig_RenderErrorReportedByStage pins that a failed render is reported
// with the machine and the stage only, never the render's own text.
func TestConfig_RenderErrorReportedByStage(t *testing.T) {
	c := newConfigRig(t)
	c.src.render, c.src.renderErr = nil, errors.New("render: patch quotes "+secretInRenderError)

	rec, line := c.wide(configQ(bootUUID, c.token))

	wantStatus(t, rec, http.StatusNotFound)
	if len(c.errs) != 1 || !strings.Contains(c.errs[0].Error(), "render") || !strings.Contains(c.errs[0].Error(), c.machine().ID) {
		t.Fatalf("reported errors = %v, want one naming the render stage and the machine", c.errs)
	}
	if strings.Contains(c.errs[0].Error(), secretInRenderError) || errors.Is(c.errs[0], c.src.renderErr) {
		t.Errorf("reported error %q carries the render error", c.errs[0])
	}
	if strings.Contains(fmt.Sprint(line), secretInRenderError) {
		t.Errorf("log line carries the render error: %v", line)
	}
}

func TestConfig_OffSubnetIs404Not403(t *testing.T) {
	c := newConfigRig(t)
	trusted := []netip.Prefix{netip.MustParsePrefix("10.10.99.0/24")}
	send := func(h http.Handler, target, peer, forwarded string) *httptest.ResponseRecorder {
		req := newRequest(t, target, true)
		req.RemoteAddr = peer
		if forwarded != "" {
			req.Header.Set("X-Forwarded-For", forwarded)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	q := configQ(bootUUID, c.token)

	cases := []struct {
		name string
		rec  *httptest.ResponseRecorder
		want int
	}{
		{"TLS /config off-subnet", send(c.h.TLS(), q, offSubnet, ""), http.StatusNotFound},
		{"TLS /config/ off-subnet", send(c.h.TLS(), "/config/", offSubnet, ""), http.StatusForbidden},
		{"TLS /boot off-subnet", send(c.h.TLS(), "/boot?"+bootQ, offSubnet, ""), http.StatusForbidden},
		{"proxied /config off-subnet client", send(c.h.Proxied(trusted), q, "10.10.99.2:1", "10.10.30.5"), http.StatusNotFound},
		{"proxied /config untrusted peer", send(c.h.Proxied(trusted), q, "10.10.98.2:1", testAddress), http.StatusForbidden},
		{"proxied /config no X-Forwarded-For", send(c.h.Proxied(trusted), q, "10.10.99.2:1", ""), http.StatusForbidden},
	}
	for _, tc := range cases {
		if tc.rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, tc.rec.Code, tc.want)
		}
	}
	if c.consumed() {
		t.Fatal("an off-subnet request consumed the ticket")
	}

	rec := send(c.h.Proxied(trusted), q, "10.10.99.2:1", "10.10.20.50")
	wantStatus(t, rec, http.StatusOK)
	if !c.consumed() {
		t.Error("the proxied on-subnet request did not consume the ticket")
	}
}

func TestConfig_RendersTicketPair(t *testing.T) {
	c := newConfigRig(t)
	c.exec("UPDATE clusters SET pending_pair_id = 'pair-2' WHERE id = ?", testCluster)

	wantStatus(t, c.fetch(), http.StatusOK)

	if len(c.src.rendered) != 1 {
		t.Fatalf("Render calls = %d, want 1", len(c.src.rendered))
	}
	got := c.src.rendered[0]
	if got.ID != c.ticket.ID || got.PairID != "pair-1" || got.SecretVersionID != "version-1" || got.HostName != testHost {
		t.Errorf("Render ticket = %+v, want the fixture ticket with pair-1, version-1 and %s", got, testHost)
	}
}

func TestConfig_ApprovalRaceRecordsOlderPair(t *testing.T) {
	c := newConfigRig(t)
	c.src.onRender = func() {
		// A cluster approval lands after the pair check and before the record.
		c.exec("UPDATE clusters SET approved_pair_id = 'pair-2' WHERE id = ?", testCluster)
		c.src.pair = clusters.Pair{ID: "pair-2", ClusterID: testCluster, SecretVersionID: "version-2", TalosVersion: talosVersion}
	}

	wantStatus(t, c.fetch(), http.StatusOK)

	if !c.consumed() || c.machine().State != fleet.StateInstalling {
		t.Errorf("consumed %t, state %s; want the delivery recorded", c.consumed(), c.machine().State)
	}
	want := `{"ticket_id":"` + c.ticket.ID + `","pair_id":"pair-1","approved_pair_id":"pair-2"}`
	if n := c.count("SELECT count(*) FROM audit_events WHERE action = 'config.older_pair' AND machine_id = ? AND detail = ?", c.machine().ID, want); n != 1 {
		t.Errorf("config.older_pair audits with %s = %d, want 1", want, n)
	}
}

func TestConfig_RecordingRetryStopsOnConsumed(t *testing.T) {
	c := newConfigRig(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	c.h.retryBase = time.Millisecond
	s := &scriptedRecord{answer: func(_ context.Context, _ fleet.RecordConfigRequest, n int) (fleet.RecordConfigResult, error) {
		if n == 0 {
			return fleet.RecordConfigResult{}, errDBDown
		}
		return fleet.RecordConfigResult{AlreadyConsumed: true}, nil
	}}
	c.h.record = s.record
	wantStatus(t, c.fetch(), http.StatusOK)
	stop := c.runRecorder()
	defer stop()

	eventually(t, "the retry", func() bool { return s.count() >= 2 && c.pending() == 0 })
	time.Sleep(20 * time.Millisecond) // room for a retry that should not happen
	stop()

	if n := s.count(); n != 2 {
		t.Errorf("record calls = %d, want 2", n)
	}
	if n := c.audits("alert.config_unrecorded"); n != 0 {
		t.Errorf("alert audits = %d, want 0", n)
	}
	if len(c.errs) != 1 || !errors.Is(c.errs[0], errDBDown) {
		t.Errorf("reported errors = %v, want the first failure only", c.errs)
	}
}

func TestConfig_RecordingRetryStopsOnInstalling(t *testing.T) {
	c := newConfigRig(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	c.h.retryBase = time.Millisecond
	s := &scriptedRecord{answer: func(ctx context.Context, req fleet.RecordConfigRequest, n int) (fleet.RecordConfigResult, error) {
		if n == 0 {
			return fleet.RecordConfigResult{}, errDBDown
		}
		return c.repo.RecordConfigServed(ctx, req)
	}}
	c.h.record = s.record
	wantStatus(t, c.fetch(), http.StatusOK)
	// A newer ticket's delivery was recorded meanwhile.
	c.setState(fleet.StateInstalling)
	stop := c.runRecorder()
	defer stop()

	eventually(t, "the retry", func() bool { return s.count() >= 2 && c.pending() == 0 })
	time.Sleep(20 * time.Millisecond)
	stop()

	if n := s.count(); n != 2 {
		t.Errorf("record calls = %d, want 2", n)
	}
	if c.consumed() {
		t.Error("the older ticket was consumed")
	}
	if n := c.audits("alert.config_unrecorded"); n != 0 {
		t.Errorf("alert audits = %d, want 0", n)
	}
	if len(c.errs) != 1 || !errors.Is(c.errs[0], errDBDown) {
		t.Errorf("reported errors = %v, want the first failure only", c.errs)
	}
}

func TestConfig_RecordingRetryOnePerTicket(t *testing.T) {
	c := newConfigRig(t)
	c.h.record = (&scriptedRecord{answer: failing}).record

	wantStatus(t, c.fetch(), http.StatusOK)
	wantStatus(t, c.fetch(), http.StatusOK)

	if n := c.pending(); n != 1 {
		t.Errorf("pending recordings = %d, want 1 for the one ticket", n)
	}
}

func TestConfig_RecordingRetryAlertsAfterAnHour(t *testing.T) {
	c := newConfigRig(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	c.h.retryBase = time.Millisecond
	s := &scriptedRecord{answer: failing}
	c.h.record = s.record
	wantStatus(t, c.fetch(), http.StatusOK)
	stop := c.runRecorder()
	defer stop()

	eventually(t, "a retry", func() bool { return s.count() >= 2 })
	if n := c.audits("alert.config_unrecorded"); n != 0 {
		t.Fatalf("alert audits before the hour = %d, want 0", n)
	}
	c.clock.advance(time.Hour)
	eventually(t, "the alert", func() bool { return c.pending() == 0 && c.audits("alert.config_unrecorded") == 1 })
	stop()

	m := c.machine()
	want := `{"machine_id":"` + m.ID + `","state":"assigned"}`
	if n := c.count("SELECT count(*) FROM audit_events WHERE action = 'alert.config_unrecorded' AND machine_id = ? AND detail = ?", m.ID, want); n != 1 {
		t.Errorf("alert audits with %s = %d, want 1", want, n)
	}
	wantMetric(t, scrapeMetrics(t, c.metrics), "hephaestus_alerts_total", `kind="config_unrecorded"`, "1")
	if got := reported(c.errs, "not recorded"); len(got) != 1 || !strings.Contains(got[0].Error(), m.ID) || !strings.Contains(got[0].Error(), "assigned") {
		t.Errorf("reported errors = %v, want one naming the machine and its state", c.errs)
	}
	if got := reported(c.errs, "abandoned"); len(got) != 0 {
		t.Errorf("abandoned reports = %v, want none", got)
	}
}

func TestConfig_RecordingAbandonedAtShutdownLogged(t *testing.T) {
	c := newConfigRig(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	c.h.record = (&scriptedRecord{answer: failing}).record
	wantStatus(t, c.fetch(), http.StatusOK)
	stop := c.runRecorder()
	defer stop()

	stop()

	got := reported(c.errs, "config recording abandoned")
	if len(got) != 1 || !strings.Contains(got[0].Error(), c.machine().ID) {
		t.Errorf("reported errors = %v, want one config recording abandoned naming %s", c.errs, c.machine().ID)
	}
}

func TestConfig_FirstFailureCounted(t *testing.T) {
	c := newConfigRig(t)
	c.h.record = (&scriptedRecord{answer: failing}).record

	wantStatus(t, c.fetch(), http.StatusOK)

	if v, ok := metricLine(scrapeMetrics(t, c.metrics), "hephaestus_config_record_failures_total"); !ok || v != "1" {
		t.Errorf("hephaestus_config_record_failures_total = %q (found %t), want 1", v, ok)
	}
	if len(c.errs) != 1 || !errors.Is(c.errs[0], errDBDown) || !strings.Contains(c.errs[0].Error(), c.machine().ID) {
		t.Errorf("reported errors = %v, want the failure naming the machine", c.errs)
	}
	if n := c.pending(); n != 1 {
		t.Errorf("pending recordings = %d, want 1", n)
	}
}

func TestConfig_RefusalAuditOncePerHour(t *testing.T) {
	c := newConfigRig(t)
	refused := func() int { return c.audits("config.refused") }
	forged := configQ(bootUUID, strings.Repeat("A", ticketChars))

	// A known host: refusals of any reason share its hour.
	c.src.pair = clusters.Pair{ID: "pair-2", ClusterID: testCluster, SecretVersionID: "version-2"}
	wantStatus(t, c.fetch(), http.StatusNotFound)
	c.clock.advance(30 * time.Minute)
	c.src.pair = newFakeConfigs().pair
	c.src.render, c.src.renderErr = nil, clusters.ErrDrift
	wantStatus(t, c.fetch(), http.StatusNotFound)
	if n := refused(); n != 1 {
		t.Fatalf("audits after two refusals of one host within the hour = %d, want 1", n)
	}
	want := `{"reason":"stale_pair","cluster_id":"lab","host":"cp-01","client":"10.10.20.50"}`
	if n := c.count("SELECT count(*) FROM audit_events WHERE action = 'config.refused' AND machine_id = ? AND detail = ?", c.machine().ID, want); n != 1 {
		t.Errorf("config.refused audits with %s for the machine = %d, want 1", want, n)
	}

	// No host yet: each reason has its own hour.
	wantStatus(t, c.config(forged), http.StatusNotFound)
	wantStatus(t, c.config(forged), http.StatusNotFound)
	wantStatus(t, c.config("/config?u="+bootUUID), http.StatusNotFound)
	if n := refused(); n != 3 {
		t.Fatalf("audits after two forged tickets and a bad query = %d, want 3", n)
	}

	// An hour after the host's audit, 31 minutes after the forged ticket's.
	c.clock.advance(31 * time.Minute)
	wantStatus(t, c.fetch(), http.StatusNotFound)
	wantStatus(t, c.config(forged), http.StatusNotFound)
	if n := refused(); n != 4 {
		t.Errorf("audits an hour after the host's first = %d, want 4", n)
	}
	c.clock.advance(29 * time.Minute)
	wantStatus(t, c.config(forged), http.StatusNotFound)
	if n := refused(); n != 5 {
		t.Errorf("audits an hour after the forged ticket's first = %d, want 5", n)
	}
	if c.consumed() {
		t.Error("a refusal consumed the ticket")
	}
}

// TestConfig_JunkRefused pins that a machine whose UUID was found cloned gets
// no config with the ticket it held.
func TestConfig_JunkRefused(t *testing.T) {
	c := newConfigRig(t)
	// Another machine boots with the same UUID and another serial.
	c.boot(queryWith("S2", "P1"))
	if !c.machine().Junk {
		t.Fatal("the machine was not marked junk")
	}

	wantStatus(t, c.fetch(), http.StatusNotFound)

	if len(c.src.rendered) != 0 {
		t.Errorf("Render calls = %d, want none", len(c.src.rendered))
	}
	if c.consumed() || c.machine().State == fleet.StateInstalling {
		t.Error("the junk machine's delivery was recorded")
	}
}

// TestConfig_RecordedAfterClientLeaves pins that a delivery is recorded even
// when the client's request ends right after the body left.
func TestConfig_RecordedAfterClientLeaves(t *testing.T) {
	c := newConfigRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	c.src.onRender = cancel
	req := newRequest(t, configQ(bootUUID, c.token), true).WithContext(ctx)
	rec := httptest.NewRecorder()

	c.h.TLS().ServeHTTP(rec, req)

	wantStatus(t, rec, http.StatusOK)
	if !c.consumed() || c.machine().State != fleet.StateInstalling {
		t.Errorf("consumed %t, state %s; want the delivery recorded", c.consumed(), c.machine().State)
	}
	if len(c.errs) != 0 {
		t.Errorf("reported errors = %v, want none", c.errs)
	}
}

// failingWriter is a ResponseWriter whose body writes fail, as to a peer that
// has gone.
type failingWriter struct{ *httptest.ResponseRecorder }

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestConfig_WriteFailureNotRecorded pins that a body that did not leave in
// full is not recorded: the ticket stays live for Talos's next try.
func TestConfig_WriteFailureNotRecorded(t *testing.T) {
	c := newConfigRig(t)

	c.h.TLS().ServeHTTP(failingWriter{httptest.NewRecorder()}, newRequest(t, configQ(bootUUID, c.token), true))

	if c.consumed() || c.machine().State != fleet.StateAssigned {
		t.Errorf("consumed %t, state %s; want the ticket live and the machine assigned", c.consumed(), c.machine().State)
	}
	if n := c.audits("config.served"); n != 0 {
		t.Errorf("config.served audits = %d, want 0", n)
	}
}

// TestConfig_RecordingRetryRecordsRevokedTicket pins that a delivered config
// is recorded by the retry even when its ticket was revoked meanwhile: the
// record follows the delivery.
func TestConfig_RecordingRetryRecordsRevokedTicket(t *testing.T) {
	c := newConfigRig(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	c.h.retryBase = time.Millisecond
	s := &scriptedRecord{answer: func(ctx context.Context, req fleet.RecordConfigRequest, n int) (fleet.RecordConfigResult, error) {
		if n == 0 {
			return fleet.RecordConfigResult{}, errDBDown
		}
		return c.repo.RecordConfigServed(ctx, req)
	}}
	c.h.record = s.record
	wantStatus(t, c.fetch(), http.StatusOK)
	// The machine boots again before the retry: a new ticket revokes the old one.
	if _, _, err := c.repo.MintTicket(t.Context(), fleet.MintRequest{
		MachineID: c.machine().ID, ClusterID: testCluster, HostName: testHost, PairID: "pair-1", SecretVersionID: "version-1",
		TalosSchematic: plainSchematic, TalosVersion: talosVersion, Arch: fleet.ArchAMD64, At: testNow(),
	}); err != nil {
		t.Fatalf("MintTicket: %v", err)
	}
	stop := c.runRecorder()
	defer stop()

	eventually(t, "the retry", func() bool { return c.pending() == 0 })
	stop()

	if !c.consumed() || c.machine().State != fleet.StateInstalling {
		t.Errorf("consumed %t, state %s; want the revoked ticket's delivery recorded", c.consumed(), c.machine().State)
	}
	if n := c.audits("alert.config_unrecorded"); n != 0 {
		t.Errorf("alert audits = %d, want 0", n)
	}
}
