package secrets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/1password/onepassword-sdk-go"

	"go.jacaudi.dev/hephaestus/internal/secret"
)

// refPrefix is what separates a hephaestus reference (ref+op://...) from the 1Password reference (op://...) the SDK reads.
const refPrefix = "ref+"

// panicMarker is the text the SDK's WebAssembly runtime puts in an error when it recovered from a panic in the core.
const panicMarker = "recovered by wazero"

// ErrRateLimited means 1Password refused a call because the service account reached its rate limit.
var ErrRateLimited = errors.New("secrets: 1Password rate limit reached")

// Result is the outcome for one reference: a value, or the reason there is none.
type Result struct {
	Value secret.Secret
	Err   error
}

// ReferenceError is the failure of one reference while the call as a whole succeeded. Kind is the SDK's error type, or
// "missing" when the response did not mention the reference. It never holds a value.
type ReferenceError struct {
	Ref  string
	Kind string
}

// Error names the reference and the kind of failure.
func (e *ReferenceError) Error() string {
	return "secrets: reference " + e.Ref + " not resolved: " + e.Kind
}

// WholeCallError is a failure of the call as a whole: transport, token, client creation. Panicked reports that the SDK's
// core panicked and its runtime recovered. Error does not quote the cause, which comes from a third party and may echo
// what it was asked; Unwrap returns it for a caller that decides it is safe to read.
type WholeCallError struct {
	Err      error
	Panicked bool
}

// Error reports a whole-call failure without the cause's text.
func (e *WholeCallError) Error() string {
	if e.Panicked {
		return "secrets: 1Password call failed: the SDK core panicked"
	}

	return "secrets: 1Password call failed"
}

// Unwrap returns the cause.
func (e *WholeCallError) Unwrap() error { return e.Err }

// OnePassword resolves references through the 1Password SDK, authenticated by a service account token. The SDK client
// is created on the first call that needs it and reused after.
type OnePassword struct {
	token   secret.Secret
	version string

	mu     sync.Mutex
	client *onepassword.Client

	// resolve and newClient are the seams a test replaces; nil means the SDK.
	resolve   func(ctx context.Context, opRefs []string) (onepassword.ResolveAllResponse, error)
	newClient func(ctx context.Context) (*onepassword.Client, error)
}

// NewOnePassword returns a resolver for the service account token. version is the integration version reported to
// 1Password. It creates no client and makes no call.
func NewOnePassword(token secret.Secret, version string) *OnePassword {
	return &OnePassword{token: token, version: version}
}

// ResolveAll resolves every reference in one SDK call. Each ref is a ref+op://... string; the result is keyed by it. A
// failure of the call as a whole is a *WholeCallError, which also matches ErrRateLimited when 1Password limited the
// call. A single reference is resolved as ResolveAll(ctx, []string{ref}). The caller sets the deadline.
func (o *OnePassword) ResolveAll(ctx context.Context, refs []string) (map[string]Result, error) {
	opRefs := make([]string, len(refs))
	for i, ref := range refs {
		opRefs[i] = strings.TrimPrefix(ref, refPrefix)
	}

	resp, err := o.call(ctx, opRefs)
	if err != nil {
		return nil, wholeCall(err)
	}

	results := make(map[string]Result, len(refs))
	for i, ref := range refs {
		r := resp.IndividualResponses[opRefs[i]]
		switch {
		case r.Content != nil:
			results[ref] = Result{Value: secret.Secret(r.Content.Secret)}
		case r.Error != nil:
			results[ref] = Result{Err: &ReferenceError{Ref: ref, Kind: string(r.Error.Type)}}
		default:
			results[ref] = Result{Err: &ReferenceError{Ref: ref, Kind: "missing"}}
		}
	}

	return results, nil
}

// call sends the 1Password references through the resolve seam, or through the SDK client.
func (o *OnePassword) call(ctx context.Context, opRefs []string) (onepassword.ResolveAllResponse, error) {
	if o.resolve != nil {
		return o.resolve(ctx, opRefs)
	}
	client, err := o.clientFor(ctx)
	if err != nil {
		return onepassword.ResolveAllResponse{}, err
	}

	return client.Secrets().ResolveAll(ctx, opRefs)
}

// clientFor returns the SDK client, creating it under the mutex on the first call. A creation failure is not kept: the
// next call tries again.
func (o *OnePassword) clientFor(ctx context.Context) (*onepassword.Client, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.client != nil {
		return o.client, nil
	}
	create := o.newClient
	if create == nil {
		create = o.createClient
	}
	client, err := create(ctx)
	if err != nil {
		return nil, err
	}
	o.client = client

	return client, nil
}

// createClient creates the SDK client for the service account token.
func (o *OnePassword) createClient(ctx context.Context) (*onepassword.Client, error) {
	return onepassword.NewClient(ctx,
		onepassword.WithServiceAccountToken(o.token.Expose()),
		onepassword.WithIntegrationInfo("hephaestus", o.version),
	)
}

// wholeCall classifies a failure of the call as a whole. A rate limit also matches ErrRateLimited.
func wholeCall(err error) error {
	whole := &WholeCallError{Err: err, Panicked: strings.Contains(err.Error(), panicMarker)}
	if _, limited := errors.AsType[*onepassword.RateLimitExceededError](err); limited { //nolint:errcheck // the match target is not a dropped error; modernize requires AsType over errors.As
		return fmt.Errorf("%w: %w", ErrRateLimited, whole)
	}

	return whole
}
