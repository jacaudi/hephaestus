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
	// ReportsKept is how many inventory reports are stored per machine; the
	// oldest are dropped.
	ReportsKept = 10

	// ActorCLI is the audit actor of an admin's command.
	ActorCLI = "cli"
	// ActorSystem is the audit actor of hephaestus acting on its own.
	ActorSystem = "system"

	actionUUIDConflict     = "alert.uuid_conflict"
	actionHold             = "hold"
	actionSetting          = "settings.unknown_action"
	actionIdentityObserved = "identity.observed"
	// holdCoalesceWindow is how long an identical hold is folded into the last one.
	holdCoalesceWindow = time.Hour

	reportSourceIPXE  = "ipxe"
	settingUnknownKey = "unknown_action"
)

// ErrBootNextChanged is returned by RecordBoot when its ClearBootNext no longer
// names the stored flag: another process set it again or cleared it since the
// caller read it. Nothing was stored; the caller reads the machine again.
var ErrBootNextChanged = errors.New("boot-next changed since it was read")

// ErrIdentityRace is returned by RecordBoot when its ObserveIdentity no longer
// holds: the machine's identity was observed since the caller read it. Nothing
// was stored; the caller reads the machine and decides again.
var ErrIdentityRace = errors.New("fleet: the machine's identity was observed since it was read")

// ErrIdentityConflict is returned by Observe when the stored serial or product
// of the machine holding the reported UUID differs from the report's.
var ErrIdentityConflict = errors.New("identity conflict: stored serial/product differ from the report")

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

// inventoryReportRow is the inventory_reports table. Facts is TEXT holding the
// report's bytes.
type inventoryReportRow struct {
	bun.BaseModel `bun:"table:inventory_reports"`

	ID         int64     `bun:"id,pk,autoincrement"`
	MachineID  string    `bun:"machine_id"`
	Source     string    `bun:"source"`
	Facts      string    `bun:"facts"`
	ReceivedAt time.Time `bun:"received_at"`
}

// settingRow is the settings table.
type settingRow struct {
	bun.BaseModel `bun:"table:settings"`

	Key   string `bun:"key,pk"`
	Value string `bun:"value"`
}

// settingDetail is the detail of a settings.* audit event.
type settingDetail struct {
	From string `json:"from"`
	To   string `json:"to"`
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

// identityDetail is the detail of an identity.observed audit event.
type identityDetail struct {
	Serial  string `json:"serial"`
	Product string `json:"product"`
}

// AuditEvent is one entry of the audit log: what Actor did at At, to the
// machine MachineID when it names one. Detail is stored as JSON.
type AuditEvent struct {
	At                       time.Time
	Actor, Action, MachineID string
	Detail                   any
}

// Repository stores machines, their boot events and the audit log.
type Repository struct {
	bdb *bun.DB
}

// NewRepository returns a Repository over d.
func NewRepository(d *db.DB) *Repository { return &Repository{bdb: d.Bun()} }

// Lookup finds the machine an Identity belongs to. A junk-pattern UUID says
// nothing about the machine, so it is found by the MAC it owns, and only when
// that machine is itself keyed by a junk-pattern UUID. Otherwise the UUID
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

// ListMachines returns every machine, oldest first and then by ID, with their MACs.
func (r *Repository) ListMachines(ctx context.Context) ([]Machine, error) {
	var out []Machine
	if err := r.bdb.NewSelect().Model(&out).Order("created_at", "id").Scan(ctx); err != nil {
		return nil, fmt.Errorf("fleet: list machines: %w", err)
	}
	if err := hydrateAll(ctx, r.bdb, out); err != nil {
		return nil, fmt.Errorf("fleet: list machines: %w", err)
	}
	return out, nil
}

// Audit stores ev on the repository's database, for a caller outside a
// transaction; InsertAudit stores it in the caller's.
func (r *Repository) Audit(ctx context.Context, ev AuditEvent) error {
	if err := InsertAudit(ctx, r.bdb, ev); err != nil {
		return fmt.Errorf("fleet: audit: %w", err)
	}
	return nil
}

// RecordBoot stores one observed boot in a single transaction and returns the
// machine as stored. With no MachineID it creates the machine (Trigger must be
// TriggerFirstContact); otherwise it records the boot against that machine. A
// Conflict boot touches nothing but the machine's junk flag and the audit log.
// The boot event is stored too, except a hold identical to the machine's last
// one within the hour, and the machine's boot events and MACs stay bounded. On
// a known boot that is not a Conflict, ObserveIdentity, ClearBootNext and then
// WantFetch run before the boot event is stored; any of them on another boot is
// ErrInvalidTransition. ObserveIdentity stores the boot's serial and product as
// the machine's first report, and ErrIdentityRace, storing nothing, says the
// identity was observed since the caller read the machine.
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
	// Bind is the host and Talos image the machine is bound to. It is required
	// for approve, adopt, adopt_by_uuid and rebind, and ignored otherwise.
	Bind  *Binding
	Actor string
	At    time.Time
}

// Transition applies req.Trigger to the machine in a single transaction and
// audits it, as TransitionTx does inside a caller's.
func (r *Repository) Transition(ctx context.Context, req TransitionRequest) (Machine, error) {
	var out Machine
	err := r.bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		out, err = TransitionTx(ctx, tx, req)
		return err
	})
	if err != nil {
		return Machine{}, fmt.Errorf("fleet: transition %s: %w", req.Trigger, err)
	}
	return out, nil
}

// Observation is what Observe found: the machine, whether the call created it
// and whether the call raised the UUID-conflict alert.
type Observation struct {
	Machine          Machine
	Created, Alerted bool
}

// Observe records one /discover observation in a single transaction. An
// unknown machine is created exactly as a first /boot creates it, with a
// transition.first_contact audit event. A known machine whose serial or product
// differs from id is a clone: Observe marks its UUID junk and alerts once (the
// first call reports Alerted), writes nothing else, and returns the
// Observation with ErrIdentityConflict. Any other known machine is touched. A
// machine whose identity was never observed takes id's serial and product as
// its identity, unless another call did first, and then they are compared.
func (r *Repository) Observe(ctx context.Context, id Identity, arch Arch, at time.Time) (Observation, error) {
	var (
		obs      Observation
		conflict bool
	)
	err := r.bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		obs, conflict, err = observe(ctx, tx, id, arch, at)
		return err
	})
	if err != nil {
		return Observation{}, fmt.Errorf("fleet: observe: %w", err)
	}
	if conflict {
		return obs, fmt.Errorf("fleet: observe: %w", ErrIdentityConflict)
	}
	return obs, nil
}

// LatestReport returns the facts of the machine's newest inventory report, and
// false when it has none.
func (r *Repository) LatestReport(ctx context.Context, machineID string) ([]byte, bool, error) {
	facts, ok, err := latestFacts(ctx, r.bdb, machineID)
	if err != nil {
		return nil, false, fmt.Errorf("fleet: latest report: %w", err)
	}
	if !ok {
		return nil, false, nil
	}
	return []byte(facts), true, nil
}

// Report is one inventory report for RecordReport.
type Report struct {
	// Facts is the report as received; it is stored and returned byte for byte.
	Facts []byte
	// MACs are the machine's addresses the report carries, already normalized.
	MACs []string
	// Manufacturer replaces the machine's when not empty.
	Manufacturer string
	At           time.Time
}

// RecordReport applies a machine's inventory report in a single transaction:
// it attaches the report's MACs, sets last_seen_at and a non-empty
// Manufacturer, and stores the facts unless they equal the latest report's
// byte for byte. It reports whether the facts were stored; only the newest
// ReportsKept reports are kept.
func (r *Repository) RecordReport(ctx context.Context, machineID string, rep Report) (bool, error) {
	var stored bool
	err := r.bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		stored, err = recordReport(ctx, tx, machineID, rep)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("fleet: record report: %w", err)
	}
	return stored, nil
}

// UnknownAction returns the unknown_action setting; a missing row reads as
// UnknownActionLocal.
func (r *Repository) UnknownAction(ctx context.Context) (UnknownAction, error) {
	value, err := storedUnknownAction(ctx, r.bdb)
	if err != nil {
		return "", fmt.Errorf("fleet: unknown action: %w", err)
	}
	var a UnknownAction
	if err := a.UnmarshalText([]byte(value)); err != nil {
		return "", fmt.Errorf("fleet: unknown action: stored setting: %w", err)
	}
	return a, nil
}

// SetUnknownAction stores the unknown_action setting and audits the change in a
// single transaction.
func (r *Repository) SetUnknownAction(ctx context.Context, a UnknownAction, actor string, at time.Time) error {
	err := r.bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// The raw stored value, so a corrupt one can still be replaced.
		from, err := storedUnknownAction(ctx, tx)
		if err != nil {
			return err
		}
		row := settingRow{Key: settingUnknownKey, Value: string(a)}
		_, err = tx.NewInsert().Model(&row).On("CONFLICT (key) DO UPDATE").Set("value = EXCLUDED.value").Exec(ctx)
		if err != nil {
			return fmt.Errorf("store setting: %w", err)
		}
		audit := auditEventRow{At: at.UTC(), Actor: actor, Action: actionSetting}
		return insertAudit(ctx, tx, audit, settingDetail{From: from, To: string(a)})
	})
	if err != nil {
		return fmt.Errorf("fleet: set unknown action: %w", err)
	}
	return nil
}

// observe is Observe's transaction body; conflict is true for the clone case,
// whose writes must commit, so it is not an error here.
func observe(ctx context.Context, tx bun.IDB, id Identity, arch Arch, at time.Time) (obs Observation, conflict bool, err error) {
	m, ok, err := lookupMachine(ctx, tx, id)
	if err != nil {
		return Observation{}, false, err
	}
	if ok && !m.IdentityObserved {
		if m, err = settleIdentity(ctx, tx, m, id, at); err != nil {
			return Observation{}, false, err
		}
	}
	switch {
	case !ok:
		m, err = createMachine(ctx, tx, BootRecord{Identity: id, Arch: arch, Trigger: TriggerFirstContact, At: at})
		if err != nil {
			return Observation{}, false, err
		}
		// A transition is applied with its event; /boot creations carry a boot
		// event instead.
		audit := auditEventRow{At: at.UTC(), Actor: ActorSystem, Action: "transition." + string(TriggerFirstContact), MachineID: m.ID}
		if err := insertAudit(ctx, tx, audit, transitionDetail{From: stateName(StateNone), To: stateName(m.State)}); err != nil {
			return Observation{}, false, err
		}
		m, err = loadMachine(ctx, tx, m.ID)
		return Observation{Machine: m, Created: true}, false, err
	case m.Serial != id.Serial || m.Product != id.Product:
		obs.Machine = m
		if !m.Junk {
			if err := markUUIDJunk(ctx, tx, m, id, at); err != nil {
				return Observation{}, false, err
			}
			obs.Machine.Junk, obs.Alerted = true, true
		}
		return obs, true, nil
	}
	m.LastSeenAt = at.UTC()
	if _, err := tx.NewUpdate().Model(&m).Column("last_seen_at").WherePK().Exec(ctx); err != nil {
		return Observation{}, false, fmt.Errorf("update machine %s: %w", m.ID, err)
	}
	return Observation{Machine: m}, false, nil
}

// settleIdentity gives a machine that was read with its identity unobserved the
// serial and product of id. When another transaction observed it first, it
// returns the machine as that left it, which observe then compares with id.
func settleIdentity(ctx context.Context, tx bun.IDB, m Machine, id Identity, at time.Time) (Machine, error) {
	observed, err := observeIdentity(ctx, tx, m.ID, id, at)
	if err != nil {
		return Machine{}, err
	}
	if !observed {
		return loadMachine(ctx, tx, m.ID)
	}
	m.Serial, m.Product, m.IdentityObserved = id.Serial, id.Product, true
	return m, nil
}

// observeIdentity stores id's serial and product as the machine's identity and
// audits it, only while the machine's identity is unobserved. It reports false,
// writing nothing, when it already is.
func observeIdentity(ctx context.Context, q bun.IDB, machineID string, id Identity, at time.Time) (bool, error) {
	observed, err := applied(ctx, q.NewUpdate().Model((*Machine)(nil)).
		Set("serial = ?", id.Serial).Set("product = ?", id.Product).Set("identity_observed = ?", true).
		Where("id = ?", machineID).Where("identity_observed = ?", false))
	if err != nil {
		return false, fmt.Errorf("observe identity of %s: %w", machineID, err)
	}
	if !observed {
		return false, nil
	}
	ev := AuditEvent{At: at, Actor: ActorSystem, Action: actionIdentityObserved, MachineID: machineID, Detail: identityDetail{Serial: id.Serial, Product: id.Product}}
	return true, InsertAudit(ctx, q, ev)
}

func recordReport(ctx context.Context, tx bun.IDB, machineID string, rep Report) (bool, error) {
	at := rep.At.UTC()
	for _, mac := range rep.MACs {
		if err := upsertMAC(ctx, tx, machineID, mac, at); err != nil {
			return false, err
		}
	}
	m := Machine{ID: machineID, LastSeenAt: at, Manufacturer: rep.Manufacturer}
	cols := []string{"last_seen_at"}
	if rep.Manufacturer != "" {
		cols = append(cols, "manufacturer")
	}
	if _, err := tx.NewUpdate().Model(&m).Column(cols...).WherePK().Exec(ctx); err != nil {
		return false, fmt.Errorf("update machine %s: %w", machineID, err)
	}

	facts := string(rep.Facts)
	latest, ok, err := latestFacts(ctx, tx, machineID)
	if err != nil || (ok && latest == facts) {
		return false, err
	}
	row := inventoryReportRow{MachineID: machineID, Source: reportSourceIPXE, Facts: facts, ReceivedAt: at}
	if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
		return false, fmt.Errorf("insert report: %w", err)
	}
	oldest := tx.NewSelect().Model((*inventoryReportRow)(nil)).Column("id").
		Where("machine_id = ?", machineID).Order("id DESC").Limit(1).Offset(ReportsKept)
	_, err = tx.NewDelete().Model((*inventoryReportRow)(nil)).
		Where("machine_id = ?", machineID).Where("id <= (?)", oldest).Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("trim reports: %w", err)
	}
	return true, nil
}

// latestFacts returns the facts of the machine's newest report.
func latestFacts(ctx context.Context, q bun.IDB, machineID string) (string, bool, error) {
	var facts string
	err := q.NewSelect().Model((*inventoryReportRow)(nil)).Column("facts").
		Where("machine_id = ?", machineID).Order("id DESC").Limit(1).Scan(ctx, &facts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("load latest report: %w", err)
	}
	return facts, true, nil
}

// storedUnknownAction returns the stored unknown_action value, undecoded; a
// missing row is the default, UnknownActionLocal.
func storedUnknownAction(ctx context.Context, q bun.IDB) (string, error) {
	var value string
	err := q.NewSelect().Model((*settingRow)(nil)).Column("value").Where("key = ?", settingUnknownKey).Scan(ctx, &value)
	if errors.Is(err, sql.ErrNoRows) {
		return string(UnknownActionLocal), nil
	}
	if err != nil {
		return "", fmt.Errorf("read setting %s: %w", settingUnknownKey, err)
	}
	return value, nil
}

// lookupMachine finds the machine id names. A request with a junk-pattern UUID
// is keyed by its MAC, but only to a machine that is itself keyed by a
// junk-pattern UUID: a machine with a valid UUID is never that request, so the
// request gets its own record and cannot mark or move the valid one.
func lookupMachine(ctx context.Context, q bun.IDB, id Identity) (Machine, bool, error) {
	if JunkPattern(id.UUID) {
		owner, err := macOwner(ctx, q, id.MAC)
		if err != nil || owner == "" {
			return Machine{}, false, err
		}
		m, ok, err := foundMachine(loadMachine(ctx, q, owner))
		if err != nil || !ok || !JunkPattern(m.UUID) {
			return Machine{}, false, err
		}
		return m, true, nil
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

// hydrateAll is hydrate for every machine of ms.
func hydrateAll(ctx context.Context, q bun.IDB, ms []Machine) error {
	for i := range ms {
		if err := hydrate(ctx, q, &ms[i]); err != nil {
			return err
		}
	}
	return nil
}

// hydrate fills the MACs a machine row does not carry and puts its times in UTC.
func hydrate(ctx context.Context, q bun.IDB, m *Machine) error {
	m.CreatedAt, m.LastSeenAt, m.BootNextSetAt = m.CreatedAt.UTC(), m.LastSeenAt.UTC(), m.BootNextSetAt.UTC()
	err := q.NewSelect().Model((*machineMAC)(nil)).Column("mac").Where("machine_id = ?", m.ID).Order("mac").Scan(ctx, &m.MACs)
	if err != nil {
		return fmt.Errorf("load MACs of %s: %w", m.ID, err)
	}
	return nil
}

func recordBoot(ctx context.Context, tx bun.IDB, rec BootRecord) (Machine, error) {
	if (rec.ClearBootNext != nil || rec.WantFetch != nil) && (rec.MachineID == "" || rec.Conflict) {
		return Machine{}, fmt.Errorf("%w: clearing boot-next or wanting a fetch needs a known boot that is not a conflict", ErrInvalidTransition)
	}
	if rec.ObserveIdentity && (rec.MachineID == "" || rec.Conflict) {
		return Machine{}, fmt.Errorf("%w: observing the identity needs a known boot that is not a conflict", ErrInvalidTransition)
	}
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
	if err := applyBootNext(ctx, tx, m.ID, rec); err != nil {
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

// applyBootNext runs the boot-next fields of rec against the machine, which
// recordKnownBoot has already written: machines are locked before utility_cache.
func applyBootNext(ctx context.Context, tx bun.IDB, machineID string, rec BootRecord) error {
	if c := rec.ClearBootNext; c != nil {
		req := ClearBootNextRequest{MachineID: machineID, Entry: c.Entry, SetAt: c.SetAt, Reason: c.Reason, Actor: ActorSystem, At: rec.At}
		cleared, err := clearBootNext(ctx, tx, req)
		if err != nil {
			return err
		}
		if !cleared {
			return ErrBootNextChanged
		}
	}
	if rec.WantFetch != nil {
		return wantFetch(ctx, tx, *rec.WantFetch, rec.At)
	}
	return nil
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
		// The insert writes the whole struct, so the zero value would store false.
		IdentityObserved: true,
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

	// First, so a lost race stores nothing.
	if rec.ObserveIdentity {
		observed, err := observeIdentity(ctx, tx, m.ID, rec.Identity, rec.At)
		if err != nil {
			return StateNone, Machine{}, err
		}
		if !observed {
			return StateNone, Machine{}, ErrIdentityRace
		}
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

// markUUIDJunk marks every machine holding stored's UUID junk, revokes their live
// tickets and audits the conflict that proved the UUID is shared. It is the one
// write both /boot and /discover make when they find a UUID reported with another
// serial or product.
func markUUIDJunk(ctx context.Context, q bun.IDB, stored Machine, seen Identity, at time.Time) error {
	if _, err := q.NewUpdate().Model((*Machine)(nil)).Set("junk = ?", true).Where("smbios_uuid = ?", stored.UUID).Exec(ctx); err != nil {
		return fmt.Errorf("mark UUID junk: %w", err)
	}
	if err := revokeUUIDTicketsTx(ctx, q, stored.UUID, at); err != nil {
		return err
	}
	audit := auditEventRow{At: at.UTC(), Actor: ActorSystem, Action: actionUUIDConflict, MachineID: stored.ID}
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

// TransitionTx applies req.Trigger to the machine inside the caller's
// transaction q and audits it. A refused transition returns a *TransitionError
// and writes nothing. The binding columns follow design section 5.2: approve,
// adopt and rebind set them from req.Bind, unassign and unbind clear them, and
// every other trigger leaves them. A bind the unique index refuses is the
// *TransitionError "host_bound". A trigger that ends in StateNone (forget)
// deletes the machine, with its MACs and boot events, and returns it with State
// StateNone. Every transition but config_served revokes the machine's live
// tickets.
func TransitionTx(ctx context.Context, q bun.IDB, req TransitionRequest) (Machine, error) {
	m, err := loadMachine(ctx, q, req.MachineID)
	if err != nil {
		return Machine{}, err
	}
	req.Guard.Junk = m.Junk
	from := m.Lifecycle()
	lc, err := Next(from, req.Trigger, req.Guard)
	if err != nil {
		return Machine{}, err
	}
	m.State, m.Prior, m.OrphanedFrom = lc.State, lc.Prior, lc.OrphanedFrom

	cols := []string{"state", "prior_state", "orphaned_from"}
	switch {
	case bindsHost(req.Trigger):
		if req.Bind == nil {
			return Machine{}, fmt.Errorf("%w: %s needs a binding", ErrInvalidTransition, req.Trigger)
		}
		setBinding(&m, req.Bind)
		cols = append(cols, bindingColumns()...)
	case clearsHost(req.Trigger):
		setBinding(&m, nil)
		cols = append(cols, bindingColumns()...)
	}

	if lc.State == StateNone {
		_, err = q.NewDelete().Model((*Machine)(nil)).Where("id = ?", m.ID).Exec(ctx)
	} else {
		_, err = q.NewUpdate().Model(&m).Column(cols...).WherePK().Exec(ctx)
	}
	if db.IsUniqueViolation(err) {
		return Machine{}, &TransitionError{From: from, Trigger: req.Trigger, Reason: reasonHostBound}
	}
	if err != nil {
		return Machine{}, fmt.Errorf("store state of %s: %w", m.ID, err)
	}
	// The machine row is written, so it is held: tickets come after it. Recording
	// a served config consumes the ticket itself and revokes the rest.
	if req.Trigger != TriggerConfigServed {
		if _, err := RevokeMachineTicketsTx(ctx, q, m.ID, req.At); err != nil {
			return Machine{}, err
		}
	}
	ev := AuditEvent{
		At: req.At, Actor: req.Actor, Action: "transition." + string(req.Trigger), MachineID: m.ID,
		Detail: transitionDetail{From: stateName(from.State), To: stateName(lc.State)},
	}
	if err := InsertAudit(ctx, q, ev); err != nil {
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

// InsertAudit stores ev in the caller's transaction or on its connection, its
// Detail encoded as JSON.
func InsertAudit(ctx context.Context, q bun.IDB, ev AuditEvent) error {
	row := auditEventRow{At: stamp(ev.At), Actor: ev.Actor, Action: ev.Action, MachineID: ev.MachineID}
	return insertAudit(ctx, q, row, ev.Detail)
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
