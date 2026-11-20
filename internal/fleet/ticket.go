package fleet

import (
	"time"

	"github.com/uptrace/bun"
)

// TicketTTL is how long a minted ticket can fetch its config: Talos retries
// /config for about this long.
const TicketTTL = 3 * time.Hour

// Ticket is a stored config ticket: the bun model of the tickets table. The row
// holds the SHA-256 of the token, never the token, and binds it to one machine,
// host, approved pair and secret version.
type Ticket struct {
	bun.BaseModel `bun:"table:tickets"`

	ID              string `bun:"id,pk"`
	MachineID       string `bun:"machine_id"`
	TokenSHA256     string `bun:"token_sha256"`
	ClusterID       string `bun:"cluster_id"`
	HostName        string `bun:"host_name"`
	PairID          string `bun:"pair_id"`
	SecretVersionID string `bun:"secret_version_id"`

	MintedAt  time.Time `bun:"minted_at"`
	ExpiresAt time.Time `bun:"expires_at"`
	// RequestedAt is when /config was first asked for with this ticket; zero
	// until then. ConsumedAt and RevokedAt are zero until the ticket is consumed
	// or revoked.
	RequestedAt time.Time `bun:"requested_at,nullzero"`
	ConsumedAt  time.Time `bun:"consumed_at,nullzero"`
	RevokedAt   time.Time `bun:"revoked_at,nullzero"`
}

// Live reports whether the ticket is neither consumed nor revoked, whatever its
// expiry: the state the database allows one row per machine in.
func (t Ticket) Live() bool { return t.ConsumedAt.IsZero() && t.RevokedAt.IsZero() }

// Valid reports whether the ticket can fetch a config at at: live and not yet expired.
func (t Ticket) Valid(at time.Time) bool { return t.Live() && at.Before(t.ExpiresAt) }

// MintRequest is a ticket to mint for MachineID, bound to a host, the approved
// pair and the pair's secret version.
type MintRequest struct {
	MachineID, ClusterID, HostName, PairID, SecretVersionID string
	At                                                      time.Time
}

// RecordConfigRequest records that /config was written in full with ticket
// TicketID at At.
type RecordConfigRequest struct {
	TicketID string
	At       time.Time
}

// RecordConfigResult is what RecordConfigServed did. Recorded is true when the
// ticket was consumed and the machine moved to installing, and Machine is then
// the machine as stored. AlreadyConsumed is true when another call recorded the
// delivery first. Neither is true when the machine is no longer assigned to the
// ticket's host.
type RecordConfigResult struct {
	Recorded        bool
	AlreadyConsumed bool
	Machine         Machine
}
