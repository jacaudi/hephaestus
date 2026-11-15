package fleet

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

const (
	zeroUUID       = "00000000-0000-0000-0000-000000000000"
	conflictReason = "identity conflict: this UUID was seen with another serial/product"
)

func TestRepository_SQLite(t *testing.T) { runRepositorySuite(t, dbtest.SQLite) }

// macN returns the fixture MAC aa:bb:cc:00:00:<n in hex>.
func macN(n int) string { return fmt.Sprintf("aa:bb:cc:00:00:%02x", n) }

// scalar runs a query that returns one value.
func scalar[T any](t *testing.T, d *db.DB, query string, args ...any) T {
	t.Helper()
	var v T
	if err := d.Bun().NewRaw(query, args...).Scan(t.Context(), &v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v
}

// execRaw runs a statement for its side effect.
func execRaw(t *testing.T, d *db.DB, query string, args ...any) error {
	t.Helper()
	_, err := d.Bun().ExecContext(t.Context(), query, args...)
	return err
}

// firstContact records a machine's first boot as the /boot handler does.
func firstContact(t *testing.T, r *Repository, id Identity, at time.Time) Machine {
	t.Helper()
	m, err := r.RecordBoot(t.Context(), BootRecord{
		Identity: id, Arch: ArchAMD64, Trigger: TriggerFirstContact,
		Event: BootEvent{Row: 7, Action: "hold", Reason: "awaiting approval"}, At: at,
	})
	if err != nil {
		t.Fatalf("first contact %+v: %v", id, err)
	}
	return m
}

// knownBoot records a boot of a machine the repository already holds.
func knownBoot(t *testing.T, r *Repository, rec BootRecord) Machine {
	t.Helper()
	if rec.Arch == "" {
		rec.Arch = ArchAMD64
	}
	m, err := r.RecordBoot(t.Context(), rec)
	if err != nil {
		t.Fatalf("known boot %+v: %v", rec, err)
	}
	return m
}

func originalIdentity() Identity {
	return Identity{UUID: fixtureUUID, MAC: macN(1), Serial: "S1", Product: "P1"}
}

// conflictBoot records the row-2 boot of a machine whose UUID was seen with another serial.
func conflictBoot(t *testing.T, r *Repository, machineID string, markJunk bool, at time.Time) Machine {
	t.Helper()
	return knownBoot(t, r, BootRecord{
		Identity: Identity{MAC: macN(9), Serial: "S2"}, MachineID: machineID, Conflict: true, MarkJunk: markJunk,
		Event: BootEvent{Row: 2, Action: "hold", Reason: conflictReason}, At: at,
	})
}

func lookup(t *testing.T, r *Repository, id Identity) (Machine, bool) {
	t.Helper()
	m, ok, err := r.Lookup(t.Context(), id)
	if err != nil {
		t.Fatalf("Lookup(%+v): %v", id, err)
	}
	return m, ok
}

func mustGet(t *testing.T, r *Repository, id string) Machine {
	t.Helper()
	m, err := r.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	return m
}

func mustTransition(t *testing.T, r *Repository, id string, trigger Trigger, at time.Time) Machine {
	t.Helper()
	m, err := r.Transition(t.Context(), TransitionRequest{MachineID: id, Trigger: trigger, Actor: "admin", At: at})
	if err != nil {
		t.Fatalf("%s: %v", trigger, err)
	}
	return m
}

func wantMACs(t *testing.T, got Machine, want ...string) {
	t.Helper()
	if !slices.Equal(got.MACs, want) {
		t.Fatalf("MACs = %v, want %v", got.MACs, want)
	}
}

// runRepositorySuite is the repository suite both databases run:
// TestRepository_SQLite untagged, TestRepository_Postgres under the
// integration tag.
func runRepositorySuite(t *testing.T, open func(testing.TB) *db.DB) {
	t.Helper()
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	t.Run("FirstContactCreatesDiscovered", func(t *testing.T) {
		r := NewRepository(open(t))
		m := firstContact(t, r, originalIdentity(), t0)
		if m.State != StateDiscovered || m.Junk || !m.CreatedAt.Equal(t0) || !m.LastSeenAt.Equal(t0) {
			t.Fatalf("machine = %+v, want discovered, not junk, created and seen at t0", m)
		}
		wantMACs(t, m, macN(1))
		got, ok := lookup(t, r, originalIdentity())
		if !ok || got.ID != m.ID {
			t.Fatalf("Lookup = %+v, %v; want machine %s", got, ok, m.ID)
		}
	})

	t.Run("MultiNICSameMachine", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := firstContact(t, r, originalIdentity(), t0)
		knownBoot(t, r, BootRecord{
			Identity: Identity{UUID: fixtureUUID, MAC: macN(2), Serial: "S1", Product: "P1"}, MachineID: m.ID,
			Event: BootEvent{Row: 7, Action: "exit"}, At: t0.Add(time.Second),
		})
		wantMACs(t, mustGet(t, r, m.ID), macN(1), macN(2))
		if n := scalar[int](t, d, "SELECT count(*) FROM machines"); n != 1 {
			t.Fatalf("machines = %d, want 1", n)
		}
	})

	t.Run("ZeroUUIDKeyedByMAC", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		a := firstContact(t, r, Identity{UUID: zeroUUID, MAC: macN(0xa)}, t0)
		b := firstContact(t, r, Identity{UUID: zeroUUID, MAC: macN(0xb)}, t0)
		if n := scalar[int](t, d, "SELECT count(*) FROM machines"); n != 2 {
			t.Fatalf("machines = %d, want 2", n)
		}
		if !a.Junk || !b.Junk {
			t.Fatalf("junk = %v, %v; want both true", a.Junk, b.Junk)
		}
		for _, want := range []*Machine{&a, &b} {
			got, ok := lookup(t, r, Identity{UUID: zeroUUID, MAC: want.MACs[0]})
			if !ok || got.ID != want.ID {
				t.Fatalf("Lookup(%s) = %s, %v; want %s", want.MACs[0], got.ID, ok, want.ID)
			}
		}
	})

	t.Run("JunkPatternWithoutMACUnidentifiable", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		_, err := r.RecordBoot(t.Context(), BootRecord{
			Arch: ArchAMD64, Trigger: TriggerFirstContact, Event: BootEvent{Row: 7, Action: "hold"}, At: t0,
		})
		if !errors.Is(err, ErrUnidentifiable) {
			t.Fatalf("RecordBoot error = %v, want ErrUnidentifiable", err)
		}
		if n := scalar[int](t, d, "SELECT count(*) FROM machines"); n != 0 {
			t.Fatalf("machines = %d, want 0", n)
		}
	})

	t.Run("MarkJunkOnConflict", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := firstContact(t, r, originalIdentity(), t0)
		got := conflictBoot(t, r, m.ID, true, t0.Add(10*time.Second))
		if !got.Junk {
			t.Fatal("machine not marked junk")
		}
		if !got.LastSeenAt.Equal(t0) {
			t.Fatalf("LastSeenAt = %v, want untouched %v", got.LastSeenAt, t0)
		}
		var rows []auditEventRow
		if err := d.Bun().NewSelect().Model(&rows).Where("action = ?", "alert.uuid_conflict").Scan(t.Context()); err != nil {
			t.Fatalf("read audit: %v", err)
		}
		if len(rows) != 1 || rows[0].MachineID != m.ID || rows[0].Actor != "system" {
			t.Fatalf("audit rows = %+v, want one system alert for %s", rows, m.ID)
		}
		const want = `{"uuid":"` + fixtureUUID + `","mac":"aa:bb:cc:00:00:09","stored_serial":"S1","stored_product":"P1","seen_serial":"S2","seen_product":""}`
		if rows[0].Detail != want {
			t.Fatalf("detail = %s, want %s", rows[0].Detail, want)
		}
		if n := scalar[int](t, d, "SELECT count(*) FROM machine_macs WHERE mac = ?", macN(9)); n != 0 {
			t.Fatalf("conflicting MAC attached: %d rows", n)
		}
	})

	t.Run("RepeatConflictNoSecondAlert", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := firstContact(t, r, originalIdentity(), t0)
		conflictRec := func(markJunk bool, at time.Time) BootRecord {
			return BootRecord{
				Identity: Identity{UUID: fixtureUUID, MAC: macN(9), Serial: "S2", Product: "P1"}, MachineID: m.ID,
				Conflict: true, MarkJunk: markJunk, Event: BootEvent{Row: 2, Action: "hold", Reason: conflictReason}, At: at,
			}
		}
		knownBoot(t, r, conflictRec(true, t0.Add(10*time.Second)))
		knownBoot(t, r, conflictRec(false, t0.Add(40*time.Second)))
		if n := scalar[int](t, d, "SELECT count(*) FROM audit_events WHERE action = 'alert.uuid_conflict'"); n != 1 {
			t.Fatalf("alerts = %d, want 1", n)
		}
		if n := scalar[int](t, d, "SELECT count(*) FROM boot_events WHERE machine_id = ? AND decision_row = 2", m.ID); n != 1 {
			t.Fatalf("row-2 boot events = %d, want 1 (the second hold coalesced)", n)
		}
		if n := scalar[int](t, d, "SELECT count(*) FROM boot_events WHERE machine_id = ?", m.ID); n != 2 {
			t.Fatalf("boot events = %d, want 2", n)
		}
	})

	// setupJunked is the state the next two subtests continue from: the
	// original machine, junked by a conflicting clone.
	setupJunked := func(t *testing.T, r *Repository) Machine {
		t.Helper()
		m := firstContact(t, r, originalIdentity(), t0)
		conflictBoot(t, r, m.ID, true, t0.Add(10*time.Second))
		return m
	}

	t.Run("CloneAfterJunkGetsOwnRecord", func(t *testing.T) {
		r := NewRepository(open(t))
		orig := setupJunked(t, r)
		cloneID := Identity{UUID: fixtureUUID, MAC: macN(9), Serial: "S2", Product: "P1"}
		if got, ok := lookup(t, r, cloneID); ok {
			t.Fatalf("Lookup(clone) = %+v, want not found", got)
		}
		clone := firstContact(t, r, cloneID, t0.Add(time.Minute))
		if !clone.Junk || clone.ID == orig.ID {
			t.Fatalf("clone = %+v, want a new junk machine", clone)
		}
		if got, ok := lookup(t, r, originalIdentity()); !ok || got.ID != orig.ID {
			t.Fatalf("Lookup(MAC 01) = %s, %v; want the original %s", got.ID, ok, orig.ID)
		}
		if got, ok := lookup(t, r, cloneID); !ok || got.ID != clone.ID {
			t.Fatalf("Lookup(MAC 09) = %s, %v; want the clone %s", got.ID, ok, clone.ID)
		}
	})

	t.Run("JunkUUIDMatchedByPairOnNewNIC", func(t *testing.T) {
		r := NewRepository(open(t))
		orig := setupJunked(t, r)
		got, ok := lookup(t, r, Identity{UUID: fixtureUUID, MAC: macN(0xc), Serial: "S1", Product: "P1"})
		if !ok || got.ID != orig.ID {
			t.Fatalf("Lookup = %s, %v; want the original %s", got.ID, ok, orig.ID)
		}
	})

	t.Run("MACMovesToNewMachine", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		a := firstContact(t, r, Identity{UUID: "11111111-1111-1111-1111-111111111111", MAC: macN(0x20), Serial: "S1", Product: "P1"}, t0)
		b := firstContact(t, r, Identity{UUID: "22222222-2222-2222-2222-222222222222", MAC: macN(0x20), Serial: "S2", Product: "P1"}, t0.Add(time.Second))
		if owner := scalar[string](t, d, "SELECT machine_id FROM machine_macs WHERE mac = ?", macN(0x20)); owner != b.ID {
			t.Fatalf("MAC owner = %s, want B %s", owner, b.ID)
		}
		wantMACs(t, mustGet(t, r, a.ID))
	})

	t.Run("FleetFull", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		rows := make([]Machine, MaxMachines)
		for i := range rows {
			rows[i] = Machine{
				ID: fmt.Sprintf("m%04d", i), UUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i),
				Serial: "S", Product: "P", Arch: ArchAMD64, State: StateDiscovered, CreatedAt: t0, LastSeenAt: t0,
			}
		}
		if _, err := d.Bun().NewInsert().Model(&rows).Exec(t.Context()); err != nil {
			t.Fatalf("bulk insert: %v", err)
		}
		_, err := r.RecordBoot(t.Context(), BootRecord{
			Identity: originalIdentity(), Arch: ArchAMD64, Trigger: TriggerFirstContact,
			Event: BootEvent{Row: 7, Action: "hold"}, At: t0,
		})
		if !errors.Is(err, ErrFleetFull) {
			t.Fatalf("RecordBoot error = %v, want ErrFleetFull", err)
		}
	})

	t.Run("NextBootTransition", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := firstContact(t, r, originalIdentity(), t0)
		if err := execRaw(t, d, "UPDATE machines SET state = 'installing' WHERE id = ?", m.ID); err != nil {
			t.Fatalf("set installing: %v", err)
		}
		got := knownBoot(t, r, BootRecord{
			Identity: originalIdentity(), MachineID: m.ID, Trigger: TriggerNextBoot,
			Event: BootEvent{Row: 6, Action: "exit"}, At: t0.Add(time.Minute),
		})
		if got.State != StateInstallUnconfirmed {
			t.Fatalf("State = %s, want %s", got.State, StateInstallUnconfirmed)
		}
		from := scalar[string](t, d, "SELECT from_state FROM boot_events WHERE machine_id = ? AND decision_row = 6", m.ID)
		to := scalar[string](t, d, "SELECT to_state FROM boot_events WHERE machine_id = ? AND decision_row = 6", m.ID)
		if from != "installing" || to != "install-unconfirmed" {
			t.Fatalf("boot event %q -> %q, want installing -> install-unconfirmed", from, to)
		}
	})

	t.Run("HoldCoalescing", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := firstContact(t, r, originalIdentity(), t0)
		hold := func(reason string, at time.Time) {
			t.Helper()
			knownBoot(t, r, BootRecord{
				Identity: originalIdentity(), MachineID: m.ID, Event: BootEvent{Row: 7, Action: "hold", Reason: reason}, At: at,
			})
		}
		events := func() int {
			t.Helper()
			return scalar[int](t, d, "SELECT count(*) FROM boot_events WHERE machine_id = ?", m.ID)
		}
		hold("awaiting approval", t0.Add(30*time.Second))
		hold("awaiting approval", t0.Add(59*time.Minute))
		if n := events(); n != 1 {
			t.Fatalf("after holds within the hour: %d events, want 1", n)
		}
		hold("awaiting approval", t0.Add(61*time.Minute))
		if n := events(); n != 2 {
			t.Fatalf("after a hold past the hour: %d events, want 2", n)
		}
		hold("another reason", t0.Add(62*time.Minute))
		if n := events(); n != 3 {
			t.Fatalf("after a hold with another reason: %d events, want 3", n)
		}
	})

	t.Run("BootEventsCapped", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		exit := BootEvent{Row: 6, Action: "exit"}
		m, err := r.RecordBoot(t.Context(), BootRecord{
			Identity: originalIdentity(), Arch: ArchAMD64, Trigger: TriggerFirstContact, Event: exit, At: t0,
		})
		if err != nil {
			t.Fatalf("first contact: %v", err)
		}
		var ids []int64
		for i := 1; i < BootEventsKept+2; i++ {
			knownBoot(t, r, BootRecord{Identity: originalIdentity(), MachineID: m.ID, Event: exit, At: t0.Add(time.Duration(i) * time.Second)})
			if i == 2 {
				if err := d.Bun().NewRaw("SELECT id FROM boot_events WHERE machine_id = ? ORDER BY id", m.ID).Scan(t.Context(), &ids); err != nil {
					t.Fatalf("read ids: %v", err)
				}
			}
		}
		if n := scalar[int](t, d, "SELECT count(*) FROM boot_events WHERE machine_id = ?", m.ID); n != BootEventsKept {
			t.Fatalf("boot events = %d, want %d", n, BootEventsKept)
		}
		if oldest := scalar[int64](t, d, "SELECT min(id) FROM boot_events WHERE machine_id = ?", m.ID); oldest != ids[2] {
			t.Fatalf("oldest id = %d, want %d (ids %v: the two oldest dropped)", oldest, ids[2], ids)
		}
	})

	t.Run("MACsCapped", func(t *testing.T) {
		r := NewRepository(open(t))
		m := firstContact(t, r, Identity{UUID: fixtureUUID, Serial: "S1", Product: "P1"}, t0)
		for i := range MACsKept + 1 {
			knownBoot(t, r, BootRecord{
				Identity: Identity{MAC: macN(0x40 + i)}, MachineID: m.ID, Event: BootEvent{Row: 6, Action: "exit"},
				At: t0.Add(time.Duration(i) * time.Second),
			})
		}
		got := mustGet(t, r, m.ID)
		if len(got.MACs) != MACsKept || slices.Contains(got.MACs, macN(0x40)) {
			t.Fatalf("MACs = %v, want %d entries without %s", got.MACs, MACsKept, macN(0x40))
		}
	})

	t.Run("AllStatesStorable", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := firstContact(t, r, originalIdentity(), t0)
		for _, s := range storedStates() {
			// The shape Next gives the state: cordoned remembers a prior state and
			// orphaned the bound state it left.
			var prior, orphanedFrom any
			if s == StateCordoned {
				prior = string(StateDiscovered)
			}
			if s == StateOrphaned {
				orphanedFrom = string(StateInstalled)
			}
			if err := execRaw(t, d, "UPDATE machines SET state = ?, prior_state = ?, orphaned_from = ? WHERE id = ?",
				string(s), prior, orphanedFrom, m.ID); err != nil {
				t.Errorf("store state %q: %v", s, err)
				continue
			}
			if got := mustGet(t, r, m.ID).State; got != s {
				t.Errorf("state read back as %q, want %q", got, s)
			}
		}
		// Cordoned over orphaned keeps the bound state the machine was orphaned from.
		if err := execRaw(t, d, "UPDATE machines SET state = 'cordoned', prior_state = 'orphaned', orphaned_from = 'installed' WHERE id = ?", m.ID); err != nil {
			t.Errorf("store cordoned over orphaned: %v", err)
		}
	})

	t.Run("CorruptShapeRejected", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := firstContact(t, r, originalIdentity(), t0)
		tests := []struct{ name, set string }{
			{"cordoned without prior", "state = 'cordoned', prior_state = NULL, orphaned_from = NULL"},
			{"prior on a state that is not cordoned", "state = 'discovered', prior_state = 'installed', orphaned_from = NULL"},
			{"orphaned_from on a state that is not orphaned", "state = 'discovered', prior_state = NULL, orphaned_from = 'installed'"},
			{"orphaned_from on cordoned over another state", "state = 'cordoned', prior_state = 'installed', orphaned_from = 'installed'"},
			{"orphaned without orphaned_from", "state = 'orphaned', prior_state = NULL, orphaned_from = NULL"},
		}
		for _, tc := range tests {
			if err := execRaw(t, d, "UPDATE machines SET "+tc.set+" WHERE id = ?", m.ID); err == nil {
				t.Errorf("%s: stored, want a CHECK violation", tc.name)
			}
		}
		if got := mustGet(t, r, m.ID); got.State != StateDiscovered || got.Prior != StateNone || got.OrphanedFrom != StateNone {
			t.Fatalf("machine = %s/%q/%q, want the rejected updates left it discovered", got.State, got.Prior, got.OrphanedFrom)
		}
	})

	t.Run("TransitionCordonUncordon", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := firstContact(t, r, originalIdentity(), t0)
		got := mustTransition(t, r, m.ID, TriggerCordon, t0.Add(time.Minute))
		if got.State != StateCordoned || got.Prior != StateDiscovered {
			t.Fatalf("after cordon: %s over %q, want cordoned over discovered", got.State, got.Prior)
		}
		var rows []auditEventRow
		if err := d.Bun().NewSelect().Model(&rows).Where("action = ?", "transition.cordon").Scan(t.Context()); err != nil {
			t.Fatalf("read audit: %v", err)
		}
		if len(rows) != 1 || rows[0].Detail != `{"from":"discovered","to":"cordoned"}` || rows[0].Actor != "admin" ||
			rows[0].MachineID != m.ID || !rows[0].At.Equal(t0.Add(time.Minute)) {
			t.Fatalf("cordon audit = %+v", rows)
		}
		got = mustTransition(t, r, m.ID, TriggerUncordon, t0.Add(2*time.Minute))
		if got.State != StateDiscovered || got.Prior != StateNone {
			t.Fatalf("after uncordon: %s over %q, want discovered over nothing", got.State, got.Prior)
		}
	})

	t.Run("TransitionForgetDeletes", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := firstContact(t, r, originalIdentity(), t0)
		mustTransition(t, r, m.ID, TriggerForget, t0.Add(time.Minute))
		if _, err := r.Get(t.Context(), m.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get error = %v, want ErrNotFound", err)
		}
		if n := scalar[int](t, d, "SELECT count(*) FROM machine_macs WHERE machine_id = ?", m.ID); n != 0 {
			t.Fatalf("machine_macs rows = %d, want 0", n)
		}
		var rows []auditEventRow
		if err := d.Bun().NewSelect().Model(&rows).Where("action = ?", "transition.forget").Scan(t.Context()); err != nil {
			t.Fatalf("read audit: %v", err)
		}
		if len(rows) != 1 || rows[0].MachineID != m.ID || rows[0].Detail != `{"from":"discovered","to":"none"}` {
			t.Fatalf("forget audit = %+v", rows)
		}
	})

	t.Run("TransitionInvalidNoWrite", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := firstContact(t, r, originalIdentity(), t0)
		mustTransition(t, r, m.ID, TriggerReject, t0.Add(time.Minute))
		_, err := r.Transition(t.Context(), TransitionRequest{
			MachineID: m.ID, Trigger: TriggerApprove, Guard: Guard{HostExists: true, HostUnbound: true}, Actor: "admin", At: t0.Add(2 * time.Minute),
		})
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("approve error = %v, want ErrInvalidTransition", err)
		}
		if n := scalar[int](t, d, "SELECT count(*) FROM audit_events WHERE action = 'transition.approve'"); n != 0 {
			t.Fatalf("approve audit rows = %d, want 0", n)
		}
		if n := scalar[int](t, d, "SELECT count(*) FROM audit_events"); n != 1 {
			t.Fatalf("audit rows = %d, want only the reject", n)
		}
		if got := mustGet(t, r, m.ID).State; got != StateRejected {
			t.Fatalf("State = %s, want it left rejected", got)
		}
	})

	t.Run("TransitionJunkGuardFromRecord", func(t *testing.T) {
		r := NewRepository(open(t))
		m := firstContact(t, r, Identity{UUID: zeroUUID, MAC: macN(0xa)}, t0)
		_, err := r.Transition(t.Context(), TransitionRequest{
			MachineID: m.ID, Trigger: TriggerApprove, Guard: Guard{HostExists: true, HostUnbound: true}, Actor: "admin", At: t0,
		})
		te, ok := errors.AsType[*TransitionError](err)
		if !ok || te.Reason != reasonJunkUUID {
			t.Fatalf("approve error = %v, want a %s TransitionError", err, reasonJunkUUID)
		}
	})

	t.Run("GetNotFound", func(t *testing.T) {
		r := NewRepository(open(t))
		if _, err := r.Get(t.Context(), "nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get error = %v, want ErrNotFound", err)
		}
	})
}

// TestRecordBoot_Refused covers the RecordBoot refusals that need no stored data.
func TestRecordBoot_Refused(t *testing.T) {
	r := NewRepository(dbtest.SQLite(t))
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		rec  BootRecord
		want error
	}{
		{"new machine needs first contact", BootRecord{Identity: originalIdentity(), Arch: ArchAMD64, Trigger: TriggerNextBoot, At: t0}, ErrInvalidTransition},
		{"new machine without trigger", BootRecord{Identity: originalIdentity(), Arch: ArchAMD64, At: t0}, ErrInvalidTransition},
		{"unknown machine", BootRecord{Identity: originalIdentity(), Arch: ArchAMD64, MachineID: "nope", At: t0}, ErrNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.RecordBoot(t.Context(), tc.rec); !errors.Is(err, tc.want) {
				t.Fatalf("RecordBoot error = %v, want %v", err, tc.want)
			}
		})
	}
}
