package clusters

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/uptrace/bun"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

const (
	actionApproved      = "cluster.approved"
	actionDriftApproved = "cluster.drift_approved"
)

// ApproveClusterRequest approves PairID, which must still be the pending pair of the cluster named Cluster.
type ApproveClusterRequest struct {
	Cluster, PairID, Actor string
	At                     time.Time
}

// ApproveClusterResult is the pair an approval made the approved one and what it did to the fleet: Orphaned machines
// lost their host, Rearmed machines had their Talos tuple changed and wanted its talos_cache row, and Revoked tickets
// of the cluster were revoked.
type ApproveClusterResult struct {
	Pair                       Pair
	Orphaned, Rearmed, Revoked int
}

// approvedDetail is the detail of a cluster.approved audit event.
type approvedDetail struct {
	Name     string `json:"name"`
	Pair     string `json:"pair"`
	Orphaned int    `json:"orphaned"`
	Rearmed  int    `json:"rearmed"`
	Revoked  int    `json:"revoked"`
}

// ApproveCluster makes the pending pair the approved one, in one transaction that holds the cluster row first. The
// older approved pair is deleted with its renders and version, and every live ticket of the cluster is revoked. A
// machine of the cluster that is assigned, installing, install-unconfirmed or installed, or cordoned over one of
// those, whose host the new pair lacks, is orphaned (host_left); any other machine whose host the new pair lacks is
// left alone. A machine whose host the new pair has takes the host's Talos tuple, and when that changed, its
// talos_cache row is created or re-armed with the host's schematic body. It audits cluster.approved. A pair id that is
// not the pending one is ErrStale.
func (r *Repository) ApproveCluster(ctx context.Context, req ApproveClusterRequest) (ApproveClusterResult, error) {
	req.At = stamp(req.At)
	var out ApproveClusterResult
	err := db.RunInTxRetry(ctx, r.bdb, func(ctx context.Context, tx bun.Tx) error {
		c, err := findCluster(ctx, tx, columnName, req.Cluster, true)
		if err != nil {
			return err
		}
		if c.PendingPairID == "" || c.PendingPairID != req.PairID {
			return fmt.Errorf("%w: %s", ErrStale, req.PairID)
		}
		if err := promote(ctx, tx, c, req.Actor, req.At); err != nil {
			return err
		}
		if out, err = approveFleet(ctx, tx, c.ID, req); err != nil {
			return err
		}
		if out.Pair, err = loadPair(ctx, tx, req.PairID); err != nil {
			return err
		}
		return fleet.InsertAudit(ctx, tx, fleet.AuditEvent{
			At: req.At, Actor: req.Actor, Action: actionApproved,
			Detail: approvedDetail{Name: c.Name, Pair: req.PairID, Orphaned: out.Orphaned, Rearmed: out.Rearmed, Revoked: out.Revoked},
		})
	})
	if err != nil {
		return ApproveClusterResult{}, fmt.Errorf("clusters: approve pair %s of %s: %w", req.PairID, req.Cluster, err)
	}
	return out, nil
}

// approveFleet brings the machines of the cluster in line with the pair req approves, in the caller's transaction,
// which holds the cluster row: it locks the machines, revokes the cluster's tickets, orphans the machines whose host
// left, sets the tuple of the others whose host stays, and wants the talos_cache row of each whose tuple changed. It
// returns the counts; req.At is already stamped.
func approveFleet(ctx context.Context, tx bun.IDB, clusterID string, req ApproveClusterRequest) (ApproveClusterResult, error) {
	var out ApproveClusterResult
	hosts, err := pairHosts(ctx, tx, req.PairID)
	if err != nil {
		return out, err
	}
	machines, err := fleet.MachinesInClusterTx(ctx, tx, clusterID)
	if err != nil {
		return out, err
	}
	// Every machine row is held now, so their tickets can be taken. Revoking them all first makes the host_left
	// transitions below find none of their own, and the count is every ticket the approval revoked.
	if out.Revoked, err = fleet.RevokeClusterTicketsTx(ctx, tx, clusterID, req.At); err != nil {
		return out, err
	}
	var wants []fleet.TalosWant
	for i := range machines {
		m := &machines[i]
		row, in := hosts[orEmpty(m.HostName)]
		switch {
		case !in && leavesHost(m):
			leave := fleet.TransitionRequest{MachineID: m.ID, Trigger: fleet.TriggerHostLeft, Actor: req.Actor, At: req.At}
			if _, err := fleet.TransitionTx(ctx, tx, leave); err != nil {
				return out, err
			}
			out.Orphaned++
		case in && (orEmpty(m.TalosSchematic) != row.SchematicID || orEmpty(m.TalosVersion) != row.TalosVersion):
			if err := fleet.SetTalosTupleTx(ctx, tx, m.ID, row.SchematicID, row.TalosVersion); err != nil {
				return out, err
			}
			wants = append(wants, talosWant(&row, m.Arch, true))
		}
	}
	// Talos rows come last in the lock order.
	for _, w := range wants {
		if _, err := fleet.WantTalosTx(ctx, tx, w, req.At); err != nil {
			return out, err
		}
	}
	out.Rearmed = len(wants)
	return out, nil
}

// promote makes c's pending pair its approved one, both ids pointing at stored pairs throughout, then deletes the
// older approved pair.
func promote(ctx context.Context, tx bun.IDB, c Cluster, actor string, at time.Time) error {
	q := tx.NewUpdate().Model((*Cluster)(nil)).Set("approved_pair_id = ?", c.PendingPairID).Set("pending_pair_id = NULL").
		Where("id = ?", c.ID).Where("pending_pair_id = ?", c.PendingPairID)
	if err := updateOne(ctx, q, db.ErrConflict); err != nil {
		return err
	}
	q = tx.NewUpdate().Model((*Pair)(nil)).Set("approved_at = ?", at).Set("approved_by = ?", actor).Where("id = ?", c.PendingPairID)
	if err := updateOne(ctx, q, fmt.Errorf("%w: pair %s", ErrNotFound, c.PendingPairID)); err != nil {
		return err
	}
	if c.ApprovedPairID == "" {
		return nil
	}
	return deletePair(ctx, tx, c.ApprovedPairID)
}

// leavesHost reports whether m is orphaned when its host leaves: it is in a bound state, or cordoned over one.
func leavesHost(m *fleet.Machine) bool {
	return m.State.Bound() || m.State == fleet.StateCordoned && m.Prior.Bound()
}

// pairHosts returns the stored renders of a pair's hosts by host; their sealed columns stay sealed.
func pairHosts(ctx context.Context, q bun.IDB, pairID string) (map[string]hostRenderRow, error) {
	var rows []hostRenderRow
	if err := q.NewSelect().Model(&rows).Where("pair_id = ?", pairID).Scan(ctx); err != nil {
		return nil, fmt.Errorf("read hosts of pair %s: %w", pairID, err)
	}
	out := make(map[string]hostRenderRow, len(rows))
	for i := range rows {
		out[rows[i].HostName] = rows[i]
	}
	return out, nil
}

// loadPair reads the pair with the id in q.
func loadPair(ctx context.Context, q bun.IDB, id string) (Pair, error) {
	var p Pair
	if err := q.NewSelect().Model(&p).Where("id = ?", id).Scan(ctx); err != nil {
		return Pair{}, fmt.Errorf("read pair %s: %w", id, err)
	}
	return p.utc(), nil
}

// talosWant is the want of the talos_cache row a machine of arch on row's host installs from, with row's body.
func talosWant(row *hostRenderRow, arch fleet.Arch, rearm bool) fleet.TalosWant {
	return fleet.TalosWant{
		Key:  fleet.TalosKey{Schematic: row.SchematicID, Version: row.TalosVersion, Arch: arch},
		Body: row.SchematicBody, Rearm: rearm,
	}
}

func orEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// MachineRequest puts machine MachineID into Host of the cluster named Cluster.
type MachineRequest struct {
	MachineID, Cluster, Host, Actor string
	At                              time.Time
}

// ApproveMachine binds a discovered machine to a host of the cluster's approved pair as assigned, with the host's
// Talos tuple, and creates or re-arms its talos_cache row with the host's schematic body, in one transaction. A host
// the approved pair lacks is the *fleet.TransitionError "host_not_found", one another machine holds is "host_bound",
// and a junk machine is "junk_uuid".
func (r *Repository) ApproveMachine(ctx context.Context, req MachineRequest) (fleet.Machine, error) {
	return r.bindMachine(ctx, req, fleet.TriggerApprove)
}

// AdoptMachine binds a discovered machine to a host of the cluster's approved pair as installed, its stored render
// the host's approved one, as ApproveMachine does but without a talos_cache row.
func (r *Repository) AdoptMachine(ctx context.Context, req MachineRequest) (fleet.Machine, error) {
	return r.bindMachine(ctx, req, fleet.TriggerAdopt)
}

// RebindMachine binds an orphaned machine to a host of the cluster's approved pair, its own former host included, as
// ApproveMachine does; it lands installed or assigned as the machine was before it was orphaned.
func (r *Repository) RebindMachine(ctx context.Context, req MachineRequest) (fleet.Machine, error) {
	return r.bindMachine(ctx, req, fleet.TriggerRebind)
}

// bindMachine applies trigger to the machine with the binding of req's host, in one transaction that holds the
// cluster row first, and wants the host's talos_cache row unless the machine is adopted.
func (r *Repository) bindMachine(ctx context.Context, req MachineRequest, trigger fleet.Trigger) (fleet.Machine, error) {
	at := stamp(req.At)
	var out fleet.Machine
	err := db.RunInTxRetry(ctx, r.bdb, func(ctx context.Context, tx bun.Tx) error {
		// The machine's own row does not hold the host against it: a rebind may return to its former host.
		h, err := targetHost(ctx, tx, req.Cluster, req.Host, req.MachineID)
		if err != nil {
			return err
		}
		out, err = fleet.TransitionTx(ctx, tx, fleet.TransitionRequest{
			MachineID: req.MachineID, Trigger: trigger, Guard: h.guard, Bind: &h.bind, Actor: req.Actor, At: at,
		})
		if err != nil || trigger == fleet.TriggerAdopt {
			return err
		}
		_, err = fleet.WantTalosTx(ctx, tx, talosWant(&h.row, out.Arch, true), at)
		return err
	})
	if err != nil {
		return fleet.Machine{}, fmt.Errorf("clusters: %s %s into %s/%s: %w", trigger, req.MachineID, req.Cluster, req.Host, err)
	}
	return out, nil
}

// AdoptByUUIDRequest creates a machine with UUID and Arch, bound as installed to Host of the cluster named Cluster.
type AdoptByUUIDRequest struct {
	UUID                 string
	Arch                 fleet.Arch
	Cluster, Host, Actor string
	At                   time.Time
}

// AdoptByUUID creates the machine and binds it to a host of the cluster's approved pair as installed, as
// fleet.AdoptByUUIDTx does, in one transaction that holds the cluster row first. It creates no talos_cache row. A
// host the approved pair lacks is the *fleet.TransitionError "host_not_found" and one a machine holds "host_bound".
func (r *Repository) AdoptByUUID(ctx context.Context, req AdoptByUUIDRequest) (fleet.Machine, error) {
	at := stamp(req.At)
	var out fleet.Machine
	err := db.RunInTxRetry(ctx, r.bdb, func(ctx context.Context, tx bun.Tx) error {
		h, err := targetHost(ctx, tx, req.Cluster, req.Host, "")
		if err != nil {
			return err
		}
		out, err = fleet.AdoptByUUIDTx(ctx, tx, fleet.AdoptByUUIDRequest{
			UUID: req.UUID, Arch: req.Arch, Bind: h.bind, Guard: h.guard, Actor: req.Actor, At: at,
		})
		return err
	})
	if err != nil {
		return fleet.Machine{}, fmt.Errorf("clusters: adopt %s into %s/%s: %w", req.UUID, req.Cluster, req.Host, err)
	}
	return out, nil
}

// bindTarget is a host as a bind sees it: the guard's facts about it, the binding it gives and its approved render.
type bindTarget struct {
	guard fleet.Guard
	bind  fleet.Binding
	row   hostRenderRow
}

// targetHost holds the row of the cluster named cluster, then reads host's render in its approved pair and whether a
// machine other than exceptMachineID is bound to it. An unknown cluster is ErrNotFound; a host the approved pair
// lacks, or a cluster without one, has HostExists false.
func targetHost(ctx context.Context, tx bun.IDB, cluster, host, exceptMachineID string) (bindTarget, error) {
	c, err := findCluster(ctx, tx, columnName, cluster, true)
	if err != nil {
		return bindTarget{}, err
	}
	row, found, err := approvedRow(ctx, tx, c.ApprovedPairID, host)
	if err != nil {
		return bindTarget{}, err
	}
	bound, err := fleet.HostBoundTx(ctx, tx, c.ID, host, exceptMachineID)
	if err != nil {
		return bindTarget{}, err
	}
	return bindTarget{
		guard: fleet.Guard{HostExists: found, HostUnbound: !bound},
		bind:  fleet.Binding{ClusterID: c.ID, Host: host, TalosSchematic: row.SchematicID, TalosVersion: row.TalosVersion},
		row:   row,
	}, nil
}

// approvedRow reads host's stored render in pairID, its sealed columns unopened; found is false when pairID is "" or
// has no such host.
func approvedRow(ctx context.Context, q bun.IDB, pairID, host string) (row hostRenderRow, found bool, err error) {
	if pairID == "" {
		return hostRenderRow{}, false, nil
	}
	err = q.NewSelect().Model(&row).Where("pair_id = ?", pairID).Where("host_name = ?", host).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return hostRenderRow{}, false, nil
	}
	if err != nil {
		return hostRenderRow{}, false, fmt.Errorf("read render of %s/%s: %w", pairID, host, err)
	}
	return row, true, nil
}

// EnsureAssets creates the talos_cache row the machine installs from, wanted with its host's approved schematic body,
// when the row is absent, and gives a row without a body that body; it never re-arms a failed or broken row. It is
// /boot's row 8. A machine not bound to a host of its cluster's approved pair is ErrNotFound.
func (r *Repository) EnsureAssets(ctx context.Context, m fleet.Machine, at time.Time) error {
	return r.wantAssets(ctx, m, at, false)
}

// WantAssets is EnsureAssets that also moves a failed or broken row back to wanted, its attempts reset and its
// recorded assets kept: machine fetch.
func (r *Repository) WantAssets(ctx context.Context, m fleet.Machine, at time.Time) error {
	return r.wantAssets(ctx, m, at, true)
}

// wantAssets wants the row of the machine's approved host, in one transaction that holds the cluster row first and
// reads the machine afresh, so its host is the one bound now.
func (r *Repository) wantAssets(ctx context.Context, m fleet.Machine, at time.Time, rearm bool) error {
	notBound := fmt.Errorf("%w: machine %s is not bound to a host of an approved pair", ErrNotFound, m.ID)
	if m.ClusterID == nil {
		return fmt.Errorf("clusters: want assets: %w", notBound)
	}
	err := db.RunInTxRetry(ctx, r.bdb, func(ctx context.Context, tx bun.Tx) error {
		c, err := findCluster(ctx, tx, columnID, *m.ClusterID, true)
		if err != nil {
			return err
		}
		var cur fleet.Machine
		err = tx.NewSelect().Model(&cur).Where("id = ?", m.ID).Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return notBound
		}
		if err != nil {
			return fmt.Errorf("read machine %s: %w", m.ID, err)
		}
		if orEmpty(cur.ClusterID) != c.ID {
			return notBound
		}
		// The tuple and the approved render change only while the cluster row is held, as it is: they agree.
		row, found, err := approvedRow(ctx, tx, c.ApprovedPairID, orEmpty(cur.HostName))
		if err != nil {
			return err
		}
		if !found {
			return notBound
		}
		_, err = fleet.WantTalosTx(ctx, tx, talosWant(&row, cur.Arch, rearm), at)
		return err
	})
	if err != nil {
		return fmt.Errorf("clusters: want assets of %s: %w", m.ID, err)
	}
	return nil
}

// Drift reports whether the render of the machine's host in its cluster's approved pair records drift. An unbound
// machine, or a host the approved pair lacks, has none.
func (r *Repository) Drift(ctx context.Context, m fleet.Machine) (bool, error) {
	if m.ClusterID == nil || m.HostName == nil {
		return false, nil
	}
	var row hostRenderRow
	err := r.bdb.NewSelect().Model(&row).Column("drift").
		Where("pair_id = (SELECT c.approved_pair_id FROM clusters AS c WHERE c.id = ?)", *m.ClusterID).
		Where("host_name = ?", *m.HostName).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("clusters: drift of %s: %w", m.ID, err)
	}
	return row.Drift, nil
}

// DriftApproveRequest makes the drift of Host in the approved pair of the cluster named Cluster its stored render,
// while the drift's config hash is still Hash.
type DriftApproveRequest struct {
	Cluster, Host, Hash, Actor string
	At                         time.Time
}

// ApproveDrift makes the host's drift hashes and drift render its stored ones and clears the drift, in one transaction
// that holds the cluster row first. The pair and its version stay, so no ticket is revoked. It audits
// cluster.drift_approved. A host whose drift config hash is not Hash, or that has no drift, is ErrStale; an unknown
// cluster or host is ErrNotFound.
func (r *Repository) ApproveDrift(ctx context.Context, req DriftApproveRequest) error {
	at := stamp(req.At)
	err := db.RunInTxRetry(ctx, r.bdb, func(ctx context.Context, tx bun.Tx) error {
		c, err := findCluster(ctx, tx, columnName, req.Cluster, true)
		if err != nil {
			return err
		}
		var row hostRenderRow
		err = tx.NewSelect().Model(&row).Where("pair_id = ?", c.ApprovedPairID).Where("host_name = ?", req.Host).Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: host %s of the approved pair", ErrNotFound, req.Host)
		}
		if err != nil {
			return fmt.Errorf("read render of %s: %w", req.Host, err)
		}
		if !row.Drift || row.DriftConfigHash != req.Hash {
			return fmt.Errorf("%w: host %s has no drift with config hash %s", ErrStale, req.Host, req.Hash)
		}
		redacted, err := r.key.OpenField(renderField(row.PairID, row.HostName, columnDriftRedact), row.DriftRedactedSealed)
		if err != nil {
			return fmt.Errorf("open drift render of %s: %w", req.Host, err)
		}
		q := tx.NewUpdate().Model((*hostRenderRow)(nil)).
			Set("config_hash = ?", row.DriftConfigHash).Set("display_hash = ?", row.DriftDisplayHash).
			Set("redacted_sealed = ?", r.key.SealField(renderField(row.PairID, row.HostName, columnRedacted), redacted)).
			Set("drift = ?", false).Set("drift_config_hash = NULL").Set("drift_display_hash = NULL").Set("drift_redacted_sealed = NULL").
			Where("pair_id = ?", row.PairID).Where("host_name = ?", row.HostName).Where("drift_config_hash = ?", req.Hash)
		if err := updateOne(ctx, q, db.ErrConflict); err != nil {
			return err
		}
		return fleet.InsertAudit(ctx, tx, fleet.AuditEvent{
			At: at, Actor: req.Actor, Action: actionDriftApproved, Detail: driftDetail{Cluster: c.Name, Host: req.Host},
		})
	})
	if err != nil {
		return fmt.Errorf("clusters: approve drift of %s/%s: %w", req.Cluster, req.Host, err)
	}
	return nil
}

// HostDiff compares two renders of a host: Old and New are the redacted renders, OldHash and NewHash their config
// hashes. A side without the host has a nil render and an empty hash. SecretValuesChanged is true when both sides
// display the same but their configs differ: only secret values changed. The renders are redacted, so they can be
// shown.
type HostDiff struct {
	Host                string
	Old, New            []byte
	OldHash, NewHash    string
	SecretValuesChanged bool
}

// hostDiff compares host's render before and after; either may be nil.
func hostDiff(host string, before, after *HostRow) HostDiff {
	d := HostDiff{Host: host}
	if before != nil {
		d.Old, d.OldHash = before.Redacted, before.ConfigHash
	}
	if after != nil {
		d.New, d.NewHash = after.Redacted, after.ConfigHash
	}
	d.SecretValuesChanged = before != nil && after != nil && before.DisplayHash == after.DisplayHash && before.ConfigHash != after.ConfigHash
	return d
}

// Diff compares the cluster's pending pair with its approved one, host by host in name order, and returns the
// pending pair's id, the id an approval of what was shown names. A cluster without a pending pair is ErrNotFound;
// without an approved one, every host is new.
func (r *Repository) Diff(ctx context.Context, cluster string) (pairID string, hosts []HostDiff, err error) {
	c, err := findCluster(ctx, r.bdb, columnName, cluster, false)
	if err != nil {
		return "", nil, fmt.Errorf("clusters: diff %s: %w", cluster, err)
	}
	if c.PendingPairID == "" {
		return "", nil, fmt.Errorf("clusters: diff %s: %w: no pending pair", cluster, ErrNotFound)
	}
	// ponytail: the renders are read outside a transaction; a pair replaced meanwhile shows as hosts on one side,
	// and the approval of the id returned is then refused as stale. Read in one transaction if that ever misleads.
	pending, err := r.HostRows(ctx, c.PendingPairID)
	if err != nil {
		return "", nil, err
	}
	var approved []HostRow
	if c.ApprovedPairID != "" {
		if approved, err = r.HostRows(ctx, c.ApprovedPairID); err != nil {
			return "", nil, err
		}
	}
	sides := map[string][2]*HostRow{}
	for i := range approved {
		s := sides[approved[i].Host]
		s[0] = &approved[i]
		sides[approved[i].Host] = s
	}
	for i := range pending {
		s := sides[pending[i].Host]
		s[1] = &pending[i]
		sides[pending[i].Host] = s
	}
	for _, host := range slices.Sorted(maps.Keys(sides)) {
		s := sides[host]
		hosts = append(hosts, hostDiff(host, s[0], s[1]))
	}
	return c.PendingPairID, hosts, nil
}

// DriftDiff compares the stored render of host in the cluster's approved pair (Old) with its drift render (New). A
// host without drift, or a cluster without an approved pair, is ErrNotFound.
func (r *Repository) DriftDiff(ctx context.Context, cluster, host string) (HostDiff, error) {
	c, err := findCluster(ctx, r.bdb, columnName, cluster, false)
	if err != nil {
		return HostDiff{}, fmt.Errorf("clusters: drift diff %s/%s: %w", cluster, host, err)
	}
	var rows []HostRow
	if c.ApprovedPairID != "" {
		if rows, err = r.HostRows(ctx, c.ApprovedPairID); err != nil {
			return HostDiff{}, err
		}
	}
	i := slices.IndexFunc(rows, func(row HostRow) bool { return row.Host == host && row.Drift })
	if i < 0 {
		return HostDiff{}, fmt.Errorf("clusters: drift diff %s/%s: %w: no drift recorded", cluster, host, ErrNotFound)
	}
	stored := &rows[i]
	drift := HostRow{Redacted: stored.DriftRedacted, ConfigHash: stored.DriftConfigHash, DisplayHash: stored.DriftDisplayHash}
	return hostDiff(host, stored, &drift), nil
}

// HostRef names a host of a cluster by the cluster's name.
type HostRef struct{ Cluster, Host string }

// UnboundHosts returns the hosts of every cluster's approved pair that no machine is bound to, by cluster name and
// then host.
func (r *Repository) UnboundHosts(ctx context.Context) ([]HostRef, error) {
	var out []HostRef
	err := r.bdb.NewSelect().TableExpr("clusters AS c").
		ColumnExpr("c.name AS cluster, hr.host_name AS host").
		Join("JOIN host_renders AS hr ON hr.pair_id = c.approved_pair_id").
		Where("NOT EXISTS (SELECT 1 FROM machines AS m WHERE m.cluster_id = c.id AND m.host_name = hr.host_name)").
		OrderExpr("c.name, hr.host_name").Scan(ctx, &out)
	if err != nil {
		return nil, fmt.Errorf("clusters: unbound hosts: %w", err)
	}
	return out, nil
}

// ApprovedPair returns the cluster's approved pair; ErrNotFound when it has none.
func (r *Repository) ApprovedPair(ctx context.Context, clusterID string) (Pair, error) {
	var p Pair
	err := r.bdb.NewSelect().Model(&p).
		Where("id = (SELECT c.approved_pair_id FROM clusters AS c WHERE c.id = ?)", clusterID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Pair{}, fmt.Errorf("clusters: approved pair of %s: %w", clusterID, ErrNotFound)
	}
	if err != nil {
		return Pair{}, fmt.Errorf("clusters: approved pair of %s: %w", clusterID, err)
	}
	return p.utc(), nil
}
