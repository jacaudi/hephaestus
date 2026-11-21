package boot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"go.jacaudi.dev/hephaestus/internal/clusters"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/telemetry"
)

const (
	contentTypeYAML = "application/yaml"

	// refusalAuditWindow is how often a /config refusal is audited for one host,
	// or for one reason while no host is known.
	refusalAuditWindow = time.Hour
	// recordRetryBase is the first wait before a failed recording is tried
	// again; each wait doubles. recordGiveUp after the first failure, a
	// recording that still fails is given up and alerted.
	recordRetryBase = time.Second
	recordGiveUp    = time.Hour

	outcomeServed          = "served"
	actionConfigRefused    = "config.refused"
	actionConfigUnrecorded = "alert.config_unrecorded"
	alertKindUnrecorded    = "config_unrecorded"
)

// refusal is why /config answered 404; the zero value is none. Each is fixed
// text, so it is safe on the wide event, the metrics and the audit log.
type refusal string

const (
	refusedOffSubnet     refusal = "off_subnet"
	refusedBadQuery      refusal = "bad_query"
	refusedUnknownTicket refusal = "unknown_ticket"
	// refusedInvalidTicket is a ticket that is consumed, revoked or expired.
	refusedInvalidTicket refusal = "invalid_ticket"
	refusedOtherMachine  refusal = "other_machine"
	refusedNotAssigned   refusal = "not_assigned"
	refusedJunk          refusal = "junk"
	refusedOtherHost     refusal = "other_host"
	// refusedStalePair is a ticket whose pair is not its cluster's approved one.
	refusedStalePair   refusal = "stale_pair"
	refusedNoRender    refusal = "no_render"
	refusedDrift       refusal = "drift"
	refusedRenderError refusal = "render_error"
)

var (
	// errRenderFailed is reported for a render that failed: the render's own
	// error can quote a secret value, so it is never reported.
	errRenderFailed = errors.New("boot: config render failed")
	// errConfigUnrecorded is reported when a delivered config can no longer be
	// recorded: the machine needs the runbook its state names.
	errConfigUnrecorded = errors.New("boot: config delivered but not recorded")
	// errRecordingAbandoned is reported for each recording still waiting for a
	// retry when RunRecorder stops.
	errRecordingAbandoned = errors.New("boot: config recording abandoned")
)

// refusedDetail is the detail of a config.refused audit event. ClusterID and
// Host are the ticket's, when one was found.
type refusedDetail struct {
	Reason    string `json:"reason"`
	ClusterID string `json:"cluster_id,omitempty"`
	Host      string `json:"host,omitempty"`
	Client    string `json:"client,omitempty"`
}

// unrecordedDetail is the detail of an alert.config_unrecorded audit event.
type unrecordedDetail struct {
	MachineID string      `json:"machine_id"`
	State     fleet.State `json:"state"`
}

// Config returns the /config route: GET /config, behind a subnet guard of its
// own that answers 404 as every other refusal does. A machine in maintenance
// mode presents its ticket and receives its rendered machine config; the
// delivery is then recorded, in the background when the first try fails.
func (h *Handlers) Config() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config", h.config)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !onSubnet(h.subnet, r) {
			h.refuse(w, r, refusedOffSubnet, fleet.Ticket{}, h.now())
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// config answers /config: it checks the ticket, the machine and the pair,
// renders the ticket's pair, writes and flushes the body and records the
// delivery. Every refusal is a 404 that leaves the ticket alone.
func (h *Handlers) config(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := h.now()
	t, m, why, err := h.configTicket(ctx, r.URL.Query(), now)
	if err == nil && why == "" {
		why, err = h.pairRefusal(ctx, t)
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	var body []byte
	if why == "" {
		body, why = h.renderConfig(ctx, t)
	}
	if why != "" {
		h.refuse(w, r, why, t, now)
		return
	}
	if err := writeConfig(w, body); err != nil {
		// Not delivered in full: the ticket stays live for Talos's next try.
		telemetry.AddAttrs(ctx, slog.String("error", err.Error()))
		return
	}
	h.countConfig(ctx, outcomeServed)
	// The body has left: a client that goes away now must not stop the record.
	h.recordServed(context.WithoutCancel(ctx), t, m.State, now)
}

// configTicket finds the ticket of the query's t and its machine, which must be
// the one with UUID u, marks the ticket requested and checks the machine is
// still the one it was minted for. It returns why the request is refused, if
// it is; an error is a failure to read or write the database.
func (h *Handlers) configTicket(ctx context.Context, q url.Values, now time.Time) (fleet.Ticket, fleet.Machine, refusal, error) {
	rawUUID, token := q.Get("u"), q.Get("t")
	uuid := fleet.NormalizeUUID(rawUUID)
	if uuid == "" || token == "" || len(rawUUID) > maxValueBytes || len(token) > maxValueBytes {
		return fleet.Ticket{}, fleet.Machine{}, refusedBadQuery, nil
	}
	t, found, err := h.repo.TicketByToken(ctx, token)
	if err != nil {
		return fleet.Ticket{}, fleet.Machine{}, "", err
	}
	if !found {
		return fleet.Ticket{}, fleet.Machine{}, refusedUnknownTicket, nil
	}
	telemetry.AddAttrs(ctx, slog.String("machine_id", t.MachineID))
	if !t.Valid(now) {
		return t, fleet.Machine{}, refusedInvalidTicket, nil
	}
	m, err := h.repo.Get(ctx, t.MachineID)
	if errors.Is(err, fleet.ErrNotFound) {
		return t, fleet.Machine{}, refusedUnknownTicket, nil
	}
	if err != nil {
		return t, fleet.Machine{}, "", err
	}
	if m.UUID != uuid {
		return t, m, refusedOtherMachine, nil
	}
	if err := h.repo.MarkTicketRequested(ctx, t.ID, now); err != nil {
		return t, m, "", err
	}
	return t, m, machineRefusal(m, t), nil
}

// machineRefusal is why m may not receive t's config: it is not assigned, it is
// junk, or it is no longer bound to t's host.
func machineRefusal(m fleet.Machine, t fleet.Ticket) refusal {
	switch {
	case m.State != fleet.StateAssigned:
		return refusedNotAssigned
	case m.Junk:
		return refusedJunk
	case m.ClusterID == nil || *m.ClusterID != t.ClusterID || m.HostName == nil || *m.HostName != t.HostName:
		return refusedOtherHost
	}
	return ""
}

// pairRefusal refuses t unless its pair and secret version are its cluster's
// approved ones, so a ticket a revocation missed still fails closed.
func (h *Handlers) pairRefusal(ctx context.Context, t fleet.Ticket) (refusal, error) {
	if h.configs == nil {
		return refusedNoRender, nil
	}
	p, err := h.configs.Approved(ctx, t.ClusterID)
	if errors.Is(err, clusters.ErrNotFound) {
		return refusedStalePair, nil
	}
	if err != nil {
		return "", fmt.Errorf("boot: config of %s: %w", t.MachineID, err)
	}
	if p.ID != t.PairID || p.SecretVersionID != t.SecretVersionID {
		return refusedStalePair, nil
	}
	return "", nil
}

// renderConfig renders t's host from t's pair. A render that failed for any
// other reason than a cache miss or drift is reported by stage only: its error
// can quote a secret value.
func (h *Handlers) renderConfig(ctx context.Context, t fleet.Ticket) ([]byte, refusal) {
	body, err := h.configs.Render(ctx, t)
	switch {
	case err == nil:
		return body, ""
	case errors.Is(err, clusters.ErrNoRender):
		return nil, refusedNoRender
	case errors.Is(err, clusters.ErrDrift):
		return nil, refusedDrift
	}
	h.onError(ctx, fmt.Errorf("%w: machine %s", errRenderFailed, t.MachineID))
	return nil, refusedRenderError
}

// writeConfig answers 200 with body under a write deadline and flushes it: nil
// means the whole body has left.
func writeConfig(w http.ResponseWriter, body []byte) error {
	if err := setWriteDeadline(w, scriptDeadline); err != nil {
		return err
	}
	w.Header().Set("Content-Type", contentTypeYAML)
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(body); err != nil { //nolint:gosec // G705: the body is the server's own render of the approved pair, served as YAML; no request text reaches it
		return fmt.Errorf("boot: write config: %w", err)
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		return fmt.Errorf("boot: flush config: %w", err)
	}
	return nil
}

// refuse answers 404 for why and counts it. The refusal is audited at most once
// per refusalAuditWindow for t's host, or for why while no ticket is known.
func (h *Handlers) refuse(w http.ResponseWriter, r *http.Request, why refusal, t fleet.Ticket, now time.Time) {
	ctx := r.Context()
	h.countConfig(ctx, "refused_"+string(why))
	key := "reason:" + string(why)
	if t.ID != "" {
		key = "host:" + t.ClusterID + "/" + t.HostName
	}
	if h.refusals.Allow(key) {
		detail := refusedDetail{Reason: string(why), ClusterID: t.ClusterID, Host: t.HostName}
		if addr, err := clientAddr(r); err == nil {
			detail.Client = addr.String()
		}
		ev := fleet.AuditEvent{At: now, Actor: fleet.ActorSystem, Action: actionConfigRefused, MachineID: t.MachineID, Detail: detail}
		if err := h.repo.Audit(ctx, ev); err != nil {
			h.onError(ctx, fmt.Errorf("boot: audit %s (%s): %w", actionConfigRefused, why, err))
		}
	}
	http.NotFound(w, r)
}

// countConfig counts a /config outcome and notes it on the wide event.
func (h *Handlers) countConfig(ctx context.Context, outcome string) {
	telemetry.AddAttrs(ctx, slog.String("outcome", outcome))
	h.requests.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// pendingRecord is a delivered config whose recording failed: the request to
// retry, the machine and its state when the config was served, when the first
// try failed (on the handlers' clock), the next wait and when the next try is
// due (on the wall clock, as the timers run).
type pendingRecord struct {
	req       fleet.RecordConfigRequest
	machineID string
	state     fleet.State
	failedAt  time.Time
	wait      time.Duration
	due       time.Time
}

// recordQueue holds the pending recordings by ticket id, so one ticket has one
// retry loop however many deliveries failed to record. wake tells RunRecorder
// that one was added.
type recordQueue struct {
	mu      sync.Mutex
	entries map[string]*pendingRecord
	wake    chan struct{}
}

// add queues p unless its ticket is queued already.
func (q *recordQueue) add(p *pendingRecord) {
	q.mu.Lock()
	if _, ok := q.entries[p.req.TicketID]; !ok {
		q.entries[p.req.TicketID] = p
	}
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// due returns a recording whose try is due at now, or else how long until the
// next one is; waiting is false when none is queued.
func (q *recordQueue) due(now time.Time) (p *pendingRecord, wait time.Duration, waiting bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range q.entries {
		if !e.due.After(now) {
			return e, 0, true
		}
		if d := e.due.Sub(now); !waiting || d < wait {
			wait, waiting = d, true
		}
	}
	return nil, wait, waiting
}

// postpone sets p's next try to due, after a wait of wait.
func (q *recordQueue) postpone(p *pendingRecord, wait time.Duration, due time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	p.wait, p.due = wait, due
}

// remove drops the recording of ticketID.
func (q *recordQueue) remove(ticketID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.entries, ticketID)
}

// drain removes and returns every queued recording.
func (q *recordQueue) drain() []*pendingRecord {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := slices.Collect(maps.Values(q.entries))
	clear(q.entries)
	return out
}

// recordServed records the delivery of t's config, served to a machine in
// state at at. A failure is counted and reported at once and queued for
// RunRecorder to retry.
func (h *Handlers) recordServed(ctx context.Context, t fleet.Ticket, state fleet.State, at time.Time) {
	req := fleet.RecordConfigRequest{TicketID: t.ID, At: at}
	res, err := h.record(ctx, req)
	if err != nil {
		h.recordFailed(ctx, t.MachineID, err)
		h.rec.add(&pendingRecord{
			req: req, machineID: t.MachineID, state: state, failedAt: at,
			wait: h.retryBase, due: h.nextTry(at, h.retryBase),
		})
		return
	}
	h.settle(ctx, t.MachineID, res)
}

// settle acts on a recording that returned. Recorded, recorded by another
// request, gone with its machine, or superseded by a newer ticket's delivery
// (the machine is installing) needs nothing more. A machine that has left
// assigned, or its host, otherwise can no longer record the delivery: that is
// alerted.
func (h *Handlers) settle(ctx context.Context, machineID string, res fleet.RecordConfigResult) {
	if res.Recorded || res.AlreadyConsumed || res.Machine.ID == "" || res.Machine.State == fleet.StateInstalling {
		return
	}
	h.alertUnrecorded(ctx, machineID, res.Machine.State)
}

// recordFailed counts and reports one failed try to record a delivery.
func (h *Handlers) recordFailed(ctx context.Context, machineID string, err error) {
	h.recordFailures.Add(ctx, 1)
	h.onError(ctx, fmt.Errorf("boot: record the config delivered to %s: %w", machineID, err))
}

// nextTry is when the next try of a recording that first failed at failedAt is
// due: wait from now, but no later than the time it is given up.
func (h *Handlers) nextTry(failedAt time.Time, wait time.Duration) time.Time {
	left := failedAt.Add(recordGiveUp).Sub(h.now())
	return time.Now().Add(max(min(wait, left), 0))
}

// RunRecorder retries the recordings of delivered configs whose first try
// failed, each with a wait that starts at a second and doubles, until ctx
// ends. A try stops the retries when it records the delivery, finds it
// recorded already, or finds the machine gone or installing on a newer ticket;
// a machine that has otherwise moved on, or a recording still failing an hour
// after its first failure, raises the config_unrecorded alert. When ctx ends,
// each recording still queued is reported as abandoned, naming its machine.
// It returns nil.
func (h *Handlers) RunRecorder(ctx context.Context) error {
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for ctx.Err() == nil {
		p, wait, waiting := h.rec.due(time.Now())
		if p != nil {
			h.retryRecord(ctx, p)
			continue
		}
		var fire <-chan time.Time
		if waiting {
			timer.Reset(wait)
			fire = timer.C
		}
		select {
		case <-ctx.Done():
		case <-h.rec.wake:
		case <-fire:
		}
		timer.Stop()
	}
	for _, p := range h.rec.drain() {
		h.onError(ctx, fmt.Errorf("%w: machine %s", errRecordingAbandoned, p.machineID))
	}
	return nil
}

// retryRecord tries p's recording once more. A try cut short because ctx ended
// leaves p queued, for RunRecorder to report.
func (h *Handlers) retryRecord(ctx context.Context, p *pendingRecord) {
	res, err := h.record(ctx, p.req)
	switch {
	case err == nil:
		h.rec.remove(p.req.TicketID)
		h.settle(ctx, p.machineID, res)
	case ctx.Err() != nil:
	case !h.now().Before(p.failedAt.Add(recordGiveUp)):
		h.recordFailed(ctx, p.machineID, err)
		h.rec.remove(p.req.TicketID)
		h.alertUnrecorded(ctx, p.machineID, p.state)
	default:
		h.recordFailed(ctx, p.machineID, err)
		wait := 2 * p.wait
		h.rec.postpone(p, wait, h.nextTry(p.failedAt, wait))
	}
}

// alertUnrecorded raises the config_unrecorded alert for the machine in state:
// it is reported and counted, and an audit event is tried, which fails when
// the database is what failed.
func (h *Handlers) alertUnrecorded(ctx context.Context, machineID string, state fleet.State) {
	h.countAlert(ctx, alertKindUnrecorded)
	h.onError(ctx, fmt.Errorf("%w: machine %s is %s", errConfigUnrecorded, machineID, state))
	ev := fleet.AuditEvent{
		At: h.now(), Actor: fleet.ActorSystem, Action: actionConfigUnrecorded, MachineID: machineID,
		Detail: unrecordedDetail{MachineID: machineID, State: state},
	}
	if err := h.repo.Audit(ctx, ev); err != nil {
		h.onError(ctx, fmt.Errorf("boot: audit %s of %s: %w", actionConfigUnrecorded, machineID, err))
	}
}
