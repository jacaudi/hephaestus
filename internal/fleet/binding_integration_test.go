//go:build integration

package fleet

import (
	"testing"

	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestBinding_Postgres(t *testing.T) { runBindingSuite(t, dbtest.Postgres, dbtest.PostgresPair) }
