package telemetry

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// serveWide runs one request through WideEvents and returns the decoded log lines.
func serveWide(t *testing.T, target string, h http.HandlerFunc) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	req.RemoteAddr = "10.10.20.50:41000"
	WideEvents(h, logger, slog.LevelInfo).ServeHTTP(httptest.NewRecorder(), req)

	var lines []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		lines = append(lines, m)
	}
	return lines
}

func TestWideEvents_OneLinePerRequest(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		AddAttrs(r.Context(), slog.String("decision", "hold"))
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("abc"))
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/boot?uuid=1&t=secret-ticket", nil)
	req.RemoteAddr = "10.10.20.50:41000"
	WideEvents(h, logger, slog.LevelInfo).ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if strings.Contains(out, "secret-ticket") || strings.Contains(out, "uuid=1") {
		t.Errorf("log line leaks the query string: %s", out)
	}
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 1 {
		t.Fatalf("got %d log lines, want 1:\n%s", n, out)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, out)
	}
	want := map[string]any{
		"msg":       "request",
		"method":    "GET",
		"path":      "/boot",
		"query":     "[redacted]",
		"status":    float64(http.StatusAccepted),
		"bytes":     float64(3),
		"remote_ip": "10.10.20.50",
		"tls":       false,
		"decision":  "hold",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if d, ok := got["duration_ms"].(float64); !ok || d < 0 {
		t.Errorf("duration_ms = %v, want a number >= 0", got["duration_ms"])
	}
}

func TestWideEvents_NoQueryKeyWithoutQuery(t *testing.T) {
	lines := serveWide(t, "/start", func(http.ResponseWriter, *http.Request) {})
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1", len(lines))
	}
	if _, ok := lines[0]["query"]; ok {
		t.Errorf("query key present without a query string: %v", lines[0])
	}
}

func TestWideEvents_DefaultStatus200(t *testing.T) {
	lines := serveWide(t, "/start", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("x"))
	})
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1", len(lines))
	}
	if got := lines[0]["status"]; got != float64(http.StatusOK) {
		t.Errorf("status = %v, want %d", got, http.StatusOK)
	}
}

func TestWideEvents_ResponseControllerReachesConn(t *testing.T) {
	deadlineErr := make(chan error, 1)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		deadlineErr <- http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Second))
		_, _ = w.Write([]byte("ok"))
	})
	srv := httptest.NewServer(WideEvents(h, slog.New(slog.DiscardHandler), slog.LevelInfo))
	defer srv.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	if err := <-deadlineErr; err != nil {
		t.Errorf("SetWriteDeadline through the wrapper: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("response = %d %q, want 200 \"ok\"", resp.StatusCode, body)
	}
}

func TestAddAttrs_NoEventIsNoop(t *testing.T) {
	AddAttrs(t.Context(), slog.Int("x", 1))
}
