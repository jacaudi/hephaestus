//go:build integration

package datakey

import (
	"testing"

	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
)

func TestEnsureCanary_Postgres(t *testing.T) {
	t.Run("WritesThenOpens", func(t *testing.T) { ensureCanaryWritesThenOpens(t, dbtest.Postgres(t)) })
	t.Run("WrongKey", func(t *testing.T) { ensureCanaryWrongKey(t, dbtest.Postgres(t)) })
}
