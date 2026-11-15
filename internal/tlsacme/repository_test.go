package tlsacme_test

import (
	"bytes"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/db"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
	"go.jacaudi.dev/hephaestus/internal/tlsacme"
)

func TestRepository_SQLite(t *testing.T) { runRepositorySuite(t, dbtest.SQLite) }

// runRepositorySuite is the one database suite: TestRepository_SQLite runs it
// untagged, TestRepository_Postgres under the integration tag.
func runRepositorySuite(t *testing.T, open func(testing.TB) *db.DB) {
	t.Helper()

	t.Run("AccountRoundTrip", func(t *testing.T) {
		repo := tlsacme.NewRepository(open(t))
		ctx := t.Context()
		const dir = "https://acme.example.test/directory"

		if _, ok, err := repo.LoadAccount(ctx, dir); err != nil || ok {
			t.Fatalf("LoadAccount(empty) = ok %v, err %v; want false, nil", ok, err)
		}

		want := tlsacme.Account{
			DirectoryURL: dir,
			KeySealed:    []byte{0x00, 0xff, 0x10, 0x80, 0x00},
			Registration: []byte(`{"body":{"contact":["mailto:Ü@example.test"]},"uri":"https://acme.example.test/acct/1"}`),
			CreatedAt:    start(),
		}
		if err := repo.SaveAccount(ctx, want); err != nil {
			t.Fatalf("SaveAccount: %v", err)
		}
		got, ok, err := repo.LoadAccount(ctx, dir)
		if err != nil || !ok {
			t.Fatalf("LoadAccount = ok %v, err %v; want true, nil", ok, err)
		}
		if !bytes.Equal(got.KeySealed, want.KeySealed) {
			t.Errorf("KeySealed = %x, want %x", got.KeySealed, want.KeySealed)
		}
		if !bytes.Equal(got.Registration, want.Registration) {
			t.Errorf("Registration = %q, want %q", got.Registration, want.Registration)
		}
		if !got.CreatedAt.Equal(want.CreatedAt) {
			t.Errorf("CreatedAt = %s, want %s", got.CreatedAt, want.CreatedAt)
		}

		want.KeySealed = []byte("second")
		if err := repo.SaveAccount(ctx, want); err != nil {
			t.Fatalf("SaveAccount (upsert): %v", err)
		}
		got, _, err = repo.LoadAccount(ctx, dir)
		if err != nil {
			t.Fatalf("LoadAccount after upsert: %v", err)
		}
		if !bytes.Equal(got.KeySealed, want.KeySealed) {
			t.Errorf("KeySealed after upsert = %q, want %q", got.KeySealed, want.KeySealed)
		}
	})

	t.Run("CertificateUpsert", func(t *testing.T) {
		repo := tlsacme.NewRepository(open(t))
		ctx := t.Context()
		first := tlsacme.StoredCertificate{
			Domain: domain, ChainSealed: []byte("chain-1"), KeySealed: []byte("key-1"),
			NotBefore: start(), NotAfter: start().Add(90 * 24 * time.Hour), UpdatedAt: start(),
		}
		second := first
		second.ChainSealed, second.KeySealed = []byte("chain-2"), []byte("key-2")
		second.NotAfter = start().Add(120 * 24 * time.Hour)
		second.UpdatedAt = start().Add(time.Hour)

		for _, c := range []*tlsacme.StoredCertificate{&first, &second} {
			if err := repo.SaveCertificate(ctx, *c); err != nil {
				t.Fatalf("SaveCertificate: %v", err)
			}
		}
		got, ok, err := repo.LoadCertificate(ctx, domain)
		if err != nil || !ok {
			t.Fatalf("LoadCertificate = ok %v, err %v; want true, nil", ok, err)
		}
		if !bytes.Equal(got.ChainSealed, second.ChainSealed) || !bytes.Equal(got.KeySealed, second.KeySealed) {
			t.Errorf("sealed = %q/%q, want the second save %q/%q", got.ChainSealed, got.KeySealed, second.ChainSealed, second.KeySealed)
		}
		if !got.NotAfter.Equal(second.NotAfter) || !got.NotBefore.Equal(second.NotBefore) || !got.UpdatedAt.Equal(second.UpdatedAt) {
			t.Errorf("times = %s/%s/%s, want %s/%s/%s", got.NotBefore, got.NotAfter, got.UpdatedAt, second.NotBefore, second.NotAfter, second.UpdatedAt)
		}
	})

	t.Run("CertificateMissing", func(t *testing.T) {
		repo := tlsacme.NewRepository(open(t))
		if _, ok, err := repo.LoadCertificate(t.Context(), domain); err != nil || ok {
			t.Fatalf("LoadCertificate(empty) = ok %v, err %v; want false, nil", ok, err)
		}
	})
}
