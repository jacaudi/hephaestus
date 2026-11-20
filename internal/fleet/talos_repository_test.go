package fleet

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"go.uber.org/goleak"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

const (
	talosCause = "factory: unexpected status 500"

	brokenAuditJSON    = `{"schematic":"` + plainSchematic + `","version":"` + talosVersion + `","arch":"amd64","cause":"` + talosCause + `"}`
	hashResetAuditJSON = `{"schematic":"` + plainSchematic + `","version":"` + talosVersion + `","arch":"amd64"}`
)

func TestTalosCache_SQLite(t *testing.T) { runTalosSuite(t, dbtest.SQLite, dbtest.SQLitePair) }

// talosKeyOf is the fixture schematic and version for arch.
func talosKeyOf(arch Arch) TalosKey {
	return TalosKey{Schematic: plainSchematic, Version: talosVersion, Arch: arch}
}

// talosKeyAt is the fixture schematic and arch at version.
func talosKeyAt(version string) TalosKey {
	return TalosKey{Schematic: plainSchematic, Version: version, Arch: ArchAMD64}
}

// talosAssets is the three assets a complete fetch records.
func talosAssets() TalosAssets {
	return TalosAssets{
		TalosKernel:    {Size: 11_000_000, SHA256: strings.Repeat("a", 64)},
		TalosInitramfs: {Size: 79_000_000, SHA256: strings.Repeat("b", 64)},
		TalosCmdline:   {Size: 120, SHA256: strings.Repeat("c", 64)},
	}
}

// talosRowIn is a row for key in state, last changed at updated, with no assets.
func talosRowIn(key TalosKey, state CacheState, updated time.Time) TalosRow {
	return TalosRow{Schematic: key.Schematic, Version: key.Version, Arch: key.Arch, State: state, Assets: TalosAssets{}, UpdatedAt: updated}
}

// seedTalos stores row as given, bypassing the repository.
func seedTalos(t *testing.T, d *db.DB, row TalosRow) {
	t.Helper()
	if _, err := d.Bun().NewInsert().Model(&row).Exec(t.Context()); err != nil {
		t.Fatalf("seed %+v: %v", row, err)
	}
}

func sameTalosRow(a, b TalosRow) bool {
	return a.Key() == b.Key() && a.State == b.State && bytes.Equal(a.Body, b.Body) && a.Attempts == b.Attempts &&
		a.NextAttemptAt.Equal(b.NextAttemptAt) && a.BytesDone == b.BytesDone && a.BytesTotal == b.BytesTotal &&
		a.LastError == b.LastError && maps.Equal(a.Assets, b.Assets) && a.UpdatedAt.Equal(b.UpdatedAt)
}

// wantTalos fails unless the stored row at key is want in every column.
func wantTalos(t *testing.T, r *Repository, key TalosKey, want TalosRow) {
	t.Helper()
	got, ok, err := r.Talos(t.Context(), key)
	if err != nil || !ok || !sameTalosRow(got, want) {
		t.Fatalf("row %+v = %+v, %v, %v; want %+v", key, got, ok, err, want)
	}
}

// mustWantTalos runs WantTalosTx in a transaction of its own.
func mustWantTalos(t *testing.T, d *db.DB, w TalosWant, at time.Time) TalosRow {
	t.Helper()
	var row TalosRow
	err := inTx(t.Context(), d, func(ctx context.Context, tx bun.Tx) error {
		var err error
		row, err = WantTalosTx(ctx, tx, w, at)
		return err
	})
	if err != nil {
		t.Fatalf("WantTalosTx(%+v): %v", w, err)
	}
	return row
}

func mustClaimTalos(t *testing.T, r *Repository, class TalosClass, at time.Time) (TalosRow, bool) {
	t.Helper()
	row, ok, err := r.ClaimTalos(t.Context(), class, at)
	if err != nil {
		t.Fatalf("ClaimTalos(%d): %v", class, err)
	}
	return row, ok
}

func mustFailTalos(t *testing.T, r *Repository, f TalosFailure) TalosRow {
	t.Helper()
	row, err := r.RecordTalosFailure(t.Context(), f)
	if err != nil {
		t.Fatalf("RecordTalosFailure(%+v): %v", f, err)
	}
	return row
}

// runTalosSuite is the talos_cache suite both databases run: TestTalosCache_SQLite
// untagged, TestTalosCache_Postgres under the integration tag. pair opens two
// handles on one database.
func runTalosSuite(t *testing.T, open func(testing.TB) *db.DB, pair func(testing.TB) (*db.DB, *db.DB)) {
	t.Helper()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	k := talosKeyOf(ArchAMD64)
	body := []byte(`{"customization":{}}`)

	t.Run("WantCreates", func(t *testing.T) {
		d := open(t)
		got := mustWantTalos(t, d, TalosWant{Key: k, Body: body}, t0)
		want := talosRowIn(k, CacheWanted, t0)
		want.Body = body
		if !sameTalosRow(got, want) {
			t.Fatalf("WantTalosTx = %+v, want %+v", got, want)
		}
		wantTalos(t, NewRepository(d), k, want)
	})

	t.Run("WantSetsMissingBody", func(t *testing.T) {
		d := open(t)
		seed := talosRowIn(k, CacheReady, t0)
		seed.Assets = talosAssets()
		seedTalos(t, d, seed)
		got := mustWantTalos(t, d, TalosWant{Key: k, Body: body}, t0.Add(time.Minute))
		want := seed
		want.Body = body
		if !sameTalosRow(got, want) {
			t.Fatalf("WantTalosTx = %+v, want the body set and nothing else changed: %+v", got, want)
		}
		wantTalos(t, NewRepository(d), k, want)
	})

	t.Run("WantKeepsExistingBody", func(t *testing.T) {
		d := open(t)
		seed := talosRowIn(k, CacheWanted, t0)
		seed.Body = []byte("first")
		seedTalos(t, d, seed)
		mustWantTalos(t, d, TalosWant{Key: k, Body: body}, t0.Add(time.Minute))
		wantTalos(t, NewRepository(d), k, seed)
	})

	t.Run("WantRearmsBroken", func(t *testing.T) {
		d := open(t)
		seed := talosRowIn(k, CacheBroken, t0)
		seed.Attempts, seed.LastError, seed.Assets = MaxFetchAttempts, talosCause, talosAssets()
		seedTalos(t, d, seed)
		at := t0.Add(time.Minute)
		got := mustWantTalos(t, d, TalosWant{Key: k, Rearm: true}, at)
		want := talosRowIn(k, CacheWanted, at)
		want.Assets = talosAssets()
		if !sameTalosRow(got, want) {
			t.Fatalf("WantTalosTx = %+v, want wanted with attempts 0 and the assets kept: %+v", got, want)
		}
		wantTalos(t, NewRepository(d), k, want)
	})

	t.Run("WantWithoutRearmKeepsBroken", func(t *testing.T) {
		d := open(t)
		seed := talosRowIn(k, CacheBroken, t0)
		seed.Attempts, seed.LastError = MaxFetchAttempts, talosCause
		seedTalos(t, d, seed)
		mustWantTalos(t, d, TalosWant{Key: k}, t0.Add(time.Minute))
		wantTalos(t, NewRepository(d), k, seed)
	})

	t.Run("RearmTalos", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		failed := talosRowIn(talosKeyAt("v1.12.9"), CacheFailed, t0)
		failed.Attempts, failed.NextAttemptAt, failed.LastError = 3, t0.Add(time.Hour), talosCause
		ready := talosRowIn(talosKeyAt("v1.12.10"), CacheReady, t0)
		ready.Assets = talosAssets()
		seedTalos(t, d, failed)
		seedTalos(t, d, ready)

		at := t0.Add(time.Minute)
		moved, err := r.RearmTalos(t.Context(), failed.Key(), at)
		wantBool(t, "RearmTalos(failed)", moved, err, true)
		wantTalos(t, r, failed.Key(), talosRowIn(failed.Key(), CacheWanted, at))

		moved, err = r.RearmTalos(t.Context(), ready.Key(), at)
		wantBool(t, "RearmTalos(ready)", moved, err, false)
		wantTalos(t, r, ready.Key(), ready)

		moved, err = r.RearmTalos(t.Context(), talosKeyAt("v0.0.0"), at)
		wantBool(t, "RearmTalos(absent)", moved, err, false)
		if _, ok, err := r.Talos(t.Context(), talosKeyAt("v0.0.0")); err != nil || ok {
			t.Fatalf("Talos(absent) = %v, %v after RearmTalos; want no row, so it never creates one", ok, err)
		}
	})

	t.Run("ClaimAssignedOnly", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		// The machine is assigned to the amd64 tuple; the arm64 row is no one's.
		assignedMachine(t, r, 1, labHost, t0)
		mine, others := talosKeyOf(ArchAMD64), talosKeyOf(ArchARM64)
		seedTalos(t, d, talosRowIn(mine, CacheWanted, t0))
		seedTalos(t, d, talosRowIn(others, CacheWanted, t0.Add(time.Minute)))

		at := t0.Add(time.Hour)
		got, ok := mustClaimTalos(t, r, TalosOther, at)
		want := talosRowIn(others, CacheFetching, at)
		if !ok || !sameTalosRow(got, want) {
			t.Fatalf("ClaimTalos(TalosOther) = %+v, %v; want %+v (the assigned machine's row is not Other, though older)", got, ok, want)
		}
		if got, ok := mustClaimTalos(t, r, TalosOther, at); ok {
			t.Fatalf("second ClaimTalos(TalosOther) = %+v; want none left", got)
		}

		got, ok = mustClaimTalos(t, r, TalosAssigned, at)
		want = talosRowIn(mine, CacheFetching, at)
		if !ok || !sameTalosRow(got, want) {
			t.Fatalf("ClaimTalos(TalosAssigned) = %+v, %v; want %+v", got, ok, want)
		}
		wantTalos(t, r, mine, want)
		if got, ok := mustClaimTalos(t, r, TalosAssigned, at); ok {
			t.Fatalf("second ClaimTalos(TalosAssigned) = %+v; want none left", got)
		}
	})

	t.Run("ClaimAssignedStateOnly", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := assignedMachine(t, r, 1, labHost, t0)
		mustTransition(t, r, m.ID, TriggerConfigServed, t0.Add(time.Minute))
		seedTalos(t, d, talosRowIn(k, CacheWanted, t0))

		at := t0.Add(time.Hour)
		if got, ok := mustClaimTalos(t, r, TalosAssigned, at); ok {
			t.Fatalf("ClaimTalos(TalosAssigned) = %+v for an installing machine; want none", got)
		}
		if got, ok := mustClaimTalos(t, r, TalosOther, at); !ok || got.Key() != k {
			t.Fatalf("ClaimTalos(TalosOther) = %+v, %v; want %+v", got, ok, k)
		}
	})

	t.Run("ClaimOldestFirst", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		newer, older := talosKeyAt("v1.12.10"), talosKeyAt("v1.12.9")
		seedTalos(t, d, talosRowIn(newer, CacheWanted, t0.Add(time.Minute)))
		seedTalos(t, d, talosRowIn(older, CacheWanted, t0))

		for _, want := range []TalosKey{older, newer} {
			if got, ok := mustClaimTalos(t, r, TalosOther, t0.Add(time.Hour)); !ok || got.Key() != want {
				t.Fatalf("ClaimTalos = %+v, %v; want %+v", got, ok, want)
			}
		}
	})

	t.Run("ClaimRespectsBackoff", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seed := talosRowIn(k, CacheFailed, t0)
		seed.Attempts, seed.NextAttemptAt, seed.LastError = 1, t0.Add(Backoff(1)), talosCause
		seedTalos(t, d, seed)

		if got, ok := mustClaimTalos(t, r, TalosOther, seed.NextAttemptAt.Add(-time.Second)); ok {
			t.Fatalf("ClaimTalos before the backoff ends = %+v; want none", got)
		}
		wantTalos(t, r, k, seed)

		got, ok := mustClaimTalos(t, r, TalosOther, seed.NextAttemptAt)
		want := seed
		want.State, want.UpdatedAt = CacheFetching, seed.NextAttemptAt
		if !ok || !sameTalosRow(got, want) {
			t.Fatalf("ClaimTalos when the backoff ends = %+v, %v; want %+v", got, ok, want)
		}
	})

	t.Run("ConcurrentClaim", func(t *testing.T) {
		serveDB, cliDB := pair(t)
		defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
		repos := [2]*Repository{NewRepository(serveDB), NewRepository(cliDB)}
		for round := range 5 {
			key := talosKeyAt(fmt.Sprintf("v1.13.%d", round))
			seedTalos(t, serveDB, talosRowIn(key, CacheWanted, t0))
			var (
				claimed [2]bool
				errs    [2]error
			)
			together(func(i int) {
				_, claimed[i], errs[i] = repos[i].ClaimTalos(t.Context(), TalosOther, t0.Add(time.Hour))
			})
			if errs[0] != nil || errs[1] != nil {
				t.Fatalf("round %d: errors = %v, %v; want none", round, errs[0], errs[1])
			}
			if claimed[0] == claimed[1] {
				t.Fatalf("round %d: claimed = %v; want exactly one worker to get the row", round, claimed)
			}
		}
	})

	t.Run("RecordTalosProgress", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		fetching := talosRowIn(k, CacheFetching, t0)
		wanted := talosRowIn(talosKeyAt("v1.12.10"), CacheWanted, t0)
		seedTalos(t, d, fetching)
		seedTalos(t, d, wanted)

		if err := r.RecordTalosProgress(t.Context(), k, 40, 100); err != nil {
			t.Fatalf("RecordTalosProgress(fetching): %v", err)
		}
		want := fetching
		want.BytesDone, want.BytesTotal = 40, 100
		wantTalos(t, r, k, want)

		if err := r.RecordTalosProgress(t.Context(), wanted.Key(), 40, 100); err != nil {
			t.Fatalf("RecordTalosProgress(wanted): %v", err)
		}
		wantTalos(t, r, wanted.Key(), wanted)
	})

	t.Run("MarkTalosReady", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seed := talosRowIn(k, CacheFetching, t0)
		seed.Attempts, seed.LastError, seed.BytesDone, seed.BytesTotal = 3, talosCause, 90_000_000, 90_000_000
		seedTalos(t, d, seed)

		at := t0.Add(time.Minute)
		moved, err := r.MarkTalosReady(t.Context(), k, talosAssets(), at)
		wantBool(t, "MarkTalosReady(fetching)", moved, err, true)
		want := talosRowIn(k, CacheReady, at)
		want.BytesDone, want.BytesTotal, want.Assets = seed.BytesDone, seed.BytesTotal, talosAssets()
		wantTalos(t, r, k, want)

		moved, err = r.MarkTalosReady(t.Context(), k, TalosAssets{}, at.Add(time.Minute))
		wantBool(t, "MarkTalosReady(ready)", moved, err, false)
		wantTalos(t, r, k, want)
	})

	t.Run("FirstFailureBacksOff", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seedTalos(t, d, talosRowIn(k, CacheFetching, t0))

		at := t0.Add(time.Minute)
		got := mustFailTalos(t, r, TalosFailure{Key: k, Cause: talosCause, At: at})
		want := talosRowIn(k, CacheFailed, at)
		want.Attempts, want.NextAttemptAt, want.LastError = 1, at.Add(Backoff(1)), talosCause
		if !sameTalosRow(got, want) {
			t.Fatalf("RecordTalosFailure = %+v, want %+v", got, want)
		}
		wantTalos(t, r, k, want)
		if got := auditsOf(t, d, "talos.broken"); len(got) != 0 {
			t.Fatalf("talos.broken audits = %+v, want none for a transient failure", got)
		}
	})

	t.Run("TenthFailureBroken", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		ninth, tenth := talosKeyAt("v1.12.10"), k
		for key, attempts := range map[TalosKey]int{ninth: MaxFetchAttempts - 2, tenth: MaxFetchAttempts - 1} {
			seed := talosRowIn(key, CacheFetching, t0)
			seed.Attempts = attempts
			seedTalos(t, d, seed)
		}

		at := t0.Add(time.Minute)
		got := mustFailTalos(t, r, TalosFailure{Key: ninth, Cause: talosCause, At: at})
		if got.State != CacheFailed || got.Attempts != MaxFetchAttempts-1 || !got.NextAttemptAt.Equal(at.Add(Backoff(MaxFetchAttempts-1))) {
			t.Fatalf("the %dth failure = %+v, want failed, attempts %d, due after %s", MaxFetchAttempts-1, got, MaxFetchAttempts-1, Backoff(MaxFetchAttempts-1))
		}

		got = mustFailTalos(t, r, TalosFailure{Key: tenth, Cause: talosCause, At: at})
		want := talosRowIn(tenth, CacheBroken, at)
		want.Attempts, want.LastError = MaxFetchAttempts, talosCause
		if !sameTalosRow(got, want) {
			t.Fatalf("the %dth failure = %+v, want %+v", MaxFetchAttempts, got, want)
		}
		wantTalos(t, r, tenth, want)
		wantOneAudit(t, d, "talos.broken", ActorSystem, "", brokenAuditJSON)
	})

	t.Run("IntegrityBroken", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seedTalos(t, d, talosRowIn(k, CacheFetching, t0))

		at := t0.Add(time.Minute)
		got := mustFailTalos(t, r, TalosFailure{Key: k, Cause: talosCause, Integrity: true, At: at})
		want := talosRowIn(k, CacheBroken, at)
		want.Attempts, want.LastError = 1, talosCause
		if !sameTalosRow(got, want) {
			t.Fatalf("RecordTalosFailure = %+v, want %+v", got, want)
		}
		wantTalos(t, r, k, want)
		wantOneAudit(t, d, "talos.broken", ActorSystem, "", brokenAuditJSON)
	})

	t.Run("FailureOfOtherStateIgnored", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seed := talosRowIn(k, CacheReady, t0)
		seed.Assets = talosAssets()
		seedTalos(t, d, seed)

		got := mustFailTalos(t, r, TalosFailure{Key: k, Cause: talosCause, Integrity: true, At: t0.Add(time.Minute)})
		if !sameTalosRow(got, seed) {
			t.Fatalf("RecordTalosFailure of a ready row = %+v, want it unchanged: %+v", got, seed)
		}
		wantTalos(t, r, k, seed)
		if got := auditsOf(t, d, "talos.broken"); len(got) != 0 {
			t.Fatalf("talos.broken audits = %+v, want none", got)
		}
	})

	t.Run("FailureCauseCut", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seedTalos(t, d, talosRowIn(k, CacheFetching, t0))

		got := mustFailTalos(t, r, TalosFailure{Key: k, Cause: strings.Repeat("x", 2*MaxCauseBytes), At: t0})
		if len(got.LastError) != MaxCauseBytes {
			t.Fatalf("stored cause is %d bytes, want %d", len(got.LastError), MaxCauseBytes)
		}
	})

	t.Run("ResetTalosFetching", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		a, b, ready := talosKeyAt("v1.12.8"), talosKeyAt("v1.12.9"), talosKeyAt("v1.12.10")
		seedTalos(t, d, talosRowIn(a, CacheFetching, t0))
		seedTalos(t, d, talosRowIn(b, CacheFetching, t0))
		seed := talosRowIn(ready, CacheReady, t0)
		seed.Assets = talosAssets()
		seedTalos(t, d, seed)

		at := t0.Add(time.Minute)
		n, err := r.ResetTalosFetching(t.Context(), at)
		if err != nil || n != 2 {
			t.Fatalf("ResetTalosFetching = %d, %v; want 2, nil", n, err)
		}
		wantTalos(t, r, a, talosRowIn(a, CacheWanted, at))
		wantTalos(t, r, b, talosRowIn(b, CacheWanted, at))
		wantTalos(t, r, ready, seed)
	})

	t.Run("ResetTalosFetchingRow", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		a, b := talosKeyAt("v1.12.8"), talosKeyAt("v1.12.9")
		seedTalos(t, d, talosRowIn(a, CacheFetching, t0))
		seedTalos(t, d, talosRowIn(b, CacheFetching, t0))

		at := t0.Add(time.Minute)
		moved, err := r.ResetTalosFetchingRow(t.Context(), a, at)
		wantBool(t, "ResetTalosFetchingRow(a)", moved, err, true)
		wantTalos(t, r, a, talosRowIn(a, CacheWanted, at))
		wantTalos(t, r, b, talosRowIn(b, CacheFetching, t0))

		moved, err = r.ResetTalosFetchingRow(t.Context(), a, at.Add(time.Minute))
		wantBool(t, "ResetTalosFetchingRow(wanted)", moved, err, false)
		wantTalos(t, r, a, talosRowIn(a, CacheWanted, at))
	})

	t.Run("RequeueTalos", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		ready, wanted := talosRowIn(k, CacheReady, t0), talosRowIn(talosKeyAt("v1.12.10"), CacheWanted, t0)
		ready.Assets = talosAssets()
		seedTalos(t, d, ready)
		seedTalos(t, d, wanted)

		at := t0.Add(time.Minute)
		moved, err := r.RequeueTalos(t.Context(), k, at)
		wantBool(t, "RequeueTalos(ready)", moved, err, true)
		want := ready
		want.State, want.UpdatedAt = CacheWanted, at
		wantTalos(t, r, k, want)

		moved, err = r.RequeueTalos(t.Context(), wanted.Key(), at)
		wantBool(t, "RequeueTalos(wanted)", moved, err, false)
		wantTalos(t, r, wanted.Key(), wanted)
	})

	t.Run("ResetHashFromReady", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		ready := talosRowIn(k, CacheReady, t0)
		ready.Assets = talosAssets()
		broken := talosRowIn(talosKeyAt("v1.12.10"), CacheBroken, t0)
		broken.Attempts, broken.LastError, broken.Assets = MaxFetchAttempts, talosCause, talosAssets()
		seedTalos(t, d, ready)
		seedTalos(t, d, broken)

		at := t0.Add(time.Minute)
		moved, err := r.ResetTalosHash(t.Context(), k, ActorCLI, at)
		wantBool(t, "ResetTalosHash(ready)", moved, err, true)
		wantTalos(t, r, k, talosRowIn(k, CacheWanted, at))
		wantOneAudit(t, d, "talos.hash_reset", ActorCLI, "", hashResetAuditJSON)

		moved, err = r.ResetTalosHash(t.Context(), broken.Key(), ActorCLI, at)
		wantBool(t, "ResetTalosHash(broken)", moved, err, true)
		wantTalos(t, r, broken.Key(), talosRowIn(broken.Key(), CacheWanted, at))

		moved, err = r.ResetTalosHash(t.Context(), talosKeyAt("v0.0.0"), ActorCLI, at)
		wantBool(t, "ResetTalosHash(absent)", moved, err, false)
		if got := auditsOf(t, d, "talos.hash_reset"); len(got) != 2 {
			t.Fatalf("talos.hash_reset audits = %+v, want the two resets and none for the absent row", got)
		}
	})

	t.Run("Talos", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seed := talosRowIn(k, CacheReady, t0)
		seed.Body, seed.Assets = body, talosAssets()
		seedTalos(t, d, seed)

		got, ok, err := r.Talos(t.Context(), k)
		if err != nil || !ok || !sameTalosRow(got, seed) {
			t.Fatalf("Talos(present) = %+v, %v, %v; want %+v, true, nil", got, ok, err, seed)
		}
		if got, ok, err := r.Talos(t.Context(), talosKeyOf(ArchARM64)); err != nil || ok {
			t.Fatalf("Talos(absent) = %+v, %v, %v; want no row", got, ok, err)
		}
	})

	t.Run("TalosRows", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		other := "0000000000000000000000000000000000000000000000000000000000000000"
		want := []TalosKey{
			{Schematic: other, Version: "v1.12.9", Arch: ArchARM64},
			{Schematic: plainSchematic, Version: "v1.12.10", Arch: ArchAMD64},
			{Schematic: plainSchematic, Version: "v1.12.10", Arch: ArchARM64},
			{Schematic: plainSchematic, Version: "v1.12.9", Arch: ArchAMD64},
		}
		for _, i := range []int{3, 1, 0, 2} {
			seedTalos(t, d, talosRowIn(want[i], CacheWanted, t0))
		}

		rows, err := r.TalosRows(t.Context())
		if err != nil {
			t.Fatalf("TalosRows: %v", err)
		}
		var got []TalosKey
		for i := range rows {
			got = append(got, rows[i].Key())
		}
		if !slices.Equal(got, want) {
			t.Fatalf("TalosRows keys = %+v, want %+v", got, want)
		}
	})

	t.Run("AbortRearms", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := assignedMachine(t, r, 1, labHost, t0)
		mustTransition(t, r, m.ID, TriggerConfigServed, t0.Add(time.Minute))
		mine, others := talosKeyOf(ArchAMD64), talosKeyOf(ArchARM64)
		for _, key := range []TalosKey{mine, others} {
			seed := talosRowIn(key, CacheBroken, t0)
			seed.Attempts, seed.LastError, seed.Assets = MaxFetchAttempts, talosCause, talosAssets()
			seedTalos(t, d, seed)
		}

		at := t0.Add(2 * time.Minute)
		mustTransition(t, r, m.ID, TriggerAbortInstall, at)
		want := talosRowIn(mine, CacheWanted, at)
		want.Assets = talosAssets()
		wantTalos(t, r, mine, want)
		if got, _, _ := r.Talos(t.Context(), others); got.State != CacheBroken {
			t.Fatalf("the row of another arch is %s, want it left broken", got.State)
		}
	})

	t.Run("ApproveRearms", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)
		seed := talosRowIn(k, CacheBroken, t0)
		seed.Attempts, seed.LastError = MaxFetchAttempts, talosCause
		seedTalos(t, d, seed)

		at := t0.Add(time.Minute)
		mustApprove(t, r, m.ID, labHost, at)
		wantTalos(t, r, k, talosRowIn(k, CacheWanted, at))
	})

	t.Run("RearmNeverCreates", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		assignedMachine(t, r, 1, labHost, t0)
		rows, err := r.TalosRows(t.Context())
		if err != nil || len(rows) != 0 {
			t.Fatalf("TalosRows after an approve with no row = %+v, %v; want none", rows, err)
		}
	})
}
