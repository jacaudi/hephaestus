package secret

import (
	"fmt"
	"io"
	"log/slog"
)

// Secret holds a sensitive string and redacts it in fmt, slog, JSON and text output.
type Secret string

// String redacts the value for %s and %v.
func (s Secret) String() string { return "****" }

// Format redacts the value for every fmt verb except %p, including %#v, %d
// and %x. fmt handles %p before calling Format and prints the raw value.
func (s Secret) Format(f fmt.State, _ rune) {
	if _, err := io.WriteString(f, "****"); err != nil {
		return // fmt.State offers no way to report a write error
	}
}

// LogValue redacts the value in slog output.
func (s Secret) LogValue() slog.Value { return slog.StringValue("****") }

// MarshalJSON redacts the value in JSON.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"****"`), nil }

// MarshalText redacts the value in text encodings.
func (s Secret) MarshalText() ([]byte, error) { return []byte("****"), nil }

// Expose returns the real value. Call it only where the secret is consumed
// (an API client, a TLS config); the name keeps those call sites grep-able.
func (s Secret) Expose() string { return string(s) }
