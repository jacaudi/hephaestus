package secrets

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/1password/onepassword-sdk-go"

	"go.jacaudi.dev/hephaestus/internal/secret"
)

// sdkResponse is the SDK's per-reference answer.
type sdkResponse = onepassword.Response[onepassword.ResolvedReference, onepassword.ResolveReferenceError]

// content is a successful per-reference answer.
func content(value string) sdkResponse {
	return sdkResponse{Content: &onepassword.ResolvedReference{Secret: value}}
}

// refusal is a failed per-reference answer of the given SDK error type.
func refusal(kind onepassword.ResolveReferenceErrorTypes) sdkResponse {
	return sdkResponse{Error: &onepassword.ResolveReferenceError{Type: kind}}
}

// answering returns a resolve seam that records the references it receives and answers with resp and err.
func answering(got *[]string, resp map[string]sdkResponse, err error) func(context.Context, []string) (onepassword.ResolveAllResponse, error) {
	return func(_ context.Context, opRefs []string) (onepassword.ResolveAllResponse, error) {
		*got = slices.Clone(opRefs)

		return onepassword.ResolveAllResponse{IndividualResponses: resp}, err
	}
}

// resolverWith builds a resolver whose resolve seam is fn.
func resolverWith(fn func(context.Context, []string) (onepassword.ResolveAllResponse, error)) *OnePassword {
	op := NewOnePassword(secret.Secret("ops_test"), "test")
	op.resolve = fn

	return op
}

func TestOnePassword_StripsRefPrefix(t *testing.T) {
	var got []string
	op := resolverWith(answering(&got, map[string]sdkResponse{"op://v/i/a": content("1")}, nil))

	res, err := op.ResolveAll(t.Context(), []string{"ref+op://v/i/a"})
	if err != nil {
		t.Fatalf("ResolveAll: %v", err)
	}
	if want := []string{"op://v/i/a"}; !slices.Equal(got, want) {
		t.Errorf("SDK received %v, want %v", got, want)
	}
	if _, ok := res["ref+op://v/i/a"]; !ok || len(res) != 1 {
		t.Errorf("result keys = %v, want exactly ref+op://v/i/a", res)
	}
}

func TestOnePassword_PerReferenceResults(t *testing.T) {
	var got []string
	op := resolverWith(answering(&got, map[string]sdkResponse{
		"op://v/i/a": content("1"),
		"op://v/i/b": refusal(onepassword.ResolveReferenceErrorTypeVariantFieldNotFound),
	}, nil))

	res, err := op.ResolveAll(t.Context(), []string{"ref+op://v/i/a", "ref+op://v/i/b"})
	if err != nil {
		t.Fatalf("ResolveAll: %v", err)
	}
	if res["ref+op://v/i/a"].Err != nil || res["ref+op://v/i/a"].Value.Expose() != "1" {
		t.Errorf("a = %+v, want the value 1 and no error", res["ref+op://v/i/a"])
	}
	refErr, ok := errors.AsType[*ReferenceError](res["ref+op://v/i/b"].Err)
	if !ok {
		t.Fatalf("b error = %v, want a *ReferenceError", res["ref+op://v/i/b"].Err)
	}
	if want := (ReferenceError{Ref: "ref+op://v/i/b", Kind: "fieldNotFound"}); *refErr != want {
		t.Errorf("b error = %+v, want %+v", *refErr, want)
	}
}

func TestOnePassword_MissingFromResponse(t *testing.T) {
	var got []string
	op := resolverWith(answering(&got, map[string]sdkResponse{"op://v/i/a": content("1")}, nil))

	res, err := op.ResolveAll(t.Context(), []string{"ref+op://v/i/a", "ref+op://v/i/gone"})
	if err != nil {
		t.Fatalf("ResolveAll: %v", err)
	}
	refErr, ok := errors.AsType[*ReferenceError](res["ref+op://v/i/gone"].Err)
	if !ok {
		t.Fatalf("gone error = %v, want a *ReferenceError", res["ref+op://v/i/gone"].Err)
	}
	if want := (ReferenceError{Ref: "ref+op://v/i/gone", Kind: "missing"}); *refErr != want {
		t.Errorf("gone error = %+v, want %+v", *refErr, want)
	}
}

func TestOnePassword_WholeCallError(t *testing.T) {
	var got []string
	cause := errors.New("transport: connection refused")
	op := resolverWith(answering(&got, nil, cause))

	res, err := op.ResolveAll(t.Context(), []string{"ref+op://v/i/a"})
	whole, ok := errors.AsType[*WholeCallError](err)
	if !ok {
		t.Fatalf("error = %v, want a *WholeCallError", err)
	}
	if whole.Panicked {
		t.Error("Panicked = true for a transport error")
	}
	if !errors.Is(err, cause) {
		t.Error("the cause is not reachable with errors.Is")
	}
	if res != nil {
		t.Errorf("result = %v, want nil on a whole-call error", res)
	}
}

func TestOnePassword_WholeCallErrorHidesCause(t *testing.T) {
	var got []string
	op := resolverWith(answering(&got, nil, errors.New("sdk says: hunter2")))

	_, err := op.ResolveAll(t.Context(), []string{"ref+op://v/i/a"})
	if err == nil {
		t.Fatal("ResolveAll: no error")
	}
	if text := err.Error(); text == "" || strings.Contains(text, "hunter2") || strings.Contains(text, "sdk says") {
		t.Errorf("error text %q carries the SDK's text", text)
	}
}

func TestOnePassword_PanicText(t *testing.T) {
	var got []string
	op := resolverWith(answering(&got, nil, errors.New("error invoking: wasm error (recovered by wazero) unreachable")))

	_, err := op.ResolveAll(t.Context(), []string{"ref+op://v/i/a"})
	whole, ok := errors.AsType[*WholeCallError](err)
	if !ok {
		t.Fatalf("error = %v, want a *WholeCallError", err)
	}
	if !whole.Panicked {
		t.Error("Panicked = false for a recovered-by-wazero error")
	}
}

func TestOnePassword_RateLimited(t *testing.T) {
	var got []string
	op := resolverWith(answering(&got, nil, &onepassword.RateLimitExceededError{}))

	_, err := op.ResolveAll(t.Context(), []string{"ref+op://v/i/a"})
	if !errors.Is(err, ErrRateLimited) {
		t.Errorf("error = %v, want one that matches ErrRateLimited", err)
	}
	if _, ok := errors.AsType[*WholeCallError](err); !ok {
		t.Errorf("error = %v, want one that also matches *WholeCallError", err)
	}
}

// fakeSecrets answers every ResolveAll with a fixed response; Resolve is never called.
type fakeSecrets struct {
	onepassword.SecretsAPI
}

func (fakeSecrets) ResolveAll(context.Context, []string) (onepassword.ResolveAllResponse, error) {
	return onepassword.ResolveAllResponse{IndividualResponses: map[string]sdkResponse{"op://v/i/a": content("1")}}, nil
}

func TestOnePassword_LazyClientRetried(t *testing.T) {
	calls := 0
	op := NewOnePassword(secret.Secret("ops_test"), "test")
	op.newClient = func(context.Context) (*onepassword.Client, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("client creation failed")
		}

		return &onepassword.Client{SecretsAPI: fakeSecrets{}}, nil
	}

	_, err := op.ResolveAll(t.Context(), []string{"ref+op://v/i/a"})
	if _, ok := errors.AsType[*WholeCallError](err); !ok {
		t.Fatalf("first call error = %v, want a *WholeCallError", err)
	}
	for range 2 {
		res, err := op.ResolveAll(t.Context(), []string{"ref+op://v/i/a"})
		if err != nil {
			t.Fatalf("later call: %v", err)
		}
		if res["ref+op://v/i/a"].Value.Expose() != "1" {
			t.Errorf("later call result = %+v, want the value 1", res["ref+op://v/i/a"])
		}
	}
	if calls != 2 {
		t.Errorf("newClient called %d times, want 2 (one failure, one success, then reuse)", calls)
	}
}
