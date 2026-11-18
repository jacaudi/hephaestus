package fleet

import (
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

const (
	pinOne = "p1"
	pinTwo = "p2"

	readyDetail   = `{"entry":"tshell","arch":"amd64","pin":"p1"}`
	insertFlagged = "INSERT INTO machines (id, smbios_uuid, junk, serial, product, arch, state, created_at, last_seen_at, boot_next, boot_next_set_at)" +
		" VALUES (?, ?, FALSE, 's', 'p', ?, 'discovered', ?, ?, ?, ?)"
)

func TestUtilityRepository_SQLite(t *testing.T) { runUtilitySuite(t, dbtest.SQLite) }

// cacheRowIn is a row for key at pin p1 in state, last changed at updated.
func cacheRowIn(key CacheKey, state CacheState, updated time.Time) CacheRow {
	return CacheRow{Entry: key.Entry, Arch: key.Arch, Pin: pinOne, State: state, UpdatedAt: updated}
}

// seedCache stores row as given, bypassing the repository.
func seedCache(t *testing.T, d *db.DB, row CacheRow) {
	t.Helper()
	if _, err := d.Bun().NewInsert().Model(&row).Exec(t.Context()); err != nil {
		t.Fatalf("seed %+v: %v", row, err)
	}
}

// seedFlagged stores a machine whose boot_next is entry, set at setAt.
func seedFlagged(t *testing.T, d *db.DB, id string, arch Arch, entry string, setAt time.Time) {
	t.Helper()
	if err := execRaw(t, d, insertFlagged, id, id, string(arch), setAt, setAt, entry, setAt); err != nil {
		t.Fatalf("seed machine %s: %v", id, err)
	}
}

// flagCleared reports whether the machine has neither boot_next nor its time.
func flagCleared(t *testing.T, d *db.DB, id string) bool {
	t.Helper()
	return scalar[int](t, d, "SELECT count(*) FROM machines WHERE id = ? AND boot_next IS NULL AND boot_next_set_at IS NULL", id) == 1
}

func sameCacheRow(a, b CacheRow) bool {
	return a.Entry == b.Entry && a.Arch == b.Arch && a.Pin == b.Pin && a.State == b.State && a.Attempts == b.Attempts &&
		a.NextAttemptAt.Equal(b.NextAttemptAt) && a.Priority == b.Priority &&
		a.BytesDone == b.BytesDone && a.BytesTotal == b.BytesTotal && a.LastError == b.LastError && a.UpdatedAt.Equal(b.UpdatedAt)
}

// wantCache fails unless the stored row at key is want in every column.
func wantCache(t *testing.T, r *Repository, key CacheKey, want CacheRow) {
	t.Helper()
	got, ok, err := r.CacheRow(t.Context(), key)
	if err != nil || !ok || !sameCacheRow(got, want) {
		t.Fatalf("row %+v = %+v, %v, %v; want %+v", key, got, ok, err, want)
	}
}

func mustRequest(t *testing.T, r *Repository, key CacheKey, pin string, priority int, at time.Time) FetchRequest {
	t.Helper()
	got, err := r.RequestFetch(t.Context(), key, pin, priority, at)
	if err != nil {
		t.Fatalf("RequestFetch(%+v, %q, %d): %v", key, pin, priority, err)
	}
	return got
}

func mustClaim(t *testing.T, r *Repository, at time.Time) (CacheRow, bool) {
	t.Helper()
	row, ok, err := r.ClaimFetch(t.Context(), at)
	if err != nil {
		t.Fatalf("ClaimFetch: %v", err)
	}
	return row, ok
}

func mustFail(t *testing.T, r *Repository, f FetchFailure) CacheRow {
	t.Helper()
	row, err := r.RecordFetchFailure(t.Context(), f)
	if err != nil {
		t.Fatalf("RecordFetchFailure(%+v): %v", f, err)
	}
	return row
}

// wantBool fails unless got is want and err is nil.
func wantBool(t *testing.T, what string, got bool, err error, want bool) {
	t.Helper()
	if err != nil || got != want {
		t.Fatalf("%s = %v, %v; want %v, nil", what, got, err, want)
	}
}

// auditsOf returns the audit events with action, oldest first.
func auditsOf(t *testing.T, d *db.DB, action string) []auditEventRow {
	t.Helper()
	var out []auditEventRow
	for _, a := range auditRows(t, d) {
		if a.Action == action {
			out = append(out, a)
		}
	}
	return out
}

// wantOneAudit fails unless exactly one audit event has action, and it has the given actor, machine and detail.
func wantOneAudit(t *testing.T, d *db.DB, action, actor, machineID, detail string) {
	t.Helper()
	got := auditsOf(t, d, action)
	if len(got) != 1 || got[0].Actor != actor || got[0].MachineID != machineID || got[0].Detail != detail {
		t.Fatalf("%s audits = %+v; want one by %q for %q with detail %s", action, got, actor, machineID, detail)
	}
}

// runUtilitySuite is the utility_cache suite both databases run:
// TestUtilityRepository_SQLite untagged, TestUtilityRepository_Postgres under
// the integration tag.
func runUtilitySuite(t *testing.T, open func(testing.TB) *db.DB) {
	t.Helper()
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	k := CacheKey{Entry: "tshell", Arch: ArchAMD64}

	t.Run("RequestCreatesWanted", func(t *testing.T) {
		r := NewRepository(open(t))
		got := mustRequest(t, r, k, pinOne, 0, t0)
		want := cacheRowIn(k, CacheWanted, t0)
		if got.Rearmed || got.PreviousError != "" || !sameCacheRow(got.Row, want) {
			t.Fatalf("RequestFetch = %+v; want %+v, not rearmed", got, want)
		}
		wantCache(t, r, k, want)
	})

	t.Run("RequestBootNextPriority", func(t *testing.T) {
		r := NewRepository(open(t))
		mustRequest(t, r, k, pinOne, 1, t0)
		want := cacheRowIn(k, CacheWanted, t0)
		want.Priority = 1
		wantCache(t, r, k, want)
	})

	t.Run("RequestRearmsFailed", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seed := cacheRowIn(k, CacheFailed, t0)
		seed.Attempts, seed.LastError, seed.NextAttemptAt, seed.BytesDone, seed.BytesTotal = 3, "boom", t0.Add(4*time.Minute), 7, 9
		seedCache(t, d, seed)

		got := mustRequest(t, r, k, pinOne, 0, t0.Add(time.Minute))
		want := cacheRowIn(k, CacheWanted, t0.Add(time.Minute))
		want.BytesDone, want.BytesTotal = 7, 9
		if !got.Rearmed || got.PreviousError != "boom" || !sameCacheRow(got.Row, want) || !got.Row.NextAttemptAt.IsZero() {
			t.Fatalf("RequestFetch = %+v; want %+v, rearmed, previous error %q", got, want, "boom")
		}
		wantCache(t, r, k, want)

		// A re-arm never lowers the priority a boot-next gave the row.
		high := CacheKey{Entry: "tlive", Arch: ArchARM64}
		armed := cacheRowIn(high, CacheFailed, t0)
		armed.Attempts, armed.LastError, armed.NextAttemptAt, armed.Priority = 2, "boom", t0.Add(2*time.Minute), 1
		seedCache(t, d, armed)

		got = mustRequest(t, r, high, pinOne, 0, t0.Add(time.Minute))
		want = cacheRowIn(high, CacheWanted, t0.Add(time.Minute))
		want.Priority = 1
		if !got.Rearmed || !sameCacheRow(got.Row, want) {
			t.Fatalf("RequestFetch of a priority 1 row at priority 0 = %+v; want %+v, rearmed", got, want)
		}
		wantCache(t, r, high, want)
	})

	t.Run("RequestRearmsBroken", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seed := cacheRowIn(k, CacheBroken, t0)
		seed.Attempts, seed.LastError = 10, "sha256 mismatch"
		seedCache(t, d, seed)

		got := mustRequest(t, r, k, pinOne, 1, t0.Add(time.Minute))
		want := cacheRowIn(k, CacheWanted, t0.Add(time.Minute))
		want.Priority = 1
		if !got.Rearmed || got.PreviousError != "sha256 mismatch" || !sameCacheRow(got.Row, want) {
			t.Fatalf("RequestFetch = %+v; want %+v, rearmed, previous error %q", got, want, "sha256 mismatch")
		}
		wantCache(t, r, k, want)
	})

	t.Run("RequestKeepsLiveStates", func(t *testing.T) {
		for _, state := range []CacheState{CacheWanted, CacheFetching, CacheReady} {
			d := open(t)
			r := NewRepository(d)
			key := CacheKey{Entry: "tlive", Arch: ArchARM64}
			seed := cacheRowIn(key, state, t0)
			seed.Attempts, seed.BytesDone, seed.BytesTotal = 2, 5, 10
			seedCache(t, d, seed)

			want := seed
			want.Priority = 1
			got := mustRequest(t, r, key, pinOne, 1, t0.Add(time.Minute))
			if got.Rearmed || got.PreviousError != "" || !sameCacheRow(got.Row, want) {
				t.Fatalf("%s: RequestFetch = %+v; want %+v, not rearmed", state, got, want)
			}
			wantCache(t, r, key, want)

			mustRequest(t, r, key, pinOne, 0, t0.Add(2*time.Minute))
			wantCache(t, r, key, want)
		}
	})

	t.Run("RequestOtherPinNoOp", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seed := cacheRowIn(k, CacheFailed, t0)
		seed.Attempts, seed.LastError, seed.NextAttemptAt = 3, "boom", t0.Add(4*time.Minute)
		seedCache(t, d, seed)

		got := mustRequest(t, r, k, pinTwo, 1, t0.Add(time.Minute))
		if got.Rearmed || !sameCacheRow(got.Row, seed) {
			t.Fatalf("RequestFetch at another pin = %+v; want %+v unchanged", got, seed)
		}
		wantCache(t, r, k, seed)
	})

	t.Run("WantCreatesWantedPriority1", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		if err := wantFetch(t.Context(), d.Bun(), FetchWant{Key: k, Pin: pinOne}, t0); err != nil {
			t.Fatalf("wantFetch: %v", err)
		}
		want := cacheRowIn(k, CacheWanted, t0)
		want.Priority = 1
		wantCache(t, r, k, want)

		// A wanted row at the pin is raised to priority 1 and otherwise left alone.
		other := CacheKey{Entry: "tlive", Arch: ArchARM64}
		seed := cacheRowIn(other, CacheWanted, t0)
		seed.Attempts = 2
		seedCache(t, d, seed)
		if err := wantFetch(t.Context(), d.Bun(), FetchWant{Key: other, Pin: pinOne}, t0.Add(time.Minute)); err != nil {
			t.Fatalf("wantFetch: %v", err)
		}
		seed.Priority = 1
		wantCache(t, r, other, seed)
	})

	t.Run("WantLeavesFailedAndBroken", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		var rows []*CacheRow
		for i, state := range []CacheState{CacheFailed, CacheBroken, CacheFetching, CacheReady} {
			row := cacheRowIn(CacheKey{Entry: "t" + string(rune('a'+i)), Arch: ArchAMD64}, state, t0)
			row.Attempts, row.LastError = 3, "boom"
			if state == CacheFailed {
				row.NextAttemptAt = t0.Add(4 * time.Minute)
			}
			seedCache(t, d, row)
			rows = append(rows, &row)
		}
		// A wanted row at another pin is not the want's either.
		stale := cacheRowIn(CacheKey{Entry: "te", Arch: ArchAMD64}, CacheWanted, t0)
		stale.Pin = pinTwo
		seedCache(t, d, stale)
		rows = append(rows, &stale)

		for i := range 3 {
			for _, row := range rows {
				if err := wantFetch(t.Context(), d.Bun(), FetchWant{Key: row.Key(), Pin: pinOne}, t0.Add(30*time.Second)); err != nil {
					t.Fatalf("wantFetch %d: %v", i, err)
				}
			}
		}
		for _, row := range rows {
			wantCache(t, r, row.Key(), *row)
		}
	})

	t.Run("ClaimOrder", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		a := cacheRowIn(CacheKey{Entry: "ta", Arch: ArchAMD64}, CacheWanted, t0)
		b := cacheRowIn(CacheKey{Entry: "tb", Arch: ArchAMD64}, CacheWanted, t0.Add(time.Minute))
		b.Priority = 1
		c := cacheRowIn(CacheKey{Entry: "tc", Arch: ArchAMD64}, CacheWanted, t0.Add(-time.Minute))
		seedCache(t, d, a)
		seedCache(t, d, b)
		seedCache(t, d, c)

		at := t0.Add(time.Hour)
		for _, next := range []*CacheRow{&b, &c, &a} {
			got, ok := mustClaim(t, r, at)
			want := *next
			want.State, want.UpdatedAt = CacheFetching, at
			if !ok || !sameCacheRow(got, want) {
				t.Fatalf("ClaimFetch = %+v, %v; want %+v", got, ok, want)
			}
			wantCache(t, r, want.Key(), want)
		}
		if got, ok := mustClaim(t, r, at); ok {
			t.Fatalf("ClaimFetch with nothing eligible = %+v, true; want false", got)
		}
	})

	t.Run("ClaimFailedOnlyWhenDue", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seed := cacheRowIn(k, CacheFailed, t0)
		seed.Attempts, seed.NextAttemptAt = 1, t0.Add(2*time.Minute)
		seedCache(t, d, seed)

		if got, ok := mustClaim(t, r, t0.Add(time.Minute)); ok {
			t.Fatalf("ClaimFetch before next_attempt_at = %+v, true; want false", got)
		}
		wantCache(t, r, k, seed)

		got, ok := mustClaim(t, r, t0.Add(2*time.Minute))
		want := seed
		want.State, want.UpdatedAt = CacheFetching, t0.Add(2*time.Minute)
		if !ok || !sameCacheRow(got, want) {
			t.Fatalf("ClaimFetch at next_attempt_at = %+v, %v; want %+v", got, ok, want)
		}
		wantCache(t, r, k, want)
	})

	t.Run("ClaimSkipsOthers", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		var rows []*CacheRow
		for i, state := range []CacheState{CacheReady, CacheBroken, CacheFetching} {
			row := cacheRowIn(CacheKey{Entry: "t" + string(rune('a'+i)), Arch: ArchAMD64}, state, t0)
			seedCache(t, d, row)
			rows = append(rows, &row)
		}
		if got, ok := mustClaim(t, r, t0.Add(time.Hour)); ok {
			t.Fatalf("ClaimFetch = %+v, true; want false", got)
		}
		for _, row := range rows {
			wantCache(t, r, row.Key(), *row)
		}
	})

	t.Run("ReadyFromFetching", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seed := cacheRowIn(k, CacheFetching, t0)
		seed.Attempts, seed.LastError, seed.NextAttemptAt, seed.BytesDone, seed.BytesTotal = 2, "x", t0.Add(time.Minute), 10, 10
		seedCache(t, d, seed)

		ok, err := r.MarkReady(t.Context(), k, pinOne, t0.Add(5*time.Minute))
		wantBool(t, "MarkReady", ok, err, true)
		want := cacheRowIn(k, CacheReady, t0.Add(5*time.Minute))
		want.BytesDone, want.BytesTotal = 10, 10
		wantCache(t, r, k, want)
		wantOneAudit(t, d, "utility.ready", "system", "", readyDetail)

		ok, err = r.MarkReady(t.Context(), k, pinOne, t0.Add(6*time.Minute))
		wantBool(t, "second MarkReady", ok, err, false)
		wantCache(t, r, k, want)
		wantOneAudit(t, d, "utility.ready", "system", "", readyDetail)
	})

	t.Run("ReadyWrongPinNoOp", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seed := cacheRowIn(k, CacheFetching, t0)
		seedCache(t, d, seed)

		ok, err := r.MarkReady(t.Context(), k, pinTwo, t0.Add(time.Minute))
		wantBool(t, "MarkReady at another pin", ok, err, false)
		wantCache(t, r, k, seed)
		if got := auditRows(t, d); len(got) != 0 {
			t.Fatalf("audit = %+v, want none", got)
		}
	})

	t.Run("FailureBacksOff", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seedCache(t, d, cacheRowIn(k, CacheWanted, t0))

		at := t0
		for n := 1; n <= 9; n++ {
			if _, ok := mustClaim(t, r, at); !ok {
				t.Fatalf("claim %d at %s: nothing eligible", n, at)
			}
			got := mustFail(t, r, FetchFailure{Key: k, Pin: pinOne, Cause: "dial tcp: timeout", At: at})
			want := cacheRowIn(k, CacheFailed, at)
			want.Attempts, want.NextAttemptAt, want.LastError = n, at.Add(Backoff(n)), "dial tcp: timeout"
			if !sameCacheRow(got, want) {
				t.Fatalf("failure %d returned %+v, want %+v", n, got, want)
			}
			wantCache(t, r, k, want)
			at = got.NextAttemptAt
		}
		if got := auditRows(t, d); len(got) != 0 {
			t.Fatalf("audit = %+v, want none before the row breaks", got)
		}
	})

	t.Run("TenthFailureBroken", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seed := cacheRowIn(k, CacheFetching, t0)
		seed.Attempts = 9
		seedCache(t, d, seed)
		seedFlagged(t, d, "m1", ArchAMD64, "tshell", t0)
		seedFlagged(t, d, "m2", ArchARM64, "tshell", t0)
		seedFlagged(t, d, "m3", ArchAMD64, "other", t0)

		at := t0.Add(time.Minute)
		got := mustFail(t, r, FetchFailure{Key: k, Pin: pinOne, Cause: "dial tcp: timeout", At: at})
		want := cacheRowIn(k, CacheBroken, at)
		want.Attempts, want.LastError = 10, "dial tcp: timeout"
		if !sameCacheRow(got, want) {
			t.Fatalf("RecordFetchFailure = %+v, want %+v", got, want)
		}
		wantCache(t, r, k, want)
		wantOneAudit(t, d, "utility.broken", "system", "", `{"entry":"tshell","arch":"amd64","pin":"p1","error":"dial tcp: timeout"}`)
		wantOneAudit(t, d, "boot_next.cleared", "system", "m1", `{"entry":"tshell","set_at":"2026-11-14T12:00:00Z","reason":"broken"}`)
		if !flagCleared(t, d, "m1") || flagCleared(t, d, "m2") || flagCleared(t, d, "m3") {
			t.Fatal("want only m1 cleared: m2 is another arch and m3 names another entry")
		}
	})

	t.Run("IntegrityBroken", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seedCache(t, d, cacheRowIn(k, CacheFetching, t0))
		seedFlagged(t, d, "m1", ArchAMD64, "tshell", t0)

		at := t0.Add(time.Minute)
		got := mustFail(t, r, FetchFailure{Key: k, Pin: pinOne, Cause: "sha256 mismatch", Integrity: true, At: at})
		want := cacheRowIn(k, CacheBroken, at)
		want.Attempts, want.LastError = 1, "sha256 mismatch"
		if !sameCacheRow(got, want) {
			t.Fatalf("RecordFetchFailure = %+v, want %+v", got, want)
		}
		wantCache(t, r, k, want)
		wantOneAudit(t, d, "utility.broken", "system", "", `{"entry":"tshell","arch":"amd64","pin":"p1","error":"sha256 mismatch"}`)
		if !flagCleared(t, d, "m1") {
			t.Fatal("m1's boot_next was not cleared")
		}
	})

	t.Run("FailureNotFetchingNoOp", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seed := cacheRowIn(k, CacheWanted, t0)
		seedCache(t, d, seed)

		got := mustFail(t, r, FetchFailure{Key: k, Pin: pinOne, Cause: "boom", At: t0.Add(time.Minute)})
		if !sameCacheRow(got, seed) {
			t.Fatalf("RecordFetchFailure on a wanted row = %+v, want %+v", got, seed)
		}
		wantCache(t, r, k, seed)

		// A fetching row at another pin is not this failure's either.
		other := cacheRowIn(CacheKey{Entry: "tlive", Arch: ArchAMD64}, CacheFetching, t0)
		seedCache(t, d, other)
		if got := mustFail(t, r, FetchFailure{Key: other.Key(), Pin: pinTwo, Cause: "boom", At: t0}); !sameCacheRow(got, other) {
			t.Fatalf("RecordFetchFailure at another pin = %+v, want %+v", got, other)
		}
		if got := auditRows(t, d); len(got) != 0 {
			t.Fatalf("audit = %+v, want none", got)
		}
	})

	t.Run("RequeueReady", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seedCache(t, d, cacheRowIn(k, CacheReady, t0))

		at := t0.Add(time.Minute)
		ok, err := r.Requeue(t.Context(), k, pinOne, at)
		wantBool(t, "Requeue of a ready row", ok, err, true)
		wantCache(t, r, k, cacheRowIn(k, CacheWanted, at))

		ok, err = r.Requeue(t.Context(), k, pinOne, at.Add(time.Minute))
		wantBool(t, "Requeue of a wanted row", ok, err, false)
		wantCache(t, r, k, cacheRowIn(k, CacheWanted, at))
	})

	t.Run("ResetFetchingAll", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		fetchingAMD := cacheRowIn(CacheKey{Entry: "ta", Arch: ArchAMD64}, CacheFetching, t0)
		fetchingARM := cacheRowIn(CacheKey{Entry: "ta", Arch: ArchARM64}, CacheFetching, t0)
		ready := cacheRowIn(CacheKey{Entry: "tb", Arch: ArchAMD64}, CacheReady, t0)
		seedCache(t, d, ready)
		seedCache(t, d, fetchingAMD)
		seedCache(t, d, fetchingARM)

		at := t0.Add(time.Minute)
		n, err := r.ResetFetching(t.Context(), at)
		if err != nil || n != 2 {
			t.Fatalf("ResetFetching = %d, %v; want 2, nil", n, err)
		}
		for _, row := range []*CacheRow{&fetchingAMD, &fetchingARM} {
			want := *row
			want.State, want.UpdatedAt = CacheWanted, at
			wantCache(t, r, row.Key(), want)
		}
		wantCache(t, r, ready.Key(), ready)
	})

	t.Run("RepinResets", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seed := cacheRowIn(k, CacheFailed, t0)
		seed.Attempts, seed.BytesDone, seed.BytesTotal, seed.LastError, seed.NextAttemptAt = 4, 10, 20, "x", t0.Add(8*time.Minute)
		seed.Priority = 1
		seedCache(t, d, seed)

		at := t0.Add(time.Minute)
		ok, err := r.Repin(t.Context(), k, pinOne, pinTwo, at)
		wantBool(t, "Repin", ok, err, true)
		want := cacheRowIn(k, CacheWanted, at)
		want.Pin, want.Priority = pinTwo, 1
		wantCache(t, r, k, want)

		ok, err = r.Repin(t.Context(), k, pinOne, "p3", at)
		wantBool(t, "Repin from a pin the row is not at", ok, err, false)
		wantCache(t, r, k, want)
	})

	t.Run("DeleteCacheRow", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seedCache(t, d, cacheRowIn(k, CacheReady, t0))

		if err := r.DeleteCacheRow(t.Context(), k); err != nil {
			t.Fatalf("DeleteCacheRow: %v", err)
		}
		if got, ok, err := r.CacheRow(t.Context(), k); err != nil || ok {
			t.Fatalf("CacheRow after delete = %+v, %v, %v; want not found", got, ok, err)
		}
		if err := r.DeleteCacheRow(t.Context(), k); err != nil {
			t.Fatalf("DeleteCacheRow of a missing row: %v", err)
		}
	})

	t.Run("RestoreReadyFromWantedAndFailed", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		seedCache(t, d, cacheRowIn(k, CacheWanted, t0))
		failedKey := CacheKey{Entry: "tshell", Arch: ArchARM64}
		failed := cacheRowIn(failedKey, CacheFailed, t0)
		failed.Attempts, failed.LastError, failed.NextAttemptAt = 3, "boom", t0.Add(4*time.Minute)
		seedCache(t, d, failed)

		at := t0.Add(time.Minute)
		ok, err := r.RestoreReady(t.Context(), k, pinOne, at)
		wantBool(t, "RestoreReady of a wanted row", ok, err, true)
		wantCache(t, r, k, cacheRowIn(k, CacheReady, at))
		wantOneAudit(t, d, "utility.ready", "system", "", readyDetail)

		ok, err = r.RestoreReady(t.Context(), failedKey, pinOne, at)
		wantBool(t, "RestoreReady of a failed row", ok, err, true)
		wantCache(t, r, failedKey, cacheRowIn(failedKey, CacheReady, at))
		if got := auditsOf(t, d, "utility.ready"); len(got) != 2 {
			t.Fatalf("utility.ready audits = %+v, want 2", got)
		}
	})

	t.Run("RestoreReadySkipsBroken", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		broken := cacheRowIn(k, CacheBroken, t0)
		broken.Attempts, broken.LastError = 10, "sha256 mismatch"
		ready := cacheRowIn(CacheKey{Entry: "tlive", Arch: ArchAMD64}, CacheReady, t0)
		seedCache(t, d, broken)
		seedCache(t, d, ready)

		for _, row := range []*CacheRow{&broken, &ready} {
			ok, err := r.RestoreReady(t.Context(), row.Key(), pinOne, t0.Add(time.Minute))
			wantBool(t, "RestoreReady of a "+string(row.State)+" row", ok, err, false)
			wantCache(t, r, row.Key(), *row)
		}
		if got := auditRows(t, d); len(got) != 0 {
			t.Fatalf("audit = %+v, want none", got)
		}
	})

	t.Run("ProgressOnlyWhileFetching", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		fetching := cacheRowIn(k, CacheFetching, t0)
		ready := cacheRowIn(CacheKey{Entry: "tlive", Arch: ArchAMD64}, CacheReady, t0)
		seedCache(t, d, fetching)
		seedCache(t, d, ready)

		if err := r.RecordProgress(t.Context(), FetchProgress{Key: k, Pin: pinTwo, Done: 1, Total: 2}); err != nil {
			t.Fatalf("RecordProgress at another pin: %v", err)
		}
		wantCache(t, r, k, fetching)

		if err := r.RecordProgress(t.Context(), FetchProgress{Key: k, Pin: pinOne, Done: 5, Total: 10}); err != nil {
			t.Fatalf("RecordProgress: %v", err)
		}
		fetching.BytesDone, fetching.BytesTotal = 5, 10 // updated_at stays at the last state change
		wantCache(t, r, k, fetching)

		if err := r.RecordProgress(t.Context(), FetchProgress{Key: ready.Key(), Pin: pinOne, Done: 5, Total: 10}); err != nil {
			t.Fatalf("RecordProgress on a ready row: %v", err)
		}
		wantCache(t, r, ready.Key(), ready)
		if got := auditRows(t, d); len(got) != 0 {
			t.Fatalf("audit = %+v, want none: progress is not a state change", got)
		}
	})

	t.Run("CacheRowsOrdered", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		for _, key := range []CacheKey{{Entry: "tb", Arch: ArchARM64}, {Entry: "ta", Arch: ArchARM64}, {Entry: "ta", Arch: ArchAMD64}} {
			seedCache(t, d, cacheRowIn(key, CacheWanted, t0))
		}

		rows, err := r.CacheRows(t.Context())
		if err != nil {
			t.Fatalf("CacheRows: %v", err)
		}
		want := []CacheKey{{Entry: "ta", Arch: ArchAMD64}, {Entry: "ta", Arch: ArchARM64}, {Entry: "tb", Arch: ArchARM64}}
		if len(rows) != len(want) {
			t.Fatalf("CacheRows = %+v, want keys %+v", rows, want)
		}
		for i := range rows {
			if rows[i].Key() != want[i] {
				t.Fatalf("CacheRows[%d] = %+v, want key %+v", i, rows[i], want[i])
			}
		}
		if got, ok, err := r.CacheRow(t.Context(), CacheKey{Entry: "missing", Arch: ArchAMD64}); err != nil || ok {
			t.Fatalf("CacheRow of a missing key = %+v, %v, %v; want not found", got, ok, err)
		}
	})

	t.Run("ClearBootNextHelper", func(t *testing.T) {
		d := open(t)
		seedFlagged(t, d, "m1", ArchAMD64, "tshell", t0)
		req := ClearBootNextRequest{MachineID: "m1", Entry: "tshell", SetAt: t0, Reason: ClearCLI, Actor: "cli", At: t0.Add(time.Minute)}

		ok, err := clearBootNext(t.Context(), d.Bun(), req)
		wantBool(t, "clearBootNext", ok, err, true)
		if !flagCleared(t, d, "m1") {
			t.Fatal("boot_next was not cleared")
		}
		wantOneAudit(t, d, "boot_next.cleared", "cli", "m1", `{"entry":"tshell","set_at":"2026-11-14T12:00:00Z","reason":"cli"}`)

		ok, err = clearBootNext(t.Context(), d.Bun(), req)
		wantBool(t, "second clearBootNext", ok, err, false)
		wantOneAudit(t, d, "boot_next.cleared", "cli", "m1", `{"entry":"tshell","set_at":"2026-11-14T12:00:00Z","reason":"cli"}`)

		// A flag set again since the caller read it is not the caller's to clear.
		if err := execRaw(t, d, "UPDATE machines SET boot_next = 'tshell', boot_next_set_at = ? WHERE id = 'm1'", t0); err != nil {
			t.Fatalf("flag m1 again: %v", err)
		}
		req.SetAt = t0.Add(time.Second)
		ok, err = clearBootNext(t.Context(), d.Bun(), req)
		wantBool(t, "clearBootNext with a stale SetAt", ok, err, false)
		if flagCleared(t, d, "m1") {
			t.Fatal("a mismatched SetAt cleared the flag")
		}
	})

	t.Run("AllCacheStatesStorable", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		for i, state := range []CacheState{CacheWanted, CacheFetching, CacheReady, CacheFailed, CacheBroken} {
			row := cacheRowIn(CacheKey{Entry: "t" + string(rune('a'+i)), Arch: ArchAMD64}, state, t0)
			if state == CacheFailed {
				row.NextAttemptAt = t0.Add(time.Minute)
			}
			seedCache(t, d, row)
			wantCache(t, r, row.Key(), row)
		}
		// A failed row always says when it is next due.
		stuck := cacheRowIn(CacheKey{Entry: "tz", Arch: ArchAMD64}, CacheFailed, t0)
		if _, err := d.Bun().NewInsert().Model(&stuck).Exec(t.Context()); err == nil {
			t.Fatal("storing a failed row without next_attempt_at succeeded, want a CHECK violation")
		}
	})

	t.Run("MicrosecondTimes", func(t *testing.T) {
		r := NewRepository(open(t))
		got := mustRequest(t, r, k, pinOne, 0, t0.Add(1234*time.Nanosecond))
		if want := t0.Add(time.Microsecond); !got.Row.UpdatedAt.Equal(want) {
			t.Fatalf("UpdatedAt = %s, want %s", got.Row.UpdatedAt, want)
		}
		wantCache(t, r, k, cacheRowIn(k, CacheWanted, t0.Add(time.Microsecond)))
	})
}
