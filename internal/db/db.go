package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"

	// Register the pgx database/sql driver ("pgx") for Open.
	_ "github.com/jackc/pgx/v5/stdlib"
	// Register the pure-Go SQLite database/sql driver ("sqlite") for Open.
	_ "modernc.org/sqlite"

	"go.jacaudi.dev/hephaestus/internal/secret"
)

//go:embed migrations
var migrations embed.FS

// Dialect names a supported database.
type Dialect string

// The supported databases.
const (
	// DialectSQLite is a SQLite file, the default.
	DialectSQLite Dialect = "sqlite"
	// DialectPostgres is a Postgres server.
	DialectPostgres Dialect = "postgres"
)

// ParseURL splits a database URL into its Dialect and what Open connects to:
// the absolute file path for SQLite (from sqlite:<path> or sqlite://<path>),
// the URL unchanged for Postgres. Its errors never include raw, which may
// hold a password.
func ParseURL(raw string) (Dialect, string, error) {
	if path, ok := strings.CutPrefix(raw, "sqlite:"); ok {
		path = strings.TrimPrefix(path, "//")
		if !strings.HasPrefix(path, "/") {
			return "", "", errors.New("sqlite path must be absolute")
		}
		return DialectSQLite, path, nil
	}
	if strings.HasPrefix(raw, "postgres://") || strings.HasPrefix(raw, "postgresql://") {
		return DialectPostgres, raw, nil
	}
	return "", "", errors.New("unsupported scheme; use sqlite:/absolute/path.db or postgres://")
}

// DB is an open database: the bun handle repositories build on, and the
// dialect it speaks.
type DB struct {
	bdb *bun.DB

	dialect Dialect
}

// Open connects to the database at url and verifies the connection with a
// ping. It does not migrate.
func Open(ctx context.Context, url secret.Secret) (*DB, error) {
	dialect, target, err := ParseURL(url.Expose())
	if err != nil {
		return nil, fmt.Errorf("db: %w", err)
	}

	var bunDB *bun.DB
	switch dialect {
	case DialectSQLite:
		sqlDB, err := sql.Open("sqlite", "file:"+target+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
		if err != nil {
			return nil, fmt.Errorf("db: open sqlite: %w", err)
		}
		sqlDB.SetMaxOpenConns(1)
		bunDB = bun.NewDB(sqlDB, sqlitedialect.New())
	case DialectPostgres:
		sqlDB, err := sql.Open("pgx", target)
		if err != nil {
			return nil, fmt.Errorf("db: open postgres: %w", err)
		}
		bunDB = bun.NewDB(sqlDB, pgdialect.New())
	}

	if err := bunDB.PingContext(ctx); err != nil {
		return nil, errors.Join(fmt.Errorf("db: ping: %w", err), bunDB.Close())
	}
	return &DB{bdb: bunDB, dialect: dialect}, nil
}

// Bun returns the bun handle repositories query through.
func (d *DB) Bun() *bun.DB { return d.bdb }

// Dialect returns the database the DB speaks.
func (d *DB) Dialect() Dialect { return d.dialect }

// Ping verifies the database is reachable.
func (d *DB) Ping(ctx context.Context) error {
	if err := d.bdb.PingContext(ctx); err != nil {
		return fmt.Errorf("db: ping: %w", err)
	}
	return nil
}

// Migrate applies every pending migration. On Postgres a table lock keeps
// concurrent replicas from migrating at once.
func (d *DB) Migrate(ctx context.Context) error {
	p, err := d.provider()
	if err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("db: migrate: %w", err)
	}
	return nil
}

// Pending reports whether any migration has not been applied.
func (d *DB) Pending(ctx context.Context) (bool, error) {
	p, err := d.provider()
	if err != nil {
		return false, err
	}
	pending, err := p.HasPending(ctx)
	if err != nil {
		return false, fmt.Errorf("db: pending migrations: %w", err)
	}
	return pending, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.bdb.Close() }

// provider builds a goose provider over the dialect's embedded migrations.
// It is never closed: Provider.Close would close the DB.
func (d *DB) provider() (*goose.Provider, error) {
	sub, err := fs.Sub(migrations, "migrations/"+string(d.dialect))
	if err != nil {
		return nil, fmt.Errorf("db: migrations for %s: %w", d.dialect, err)
	}
	if d.dialect == DialectPostgres {
		l, err := lock.NewPostgresTableLocker()
		if err != nil {
			return nil, fmt.Errorf("db: migration lock: %w", err)
		}
		return newProvider(goose.DialectPostgres, d.bdb.DB, sub, goose.WithLocker(l))
	}
	return newProvider(goose.DialectSQLite3, d.bdb.DB, sub)
}

func newProvider(dialect goose.Dialect, sqlDB *sql.DB, sub fs.FS, opts ...goose.ProviderOption) (*goose.Provider, error) {
	p, err := goose.NewProvider(dialect, sqlDB, sub, opts...)
	if err != nil {
		return nil, fmt.Errorf("db: migration provider: %w", err)
	}
	return p, nil
}
