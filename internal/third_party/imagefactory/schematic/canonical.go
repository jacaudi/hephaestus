// hephaestus: new

package schematic

import (
	"crypto/sha256"
	"encoding/hex"
)

// Canonical returns the bytes image-factory hashes to name the schematic in body: body is read with Unmarshal
// (so an unknown field is an error), then written back with (*Schematic).Marshal, which orders the fields,
// drops comments and spells every value the one way.
func Canonical(body []byte) ([]byte, error) {
	s, err := Unmarshal(body)
	if err != nil {
		return nil, err
	}

	return s.Marshal()
}

// ID returns the id image-factory gives the schematic in body: the hex SHA-256 of Canonical(body), which equals
// the ID of the unmarshalled schematic.
func ID(body []byte) (string, error) {
	canonical, err := Canonical(body)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(canonical)

	return hex.EncodeToString(sum[:]), nil
}
