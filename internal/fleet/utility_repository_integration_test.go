//go:build integration

package fleet

import (
	"testing"

	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestUtilityRepository_Postgres(t *testing.T) { runUtilitySuite(t, dbtest.Postgres) }
