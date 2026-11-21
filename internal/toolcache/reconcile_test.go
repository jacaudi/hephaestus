package toolcache

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/catalog/catalogtest"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/store"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const stalePin = "stale0000000"

func tshellARM64() fleet.CacheKey { return fleet.CacheKey{Entry: "tshell", Arch: fleet.ArchARM64} }

// reconcileEnv is a SQLite database, a store, a spool directory and the fixture
// catalog, with helpers to put each of them in the state a test names.
type reconcileEnv struct {
	t        *testing.T
	d        *db.DB
	repo     *fleet.Repository
	st       *store.Store
	storeDir string
	spool    string
	fx       catalogtest.Fixture
}

func newReconcileEnv(t *testing.T) *reconcileEnv {
	t.Helper()
	e := &reconcileEnv{
		t: t, d: dbtest.SQLite(t), storeDir: filepath.Join(t.TempDir(), "store"), spool: t.TempDir(),
		fx: catalogtest.New(t, "https://upstream.invalid"),
	}
	e.repo = fleet.NewRepository(e.d)
	e.openStore()
	return e
}

// openStore opens the store at storeDir and returns the path it moved aside, if any.
func (e *reconcileEnv) openStore() string {
	e.t.Helper()
	st, moved, err := store.Open(e.t.Context(), e.storeDir, fetchT0)
	if err != nil {
		e.t.Fatalf("store.Open: %v", err)
	}
	e.st = st
	return moved
}

// pin is the catalog's current pin of key.
func (e *reconcileEnv) pin(key fleet.CacheKey) string {
	e.t.Helper()
	ent, ok := e.fx.Catalog.Lookup(key.Entry, string(key.Arch))
	if !ok {
		e.t.Fatalf("the fixture has no %v", key)
	}
	return ent.Pin.ID
}

// insert stores a row in state at pin.
func (e *reconcileEnv) insert(key fleet.CacheKey, pin string, state fleet.CacheState, attempts int) {
	e.t.Helper()
	row := fleet.CacheRow{Entry: key.Entry, Arch: key.Arch, Pin: pin, State: state, Attempts: attempts, UpdatedAt: fetchT0().Add(-time.Hour)}
	if state == fleet.CacheFailed {
		row.NextAttemptAt = fetchT0().Add(time.Hour)
		row.LastError = "boom"
	}
	if _, err := e.d.Bun().NewInsert().Model(&row).Exec(e.t.Context()); err != nil {
		e.t.Fatalf("insert %v: %v", key, err)
	}
}

// stock pushes key's entry into the store, as a finished fetch would.
func (e *reconcileEnv) stock(key fleet.CacheKey) {
	e.t.Helper()
	catalogtest.Stock(e.t, e.st, e.fx, catalog.Key{Entry: key.Entry, Arch: string(key.Arch)})
}

func (e *reconcileEnv) spoolFile(name string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.spool, name), []byte("partial"), 0o600); err != nil {
		e.t.Fatalf("write spool file %s: %v", name, err)
	}
}

// spoolNames lists the spool directory, sorted.
func (e *reconcileEnv) spoolNames() []string {
	e.t.Helper()
	entries, err := os.ReadDir(e.spool)
	if err != nil {
		e.t.Fatalf("read spool dir: %v", err)
	}
	var names []string
	for _, ent := range entries {
		names = append(names, ent.Name())
	}
	return names
}

func (e *reconcileEnv) reconcile() ReconcileReport {
	e.t.Helper()
	rep, err := Reconcile(e.t.Context(), ReconcileConfig{
		Repo: e.repo, Store: e.st, Catalog: e.fx.Catalog, SpoolDir: e.spool, At: fetchT0(),
	})
	if err != nil {
		e.t.Fatalf("Reconcile: %v", err)
	}
	return rep
}

// wantRow fails unless the row at key is in state at pin with attempts failed attempts.
func (e *reconcileEnv) wantRow(key fleet.CacheKey, state fleet.CacheState, pin string, attempts int) {
	e.t.Helper()
	row, ok, err := e.repo.CacheRow(e.t.Context(), key)
	if err != nil || !ok {
		e.t.Fatalf("CacheRow(%v) = %v, %v", key, ok, err)
	}
	if row.State != state || row.Pin != pin || row.Attempts != attempts {
		e.t.Errorf("row %v = %s at %s with %d attempts; want %s at %s with %d", key, row.State, row.Pin, row.Attempts, state, pin, attempts)
	}
}

// blobFile is the path of the blob file of the layer of key's entry titled title.
func (e *reconcileEnv) blobFile(key fleet.CacheKey, title string) string {
	e.t.Helper()
	ent, _ := e.fx.Catalog.Lookup(key.Entry, string(key.Arch))
	layers, err := e.st.Layers(e.t.Context(), ent.Tag())
	if err != nil {
		e.t.Fatalf("Layers(%s): %v", ent.Tag(), err)
	}
	for _, l := range layers {
		if l.Annotations[ocispec.AnnotationTitle] == title {
			return filepath.Join(e.storeDir, ocispec.ImageBlobsDir, l.Digest.Algorithm().String(), l.Digest.Encoded())
		}
	}
	e.t.Fatalf("%s has no layer %q", ent.Tag(), title)
	return ""
}

func TestReconcile_Steps(t *testing.T) {
	e := newReconcileEnv(t)
	live, shell := e.pin(tliveAMD64()), e.pin(tshellAMD64())
	e.insert(tliveAMD64(), live, fleet.CacheFetching, 0)
	e.insert(tshellAMD64(), shell, fleet.CacheReady, 0) // ready, but not in the store
	e.insert(tshellARM64(), e.pin(tshellARM64()), fleet.CacheReady, 0)
	e.stock(tshellARM64())
	e.insert(tliveARM64(), stalePin, fleet.CacheFailed, 3)
	e.insert(fleet.CacheKey{Entry: "gone", Arch: fleet.ArchAMD64}, "gone00000000", fleet.CacheWanted, 0)
	for _, name := range []string{SpoolName(live, 0), ETagName(live, 0), "junk", SpoolName(stalePin, 0)} {
		e.spoolFile(name)
	}

	got := e.reconcile()

	if want := (ReconcileReport{Interrupted: 1, Deleted: 1, Repinned: 1, Requeued: 1, Restored: 0, SpoolRemoved: 2}); got != want {
		t.Errorf("report = %+v, want %+v", got, want)
	}
	e.wantRow(tliveAMD64(), fleet.CacheWanted, live, 0)
	e.wantRow(tshellAMD64(), fleet.CacheWanted, shell, 0)
	e.wantRow(tshellARM64(), fleet.CacheReady, e.pin(tshellARM64()), 0)
	e.wantRow(tliveARM64(), fleet.CacheWanted, e.pin(tliveARM64()), 0)
	if _, ok, err := e.repo.CacheRow(t.Context(), fleet.CacheKey{Entry: "gone", Arch: fleet.ArchAMD64}); err != nil || ok {
		t.Errorf("CacheRow(gone) = %v, %v; want the row deleted", ok, err)
	}
	if got, want := e.spoolNames(), []string{SpoolName(live, 0), ETagName(live, 0)}; !slices.Equal(got, want) {
		t.Errorf("spool = %v, want %v", got, want)
	}
}

func TestReconcile_RestoresAfterCrashBetweenTagAndState(t *testing.T) {
	e := newReconcileEnv(t)
	e.insert(tliveAMD64(), e.pin(tliveAMD64()), fleet.CacheFetching, 0)
	e.stock(tliveAMD64())

	got := e.reconcile()

	if got.Interrupted != 1 || got.Restored != 1 {
		t.Errorf("report = %+v, want Interrupted 1 and Restored 1", got)
	}
	e.wantRow(tliveAMD64(), fleet.CacheReady, e.pin(tliveAMD64()), 0)
}

func TestReconcile_AfterCorruptIndex(t *testing.T) {
	e := newReconcileEnv(t)
	e.insert(tliveAMD64(), e.pin(tliveAMD64()), fleet.CacheReady, 0)
	e.stock(tliveAMD64())
	if err := os.WriteFile(filepath.Join(e.storeDir, ocispec.ImageIndexFile), []byte("{"), 0o600); err != nil {
		t.Fatalf("corrupt index.json: %v", err)
	}
	if moved := e.openStore(); moved == "" {
		t.Fatal("store.Open moved nothing aside, want the corrupt layout moved")
	}

	got := e.reconcile()

	if got.Requeued != 1 {
		t.Errorf("report = %+v, want Requeued 1", got)
	}
	e.wantRow(tliveAMD64(), fleet.CacheWanted, e.pin(tliveAMD64()), 0)
}

func TestReconcile_AfterEmptyIndex(t *testing.T) {
	e := newReconcileEnv(t)
	e.insert(tliveAMD64(), e.pin(tliveAMD64()), fleet.CacheReady, 0)
	e.stock(tliveAMD64())
	if err := os.Truncate(filepath.Join(e.storeDir, ocispec.ImageIndexFile), 0); err != nil {
		t.Fatalf("empty index.json: %v", err)
	}
	if moved := e.openStore(); moved != "" {
		t.Fatalf("store.Open moved %s aside, want an empty index reset in place", moved)
	}

	got := e.reconcile()

	if got.Requeued != 1 {
		t.Errorf("report = %+v, want Requeued 1: the blobs stay but no tag reaches them", got)
	}
	e.wantRow(tliveAMD64(), fleet.CacheWanted, e.pin(tliveAMD64()), 0)
}

func TestReconcile_TruncatedLayer(t *testing.T) {
	e := newReconcileEnv(t)
	e.insert(tliveAMD64(), e.pin(tliveAMD64()), fleet.CacheReady, 0)
	e.stock(tliveAMD64())
	blob := e.blobFile(tliveAMD64(), squashfsPath)
	if err := os.Chmod(blob, 0o644); err != nil { // blobs are 0444; production never chmods
		t.Fatalf("chmod blob: %v", err)
	}
	if err := os.Truncate(blob, 1000); err != nil {
		t.Fatalf("truncate blob: %v", err)
	}

	got := e.reconcile()

	if got.Requeued != 1 {
		t.Errorf("report = %+v, want Requeued 1", got)
	}
	e.wantRow(tliveAMD64(), fleet.CacheWanted, e.pin(tliveAMD64()), 0)
}

func TestReconcile_LeavesBrokenAndReadyAlone(t *testing.T) {
	e := newReconcileEnv(t)
	e.insert(tliveAMD64(), e.pin(tliveAMD64()), fleet.CacheBroken, 10)
	e.insert(tshellARM64(), e.pin(tshellARM64()), fleet.CacheReady, 0)
	e.stock(tliveAMD64()) // a broken row's content may be in the store; only an admin re-arms it
	e.stock(tshellARM64())

	got := e.reconcile()

	if got != (ReconcileReport{}) {
		t.Errorf("report = %+v, want nothing changed", got)
	}
	e.wantRow(tliveAMD64(), fleet.CacheBroken, e.pin(tliveAMD64()), 10)
	e.wantRow(tshellARM64(), fleet.CacheReady, e.pin(tshellARM64()), 0)
}

func TestReconcile_StoppedByContext(t *testing.T) {
	e := newReconcileEnv(t)
	e.insert(tliveAMD64(), e.pin(tliveAMD64()), fleet.CacheReady, 0)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := Reconcile(ctx, ReconcileConfig{Repo: e.repo, Store: e.st, Catalog: e.fx.Catalog, SpoolDir: e.spool, At: fetchT0()})

	if err == nil {
		t.Fatal("Reconcile with a cancelled context = nil, want an error")
	}
	e.wantRow(tliveAMD64(), fleet.CacheReady, e.pin(tliveAMD64()), 0)
}

func TestCollectGarbage(t *testing.T) {
	e := newReconcileEnv(t)
	e.stock(tliveAMD64())
	current, _ := e.fx.Catalog.Lookup("tlive", "amd64")

	// An old tag with a layer of its own, and a tag of another namespace.
	oldLayer := []byte("the old pin's own layer")
	desc, err := store.Layer("old.bin", int64(len(oldLayer)), sha(oldLayer))
	if err != nil {
		t.Fatalf("Layer: %v", err)
	}
	if err := e.st.PushBlob(t.Context(), desc, bytes.NewReader(oldLayer)); err != nil {
		t.Fatalf("PushBlob: %v", err)
	}
	oldTag := catalog.TagPrefix + "tlive-amd64:oldpin00000"
	if _, err := e.st.TagArtifact(t.Context(), oldTag, catalog.ArtifactType, []ocispec.Descriptor{desc}); err != nil {
		t.Fatalf("TagArtifact(%s): %v", oldTag, err)
	}
	other, _ := store.Layer("y.bin", 1, sha([]byte("y")))
	if err := e.st.PushBlob(t.Context(), other, bytes.NewReader([]byte("y"))); err != nil {
		t.Fatalf("PushBlob: %v", err)
	}
	if _, err := e.st.TagArtifact(t.Context(), "talos/x:y", "application/vnd.test", []ocispec.Descriptor{other}); err != nil {
		t.Fatalf("TagArtifact(talos/x:y): %v", err)
	}
	ageBlobs(t, e.storeDir, 2*time.Hour)

	n, err := CollectGarbage(t.Context(), e.st, e.fx.Catalog)

	if err != nil || n != 1 {
		t.Fatalf("CollectGarbage = %d, %v; want 1, nil", n, err)
	}
	tags, err := e.st.Tags(t.Context(), "")
	if err != nil {
		t.Fatalf("Tags: %v", err)
	}
	if want := []string{"talos/x:y", current.Tag()}; !slices.Equal(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
	if err := e.st.CheckLayers(t.Context(), current.Tag()); err != nil {
		t.Errorf("CheckLayers(%s) = %v, want the current entry intact", current.Tag(), err)
	}
	if _, err := os.Stat(filepath.Join(e.storeDir, ocispec.ImageBlobsDir, "sha256", desc.Digest.Encoded())); !os.IsNotExist(err) {
		t.Errorf("the old tag's layer blob: stat error = %v, want it removed", err)
	}
}

// ageBlobs sets the modification time of every blob under root to d ago, past
// the hour oras keeps unreferenced blobs for.
func ageBlobs(t *testing.T, root string, d time.Duration) {
	t.Helper()
	dir := filepath.Join(root, ocispec.ImageBlobsDir, "sha256")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read blobs: %v", err)
	}
	old := time.Now().Add(-d)
	for _, ent := range entries {
		if err := os.Chtimes(filepath.Join(dir, ent.Name()), old, old); err != nil {
			t.Fatalf("age %s: %v", ent.Name(), err)
		}
	}
}

// TestReconcile_UnreadableContentNotRequeued pins that content the store cannot
// read is not content it lacks: a permission failure stops Reconcile and leaves
// the row alone, rather than refetching (and, offline, breaking) it.
func TestReconcile_UnreadableContentNotRequeued(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	for _, state := range []fleet.CacheState{fleet.CacheReady, fleet.CacheWanted} {
		t.Run(string(state), func(t *testing.T) {
			e := newReconcileEnv(t)
			ent, _ := e.fx.Catalog.Lookup("tlive", "amd64")
			e.insert(tliveAMD64(), ent.Pin.ID, state, 0)
			e.stock(tliveAMD64())
			manifest, err := e.st.Resolve(t.Context(), ent.Tag())
			if err != nil {
				t.Fatalf("Resolve(%s): %v", ent.Tag(), err)
			}
			blob := filepath.Join(e.storeDir, ocispec.ImageBlobsDir, manifest.Digest.Algorithm().String(), manifest.Digest.Encoded())
			if err := os.Chmod(blob, 0o000); err != nil {
				t.Fatalf("chmod manifest blob: %v", err)
			}
			t.Cleanup(func() {
				if err := os.Chmod(blob, 0o444); err != nil {
					t.Errorf("restore manifest blob mode: %v", err)
				}
			})

			_, err = Reconcile(t.Context(), ReconcileConfig{
				Repo: e.repo, Store: e.st, Catalog: e.fx.Catalog, SpoolDir: e.spool, At: fetchT0(),
			})

			if err == nil {
				t.Fatal("Reconcile = nil, want the permission error")
			}
			if store.ContentLost(err) {
				t.Errorf("Reconcile error %v counts as lost content, want a read failure", err)
			}
			e.wantRow(tliveAMD64(), state, ent.Pin.ID, 0)
		})
	}
}

// TestReconcile_SpoolSweepFailureDoesNotFailReconcile pins that stray spool
// files are only disk: a sweep that cannot remove them is reported in the
// report, next to the counts of everything else Reconcile did, and is not an
// error that stops serve.
func TestReconcile_SpoolSweepFailureDoesNotFailReconcile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	e := newReconcileEnv(t)
	e.insert(tliveAMD64(), e.pin(tliveAMD64()), fleet.CacheFetching, 0)
	e.spoolFile("stray")
	if err := os.Chmod(e.spool, 0o500); err != nil {
		t.Fatalf("chmod spool dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(e.spool, 0o700); err != nil {
			t.Errorf("restore spool dir mode: %v", err)
		}
	})

	rep, err := Reconcile(t.Context(), ReconcileConfig{Repo: e.repo, Store: e.st, Catalog: e.fx.Catalog, SpoolDir: e.spool, At: fetchT0()})

	if err != nil {
		t.Fatalf("Reconcile = %v, want nil: a sweep failure is the report's", err)
	}
	if rep.SpoolErr == nil {
		t.Error("SpoolErr = nil, want the failure to remove the stray file")
	}
	if rep.Interrupted != 1 || rep.SpoolRemoved != 0 {
		t.Errorf("report = %+v, want the interrupted row counted and nothing removed", rep)
	}
}

func TestReconcile_CreatesSpoolDir(t *testing.T) {
	e := newReconcileEnv(t)
	e.spool = filepath.Join(t.TempDir(), "spool")

	e.reconcile()

	fi, err := os.Stat(e.spool)
	if err != nil || !fi.IsDir() {
		t.Fatalf("stat spool dir = %v, %v; want a directory", fi, err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("spool dir mode = %#o, want 0700", got)
	}
}

// TestSweepSpool_KeepsTalosSpools pins that the sweep keeps the spool files and
// ETag sidecars of every Talos row a fetch may still resume (wanted, fetching,
// failed) and removes those of a ready or broken row with the unknown files.
func TestSweepSpool_KeepsTalosSpools(t *testing.T) {
	e := newReconcileEnv(t)
	wanted, fetching, failed := talosKeyAt("v1.12.11"), talosKeyAt("v1.12.10"), talosKeyAt("v1.12.9")
	ready, broken := talosKeyAt("v1.12.8"), talosKeyAt("v1.12.7")
	e.insertTalos(wanted, fleet.CacheWanted, nil)
	e.insertTalos(fetching, fleet.CacheFetching, nil)
	e.insertTalos(failed, fleet.CacheFailed, nil)
	e.insertTalos(ready, fleet.CacheReady, talosAssetsOf(talosContent("one")))
	e.insertTalos(broken, fleet.CacheBroken, nil)
	var kept []string
	for n := range 3 {
		kept = append(kept, TalosSpoolName(wanted, n), TalosSpoolName(wanted, n)+".etag")
	}
	kept = append(kept, TalosSpoolName(fetching, 0), TalosSpoolName(failed, 1), TalosSpoolName(failed, 1)+".etag")
	for _, name := range append([]string{TalosSpoolName(ready, 0), TalosSpoolName(broken, 2), "unknown"}, kept...) {
		e.spoolFile(name)
	}

	removed, err := sweepSpool(t.Context(), ReconcileConfig{Repo: e.repo, Store: e.st, Catalog: e.fx.Catalog, SpoolDir: e.spool, At: fetchT0()})

	if err != nil || removed != 3 {
		t.Errorf("sweepSpool = %d, %v; want 3, nil", removed, err)
	}
	if got, want := e.spoolNames(), slices.Sorted(slices.Values(kept)); !slices.Equal(got, want) {
		t.Errorf("spool = %v, want %v", got, want)
	}
}

// TestSweepSpool_TalosRowsErrorRemovesNothing pins that a sweep that cannot
// read the Talos rows does not know what to keep, so it removes nothing.
func TestSweepSpool_TalosRowsErrorRemovesNothing(t *testing.T) {
	e := newReconcileEnv(t)
	wanted := talosKeyAt(talosTestVersion)
	e.insertTalos(wanted, fleet.CacheWanted, nil)
	names := []string{TalosSpoolName(wanted, 0), TalosSpoolName(wanted, 0) + ".etag", SpoolName(e.pin(tliveAMD64()), 0), "unknown"}
	for _, name := range names {
		e.spoolFile(name)
	}
	if err := e.d.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	removed, err := sweepSpool(t.Context(), ReconcileConfig{Repo: e.repo, Store: e.st, Catalog: e.fx.Catalog, SpoolDir: e.spool, At: fetchT0()})

	if err == nil || removed != 0 {
		t.Errorf("sweepSpool = %d, %v; want 0 and the database error", removed, err)
	}
	if got, want := e.spoolNames(), slices.Sorted(slices.Values(names)); !slices.Equal(got, want) {
		t.Errorf("spool = %v, want every file kept: %v", got, want)
	}
}
