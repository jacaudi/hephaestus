package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestMetrics(t *testing.T) *Metrics {
	t.Helper()
	m, err := NewMetrics("v1.2.3", "abc1234")
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

// scrape returns the Prometheus exposition body served by m.Handler.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestNewMetrics_BuildInfoAndRuntime(t *testing.T) {
	body := scrape(t, newTestMetrics(t))

	for _, want := range []string{
		"hephaestus_build_info{",
		`version="v1.2.3"`,
		`commit="abc1234"`,
		"go_goroutine_count",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body missing %q:\n%s", want, body)
		}
	}
}

func TestInstrument_RecordsRED(t *testing.T) {
	m := newTestMetrics(t)
	h := m.Instrument(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}), "boot")

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/boot", nil))

	if body := scrape(t, m); !strings.Contains(body, "http_server_request_duration_seconds") {
		t.Errorf("metrics body missing http_server_request_duration_seconds:\n%s", body)
	}
}
