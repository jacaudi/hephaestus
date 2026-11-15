package telemetry

import (
	"context"
	"crypto/tls"
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
	}), "boot", "10.10.20.5:80")

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/boot", nil))

	if body := scrape(t, m); !strings.Contains(body, "http_server_request_duration_seconds") {
		t.Errorf("metrics body missing http_server_request_duration_seconds:\n%s", body)
	}
}

// TestInstrument_LabelsIgnoreHostHeader pins that the server labels come from
// the listener the handler serves, never from the client's Host header, so a
// client cannot mint metric series.
func TestInstrument_LabelsIgnoreHostHeader(t *testing.T) {
	tests := []struct {
		name   string
		host   string
		secure bool
	}{
		{"unknown host and port", "random-host-1.example:8123", false},
		{"unknown host, no port", "random-host-2.example", false},
		{"unknown host over TLS", "random-host-3.example:8444", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestMetrics(t)
			h := m.Instrument(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("ok"))
			}), "boot", "10.10.20.5:443")
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/boot", nil)
			req.Host = tc.host
			if tc.secure {
				req.TLS = &tls.ConnectionState{}
			}

			h.ServeHTTP(httptest.NewRecorder(), req)

			body := scrape(t, m)
			if !strings.Contains(body, `server_address="10.10.20.5"`) {
				t.Errorf("metrics body missing the listener's server_address:\n%s", body)
			}
			host, _, _ := strings.Cut(tc.host, ":")
			for _, unwanted := range []string{host, "8123", "8444"} {
				if strings.Contains(body, unwanted) {
					t.Errorf("metrics body contains the client-supplied %q:\n%s", unwanted, body)
				}
			}
		})
	}
}
