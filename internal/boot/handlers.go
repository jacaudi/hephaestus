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
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"go.jacaudi.dev/hephaestus/internal/boot/ipxe"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/telemetry"
)

const (
	// maxValueBytes is the longest a request value may be. Real UUIDs, MACs and
	// SMBIOS strings are far shorter; longer ones are hostile or broken.
	maxValueBytes = 256

	// scriptDeadline bounds writing an iPXE script; tftpIdle bounds each write of
	// a binary, so a slow client may take as long as it keeps making progress.
	scriptDeadline = 10 * time.Second
	tftpIdle       = 30 * time.Second

	contentTypeScript = "text/plain; charset=utf-8"
	contentTypeBinary = "application/octet-stream"

	// M1 has neither utility boots nor ticket minting; the machine holds instead.
	reasonNoTicket  = "ticket minting not available (M3)"
	reasonNoUtility = "utility boot not available (M2)"

	alertKindUUIDConflict = "uuid_conflict"
)

// errValueTooLong is returned by parseBootQuery for a value over maxValueBytes.
var errValueTooLong = errors.New("boot: query value too long")

// Handlers serves the boot routes: the iPXE chain scripts and the iPXE binaries
// over plain HTTP, and /boot over TLS.
type Handlers struct {
	repo   *fleet.Repository
	fqdn   string
	subnet netip.Prefix
	files  map[string][]byte
	now    func() time.Time
	meter  metric.Meter

	decisions, alerts metric.Int64Counter
}

// Option configures Handlers.
type Option func(*Handlers)

// WithClock sets the clock that stamps recorded boots; the default is time.Now.
func WithClock(now func() time.Time) Option {
	return func(h *Handlers) { h.now = now }
}

// WithMeter sets the meter the boot counters are created on; the default is a
// no-op meter.
func WithMeter(m metric.Meter) Option {
	return func(h *Handlers) { h.meter = m }
}

// NewHandlers returns the boot routes for the service at fqdn. Only clients in
// subnet are served. files holds the iPXE binaries by the names /tftp serves
// them under, such as "amd64/snp.efi".
func NewHandlers(repo *fleet.Repository, fqdn string, subnet netip.Prefix, files map[string][]byte, opts ...Option) (*Handlers, error) {
	h := &Handlers{
		repo: repo, fqdn: fqdn, subnet: subnet, files: files,
		now: time.Now, meter: noop.NewMeterProvider().Meter(""),
	}
	for _, opt := range opts {
		opt(h)
	}
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
	return h, nil
}

// PlainHTTP returns the handler for the plain-HTTP listener: /start, /boot
// (which only chains to TLS) and /tftp, behind the subnet guard.
func (h *Handlers) PlainHTTP() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /start", h.start)
	mux.HandleFunc("GET /boot", h.boot)
	mux.HandleFunc("GET /tftp/{arch}/snp.efi", h.tftp)
	return RequireSubnet(h.subnet, mux)
}

// TLS returns the handler for the TLS listener: /boot, behind the subnet guard.
func (h *Handlers) TLS() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boot", h.boot)
	return RequireSubnet(h.subnet, mux)
}

// start answers every request with the chain script, whatever it says.
func (h *Handlers) start(w http.ResponseWriter, r *http.Request) {
	h.writeScript(w, r, StartScript(h.fqdn))
}

// boot answers /boot. Over plain HTTP it only chains to TLS, without parsing
// the query or touching the database; over TLS it decides, records and renders.
func (h *Handlers) boot(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil {
		h.serve(w, r, Decide(Input{}))
		return
	}
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
	// The one-shot utility target and the M2 and M3 readiness facts are not
	// known yet: Decide gets BootNext "" and the three flags false. M2 reads the
	// target here, for a recorded machine only.
	d := m1Fallback(Decide(Input{TLS: true, Known: found, Machine: m, Request: req, UnknownAction: unknown}))

	rec := fleet.BootRecord{
		Identity: id, Arch: req.Arch, MachineID: m.ID, Trigger: d.Trigger, Conflict: d.Row == 2, MarkJunk: d.MarkJunk,
		Event: fleet.BootEvent{Row: d.Row, Action: string(d.Action), Reason: d.Reason}, At: h.now(),
	}
	stored, err := h.repo.RecordBoot(ctx, rec)
	if err == nil {
		m = stored
		telemetry.AddAttrs(ctx, slog.String("state", string(m.State)))
	}
	if m.ID != "" {
		telemetry.AddAttrs(ctx, slog.String("machine_id", m.ID))
	}
	// A full fleet and an unidentifiable machine are not failures: the machine
	// still gets its script, it just is not recorded.
	if err = noteUnrecorded(ctx, err); err != nil {
		h.internalError(w, r, err)
		return
	}
	if d.Alert {
		h.alerts.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", alertKindUUIDConflict)))
	}
	h.serve(w, r, d)
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

// serve renders d, counts it and answers with the script.
func (h *Handlers) serve(w http.ResponseWriter, r *http.Request, d Decision) {
	script, err := Render(d, h.fqdn)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	ctx := r.Context()
	attrs := []slog.Attr{slog.Int("decision_row", d.Row), slog.String("action", string(d.Action))}
	if d.Action == ActionHold {
		attrs = append(attrs, slog.String("hold_reason", d.Reason))
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
	if err := setWriteDeadline(w.ResponseWriter, tftpIdle); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the connection.
func (w idleWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// m1Fallback turns the decisions M1 cannot carry out into holds: install needs
// a config ticket (M3) and utility needs a cached utility OS (M2).
func m1Fallback(d Decision) Decision {
	switch {
	case d.NeedsTicket:
		return Decision{Row: d.Row, Action: ActionHold, Reason: reasonNoTicket}
	case d.Action == ActionUtility:
		return Decision{Row: d.Row, Action: ActionHold, Reason: reasonNoUtility}
	}
	return d
}

// parseBootQuery reads the /boot query into a Request. UUID and MAC are
// normalized, and "" when malformed; Serial and Product are text with NUL bytes
// and invalid UTF-8 removed, since a database text column rejects both. A value
// over maxValueBytes is errValueTooLong.
func parseBootQuery(q url.Values) (Request, error) {
	for _, key := range []string{"uuid", "mac", "serial", "product", "arch"} {
		if len(q.Get(key)) > maxValueBytes {
			return Request{}, fmt.Errorf("%w: %s", errValueTooLong, key)
		}
	}
	mac, err := fleet.NormalizeMAC(q.Get("mac"))
	if err != nil {
		mac = "" // a malformed MAC is an absent one
	}
	return Request{
		UUID:    fleet.NormalizeUUID(q.Get("uuid")),
		MAC:     mac,
		Serial:  cleanText(q.Get("serial")),
		Product: cleanText(q.Get("product")),
		Arch:    fleet.ArchFromBuildArch(q.Get("arch")),
	}, nil
}

// cleanText removes NUL bytes and invalid UTF-8 from s.
func cleanText(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "")
}
