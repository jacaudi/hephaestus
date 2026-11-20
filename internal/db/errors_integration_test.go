//go:build integration

package db_test

import (
	"testing"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestErrors_Postgres(t *testing.T) {
	d := dbtest.Postgres(t)

	t.Run("UniqueViolation", func(t *testing.T) {
		_, err := d.Bun().ExecContext(t.Context(), "INSERT INTO settings (key, value) VALUES ('unknown_action', 'x')")
		if err == nil {
			t.Fatal("duplicate settings key succeeded, want a unique violation")
		}
		if !db.IsUniqueViolation(err) {
			t.Errorf("IsUniqueViolation(%v) = false, want true", err)
		}
		if db.IsRetryable(err) {
			t.Errorf("IsRetryable(%v) = true, want false for a unique violation", err)
		}
	})

	for _, tc := range []struct {
		name, code string
	}{
		{"SerializationFailure", "40001"},
		{"Deadlock", "40P01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.Bun().ExecContext(t.Context(), "DO $$ BEGIN RAISE EXCEPTION USING ERRCODE = '"+tc.code+"'; END $$")
			if err == nil {
				t.Fatalf("RAISE with ERRCODE %s succeeded, want an error", tc.code)
			}
			if !db.IsRetryable(err) {
				t.Errorf("IsRetryable(%v) = false, want true", err)
			}
			if db.IsUniqueViolation(err) {
				t.Errorf("IsUniqueViolation(%v) = true, want false", err)
			}
		})
	}
}
