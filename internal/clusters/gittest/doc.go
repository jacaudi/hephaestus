// Package gittest serves git repositories over TLS 1.3 smart HTTP for tests,
// with the host git's http-backend, and makes commits in them. Handler is the
// same server side without a test, for programs that stand in for a git host.
// It is for tests only.
package gittest
