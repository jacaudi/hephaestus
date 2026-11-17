package toolcache

// IntegrityError means an upstream served something it must not have: a
// redirect off https, bytes that fail their hash, a Content-Range that
// disagrees with the request. The fetcher marks an entry broken on one, and
// retries any other failure.
type IntegrityError struct {
	// Err is the specific refusal.
	Err error
}

// Error implements error.
func (e *IntegrityError) Error() string {
	if e.Err == nil {
		return "toolcache: integrity failure"
	}
	return "toolcache: integrity failure: " + e.Err.Error()
}

// Unwrap returns the specific refusal, so errors.Is and errors.AsType see it.
func (e *IntegrityError) Unwrap() error { return e.Err }
