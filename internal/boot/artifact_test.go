package boot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/catalog/catalogtest"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/store"
)

const (
	liveEntry = "tlive"
	liveArch  = "amd64"
	// liveFile is the fixture live entry's second stage, 4 MiB.
	liveFile = "live/filesystem.squashfs"
	// liveURL is where the fixture live entry's second stage is served.
	liveURL = "/u/tlive/amd64/1/" + liveFile
)

func liveCacheKey() fleet.CacheKey {
	return fleet.CacheKey{Entry: liveEntry, Arch: fleet.ArchAMD64}
}

// live is the fixture catalog's tlive entry for amd64.
func (g *rig) live() catalog.Entry {
	g.t.Helper()
	e, ok := g.fix.Catalog.Lookup(liveEntry, liveArch)
	if !ok {
		g.t.Fatalf("fixture catalog has no %s/%s", liveEntry, liveArch)
	}
	return e
}

// stockLive puts the fixture live entry's content in the store and makes its
// cache row ready at the pin, as a finished fetch would.
func (g *rig) stockLive() {
	g.t.Helper()
	catalogtest.Stock(g.t, g.st, g.fix, catalog.Key{Entry: liveEntry, Arch: liveArch})
	if _, err := g.repo.RequestFetch(g.t.Context(), liveCacheKey(), g.live().Pin.ID, 1, testNow()); err != nil {
		g.t.Fatalf("RequestFetch: %v", err)
	}
	moved, err := g.repo.RestoreReady(g.t.Context(), liveCacheKey(), g.live().Pin.ID, testNow())
	if err != nil || !moved {
		g.t.Fatalf("RestoreReady = %v, %v; want the wanted row moved to ready", moved, err)
	}
}

// liveBytes is the second stage's content.
func (g *rig) liveBytes() []byte {
	g.t.Helper()
	return g.fix.Files[catalog.Key{Entry: liveEntry, Arch: liveArch}][liveFile]
}

// liveBlob is the path of the second stage's blob in the store.
func (g *rig) liveBlob() string {
	g.t.Helper()
	f, ok := g.live().File(liveFile)
	if !ok {
		g.t.Fatalf("live entry has no file %s", liveFile)
	}
	return filepath.Join(g.storeRoot, "blobs", "sha256", f.SHA256)
}

// liveRow is the fixture live entry's cache row, which must exist.
func (g *rig) liveRow() fleet.CacheRow {
	g.t.Helper()
	row, ok, err := g.repo.CacheRow(g.t.Context(), liveCacheKey())
	if err != nil || !ok {
		g.t.Fatalf("CacheRow = %v, %v; want a row", ok, err)
	}
	return row
}

// plainWith serves a plain-HTTP GET of target with one request header.
func (g *rig) plainWith(target, key, value string) *httptest.ResponseRecorder {
	g.t.Helper()
	req := newRequest(g.t, target, false)
	req.Header.Set(key, value)
	rec := httptest.NewRecorder()
	g.h.PlainHTTP().ServeHTTP(rec, req)
	return rec
}

// wantRequeued fails unless the handlers reported exactly one
// ErrUtilityMissing, and the ready row went back to wanted.
func (g *rig) wantRequeued() {
	g.t.Helper()
	if row := g.liveRow(); row.State != fleet.CacheWanted {
		g.t.Errorf("cache row = %q, want wanted again", row.State)
	}
	if len(g.errs) != 1 || !errors.Is(g.errs[0], ErrUtilityMissing) {
		g.t.Errorf("reported errors = %v, want one ErrUtilityMissing", g.errs)
	}
}

func TestArtifact_ServesWithRange(t *testing.T) {
	g := newRig(t)
	g.stockLive()
	want := g.liveBytes()
	f, _ := g.live().File(liveFile)
	etag := `"sha256:` + f.SHA256 + `"`

	t.Run("range", func(t *testing.T) {
		rec := g.plainWith(liveURL, "Range", "bytes=0-9")

		wantStatus(t, rec, http.StatusPartialContent)
		if got := rec.Body.Bytes(); !bytes.Equal(got, want[0:10]) {
			t.Errorf("body = %x, want %x", got, want[0:10])
		}
		if got := rec.Header().Get("ETag"); got != etag {
			t.Errorf("ETag = %q, want %q", got, etag)
		}
		if got := rec.Header().Get("Content-Type"); got != contentTypeBinary {
			t.Errorf("Content-Type = %q, want %q", got, contentTypeBinary)
		}
	})
	t.Run("full", func(t *testing.T) {
		rec := g.plain(liveURL)

		wantStatus(t, rec, http.StatusOK)
		if got := rec.Body.Bytes(); !bytes.Equal(got, want) || len(got) != 4<<20 {
			t.Errorf("body is %d bytes, want the %d bytes of the file", len(got), len(want))
		}
		if got := rec.Header().Get("ETag"); got != etag {
			t.Errorf("ETag = %q, want %q", got, etag)
		}
	})
	t.Run("etag match", func(t *testing.T) {
		rec := g.plainWith(liveURL, "If-None-Match", etag)

		wantStatus(t, rec, http.StatusNotModified)
	})
	if len(g.errs) != 0 {
		t.Errorf("reported errors = %v, want none", g.errs)
	}
}

func TestArtifact_NotFound(t *testing.T) {
	g := newRig(t)
	g.stockLive()
	cases := []struct{ name, target string }{
		{"other version", "/u/tlive/amd64/2/" + liveFile},
		{"unknown entry", "/u/nope/amd64/1/" + liveFile},
		{"file not in the entry", "/u/tlive/amd64/1/live/other"},
		{"arch not stocked", "/u/tlive/arm64/1/" + liveFile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := g.plain(tc.target)

			wantStatus(t, rec, http.StatusNotFound)
		})
	}
	t.Run("not on TLS", func(t *testing.T) {
		rec := g.secure(liveURL)

		wantStatus(t, rec, http.StatusNotFound)
	})
	t.Run("outside the subnet", func(t *testing.T) {
		req := newRequest(t, liveURL, false)
		req.RemoteAddr = "10.10.21.9:1"
		rec := httptest.NewRecorder()

		g.h.PlainHTTP().ServeHTTP(rec, req)

		wantStatus(t, rec, http.StatusForbidden)
	})
	// A miss that is not lost content is not a failure to report.
	if len(g.errs) != 0 {
		t.Errorf("reported errors = %v, want none", g.errs)
	}
	if row := g.liveRow(); row.State != fleet.CacheReady {
		t.Errorf("cache row = %q, want it left ready", row.State)
	}
}

func TestArtifact_WrongSizedBlobRequeues(t *testing.T) {
	g := newRig(t)
	g.stockLive()
	blob := g.liveBlob()
	if err := os.Chmod(blob, 0o644); err != nil { // blobs are stored 0444
		t.Fatalf("chmod blob: %v", err)
	}
	if err := os.Truncate(blob, 10); err != nil {
		t.Fatalf("truncate blob: %v", err)
	}

	rec := g.plain(liveURL)

	wantStatus(t, rec, http.StatusNotFound)
	g.wantRequeued()
}

func TestArtifact_MissingBlobRequeues(t *testing.T) {
	g := newRig(t)
	g.stockLive()
	if err := os.Remove(g.liveBlob()); err != nil {
		t.Fatalf("remove blob: %v", err)
	}

	rec := g.plain(liveURL)

	wantStatus(t, rec, http.StatusNotFound)
	g.wantRequeued()
}

// stockLiveWithLayer is stockLive for a tag whose layer for file is the given
// bytes instead of the pinned ones: its blob is whole and its descriptor says
// what the blob is, so the store's own check passes.
func (g *rig) stockLiveWithLayer(file string, content []byte) {
	g.t.Helper()
	e := g.live()
	var layers []ocispec.Descriptor
	for _, fp := range e.Pin.Objects[0].Files {
		data := g.fix.Files[catalog.Key{Entry: liveEntry, Arch: liveArch}][fp.Path]
		if fp.Path == file {
			data = content
		}
		sum := sha256.Sum256(data)
		desc, err := store.Layer(fp.Path, int64(len(data)), hex.EncodeToString(sum[:]))
		if err != nil {
			g.t.Fatalf("Layer %s: %v", fp.Path, err)
		}
		if err := g.st.PushBlob(g.t.Context(), desc, bytes.NewReader(data)); err != nil {
			g.t.Fatalf("PushBlob %s: %v", fp.Path, err)
		}
		layers = append(layers, desc)
	}
	if _, err := g.st.TagArtifact(g.t.Context(), e.Tag(), catalog.ArtifactType, layers); err != nil {
		g.t.Fatalf("TagArtifact: %v", err)
	}
	if _, err := g.repo.RequestFetch(g.t.Context(), liveCacheKey(), e.Pin.ID, 1, testNow()); err != nil {
		g.t.Fatalf("RequestFetch: %v", err)
	}
	if moved, err := g.repo.RestoreReady(g.t.Context(), liveCacheKey(), e.Pin.ID, testNow()); err != nil || !moved {
		g.t.Fatalf("RestoreReady = %v, %v; want the wanted row moved to ready", moved, err)
	}
}

// TestArtifact_LayerNotThePinnedFileRequeues pins that a tag whose layer is not
// the file the catalog pins (another digest, or another size) is lost content,
// not something to serve: 404, and the ready row goes back to wanted.
func TestArtifact_LayerNotThePinnedFileRequeues(t *testing.T) {
	tests := []struct {
		name    string
		content func(pinned []byte) []byte
	}{
		{name: "another digest at the pinned size", content: func(pinned []byte) []byte {
			other := bytes.Clone(pinned)
			other[0] ^= 0xff
			return other
		}},
		{name: "another size", content: func(pinned []byte) []byte { return pinned[:len(pinned)-1] }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newRig(t)
			g.stockLiveWithLayer(liveFile, tt.content(g.liveBytes()))

			rec := g.plain(liveURL)

			wantStatus(t, rec, http.StatusNotFound)
			g.wantRequeued()
		})
	}
}

// TestArtifact_UntaggedReadyRowRequeues is a store that was set aside and
// started empty: the row says ready, the store has no such tag.
func TestArtifact_UntaggedReadyRowRequeues(t *testing.T) {
	g := newRig(t)
	g.stockLive()
	if err := g.st.Untag(t.Context(), g.live().Tag()); err != nil {
		t.Fatalf("Untag: %v", err)
	}

	rec := g.plain(liveURL)

	wantStatus(t, rec, http.StatusNotFound)
	g.wantRequeued()
}

// TestArtifact_UnreadableBlobIsAnErrorNotARequeue pins that a blob the store
// cannot read, as opposed to one it lacks, is a 500 and leaves the row alone.
func TestArtifact_UnreadableBlobIsAnErrorNotARequeue(t *testing.T) {
	g := newRig(t)
	g.stockLive()
	g.makeUnreadable(g.liveBlob())

	rec := g.plain(liveURL)

	wantStatus(t, rec, http.StatusInternalServerError)
	if row := g.liveRow(); row.State != fleet.CacheReady {
		t.Errorf("cache row = %q, want it left ready", row.State)
	}
	if len(g.errs) != 1 || errors.Is(g.errs[0], ErrUtilityMissing) {
		t.Errorf("reported errors = %v, want one that is not ErrUtilityMissing", g.errs)
	}
}

// makeUnreadable replaces the blob file at path with a symlink to itself, which
// every open and stat refuses with ELOOP: an error that is neither a missing
// file nor a wrong size, and that needs no privilege to make or to see.
func (g *rig) makeUnreadable(path string) {
	g.t.Helper()
	if err := os.Remove(path); err != nil {
		g.t.Fatalf("remove blob: %v", err)
	}
	if err := os.Symlink(filepath.Base(path), path); err != nil {
		g.t.Fatalf("symlink blob to itself: %v", err)
	}
}

// shortTimeout is how long the server of TestArtifact_IdleWriterOutlivesWriteTimeout
// allows reading a request and writing a response, and how long its client
// pauses mid-download.
const (
	shortTimeout = 200 * time.Millisecond
	clientPause  = 600 * time.Millisecond
	// firstRead is what the client reads before it pauses.
	firstRead = 64 << 10
	// socketBuffer is the kernel buffer of both ends of the connection. With the
	// default sizes a loopback socket may swallow the whole file, and a server
	// that is out of time would still finish writing.
	socketBuffer = 16 << 10
)

// startShortTimeoutServer serves h on loopback with ReadTimeout and
// WriteTimeout of shortTimeout and small server-side socket buffers, and closes
// it when the test ends.
func startShortTimeoutServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ReadTimeout = shortTimeout
	srv.Config.WriteTimeout = shortTimeout
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if tc, ok := c.(*net.TCPConn); ok && s == http.StateNew {
			if err := tc.SetWriteBuffer(socketBuffer); err != nil {
				t.Errorf("set server write buffer: %v", err)
			}
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// slowGet downloads url as a client that reads firstRead bytes, stops reading
// for clientPause, then reads the rest. It returns every byte it got and the
// error that ended the read, if any.
func slowGet(t *testing.T, url string) ([]byte, error) {
	t.Helper()
	dialer := &net.Dialer{}
	client := &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if tc, ok := c.(*net.TCPConn); ok {
				if err := tc.SetReadBuffer(socketBuffer); err != nil {
					return nil, errors.Join(err, c.Close())
				}
			}
			return c, nil
		},
	}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build GET %s: %v", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, want 200", url, resp.StatusCode)
	}
	head := make([]byte, firstRead)
	n, err := io.ReadFull(resp.Body, head)
	if err != nil {
		return head[:n], err
	}
	time.Sleep(clientPause)
	rest, err := io.ReadAll(resp.Body)
	return append(head, rest...), err
}

// TestArtifact_IdleWriterOutlivesWriteTimeout pins that a client that pauses
// for longer than the server's WriteTimeout and ReadTimeout still gets the whole
// file: /u/ pushes the write deadline out per write and never watches the
// request context. The control, the same blob served without the idle writer,
// is cut off, so the test can fail. The shortened ReadTimeout is plan gate 1's
// S6 guard: net/http clears the read deadline once a handler runs, so today it
// cancels nothing and only a future change that makes it do so would show.
func TestArtifact_IdleWriterOutlivesWriteTimeout(t *testing.T) {
	g := newRig(t)
	g.stockLive()
	want := g.liveBytes()

	cfg, _ := testConfig(t, g.repo, g.storeRoot)
	cfg.Subnet = netip.MustParsePrefix("127.0.0.0/8")
	h, err := NewHandlers(cfg, WithClock(testNow))
	if err != nil {
		t.Fatalf("NewHandlers: %v", err)
	}

	t.Run("with idleWriter", func(t *testing.T) {
		srv := startShortTimeoutServer(t, h.PlainHTTP())

		got, err := slowGet(t, srv.URL+liveURL)

		if err != nil {
			t.Fatalf("download: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("downloaded %d bytes, want the %d bytes of the file, byte for byte", len(got), len(want))
		}
	})
	t.Run("control without idleWriter", func(t *testing.T) {
		f, _ := g.live().File(liveFile)
		desc, err := store.Layer(liveFile, f.Size, f.SHA256)
		if err != nil {
			t.Fatalf("layer: %v", err)
		}
		srv := startShortTimeoutServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			blob, err := g.st.OpenBlob(desc)
			if err != nil {
				t.Errorf("OpenBlob: %v", err)
				return
			}
			defer func() {
				if err := blob.Close(); err != nil {
					t.Errorf("close blob: %v", err)
				}
			}()
			w.Header().Set("Content-Type", contentTypeBinary)
			http.ServeContent(w, r, "filesystem.squashfs", time.Time{}, blob)
		}))

		got, err := slowGet(t, srv.URL+liveURL)

		if err == nil {
			t.Error("control download ended without an error: the timeouts did not cut it off, so the test proves nothing")
		}
		if !bytes.HasPrefix(want, got) || len(got) >= len(want) {
			t.Errorf("control downloaded %d of %d bytes, want a strict prefix of the file", len(got), len(want))
		}
	})
}

// TestArtifact_MissingManifestRequeues is a store whose manifest blob is gone
// while its tag is not: lost content, like a missing layer.
func TestArtifact_MissingManifestRequeues(t *testing.T) {
	g := newRig(t)
	g.stockLive()
	desc, err := g.st.Resolve(t.Context(), g.live().Tag())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := os.Remove(filepath.Join(g.storeRoot, "blobs", "sha256", desc.Digest.Encoded())); err != nil {
		t.Fatalf("remove manifest blob: %v", err)
	}

	rec := g.plain(liveURL)

	wantStatus(t, rec, http.StatusNotFound)
	g.wantRequeued()
}

// TestArtifact_LostContentReportsWhenTheRowCannotBeRead pins that the store's
// error is not dropped when the cache row it would requeue cannot be read.
func TestArtifact_LostContentReportsWhenTheRowCannotBeRead(t *testing.T) {
	g := newRig(t)
	g.stockLive()
	if err := os.Remove(g.liveBlob()); err != nil {
		t.Fatalf("remove blob: %v", err)
	}
	if err := g.d.Close(); err != nil { // dbtest closes it again; database/sql allows that
		t.Fatalf("close database: %v", err)
	}

	rec := g.plain(liveURL)

	wantStatus(t, rec, http.StatusNotFound)
	if len(g.errs) != 1 || !errors.Is(g.errs[0], store.ErrLayerMissing) {
		t.Errorf("reported errors = %v, want one that wraps the store's ErrLayerMissing", g.errs)
	}
}
