package toolcache

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/store"
)

// talosKeyAt is the fixture schematic for amd64 at version.
func talosKeyAt(version string) fleet.TalosKey {
	return fleet.TalosKey{Schematic: plainSchematicID, Version: version, Arch: fleet.ArchAMD64}
}

// talosContent is the bytes of the three parts of one build, by part.
func talosContent(build string) map[fleet.TalosPart][]byte {
	return map[fleet.TalosPart][]byte{
		fleet.TalosKernel:    []byte("kernel of " + build),
		fleet.TalosInitramfs: []byte("initramfs of " + build),
		fleet.TalosCmdline:   []byte("talos.platform=metal build=" + build),
	}
}

// talosAssetsOf is what a fetch of content records.
func talosAssetsOf(content map[fleet.TalosPart][]byte) fleet.TalosAssets {
	assets := fleet.TalosAssets{}
	for p, b := range content {
		assets[p] = assetOf(b)
	}
	return assets
}

// insertTalos stores a row at key in state with recorded assets, last changed
// an hour before fetchT0. A failed row is in its backoff past fetchT0.
func (e *reconcileEnv) insertTalos(key fleet.TalosKey, state fleet.CacheState, assets fleet.TalosAssets) {
	e.t.Helper()
	row := fleet.TalosRow{
		Schematic: key.Schematic, Version: key.Version, Arch: key.Arch, State: state,
		Assets: orNone(assets), UpdatedAt: fetchT0().Add(-time.Hour),
	}
	switch state {
	case fleet.CacheFailed:
		row.Attempts, row.NextAttemptAt, row.LastError = 2, fetchT0().Add(time.Hour), "boom"
	case fleet.CacheBroken:
		row.Attempts, row.LastError = fleet.MaxFetchAttempts, "boom"
	case fleet.CacheWanted, fleet.CacheFetching, fleet.CacheReady:
	}
	if _, err := e.d.Bun().NewInsert().Model(&row).Exec(e.t.Context()); err != nil {
		e.t.Fatalf("insert Talos row %s: %v", key.Tag(), err)
	}
}

// orNone is assets, or none recorded for nil: the column is NOT NULL.
func orNone(assets fleet.TalosAssets) fleet.TalosAssets {
	if assets == nil {
		return fleet.TalosAssets{}
	}
	return assets
}

// stockTalos pushes content and tags it under key, as a finished Talos fetch would.
func (e *reconcileEnv) stockTalos(key fleet.TalosKey, content map[fleet.TalosPart][]byte) {
	e.t.Helper()
	layers, err := talosLayers(talosAssetsOf(content))
	if err != nil {
		e.t.Fatalf("talosLayers: %v", err)
	}
	for i, p := range fleet.TalosParts() {
		if err := e.st.PushBlob(e.t.Context(), layers[i], bytes.NewReader(content[p])); err != nil {
			e.t.Fatalf("PushBlob(%s): %v", p, err)
		}
	}
	if _, err := e.st.TagArtifact(e.t.Context(), key.Tag(), TalosArtifactType, layers); err != nil {
		e.t.Fatalf("TagArtifact(%s): %v", key.Tag(), err)
	}
}

func (e *reconcileEnv) reconcileTalos() TalosReport {
	e.t.Helper()
	rep, err := ReconcileTalos(e.t.Context(), ReconcileConfig{
		Repo: e.repo, Store: e.st, Catalog: e.fx.Catalog, SpoolDir: e.spool, At: fetchT0(),
	})
	if err != nil {
		e.t.Fatalf("ReconcileTalos: %v", err)
	}
	return rep
}

// wantTalosRow fails unless the row at key is in state with attempts failed
// attempts and assets recorded.
func (e *reconcileEnv) wantTalosRow(key fleet.TalosKey, state fleet.CacheState, attempts int, assets fleet.TalosAssets) {
	e.t.Helper()
	row, ok, err := e.repo.Talos(e.t.Context(), key)
	if err != nil || !ok {
		e.t.Fatalf("Talos(%s) = %v, %v", key.Tag(), ok, err)
	}
	if row.State != state || row.Attempts != attempts || !maps.Equal(row.Assets, orNone(assets)) {
		e.t.Errorf("row %s = %s, %d attempts, assets %v; want %s, %d, %v", key.Tag(), row.State, row.Attempts, row.Assets, state, attempts, assets)
	}
}

func TestReconcileTalos(t *testing.T) {
	built, rebuilt := talosContent("one"), talosContent("two")
	recorded, other := talosAssetsOf(built), talosAssetsOf(rebuilt)
	tests := []struct {
		name     string
		state    fleet.CacheState
		assets   fleet.TalosAssets
		stocked  bool // the store holds built under the row's tag
		want     fleet.CacheState
		attempts int
		report   TalosReport
	}{
		{name: "fetching becomes wanted", state: fleet.CacheFetching, assets: recorded, want: fleet.CacheWanted, report: TalosReport{Interrupted: 1}},
		{name: "fetching with its tag stored becomes ready", state: fleet.CacheFetching, assets: recorded, stocked: true, want: fleet.CacheReady, report: TalosReport{Interrupted: 1, Restored: 1}},
		{name: "ready with a lost tag becomes wanted", state: fleet.CacheReady, assets: recorded, want: fleet.CacheWanted, report: TalosReport{Requeued: 1}},
		{name: "ready with a digest mismatch becomes wanted", state: fleet.CacheReady, assets: other, stocked: true, want: fleet.CacheWanted, report: TalosReport{Requeued: 1}},
		{name: "ready with its tag stored stays", state: fleet.CacheReady, assets: recorded, stocked: true, want: fleet.CacheReady},
		{name: "broken stays", state: fleet.CacheBroken, assets: recorded, stocked: true, want: fleet.CacheBroken, attempts: fleet.MaxFetchAttempts},
		{name: "wanted with matching assets and tag becomes ready", state: fleet.CacheWanted, assets: recorded, stocked: true, want: fleet.CacheReady, report: TalosReport{Restored: 1}},
		{name: "failed in its backoff with matching assets and tag becomes ready", state: fleet.CacheFailed, assets: recorded, stocked: true, want: fleet.CacheReady, report: TalosReport{Restored: 1}},
		{name: "wanted without recorded hashes stays", state: fleet.CacheWanted, stocked: true, want: fleet.CacheWanted},
		{name: "wanted with other recorded hashes stays", state: fleet.CacheWanted, assets: other, stocked: true, want: fleet.CacheWanted},
		{name: "wanted with a lost tag stays", state: fleet.CacheWanted, assets: recorded, want: fleet.CacheWanted},
		{name: "failed with a lost tag stays", state: fleet.CacheFailed, assets: recorded, want: fleet.CacheFailed, attempts: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newReconcileEnv(t)
			key := talosKeyAt(talosTestVersion)
			e.insertTalos(key, tt.state, tt.assets)
			if tt.stocked {
				e.stockTalos(key, built)
			}

			got := e.reconcileTalos()

			if got != tt.report {
				t.Errorf("report = %+v, want %+v", got, tt.report)
			}
			// Reconcile keeps the recorded hashes in every case: only an admin's
			// hash reset clears them.
			e.wantTalosRow(key, tt.want, tt.attempts, tt.assets)
		})
	}
}

// TestReconcileTalos_HashResetAfterReadNotRepinned pins that a row is made
// ready only with the hashes it holds once claimed: an admin's hash reset that
// lands after the row was read leaves it wanted, without the old hashes.
func TestReconcileTalos_HashResetAfterReadNotRepinned(t *testing.T) {
	e := newReconcileEnv(t)
	key := talosKeyAt(talosTestVersion)
	built := talosContent("one")
	e.insertTalos(key, fleet.CacheWanted, talosAssetsOf(built))
	e.stockTalos(key, built)
	read, _, err := e.repo.Talos(t.Context(), key)
	if err != nil {
		t.Fatalf("Talos: %v", err)
	}
	if moved, err := e.repo.ResetTalosHash(t.Context(), key, fleet.ActorCLI, fetchT0()); err != nil || !moved {
		t.Fatalf("ResetTalosHash = %v, %v; want true", moved, err)
	}

	var rep TalosReport
	cfg := ReconcileConfig{Repo: e.repo, Store: e.st, Catalog: e.fx.Catalog, SpoolDir: e.spool, At: fetchT0()}
	if err := reconcileTalosRow(t.Context(), cfg, read, &rep); err != nil {
		t.Fatalf("reconcileTalosRow: %v", err)
	}

	if rep != (TalosReport{}) {
		t.Errorf("report = %+v, want nothing restored", rep)
	}
	e.wantTalosRow(key, fleet.CacheWanted, 0, nil)
}

// TestReconcileTalos_UnreadableContentStops pins that content the store cannot
// read is not content it lacks: ReconcileTalos stops with the error and leaves
// the row alone.
func TestReconcileTalos_UnreadableContentStops(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	for _, state := range []fleet.CacheState{fleet.CacheReady, fleet.CacheWanted} {
		t.Run(string(state), func(t *testing.T) {
			e := newReconcileEnv(t)
			key := talosKeyAt(talosTestVersion)
			built := talosContent("one")
			e.insertTalos(key, state, talosAssetsOf(built))
			e.stockTalos(key, built)
			manifest, err := e.st.Resolve(t.Context(), key.Tag())
			if err != nil {
				t.Fatalf("Resolve(%s): %v", key.Tag(), err)
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

			_, err = ReconcileTalos(t.Context(), ReconcileConfig{Repo: e.repo, Store: e.st, Catalog: e.fx.Catalog, SpoolDir: e.spool, At: fetchT0()})

			if err == nil {
				t.Fatal("ReconcileTalos = nil, want the permission error")
			}
			if store.ContentLost(err) {
				t.Errorf("ReconcileTalos error %v counts as lost content, want a read failure", err)
			}
			e.wantTalosRow(key, state, 0, talosAssetsOf(built))
		})
	}
}
