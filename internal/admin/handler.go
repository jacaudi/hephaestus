package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	serviceID       = "hephaestus"
	healthMediaType = "application/health+json"
)

// Info identifies the running build for /health.
type Info struct {
	Version   string
	ReleaseID string
	Started   time.Time
}

// checkEntry is one element of a /health check array.
type checkEntry struct {
	Status        Status    `json:"status"`
	ObservedValue any       `json:"observedValue,omitempty"`
	ObservedUnit  string    `json:"observedUnit,omitempty"`
	Time          time.Time `json:"time,omitzero"`
	Output        string    `json:"output,omitempty"`
}

// healthDocument is the /health body.
type healthDocument struct {
	Status    Status                  `json:"status"`
	Version   string                  `json:"version"`
	ReleaseID string                  `json:"releaseId"`
	ServiceID string                  `json:"serviceId"`
	Checks    map[string][]checkEntry `json:"checks"`
}

// handler serves the admin routes from one Registry and Lifecycle.
type handler struct {
	reg  *Registry
	life *Lifecycle
	info Info
}

// NewHandler returns the admin handler: GET /healthz, /readyz, /health and
// /metrics, which is served by metrics. It serves no other route.
func NewHandler(reg *Registry, life *Lifecycle, metrics http.Handler, info Info) http.Handler {
	h := &handler{reg: reg, life: life, info: info}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /readyz", h.readyz)
	mux.HandleFunc("GET /health", h.health)
	mux.Handle("GET /metrics", metrics)
	return mux
}

// healthz reports liveness: it fails only once the service has stopped and
// checks no dependency.
func (h *handler) healthz(w http.ResponseWriter, _ *http.Request) {
	if h.life.Current() == Stopped {
		write(w, http.StatusServiceUnavailable, "application/json", `{"status":"stopped"}`)
		return
	}
	write(w, http.StatusOK, "application/json", `{"status":"ok"}`)
}

// readyz is 200 only when the lifecycle is Ready and every critical check
// passes; otherwise it names each problem. ?verbose lists every check.
func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	obs := h.reg.snapshot()
	var body strings.Builder
	code := http.StatusOK
	if state := h.life.Current(); state != Ready {
		code = http.StatusServiceUnavailable
		fmt.Fprintf(&body, "fail: lifecycle: %s\n", state)
	}
	for _, o := range obs {
		if o.check.Critical && o.result.Status != StatusPass {
			code = http.StatusServiceUnavailable
			fmt.Fprintf(&body, "fail: %s: %s\n", o.check.Name, outputOrStatus(o.result))
		}
	}
	if code == http.StatusOK {
		body.WriteString("ok\n")
	}
	if r.URL.Query().Has("verbose") {
		for _, o := range obs {
			fmt.Fprintf(&body, "%s: %s\n", o.result.Status, o.check.Name)
		}
	}
	write(w, code, "text/plain; charset=utf-8", body.String())
}

// health serves the health+json document. A critical check that is not
// passing, or a lifecycle that is not Ready, makes it fail with 503; any
// other warn or fail makes it warn with 200.
func (h *handler) health(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().UTC()
	status := StatusPass
	checks := map[string][]checkEntry{
		"uptime": {{
			Status: StatusPass, ObservedValue: time.Since(h.info.Started).Seconds(), ObservedUnit: "s", Time: now,
		}},
		"lifecycle": {{Status: StatusPass, Time: now}},
	}
	if state := h.life.Current(); state != Ready {
		status = StatusFail
		checks["lifecycle"][0].Status = StatusFail
		checks["lifecycle"][0].Output = state.String()
	}
	for _, o := range h.reg.snapshot() {
		checks[o.check.Name] = append(checks[o.check.Name], checkEntry{
			Status:        o.result.Status,
			ObservedValue: o.result.ObservedValue,
			ObservedUnit:  o.result.ObservedUnit,
			Time:          o.at,
			Output:        o.result.Output,
		})
		switch {
		case o.result.Status == StatusPass:
		case o.check.Critical:
			status = StatusFail
		case status == StatusPass:
			status = StatusWarn
		}
	}

	body, err := json.Marshal(healthDocument{
		Status: status, Version: h.info.Version, ReleaseID: h.info.ReleaseID, ServiceID: serviceID, Checks: checks,
	})
	if err != nil {
		http.Error(w, "encode health document", http.StatusInternalServerError)
		return
	}
	code := http.StatusOK
	if status == StatusFail {
		code = http.StatusServiceUnavailable
	}
	write(w, code, healthMediaType, string(body))
}

// outputOrStatus names why a check is not passing.
func outputOrStatus(r Result) string {
	if r.Output != "" {
		return r.Output
	}
	return string(r.Status)
}

func write(w http.ResponseWriter, code int, contentType, body string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(code)
	if _, err := fmt.Fprint(w, body); err != nil {
		return // the probe client went away; there is no one left to tell
	}
}
