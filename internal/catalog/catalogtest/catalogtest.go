package catalogtest

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/store"
)

// squashfsSize is the size of the fixture live entries' second stage.
const squashfsSize = 4 << 20

// Fixture is a catalog of two fixture entries, "tshell" and "tlive", with the
// bytes behind it.
type Fixture struct {
	// Catalog holds every fixture entry, pinned.
	Catalog *catalog.Catalog
	// Objects maps each object URL to its zip bytes: what a fake upstream serves.
	Objects map[string][]byte
	// Files maps each key to its extracted files' contents by path.
	Files map[catalog.Key]map[string][]byte
}

// New builds the fixture. Every object URL is baseURL plus a file name, and
// every entry has version "1" and a TOFU signature. tshell is a chain entry
// for amd64 and arm64 that share one zip; tlive is a live-boot entry with one
// zip per arch, amd64 on efi and pcbios and arm64 on efi.
func New(tb testing.TB, baseURL string) Fixture {
	tb.Helper()
	f := Fixture{Objects: map[string][]byte{}, Files: map[catalog.Key]map[string][]byte{}}
	var specs []catalog.Spec
	pins := map[catalog.Key]catalog.Pin{}

	add := func(spec catalog.Spec, object []byte, files map[string][]byte) {
		tb.Helper()
		url := spec.Objects[0].URL
		f.Objects[url] = object
		f.Files[spec.Key()] = files
		specs = append(specs, spec)
		pins[spec.Key()] = pinOf(spec, object, files)
	}

	shell := zipOf(tb, []member{
		{"EFI/Shell/X64/Shell.efi", []byte("tshell-x64")},
		{"EFI/Shell/AArch64/Shell.efi", []byte("tshell-aa64")},
	})
	for _, c := range []struct{ arch, path, body string }{
		{"amd64", "EFI/Shell/X64/Shell.efi", "tshell-x64"},
		{"arm64", "EFI/Shell/AArch64/Shell.efi", "tshell-aa64"},
	} {
		add(catalog.Spec{
			Entry: "tshell", Arch: c.arch, Version: "1",
			Recipe:    catalog.RecipeChain,
			Platforms: []catalog.Platform{catalog.PlatformEFI},
			Objects: []catalog.ObjectSpec{{
				URL:       baseURL + "/tshell.zip",
				Container: catalog.ContainerZip,
				Signature: catalog.Signature{TOFU: true},
				Files:     []string{c.path},
			}},
			Boot: catalog.Boot{Kernel: c.path},
		}, shell, map[string][]byte{c.path: []byte(c.body)})
	}

	squashfs := make([]byte, squashfsSize)
	for i := range squashfs {
		squashfs[i] = byte(i % 251)
	}
	for _, c := range []struct {
		arch      string
		platforms []catalog.Platform
	}{
		{"amd64", []catalog.Platform{catalog.PlatformEFI, catalog.PlatformPCBIOS}},
		{"arm64", []catalog.Platform{catalog.PlatformEFI}},
	} {
		files := map[string][]byte{
			"live/vmlinuz":             []byte("tlive-kernel-" + c.arch),
			"live/initrd.img":          []byte("tlive-initrd-" + c.arch),
			"live/filesystem.squashfs": squashfs,
		}
		add(catalog.Spec{
			Entry: "tlive", Arch: c.arch, Version: "1",
			Recipe:    catalog.RecipeLiveBoot,
			Platforms: c.platforms,
			Objects: []catalog.ObjectSpec{{
				URL:       baseURL + "/tlive-" + c.arch + ".zip",
				Container: catalog.ContainerZip,
				Signature: catalog.Signature{TOFU: true},
				Files:     []string{"live/vmlinuz", "live/initrd.img", "live/filesystem.squashfs"},
			}},
			Boot: catalog.Boot{Kernel: "live/vmlinuz", Initrds: []string{"live/initrd.img"}, Second: "live/filesystem.squashfs"},
			Args: "boot=live",
		}, zipOf(tb, []member{
			{"live/vmlinuz", files["live/vmlinuz"]},
			{"live/initrd.img", files["live/initrd.img"]},
			{"live/filesystem.squashfs", squashfs},
		}), files)
	}

	c, err := catalog.New(specs, pins)
	if err != nil {
		tb.Fatalf("catalogtest: build catalog: %v", err)
	}
	f.Catalog = c
	return f
}

// Stock pushes every file of key's entry into st as a layer blob and tags the
// entry's Tag with catalog.ArtifactType, as a finished fetch would.
func Stock(tb testing.TB, st *store.Store, f Fixture, key catalog.Key) {
	tb.Helper()
	e, ok := f.Catalog.Lookup(key.Entry, key.Arch)
	if !ok {
		tb.Fatalf("catalogtest: %v is not in the fixture", key)
	}
	var layers []ocispec.Descriptor
	for _, o := range e.Pin.Objects {
		for _, fp := range o.Files {
			desc, err := store.Layer(fp.Path, fp.Size, fp.SHA256)
			if err != nil {
				tb.Fatalf("catalogtest: layer %s: %v", fp.Path, err)
			}
			if err := st.PushBlob(tb.Context(), desc, bytes.NewReader(f.Files[key][fp.Path])); err != nil {
				tb.Fatalf("catalogtest: push %s: %v", fp.Path, err)
			}
			layers = append(layers, desc)
		}
	}
	if _, err := st.TagArtifact(tb.Context(), e.Tag(), catalog.ArtifactType, layers); err != nil {
		tb.Fatalf("catalogtest: tag %s: %v", e.Tag(), err)
	}
}

// pinOf builds the pin of spec over its object bytes and extracted files.
func pinOf(spec catalog.Spec, object []byte, files map[string][]byte) catalog.Pin {
	o := spec.Objects[0]
	op := catalog.ObjectPin{URL: o.URL, Size: int64(len(object)), SHA256: sum(object)}
	for _, p := range o.Files {
		op.Files = append(op.Files, catalog.FilePin{Path: p, Size: int64(len(files[p])), SHA256: sum(files[p])})
	}
	objects := []catalog.ObjectPin{op}
	return catalog.Pin{ID: catalog.PinID(objects), Objects: objects}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// member is one file of a fixture zip.
type member struct {
	name string
	body []byte
}

// zipOf returns a zip of members, stored uncompressed with a fixed modification
// time, so the bytes are reproducible.
func zipOf(tb testing.TB, members []member) []byte {
	tb.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, m := range members {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: m.name, Method: zip.Store, Modified: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)})
		if err != nil {
			tb.Fatalf("catalogtest: zip %s: %v", m.name, err)
		}
		if _, err := w.Write(m.body); err != nil {
			tb.Fatalf("catalogtest: zip %s: %v", m.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		tb.Fatalf("catalogtest: close zip: %v", err)
	}
	return buf.Bytes()
}
