//go:build integration

package fleet

import (
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestBootNextRepository_Postgres(t *testing.T) {
	runBootNextSuite(t, dbtest.Postgres, dbtest.PostgresPair)
}

// Postgres hands timestamptz back in the process's local zone; every read puts
// boot_next_set_at in UTC, as it does created_at and last_seen_at.
func TestBootNextSetAtReadInUTC_Postgres(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	r := NewRepository(dbtest.Postgres(t))
	m := bootNextMachine(t, r, 1, t0)
	mustSetBootNext(t, r, m.ID, "tshell", t0)

	got := mustGet(t, r, m.ID)
	if got.BootNextSetAt.Location() != time.UTC || !got.BootNextSetAt.Equal(t0) {
		t.Fatalf("Get: boot_next_set_at = %v in %v, want %v in UTC", got.BootNextSetAt, got.BootNextSetAt.Location(), t0)
	}
	listed, err := r.ListBootNext(t.Context())
	if err != nil || len(listed) != 1 || listed[0].BootNextSetAt.Location() != time.UTC {
		t.Fatalf("ListBootNext = %+v, %v; want one machine with boot_next_set_at in UTC", listed, err)
	}
}
