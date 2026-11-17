// Package toolcache holds the utility-boot file cache: it opens one member of
// an upstream object (a zip, an ISO9660 image or a raw file) without trusting
// the object's bytes. The design calls it the utility package; the name is
// toolcache because the revive linter bans "utility". Extraction lives in
// extract.go and the go-diskfs storage adapter in isostorage.go.
package toolcache
