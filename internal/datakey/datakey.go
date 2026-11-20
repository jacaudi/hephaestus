package datakey

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"go.jacaudi.dev/hephaestus/internal/secret"
)

// Size is the data key length in bytes (AES-256).
const Size = 32

// ErrDecrypt is returned by Open for every failure, so a caller cannot tell a
// wrong key from tampered or truncated data.
var ErrDecrypt = errors.New("datakey: decrypt failed: wrong data key or corrupted data")

// Key seals and opens values with AES-256-GCM under a random nonce. It
// redacts itself in fmt and slog output.
type Key struct {
	aead cipher.AEAD
}

// Parse builds a Key from a base64 (standard encoding) string of Size bytes.
// Surrounding whitespace is ignored.
func Parse(encoded secret.Secret) (*Key, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded.Expose()))
	if err != nil {
		return nil, fmt.Errorf("data key: not valid base64: %w", err)
	}
	if len(raw) != Size {
		return nil, fmt.Errorf("data key: decoded %d bytes, want %d (generate one with: openssl rand -base64 32)", len(raw), Size)
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("data key: build cipher: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, fmt.Errorf("data key: build AES-GCM: %w", err)
	}
	return &Key{aead: aead}, nil
}

// Seal encrypts plaintext and authenticates aad. The result is the random
// nonce, the ciphertext and the tag; it is 28 bytes longer than plaintext.
func (k *Key) Seal(plaintext, aad []byte) []byte {
	// The nonce argument must be nil: NewGCMWithRandomNonce generates a random one and prepends it.
	return k.aead.Seal(nil, nil, plaintext, aad) //nolint:gosec // G407: nil nonce is required, the cipher picks a random one
}

// Open reverses Seal. It returns ErrDecrypt if the key, the aad or the sealed
// bytes do not match.
func (k *Key) Open(sealed, aad []byte) ([]byte, error) {
	plaintext, err := k.aead.Open(nil, nil, sealed, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// String redacts the key for %s and %v.
func (k *Key) String() string { return "****" }

// LogValue redacts the key in slog output.
func (k *Key) LogValue() slog.Value { return slog.StringValue("****") }

// FormatV1 is the first byte of every sealed field: the format of what follows.
// A later format takes a new value, and OpenField refuses any it does not know.
const FormatV1 byte = 0x01

// Field names the stored value a sealed field belongs to. Binding it into the
// additional data makes a sealed value open only at its own table, row and
// column, so a value copied to another row fails to open.
type Field struct{ Table, Row, Column string }

// aad is the additional data of f: the format byte and "<table>/<row>/<column>".
func (f Field) aad() []byte {
	return fmt.Appendf([]byte{FormatV1}, "%s/%s/%s", f.Table, f.Row, f.Column)
}

// SealField seals plaintext for f as FormatV1 followed by Seal's output, with
// the format byte and the field's name as additional data.
func (k *Key) SealField(f Field, plaintext []byte) []byte {
	return append([]byte{FormatV1}, k.Seal(plaintext, f.aad())...)
}

// OpenField reverses SealField. It returns ErrDecrypt for a wrong key, a
// different field, an unknown or missing format byte and truncated data.
func (k *Key) OpenField(f Field, sealed []byte) ([]byte, error) {
	if len(sealed) == 0 || sealed[0] != FormatV1 {
		return nil, ErrDecrypt
	}
	return k.Open(sealed[1:], f.aad())
}
