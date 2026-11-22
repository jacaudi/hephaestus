package boot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/telemetry"
)

const (
	// discoverPerSource is how many new machines or changed reports one source
	// address may submit per discoverWindow.
	discoverPerSource = 8
	discoverWindow    = 10 * time.Minute
	// limiterCapacity is how many source addresses the limiter tracks.
	limiterCapacity = 4096

	// maxFormBytes bounds a /discover body; maxNICs is how many nic<N>_mac
	// fields are read, so a report stays bounded whatever it carries.
	maxFormBytes = 16 << 10
	maxNICs      = 16

	// retryAfterSeconds is the Retry-After of both overload answers: the limiter
	// window, so a retry lands after it.
	retryAfterSeconds = "600"

	// memsizeUnknown stands in for a memsize of "" or "0": EFI iPXE reports 0.
	memsizeUnknown = "unknown"

	contentTypeProblem = "application/problem+json"
)

// The outcome attribute values of the hephaestus.discovery.reports counter.
const (
	outcomeStored      = "stored"
	outcomeUnchanged   = "unchanged"
	outcomeRateLimited = "rate_limited"
	outcomeFleetFull   = "fleet_full"
	outcomeConflict    = "conflict"
	outcomeInvalid     = "invalid"
)

var (
	// errNoIdentity is returned by parseDiscoverForm for a report with neither
	// a valid UUID nor a MAC.
	errNoIdentity = errors.New("boot: discover: no valid UUID and no MAC")
	// errInvalidMAC is returned for a mac or nic<N>_mac that is not a 48-bit MAC.
	errInvalidMAC = errors.New("boot: discover: invalid MAC address")
	// errInvalidText is returned for a facts value with a NUL byte or invalid
	// UTF-8. The stored facts are JSON, which escapes the NUL and replaces the
	// invalid UTF-8, so those fields would be silently mangled; Manufacturer is
	// stored raw in a column, which Postgres rejects and SQLite accepts.
	errInvalidText = errors.New("boot: discover: value has a NUL byte or invalid UTF-8")
)

// nic is one network interface of a report.
type nic struct {
	Index int    `json:"index"`
	MAC   string `json:"mac"`
	Chip  string `json:"chip"`
}

// facts is the report as stored: its JSON encoding, in field order, is the
// byte string that identical reports are compared by.
type facts struct {
	UUID         string `json:"uuid"`
	MAC          string `json:"mac"`
	Serial       string `json:"serial"`
	Manufacturer string `json:"manufacturer"`
	Product      string `json:"product"`
	CPUVendor    string `json:"cpuvendor"`
	CPUModel     string `json:"cpumodel"`
	MemSize      string `json:"memsize"`
	Platform     string `json:"platform"`
	Arch         string `json:"arch"`
	NICs         []nic  `json:"nics"`
}

// discoverInput is a validated /discover report.
type discoverInput struct {
	Identity fleet.Identity
	Facts    facts
	// MACs are the report's NIC addresses, then mac; none is empty and none repeats.
	MACs []string
}

// parseDiscoverForm validates a /discover form before any I/O. A value over
// maxValueBytes, a malformed mac or nic<N>_mac, and a report with neither a
// valid UUID nor a MAC are errors. Identity text goes through fleet.CleanText,
// as /boot's does, so both routes match a machine the same way. Every other
// text value must be valid UTF-8 without NUL: Manufacturer is stored raw, and the
// facts are JSON-encoded, which would silently mangle the rest (NUL escaped,
// invalid UTF-8 replaced). A NIC without a MAC is left out. memsize "" or "0" is
// stored as "unknown".
func parseDiscoverForm(form url.Values) (discoverInput, error) {
	for key, values := range form {
		for _, v := range values {
			if len(v) > maxValueBytes {
				// key is the client's; bound it in the error that reaches the wide event.
				return discoverInput{}, fmt.Errorf("%w: %q", errValueTooLong, key[:min(len(key), maxValueBytes)])
			}
		}
	}
	var in discoverInput
	f := &in.Facts
	f.UUID = fleet.NormalizeUUID(form.Get("uuid"))
	f.Serial, f.Product = fleet.CleanText(form.Get("serial")), fleet.CleanText(form.Get("product"))
	var err error
	if f.MAC, err = optionalMAC(form.Get("mac")); err != nil {
		return discoverInput{}, err
	}
	for _, field := range []struct {
		key string
		dst *string
	}{
		{"manufacturer", &f.Manufacturer}, {"cpuvendor", &f.CPUVendor}, {"cpumodel", &f.CPUModel},
		{"memsize", &f.MemSize}, {"platform", &f.Platform}, {"arch", &f.Arch},
	} {
		if *field.dst, err = strictText(form.Get(field.key)); err != nil {
			return discoverInput{}, err
		}
	}
	if f.MemSize == "" || f.MemSize == "0" {
		f.MemSize = memsizeUnknown
	}
	if f.NICs, err = parseNICs(form); err != nil {
		return discoverInput{}, err
	}
	for _, n := range f.NICs {
		in.MACs = appendMissing(in.MACs, n.MAC)
	}
	if f.MAC != "" {
		in.MACs = appendMissing(in.MACs, f.MAC)
	}
	if f.UUID == "" && f.MAC == "" {
		return discoverInput{}, errNoIdentity
	}
	in.Identity = fleet.Identity{UUID: f.UUID, MAC: f.MAC, Serial: f.Serial, Product: f.Product}
	return in, nil
}

// parseNICs reads nic<N>_mac and nic<N>_chip for N below maxNICs. A NIC whose
// MAC is empty is left out: it would take one of the machine's MAC slots.
func parseNICs(form url.Values) ([]nic, error) {
	var nics []nic
	for i := range maxNICs {
		prefix := "nic" + strconv.Itoa(i)
		mac, err := optionalMAC(form.Get(prefix + "_mac"))
		if err != nil {
			return nil, err
		}
		if mac == "" {
			continue
		}
		chip, err := strictText(form.Get(prefix + "_chip"))
		if err != nil {
			return nil, err
		}
		nics = append(nics, nic{Index: i, MAC: mac, Chip: chip})
	}
	return nics, nil
}

// optionalMAC normalizes raw, and returns "" for an absent one.
func optionalMAC(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	mac, err := fleet.NormalizeMAC(raw)
	if err != nil {
		return "", errInvalidMAC
	}
	return mac, nil
}

// strictText returns s, or errInvalidText when it has a NUL byte or invalid UTF-8.
func strictText(s string) (string, error) {
	if !utf8.ValidString(s) || strings.Contains(s, "\x00") {
		return "", errInvalidText
	}
	return s, nil
}

// appendMissing appends s to list unless it is already there.
func appendMissing(list []string, s string) []string {
	if slices.Contains(list, s) {
		return list
	}
	return append(list, s)
}

// report is one /discover request after validation.
type report struct {
	in discoverInput
	// payload is the bytes stored, and compared against the latest report's.
	payload []byte
	source  netip.Addr
}

// discover answers POST /discover: it validates an inventory report, records
// the machine it describes and stores the report. A new machine or a changed
// report costs its source address one limiter token; an unchanged report is
// free, so a machine that holds and re-reports every 30 s is never throttled.
// Each step answers the request itself when it refuses.
func (h *Handlers) discover(w http.ResponseWriter, r *http.Request) {
	rep, ok := h.readReport(w, r)
	if !ok {
		return
	}
	obs, ok := h.admit(w, r, rep)
	if !ok {
		return
	}
	h.store(w, r, rep, obs)
}

// readReport reads and validates the request body.
func (h *Handlers) readReport(w http.ResponseWriter, r *http.Request) (report, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		status := http.StatusBadRequest
		if tooBig, ok := errors.AsType[*http.MaxBytesError](err); ok {
			status = http.StatusRequestEntityTooLarge
			err = fmt.Errorf("boot: discover: body over %d bytes: %w", tooBig.Limit, err)
		}
		h.discoverReply(w, r, outcomeInvalid, status, err)
		return report{}, false
	}
	in, err := parseDiscoverForm(r.PostForm)
	if err != nil {
		h.discoverReply(w, r, outcomeInvalid, http.StatusBadRequest, err)
		return report{}, false
	}
	payload, err := json.Marshal(in.Facts)
	if err != nil {
		h.discoverInternalError(w, r, fmt.Errorf("boot: discover: encode facts: %w", err))
		return report{}, false
	}
	source, err := clientAddr(r)
	if err != nil {
		// RequireSubnet has already parsed it; only a handler wired without the guard gets here.
		h.discoverInternalError(w, r, err)
		return report{}, false
	}
	return report{in: in, payload: payload, source: source}, true
}

// admit finds or creates the machine the report describes. An unknown machine
// is charged one limiter token here, before it is created, and store does not
// charge that request again.
func (h *Handlers) admit(w http.ResponseWriter, r *http.Request, rep report) (fleet.Observation, bool) {
	ctx := r.Context()
	_, known, err := h.repo.Lookup(ctx, rep.in.Identity)
	if err != nil {
		h.discoverInternalError(w, r, err)
		return fleet.Observation{}, false
	}
	if !known && !h.limiter.Allow(rep.source) {
		h.discoverReply(w, r, outcomeRateLimited, http.StatusTooManyRequests, nil)
		return fleet.Observation{}, false
	}
	obs, err := h.repo.Observe(ctx, rep.in.Identity, fleet.ArchFromBuildArch(rep.in.Facts.Arch), h.now())
	if obs.Machine.ID != "" {
		telemetry.AddAttrs(ctx, slog.String("machine_id", obs.Machine.ID))
	}
	switch {
	case err == nil:
		if obs.MACMoved {
			h.countAlert(ctx, alertKindMACMoved)
		}
		return obs, true
	case errors.Is(err, fleet.ErrIdentityConflict):
		if obs.Alerted {
			h.countAlert(ctx, alertKindUUIDConflict)
		}
		h.discoverReply(w, r, outcomeConflict, http.StatusConflict, err)
	case errors.Is(err, fleet.ErrFleetFull):
		h.discoverReply(w, r, outcomeFleetFull, http.StatusServiceUnavailable, err)
	case errors.Is(err, fleet.ErrUnidentifiable):
		h.discoverReply(w, r, outcomeInvalid, http.StatusBadRequest, err)
	default:
		h.discoverInternalError(w, r, err)
	}
	return fleet.Observation{}, false
}

// store records the report. A machine created by this request is not charged
// again; for any other, a report that differs from the latest costs a token and
// an identical one is free.
func (h *Handlers) store(w http.ResponseWriter, r *http.Request, rep report, obs fleet.Observation) {
	ctx := r.Context()
	if !obs.Created {
		latest, found, err := h.repo.LatestReport(ctx, obs.Machine.ID)
		if err != nil {
			h.discoverInternalError(w, r, err)
			return
		}
		changed := !found || !bytes.Equal(latest, rep.payload)
		if changed && !h.limiter.Allow(rep.source) {
			h.discoverReply(w, r, outcomeRateLimited, http.StatusTooManyRequests, nil)
			return
		}
	}
	stored, moved, err := h.repo.RecordReport(ctx, obs.Machine.ID, fleet.Report{
		Facts: rep.payload, MACs: rep.in.MACs, Manufacturer: rep.in.Facts.Manufacturer, At: h.now(),
	})
	if err != nil {
		h.discoverInternalError(w, r, err)
		return
	}
	for range moved {
		h.countAlert(ctx, alertKindMACMoved)
	}
	outcome := outcomeUnchanged
	if stored {
		outcome = outcomeStored
	}
	h.countReport(ctx, outcome)
	if _, err := io.WriteString(w, "ok\n"); err != nil {
		telemetry.AddAttrs(ctx, slog.String("write_error", err.Error()))
	}
}

// discoverReply counts outcome and refuses with status as a problem document;
// err, when not nil, is left on the wide event. The overload statuses carry
// Retry-After.
func (h *Handlers) discoverReply(w http.ResponseWriter, r *http.Request, outcome string, status int, err error) {
	ctx := r.Context()
	if err != nil {
		telemetry.AddAttrs(ctx, slog.String("error", err.Error()))
	}
	h.countReport(ctx, outcome)
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", retryAfterSeconds)
	}
	writeProblem(ctx, w, status)
}

// writeProblem answers status with an RFC 9457 problem document: type
// about:blank and the status text as the title, so nothing about the failure
// reaches the machine.
func writeProblem(ctx context.Context, w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", contentTypeProblem)
	w.WriteHeader(status)
	// The title is a fixed ASCII status text, so %q is valid JSON.
	if _, err := fmt.Fprintf(w, `{"type":"about:blank","title":%q,"status":%d}`, http.StatusText(status), status); err != nil {
		telemetry.AddAttrs(ctx, slog.String("write_error", err.Error()))
	}
}

// countReport counts a /discover outcome and notes it on the wide event.
func (h *Handlers) countReport(ctx context.Context, outcome string) {
	telemetry.AddAttrs(ctx, slog.String("outcome", outcome))
	h.reports.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// discoverInternalError answers 500 with the problem document, and leaves the
// cause on the wide event.
func (h *Handlers) discoverInternalError(w http.ResponseWriter, r *http.Request, err error) {
	telemetry.AddAttrs(r.Context(), slog.String("error", err.Error()))
	writeProblem(r.Context(), w, http.StatusInternalServerError)
}
