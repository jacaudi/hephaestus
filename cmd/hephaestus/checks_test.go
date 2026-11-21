package main

import (
	"strings"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/admin"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// checkT0 is the time the check fixtures are made at.
var checkT0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

const (
	// runbookInstalling and runbookUnconfirmed are the two branches of the "recorded but not received" runbook.
	runbookInstalling  = "installing: machine abort, then reboot"
	runbookUnconfirmed = "install-unconfirmed: topf reset, machine reinstall --topf-reset-full-done, then reboot"
	// plainSchematic is the schematic id of a plain Talos image.
	plainSchematic = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"
)

func TestConfigPendingCheck_Runbook(t *testing.T) {
	tests := []struct {
		name      string
		requested bool
		at        time.Duration
		want      admin.Status
	}{
		{name: "unrequested for 31 minutes", at: 31 * time.Minute, want: admin.StatusWarn},
		{name: "unrequested for 29 minutes", at: 29 * time.Minute, want: admin.StatusPass},
		{name: "requested", requested: true, at: 31 * time.Minute, want: admin.StatusPass},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := dbtest.SQLite(t)
			repo := fleet.NewRepository(d)
			m, err := repo.RecordBoot(t.Context(), fleet.BootRecord{
				Identity: fleet.Identity{UUID: "4c4c4544-0042-3510-8052-b4c04f4e4d32", MAC: "aa:bb:cc:00:00:01", Serial: "S", Product: "P"},
				Arch:     fleet.ArchAMD64, Trigger: fleet.TriggerFirstContact,
				Event: fleet.BootEvent{Row: 7, Action: "hold", Reason: "awaiting approval"}, At: checkT0,
			})
			if err != nil {
				t.Fatalf("RecordBoot: %v", err)
			}
			ticket := fleet.Ticket{
				ID: "ticket-1", MachineID: m.ID, TokenSHA256: "hash-1", ClusterID: "cluster-1", HostName: "cp-01",
				PairID: "pair-1", SecretVersionID: "version-1", MintedAt: checkT0, ExpiresAt: checkT0.Add(fleet.TicketTTL),
			}
			if tt.requested {
				ticket.RequestedAt = checkT0.Add(time.Minute)
			}
			if _, err := d.Bun().NewInsert().Model(&ticket).Exec(t.Context()); err != nil {
				t.Fatalf("insert ticket: %v", err)
			}
			check := configPendingCheck(repo, func() time.Time { return checkT0.Add(tt.at) })

			res := check.Run(t.Context())

			if check.Name != "config:pending" || check.Critical {
				t.Errorf("check = %q critical %t, want config:pending, informational", check.Name, check.Critical)
			}
			if res.Status != tt.want {
				t.Fatalf("status = %s (%q), want %s", res.Status, res.Output, tt.want)
			}
			if tt.want != admin.StatusWarn {
				return
			}
			for _, want := range []string{m.ID, runbookInstalling, runbookUnconfirmed} {
				if !strings.Contains(res.Output, want) {
					t.Errorf("output %q lacks %q", res.Output, want)
				}
			}
		})
	}
}

func TestTalosCacheCheck(t *testing.T) {
	tests := []struct {
		name   string
		broken bool
		want   admin.Status
	}{
		{name: "a broken row", broken: true, want: admin.StatusWarn},
		{name: "no broken row", want: admin.StatusPass},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := dbtest.SQLite(t)
			repo := fleet.NewRepository(d)
			key := fleet.TalosKey{Schematic: plainSchematic, Version: "v1.12.11", Arch: fleet.ArchARM64}
			if _, err := fleet.WantTalosTx(t.Context(), d.Bun(), fleet.TalosWant{Key: key}, checkT0); err != nil {
				t.Fatalf("WantTalosTx: %v", err)
			}
			if tt.broken {
				row, ok, err := repo.ClaimTalosRow(t.Context(), key, checkT0)
				if err != nil || !ok {
					t.Fatalf("ClaimTalosRow = %v, %v; want the row claimed", ok, err)
				}
				f := fleet.TalosFailure{Claim: row.Claim(), Cause: "sha256 mismatch", Integrity: true, At: checkT0}
				if row, err := repo.RecordTalosFailure(t.Context(), f); err != nil || row.State != fleet.CacheBroken {
					t.Fatalf("RecordTalosFailure = %+v, %v; want a broken row", row, err)
				}
			}
			check := talosCacheCheck(repo)

			res := check.Run(t.Context())

			if check.Name != "talos:cache" || check.Critical {
				t.Errorf("check = %q critical %t, want talos:cache, informational", check.Name, check.Critical)
			}
			if res.Status != tt.want {
				t.Fatalf("status = %s (%q), want %s", res.Status, res.Output, tt.want)
			}
			if tt.want != admin.StatusWarn {
				return
			}
			for _, want := range []string{key.Schematic, key.Version, string(key.Arch)} {
				if !strings.Contains(res.Output, want) {
					t.Errorf("output %q lacks %q", res.Output, want)
				}
			}
		})
	}
}
