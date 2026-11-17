//go:build integration

package fleet

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestUtilityRepository_Postgres(t *testing.T) { runUtilitySuite(t, dbtest.Postgres) }

// raceCacheRow stands in for another process: it opens a transaction that
// runs update (which must lock a cache row), starts act in a goroutine, waits
// until the database shows a statement blocked on a lock, then commits. act
// must not call t.Fatal; it records its result in variables the caller reads.
func raceCacheRow(t *testing.T, d *db.DB, update string, args []any, act func()) {
	t.Helper()
	tx, err := d.Bun().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback: %v", err)
		}
	})
	if _, err := tx.ExecContext(t.Context(), update, args...); err != nil {
		t.Fatalf("hold the row: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		act()
	}()
	const blocked = "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'"
	for deadline := time.Now().Add(10 * time.Second); scalar[int](t, d, blocked) == 0; {
		select {
		case <-done:
			t.Fatal("the action finished without blocking on the held row")
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the action never blocked on the held row")
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	<-done
}

// Another process moves the row to ready while RecordFetchFailure, which has
// decided the row is broken, waits for its row lock. The failure is then not
// this row's: nothing is written, and the machine keeps its boot-next.
func TestRecordFetchFailure_RowMovedUnderTx_Postgres(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	k := CacheKey{Entry: "tshell", Arch: ArchAMD64}
	d := dbtest.Postgres(t)
	r := NewRepository(d)
	seed := cacheRowIn(k, CacheFetching, t0)
	seed.Attempts = 9
	seedCache(t, d, seed)
	seedFlagged(t, d, "m1", ArchAMD64, "tshell", t0)

	var (
		got CacheRow
		err error
	)
	raceCacheRow(t, d, "UPDATE utility_cache SET state = 'ready', attempts = 0 WHERE entry = ? AND arch = ?", []any{k.Entry, string(k.Arch)}, func() {
		got, err = r.RecordFetchFailure(t.Context(), FetchFailure{Key: k, Pin: pinOne, Cause: "boom", At: t0.Add(time.Minute)})
	})
	if err != nil {
		t.Fatalf("RecordFetchFailure: %v", err)
	}
	if got.State != CacheReady || got.Attempts != 0 {
		t.Fatalf("RecordFetchFailure = %+v, want the row as the other process left it: ready, attempts 0", got)
	}
	if flagCleared(t, d, "m1") {
		t.Fatal("m1's boot_next was cleared for a break that did not happen")
	}
	if audits := auditRows(t, d); len(audits) != 0 {
		t.Fatalf("audit = %+v, want none", audits)
	}
}
