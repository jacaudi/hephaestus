//go:build integration

package tlsacme_test

import (
	"testing"

	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestRepository_Postgres(t *testing.T) { runRepositorySuite(t, dbtest.Postgres) }
