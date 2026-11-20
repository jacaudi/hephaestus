// hephaestus: new

package schematic

import (
	"bytes"
	"testing"
)

// emptyID is the image-factory id of a schematic with no customization.
const emptyID = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"

func TestCanonical_EmptySchematic(t *testing.T) {
	body := []byte("customization: {}\n")

	got, err := Canonical(body)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}

	if want := "customization: {}\n"; string(got) != want {
		t.Errorf("Canonical = %q, want %q", got, want)
	}

	id, err := ID(body)
	if err != nil {
		t.Fatalf("ID: %v", err)
	}

	if id != emptyID {
		t.Errorf("ID = %s, want %s", id, emptyID)
	}
}

func TestCanonical_ReordersAndStrips(t *testing.T) {
	body := []byte(`# the comment
customization:
  systemExtensions:
    officialExtensions:
      - siderolabs/iscsi-tools
  extraKernelArgs:
    - net.ifnames=0
`)

	s, err := Unmarshal(body)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	want, err := s.ID()
	if err != nil {
		t.Fatalf("(*Schematic).ID: %v", err)
	}

	got, err := ID(body)
	if err != nil {
		t.Fatalf("ID: %v", err)
	}

	if got != want {
		t.Errorf("ID = %s, want %s", got, want)
	}

	canonical, err := Canonical(body)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}

	if bytes.Contains(canonical, []byte("comment")) {
		t.Errorf("Canonical = %q, want no comment", canonical)
	}

	args, ext := bytes.Index(canonical, []byte("extraKernelArgs")), bytes.Index(canonical, []byte("systemExtensions"))
	if args < 0 || ext < 0 || args > ext {
		t.Errorf("Canonical = %q, want extraKernelArgs before systemExtensions", canonical)
	}
}

func TestCanonical_UnknownFieldRefused(t *testing.T) {
	body := []byte("customization: {bogus: 1}")

	if _, err := Canonical(body); err == nil {
		t.Error("Canonical accepted an unknown field, want an error")
	}

	if _, err := ID(body); err == nil {
		t.Error("ID accepted an unknown field, want an error")
	}
}
