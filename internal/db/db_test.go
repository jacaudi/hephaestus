package db_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/admin"
	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
	"go.jacaudi.dev/hephaestus/internal/secret"
)

const (
	errAbsolute = "sqlite path must be absolute"
	errScheme   = "unsupported scheme; use sqlite:/absolute/path.db or postgres://"
)

var parseURLCases = []struct {
	name        string
	raw         string
	wantDialect db.Dialect
	wantTarget  string
	wantErr     string
}{
	{"sqlite colon", "sqlite:/var/lib/h/h.db", db.DialectSQLite, "/var/lib/h/h.db", ""},
	{"sqlite slashes", "sqlite:///var/lib/h/h.db", db.DialectSQLite, "/var/lib/h/h.db", ""},
	{"postgres", "postgres://u:p@db.example.test/heph", db.DialectPostgres, "postgres://u:p@db.example.test/heph", ""},
	{"postgresql", "postgresql://u:p@db.example.test/heph", db.DialectPostgres, "postgresql://u:p@db.example.test/heph", ""},
	{"sqlite relative", "sqlite:rel.db", "", "", errAbsolute},
	{"mysql", "mysql://x", "", "", errScheme},
	{"mysql with credentials", "mysql://u:p@db.example.test/x", "", "", errScheme},
	{"empty", "", "", "", errScheme},
}

func TestParseURL(t *testing.T) {
	for _, tc := range parseURLCases {
		t.Run(tc.name, func(t *testing.T) {
			gotDialect, gotTarget, err := db.ParseURL(tc.raw)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("ParseURL(%q) error = %v, want %q", tc.raw, err, tc.wantErr)
				}
				if strings.Contains(err.Error(), "u:p") {
					t.Fatalf("error %q leaks the input", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseURL(%q) error = %v", tc.raw, err)
			}
			if gotDialect != tc.wantDialect || gotTarget != tc.wantTarget {
				t.Fatalf("ParseURL(%q) = %q, %q; want %q, %q", tc.raw, gotDialect, gotTarget, tc.wantDialect, tc.wantTarget)
			}
		})
	}
}

func FuzzParseURL(f *testing.F) {
	for _, tc := range parseURLCases {
		f.Add(tc.raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		dialect, target, err := db.ParseURL(raw)
		if err != nil {
			if strings.Contains(raw, "@") && strings.Contains(err.Error(), raw) {
				t.Fatalf("error %q contains the input %q", err, raw)
			}
			return
		}
		if dialect == db.DialectSQLite && !strings.HasPrefix(target, "/") {
			t.Fatalf("sqlite path %q is not absolute", target)
		}
	})
}

func TestSQLite_Strict(t *testing.T) {
	d := dbtest.SQLite(t)
	ctx := t.Context()

	var ddl string
	if err := d.Bun().NewRaw("SELECT sql FROM sqlite_master WHERE name = 'machines'").Scan(ctx, &ddl); err != nil {
		t.Fatalf("read machines DDL: %v", err)
	}
	if !strings.Contains(ddl, "STRICT") {
		t.Fatalf("machines DDL is not STRICT: %s", ddl)
	}
	if _, err := d.Bun().ExecContext(ctx, "INSERT INTO machines (id, smbios_uuid, junk, serial, product, arch, state, created_at, last_seen_at) VALUES ('m1', 'u', 'yes', 's', 'p', 'amd64', 'discovered', 't', 't')"); err == nil {
		t.Fatal("inserting junk = 'yes' succeeded, want a STRICT type error")
	}
}

func TestPending_BeforeMigrate(t *testing.T) {
	ctx := t.Context()
	d, err := db.Open(ctx, secret.Secret("sqlite:"+t.TempDir()+"/fresh.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	got, err := d.Pending(ctx)
	if err != nil || !got {
		t.Fatalf("Pending before Migrate = %v, %v; want true, nil", got, err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	got, err = d.Pending(ctx)
	if err != nil || got {
		t.Fatalf("Pending after Migrate = %v, %v; want false, nil", got, err)
	}
}

func TestChecks_FailWhenUnhealthy(t *testing.T) {
	ctx := t.Context()
	d, err := db.Open(ctx, secret.Secret("sqlite:"+t.TempDir()+"/fresh.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	checks := byName(d.Checks())

	if got := checks["database:migrations"].Run(ctx); got.Status != admin.StatusFail || got.Output != "pending migrations" {
		t.Fatalf("migrations check before Migrate = %+v, want fail with %q", got, "pending migrations")
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for name, c := range checks {
		if got := c.Run(ctx); got.Status != admin.StatusFail {
			t.Errorf("%s on a closed database = %+v, want fail", name, got)
		}
	}
}

func byName(checks []admin.Check) map[string]admin.Check {
	m := make(map[string]admin.Check, len(checks))
	for _, c := range checks {
		m[c.Name] = c
	}
	return m
}

// Two handles on one file stand in for serve and the CLI. B begins, reads, then A writes in a goroutine;
// B's update and commit succeed, and A's write completes after B commits, without error.
// Under deferred transactions B's update fails with SQLITE_BUSY_SNAPSHOT, so this fails before the DSN change.
func TestOpen_SQLiteReadThenWriteNoBusySnapshot(t *testing.T) {
	ctx := t.Context()
	url := secret.Secret("sqlite:" + t.TempDir() + "/shared.db")
	open := func() *db.DB {
		t.Helper()
		d, err := db.Open(ctx, url)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() {
			if err := d.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		return d
	}
	a, b := open(), open()
	if err := a.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	tx, err := b.Bun().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("B begin: %v", err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("B rollback: %v", err)
		}
	})
	var n int
	if err := tx.NewRaw("SELECT count(*) FROM settings").Scan(ctx, &n); err != nil {
		t.Fatalf("B read: %v", err)
	}

	aDone := make(chan error, 1)
	go func() {
		_, err := a.Bun().ExecContext(ctx, "INSERT INTO settings (key, value) VALUES ('from_a', '1')")
		aDone <- err
	}()
	// Deferred: A completes while B holds only a read snapshot. Immediate: A waits for B's write lock.
	var aErr error
	aFinished := false
	select {
	case aErr = <-aDone:
		aFinished = true
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := tx.ExecContext(ctx, "INSERT INTO settings (key, value) VALUES ('from_b', '2')"); err != nil {
		t.Fatalf("B write after a read: %v (A finished first: %v)", err, aFinished)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("B commit: %v", err)
	}
	if !aFinished {
		aErr = <-aDone
	}
	if aErr != nil {
		t.Fatalf("A write: %v", aErr)
	}
	var got int
	if err := a.Bun().NewRaw("SELECT count(*) FROM settings WHERE key IN ('from_a', 'from_b')").Scan(ctx, &got); err != nil || got != 2 {
		t.Fatalf("rows from A and B = %d, %v; want 2, nil", got, err)
	}
}
