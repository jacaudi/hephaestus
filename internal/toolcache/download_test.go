package toolcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/catalog/catalogtest"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// newDownloader is a downloader over a fresh spool directory and a client that
// trusts up, with the state a job would own.
func newDownloader(t *testing.T, up *upstream) (*downloader, *harness) {
	t.Helper()
	client := NewHTTPClient(testUserAgent, poolOf(up.srv))
	t.Cleanup(client.CloseIdleConnections)
	h := newHarness(t, catalogtest.New(t, "https://example.test").Catalog, client)
	root, err := os.OpenRoot(h.spool)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("close the spool root: %v", err)
		}
	})
	d := &downloader{
		f: h.f, root: root, downloaded: new(int64), reserved: new(uint64),
		bytes: h.f.bytes, progress: func(context.Context, int64, int64) {},
	}
	return d, h
}

// testBody is n bytes that differ from place to place, so a splice of two
// versions shows in a hash.
func testBody(n int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "block %d;", i)
	}
	return b.Bytes()[:n]
}

// spoolOf reads the spool file name of h, failing when it is not there.
func spoolOf(t *testing.T, h *harness, name string) []byte {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(h.spool, name))
	if err != nil {
		t.Fatalf("read the spool %s: %v", name, err)
	}
	return got
}

// seedHalfOf writes the first half of data as the spool name and etag as its sidecar.
func seedHalfOf(t *testing.T, h *harness, name string, data []byte, etag string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.spool, name), data[:len(data)/2], 0o600); err != nil {
		t.Fatalf("seed the spool: %v", err)
	}
	if err := os.WriteFile(filepath.Join(h.spool, name+".etag"), []byte(etag), 0o600); err != nil {
		t.Fatalf("seed the sidecar: %v", err)
	}
}

func TestResponseSize(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		length  int64
		rng     string
		want    int64
		wantErr bool
	}{
		{name: "200 with a length", status: http.StatusOK, length: 10, want: 10},
		{name: "206 with a total", status: http.StatusPartialContent, length: 5, rng: "bytes 5-9/10", want: 10},
		{name: "200 of unknown length", status: http.StatusOK, length: -1, wantErr: true},
		{name: "200 of no bytes", status: http.StatusOK, length: 0, wantErr: true},
		{name: "206 of unknown total", status: http.StatusPartialContent, length: 5, rng: "bytes 5-9/*", wantErr: true},
		{name: "206 with no Content-Range", status: http.StatusPartialContent, length: 5, wantErr: true},
		{name: "404", status: http.StatusNotFound, length: 10, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tt.status, ContentLength: tt.length, Header: http.Header{}}
			if tt.rng != "" {
				resp.Header.Set("Content-Range", tt.rng)
			}

			got, err := responseSize(resp)

			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("responseSize = %d, %v; want %d, error: %t", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestDownloader_UnknownSizeReservesAfterHeaders(t *testing.T) {
	up := newUpstream(t)
	data := testBody(4096)
	up.handle(serveObject(data))
	d, h := newDownloader(t, up)
	var (
		mu     sync.Mutex
		seenAt = -1 // requests the upstream had when statfs was first asked
	)
	h.f.statfs = func(string) (uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		if seenAt < 0 {
			seenAt = len(up.seen())
		}
		return 1 << 40, nil
	}
	o := object{URL: up.srv.URL + "/obj", SpoolName: "obj-0", Max: 1 << 20}

	if err := d.fetch(t.Context(), o); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if seenAt < 1 {
		t.Errorf("statfs was first asked after %d requests, want at least 1: the size is known from the headers", seenAt)
	}
	if got := spoolOf(t, h, o.SpoolName); !bytes.Equal(got, data) {
		t.Errorf("the spool holds %d bytes that are not the body's %d", len(got), len(data))
	}
	if want := 2 * uint64(len(data)); *d.reserved != want {
		t.Errorf("reserved = %d, want %d: two copies of the size the headers gave", *d.reserved, want)
	}
}

func TestDownloader_UnknownSizeOverMaxIsIntegrity(t *testing.T) {
	const limit = 1 << 10
	up := newUpstream(t)
	up.handle(serveObject(testBody(limit + 1)))
	d, h := newDownloader(t, up)
	asked := false
	h.f.statfs = func(string) (uint64, error) { asked = true; return 1 << 40, nil }
	o := object{URL: up.srv.URL + "/obj", SpoolName: "obj-0", Max: limit}

	err := d.fetch(t.Context(), o)

	if _, ok := errors.AsType[*IntegrityError](err); !ok {
		t.Errorf("fetch = %v, want an *IntegrityError", err)
	}
	if asked || *d.reserved != 0 || h.f.reserved != 0 {
		t.Errorf("space asked for: %t, job holds %d, pool holds %d; want nothing reserved", asked, *d.reserved, h.f.reserved)
	}
}

func TestDownloader_UnknownSizeResumeFromContentRange(t *testing.T) {
	up := newUpstream(t)
	data := testBody(4096)
	up.handle(serveObject(data))
	d, h := newDownloader(t, up)
	o := object{URL: up.srv.URL + "/obj", SpoolName: "obj-0", Max: 1 << 20}
	seedHalfOf(t, h, o.SpoolName, data, testETag)

	if err := d.fetch(t.Context(), o); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	seen := up.seen()
	if want := fmt.Sprintf("bytes=%d-", len(data)/2); len(seen) != 1 || seen[0].Range != want || seen[0].IfRange != testETag {
		t.Errorf("requests = %+v, want one with Range %q and If-Range %q", seen, want, testETag)
	}
	if got := spoolOf(t, h, o.SpoolName); !bytes.Equal(got, data) {
		t.Errorf("the spool holds %d bytes that are not the body's %d", len(got), len(data))
	}
	if want := 2 * uint64(len(data)); *d.reserved != want {
		t.Errorf("reserved = %d, want %d: two copies of the total in Content-Range", *d.reserved, want)
	}
}

func TestDownloader_UnpinnedVerifyAndDigest(t *testing.T) {
	data := testBody(300)
	tests := []struct {
		name    string
		size    int64
		wantErr bool
	}{
		{name: "the size matches", size: int64(len(data))},
		{name: "the size is unknown", size: 0},
		{name: "the size differs", size: int64(len(data)) + 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, h := newDownloader(t, newUpstream(t))
			if err := os.WriteFile(filepath.Join(h.spool, "obj-0"), data, 0o600); err != nil {
				t.Fatalf("write the spool: %v", err)
			}
			o := object{SpoolName: "obj-0", Size: tt.size}

			err := d.verify(t.Context(), o)

			if _, ok := errors.AsType[*IntegrityError](err); ok != tt.wantErr || (err != nil) != tt.wantErr {
				t.Errorf("verify = %v, want an *IntegrityError: %t", err, tt.wantErr)
			}
			sum, size, err := d.digest(t.Context(), o)
			if err != nil || sum != sha(data) || size != int64(len(data)) {
				t.Errorf("digest = %q, %d, %v; want %q, %d, nil", sum, size, err, sha(data), len(data))
			}
		})
	}
}

func TestDownloader_StatusError(t *testing.T) {
	tests := []struct {
		name       string
		code       int
		body       string
		wantPrefix string
		wantExact  string
	}{
		{
			name: "404 with a long body", code: http.StatusNotFound, body: "unknown schematic\x1b[2J" + strings.Repeat("x", 2<<10),
			wantPrefix: "the upstream answered 404 Not Found: unknown schematic",
		},
		{name: "503 with no body", code: http.StatusServiceUnavailable, wantExact: "the upstream answered 503 Service Unavailable"},
		{name: "a status with no text", code: 599, body: "odd", wantExact: "the upstream answered 599: odd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := newUpstream(t)
			up.handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.code)
				send(t, w, []byte(tt.body))
			}))
			d, _ := newDownloader(t, up)
			o := object{URL: up.srv.URL + "/obj", SpoolName: "obj-0", Size: 10, SHA256: strings.Repeat("0", 64)}

			err := d.fetch(t.Context(), o)

			se, ok := errors.AsType[*StatusError](err)
			if !ok || se.Code != tt.code {
				t.Fatalf("fetch = %v, want a *StatusError with Code %d", err, tt.code)
			}
			if len(se.Body) > fleet.MaxCauseBytes || fleet.PrintableText(se.Body) != se.Body {
				t.Errorf("Body is %d bytes, printable: %t; want at most %d and printable", len(se.Body), fleet.PrintableText(se.Body) == se.Body, fleet.MaxCauseBytes)
			}
			got := se.Error()
			if tt.wantExact != "" && got != tt.wantExact || !strings.HasPrefix(got, tt.wantPrefix) {
				t.Errorf("Error() = %q, want exactly %q or prefix %q", got, tt.wantExact, tt.wantPrefix)
			}
		})
	}
}

// otherVersionAfterRange serves a 206 under ETag "B" to a ranged request, with
// the second half of other bytes, and the whole object under "B" otherwise.
func otherVersionAfterRange(t *testing.T, data []byte) http.Handler {
	t.Helper()
	other := bytes.Clone(data)
	for i := len(other) / 2; i < len(other); i++ {
		other[i] ^= 0xff
	}
	half := len(data) / 2
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"B"`)
		if r.Header.Get("Range") == "" {
			send(t, w, data)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		send(t, w, other[half:])
	})
}

func TestDownloader_RestartIsOnePerAttempt(t *testing.T) {
	data := testBody(4096)
	up := newUpstream(t)
	up.handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A 206 under another ETag, and one that starts at the half even for a
		// request that asks for no range, so the restart cannot succeed.
		w.Header().Set("ETag", `"B"`)
		start := len(data) / 2
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		send(t, w, data[start:])
	}))
	d, h := newDownloader(t, up)
	o := object{URL: up.srv.URL + "/obj", SpoolName: "obj-0", Size: int64(len(data)), SHA256: sha(data)}
	seedHalfOf(t, h, o.SpoolName, data, `"A"`)

	err := d.fetch(t.Context(), o)

	if err == nil {
		t.Fatal("fetch = nil, want the second mismatch to be an error")
	}
	if _, ok := errors.AsType[*IntegrityError](err); ok || errors.Is(err, errRestart) {
		t.Errorf("fetch = %v, want an ordinary error: neither an *IntegrityError nor a third try", err)
	}
	if seen := up.seen(); len(seen) != 2 || seen[1].Range != "" {
		t.Errorf("requests = %+v, want the ranged one and one restart with no Range", seen)
	}
}

func TestDownloader_RangeNotSatisfiableRestarts(t *testing.T) {
	data := testBody(4096)
	up := newUpstream(t)
	up.handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", testETag)
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(data)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		send(t, w, data)
	}))
	d, h := newDownloader(t, up)
	// A whole spool of an object whose size nobody knows: the range starts at its end.
	o := object{URL: up.srv.URL + "/obj", SpoolName: "obj-0", Max: 1 << 20}
	if err := os.WriteFile(filepath.Join(h.spool, o.SpoolName), data, 0o600); err != nil {
		t.Fatalf("seed the spool: %v", err)
	}
	if err := os.WriteFile(filepath.Join(h.spool, o.SpoolName+".etag"), []byte(testETag), 0o600); err != nil {
		t.Fatalf("seed the sidecar: %v", err)
	}

	if err := d.fetch(t.Context(), o); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if got := spoolOf(t, h, o.SpoolName); !bytes.Equal(got, data) {
		t.Errorf("the spool holds %d bytes that are not the body's %d", len(got), len(data))
	}
	if seen := up.seen(); len(seen) != 2 || seen[0].Range == "" || seen[1].Range != "" {
		t.Errorf("requests = %+v, want a ranged one and a restart with no Range", seen)
	}
}

// TestDownloader_ErrorsNameTheObject pins the object number in the error text
// for an object that is not the first: it is stored as the row's last_error.
func TestDownloader_ErrorsNameTheObject(t *testing.T) {
	data := testBody(4096)
	tests := []struct {
		name  string
		serve func(t *testing.T, w http.ResponseWriter)
		url   string // replaces the upstream's, when set
		want  string
	}{
		{name: "the request fails", url: "https://127.0.0.1:1/obj", want: "get object 1:"},
		{
			name: "the body breaks off", want: "copy object 1:",
			serve: func(t *testing.T, w http.ResponseWriter) {
				t.Helper()
				hijackSend(t, w, fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(data), data[:len(data)/2]))
			},
		},
		{
			name: "the body ends short", want: "the body of object 1 ended at byte",
			serve: func(t *testing.T, w http.ResponseWriter) {
				t.Helper()
				hijackSend(t, w, "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n"+string(data[:len(data)/2]))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := newUpstream(t)
			up.handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { tt.serve(t, w) }))
			d, _ := newDownloader(t, up)
			o := object{URL: up.srv.URL + "/obj", SpoolName: "obj-1", Size: int64(len(data)), SHA256: sha(data), Index: 1}
			if tt.url != "" {
				o.URL = tt.url
			}

			err := d.fetch(t.Context(), o)

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("fetch = %v, want text containing %q", err, tt.want)
			}
		})
	}
}

// TestDownloader_StatusBodyThatNeverComesIsCutByTheIdleTimeout pins that the
// error body is read under the same idle bound as a success body: the client
// has no total timeout, so a header followed by silence would hold the worker.
func TestDownloader_StatusBodyThatNeverComesIsCutByTheIdleTimeout(t *testing.T) {
	up := newUpstream(t)
	up.handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "512")
		w.WriteHeader(http.StatusServiceUnavailable)
		flush(t, w)
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	d, h := newDownloader(t, up)
	h.f.idle = 100 * time.Millisecond
	o := object{URL: up.srv.URL + "/obj", SpoolName: "obj-0", Size: 10, SHA256: strings.Repeat("0", 64)}
	done := make(chan error, 1)

	go func() { done <- d.fetch(t.Context(), o) }()

	select {
	case err := <-done:
		if se, ok := errors.AsType[*StatusError](err); !ok || se.Code != http.StatusServiceUnavailable {
			t.Errorf("fetch = %v, want a *StatusError with Code 503", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fetch did not return: the error body is read with no idle bound")
	}
}

// TestDownloader_FullRange206WithoutRangeIsAccepted pins that an upstream that
// answers every request with a 206 of the whole object still serves a fetch
// that asked for no range: at offset 0 the spool is overwritten, so nothing
// can be spliced.
func TestDownloader_FullRange206WithoutRangeIsAccepted(t *testing.T) {
	data := testBody(4096)
	up := newUpstream(t)
	up.handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", testETag)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(data)-1, len(data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusPartialContent)
		send(t, w, data)
	}))
	d, h := newDownloader(t, up)
	o := object{URL: up.srv.URL + "/obj", SpoolName: "obj-0", Size: int64(len(data)), SHA256: sha(data)}

	if err := d.fetch(t.Context(), o); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if got := spoolOf(t, h, o.SpoolName); !bytes.Equal(got, data) {
		t.Errorf("the spool holds %d bytes that are not the body's %d", len(got), len(data))
	}
	if got := spoolOf(t, h, o.SpoolName+".etag"); string(got) != testETag {
		t.Errorf("sidecar = %q, want %q: a later resume is checked against this response", got, testETag)
	}
	if seen := up.seen(); len(seen) != 1 {
		t.Errorf("requests = %+v, want one", seen)
	}
}

// TestDownloader_UnknownSize206WithoutTotalRestarts pins that a ranged
// response whose Content-Range gives no usable total is a fault of that
// response, not of the object: the spool restarts from zero and the 200 that
// follows is fetched, with no integrity failure.
func TestDownloader_UnknownSize206WithoutTotalRestarts(t *testing.T) {
	tests := []struct {
		name string
		rng  func(half, n int) string
	}{
		{name: "an unknown total", rng: func(half, n int) string { return fmt.Sprintf("bytes %d-%d/*", half, n-1) }},
		{name: "no Content-Range", rng: func(int, int) string { return "" }},
		{name: "a malformed Content-Range", rng: func(int, int) string { return "bytes nonsense" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := testBody(4096)
			half := len(data) / 2
			up := newUpstream(t)
			up.handle(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", testETag)
				if r.Header.Get("Range") == "" {
					w.Header().Set("Content-Length", strconv.Itoa(len(data)))
					send(t, w, data)
					return
				}
				if rng := tt.rng(half, len(data)); rng != "" {
					w.Header().Set("Content-Range", rng)
				}
				w.WriteHeader(http.StatusPartialContent)
				send(t, w, data[half:])
			}))
			d, h := newDownloader(t, up)
			o := object{URL: up.srv.URL + "/obj", SpoolName: "obj-0", Max: 1 << 20}
			seedHalfOf(t, h, o.SpoolName, data, testETag)

			if err := d.fetch(t.Context(), o); err != nil {
				t.Fatalf("fetch = %v, want the restart to succeed", err)
			}

			if got := spoolOf(t, h, o.SpoolName); !bytes.Equal(got, data) {
				t.Errorf("the spool holds %d bytes that are not the body's %d", len(got), len(data))
			}
			if seen := up.seen(); len(seen) != 2 || seen[0].Range == "" || seen[1].Range != "" {
				t.Errorf("requests = %+v, want a ranged one and a restart with no Range", seen)
			}
		})
	}
}

// TestDownloader_UnknownSize206WithoutTotalOnFirstRequestIsOrdinary pins that
// the restart is only for a request that sent a Range: a first request
// answered by a 206 with no total cannot be mended by asking the same again.
func TestDownloader_UnknownSize206WithoutTotalOnFirstRequestIsOrdinary(t *testing.T) {
	data := testBody(4096)
	up := newUpstream(t)
	up.handle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/*", len(data)-1))
		w.WriteHeader(http.StatusPartialContent)
		send(t, w, data)
	}))
	d, _ := newDownloader(t, up)
	o := object{URL: up.srv.URL + "/obj", SpoolName: "obj-0", Max: 1 << 20}

	err := d.fetch(t.Context(), o)

	if err == nil {
		t.Fatal("fetch = nil, want an error")
	}
	if _, ok := errors.AsType[*IntegrityError](err); ok || errors.Is(err, errRestart) {
		t.Errorf("fetch = %v, want an ordinary error: neither an *IntegrityError nor errRestart", err)
	}
	if seen := up.seen(); len(seen) != 1 {
		t.Errorf("requests = %+v, want one", seen)
	}
}
