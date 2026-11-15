package tlsacme_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.jacaudi.dev/hephaestus/internal/datakey"
	"go.jacaudi.dev/hephaestus/internal/db/dbtest"
	"go.jacaudi.dev/hephaestus/internal/tlsacme"
)

const testDirectory = "https://acme.example.test/directory"

func newLego(t *testing.T, directory string, key *datakey.Key) (*tlsacme.LegoIssuer, *tlsacme.Repository) {
	t.Helper()
	repo := tlsacme.NewRepository(dbtest.SQLite(t))
	return tlsacme.NewLegoIssuer(tlsacme.LegoConfig{
		DirectoryURL:    directory,
		CloudflareToken: "cf-secret",
		Repo:            repo,
		Key:             key,
	}), repo
}

func TestLegoIssuer_Redacts(t *testing.T) {
	iss, _ := newLego(t, testDirectory, mustKey(t, fixtureKey))
	cfg := tlsacme.LegoConfig{DirectoryURL: testDirectory, CloudflareToken: "cf-secret", Key: mustKey(t, fixtureKey)}

	for name, v := range map[string]any{"config": cfg, "config pointer": &cfg, "issuer": iss} {
		out := fmt.Sprintf("%v %+v %#v", v, v, v)

		var buf bytes.Buffer
		slog.New(slog.NewJSONHandler(&buf, nil)).Info("lego", "value", v)
		out += buf.String()

		if strings.Contains(out, "cf-secret") {
			t.Errorf("%s output leaks the Cloudflare token: %s", name, out)
		}
		for _, want := range []string{testDirectory, "****"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s output = %s, want it to contain %q", name, out, want)
			}
		}
	}
}

func TestLegoIssuer_LoadWrongKeyFails(t *testing.T) {
	good := mustKey(t, fixtureKey)
	iss, repo := newLego(t, testDirectory, mustKey(t, otherKey))

	if err := iss.Load(t.Context()); err != nil {
		t.Fatalf("Load on an empty repository = %v, want nil", err)
	}

	err := repo.SaveAccount(t.Context(), tlsacme.Account{
		DirectoryURL: testDirectory,
		KeySealed:    good.Seal([]byte("account key"), []byte("acme_accounts/key/"+testDirectory)),
		Registration: []byte(`{}`),
		CreatedAt:    start(),
	})
	if err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	err = iss.Load(t.Context())
	if !errors.Is(err, datakey.ErrDecrypt) {
		t.Fatalf("Load error = %v, want one wrapping datakey.ErrDecrypt", err)
	}
	for _, want := range []string{"stored ACME account for", "cannot be decrypted with the configured data key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load error = %q, want it to contain %q", err, want)
		}
	}
}

func TestLegoIssuer_LoadRightKeyPasses(t *testing.T) {
	key := mustKey(t, fixtureKey)
	iss, repo := newLego(t, testDirectory, key)
	accountKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(accountKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	err = repo.SaveAccount(t.Context(), tlsacme.Account{
		DirectoryURL: testDirectory,
		KeySealed:    key.Seal(keyDER, []byte("acme_accounts/key/"+testDirectory)),
		Registration: []byte(`{}`),
		CreatedAt:    start(),
	})
	if err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	if err := iss.Load(t.Context()); err != nil {
		t.Errorf("Load = %v, want nil", err)
	}
}

func TestLegoIssuer_CanceledContext(t *testing.T) {
	iss, repo := newLego(t, "http://127.0.0.1:1/directory", mustKey(t, fixtureKey))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := iss.Obtain(ctx, domain); !errors.Is(err, context.Canceled) {
		t.Errorf("Obtain error = %v, want context.Canceled", err)
	}
	if _, ok, err := repo.LoadAccount(t.Context(), "http://127.0.0.1:1/directory"); err != nil || ok {
		t.Errorf("LoadAccount = ok %v, err %v; want no account row", ok, err)
	}
}

func TestLegoIssuer_BadDirectory(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	iss, repo := newLego(t, srv.URL+"/directory", mustKey(t, fixtureKey))

	_, err := iss.Obtain(t.Context(), domain)
	if err == nil || !strings.HasPrefix(err.Error(), "acme: ") {
		t.Fatalf("Obtain error = %v, want one starting with %q", err, "acme: ")
	}
	if _, ok, err := repo.LoadAccount(t.Context(), srv.URL+"/directory"); err != nil || ok {
		t.Errorf("LoadAccount = ok %v, err %v; want no account row", ok, err)
	}
}
