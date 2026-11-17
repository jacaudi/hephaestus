package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestBootNextRepository_SQLite(t *testing.T) {
	runBootNextSuite(t, dbtest.SQLite, dbtest.SQLitePair)
}

// bootNextMachine records the first contact of the n-th fixture machine: amd64,
// its own UUID and the MAC macN(n).
func bootNextMachine(t *testing.T, r *Repository, n int, at time.Time) Machine {
	t.Helper()
	uuid := fmt.Sprintf("4c4c4544-0042-3510-8052-b4c04f4e4d%02x", n)
	return firstContact(t, r, Identity{UUID: uuid, MAC: macN(n), Serial: "S", Product: "P"}, at)
}

// bootOf is a known, non-conflict boot of m that decided row.
func bootOf(m Machine, row int, action string, at time.Time) BootRecord {
	return BootRecord{Identity: Identity{MAC: m.MACs[0]}, Arch: m.Arch, MachineID: m.ID, Event: BootEvent{Row: row, Action: action}, At: at}
}

func mustSetBootNext(t *testing.T, r *Repository, id, entry string, at time.Time) SetBootNextResult {
	t.Helper()
	res, err := r.SetBootNext(t.Context(), SetBootNextRequest{MachineID: id, Entry: entry, Pin: pinOne, At: at})
	if err != nil {
		t.Fatalf("SetBootNext(%s, %q): %v", id, entry, err)
	}
	return res
}

// wantFlag fails unless the stored machine's boot-next is entry, set at setAt.
func wantFlag(t *testing.T, r *Repository, id, entry string, setAt time.Time) {
	t.Helper()
	if m := mustGet(t, r, id); m.BootNext != entry || !m.BootNextSetAt.Equal(setAt) {
		t.Fatalf("boot-next of %s = %q set %v; want %q set %v", id, m.BootNext, m.BootNextSetAt, entry, setAt)
	}
}

// bootEvents counts the machine's stored boot events that decided row.
func bootEvents(t *testing.T, d *db.DB, id string, row int) int {
	t.Helper()
	return scalar[int](t, d, "SELECT count(*) FROM boot_events WHERE machine_id = ? AND decision_row = ?", id, row)
}

// serveOnce is what serve does at one boot of m: read the flag and, when one
// is set, serve it by compare-and-clear. A flag set again in between makes the
// clear lose; serve reads again and tries once more. saw reports whether a flag
// was set, and lost how many clears lost: two means serve falls through.
func serveOnce(ctx context.Context, r *Repository, m Machine, at time.Time) (saw bool, lost int, err error) {
	for range 2 {
		cur, err := r.Get(ctx, m.ID)
		if err != nil {
			return saw, lost, err
		}
		if cur.BootNext == "" {
			return saw, lost, nil
		}
		saw = true
		rec := bootOf(m, 3, "utility", at)
		rec.ClearBootNext = &BootNextClear{Entry: cur.BootNext, SetAt: cur.BootNextSetAt, Reason: ClearServed}
		if _, err := r.RecordBoot(ctx, rec); !errors.Is(err, ErrBootNextChanged) {
			return saw, lost, err
		}
		lost++
	}
	return saw, lost, nil
}

// runConcurrentSetAndServe has the CLI set a boot-next over and over while
// serve serves it, each through its own handle on one database, and checks that
// no flag is served twice and nothing fails.
func runConcurrentSetAndServe(t *testing.T, pair func(testing.TB) (*db.DB, *db.DB)) {
	t.Helper()
	const rounds = 200
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	serveDB, cliDB := pair(t)
	cli, serve := NewRepository(cliDB), NewRepository(serveDB)
	m := bootNextMachine(t, cli, 1, t0)

	var (
		wg                  sync.WaitGroup
		cliErr, serveErr    error
		lostOnce, lostTwice int
		start               = make(chan struct{})
		cliDone, serveDone  = make(chan struct{}), make(chan struct{})
		at                  = func(i int) time.Time { return t0.Add(time.Duration(i) * time.Millisecond) }
	)
	// The CLI sets the next flag as serve starts each boot, so serve's read and
	// its clear overlap the CLI's set instead of the two taking turns.
	wg.Go(func() {
		defer close(cliDone)
		for i := range rounds {
			req := SetBootNextRequest{MachineID: m.ID, Entry: "tshell", Pin: pinOne, At: at(i)}
			if _, cliErr = cli.SetBootNext(t.Context(), req); cliErr != nil {
				return
			}
			select {
			case <-start:
			case <-serveDone:
				return
			}
		}
	})
	wg.Go(func() {
		defer close(serveDone)
		// Boots that find no flag do not count: serve keeps going until it has
		// spent its rounds on flags, or the CLI is done.
		for served := 0; served < rounds; {
			select {
			case start <- struct{}{}:
			case <-cliDone:
				return
			}
			saw, lost, err := serveOnce(t.Context(), serve, m, at(served))
			if serveErr = err; err != nil {
				return
			}
			switch lost {
			case 1:
				lostOnce++
			case 2:
				lostTwice++
			}
			if saw {
				served++
			}
		}
	})
	wg.Wait()
	if cliErr != nil || serveErr != nil {
		t.Fatalf("cli error = %v, serve error = %v; want neither", cliErr, serveErr)
	}

	served := auditsOf(t, serveDB, actionBootNextCleared)
	if len(served) > rounds {
		t.Fatalf("%d flags served from %d serve attempts", len(served), rounds)
	}
	seen := make(map[string]bool, len(served))
	for _, a := range served {
		var d clearDetail
		if err := json.Unmarshal([]byte(a.Detail), &d); err != nil {
			t.Fatalf("audit detail %q: %v", a.Detail, err)
		}
		if d.Reason != ClearServed || seen[d.SetAt] {
			t.Fatalf("audit detail %q: reason %q, set_at seen before: %v; want a served flag no other clear served", a.Detail, d.Reason, seen[d.SetAt])
		}
		seen[d.SetAt] = true
	}
	if n := bootEvents(t, serveDB, m.ID, 3); n != len(served) {
		t.Fatalf("%d row-3 boot events for %d served flags", n, len(served))
	}
	if n := len(auditsOf(t, serveDB, "boot_next.set")); n != rounds {
		t.Fatalf("%d boot_next.set audits, want %d", n, rounds)
	}
	if final := mustGet(t, serve, m.ID); final.BootNext != "" && !final.BootNextSetAt.Equal(at(rounds-1)) {
		t.Fatalf("final flag set at %v, want the last set, at %v", final.BootNextSetAt, at(rounds-1))
	}
	t.Logf("served %d of %d flags; %d clears lost once, %d twice (serve falls through)", len(served), rounds, lostOnce, lostTwice)
}

// runBootNextSuite is the boot-next suite both databases run:
// TestBootNextRepository_SQLite untagged, TestBootNextRepository_Postgres under
// the integration tag. pair opens two handles on one database, the CLI's and serve's.
func runBootNextSuite(t *testing.T, open func(testing.TB) *db.DB, pair func(testing.TB) (*db.DB, *db.DB)) {
	t.Helper()
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	k := CacheKey{Entry: "tshell", Arch: ArchAMD64}

	t.Run("SetBootNextSetsFlag", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)

		got := mustSetBootNext(t, r, m.ID, "tshell", t0)
		if got.Machine.BootNext != "tshell" || !got.Machine.BootNextSetAt.Equal(t0) {
			t.Fatalf("Machine = %+v, want boot-next tshell set at t0", got.Machine)
		}
		wantFlag(t, r, m.ID, "tshell", t0)
		wantOneAudit(t, d, "boot_next.set", "cli", m.ID, `{"entry":"tshell","set_at":"2026-11-14T12:00:00Z","replaced":""}`)
		want := cacheRowIn(k, CacheWanted, t0)
		want.Priority = 1
		if got.Fetch.Rearmed || !sameCacheRow(got.Fetch.Row, want) {
			t.Fatalf("Fetch = %+v; want %+v, not rearmed", got.Fetch, want)
		}
		wantCache(t, r, k, want)
	})

	t.Run("SetBootNextReplaces", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)

		mustSetBootNext(t, r, m.ID, "tshell", t0)
		got := mustSetBootNext(t, r, m.ID, "tlive", t0.Add(time.Minute))
		if got.Machine.BootNext != "tlive" || !got.Machine.BootNextSetAt.Equal(t0.Add(time.Minute)) {
			t.Fatalf("Machine = %+v, want boot-next tlive set at t0+1m", got.Machine)
		}
		wantFlag(t, r, m.ID, "tlive", t0.Add(time.Minute))
		audits := auditsOf(t, d, "boot_next.set")
		const second = `{"entry":"tlive","set_at":"2026-11-14T12:01:00Z","replaced":"tshell"}`
		if len(audits) != 2 || audits[1].Detail != second {
			t.Fatalf("boot_next.set audits = %+v; want two, the second with detail %s", audits, second)
		}
	})

	t.Run("SetBootNextRearmsBroken", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)
		seed := cacheRowIn(k, CacheBroken, t0)
		seed.Attempts, seed.LastError = 10, "sha256 mismatch"
		seedCache(t, d, seed)

		got := mustSetBootNext(t, r, m.ID, "tshell", t0.Add(time.Minute))
		want := cacheRowIn(k, CacheWanted, t0.Add(time.Minute))
		want.Priority = 1
		if !got.Fetch.Rearmed || got.Fetch.PreviousError != "sha256 mismatch" || !sameCacheRow(got.Fetch.Row, want) {
			t.Fatalf("Fetch = %+v; want %+v, rearmed, previous error %q", got.Fetch, want, "sha256 mismatch")
		}
		wantCache(t, r, k, want)
	})

	t.Run("SetBootNextUnknownMachine", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		_, err := r.SetBootNext(t.Context(), SetBootNextRequest{MachineID: "nope", Entry: "tshell", Pin: pinOne, At: t0})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("SetBootNext error = %v, want ErrNotFound", err)
		}
		if audits := auditRows(t, d); len(audits) != 0 {
			t.Fatalf("audit = %+v, want none", audits)
		}
		if rows, err := r.CacheRows(t.Context()); err != nil || len(rows) != 0 {
			t.Fatalf("CacheRows = %+v, %v; want none", rows, err)
		}
	})

	t.Run("ClearBootNextCompare", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)
		mustSetBootNext(t, r, m.ID, "tshell", t0)

		req := ClearBootNextRequest{MachineID: m.ID, Entry: "tshell", SetAt: t0, Reason: ClearCLI, Actor: "cli", At: t0.Add(time.Minute)}
		got, err := r.ClearBootNext(t.Context(), req)
		wantBool(t, "ClearBootNext", got, err, true)
		if !flagCleared(t, d, m.ID) {
			t.Fatal("the flag is still set")
		}
		wantOneAudit(t, d, "boot_next.cleared", "cli", m.ID, `{"entry":"tshell","set_at":"2026-11-14T12:00:00Z","reason":"cli"}`)

		got, err = r.ClearBootNext(t.Context(), req)
		wantBool(t, "repeated ClearBootNext", got, err, false)
		if n := len(auditsOf(t, d, "boot_next.cleared")); n != 1 {
			t.Fatalf("%d boot_next.cleared audits after the repeat, want 1", n)
		}
	})

	t.Run("ClearBootNextStale", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)
		mustSetBootNext(t, r, m.ID, "tshell", t0)
		mustSetBootNext(t, r, m.ID, "tshell", t0.Add(time.Minute))

		req := ClearBootNextRequest{MachineID: m.ID, Entry: "tshell", SetAt: t0, Reason: ClearCLI, Actor: "cli", At: t0.Add(2 * time.Minute)}
		got, err := r.ClearBootNext(t.Context(), req)
		wantBool(t, "ClearBootNext of a flag set again", got, err, false)
		wantFlag(t, r, m.ID, "tshell", t0.Add(time.Minute))
	})

	t.Run("ListBootNext", func(t *testing.T) {
		r := NewRepository(open(t))
		a := bootNextMachine(t, r, 1, t0)
		b := bootNextMachine(t, r, 2, t0)
		bootNextMachine(t, r, 3, t0)
		mustSetBootNext(t, r, a.ID, "tlive", t0.Add(time.Minute))
		mustSetBootNext(t, r, b.ID, "tshell", t0)

		got, err := r.ListBootNext(t.Context())
		if err != nil || len(got) != 2 {
			t.Fatalf("ListBootNext = %+v, %v; want two machines", got, err)
		}
		if got[0].ID != b.ID || got[1].ID != a.ID {
			t.Fatalf("ListBootNext order = %s, %s; want %s (set at t0), then %s", got[0].ID, got[1].ID, b.ID, a.ID)
		}
		wantMACs(t, got[0], macN(2))
		wantMACs(t, got[1], macN(1))
		if got[0].BootNext != "tshell" || !got[0].BootNextSetAt.Equal(t0) {
			t.Fatalf("first machine = %+v, want boot-next tshell set at t0", got[0])
		}
	})

	t.Run("ResolveByMAC", func(t *testing.T) {
		r := NewRepository(open(t))
		m := bootNextMachine(t, r, 0xab, t0)
		bootNextMachine(t, r, 2, t0)

		for _, ref := range []string{macN(0xab), "AA:BB:CC:00:00:AB", "aa-bb-cc-00-00-ab"} {
			got, err := r.ResolveMachine(t.Context(), ref)
			if err != nil || got.ID != m.ID {
				t.Errorf("ResolveMachine(%q) = %+v, %v; want machine %s", ref, got, err, m.ID)
				continue
			}
			wantMACs(t, got, macN(0xab))
		}
	})

	t.Run("ResolveByUUID", func(t *testing.T) {
		r := NewRepository(open(t))
		m := bootNextMachine(t, r, 1, t0)
		bootNextMachine(t, r, 2, t0)

		for _, ref := range []string{m.UUID, "4C4C4544-0042-3510-8052-B4C04F4E4D01"} {
			got, err := r.ResolveMachine(t.Context(), ref)
			if err != nil || got.ID != m.ID {
				t.Errorf("ResolveMachine(%q) = %+v, %v; want machine %s", ref, got, err, m.ID)
				continue
			}
			wantMACs(t, got, macN(1))
		}
	})

	t.Run("ResolveUnknown", func(t *testing.T) {
		r := NewRepository(open(t))
		bootNextMachine(t, r, 1, t0)

		for _, ref := range []string{"nope", "", macN(9), "4c4c4544-0042-3510-8052-b4c04f4e4dff"} {
			if _, err := r.ResolveMachine(t.Context(), ref); !errors.Is(err, ErrNotFound) {
				t.Errorf("ResolveMachine(%q) error = %v, want ErrNotFound", ref, err)
			}
		}
	})

	t.Run("ResolveAmbiguousJunkUUID", func(t *testing.T) {
		r := NewRepository(open(t))
		a := firstContact(t, r, Identity{UUID: zeroUUID, MAC: macN(0xa)}, t0)
		firstContact(t, r, Identity{UUID: zeroUUID, MAC: macN(0xb)}, t0)

		_, err := r.ResolveMachine(t.Context(), zeroUUID)
		amb, ok := errors.AsType[*AmbiguousError](err)
		if !ok {
			t.Fatalf("ResolveMachine(%s) error = %v, want *AmbiguousError", zeroUUID, err)
		}
		if want := []string{macN(0xa), macN(0xb)}; !slices.Equal(amb.MACs, want) {
			t.Fatalf("MACs = %v, want %v", amb.MACs, want)
		}
		const want = "00000000-0000-0000-0000-000000000000 matches 2 machines; name one by MAC: aa:bb:cc:00:00:0a, aa:bb:cc:00:00:0b"
		if err.Error() != want {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}

		got, err := r.ResolveMachine(t.Context(), macN(0xa))
		if err != nil || got.ID != a.ID {
			t.Fatalf("ResolveMachine(%s) = %+v, %v; want machine %s", macN(0xa), got, err, a.ID)
		}
	})

	t.Run("RecordBootClearsServed", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)
		mustSetBootNext(t, r, m.ID, "tshell", t0)

		rec := bootOf(m, 3, "utility", t0.Add(time.Minute))
		rec.ClearBootNext = &BootNextClear{Entry: "tshell", SetAt: t0, Reason: ClearServed}
		got := knownBoot(t, r, rec)
		if got.BootNext != "" || !flagCleared(t, d, m.ID) {
			t.Fatalf("Machine = %+v, want the flag cleared", got)
		}
		wantOneAudit(t, d, "boot_next.cleared", "system", m.ID, `{"entry":"tshell","set_at":"2026-11-14T12:00:00Z","reason":"served"}`)
		if n := bootEvents(t, d, m.ID, 3); n != 1 {
			t.Fatalf("%d row-3 boot events, want 1", n)
		}
	})

	t.Run("RecordBootClearRace", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)
		mustSetBootNext(t, r, m.ID, "tshell", t0)
		mustSetBootNext(t, r, m.ID, "tshell", t0.Add(time.Minute))

		rec := bootOf(m, 3, "utility", t0.Add(2*time.Minute))
		rec.ClearBootNext = &BootNextClear{Entry: "tshell", SetAt: t0, Reason: ClearServed}
		if _, err := r.RecordBoot(t.Context(), rec); !errors.Is(err, ErrBootNextChanged) {
			t.Fatalf("RecordBoot error = %v, want ErrBootNextChanged", err)
		}
		if n := bootEvents(t, d, m.ID, 3); n != 0 {
			t.Fatalf("%d row-3 boot events after the lost race, want none", n)
		}
		wantFlag(t, r, m.ID, "tshell", t0.Add(time.Minute))
		if got := mustGet(t, r, m.ID); !got.LastSeenAt.Equal(t0) {
			t.Fatalf("last_seen_at = %v after the lost race, want the transaction rolled back to t0", got.LastSeenAt)
		}
		if audits := auditsOf(t, d, "boot_next.cleared"); len(audits) != 0 {
			t.Fatalf("boot_next.cleared audits = %+v, want none", audits)
		}
	})

	t.Run("RecordBootWantFetch", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)

		rec := bootOf(m, 6, "exit", t0.Add(time.Minute))
		rec.WantFetch = &FetchWant{Key: k, Pin: pinOne}
		knownBoot(t, r, rec)
		want := cacheRowIn(k, CacheWanted, t0.Add(time.Minute))
		want.Priority = 1
		wantCache(t, r, k, want)
		if n := bootEvents(t, d, m.ID, 6); n != 1 {
			t.Fatalf("%d row-6 boot events, want 1", n)
		}
	})

	t.Run("RecordBootFieldsNeedKnownBoot", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)
		first := BootRecord{
			Identity: Identity{UUID: fixtureUUID, MAC: macN(9), Serial: "S", Product: "P"}, Arch: ArchAMD64, Trigger: TriggerFirstContact,
			Event: BootEvent{Row: 7, Action: "hold"}, At: t0,
		}
		conflict := bootOf(m, 2, "hold", t0)
		conflict.Conflict = true

		tests := []struct {
			name string
			rec  BootRecord
			set  func(*BootRecord)
		}{
			{"want fetch on first contact", first, func(r *BootRecord) { r.WantFetch = &FetchWant{Key: k, Pin: pinOne} }},
			{"clear on first contact", first, func(r *BootRecord) { r.ClearBootNext = &BootNextClear{Entry: "tshell", SetAt: t0, Reason: ClearServed} }},
			{"want fetch on a conflict", conflict, func(r *BootRecord) { r.WantFetch = &FetchWant{Key: k, Pin: pinOne} }},
			{"clear on a conflict", conflict, func(r *BootRecord) { r.ClearBootNext = &BootNextClear{Entry: "tshell", SetAt: t0, Reason: ClearServed} }},
		}
		for i := range tests {
			tc := &tests[i]
			tc.set(&tc.rec)
			if _, err := r.RecordBoot(t.Context(), tc.rec); !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s: RecordBoot error = %v, want ErrInvalidTransition", tc.name, err)
			}
		}
		if n := scalar[int](t, d, "SELECT count(*) FROM machines"); n != 1 {
			t.Fatalf("machines = %d, want 1: a refused first contact creates nothing", n)
		}
		if rows, err := r.CacheRows(t.Context()); err != nil || len(rows) != 0 {
			t.Fatalf("CacheRows = %+v, %v; want none", rows, err)
		}
	})

	t.Run("ConcurrentSetAndServe", func(t *testing.T) { runConcurrentSetAndServe(t, pair) })
}
