package fleet

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"go.jacaudi.dev/hephaestus/internal/db"
)

// ErrUUIDExists is returned by AdoptByUUIDTx when a machine already holds the UUID.
var ErrUUIDExists = errors.New("fleet: a machine with this UUID already exists")

// Binding is the host a machine is bound to and the Talos image it installs
// there: the four columns design section 5.2 sets together.
type Binding struct {
	ClusterID, Host, TalosSchematic, TalosVersion string
}

// bindsHost reports whether trigger puts a machine into a host, and clearsHost
// whether it releases the host: the two rows of design section 5.2's binding
// table that change the columns of a machine that stays.
func bindsHost(t Trigger) bool {
	return t == TriggerApprove || t == TriggerAdopt || t == TriggerRebind
}

func clearsHost(t Trigger) bool { return t == TriggerUnassign || t == TriggerUnbind }

// bindingColumns are the machine columns setBinding writes.
func bindingColumns() []string {
	return []string{"cluster_id", "host_name", "talos_schematic", "talos_version"}
}

// setBinding puts b on m, or clears all four fields when b is nil.
func setBinding(m *Machine, b *Binding) {
	if b == nil {
		m.ClusterID, m.HostName, m.TalosSchematic, m.TalosVersion = nil, nil, nil, nil
		return
	}
	m.ClusterID, m.HostName = new(b.ClusterID), new(b.Host)
	m.TalosSchematic, m.TalosVersion = new(b.TalosSchematic), new(b.TalosVersion)
}

// AdoptByUUIDRequest creates a machine from a typed UUID and binds it to a host
// as installed. UUID must be normalized. Guard carries HostExists and
// HostUnbound, which the caller computes (HostBoundTx); AdoptByUUIDTx fills
// Guard.Junk.
type AdoptByUUIDRequest struct {
	UUID  string
	Arch  Arch
	Bind  Binding
	Guard Guard
	Actor string
	At    time.Time
}

// AdoptByUUIDTx creates the machine of req in the caller's transaction q,
// installed and bound, with an empty serial and product and no MACs until a
// boot reports them (IdentityObserved is false), and audits it as
// transition.adopt_by_uuid. A UUID that is junk, or not normalized and so never
// matched by a boot, is the *TransitionError "junk_uuid"; a host the guard
// refuses, or the unique index does, is "host_not_found" or "host_bound"; a
// UUID some machine holds is ErrUUIDExists.
func AdoptByUUIDTx(ctx context.Context, q bun.IDB, req AdoptByUUIDRequest) (Machine, error) {
	g := req.Guard
	g.Junk = JunkPattern(req.UUID) || NormalizeUUID(req.UUID) != req.UUID
	lc, err := Next(Lifecycle{}, TriggerAdoptByUUID, g)
	if err != nil {
		return Machine{}, err
	}
	held, err := q.NewSelect().Model((*Machine)(nil)).Where("smbios_uuid = ?", req.UUID).Exists(ctx)
	if err != nil {
		return Machine{}, fmt.Errorf("check UUID in use: %w", err)
	}
	if held {
		return Machine{}, ErrUUIDExists
	}

	at := stamp(req.At)
	m := Machine{
		ID: rand.Text(), UUID: req.UUID, Arch: req.Arch, State: lc.State, CreatedAt: at, LastSeenAt: at,
		// IdentityObserved stays false: the insert writes the whole struct.
	}
	setBinding(&m, &req.Bind)
	_, err = q.NewInsert().Model(&m).Exec(ctx)
	if db.IsUniqueViolation(err) {
		return Machine{}, &TransitionError{Trigger: TriggerAdoptByUUID, Reason: reasonHostBound}
	}
	if err != nil {
		return Machine{}, fmt.Errorf("insert machine: %w", err)
	}
	ev := AuditEvent{
		At: at, Actor: req.Actor, Action: "transition." + string(TriggerAdoptByUUID), MachineID: m.ID,
		Detail: transitionDetail{From: stateName(StateNone), To: stateName(lc.State)},
	}
	if err := InsertAudit(ctx, q, ev); err != nil {
		return Machine{}, err
	}
	return loadMachine(ctx, q, m.ID)
}

// HostBoundTx reports whether a machine other than exceptMachineID is bound to
// the host. A machine rebinding to its own former host passes its own ID, so
// its own row does not count.
func HostBoundTx(ctx context.Context, q bun.IDB, clusterID, host, exceptMachineID string) (bool, error) {
	bound, err := q.NewSelect().Model((*Machine)(nil)).
		Where("cluster_id = ?", clusterID).Where("host_name = ?", host).Where("id <> ?", exceptMachineID).Exists(ctx)
	if err != nil {
		return false, fmt.Errorf("check host %s/%s bound: %w", clusterID, host, err)
	}
	return bound, nil
}

// MachinesInClusterTx returns the machines bound to the cluster, ordered by ID,
// with their MACs. On Postgres it locks their rows (FOR UPDATE) until the
// caller's transaction ends; SQLite has one writer, so there is nothing to lock.
func MachinesInClusterTx(ctx context.Context, q bun.IDB, clusterID string) ([]Machine, error) {
	var out []Machine
	sel := q.NewSelect().Model(&out).Where("cluster_id = ?", clusterID).Order("id")
	if q.Dialect().Name() == dialect.PG {
		sel = sel.For("UPDATE")
	}
	if err := sel.Scan(ctx); err != nil {
		return nil, fmt.Errorf("load machines of cluster %s: %w", clusterID, err)
	}
	if err := hydrateAll(ctx, q, out); err != nil {
		return nil, err
	}
	return out, nil
}

// SetTalosTupleTx sets the Talos schematic and version of a machine in the
// caller's transaction, ErrNotFound when no machine has the ID. The approval of
// a pair uses it, so the tuple of every machine of the cluster matches the
// approved pair.
func SetTalosTupleTx(ctx context.Context, q bun.IDB, machineID, schematic, version string) error {
	found, err := applied(ctx, q.NewUpdate().Model((*Machine)(nil)).
		Set("talos_schematic = ?", schematic).Set("talos_version = ?", version).Where("id = ?", machineID))
	if err != nil {
		return fmt.Errorf("set Talos tuple of %s: %w", machineID, err)
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrNotFound, machineID)
	}
	return nil
}
