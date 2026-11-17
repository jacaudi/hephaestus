package db_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"go.jacaudi.dev/hephaestus/internal/admin"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

const fixtureUUID = "4c4c4544-0042-3510-8052-b4c04f4e4d32"

func TestMigrations_SameVersions(t *testing.T) {
	versions := func(dir string) []string {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		var out []string
		for _, e := range entries {
			v, _, _ := strings.Cut(e.Name(), "_")
			out = append(out, v)
		}
		return out
	}
	got, want := versions("migrations/sqlite"), versions("migrations/postgres")
	if len(got) == 0 {
		t.Fatal("no sqlite migrations")
	}
	if !slices.Equal(got, want) {
		t.Fatalf("sqlite versions %v != postgres versions %v", got, want)
	}
}

func TestMigrate_SQLite(t *testing.T) { runMigrateSuite(t, dbtest.SQLite) }

// runMigrateSuite is the schema suite both databases run: TestMigrate_SQLite
// untagged, TestMigrate_Postgres under the integration tag.
func runMigrateSuite(t *testing.T, open func(testing.TB) *db.DB) {
	t.Helper()
	d := open(t)

	t.Run("Tables", func(t *testing.T) {
		query := "SELECT name FROM sqlite_master WHERE type = 'table'"
		if d.Dialect() == db.DialectPostgres {
			query = "SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema()"
		}
		var got []string
		if err := d.Bun().NewRaw(query).Scan(t.Context(), &got); err != nil {
			t.Fatalf("list tables: %v", err)
		}
		for _, want := range []string{"machines", "machine_macs", "inventory_reports", "boot_events", "audit_events", "settings", "utility_cache"} {
			if !slices.Contains(got, want) {
				t.Errorf("table %q missing; have %v", want, got)
			}
		}
		// TLS comes from files or a proxy; nothing in the database holds a certificate or an ACME account.
		for _, gone := range []string{"acme_accounts", "tls_certificates"} {
			if slices.Contains(got, gone) {
				t.Errorf("table %q exists, want it gone with the in-process ACME client; have %v", gone, got)
			}
		}
	})

	t.Run("SeededUnknownAction", func(t *testing.T) {
		var got string
		if err := d.Bun().NewRaw("SELECT value FROM settings WHERE key = 'unknown_action'").Scan(t.Context(), &got); err != nil {
			t.Fatalf("read unknown_action: %v", err)
		}
		if got != "local" {
			t.Fatalf("unknown_action = %q, want %q", got, "local")
		}
	})

	t.Run("Idempotent", func(t *testing.T) {
		if err := d.Migrate(t.Context()); err != nil {
			t.Fatalf("second Migrate: %v", err)
		}
		pending, err := d.Pending(t.Context())
		if err != nil || pending {
			t.Fatalf("Pending = %v, %v; want false, nil", pending, err)
		}
	})

	t.Run("UniqueValidUUID", func(t *testing.T) {
		if err := insertMachine(t, d, "m1", false); err != nil {
			t.Fatalf("first valid machine: %v", err)
		}
		if err := insertMachine(t, d, "m2", false); err == nil {
			t.Fatal("second valid machine with the same UUID succeeded, want a unique violation")
		}
		if err := insertMachine(t, d, "m3", true); err != nil {
			t.Fatalf("junk machine with the same UUID: %v", err)
		}
	})

	t.Run("BootNextPair", func(t *testing.T) {
		const insert = "INSERT INTO machines (id, smbios_uuid, junk, serial, product, arch, state, created_at, last_seen_at, boot_next, boot_next_set_at) VALUES (?, ?, TRUE, 's', 'p', 'amd64', 'discovered', '2026-11-14T12:00:00Z', '2026-11-14T12:00:00Z', ?, ?)"
		for _, tc := range []struct {
			name    string
			id      string
			next    any
			setAt   any
			wantErr bool
		}{
			{"flag without time", "bn1", "x", nil, true},
			{"time without flag", "bn2", nil, "2026-11-14T12:00:00Z", true},
			{"both set", "bn3", "x", "2026-11-14T12:00:00Z", false},
			{"neither set", "bn4", nil, nil, false},
		} {
			_, err := d.Bun().ExecContext(t.Context(), insert, tc.id, fixtureUUID, tc.next, tc.setAt)
			if (err != nil) != tc.wantErr {
				t.Errorf("%s: insert error = %v, want error: %v", tc.name, err, tc.wantErr)
			}
		}
	})

	t.Run("ChecksPass", func(t *testing.T) {
		for _, c := range d.Checks() {
			if got := c.Run(t.Context()); got.Status != admin.StatusPass {
				t.Errorf("%s = %+v, want pass", c.Name, got)
			}
		}
	})
}

func insertMachine(t *testing.T, d *db.DB, id string, junk bool) error {
	t.Helper()
	_, err := d.Bun().ExecContext(t.Context(),
		"INSERT INTO machines (id, smbios_uuid, junk, serial, product, arch, state, created_at, last_seen_at) VALUES (?, ?, ?, 's', 'p', 'amd64', 'discovered', '2026-11-14T12:00:00Z', '2026-11-14T12:00:00Z')",
		id, fixtureUUID, junk)
	return err
}
