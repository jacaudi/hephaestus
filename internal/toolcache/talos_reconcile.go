package toolcache

import (
	"context"
	"fmt"
	"maps"

	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// TalosReport counts what ReconcileTalos changed.
type TalosReport struct {
	// Interrupted is the fetching rows given back to wanted.
	Interrupted int
	// Requeued is the ready rows whose tag is lost or does not match their
	// recorded hashes.
	Requeued int
	// Restored is the wanted or failed rows found whole in the store and made ready.
	Restored int
}

// ReconcileTalos brings the talos_cache rows into agreement with the store, in
// design section 5.7's order: fetching rows become wanted; a ready row whose tag
// is lost (store.ContentLost) or whose layers are not its recorded hashes becomes
// wanted, its hashes kept; a broken row stays broken; and a wanted or failed row
// with all three hashes recorded, whose tag passes the layer check and matches
// them, becomes ready. A row without recorded hashes is never made ready: its tag
// cannot be checked against anything. It runs after Reconcile, whose spool sweep
// keeps the spools of the rows still to fetch, and before the fetcher starts. It
// stops at the first repository error and at the first store error that is not
// lost content.
func ReconcileTalos(ctx context.Context, cfg ReconcileConfig) (TalosReport, error) {
	var rep TalosReport
	n, err := cfg.Repo.ResetTalosFetching(ctx, cfg.At)
	if err != nil {
		return rep, err
	}
	rep.Interrupted = n

	rows, err := cfg.Repo.TalosRows(ctx)
	if err != nil {
		return rep, err
	}
	for i := range rows {
		if err := reconcileTalosRow(ctx, cfg, rows[i], &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// reconcileTalosRow settles one row, as read, against the store.
func reconcileTalosRow(ctx context.Context, cfg ReconcileConfig, row fleet.TalosRow, rep *TalosReport) error {
	switch row.State {
	case fleet.CacheReady:
		stored, err := talosVerified(ctx, cfg, row)
		if err != nil || stored {
			return err
		}
		moved, err := cfg.Repo.RequeueTalos(ctx, row.Key(), cfg.At)
		if err != nil {
			return err
		}
		if moved {
			rep.Requeued++
		}
	case fleet.CacheWanted, fleet.CacheFailed:
		stored, err := talosVerified(ctx, cfg, row)
		if err != nil || !stored {
			return err
		}
		moved, err := restoreTalos(ctx, cfg, row)
		if err != nil {
			return err
		}
		if moved {
			rep.Restored++
		}
	case fleet.CacheFetching, fleet.CacheBroken:
		// Only an admin re-arms a broken row, and no row is fetching once
		// ResetTalosFetching has run.
	}
	return nil
}

// talosVerified reports whether row records all three hashes and its tag passes
// the layer check and matches them.
func talosVerified(ctx context.Context, cfg ReconcileConfig, row fleet.TalosRow) (bool, error) {
	if !row.Assets.Complete() {
		return false, nil
	}
	stored, err := talosStored(ctx, cfg.Store, row.Key(), row.Assets)
	if err != nil {
		return false, fmt.Errorf("toolcache: reconcile Talos: %w", err)
	}
	return stored, nil
}

// restoreTalos makes ready the wanted or failed row whose tag matched the hashes
// it was read with. Every ready write needs a claim, and the claim also fences
// what was read: the row is made ready only when it holds the same hashes once
// claimed, so a hash reset since the read is never undone. Otherwise the claim
// is given back and the row is fetched, as the reset asks.
func restoreTalos(ctx context.Context, cfg ReconcileConfig, read fleet.TalosRow) (bool, error) {
	claimed, ok, err := cfg.Repo.ClaimTalosRow(ctx, read.Key(), cfg.At)
	if err != nil || !ok {
		return false, err
	}
	if !maps.Equal(claimed.Assets, read.Assets) {
		_, err := cfg.Repo.ResetTalosFetchingRow(ctx, claimed.Claim(), cfg.At)
		return false, err
	}
	return cfg.Repo.MarkTalosReady(ctx, claimed.Claim(), claimed.Assets, cfg.At)
}
