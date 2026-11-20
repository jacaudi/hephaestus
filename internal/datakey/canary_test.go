package datakey

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
	"go.jacaudi.dev/hephaestus/internal/secret"
)

// otherKey is a data key that differs from fixtureKey.
func otherKey(t *testing.T) *Key {
	t.Helper()

	return mustParse(t, secret.Secret(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, Size))))
}

// canaryValue reads the stored canary setting.
func canaryValue(t *testing.T, d *db.DB) (string, int) {
	t.Helper()

	var values []string
	if err := d.Bun().NewRaw("SELECT value FROM settings WHERE key = ?", CanarySetting).Scan(t.Context(), &values); err != nil {
		t.Fatalf("read canary: %v", err)
	}
	if len(values) == 0 {
		return "", 0
	}
	return values[0], len(values)
}

// ensureCanaryWritesThenOpens runs the first-start and later-start cases on d.
func ensureCanaryWritesThenOpens(t *testing.T, d *db.DB) {
	t.Helper()

	k := mustParse(t, fixtureKey)
	if _, n := canaryValue(t, d); n != 0 {
		t.Fatalf("canary rows before the first call = %d, want 0", n)
	}

	if err := EnsureCanary(t.Context(), d.Bun(), k); err != nil {
		t.Fatalf("first EnsureCanary: %v", err)
	}
	stored, n := canaryValue(t, d)
	if n != 1 {
		t.Fatalf("canary rows after the first call = %d, want 1", n)
	}
	sealed, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		t.Fatalf("stored canary is not base64: %v", err)
	}
	got, err := k.OpenField(Field{Table: "settings", Row: CanarySetting, Column: "value"}, sealed)
	if err != nil {
		t.Fatalf("stored canary does not open: %v", err)
	}
	if want := "hephaestus-canary-v1"; string(got) != want {
		t.Errorf("canary plaintext = %q, want %q", got, want)
	}

	if err := EnsureCanary(t.Context(), d.Bun(), k); err != nil {
		t.Fatalf("second EnsureCanary: %v", err)
	}
	if again, n := canaryValue(t, d); n != 1 || again != stored {
		t.Errorf("canary after the second call = %q (%d rows), want it unchanged: %q", again, n, stored)
	}
}

// ensureCanaryWrongKey runs the wrong-key and damaged-row cases on d.
func ensureCanaryWrongKey(t *testing.T, d *db.DB) {
	t.Helper()

	if err := EnsureCanary(t.Context(), d.Bun(), mustParse(t, fixtureKey)); err != nil {
		t.Fatalf("EnsureCanary with key A: %v", err)
	}
	stored, _ := canaryValue(t, d)

	if err := EnsureCanary(t.Context(), d.Bun(), otherKey(t)); !errors.Is(err, ErrCanary) {
		t.Errorf("EnsureCanary with key B error = %v, want ErrCanary", err)
	}
	if after, _ := canaryValue(t, d); after != stored {
		t.Error("a failed check changed the stored canary")
	}

	if _, err := d.Bun().NewRaw("UPDATE settings SET value = ? WHERE key = ?", "not base64!", CanarySetting).Exec(t.Context()); err != nil {
		t.Fatalf("damage canary: %v", err)
	}
	if err := EnsureCanary(t.Context(), d.Bun(), mustParse(t, fixtureKey)); !errors.Is(err, ErrCanary) {
		t.Errorf("EnsureCanary on a damaged row error = %v, want ErrCanary", err)
	}
}

func TestEnsureCanary_WritesThenOpens(t *testing.T) {
	ensureCanaryWritesThenOpens(t, dbtest.SQLite(t))
}

func TestEnsureCanary_WrongKey(t *testing.T) {
	ensureCanaryWrongKey(t, dbtest.SQLite(t))
}
