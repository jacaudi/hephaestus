// Package db opens the service database, SQLite by default or Postgres, runs
// its embedded goose migrations, and reports its health. Callers build
// repositories on the bun handle that DB exposes.
package db
