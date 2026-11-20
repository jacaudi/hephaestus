package toolcache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.uber.org/goleak"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/catalog/catalogtest"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/store"
)

const (
	bytesCounter    = "hephaestus.utility.fetch.bytes"
	failuresCounter = "hephaestus.utility.fetch.failures"
	testETag        = `"v1"`
	secretQuery     = "sig=s3cret"
	fixtureUUID     = "4c4c4544-0042-3510-8052-b4c04f4e4d32"
	squashfsPath    = "live/filesystem.squashfs"
	squashfsSize    = 4 << 20
)

// The fixture rows the tests fetch.
func tliveAMD64() fleet.CacheKey  { return fleet.CacheKey{Entry: "tlive", Arch: fleet.ArchAMD64} }
func tliveARM64() fleet.CacheKey  { return fleet.CacheKey{Entry: "tlive", Arch: fleet.ArchARM64} }
func tshellAMD64() fleet.CacheKey { return fleet.CacheKey{Entry: "tshell", Arch: fleet.ArchAMD64} }

// fetchT0 is the fixed clock of the fetcher tests.
func fetchT0() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }

// seenRequest is what the fake upstream kept of one request.
type seenRequest struct{ Path, Range, IfRange string }

// upstream is a TLS 1.3 fake object server that records every request.
type upstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	h    http.Handler
	reqs []seenRequest
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	u := &upstream{}
	u.srv = startTLS(t, http.HandlerFunc(u.serve), false)
	return u
}

func (u *upstream) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.reqs = append(u.reqs, seenRequest{Path: r.URL.Path, Range: r.Header.Get("Range"), IfRange: r.Header.Get("If-Range")})
	h := u.h
	u.mu.Unlock()
	if h == nil {
		http.NotFound(w, r)
		return
	}
	h.ServeHTTP(w, r)
}

func (u *upstream) handle(h http.Handler) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.h = h
}

func (u *upstream) seen() []seenRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.reqs)
}

// send writes b to w; a write error is expected when the client gives up.
func send(t *testing.T, w io.Writer, b []byte) {
	t.Helper()
	if _, err := w.Write(b); err != nil {
		t.Logf("upstream write: %v", err)
	}
}

// flush pushes what the handler has written to the client.
func flush(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	fl, ok := w.(http.Flusher)
	if !ok {
		t.Error("the response writer cannot flush")
		return
	}
	fl.Flush()
}

// serveObject serves object with a strong ETag and full Range support.
func serveObject(object []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", testETag)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(object))
	})
}

// objectsAt serves each object at its URL's path.
func objectsAt(base string, objects map[string][]byte) http.Handler {
	mux := http.NewServeMux()
	for u, b := range objects {
		mux.Handle("GET "+strings.TrimPrefix(u, base), serveObject(b))
	}
	return mux
}

// redirectTo answers every request with a 302 to loc.
func redirectTo(loc string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, loc, http.StatusFound) })
}

// hijackSend writes raw straight to the connection and closes it, so a
// response can end short of its Content-Length.
func hijackSend(t *testing.T, w http.ResponseWriter, raw string) {
	t.Helper()
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Error("the response writer cannot hijack")
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	if _, err := buf.WriteString(raw); err != nil {
		t.Logf("hijacked write: %v", err)
	}
	if err := buf.Flush(); err != nil {
		t.Logf("hijacked flush: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Logf("hijacked close: %v", err)
	}
}

// harness is a Fetcher over a SQLite database, a store and a spool directory,
// with its events, errors and metrics captured.
type harness struct {
	t        *testing.T
	d        *db.DB
	repo     *fleet.Repository
	store    *store.Store
	storeDir string
	spool    string
	f        *Fetcher
	w        *worker // the worker the tests drive with runOnce
	reader   *sdkmetric.ManualReader

	mu     sync.Mutex
	events []FetchEvent
	errs   []error
}

func newHarness(t *testing.T, cat *catalog.Catalog, client *http.Client) *harness {
	t.Helper()
	h := &harness{t: t, d: dbtest.SQLite(t), storeDir: filepath.Join(t.TempDir(), "store"), spool: t.TempDir(), reader: sdkmetric.NewManualReader()}
	h.repo = fleet.NewRepository(h.d)
	st, _, err := store.Open(t.Context(), h.storeDir, fetchT0)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	h.store = st
	f, err := NewFetcher(
		FetcherConfig{Repo: h.repo, Store: st, Catalog: cat, Client: client, SpoolDir: h.spool, Workers: 1},
		WithClock(fetchT0),
		WithMeter(sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader)).Meter("test")),
		WithEvents(func(_ context.Context, ev FetchEvent) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.events = append(h.events, ev)
		}),
		WithErrorHandler(func(_ context.Context, err error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.errs = append(h.errs, err)
		}),
	)
	if err != nil {
		t.Fatalf("NewFetcher: %v", err)
	}
	// The default statfs reads the host's disk, whose free space the test must not depend on.
	f.statfs = func(string) (uint64, error) { return 1 << 40, nil }
	h.f = f
	h.w = &worker{f: f}
	return h
}

func (h *harness) once() (bool, error) {
	h.t.Helper()
	return h.w.runOnce(h.t.Context())
}

// fetchOnce runs one fetch that must claim a row and store its outcome.
func (h *harness) fetchOnce() {
	h.t.Helper()
	if claimed, err := h.once(); err != nil || !claimed {
		h.t.Fatalf("runOnce = %v, %v; want true, nil", claimed, err)
	}
}

func (h *harness) request(entry, arch string, priority int) catalog.Entry {
	h.t.Helper()
	e, ok := h.f.cfg.Catalog.Lookup(entry, arch)
	if !ok {
		h.t.Fatalf("catalog has no %s/%s", entry, arch)
	}
	key := fleet.CacheKey{Entry: entry, Arch: fleet.Arch(arch)}
	if _, err := h.repo.RequestFetch(h.t.Context(), key, e.Pin.ID, priority, fetchT0()); err != nil {
		h.t.Fatalf("RequestFetch(%v): %v", key, err)
	}
	return e
}

func (h *harness) row(key fleet.CacheKey) fleet.CacheRow {
	h.t.Helper()
	row, ok, err := h.repo.CacheRow(h.t.Context(), key)
	if err != nil || !ok {
		h.t.Fatalf("CacheRow(%v) = %v, %v", key, ok, err)
	}
	return row
}

// wantRow fails unless the row is in state after attempts failed attempts.
func (h *harness) wantRow(key fleet.CacheKey, state fleet.CacheState, attempts int) fleet.CacheRow {
	h.t.Helper()
	row := h.row(key)
	if row.State != state || row.Attempts != attempts {
		h.t.Fatalf("row %v = %s, %d attempts (%q); want %s, %d", key, row.State, row.Attempts, row.LastError, state, attempts)
	}
	return row
}

func (h *harness) lastEvent() FetchEvent {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.events) == 0 {
		h.t.Fatal("no FetchEvent was emitted")
	}
	return h.events[len(h.events)-1]
}

func (h *harness) transitions() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for i := range h.events {
		out = append(out, string(h.events[i].From)+">"+string(h.events[i].To))
	}
	return out
}

func (h *harness) handlerErrors() []error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.errs)
}

// wantSpool fails unless the spool directory holds exactly names.
func (h *harness) wantSpool(names ...string) {
	h.t.Helper()
	entries, err := os.ReadDir(h.spool)
	if err != nil {
		h.t.Fatalf("read spool dir: %v", err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if !slices.Equal(got, names) {
		h.t.Errorf("spool dir = %v, want %v", got, names)
	}
}

// spoolSize is the size of a spool file, or -1 when it is not there.
func (h *harness) spoolSize(name string) int64 {
	h.t.Helper()
	fi, err := os.Stat(filepath.Join(h.spool, name))
	if errors.Is(err, os.ErrNotExist) {
		return -1
	}
	if err != nil {
		h.t.Fatalf("stat %s: %v", name, err)
	}
	return fi.Size()
}

func (h *harness) counter(name, class string) int64 {
	h.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(h.t.Context(), &rm); err != nil {
		h.t.Fatalf("collect metrics: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				h.t.Fatalf("%s is a %T, want Sum[int64]", name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				if v, has := dp.Attributes.Value(attribute.Key("class")); class != "" && (!has || v.AsString() != class) {
					continue
				}
				total += dp.Value
			}
		}
	}
	return total
}

// layerFile returns the path of the blob file of e's layer titled title.
func (h *harness) layerFile(e catalog.Entry, title string) string {
	h.t.Helper()
	layers, err := h.store.Layers(h.t.Context(), e.Tag())
	if err != nil {
		h.t.Fatalf("Layers(%s): %v", e.Tag(), err)
	}
	for _, l := range layers {
		if l.Annotations[ocispec.AnnotationTitle] == title {
			return filepath.Join(h.storeDir, ocispec.ImageBlobsDir, l.Digest.Algorithm().String(), l.Digest.Encoded())
		}
	}
	h.t.Fatalf("%s has no layer %q", e.Tag(), title)
	return ""
}

// wantStored fails unless the store holds every file of e, tagged and intact.
func (h *harness) wantStored(e catalog.Entry, files map[string][]byte) {
	h.t.Helper()
	if err := h.store.CheckLayers(h.t.Context(), e.Tag()); err != nil {
		h.t.Fatalf("CheckLayers(%s): %v", e.Tag(), err)
	}
	layers, err := h.store.Layers(h.t.Context(), e.Tag())
	if err != nil {
		h.t.Fatalf("Layers(%s): %v", e.Tag(), err)
	}
	if len(layers) != len(files) {
		h.t.Fatalf("%s has %d layers, want %d", e.Tag(), len(layers), len(files))
	}
	for _, l := range layers {
		rc, err := h.store.OpenBlob(l)
		if err != nil {
			h.t.Fatalf("OpenBlob(%s): %v", l.Digest, err)
		}
		got, err := io.ReadAll(rc)
		if cerr := rc.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			h.t.Fatalf("read blob %s: %v", l.Digest, err)
		}
		title := l.Annotations[ocispec.AnnotationTitle]
		if !bytes.Equal(got, files[title]) {
			h.t.Errorf("layer %q holds %d bytes that are not the file's %d", title, len(got), len(files[title]))
		}
	}
}

// wantNotTagged fails when the store has a tag for e.
func (h *harness) wantNotTagged(e catalog.Entry) {
	h.t.Helper()
	if _, err := h.store.Resolve(h.t.Context(), e.Tag()); !errors.Is(err, store.ErrNotFound) {
		h.t.Errorf("Resolve(%s) = %v, want ErrNotFound", e.Tag(), err)
	}
}

// scenario is one upstream behaviour for one fixture entry.
type scenario struct {
	cat     *catalog.Catalog
	key     fleet.CacheKey
	handler http.Handler
	// fx and object are the fixture behind cat and the key's first object, when it is one.
	fx     catalogtest.Fixture
	object []byte
	// pool lists servers besides the upstream the client must trust.
	pool []*httptest.Server
	// seed starts the spool partway before the fetch.
	seed func(t *testing.T, h *harness, e catalog.Entry)
	// tune adjusts the fetcher's test hooks.
	tune func(f *Fetcher)
}

// run is a scenario set up: its harness, its wanted entry and the scenario.
type run struct {
	*harness

	e  catalog.Entry
	sc scenario
	up *upstream
}

// runScenario builds the upstream, the harness and the wanted row of the
// scenario mk returns, and applies its seed and tune.
func runScenario(t *testing.T, mk func(t *testing.T, base string) scenario) *run {
	t.Helper()
	up := newUpstream(t)
	sc := mk(t, up.srv.URL)
	up.handle(sc.handler)
	client := NewHTTPClient(testUserAgent, poolOf(append([]*httptest.Server{up.srv}, sc.pool...)...))
	t.Cleanup(client.CloseIdleConnections)
	h := newHarness(t, sc.cat, client)
	r := &run{harness: h, sc: sc, up: up}
	r.e = h.request(sc.key.Entry, string(sc.key.Arch), 0)
	if sc.seed != nil {
		sc.seed(t, h, r.e)
	}
	if sc.tune != nil {
		sc.tune(h.f)
	}
	return r
}

// files returns the extracted files of the scenario's fixture entry.
func (r *run) files() map[string][]byte {
	return r.sc.fx.Files[catalog.Key{Entry: r.sc.key.Entry, Arch: string(r.sc.key.Arch)}]
}

// fixtureOf is catalogtest's catalog served by serve(object), for key.
func fixtureOf(t *testing.T, base string, key fleet.CacheKey, serve func(object []byte) http.Handler) scenario {
	t.Helper()
	fx := catalogtest.New(t, base)
	e, ok := fx.Catalog.Lookup(key.Entry, string(key.Arch))
	if !ok {
		t.Fatalf("fixture has no %v", key)
	}
	object := fx.Objects[e.Pin.Objects[0].URL]
	return scenario{cat: fx.Catalog, key: key, fx: fx, object: object, handler: serve(object)}
}

func liveOf(t *testing.T, base string, serve func(object []byte) http.Handler) scenario {
	t.Helper()
	return fixtureOf(t, base, tliveAMD64(), serve)
}

// seedSpool writes data as the spool of the entry's first object and, when
// etag is not empty, its sidecar.
func seedSpool(t *testing.T, h *harness, e catalog.Entry, data []byte, etag string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.spool, SpoolName(e.Pin.ID, 0)), data, 0o600); err != nil {
		t.Fatalf("seed spool: %v", err)
	}
	if etag == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(h.spool, ETagName(e.Pin.ID, 0)), []byte(etag), 0o600); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}
}

// seedHalf starts the spool of the entry's object at its first half, under a
// strong ETag.
func seedHalf(object []byte) func(*testing.T, *harness, catalog.Entry) {
	return func(t *testing.T, h *harness, e catalog.Entry) {
		t.Helper()
		seedSpool(t, h, e, object[:len(object)/2], testETag)
	}
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// pinOf pins spec's first object over object and its files over files.
func pinOf(spec catalog.Spec, object []byte, files map[string][]byte) catalog.Pin {
	op := catalog.ObjectPin{URL: spec.Objects[0].URL, Size: int64(len(object)), SHA256: sha(object)}
	for _, p := range spec.Objects[0].Files {
		op.Files = append(op.Files, catalog.FilePin{Path: p, Size: int64(len(files[p])), SHA256: sha(files[p])})
	}
	objects := []catalog.ObjectPin{op}
	return catalog.Pin{ID: catalog.PinID(objects), Objects: objects}
}

// singleCatalog is a catalog of spec alone, pinned as pin.
func singleCatalog(t *testing.T, spec catalog.Spec, pin catalog.Pin) *catalog.Catalog {
	t.Helper()
	c, err := catalog.New([]catalog.Spec{spec}, map[catalog.Key]catalog.Pin{spec.Key(): pin})
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	return c
}

func TestFetcher_ZipToReady(t *testing.T) {
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		fx := catalogtest.New(t, base)
		return scenario{cat: fx.Catalog, key: tliveAMD64(), fx: fx, handler: objectsAt(base, fx.Objects)}
	})
	r.fetchOnce()
	row := r.wantRow(tliveAMD64(), fleet.CacheReady, 0)
	size := r.e.Pin.Objects[0].Size
	if row.BytesDone != size || row.BytesTotal != size {
		t.Errorf("progress = %d of %d, want %d of %d", row.BytesDone, row.BytesTotal, size, size)
	}
	r.wantStored(r.e, r.files())
	r.wantSpool()
	if got := r.counter(bytesCounter, ""); got != size {
		t.Errorf("%s = %d, want %d", bytesCounter, got, size)
	}
	if got, want := r.transitions(), []string{"wanted>fetching", "fetching>ready"}; !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	if ev := r.lastEvent(); ev.Key != tliveAMD64() || ev.Pin != r.e.Pin.ID || ev.Class != "" || ev.Err != nil || ev.Bytes != size {
		t.Errorf("last event = %+v", ev)
	}
}

func TestFetcher_ISOToReady(t *testing.T) {
	iso := readFixture(t)
	files := map[string][]byte{"boot/vmlinuz": []byte("kernel-fixture\n"), "live/tool-amd64/tool-amd64.squashfs": seqText(40000)}
	key := fleet.CacheKey{Entry: "tiso", Arch: fleet.ArchAMD64}
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		spec := catalog.Spec{
			Entry: "tiso", Arch: "amd64", Version: "1", Recipe: catalog.RecipeLiveBoot, Platforms: []catalog.Platform{catalog.PlatformEFI},
			Objects: []catalog.ObjectSpec{{
				URL: base + "/rr.iso", Container: catalog.ContainerISO, Signature: catalog.Signature{TOFU: true},
				Files: []string{"boot/vmlinuz", "live/tool-amd64/tool-amd64.squashfs"},
			}},
			Boot: catalog.Boot{Kernel: "boot/vmlinuz", Second: "live/tool-amd64/tool-amd64.squashfs"},
		}
		return scenario{
			cat: singleCatalog(t, spec, pinOf(spec, iso, files)), key: key,
			handler: objectsAt(base, map[string][]byte{spec.Objects[0].URL: iso}),
		}
	})
	r.fetchOnce()
	r.wantRow(key, fleet.CacheReady, 0)
	r.wantStored(r.e, files)
	r.wantSpool()
}

// TestFetcher_ProgressIsOfTheCurrentObject pins design §5.3: bytes_done and
// bytes_total describe the object being fetched, not the sum of the entry's objects.
func TestFetcher_ProgressIsOfTheCurrentObject(t *testing.T) {
	iso := readFixture(t)
	first := map[string][]byte{"boot/vmlinuz": []byte("kernel-fixture\n")}
	second := map[string][]byte{"live/tool-amd64/tool-amd64.squashfs": seqText(40000)}
	key := fleet.CacheKey{Entry: "ttwo", Arch: fleet.ArchAMD64}
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		spec := catalog.Spec{
			Entry: "ttwo", Arch: "amd64", Version: "1", Recipe: catalog.RecipeLiveBoot, Platforms: []catalog.Platform{catalog.PlatformEFI},
			Objects: []catalog.ObjectSpec{
				{URL: base + "/a.iso", Container: catalog.ContainerISO, Signature: catalog.Signature{TOFU: true}, Files: []string{"boot/vmlinuz"}},
				{URL: base + "/b.iso", Container: catalog.ContainerISO, Signature: catalog.Signature{TOFU: true}, Files: []string{"live/tool-amd64/tool-amd64.squashfs"}},
			},
			Boot: catalog.Boot{Kernel: "boot/vmlinuz", Second: "live/tool-amd64/tool-amd64.squashfs"},
		}
		var objects []catalog.ObjectPin
		for i, files := range []map[string][]byte{first, second} {
			op := catalog.ObjectPin{URL: spec.Objects[i].URL, Size: int64(len(iso)), SHA256: sha(iso)}
			for _, p := range spec.Objects[i].Files {
				op.Files = append(op.Files, catalog.FilePin{Path: p, Size: int64(len(files[p])), SHA256: sha(files[p])})
			}
			objects = append(objects, op)
		}
		pin := catalog.Pin{ID: catalog.PinID(objects), Objects: objects}
		return scenario{
			cat: singleCatalog(t, spec, pin), key: key,
			handler: objectsAt(base, map[string][]byte{spec.Objects[0].URL: iso, spec.Objects[1].URL: iso}),
		}
	})
	r.fetchOnce()

	row := r.wantRow(key, fleet.CacheReady, 0)
	size := int64(len(iso))
	if row.BytesDone != size || row.BytesTotal != size {
		t.Errorf("progress = %d of %d, want the last object's %d of %d", row.BytesDone, row.BytesTotal, size, size)
	}
}

func TestFetcher_AlreadyInStoreShortcut(t *testing.T) {
	var fx catalogtest.Fixture
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		fx = catalogtest.New(t, base)
		return scenario{cat: fx.Catalog, key: tliveAMD64(), fx: fx, handler: objectsAt(base, fx.Objects)}
	})
	catalogtest.Stock(t, r.store, fx, catalog.Key{Entry: "tlive", Arch: "amd64"})

	r.fetchOnce()
	r.wantRow(tliveAMD64(), fleet.CacheReady, 0)
	if got := r.up.seen(); len(got) != 0 {
		t.Errorf("the upstream saw %d requests, want 0: %v", len(got), got)
	}
	r.wantSpool()
}

// TestFetcher_UnreadableStoreFailsTheAttempt pins that content the store cannot
// read is not content it lacks: the claim-time check fails the attempt in the
// default class and downloads nothing, as the other store checks do.
func TestFetcher_UnreadableStoreFailsTheAttempt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	var fx catalogtest.Fixture
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		fx = catalogtest.New(t, base)
		return scenario{cat: fx.Catalog, key: tliveAMD64(), fx: fx, handler: objectsAt(base, fx.Objects)}
	})
	catalogtest.Stock(t, r.store, fx, catalog.Key{Entry: "tlive", Arch: "amd64"})
	manifest, err := r.store.Resolve(t.Context(), r.e.Tag())
	if err != nil {
		t.Fatalf("Resolve(%s): %v", r.e.Tag(), err)
	}
	blob := filepath.Join(r.storeDir, ocispec.ImageBlobsDir, manifest.Digest.Algorithm().String(), manifest.Digest.Encoded())
	if err := os.Chmod(blob, 0o000); err != nil {
		t.Fatalf("chmod manifest blob: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(blob, 0o444); err != nil {
			t.Errorf("restore manifest blob mode: %v", err)
		}
	})

	r.fetchOnce()

	r.wantRow(tliveAMD64(), fleet.CacheFailed, 1)
	if ev := r.lastEvent(); ev.To != fleet.CacheFailed || ev.Class != "other" {
		t.Errorf("last event = %+v, want To failed, Class other", ev)
	}
	if got := r.up.seen(); len(got) != 0 {
		t.Errorf("the upstream saw %d requests, want 0: %v", len(got), got)
	}
}

func TestFetcher_NewRequiresClient(t *testing.T) {
	d := dbtest.SQLite(t)
	st, _, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "store"), fetchT0)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	cat := catalogtest.New(t, "https://example.test").Catalog
	full := FetcherConfig{
		Repo: fleet.NewRepository(d), Store: st, Catalog: cat, Client: NewHTTPClient(testUserAgent, nil), SpoolDir: filepath.Join(t.TempDir(), "spool"),
	}
	if _, err := NewFetcher(full); err != nil {
		t.Fatalf("NewFetcher(complete config): %v", err)
	}

	tests := []struct {
		name string
		drop func(*FetcherConfig)
	}{
		{"no client", func(c *FetcherConfig) { c.Client = nil }},
		{"no repository", func(c *FetcherConfig) { c.Repo = nil }},
		{"no store", func(c *FetcherConfig) { c.Store = nil }},
		{"no catalog", func(c *FetcherConfig) { c.Catalog = nil }},
		{"no spool dir", func(c *FetcherConfig) { c.SpoolDir = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := full
			tt.drop(&cfg)
			if f, err := NewFetcher(cfg); err == nil {
				t.Errorf("NewFetcher = %v, nil; want an error", f)
			}
		})
	}
}

func TestFetcher_Resume(t *testing.T) {
	tests := []struct {
		name    string
		sidecar string
		whole   bool // the spool holds the whole object, not its first half
		longer  bool // the spool holds the object and ten bytes more
		serve   func(object []byte) http.Handler
		ranged  bool // the request asks for the rest under If-Range
		reqs    int
	}{
		{name: "strong etag", sidecar: testETag, serve: serveObject, ranged: true, reqs: 1},
		{name: "weak etag", sidecar: `W/"v1"`, serve: serveObject, reqs: 1},
		{
			name: "200 to range", sidecar: testETag, ranged: true, reqs: 1,
			serve: func(object []byte) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("ETag", testETag)
					send(t, w, object)
				})
			},
		},
		{name: "complete spool", whole: true, serve: serveObject},
		{name: "spool longer than the pin", sidecar: testETag, longer: true, serve: serveObject, reqs: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var half int
			r := runScenario(t, func(t *testing.T, base string) scenario {
				t.Helper()
				sc := fixtureOf(t, base, tshellAMD64(), tt.serve)
				half = len(sc.object) / 2
				sc.seed = func(t *testing.T, h *harness, e catalog.Entry) {
					t.Helper()
					data := sc.object[:half]
					if tt.whole {
						data = sc.object
					}
					if tt.longer {
						data = slices.Concat(sc.object, make([]byte, 10))
					}
					seedSpool(t, h, e, data, tt.sidecar)
				}
				return sc
			})
			r.fetchOnce()

			r.wantRow(tshellAMD64(), fleet.CacheReady, 0)
			r.wantStored(r.e, r.files())
			r.wantSpool()
			seen := r.up.seen()
			if len(seen) != tt.reqs {
				t.Fatalf("requests = %v, want %d", seen, tt.reqs)
			}
			if tt.reqs == 0 {
				return
			}
			wantRange, wantIfRange := "", ""
			if tt.ranged {
				wantRange, wantIfRange = fmt.Sprintf("bytes=%d-", half), testETag
			}
			if seen[0].Range != wantRange || seen[0].IfRange != wantIfRange {
				t.Errorf("Range, If-Range = %q, %q; want %q, %q", seen[0].Range, seen[0].IfRange, wantRange, wantIfRange)
			}
		})
	}
}

// TestFetcher_206WithOtherETagRestarts pins that a resume is never spliced
// from two upstream versions: an upstream that answers 206 under another ETag
// than the spool was started under (some ignore If-Range) sends bytes that
// belong to another object, so the spool is emptied and the whole object is
// fetched again within the same attempt.
func TestFetcher_206WithOtherETagRestarts(t *testing.T) {
	var half int
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		sc := liveOf(t, base, func(object []byte) http.Handler { return otherVersionAfterRange(t, object) })
		half = len(sc.object) / 2
		sc.seed = func(t *testing.T, h *harness, e catalog.Entry) {
			t.Helper()
			seedSpool(t, h, e, sc.object[:half], `"A"`)
		}
		return sc
	})

	r.fetchOnce()

	r.wantRow(tliveAMD64(), fleet.CacheReady, 0)
	r.wantStored(r.e, r.files())
	r.wantSpool()
	seen := r.up.seen()
	if len(seen) != 2 {
		t.Fatalf("requests = %+v, want the ranged one and one restart", seen)
	}
	if want := fmt.Sprintf("bytes=%d-", half); seen[0].Range != want || seen[0].IfRange != `"A"` {
		t.Errorf("first request Range, If-Range = %q, %q; want %q, %q", seen[0].Range, seen[0].IfRange, want, `"A"`)
	}
	if seen[1].Range != "" || seen[1].IfRange != "" {
		t.Errorf("restart Range, If-Range = %q, %q; want neither", seen[1].Range, seen[1].IfRange)
	}
}

func TestFetcher_206WithSameETagResumes(t *testing.T) {
	var half int
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		sc := liveOf(t, base, func(object []byte) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", `"A"`)
				http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(object))
			})
		})
		half = len(sc.object) / 2
		sc.seed = func(t *testing.T, h *harness, e catalog.Entry) {
			t.Helper()
			seedSpool(t, h, e, sc.object[:half], `"A"`)
		}
		return sc
	})

	r.fetchOnce()

	r.wantRow(tliveAMD64(), fleet.CacheReady, 0)
	r.wantStored(r.e, r.files())
	seen := r.up.seen()
	if want := fmt.Sprintf("bytes=%d-", half); len(seen) != 1 || seen[0].Range != want {
		t.Errorf("requests = %+v, want one with Range %q", seen, want)
	}
}

func TestFetcher_IntegrityFailures(t *testing.T) {
	tests := []struct {
		name string
		mk   func(t *testing.T, base string) scenario
	}{
		{"oversize", func(t *testing.T, base string) scenario {
			t.Helper()
			return liveOf(t, base, func(object []byte) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { send(t, w, slices.Concat(object, make([]byte, 10))) })
			})
		}},
		{"interstitial", func(t *testing.T, base string) scenario {
			t.Helper()
			const page = "<html>interstitial</html>"
			return liveOf(t, base, func([]byte) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/html")
					w.Header().Set("Content-Length", strconv.Itoa(len(page)))
					send(t, w, []byte(page))
				})
			})
		}},
		{"content-range total", func(t *testing.T, base string) scenario {
			t.Helper()
			sc := liveOf(t, base, func(object []byte) http.Handler {
				half := len(object) / 2
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, len(object), len(object)+1))
					w.WriteHeader(http.StatusPartialContent)
					send(t, w, object[half:])
				})
			})
			sc.seed = seedHalf(sc.object)
			return sc
		}},
		{"206 content-length", func(t *testing.T, base string) scenario {
			t.Helper()
			sc := liveOf(t, base, func(object []byte) http.Handler {
				half := len(object) / 2
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, len(object)-1, len(object)))
					w.Header().Set("Content-Length", strconv.Itoa(len(object)-half+1))
					w.WriteHeader(http.StatusPartialContent)
					send(t, w, object[half:])
				})
			})
			sc.seed = seedHalf(sc.object)
			return sc
		}},
		{"hash mismatch", func(t *testing.T, base string) scenario {
			t.Helper()
			return liveOf(t, base, func(object []byte) http.Handler {
				flipped := slices.Clone(object)
				flipped[len(flipped)/2] ^= 0xff
				return serveObject(flipped)
			})
		}},
		{"non-https redirect", func(t *testing.T, base string) scenario {
			t.Helper()
			return liveOf(t, base, func([]byte) http.Handler { return redirectTo("http://127.0.0.1:1/x") })
		}},
		{"member hash mismatch", func(t *testing.T, base string) scenario {
			t.Helper()
			sc := fixtureOf(t, base, tshellAMD64(), serveObject)
			e, _ := sc.cat.Lookup("tshell", "amd64")
			e.Pin.Objects[0].Files[0].SHA256 = strings.Repeat("0", 64)
			e.Pin.ID = catalog.PinID(e.Pin.Objects)
			sc.cat = singleCatalog(t, e.Spec, e.Pin)
			return sc
		}},
		{"unsafe member", func(t *testing.T, base string) scenario {
			t.Helper()
			spec := catalog.Spec{
				Entry: "tsym", Arch: "amd64", Version: "1", Recipe: catalog.RecipeChain, Platforms: []catalog.Platform{catalog.PlatformEFI},
				Objects: []catalog.ObjectSpec{{URL: base + "/sym.zip", Container: catalog.ContainerZip, Signature: catalog.Signature{TOFU: true}, Files: []string{"live/vmlinuz"}}},
				Boot:    catalog.Boot{Kernel: "live/vmlinuz"},
			}
			object := symlinkZip(t)
			pin := pinOf(spec, object, map[string][]byte{"live/vmlinuz": []byte("k")})
			return scenario{cat: singleCatalog(t, spec, pin), key: fleet.CacheKey{Entry: "tsym", Arch: fleet.ArchAMD64}, handler: serveObject(object)}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runScenario(t, tt.mk)
			r.fetchOnce()

			r.wantRow(r.sc.key, fleet.CacheBroken, 1)
			if ev := r.lastEvent(); ev.To != fleet.CacheBroken || ev.Class != "integrity" || ev.Err == nil {
				t.Errorf("last event = %+v, want To broken, Class integrity and an Err", ev)
			}
			r.wantSpool()
			r.wantNotTagged(r.e)
			if got := r.counter(failuresCounter, "integrity"); got != 1 {
				t.Errorf("%s{class=integrity} = %d, want 1", failuresCounter, got)
			}
		})
	}
}

func TestFetcher_TransientFailures(t *testing.T) {
	tests := []struct {
		name      string
		mk        func(t *testing.T, base string) scenario
		noRequest bool
		want      string                                          // last_error contains
		check     func(t *testing.T, h *harness, e catalog.Entry) // extra state
	}{
		{name: "tls12 mirror", want: "protocol version", mk: func(t *testing.T, base string) scenario {
			t.Helper()
			mirror := startTLS(t, http.NotFoundHandler(), true)
			sc := liveOf(t, base, func([]byte) http.Handler { return redirectTo(mirror.URL + "/x") })
			sc.pool = []*httptest.Server{mirror}
			return sc
		}},
		{name: "status 503", want: "503", mk: func(t *testing.T, base string) scenario {
			t.Helper()
			return liveOf(t, base, func([]byte) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
			})
		}},
		{name: "status 404", want: "404", mk: func(t *testing.T, base string) scenario {
			t.Helper()
			return liveOf(t, base, func([]byte) http.Handler { return http.NotFoundHandler() })
		}},
		{name: "content-range end", want: "Content-Range", mk: func(t *testing.T, base string) scenario {
			t.Helper()
			sc := liveOf(t, base, func(object []byte) http.Handler {
				half := len(object) / 2
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, len(object)-2, len(object)))
					w.Header().Set("Content-Length", strconv.Itoa(len(object)-half))
					w.WriteHeader(http.StatusPartialContent)
					send(t, w, object[half:])
				})
			})
			sc.seed = seedHalf(sc.object)
			return sc
		}},
		{name: "content-range end with a matching length", want: "Content-Range", mk: func(t *testing.T, base string) scenario {
			t.Helper()
			sc := liveOf(t, base, func(object []byte) http.Handler {
				half := len(object) / 2
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, len(object)-2, len(object)))
					w.Header().Set("Content-Length", strconv.Itoa(len(object)-half-1))
					w.WriteHeader(http.StatusPartialContent)
					send(t, w, object[half:len(object)-1])
				})
			})
			sc.seed = seedHalf(sc.object)
			return sc
		}, check: func(t *testing.T, h *harness, e catalog.Entry) {
			t.Helper()
			if got, half := h.spoolSize(SpoolName(e.Pin.ID, 0)), e.Pin.Objects[0].Size/2; got != half {
				t.Errorf("spool size = %d, want %d: the spool is kept", got, half)
			}
		}},
		{name: "weak etag drops the sidecar", mk: func(t *testing.T, base string) scenario {
			t.Helper()
			return droppedSidecar(t, base, `ETag: W/"v2"`+"\r\n")
		}, check: wantSidecarGone},
		{name: "no etag drops the sidecar", mk: func(t *testing.T, base string) scenario {
			t.Helper()
			return droppedSidecar(t, base, "")
		}, check: wantSidecarGone},
		{name: "content-range start", mk: func(t *testing.T, base string) scenario {
			t.Helper()
			sc := liveOf(t, base, func(object []byte) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(object)-1, len(object)))
					w.Header().Set("Content-Length", strconv.Itoa(len(object)))
					w.WriteHeader(http.StatusPartialContent)
					send(t, w, object)
				})
			})
			sc.seed = seedHalf(sc.object)
			return sc
		}, check: func(t *testing.T, h *harness, e catalog.Entry) {
			t.Helper()
			if got := h.spoolSize(SpoolName(e.Pin.ID, 0)); got != 0 {
				t.Errorf("spool size = %d, want 0", got)
			}
			if got := h.spoolSize(ETagName(e.Pin.ID, 0)); got != -1 {
				t.Errorf("sidecar size = %d, want it removed", got)
			}
		}},
		{name: "dropped connection", mk: func(t *testing.T, base string) scenario {
			t.Helper()
			return liveOf(t, base, func(object []byte) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					head := fmt.Sprintf("HTTP/1.1 200 OK\r\nETag: %s\r\nContent-Length: %d\r\n\r\n", testETag, len(object))
					hijackSend(t, w, head+string(object[:len(object)/2]))
				})
			})
		}, check: func(t *testing.T, h *harness, e catalog.Entry) {
			t.Helper()
			half := e.Pin.Objects[0].Size / 2
			if got := h.spoolSize(SpoolName(e.Pin.ID, 0)); got != half {
				t.Errorf("spool size = %d, want %d", got, half)
			}
			if got := h.spoolSize(ETagName(e.Pin.ID, 0)); got <= 0 {
				t.Errorf("sidecar size = %d, want the strong ETag kept", got)
			}
		}},
		{name: "short close-delimited", mk: func(t *testing.T, base string) scenario {
			t.Helper()
			return liveOf(t, base, func(object []byte) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					head := fmt.Sprintf("HTTP/1.1 200 OK\r\nETag: %s\r\nConnection: close\r\n\r\n", testETag)
					hijackSend(t, w, head+string(object[:len(object)/2]))
				})
			})
		}, check: func(t *testing.T, h *harness, e catalog.Entry) {
			t.Helper()
			if got, half := h.spoolSize(SpoolName(e.Pin.ID, 0)), e.Pin.Objects[0].Size/2; got != half {
				t.Errorf("spool size = %d, want %d", got, half)
			}
		}},
		{name: "idle timeout", want: "idle", mk: func(t *testing.T, base string) scenario {
			t.Helper()
			sc := liveOf(t, base, func(object []byte) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("ETag", testETag)
					w.Header().Set("Content-Length", strconv.Itoa(len(object)))
					w.WriteHeader(http.StatusOK)
					send(t, w, object[:1024])
					flush(t, w)
					<-r.Context().Done()
				})
			})
			sc.tune = func(f *Fetcher) { f.idle = 100 * time.Millisecond }
			return sc
		}},
		{name: "status text with a NUL byte", want: "502", mk: func(t *testing.T, base string) scenario {
			t.Helper()
			return liveOf(t, base, func([]byte) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					hijackSend(t, w, "HTTP/1.1 502 Bad\x00Gateway\r\nContent-Length: 0\r\n\r\n")
				})
			})
		}, check: func(t *testing.T, h *harness, _ catalog.Entry) {
			t.Helper()
			if got := h.row(tliveAMD64()).LastError; strings.ContainsRune(got, 0) {
				t.Errorf("last_error = %q, want it free of NUL", got)
			}
		}},
		{name: "status text over the cause cap", want: "502", mk: func(t *testing.T, base string) scenario {
			t.Helper()
			return liveOf(t, base, func([]byte) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					hijackSend(t, w, "HTTP/1.1 502 "+strings.Repeat("x", 3*fleet.MaxCauseBytes)+"\r\nContent-Length: 0\r\n\r\n")
				})
			})
		}, check: func(t *testing.T, h *harness, _ catalog.Entry) {
			t.Helper()
			if got := h.row(tliveAMD64()).LastError; len(got) > fleet.MaxCauseBytes {
				t.Errorf("last_error is %d bytes, want at most %d", len(got), fleet.MaxCauseBytes)
			}
		}},
		{name: "free space", want: "free space", noRequest: true, mk: func(t *testing.T, base string) scenario {
			t.Helper()
			sc := liveOf(t, base, serveObject)
			sc.tune = func(f *Fetcher) { f.statfs = func(string) (uint64, error) { return 1, nil } }
			return sc
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runScenario(t, tt.mk)
			start := time.Now()
			r.fetchOnce()
			if took := time.Since(start); took > 2*time.Second {
				t.Errorf("the fetch took %s, want under 2s", took)
			}

			row := r.wantRow(r.sc.key, fleet.CacheFailed, 1)
			if want := fetchT0().Add(time.Minute); !row.NextAttemptAt.Equal(want) {
				t.Errorf("NextAttemptAt = %v, want %v", row.NextAttemptAt, want)
			}
			if !strings.Contains(row.LastError, tt.want) {
				t.Errorf("last_error = %q, want it to contain %q", row.LastError, tt.want)
			}
			if ev := r.lastEvent(); ev.To != fleet.CacheFailed || ev.Class != "other" || ev.Err == nil || ev.Attempts != 1 {
				t.Errorf("last event = %+v, want To failed, Class other, Attempts 1 and an Err", ev)
			}
			if got := r.counter(failuresCounter, "other"); got != 1 {
				t.Errorf("%s{class=other} = %d, want 1", failuresCounter, got)
			}
			if tt.noRequest && len(r.up.seen()) != 0 {
				t.Errorf("the upstream saw %v, want no request", r.up.seen())
			}
			if tt.check != nil {
				tt.check(t, r.harness, r.e)
			}
		})
	}
}

func TestFetcher_TenthFailureClearsBootNext(t *testing.T) {
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		return liveOf(t, base, func([]byte) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
		})
	})
	m, err := r.repo.RecordBoot(t.Context(), fleet.BootRecord{
		Identity: fleet.Identity{UUID: fixtureUUID, MAC: "aa:bb:cc:00:00:01", Serial: "S1", Product: "P1"},
		Arch:     fleet.ArchAMD64, Trigger: fleet.TriggerFirstContact,
		Event: fleet.BootEvent{Row: 7, Action: "hold", Reason: "awaiting approval"}, At: fetchT0(),
	})
	if err != nil {
		t.Fatalf("RecordBoot: %v", err)
	}
	// Flag the machine first: SetBootNext re-arms the row, so attempts 9 goes in after it.
	if _, err := r.repo.SetBootNext(t.Context(), fleet.SetBootNextRequest{MachineID: m.ID, Entry: "tlive", Pin: r.e.Pin.ID, At: fetchT0()}); err != nil {
		t.Fatalf("SetBootNext: %v", err)
	}
	if _, err := r.d.Bun().ExecContext(t.Context(), "UPDATE utility_cache SET attempts = 9 WHERE entry = 'tlive' AND arch = 'amd64'"); err != nil {
		t.Fatalf("set attempts: %v", err)
	}

	r.fetchOnce()

	r.wantRow(tliveAMD64(), fleet.CacheBroken, fleet.MaxFetchAttempts)
	got, err := r.repo.Get(t.Context(), m.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.BootNext != "" || !got.BootNextSetAt.IsZero() {
		t.Errorf("boot-next = %q set at %v, want it cleared", got.BootNext, got.BootNextSetAt)
	}
	var cleared int
	err = r.d.Bun().NewRaw("SELECT count(*) FROM audit_events WHERE action = ? AND machine_id = ? AND detail LIKE ?",
		"boot_next.cleared", m.ID, `%"reason":"broken"%`).Scan(t.Context(), &cleared)
	if err != nil || cleared != 1 {
		t.Errorf("boot_next.cleared audit events with reason broken = %d, %v; want 1", cleared, err)
	}
	if ev := r.lastEvent(); ev.To != fleet.CacheBroken || ev.Class != "other" || ev.Attempts != fleet.MaxFetchAttempts {
		t.Errorf("last event = %+v, want To broken, Class other, Attempts %d", ev, fleet.MaxFetchAttempts)
	}
}

func TestFetcher_RepairsTruncatedLayer(t *testing.T) {
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		fx := catalogtest.New(t, base)
		return scenario{cat: fx.Catalog, key: tliveAMD64(), fx: fx, handler: objectsAt(base, fx.Objects)}
	})
	r.fetchOnce()
	r.wantRow(tliveAMD64(), fleet.CacheReady, 0)

	blob := r.layerFile(r.e, squashfsPath)
	if err := os.Chmod(blob, 0o644); err != nil { // blobs are 0444; production never chmods
		t.Fatalf("chmod blob: %v", err)
	}
	if err := os.Truncate(blob, 1000); err != nil {
		t.Fatalf("truncate blob: %v", err)
	}
	if moved, err := r.repo.Requeue(t.Context(), tliveAMD64(), r.e.Pin.ID, fetchT0()); err != nil || !moved {
		t.Fatalf("Requeue = %v, %v; want true, nil", moved, err)
	}

	r.fetchOnce()

	r.wantRow(tliveAMD64(), fleet.CacheReady, 0)
	r.wantStored(r.e, r.files())
	if fi, err := os.Stat(blob); err != nil || fi.Size() != squashfsSize {
		t.Errorf("blob = %v, %v; want %d bytes", fi, err, squashfsSize)
	}
	if got := len(r.up.seen()); got != 2 {
		t.Errorf("requests = %d, want 2: the repair downloads again", got)
	}
}

const (
	failReadyTrigger = `CREATE TRIGGER inject BEFORE UPDATE OF state ON utility_cache WHEN NEW.state = 'ready' BEGIN SELECT RAISE(ABORT, 'injected'); END`
	dropTrigger      = `DROP TRIGGER inject`
)

// injectedRun is a tlive fetch whose MarkReady fails from the start.
func injectedRun(t *testing.T) *run {
	t.Helper()
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		return liveOf(t, base, serveObject)
	})
	if _, err := r.d.Bun().ExecContext(t.Context(), failReadyTrigger); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	return r
}

func TestFetcher_BookkeepingRetriedNextPoll(t *testing.T) {
	r := injectedRun(t)

	if _, err := r.once(); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("runOnce = %v, want the injected MarkReady error", err)
	}
	r.wantRow(tliveAMD64(), fleet.CacheFetching, 0)

	if _, err := r.d.Bun().ExecContext(t.Context(), dropTrigger); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	r.fetchOnce()

	r.wantRow(tliveAMD64(), fleet.CacheReady, 0)
	if got := len(r.up.seen()); got != 1 {
		t.Errorf("requests = %d, want 1: the retry stores the outcome without downloading", got)
	}
	r.wantStored(r.e, r.files())
	r.wantSpool()
	if got, want := r.transitions(), []string{"wanted>fetching", "fetching>ready"}; !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

func TestFetcher_BookkeepingFailsTwiceResetsRow(t *testing.T) {
	r := injectedRun(t)

	if _, err := r.once(); err == nil {
		t.Fatal("first runOnce succeeded, want the injected error")
	}
	r.wantRow(tliveAMD64(), fleet.CacheFetching, 0)
	if _, err := r.once(); err == nil {
		t.Fatal("second runOnce succeeded, want the injected error again")
	}
	r.wantRow(tliveAMD64(), fleet.CacheWanted, 0)
	if got, want := r.transitions(), []string{"wanted>fetching", "fetching>wanted"}; !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	if ev := r.lastEvent(); ev.Err == nil || !strings.Contains(ev.Err.Error(), "injected") || ev.Class != "" {
		t.Errorf("last event = %+v, want the held cause as Err and no Class", ev)
	}
}

// TestFetcher_ResetOfAMovedRowIsSilent moves the held row before the poll that
// resets it: the reset did not move that row at that pin, so no event says it
// did. The reset is of the held row at its pin only, so a row that was claimed
// again at another pin is left to the worker that holds it.
func TestFetcher_ResetOfAMovedRowIsSilent(t *testing.T) {
	const newPin = "repinned"
	repin := func(t *testing.T, r *run) {
		t.Helper()
		if moved, err := r.repo.Repin(t.Context(), tliveAMD64(), r.e.Pin.ID, newPin, fetchT0()); err != nil || !moved {
			t.Fatalf("Repin = %v, %v; want true, nil", moved, err)
		}
	}
	tests := []struct {
		name      string
		move      func(t *testing.T, r *run)
		wantPin   func(r *run) string
		wantState fleet.CacheState
	}{
		{"another reset moved the row first", func(t *testing.T, r *run) {
			t.Helper()
			if n, err := r.repo.ResetFetching(t.Context(), fetchT0()); err != nil || n != 1 {
				t.Fatalf("ResetFetching = %d, %v; want 1, nil", n, err)
			}
		}, func(r *run) string { return r.e.Pin.ID }, fleet.CacheWanted},
		{"the row was repinned", repin, func(*run) string { return newPin }, fleet.CacheWanted},
		{"the row was repinned and claimed again", func(t *testing.T, r *run) {
			t.Helper()
			repin(t, r)
			// As another worker's claim would; its row, at another pin, is not the held one.
			if _, ok, err := r.repo.ClaimFetch(t.Context(), fetchT0()); err != nil || !ok {
				t.Fatalf("ClaimFetch = %v, %v; want true, nil", ok, err)
			}
		}, func(*run) string { return newPin }, fleet.CacheFetching},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runScenario(t, func(t *testing.T, base string) scenario {
				t.Helper()
				return liveOf(t, base, serveObject)
			})
			// MarkReady and the reset both fail, so the outcome stays held with its retry spent.
			trigger := `CREATE TRIGGER inject BEFORE UPDATE OF state ON utility_cache WHEN NEW.state IN ('ready', 'wanted') BEGIN SELECT RAISE(ABORT, 'injected'); END`
			if _, err := r.d.Bun().ExecContext(t.Context(), trigger); err != nil {
				t.Fatalf("create trigger: %v", err)
			}
			for i := range 2 {
				if _, err := r.once(); err == nil {
					t.Fatalf("runOnce %d succeeded, want the injected error", i+1)
				}
			}
			if _, err := r.d.Bun().ExecContext(t.Context(), dropTrigger); err != nil {
				t.Fatalf("drop trigger: %v", err)
			}
			tt.move(t, r)

			if _, err := r.once(); err != nil {
				t.Fatalf("runOnce = %v, want nil", err)
			}

			if row, want := r.wantRow(tliveAMD64(), tt.wantState, 0), tt.wantPin(r); row.Pin != want {
				t.Errorf("row pin = %q, want %q", row.Pin, want)
			}
			if got, want := r.transitions(), []string{"wanted>fetching"}; !slices.Equal(got, want) {
				t.Errorf("events = %v, want %v", got, want)
			}
		})
	}
}

// TestFetcher_HeldResetOnlyOwnRow gives a worker whose outcome the database
// refused twice its own row back to wanted, and leaves a row another worker
// holds fetching.
func TestFetcher_HeldResetOnlyOwnRow(t *testing.T) {
	r := injectedRun(t)
	r.request("tshell", "amd64", 1)
	// Another worker holds tshell: it claimed the row, and never settles it here.
	other, ok, err := r.repo.ClaimFetch(t.Context(), fetchT0())
	if err != nil || !ok || other.Key() != tshellAMD64() {
		t.Fatalf("ClaimFetch = %v, %v, %v; want the tshell row, true, nil", other.Key(), ok, err)
	}

	for i := range 2 {
		if _, err := r.once(); err == nil {
			t.Fatalf("runOnce %d succeeded, want the injected error", i+1)
		}
	}

	r.wantRow(tliveAMD64(), fleet.CacheWanted, 0)
	r.wantRow(tshellAMD64(), fleet.CacheFetching, 0)
}

// TestFetcher_HeldResetEventIsPrintable pins that the reset event's Err is
// printable text, like every FetchEvent's: the database error that kept the
// outcome from being stored may hold control and format runes.
func TestFetcher_HeldResetEventIsPrintable(t *testing.T) {
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		return liveOf(t, base, serveObject)
	})
	trigger := "CREATE TRIGGER inject BEFORE UPDATE OF state ON utility_cache WHEN NEW.state = 'ready' BEGIN SELECT RAISE(ABORT, 'inj\x1b\u202eected'); END"
	if _, err := r.d.Bun().ExecContext(t.Context(), trigger); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	for i := range 2 {
		if _, err := r.once(); err == nil {
			t.Fatalf("runOnce %d succeeded, want the injected error", i+1)
		}
	}

	ev := r.lastEvent()

	if ev.To != fleet.CacheWanted || ev.Err == nil {
		t.Fatalf("last event = %+v, want the reset to wanted with an Err", ev)
	}
	if got := ev.Err.Error(); !strings.Contains(got, "inj") || !strings.Contains(got, "ected") {
		t.Errorf("event Err = %q, want it to keep the printable text", got)
	}
	for _, c := range ev.Err.Error() {
		if !unicode.IsGraphic(c) {
			t.Errorf("event Err = %q holds the non-printable rune %U", ev.Err, c)
			break
		}
	}
}

func TestFetcher_ShutdownAbandons(t *testing.T) {
	chunk := make(chan struct{})
	var once sync.Once
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		return liveOf(t, base, func(object []byte) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("ETag", testETag)
				w.Header().Set("Content-Length", strconv.Itoa(len(object)))
				w.WriteHeader(http.StatusOK)
				send(t, w, object[:64<<10])
				flush(t, w)
				once.Do(func() { close(chunk) })
				<-r.Context().Done()
			})
		})
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	spool := filepath.Join(r.spool, SpoolName(r.e.Pin.ID, 0))
	go func() {
		select {
		case <-chunk:
		case <-ctx.Done():
			return
		}
		// The handler having flushed is not the client having the chunk: cancelled
		// before the response arrives, the fetch leaves no spool at all.
		deadline := time.Now().Add(10 * time.Second)
		for {
			if fi, err := os.Stat(spool); err == nil && fi.Size() >= 64<<10 {
				cancel()
				return
			}
			if time.Now().After(deadline) {
				t.Error("timed out waiting for the spool to hold the chunk")
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()

	_, err := r.w.runOnce(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runOnce = %v, want context.Canceled", err)
	}
	r.wantRow(tliveAMD64(), fleet.CacheFetching, 0)
	r.wantSpool(SpoolName(r.e.Pin.ID, 0), ETagName(r.e.Pin.ID, 0))
}

func TestFetcher_PriorityFirst(t *testing.T) {
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		fx := catalogtest.New(t, base)
		return scenario{cat: fx.Catalog, key: tliveARM64(), fx: fx, handler: objectsAt(base, fx.Objects)}
	})
	r.request("tshell", "amd64", 1)

	r.fetchOnce()

	r.wantRow(tshellAMD64(), fleet.CacheReady, 0)
	r.wantRow(tliveARM64(), fleet.CacheWanted, 0)
}

// closedHarness is a harness whose database has been closed, so every claim fails.
func closedHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, catalogtest.New(t, "https://example.test").Catalog, NewHTTPClient(testUserAgent, nil))
	if err := h.d.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	return h
}

func TestFetcher_RunSurvivesRepositoryError(t *testing.T) {
	h := closedHarness(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	h.f.poll = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.f.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for len(h.handlerErrors()) < 2 {
		select {
		case err := <-done:
			t.Fatalf("Run returned %v after %d errors, want it to keep polling", err, len(h.handlerErrors()))
		case <-deadline:
			t.Fatalf("the error handler was called %d times in 5s, want at least 2", len(h.handlerErrors()))
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil on a done context", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func TestFetcher_RunStops(t *testing.T) {
	h := closedHarness(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	go func() { done <- h.f.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return for a cancelled context")
	}
	if got := h.handlerErrors(); len(got) != 0 {
		t.Errorf("the error handler got %v for the context's own cancellation", got)
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"userinfo, query and fragment", "https://u:p@h/x?" + secretQuery + "#frag", "https://h/x"},
		{"a bare question mark", "https://h/x?", "https://h/x"},
		{"nothing to strip", "https://h/x", "https://h/x"},
		{"unparsable", "https://h/%zz?" + secretQuery, "<unparsable>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactURL(tt.in); got != tt.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRedactURLError(t *testing.T) {
	tests := []struct {
		name string
		in   error
		want string
	}{
		{"userinfo, query and fragment", &url.Error{Op: "Get", URL: "https://u:p@h/x?" + secretQuery + "#frag", Err: io.EOF}, `Get "https://h/x": EOF`},
		{"unparsable", &url.Error{Op: "Get", URL: "https://h/%zz?" + secretQuery, Err: io.EOF}, `Get "<unparsable>": EOF`},
		{"wrapped", fmt.Errorf("outer: %w", &url.Error{Op: "Head", URL: "https://h/?" + secretQuery, Err: io.EOF}), `Head "https://h/": EOF`},
		{"unparsable Location", &url.Error{Op: "Get", URL: "https://h/x", Err: fmt.Errorf("failed to parse Location header %q: %s", "https://h/%zz?"+secretQuery, io.EOF.Error())}, `Get "https://h/x": unparsable Location header`},
		{"not a url error", io.EOF, "EOF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactURLError(tt.in).Error(); got != tt.want {
				t.Errorf("RedactURLError = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("keeps the cause", func(t *testing.T) {
		in := &url.Error{Op: "Get", URL: "https://h/?" + secretQuery, Err: &IntegrityError{Err: io.EOF}}
		got := RedactURLError(in)
		if _, ok := errors.AsType[*IntegrityError](got); !ok {
			t.Errorf("RedactURLError(%v) lost the *IntegrityError", in)
		}
		if !errors.Is(got, io.EOF) {
			t.Errorf("RedactURLError(%v) lost io.EOF", in)
		}
	})
}

func TestFetcher_RedirectURLsAreRedacted(t *testing.T) {
	tests := []struct {
		name   string
		target func(t *testing.T) (loc string, pool []*httptest.Server)
		cause  string // the fetch error's own text must survive redaction
	}{
		{"tls12 target", func(t *testing.T) (string, []*httptest.Server) {
			t.Helper()
			mirror := startTLS(t, http.NotFoundHandler(), true)
			return mirror.URL + "/x?" + secretQuery, []*httptest.Server{mirror}
		}, "protocol version"},
		{"plain http target", func(*testing.T) (string, []*httptest.Server) {
			return "http://127.0.0.1:1/x?" + secretQuery, nil
		}, "https only"},
		{"unparsable Location", func(*testing.T) (string, []*httptest.Server) {
			return "https://h/%zz?" + secretQuery, nil
		}, "unparsable Location"},
	}
	for _, tt := range tests {
		mk := func(t *testing.T, base string) scenario {
			t.Helper()
			loc, pool := tt.target(t)
			sc := liveOf(t, base, func([]byte) http.Handler { return redirectTo(loc) })
			sc.pool = pool
			return sc
		}
		t.Run(tt.name+"/stored", func(t *testing.T) {
			r := runScenario(t, mk)
			r.fetchOnce()

			row := r.row(tliveAMD64())
			if strings.Contains(row.LastError, "s3cret") || !strings.Contains(row.LastError, tt.cause) {
				t.Errorf("last_error = %q, want %q and no secret", row.LastError, tt.cause)
			}
			if ev := r.lastEvent(); ev.Err == nil || strings.Contains(ev.Err.Error(), "s3cret") {
				t.Errorf("event Err = %v, want an error with no secret", ev.Err)
			}
		})
		t.Run(tt.name+"/unrecorded", func(t *testing.T) {
			r := runScenario(t, mk)
			trigger := `CREATE TRIGGER inject BEFORE UPDATE OF state ON utility_cache WHEN NEW.state IN ('failed', 'broken') BEGIN SELECT RAISE(ABORT, 'injected'); END`
			if _, err := r.d.Bun().ExecContext(t.Context(), trigger); err != nil {
				t.Fatalf("create trigger: %v", err)
			}

			_, err := r.once()

			// Run hands this error to the handler unchanged.
			if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), tt.cause) {
				t.Errorf("runOnce = %v, want an error that names %q and holds no secret", err, tt.cause)
			}
			if _, ok := errors.AsType[*IntegrityError](err); ok {
				t.Errorf("runOnce = %v, which classes as integrity though the repository failed", err)
			}
		})
	}
}

// blockUntilDone is a reader that blocks until ctx is done, then fails with its cause.
func blockUntilDone(ctx context.Context) readerFunc {
	return func([]byte) (int, error) {
		<-ctx.Done()
		return 0, context.Cause(ctx)
	}
}

// readerFunc is an io.Reader made of a function.
type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestIdleReader(t *testing.T) {
	defer goleak.VerifyNone(t)
	t.Run("progress resets the deadline", func(t *testing.T) {
		pr, pw := io.Pipe()
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		ir := newIdleReader(pr, 300*time.Millisecond, cancel)
		defer ir.Stop()
		go func() {
			for range 4 { // 400 ms in all, more than one idle period
				time.Sleep(100 * time.Millisecond)
				if _, err := pw.Write([]byte("x")); err != nil {
					return
				}
			}
			if err := pw.Close(); err != nil {
				t.Logf("close pipe: %v", err)
			}
		}()

		got, err := io.ReadAll(ir)

		if err != nil || string(got) != "xxxx" {
			t.Errorf("ReadAll = %q, %v; want xxxx, nil", got, err)
		}
		if ctx.Err() != nil {
			t.Errorf("the request context was cancelled: %v", context.Cause(ctx))
		}
	})

	t.Run("a stall cancels the request", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		ir := newIdleReader(blockUntilDone(ctx), 50*time.Millisecond, cancel)
		defer ir.Stop()

		_, err := ir.Read(make([]byte, 1))

		if !errors.Is(err, errIdle) || !errors.Is(context.Cause(ctx), errIdle) {
			t.Errorf("Read = %v, cause = %v; want errIdle in both", err, context.Cause(ctx))
		}
	})

	t.Run("Stop prevents the cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(t.Context())
		defer cancel(nil)
		ir := newIdleReader(blockUntilDone(ctx), 50*time.Millisecond, cancel)
		ir.Stop()

		time.Sleep(200 * time.Millisecond)

		if ctx.Err() != nil {
			t.Errorf("the request was cancelled after Stop: %v", context.Cause(ctx))
		}
	})
}

// droppedSidecar starts a live fetch from half a spool under a strong sidecar,
// and answers with a 200 that carries extraHeader (an ETag line, or none) and
// half the body, then drops the connection.
func droppedSidecar(t *testing.T, base, extraHeader string) scenario {
	t.Helper()
	sc := liveOf(t, base, func(object []byte) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			head := fmt.Sprintf("HTTP/1.1 200 OK\r\n%sContent-Length: %d\r\n\r\n", extraHeader, len(object))
			hijackSend(t, w, head+string(object[:len(object)/2]))
		})
	})
	sc.seed = seedHalf(sc.object)
	return sc
}

func wantSidecarGone(t *testing.T, h *harness, e catalog.Entry) {
	t.Helper()
	if got := h.spoolSize(ETagName(e.Pin.ID, 0)); got != -1 {
		t.Errorf("sidecar size = %d, want it removed", got)
	}
}

func TestFetcher_PinNotInCatalog(t *testing.T) {
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		return liveOf(t, base, serveObject)
	})
	if moved, err := r.repo.Repin(t.Context(), tliveAMD64(), r.e.Pin.ID, "000000000000", fetchT0()); err != nil || !moved {
		t.Fatalf("Repin = %v, %v; want true, nil", moved, err)
	}

	r.fetchOnce()

	row := r.wantRow(tliveAMD64(), fleet.CacheFailed, 1)
	if !strings.Contains(row.LastError, "not in the catalog at this pin") {
		t.Errorf("last_error = %q, want it to say the pin is not in the catalog", row.LastError)
	}
	if ev := r.lastEvent(); ev.Class != "other" || ev.To != fleet.CacheFailed {
		t.Errorf("last event = %+v, want To failed, Class other", ev)
	}
	if got := len(r.up.seen()); got != 0 {
		t.Errorf("the upstream saw %d requests, want 0", got)
	}
}

func TestExtractionError_LocalFailureIsNotIntegrity(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"rename ENOSPC", &os.LinkError{Op: "rename", Err: syscall.ENOSPC}, classOther},
		{"path EIO", &fs.PathError{Op: "write", Path: "x", Err: syscall.EIO}, classOther},
		{"syscall error", os.NewSyscallError("write", syscall.ENOSPC), classOther},
		{"path error of another kind", &fs.PathError{Op: "read", Path: "x", Err: io.ErrClosedPipe}, classOther},
		{"digest mismatch", fmt.Errorf("store x: %w", io.ErrUnexpectedEOF), classIntegrity},
		{"malformed object", fmt.Errorf("%w: cut short", ErrMalformed), classIntegrity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classOf(extractionError(tt.err)); got != tt.want {
				t.Errorf("class = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFetcher_FreeSpaceBoundary(t *testing.T) {
	tests := []struct {
		name      string
		short     uint64 // how far below the need the volume is
		size      int64  // the pinned size when not 0, on a volume reporting the most free space it can
		wantReady bool
	}{
		{"one byte short fails", 1, 0, false},
		{"exactly enough passes", 0, 0, true},
		// Twice this plus the reserve is 2^64, which wraps to 0 in uint64.
		{"a size whose need overflows fails", 0, math.MaxInt64 - spaceReserve/2 + 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runScenario(t, func(t *testing.T, base string) scenario {
				t.Helper()
				sc := fixtureOf(t, base, tshellAMD64(), serveObject)
				free := 2*uint64(len(sc.object)) + spaceReserve - tt.short
				if tt.size != 0 {
					e, _ := sc.cat.Lookup("tshell", "amd64")
					e.Pin.Objects[0].Size = tt.size
					e.Pin.ID = catalog.PinID(e.Pin.Objects)
					sc.cat = singleCatalog(t, e.Spec, e.Pin)
					free = math.MaxUint64
				}
				sc.tune = func(f *Fetcher) { f.statfs = func(string) (uint64, error) { return free, nil } }
				return sc
			})
			r.fetchOnce()

			if tt.wantReady {
				r.wantRow(tshellAMD64(), fleet.CacheReady, 0)
				return
			}
			row := r.wantRow(tshellAMD64(), fleet.CacheFailed, 1)
			if !strings.Contains(row.LastError, "free space") {
				t.Errorf("last_error = %q, want it to mention free space", row.LastError)
			}
			if got := len(r.up.seen()); got != 0 {
				t.Errorf("the upstream saw %d requests, want 0", got)
			}
		})
	}
}

// blockingAt serves next, except that a request for one of paths first waits
// for release, or for the client to give up.
func blockingAt(next http.Handler, release <-chan struct{}, paths ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slices.Contains(paths, r.URL.Path) {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// meetingOf serves next once n requests are waiting, so n downloads start together.
func meetingOf(next http.Handler, n int32) http.Handler {
	var arrived atomic.Int32
	all := make(chan struct{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if arrived.Add(1) == n {
			close(all)
		}
		select {
		case <-all:
		case <-r.Context().Done():
			return
		}
		next.ServeHTTP(w, r)
	})
}

// waitFor polls cond until it holds, and fails the test after 10 seconds.
func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) waitRow(key fleet.CacheKey, state fleet.CacheState) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("%v to be %s", key, state), func() bool { return h.row(key).State == state })
}

// startRun runs the fetcher's pool, polling every 20 ms. The returned function
// cancels it and fails the test unless Run returned nil only once it was
// cancelled and every worker loop is gone; it is safe to call twice.
func (h *harness) startRun() (stop func()) {
	h.t.Helper()
	if h.f.poll == PollInterval { // a test may set its own
		h.f.poll = 20 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(h.t.Context())
	done := make(chan error, 1)
	go func() { done <- h.f.Run(ctx) }()
	return sync.OnceFunc(func() {
		select {
		case err := <-done:
			h.t.Errorf("Run returned %v before its context was done", err)
			return
		default:
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				h.t.Errorf("Run = %v, want nil on a done context", err)
			}
		case <-time.After(10 * time.Second):
			h.t.Error("Run did not return after its context was cancelled")
			return
		}
		// goleak retries until the goroutines are gone, which would hide a Run
		// that returned too soon: look once, the moment Run has returned.
		stacks := make([]byte, 1<<20)
		if bytes.Contains(stacks[:runtime.Stack(stacks, true)], []byte("toolcache.(*worker).run")) {
			h.t.Error("Run returned while a worker loop was still running")
		}
	})
}

// poolScenario is the fixture catalog, with the upstream serve builds from its
// objects, fetched by a pool of workers.
func poolScenario(workers int, serve func(base string, objects map[string][]byte) http.Handler) func(t *testing.T, base string) scenario {
	return func(t *testing.T, base string) scenario {
		t.Helper()
		fx := catalogtest.New(t, base)
		return scenario{
			cat: fx.Catalog, key: tliveAMD64(), fx: fx, handler: serve(base, fx.Objects),
			tune: func(f *Fetcher) { f.cfg.Workers = workers },
		}
	}
}

// TestFetcher_PoolSlowDoesNotBlockFast is the stall the pool exists to end: one
// upstream that does not answer must not hold back a download from another.
func TestFetcher_PoolSlowDoesNotBlockFast(t *testing.T) {
	release := make(chan struct{})
	r := runScenario(t, poolScenario(2, func(base string, objects map[string][]byte) http.Handler {
		return blockingAt(objectsAt(base, objects), release, "/tlive-amd64.zip")
	}))
	r.request("tshell", "amd64", 0)
	stop := r.startRun()
	defer stop()
	// The slow download is tlive/amd64: it must be claimed before the fast one is judged.
	r.waitRow(tliveAMD64(), fleet.CacheFetching)

	r.waitRow(tshellAMD64(), fleet.CacheReady)

	r.wantRow(tliveAMD64(), fleet.CacheFetching, 0)
	close(release)
	r.waitRow(tliveAMD64(), fleet.CacheReady)
}

// TestFetcher_PoolShutdownLeavesNoGoroutines cancels a pool of four with two
// downloads in flight: Run returns nil once every loop has, and nothing is left running.
func TestFetcher_PoolShutdownLeavesNoGoroutines(t *testing.T) {
	r := runScenario(t, poolScenario(4, func(base string, objects map[string][]byte) http.Handler {
		return blockingAt(objectsAt(base, objects), nil, "/tlive-amd64.zip", "/tlive-arm64.zip")
	}))
	r.request("tlive", "arm64", 0)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer r.f.cfg.Client.CloseIdleConnections()
	stop := r.startRun()
	defer stop()
	r.waitFor("both downloads to be requested", func() bool { return len(r.up.seen()) == 2 })

	stop()

	r.wantRow(tliveAMD64(), fleet.CacheFetching, 0)
	r.wantRow(tliveARM64(), fleet.CacheFetching, 0)
}

// TestFetcher_PoolSharedBlob fetches two entries whose files include one blob
// of the same digest, both downloads at the same moment.
func TestFetcher_PoolSharedBlob(t *testing.T) {
	r := runScenario(t, poolScenario(2, func(base string, objects map[string][]byte) http.Handler {
		return meetingOf(objectsAt(base, objects), 2)
	}))
	arm := r.request("tlive", "arm64", 0)
	stop := r.startRun()
	defer stop()

	r.waitRow(tliveAMD64(), fleet.CacheReady)
	r.waitRow(tliveARM64(), fleet.CacheReady)

	if got := r.handlerErrors(); len(got) != 0 {
		t.Errorf("the error handler got %v", got)
	}
	r.wantStored(r.e, r.files())
	r.wantStored(arm, r.sc.fx.Files[catalog.Key{Entry: "tlive", Arch: "arm64"}])
}

// oneNeed is the free space that holds exactly one of the fixture's live
// downloads: twice its size, and spaceReserve left free.
func oneNeed(fx catalogtest.Fixture, base string) uint64 {
	return 2*uint64(max(len(fx.Objects[base+"/tlive-amd64.zip"]), len(fx.Objects[base+"/tlive-arm64.zip"]))) + spaceReserve
}

// TestFetcher_SpaceReserveCountedOnce has room for the large download's need,
// the need of each of two small downloads and one spaceReserve: the margin the
// volume keeps is counted once, not once per download in flight, so all three
// run together.
func TestFetcher_SpaceReserveCountedOnce(t *testing.T) {
	release := make(chan struct{})
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		sc := poolScenario(3, func(base string, objects map[string][]byte) http.Handler {
			return blockingAt(objectsAt(base, objects), release, "/tlive-amd64.zip")
		})(t, base)
		large, small := uint64(len(sc.fx.Objects[base+"/tlive-amd64.zip"])), uint64(len(sc.fx.Objects[base+"/tshell.zip"]))
		free := 2*large + 2*(2*small) + spaceReserve
		sc.tune = func(f *Fetcher) {
			f.cfg.Workers = 3
			f.statfs = func(string) (uint64, error) { return free, nil }
		}
		return sc
	})
	r.request("tshell", "amd64", 0)
	r.request("tshell", "arm64", 0)
	stop := r.startRun()
	defer stop()
	r.waitRow(tliveAMD64(), fleet.CacheFetching)

	r.waitRow(tshellAMD64(), fleet.CacheReady)
	r.waitRow(fleet.CacheKey{Entry: "tshell", Arch: fleet.ArchARM64}, fleet.CacheReady)

	r.wantRow(tliveAMD64(), fleet.CacheFetching, 0)
	close(release)
	r.waitRow(tliveAMD64(), fleet.CacheReady)
	if got := r.counter(failuresCounter, classOther); got != 0 {
		t.Errorf("%s{class=other} = %d, want 0", failuresCounter, got)
	}
}

// claimsOf is how many times the fetcher claimed key.
func (h *harness) claimsOf(key fleet.CacheKey) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	var n int
	for i := range h.events {
		if h.events[i].Key == key && h.events[i].To == fleet.CacheFetching {
			n++
		}
	}
	return n
}

// givenBack is the key of a row a worker gave back to wanted without settling it.
func (h *harness) givenBack() (fleet.CacheKey, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.events {
		if h.events[i].From == fleet.CacheFetching && h.events[i].To == fleet.CacheWanted {
			return h.events[i].Key, true
		}
	}
	return fleet.CacheKey{}, false
}

// TestFetcher_DeferredRowWaitsAPoll has room for one live download and a pool
// of two whose poll is an hour. The row that does not fit beside the other is
// claimed once, not again and again: its worker waits a poll. When the first
// download ends, the worker that finished it claims the deferred row.
func TestFetcher_DeferredRowWaitsAPoll(t *testing.T) {
	release := make(chan struct{})
	r := runScenario(t, oneAtATime(release))
	r.request("tlive", "arm64", 0)
	r.f.poll = time.Hour
	stop := r.startRun()
	defer stop()
	var deferred fleet.CacheKey
	r.waitFor("a row to be given back", func() bool {
		key, ok := r.givenBack()
		deferred = key
		return ok
	})

	time.Sleep(300 * time.Millisecond)

	if got := r.claimsOf(deferred); got != 1 {
		t.Errorf("%v was claimed %d times in the poll, want 1", deferred, got)
	}
	close(release)
	r.waitRow(tliveAMD64(), fleet.CacheReady)
	r.waitRow(tliveARM64(), fleet.CacheReady)
}

// oneAtATime is the scenario of two live downloads and a volume with room for
// one of them: the second fits alone but not beside the first, which release
// holds up.
func oneAtATime(release <-chan struct{}) func(t *testing.T, base string) scenario {
	return func(t *testing.T, base string) scenario {
		t.Helper()
		sc := poolScenario(2, func(base string, objects map[string][]byte) http.Handler {
			return blockingAt(objectsAt(base, objects), release, "/tlive-amd64.zip", "/tlive-arm64.zip")
		})(t, base)
		free := oneNeed(sc.fx, base)
		sc.tune = func(f *Fetcher) {
			f.cfg.Workers = 2
			f.statfs = func(string) (uint64, error) { return free, nil }
		}
		return sc
	}
}

// TestFetcher_SecondThatFitsAloneIsDeferred has room for one download. While
// the first is in flight the second does not fit beside it, but it would fit
// alone: it goes back to wanted with no attempt, no backoff and no failure, and
// downloads once the first is done.
func TestFetcher_SecondThatFitsAloneIsDeferred(t *testing.T) {
	release := make(chan struct{})
	r := runScenario(t, oneAtATime(release))
	r.request("tlive", "arm64", 0)
	firstDone := make(chan error, 1)
	go func() {
		_, err := r.w.runOnce(t.Context())
		firstDone <- err
	}()
	r.waitFor("the first download to be requested", func() bool { return len(r.up.seen()) == 1 })
	second := &worker{f: r.f}

	claimed, err := second.runOnce(t.Context())

	if !claimed || !errors.Is(err, errSpaceBusy) {
		t.Fatalf("runOnce = %v, %v; want true and errSpaceBusy", claimed, err)
	}
	row := r.wantRow(tliveARM64(), fleet.CacheWanted, 0)
	if row.LastError != "" || !row.NextAttemptAt.IsZero() {
		t.Errorf("the deferred row holds last_error %q and next_attempt_at %v, want neither", row.LastError, row.NextAttemptAt)
	}
	if ev := r.lastEvent(); ev.Key != tliveARM64() || ev.From != fleet.CacheFetching || ev.To != fleet.CacheWanted || ev.Class != "" || ev.Err == nil || ev.Attempts != 0 {
		t.Errorf("last event = %+v, want tlive/arm64 fetching to wanted, no class, no attempt and the reason", ev)
	}
	for _, class := range []string{classOther, classIntegrity} {
		if got := r.counter(failuresCounter, class); got != 0 {
			t.Errorf("%s{class=%s} = %d, want 0", failuresCounter, class, got)
		}
	}
	if got := len(r.up.seen()); got != 1 {
		t.Errorf("the upstream saw %d requests, want only the first download's", got)
	}

	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("the first runOnce: %v", err)
	}
	r.wantRow(tliveAMD64(), fleet.CacheReady, 0)
	r.fetchOnce()
	r.wantRow(tliveARM64(), fleet.CacheReady, 0)
}

// TestFetcher_ReservationReleasedOnFailure has room for one download. However
// the first ends, the second is not refused for room that is no longer in use.
func TestFetcher_ReservationReleasedOnFailure(t *testing.T) {
	tests := []struct {
		name string
		// first serves the first download's object; cancel ends its context.
		first func(object []byte, cancel context.CancelFunc) http.Handler
		// wantState and wantAttempts are where tlive/amd64, the first row claimed, ends.
		wantState    fleet.CacheState
		wantAttempts int
	}{
		{"integrity failure", func(object []byte, _ context.CancelFunc) http.Handler {
			bad := slices.Clone(object)
			bad[0] ^= 0xff
			return serveObject(bad)
		}, fleet.CacheBroken, 1},
		{"other failure", func([]byte, context.CancelFunc) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
		}, fleet.CacheFailed, 1},
		{"cancelled", func(_ []byte, cancel context.CancelFunc) http.Handler {
			return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				cancel()
				<-r.Context().Done()
			})
		}, fleet.CacheFetching, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := runScenario(t, func(t *testing.T, base string) scenario {
				t.Helper()
				sc := poolScenario(1, func(base string, objects map[string][]byte) http.Handler {
					mux := http.NewServeMux()
					mux.Handle("GET /tlive-amd64.zip", tt.first(objects[base+"/tlive-amd64.zip"], cancel))
					mux.Handle("GET /", objectsAt(base, objects))
					return mux
				})(t, base)
				free := oneNeed(sc.fx, base)
				sc.tune = func(f *Fetcher) { f.statfs = func(string) (uint64, error) { return free, nil } }
				return sc
			})
			r.request("tlive", "arm64", 0)

			if claimed, _ := r.w.runOnce(ctx); !claimed {
				t.Fatal("the first runOnce claimed nothing")
			}
			// The first claim must have been tlive/amd64, the row whose upstream misbehaves.
			if got := r.row(tliveAMD64()).State; got == fleet.CacheWanted {
				t.Fatalf("tlive/amd64 is %s, want it claimed first", got)
			}
			r.wantRow(tliveAMD64(), tt.wantState, tt.wantAttempts)
			r.fetchOnce()

			r.wantRow(tliveARM64(), fleet.CacheReady, 0)
		})
	}
}

// TestFetcher_SpooledObjectReservesItsExtraction holds a job on an object that
// is whole in the spool: extracting it still writes about its size into the
// store, so the job holds that room, and another job's space check sees it.
func TestFetcher_SpooledObjectReservesItsExtraction(t *testing.T) {
	r := runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		sc := poolScenario(1, objectsAt)(t, base)
		spooled, other := uint64(len(sc.fx.Objects[base+"/tlive-amd64.zip"])), uint64(len(sc.fx.Objects[base+"/tlive-arm64.zip"]))
		// Room for the other job's whole need, but for the byte the spooled job's extraction holds.
		free := spooled + 2*other + spaceReserve - 1
		sc.seed = func(t *testing.T, h *harness, e catalog.Entry) {
			t.Helper()
			seedSpool(t, h, e, sc.fx.Objects[base+"/tlive-amd64.zip"], "")
		}
		sc.tune = func(f *Fetcher) { f.statfs = func(string) (uint64, error) { return free, nil } }
		return sc
	})
	arm := r.request("tlive", "arm64", 0)
	jobOf := func(key fleet.CacheKey, e catalog.Entry) *fetchJob {
		t.Helper()
		root, err := os.OpenRoot(r.spool)
		if err != nil {
			t.Fatalf("OpenRoot: %v", err)
		}
		return &fetchJob{f: r.f, row: r.row(key), entry: e, root: root}
	}
	spooledJob, otherJob := jobOf(tliveAMD64(), r.e), jobOf(tliveARM64(), arm)
	defer spooledJob.close(t.Context())
	defer otherJob.close(t.Context())

	if err := spooledJob.download(t.Context(), 0); err != nil {
		t.Fatalf("download of the spooled object: %v", err)
	}

	if want := uint64(r.e.Pin.Objects[0].Size); r.f.reserved != want {
		t.Errorf("reserved = %d, want %d: the extraction share", r.f.reserved, want)
	}
	if err := otherJob.download(t.Context(), 0); !errors.Is(err, errSpaceBusy) {
		t.Errorf("the other job's download = %v, want errSpaceBusy: it fits alone, not beside the extraction", err)
	}
}

// TestFetchJob_ReserveSpaceSwaps pins that a job with several objects holds one
// reservation, the latest, not a stack of them: its first object is on the disk
// by the time it asks for the second, so counting it again would refuse the job
// for room it does not need.
func TestFetchJob_ReserveSpaceSwaps(t *testing.T) {
	h := newHarness(t, catalogtest.New(t, "https://example.test").Catalog, NewHTTPClient(testUserAgent, nil))
	const first, second = int64(1 << 20), int64(2 << 20)
	need2 := 2 * uint64(second)
	h.f.statfs = func(string) (uint64, error) { return need2 + spaceReserve, nil } // room for one of them
	j := &fetchJob{f: h.f}

	if err := j.reserveSpace(first, 2); err != nil {
		t.Fatalf("reserveSpace of the first object: %v", err)
	}
	if err := j.reserveSpace(second, 2); err != nil {
		t.Fatalf("reserveSpace of the second object: %v, want the first reservation swapped out, not counted against it", err)
	}

	if h.f.reserved != need2 || j.reserved != need2 {
		t.Errorf("reserved = %d, job holds %d; want both %d", h.f.reserved, j.reserved, need2)
	}
	j.close(t.Context())
	if h.f.reserved != 0 {
		t.Errorf("reserved after close = %d, want 0", h.f.reserved)
	}
}

// TestFetchJob_ReserveSpaceRefusals pins the two refusals apart: a download
// that fits on the volume alone but not beside the others in flight is
// errSpaceBusy, and one the volume cannot hold even alone is an ordinary
// failure, however much the others hold.
func TestFetchJob_ReserveSpaceRefusals(t *testing.T) {
	const size = int64(1 << 20)
	need, other := 2*uint64(size), uint64(1<<20) // other is what another download holds
	tests := []struct {
		name         string
		free, others uint64
		wantBusy     bool
		wantErr      bool
	}{
		{"room beside the others", other + need + spaceReserve, other, false, false},
		{"one byte short beside the others", other + need + spaceReserve - 1, other, true, true},
		{"fits alone, the others hold the rest", need + spaceReserve, other, true, true},
		{"one byte short of fitting alone", need + spaceReserve - 1, 0, false, true},
		{"one byte short of fitting alone, beside others", need + spaceReserve - 1, other, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, catalogtest.New(t, "https://example.test").Catalog, NewHTTPClient(testUserAgent, nil))
			h.f.statfs = func(string) (uint64, error) { return tt.free, nil }
			h.f.reserved = tt.others
			j := &fetchJob{f: h.f}

			err := j.reserveSpace(size, 2)

			if (err != nil) != tt.wantErr || errors.Is(err, errSpaceBusy) != tt.wantBusy {
				t.Fatalf("reserveSpace = %v, want an error: %t, errSpaceBusy: %t", err, tt.wantErr, tt.wantBusy)
			}
			if err != nil && !strings.Contains(err.Error(), "free space") {
				t.Errorf("reserveSpace = %v, want it to mention free space", err)
			}
			want := tt.others // a refusal holds nothing
			if err == nil {
				want += need
			}
			if h.f.reserved != want {
				t.Errorf("reserved = %d after %v, want %d", h.f.reserved, err, want)
			}
		})
	}
}

func TestNewFetcher_Workers(t *testing.T) {
	d := dbtest.SQLite(t)
	st, _, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "store"), fetchT0)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	full := FetcherConfig{
		Repo: fleet.NewRepository(d), Store: st, Catalog: catalogtest.New(t, "https://example.test").Catalog,
		Client: NewHTTPClient(testUserAgent, nil), SpoolDir: filepath.Join(t.TempDir(), "spool"),
	}
	tests := []struct {
		name    string
		workers int
		want    int // the worker count Run starts, when NewFetcher accepts it
		wantErr bool
	}{
		{"zero is the default", 0, 4, false},
		{"one", 1, 1, false},
		{"the maximum", 16, 16, false},
		{"one over the maximum", 17, 0, true},
		{"negative", -1, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := full
			cfg.Workers = tt.workers

			f, err := NewFetcher(cfg)

			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "1 to 16") {
					t.Fatalf("NewFetcher = %v, %v; want an error that names the range 1 to 16", f, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewFetcher: %v", err)
			}
			if f.cfg.Workers != tt.want {
				t.Errorf("workers = %d, want %d", f.cfg.Workers, tt.want)
			}
		})
	}
	if DefaultFetchWorkers != 4 || MaxFetchWorkers != 16 {
		t.Errorf("DefaultFetchWorkers, MaxFetchWorkers = %d, %d; want 4, 16", DefaultFetchWorkers, MaxFetchWorkers)
	}
}

func TestDiskFree(t *testing.T) {
	free, err := diskFree(t.TempDir())
	if err != nil || free == 0 {
		t.Errorf("diskFree = %d, %v; want some free space and no error", free, err)
	}
	if _, err := diskFree(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("diskFree of a missing path succeeded, want an error")
	}
}

// TestFetcher_FailureCauseIsPrintable fails the transfer with an error whose
// text holds control, format and invalid UTF-8 content. The stored last_error
// and the emitted event carry printable text only.
func TestFetcher_FailureCauseIsPrintable(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("reset\u009b by\u202e peer\u200b\x00\xff")
	})}
	h := newHarness(t, catalogtest.New(t, "https://example.test").Catalog, client)
	h.request("tlive", "amd64", 0)

	h.fetchOnce()

	row := h.wantRow(tliveAMD64(), fleet.CacheFailed, 1)
	ev := h.lastEvent()
	if ev.Err == nil {
		t.Fatal("event Err = nil, want the failure")
	}
	for name, text := range map[string]string{"last_error": row.LastError, "event Err": ev.Err.Error()} {
		if !strings.Contains(text, "reset by peer") {
			t.Errorf("%s = %q, want it to keep the printable text", name, text)
		}
		for _, r := range text {
			if !unicode.IsGraphic(r) && r != ' ' {
				t.Errorf("%s = %q holds the non-printable rune %U", name, text, r)
				break
			}
		}
	}
}

// simClock is a clock the tests move by hand; it is safe for concurrent use.
type simClock struct{ elapsed atomic.Int64 }

func (c *simClock) Now() time.Time          { return fetchT0().Add(time.Duration(c.elapsed.Load())) }
func (c *simClock) advance(d time.Duration) { c.elapsed.Add(int64(d)) }

// linkTransport simulates a link of rate bytes per second: every Read of a
// response body moves the clock on by the time its bytes would take, whatever
// the real speed of the loopback.
type linkTransport struct {
	next http.RoundTripper
	clk  *simClock
	rate int64
}

func (l linkTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := l.next.RoundTrip(req)
	if err == nil {
		resp.Body = linkBody{ReadCloser: resp.Body, clk: l.clk, rate: l.rate}
	}
	return resp, err
}

type linkBody struct {
	io.ReadCloser

	clk  *simClock
	rate int64
}

func (b linkBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.clk.advance(time.Duration(n) * time.Second / time.Duration(b.rate))
	return n, err
}

// simulateLink makes f's downloads run at rate bytes per simulated second, on a
// clock that moves only with the bytes read. It replaces f's client's
// Transport, so a scenario's client must not be shared with another scenario.
func simulateLink(f *Fetcher, rate int64) {
	clk := &simClock{}
	f.now = clk.Now
	f.cfg.Client.Transport = linkTransport{next: f.cfg.Client.Transport, clk: clk, rate: rate}
}

// floorRun is a fetch of one raw object of size bytes over a link of rate
// bytes per simulated second, from a spool that holds the object's first
// spooled bytes.
func floorRun(t *testing.T, rate int64, size, spooled int) *run {
	t.Helper()
	key := fleet.CacheKey{Entry: "tbig", Arch: fleet.ArchAMD64}
	return runScenario(t, func(t *testing.T, base string) scenario {
		t.Helper()
		object := make([]byte, size)
		for i := range object {
			object[i] = byte(i % 251)
		}
		spec := catalog.Spec{
			Entry: "tbig", Arch: "amd64", Version: "1", Recipe: catalog.RecipeChain, Platforms: []catalog.Platform{catalog.PlatformEFI},
			Objects: []catalog.ObjectSpec{{URL: base + "/big.bin", Container: catalog.ContainerRaw, Signature: catalog.Signature{TOFU: true}, Files: []string{"live/vmlinuz"}}},
			Boot:    catalog.Boot{Kernel: "live/vmlinuz"},
		}
		sc := scenario{
			cat: singleCatalog(t, spec, pinOf(spec, object, map[string][]byte{"live/vmlinuz": object})), key: key, object: object,
			handler: serveObject(object),
			tune:    func(f *Fetcher) { simulateLink(f, rate) },
		}
		if spooled > 0 {
			sc.seed = func(t *testing.T, h *harness, e catalog.Entry) {
				t.Helper()
				seedSpool(t, h, e, object[:spooled], testETag)
			}
		}
		return sc
	})
}

// wantCutOff fails unless the fetch ended in one failed attempt for being too slow.
func wantCutOff(r *run) {
	r.t.Helper()
	row := r.wantRow(r.sc.key, fleet.CacheFailed, 1)
	if ev := r.lastEvent(); ev.Class != classOther || ev.Err == nil || !strings.Contains(ev.Err.Error(), "32 KiB/s") {
		r.t.Errorf("last event = %+v, want class %q and an error naming 32 KiB/s", ev, classOther)
	}
	if !strings.Contains(row.LastError, "32 KiB/s") {
		r.t.Errorf("LastError = %q, want it to name 32 KiB/s", row.LastError)
	}
}

const floorObject = 8 << 20 // at 64 KiB/s, 128 s: longer than the window

func TestFetcher_FloorCutsTrickle(t *testing.T) {
	r := floorRun(t, 1<<10, floorObject, 0)

	r.fetchOnce()

	wantCutOff(r)
}

func TestFetcher_FloorAllowsSteady(t *testing.T) {
	r := floorRun(t, 64<<10, floorObject, 0)

	r.fetchOnce()

	r.wantRow(r.sc.key, fleet.CacheReady, 0)
}

// TestFetcher_FloorMeasuresThisTransfer: the spool's floorObject bytes were not
// received by this transfer, so a fast resume passes, and a slow one is cut off
// even though its bytes on disk, averaged over its whole run, would clear the
// floor.
func TestFetcher_FloorMeasuresThisTransfer(t *testing.T) {
	tests := []struct {
		name string
		rate int64
		size int
		cut  bool
	}{
		{name: "fast resume", rate: 64 << 10, size: 2 * floorObject},
		{name: "slow resume", rate: 1 << 10, size: floorObject + 256<<10, cut: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := floorRun(t, tt.rate, tt.size, floorObject)

			r.fetchOnce()

			if seen := r.up.seen(); len(seen) != 1 || seen[0].Range != fmt.Sprintf("bytes=%d-", floorObject) {
				t.Fatalf("requests = %v, want one for the second half", seen)
			}
			if tt.cut {
				wantCutOff(r)
				return
			}
			r.wantRow(r.sc.key, fleet.CacheReady, 0)
		})
	}
}

// TestFetcher_FloorOnTheLastRead: the floor judges a Read after it returned its
// bytes, so it can cut the very Read that delivers the object's last byte. The
// spool is then whole, and the transfer succeeded; a cut that leaves bytes
// undelivered is still the failure it was.
func TestFetcher_FloorOnTheLastRead(t *testing.T) {
	tests := []struct {
		name    string
		size    int // the pinned size; the body holds 1024 bytes, the second half with the cut
		wantErr error
	}{
		{"the cut Read delivers the last byte", 1024, nil},
		{"the cut Read leaves bytes undelivered", 2048, errTooSlow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := floorRun(t, 64<<10, tt.size, 0)
			root, err := os.OpenRoot(r.spool)
			if err != nil {
				t.Fatalf("OpenRoot: %v", err)
			}
			j := &fetchJob{f: r.f, row: r.row(r.sc.key), entry: r.e, root: root}
			defer j.close(t.Context())
			var clk simClock
			reads := []time.Duration{time.Minute, ThroughputWindow} // each delivers 512 bytes
			body := readerFunc(func(p []byte) (int, error) {
				if len(reads) == 0 {
					return 0, io.EOF
				}
				clk.advance(reads[0] - time.Duration(clk.elapsed.Load()))
				reads = reads[1:]
				return 512, nil
			})

			err = j.write(t.Context(), newFloorReader(body, clk.Now), 0, 0)

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("write = %v, want %v", err, tt.wantErr)
			}
			if got := r.spoolSize(SpoolName(r.e.Pin.ID, 0)); got != 1024 {
				t.Errorf("spool size = %d, want 1024: the bytes of the cut Read are written", got)
			}
		})
	}
}

func TestFloorReader(t *testing.T) {
	const perSecond = MinThroughput // bytes in one second at exactly the floor
	type step struct {
		at      time.Duration // since the transfer started
		n       int           // bytes the read returns
		srcErr  error
		wantErr error
	}
	steady := func(per int, until int) []step {
		var steps []step
		for s := 1; s <= until; s++ {
			steps = append(steps, step{at: time.Duration(s) * time.Second, n: per})
		}
		return steps
	}
	// halves is a Read every half second at exactly the floor, from the first
	// to past the first judged second, so one second holds two Reads.
	halves := func() []step {
		var steps []step
		for at := 500 * time.Millisecond; at <= ThroughputWindow+5*time.Second; at += 500 * time.Millisecond {
			steps = append(steps, step{at: at, n: perSecond / 2})
		}
		return steps
	}
	// pulsed is cycles rounds of a one byte Read, then half a second later a
	// burst of burst bytes, one gap apart (under the idle timeout), and then a
	// one byte Read that must be cut off: the burst every 59 s averages
	// 21.8 KiB/s, but a baseline that lags the window's start counts a burst
	// from before the window, and the upstream is never cut.
	pulsed := func(cycles int, gap time.Duration, burst int) []step {
		var steps []step
		for k := range cycles {
			at := time.Duration(k) * gap
			steps = append(steps, step{at: at, n: 1}, step{at: at + 500*time.Millisecond, n: burst})
		}
		return append(steps, step{at: time.Duration(cycles) * gap, n: 1, wantErr: errTooSlow})
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"nothing is judged before the window has run", []step{{at: ThroughputWindow - time.Second, n: 1}}},
		{"slow when the window has run", []step{{at: time.Minute, n: 1}, {at: ThroughputWindow, n: 1, wantErr: errTooSlow}}},
		{"exactly the floor passes", steady(perSecond, 200)},
		{"a byte under the floor is cut off", append(steady(perSecond-1, int(ThroughputWindow/time.Second)-1), step{at: ThroughputWindow, n: perSecond - 1, wantErr: errTooSlow})},
		{"a burst older than the window does not count", []step{
			{at: time.Second, n: 4 << 20},
			{at: ThroughputWindow, n: 1}, // the burst is still inside the last window
			{at: ThroughputWindow + time.Second, n: 1, wantErr: errTooSlow},
		}},
		{"exactly the floor in half-second Reads passes", halves()},
		{"bursts spaced by the idle limit do not hide a slow average", pulsed(3, 59*time.Second, 1_320_000)},
		{"a read that ends the body is not judged", []step{{at: time.Minute, n: 1}, {at: 3 * ThroughputWindow, srcErr: io.EOF, wantErr: io.EOF}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var clk simClock
			var n int
			var srcErr error
			fr := newFloorReader(readerFunc(func([]byte) (int, error) { return n, srcErr }), clk.Now)
			buf := make([]byte, 4<<20)
			var last time.Duration
			for i, s := range tt.steps {
				clk.advance(s.at - last)
				last, n, srcErr = s.at, s.n, s.srcErr

				got, err := fr.Read(buf)

				if !errors.Is(err, s.wantErr) {
					t.Fatalf("step %d (at %s): Read = %d, %v; want error %v", i, s.at, got, err, s.wantErr)
				}
				if got != s.n {
					t.Fatalf("step %d: Read returned %d bytes, want %d", i, got, s.n)
				}
			}
		})
	}
}
