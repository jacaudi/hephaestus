package boot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"go.jacaudi.dev/hephaestus/internal/boot/ipxe"
	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/store"
	"go.jacaudi.dev/hephaestus/internal/telemetry"
)

const (
	// maxValueBytes is the longest a request value may be. Real UUIDs, MACs and
	// SMBIOS strings are far shorter; longer ones are hostile or broken.
	maxValueBytes = 256

	// scriptDeadline bounds writing an iPXE script; idleWrite bounds each write of
	// a binary, so a slow client may take as long as it keeps making progress.
	scriptDeadline = 10 * time.Second
	idleWrite      = 30 * time.Second

	contentTypeScript = "text/plain; charset=utf-8"
	contentTypeBinary = "application/octet-stream"

	alertKindUUIDConflict = "uuid_conflict"
)

// errValueTooLong is returned by parseBootQuery and parseDiscoverForm for a
// value over maxValueBytes, wrapped with the key it came from.
var errValueTooLong = errors.New("boot: value too long")

// ErrBootNextRace is reported to the error handler when /boot found a machine's
// boot-next changed by another process on two decisions in a row. The boot is
// answered by rows 4 to 10 and the flag is left as it is.
var ErrBootNextRace = errors.New("boot: boot-next changed twice while deciding; not cleared")

// ErrUtilityMissing is reported to the error handler when a utility whose cache
// row is ready has lost content in the store. The row is put back to wanted.
var ErrUtilityMissing = errors.New("boot: ready utility content is missing from the store; fetch re-queued")

// Config is what Handlers serves from: the fleet and the utility content, the
// service's name and address, and the iPXE binaries.
type Config struct {
	Repo *fleet.Repository
	// Store holds the utility content and Catalog says which entries exist and
	// how each boots.
	Store   *store.Store
	Catalog *catalog.Catalog
	// FQDN names the service in the scripts. Address is its address on the
	// provisioning subnet, the host of the utility scripts' URLs; only clients in
	// Subnet are served.
	FQDN    string
	Address netip.Addr
	Subnet  netip.Prefix
	// Files holds the iPXE binaries by the names /tftp serves them under, such as
	// "amd64/snp.efi".
	Files map[string][]byte
	// Configs answers rows 8 to 10 for an assigned machine: drift, the creation
	// of its Talos row and the approved pair a ticket binds. When nil, an assigned
	// machine holds at row 8.
	Configs ConfigSource
}

// Handlers serves the boot routes: the iPXE chain scripts over plain HTTP, the
// iPXE binaries over plain HTTP and TLS, and /boot and /discover over TLS.
type Handlers struct {
	repo    *fleet.Repository
	content *store.Store
	catalog *catalog.Catalog
	fqdn    string
	address netip.Addr
	subnet  netip.Prefix
	files   map[string][]byte
	configs ConfigSource
	now     func() time.Time
	meter   metric.Meter
	onError func(context.Context, error)

	limit             limiterConfig
	limiter           *windowLimiter
	decisions, alerts metric.Int64Counter
	reports, boots    metric.Int64Counter
	minted            metric.Int64Counter
}

// limiterConfig is what WithLimiter sets and NewHandlers builds the limiter from.
type limiterConfig struct {
	limit    int
	window   time.Duration
	capacity int
}

// Option configures Handlers.
type Option func(*Handlers)

// WithClock sets the clock that stamps recorded boots and /discover reports and
// drives the /discover limiter windows; the default is time.Now. A frozen clock
// never expires a window.
func WithClock(now func() time.Time) Option {
	return func(h *Handlers) { h.now = now }
}

// WithMeter sets the meter the boot counters are created on; the default is a
// no-op meter.
func WithMeter(m metric.Meter) Option {
	return func(h *Handlers) { h.meter = m }
}

// WithLimiter sets how many new machines or changed reports one source address
// may submit to /discover per window, and how many source addresses are
// tracked. The defaults are discoverPerSource per discoverWindow over
// limiterCapacity addresses; tests shrink them.
func WithLimiter(limit int, window time.Duration, capacity int) Option {
	return func(h *Handlers) { h.limit = limiterConfig{limit: limit, window: window, capacity: capacity} }
}

// WithErrorHandler sets the function that is told of failures a request
// survives, such as ErrBootNextRace and ErrUtilityMissing; the default does
// nothing. The package never logs, so the caller decides what these are worth.
func WithErrorHandler(fn func(context.Context, error)) Option {
	return func(h *Handlers) { h.onError = fn }
}

// NewHandlers returns the boot routes for the service described by cfg. It
// returns an error for a nil Repo, Store or Catalog and for a nil error
// handler, so that a misconfiguration stops startup instead of panicking on the
// first boot that needs one.
func NewHandlers(cfg Config, opts ...Option) (*Handlers, error) {
	h := &Handlers{
		repo: cfg.Repo, content: cfg.Store, catalog: cfg.Catalog,
		fqdn: cfg.FQDN, address: cfg.Address, subnet: cfg.Subnet, files: cfg.Files, configs: cfg.Configs,
		now: time.Now, meter: noop.NewMeterProvider().Meter(""),
		onError: func(context.Context, error) {},
		limit:   limiterConfig{limit: discoverPerSource, window: discoverWindow, capacity: limiterCapacity},
	}
	for _, opt := range opts {
		opt(h)
	}
	if err := h.validate(); err != nil {
		return nil, err
	}
	h.limiter = newWindowLimiter(h.limit.limit, h.limit.window, h.limit.capacity, h.now)
	var err error
	h.decisions, err = h.meter.Int64Counter("hephaestus.boot.decisions",
		metric.WithDescription("Boot decisions served, by decision-table row and action."))
	if err != nil {
		return nil, fmt.Errorf("boot: decisions counter: %w", err)
	}
	h.alerts, err = h.meter.Int64Counter("hephaestus.alerts",
		metric.WithDescription("Operator alerts raised, by kind."))
	if err != nil {
		return nil, fmt.Errorf("boot: alerts counter: %w", err)
	}
	h.reports, err = h.meter.Int64Counter("hephaestus.discovery.reports",
		metric.WithDescription("Inventory reports received on /discover, by outcome."))
	if err != nil {
		return nil, fmt.Errorf("boot: discovery reports counter: %w", err)
	}
	h.boots, err = h.meter.Int64Counter("hephaestus.utility.boots",
		metric.WithDescription("Utility OS boots served, by catalog entry."))
	if err != nil {
		return nil, fmt.Errorf("boot: utility boots counter: %w", err)
	}
	h.minted, err = h.meter.Int64Counter("hephaestus.tickets.minted",
		metric.WithDescription("Config tickets minted for Talos installs."))
	if err != nil {
		return nil, fmt.Errorf("boot: tickets minted counter: %w", err)
	}
	return h, nil
}

// validate reports every dependency h lacks.
func (h *Handlers) validate() error {
	var errs []error
	for _, d := range []struct {
		name    string
		missing bool
	}{
		{"Config.Repo", h.repo == nil},
		{"Config.Store", h.content == nil},
		{"Config.Catalog", h.catalog == nil},
		{"the error handler (WithErrorHandler)", h.onError == nil},
	} {
		if d.missing {
			errs = append(errs, fmt.Errorf("boot: %s is nil", d.name))
		}
	}
	return errors.Join(errs...)
}

// PlainHTTP returns the handler for the plain-HTTP listener: /start, /boot
// (which only chains to TLS), /tftp, the utility artifacts under /u/ and the
// Talos artifacts under /t/, behind the subnet guard.
func (h *Handlers) PlainHTTP() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /start", h.start)
	mux.HandleFunc("GET /boot", h.chain)
	mux.HandleFunc("GET /tftp/{arch}/snp.efi", h.tftp)
	mux.HandleFunc("GET /u/{entry}/{arch}/{version}/{path...}", h.artifact)
	mux.HandleFunc(talosRoute, h.talosArtifact)
	return RequireSubnet(h.subnet, mux)
}

// TLS returns the handler for the TLS listener: /boot, /discover, the iPXE
// binaries and the Talos artifacts, behind the subnet guard. Which listener a
// request came in on, not the request, says it is a TLS one: serving it is what
// makes /boot decide.
func (h *Handlers) TLS() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boot", h.boot)
	mux.HandleFunc("POST /discover", h.discover)
	mux.HandleFunc("GET /tftp/{arch}/snp.efi", h.tftp)
	mux.HandleFunc(talosRoute, h.talosArtifact)
	return RequireSubnet(h.subnet, mux)
}

// talosRoute is the pattern of the Talos artifact route, on both listeners.
const talosRoute = "GET /t/{schematic}/{version}/{arch}/{asset}"

// Proxied returns the handler for the listener a TLS-terminating reverse proxy
// forwards to: the TLS routes, for clients named by X-Forwarded-For from a peer
// inside trusted (see TrustProxies). The request arrives as plain HTTP because
// the proxy has already terminated the TLS.
func (h *Handlers) Proxied(trusted []netip.Prefix) http.Handler {
	return TrustProxies(trusted, h.TLS())
}

// start answers every request with the chain script, whatever it says.
func (h *Handlers) start(w http.ResponseWriter, r *http.Request) {
	h.writeScript(w, r, StartScript(h.fqdn))
}

// chain answers /boot on the plain-HTTP listener: it only chains to TLS,
// without parsing the query or touching the database.
func (h *Handlers) chain(w http.ResponseWriter, r *http.Request) {
	h.serve(w, r, bootOutcome{d: Decide(Input{})})
}

// boot answers /boot on a TLS listener: it decides, records and renders.
func (h *Handlers) boot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, err := parseBootQuery(r.URL.Query())
	if err != nil {
		telemetry.AddAttrs(ctx, slog.String("error", err.Error()))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	id := fleet.Identity{UUID: req.UUID, MAC: req.MAC, Serial: req.Serial, Product: req.Product}
	m, found, err := h.repo.Lookup(ctx, id)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	unknown, err := h.repo.UnknownAction(ctx)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	out, err := h.decideAndRecord(ctx, id, req, bootState{m: m, found: found, unknown: unknown})
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	if req.Platform != "" {
		telemetry.AddAttrs(ctx, slog.String("platform", string(req.Platform)))
	}
	if out.in.BootNext != "" {
		telemetry.AddAttrs(ctx, slog.String("boot_next", out.in.BootNext))
	}
	if out.d.BootNextClear != "" {
		telemetry.AddAttrs(ctx, slog.String("boot_next_clear", string(out.d.BootNextClear)))
	}
	if out.d.Alert {
		h.countAlert(ctx, alertKindUUIDConflict)
	}
	if out.tooLong > 0 {
		h.alertCmdline(ctx, out.in.Machine, out.tooLong, out.in.Now)
	}
	if out.d.Row == 8 && h.configs != nil {
		h.ensureAssets(ctx, out.in.Machine, out.in.Now)
	}
	if out.d.NeedsTicket {
		if out.script, err = h.installScript(ctx, out); err != nil {
			h.internalError(w, r, err)
			return
		}
	}
	h.serve(w, r, out)
}

// bootAttempts is how many times /boot decides a request whose machine's
// boot-next another process keeps changing; the last decision leaves it alone.
const bootAttempts = 3

// bootState is what /boot knows of the machine before it decides: the recorded
// machine, when there is one, and the unknown_action setting.
type bootState struct {
	m       fleet.Machine
	found   bool
	unknown fleet.UnknownAction
}

// bootOutcome is the decision /boot stored: the input of the last attempt, the
// decision and the utility entry for the machine's architecture, when one was
// read. cmdline is the cached kernel command line of an assigned machine whose
// assets are ready, tooLong the length of a composed command line that did not
// fit (0 when none), and script the install script once a ticket is minted.
type bootOutcome struct {
	in      Input
	d       Decision
	entry   catalog.Entry
	cmdline string
	tooLong int
	script  string
}

// decideAndRecord decides the request and stores the boot. The boot-next
// compare-and-clear is part of the stored boot, so when another process has
// changed the flag since it was read the request is decided again from the
// fresh machine, once; a second change is reported as ErrBootNextRace and the
// third decision leaves the flag out. The first report of an adopted machine's
// identity is part of the stored boot too: when another boot or /discover
// reported it first, the request is decided again from the fresh machine, whose
// identity is now observed, so it meets row 2 when its serial or product
// differs. The identity is observed once, so that retry happens once.
func (h *Handlers) decideAndRecord(ctx context.Context, id fleet.Identity, req Request, s bootState) (bootOutcome, error) {
	raced := false
	for attempt := 0; ; {
		now := h.now() // one read per attempt: the decision and the record agree on it
		out, err := h.decide(ctx, req, s, attempt, now)
		if err != nil {
			return bootOutcome{}, err
		}

		stored, err := h.repo.RecordBoot(ctx, bootRecord(id, req, s, out))
		switch {
		case errors.Is(err, fleet.ErrBootNextChanged):
			if attempt == bootAttempts-2 {
				h.onError(ctx, ErrBootNextRace)
			}
			attempt++
		case errors.Is(err, fleet.ErrIdentityRace) && !raced:
			raced = true
		default:
			if err == nil {
				s.m = stored
				telemetry.AddAttrs(ctx, slog.String("state", string(s.m.State)))
			}
			if s.m.ID != "" {
				telemetry.AddAttrs(ctx, slog.String("machine_id", s.m.ID))
			}
			// A full fleet and an unidentifiable machine are not failures: the machine
			// still gets its script, it just is not recorded.
			return out, noteUnrecorded(ctx, err)
		}
		if s.m, s.found, err = h.repo.Lookup(ctx, id); err != nil {
			return bootOutcome{}, err
		}
	}
}

// decide decides the request for the machine in s at now. attempt counts the
// boot-next changes seen so far: the last attempt leaves the flag out. An
// install whose kernel command line cannot fit becomes a row 10 hold.
func (h *Handlers) decide(ctx context.Context, req Request, s bootState, attempt int, now time.Time) (bootOutcome, error) {
	var out bootOutcome
	out.in = Input{TLS: true, Known: s.found, Machine: s.m, Request: req, UnknownAction: s.unknown, Now: now}
	if s.found && s.m.BootNext != "" && attempt < bootAttempts-1 && flagServesMAC(s.m, req.MAC) {
		var err error
		out.in.BootNext, out.in.BootNextSetAt, out.in.PlatformKnown = s.m.BootNext, s.m.BootNextSetAt, req.Platform != ""
		out.in.Utility, out.entry, err = h.utilityFacts(ctx, s.m.BootNext, req.Arch, req.Platform, now)
		if err != nil {
			return bootOutcome{}, err
		}
	}
	if s.found {
		var err error
		if out.cmdline, err = h.assignedFacts(ctx, &out.in); err != nil {
			return bootOutcome{}, err
		}
	}
	out.d = Decide(out.in)
	out.tooLong = h.installCheck(&out.d, s.m, out.cmdline)
	return out, nil
}

// bootRecord is the boot to store for the decision out made on the machine in s.
func bootRecord(id fleet.Identity, req Request, s bootState, out bootOutcome) fleet.BootRecord {
	d := out.d
	rec := fleet.BootRecord{
		Identity: id, Arch: req.Arch, MachineID: s.m.ID, Trigger: d.Trigger, Conflict: d.Row == 2, MarkJunk: d.MarkJunk,
		Event: fleet.BootEvent{Row: d.Row, Action: string(d.Action), Reason: d.Reason}, At: out.in.Now,
		ObserveIdentity: s.found && !s.m.IdentityObserved,
	}
	if d.BootNextClear != "" {
		rec.ClearBootNext = &fleet.BootNextClear{Entry: s.m.BootNext, SetAt: s.m.BootNextSetAt, Reason: d.BootNextClear}
	}
	if d.WantUtility && out.entry.Entry != "" {
		rec.WantFetch = &fleet.FetchWant{Key: fleet.CacheKey{Entry: s.m.BootNext, Arch: req.Arch}, Pin: out.entry.Pin.ID}
	}
	return rec
}

// flagServesMAC reports whether m's boot-next is for a boot from mac. A junk
// record (one marked junk, which includes every junk-pattern UUID) is identified
// only by its MACs, and a MAC can move to another record, so its flag is bound to
// the MAC the operator named and applies only to a boot from it. A junk record
// whose flag names no MAC (it was armed before the record was marked junk) is
// served to no one. Any other request is decided as if the machine had no flag,
// which leaves the flag for the right boot.
func flagServesMAC(m fleet.Machine, mac string) bool {
	return !m.Junk || (m.BootNextMAC != "" && m.BootNextMAC == mac)
}

// countAlert counts one raised alert of kind.
func (h *Handlers) countAlert(ctx context.Context, kind string) {
	h.alerts.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind)))
}

// noteUnrecorded notes on the request's wide event why RecordBoot stored
// nothing and returns nil when the boot may still be answered; any other error
// is returned as is.
func noteUnrecorded(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, fleet.ErrFleetFull):
		telemetry.AddAttrs(ctx, slog.Bool("fleet_full", true))
		return nil
	case errors.Is(err, fleet.ErrUnidentifiable):
		telemetry.AddAttrs(ctx, slog.Bool("unidentified", true))
		return nil
	}
	return err
}

// serve renders out's decision, counts it and answers with the script. A
// utility decision answers with the recipe of out's catalog entry, and an
// install with out's install script.
func (h *Handlers) serve(w http.ResponseWriter, r *http.Request, out bootOutcome) {
	d := out.d
	script := out.script
	if d.Action == ActionUtility {
		script = out.entry.Script(h.address)
	} else if d.Action != ActionInstall {
		var err error
		if script, err = Render(d, h.fqdn); err != nil {
			h.internalError(w, r, err)
			return
		}
	}
	ctx := r.Context()
	attrs := []slog.Attr{slog.Int("decision_row", d.Row), slog.String("action", string(d.Action))}
	switch d.Action {
	case ActionHold:
		attrs = append(attrs, slog.String("hold_reason", d.Reason))
	case ActionUtility:
		attrs = append(attrs, slog.String("utility", d.Utility))
		h.boots.Add(ctx, 1, metric.WithAttributes(attribute.String("entry", d.Utility)))
	case ActionChainTLS, ActionExit, ActionInstall:
	}
	telemetry.AddAttrs(ctx, attrs...)
	h.decisions.Add(ctx, 1, metric.WithAttributes(attribute.Int("decision_row", d.Row), attribute.String("action", string(d.Action))))
	h.writeScript(w, r, script)
}

// writeScript answers 200 with script as text, under a write deadline.
func (h *Handlers) writeScript(w http.ResponseWriter, r *http.Request, script string) {
	if err := setWriteDeadline(w, scriptDeadline); err != nil {
		h.internalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", contentTypeScript)
	if _, err := io.WriteString(w, script); err != nil {
		telemetry.AddAttrs(r.Context(), slog.String("error", err.Error()))
	}
}

// internalError answers 500 with a fixed body and leaves the cause on the wide
// event, so nothing about the failure reaches the machine.
func (h *Handlers) internalError(w http.ResponseWriter, r *http.Request, err error) {
	telemetry.AddAttrs(r.Context(), slog.String("error", err.Error()))
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// tftp serves the iPXE binary for an architecture to HTTP-booting firmware.
func (h *Handlers) tftp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("arch") + "/snp.efi"
	body, ok := h.files[name]
	if !ok || (name != ipxe.HTTPAMD64 && name != ipxe.HTTPARM64) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentTypeBinary)
	http.ServeContent(idleWriter{w}, r, "snp.efi", time.Time{}, bytes.NewReader(body))
}

// setWriteDeadline gives the next writes to w d from now. A connection
// deadline is a wall-clock instant, so it never follows the injected clock. A
// writer without deadline support is left alone.
func setWriteDeadline(w http.ResponseWriter, d time.Duration) error {
	err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("boot: set write deadline: %w", err)
	}
	return nil
}

// idleWriter pushes the write deadline out before every Write, so a binary
// download is cut off only when the client stops reading, not for being slow.
type idleWriter struct{ http.ResponseWriter }

func (w idleWriter) Write(b []byte) (int, error) {
	if err := setWriteDeadline(w.ResponseWriter, idleWrite); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection.
func (w idleWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// parseBootQuery reads the /boot query into a Request. UUID and MAC are
// normalized, and "" when malformed; Serial and Product are text with NUL bytes
// and invalid UTF-8 removed, since a database text column rejects both; Platform
// is "" unless it names a firmware the catalog knows. A value over
// maxValueBytes is errValueTooLong.
func parseBootQuery(q url.Values) (Request, error) {
	for _, key := range []string{"uuid", "mac", "serial", "product", "arch", "platform"} {
		if len(q.Get(key)) > maxValueBytes {
			return Request{}, fmt.Errorf("%w: %s", errValueTooLong, key)
		}
	}
	mac, err := fleet.NormalizeMAC(q.Get("mac"))
	if err != nil {
		mac = "" // a malformed MAC is an absent one
	}
	platform, _ := catalog.ParsePlatform(q.Get("platform")) // an unrecognised platform is an absent one
	return Request{
		UUID:     fleet.NormalizeUUID(q.Get("uuid")),
		MAC:      mac,
		Serial:   fleet.CleanText(q.Get("serial")),
		Product:  fleet.CleanText(q.Get("product")),
		Arch:     fleet.ArchFromBuildArch(q.Get("arch")),
		Platform: platform,
	}, nil
}
