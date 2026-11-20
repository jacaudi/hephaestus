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
		got := tableNames(t, d)
		for _, want := range []string{
			"machines", "machine_macs", "inventory_reports", "boot_events", "audit_events", "settings", "utility_cache",
			"clusters", "secret_versions", "cluster_pairs", "host_renders", "tickets", "talos_cache",
		} {
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

	t.Run("BindingUnique", func(t *testing.T) {
		for _, id := range []string{"bu1", "bu2", "bu3", "bu4", "bu5", "bu6"} {
			if err := insertMachine(t, d, id, true); err != nil {
				t.Fatalf("insert %s: %v", id, err)
			}
		}
		const bind = "UPDATE machines SET cluster_id = ?, host_name = ? WHERE id = ?"
		for _, tc := range []struct {
			name    string
			id      string
			cluster any
			host    any
			wantErr bool
		}{
			{"first binding", "bu1", "lab", "cp-01", false},
			{"same host again", "bu2", "lab", "cp-01", true},
			{"other host of the cluster", "bu2", "lab", "wrk-01", false},
			{"same host name, other cluster", "bu3", "other", "cp-01", false},
			{"unbound once", "bu4", nil, nil, false},
			{"unbound twice", "bu5", nil, nil, false},
			{"cluster without host once", "bu5", "lab", nil, false},
			{"cluster without host twice", "bu6", "lab", nil, false},
		} {
			_, err := d.Bun().ExecContext(t.Context(), bind, tc.cluster, tc.host, tc.id)
			if (err != nil) != tc.wantErr {
				t.Errorf("%s: update error = %v, want error: %v", tc.name, err, tc.wantErr)
			}
		}
	})

	t.Run("OneLiveTicket", func(t *testing.T) {
		for _, id := range []string{"ot1", "ot2"} {
			if err := insertMachine(t, d, id, true); err != nil {
				t.Fatalf("insert %s: %v", id, err)
			}
		}
		const insert = "INSERT INTO tickets (id, machine_id, token_sha256, cluster_id, host_name, pair_id, secret_version_id, minted_at, expires_at, consumed_at, revoked_at) VALUES (?, ?, ?, 'lab', 'cp-01', 'p', 'v', '2026-11-14T12:00:00Z', '2026-11-14T12:15:00Z', ?, ?)"
		const when = "2026-11-14T12:01:00Z"
		for _, tc := range []struct {
			name     string
			id       string
			machine  string
			consumed any
			revoked  any
			wantErr  bool
		}{
			{"revoked ticket", "t1", "ot1", nil, when, false},
			{"consumed ticket", "t2", "ot1", when, nil, false},
			{"first live ticket", "t3", "ot1", nil, nil, false},
			{"second live ticket", "t4", "ot1", nil, nil, true},
			{"live ticket of another machine", "t5", "ot2", nil, nil, false},
		} {
			_, err := d.Bun().ExecContext(t.Context(), insert, tc.id, tc.machine, "sha-"+tc.id, tc.consumed, tc.revoked)
			if (err != nil) != tc.wantErr {
				t.Errorf("%s: insert error = %v, want error: %v", tc.name, err, tc.wantErr)
			}
		}
	})

	t.Run("DriftPair", func(t *testing.T) {
		const stamp = "'2026-11-14T12:00:00Z'"
		sealed := []byte("sealed")
		for _, seed := range []struct {
			stmt string
			args []any
		}{
			{"INSERT INTO clusters (id, name, repo_url, ref, subpath, created_at, updated_at) VALUES ('c1', 'lab', 'https://git.example.test/lab.git', 'main', '.', " + stamp + ", " + stamp + ")", nil},
			{"INSERT INTO secret_versions (id, cluster_id, values_sealed, created_at) VALUES ('sv1', 'c1', ?, " + stamp + ")", []any{sealed}},
			{"INSERT INTO cluster_pairs (id, cluster_id, tree_hash, secret_version_id, talos_version, created_at) VALUES ('cp1', 'c1', 'tree', 'sv1', 'v1.12.11', " + stamp + ")", nil},
		} {
			if _, err := d.Bun().ExecContext(t.Context(), seed.stmt, seed.args...); err != nil {
				t.Fatalf("seed %q: %v", seed.stmt, err)
			}
		}
		const insert = "INSERT INTO host_renders (pair_id, host_name, role, ip, config_hash, display_hash, redacted_sealed, schematic_id, talos_version, drift, drift_config_hash) VALUES ('cp1', ?, 'control-plane', '10.10.20.11', 'c', 'd', ?, 's', 'v1.12.11', ?, ?)"
		for _, tc := range []struct {
			name    string
			host    string
			drift   bool
			hash    any
			wantErr bool
		}{
			{"no drift", "h1", false, nil, false},
			{"drift with its hash", "h2", true, "x", false},
			{"drift without a hash", "h3", true, nil, true},
			{"a hash without drift", "h4", false, "x", true},
		} {
			_, err := d.Bun().ExecContext(t.Context(), insert, tc.host, sealed, tc.drift, tc.hash)
			if (err != nil) != tc.wantErr {
				t.Errorf("%s: insert error = %v, want error: %v", tc.name, err, tc.wantErr)
			}
		}
	})

	t.Run("TupleBothOrNeither", func(t *testing.T) {
		if err := insertMachine(t, d, "tb1", true); err != nil {
			t.Fatalf("insert: %v", err)
		}
		const set = "UPDATE machines SET talos_schematic = ?, talos_version = ? WHERE id = 'tb1'"
		for _, tc := range []struct {
			name      string
			schematic any
			version   any
			wantErr   bool
		}{
			{"schematic without version", "s", nil, true},
			{"version without schematic", nil, "v1.12.11", true},
			{"both set", "s", "v1.12.11", false},
			{"neither set", nil, nil, false},
		} {
			_, err := d.Bun().ExecContext(t.Context(), set, tc.schematic, tc.version)
			if (err != nil) != tc.wantErr {
				t.Errorf("%s: update error = %v, want error: %v", tc.name, err, tc.wantErr)
			}
		}
	})

	t.Run("IdentityObservedDefaultsTrue", func(t *testing.T) {
		if err := insertMachine(t, d, "io1", true); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var got bool
		if err := d.Bun().NewRaw("SELECT identity_observed FROM machines WHERE id = 'io1'").Scan(t.Context(), &got); err != nil {
			t.Fatalf("read identity_observed: %v", err)
		}
		if !got {
			t.Fatal("identity_observed = false, want true for a machine inserted without it")
		}
	})

	t.Run("ChecksPass", func(t *testing.T) {
		for _, c := range d.Checks() {
			if got := c.Run(t.Context()); got.Status != admin.StatusPass {
				t.Errorf("%s = %+v, want pass", c.Name, got)
			}
		}
	})

	// Down runs last: it removes what the subtests above filled.
	t.Run("Down", func(t *testing.T) {
		if err := d.MigrateDownTo(t.Context(), 2); err != nil {
			t.Fatalf("MigrateDownTo(2): %v", err)
		}
		tables := tableNames(t, d)
		for _, gone := range []string{"clusters", "secret_versions", "cluster_pairs", "host_renders", "tickets", "talos_cache"} {
			if slices.Contains(tables, gone) {
				t.Errorf("table %q exists after Down to 2; have %v", gone, tables)
			}
		}
		query := "SELECT name FROM pragma_table_info('machines')"
		if d.Dialect() == db.DialectPostgres {
			query = "SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'machines'"
		}
		var columns []string
		if err := d.Bun().NewRaw(query).Scan(t.Context(), &columns); err != nil {
			t.Fatalf("list machines columns: %v", err)
		}
		for _, gone := range []string{"identity_observed", "talos_schematic", "talos_version"} {
			if slices.Contains(columns, gone) {
				t.Errorf("machines column %q exists after Down to 2; have %v", gone, columns)
			}
		}
		if err := d.Migrate(t.Context()); err != nil {
			t.Fatalf("Migrate after Down: %v", err)
		}
	})
}

func tableNames(t *testing.T, d *db.DB) []string {
	t.Helper()
	query := "SELECT name FROM sqlite_master WHERE type = 'table'"
	if d.Dialect() == db.DialectPostgres {
		query = "SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema()"
	}
	var got []string
	if err := d.Bun().NewRaw(query).Scan(t.Context(), &got); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	return got
}

func insertMachine(t *testing.T, d *db.DB, id string, junk bool) error {
	t.Helper()
	_, err := d.Bun().ExecContext(t.Context(),
		"INSERT INTO machines (id, smbios_uuid, junk, serial, product, arch, state, created_at, last_seen_at) VALUES (?, ?, ?, 's', 'p', 'amd64', 'discovered', '2026-11-14T12:00:00Z', '2026-11-14T12:00:00Z')",
		id, fixtureUUID, junk)
	return err
}
