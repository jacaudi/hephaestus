package tlsacme_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	trustServer(t, srv)
	iss, repo := newLego(t, srv.URL+"/directory", mustKey(t, fixtureKey))

	_, err := iss.Obtain(t.Context(), domain)
	if err == nil || !strings.HasPrefix(err.Error(), "acme: ") {
		t.Fatalf("Obtain error = %v, want one starting with %q", err, "acme: ")
	}
	if _, ok, err := repo.LoadAccount(t.Context(), srv.URL+"/directory"); err != nil || ok {
		t.Errorf("LoadAccount = ok %v, err %v; want no account row", ok, err)
	}
}

// fakeACME is just enough of an ACME server for lego to register an account:
// a directory, a nonce and newAccount. Every other path answers 404, so the
// order that follows registration fails. onNewAccount runs inside the
// newAccount handler, before the server answers.
func fakeACME(t *testing.T, onNewAccount func()) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /directory", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"newNonce": %[1]q, "newAccount": %[2]q, "newOrder": %[3]q}`,
			srv.URL+"/nonce", srv.URL+"/account", srv.URL+"/order")
	})
	mux.HandleFunc("HEAD /nonce", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Replay-Nonce", "nonce-1")
	})
	mux.HandleFunc("POST /account", func(w http.ResponseWriter, _ *http.Request) {
		onNewAccount()
		w.Header().Set("Replay-Nonce", "nonce-2")
		w.Header().Set("Location", srv.URL+"/acct/1")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"status": "valid"}`)
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	trustServer(t, srv)
	return srv
}

// trustServer makes lego trust srv's certificate: lego insists on HTTPS and
// reads extra root CAs from LEGO_CA_CERTIFICATES when it builds its client.
func trustServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}
	t.Setenv("LEGO_CA_CERTIFICATES", path)
}

// TestLegoIssuer_AbandonedObtainStillStoresAccount cancels the context while
// the server is registering the account, as Manager does when it abandons an
// Obtain at shutdown. The account must still be stored, or the next start
// registers a second one.
func TestLegoIssuer_AbandonedObtainStillStoresAccount(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	srv := fakeACME(t, cancel)
	iss, repo := newLego(t, srv.URL+"/directory", mustKey(t, fixtureKey))

	if _, err := iss.Obtain(ctx, domain); err == nil {
		t.Fatal("Obtain error = nil, want the failed order")
	}
	row, ok, err := repo.LoadAccount(t.Context(), srv.URL+"/directory")
	if err != nil || !ok {
		t.Fatalf("LoadAccount = ok %v, err %v; want the registered account stored", ok, err)
	}
	if !strings.Contains(string(row.Registration), srv.URL+"/acct/1") {
		t.Errorf("Registration = %s, want the account URI %s/acct/1", row.Registration, srv.URL)
	}
	if err := iss.Load(t.Context()); err != nil {
		t.Errorf("Load of the stored account = %v, want nil", err)
	}
}
