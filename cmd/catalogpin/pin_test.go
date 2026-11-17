package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/toolcache"
)

const (
	testFPR  = "54C0821A48715DAFD61BFCAF667857D045599AFD"
	otherFPR = "0FF11AF081E98345594812037091115F8320B897"
)

// member is one file of a test zip.
type member struct {
	name string
	data []byte
}

func makeZip(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		w, err := zw.Create(m.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(m.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sha1Hex(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

func serveBytes(b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(b) }
}

// counted wraps h so the test can read how many requests reached it.
func counted(h http.HandlerFunc) (http.Handler, *atomic.Int32) {
	var n atomic.Int32
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}), &n
}

// startServer starts an https test server. It requires TLS 1.3, or is limited
// to TLS 1.2 when tls12 is set, to prove the pin tool refuses it.
func startServer(t *testing.T, h http.Handler, tls12 bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	if tls12 {
		srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// testClient is the production upstream client trusting the test servers' certificate.
func testClient(t *testing.T, srvs ...*httptest.Server) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	for _, s := range srvs {
		pool.AddCert(s.Certificate())
	}
	return toolcache.NewHTTPClient("catalogpin-test", pool)
}

func hostOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

// fakeGPG stands in for gpg: verify and keyPrimary return fixed answers and
// verify records what it was asked to check.
type fakeGPG struct {
	signer     string
	verifyErr  error
	primary    string
	primaryErr error

	verifies   int
	keyFile    string
	signedData string
}

func (g *fakeGPG) verify(_ context.Context, keyFile, _, dataFile string) (string, error) {
	g.verifies++
	g.keyFile = keyFile
	b, err := os.ReadFile(dataFile)
	if err != nil {
		return "", err
	}
	g.signedData = string(b)
	return g.signer, g.verifyErr
}

func (g *fakeGPG) keyPrimary(context.Context, string) (string, error) {
	return g.primary, g.primaryErr
}

// newPinner returns a pinner over fresh work and keys directories; keys holds
// test.asc. Its provenance is empty and it waits nothing between attempts.
func newPinner(t *testing.T, client *http.Client, g *fakeGPG) *pinner {
	t.Helper()
	workDir, keysDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(keysDir, "test.asc"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	work, err := os.OpenRoot(workDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { work.Close() })
	keys, err := os.OpenRoot(keysDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { keys.Close() })
	return &pinner{
		client: client, work: work, keys: keys, provenance: map[string]bool{},
		verify: g.verify, keyPrimary: g.keyPrimary,
	}
}

func assertWorkEmpty(t *testing.T, p *pinner) {
	t.Helper()
	entries, err := os.ReadDir(p.work.Name())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("work directory still holds %q", e.Name())
	}
}

// assertReason checks that err is an absent reason that starts with prefix
// and holds every text in contains.
func assertReason(t *testing.T, err error, prefix string, contains ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("pinSpec succeeded, want an error starting %q", prefix)
	}
	got := err.Error()
	if !strings.HasPrefix(got, prefix) {
		t.Errorf("reason = %q, want prefix %q", got, prefix)
	}
	for _, c := range contains {
		if !strings.Contains(got, c) {
			t.Errorf("reason = %q, want it to hold %q", got, c)
		}
	}
}

// zipSpec is a TOFU spec for one zip object holding files; the first is its kernel.
func zipSpec(entry, objURL string, files ...string) catalog.Spec {
	return catalog.Spec{
		Entry: entry, Arch: "amd64", Version: "1", Recipe: catalog.RecipeKernel,
		Platforms: []catalog.Platform{catalog.PlatformEFI},
		Objects: []catalog.ObjectSpec{{
			URL: objURL, Container: catalog.ContainerZip,
			Signature: catalog.Signature{TOFU: true}, Files: files,
		}},
		Boot: catalog.Boot{Kernel: files[0]},
	}
}

// flush sends what the handler has written so far.
func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// hangUp flushes w and closes its connection, so a body declared longer than
// the bytes written ends short.
func hangUp(w http.ResponseWriter) {
	flush(w)
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			conn.Close()
		}
	}
}

func hopLines(lines []string) []string {
	return slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return !strings.HasPrefix(l, "hop ") })
}

func TestPin_TOFUZip(t *testing.T) {
	alpha, bravo := []byte("alpha-bytes"), []byte("bravo")
	body := makeZip(t, member{"bin/a.efi", alpha}, member{"b.txt", bravo})
	srv := startServer(t, serveBytes(body), false)
	g := &fakeGPG{}
	p := newPinner(t, testClient(t, srv), g)

	pin, _, err := p.pinSpec(t.Context(), zipSpec("tool", srv.URL+"/tool.zip", "bin/a.efi", "b.txt"))
	if err != nil {
		t.Fatalf("pinSpec: %v", err)
	}
	if len(pin.Objects) != 1 {
		t.Fatalf("pin has %d objects, want 1", len(pin.Objects))
	}
	o := pin.Objects[0]
	if o.URL != srv.URL+"/tool.zip" || o.Size != int64(len(body)) || o.SHA256 != sha256Hex(body) || o.Signer != "" {
		t.Errorf("object = %+v, want the zip's url, size %d, sha256 %s and no signer", o, len(body), sha256Hex(body))
	}
	wantFiles := []catalog.FilePin{
		{Path: "bin/a.efi", Size: int64(len(alpha)), SHA256: sha256Hex(alpha)},
		{Path: "b.txt", Size: int64(len(bravo)), SHA256: sha256Hex(bravo)},
	}
	if !slices.Equal(o.Files, wantFiles) {
		t.Errorf("files = %+v, want %+v", o.Files, wantFiles)
	}
	if want := catalog.PinID(pin.Objects); pin.ID != want {
		t.Errorf("ID = %q, want PinID(objects) = %q", pin.ID, want)
	}
	if g.verifies != 0 {
		t.Errorf("verify ran %d times for a TOFU spec", g.verifies)
	}
	assertWorkEmpty(t, p)
}

func TestPin_RefusesTLS12Hop(t *testing.T) {
	srv := startServer(t, serveBytes(makeZip(t, member{"f.bin", []byte("x")})), true)
	p := newPinner(t, testClient(t, srv), &fakeGPG{})

	_, _, err := p.pinSpec(t.Context(), zipSpec("tool", srv.URL+"/tool.zip", "f.bin"))

	assertReason(t, err, "tls13: ", hostOf(t, srv))
}

func TestPin_RefusesTLS12RedirectTarget(t *testing.T) {
	old := startServer(t, serveBytes(makeZip(t, member{"f.bin", []byte("x")})), true)
	front := startServer(t, http.RedirectHandler(old.URL+"/tool.zip", http.StatusFound), false)
	p := newPinner(t, testClient(t, front, old), &fakeGPG{})

	_, _, err := p.pinSpec(t.Context(), zipSpec("tool", front.URL+"/tool.zip", "f.bin"))

	assertReason(t, err, "tls13: ", hostOf(t, old))
}

func TestPin_RecordsHops(t *testing.T) {
	back := startServer(t, serveBytes(makeZip(t, member{"f.bin", []byte("x")})), false)
	front := startServer(t, http.RedirectHandler(back.URL+"/final.zip", http.StatusFound), false)
	p := newPinner(t, testClient(t, front, back), &fakeGPG{})

	_, lines, err := p.pinSpec(t.Context(), zipSpec("tool", front.URL+"/tool.zip", "f.bin"))
	if err != nil {
		t.Fatalf("pinSpec: %v", err)
	}

	hops := hopLines(lines)
	if len(hops) != 2 {
		t.Fatalf("report has %d hop lines, want 2:\n%s", len(hops), strings.Join(lines, "\n"))
	}
	for i, want := range []struct{ url, status string }{
		{front.URL + "/tool.zip", "status=302"},
		{back.URL + "/final.zip", "status=200"},
	} {
		for _, text := range []string{want.url, want.status, "tls=0x0304"} {
			if !strings.Contains(hops[i], text) {
				t.Errorf("hop line %d = %q, want it to hold %q", i+1, hops[i], text)
			}
		}
	}
}

// fakeTransport answers every request with a 200 whose connection state says
// it negotiated version (nil state when version is 0), bypassing net/http's
// own length and version checks.
func fakeTransport(version uint16, contentLength int64, body string) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp := &http.Response{
			Status: "200 OK", StatusCode: http.StatusOK, Header: http.Header{}, Request: r,
			ContentLength: contentLength, Body: io.NopCloser(strings.NewReader(body)),
		}
		if version != 0 {
			resp.TLS = &tls.ConnectionState{Version: version}
		}
		return resp, nil
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPin_RefusesCompletedHopBelowTLS13(t *testing.T) {
	tests := []struct {
		name    string
		version uint16
	}{
		{"TLS 1.2", tls.VersionTLS12},
		{"no TLS", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newPinner(t, fakeTransport(tt.version, 4, "data"), &fakeGPG{})

			_, _, err := p.pinSpec(t.Context(), zipSpec("tool", "https://mirror.example.test/tool.zip", "f.bin"))

			assertReason(t, err, "tls13: ", "mirror.example.test")
		})
	}
}

func TestPin_ShortSizeIsIntegrity(t *testing.T) {
	// A body shorter than its Content-Length that ends cleanly cannot come from
	// net/http, which reports it as io.ErrUnexpectedEOF; the pin tool checks anyway.
	p := newPinner(t, fakeTransport(tls.VersionTLS13, 10, "12345"), &fakeGPG{})

	_, _, err := p.pinSpec(t.Context(), zipSpec("tool", "https://mirror.example.test/tool.zip", "f.bin"))

	assertReason(t, err, "integrity: ")
}

func TestClassifyFetchError(t *testing.T) {
	alert70 := &net.OpError{Op: "remote error", Err: errors.New("tls: protocol version not supported")}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"received alert 70 over TCP", alert70, "tls13"},
		{"received alert 70 inside a url.Error", &url.Error{Op: "Get", URL: "https://h/", Err: alert70}, "tls13"},
		{"tls.AlertError 70", tls.AlertError(70), "tls13"},
		{"the server selected TLS 1.2", fmt.Errorf("tls: server selected unsupported protocol version %x", 0x303), "tls13"},
		{"another received alert", &net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")}, "network"},
		{"another tls.AlertError", tls.AlertError(40), "network"},
		{"connection refused", errors.New("dial tcp: connect: connection refused"), "network"},
		{"redirect off https", &toolcache.IntegrityError{Err: errors.New("redirect to http")}, "tls13"},
		{"the phrase only in the URL", &url.Error{Op: "Get", URL: "https://h/tls: server selected unsupported protocol version", Err: errors.New("connection refused")}, "network"},
		{"the phrase in the cause of a url.Error", &url.Error{Op: "Get", URL: "https://h/", Err: errors.New("tls: server selected unsupported protocol version 303")}, "tls13"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyFetchError(tt.err); got != tt.want {
				t.Errorf("classifyFetchError = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPin_AbsentReasonClasses(t *testing.T) {
	good := makeZip(t, member{"f.bin", []byte("payload")})
	plain := func(base string) catalog.Spec { return zipSpec("tool", base+"/obj.zip", "f.bin") }
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { http.Error(w, http.StatusText(code), code) }
	}
	signed := func(key string) func(string) catalog.Spec {
		return func(base string) catalog.Spec {
			s := plain(base)
			s.Objects[0].Signature = catalog.Signature{SigURL: base + "/obj.zip.sig", Key: key}
			return s
		}
	}
	serveObjectAndSig := func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sig") {
			_, _ = w.Write([]byte("signature"))
			return
		}
		_, _ = w.Write(good)
	}
	listed := func(base string) catalog.Spec {
		s := plain(base)
		s.Objects[0].Signature = catalog.Signature{TOFU: true, ChecksumsURL: base + "/SUMS", ChecksumAlgo: "sha256"}
		return s
	}
	serveList := func(list string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/SUMS" {
				_, _ = w.Write([]byte(list))
				return
			}
			_, _ = w.Write(good)
		}
	}
	trusted := []string{testFPR, otherFPR}

	tests := []struct {
		name   string
		serve  http.HandlerFunc
		tls12  bool
		closed bool
		spec   func(base string) catalog.Spec
		gpg    fakeGPG
		prov   []string
		want   string
		reqs   int // requests the server must see; 0 leaves it unchecked
	}{
		{name: "TLS 1.2 only server", serve: serveBytes(good), tls12: true, spec: plain, want: "tls13: "},
		{name: "closed port", serve: serveBytes(good), closed: true, spec: plain, want: "network: "},
		{name: "503 three times", serve: status(503), spec: plain, want: "http-503: ", reqs: 3},
		{name: "404 once", serve: status(404), spec: plain, want: "http-404: ", reqs: 1},
		{name: "429 three times", serve: status(429), spec: plain, want: "http-429: ", reqs: 3},
		{
			name: "body ends before its Content-Length",
			serve: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", strconv.Itoa(len(good)+1))
				_, _ = w.Write(good)
				hangUp(w)
			},
			spec: plain, want: "network: ", reqs: 3,
		},
		{name: "redirect off https", serve: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://127.0.0.1:1/obj.zip", http.StatusFound)
		}, spec: plain, want: "tls13: ", reqs: 1},
		{
			name: "missing member", serve: serveBytes(good), want: "integrity: ", reqs: 1,
			spec: func(base string) catalog.Spec { return zipSpec("tool", base+"/obj.zip", "missing.bin") },
		},
		{name: "not a zip", serve: serveBytes([]byte("this is not a zip")), spec: plain, want: "integrity: ", reqs: 1},
		{name: "empty member", serve: serveBytes(makeZip(t, member{"f.bin", nil})), spec: plain, want: "integrity: ", reqs: 1},
		{
			name: "verify fails", serve: serveObjectAndSig, spec: signed("test.asc"),
			gpg: fakeGPG{verifyErr: errors.New("bad signature")}, prov: trusted, want: "signature: ",
		},
		{
			name: "signer absent from PROVENANCE", serve: serveObjectAndSig, spec: signed("test.asc"),
			gpg: fakeGPG{signer: testFPR, primary: testFPR}, want: "signature: ",
		},
		{
			name: "key file holds another primary", serve: serveObjectAndSig, spec: signed("test.asc"),
			gpg: fakeGPG{signer: testFPR, primary: otherFPR}, prov: trusted, want: "signature: ",
		},
		{
			name: "gpg fails locally while verifying", serve: serveObjectAndSig, spec: signed("test.asc"),
			gpg: fakeGPG{verifyErr: fmt.Errorf("make a gpg home: %w", errGPGLocal)}, prov: trusted, want: "network: ",
		},
		{
			name: "gpg fails locally reading the key", serve: serveObjectAndSig, spec: signed("test.asc"),
			gpg: fakeGPG{signer: testFPR, primaryErr: fmt.Errorf("list the key: %w", errGPGLocal)}, prov: trusted, want: "network: ",
		},
		{
			name: "key file missing", serve: serveObjectAndSig, spec: signed("absent.asc"),
			gpg: fakeGPG{signer: testFPR, primary: testFPR}, prov: trusted, want: "network: ",
		},
		{
			name: "TOFU list names another digest", serve: serveList(strings.Repeat("0", 64) + "  obj.zip\n"),
			spec: listed, want: "checksum: ",
		},
		{name: "TOFU list does not name the object", serve: serveList(strings.Repeat("0", 64) + "  other.zip\n"), spec: listed, want: "checksum: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, reqs := counted(tt.serve)
			srv := startServer(t, h, tt.tls12)
			if tt.closed {
				srv.Close()
			}
			g := tt.gpg
			p := newPinner(t, testClient(t, srv), &g)
			for _, f := range tt.prov {
				p.provenance[f] = true
			}

			_, _, err := p.pinSpec(t.Context(), tt.spec(srv.URL))

			assertReason(t, err, tt.want)
			if tt.reqs > 0 && int(reqs.Load()) != tt.reqs {
				t.Errorf("server saw %d requests, want %d", reqs.Load(), tt.reqs)
			}
			assertWorkEmpty(t, p)
		})
	}
}

func TestPin_SignedChecksums(t *testing.T) {
	good := makeZip(t, member{"f.bin", []byte("payload")})
	list := sha256Hex(good) + "  obj.zip\n"
	srv := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SUMS":
			_, _ = w.Write([]byte(list))
		case "/SUMS.gpg":
			_, _ = w.Write([]byte("signature"))
		default:
			_, _ = w.Write(good)
		}
	}), false)
	g := &fakeGPG{signer: testFPR, primary: testFPR}
	p := newPinner(t, testClient(t, srv), g)
	p.provenance[testFPR] = true
	spec := zipSpec("tool", srv.URL+"/obj.zip", "f.bin")
	spec.Objects[0].Signature = catalog.Signature{
		SigURL: srv.URL + "/SUMS.gpg", ChecksumsURL: srv.URL + "/SUMS", ChecksumAlgo: "sha256", Key: "test.asc",
	}

	pin, lines, err := p.pinSpec(t.Context(), spec)
	if err != nil {
		t.Fatalf("pinSpec: %v", err)
	}

	if got := pin.Objects[0].Signer; got != testFPR {
		t.Errorf("Signer = %q, want %q", got, testFPR)
	}
	if g.signedData != list {
		t.Errorf("verify checked %q, want the checksum list %q", g.signedData, list)
	}
	if filepath.Base(g.keyFile) != "test.asc" {
		t.Errorf("verify got key file %q, want test.asc", g.keyFile)
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, testFPR) }) {
		t.Errorf("report does not name the signer:\n%s", strings.Join(lines, "\n"))
	}
	assertWorkEmpty(t, p)
}

func TestPin_SignedObjectWithoutList(t *testing.T) {
	good := makeZip(t, member{"f.bin", []byte("payload")})
	srv := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".asc") {
			_, _ = w.Write([]byte("signature"))
			return
		}
		_, _ = w.Write(good)
	}), false)
	g := &fakeGPG{signer: testFPR, primary: testFPR}
	p := newPinner(t, testClient(t, srv), g)
	p.provenance[testFPR] = true
	spec := zipSpec("tool", srv.URL+"/obj.zip", "f.bin")
	spec.Objects[0].Signature = catalog.Signature{SigURL: srv.URL + "/obj.zip.asc", Key: "test.asc"}

	pin, _, err := p.pinSpec(t.Context(), spec)
	if err != nil {
		t.Fatalf("pinSpec: %v", err)
	}

	if pin.Objects[0].Signer != testFPR {
		t.Errorf("Signer = %q, want %q", pin.Objects[0].Signer, testFPR)
	}
	if g.signedData != string(good) {
		t.Errorf("verify checked %d bytes, want the %d-byte object", len(g.signedData), len(good))
	}
}

func TestPin_TOFUListNamesObject(t *testing.T) {
	good := makeZip(t, member{"f.bin", []byte("payload")})
	srv := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/SUMS" {
			_, _ = fmt.Fprintf(w, "%s  other.zip\n%s *obj.zip\n", strings.Repeat("0", 64), strings.ToUpper(sha256Hex(good)))
			return
		}
		_, _ = w.Write(good)
	}), false)
	g := &fakeGPG{}
	p := newPinner(t, testClient(t, srv), g)
	spec := zipSpec("tool", srv.URL+"/obj.zip", "f.bin")
	spec.Objects[0].Signature = catalog.Signature{TOFU: true, ChecksumsURL: srv.URL + "/SUMS", ChecksumAlgo: "sha256"}

	pin, _, err := p.pinSpec(t.Context(), spec)
	if err != nil {
		t.Fatalf("pinSpec: %v", err)
	}

	if pin.Objects[0].Signer != "" || g.verifies != 0 {
		t.Errorf("a TOFU list pin has signer %q and %d verifies, want none", pin.Objects[0].Signer, g.verifies)
	}
}

func TestPin_SHA1List(t *testing.T) {
	good := makeZip(t, member{"f.bin", []byte("payload")})
	sum := sha1Hex(good)
	srv := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/obj.zip.sha1" {
			_, _ = fmt.Fprintf(w, "%s  obj.zip\n", sum)
			return
		}
		_, _ = w.Write(good)
	}), false)
	p := newPinner(t, testClient(t, srv), &fakeGPG{})
	spec := zipSpec("tool", srv.URL+"/obj.zip", "f.bin")
	spec.Objects[0].Signature = catalog.Signature{TOFU: true, ChecksumsURL: srv.URL + "/obj.zip.sha1", ChecksumAlgo: "sha1"}

	if _, _, err := p.pinSpec(t.Context(), spec); err != nil {
		t.Fatalf("pinSpec: %v", err)
	}
}

func TestPin_EFIStubRecorded(t *testing.T) {
	body := makeZip(t, member{"kernel.efi", peHeader(10)}, member{"plain", []byte("plain text")})
	srv := startServer(t, serveBytes(body), false)
	p := newPinner(t, testClient(t, srv), &fakeGPG{})

	pin, lines, err := p.pinSpec(t.Context(), zipSpec("tool", srv.URL+"/tool.zip", "kernel.efi", "plain"))
	if err != nil {
		t.Fatalf("pinSpec: %v", err)
	}

	files := pin.Objects[0].Files
	if !files[0].EFIStub || files[1].EFIStub {
		t.Errorf("EFIStub = %t, %t, want true, false", files[0].EFIStub, files[1].EFIStub)
	}
	report := strings.Join(lines, "\n")
	for _, want := range []string{"finding efi_stub kernel.efi true", "finding bootif recipe=kernel honoured=false", "pin " + pin.ID} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
}

func TestPin_RedactsQueryStrings(t *testing.T) {
	good := makeZip(t, member{"f.bin", []byte("payload")})
	tests := []struct {
		name   string
		target func(t *testing.T) string // the redirect location, query included
		want   string                    // the reason prefix; empty when the pin succeeds
	}{
		{"target answers 404", func(t *testing.T) string {
			t.Helper()
			return startServer(t, http.NotFoundHandler(), false).URL + "/f?sig=s3cret"
		}, "http-404: "},
		{"target is TLS 1.2 only", func(t *testing.T) string {
			t.Helper()
			return startServer(t, serveBytes(good), true).URL + "/f?sig=s3cret"
		}, "tls13: "},
		{"target is plain http", func(*testing.T) string { return "http://127.0.0.1:1/f?sig=s3cret" }, "tls13: "},
		{"target is a closed port", func(t *testing.T) string {
			t.Helper()
			s := startServer(t, serveBytes(good), false)
			s.Close()
			return s.URL + "/f?sig=s3cret"
		}, "network: "},
		{"target serves the object", func(t *testing.T) string {
			t.Helper()
			return startServer(t, serveBytes(good), false).URL + "/f?sig=s3cret"
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			location := tt.target(t)
			front := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, location, http.StatusFound)
			}), false)
			p := newPinner(t, testClient(t, front), &fakeGPG{})

			_, lines, err := p.pinSpec(t.Context(), zipSpec("tool", front.URL+"/obj.zip", "f.bin"))

			all := strings.Join(lines, "\n")
			if tt.want == "" {
				if err != nil {
					t.Fatalf("pinSpec: %v", err)
				}
			} else {
				assertReason(t, err, tt.want)
				all += "\n" + err.Error()
			}
			if strings.Contains(all, "s3cret") {
				t.Errorf("the query string leaked into the reason or report:\n%s", all)
			}
			if tt.want == "" && !strings.Contains(all, "/f ") {
				t.Errorf("the redirect hop is missing from the report:\n%s", all)
			}
		})
	}
}

func TestPin_CancelAbortsStalledBody(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	h, reqs := counted(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("partial"))
		flush(w)
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	srv := startServer(t, h, false)
	t.Cleanup(func() { close(release) }) // runs before srv.Close, which waits for the handler
	p := newPinner(t, testClient(t, srv), &fakeGPG{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, _, err := p.pinSpec(ctx, zipSpec("tool", srv.URL+"/obj.zip", "f.bin"))
		done <- err
	}()
	<-started
	cancel()

	select {
	case err := <-done:
		assertReason(t, err, "network: ")
		if n := reqs.Load(); n != 1 {
			t.Errorf("server saw %d requests, want 1 (a cancelled pin is not retried)", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("pinSpec did not return after its context was cancelled")
	}
}

func TestPinAll_SpecsWithoutPins(t *testing.T) {
	body := makeZip(t, member{"f.bin", []byte("payload")})
	srv := startServer(t, serveBytes(body), false)
	p := newPinner(t, testClient(t, srv), &fakeGPG{})
	unrelated := catalog.Key{Entry: "prod", Arch: "amd64"}

	pins, absent, lines := p.pinAll(t.Context(), pinRun{
		Specs: []catalog.Spec{
			zipSpec("alpha", srv.URL+"/a.zip", "f.bin"),
			zipSpec("beta", srv.URL+"/b.zip", "f.bin"),
		},
		Existing: map[catalog.Key]catalog.Pin{unrelated: {ID: "unvalidated"}},
		Absent:   map[catalog.Key]string{unrelated: "signature: stale"},
	})

	for _, entry := range []string{"alpha", "beta"} {
		k := catalog.Key{Entry: entry, Arch: "amd64"}
		if pin, ok := pins[k]; !ok || pin.ID == "" {
			t.Errorf("no pin for %v", k)
		}
	}
	if _, ok := pins[unrelated]; ok || len(pins) != 2 {
		t.Errorf("pins = %v, want exactly alpha and beta", pins)
	}
	if len(absent) != 0 {
		t.Errorf("absent = %v, want none (the unrelated key is dropped)", absent)
	}
	if len(lines) == 0 {
		t.Error("pinAll returned no report lines")
	}
}

func TestPinAll_OnlyMerges(t *testing.T) {
	body := makeZip(t, member{"f.bin", []byte("payload")})
	h, reqs := counted(serveBytes(body))
	srv := startServer(t, h, false)
	p := newPinner(t, testClient(t, srv), &fakeGPG{})
	key := func(entry string) catalog.Key { return catalog.Key{Entry: entry, Arch: "amd64"} }
	keepA := catalog.Pin{ID: "keepa", Objects: []catalog.ObjectPin{{URL: "https://old.example.test/a.zip", Size: 1, SHA256: strings.Repeat("a", 64)}}}

	pins, absent, lines := p.pinAll(t.Context(), pinRun{
		Specs: []catalog.Spec{
			zipSpec("alpha", srv.URL+"/a.zip", "f.bin"),
			zipSpec("beta", srv.URL+"/b.zip", "f.bin"),
			zipSpec("gamma", srv.URL+"/c.zip", "f.bin"),
			zipSpec("delta", srv.URL+"/d.zip", "f.bin"),
		},
		Existing: map[catalog.Key]catalog.Pin{key("alpha"): keepA, key("beta"): {ID: "oldb"}},
		Absent:   map[catalog.Key]string{key("gamma"): "signature: x"},
		Only:     []string{"beta"},
	})

	if !reflect.DeepEqual(pins[key("alpha")], keepA) {
		t.Errorf("alpha = %+v, want its compiled-in pin kept as it is", pins[key("alpha")])
	}
	if b := pins[key("beta")]; b.ID == "oldb" || b.ID != catalog.PinID(b.Objects) || len(b.Objects) != 1 {
		t.Errorf("beta = %+v, want it re-pinned", b)
	}
	if absent[key("gamma")] != "signature: x" {
		t.Errorf("gamma reason = %q, want %q verbatim", absent[key("gamma")], "signature: x")
	}
	if !strings.HasPrefix(absent[key("delta")], "not pinned yet") {
		t.Errorf("delta reason = %q, want a not-pinned-yet reason (nothing to keep)", absent[key("delta")])
	}
	if _, ok := pins[key("gamma")]; ok || len(pins) != 2 || len(absent) != 2 {
		t.Errorf("pins = %v, absent = %v, want alpha and beta pinned, gamma and delta absent", pins, absent)
	}
	if n := reqs.Load(); n != 1 {
		t.Errorf("server saw %d requests, want 1: only beta is downloaded", n)
	}
	if len(lines) == 0 {
		t.Error("pinAll returned no report lines")
	}
}

func TestPinAll_ReportsAbsent(t *testing.T) {
	srv := startServer(t, http.NotFoundHandler(), false)
	p := newPinner(t, testClient(t, srv), &fakeGPG{})

	pins, absent, lines := p.pinAll(t.Context(), pinRun{Specs: []catalog.Spec{zipSpec("alpha", srv.URL+"/a.zip", "f.bin")}})

	if len(pins) != 0 {
		t.Errorf("pins = %v, want none", pins)
	}
	if got := absent[catalog.Key{Entry: "alpha", Arch: "amd64"}]; !strings.HasPrefix(got, "http-404: ") {
		t.Errorf("absent reason = %q, want an http-404 reason", got)
	}
	if report := strings.Join(lines, "\n"); !strings.Contains(report, "alpha/amd64") || !strings.Contains(report, "ABSENT http-404: ") {
		t.Errorf("report does not name the absent entry and its reason:\n%s", report)
	}
}

func TestPinAll_StopsOnCancelledContext(t *testing.T) {
	h, reqs := counted(serveBytes(makeZip(t, member{"f.bin", []byte("x")})))
	srv := startServer(t, h, false)
	p := newPinner(t, testClient(t, srv), &fakeGPG{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	pins, absent, _ := p.pinAll(ctx, pinRun{Specs: []catalog.Spec{zipSpec("alpha", srv.URL+"/a.zip", "f.bin")}})

	if len(pins) != 0 || len(absent) != 0 || reqs.Load() != 0 {
		t.Errorf("pins = %v, absent = %v, requests = %d, want nothing done", pins, absent, reqs.Load())
	}
}

func TestPin_ReportRedactsSpecURL(t *testing.T) {
	srv := startServer(t, serveBytes(makeZip(t, member{"f.bin", []byte("payload")})), false)
	p := newPinner(t, testClient(t, srv), &fakeGPG{})

	_, lines, err := p.pinSpec(t.Context(), zipSpec("tool", srv.URL+"/obj.zip?token=s3cret", "f.bin"))
	if err != nil {
		t.Fatalf("pinSpec: %v", err)
	}

	if report := strings.Join(lines, "\n"); strings.Contains(report, "s3cret") {
		t.Errorf("the spec URL's query leaked into the report:\n%s", report)
	}
}
