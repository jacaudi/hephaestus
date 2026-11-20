//go:build integration

package fleet

import (
	"testing"

	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestTickets_Postgres(t *testing.T) { runTicketSuite(t, dbtest.Postgres, dbtest.PostgresPair) }
