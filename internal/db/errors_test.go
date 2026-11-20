package db_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestIsUniqueViolation_SQLite(t *testing.T) {
	d := dbtest.SQLite(t)
	_, uniqueErr := d.Bun().ExecContext(t.Context(), "INSERT INTO settings (key, value) VALUES ('unknown_action', 'x')")
	_, checkErr := d.Bun().ExecContext(t.Context(),
		"INSERT INTO machines (id, smbios_uuid, serial, product, arch, state, created_at, last_seen_at) VALUES ('m', 'u', 's', 'p', 'sparc', 'discovered', '2026-11-14T12:00:00Z', '2026-11-14T12:00:00Z')")
	if uniqueErr == nil || checkErr == nil {
		t.Fatalf("setup statements succeeded: unique %v, check %v; want errors", uniqueErr, checkErr)
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"duplicate settings key", uniqueErr, true},
		{"wrapped duplicate settings key", fmt.Errorf("insert: %w", uniqueErr), true},
		{"CHECK violation", checkErr, false},
		{"unrelated error", errors.New("boom"), false},
		{"nil", nil, false},
	} {
		if got := db.IsUniqueViolation(tc.err); got != tc.want {
			t.Errorf("%s: IsUniqueViolation(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

func TestIsRetryable_SQLiteBusy(t *testing.T) {
	path := t.TempDir() + "/busy.db"
	const dsn = "file:%s?_pragma=busy_timeout(0)"
	holder, err := sql.Open("sqlite", fmt.Sprintf(dsn, path))
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	t.Cleanup(func() { closeHandle(t, holder) })
	waiter, err := sql.Open("sqlite", fmt.Sprintf(dsn, path))
	if err != nil {
		t.Fatalf("open waiter: %v", err)
	}
	t.Cleanup(func() { closeHandle(t, waiter) })

	if _, err := holder.ExecContext(t.Context(), "CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	conn, err := holder.Conn(t.Context())
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.ExecContext(context.WithoutCancel(t.Context()), "ROLLBACK"); err != nil {
			t.Errorf("rollback: %v", err)
		}
		if err := conn.Close(); err != nil {
			t.Errorf("close holder conn: %v", err)
		}
	})
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("begin immediate: %v", err)
	}

	_, err = waiter.ExecContext(t.Context(), "INSERT INTO t (x) VALUES (1)")
	if err == nil {
		t.Fatal("write while another handle holds BEGIN IMMEDIATE succeeded, want a busy error")
	}
	if !db.IsRetryable(err) {
		t.Errorf("IsRetryable(%v) = false, want true", err)
	}
	if db.IsRetryable(errors.New("boom")) || db.IsRetryable(nil) {
		t.Error("IsRetryable is true for an unrelated error or nil, want false")
	}
}

func closeHandle(t *testing.T, h *sql.DB) {
	t.Helper()
	if err := h.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

// runInTxCalls runs RunInTxRetry over a fresh SQLite database with a function
// that returns each of results in turn, and returns the error and the number
// of calls.
func runInTxCalls(t *testing.T, results ...error) (int, error) {
	t.Helper()
	d := dbtest.SQLite(t)
	calls := 0
	err := db.RunInTxRetry(t.Context(), d.Bun(), func(context.Context, bun.Tx) error {
		calls++
		return results[calls-1]
	})
	return calls, err
}

func TestRunInTxRetry_ConflictRetriedOnce(t *testing.T) {
	calls, err := runInTxCalls(t, fmt.Errorf("claim: %w", db.ErrConflict), nil)
	if err != nil || calls != 2 {
		t.Fatalf("RunInTxRetry = %v after %d calls, want nil after 2", err, calls)
	}
}

func TestRunInTxRetry_DeadlockRetriedOnce(t *testing.T) {
	deadlock := func() error { return fmt.Errorf("x: %w", &pgconn.PgError{Code: "40P01"}) }
	calls, err := runInTxCalls(t, deadlock(), deadlock())
	if !db.IsRetryable(err) || calls != 2 {
		t.Fatalf("RunInTxRetry = %v after %d calls, want the second deadlock after 2", err, calls)
	}
}

func TestRunInTxRetry_OtherErrorNotRetried(t *testing.T) {
	boom := errors.New("boom")
	calls, err := runInTxCalls(t, boom, nil)
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("RunInTxRetry = %v after %d calls, want boom after 1", err, calls)
	}
}
