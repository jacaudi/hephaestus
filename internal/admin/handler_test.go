package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// wireHealth mirrors the /health body independently of the handler's own types.
type wireHealth struct {
	Status    string                 `json:"status"`
	Version   string                 `json:"version"`
	ReleaseID string                 `json:"releaseId"`
	ServiceID string                 `json:"serviceId"`
	Checks    map[string][]wireCheck `json:"checks"`
}

type wireCheck struct {
	Status        string  `json:"status"`
	ObservedValue float64 `json:"observedValue"`
	ObservedUnit  string  `json:"observedUnit"`
	Time          string  `json:"time"`
	Output        string  `json:"output"`
}

func fixed(name string, critical bool, r Result) Check {
	return Check{Name: name, Critical: critical, Run: func(context.Context) Result { return r }}
}

// newTestHandler returns a handler over a registry that has run each check once and a lifecycle in state.
func newTestHandler(t *testing.T, state State, info Info, checks ...Check) http.Handler {
	t.Helper()
	reg := NewRegistry(checks...)
	reg.RunOnce(t.Context())
	life := &Lifecycle{}
	life.Set(state)
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("metrics-ok")) })
	return NewHandler(reg, life, metrics, info)
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))
	return rec
}

func decodeHealth(t *testing.T, rec *httptest.ResponseRecorder) wireHealth {
	t.Helper()
	var doc wireHealth
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode /health body %q: %v", rec.Body.String(), err)
	}
	return doc
}

func TestHealthz(t *testing.T) {
	tests := []struct {
		state    State
		wantCode int
		wantBody string
	}{
		{Ready, http.StatusOK, `{"status":"ok"}`},
		{Draining, http.StatusOK, `{"status":"ok"}`},
		{Stopped, http.StatusServiceUnavailable, `{"status":"stopped"}`},
	}
	for _, tt := range tests {
		t.Run(tt.state.String(), func(t *testing.T) {
			rec := get(t, newTestHandler(t, tt.state, Info{}), "/healthz")

			if rec.Code != tt.wantCode {
				t.Errorf("code = %d, want %d", rec.Code, tt.wantCode)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}

func TestReadyz_PendingCriticalCheck(t *testing.T) {
	reg := NewRegistry(fixed("database:ping", true, Result{Status: StatusPass}))
	life := &Lifecycle{}
	life.Set(Ready)
	h := NewHandler(reg, life, http.NotFoundHandler(), Info{}) // reg.RunOnce never called

	rec := get(t, h, "/readyz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rec.Code)
	}
	if want := "fail: database:ping: pending"; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body = %q, want it to contain %q", rec.Body.String(), want)
	}
}

func TestReadyz_AllPass(t *testing.T) {
	h := newTestHandler(t, Ready, Info{}, fixed("database:ping", true, Result{Status: StatusPass}))

	rec := get(t, h, "/readyz")

	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Errorf("got %d %q, want 200 \"ok\\n\"", rec.Code, rec.Body.String())
	}
}

func TestReadyz_InformationalNeverFails(t *testing.T) {
	h := newTestHandler(t, Ready, Info{}, fixed("tls:expiry", false, Result{Status: StatusFail, Output: "expired"}))

	if rec := get(t, h, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
}

func TestReadyz_CriticalWarnIs503(t *testing.T) {
	h := newTestHandler(t, Ready, Info{}, fixed("database:ping", true, Result{Status: StatusWarn, Output: "slow"}))

	ready := get(t, h, "/readyz")
	if ready.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz code = %d, want 503", ready.Code)
	}
	if want := "fail: database:ping: slow\n"; !strings.Contains(ready.Body.String(), want) {
		t.Errorf("/readyz body = %q, want it to contain %q", ready.Body.String(), want)
	}

	health := get(t, h, "/health")
	if health.Code != http.StatusServiceUnavailable {
		t.Errorf("/health code = %d, want 503", health.Code)
	}
	if got := decodeHealth(t, health).Status; got != "fail" {
		t.Errorf("/health status = %q, want fail", got)
	}
}

func TestReadyz_Draining(t *testing.T) {
	rec := get(t, newTestHandler(t, Draining, Info{}), "/readyz")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rec.Code)
	}
	if want := "fail: lifecycle: draining\n"; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("body = %q, want it to contain %q", rec.Body.String(), want)
	}
}

func TestReadyz_Verbose(t *testing.T) {
	h := newTestHandler(t, Ready, Info{},
		fixed("b", false, Result{Status: StatusWarn}),
		fixed("a", true, Result{Status: StatusPass}),
	)

	rec := get(t, h, "/readyz?verbose")

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	if want := "ok\npass: a\nwarn: b\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestHealth_Format(t *testing.T) {
	info := Info{Version: "v1.2.3", ReleaseID: "abc1234", Started: time.Now()}
	h := newTestHandler(t, Ready, info,
		fixed("database:ping", true, Result{Status: StatusPass, ObservedValue: 1.5, ObservedUnit: "ms"}),
		fixed("tls:expiry", false, Result{
			Status: StatusWarn, ObservedValue: 10, ObservedUnit: "d", Output: "certificate expires in 10 days",
		}),
	)

	rec := get(t, h, "/health")

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/health+json" {
		t.Errorf("Content-Type = %q, want application/health+json", got)
	}
	doc := decodeHealth(t, rec)
	if doc.Status != "warn" || doc.Version != "v1.2.3" || doc.ReleaseID != "abc1234" || doc.ServiceID != "hephaestus" {
		t.Errorf("document = %+v, want status warn, version v1.2.3, releaseId abc1234, serviceId hephaestus", doc)
	}
	ping := doc.Checks["database:ping"]
	if len(ping) != 1 || ping[0].Status != "pass" || ping[0].ObservedValue != 1.5 || ping[0].ObservedUnit != "ms" {
		t.Fatalf("database:ping = %+v, want one pass entry observing 1.5 ms", ping)
	}
	if _, err := time.Parse(time.RFC3339, ping[0].Time); err != nil {
		t.Errorf("time %q is not RFC 3339: %v", ping[0].Time, err)
	}
	tls := doc.Checks["tls:expiry"]
	if len(tls) != 1 || tls[0].Output != "certificate expires in 10 days" {
		t.Errorf("tls:expiry = %+v, want output %q", tls, "certificate expires in 10 days")
	}
	if strings.Contains(rec.Body.String(), `"output":""`) {
		t.Errorf("body %q carries an empty output; it must be omitted", rec.Body.String())
	}
}

func TestHealth_CriticalFail503(t *testing.T) {
	h := newTestHandler(t, Ready, Info{}, fixed("database:ping", true, Result{Status: StatusFail, Output: "down"}))

	rec := get(t, h, "/health")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rec.Code)
	}
	if got := decodeHealth(t, rec).Status; got != "fail" {
		t.Errorf("status = %q, want fail", got)
	}
}

func TestHealth_InformationalFailIsWarn(t *testing.T) {
	h := newTestHandler(t, Ready, Info{}, fixed("tls:expiry", false, Result{Status: StatusFail, Output: "expired"}))

	rec := get(t, h, "/health")

	if rec.Code != http.StatusOK {
		t.Errorf("code = %d, want 200", rec.Code)
	}
	if got := decodeHealth(t, rec).Status; got != "warn" {
		t.Errorf("status = %q, want warn", got)
	}
}

func TestHealth_Uptime(t *testing.T) {
	h := newTestHandler(t, Ready, Info{Started: time.Now().Add(-90 * time.Second)})

	up := decodeHealth(t, get(t, h, "/health")).Checks["uptime"]

	if len(up) != 1 || up[0].Status != "pass" || up[0].ObservedUnit != "s" || up[0].ObservedValue < 90 {
		t.Errorf("uptime = %+v, want a pass entry observing at least 90 s", up)
	}
}

func TestHealth_Draining(t *testing.T) {
	rec := get(t, newTestHandler(t, Draining, Info{}), "/health")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rec.Code)
	}
	doc := decodeHealth(t, rec)
	if doc.Status != "fail" {
		t.Errorf("status = %q, want fail", doc.Status)
	}
	if lc := doc.Checks["lifecycle"]; len(lc) != 1 || lc[0].Status != "fail" || lc[0].Output != "draining" {
		t.Errorf("lifecycle = %+v, want a fail entry with output draining", lc)
	}
}

func TestMetricsRoute(t *testing.T) {
	rec := get(t, newTestHandler(t, Ready, Info{}), "/metrics")

	if rec.Code != http.StatusOK || rec.Body.String() != "metrics-ok" {
		t.Errorf("got %d %q, want 200 \"metrics-ok\"", rec.Code, rec.Body.String())
	}
}

func TestOnlyFourRoutes(t *testing.T) {
	h := newTestHandler(t, Ready, Info{})
	for _, path := range []string{"/ping", "/ready", "/livez"} {
		t.Run(path, func(t *testing.T) {
			if rec := get(t, h, path); rec.Code != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404", path, rec.Code)
			}
		})
	}
}
