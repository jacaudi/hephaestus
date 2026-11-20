package fleet

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"go.uber.org/goleak"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

const (
	labCluster     = "lab"
	labHost        = "cp-01"
	plainSchematic = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"
	talosVersion   = "v1.12.11"
	adoptedUUID    = "4c4c4544-0042-3510-8052-b4c04f4e4d99"
)

func TestBinding_SQLite(t *testing.T) { runBindingSuite(t, dbtest.SQLite, dbtest.SQLitePair) }

// labBinding is the fixture binding of host in cluster lab.
func labBinding(host string) Binding {
	return Binding{ClusterID: labCluster, Host: host, TalosSchematic: plainSchematic, TalosVersion: talosVersion}
}

// approveReq approves machine id into host of cluster lab.
func approveReq(id, host string, at time.Time) TransitionRequest {
	b := labBinding(host)
	return TransitionRequest{MachineID: id, Trigger: TriggerApprove, Guard: guardOK, Bind: &b, Actor: "admin", At: at}
}

// mustApprove approves machine id into host of cluster lab.
func mustApprove(t *testing.T, r *Repository, id, host string, at time.Time) Machine {
	t.Helper()
	m, err := r.Transition(t.Context(), approveReq(id, host, at))
	if err != nil {
		t.Fatalf("approve %s into %s: %v", id, host, err)
	}
	return m
}

// wantBound fails unless m is bound to host of cluster lab with the fixture Talos tuple.
func wantBound(t *testing.T, m Machine, host string) {
	t.Helper()
	got := [4]string{deref(m.ClusterID), deref(m.HostName), deref(m.TalosSchematic), deref(m.TalosVersion)}
	if want := [4]string{labCluster, host, plainSchematic, talosVersion}; got != want {
		t.Fatalf("binding of %s = %q, want %q", m.ID, got, want)
	}
}

// wantUnbound fails unless all four binding columns of m are NULL.
func wantUnbound(t *testing.T, m Machine) {
	t.Helper()
	if m.ClusterID != nil || m.HostName != nil || m.TalosSchematic != nil || m.TalosVersion != nil {
		t.Fatalf("binding of %s = %q %q %q %q, want all NULL",
			m.ID, deref(m.ClusterID), deref(m.HostName), deref(m.TalosSchematic), deref(m.TalosVersion))
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// inTx runs fn in a transaction on d.
func inTx(ctx context.Context, d *db.DB, fn func(ctx context.Context, tx bun.Tx) error) error {
	return d.Bun().RunInTx(ctx, nil, fn)
}

// adopt runs AdoptByUUIDTx in a transaction of its own.
func adopt(ctx context.Context, d *db.DB, req AdoptByUUIDRequest) (Machine, error) {
	var m Machine
	err := inTx(ctx, d, func(ctx context.Context, tx bun.Tx) error {
		var err error
		m, err = AdoptByUUIDTx(ctx, tx, req)
		return err
	})
	return m, err
}

func adoptReq(uuid, host string, at time.Time) AdoptByUUIDRequest {
	return AdoptByUUIDRequest{UUID: uuid, Arch: ArchAMD64, Bind: labBinding(host), Guard: guardOK, Actor: "admin", At: at}
}

func mustAdopt(t *testing.T, d *db.DB, req AdoptByUUIDRequest) Machine {
	t.Helper()
	m, err := adopt(t.Context(), d, req)
	if err != nil {
		t.Fatalf("AdoptByUUIDTx(%+v): %v", req, err)
	}
	return m
}

// firstReport is a boot of m that carries its first report: serial and product.
func firstReport(m Machine, serial string, at time.Time) BootRecord {
	return BootRecord{
		Identity: Identity{UUID: m.UUID, MAC: macN(1), Serial: serial, Product: "P1"}, Arch: ArchAMD64, MachineID: m.ID,
		ObserveIdentity: true, Event: BootEvent{Row: 7, Action: "exit"}, At: at,
	}
}

// together runs f(0) and f(1) at once, each released only when both are ready.
func together(f func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 2 {
		wg.Go(func() {
			<-start
			f(i)
		})
	}
	close(start)
	wg.Wait()
}

// wantTransitionReason fails unless err is a *TransitionError with reason.
func wantTransitionReason(t *testing.T, err error, reason string) {
	t.Helper()
	te, ok := errors.AsType[*TransitionError](err)
	if !ok || te.Reason != reason {
		t.Fatalf("error = %v, want a %s TransitionError", err, reason)
	}
}

// runBindingSuite is the bindings suite both databases run: TestBinding_SQLite
// untagged, TestBinding_Postgres under the integration tag. pair opens two
// handles on one database.
func runBindingSuite(t *testing.T, open func(testing.TB) *db.DB, pair func(testing.TB) (*db.DB, *db.DB)) {
	t.Helper()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	t.Run("TransitionTx", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)
		err := inTx(t.Context(), d, func(ctx context.Context, tx bun.Tx) error {
			got, err := TransitionTx(ctx, tx, TransitionRequest{MachineID: m.ID, Trigger: TriggerCordon, Actor: "admin", At: t0.Add(time.Minute)})
			if err == nil && (got.State != StateCordoned || got.Prior != StateDiscovered) {
				t.Errorf("TransitionTx = %s over %q, want cordoned over discovered", got.State, got.Prior)
			}
			return err
		})
		if err != nil {
			t.Fatalf("cordon in a caller's transaction: %v", err)
		}
		if got := mustGet(t, r, m.ID); got.State != StateCordoned {
			t.Fatalf("State = %s, want cordoned", got.State)
		}
		wantOneAudit(t, d, "transition.cordon", "admin", m.ID, `{"from":"discovered","to":"cordoned"}`)

		errRollBack := errors.New("roll back")
		err = inTx(t.Context(), d, func(ctx context.Context, tx bun.Tx) error {
			if _, err := TransitionTx(ctx, tx, TransitionRequest{MachineID: m.ID, Trigger: TriggerUncordon, Actor: "admin", At: t0.Add(2 * time.Minute)}); err != nil {
				return err
			}
			return errRollBack
		})
		if !errors.Is(err, errRollBack) {
			t.Fatalf("transaction error = %v, want the roll-back error", err)
		}
		if got := mustGet(t, r, m.ID); got.State != StateCordoned || got.Prior != StateDiscovered {
			t.Fatalf("after the rolled-back uncordon: %s over %q, want cordoned over discovered", got.State, got.Prior)
		}
		if n := len(auditsOf(t, d, "transition.uncordon")); n != 0 {
			t.Fatalf("uncordon audits = %d, want 0 after the rollback", n)
		}
	})

	t.Run("InsertAudit", func(t *testing.T) {
		d := open(t)
		m := bootNextMachine(t, NewRepository(d), 1, t0)
		ev := AuditEvent{At: t0, Actor: ActorCLI, Action: "test.insert", MachineID: m.ID, Detail: map[string]string{"k": "v"}}
		if err := InsertAudit(t.Context(), d.Bun(), ev); err != nil {
			t.Fatalf("InsertAudit: %v", err)
		}
		got := auditsOf(t, d, "test.insert")
		if len(got) != 1 || got[0].Actor != "cli" || got[0].MachineID != m.ID || got[0].Detail != `{"k":"v"}` || !got[0].At.Equal(t0) {
			t.Fatalf("audits = %+v, want one by cli for %s with detail {\"k\":\"v\"} at t0", got, m.ID)
		}
	})

	t.Run("Audit", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)
		ev := AuditEvent{At: t0, Actor: ActorSystem, Action: "test.insert", MachineID: m.ID, Detail: map[string]string{"k": "v"}}
		if err := InsertAudit(t.Context(), d.Bun(), ev); err != nil {
			t.Fatalf("InsertAudit: %v", err)
		}
		ev.Action = "test.repository"
		if err := r.Audit(t.Context(), ev); err != nil {
			t.Fatalf("Audit: %v", err)
		}
		viaInsert, viaRepo := auditsOf(t, d, "test.insert"), auditsOf(t, d, "test.repository")
		if len(viaInsert) != 1 || len(viaRepo) != 1 {
			t.Fatalf("audits = %+v and %+v, want one each", viaInsert, viaRepo)
		}
		a, b := viaInsert[0], viaRepo[0]
		if a.Actor != b.Actor || a.MachineID != b.MachineID || a.Detail != b.Detail || !a.At.Equal(b.At) {
			t.Fatalf("Audit stored %+v, want InsertAudit's %+v (but for the action)", b, a)
		}
	})

	t.Run("ListMachines", func(t *testing.T) {
		r := NewRepository(open(t))
		// Created out of numbering order, so neither the id nor the insert order is the answer.
		third := bootNextMachine(t, r, 3, t0.Add(2*time.Second))
		first := bootNextMachine(t, r, 1, t0)
		second := bootNextMachine(t, r, 2, t0.Add(time.Second))
		got, err := r.ListMachines(t.Context())
		if err != nil {
			t.Fatalf("ListMachines: %v", err)
		}
		want := []Machine{first, second, third}
		if len(got) != len(want) {
			t.Fatalf("ListMachines = %d machines, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i].ID != want[i].ID || !slices.Equal(got[i].MACs, want[i].MACs) || len(got[i].MACs) != 1 {
				t.Fatalf("machine %d = %s MACs %v, want %s MACs %v", i, got[i].ID, got[i].MACs, want[i].ID, want[i].MACs)
			}
		}
	})

	t.Run("ApproveSetsBinding", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)
		got := mustApprove(t, r, m.ID, labHost, t0.Add(time.Minute))
		if got.State != StateAssigned {
			t.Fatalf("State = %s, want assigned", got.State)
		}
		wantBound(t, mustGet(t, r, m.ID), labHost)
		wantOneAudit(t, d, "transition.approve", "admin", m.ID, `{"from":"discovered","to":"assigned"}`)
	})

	t.Run("ApproveNeedsBinding", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)
		_, err := r.Transition(t.Context(), TransitionRequest{MachineID: m.ID, Trigger: TriggerApprove, Guard: guardOK, Actor: "admin", At: t0})
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("approve without Bind error = %v, want ErrInvalidTransition", err)
		}
		wantUnbound(t, mustGet(t, r, m.ID))
		if n := len(auditsOf(t, d, "transition.approve")); n != 0 {
			t.Fatalf("approve audits = %d, want 0", n)
		}
	})

	t.Run("UnassignClearsBinding", func(t *testing.T) {
		r := NewRepository(open(t))
		m := bootNextMachine(t, r, 1, t0)
		mustApprove(t, r, m.ID, labHost, t0)
		got := mustTransition(t, r, m.ID, TriggerUnassign, t0.Add(time.Minute))
		if got.State != StateDiscovered {
			t.Fatalf("State = %s, want discovered", got.State)
		}
		wantUnbound(t, mustGet(t, r, m.ID))
	})

	t.Run("UnbindClearsBinding", func(t *testing.T) {
		r := NewRepository(open(t))
		m := bootNextMachine(t, r, 1, t0)
		mustApprove(t, r, m.ID, labHost, t0)
		mustTransition(t, r, m.ID, TriggerHostLeft, t0.Add(time.Minute))
		got := mustTransition(t, r, m.ID, TriggerUnbind, t0.Add(2*time.Minute))
		if got.State != StateDiscovered {
			t.Fatalf("State = %s, want discovered", got.State)
		}
		wantUnbound(t, mustGet(t, r, m.ID))
	})

	t.Run("HostLeftKeepsBinding", func(t *testing.T) {
		r := NewRepository(open(t))
		m := bootNextMachine(t, r, 1, t0)
		mustApprove(t, r, m.ID, labHost, t0)
		mustTransition(t, r, m.ID, TriggerHostLeft, t0.Add(time.Minute))
		got := mustGet(t, r, m.ID)
		if got.State != StateOrphaned {
			t.Fatalf("State = %s, want orphaned", got.State)
		}
		wantBound(t, got, labHost)
	})

	t.Run("CordonKeepsBinding", func(t *testing.T) {
		r := NewRepository(open(t))
		m := bootNextMachine(t, r, 1, t0)
		mustApprove(t, r, m.ID, labHost, t0)
		mustTransition(t, r, m.ID, TriggerCordon, t0.Add(time.Minute))
		wantBound(t, mustGet(t, r, m.ID), labHost)
		mustTransition(t, r, m.ID, TriggerUncordon, t0.Add(2*time.Minute))
		wantBound(t, mustGet(t, r, m.ID), labHost)
	})

	t.Run("ConcurrentApproveOneHost", func(t *testing.T) {
		serveDB, cliDB := pair(t)
		defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
		repos := [2]*Repository{NewRepository(serveDB), NewRepository(cliDB)}
		for round := range 10 {
			host := fmt.Sprintf("cp-%02d", round)
			ids := [2]string{
				bootNextMachine(t, repos[0], 2*round+1, t0).ID,
				bootNextMachine(t, repos[0], 2*round+2, t0).ID,
			}
			var errs [2]error
			together(func(i int) {
				// Both callers saw the host unbound; the unique index decides.
				_, errs[i] = repos[i].Transition(t.Context(), approveReq(ids[i], host, t0.Add(time.Minute)))
			})
			if (errs[0] == nil) == (errs[1] == nil) {
				t.Fatalf("round %d: errors = %v, %v; want exactly one approve to succeed", round, errs[0], errs[1])
			}
			for _, err := range errs {
				if err != nil {
					wantTransitionReason(t, err, "host_bound")
				}
			}
			if n := scalar[int](t, serveDB, "SELECT count(*) FROM machines WHERE cluster_id = ? AND host_name = ?", labCluster, host); n != 1 {
				t.Fatalf("round %d: %d machines on %s, want 1", round, n, host)
			}
			for i, id := range ids {
				if got := mustGet(t, repos[0], id); (errs[i] == nil) != (got.State == StateAssigned) {
					t.Fatalf("round %d: machine %d state %s with error %v; want assigned only for the winner", round, i, got.State, errs[i])
				}
			}
		}
	})

	t.Run("HostBoundTx", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)
		mustApprove(t, r, m.ID, labHost, t0)
		tests := []struct {
			name                   string
			cluster, host, exclude string
			want                   bool
		}{
			{"bound host", labCluster, labHost, "", true},
			{"bound host, excepting its own machine", labCluster, labHost, m.ID, false},
			{"bound host, excepting another machine", labCluster, labHost, "other", true},
			{"another host", labCluster, "cp-02", "", false},
			{"another cluster", "other", labHost, "", false},
		}
		for _, tc := range tests {
			var got bool
			err := inTx(t.Context(), d, func(ctx context.Context, tx bun.Tx) error {
				var err error
				got, err = HostBoundTx(ctx, tx, tc.cluster, tc.host, tc.exclude)
				return err
			})
			if err != nil || got != tc.want {
				t.Errorf("%s: HostBoundTx = %v, %v; want %v, nil", tc.name, got, err, tc.want)
			}
		}
	})

	t.Run("RebindRefusesJunk", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := firstContact(t, r, Identity{UUID: zeroUUID, MAC: macN(0xa)}, t0)
		const orphan = "UPDATE machines SET state = 'orphaned', orphaned_from = 'installed', cluster_id = ?, host_name = ?, talos_schematic = ?, talos_version = ? WHERE id = ?"
		if err := execRaw(t, d, orphan, labCluster, labHost, plainSchematic, talosVersion, m.ID); err != nil {
			t.Fatalf("orphan the machine: %v", err)
		}
		b := labBinding("cp-02")
		_, err := r.Transition(t.Context(), TransitionRequest{MachineID: m.ID, Trigger: TriggerRebind, Guard: guardOK, Bind: &b, Actor: "admin", At: t0})
		wantTransitionReason(t, err, "junk_uuid")
		got := mustGet(t, r, m.ID)
		if got.State != StateOrphaned {
			t.Fatalf("State = %s, want it left orphaned", got.State)
		}
		wantBound(t, got, labHost)
	})

	t.Run("RebindOwnFormerHost", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)
		mustApprove(t, r, m.ID, labHost, t0)
		mustTransition(t, r, m.ID, TriggerHostLeft, t0.Add(time.Minute))

		// The host is back in nodes[]: its only binding is the machine's own row.
		var bound, boundExceptOwn bool
		err := inTx(t.Context(), d, func(ctx context.Context, tx bun.Tx) error {
			var err error
			if bound, err = HostBoundTx(ctx, tx, labCluster, labHost, ""); err != nil {
				return err
			}
			boundExceptOwn, err = HostBoundTx(ctx, tx, labCluster, labHost, m.ID)
			return err
		})
		if err != nil || !bound || boundExceptOwn {
			t.Fatalf("HostBoundTx = %v and, excepting the machine, %v (%v); want true and false", bound, boundExceptOwn, err)
		}

		b := labBinding(labHost)
		got, err := r.Transition(t.Context(), TransitionRequest{
			MachineID: m.ID, Trigger: TriggerRebind, Guard: Guard{HostExists: true, HostUnbound: !boundExceptOwn}, Bind: &b, Actor: "admin", At: t0.Add(2 * time.Minute),
		})
		if err != nil {
			t.Fatalf("rebind to the machine's own former host: %v", err)
		}
		if got.State != StateAssigned {
			t.Fatalf("State = %s, want assigned", got.State)
		}
		wantBound(t, mustGet(t, r, m.ID), labHost)
	})

	t.Run("RebindToOtherHostRebinds", func(t *testing.T) {
		r := NewRepository(open(t))
		m := bootNextMachine(t, r, 1, t0)
		mustApprove(t, r, m.ID, labHost, t0)
		mustTransition(t, r, m.ID, TriggerHostLeft, t0.Add(time.Minute))
		b := labBinding("cp-02")
		b.TalosVersion = "v1.13.0"
		_, err := r.Transition(t.Context(), TransitionRequest{MachineID: m.ID, Trigger: TriggerRebind, Guard: guardOK, Bind: &b, Actor: "admin", At: t0.Add(2 * time.Minute)})
		if err != nil {
			t.Fatalf("rebind: %v", err)
		}
		got := mustGet(t, r, m.ID)
		if deref(got.HostName) != "cp-02" || deref(got.TalosVersion) != "v1.13.0" {
			t.Fatalf("binding = %s %s, want cp-02 at v1.13.0", deref(got.HostName), deref(got.TalosVersion))
		}
	})

	t.Run("AdoptByUUIDCreates", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		created := mustAdopt(t, d, adoptReq(adoptedUUID, labHost, t0))
		m := mustGet(t, r, created.ID)
		if m.State != StateInstalled || m.IdentityObserved || m.Serial != "" || m.Product != "" || len(m.MACs) != 0 ||
			m.UUID != adoptedUUID || m.Junk || m.Arch != ArchAMD64 || !m.CreatedAt.Equal(t0) {
			t.Fatalf("machine = %+v, want an installed, unobserved machine with no identity, no MACs and the typed UUID", m)
		}
		wantBound(t, m, labHost)
		wantOneAudit(t, d, "transition.adopt_by_uuid", "admin", m.ID, `{"from":"none","to":"installed"}`)
	})

	t.Run("AdoptByUUIDJunkRefused", func(t *testing.T) {
		d := open(t)
		_, err := adopt(t.Context(), d, adoptReq(zeroUUID, labHost, t0))
		wantTransitionReason(t, err, "junk_uuid")
		if n := scalar[int](t, d, "SELECT count(*) FROM machines"); n != 0 {
			t.Fatalf("machines = %d, want 0", n)
		}
	})

	t.Run("AdoptByUUIDGuard", func(t *testing.T) {
		d := open(t)
		tests := []struct {
			name   string
			guard  Guard
			reason string
		}{
			{"host missing", Guard{HostUnbound: true}, "host_not_found"},
			{"host bound", Guard{HostExists: true}, "host_bound"},
		}
		for _, tc := range tests {
			req := adoptReq(adoptedUUID, labHost, t0)
			req.Guard = tc.guard
			_, err := adopt(t.Context(), d, req)
			if te, ok := errors.AsType[*TransitionError](err); !ok || te.Reason != tc.reason {
				t.Errorf("%s: error = %v, want a %s TransitionError", tc.name, err, tc.reason)
			}
		}
		if n := scalar[int](t, d, "SELECT count(*) FROM machines"); n != 0 {
			t.Fatalf("machines = %d, want 0", n)
		}
	})

	t.Run("AdoptByUUIDHostBoundByIndex", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		first := mustAdopt(t, d, adoptReq(adoptedUUID, labHost, t0))
		// The caller's check passed on a stale read; the unique index decides.
		_, err := adopt(t.Context(), d, adoptReq("4c4c4544-0042-3510-8052-b4c04f4e4d98", labHost, t0))
		wantTransitionReason(t, err, "host_bound")
		if n := scalar[int](t, d, "SELECT count(*) FROM machines"); n != 1 {
			t.Fatalf("machines = %d, want only %s", n, first.ID)
		}
		wantBound(t, mustGet(t, r, first.ID), labHost)
	})

	t.Run("AdoptByUUIDExisting", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		first := mustAdopt(t, d, adoptReq(adoptedUUID, labHost, t0))
		if _, err := adopt(t.Context(), d, adoptReq(adoptedUUID, "cp-02", t0)); !errors.Is(err, ErrUUIDExists) {
			t.Fatalf("adopting an adopted UUID: error = %v, want ErrUUIDExists", err)
		}
		known := bootNextMachine(t, r, 7, t0)
		if _, err := adopt(t.Context(), d, adoptReq(known.UUID, "cp-02", t0)); !errors.Is(err, ErrUUIDExists) {
			t.Fatalf("adopting a discovered machine's UUID: error = %v, want ErrUUIDExists", err)
		}
		if n := scalar[int](t, d, "SELECT count(*) FROM machines"); n != 2 {
			t.Fatalf("machines = %d, want 2 (%s and %s)", n, first.ID, known.ID)
		}
	})

	t.Run("MachinesInClusterTx", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		a, b, c := bootNextMachine(t, r, 1, t0), bootNextMachine(t, r, 2, t0), bootNextMachine(t, r, 3, t0)
		mustApprove(t, r, a.ID, "cp-01", t0)
		mustApprove(t, r, b.ID, "cp-02", t0)
		other := labBinding("cp-01")
		other.ClusterID = "other"
		if _, err := r.Transition(t.Context(), TransitionRequest{MachineID: c.ID, Trigger: TriggerApprove, Guard: guardOK, Bind: &other, Actor: "admin", At: t0}); err != nil {
			t.Fatalf("approve into the other cluster: %v", err)
		}
		var got []Machine
		err := inTx(t.Context(), d, func(ctx context.Context, tx bun.Tx) error {
			var err error
			got, err = MachinesInClusterTx(ctx, tx, labCluster)
			return err
		})
		if err != nil {
			t.Fatalf("MachinesInClusterTx: %v", err)
		}
		want := []string{a.ID, b.ID}
		slices.Sort(want)
		ids := make([]string, len(got))
		for i := range got {
			ids[i] = got[i].ID
			if len(got[i].MACs) != 1 {
				t.Errorf("machine %s MACs = %v, want one", got[i].ID, got[i].MACs)
			}
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("machines in lab = %v, want %v (ordered by id)", ids, want)
		}
	})

	t.Run("SetTalosTupleTx", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := bootNextMachine(t, r, 1, t0)
		mustApprove(t, r, m.ID, labHost, t0)
		set := func(id string) error {
			return inTx(t.Context(), d, func(ctx context.Context, tx bun.Tx) error {
				return SetTalosTupleTx(ctx, tx, id, "newschematic", "v1.13.0")
			})
		}
		if err := set(m.ID); err != nil {
			t.Fatalf("SetTalosTupleTx: %v", err)
		}
		got := mustGet(t, r, m.ID)
		if deref(got.TalosSchematic) != "newschematic" || deref(got.TalosVersion) != "v1.13.0" {
			t.Fatalf("tuple = %s %s, want newschematic v1.13.0", deref(got.TalosSchematic), deref(got.TalosVersion))
		}
		if err := set("nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("SetTalosTupleTx of an unknown machine: error = %v, want ErrNotFound", err)
		}
	})

	t.Run("FirstReportFillsIdentity", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		created := mustAdopt(t, d, adoptReq(adoptedUUID, labHost, t0))
		knownBoot(t, r, firstReport(created, "S1", t0.Add(time.Minute)))
		m := mustGet(t, r, created.ID)
		if m.Serial != "S1" || m.Product != "P1" || !m.IdentityObserved || m.Junk {
			t.Fatalf("machine = serial %q product %q observed %v junk %v, want S1 P1 observed, not junk", m.Serial, m.Product, m.IdentityObserved, m.Junk)
		}
		wantOneAudit(t, d, "identity.observed", "system", m.ID, `{"serial":"S1","product":"P1"}`)
	})

	t.Run("FirstObserveFillsIdentity", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		created := mustAdopt(t, d, adoptReq(adoptedUUID, labHost, t0))
		obs, err := r.Observe(t.Context(), Identity{UUID: adoptedUUID, MAC: macN(1), Serial: "S1", Product: "P1"}, ArchAMD64, t0.Add(time.Minute))
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
		if obs.Created || obs.Alerted || obs.Machine.ID != created.ID {
			t.Fatalf("observation = %+v, want the adopted machine %s, not created, not alerted", obs, created.ID)
		}
		m := mustGet(t, r, created.ID)
		if m.Serial != "S1" || m.Product != "P1" || !m.IdentityObserved || m.Junk {
			t.Fatalf("machine = serial %q product %q observed %v junk %v, want S1 P1 observed, not junk", m.Serial, m.Product, m.IdentityObserved, m.Junk)
		}
		wantOneAudit(t, d, "identity.observed", "system", m.ID, `{"serial":"S1","product":"P1"}`)
	})

	t.Run("SecondReportAfterObservationConflicts", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		created := mustAdopt(t, d, adoptReq(adoptedUUID, labHost, t0))
		knownBoot(t, r, firstReport(created, "S1", t0.Add(time.Minute)))

		obs, err := r.Observe(t.Context(), Identity{UUID: adoptedUUID, MAC: macN(1), Serial: "S2", Product: "P1"}, ArchAMD64, t0.Add(2*time.Minute))
		if !errors.Is(err, ErrIdentityConflict) || !obs.Alerted || !obs.Machine.Junk {
			t.Fatalf("Observe of another serial = %+v, %v; want an alerted junk ErrIdentityConflict", obs, err)
		}
		if got := mustGet(t, r, created.ID); !got.Junk || got.Serial != "S1" {
			t.Fatalf("machine = junk %v serial %q, want junk and the first report's S1 kept", got.Junk, got.Serial)
		}
	})

	t.Run("IdentityRaceSentinel", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		created := mustAdopt(t, d, adoptReq(adoptedUUID, labHost, t0))
		// Both reports were decided on the unobserved read; the first lands.
		stale := firstReport(created, "S2", t0.Add(2*time.Minute))
		knownBoot(t, r, firstReport(created, "S1", t0.Add(time.Minute)))
		events := scalar[int](t, d, "SELECT count(*) FROM boot_events WHERE machine_id = ?", created.ID)
		audits := len(auditRows(t, d))

		_, err := r.RecordBoot(t.Context(), stale)
		if !errors.Is(err, ErrIdentityRace) {
			t.Fatalf("RecordBoot error = %v, want ErrIdentityRace", err)
		}
		if got := mustGet(t, r, created.ID); got.Serial != "S1" || !got.LastSeenAt.Equal(t0.Add(time.Minute)) {
			t.Fatalf("machine = serial %q seen %v, want the first report's S1 and nothing of the second", got.Serial, got.LastSeenAt)
		}
		if n := scalar[int](t, d, "SELECT count(*) FROM boot_events WHERE machine_id = ?", created.ID); n != events {
			t.Fatalf("boot events = %d, want the %d before", n, events)
		}
		if n := len(auditRows(t, d)); n != audits {
			t.Fatalf("audit rows = %d, want the %d before", n, audits)
		}
	})

	t.Run("ObserveIdentityNeedsKnownBoot", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		created := mustAdopt(t, d, adoptReq(adoptedUUID, labHost, t0))
		tests := []struct {
			name string
			rec  *BootRecord
		}{
			{"first contact", &BootRecord{Identity: originalIdentity(), Arch: ArchAMD64, Trigger: TriggerFirstContact, ObserveIdentity: true, At: t0}},
			{"conflict", &BootRecord{Identity: originalIdentity(), Arch: ArchAMD64, MachineID: created.ID, Conflict: true, ObserveIdentity: true, At: t0}},
		}
		for _, tc := range tests {
			if _, err := r.RecordBoot(t.Context(), *tc.rec); !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("%s: RecordBoot error = %v, want ErrInvalidTransition", tc.name, err)
			}
		}
		if got := mustGet(t, r, created.ID); got.IdentityObserved {
			t.Fatal("identity observed by a refused boot")
		}
	})

	t.Run("ConcurrentFirstReports", func(t *testing.T) {
		serveDB, cliDB := pair(t)
		defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
		repos := [2]*Repository{NewRepository(serveDB), NewRepository(cliDB)}
		created := mustAdopt(t, serveDB, adoptReq(adoptedUUID, labHost, t0))
		serials := [2]string{"S1", "S2"}
		var errs [2]error
		together(func(i int) {
			_, errs[i] = repos[i].RecordBoot(t.Context(), firstReport(created, serials[i], t0.Add(time.Minute)))
		})
		winner := 0
		if errs[0] != nil {
			winner = 1
		}
		if errs[winner] != nil || !errors.Is(errs[1-winner], ErrIdentityRace) {
			t.Fatalf("errors = %v, %v; want exactly one nil and the other ErrIdentityRace", errs[0], errs[1])
		}
		if got := mustGet(t, repos[0], created.ID); got.Serial != serials[winner] || !got.IdentityObserved {
			t.Fatalf("serial = %q observed %v, want the winner's %q", got.Serial, got.IdentityObserved, serials[winner])
		}
		if n := len(auditsOf(t, serveDB, "identity.observed")); n != 1 {
			t.Fatalf("identity.observed audits = %d, want 1", n)
		}
		if n := scalar[int](t, serveDB, "SELECT count(*) FROM boot_events WHERE machine_id = ?", created.ID); n != 1 {
			t.Fatalf("boot events = %d, want only the winner's", n)
		}
	})

	t.Run("ConcurrentFirstObserves", func(t *testing.T) {
		serveDB, cliDB := pair(t)
		defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
		repos := [2]*Repository{NewRepository(serveDB), NewRepository(cliDB)}
		created := mustAdopt(t, serveDB, adoptReq(adoptedUUID, labHost, t0))
		serials := [2]string{"S1", "S2"}
		var errs [2]error
		together(func(i int) {
			_, errs[i] = repos[i].Observe(t.Context(), Identity{UUID: adoptedUUID, MAC: macN(1), Serial: serials[i], Product: "P1"}, ArchAMD64, t0.Add(time.Minute))
		})
		winner := 0
		if errs[0] != nil {
			winner = 1
		}
		if errs[winner] != nil || !errors.Is(errs[1-winner], ErrIdentityConflict) {
			t.Fatalf("errors = %v, %v; want exactly one nil and the other ErrIdentityConflict", errs[0], errs[1])
		}
		if got := mustGet(t, repos[0], created.ID); got.Serial != serials[winner] || !got.IdentityObserved || !got.Junk {
			t.Fatalf("machine = serial %q observed %v junk %v, want the winner's %q, observed, and the UUID junk", got.Serial, got.IdentityObserved, got.Junk, serials[winner])
		}
		if n := len(auditsOf(t, serveDB, "identity.observed")); n != 1 {
			t.Fatalf("identity.observed audits = %d, want 1", n)
		}
	})
}
