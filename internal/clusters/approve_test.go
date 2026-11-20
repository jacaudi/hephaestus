package clusters

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/render"
)

const (
	// nextTalos is a Talos version a later pair moves a host to, which changes its machine's tuple.
	nextTalos = "v1.12.12"
	labBody   = "customization: {}\n"
	nextBody  = "customization:\n  extraKernelArgs:\n    - quiet\n"
	// adoptUUID is the fixture UUID, for a machine created by adopting it.
	adoptUUID = "4c4c4544-0042-3510-8052-b4c04f4e4d32"
)

func TestApprove_SQLite(t *testing.T) { runApproveSuite(t, dbtest.SQLite) }

// approveEnv is cluster lab and the fleet over one database, with a clock each step advances by a minute.
type approveEnv struct {
	t     *testing.T
	r     *Repository
	f     *fleet.Repository
	d     *db.DB
	c     Cluster
	at    time.Time
	trees int
}

func newApproveEnv(t *testing.T, open func(testing.TB) *db.DB, at time.Time) *approveEnv {
	t.Helper()
	r, d := testRepo(t, open)
	return &approveEnv{t: t, r: r, f: fleet.NewRepository(d), d: d, c: mustImport(t, r, labImport(at)), at: at}
}

// tick advances the clock and returns it.
func (e *approveEnv) tick() time.Time {
	e.at = e.at.Add(time.Minute)
	return e.at
}

// hostRender is a render of host at a Talos version, with a schematic body when body is not empty. Its hashes and
// redacted text name the host and the version.
func hostRender(host, version, body string) render.HostRender {
	h := render.HostRender{
		Host: host, Role: "worker", IP: "10.10.20.21",
		Config: []byte("machine: " + leaked), Redacted: []byte("machine:\n  host: " + host + "\n  talos: " + version + "\n"),
		ConfigHash: "c-" + host + "-" + version, DisplayHash: "d-" + host + "-" + version,
		SchematicID: plainSchematic, TalosVersion: version,
	}
	if body != "" {
		h.SchematicBody = []byte(body)
	}
	return h
}

// labHosts are renders of hosts at the fixture Talos version, without a schematic body.
func labHosts(hosts ...string) []render.HostRender {
	out := make([]render.HostRender, len(hosts))
	for i, h := range hosts {
		out[i] = hostRender(h, talosVersion, "")
	}
	return out
}

// pending stores renders as the pending pair of a new tree and returns the pair id.
func (e *approveEnv) pending(renders ...render.HostRender) string {
	e.t.Helper()
	e.trees++
	tree, at := "T"+strconv.Itoa(e.trees), e.tick()
	mustMark(e.t, e.r, e.c, tree, TriggerHead, at)
	req := pendingReq(e.c, tree, at)
	req.Renders = renders
	id, stored, err := e.r.StorePending(e.t.Context(), req)
	if err != nil || !stored {
		e.t.Fatalf("StorePending(%s) = %q, %v, %v; want a pair id, true, nil", tree, id, stored, err)
	}
	return id
}

func (e *approveEnv) approveReq(pairID string) ApproveClusterRequest {
	return ApproveClusterRequest{Cluster: labName, PairID: pairID, Actor: fleet.ActorCLI, At: e.tick()}
}

// approved stores renders as the pending pair and approves it.
func (e *approveEnv) approved(renders ...render.HostRender) ApproveClusterResult {
	e.t.Helper()
	res, err := e.r.ApproveCluster(e.t.Context(), e.approveReq(e.pending(renders...)))
	if err != nil {
		e.t.Fatalf("ApproveCluster: %v", err)
	}
	return res
}

// discovered is a new machine, n-th of the suite's fixture identities, as its first boot records it.
func (e *approveEnv) discovered(n int) fleet.Machine {
	e.t.Helper()
	id := fleet.Identity{
		UUID: fmt.Sprintf("4c4c4544-0042-3510-8052-b4c04f4e%04x", 0x4d30+n), MAC: fmt.Sprintf("aa:bb:cc:00:00:%02x", n),
		Serial: "S" + strconv.Itoa(n), Product: "P",
	}
	m, err := e.f.RecordBoot(e.t.Context(), fleet.BootRecord{
		Identity: id, Arch: fleet.ArchAMD64, Trigger: fleet.TriggerFirstContact,
		Event: fleet.BootEvent{Row: 7, Action: "hold", Reason: "awaiting approval"}, At: e.tick(),
	})
	if err != nil {
		e.t.Fatalf("first contact of machine %d: %v", n, err)
	}
	return m
}

func (e *approveEnv) machineReq(m fleet.Machine, host string) MachineRequest {
	return MachineRequest{MachineID: m.ID, Cluster: labName, Host: host, Actor: fleet.ActorCLI, At: e.tick()}
}

// bind binds m into host of cluster lab with one of the repository's bind calls.
func (e *approveEnv) bind(call func(ctx context.Context, req MachineRequest) (fleet.Machine, error), m fleet.Machine, host string) fleet.Machine {
	e.t.Helper()
	out, err := call(e.t.Context(), e.machineReq(m, host))
	if err != nil {
		e.t.Fatalf("bind %s into %s: %v", m.ID, host, err)
	}
	return out
}

// transition applies a fleet trigger to m.
func (e *approveEnv) transition(m fleet.Machine, trigger fleet.Trigger) fleet.Machine {
	e.t.Helper()
	out, err := e.f.Transition(e.t.Context(), fleet.TransitionRequest{MachineID: m.ID, Trigger: trigger, Actor: fleet.ActorCLI, At: e.tick()})
	if err != nil {
		e.t.Fatalf("%s %s: %v", trigger, m.ID, err)
	}
	return out
}

func (e *approveEnv) machine(id string) fleet.Machine {
	e.t.Helper()
	m, err := e.f.Get(e.t.Context(), id)
	if err != nil {
		e.t.Fatalf("Get(%s): %v", id, err)
	}
	return m
}

func (e *approveEnv) talos(key fleet.TalosKey) (fleet.TalosRow, bool) {
	e.t.Helper()
	row, ok, err := e.f.Talos(e.t.Context(), key)
	if err != nil {
		e.t.Fatalf("Talos(%s): %v", key.Tag(), err)
	}
	return row, ok
}

// recordedAssets is the asset a broken row keeps recorded.
const recordedAssets = `{"kernel":{"size":5,"sha256":"ab"}}`

// breakTalos stores the row of key broken, after ten attempts, with a recorded asset and no body; it creates the row
// when absent.
func (e *approveEnv) breakTalos(key fleet.TalosKey) {
	e.t.Helper()
	if _, err := fleet.WantTalosTx(e.t.Context(), e.d.Bun(), fleet.TalosWant{Key: key}, e.tick()); err != nil {
		e.t.Fatalf("WantTalosTx(%s): %v", key.Tag(), err)
	}
	execRaw(e.t, e.d, "UPDATE talos_cache SET state = 'broken', attempts = 10, last_error = 'gone', assets = ?, body = NULL WHERE schematic = ? AND version = ? AND arch = ?",
		recordedAssets, key.Schematic, key.Version, string(key.Arch))
}

// dropTalos deletes every talos_cache row.
func (e *approveEnv) dropTalos() { execRaw(e.t, e.d, "DELETE FROM talos_cache") }

func (e *approveEnv) hostRow(pairID, host string) HostRow {
	e.t.Helper()
	rows, err := e.r.HostRows(e.t.Context(), pairID)
	if err != nil {
		e.t.Fatalf("HostRows(%s): %v", pairID, err)
	}
	for i := range rows {
		if rows[i].Host == host {
			return rows[i]
		}
	}
	e.t.Fatalf("pair %s has no render of %s", pairID, host)
	return HostRow{}
}

// setDrift records drift as the drift of host in the pair, as /config does.
func (e *approveEnv) setDrift(pairID, host string, drift DriftRender) bool {
	e.t.Helper()
	ok, err := e.r.SetDrift(e.t.Context(), SetDriftRequest{PairID: pairID, Host: host, ReadHash: e.hostRow(pairID, host).ConfigHash, Render: drift})
	if err != nil {
		e.t.Fatalf("SetDrift(%s/%s): %v", pairID, host, err)
	}
	return ok
}

// mint mints a ticket of m on host of the pair.
func (e *approveEnv) mint(m fleet.Machine, host string, p Pair) {
	e.t.Helper()
	_, _, err := e.f.MintTicket(e.t.Context(), fleet.MintRequest{
		MachineID: m.ID, ClusterID: e.c.ID, HostName: host, PairID: p.ID, SecretVersionID: p.SecretVersionID, At: e.tick(),
	})
	if err != nil {
		e.t.Fatalf("MintTicket(%s): %v", m.ID, err)
	}
}

func (e *approveEnv) live(m fleet.Machine) bool {
	e.t.Helper()
	_, ok, err := e.f.LiveTicket(e.t.Context(), m.ID)
	if err != nil {
		e.t.Fatalf("LiveTicket(%s): %v", m.ID, err)
	}
	return ok
}

func labKey(version string) fleet.TalosKey {
	return fleet.TalosKey{Schematic: plainSchematic, Version: version, Arch: fleet.ArchAMD64}
}

// wantRefused fails unless err is a *fleet.TransitionError with the reason.
func wantRefused(t *testing.T, err error, reason string) {
	t.Helper()
	te, ok := errors.AsType[*fleet.TransitionError](err)
	if !ok || te.Reason != reason {
		t.Fatalf("error = %v, want a *fleet.TransitionError %q", err, reason)
	}
}

// wantBinding fails unless m is bound to host of the cluster with the fixture schematic at version.
func wantBinding(t *testing.T, m fleet.Machine, clusterID, host, version string) {
	t.Helper()
	got := [4]string{derefOr(m.ClusterID), derefOr(m.HostName), derefOr(m.TalosSchematic), derefOr(m.TalosVersion)}
	if want := [4]string{clusterID, host, plainSchematic, version}; got != want {
		t.Fatalf("binding of %s = %q, want %q", m.ID, got, want)
	}
}

func derefOr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// wantWanted fails unless the row of key is wanted with no attempts and the body.
func (e *approveEnv) wantWanted(key fleet.TalosKey, body string) fleet.TalosRow {
	e.t.Helper()
	row, ok := e.talos(key)
	if !ok || row.State != fleet.CacheWanted || row.Attempts != 0 || string(row.Body) != body {
		e.t.Fatalf("Talos(%s) = %+v, %v; want a wanted row, no attempts, body %q", key.Tag(), row, ok, body)
	}
	return row
}

// wantNoTalos fails unless no talos_cache row exists.
func (e *approveEnv) wantNoTalos() {
	e.t.Helper()
	if n := count(e.t, e.d, "talos_cache", "1 = 1"); n != 0 {
		e.t.Fatalf("talos_cache rows = %d, want none", n)
	}
}

// runApproveSuite is the approval suite both databases run: TestApprove_SQLite untagged, TestApprove_Postgres under
// the integration tag.
func runApproveSuite(t *testing.T, open func(testing.TB) *db.DB) {
	t.Helper()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	t.Run("ApproveClusterStale", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		replaced := e.pending(labHosts("cp-01")...)
		current := e.pending(labHosts("cp-01")...)
		if _, err := e.r.ApproveCluster(t.Context(), e.approveReq(replaced)); !errors.Is(err, ErrStale) {
			t.Fatalf("ApproveCluster(replaced) error = %v, want ErrStale", err)
		}
		if c := mustGet(t, e.r, labName); c.PendingPairID != current || c.ApprovedPairID != "" {
			t.Fatalf("cluster = pending %q, approved %q; want pending %q, none approved", c.PendingPairID, c.ApprovedPairID, current)
		}
		if _, err := e.r.ApproveCluster(t.Context(), e.approveReq(current)); err != nil {
			t.Fatalf("ApproveCluster(current): %v", err)
		}
		if _, err := e.r.ApproveCluster(t.Context(), e.approveReq(current)); !errors.Is(err, ErrStale) {
			t.Fatalf("a second ApproveCluster(current) error = %v, want ErrStale", err)
		}
		if n := len(auditsOf(t, e.d, "cluster.approved")); n != 1 {
			t.Fatalf("cluster.approved audits = %d, want 1", n)
		}
	})

	t.Run("ApproveClusterSideEffects", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		old := e.approved(labHosts("cp-01", "wrk-01", "wrk-02", "wrk-03")...).Pair
		assigned := e.bind(e.r.ApproveMachine, e.discovered(1), "cp-01")
		installed := e.bind(e.r.AdoptMachine, e.discovered(2), "wrk-01")
		cordoned := e.transition(e.bind(e.r.AdoptMachine, e.discovered(3), "wrk-02"), fleet.TriggerCordon)
		orphan := e.transition(e.bind(e.r.AdoptMachine, e.discovered(4), "wrk-03"), fleet.TriggerHostLeft)
		e.mint(assigned, "cp-01", old)
		e.breakTalos(labKey(nextTalos))

		pending := e.pending(hostRender("cp-01", nextTalos, nextBody))
		at := e.tick()
		res, err := e.r.ApproveCluster(t.Context(), ApproveClusterRequest{Cluster: labName, PairID: pending, Actor: fleet.ActorCLI, At: at})
		if err != nil {
			t.Fatalf("ApproveCluster: %v", err)
		}

		if res.Orphaned != 2 || res.Rearmed != 1 || res.Revoked != 1 {
			t.Errorf("result counts = orphaned %d, rearmed %d, revoked %d; want 2, 1, 1", res.Orphaned, res.Rearmed, res.Revoked)
		}
		if res.Pair.ID != pending || !res.Pair.ApprovedAt.Equal(at) || res.Pair.ApprovedBy != fleet.ActorCLI {
			t.Errorf("result pair = %+v, want %s approved at %v by %s", res.Pair, pending, at, fleet.ActorCLI)
		}
		if c := mustGet(t, e.r, labName); c.ApprovedPairID != pending || c.PendingPairID != "" {
			t.Errorf("cluster = approved %q, pending %q; want approved %q, none pending", c.ApprovedPairID, c.PendingPairID, pending)
		}
		if _, err := e.r.Pair(t.Context(), old.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("Pair(old) error = %v, want ErrNotFound", err)
		}
		if n := count(t, e.d, "host_renders", "pair_id = ?", old.ID) + count(t, e.d, "secret_versions", "id = ?", old.SecretVersionID); n != 0 {
			t.Errorf("rows of the old pair left = %d, want its renders and version deleted", n)
		}

		if m := e.machine(installed.ID); m.State != fleet.StateOrphaned || m.OrphanedFrom != fleet.StateInstalled || derefOr(m.HostName) != "wrk-01" {
			t.Errorf("installed machine = %s from %s on %s; want orphaned from installed, still on wrk-01", m.State, m.OrphanedFrom, derefOr(m.HostName))
		}
		if m := e.machine(cordoned.ID); m.State != fleet.StateCordoned || m.Prior != fleet.StateOrphaned || m.OrphanedFrom != fleet.StateInstalled {
			t.Errorf("cordoned machine = %s over %s from %s; want cordoned over orphaned from installed", m.State, m.Prior, m.OrphanedFrom)
		}
		if m := e.machine(orphan.ID); m.State != orphan.State || m.OrphanedFrom != orphan.OrphanedFrom || derefOr(m.TalosVersion) != talosVersion {
			t.Errorf("orphaned machine = %s from %s at %s; want it untouched", m.State, m.OrphanedFrom, derefOr(m.TalosVersion))
		}
		m := e.machine(assigned.ID)
		if m.State != fleet.StateAssigned {
			t.Errorf("assigned machine = %s, want assigned", m.State)
		}
		wantBinding(t, m, e.c.ID, "cp-01", nextTalos)
		if e.live(assigned) {
			t.Error("the cluster's ticket is still live")
		}
		row := e.wantWanted(labKey(nextTalos), nextBody)
		if got := row.Assets[fleet.TalosKernel]; got.Size != 5 || got.SHA256 != "ab" {
			t.Errorf("recorded kernel = %+v, want the recorded asset kept", got)
		}
		detail := `{"name":"lab","pair":"` + pending + `","orphaned":2,"rearmed":1,"revoked":1}`
		audits := auditsOf(t, e.d, "cluster.approved") // the first approves the fixture's pair
		if want := (auditRow{Action: "cluster.approved", Actor: fleet.ActorCLI, Detail: detail}); len(audits) != 2 || audits[1] != want {
			t.Errorf("cluster.approved audits = %+v; want the second %+v", audits, want)
		}
	})

	t.Run("ApprovalDuringConfigSetsNoDrift", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		old := e.approved(labHosts("cp-01")...).Pair
		read := e.hostRow(old.ID, "cp-01") // what a /config render of the old pair read
		e.approved(hostRender("cp-01", nextTalos, ""))

		ok, err := e.r.SetDrift(t.Context(), SetDriftRequest{
			PairID: old.ID, Host: "cp-01", ReadHash: read.ConfigHash,
			Render: DriftRender{ConfigHash: "dc", DisplayHash: "dd", Redacted: []byte("drifted")},
		})
		if err != nil || ok {
			t.Fatalf("SetDrift(old pair) = %v, %v; want false, nil", ok, err)
		}
		if n := count(t, e.d, "host_renders", "drift = ?", true); n != 0 {
			t.Fatalf("rows with drift = %d, want none", n)
		}
	})

	t.Run("ApproveMachineBindsAndWants", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(hostRender("cp-01", talosVersion, labBody))
		m := e.bind(e.r.ApproveMachine, e.discovered(1), "cp-01")
		if m.State != fleet.StateAssigned {
			t.Fatalf("state = %s, want assigned", m.State)
		}
		wantBinding(t, m, e.c.ID, "cp-01", talosVersion)
		wantBinding(t, e.machine(m.ID), e.c.ID, "cp-01", talosVersion)
		e.wantWanted(labKey(talosVersion), labBody)
	})

	t.Run("ApproveMachineHostMissing", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		m := e.discovered(1)
		_, err := e.r.ApproveMachine(t.Context(), e.machineReq(m, "cp-01"))
		wantRefused(t, err, "host_not_found") // no approved pair yet
		e.approved(labHosts("cp-01")...)
		_, err = e.r.ApproveMachine(t.Context(), e.machineReq(m, "wrk-09"))
		wantRefused(t, err, "host_not_found")
		if got := e.machine(m.ID); got.State != fleet.StateDiscovered || got.HostName != nil {
			t.Fatalf("machine = %s on %s, want discovered and unbound", got.State, derefOr(got.HostName))
		}
		e.wantNoTalos()
	})

	t.Run("ApproveMachineHostBound", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(labHosts("cp-01")...)
		e.bind(e.r.ApproveMachine, e.discovered(1), "cp-01")
		second := e.discovered(2)
		_, err := e.r.ApproveMachine(t.Context(), e.machineReq(second, "cp-01"))
		wantRefused(t, err, "host_bound")
		if got := e.machine(second.ID); got.State != fleet.StateDiscovered || got.HostName != nil {
			t.Fatalf("second machine = %s on %s, want discovered and unbound", got.State, derefOr(got.HostName))
		}
	})

	t.Run("ApproveMachineJunk", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(labHosts("cp-01")...)
		m := e.discovered(1)
		execRaw(t, e.d, "UPDATE machines SET junk = ? WHERE id = ?", true, m.ID)
		_, err := e.r.ApproveMachine(t.Context(), e.machineReq(m, "cp-01"))
		wantRefused(t, err, "junk_uuid")
		e.wantNoTalos()
	})

	t.Run("ApproveMachineUnknownCluster", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		req := e.machineReq(e.discovered(1), "cp-01")
		req.Cluster = "gone"
		if _, err := e.r.ApproveMachine(t.Context(), req); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ApproveMachine into an unknown cluster error = %v, want ErrNotFound", err)
		}
	})

	t.Run("AdoptStoresCurrentRender", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(hostRender("cp-01", talosVersion, labBody))
		m := e.bind(e.r.AdoptMachine, e.discovered(1), "cp-01")
		if m.State != fleet.StateInstalled {
			t.Fatalf("state = %s, want installed", m.State)
		}
		wantBinding(t, e.machine(m.ID), e.c.ID, "cp-01", talosVersion)
		e.wantNoTalos()
	})

	t.Run("AdoptByUUIDFlow", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(labHosts("cp-01")...)
		req := AdoptByUUIDRequest{UUID: adoptUUID, Arch: fleet.ArchARM64, Cluster: labName, Host: "cp-01", Actor: fleet.ActorCLI, At: e.tick()}
		m, err := e.r.AdoptByUUID(t.Context(), req)
		if err != nil {
			t.Fatalf("AdoptByUUID: %v", err)
		}
		stored := e.machine(m.ID)
		if stored.State != fleet.StateInstalled || stored.IdentityObserved || stored.UUID != adoptUUID || stored.Arch != fleet.ArchARM64 {
			t.Fatalf("machine = %+v; want installed, identity not observed, the UUID and arch adopted", stored)
		}
		wantBinding(t, stored, e.c.ID, "cp-01", talosVersion)
		e.wantNoTalos()

		req.UUID, req.At = "4c4c4544-0042-3510-8052-b4c04f4e4d33", e.tick()
		_, err = e.r.AdoptByUUID(t.Context(), req)
		wantRefused(t, err, "host_bound")
		req.Host = "wrk-09"
		_, err = e.r.AdoptByUUID(t.Context(), req)
		wantRefused(t, err, "host_not_found")
	})

	t.Run("RebindIntoInstalledWantsAssets", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(labHosts("cp-01", "wrk-01")...)
		m := e.bind(e.r.AdoptMachine, e.discovered(1), "wrk-01")
		e.approved(hostRender("cp-01", talosVersion, ""), hostRender("wrk-02", nextTalos, nextBody))
		if got := e.machine(m.ID); got.State != fleet.StateOrphaned || got.OrphanedFrom != fleet.StateInstalled {
			t.Fatalf("machine = %s from %s after its host left, want orphaned from installed", got.State, got.OrphanedFrom)
		}
		e.wantNoTalos()

		got := e.bind(e.r.RebindMachine, m, "wrk-02")
		if got.State != fleet.StateInstalled {
			t.Fatalf("state = %s, want installed", got.State)
		}
		wantBinding(t, e.machine(m.ID), e.c.ID, "wrk-02", nextTalos)
		e.wantWanted(labKey(nextTalos), nextBody)
	})

	t.Run("RebindOwnFormerHost", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(labHosts("cp-01", "wrk-01")...)
		m := e.bind(e.r.AdoptMachine, e.discovered(1), "wrk-01")
		e.approved(labHosts("cp-01")...)
		e.approved(labHosts("cp-01", "wrk-01")...)
		if got := e.machine(m.ID); got.State != fleet.StateOrphaned {
			t.Fatalf("machine = %s after its host came back, want still orphaned", got.State)
		}
		if got := e.bind(e.r.RebindMachine, m, "wrk-01"); got.State != fleet.StateInstalled {
			t.Fatalf("state = %s after a rebind to its own former host, want installed", got.State)
		}
		wantBinding(t, e.machine(m.ID), e.c.ID, "wrk-01", talosVersion)
	})

	t.Run("EnsureAssetsCreatesWithoutRearm", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(hostRender("cp-01", talosVersion, labBody))
		m := e.bind(e.r.ApproveMachine, e.discovered(1), "cp-01")
		e.breakTalos(labKey(talosVersion))
		if err := e.r.EnsureAssets(t.Context(), m, e.tick()); err != nil {
			t.Fatalf("EnsureAssets: %v", err)
		}
		if row, _ := e.talos(labKey(talosVersion)); row.State != fleet.CacheBroken || row.Attempts != 10 {
			t.Fatalf("row = %s after %d attempts, want it still broken", row.State, row.Attempts)
		}

		e.dropTalos()
		if err := e.r.EnsureAssets(t.Context(), m, e.tick()); err != nil {
			t.Fatalf("EnsureAssets: %v", err)
		}
		e.wantWanted(labKey(talosVersion), labBody)
	})

	t.Run("EnsureAssetsFillsMissingBody", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(hostRender("cp-01", talosVersion, labBody))
		m := e.bind(e.r.ApproveMachine, e.discovered(1), "cp-01")
		execRaw(t, e.d, "UPDATE talos_cache SET body = NULL")
		if err := e.r.EnsureAssets(t.Context(), m, e.tick()); err != nil {
			t.Fatalf("EnsureAssets: %v", err)
		}
		e.wantWanted(labKey(talosVersion), labBody)
	})

	t.Run("EnsureAssetsUnbound", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(labHosts("cp-01")...)
		m := e.discovered(1)
		if err := e.r.EnsureAssets(t.Context(), m, e.tick()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("EnsureAssets of an unbound machine error = %v, want ErrNotFound", err)
		}
		e.wantNoTalos()
	})

	t.Run("WantAssetsRearmsBroken", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(hostRender("cp-01", talosVersion, labBody))
		m := e.bind(e.r.ApproveMachine, e.discovered(1), "cp-01")
		e.breakTalos(labKey(talosVersion))
		if err := e.r.WantAssets(t.Context(), m, e.tick()); err != nil {
			t.Fatalf("WantAssets: %v", err)
		}
		row := e.wantWanted(labKey(talosVersion), labBody)
		if got := row.Assets[fleet.TalosKernel]; got.Size != 5 || got.SHA256 != "ab" {
			t.Fatalf("recorded kernel = %+v, want the recorded asset kept", got)
		}

		e.dropTalos()
		if err := e.r.WantAssets(t.Context(), m, e.tick()); err != nil {
			t.Fatalf("WantAssets: %v", err)
		}
		e.wantWanted(labKey(talosVersion), labBody)
	})

	t.Run("DriftApproveMatching", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		p := e.approved(labHosts("cp-01", "wrk-01")...).Pair
		m := e.bind(e.r.ApproveMachine, e.discovered(1), "cp-01")
		e.mint(m, "cp-01", p)
		drift := DriftRender{ConfigHash: "dc", DisplayHash: "dd", Redacted: []byte("machine:\n  host: cp-01\n  drifted: true\n")}
		if !e.setDrift(p.ID, "cp-01", drift) {
			t.Fatal("SetDrift wrote nothing")
		}

		err := e.r.ApproveDrift(t.Context(), DriftApproveRequest{Cluster: labName, Host: "cp-01", Hash: "dc", Actor: fleet.ActorCLI, At: e.tick()})
		if err != nil {
			t.Fatalf("ApproveDrift: %v", err)
		}
		row := e.hostRow(p.ID, "cp-01")
		if row.ConfigHash != "dc" || row.DisplayHash != "dd" || !bytes.Equal(row.Redacted, drift.Redacted) {
			t.Errorf("cp-01 = %s, %s, %q; want the former drift hashes and render", row.ConfigHash, row.DisplayHash, row.Redacted)
		}
		if row.Drift || row.DriftConfigHash != "" || row.DriftDisplayHash != "" || row.DriftRedacted != nil {
			t.Errorf("cp-01 drift = %+v, want cleared", row)
		}
		if !e.live(m) {
			t.Error("a drift approval revoked the machine's ticket")
		}
		wantOneAudit(t, e.d, "cluster.drift_approved", fleet.ActorCLI, `{"cluster":"lab","host":"cp-01"}`)
	})

	t.Run("DriftApproveStale", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		p := e.approved(labHosts("cp-01", "wrk-01")...).Pair
		e.setDrift(p.ID, "cp-01", DriftRender{ConfigHash: "dc", DisplayHash: "dd", Redacted: []byte("drifted")})
		for _, tt := range []struct{ host, hash string }{{"cp-01", "other"}, {"wrk-01", "dc"}} {
			err := e.r.ApproveDrift(t.Context(), DriftApproveRequest{Cluster: labName, Host: tt.host, Hash: tt.hash, Actor: fleet.ActorCLI, At: e.tick()})
			if !errors.Is(err, ErrStale) {
				t.Errorf("ApproveDrift(%s, %s) error = %v, want ErrStale", tt.host, tt.hash, err)
			}
		}
		if row := e.hostRow(p.ID, "cp-01"); !row.Drift || row.DriftConfigHash != "dc" || row.ConfigHash != "c-cp-01-"+talosVersion {
			t.Errorf("cp-01 = %+v, want its drift and stored render untouched", row)
		}
		if n := len(auditsOf(t, e.d, "cluster.drift_approved")); n != 0 {
			t.Errorf("cluster.drift_approved audits = %d, want none", n)
		}
	})

	t.Run("DriftReadsApprovedRow", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		approved := e.approved(labHosts("cp-01", "wrk-01")...).Pair
		pending := e.pending(labHosts("cp-01", "wrk-01")...)
		m := e.bind(e.r.ApproveMachine, e.discovered(1), "cp-01")
		drift := DriftRender{ConfigHash: "dc", DisplayHash: "dd", Redacted: []byte("drifted")}
		wantDrift := func(want bool) {
			t.Helper()
			if got, err := e.r.Drift(t.Context(), m); err != nil || got != want {
				t.Fatalf("Drift = %v, %v; want %v, nil", got, err, want)
			}
		}
		wantDrift(false)
		e.setDrift(pending, "cp-01", drift)
		wantDrift(false)
		e.setDrift(approved.ID, "cp-01", drift)
		wantDrift(true)
	})

	t.Run("DriftDiffStoredVsDrift", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		p := e.approved(labHosts("cp-01", "wrk-01")...).Pair
		stored := e.hostRow(p.ID, "cp-01")
		drift := DriftRender{ConfigHash: "dc", DisplayHash: "dd", Redacted: []byte("machine:\n  drifted: true\n")}
		e.setDrift(p.ID, "cp-01", drift)

		got, err := e.r.DriftDiff(t.Context(), labName, "cp-01")
		if err != nil {
			t.Fatalf("DriftDiff: %v", err)
		}
		if got.Host != "cp-01" || !bytes.Equal(got.Old, stored.Redacted) || !bytes.Equal(got.New, drift.Redacted) ||
			got.OldHash != stored.ConfigHash || got.NewHash != "dc" || got.SecretValuesChanged {
			t.Fatalf("DriftDiff = %+v; want the stored render and hash against the drift ones", got)
		}
		if _, err := e.r.DriftDiff(t.Context(), labName, "wrk-01"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("DriftDiff of a host without drift error = %v, want ErrNotFound", err)
		}
	})

	t.Run("DiffSecretValuesChanged", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(labHosts("cp-01")...)
		next := hostRender("cp-01", talosVersion, "")
		next.ConfigHash = "c-cp-01-new-values" // the same display render, other secret values
		pending := e.pending(next)

		pairID, hosts, err := e.r.Diff(t.Context(), labName)
		if err != nil || pairID != pending || len(hosts) != 1 {
			t.Fatalf("Diff = %q, %+v, %v; want the pending pair and one host", pairID, hosts, err)
		}
		if !hosts[0].SecretValuesChanged || hosts[0].OldHash != "c-cp-01-"+talosVersion || hosts[0].NewHash != next.ConfigHash {
			t.Fatalf("cp-01 = %+v, want the secret values changed between the two config hashes", hosts[0])
		}
	})

	t.Run("DiffHostOnOneSide", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(labHosts("cp-01", "wrk-01")...)
		e.pending(labHosts("cp-01", "wrk-02")...)
		_, hosts, err := e.r.Diff(t.Context(), labName)
		if err != nil || len(hosts) != 3 {
			t.Fatalf("Diff = %+v, %v; want three hosts", hosts, err)
		}
		cp, wrk1, wrk2 := hosts[0], hosts[1], hosts[2]
		if cp.Host != "cp-01" || !bytes.Equal(cp.Old, cp.New) || cp.Old == nil || cp.SecretValuesChanged {
			t.Errorf("cp-01 = %+v, want the same render on both sides", cp)
		}
		if wrk1.Host != "wrk-01" || wrk1.Old == nil || wrk1.New != nil || wrk1.NewHash != "" {
			t.Errorf("wrk-01 = %+v, want it only on the approved side", wrk1)
		}
		if wrk2.Host != "wrk-02" || wrk2.Old != nil || wrk2.New == nil || wrk2.OldHash != "" {
			t.Errorf("wrk-02 = %+v, want it only on the pending side", wrk2)
		}
	})

	t.Run("DiffNoPending", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(labHosts("cp-01")...)
		if _, _, err := e.r.Diff(t.Context(), labName); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Diff without a pending pair error = %v, want ErrNotFound", err)
		}
	})

	t.Run("UnboundHosts", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(labHosts("cp-01", "wrk-01")...)
		e.pending(labHosts("cp-01", "wrk-01", "wrk-02")...)
		e.bind(e.r.ApproveMachine, e.discovered(1), "cp-01")
		got, err := e.r.UnboundHosts(t.Context())
		if err != nil || len(got) != 1 || got[0] != (HostRef{Cluster: labName, Host: "wrk-01"}) {
			t.Fatalf("UnboundHosts = %+v, %v; want only lab/wrk-01", got, err)
		}
	})

	t.Run("ApprovedPairNone", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.pending(labHosts("cp-01")...)
		if _, err := e.r.ApprovedPair(t.Context(), e.c.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("ApprovedPair with only a pending pair error = %v, want ErrNotFound", err)
		}
	})

	t.Run("ApprovedPairCurrent", func(t *testing.T) {
		e := newApproveEnv(t, open, t0)
		e.approved(labHosts("cp-01")...)
		want := e.approved(labHosts("cp-01", "wrk-01")...).Pair
		got, err := e.r.ApprovedPair(t.Context(), e.c.ID)
		if err != nil || got.ID != want.ID || !got.ApprovedAt.Equal(want.ApprovedAt) || got.ApprovedBy != fleet.ActorCLI {
			t.Fatalf("ApprovedPair = %+v, %v; want %+v", got, err, want)
		}
	})
}
