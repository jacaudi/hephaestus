//go:build integration

package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

// holdThenRun lets hold write inside a transaction it keeps open, starts run in
// the background, waits until run is blocked on a lock, commits hold's
// transaction and returns run's error. Postgres only: SQLite has one writer, so
// run would not start until hold's transaction ended.
func holdThenRun(t *testing.T, d *db.DB, machineID string, hold func(ctx context.Context, tx bun.Tx) error, run func(ctx context.Context) error) error {
	t.Helper()
	tx, err := d.Bun().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed transaction has nothing to roll back
	if _, err := lockMachine(t.Context(), tx, machineID); err != nil {
		t.Fatalf("lock machine: %v", err)
	}
	if err := hold(t.Context(), tx); err != nil {
		t.Fatalf("hold: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- run(t.Context()) }()
	deadline := time.Now().Add(10 * time.Second)
	for scalar[int](t, d, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the competing transaction never waited on a lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return <-done
}

func TestTransition_Postgres_ReadsTheMachineUnderItsLock(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	t.Run("CordonDuringConfigServed", func(t *testing.T) {
		d := dbtest.Postgres(t)
		r := NewRepository(d)
		m := assignedMachine(t, r, 1, labHost, t0)
		err := holdThenRun(t, d, m.ID,
			func(ctx context.Context, tx bun.Tx) error {
				_, err := TransitionTx(ctx, tx, TransitionRequest{MachineID: m.ID, Trigger: TriggerConfigServed, Actor: ActorSystem, At: t0.Add(time.Minute)})
				return err
			},
			func(ctx context.Context) error {
				_, err := r.Transition(ctx, TransitionRequest{MachineID: m.ID, Trigger: TriggerCordon, Actor: "admin", At: t0.Add(2 * time.Minute)})
				return err
			})
		if err != nil {
			t.Fatalf("cordon: %v", err)
		}
		got := mustGet(t, r, m.ID)
		if got.State != StateCordoned || got.Prior != StateInstalling {
			t.Fatalf("machine = %s over %q, want cordoned over installing (the state the delivery committed)", got.State, got.Prior)
		}
	})

	t.Run("AbortDuringNextBoot", func(t *testing.T) {
		d := dbtest.Postgres(t)
		r := NewRepository(d)
		m := mustTransition(t, r, assignedMachine(t, r, 1, labHost, t0).ID, TriggerConfigServed, t0.Add(time.Minute))
		err := holdThenRun(t, d, m.ID,
			func(ctx context.Context, tx bun.Tx) error {
				_, err := TransitionTx(ctx, tx, TransitionRequest{MachineID: m.ID, Trigger: TriggerAbortInstall, Actor: "admin", At: t0.Add(2 * time.Minute)})
				return err
			},
			func(ctx context.Context) error {
				rec := bootOf(m, 10, "install", t0.Add(3*time.Minute))
				rec.Trigger = TriggerNextBoot
				_, err := r.RecordBoot(ctx, rec)
				return err
			})
		_ = err // the boot is refused or recorded against the aborted state; either is consistent
		if got := mustGet(t, r, m.ID); got.State != StateAssigned {
			t.Fatalf("machine = %s, want assigned: the abort committed first and a boot cannot move it to install-unconfirmed", got.State)
		}
	})
}
