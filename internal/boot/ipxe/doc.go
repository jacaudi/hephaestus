// Package ipxe embeds the iPXE binaries hephaestus hands to PXE clients and
// names the files the proxy serves them under. The binaries are built by
// build/ipxe (a pinned iPXE commit, the ISRG roots as the only trust anchors,
// and the chain script of design section 5.10 compiled in); run "task
// ipxe:build" to rebuild them. bin/BUILDINFO records the inputs.
package ipxe
