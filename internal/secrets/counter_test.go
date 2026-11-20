package secrets

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// okTransport answers every request with 204 and keeps no connection.
type okTransport struct{}

func (okTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: r}, nil
}

// send makes one request to url through c and closes the response.
func send(t *testing.T, c *Counter, url string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	resp, err := c.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip %s: %v", url, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
}

func TestCounter_CountsOnlyOnePasswordHosts(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		url  string
		want int
	}{
		{"https://my.1password.com/api/v1/x", 1},
		{"https://my.1password.ca/api/v1/x", 1},
		{"https://x.1password.eu/api/v1/x", 1},
		{"https://MY.1Password.COM:443/api/v1/x", 1},
		{"https://factory.talos.dev/schematics", 0},
		{"https://1password.com.evil.test/api/v1/x", 0},
		{"https://evil.test/my.1password.com", 0},
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			c := NewCounter(okTransport{}, func() time.Time { return t0 })
			send(t, c, tc.url)
			if got := c.Last24h(); got != tc.want {
				t.Errorf("Last24h = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCounter_RollingWindow(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	now := t0
	c := NewCounter(okTransport{}, func() time.Time { return now })

	for range 3 {
		send(t, c, "https://my.1password.com/x")
	}
	now = t0.Add(23 * time.Hour)
	for range 2 {
		send(t, c, "https://my.1password.com/x")
	}
	if got := c.Last24h(); got != 5 {
		t.Errorf("Last24h at t0+23h = %d, want 5", got)
	}

	now = t0.Add(24*time.Hour + time.Second)
	if got := c.Last24h(); got != 2 {
		t.Errorf("Last24h at t0+24h+1s = %d, want 2", got)
	}
}

func TestCounter_DelegatesToNext(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	c := NewCounter(okTransport{}, func() time.Time { return t0 })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "https://my.1password.com/x", nil)
	resp, err := c.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want the next transport's %d", resp.StatusCode, http.StatusNoContent)
	}
}
