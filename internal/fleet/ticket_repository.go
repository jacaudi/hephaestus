package fleet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"go.jacaudi.dev/hephaestus/internal/db"
)

const (
	actionConfigServed = "config.served"

	// tokenBytes is the entropy of a ticket token: 32 bytes, 43 base64url characters.
	tokenBytes = 32
)

// configServedDetail is the detail of a config.served audit event.
type configServedDetail struct {
	TicketID string `json:"ticket_id"`
	PairID   string `json:"pair_id"`
}

// MintTicket mints a ticket for req.MachineID: in one transaction it locks the
// machine, revokes the machine's live tickets (expired or not) and stores the
// new one. It returns the token, which is shown once and never stored, and the
// stored ticket. A concurrent mint the unique index refuses is retried once with
// a fresh token. ErrNotFound means no machine has the ID.
func (r *Repository) MintTicket(ctx context.Context, req MintRequest) (string, Ticket, error) {
	var (
		token string
		out   Ticket
	)
	err := db.RunInTxRetry(ctx, r.bdb, func(ctx context.Context, tx bun.Tx) error {
		var err error
		token, out, err = mintTicket(ctx, tx, req)
		return err
	})
	if err != nil {
		return "", Ticket{}, fmt.Errorf("fleet: mint ticket: %w", err)
	}
	return token, out, nil
}

func mintTicket(ctx context.Context, tx bun.IDB, req MintRequest) (string, Ticket, error) {
	if _, err := lockMachine(ctx, tx, req.MachineID); err != nil {
		return "", Ticket{}, err
	}
	if _, err := RevokeMachineTicketsTx(ctx, tx, req.MachineID, req.At); err != nil {
		return "", Ticket{}, err
	}

	var raw [tokenBytes]byte
	rand.Read(raw[:])
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	at := stamp(req.At)
	t := Ticket{
		ID: rand.Text(), MachineID: req.MachineID, TokenSHA256: hashToken(token),
		ClusterID: req.ClusterID, HostName: req.HostName, PairID: req.PairID, SecretVersionID: req.SecretVersionID,
		MintedAt: at, ExpiresAt: at.Add(TicketTTL),
	}
	if _, err := tx.NewInsert().Model(&t).Exec(ctx); err != nil {
		if db.IsUniqueViolation(err) {
			// A concurrent mint won; the transaction ends here, so the retry is a new one.
			return "", Ticket{}, fmt.Errorf("insert ticket: %w", db.ErrConflict)
		}
		return "", Ticket{}, fmt.Errorf("insert ticket: %w", err)
	}
	return token, t, nil
}

// RevokeMachineTicketsTx revokes the machine's live tickets at at in the
// caller's transaction q and returns how many it revoked. A caller that already
// holds the machine's row, as every transition does, keeps the machine-before-
// ticket lock order.
func RevokeMachineTicketsTx(ctx context.Context, q bun.IDB, machineID string, at time.Time) (int, error) {
	n, err := revokeLive(ctx, q, at, "machine_id = ?", machineID)
	if err != nil {
		return 0, fmt.Errorf("revoke tickets of machine %s: %w", machineID, err)
	}
	return n, nil
}

// RevokeClusterTicketsTx revokes the live tickets of the cluster at at in the
// caller's transaction q and returns how many it revoked. The caller holds the
// cluster's machine rows (MachinesInClusterTx) first.
func RevokeClusterTicketsTx(ctx context.Context, q bun.IDB, clusterID string, at time.Time) (int, error) {
	n, err := revokeLive(ctx, q, at, "cluster_id = ?", clusterID)
	if err != nil {
		return 0, fmt.Errorf("revoke tickets of cluster %s: %w", clusterID, err)
	}
	return n, nil
}

// revokeUUIDTicketsTx revokes the live tickets of every machine holding uuid.
// markUUIDJunk calls it after it has written the machine rows.
func revokeUUIDTicketsTx(ctx context.Context, q bun.IDB, uuid string, at time.Time) error {
	holders := q.NewSelect().Model((*Machine)(nil)).Column("id").Where("smbios_uuid = ?", uuid)
	_, err := revokeLive(ctx, q, at, "machine_id IN (?)", holders)
	if err != nil {
		return fmt.Errorf("revoke tickets of UUID %s: %w", uuid, err)
	}
	return nil
}

// revokeLive revokes the live tickets that match where and counts them.
func revokeLive(ctx context.Context, q bun.IDB, at time.Time, where string, args ...any) (int, error) {
	res, err := q.NewUpdate().Model((*Ticket)(nil)).Set("revoked_at = ?", stamp(at)).
		Where("consumed_at IS NULL").Where("revoked_at IS NULL").Where(where, args...).Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// TicketByToken returns the ticket whose stored hash is the SHA-256 of token,
// whatever its state, and false when none is.
func (r *Repository) TicketByToken(ctx context.Context, token string) (Ticket, bool, error) {
	t, ok, err := loadTicket(ctx, r.bdb, "token_sha256 = ?", hashToken(token))
	if err != nil {
		return Ticket{}, false, fmt.Errorf("fleet: ticket by token: %w", err)
	}
	return t, ok, nil
}

// MarkTicketRequested sets the ticket's requested time to at unless it is set
// already: the first request of /config with the ticket wins.
func (r *Repository) MarkTicketRequested(ctx context.Context, id string, at time.Time) error {
	_, err := r.bdb.NewUpdate().Model((*Ticket)(nil)).Set("requested_at = ?", stamp(at)).
		Where("id = ?", id).Where("requested_at IS NULL").Exec(ctx)
	if err != nil {
		return fmt.Errorf("fleet: mark ticket %s requested: %w", id, err)
	}
	return nil
}

// RecordConfigServed records that /config was written in full with the ticket,
// in one transaction: it consumes the ticket, moves the machine to installing
// (config_served), revokes the machine's other live tickets and audits
// config.served with the ticket and pair. The machine must be assigned and still
// bound to the ticket's cluster and host. A revocation after the request was
// checked does not stop the recording: the body was delivered, so the record
// follows the fact. It records nothing, without an error, for a ticket that does
// not exist, for one already consumed (AlreadyConsumed) and for a machine that
// has moved on.
func (r *Repository) RecordConfigServed(ctx context.Context, req RecordConfigRequest) (RecordConfigResult, error) {
	var res RecordConfigResult
	err := db.RunInTxRetry(ctx, r.bdb, func(ctx context.Context, tx bun.Tx) error {
		var err error
		res, err = recordConfigServed(ctx, tx, req)
		return err
	})
	if err != nil {
		return RecordConfigResult{}, fmt.Errorf("fleet: record config served: %w", err)
	}
	return res, nil
}

func recordConfigServed(ctx context.Context, tx bun.IDB, req RecordConfigRequest) (RecordConfigResult, error) {
	t, m, found, err := lockTicketMachine(ctx, tx, req.TicketID)
	if err != nil || !found {
		return RecordConfigResult{}, err
	}
	if !t.ConsumedAt.IsZero() {
		return RecordConfigResult{AlreadyConsumed: true}, nil
	}
	if m.State != StateAssigned || !boundTo(m, t.ClusterID, t.HostName) {
		return RecordConfigResult{}, nil
	}
	served, err := recordDelivery(ctx, tx, t, req.At)
	if err != nil {
		return RecordConfigResult{}, err
	}
	return RecordConfigResult{Recorded: true, Machine: served}, nil
}

// lockTicketMachine reads the ticket to learn its machine, locks the machine
// and reads the ticket again under that lock, so the ticket is the state the
// machine's other writers see. found is false when the ticket or its machine is gone.
func lockTicketMachine(ctx context.Context, tx bun.IDB, ticketID string) (Ticket, Machine, bool, error) {
	t, ok, err := loadTicket(ctx, tx, "id = ?", ticketID)
	if err != nil || !ok {
		return Ticket{}, Machine{}, false, err
	}
	m, err := lockMachine(ctx, tx, t.MachineID)
	if errors.Is(err, ErrNotFound) {
		return Ticket{}, Machine{}, false, nil
	}
	if err != nil {
		return Ticket{}, Machine{}, false, err
	}
	t, ok, err = loadTicket(ctx, tx, "id = ?", ticketID)
	if err != nil || !ok {
		return Ticket{}, Machine{}, false, err
	}
	return t, m, true, nil
}

// boundTo reports whether the machine is bound to host of the cluster.
func boundTo(m Machine, clusterID, host string) bool {
	return m.ClusterID != nil && *m.ClusterID == clusterID && m.HostName != nil && *m.HostName == host
}

// recordDelivery stores a delivered config: the ticket is consumed, the machine
// moves to installing, the machine's other live tickets are revoked and the
// delivery is audited. The caller holds the machine row and has checked the machine.
func recordDelivery(ctx context.Context, tx bun.IDB, t Ticket, at time.Time) (Machine, error) {
	consumed, err := applied(ctx, tx.NewUpdate().Model((*Ticket)(nil)).Set("consumed_at = ?", stamp(at)).
		Where("id = ?", t.ID).Where("consumed_at IS NULL"))
	if err != nil {
		return Machine{}, fmt.Errorf("consume ticket %s: %w", t.ID, err)
	}
	if !consumed {
		return Machine{}, fmt.Errorf("consume ticket %s: %w", t.ID, db.ErrConflict)
	}
	served, err := TransitionTx(ctx, tx, TransitionRequest{MachineID: t.MachineID, Trigger: TriggerConfigServed, Actor: ActorSystem, At: at})
	if err != nil {
		return Machine{}, err
	}
	if _, err := RevokeMachineTicketsTx(ctx, tx, t.MachineID, at); err != nil {
		return Machine{}, err
	}
	ev := AuditEvent{
		At: at, Actor: ActorSystem, Action: actionConfigServed, MachineID: t.MachineID,
		Detail: configServedDetail{TicketID: t.ID, PairID: t.PairID},
	}
	if err := InsertAudit(ctx, tx, ev); err != nil {
		return Machine{}, err
	}
	return served, nil
}

// PruneTickets deletes the tickets consumed, revoked or expired before the given
// time and returns how many it deleted.
func (r *Repository) PruneTickets(ctx context.Context, before time.Time) (int, error) {
	at := stamp(before)
	res, err := r.bdb.NewDelete().Model((*Ticket)(nil)).
		Where("consumed_at < ? OR revoked_at < ? OR expires_at < ?", at, at, at).Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("fleet: prune tickets: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("fleet: prune tickets: %w", err)
	}
	return int(n), nil
}

// PendingTickets returns the tickets that are live, neither consumed nor
// expired, and were minted before mintedBefore: configs that should have been
// fetched by now. A ticket is unexpired when it expires after mintedBefore,
// which is the caller's now less its grace period. They are ordered by mint
// time.
func (r *Repository) PendingTickets(ctx context.Context, mintedBefore time.Time) ([]Ticket, error) {
	at := stamp(mintedBefore)
	var out []Ticket
	err := r.bdb.NewSelect().Model(&out).
		Where("consumed_at IS NULL").Where("revoked_at IS NULL").
		Where("minted_at < ?", at).Where("expires_at > ?", at).
		Order("minted_at", "id").Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("fleet: pending tickets: %w", err)
	}
	for i := range out {
		out[i] = out[i].utc()
	}
	return out, nil
}

// LiveTicket returns the machine's live ticket, and false when it has none.
func (r *Repository) LiveTicket(ctx context.Context, machineID string) (Ticket, bool, error) {
	t, ok, err := loadTicket(ctx, r.bdb, "machine_id = ? AND consumed_at IS NULL AND revoked_at IS NULL", machineID)
	if err != nil {
		return Ticket{}, false, fmt.Errorf("fleet: live ticket of %s: %w", machineID, err)
	}
	return t, ok, nil
}

// hashToken is what the tickets table stores for token: the lowercase hex of its SHA-256.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// loadTicket returns the one ticket that matches where, and false when none does.
func loadTicket(ctx context.Context, q bun.IDB, where string, args ...any) (Ticket, bool, error) {
	var t Ticket
	err := q.NewSelect().Model(&t).Where(where, args...).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Ticket{}, false, nil
	}
	if err != nil {
		return Ticket{}, false, err
	}
	return t.utc(), true, nil
}

// utc puts the ticket's times in UTC.
func (t Ticket) utc() Ticket {
	t.MintedAt, t.ExpiresAt = t.MintedAt.UTC(), t.ExpiresAt.UTC()
	t.RequestedAt, t.ConsumedAt, t.RevokedAt = t.RequestedAt.UTC(), t.ConsumedAt.UTC(), t.RevokedAt.UTC()
	return t
}

// lockMachine reads the machine and, on Postgres, holds its row (FOR UPDATE)
// until the caller's transaction ends: every transaction that touches a
// machine's tickets takes the machine row first. SQLite has one writer, so
// there is nothing to hold. It returns ErrNotFound when no machine has the ID.
func lockMachine(ctx context.Context, q bun.IDB, id string) (Machine, error) {
	m := Machine{ID: id}
	sel := q.NewSelect().Model(&m).WherePK()
	if q.Dialect().Name() == dialect.PG {
		sel = sel.For("UPDATE")
	}
	err := sel.Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Machine{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Machine{}, fmt.Errorf("lock machine %s: %w", id, err)
	}
	return m, nil
}
