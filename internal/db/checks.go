package db

import (
	"context"
	"time"

	"go.jacaudi.dev/hephaestus/internal/admin"
)

// Checks returns the database health checks: ping, with the round-trip time
// in milliseconds, and pending migrations. Both are critical.
func (d *DB) Checks() []admin.Check {
	return []admin.Check{
		{Name: "database:ping", Critical: true, Run: d.pingCheck},
		{Name: "database:migrations", Critical: true, Run: d.migrationsCheck},
	}
}

func (d *DB) pingCheck(ctx context.Context) admin.Result {
	start := time.Now()
	if err := d.Ping(ctx); err != nil {
		return admin.Result{Status: admin.StatusFail, Output: err.Error()}
	}
	return admin.Result{
		Status:        admin.StatusPass,
		ObservedValue: float64(time.Since(start)) / float64(time.Millisecond),
		ObservedUnit:  "ms",
	}
}

func (d *DB) migrationsCheck(ctx context.Context) admin.Result {
	pending, err := d.Pending(ctx)
	if err != nil {
		return admin.Result{Status: admin.StatusFail, Output: err.Error()}
	}
	if pending {
		return admin.Result{Status: admin.StatusFail, Output: "pending migrations"}
	}
	return admin.Result{Status: admin.StatusPass}
}
