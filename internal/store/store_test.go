package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oras-project/oras-go/v3/content"
	"github.com/oras-project/oras-go/v3/errdef"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"go.jacaudi.dev/hephaestus/internal/admin"
)

const (
	artifactType = "application/vnd.hephaestus.utility.v1"
	utilityRef   = "utility/t-amd64:43a4f134ae5e"
)

// testNow is the injected clock: Open's move-aside name derives from it.
func testNow() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }

// writeFile writes a fixture file the store later reads.
func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// openAt opens the store at root and fails the test on any error.
func openAt(t *testing.T, root string) (*Store, string) {
	t.Helper()
	s, moved, err := Open(t.Context(), root, testNow)
	if err != nil {
		t.Fatalf("Open(%s) error = %v", root, err)
	}
	return s, moved
}

// newStore opens a fresh store at <tmp>/store, so its parent is private to the test.
func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "store")
	s, moved := openAt(t, root)
	if moved != "" {
		t.Fatalf("fresh Open moved = %q, want empty", moved)
	}
	return s, root
}

func blobFile(root string, d ocispec.Descriptor) string {
	return filepath.Join(root, "blobs", "sha256", d.Digest.Encoded())
}

// pushLayer pushes data as a layer titled title.
func pushLayer(t *testing.T, s *Store, title string, data []byte) ocispec.Descriptor {
	t.Helper()
	sum := sha256.Sum256(data)
	desc, err := Layer(title, int64(len(data)), hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatalf("Layer(%q) error = %v", title, err)
	}
	if err := s.PushBlob(t.Context(), desc, bytes.NewReader(data)); err != nil {
		t.Fatalf("PushBlob(%q) error = %v", title, err)
	}
	return desc
}

// tagLayers tags a manifest over layers as ref.
func tagLayers(t *testing.T, s *Store, ref string, layers ...ocispec.Descriptor) ocispec.Descriptor {
	t.Helper()
	m, err := s.TagArtifact(t.Context(), ref, artifactType, layers)
	if err != nil {
		t.Fatalf("TagArtifact(%q) error = %v", ref, err)
	}
	return m
}

// truncateBlob drops the last byte of a blob. Blobs are 0444, so the test
// makes the file writable first; production never chmods.
func truncateBlob(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	if err := os.Truncate(path, info.Size()-1); err != nil {
		t.Fatalf("truncate %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("restore mode of %s: %v", path, err)
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestLayer(t *testing.T) {
	good := sha256Hex([]byte("kernel"))
	tests := []struct {
		name    string
		size    int64
		hex     string
		wantErr bool
	}{
		{name: "valid", size: 6, hex: good},
		{name: "short digest", size: 6, hex: good[:10], wantErr: true},
		{name: "not hex", size: 6, hex: "zz" + good[2:], wantErr: true},
		{name: "negative size", size: -1, hex: good, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Layer("boot/vmlinuz", tt.size, tt.hex)

			if (err != nil) != tt.wantErr {
				t.Fatalf("Layer() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.MediaType != LayerMediaType || got.Size != tt.size ||
				got.Digest.String() != "sha256:"+good || got.Annotations[ocispec.AnnotationTitle] != "boot/vmlinuz" {
				t.Fatalf("Layer() = %+v, want %s, size %d, sha256:%s, title boot/vmlinuz", got, LayerMediaType, tt.size, good)
			}
		})
	}
}

func TestOpen_CreatesLayout(t *testing.T) {
	_, root := newStore(t)

	for _, name := range []string{"oci-layout", "index.json"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("stat %s: %v", name, err)
		}
	}
}

func TestPushTagLayers(t *testing.T) {
	s, _ := newStore(t)
	kernel := pushLayer(t, s, "boot/vmlinuz", []byte("kernel"))
	initrd := pushLayer(t, s, "boot/initrd.img", []byte("initrd"))

	m := tagLayers(t, s, utilityRef, kernel, initrd)

	got, err := s.Layers(t.Context(), utilityRef)
	if err != nil {
		t.Fatalf("Layers() error = %v", err)
	}
	var titles []string
	for _, l := range got {
		titles = append(titles, l.Annotations[ocispec.AnnotationTitle])
	}
	if want := []string{"boot/vmlinuz", "boot/initrd.img"}; !slices.Equal(titles, want) {
		t.Fatalf("layer titles = %v, want %v", titles, want)
	}
	resolved, err := s.Resolve(t.Context(), utilityRef)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved.Digest != m.Digest {
		t.Errorf("Resolve() digest = %s, want %s", resolved.Digest, m.Digest)
	}
	raw, err := content.FetchAll(t.Context(), s.oci, m)
	if err != nil {
		t.Fatalf("fetch manifest: %v", err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.ArtifactType != artifactType {
		t.Errorf("ArtifactType = %q, want %q", manifest.ArtifactType, artifactType)
	}
}

func TestPushBlob_DigestMismatch(t *testing.T) {
	s, root := newStore(t)
	desc, err := Layer("boot/vmlinuz", 6, sha256Hex([]byte("kernel")))
	if err != nil {
		t.Fatalf("Layer() error = %v", err)
	}

	err = s.PushBlob(t.Context(), desc, bytes.NewReader([]byte("KERNEL")))

	if !errors.Is(err, content.ErrMismatchedDigest) {
		t.Fatalf("PushBlob() error = %v, want content.ErrMismatchedDigest", err)
	}
	if _, err := os.Stat(blobFile(root, desc)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("blob file stat error = %v, want not-exist", err)
	}
}

// unreadReader fails the test if anything reads it.
type unreadReader struct{ read bool }

func (u *unreadReader) Read([]byte) (int, error) {
	u.read = true
	return 0, io.EOF
}

func TestPushBlob_AlreadyPresent(t *testing.T) {
	s, _ := newStore(t)
	data := []byte("kernel")
	desc := pushLayer(t, s, "boot/vmlinuz", data)
	second := &unreadReader{}

	err := s.PushBlob(t.Context(), desc, second)

	if err != nil {
		t.Fatalf("second PushBlob() error = %v, want nil", err)
	}
	if second.read {
		t.Error("second PushBlob() read its reader, want the present blob left alone")
	}
}

func TestPushBlob_ReplacesWrongSized(t *testing.T) {
	s, root := newStore(t)
	data := []byte("kernel bytes")
	desc := pushLayer(t, s, "boot/vmlinuz", data)
	tagLayers(t, s, utilityRef, desc)
	path := blobFile(root, desc)
	truncateBlob(t, path)
	if err := s.CheckLayers(t.Context(), utilityRef); !errors.Is(err, ErrLayerSize) {
		t.Fatalf("CheckLayers() before repair error = %v, want ErrLayerSize", err)
	}

	err := s.PushBlob(t.Context(), desc, bytes.NewReader(data))

	if err != nil {
		t.Fatalf("PushBlob() repair error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read repaired blob: %v", err)
	}
	if int64(len(got)) != desc.Size || sha256Hex(got) != desc.Digest.Encoded() {
		t.Errorf("repaired blob has %d bytes and sha256 %s, want %d bytes and %s", len(got), sha256Hex(got), desc.Size, desc.Digest.Encoded())
	}
	if err := s.CheckLayers(t.Context(), utilityRef); err != nil {
		t.Errorf("CheckLayers() after repair error = %v, want nil", err)
	}
}

func TestCheckLayers(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, root string, layers []ocispec.Descriptor)
		ref    string
		want   error
	}{
		{name: "ok", ref: utilityRef},
		{name: "unknown ref", ref: "utility/none:p1", want: ErrNotFound},
		{
			name: "blob deleted", ref: utilityRef, want: ErrLayerMissing,
			mutate: func(t *testing.T, root string, layers []ocispec.Descriptor) {
				t.Helper()
				if err := os.Remove(blobFile(root, layers[1])); err != nil {
					t.Fatalf("remove blob: %v", err)
				}
			},
		},
		{
			name: "blob truncated", ref: utilityRef, want: ErrLayerSize,
			mutate: func(t *testing.T, root string, layers []ocispec.Descriptor) {
				t.Helper()
				truncateBlob(t, blobFile(root, layers[0]))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, root := newStore(t)
			layers := []ocispec.Descriptor{
				pushLayer(t, s, "boot/vmlinuz", []byte("kernel")),
				pushLayer(t, s, "boot/initrd.img", []byte("initrd")),
			}
			tagLayers(t, s, utilityRef, layers...)
			if tt.mutate != nil {
				tt.mutate(t, root, layers)
			}

			err := s.CheckLayers(t.Context(), tt.ref)

			if tt.want == nil {
				if err != nil {
					t.Fatalf("CheckLayers() error = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("CheckLayers() error = %v, want %v", err, tt.want)
			}
			if errors.Is(tt.want, ErrNotFound) {
				if !errors.Is(err, errdef.ErrNotFound) {
					t.Errorf("ErrNotFound does not wrap errdef.ErrNotFound: %v", err)
				}
				return
			}
			if !strings.Contains(err.Error(), "sha256:") {
				t.Errorf("error %q does not name the digest", err)
			}
		})
	}
}

func TestCheckLayers_MissingManifestBlob(t *testing.T) {
	s, root := newStore(t)
	m := tagLayers(t, s, utilityRef, pushLayer(t, s, "boot/vmlinuz", []byte("kernel")))
	if err := os.Remove(blobFile(root, m)); err != nil {
		t.Fatalf("remove manifest blob: %v", err)
	}

	err := s.CheckLayers(t.Context(), utilityRef)

	if !errors.Is(err, ErrLayerMissing) {
		t.Fatalf("CheckLayers() error = %v, want ErrLayerMissing", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("CheckLayers() error = %v, want the tag still known (not ErrNotFound)", err)
	}
}

func TestOpenBlob_ReadsAndSeeks(t *testing.T) {
	s, root := newStore(t)
	data := []byte("kernel bytes")
	desc := pushLayer(t, s, "boot/vmlinuz", data)

	t.Run("reads and seeks", func(t *testing.T) {
		f, err := s.OpenBlob(desc)
		if err != nil {
			t.Fatalf("OpenBlob() error = %v", err)
		}
		defer f.Close()
		if _, err := f.Seek(2, io.SeekStart); err != nil {
			t.Fatalf("Seek() error = %v", err)
		}

		got, err := io.ReadAll(f)

		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		if !bytes.Equal(got, data[2:]) {
			t.Errorf("ReadAll() = %q, want %q", got, data[2:])
		}
	})
	t.Run("missing", func(t *testing.T) {
		missing, err := Layer("boot/other", 3, sha256Hex([]byte("abc")))
		if err != nil {
			t.Fatalf("Layer() error = %v", err)
		}

		_, err = s.OpenBlob(missing)

		if !errors.Is(err, ErrLayerMissing) {
			t.Errorf("OpenBlob() error = %v, want ErrLayerMissing", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		truncateBlob(t, blobFile(root, desc))

		_, err := s.OpenBlob(desc)

		if !errors.Is(err, ErrLayerSize) {
			t.Errorf("OpenBlob() error = %v, want ErrLayerSize", err)
		}
	})
}

func TestOpen_UndecodableIndexMovedAside(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	parent := filepath.Dir(root)
	openAt(t, root)
	writeFile(t, filepath.Join(root, "index.json"), "{not json")
	older := filepath.Join(parent, "store.corrupt-20261113T000000Z")
	if err := os.Mkdir(older, 0o700); err != nil {
		t.Fatalf("mkdir older sibling: %v", err)
	}

	s, moved := openAt(t, root)

	wantMoved := filepath.Join(parent, "store.corrupt-20261114T120000Z")
	if moved != wantMoved {
		t.Fatalf("moved = %q, want %q", moved, wantMoved)
	}
	old, err := os.ReadFile(filepath.Join(moved, "index.json"))
	if err != nil || string(old) != "{not json" {
		t.Errorf("moved index.json = %q, %v, want the old undecodable index", old, err)
	}
	if _, err := os.Stat(older); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("older corrupt sibling stat error = %v, want not-exist", err)
	}
	tagLayers(t, s, utilityRef, pushLayer(t, s, "boot/vmlinuz", []byte("kernel")))
}

func TestOpen_EmptyIndexReset(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	s, _ := openAt(t, root)
	desc := pushLayer(t, s, "boot/vmlinuz", []byte("kernel"))
	tagLayers(t, s, utilityRef, desc)
	if tags, err := s.Tags(t.Context(), "utility/"); err != nil || len(tags) != 1 {
		t.Fatalf("Tags() before = %v, %v, want the one tag the reset must drop", tags, err)
	}
	writeFile(t, filepath.Join(root, "index.json"), "")

	s2, moved := openAt(t, root)

	if moved != "" {
		t.Fatalf("moved = %q, want empty: oras resets an empty index itself", moved)
	}
	tags, err := s2.Tags(t.Context(), "utility/")
	if err != nil || len(tags) != 0 {
		t.Errorf("Tags() = %v, %v, want none", tags, err)
	}
	if _, err := os.Stat(blobFile(root, desc)); err != nil {
		t.Errorf("blob file stat error = %v, want present", err)
	}
}

func TestOpen_CorruptManifestMovedAside(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	s, _ := openAt(t, root)
	m := tagLayers(t, s, utilityRef, pushLayer(t, s, "boot/vmlinuz", []byte("kernel")))
	path := blobFile(root, m)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove manifest blob: %v", err)
	}
	writeFile(t, path, string(bytes.Repeat([]byte("x"), int(m.Size))))

	s2, moved := openAt(t, root)

	if moved == "" {
		t.Fatal("moved is empty, want the store with the corrupt manifest moved aside")
	}
	tags, err := s2.Tags(t.Context(), "")
	if err != nil || len(tags) != 0 {
		t.Errorf("Tags() = %v, %v, want none in the new store", tags, err)
	}
}

// TestOpen_CorruptionClassesMovedAside pins each allow-list class that no
// other Open test reaches.
func TestOpen_CorruptionClassesMovedAside(t *testing.T) {
	// rewriteManifest replaces the manifest blob with data; blobs are 0444, so
	// it unlinks the file first, as the store does.
	rewriteManifest := func(t *testing.T, root string, m ocispec.Descriptor, data []byte) {
		t.Helper()
		path := blobFile(root, m)
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove manifest blob: %v", err)
		}
		writeFile(t, path, string(data))
	}
	tests := []struct {
		name   string
		mutate func(t *testing.T, root string, m ocispec.Descriptor)
	}{
		{
			name: "truncated manifest (io.ErrUnexpectedEOF)",
			mutate: func(t *testing.T, root string, m ocispec.Descriptor) {
				t.Helper()
				full, err := os.ReadFile(blobFile(root, m))
				if err != nil {
					t.Fatalf("read manifest blob: %v", err)
				}
				rewriteManifest(t, root, m, full[:len(full)/2])
			},
		},
		{
			name: "manifest with a trailing byte (content.ErrTrailingData)",
			mutate: func(t *testing.T, root string, m ocispec.Descriptor) {
				t.Helper()
				full, err := os.ReadFile(blobFile(root, m))
				if err != nil {
					t.Fatalf("read manifest blob: %v", err)
				}
				rewriteManifest(t, root, m, append(full, 'x'))
			},
		},
		{
			name: "index of the wrong JSON type (*json.UnmarshalTypeError)",
			mutate: func(t *testing.T, root string, _ ocispec.Descriptor) {
				t.Helper()
				writeFile(t, filepath.Join(root, "index.json"), `{"manifests":5}`)
			},
		},
		{
			name: "index entry with a bad digest (errdef.ErrInvalidDigest)",
			mutate: func(t *testing.T, root string, _ ocispec.Descriptor) {
				t.Helper()
				writeFile(t, filepath.Join(root, "index.json"),
					`{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:zz","size":1}]}`)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "store")
			s, _ := openAt(t, root)
			m := tagLayers(t, s, utilityRef, pushLayer(t, s, "boot/vmlinuz", []byte("kernel")))
			tt.mutate(t, root, m)

			s2, moved := openAt(t, root)

			if moved == "" {
				t.Fatal("moved is empty, want the corrupt store moved aside")
			}
			if tags, err := s2.Tags(t.Context(), ""); err != nil || len(tags) != 0 {
				t.Errorf("Tags() = %v, %v, want none in the new store", tags, err)
			}
		})
	}
}

func TestOpen_InvalidLayoutMovedAside(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	openAt(t, root)
	writeFile(t, filepath.Join(root, "oci-layout"), `{"imageLayoutVersion":"9.9.9"}`)

	s, moved := openAt(t, root)

	if moved == "" {
		t.Fatal("moved is empty, want the store with the unsupported layout moved aside")
	}
	tagLayers(t, s, utilityRef, pushLayer(t, s, "boot/vmlinuz", []byte("kernel")))
}

func TestOpen_PermissionDeniedStops(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	root := filepath.Join(t.TempDir(), "store")
	openAt(t, root)
	index := filepath.Join(root, "index.json")
	before, err := os.Stat(index)
	if err != nil {
		t.Fatalf("stat index.json: %v", err)
	}
	if err := os.Chmod(index, 0o000); err != nil {
		t.Fatalf("chmod index.json: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(index, 0o600); err != nil {
			t.Errorf("restore index.json mode: %v", err)
		}
	})

	_, moved, err := Open(t.Context(), root, testNow)

	if err == nil {
		t.Fatal("Open() error = nil, want a permission error")
	}
	if _, ok := errors.AsType[*fs.PathError](err); !ok {
		t.Errorf("Open() error = %v, want a *fs.PathError", err)
	}
	if moved != "" {
		t.Errorf("moved = %q, want empty", moved)
	}
	after, err := os.Stat(index)
	if err != nil || !os.SameFile(before, after) {
		t.Errorf("index.json stat = %v, %v, want the original file left in place", after, err)
	}
	siblings, err := filepath.Glob(filepath.Join(filepath.Dir(root), corruptPrefix+"*"))
	if err != nil || len(siblings) != 0 {
		t.Errorf("store.corrupt-* siblings = %v, %v, want none", siblings, err)
	}
}

func TestOpen_CancelledContextKeepsStore(t *testing.T) {
	cancelled := func(t *testing.T) context.Context {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		return ctx
	}

	t.Run("healthy store opens", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "store")
		s, _ := openAt(t, root)
		tagLayers(t, s, utilityRef, pushLayer(t, s, "boot/vmlinuz", []byte("kernel")))

		s2, moved, err := Open(cancelled(t), root, testNow)

		if err != nil || moved != "" {
			t.Fatalf("Open() = moved %q, error %v, want an open store", moved, err)
		}
		if _, err := s2.Resolve(t.Context(), utilityRef); err != nil {
			t.Errorf("Resolve() error = %v, want the tag kept", err)
		}
	})

	t.Run("corrupt store is not moved aside", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "store")
		parent := filepath.Dir(root)
		openAt(t, root)
		writeFile(t, filepath.Join(root, "index.json"), "{")
		older := filepath.Join(parent, "store.corrupt-20261113T000000Z")
		if err := os.Mkdir(older, 0o700); err != nil {
			t.Fatalf("mkdir older sibling: %v", err)
		}

		_, moved, err := Open(cancelled(t), root, testNow)

		if err == nil {
			t.Fatal("Open() error = nil, want the load error")
		}
		if moved != "" {
			t.Errorf("moved = %q, want empty", moved)
		}
		for _, p := range []string{filepath.Join(root, "index.json"), older} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("stat %s: %v, want it left in place", p, err)
			}
		}
	})
}

func TestTagsUntagGC(t *testing.T) {
	s, root := newStore(t)
	a := pushLayer(t, s, "boot/a", []byte("layer a"))
	b := pushLayer(t, s, "boot/b", []byte("layer b"))
	tagLayers(t, s, "utility/a-amd64:p1", a)
	tagLayers(t, s, "other/b:p1", b)

	tags, err := s.Tags(t.Context(), "utility/")
	if err != nil {
		t.Fatalf("Tags() error = %v", err)
	}
	if want := []string{"utility/a-amd64:p1"}; !slices.Equal(tags, want) {
		t.Fatalf("Tags(utility/) = %v, want %v", tags, want)
	}
	if err := s.Untag(t.Context(), "utility/a-amd64:p1"); err != nil {
		t.Fatalf("Untag() error = %v", err)
	}
	if err := s.Untag(t.Context(), "utility/a-amd64:p1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Untag() error = %v, want ErrNotFound", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	entries, err := os.ReadDir(filepath.Join(root, "blobs", "sha256"))
	if err != nil {
		t.Fatalf("read blobs: %v", err)
	}
	for _, e := range entries {
		if err := os.Chtimes(filepath.Join(root, "blobs", "sha256", e.Name()), old, old); err != nil {
			t.Fatalf("age %s: %v", e.Name(), err)
		}
	}

	if err := s.GC(t.Context()); err != nil {
		t.Fatalf("GC() error = %v", err)
	}

	if _, err := os.Stat(blobFile(root, a)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a's layer stat error = %v, want collected", err)
	}
	if _, err := os.Stat(blobFile(root, b)); err != nil {
		t.Errorf("b's layer stat error = %v, want kept", err)
	}
}

func TestChecks(t *testing.T) {
	s, root := newStore(t)
	pushLayer(t, s, "boot/vmlinuz", []byte("kernel"))
	pushLayer(t, s, "boot/initrd.img", []byte("initrd bytes"))
	var readable, size admin.Check
	for _, c := range s.Checks() {
		switch c.Name {
		case "store:readable":
			readable = c
		case "store:bytes":
			size = c
		default:
			t.Errorf("unexpected check %q", c.Name)
		}
	}
	if !readable.Critical || size.Name == "" || size.Critical {
		t.Fatalf("checks = readable %+v, bytes %+v, want a Critical store:readable and an informational store:bytes", readable, size)
	}

	if got := readable.Run(t.Context()); got.Status != admin.StatusPass {
		t.Errorf("store:readable = %+v, want pass", got)
	}
	got := size.Run(t.Context())
	if got.Status != admin.StatusPass || got.ObservedValue != int64(len("kernel")+len("initrd bytes")) || got.ObservedUnit != "bytes" {
		t.Errorf("store:bytes = %+v, want pass, %d bytes", got, len("kernel")+len("initrd bytes"))
	}

	if err := os.Remove(filepath.Join(root, "index.json")); err != nil {
		t.Fatalf("remove index.json: %v", err)
	}
	if got := readable.Run(t.Context()); got.Status != admin.StatusFail || got.Output == "" {
		t.Errorf("store:readable without index.json = %+v, want fail with output", got)
	}
}

// TestChecks_FreshStore pins that a store nothing was pushed to is healthy: oras
// creates blobs/sha256 with the first blob, so a first run has no such directory.
func TestChecks_FreshStore(t *testing.T) {
	s, root := newStore(t)
	byName := map[string]admin.Check{}
	for _, c := range s.Checks() {
		byName[c.Name] = c
	}

	if got := byName["store:readable"].Run(t.Context()); got.Status != admin.StatusPass {
		t.Errorf("store:readable on a fresh store = %+v, want pass", got)
	}
	if got := byName["store:bytes"].Run(t.Context()); got.Status != admin.StatusPass || got.ObservedValue != int64(0) {
		t.Errorf("store:bytes on a fresh store = %+v, want pass, 0 bytes", got)
	}

	if err := os.RemoveAll(filepath.Join(root, "blobs")); err != nil {
		t.Fatalf("remove blobs: %v", err)
	}
	if got := byName["store:readable"].Run(t.Context()); got.Status != admin.StatusFail {
		t.Errorf("store:readable without the blobs directory = %+v, want fail", got)
	}
}

func TestLayers_MissingManifestBlob(t *testing.T) {
	s, root := newStore(t)
	m := tagLayers(t, s, utilityRef, pushLayer(t, s, "boot/vmlinuz", []byte("kernel")))
	if err := os.Remove(blobFile(root, m)); err != nil {
		t.Fatalf("remove manifest blob: %v", err)
	}

	_, err := s.Layers(t.Context(), utilityRef)

	if !errors.Is(err, ErrLayerMissing) {
		t.Fatalf("Layers() error = %v, want ErrLayerMissing", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("Layers() error = %v, want the tag still known (not ErrNotFound)", err)
	}
}
