// Package toolcache holds the utility-boot file cache. It opens one member of
// an upstream object (a zip, an ISO9660 image or a raw file) without trusting
// the object's bytes, and its fetcher downloads the objects of wanted cache
// rows, and of failed rows that are due again. The design calls it the utility
// package; the name is toolcache because the revive linter bans "utility".
//
// Extraction lives in extract.go and the go-diskfs storage adapter in
// isostorage.go. The upstream client is in httpclient.go, the spool file names,
// ETag test and Content-Range parser in spool.go, and the error types in
// errors.go. The fetcher, which downloads into a spool, verifies against the
// catalog's pins and settles each row, is in fetcher.go.
package toolcache
