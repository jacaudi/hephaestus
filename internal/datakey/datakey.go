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
