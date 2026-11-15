package telemetry

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"
)

type eventKey struct{}

// event collects the attributes a handler adds to its request's log line.
type event struct {
	mu    sync.Mutex
	attrs []slog.Attr
}

// AddAttrs appends attrs to the wide event of the request that ctx belongs
// to. It does nothing when ctx carries no event, and is safe for concurrent
// use.
func AddAttrs(ctx context.Context, attrs ...slog.Attr) {
	ev, ok := ctx.Value(eventKey{}).(*event)
	if !ok {
		return
	}
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.attrs = append(ev.attrs, attrs...)
}

// WideEvents logs one "request" record at level per request, after next
// returns. The query string is never logged because it carries boot tickets;
// the record only notes that one was present. Handlers add their own
// attributes with AddAttrs.
func WideEvents(next http.Handler, logger *slog.Logger, level slog.Level) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ev := &event{}
		ctx := context.WithValue(r.Context(), eventKey{}, ev)
		rw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(rw, r.WithContext(ctx))

		remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			remoteIP = r.RemoteAddr
		}
		attrs := []slog.Attr{slog.String("method", r.Method), slog.String("path", r.URL.Path)}
		if r.URL.RawQuery != "" {
			attrs = append(attrs, slog.String("query", "[redacted]"))
		}
		attrs = append(attrs,
			slog.Int("status", rw.status),
			slog.Int("bytes", rw.bytes),
			slog.Float64("duration_ms", float64(time.Since(start))/float64(time.Millisecond)),
			slog.String("remote_ip", remoteIP),
			slog.Bool("tls", r.TLS != nil),
		)
		ev.mu.Lock()
		attrs = append(attrs, ev.attrs...)
		ev.mu.Unlock()
		logger.LogAttrs(ctx, level, "request", attrs...)
	})
}

// statusWriter records the status code and body size a handler produced.
type statusWriter struct {
	http.ResponseWriter

	status      int
	bytes       int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying connection.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
