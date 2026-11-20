// Package secrets resolves 1Password references for hephaestus and counts the requests it sends to 1Password.
//
// OnePassword resolves a batch of ref+op:// references in one SDK call and answers per reference: a value, or a
// *ReferenceError that carries the reference and the SDK's error class, never a value. A failure of the call as a whole
// (transport, token, client creation) is a *WholeCallError; a rate limit also matches ErrRateLimited. The text of a
// *WholeCallError is fixed and never quotes the SDK's error; the cause stays reachable through errors.Is and errors.As for
// a caller that has decided it is safe to read.
//
// Counter is an http.RoundTripper that counts requests to 1Password hosts over a rolling 24 hours, so the operator can see
// how close the service account is to its hourly and daily limits.
package secrets
