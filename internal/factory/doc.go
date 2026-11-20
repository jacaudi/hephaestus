// Package factory is the client of the Talos image factory: it registers a
// schematic body for its ID and names the URL of each asset (kernel, initramfs
// and kernel command line) of a schematic, Talos version and architecture. It
// downloads nothing; the toolcache package fetches the URLs it names. Package
// factorytest is the fake factory the tests of both packages serve.
package factory
