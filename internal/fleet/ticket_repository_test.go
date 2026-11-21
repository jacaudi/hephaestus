package fleet

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"go.uber.org/goleak"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestTickets_SQLite(t *testing.T) { runTicketSuite(t, dbtest.SQLite, dbtest.SQLitePair) }

// hexSHA256 is the lowercase hex of the SHA-256 of s, as a ticket row stores its token.
func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// mintReq asks for a ticket of cluster lab for host, pair pair-1 and version
// ver-1, for an amd64 machine decided on with the fixture Talos tuple.
func mintReq(machineID, host string, at time.Time) MintRequest {
	return MintRequest{
		MachineID: machineID, ClusterID: labCluster, HostName: host, PairID: "pair-1", SecretVersionID: "ver-1",
		TalosSchematic: plainSchematic, TalosVersion: talosVersion, Arch: ArchAMD64, At: at,
	}
}

func mustMint(t *testing.T, r *Repository, req MintRequest) (string, Ticket) {
	t.Helper()
	token, tk, err := r.MintTicket(t.Context(), req)
	if err != nil {
		t.Fatalf("MintTicket(%+v): %v", req, err)
	}
	return token, tk
}

// insertTicket stores a live ticket of req as a row, without MintTicket's check
// of the machine: a ticket minted while the machine was still as req says,
// which has since changed.
func insertTicket(t *testing.T, d *db.DB, req MintRequest) Ticket {
	t.Helper()
	tk := Ticket{
		ID: rand.Text(), MachineID: req.MachineID, TokenSHA256: hexSHA256(rand.Text()),
		ClusterID: req.ClusterID, HostName: req.HostName, PairID: req.PairID, SecretVersionID: req.SecretVersionID,
		MintedAt: req.At, ExpiresAt: req.At.Add(TicketTTL),
	}
	if _, err := d.Bun().NewInsert().Model(&tk).Exec(t.Context()); err != nil {
		t.Fatalf("insert ticket %+v: %v", req, err)
	}
	return tk
}

// assignedMachine is machine n, discovered and then approved into host of cluster lab.
func assignedMachine(t *testing.T, r *Repository, n int, host string, at time.Time) Machine {
	t.Helper()
	return mustApprove(t, r, bootNextMachine(t, r, n, at).ID, host, at)
}

// readTickets reads every ticket row, oldest minted first.
func readTickets(t *testing.T, d *db.DB) []Ticket {
	t.Helper()
	var out []Ticket
	if err := d.Bun().NewSelect().Model(&out).Order("minted_at", "id").Scan(t.Context()); err != nil {
		t.Fatalf("read tickets: %v", err)
	}
	return out
}

// readTicket reads the row with id, failing when there is none.
func readTicket(t *testing.T, d *db.DB, id string) Ticket {
	t.Helper()
	tk := Ticket{ID: id}
	if err := d.Bun().NewSelect().Model(&tk).WherePK().Scan(t.Context()); err != nil {
		t.Fatalf("read ticket %s: %v", id, err)
	}
	return tk
}

// seedTicket inserts tk as it is, filling what a test does not care about: a
// random ID with the hash of it as the token hash, cluster lab, host cp-01,
// pair-1, ver-1, minted at at and expiring TicketTTL later.
func seedTicket(t *testing.T, d *db.DB, at time.Time, tk Ticket) Ticket {
	t.Helper()
	if tk.ID == "" {
		tk.ID = rand.Text()
	}
	if tk.TokenSHA256 == "" {
		tk.TokenSHA256 = hexSHA256(tk.ID)
	}
	if tk.ClusterID == "" {
		tk.ClusterID, tk.HostName = labCluster, labHost
	}
	if tk.PairID == "" {
		tk.PairID, tk.SecretVersionID = "pair-1", "ver-1"
	}
	if tk.MintedAt.IsZero() {
		tk.MintedAt = at
	}
	if tk.ExpiresAt.IsZero() {
		tk.ExpiresAt = tk.MintedAt.Add(TicketTTL)
	}
	if _, err := d.Bun().NewInsert().Model(&tk).Exec(t.Context()); err != nil {
		t.Fatalf("seed ticket %+v: %v", tk, err)
	}
	return tk
}

// revokedBy runs fn, one of the Revoke...Tx functions, in a transaction of its own.
func revokedBy(t *testing.T, d *db.DB, fn func(ctx context.Context, q bun.IDB) (int, error)) int {
	t.Helper()
	var n int
	err := inTx(t.Context(), d, func(ctx context.Context, tx bun.Tx) error {
		var err error
		n, err = fn(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	return n
}

func revokeMachine(t *testing.T, d *db.DB, machineID string, at time.Time) int {
	t.Helper()
	return revokedBy(t, d, func(ctx context.Context, q bun.IDB) (int, error) {
		return RevokeMachineTicketsTx(ctx, q, machineID, at)
	})
}

func mustRecord(t *testing.T, r *Repository, ticketID string, at time.Time) RecordConfigResult {
	t.Helper()
	res, err := r.RecordConfigServed(t.Context(), RecordConfigRequest{TicketID: ticketID, At: at})
	if err != nil {
		t.Fatalf("RecordConfigServed(%s): %v", ticketID, err)
	}
	return res
}

// wantLive fails unless the ticket's row is live.
func wantLive(t *testing.T, d *db.DB, id string) {
	t.Helper()
	if !readTicket(t, d, id).Live() {
		t.Fatalf("ticket %s is not live, want live", id)
	}
}

// wantRevokedAt fails unless the ticket's row was revoked at at.
func wantRevokedAt(t *testing.T, d *db.DB, id string, at time.Time) {
	t.Helper()
	if got := readTicket(t, d, id); !got.RevokedAt.Equal(at) {
		t.Fatalf("ticket %s RevokedAt = %v, want %v", id, got.RevokedAt, at)
	}
}

// runTicketSuite is the tickets suite both databases run: TestTickets_SQLite
// untagged, TestTickets_Postgres under the integration tag. pair opens two
// handles on one database.
func runTicketSuite(t *testing.T, open func(testing.TB) *db.DB, pair func(testing.TB) (*db.DB, *db.DB)) {
	t.Helper()
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	// assignedWithTicket is the state the RecordConfigServed subtests start
	// from: an assigned machine in cp-01 with a live ticket.
	assignedWithTicket := func(t *testing.T) (*db.DB, *Repository, Machine, Ticket) {
		t.Helper()
		d := open(t)
		r := NewRepository(d)
		m := assignedMachine(t, r, 1, labHost, t0)
		_, tk := mustMint(t, r, mintReq(m.ID, labHost, t0.Add(time.Minute)))
		return d, r, m, tk
	}

	t.Run("MintTicket", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := assignedMachine(t, r, 1, labHost, t0)
		at := t0.Add(time.Minute)
		token, tk := mustMint(t, r, mintReq(m.ID, labHost, at))
		if len(token) != 43 {
			t.Fatalf("token length = %d, want 43", len(token))
		}
		if raw, err := base64.RawURLEncoding.DecodeString(token); err != nil || len(raw) != 32 {
			t.Fatalf("token decodes to %d bytes, error %v; want 32 bytes of unpadded base64url", len(raw), err)
		}
		if tk.TokenSHA256 != hexSHA256(token) {
			t.Fatalf("TokenSHA256 = %s, want hex(sha256(token)) %s", tk.TokenSHA256, hexSHA256(token))
		}
		if tk.MachineID != m.ID || tk.ClusterID != labCluster || tk.HostName != labHost || tk.PairID != "pair-1" || tk.SecretVersionID != "ver-1" {
			t.Fatalf("ticket = %+v, want machine %s bound to lab/%s, pair-1, ver-1", tk, m.ID, labHost)
		}
		if !tk.MintedAt.Equal(at) || !tk.ExpiresAt.Equal(at.Add(TicketTTL)) || !tk.Live() {
			t.Fatalf("ticket times = minted %v expires %v live %t, want %v and %v live", tk.MintedAt, tk.ExpiresAt, tk.Live(), at, at.Add(TicketTTL))
		}
		rows := readTickets(t, d)
		if len(rows) != 1 || rows[0].ID != tk.ID || rows[0].TokenSHA256 != tk.TokenSHA256 || !rows[0].ExpiresAt.Equal(tk.ExpiresAt) {
			t.Fatalf("stored tickets = %+v, want the returned one", rows)
		}
		n := scalar[int](t, d, `SELECT count(*) FROM tickets WHERE ? IN (id, machine_id, token_sha256, cluster_id, host_name, pair_id, secret_version_id)`, token)
		if n != 0 {
			t.Fatalf("%d tickets have a column equal to the token, want 0", n)
		}
	})

	t.Run("MintUnknownMachine", func(t *testing.T) {
		d := open(t)
		_, _, err := NewRepository(d).MintTicket(t.Context(), mintReq("no-such-machine", labHost, t0))
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("MintTicket for an unknown machine error = %v, want ErrNotFound", err)
		}
		if n := len(readTickets(t, d)); n != 0 {
			t.Fatalf("tickets = %d, want 0", n)
		}
	})

	t.Run("MintRefusesChangedMachine", func(t *testing.T) {
		cases := []struct {
			name   string
			req    func(*MintRequest)
			update string
		}{
			{name: "other host", req: func(r *MintRequest) { r.HostName = "wrk-01" }},
			{name: "other cluster", req: func(r *MintRequest) { r.ClusterID = "other" }},
			{name: "other schematic", req: func(r *MintRequest) { r.TalosSchematic = strings.Repeat("ab", 32) }},
			{name: "other Talos version", req: func(r *MintRequest) { r.TalosVersion = "v1.13.0" }},
			{name: "other arch", req: func(r *MintRequest) { r.Arch = ArchARM64 }},
			{name: "no longer assigned", update: "UPDATE machines SET state = 'installing' WHERE id = ?"},
			{name: "marked junk", update: "UPDATE machines SET junk = TRUE WHERE id = ?"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				d := open(t)
				r := NewRepository(d)
				m := assignedMachine(t, r, 1, labHost, t0)
				_, live := mustMint(t, r, mintReq(m.ID, labHost, t0))
				req := mintReq(m.ID, labHost, t0.Add(time.Minute))
				if tc.req != nil {
					tc.req(&req)
				}
				if tc.update != "" {
					if err := execRaw(t, d, tc.update, m.ID); err != nil {
						t.Fatalf("%s: %v", tc.update, err)
					}
				}

				_, _, err := r.MintTicket(t.Context(), req)

				if !errors.Is(err, ErrMachineChanged) {
					t.Fatalf("MintTicket error = %v, want ErrMachineChanged", err)
				}
				if n := len(readTickets(t, d)); n != 1 {
					t.Fatalf("tickets = %d, want only the earlier one", n)
				}
				wantLive(t, d, live.ID)
			})
		}
	})

	t.Run("MintRevokesPrevious", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := assignedMachine(t, r, 1, labHost, t0)
		_, first := mustMint(t, r, mintReq(m.ID, labHost, t0.Add(time.Minute)))
		_, second := mustMint(t, r, mintReq(m.ID, labHost, t0.Add(2*time.Minute)))
		wantRevokedAt(t, d, first.ID, t0.Add(2*time.Minute))
		wantLive(t, d, second.ID)
		if n := scalar[int](t, d, "SELECT count(*) FROM tickets WHERE machine_id = ? AND consumed_at IS NULL AND revoked_at IS NULL", m.ID); n != 1 {
			t.Fatalf("live tickets = %d, want 1", n)
		}
	})

	t.Run("ExpiredLiveTicketRevokedByMint", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := assignedMachine(t, r, 1, labHost, t0)
		_, first := mustMint(t, r, mintReq(m.ID, labHost, t0))
		later := t0.Add(TicketTTL + time.Hour)
		if first.Valid(later) {
			t.Fatalf("the first ticket is still valid at %v; the test needs it expired", later)
		}
		_, second := mustMint(t, r, mintReq(m.ID, labHost, later))
		wantRevokedAt(t, d, first.ID, later)
		wantLive(t, d, second.ID)
	})

	t.Run("ConcurrentMint", func(t *testing.T) {
		serveDB, cliDB := pair(t)
		defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
		repos := [2]*Repository{NewRepository(serveDB), NewRepository(cliDB)}
		m := assignedMachine(t, repos[0], 1, labHost, t0)
		for round := range 5 {
			at := t0.Add(time.Duration(round+1) * time.Minute)
			var (
				tokens [2]string
				errs   [2]error
			)
			together(func(i int) {
				tokens[i], _, errs[i] = repos[i].MintTicket(t.Context(), mintReq(m.ID, labHost, at))
			})
			if errs[0] != nil || errs[1] != nil {
				t.Fatalf("round %d: errors = %v, %v; want both mints to succeed", round, errs[0], errs[1])
			}
			if tokens[0] == tokens[1] {
				t.Fatalf("round %d: both mints returned the token %q", round, tokens[0])
			}
			if n := scalar[int](t, serveDB, "SELECT count(*) FROM tickets WHERE machine_id = ? AND consumed_at IS NULL AND revoked_at IS NULL", m.ID); n != 1 {
				t.Fatalf("round %d: live tickets = %d, want exactly 1", round, n)
			}
		}
		if n := len(readTickets(t, serveDB)); n != 10 {
			t.Fatalf("tickets = %d, want 10 (two per round, all but the last revoked)", n)
		}
	})

	t.Run("TransitionRevokes", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := assignedMachine(t, r, 1, labHost, t0)
		_, tk := mustMint(t, r, mintReq(m.ID, labHost, t0.Add(time.Minute)))
		mustTransition(t, r, m.ID, TriggerConfigServed, t0.Add(2*time.Minute))
		wantLive(t, d, tk.ID)
		mustTransition(t, r, m.ID, TriggerCordon, t0.Add(3*time.Minute))
		wantRevokedAt(t, d, tk.ID, t0.Add(3*time.Minute))
	})

	t.Run("RevokeMachineTicketsTx", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		a := assignedMachine(t, r, 1, labHost, t0)
		b := assignedMachine(t, r, 2, "wrk-01", t0)
		_, ta := mustMint(t, r, mintReq(a.ID, labHost, t0))
		_, tb := mustMint(t, r, mintReq(b.ID, "wrk-01", t0))
		at := t0.Add(time.Minute)
		if n := revokeMachine(t, d, a.ID, at); n != 1 {
			t.Fatalf("revoked = %d, want 1", n)
		}
		wantRevokedAt(t, d, ta.ID, at)
		wantLive(t, d, tb.ID)
		if n := revokeMachine(t, d, a.ID, at.Add(time.Minute)); n != 0 {
			t.Fatalf("second revoke = %d, want 0", n)
		}
		wantRevokedAt(t, d, ta.ID, at)
	})

	t.Run("RevokeClusterTicketsTx", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		a := assignedMachine(t, r, 1, labHost, t0)
		b := assignedMachine(t, r, 2, "wrk-01", t0)
		c := bootNextMachine(t, r, 3, t0)
		_, ta := mustMint(t, r, mintReq(a.ID, labHost, t0))
		_, tb := mustMint(t, r, mintReq(b.ID, "wrk-01", t0))
		otherReq := mintReq(c.ID, labHost, t0)
		otherReq.ClusterID = "other"
		tc := insertTicket(t, d, otherReq)
		at := t0.Add(time.Minute)
		n := revokedBy(t, d, func(ctx context.Context, q bun.IDB) (int, error) {
			return RevokeClusterTicketsTx(ctx, q, labCluster, at)
		})
		if n != 2 {
			t.Fatalf("revoked = %d, want 2", n)
		}
		wantRevokedAt(t, d, ta.ID, at)
		wantRevokedAt(t, d, tb.ID, at)
		wantLive(t, d, tc.ID)
	})

	t.Run("JunkMarkRevokes", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		a := firstContact(t, r, originalIdentity(), t0)
		mustApprove(t, r, a.ID, labHost, t0)
		_, first := mustMint(t, r, mintReq(a.ID, labHost, t0.Add(time.Minute)))
		conflictBoot(t, r, a.ID, true, t0.Add(10*time.Second))
		wantRevokedAt(t, d, first.ID, t0.Add(10*time.Second))

		// A clone of the junked machine is stored junk with the same UUID; the
		// next conflict reaches every machine holding it.
		b := firstContact(t, r, Identity{UUID: fixtureUUID, MAC: macN(2), Serial: "S1", Product: "P1"}, t0.Add(20*time.Second))
		if !b.Junk {
			t.Fatalf("clone %s is not junk, want junk", b.ID)
		}
		ofA := insertTicket(t, d, mintReq(a.ID, labHost, t0.Add(30*time.Second)))
		ofB := insertTicket(t, d, mintReq(b.ID, labHost, t0.Add(30*time.Second)))
		conflictBoot(t, r, a.ID, true, t0.Add(40*time.Second))
		wantRevokedAt(t, d, ofA.ID, t0.Add(40*time.Second))
		wantRevokedAt(t, d, ofB.ID, t0.Add(40*time.Second))
	})

	t.Run("TicketByToken", func(t *testing.T) {
		r := NewRepository(open(t))
		m := assignedMachine(t, r, 1, labHost, t0)
		token, tk := mustMint(t, r, mintReq(m.ID, labHost, t0))
		got, ok, err := r.TicketByToken(t.Context(), token)
		if err != nil || !ok || got.ID != tk.ID || got.MachineID != m.ID || !got.ExpiresAt.Equal(tk.ExpiresAt) {
			t.Fatalf("TicketByToken(token) = %+v, %t, %v; want ticket %s of %s", got, ok, err, tk.ID, m.ID)
		}
		// The stored hash is not a token.
		for _, other := range []string{"", token + "x", tk.TokenSHA256} {
			if got, ok, err := r.TicketByToken(t.Context(), other); err != nil || ok {
				t.Fatalf("TicketByToken(%q) = %+v, %t, %v; want not found", other, got, ok, err)
			}
		}
	})

	t.Run("MarkTicketRequested", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := assignedMachine(t, r, 1, labHost, t0)
		_, tk := mustMint(t, r, mintReq(m.ID, labHost, t0))
		if got := readTicket(t, d, tk.ID); !got.RequestedAt.IsZero() {
			t.Fatalf("RequestedAt = %v, want zero before any request", got.RequestedAt)
		}
		if err := r.MarkTicketRequested(t.Context(), tk.ID, t0); err != nil {
			t.Fatalf("MarkTicketRequested: %v", err)
		}
		if err := r.MarkTicketRequested(t.Context(), tk.ID, t0.Add(time.Minute)); err != nil {
			t.Fatalf("MarkTicketRequested again: %v", err)
		}
		if got := readTicket(t, d, tk.ID); !got.RequestedAt.Equal(t0) {
			t.Fatalf("RequestedAt = %v, want the first request %v", got.RequestedAt, t0)
		}
	})

	t.Run("RecordConfigServed", func(t *testing.T) {
		d, r, m, tk := assignedWithTicket(t)
		at := t0.Add(2 * time.Minute)
		res := mustRecord(t, r, tk.ID, at)
		if !res.Recorded || res.AlreadyConsumed || res.Machine.ID != m.ID || res.Machine.State != StateInstalling {
			t.Fatalf("result = %+v, want recorded with machine %s installing", res, m.ID)
		}
		got := mustGet(t, r, m.ID)
		if got.State != StateInstalling {
			t.Fatalf("State = %s, want installing", got.State)
		}
		wantBound(t, got, labHost)
		if row := readTicket(t, d, tk.ID); !row.ConsumedAt.Equal(at) || !row.RevokedAt.IsZero() {
			t.Fatalf("ticket consumed %v revoked %v, want consumed at %v and not revoked", row.ConsumedAt, row.RevokedAt, at)
		}
		wantOneAudit(t, d, "config.served", ActorSystem, m.ID, `{"ticket_id":"`+tk.ID+`","pair_id":"pair-1"}`)
		wantOneAudit(t, d, "transition.config_served", ActorSystem, m.ID, `{"from":"assigned","to":"installing"}`)
	})

	t.Run("RecordAfterRevocation", func(t *testing.T) {
		d, r, m, tk := assignedWithTicket(t)
		revokeMachine(t, d, m.ID, t0.Add(2*time.Minute))
		res := mustRecord(t, r, tk.ID, t0.Add(3*time.Minute))
		if !res.Recorded || res.Machine.State != StateInstalling {
			t.Fatalf("result = %+v, want recorded with the machine installing (the body was delivered)", res)
		}
		if got := mustGet(t, r, m.ID); got.State != StateInstalling {
			t.Fatalf("State = %s, want installing", got.State)
		}
		if row := readTicket(t, d, tk.ID); row.ConsumedAt.IsZero() {
			t.Fatal("the revoked ticket was not consumed")
		}
	})

	t.Run("RecordAlreadyConsumed", func(t *testing.T) {
		d, r, m, tk := assignedWithTicket(t)
		mustRecord(t, r, tk.ID, t0.Add(2*time.Minute))
		res := mustRecord(t, r, tk.ID, t0.Add(3*time.Minute))
		if res.Recorded || !res.AlreadyConsumed {
			t.Fatalf("second result = %+v, want already consumed and not recorded", res)
		}
		if row := readTicket(t, d, tk.ID); !row.ConsumedAt.Equal(t0.Add(2 * time.Minute)) {
			t.Fatalf("ConsumedAt = %v, want the first recording's time", row.ConsumedAt)
		}
		if n := len(auditsOf(t, d, "config.served")); n != 1 {
			t.Fatalf("config.served audits = %d, want 1", n)
		}
		if got := mustGet(t, r, m.ID); got.State != StateInstalling {
			t.Fatalf("State = %s, want installing", got.State)
		}
	})

	t.Run("RecordMachineUnassigned", func(t *testing.T) {
		d, r, m, tk := assignedWithTicket(t)
		mustTransition(t, r, m.ID, TriggerUnassign, t0.Add(2*time.Minute))
		res := mustRecord(t, r, tk.ID, t0.Add(3*time.Minute))
		if res.Recorded || res.AlreadyConsumed {
			t.Fatalf("result = %+v, want not recorded", res)
		}
		if got := mustGet(t, r, m.ID); got.State != StateDiscovered {
			t.Fatalf("State = %s, want discovered", got.State)
		}
		if row := readTicket(t, d, tk.ID); !row.ConsumedAt.IsZero() {
			t.Fatalf("ConsumedAt = %v, want the ticket left unconsumed", row.ConsumedAt)
		}
		if n := len(auditsOf(t, d, "config.served")); n != 0 {
			t.Fatalf("config.served audits = %d, want 0", n)
		}
	})

	t.Run("RecordHostChanged", func(t *testing.T) {
		d, r, m, tk := assignedWithTicket(t)
		if err := execRaw(t, d, "UPDATE machines SET host_name = ? WHERE id = ?", "wrk-01", m.ID); err != nil {
			t.Fatalf("move the machine to another host: %v", err)
		}
		res := mustRecord(t, r, tk.ID, t0.Add(2*time.Minute))
		if res.Recorded || res.AlreadyConsumed {
			t.Fatalf("result = %+v, want not recorded for a ticket of another host", res)
		}
		if got := mustGet(t, r, m.ID); got.State != StateAssigned {
			t.Fatalf("State = %s, want assigned", got.State)
		}
		if row := readTicket(t, d, tk.ID); !row.ConsumedAt.IsZero() {
			t.Fatalf("ConsumedAt = %v, want the ticket left unconsumed", row.ConsumedAt)
		}
	})

	t.Run("RecordUnknownTicket", func(t *testing.T) {
		d := open(t)
		res := mustRecord(t, NewRepository(d), "no-such-ticket", t0)
		if res.Recorded || res.AlreadyConsumed {
			t.Fatalf("result = %+v, want not recorded", res)
		}
	})

	t.Run("RecordRevokesOthers", func(t *testing.T) {
		d, r, m, tk := assignedWithTicket(t)
		// The index allows one live ticket per machine: revoke the first, then
		// a second for another pair goes in directly.
		revokeMachine(t, d, m.ID, t0.Add(2*time.Minute))
		other := seedTicket(t, d, t0.Add(3*time.Minute), Ticket{MachineID: m.ID, PairID: "pair-2", SecretVersionID: "ver-2"})
		at := t0.Add(4 * time.Minute)
		if res := mustRecord(t, r, tk.ID, at); !res.Recorded {
			t.Fatalf("result = %+v, want recorded", res)
		}
		wantRevokedAt(t, d, other.ID, at)
	})

	t.Run("ConcurrentRecord", func(t *testing.T) {
		serveDB, cliDB := pair(t)
		defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
		repos := [2]*Repository{NewRepository(serveDB), NewRepository(cliDB)}
		m := assignedMachine(t, repos[0], 1, labHost, t0)
		_, tk := mustMint(t, repos[0], mintReq(m.ID, labHost, t0.Add(time.Minute)))
		var (
			res  [2]RecordConfigResult
			errs [2]error
		)
		together(func(i int) {
			res[i], errs[i] = repos[i].RecordConfigServed(t.Context(), RecordConfigRequest{TicketID: tk.ID, At: t0.Add(2 * time.Minute)})
		})
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("errors = %v, %v; want both calls to return normally", errs[0], errs[1])
		}
		if res[0].Recorded == res[1].Recorded || res[0].AlreadyConsumed == res[1].AlreadyConsumed || res[0].Recorded == res[0].AlreadyConsumed {
			t.Fatalf("results = %+v and %+v; want exactly one recorded and the other already consumed", res[0], res[1])
		}
		if n := len(auditsOf(t, serveDB, "config.served")); n != 1 {
			t.Fatalf("config.served audits = %d, want 1", n)
		}
		if got := mustGet(t, repos[0], m.ID); got.State != StateInstalling {
			t.Fatalf("State = %s, want installing", got.State)
		}
	})

	t.Run("PruneTickets", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		a := bootNextMachine(t, r, 1, t0)
		b := bootNextMachine(t, r, 2, t0)
		c := bootNextMachine(t, r, 3, t0)
		cutoff := t0.Add(7*24*time.Hour + time.Second)
		far := t0.Add(30 * 24 * time.Hour)
		consumed := seedTicket(t, d, t0, Ticket{MachineID: a.ID, ExpiresAt: far, ConsumedAt: t0})
		revoked := seedTicket(t, d, t0, Ticket{MachineID: a.ID, ExpiresAt: far, RevokedAt: t0})
		recent := seedTicket(t, d, t0, Ticket{MachineID: a.ID, ExpiresAt: far, ConsumedAt: cutoff.Add(time.Second)})
		expired := seedTicket(t, d, t0, Ticket{MachineID: b.ID, ExpiresAt: t0})
		live := seedTicket(t, d, t0, Ticket{MachineID: c.ID, ExpiresAt: far})

		n, err := r.PruneTickets(t.Context(), cutoff)
		if err != nil || n != 3 {
			t.Fatalf("PruneTickets = %d, %v; want 3 deleted", n, err)
		}
		gone := map[string]bool{consumed.ID: true, revoked.ID: true, expired.ID: true}
		kept := map[string]bool{recent.ID: true, live.ID: true}
		rows := readTickets(t, d)
		for i := range rows {
			if gone[rows[i].ID] {
				t.Errorf("ticket %s survived the prune, want it deleted", rows[i].ID)
			}
			delete(kept, rows[i].ID)
		}
		if len(kept) != 0 {
			t.Fatalf("pruned tickets that are not past the cutoff: %v", kept)
		}
	})

	t.Run("PendingTickets", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		ms := make([]Machine, 5)
		for i := range ms {
			ms[i] = bootNextMachine(t, r, i+1, t0)
		}
		cutoff := t0.Add(30 * time.Minute)
		pending := seedTicket(t, d, t0, Ticket{MachineID: ms[0].ID})
		seedTicket(t, d, t0, Ticket{MachineID: ms[1].ID, ConsumedAt: t0.Add(time.Minute)})
		seedTicket(t, d, t0, Ticket{MachineID: ms[2].ID, RevokedAt: t0.Add(time.Minute)})
		seedTicket(t, d, t0, Ticket{MachineID: ms[3].ID, MintedAt: cutoff.Add(time.Minute)})
		// Expired before the cutoff: it can no longer be fetched at all.
		seedTicket(t, d, t0, Ticket{MachineID: ms[4].ID, MintedAt: t0.Add(-4 * time.Hour), ExpiresAt: t0.Add(-time.Hour)})

		got, err := r.PendingTickets(t.Context(), cutoff)
		if err != nil {
			t.Fatalf("PendingTickets: %v", err)
		}
		if len(got) != 1 || got[0].ID != pending.ID {
			t.Fatalf("PendingTickets = %+v, want only %s (live, unexpired, unconsumed, minted before the cutoff)", got, pending.ID)
		}
	})

	t.Run("LiveTicket", func(t *testing.T) {
		d := open(t)
		r := NewRepository(d)
		m := assignedMachine(t, r, 1, labHost, t0)
		if got, ok, err := r.LiveTicket(t.Context(), m.ID); err != nil || ok {
			t.Fatalf("LiveTicket before any mint = %+v, %t, %v; want none", got, ok, err)
		}
		_, tk := mustMint(t, r, mintReq(m.ID, labHost, t0))
		got, ok, err := r.LiveTicket(t.Context(), m.ID)
		if err != nil || !ok || got.ID != tk.ID {
			t.Fatalf("LiveTicket = %+v, %t, %v; want %s", got, ok, err, tk.ID)
		}
		revokeMachine(t, d, m.ID, t0.Add(time.Minute))
		if got, ok, err := r.LiveTicket(t.Context(), m.ID); err != nil || ok {
			t.Fatalf("LiveTicket after a revoke = %+v, %t, %v; want none", got, ok, err)
		}
	})
}
