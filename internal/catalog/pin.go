package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// pinIDHeader opens the text a pin id hashes. Changing the format changes
// every pin id, so it is versioned.
const pinIDHeader = "hephaestus-pin-v1\n"

// pinIDLen is the number of hex characters kept from the hash.
const pinIDLen = 12

// PinID returns the pin id of objects: the first 12 hex characters of the
// sha256 of pinIDHeader, then, per object in order, the line
// "object <url> <size> <sha256>" followed by "file <path> <size> <sha256>"
// per file in order. A signer and the EFI-stub finding are not part of it.
func PinID(objects []ObjectPin) string {
	var b strings.Builder
	b.WriteString(pinIDHeader)
	for _, o := range objects {
		b.WriteString("object " + o.URL + " " + strconv.FormatInt(o.Size, 10) + " " + o.SHA256 + "\n")
		for _, f := range o.Files {
			b.WriteString("file " + f.Path + " " + strconv.FormatInt(f.Size, 10) + " " + f.SHA256 + "\n")
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:pinIDLen]
}
