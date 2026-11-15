//go:build integration

package db_test

import (
	"testing"

	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestMigrate_Postgres(t *testing.T) { runMigrateSuite(t, dbtest.Postgres) }
