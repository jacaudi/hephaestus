package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/secret"
)

// PostgresImage is the Postgres image the integration tests run, pinned by digest.
const PostgresImage = "postgres:18-alpine@sha256:1018733457011bf88d81db98cf43aac6d5150afc18787dbe204771fedf80ff95"

const (
	postgresUser = "hephaestus"
	postgresPass = "hephaestus"
	postgresName = "hephaestus"

	readyEvery = 250 * time.Millisecond
	readyLimit = 60 * time.Second
)

// postgresAddr is the host:port of the container RunWithPostgres started;
// empty when none is running.
var postgresAddr string //nolint:gochecknoglobals // process-wide test container shared by RunWithPostgres and Postgres

// SQLite opens a migrated SQLite database in a file under tb.TempDir and
// closes it when the test ends.
func SQLite(tb testing.TB) *db.DB {
	tb.Helper()
	return openMigrated(tb, "sqlite", sqliteURL(tb))
}

// SQLitePair opens two handles on one migrated SQLite file under tb.TempDir,
// serve's and the CLI's, as two processes share it, and closes both when the
// test ends.
func SQLitePair(tb testing.TB) (serve, cli *db.DB) {
	tb.Helper()
	url := sqliteURL(tb)
	serve = openMigrated(tb, "sqlite", url)
	return serve, open(tb, "sqlite", url)
}

func sqliteURL(tb testing.TB) secret.Secret {
	tb.Helper()
	return secret.Secret("sqlite:" + tb.TempDir() + "/test.db")
}

// open connects to url and closes the handle when the test ends. label names
// the database in failures; the URL never appears in them, as it holds a secret.
func open(tb testing.TB, label string, url secret.Secret) *db.DB {
	tb.Helper()
	d, err := db.Open(tb.Context(), url)
	if err != nil {
		tb.Fatalf("dbtest: open %s: %v", label, err)
	}
	tb.Cleanup(func() { closeDB(tb, d) })
	return d
}

// openMigrated is open followed by Migrate.
func openMigrated(tb testing.TB, label string, url secret.Secret) *db.DB {
	tb.Helper()
	d := open(tb, label, url)
	if err := d.Migrate(tb.Context()); err != nil {
		tb.Fatalf("dbtest: migrate %s: %v", label, err)
	}
	return d
}

// RunWithPostgres starts a Postgres container, runs the package's tests with
// m.Run, removes the container and returns the exit code for os.Exit. It
// needs a reachable docker daemon (DOCKER_HOST).
func RunWithPostgres(m *testing.M) int {
	ctx := context.Background()
	id, err := startPostgres(ctx)
	if id != "" {
		defer func() {
			if out, rmErr := exec.CommandContext(ctx, "docker", "rm", "-f", id).CombinedOutput(); rmErr != nil { //nolint:gosec // G204: fixed docker CLI args in tests
				fmt.Fprintf(os.Stderr, "dbtest: remove postgres %s: %v: %s\n", id, rmErr, out)
			}
		}()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "dbtest: start postgres: %v (is DOCKER_HOST set?)\n", err)
		return 1
	}
	defer func() { postgresAddr = "" }()
	return m.Run()
}

// startPostgres runs the container and waits until it accepts queries. It
// returns the container ID as soon as there is one, so the caller can remove
// a container that never became ready.
func startPostgres(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "--label", "hephaestus-dbtest=1",
		"-e", "POSTGRES_USER="+postgresUser, "-e", "POSTGRES_PASSWORD="+postgresPass, "-e", "POSTGRES_DB="+postgresName,
		"-p", "127.0.0.1::5432", PostgresImage).Output()
	if err != nil {
		return "", fmt.Errorf("docker run: %w", err)
	}
	id := strings.TrimSpace(string(out))

	out, err = exec.CommandContext(ctx, "docker", "port", id, "5432/tcp").Output() //nolint:gosec // G204: fixed docker CLI args in tests
	if err != nil {
		return id, fmt.Errorf("docker port: %w", err)
	}
	mapping, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if _, _, err := net.SplitHostPort(mapping); err != nil {
		return id, fmt.Errorf("docker port printed %q: %w", mapping, err)
	}

	// A TCP connect is not readiness (colima's forwarder accepts before
	// Postgres does), so wait for a real ping.
	deadline := time.Now().Add(readyLimit)
	for {
		d, err := db.Open(ctx, postgresURL(mapping, postgresName))
		if err == nil {
			postgresAddr = mapping
			return id, d.Close()
		}
		if time.Now().After(deadline) {
			return id, fmt.Errorf("not ready after %s: %w", readyLimit, err)
		}
		time.Sleep(readyEvery)
	}
}

func postgresURL(addr, name string) secret.Secret {
	return secret.Secret("postgres://" + postgresUser + ":" + postgresPass + "@" + addr + "/" + name + "?sslmode=disable")
}

// Postgres creates a fresh, migrated database in the container that
// RunWithPostgres started and drops it when the test ends.
func Postgres(tb testing.TB) *db.DB {
	tb.Helper()
	name, url := createPostgres(tb)
	return openMigrated(tb, name, url)
}

// PostgresPair is Postgres with a second handle on the same database: serve's
// and the CLI's.
func PostgresPair(tb testing.TB) (serve, cli *db.DB) {
	tb.Helper()
	name, url := createPostgres(tb)
	serve = openMigrated(tb, name, url)
	return serve, open(tb, name, url)
}

// createPostgres creates an empty database in the RunWithPostgres container,
// drops it when the test ends, and returns its name and URL.
func createPostgres(tb testing.TB) (string, secret.Secret) {
	tb.Helper()
	if postgresAddr == "" {
		tb.Fatal("dbtest: Postgres needs TestMain to call dbtest.RunWithPostgres")
	}
	ctx := tb.Context()

	admin, err := db.Open(ctx, postgresURL(postgresAddr, postgresName))
	if err != nil {
		tb.Fatalf("dbtest: open postgres: %v", err)
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		tb.Fatalf("dbtest: random database name: %v", err)
	}
	name := "t_" + hex.EncodeToString(id[:])
	if _, err := admin.Bun().ExecContext(ctx, "CREATE DATABASE ?", bun.Ident(name)); err != nil {
		tb.Fatalf("dbtest: create database: %v", errors.Join(err, admin.Close()))
	}
	tb.Cleanup(func() {
		defer closeDB(tb, admin)
		if _, err := admin.Bun().ExecContext(context.WithoutCancel(ctx), "DROP DATABASE ? WITH (FORCE)", bun.Ident(name)); err != nil {
			tb.Errorf("dbtest: drop database %s: %v", name, err)
		}
	})
	return name, postgresURL(postgresAddr, name)
}

func closeDB(tb testing.TB, d *db.DB) {
	tb.Helper()
	if err := d.Close(); err != nil {
		tb.Errorf("dbtest: close: %v", err)
	}
}
