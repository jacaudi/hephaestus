//go:build integration

package fleet

import (
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestUtilityRepository_Postgres(t *testing.T) { runUtilitySuite(t, dbtest.Postgres) }

// raceCacheRow stands in for another process: it opens a transaction that
// runs update (which must lock a cache row), starts act in a goroutine, waits
// until the database shows a statement blocked on a lock, runs the statements
// in then on the same transaction, and commits. act must not call t.Fatal; it
// records its result in variables the caller reads.
func raceCacheRow(t *testing.T, d *db.DB, update string, args []any, act func(), then ...string) {
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
	for _, stmt := range then {
		if _, err := tx.ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
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

// Another process holds a read lock on the row and then breaks it (its tenth
// failure) while SetBootNext asks for the entry. SetBootNext must lock the row
// before it reads the state, so it decides on the row the other process left,
// not on the state it read before: a broken row that an admin sets a boot-next
// for is re-armed, never left broken with the flag armed.
func TestSetBootNext_RowBrokenUnderTx_Postgres(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	k := CacheKey{Entry: "tshell", Arch: ArchAMD64}
	d := dbtest.Postgres(t)
	r := NewRepository(d)
	seed := cacheRowIn(k, CacheFetching, t0)
	seed.Attempts = 9
	seedCache(t, d, seed)
	seedFlagged(t, d, "m1", ArchAMD64, "other", t0)

	var (
		got SetBootNextResult
		err error
	)
	raceCacheRow(t, d, "SELECT 1 FROM utility_cache WHERE entry = ? AND arch = ? FOR SHARE", []any{k.Entry, string(k.Arch)}, func() {
		got, err = r.SetBootNext(t.Context(), SetBootNextRequest{MachineID: "m1", Entry: k.Entry, Pin: pinOne, At: t0.Add(time.Minute)})
	}, "UPDATE utility_cache SET state = 'broken', attempts = 10 WHERE entry = 'tshell' AND arch = 'amd64'")
	if err != nil {
		t.Fatalf("SetBootNext: %v", err)
	}
	if !got.Fetch.Rearmed {
		t.Errorf("Fetch = %+v, want the broken row re-armed", got.Fetch)
	}
	row, ok, err := r.CacheRow(t.Context(), k)
	if err != nil || !ok {
		t.Fatalf("CacheRow = %v, %v", ok, err)
	}
	if row.State != CacheWanted || row.Attempts != 0 {
		t.Errorf("row = %s with %d attempts, want wanted with none", row.State, row.Attempts)
	}
}

// claimTogether has every repository claim at once and returns what each got.
func claimTogether(t *testing.T, repos []*Repository, at time.Time) (rows []CacheRow, oks []bool) {
	t.Helper()
	rows, oks = make([]CacheRow, len(repos)), make([]bool, len(repos))
	errs := make([]error, len(repos))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, r := range repos {
		wg.Go(func() {
			<-start
			rows[i], oks[i], errs[i] = r.ClaimFetch(t.Context(), at)
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("ClaimFetch %d: %v", i, err)
		}
	}
	return rows, oks
}

// Two fetchers claim at once from one database. The one that loses the row lock
// finds its candidate taken and must take the next row, not report none. It is
// repeated so the claims overlap on some rounds.
func TestClaimFetch_ConcurrentClaims_Postgres(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	d := dbtest.Postgres(t)
	repos := []*Repository{NewRepository(d), NewRepository(d)}
	k1, k2 := CacheKey{Entry: "ta", Arch: ArchAMD64}, CacheKey{Entry: "tb", Arch: ArchAMD64}
	seedCache(t, d, cacheRowIn(k1, CacheWanted, t0))
	seedCache(t, d, cacheRowIn(k2, CacheWanted, t0))

	for round := range 25 {
		if _, err := repos[0].ResetFetching(t.Context(), t0); err != nil {
			t.Fatalf("round %d: reset: %v", round, err)
		}
		rows, oks := claimTogether(t, repos, t0.Add(time.Hour))
		if !oks[0] || !oks[1] || rows[0].Key() == rows[1].Key() {
			t.Fatalf("round %d: claims = %+v %v, want two rows with different keys", round, rows, oks)
		}
	}

	// One row, two claimers: exactly one gets it.
	if err := repos[0].DeleteCacheRow(t.Context(), k2); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for round := range 25 {
		if _, err := repos[0].ResetFetching(t.Context(), t0); err != nil {
			t.Fatalf("round %d: reset: %v", round, err)
		}
		_, oks := claimTogether(t, repos, t0.Add(time.Hour))
		if oks[0] == oks[1] {
			t.Fatalf("round %d: oks = %v, want exactly one claim of the one row", round, oks)
		}
	}
}
