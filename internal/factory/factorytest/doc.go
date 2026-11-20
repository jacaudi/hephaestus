// Package factorytest is a fake Talos image factory for tests: it redirects an
// asset request to a short-lived asset URL, honours Range and ignores If-Range,
// serves a strong ETag, and registers schematics, as the public factory does.
// Serve it with httptest over TLS 1.3. It is for tests only.
package factorytest
