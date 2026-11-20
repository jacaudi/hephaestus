//go:build integration

package fleet

import (
	"testing"

	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestTalosCache_Postgres(t *testing.T) { runTalosSuite(t, dbtest.Postgres, dbtest.PostgresPair) }
