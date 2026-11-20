//go:build integration

package clusters

import (
	"testing"

	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestRepository_Postgres(t *testing.T) { runRepositorySuite(t, dbtest.Postgres) }
