package toolcache

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"go.jacaudi.dev/hephaestus/internal/catalog/catalogtest"
	"go.jacaudi.dev/hephaestus/internal/factory"
	"go.jacaudi.dev/hephaestus/internal/factory/factorytest"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/store"
)

const (
	plainSchematicID = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"
	talosTestVersion = "v1.12.11"
	talosTestCmdline = "talos.platform=metal console=tty0 init_on_alloc=1"

	talosBytesCounter    = "hephaestus.talos.fetch.bytes"
	talosFailuresCounter = "hephaestus.talos.fetch.failures"
)

// talosFiles is what the fake factory serves for amd64: a kernel, an initramfs
// and cmdline, which the factory sends with its trailing newline.
func talosFiles(cmdline string) map[string][]byte {
	return map[string][]byte{
		factory.FileName(fleet.TalosKernel, fleet.ArchAMD64):    bytes.Repeat([]byte("kernel."), 40_000),
		factory.FileName(fleet.TalosInitramfs, fleet.ArchAMD64): bytes.Repeat([]byte("initramfs."), 30_000),
		factory.FileName(fleet.TalosCmdline, fleet.ArchAMD64):   []byte(cmdline),
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func assetOf(b []byte) fleet.TalosAsset {
	return fleet.TalosAsset{Size: int64(len(b)), SHA256: sha256Hex(b)}
}

// talosSetup shapes a talosRig. Every field is optional.
type talosSetup struct {
	// body is the schematic body of the row; the row's schematic is its hash.
	body []byte
	// schematic is the row's schematic when there is no body: the plain ID by default.
	schematic string
	// registerID is what the fake answers a registration with: the hash of the body by default.
	registerID string
	// cmdline is the cmdline file the fake serves: talosTestCmdline and a newline by default.
	cmdline string
	// urlTTL is how long the fake's asset URLs live: an hour by default.
	urlTTL time.Duration
	// cut is the file whose first full answer is cut off after half its bytes.
	cut fleet.TalosPart
}

// talosRig is a harness with the Talos source over a fake factory behind TLS
// 1.3, and one wanted Talos row at key. The fake serves the plain schematic or
// the hash of the body.
type talosRig struct {
	*harness

	fake  *factorytest.Fake
	key   fleet.TalosKey
	files map[string][]byte

	mu      sync.Mutex
	seenReq []seenRequest // Path is "<method> <path>"
	fakeNow time.Time
	cutDone bool
	// onRequest, when set, runs with each request's "<method> <path>" before it is served.
	onRequest func(path string)
}

func newTalosRig(t *testing.T, s talosSetup) *talosRig {
	t.Helper()
	key := fleet.TalosKey{Schematic: cmp.Or(s.schematic, plainSchematicID), Version: talosTestVersion, Arch: fleet.ArchAMD64}
	served := plainSchematicID
	if s.body != nil {
		key.Schematic = sha256Hex(s.body)
		served = key.Schematic
	}
	if s.cmdline == "" {
		s.cmdline = talosTestCmdline + "\n"
	}
	r := &talosRig{key: key, files: talosFiles(s.cmdline), fakeNow: fetchT0()}
	r.fake = factorytest.New(factorytest.Options{
		Schematic: served, Version: talosTestVersion, Assets: r.files, RegisterID: s.registerID, URLTTL: s.urlTTL,
		Now: func() time.Time {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.fakeNow
		},
	})
	var cut []byte
	if s.cut != "" {
		cut = r.files[factory.FileName(s.cut, fleet.ArchAMD64)]
	}
	srv := startTLS(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.seenReq = append(r.seenReq, seenRequest{Path: req.Method + " " + req.URL.Path, Range: req.Header.Get("Range"), IfRange: req.Header.Get("If-Range")})
		hook := r.onRequest
		r.mu.Unlock()
		if hook != nil {
			hook(req.Method + " " + req.URL.Path)
		}
		r.serve(t, w, req, cut)
	}), false)
	client := NewHTTPClient(testUserAgent, poolOf(srv))
	t.Cleanup(client.CloseIdleConnections)
	r.harness = newHarnessFor(t, FetcherConfig{
		Catalog: catalogtest.New(t, "https://example.test").Catalog, Client: client, Talos: factory.New(srv.URL, client),
	})
	if _, err := fleet.WantTalosTx(t.Context(), r.d.Bun(), fleet.TalosWant{Key: key, Body: s.body}, fetchT0()); err != nil {
		t.Fatalf("WantTalosTx: %v", err)
	}
	return r
}

// serve passes req to the fake, except the first unranged answer that carries
// cut: that one is sent with its status, size and ETag but only half its body,
// and the connection is closed.
func (r *talosRig) serve(t *testing.T, w http.ResponseWriter, req *http.Request, cut []byte) {
	t.Helper()
	r.mu.Lock()
	cutting := cut != nil && !r.cutDone && req.Header.Get("Range") == ""
	r.mu.Unlock()
	if !cutting {
		r.fake.ServeHTTP(w, req)
		return
	}
	rec := httptest.NewRecorder()
	r.fake.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), cut) {
		maps.Copy(w.Header(), rec.Header())
		w.WriteHeader(rec.Code)
		send(t, w, rec.Body.Bytes())
		return
	}
	r.mu.Lock()
	r.cutDone = true
	r.mu.Unlock()
	hijackSend(t, w, fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\nETag: %s\r\n\r\n%s", len(cut), rec.Header().Get("ETag"), cut[:len(cut)/2]))
}

func (r *talosRig) seen() []seenRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seenReq)
}

// setOnRequest sets the hook run before each request is served.
func (r *talosRig) setOnRequest(fn func(path string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onRequest = fn
}

func (r *talosRig) setFakeNow(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fakeNow = at
}

func (r *talosRig) imagePath(p fleet.TalosPart) string {
	return "GET /image/" + r.key.Schematic + "/" + talosTestVersion + "/" + factory.FileName(p, fleet.ArchAMD64)
}

func (r *talosRig) file(p fleet.TalosPart) []byte {
	return r.files[factory.FileName(p, fleet.ArchAMD64)]
}

// talosRow returns the rig's row.
func (r *talosRig) talosRow() fleet.TalosRow {
	r.t.Helper()
	row, ok, err := r.repo.Talos(r.t.Context(), r.key)
	if err != nil || !ok {
		r.t.Fatalf("Talos(%v) = %v, %v", r.key, ok, err)
	}
	return row
}

// wantTalosRow fails unless the rig's row is in state after attempts failed attempts.
func (r *talosRig) wantTalosRow(state fleet.CacheState, attempts int) fleet.TalosRow {
	r.t.Helper()
	row := r.talosRow()
	if row.State != state || row.Attempts != attempts {
		r.t.Fatalf("row %s = %s, %d attempts (%q); want %s, %d", r.key.Tag(), row.State, row.Attempts, row.LastError, state, attempts)
	}
	return row
}

// rearm moves the rig's failed row back to wanted, so the fixed clock can claim it again.
func (r *talosRig) rearm() {
	r.t.Helper()
	if moved, err := r.repo.RearmTalos(r.t.Context(), r.key, fetchT0()); err != nil || !moved {
		r.t.Fatalf("RearmTalos = %v, %v; want true", moved, err)
	}
}

// requeue moves the rig's ready row back to wanted and, when untag is set,
// removes its tag, so the next fetch cannot take the store shortcut.
func (r *talosRig) requeue(untag bool) {
	r.t.Helper()
	if moved, err := r.repo.RequeueTalos(r.t.Context(), r.key, fetchT0()); err != nil || !moved {
		r.t.Fatalf("RequeueTalos = %v, %v; want true", moved, err)
	}
	if untag {
		if err := r.store.Untag(r.t.Context(), r.key.Tag()); err != nil {
			r.t.Fatalf("Untag: %v", err)
		}
	}
}

// wantLayers fails unless the rig's tag holds the three layers, titled by
// part, whose bytes are want's.
func (r *talosRig) wantLayers(want map[fleet.TalosPart][]byte) {
	r.t.Helper()
	ctx := r.t.Context()
	if err := r.store.CheckLayers(ctx, r.key.Tag()); err != nil {
		r.t.Fatalf("CheckLayers(%s): %v", r.key.Tag(), err)
	}
	layers, err := r.store.Layers(ctx, r.key.Tag())
	if err != nil {
		r.t.Fatalf("Layers(%s): %v", r.key.Tag(), err)
	}
	var titles []string
	for _, l := range layers {
		title := l.Annotations[ocispec.AnnotationTitle]
		titles = append(titles, title)
		rc, err := r.store.OpenBlob(l)
		if err != nil {
			r.t.Fatalf("OpenBlob(%s): %v", title, err)
		}
		got, err := io.ReadAll(rc)
		if cerr := rc.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			r.t.Fatalf("read layer %s: %v", title, err)
		}
		if !bytes.Equal(got, want[fleet.TalosPart(title)]) {
			r.t.Errorf("layer %s holds %d bytes that are not the %d wanted", title, len(got), len(want[fleet.TalosPart(title)]))
		}
		if l.Digest.Encoded() != sha256Hex(got) {
			r.t.Errorf("layer %s digest %s is not its bytes' sha256", title, l.Digest)
		}
	}
	if wantTitles := []string{"kernel", "initramfs", "cmdline"}; !slices.Equal(titles, wantTitles) {
		r.t.Errorf("layer titles = %q, want %q", titles, wantTitles)
	}
}

func TestTalosSource_FirstFetchRecordsAssets(t *testing.T) {
	body := []byte("customization:\n  extraKernelArgs: [console=tty0]\n")
	r := newTalosRig(t, talosSetup{body: body})
	r.fetchOnce()

	row := r.wantTalosRow(fleet.CacheReady, 0)
	trimmed := []byte(talosTestCmdline)
	want := fleet.TalosAssets{
		fleet.TalosKernel:    assetOf(r.file(fleet.TalosKernel)),
		fleet.TalosInitramfs: assetOf(r.file(fleet.TalosInitramfs)),
		fleet.TalosCmdline:   assetOf(trimmed),
	}
	if !maps.Equal(row.Assets, want) {
		t.Errorf("Assets = %v, want %v", row.Assets, want)
	}
	r.wantLayers(map[fleet.TalosPart][]byte{
		fleet.TalosKernel: r.file(fleet.TalosKernel), fleet.TalosInitramfs: r.file(fleet.TalosInitramfs), fleet.TalosCmdline: trimmed,
	})

	seen := r.seen()
	if len(seen) == 0 || seen[0].Path != "POST /schematics" {
		t.Errorf("first request = %v, want the registration first", seen)
	}
	for _, p := range fleet.TalosParts() {
		if !slices.ContainsFunc(seen, func(s seenRequest) bool { return s.Path == r.imagePath(p) }) {
			t.Errorf("requests %v never asked the factory for %s", seen, p)
		}
	}
	ev := r.lastEvent()
	if ev.Source != SourceTalos || ev.Talos != r.key || ev.Key != (fleet.CacheKey{}) || ev.From != fleet.CacheFetching || ev.To != fleet.CacheReady {
		t.Errorf("last event = %+v, want a Talos fetching > ready of %v", ev, r.key)
	}
	total := int64(len(r.file(fleet.TalosKernel)) + len(r.file(fleet.TalosInitramfs)) + len(r.file(fleet.TalosCmdline)))
	if got := r.counter(talosBytesCounter, ""); got != total || ev.Bytes != total {
		t.Errorf("%s = %d, event Bytes = %d; want %d", talosBytesCounter, got, ev.Bytes, total)
	}
	if got := r.counter(bytesCounter, ""); got != 0 {
		t.Errorf("%s = %d, want 0 for a Talos fetch", bytesCounter, got)
	}
	r.wantSpool()
}

func TestTalosSource_FirstFetchSizedFromContentRange(t *testing.T) {
	r := newTalosRig(t, talosSetup{cut: fleet.TalosInitramfs})
	r.fetchOnce()
	row := r.wantTalosRow(fleet.CacheFailed, 1)
	if !strings.Contains(row.LastError, "initramfs") || !strings.Contains(row.LastError, "object 1") {
		t.Errorf("LastError = %q, want it to name the initramfs, object 1", row.LastError)
	}

	r.rearm()
	r.fetchOnce()
	row = r.wantTalosRow(fleet.CacheReady, 0)
	initramfs := r.file(fleet.TalosInitramfs)
	if got, want := row.Assets[fleet.TalosInitramfs], assetOf(initramfs); got != want {
		t.Errorf("initramfs asset = %+v, want %+v", got, want)
	}
	half := len(initramfs) / 2
	resumed := slices.ContainsFunc(r.seen(), func(s seenRequest) bool {
		return strings.HasPrefix(s.Path, "GET /assets/") && s.Range == fmt.Sprintf("bytes=%d-", half)
	})
	if !resumed {
		t.Errorf("requests %v never resumed the initramfs at byte %d", r.seen(), half)
	}
}

func TestTalosSource_ExpiredURLRestartsAtFactory(t *testing.T) {
	r := newTalosRig(t, talosSetup{cut: fleet.TalosKernel, urlTTL: time.Minute})
	r.fetchOnce()
	r.wantTalosRow(fleet.CacheFailed, 1)
	before := len(r.seen())

	// Every asset URL the first attempt was sent to has expired.
	r.setFakeNow(fetchT0().Add(time.Hour))
	r.rearm()
	r.fetchOnce()
	r.wantTalosRow(fleet.CacheReady, 0)
	retry := r.seen()[before:]
	if len(retry) < 2 || retry[0].Path != r.imagePath(fleet.TalosKernel) || retry[1].Range == "" {
		t.Errorf("retry requests = %v; want the kernel asked of the factory again, then resumed", retry)
	}
}

func TestTalosSource_RecordedHashMismatchIsIntegrity(t *testing.T) {
	r := newTalosRig(t, talosSetup{})
	r.fetchOnce()
	r.wantTalosRow(fleet.CacheReady, 0)

	r.requeue(true)
	files := maps.Clone(r.files)
	files[factory.FileName(fleet.TalosKernel, fleet.ArchAMD64)] = bytes.Repeat([]byte("KERNEL."), 40_000)
	r.fake.SetAssets(files)
	r.fetchOnce()

	row := r.wantTalosRow(fleet.CacheBroken, 1)
	if !strings.Contains(row.LastError, "kernel") || !strings.Contains(row.LastError, "sha256") {
		t.Errorf("LastError = %q, want a kernel sha256 mismatch", row.LastError)
	}
	if got, want := row.Assets[fleet.TalosKernel], assetOf(r.file(fleet.TalosKernel)); got != want {
		t.Errorf("kernel asset = %+v, want the first fetch's %+v kept", got, want)
	}
	if ev := r.lastEvent(); ev.Class != classIntegrity || ev.To != fleet.CacheBroken {
		t.Errorf("last event = %+v, want an integrity failure to broken", ev)
	}
	if got := r.counter(talosFailuresCounter, classIntegrity); got != 1 {
		t.Errorf("%s{integrity} = %d, want 1", talosFailuresCounter, got)
	}
	r.wantSpool()
}

func TestTalosSource_UnknownSchematicBroken(t *testing.T) {
	r := newTalosRig(t, talosSetup{schematic: strings.Repeat("0", 64)})
	r.fetchOnce()
	row := r.wantTalosRow(fleet.CacheBroken, 1)
	if !strings.Contains(row.LastError, "factory: ") || !strings.Contains(row.LastError, "unknown schematic "+r.key.Schematic) {
		t.Errorf("LastError = %q, want the factory's 404 body after \"factory: \"", row.LastError)
	}
}

func TestTalosSource_MalformedSchematicBroken(t *testing.T) {
	r := newTalosRig(t, talosSetup{schematic: "abcde"})
	r.fetchOnce()
	row := r.wantTalosRow(fleet.CacheBroken, 1)
	if !strings.Contains(row.LastError, "schematic") {
		t.Errorf("LastError = %q, want it to name the schematic", row.LastError)
	}
	if seen := r.seen(); len(seen) != 0 {
		t.Errorf("requests = %v, want none for a malformed schematic", seen)
	}
}

func TestTalosSource_RegisterIDMismatchBroken(t *testing.T) {
	r := newTalosRig(t, talosSetup{body: []byte("customization: {}\n"), registerID: strings.Repeat("e", 64)})
	r.fetchOnce()
	row := r.wantTalosRow(fleet.CacheBroken, 1)
	if !strings.Contains(row.LastError, strings.Repeat("e", 64)) {
		t.Errorf("LastError = %q, want it to name the id the factory answered", row.LastError)
	}
	if seen := r.seen(); len(seen) != 1 || seen[0].Path != "POST /schematics" {
		t.Errorf("requests = %v, want the registration alone", seen)
	}
}

func TestTalosSource_RegisterOnlyWithBody(t *testing.T) {
	r := newTalosRig(t, talosSetup{})
	r.fetchOnce()
	r.wantTalosRow(fleet.CacheReady, 0)
	for _, s := range r.seen() {
		if s.Path == "POST /schematics" {
			t.Errorf("requests = %v, want no registration of a literal schematic ID", r.seen())
		}
	}
}

func TestTalosSource_CmdlineRejected(t *testing.T) {
	tests := []struct {
		name    string
		cmdline string
	}{
		{"comment token", "console=tty0 #x"},
		{"semicolon", "a;b"},
		{"and", "a && b"},
		{"or", "a || b"},
		{"dollar", "a$b"},
		{"newline inside", "a\nb"},
		{"two trailing newlines", "a\n\n"},
		{"2047 bytes", strings.Repeat("a", 2047)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTalosRig(t, talosSetup{cmdline: tt.cmdline})
			r.fetchOnce()
			row := r.wantTalosRow(fleet.CacheBroken, 1)
			if !strings.Contains(row.LastError, "cmdline") {
				t.Errorf("LastError = %q, want it to name the cmdline", row.LastError)
			}
			if _, err := r.store.Resolve(t.Context(), r.key.Tag()); err == nil {
				t.Errorf("%s is tagged for a refused cmdline", r.key.Tag())
			}
		})
	}
}

func TestTalosSource_CmdlineTrailingNewlineTrimmed(t *testing.T) {
	r := newTalosRig(t, talosSetup{cmdline: "console=tty0\n"})
	r.fetchOnce()
	row := r.wantTalosRow(fleet.CacheReady, 0)
	want := fleet.TalosAsset{Size: 12, SHA256: sha256Hex([]byte("console=tty0"))}
	if got := row.Assets[fleet.TalosCmdline]; got != want {
		t.Errorf("cmdline asset = %+v, want %+v", got, want)
	}
	r.wantLayers(map[fleet.TalosPart][]byte{
		fleet.TalosKernel: r.file(fleet.TalosKernel), fleet.TalosInitramfs: r.file(fleet.TalosInitramfs), fleet.TalosCmdline: []byte("console=tty0"),
	})

	// The second fetch downloads the same bytes, newline and all, and finds
	// them equal to what the first recorded.
	r.requeue(true)
	r.fetchOnce()
	if row := r.wantTalosRow(fleet.CacheReady, 0); row.Assets[fleet.TalosCmdline] != want {
		t.Errorf("cmdline asset after the second fetch = %+v, want %+v", row.Assets[fleet.TalosCmdline], want)
	}
}

func TestTalosSpoolName(t *testing.T) {
	tests := []struct {
		name string
		key  fleet.TalosKey
		n    int
		want string
	}{
		{"plain id", fleet.TalosKey{Schematic: plainSchematicID, Version: talosTestVersion, Arch: fleet.ArchAMD64}, 0, "talos-376567988ad3-v1.12.11-amd64-0"},
		{"short schematic", fleet.TalosKey{Schematic: "abcde", Version: talosTestVersion, Arch: fleet.ArchAMD64}, 0, "talos-abcde-v1.12.11-amd64-0"},
		{"cmdline on arm64", fleet.TalosKey{Schematic: plainSchematicID, Version: talosTestVersion, Arch: fleet.ArchARM64}, 2, "talos-376567988ad3-v1.12.11-arm64-2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TalosSpoolName(tt.key, tt.n); got != tt.want {
				t.Errorf("TalosSpoolName(%v, %d) = %q, want %q", tt.key, tt.n, got, tt.want)
			}
		})
	}
}

func TestTalosSource_ShortcutNeedsRecordedHashes(t *testing.T) {
	r := newTalosRig(t, talosSetup{})
	r.fetchOnce()
	ready := r.wantTalosRow(fleet.CacheReady, 0)

	// The tag stays; the hashes go. The fetch downloads and records them again.
	if moved, err := r.repo.ResetTalosHash(t.Context(), r.key, fleet.ActorCLI, fetchT0()); err != nil || !moved {
		t.Fatalf("ResetTalosHash = %v, %v; want true", moved, err)
	}
	before := len(r.seen())
	r.fetchOnce()
	if row := r.wantTalosRow(fleet.CacheReady, 0); !maps.Equal(row.Assets, ready.Assets) {
		t.Errorf("Assets after the reset = %v, want %v recorded again", row.Assets, ready.Assets)
	}
	if got := len(r.seen()) - before; got < 3 {
		t.Errorf("the fetch after the reset sent %d requests; want the three parts downloaded", got)
	}

	// With the hashes recorded and the tag matching them, nothing is downloaded.
	r.requeue(false)
	before = len(r.seen())
	r.fetchOnce()
	r.wantTalosRow(fleet.CacheReady, 0)
	if got := r.seen()[before:]; len(got) != 0 {
		t.Errorf("the shortcut sent %v; want no request", got)
	}
}

func TestFetcher_ClaimOrderAcrossSources(t *testing.T) {
	r := newTalosRig(t, talosSetup{})
	ctx := t.Context()
	t0 := fetchT0()
	// Within each table the oldest row comes first, so the order below is the
	// classes' and not the tables'.
	urgent, rest := tliveAMD64(), tshellAMD64()
	if _, err := r.repo.RequestFetch(ctx, rest, "p-rest", 0, t0.Add(-2*time.Hour)); err != nil {
		t.Fatalf("RequestFetch(rest): %v", err)
	}
	if _, err := r.repo.RequestFetch(ctx, urgent, "p-urgent", 1, t0); err != nil {
		t.Fatalf("RequestFetch(urgent): %v", err)
	}
	other := fleet.TalosKey{Schematic: plainSchematicID, Version: "v1.12.10", Arch: fleet.ArchAMD64}
	if _, err := fleet.WantTalosTx(ctx, r.d.Bun(), fleet.TalosWant{Key: other}, t0.Add(-3*time.Hour)); err != nil {
		t.Fatalf("WantTalosTx(other): %v", err)
	}
	assigned := fleet.Machine{
		ID: "m-1", UUID: fixtureUUID, Serial: "s", Product: "p", Arch: fleet.ArchAMD64, State: fleet.StateAssigned,
		CreatedAt: t0, LastSeenAt: t0, TalosSchematic: new(r.key.Schematic), TalosVersion: new(r.key.Version),
	}
	if _, err := r.d.Bun().NewInsert().Model(&assigned).Exec(ctx); err != nil {
		t.Fatalf("insert the assigned machine: %v", err)
	}

	want := []string{"utility tlive/amd64", "talos " + r.key.Tag(), "utility tshell/amd64", "talos " + other.Tag()}
	var got []string
	for range want {
		c, ok, err := r.w.claim(ctx)
		if err != nil || !ok {
			t.Fatalf("claim after %q = %v, %v; want a row", got, ok, err)
		}
		c.run.close(ctx)
		switch c.event.Source {
		case SourceUtility:
			got = append(got, c.event.Source+" "+c.event.Key.Entry+"/"+string(c.event.Key.Arch))
		default:
			got = append(got, c.event.Source+" "+c.event.Talos.Tag())
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("claim order = %q, want %q", got, want)
	}
	if c, ok, err := r.w.claim(ctx); err != nil || ok {
		t.Errorf("claim with every row fetching = %+v, %v, %v; want none", c.event, ok, err)
	}
}

func FuzzValidCmdline(f *testing.F) {
	for _, seed := range []string{
		"", talosTestCmdline, "console=tty0", "console=tty0 #x", "#x", "a;b", "a && b", "a || b", "a$b", "a\nb", "a\n",
		"a\tb", "a&b", "a|b", "a#b", strings.Repeat("a", 2046), strings.Repeat("a", 2047), "caf\xc3\xa9",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if validCmdline(s) != nil {
			return
		}
		if len(s) > 2046 {
			t.Errorf("validCmdline accepted %d bytes", len(s))
		}
		for i := range len(s) {
			if s[i] < 0x20 || s[i] > 0x7e {
				t.Errorf("validCmdline accepted byte %#x at %d", s[i], i)
			}
		}
		if strings.ContainsAny(s, "$;") || strings.Contains(s, "&&") || strings.Contains(s, "||") {
			t.Errorf("validCmdline accepted %q", s)
		}
		for tok := range strings.FieldsSeq(s) {
			if strings.HasPrefix(tok, "#") {
				t.Errorf("validCmdline accepted the comment token %q in %q", tok, s)
			}
		}
	})
}

func TestTalosSource_ResetDuringFetchNotRepinned(t *testing.T) {
	r := newTalosRig(t, talosSetup{})
	r.fetchOnce()
	r.wantTalosRow(fleet.CacheReady, 0)
	r.requeue(true)

	// While the fetch downloads, the admin resets the hashes and a second worker
	// claims the row again. The first worker's writes must all miss: its ready
	// would put back the hashes the admin just cleared.
	var once sync.Once
	r.setOnRequest(func(path string) {
		if path != r.imagePath(fleet.TalosCmdline) {
			return
		}
		once.Do(func() {
			if moved, err := r.repo.ResetTalosHash(t.Context(), r.key, fleet.ActorCLI, fetchT0()); err != nil || !moved {
				t.Errorf("ResetTalosHash during the fetch = %v, %v; want true, nil", moved, err)
			}
			if _, ok, err := r.repo.ClaimTalos(t.Context(), fleet.TalosOther, fetchT0()); err != nil || !ok {
				t.Errorf("the second worker's ClaimTalos = %v, %v; want the reset row", ok, err)
			}
		})
	})
	r.fetchOnce()

	row := r.wantTalosRow(fleet.CacheFetching, 0)
	if len(row.Assets) != 0 || row.BytesDone != 0 || row.BytesTotal != 0 {
		t.Errorf("row after the first worker settled = assets %v, progress %d/%d; want the reset's: none, 0/0", row.Assets, row.BytesDone, row.BytesTotal)
	}
}

func TestTalosSource_PushReservesSpace(t *testing.T) {
	r := newTalosRig(t, talosSetup{})
	var free atomic.Uint64
	free.Store(1 << 40)
	r.f.statfs = func(string) (uint64, error) { return spaceReserve + free.Load(), nil }
	// Once the kernel and initramfs are in the spool, the volume has room beyond
	// its margin for the largest part, but not for the store copies of all three.
	r.setOnRequest(func(path string) {
		if path == r.imagePath(fleet.TalosCmdline) {
			free.Store(uint64(len(r.file(fleet.TalosInitramfs))) + 1000)
		}
	})
	r.fetchOnce()

	row := r.wantTalosRow(fleet.CacheFailed, 1)
	if !strings.Contains(row.LastError, "free space") {
		t.Errorf("LastError = %q, want the free space refusal", row.LastError)
	}
	for _, p := range []fleet.TalosPart{fleet.TalosKernel, fleet.TalosInitramfs} {
		a := assetOf(r.file(p))
		desc, err := store.Layer(string(p), a.Size, a.SHA256)
		if err != nil {
			t.Fatalf("Layer(%s): %v", p, err)
		}
		if rc, err := r.store.OpenBlob(desc); !errors.Is(err, store.ErrLayerMissing) {
			if err == nil {
				if cerr := rc.Close(); cerr != nil {
					t.Errorf("close blob %s: %v", p, cerr)
				}
			}
			t.Errorf("OpenBlob(%s) = %v; want ErrLayerMissing, nothing pushed", p, err)
		}
	}
	if _, err := r.store.Resolve(t.Context(), r.key.Tag()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Resolve(%s) = %v; want ErrNotFound", r.key.Tag(), err)
	}
}
