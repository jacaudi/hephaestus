// Command upstreams stands in, on the end-to-end network, for the two internet services the install phase reaches: the
// Talos image factory and the git host of a cluster definition. One TLS 1.3 listener serves three host names, each
// with its own certificate, picked by SNI:
//
//	factory.e2e.test         the fake image factory (internal/factory/factorytest): schematic registration and image
//	                         requests, which it answers with a 302 to the assets host, as the public factory does
//	assets.factory.e2e.test  the asset URLs those redirects name, with Range and a strong ETag
//	git.e2e.test             git smart HTTP (internal/clusters/gittest.Handler) over lab.git, a repository built at
//	                         start from the fixture tree with the host git
//
// It writes one JSON line per request (host, protocol, method, path, status) to stdout. It is configured by the
// environment only, and every variable is required: UPSTREAMS_ADDR (the listen address), UPSTREAMS_FACTORY_CERT_FILE
// and UPSTREAMS_FACTORY_KEY_FILE, UPSTREAMS_ASSETS_CERT_FILE and UPSTREAMS_ASSETS_KEY_FILE, UPSTREAMS_GIT_CERT_FILE and
// UPSTREAMS_GIT_KEY_FILE (each name's leaf), UPSTREAMS_FIXTURE_DIR (the tree committed to lab.git) and
// UPSTREAMS_WORK_DIR (a writable directory for the repository).
//
// Exit codes: 0 after a signal stopped it, 1 when it cannot serve, 2 when a variable is missing. test/e2e/run.sh runs
// it; it is never part of hephaestus.
//
// upstreams validate-events <schema> <events.jsonl> instead checks the harness's event record: every line validates
// against the JSON Schema (format asserted too) and its seq is its line number. It exits 0 when the record is valid, 1
// when it is not, printing every problem, and 2 on a usage error. run.sh runs it at the end of every run.
package main
