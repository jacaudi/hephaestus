package catalogtest_test

import (
	"archive/zip"
	"bytes"
	"io"
	"maps"
	"path/filepath"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/catalog/catalogtest"
	"go.jacaudi.dev/hephaestus/internal/store"
)

const baseURL = "https://upstream.example.test"

func TestNew_Deterministic(t *testing.T) {
	a, b := catalogtest.New(t, baseURL), catalogtest.New(t, baseURL)
	if len(a.Objects) != 3 {
		t.Fatalf("Objects holds %d zips, want 3", len(a.Objects))
	}
	for url, want := range a.Objects {
		if !bytes.Equal(b.Objects[url], want) {
			t.Errorf("%s differs between two builds", url)
		}
	}
	for k := range a.Files {
		e, _ := a.Catalog.Lookup(k.Entry, k.Arch)
		e2, _ := b.Catalog.Lookup(k.Entry, k.Arch)
		if e.Pin.ID != e2.Pin.ID {
			t.Errorf("%v: pin id differs between two builds", k)
		}
	}
}

func TestNew_ZipHoldsTheFiles(t *testing.T) {
	f := catalogtest.New(t, baseURL)
	zr, err := zip.NewReader(bytes.NewReader(f.Objects[baseURL+"/tshell.zip"]), int64(len(f.Objects[baseURL+"/tshell.zip"])))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range zr.File {
		rc, err := m.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		if err := rc.Close(); err != nil {
			t.Fatal(err)
		}
		got[m.Name] = string(body)
	}
	want := map[string]string{"EFI/Shell/X64/Shell.efi": "tshell-x64", "EFI/Shell/AArch64/Shell.efi": "tshell-aa64"}
	if !maps.Equal(got, want) {
		t.Errorf("tshell.zip holds %v, want %v", got, want)
	}

	squash := f.Files[catalog.Key{Entry: "tlive", Arch: "arm64"}]["live/filesystem.squashfs"]
	if len(squash) != 4<<20 || squash[251] != 0 || squash[250] != 250 {
		t.Errorf("squashfs is %d bytes, [250]=%d [251]=%d", len(squash), squash[250], squash[251])
	}
}

func TestStock_TagsEveryFile(t *testing.T) {
	st, _, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "store"), func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatal(err)
	}
	f := catalogtest.New(t, baseURL)
	key := catalog.Key{Entry: "tlive", Arch: "amd64"}
	catalogtest.Stock(t, st, f, key)

	e, _ := f.Catalog.Lookup(key.Entry, key.Arch)
	layers, err := st.Layers(t.Context(), e.Tag())
	if err != nil {
		t.Fatalf("Layers(%s): %v", e.Tag(), err)
	}
	var titles []string
	for _, l := range layers {
		titles = append(titles, l.Annotations[ocispec.AnnotationTitle])
	}
	want := []string{"live/vmlinuz", "live/initrd.img", "live/filesystem.squashfs"}
	if len(titles) != len(want) {
		t.Fatalf("layers = %v, want %v", titles, want)
	}
	for i := range want {
		if titles[i] != want[i] {
			t.Errorf("layer %d = %q, want %q", i, titles[i], want[i])
		}
	}
	if err := st.CheckLayers(t.Context(), e.Tag()); err != nil {
		t.Errorf("CheckLayers: %v", err)
	}
}
