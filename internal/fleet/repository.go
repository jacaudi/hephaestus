package fleet

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/uptrace/bun"

	"go.jacaudi.dev/hephaestus/internal/db"
)

const (
	actorSystem        = "system"
	actionUUIDConflict = "alert.uuid_conflict"
	actionHold         = "hold"
	// holdCoalesceWindow is how long an identical hold is folded into the last one.
	holdCoalesceWindow = time.Hour
)

// machineMAC is the machine_macs table.
type machineMAC struct {
	bun.BaseModel `bun:"table:machine_macs"`

	MAC       string    `bun:"mac,pk"`
	MachineID string    `bun:"machine_id"`
	SeenAt    time.Time `bun:"seen_at"`
}

// bootEventRow is the boot_events table.
type bootEventRow struct {
	bun.BaseModel `bun:"table:boot_events"`

	ID          int64     `bun:"id,pk,autoincrement"`
	MachineID   string    `bun:"machine_id"`
	At          time.Time `bun:"at"`
	DecisionRow int       `bun:"decision_row"`
	Action      string    `bun:"action"`
	Reason      string    `bun:"reason"`
	FromState   State     `bun:"from_state,nullzero"`
	ToState     State     `bun:"to_state,nullzero"`
}

// auditEventRow is the audit_events table. Detail is TEXT holding JSON.
type auditEventRow struct {
	bun.BaseModel `bun:"table:audit_events"`

	ID        int64     `bun:"id,pk,autoincrement"`
	At        time.Time `bun:"at"`
	Actor     string    `bun:"actor"`
	Action    string    `bun:"action"`
	MachineID string    `bun:"machine_id,nullzero"`
	Detail    string    `bun:"detail"`
}

// conflictDetail is the detail of an alert.uuid_conflict audit event.
type conflictDetail struct {
	UUID          string `json:"uuid"`
	MAC           string `json:"mac"`
	StoredSerial  string `json:"stored_serial"`
	StoredProduct string `json:"stored_product"`
	SeenSerial    string `json:"seen_serial"`
	SeenProduct   string `json:"seen_product"`
}

// transitionDetail is the detail of a transition.* audit event.
type transitionDetail struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Repository stores machines, their boot events and the audit log.
type Repository struct {
	bdb *bun.DB
}

// NewRepository returns a Repository over d.
func NewRepository(d *db.DB) *Repository { return &Repository{bdb: d.Bun()} }

// Lookup finds the machine an Identity belongs to. A junk UUID says nothing
// about the machine, so it is found by the MAC it owns. Otherwise the UUID
// finds it; when every machine holding that UUID is junk, the one owning the
// MAC wins, then the oldest with the same serial and product.
func (r *Repository) Lookup(ctx context.Context, id Identity) (Machine, bool, error) {
	m, ok, err := lookupMachine(ctx, r.bdb, id)
	if err != nil {
		return Machine{}, false, fmt.Errorf("fleet: lookup: %w", err)
	}
	return m, ok, nil
}

// Get returns the machine with the given ID, or ErrNotFound.
func (r *Repository) Get(ctx context.Context, id string) (Machine, error) {
	return loadMachine(ctx, r.bdb, id)
}

// RecordBoot stores one observed boot in a single transaction and returns the
// machine as stored. With no MachineID it creates the machine (Trigger must be
// TriggerFirstContact); otherwise it records the boot against that machine. A
// Conflict boot touches nothing but the machine's junk flag and the audit log.
// The boot event is stored too, except a hold identical to the machine's last
// one within the hour, and the machine's boot events and MACs stay bounded.
func (r *Repository) RecordBoot(ctx context.Context, rec BootRecord) (Machine, error) {
	var out Machine
	err := r.bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		out, err = recordBoot(ctx, tx, rec)
		return err
	})
	if err != nil {
		return Machine{}, fmt.Errorf("fleet: record boot: %w", err)
	}
	return out, nil
}

// TransitionRequest is an admin or system lifecycle change for Transition.
type TransitionRequest struct {
	MachineID string
	Trigger   Trigger
	// Guard carries the facts about the host; Transition fills Guard.Junk from
	// the stored machine.
	Guard Guard
	Actor string
	At    time.Time
}

// Transition applies req.Trigger to the machine in a single transaction and
// audits it. A refused transition returns a *TransitionError and writes
// nothing. A trigger that ends in StateNone (forget) deletes the machine, with
// its MACs and boot events, and returns it with State StateNone.
func (r *Repository) Transition(ctx context.Context, req TransitionRequest) (Machine, error) {
	var out Machine
	err := r.bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		out, err = transition(ctx, tx, req)
		return err
	})
	if err != nil {
		return Machine{}, fmt.Errorf("fleet: transition %s: %w", req.Trigger, err)
	}
	return out, nil
}

func lookupMachine(ctx context.Context, q bun.IDB, id Identity) (Machine, bool, error) {
	if JunkPattern(id.UUID) {
		owner, err := macOwner(ctx, q, id.MAC)
		if err != nil || owner == "" {
			return Machine{}, false, err
		}
		return foundMachine(loadMachine(ctx, q, owner))
	}

	var cands []Machine
	if err := q.NewSelect().Model(&cands).Where("smbios_uuid = ?", id.UUID).Order("created_at", "id").Scan(ctx); err != nil {
		return Machine{}, false, fmt.Errorf("load machines by UUID: %w", err)
	}
	i := slices.IndexFunc(cands, func(m Machine) bool { return !m.Junk })
	if i < 0 {
		owner, err := macOwner(ctx, q, id.MAC)
		if err != nil {
			return Machine{}, false, err
		}
		i = slices.IndexFunc(cands, func(m Machine) bool { return owner != "" && m.ID == owner })
	}
	if i < 0 {
		i = slices.IndexFunc(cands, func(m Machine) bool { return m.Serial == id.Serial && m.Product == id.Product })
	}
	if i < 0 {
		return Machine{}, false, nil
	}
	m := cands[i]
	if err := hydrate(ctx, q, &m); err != nil {
		return Machine{}, false, err
	}
	return m, true, nil
}

// foundMachine turns a load that may be ErrNotFound into Lookup's result.
func foundMachine(m Machine, err error) (Machine, bool, error) {
	if errors.Is(err, ErrNotFound) {
		return Machine{}, false, nil
	}
	return m, err == nil, err
}

// macOwner returns the ID of the machine owning mac, or "" when none does or
// mac is empty.
func macOwner(ctx context.Context, q bun.IDB, mac string) (string, error) {
	if mac == "" {
		return "", nil
	}
	var owner string
	err := q.NewSelect().Model((*machineMAC)(nil)).Column("machine_id").Where("mac = ?", mac).Scan(ctx, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find MAC owner: %w", err)
	}
	return owner, nil
}

func loadMachine(ctx context.Context, q bun.IDB, id string) (Machine, error) {
	m := Machine{ID: id}
	err := q.NewSelect().Model(&m).WherePK().Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Machine{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Machine{}, fmt.Errorf("load machine %s: %w", id, err)
	}
	if err := hydrate(ctx, q, &m); err != nil {
		return Machine{}, err
	}
	return m, nil
}

// hydrate fills the MACs a machine row does not carry and puts its times in UTC.
func hydrate(ctx context.Context, q bun.IDB, m *Machine) error {
	m.CreatedAt, m.LastSeenAt = m.CreatedAt.UTC(), m.LastSeenAt.UTC()
	err := q.NewSelect().Model((*machineMAC)(nil)).Column("mac").Where("machine_id = ?", m.ID).Order("mac").Scan(ctx, &m.MACs)
	if err != nil {
		return fmt.Errorf("load MACs of %s: %w", m.ID, err)
	}
	return nil
}

func recordBoot(ctx context.Context, tx bun.IDB, rec BootRecord) (Machine, error) {
	var (
		from State
		m    Machine
		err  error
	)
	if rec.MachineID == "" {
		m, err = createMachine(ctx, tx, rec)
	} else {
		from, m, err = recordKnownBoot(ctx, tx, rec)
	}
	if err != nil {
		return Machine{}, err
	}
	row := bootEventRow{
		MachineID: m.ID, At: rec.At.UTC(), DecisionRow: rec.Event.Row, Action: rec.Event.Action, Reason: rec.Event.Reason,
	}
	if from != m.State {
		row.FromState, row.ToState = from, m.State
	}
	if err := insertBootEvent(ctx, tx, row); err != nil {
		return Machine{}, err
	}
	return loadMachine(ctx, tx, m.ID)
}

// createMachine inserts the machine and its MAC and returns the machine.
func createMachine(ctx context.Context, tx bun.IDB, rec BootRecord) (Machine, error) {
	// Only first contact leaves StateNone without a host, so Next refuses every
	// other trigger here.
	lc, err := Next(Lifecycle{}, rec.Trigger, Guard{})
	if err != nil {
		return Machine{}, err
	}
	id := rec.Identity
	junk := JunkPattern(id.UUID)
	if junk && id.MAC == "" {
		return Machine{}, ErrUnidentifiable
	}
	n, err := tx.NewSelect().Model((*Machine)(nil)).Count(ctx)
	if err != nil {
		return Machine{}, fmt.Errorf("count machines: %w", err)
	}
	if n >= MaxMachines {
		return Machine{}, ErrFleetFull
	}
	if !junk {
		// A valid UUID that is already stored only reaches here as a clone of a
		// junked machine; the unique index covers valid UUIDs only.
		if junk, err = tx.NewSelect().Model((*Machine)(nil)).Where("smbios_uuid = ?", id.UUID).Exists(ctx); err != nil {
			return Machine{}, fmt.Errorf("check UUID in use: %w", err)
		}
	}

	at := rec.At.UTC()
	m := Machine{
		ID: rand.Text(), UUID: id.UUID, Junk: junk, Serial: id.Serial, Product: id.Product,
		Arch: rec.Arch, State: lc.State, CreatedAt: at, LastSeenAt: at,
	}
	if _, err := tx.NewInsert().Model(&m).Exec(ctx); err != nil {
		return Machine{}, fmt.Errorf("insert machine: %w", err)
	}
	if id.MAC != "" {
		if err := upsertMAC(ctx, tx, m.ID, id.MAC, at); err != nil {
			return Machine{}, err
		}
	}
	return m, nil
}

// recordKnownBoot applies a boot of an existing machine and returns the state
// it was in and the machine after.
func recordKnownBoot(ctx context.Context, tx bun.IDB, rec BootRecord) (State, Machine, error) {
	m, err := loadMachine(ctx, tx, rec.MachineID)
	if err != nil {
		return StateNone, Machine{}, err
	}
	if rec.Conflict {
		if rec.MarkJunk {
			if err := markUUIDJunk(ctx, tx, m, rec.Identity, rec.At); err != nil {
				return StateNone, Machine{}, err
			}
		}
		return m.State, m, nil
	}

	at := rec.At.UTC()
	if rec.Identity.MAC != "" {
		if err := upsertMAC(ctx, tx, m.ID, rec.Identity.MAC, at); err != nil {
			return StateNone, Machine{}, err
		}
	}
	from := m.State
	cols := []string{"arch", "last_seen_at"}
	if rec.Trigger == TriggerNextBoot {
		lc, err := Next(m.Lifecycle(), rec.Trigger, Guard{Junk: m.Junk})
		if err != nil {
			return StateNone, Machine{}, err
		}
		m.State, m.Prior, m.OrphanedFrom = lc.State, lc.Prior, lc.OrphanedFrom
		cols = append(cols, "state", "prior_state", "orphaned_from")
	}
	m.Arch, m.LastSeenAt = rec.Arch, at
	if _, err := tx.NewUpdate().Model(&m).Column(cols...).WherePK().Exec(ctx); err != nil {
		return StateNone, Machine{}, fmt.Errorf("update machine %s: %w", m.ID, err)
	}
	return from, m, nil
}

// markUUIDJunk marks every machine holding stored's UUID junk and audits the
// conflict that proved the UUID is shared. It is the one write both /boot and
// /discover make when they find a UUID reported with another serial or product.
func markUUIDJunk(ctx context.Context, q bun.IDB, stored Machine, seen Identity, at time.Time) error {
	if _, err := q.NewUpdate().Model((*Machine)(nil)).Set("junk = ?", true).Where("smbios_uuid = ?", stored.UUID).Exec(ctx); err != nil {
		return fmt.Errorf("mark UUID junk: %w", err)
	}
	audit := auditEventRow{At: at.UTC(), Actor: actorSystem, Action: actionUUIDConflict, MachineID: stored.ID}
	return insertAudit(ctx, q, audit, conflictDetail{
		UUID: stored.UUID, MAC: seen.MAC,
		StoredSerial: stored.Serial, StoredProduct: stored.Product,
		SeenSerial: seen.Serial, SeenProduct: seen.Product,
	})
}

// upsertMAC attaches mac to the machine, moving it from any machine that held
// it, and drops the machine's least recently seen MACs beyond MACsKept. Every
// MAC write goes through it so the cap always applies.
func upsertMAC(ctx context.Context, q bun.IDB, machineID, mac string, at time.Time) error {
	row := machineMAC{MAC: mac, MachineID: machineID, SeenAt: at.UTC()}
	_, err := q.NewInsert().Model(&row).
		On("CONFLICT (mac) DO UPDATE").
		Set("machine_id = EXCLUDED.machine_id").
		Set("seen_at = EXCLUDED.seen_at").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("upsert MAC: %w", err)
	}
	newest := q.NewSelect().Model((*machineMAC)(nil)).Column("mac").
		Where("machine_id = ?", machineID).Order("seen_at DESC", "mac").Limit(MACsKept)
	_, err = q.NewDelete().Model((*machineMAC)(nil)).
		Where("machine_id = ?", machineID).Where("mac NOT IN (?)", newest).Exec(ctx)
	if err != nil {
		return fmt.Errorf("trim MACs: %w", err)
	}
	return nil
}

// insertBootEvent stores row and drops the machine's events beyond
// BootEventsKept. A hold identical to the machine's last event, within
// holdCoalesceWindow of it, is not stored: a held machine re-chains every 30 s.
func insertBootEvent(ctx context.Context, q bun.IDB, row bootEventRow) error {
	if row.Action == actionHold {
		var last bootEventRow
		err := q.NewSelect().Model(&last).Where("machine_id = ?", row.MachineID).Order("id DESC").Limit(1).Scan(ctx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("load last boot event: %w", err)
		}
		if err == nil && last.Action == actionHold && last.Reason == row.Reason && last.At.After(row.At.Add(-holdCoalesceWindow)) {
			return nil
		}
	}

	if _, err := q.NewInsert().Model(&row).Exec(ctx); err != nil {
		return fmt.Errorf("insert boot event: %w", err)
	}
	oldest := q.NewSelect().Model((*bootEventRow)(nil)).Column("id").
		Where("machine_id = ?", row.MachineID).Order("id DESC").Limit(1).Offset(BootEventsKept)
	_, err := q.NewDelete().Model((*bootEventRow)(nil)).
		Where("machine_id = ?", row.MachineID).Where("id <= (?)", oldest).Exec(ctx)
	if err != nil {
		return fmt.Errorf("trim boot events: %w", err)
	}
	return nil
}

func transition(ctx context.Context, tx bun.IDB, req TransitionRequest) (Machine, error) {
	m, err := loadMachine(ctx, tx, req.MachineID)
	if err != nil {
		return Machine{}, err
	}
	req.Guard.Junk = m.Junk
	from := m.State
	lc, err := Next(m.Lifecycle(), req.Trigger, req.Guard)
	if err != nil {
		return Machine{}, err
	}
	m.State, m.Prior, m.OrphanedFrom = lc.State, lc.Prior, lc.OrphanedFrom

	if lc.State == StateNone {
		_, err = tx.NewDelete().Model((*Machine)(nil)).Where("id = ?", m.ID).Exec(ctx)
	} else {
		_, err = tx.NewUpdate().Model(&m).Column("state", "prior_state", "orphaned_from").WherePK().Exec(ctx)
	}
	if err != nil {
		return Machine{}, fmt.Errorf("store state of %s: %w", m.ID, err)
	}
	detail := transitionDetail{From: stateName(from), To: stateName(lc.State)}
	audit := auditEventRow{At: req.At.UTC(), Actor: req.Actor, Action: "transition." + string(req.Trigger), MachineID: m.ID}
	if err := insertAudit(ctx, tx, audit, detail); err != nil {
		return Machine{}, err
	}
	return m, nil
}

// stateName names a state for the audit log, where StateNone reads "none".
func stateName(s State) string {
	if s == StateNone {
		return "none"
	}
	return string(s)
}

// insertAudit stores row with detail encoded as its JSON Detail.
func insertAudit(ctx context.Context, q bun.IDB, row auditEventRow, detail any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("encode audit detail: %w", err)
	}
	row.Detail = string(b)
	if _, err := q.NewInsert().Model(&row).Exec(ctx); err != nil {
		return fmt.Errorf("insert audit event %s: %w", row.Action, err)
	}
	return nil
}
