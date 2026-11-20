//go:build sdksmoke

package secrets

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/secret"
)

// TestOnePassword_SmokeDummyToken drives the real SDK, built without cgo, with a token no account issued. The SDK must
// refuse the call with an error, within a minute, and must not panic inside its WebAssembly core. It needs no network
// and no account.
func TestOnePassword_SmokeDummyToken(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	op := NewOnePassword(secret.Secret("ops_hephaestus-smoke-test"), "smoke")
	res, err := op.ResolveAll(ctx, []string{"ref+op://v/i/f"})

	whole, ok := errors.AsType[*WholeCallError](err)
	if !ok {
		t.Fatalf("ResolveAll = (%v, %v), want a *WholeCallError", res, err)
	}
	if whole.Panicked {
		t.Fatal("the SDK panicked inside its core (recovered by wazero)")
	}
}
