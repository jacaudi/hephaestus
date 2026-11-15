package datakey

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.jacaudi.dev/hephaestus/internal/secret"
)

// fixtureKey is the shared data key fixture: bytes 0x00 to 0x1f, base64.
const fixtureKey = secret.Secret("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")

func mustParse(t *testing.T, encoded secret.Secret) *Key {
	t.Helper()

	k, err := Parse(encoded)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return k
}

func TestSealOpen_RoundTrip(t *testing.T) {
	k := mustParse(t, fixtureKey)
	plaintext := []byte("acme key")

	sealed := k.Seal(plaintext, []byte("aad-1"))
	if got, want := len(sealed), len(plaintext)+28; got != want {
		t.Errorf("len(sealed) = %d, want %d", got, want)
	}

	got, err := k.Open(sealed, []byte("aad-1"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("Open = %q, want %q", got, plaintext)
	}
}

func TestSeal_RandomNonce(t *testing.T) {
	k := mustParse(t, fixtureKey)

	a := k.Seal([]byte("same"), nil)
	b := k.Seal([]byte("same"), nil)
	if bytes.Equal(a, b) {
		t.Error("two Seals of the same plaintext are identical")
	}
}

func TestOpen_Rejects(t *testing.T) {
	k := mustParse(t, fixtureKey)
	sealed := k.Seal([]byte("acme key"), []byte("aad-1"))

	flipped := bytes.Clone(sealed)
	flipped[len(flipped)-1] ^= 0x01

	otherKey := mustParse(t, secret.Secret(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, Size))))

	tests := []struct {
		name   string
		key    *Key
		sealed []byte
		aad    []byte
	}{
		{"wrong aad", k, sealed, []byte("aad-2")},
		{"flipped byte", k, flipped, []byte("aad-1")},
		{"different key", otherKey, sealed, []byte("aad-1")},
		{"truncated", k, sealed[:10], []byte("aad-1")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.key.Open(tt.sealed, tt.aad)
			if !errors.Is(err, ErrDecrypt) {
				t.Errorf("Open error = %v, want ErrDecrypt", err)
			}
			if got != nil {
				t.Errorf("Open = %q, want nil", got)
			}
		})
	}
}

func TestParse_Rejects(t *testing.T) {
	tests := []struct {
		name    string
		encoded secret.Secret
		want    []string
	}{
		{"empty", "", []string{"not valid base64", "decoded 0 bytes"}},
		{"not base64", "not-base64!", []string{"not valid base64", "decoded 0 bytes"}},
		{
			"too short", "AAECAwQFBgcICQoLDA0ODw==",
			[]string{"decoded 16 bytes, want 32 (generate one with: openssl rand -base64 32)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, err := Parse(tt.encoded)
			if err == nil {
				t.Fatalf("Parse(%q) error = nil, want error", tt.encoded.Expose())
			}
			if k != nil {
				t.Errorf("Parse key = %v, want nil", k)
			}
			if !slices.ContainsFunc(tt.want, func(w string) bool { return strings.Contains(err.Error(), w) }) {
				t.Errorf("Parse error = %q, want one of %q", err, tt.want)
			}
			if strings.Contains(err.Error(), tt.encoded.Expose()) && tt.encoded != "" {
				t.Errorf("Parse error %q echoes the key material", err)
			}
		})
	}
}

func TestParse_TrimsWhitespace(t *testing.T) {
	if _, err := Parse(fixtureKey + "\n"); err != nil {
		t.Errorf("Parse with trailing newline: %v", err)
	}
}

func TestKey_Redacts(t *testing.T) {
	k := mustParse(t, fixtureKey)

	for _, verb := range []string{"%s", "%v"} {
		if got := fmt.Sprintf(verb, k); got != "****" {
			t.Errorf("Sprintf(%q) = %q, want %q", verb, got, "****")
		}
	}
	if got := k.LogValue().String(); got != "****" {
		t.Errorf("LogValue() = %q, want %q", got, "****")
	}
}
