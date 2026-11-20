package fleet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"go.jacaudi.dev/hephaestus/internal/db"
)

const (
	actionTalosBroken    = "talos.broken"
	actionTalosHashReset = "talos.hash_reset"
)

// talosDetail is the detail of a talos.hash_reset audit event.
type talosDetail struct {
	Schematic string `json:"schematic"`
	Version   string `json:"version"`
	Arch      Arch   `json:"arch"`
}

// talosFailureDetail is the detail of a talos.broken audit event.
type talosFailureDetail struct {
	talosDetail

	Cause string `json:"cause"`
}

// utc puts the row's times in UTC.
func (r TalosRow) utc() TalosRow {
	r.NextAttemptAt, r.UpdatedAt = r.NextAttemptAt.UTC(), r.UpdatedAt.UTC()
	return r
}

// WantTalosTx wants the row at w.Key in the caller's transaction q: an absent row
// is created wanted with w.Body, and a row without a body takes w.Body when it
// has one. With w.Rearm a failed or broken row moves to wanted with its attempts
// reset and the assets it recorded kept. It returns the row as stored.
func WantTalosTx(ctx context.Context, q bun.IDB, w TalosWant, at time.Time) (TalosRow, error) {
	at = stamp(at)
	row := TalosRow{
		Schematic: w.Key.Schematic, Version: w.Key.Version, Arch: w.Key.Arch,
		State: CacheWanted, Body: w.Body, Assets: TalosAssets{}, UpdatedAt: at,
	}
	if _, err := q.NewInsert().Model(&row).On("CONFLICT (schematic, version, arch) DO NOTHING").Exec(ctx); err != nil {
		return TalosRow{}, fmt.Errorf("insert Talos row %s: %w", w.Key.Tag(), err)
	}
	if w.Body != nil {
		if _, err := updateTalos(q, w.Key).Where("body IS NULL").Set("body = ?", w.Body).Exec(ctx); err != nil {
			return TalosRow{}, fmt.Errorf("set body of Talos row %s: %w", w.Key.Tag(), err)
		}
	}
	if w.Rearm {
		if _, err := rearmTalosTx(ctx, q, w.Key, at); err != nil {
			return TalosRow{}, err
		}
	}
	return mustLoadTalos(ctx, q, w.Key)
}

// RearmTalos moves a failed or broken row at key to wanted, as a machine
// entering assigned does. It never creates a row, and reports whether it moved one.
func (r *Repository) RearmTalos(ctx context.Context, key TalosKey, at time.Time) (bool, error) {
	moved, err := rearmTalosTx(ctx, r.bdb, key, at)
	if err != nil {
		return false, fmt.Errorf("fleet: %w", err)
	}
	return moved, nil
}

// rearmTalosTx moves a failed or broken row at key to wanted with its attempts
// and last error reset and its recorded assets kept. It reports whether it did.
func rearmTalosTx(ctx context.Context, q bun.IDB, key TalosKey, at time.Time) (bool, error) {
	moved, err := applied(ctx, updateTalos(q, key).Where("state IN (?)", bun.List([]CacheState{CacheFailed, CacheBroken})).
		Set("state = ?", CacheWanted).Set("attempts = 0").Set("next_attempt_at = NULL").Set("last_error = ''").Set("updated_at = ?", stamp(at)))
	if err != nil {
		return false, fmt.Errorf("rearm Talos row %s: %w", key.Tag(), err)
	}
	return moved, nil
}

// rearmTalosOf re-arms the row of m's Talos tuple and arch, when m has a tuple.
func rearmTalosOf(ctx context.Context, q bun.IDB, m Machine, at time.Time) error {
	if m.TalosSchematic == nil || m.TalosVersion == nil {
		return nil
	}
	_, err := rearmTalosTx(ctx, q, TalosKey{Schematic: *m.TalosSchematic, Version: *m.TalosVersion, Arch: m.Arch}, at)
	return err
}

// ClaimTalos moves the next row of class the fetcher should work on to fetching
// and returns it: the wanted rows and the failed rows due at at, oldest
// updated_at first. TalosAssigned takes the rows whose key is the Talos tuple and
// arch of a machine in assigned, and TalosOther the rest. A row another worker
// takes first is skipped for the next. It reports false when none is eligible.
func (r *Repository) ClaimTalos(ctx context.Context, class TalosClass, at time.Time) (TalosRow, bool, error) {
	at = stamp(at)
	assigned := r.bdb.NewSelect().TableExpr("machines AS m").ColumnExpr("1").
		Where("m.state = ?", StateAssigned).
		Where("m.talos_schematic = talos_row.schematic AND m.talos_version = talos_row.version AND m.arch = talos_row.arch")
	exists := "EXISTS (?)"
	if class == TalosOther {
		exists = "NOT EXISTS (?)"
	}
	var rows []TalosRow
	err := r.bdb.NewSelect().Model(&rows).
		Where("state = ? OR (state = ? AND next_attempt_at <= ?)", CacheWanted, CacheFailed, at).
		Where(exists, assigned).
		Order("updated_at", "schematic", "version", "arch").Scan(ctx)
	if err != nil {
		return TalosRow{}, false, fmt.Errorf("fleet: claim Talos: load claimable rows: %w", err)
	}

	// Another worker may take a candidate first, or move it on: the update that
	// moves nothing means the candidate is no longer claimable as selected, and
	// the next is tried. The claimed row is read back from the update.
	for i := range rows {
		c := &rows[i]
		up := updateTalos(r.bdb, c.Key()).Where("state = ?", c.State)
		if c.State == CacheFailed {
			up = up.Where("next_attempt_at <= ?", at)
		}
		var claimed TalosRow
		err := up.Set("state = ?", CacheFetching).Set("updated_at = ?", at).Returning("*").Scan(ctx, &claimed)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return TalosRow{}, false, fmt.Errorf("fleet: claim Talos row %s: %w", c.Key().Tag(), err)
		}
		return claimed.utc(), true, nil
	}
	return TalosRow{}, false, nil
}

// RecordTalosProgress stores how much of the asset being fetched is downloaded.
// It touches only a fetching row. It is not a state change, so it is not audited
// and leaves updated_at alone.
func (r *Repository) RecordTalosProgress(ctx context.Context, key TalosKey, done, total int64) error {
	_, err := updateTalos(r.bdb, key).Where("state = ?", CacheFetching).
		Set("bytes_done = ?", done).Set("bytes_total = ?", total).Exec(ctx)
	if err != nil {
		return fmt.Errorf("fleet: record Talos progress %s: %w", key.Tag(), err)
	}
	return nil
}

// MarkTalosReady moves a fetching row to ready with the assets its fetch
// recorded, and clears its failure history. It reports whether it did.
func (r *Repository) MarkTalosReady(ctx context.Context, key TalosKey, assets TalosAssets, at time.Time) (bool, error) {
	moved, err := applied(ctx, updateTalos(r.bdb, key).Where("state = ?", CacheFetching).
		Set("state = ?", CacheReady).Set("assets = ?", assets).
		Set("attempts = 0").Set("last_error = ''").Set("next_attempt_at = NULL").Set("updated_at = ?", stamp(at)))
	if err != nil {
		return false, fmt.Errorf("fleet: mark Talos %s ready: %w", key.Tag(), err)
	}
	return moved, nil
}

// RecordTalosFailure records a failed fetch of the fetching row at f.Key and
// returns the row as stored; any other row is returned unchanged, and an absent
// one as the zero row. It adds an attempt and the cause, stored clean and cut to
// MaxCauseBytes. An integrity failure, or the MaxFetchAttempts-th attempt,
// breaks the row and audits talos.broken; otherwise the row is failed, due
// again after Backoff.
func (r *Repository) RecordTalosFailure(ctx context.Context, f TalosFailure) (TalosRow, error) {
	f.Cause = storedCause(f.Cause)
	var out TalosRow
	err := db.RunInTxRetry(ctx, r.bdb, func(ctx context.Context, tx bun.Tx) error {
		var err error
		out, err = recordTalosFailure(ctx, tx, f)
		return err
	})
	if err != nil {
		return TalosRow{}, fmt.Errorf("fleet: record Talos failure %s: %w", f.Key.Tag(), err)
	}
	return out, nil
}

func recordTalosFailure(ctx context.Context, tx bun.IDB, f TalosFailure) (TalosRow, error) {
	row, ok, err := loadTalos(ctx, tx, f.Key)
	if err != nil || !ok || row.State != CacheFetching {
		return row, err
	}
	at := stamp(f.At)
	attempts := row.Attempts + 1
	broken := f.Integrity || attempts >= MaxFetchAttempts

	up := updateTalos(tx, f.Key).Where("state = ? AND attempts = ?", CacheFetching, row.Attempts).
		Set("attempts = ?", attempts).Set("last_error = ?", f.Cause).Set("updated_at = ?", at)
	if broken {
		up = up.Set("state = ?", CacheBroken).Set("next_attempt_at = NULL")
	} else {
		up = up.Set("state = ?", CacheFailed).Set("next_attempt_at = ?", at.Add(Backoff(attempts)))
	}
	moved, err := applied(ctx, up)
	if err != nil {
		return TalosRow{}, fmt.Errorf("record failure of %s: %w", f.Key.Tag(), err)
	}
	if !moved {
		return TalosRow{}, fmt.Errorf("record failure of %s: %w", f.Key.Tag(), db.ErrConflict)
	}
	if broken {
		ev := AuditEvent{At: at, Actor: ActorSystem, Action: actionTalosBroken, Detail: talosFailureDetail{talosDetail: talosDetail(f.Key), Cause: f.Cause}}
		if err := InsertAudit(ctx, tx, ev); err != nil {
			return TalosRow{}, err
		}
	}
	return mustLoadTalos(ctx, tx, f.Key)
}

// ResetTalosFetching moves every fetching row back to wanted, as when the
// process that was fetching them is gone, and returns how many it moved.
func (r *Repository) ResetTalosFetching(ctx context.Context, at time.Time) (int, error) {
	res, err := r.bdb.NewUpdate().Model((*TalosRow)(nil)).
		Set("state = ?", CacheWanted).Set("updated_at = ?", stamp(at)).Where("state = ?", CacheFetching).Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("fleet: reset fetching Talos rows: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("fleet: reset fetching Talos rows: %w", err)
	}
	return int(n), nil
}

// ResetTalosFetchingRow moves the fetching row at key back to wanted, when the
// fetch that held it ends without an outcome to record; every other fetching row
// is left to its own worker. It reports whether it did.
func (r *Repository) ResetTalosFetchingRow(ctx context.Context, key TalosKey, at time.Time) (bool, error) {
	moved, err := applied(ctx, updateTalos(r.bdb, key).Where("state = ?", CacheFetching).
		Set("state = ?", CacheWanted).Set("updated_at = ?", stamp(at)))
	if err != nil {
		return false, fmt.Errorf("fleet: reset fetching Talos row %s: %w", key.Tag(), err)
	}
	return moved, nil
}

// RequeueTalos moves a ready row at key back to wanted, when its content is
// found missing. Its recorded assets are kept. It reports whether it did.
func (r *Repository) RequeueTalos(ctx context.Context, key TalosKey, at time.Time) (bool, error) {
	moved, err := applied(ctx, updateTalos(r.bdb, key).Where("state = ?", CacheReady).
		Set("state = ?", CacheWanted).Set("updated_at = ?", stamp(at)))
	if err != nil {
		return false, fmt.Errorf("fleet: requeue Talos row %s: %w", key.Tag(), err)
	}
	return moved, nil
}

// ResetTalosHash is an admin's trust decision to accept a changed upstream
// build: it moves the row at key, in any state, to wanted with its recorded
// assets, attempts, progress and last error cleared, and audits talos.hash_reset
// as actor, in a single transaction. It reports whether a row was there.
func (r *Repository) ResetTalosHash(ctx context.Context, key TalosKey, actor string, at time.Time) (bool, error) {
	at = stamp(at)
	var moved bool
	err := db.RunInTxRetry(ctx, r.bdb, func(ctx context.Context, tx bun.Tx) error {
		var err error
		moved, err = applied(ctx, updateTalos(tx, key).
			Set("state = ?", CacheWanted).Set("assets = ?", TalosAssets{}).Set("attempts = 0").Set("next_attempt_at = NULL").
			Set("bytes_done = 0").Set("bytes_total = 0").Set("last_error = ''").Set("updated_at = ?", at))
		if err != nil || !moved {
			return err
		}
		return InsertAudit(ctx, tx, AuditEvent{At: at, Actor: actor, Action: actionTalosHashReset, Detail: talosDetail(key)})
	})
	if err != nil {
		return false, fmt.Errorf("fleet: reset hash of Talos row %s: %w", key.Tag(), err)
	}
	return moved, nil
}

// Talos returns the row at key, and false when there is none.
func (r *Repository) Talos(ctx context.Context, key TalosKey) (TalosRow, bool, error) {
	row, ok, err := loadTalos(ctx, r.bdb, key)
	if err != nil {
		return TalosRow{}, false, fmt.Errorf("fleet: Talos row: %w", err)
	}
	return row, ok, nil
}

// TalosRows returns every row, ordered by schematic, version and architecture.
func (r *Repository) TalosRows(ctx context.Context) ([]TalosRow, error) {
	var rows []TalosRow
	if err := r.bdb.NewSelect().Model(&rows).Order("schematic", "version", "arch").Scan(ctx); err != nil {
		return nil, fmt.Errorf("fleet: Talos rows: %w", err)
	}
	for i := range rows {
		rows[i] = rows[i].utc()
	}
	return rows, nil
}

func loadTalos(ctx context.Context, q bun.IDB, key TalosKey) (TalosRow, bool, error) {
	var row TalosRow
	err := q.NewSelect().Model(&row).Where("schematic = ? AND version = ? AND arch = ?", key.Schematic, key.Version, key.Arch).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return TalosRow{}, false, nil
	}
	if err != nil {
		return TalosRow{}, false, fmt.Errorf("load Talos row %s: %w", key.Tag(), err)
	}
	return row.utc(), true, nil
}

// mustLoadTalos loads a row the transaction just wrote or found; a row that is
// gone was deleted by another process, and the transaction is worth running again.
func mustLoadTalos(ctx context.Context, q bun.IDB, key TalosKey) (TalosRow, error) {
	row, ok, err := loadTalos(ctx, q, key)
	if err == nil && !ok {
		err = fmt.Errorf("Talos row %s is gone: %w", key.Tag(), db.ErrConflict)
	}
	return row, err
}

// updateTalos starts an update of the row at key; the caller adds the state it
// expects and what to set.
func updateTalos(q bun.IDB, key TalosKey) *bun.UpdateQuery {
	return q.NewUpdate().Model((*TalosRow)(nil)).Where("schematic = ? AND version = ? AND arch = ?", key.Schematic, key.Version, key.Arch)
}
