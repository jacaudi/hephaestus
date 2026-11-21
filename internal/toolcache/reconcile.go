package toolcache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/store"
)

// ReconcileConfig is what Reconcile and ReconcileTalos bring into agreement.
// Every field is required.
type ReconcileConfig struct {
	// Repo holds the utility_cache and talos_cache rows.
	Repo *fleet.Repository
	// Store holds the fetched content.
	Store *store.Store
	// Catalog says which entries and pins are current.
	Catalog *catalog.Catalog
	// SpoolDir holds the partial downloads. Reconcile creates it when missing.
	SpoolDir string
	// At is the time written to every row Reconcile changes.
	At time.Time
}

// ReconcileReport counts what Reconcile changed.
type ReconcileReport struct {
	// Interrupted is the fetching rows given back to wanted.
	Interrupted int
	// Deleted is the rows of entries the catalog no longer has.
	Deleted int
	// Repinned is the rows moved to the catalog's current pin.
	Repinned int
	// Requeued is the ready rows whose content is missing from the store.
	Requeued int
	// Restored is the rows found complete in the store and made ready.
	Restored int
	// SpoolRemoved is the spool files and directories removed.
	SpoolRemoved int
	// SpoolErr joins what the spool sweep could not do. It is not an error of
	// Reconcile: leftovers are only disk, as garbage is.
	SpoolErr error
}

// Reconcile creates the spool directory when it is missing, then brings the
// utility_cache rows, the store and the spool into agreement with the catalog,
// in design section 5.7's order: fetching rows become wanted; a row of an entry
// or architecture the catalog lacks is deleted; a row at another pin is
// repinned; a ready row whose tag or layers the store lacks is requeued; a
// wanted or failed row whose content is complete in the store becomes ready;
// and every spool file that is neither a current pin's nor a Talos row's still
// to fetch is removed. It runs before the fetcher starts. It stops at the first
// repository error, which a done ctx causes, and at the first store error that
// is not lost content (see store.ContentLost), such as a permission failure. A
// spool that cannot be swept is not one of those stops: the report carries the
// failure in SpoolErr.
func Reconcile(ctx context.Context, cfg ReconcileConfig) (ReconcileReport, error) {
	var rep ReconcileReport
	if err := os.MkdirAll(cfg.SpoolDir, spoolDirMode); err != nil {
		return rep, fmt.Errorf("toolcache: create the spool dir: %w", err)
	}
	n, err := cfg.Repo.ResetFetching(ctx, cfg.At)
	if err != nil {
		return rep, err
	}
	rep.Interrupted = n

	rows, err := cfg.Repo.CacheRows(ctx)
	if err != nil {
		return rep, err
	}
	for i := range rows {
		if err := reconcileRow(ctx, cfg, rows[i], &rep); err != nil {
			return rep, err
		}
	}

	entries := cfg.Catalog.Entries()
	for i := range entries {
		e := &entries[i]
		switch err := cfg.Store.CheckLayers(ctx, e.Tag()); {
		case store.ContentLost(err):
			continue
		case err != nil:
			return rep, fmt.Errorf("toolcache: check %s/%s content: %w", e.Entry, e.Arch, err)
		}
		key := fleet.CacheKey{Entry: e.Entry, Arch: fleet.Arch(e.Arch)}
		moved, err := cfg.Repo.RestoreReady(ctx, key, e.Pin.ID, cfg.At)
		if err != nil {
			return rep, err
		}
		if moved {
			rep.Restored++
		}
	}

	rep.SpoolRemoved, rep.SpoolErr = sweepSpool(ctx, cfg)
	return rep, nil
}

// reconcileRow settles one row against the catalog and the store. A ready row
// whose content the store lacks has nothing to serve, so it goes back to wanted
// and is refetched, unattended. Content the store cannot read is not content it
// lacks: that is returned, and the row is left alone.
func reconcileRow(ctx context.Context, cfg ReconcileConfig, row fleet.CacheRow, rep *ReconcileReport) error {
	key := row.Key()
	e, ok := cfg.Catalog.Lookup(row.Entry, string(row.Arch))
	switch {
	case !ok:
		if err := cfg.Repo.DeleteCacheRow(ctx, key); err != nil {
			return err
		}
		rep.Deleted++
	case row.Pin != e.Pin.ID:
		moved, err := cfg.Repo.Repin(ctx, key, row.Pin, e.Pin.ID, cfg.At)
		if err != nil {
			return err
		}
		if moved {
			rep.Repinned++
		}
	case row.State == fleet.CacheReady:
		switch err := cfg.Store.CheckLayers(ctx, e.Tag()); {
		case err == nil:
			return nil
		case !store.ContentLost(err):
			return fmt.Errorf("toolcache: check %s/%s content: %w", row.Entry, row.Arch, err)
		}
		moved, err := cfg.Repo.Requeue(ctx, key, row.Pin, cfg.At)
		if err != nil {
			return err
		}
		if moved {
			rep.Requeued++
		}
	}
	return nil
}

// sweepSpool removes every entry of the spool directory that is not the spool
// file or ETag sidecar of some current pin's object or of a part of a Talos row
// still to fetch: wanted, fetching or failed. It runs before ReconcileTalos, so
// a fetching row keeps its spool for when ReconcileTalos makes it wanted again.
// A ready or broken row's is removed. Without the Talos rows it does not know
// what to keep, so it removes nothing and returns their error. It removes what
// it can and joins the failures. It returns how many entries it removed.
func sweepSpool(ctx context.Context, cfg ReconcileConfig) (removed int, err error) {
	rows, err := cfg.Repo.TalosRows(ctx)
	if err != nil {
		return 0, fmt.Errorf("toolcache: list the Talos rows to keep their spools: %w", err)
	}
	keep := map[string]bool{}
	current := cfg.Catalog.Entries()
	for i := range current {
		pin := &current[i].Pin
		for n := range pin.Objects {
			keep[SpoolName(pin.ID, n)] = true
			keep[ETagName(pin.ID, n)] = true
		}
	}
	parts := fleet.TalosParts()
	for i := range rows {
		switch rows[i].State {
		case fleet.CacheWanted, fleet.CacheFetching, fleet.CacheFailed:
			for n := range parts[:] {
				o := object{SpoolName: TalosSpoolName(rows[i].Key(), n)}
				keep[o.SpoolName] = true
				keep[o.etagName()] = true
			}
		case fleet.CacheReady, fleet.CacheBroken:
		}
	}
	root, err := os.OpenRoot(cfg.SpoolDir)
	if err != nil {
		return 0, fmt.Errorf("toolcache: open the spool dir: %w", err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return 0, fmt.Errorf("toolcache: list the spool dir: %w", err)
	}
	var errs []error
	for _, ent := range entries {
		if keep[ent.Name()] {
			continue
		}
		if err := root.RemoveAll(ent.Name()); err != nil {
			errs = append(errs, fmt.Errorf("toolcache: remove spool entry %s: %w", ent.Name(), err))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// CollectGarbage untags every tag that starts with catalog.TagPrefix and is not
// the tag of some current catalog entry, then removes the blobs no tag reaches
// (the store keeps unreachable blobs written in the last hour). Tags of other
// prefixes are left alone. It returns how many tags it removed. Run it at
// startup only: nothing may be pushing blobs, and a removed tag is not served.
func CollectGarbage(ctx context.Context, st *store.Store, cat *catalog.Catalog) (int, error) {
	current := map[string]bool{}
	entries := cat.Entries()
	for i := range entries {
		current[entries[i].Tag()] = true
	}
	tags, err := st.Tags(ctx, catalog.TagPrefix)
	if err != nil {
		return 0, err
	}
	var untagged int
	for _, tag := range tags {
		if current[tag] {
			continue
		}
		if err := st.Untag(ctx, tag); err != nil {
			return untagged, err
		}
		untagged++
	}
	return untagged, st.GC(ctx)
}
