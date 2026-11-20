//go:build integration

package clusters

import (
	"testing"

	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestApprove_Postgres(t *testing.T) { runApproveSuite(t, dbtest.Postgres) }
