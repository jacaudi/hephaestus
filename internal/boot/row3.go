package boot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/store"
)

// utilityFacts reads what the utility name is for a machine of architecture arch
// booting with firmware p: whether the catalog has it, whether it supports the
// machine, and whether its content is ready to serve. It also returns the
// catalog entry for arch, or the zero Entry when the catalog has none.
//
// A cache row that is ready but whose layers fail the store's check is not
// ready. When the store lacks the content, the row is put back to wanted and
// ErrUtilityMissing is reported; when the store merely cannot read it, the row
// is left alone and the failure is reported as it is. A failure to read the
// cache row is returned.
func (h *Handlers) utilityFacts(ctx context.Context, name string, arch fleet.Arch, p catalog.Platform, now time.Time) (UtilityFacts, catalog.Entry, error) {
	facts := UtilityFacts{InCatalog: h.catalog.Has(name)}
	e, ok := h.catalog.Lookup(name, string(arch))
	if !ok {
		return facts, catalog.Entry{}, nil
	}
	facts.Supported = p != "" && e.Supports(p)
	row, found, err := h.repo.CacheRow(ctx, fleet.CacheKey{Entry: name, Arch: arch})
	if err != nil {
		return UtilityFacts{}, catalog.Entry{}, err
	}
	if !found || row.State != fleet.CacheReady || row.Pin != e.Pin.ID {
		return facts, e, nil
	}
	if err := h.content.CheckLayers(ctx, e.Tag()); err != nil {
		if contentLost(err) {
			h.requeueMissing(ctx, e, err, now)
		} else {
			h.reportUnreadable(ctx, e, err)
		}
		return facts, e, nil
	}
	facts.Ready = true
	return facts, e, nil
}

// contentLost reports whether err says the store lacks the content, a tag, a
// layer or its full size, as opposed to failing to read content it may have.
func contentLost(err error) bool {
	return errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrLayerMissing) || errors.Is(err, store.ErrLayerSize)
}

// requeueMissing puts e's ready cache row back to wanted, because its content
// failed cause in the store, and reports ErrUtilityMissing to the error handler
// with the failure of the requeue, if any.
func (h *Handlers) requeueMissing(ctx context.Context, e catalog.Entry, cause error, at time.Time) {
	_, err := h.repo.Requeue(ctx, fleet.CacheKey{Entry: e.Entry, Arch: fleet.Arch(e.Arch)}, e.Pin.ID, at)
	h.onError(ctx, errors.Join(fmt.Errorf("%w: %s/%s: %w", ErrUtilityMissing, e.Entry, e.Arch, cause), err))
}

// reportUnreadable reports to the error handler that the store failed to read
// e's content for a reason other than lacking it. The cache row is not touched:
// a permission or I/O failure is not a reason to fetch the content again.
func (h *Handlers) reportUnreadable(ctx context.Context, e catalog.Entry, err error) {
	h.onError(ctx, fmt.Errorf("boot: read %s/%s content: %w", e.Entry, e.Arch, err))
}
