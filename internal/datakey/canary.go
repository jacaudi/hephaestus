package datakey

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

// ErrCanary is returned by EnsureCanary when the stored canary does not open
// under the key: the key is not the one the data was sealed with, or the
// canary row is damaged.
var ErrCanary = errors.New("datakey: the data key does not open the stored canary")

// CanarySetting is the settings key that holds the sealed canary.
const CanarySetting = "data_key_canary"

// canaryText is what the canary seals.
const canaryText = "hephaestus-canary-v1"

// canaryField is where the canary is stored.
func canaryField() Field { return Field{Table: "settings", Row: CanarySetting, Column: "value"} }

// EnsureCanary writes the sealed canary to the settings table when none is
// stored, then opens the stored one with k. It returns ErrCanary when that
// fails, which tells a wrong key from a damaged data row: any other sealed
// value that fails to open is not a startup failure.
func EnsureCanary(ctx context.Context, q bun.IDB, k *Key) error {
	sealed := base64.StdEncoding.EncodeToString(k.SealField(canaryField(), []byte(canaryText)))
	// Two processes may start together: the loser's insert is a no-op and it
	// checks the winner's row.
	if _, err := q.NewRaw(
		"INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO NOTHING", CanarySetting, sealed,
	).Exec(ctx); err != nil {
		return fmt.Errorf("write the data key canary: %w", err)
	}

	var stored string
	if err := q.NewRaw("SELECT value FROM settings WHERE key = ?", CanarySetting).Scan(ctx, &stored); err != nil {
		return fmt.Errorf("read the data key canary: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(stored)
	if err != nil {
		return fmt.Errorf("%w: stored value is not base64", ErrCanary)
	}
	if _, err := k.OpenField(canaryField(), raw); err != nil {
		return ErrCanary
	}
	return nil
}
