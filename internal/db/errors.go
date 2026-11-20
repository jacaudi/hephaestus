package db

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrConflict is returned by a conditional write that matched no row because
// what it read has changed since; RunInTxRetry runs such a transaction again.
var ErrConflict = errors.New("db: a conditional write matched no row")

// Postgres SQLSTATE codes.
const (
	pgUniqueViolation     = "23505"
	pgDeadlockDetected    = "40P01"
	pgSerializationFailed = "40001"
)

// IsUniqueViolation reports whether err is a unique or primary-key violation
// from either database.
func IsUniqueViolation(err error) bool {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Code == pgUniqueViolation
	}
	if sqliteErr, ok := errors.AsType[*sqlite.Error](err); ok {
		code := sqliteErr.Code()
		if code == sqlite3.SQLITE_CONSTRAINT_UNIQUE || code == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY {
			return true
		}
		// A driver that reports only the primary code names the constraint in
		// the message, after the result-code text the driver puts first.
		return code&0xff == sqlite3.SQLITE_CONSTRAINT && strings.Contains(sqliteErr.Error(), "UNIQUE constraint failed")
	}
	return false
}

// IsRetryable reports whether err is a deadlock or serialization failure
// (Postgres) or a busy or locked database (SQLite): a failure a fresh
// transaction can succeed after.
func IsRetryable(err error) bool {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Code == pgDeadlockDetected || pgErr.Code == pgSerializationFailed
	}
	if sqliteErr, ok := errors.AsType[*sqlite.Error](err); ok {
		code := sqliteErr.Code() & 0xff
		return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
	}
	return false
}

// RunInTxRetry runs fn in a transaction. When the transaction fails with
// ErrConflict or a retryable error it runs fn once more, in a new
// transaction, and returns that result.
func RunInTxRetry(ctx context.Context, bdb *bun.DB, fn func(ctx context.Context, tx bun.Tx) error) error {
	err := bdb.RunInTx(ctx, nil, fn)
	if errors.Is(err, ErrConflict) || IsRetryable(err) {
		return bdb.RunInTx(ctx, nil, fn)
	}
	return err
}
