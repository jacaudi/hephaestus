package fleet

import (
	"errors"
	"time"

	"github.com/uptrace/bun"
)

const (
	// MaxMachines is the most machines the fleet records; first contact beyond
	// it is refused with ErrFleetFull, which bounds storage against a flood of
	// forged identities.
	MaxMachines = 1024
	// BootEventsKept is how many boot events are stored per machine; the oldest
	// are dropped.
	BootEventsKept = 500
	// MACsKept is how many MAC addresses are stored per machine; the least
	// recently seen are dropped.
	MACsKept = 16
)

var (
	// ErrFleetFull is returned when a new machine would exceed MaxMachines.
	ErrFleetFull = errors.New("fleet is full: 1024 machines recorded")
	// ErrNotFound is returned when no machine has the requested ID.
	ErrNotFound = errors.New("machine not found")
	// ErrUnidentifiable is returned when a new machine has a junk UUID and no
	// MAC, so nothing could ever find its record again.
	ErrUnidentifiable = errors.New("machine cannot be identified: no valid UUID and no MAC")
)

// Machine is a stored machine: the bun model of the machines table.
type Machine struct {
	bun.BaseModel `bun:"table:machines"`

	ID           string    `bun:"id,pk"`
	UUID         string    `bun:"smbios_uuid"`
	Junk         bool      `bun:"junk"`
	Serial       string    `bun:"serial"`
	Product      string    `bun:"product"`
	Manufacturer string    `bun:"manufacturer"`
	Arch         Arch      `bun:"arch"`
	State        State     `bun:"state"`
	Prior        State     `bun:"prior_state,nullzero"`
	OrphanedFrom State     `bun:"orphaned_from,nullzero"`
	ClusterID    *string   `bun:"cluster_id"`
	HostName     *string   `bun:"host_name"`
	CreatedAt    time.Time `bun:"created_at"`
	LastSeenAt   time.Time `bun:"last_seen_at"`
	// MACs are the machine's addresses, sorted. Get and Lookup fill them.
	MACs []string `bun:"-"`
}

// Lifecycle returns the machine's state and the states it remembers.
func (m Machine) Lifecycle() Lifecycle {
	return Lifecycle{State: m.State, Prior: m.Prior, OrphanedFrom: m.OrphanedFrom}
}

// Identity is what a boot request says about the machine. UUID and MAC are
// already normalized: "" when absent or malformed.
type Identity struct {
	UUID, MAC, Serial, Product string
}

// BootEvent is the decision /boot made for a request: the row of design
// section 5.3's decision table, the action and the reason.
type BootEvent struct {
	Row            int
	Action, Reason string
}

// BootRecord is one observed boot for RecordBoot.
type BootRecord struct {
	Identity Identity
	Arch     Arch
	// MachineID is the machine's ID, or "" when this is its first contact.
	MachineID string
	// Trigger is applied to the machine's lifecycle: TriggerFirstContact when
	// MachineID is "", TriggerNextBoot to advance an installing machine, and
	// otherwise empty.
	Trigger Trigger
	// Conflict is true when the decision matched row 2: the machine's UUID was
	// seen with another serial or product.
	Conflict bool
	// MarkJunk asks for the machine's UUID to be marked junk and alerted on;
	// only the first conflict does.
	MarkJunk bool
	Event    BootEvent
	At       time.Time
}
