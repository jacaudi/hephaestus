package fleet

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

const actionBootNextSet = "boot_next.set"

// AmbiguousError is returned by ResolveMachine when a UUID names more than one
// machine, as a junk-pattern UUID does: Machines is how many, and MACs are
// every MAC of those machines, sorted, for the admin to name one by.
type AmbiguousError struct {
	Ref      string
	Machines int
	MACs     []string
}

// Error formats as "<ref> matches <n> machines; name one by MAC: <MACs>".
func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%s matches %d machines; name one by MAC: %s", e.Ref, e.Machines, strings.Join(e.MACs, ", "))
}

// setDetail is the detail of a boot_next.set audit event; Replaced is the entry
// the flag held before, or "".
type setDetail struct {
	Entry    string `json:"entry"`
	SetAt    string `json:"set_at"`
	Replaced string `json:"replaced"`
}

// SetBootNextRequest arms a machine's boot-next: the entry its next boot serves
// and the catalog pin its content must have. BoundMAC, normalized, binds a
// junk machine's flag to the MAC it was named by; it is empty for any
// other machine.
type SetBootNextRequest struct {
	MachineID, Entry, Pin string
	BoundMAC              string
	At                    time.Time
}

// SetBootNextResult is the machine after SetBootNext and what asking for the
// entry's content found.
type SetBootNextResult struct {
	Machine Machine
	Fetch   FetchRequest
}

// ResolveMachine finds the machine ref names: a MAC (any form NormalizeMAC
// accepts) finds the machine owning it, a UUID the machines holding it. A UUID
// shared by several machines is an *AmbiguousError. Anything else, or a ref
// nothing has, is ErrNotFound.
func (r *Repository) ResolveMachine(ctx context.Context, ref string) (Machine, error) {
	if mac, err := NormalizeMAC(ref); err == nil {
		owner, err := macOwner(ctx, r.bdb, mac)
		if err != nil {
			return Machine{}, err
		}
		if owner == "" {
			return Machine{}, fmt.Errorf("%w: %s", ErrNotFound, ref)
		}
		return loadMachine(ctx, r.bdb, owner)
	}

	uuid := NormalizeUUID(ref)
	if uuid == "" {
		return Machine{}, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	var cands []Machine
	if err := r.bdb.NewSelect().Model(&cands).Where("smbios_uuid = ?", uuid).Order("id").Scan(ctx); err != nil {
		return Machine{}, fmt.Errorf("load machines by UUID: %w", err)
	}
	var macs []string
	for i := range cands {
		if err := hydrate(ctx, r.bdb, &cands[i]); err != nil {
			return Machine{}, err
		}
		macs = append(macs, cands[i].MACs...)
	}
	switch len(cands) {
	case 0:
		return Machine{}, fmt.Errorf("%w: %s", ErrNotFound, ref)
	case 1:
		return cands[0], nil
	}
	slices.Sort(macs)
	return Machine{}, &AmbiguousError{Ref: ref, Machines: len(cands), MACs: macs}
}

// SetBootNext arms the machine's boot-next with req.Entry at req.At, replacing
// any flag, and audits boot_next.set by actor "cli" with the entry it replaced.
// In the same transaction it asks for the entry's content for the machine's
// architecture at priority 1, re-arming a failed or broken row at req.Pin.
func (r *Repository) SetBootNext(ctx context.Context, req SetBootNextRequest) (SetBootNextResult, error) {
	var out SetBootNextResult
	err := r.bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		out, err = setBootNext(ctx, tx, req)
		return err
	})
	if err != nil {
		return SetBootNextResult{}, fmt.Errorf("fleet: set boot-next of %s: %w", req.MachineID, err)
	}
	return out, nil
}

func setBootNext(ctx context.Context, tx bun.IDB, req SetBootNextRequest) (SetBootNextResult, error) {
	// Lock the machine's row before reading it (a no-op write, as SQLite has no
	// FOR UPDATE), so the flag read is the one this transaction replaces, and
	// before utility_cache, the order every transaction takes.
	if _, err := tx.NewUpdate().Model((*Machine)(nil)).Set("boot_next = boot_next").Where("id = ?", req.MachineID).Exec(ctx); err != nil {
		return SetBootNextResult{}, fmt.Errorf("lock machine %s: %w", req.MachineID, err)
	}
	m, err := loadMachine(ctx, tx, req.MachineID)
	if err != nil {
		return SetBootNextResult{}, err
	}

	at := stamp(req.At)
	_, err = tx.NewUpdate().Model((*Machine)(nil)).Set("boot_next = ?", req.Entry).Set("boot_next_set_at = ?", at).
		Set("boot_next_mac = NULLIF(?, '')", req.BoundMAC).Where("id = ?", m.ID).Exec(ctx)
	if err != nil {
		return SetBootNextResult{}, fmt.Errorf("set boot_next of %s: %w", m.ID, err)
	}
	audit := auditEventRow{At: at, Actor: ActorCLI, Action: actionBootNextSet, MachineID: m.ID}
	detail := setDetail{Entry: req.Entry, SetAt: at.Format(time.RFC3339Nano), Replaced: m.BootNext}
	if err := insertAudit(ctx, tx, audit, detail); err != nil {
		return SetBootNextResult{}, err
	}

	want := FetchWant{Key: CacheKey{Entry: req.Entry, Arch: m.Arch}, Pin: req.Pin}
	fetch, err := requestFetch(ctx, tx, want, bootNextPriority, at)
	if err != nil {
		return SetBootNextResult{}, err
	}
	m.BootNext, m.BootNextSetAt, m.BootNextMAC = req.Entry, at, req.BoundMAC
	return SetBootNextResult{Machine: m, Fetch: fetch}, nil
}

// ClearBootNext compare-and-clears the machine's boot-next in a transaction of
// its own, as clearBootNext does. It reports whether it cleared one.
func (r *Repository) ClearBootNext(ctx context.Context, req ClearBootNextRequest) (bool, error) {
	var cleared bool
	err := r.bdb.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		cleared, err = clearBootNext(ctx, tx, req)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("fleet: clear boot-next of %s: %w", req.MachineID, err)
	}
	return cleared, nil
}

// ListBootNext returns the machines with a boot-next flag, oldest flag first
// and then by ID, with their MACs.
func (r *Repository) ListBootNext(ctx context.Context) ([]Machine, error) {
	var out []Machine
	err := r.bdb.NewSelect().Model(&out).Where("boot_next IS NOT NULL").Order("boot_next_set_at", "id").Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("fleet: list boot-next: %w", err)
	}
	if err := hydrateAll(ctx, r.bdb, out); err != nil {
		return nil, fmt.Errorf("fleet: list boot-next: %w", err)
	}
	return out, nil
}
