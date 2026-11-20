package fleet

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/uptrace/bun"
)

const (
	actionBootNextCleared = "boot_next.cleared"
	actionUtilityReady    = "utility.ready"
	actionUtilityBroken   = "utility.broken"

	// bootNextPriority is the priority of a row a boot-next names.
	bootNextPriority = 1
)

// errCacheChanged means a cache row moved under a transaction that expected it
// where it loaded it, and the transaction rolls back. RecordFetchFailure turns
// it into a no-op, the failure not being the row's; RequestFetch returns it as
// an error, which the CLI can report.
var errCacheChanged = errors.New("cache row changed under the transaction")

// cacheDetail is the detail of a utility.ready audit event.
type cacheDetail struct {
	Entry string `json:"entry"`
	Arch  Arch   `json:"arch"`
	Pin   string `json:"pin"`
}

// brokenDetail is the detail of a utility.broken audit event.
type brokenDetail struct {
	cacheDetail

	Error string `json:"error"`
}

// clearDetail is the detail of a boot_next.cleared audit event.
type clearDetail struct {
	Entry  string      `json:"entry"`
	SetAt  string      `json:"set_at"`
	Reason ClearReason `json:"reason"`
}

// stamp is how every time is written: UTC, to the microsecond Postgres keeps.
func stamp(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// utc puts the row's times in UTC.
func (c CacheRow) utc() CacheRow {
	c.NextAttemptAt, c.UpdatedAt = c.NextAttemptAt.UTC(), c.UpdatedAt.UTC()
	return c
}

// RequestFetch is an admin asking for the row at key to be fetched at pin, in a
// single transaction. A missing row is created wanted at priority. A failed or broken
// row at pin is re-armed: wanted, attempts reset, its priority raised to
// priority, with the cleared last_error returned as PreviousError. A wanted,
// fetching or ready row at pin keeps its state and has its priority raised to
// priority. A row at another pin is left alone.
func (r *Repository) RequestFetch(ctx context.Context, key CacheKey, pin string, priority int, at time.Time) (FetchRequest, error) {
	var out FetchRequest
	err := r.bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		out, err = requestFetch(ctx, tx, FetchWant{Key: key, Pin: pin}, priority, at)
		return err
	})
	if err != nil {
		return FetchRequest{}, fmt.Errorf("fleet: request fetch %s/%s: %w", key.Entry, key.Arch, err)
	}
	return out, nil
}

// requestFetch is RequestFetch's transaction body, which SetBootNext runs in its own.
func requestFetch(ctx context.Context, q bun.IDB, w FetchWant, priority int, at time.Time) (FetchRequest, error) {
	at = stamp(at)
	if err := insertWanted(ctx, q, w, priority, at); err != nil {
		return FetchRequest{}, err
	}
	// Lock the row before reading its state, so the state this decides on is the
	// one it writes over (a no-op write, as SQLite has no FOR UPDATE). Machines
	// were locked first by the caller, the order every transaction takes.
	if _, err := updateCache(q, w.Key, w.Pin).Set("state = state").Exec(ctx); err != nil {
		return FetchRequest{}, fmt.Errorf("lock cache row %s/%s: %w", w.Key.Entry, w.Key.Arch, err)
	}
	row, err := mustLoadCacheRow(ctx, q, w.Key)
	if err != nil {
		return FetchRequest{}, err
	}
	req := FetchRequest{Row: row}
	if row.Pin != w.Pin {
		return req, nil
	}

	switch row.State {
	case CacheFailed, CacheBroken:
		req.Rearmed, err = applied(ctx, updateCache(q, w.Key, w.Pin).Where("state = ?", row.State).
			Set("state = ?", CacheWanted).Set("attempts = 0").Set("next_attempt_at = NULL").Set("last_error = ''").
			Set("priority = CASE WHEN priority < ? THEN ? ELSE priority END", priority, priority).
			Set("updated_at = ?", at))
		if req.Rearmed {
			req.PreviousError = row.LastError
		}
	case CacheWanted, CacheFetching, CacheReady:
		// Only the priority moves, so updated_at, the queue order among equals, stays.
		_, err = updateCache(q, w.Key, w.Pin).Where("state = ?", row.State).Where("priority < ?", priority).Set("priority = ?", priority).Exec(ctx)
	}
	if err != nil {
		return FetchRequest{}, fmt.Errorf("request %s/%s: %w", w.Key.Entry, w.Key.Arch, err)
	}
	if req.Row, err = mustLoadCacheRow(ctx, q, w.Key); err != nil {
		return FetchRequest{}, err
	}
	return req, nil
}

// wantFetch is row 3 of the boot decision wanting the entry: a missing row is
// created wanted at priority 1, and a wanted row at the pin is raised to 1. Every other
// state is left alone, so a held machine re-chaining every 30 s never resets
// backoff or re-arms a broken row.
func wantFetch(ctx context.Context, q bun.IDB, w FetchWant, at time.Time) error {
	if err := insertWanted(ctx, q, w, bootNextPriority, stamp(at)); err != nil {
		return err
	}
	_, err := updateCache(q, w.Key, w.Pin).Where("state = ?", CacheWanted).Where("priority < ?", bootNextPriority).
		Set("priority = ?", bootNextPriority).Exec(ctx)
	if err != nil {
		return fmt.Errorf("want %s/%s: %w", w.Key.Entry, w.Key.Arch, err)
	}
	return nil
}

// ClaimFetch moves the next row the fetcher should work on to fetching and
// returns it: the wanted rows and the failed rows due at at, highest priority
// first, then the oldest updated_at. A row another worker takes first is skipped
// for the next. It reports false when none is eligible.
func (r *Repository) ClaimFetch(ctx context.Context, at time.Time) (CacheRow, bool, error) {
	row, ok, err := r.claimFetchInTx(ctx, nil, at)
	if err != nil {
		return CacheRow{}, false, fmt.Errorf("fleet: claim fetch: %w", err)
	}
	return row, ok, nil
}

// ClaimFetchAtPriority is ClaimFetch restricted to the rows at priority, so a
// fetcher can put other work between the priorities.
func (r *Repository) ClaimFetchAtPriority(ctx context.Context, priority int, at time.Time) (CacheRow, bool, error) {
	row, ok, err := r.claimFetchInTx(ctx, &priority, at)
	if err != nil {
		return CacheRow{}, false, fmt.Errorf("fleet: claim fetch at priority %d: %w", priority, err)
	}
	return row, ok, nil
}

// claimFetchInTx runs claimFetch in a transaction of its own.
func (r *Repository) claimFetchInTx(ctx context.Context, priority *int, at time.Time) (CacheRow, bool, error) {
	var (
		claimed CacheRow
		ok      bool
	)
	err := r.bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		claimed, ok, err = claimFetch(ctx, tx, stamp(at), priority, nil)
		return err
	})
	return claimed, ok, err
}

// claimFetch is ClaimFetch's transaction body, over the rows at priority when it
// is set and every row otherwise. afterSelect, when set, runs between the select
// of the candidates and the update of the first: tests land another worker's
// claim there, a race only Postgres lets happen on its own.
func claimFetch(ctx context.Context, tx bun.IDB, at time.Time, priority *int, afterSelect func()) (CacheRow, bool, error) {
	var rows []CacheRow
	q := tx.NewSelect().Model(&rows).Where("state = ? OR (state = ? AND next_attempt_at <= ?)", CacheWanted, CacheFailed, at)
	if priority != nil {
		q = q.Where("priority = ?", *priority)
	}
	err := q.Scan(ctx)
	if err != nil {
		return CacheRow{}, false, fmt.Errorf("load claimable rows: %w", err)
	}
	if len(rows) == 0 {
		return CacheRow{}, false, nil
	}
	if afterSelect != nil {
		afterSelect()
	}
	slices.SortFunc(rows, func(a, b CacheRow) int {
		return cmp.Or(cmp.Compare(b.Priority, a.Priority), a.UpdatedAt.Compare(b.UpdatedAt), cmp.Compare(a.Entry, b.Entry), cmp.Compare(a.Arch, b.Arch))
	})

	// Another worker may take a candidate first, or move it on (fail it into a new
	// backoff): the update that moves nothing means the candidate is no longer
	// claimable as selected, and the next is tried. The claimed row is read back
	// from the update, so it is the row as stored, not as the select saw it.
	for i := range rows {
		c := &rows[i]
		up := updateCache(tx, c.Key(), c.Pin).Where("state = ?", c.State)
		if c.State == CacheFailed {
			up = up.Where("next_attempt_at <= ?", at)
		}
		var claimed CacheRow
		err := up.Set("state = ?", CacheFetching).Set("updated_at = ?", at).Returning("*").Scan(ctx, &claimed)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return CacheRow{}, false, wrapIf("claim "+c.Entry+"/"+string(c.Arch), err)
		}
		return claimed.utc(), true, nil
	}
	return CacheRow{}, false, nil
}

// RecordProgress stores how much of the object being fetched is downloaded. It
// touches only a fetching row at p.Pin. It is not a state change, so it is not
// audited and leaves updated_at, the time of the last state change, alone.
func (r *Repository) RecordProgress(ctx context.Context, p FetchProgress) error {
	_, err := updateCache(r.bdb, p.Key, p.Pin).Where("state = ?", CacheFetching).
		Set("bytes_done = ?", p.Done).Set("bytes_total = ?", p.Total).Exec(ctx)
	if err != nil {
		return fmt.Errorf("fleet: record progress %s/%s: %w", p.Key.Entry, p.Key.Arch, err)
	}
	return nil
}

// MarkReady moves a fetching row at pin to ready, clears its failure history
// and audits utility.ready, in a single transaction. It reports whether it did.
func (r *Repository) MarkReady(ctx context.Context, key CacheKey, pin string, at time.Time) (bool, error) {
	return r.markReady(ctx, FetchWant{Key: key, Pin: pin}, at, CacheFetching)
}

// RestoreReady is MarkReady for a row reconciliation found complete in the
// store: a wanted or failed row at pin becomes ready. A broken row stays broken.
func (r *Repository) RestoreReady(ctx context.Context, key CacheKey, pin string, at time.Time) (bool, error) {
	return r.markReady(ctx, FetchWant{Key: key, Pin: pin}, at, CacheWanted, CacheFailed)
}

func (r *Repository) markReady(ctx context.Context, w FetchWant, at time.Time, from ...CacheState) (bool, error) {
	var moved bool
	err := r.bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		moved, err = markReady(ctx, tx, w, stamp(at), from)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("fleet: mark %s/%s ready: %w", w.Key.Entry, w.Key.Arch, err)
	}
	return moved, nil
}

func markReady(ctx context.Context, tx bun.IDB, w FetchWant, at time.Time, from []CacheState) (bool, error) {
	moved, err := applied(ctx, updateCache(tx, w.Key, w.Pin).Where("state IN (?)", bun.List(from)).
		Set("state = ?", CacheReady).Set("attempts = 0").Set("last_error = ''").Set("next_attempt_at = NULL").Set("updated_at = ?", at))
	if err != nil || !moved {
		return false, err
	}
	audit := auditEventRow{At: at, Actor: ActorSystem, Action: actionUtilityReady}
	if err := insertAudit(ctx, tx, audit, cacheDetail{Entry: w.Key.Entry, Arch: w.Key.Arch, Pin: w.Pin}); err != nil {
		return false, err
	}
	return true, nil
}

// RecordFetchFailure records a failed fetch of the fetching row at f.Pin and
// returns the row as stored; any other row is returned unchanged. It adds an
// attempt and the cause, stored clean and cut to MaxCauseBytes: its text comes
// from the network, and a cause the database refused would leave the attempt
// unrecorded. An integrity failure, or the MaxFetchAttempts-th
// attempt, breaks the row: it audits utility.broken and clears every boot-next
// that names the row, machines first. Otherwise the row is failed, due again
// after Backoff.
func (r *Repository) RecordFetchFailure(ctx context.Context, f FetchFailure) (CacheRow, error) {
	f.Cause = storedCause(f.Cause)
	var out CacheRow
	err := r.bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		out, err = recordFetchFailure(ctx, tx, f)
		return err
	})
	if errors.Is(err, errCacheChanged) {
		// Another process moved the row first: the failure is not this row's.
		out, _, err = loadCacheRow(ctx, r.bdb, f.Key)
	}
	if err != nil {
		return CacheRow{}, fmt.Errorf("fleet: record fetch failure %s/%s: %w", f.Key.Entry, f.Key.Arch, err)
	}
	return out, nil
}

func recordFetchFailure(ctx context.Context, tx bun.IDB, f FetchFailure) (CacheRow, error) {
	row, ok, err := loadCacheRow(ctx, tx, f.Key)
	if err != nil || !ok || row.State != CacheFetching || row.Pin != f.Pin {
		return row, err
	}
	at := stamp(f.At)
	attempts := row.Attempts + 1
	broken := f.Integrity || attempts >= MaxFetchAttempts

	up := updateCache(tx, f.Key, f.Pin).Where("state = ?", CacheFetching).
		Set("attempts = ?", attempts).Set("last_error = ?", f.Cause).Set("updated_at = ?", at)
	if broken {
		// Machines before the cache row, the order every transaction that touches both takes.
		if err := clearBrokenFlags(ctx, tx, f.Key, at); err != nil {
			return CacheRow{}, err
		}
		up = up.Set("state = ?", CacheBroken).Set("next_attempt_at = NULL")
	} else {
		up = up.Set("state = ?", CacheFailed).Set("next_attempt_at = ?", at.Add(Backoff(attempts)))
	}
	moved, err := applied(ctx, up)
	if err != nil {
		return CacheRow{}, err
	}
	if !moved {
		return CacheRow{}, errCacheChanged
	}

	if broken {
		audit := auditEventRow{At: at, Actor: ActorSystem, Action: actionUtilityBroken}
		detail := brokenDetail{cacheDetail: cacheDetail{Entry: f.Key.Entry, Arch: f.Key.Arch, Pin: f.Pin}, Error: f.Cause}
		if err := insertAudit(ctx, tx, audit, detail); err != nil {
			return CacheRow{}, err
		}
	}
	return mustLoadCacheRow(ctx, tx, f.Key)
}

// clearBrokenFlags compare-and-clears the boot-next of every machine of key's
// architecture that names its entry.
func clearBrokenFlags(ctx context.Context, tx bun.IDB, key CacheKey, at time.Time) error {
	var flagged []Machine
	err := tx.NewSelect().Model(&flagged).Column("id", "boot_next_set_at").
		Where("boot_next = ? AND arch = ?", key.Entry, key.Arch).Order("id").Scan(ctx)
	if err != nil {
		return fmt.Errorf("load machines flagged %s: %w", key.Entry, err)
	}
	for i := range flagged {
		req := ClearBootNextRequest{
			MachineID: flagged[i].ID, Entry: key.Entry, SetAt: flagged[i].BootNextSetAt, Reason: ClearBroken, Actor: ActorSystem, At: at,
		}
		if _, err := clearBootNext(ctx, tx, req); err != nil {
			return err
		}
	}
	return nil
}

// clearBootNext clears the machine's boot-next if it still names req.Entry and
// was set at req.SetAt, and audits boot_next.cleared; a flag set again since
// the caller read it is not the caller's to clear. It reports whether it did.
func clearBootNext(ctx context.Context, q bun.IDB, req ClearBootNextRequest) (bool, error) {
	cleared, err := applied(ctx, q.NewUpdate().Model((*Machine)(nil)).Set("boot_next = NULL").Set("boot_next_set_at = NULL").Set("boot_next_mac = NULL").
		Where("id = ? AND boot_next = ? AND boot_next_set_at = ?", req.MachineID, req.Entry, req.SetAt.UTC()))
	if err != nil || !cleared {
		return false, wrapIf("clear boot_next of "+req.MachineID, err)
	}
	audit := auditEventRow{At: stamp(req.At), Actor: req.Actor, Action: actionBootNextCleared, MachineID: req.MachineID}
	detail := clearDetail{Entry: req.Entry, SetAt: req.SetAt.UTC().Format(time.RFC3339Nano), Reason: req.Reason}
	if err := insertAudit(ctx, q, audit, detail); err != nil {
		return false, err
	}
	return true, nil
}

// Requeue moves a ready row at pin back to wanted, when its content is found
// missing. It reports whether it did.
func (r *Repository) Requeue(ctx context.Context, key CacheKey, pin string, at time.Time) (bool, error) {
	moved, err := applied(ctx, updateCache(r.bdb, key, pin).Where("state = ?", CacheReady).
		Set("state = ?", CacheWanted).Set("updated_at = ?", stamp(at)))
	if err != nil {
		return false, fmt.Errorf("fleet: requeue %s/%s: %w", key.Entry, key.Arch, err)
	}
	return moved, nil
}

// ResetFetching moves every fetching row back to wanted, as when the process
// that was fetching them is gone, and returns how many it moved.
func (r *Repository) ResetFetching(ctx context.Context, at time.Time) (int, error) {
	res, err := r.bdb.NewUpdate().Model((*CacheRow)(nil)).
		Set("state = ?", CacheWanted).Set("updated_at = ?", stamp(at)).Where("state = ?", CacheFetching).Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("fleet: reset fetching: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("fleet: reset fetching: %w", err)
	}
	return int(n), nil
}

// ResetFetchingRow moves the fetching row at key and pin back to wanted, when the
// fetch that held it ends without an outcome to record. Unlike ResetFetching it
// leaves every other fetching row to its own worker. It reports whether it did.
func (r *Repository) ResetFetchingRow(ctx context.Context, key CacheKey, pin string, at time.Time) (bool, error) {
	moved, err := applied(ctx, updateCache(r.bdb, key, pin).Where("state = ?", CacheFetching).
		Set("state = ?", CacheWanted).Set("updated_at = ?", stamp(at)))
	if err != nil {
		return false, fmt.Errorf("fleet: reset fetching %s/%s: %w", key.Entry, key.Arch, err)
	}
	return moved, nil
}

// Repin moves a row at oldPin, in any state, to wanted at newPin with its
// attempts, progress and last_error reset. It reports whether it did.
func (r *Repository) Repin(ctx context.Context, key CacheKey, oldPin, newPin string, at time.Time) (bool, error) {
	moved, err := applied(ctx, updateCache(r.bdb, key, oldPin).
		Set("pin = ?", newPin).Set("state = ?", CacheWanted).Set("attempts = 0").Set("next_attempt_at = NULL").
		Set("bytes_done = 0").Set("bytes_total = 0").Set("last_error = ''").Set("updated_at = ?", stamp(at)))
	if err != nil {
		return false, fmt.Errorf("fleet: repin %s/%s: %w", key.Entry, key.Arch, err)
	}
	return moved, nil
}

// DeleteCacheRow removes the row at key; a missing row is not an error.
func (r *Repository) DeleteCacheRow(ctx context.Context, key CacheKey) error {
	_, err := r.bdb.NewDelete().Model((*CacheRow)(nil)).Where("entry = ? AND arch = ?", key.Entry, key.Arch).Exec(ctx)
	if err != nil {
		return fmt.Errorf("fleet: delete cache row %s/%s: %w", key.Entry, key.Arch, err)
	}
	return nil
}

// CacheRow returns the row at key, and false when there is none.
func (r *Repository) CacheRow(ctx context.Context, key CacheKey) (CacheRow, bool, error) {
	row, ok, err := loadCacheRow(ctx, r.bdb, key)
	if err != nil {
		return CacheRow{}, false, fmt.Errorf("fleet: cache row: %w", err)
	}
	return row, ok, nil
}

// CacheRows returns every row, ordered by entry and architecture.
func (r *Repository) CacheRows(ctx context.Context) ([]CacheRow, error) {
	var rows []CacheRow
	if err := r.bdb.NewSelect().Model(&rows).Order("entry", "arch").Scan(ctx); err != nil {
		return nil, fmt.Errorf("fleet: cache rows: %w", err)
	}
	for i := range rows {
		rows[i] = rows[i].utc()
	}
	return rows, nil
}

// insertWanted creates the row wanted at w.Pin unless one exists, whatever its
// state or pin.
func insertWanted(ctx context.Context, q bun.IDB, w FetchWant, priority int, at time.Time) error {
	row := CacheRow{Entry: w.Key.Entry, Arch: w.Key.Arch, Pin: w.Pin, State: CacheWanted, Priority: priority, UpdatedAt: at}
	if _, err := q.NewInsert().Model(&row).On("CONFLICT (entry, arch) DO NOTHING").Exec(ctx); err != nil {
		return fmt.Errorf("insert cache row %s/%s: %w", w.Key.Entry, w.Key.Arch, err)
	}
	return nil
}

func loadCacheRow(ctx context.Context, q bun.IDB, key CacheKey) (CacheRow, bool, error) {
	var row CacheRow
	err := q.NewSelect().Model(&row).Where("entry = ? AND arch = ?", key.Entry, key.Arch).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return CacheRow{}, false, nil
	}
	if err != nil {
		return CacheRow{}, false, fmt.Errorf("load cache row %s/%s: %w", key.Entry, key.Arch, err)
	}
	return row.utc(), true, nil
}

// mustLoadCacheRow loads a row the transaction just wrote or found; a row that
// is gone was deleted by another process.
func mustLoadCacheRow(ctx context.Context, q bun.IDB, key CacheKey) (CacheRow, error) {
	row, ok, err := loadCacheRow(ctx, q, key)
	if err == nil && !ok {
		err = fmt.Errorf("%w: %s/%s is gone", errCacheChanged, key.Entry, key.Arch)
	}
	return row, err
}

// updateCache starts an update of the row at key and pin; the caller adds the
// state it expects and what to set.
func updateCache(q bun.IDB, key CacheKey, pin string) *bun.UpdateQuery {
	return q.NewUpdate().Model((*CacheRow)(nil)).Where("entry = ? AND arch = ? AND pin = ?", key.Entry, key.Arch, pin)
}

// applied runs q and reports whether it changed a row.
func applied(ctx context.Context, q *bun.UpdateQuery) (bool, error) {
	res, err := q.Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// wrapIf wraps err with what was being done; it is nil for a nil err.
func wrapIf(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}

// storedCause is cause as a row keeps it: printable text of at most MaxCauseBytes,
// cut on a rune boundary.
func storedCause(cause string) string {
	cause = PrintableText(cause)
	if len(cause) <= MaxCauseBytes {
		return cause
	}
	n := MaxCauseBytes
	for !utf8.RuneStart(cause[n]) {
		n--
	}
	return cause[:n]
}
