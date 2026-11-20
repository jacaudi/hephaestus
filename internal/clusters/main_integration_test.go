//go:build integration

package clusters

import (
	"os"
	"testing"

	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestMain(m *testing.M) { os.Exit(dbtest.RunWithPostgres(m)) }
