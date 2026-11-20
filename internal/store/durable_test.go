package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

var errInjected = errors.New("injected failure")

// layerOf returns the descriptor of data as a layer, without pushing it.
func layerOf(t *testing.T, title string, data []byte) ocispec.Descriptor {
	t.Helper()
	desc, err := Layer(title, int64(len(data)), sha256Hex(data))
	if err != nil {
		t.Fatalf("Layer(%q) error = %v", title, err)
	}
	return desc
}

// exists reports whether path is on disk.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// emptyDir fails the test unless dir holds no entry.
func emptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 0 {
		t.Errorf("%s holds %d entries, want none", dir, len(entries))
	}
}

func TestPushBlob_SyncsBeforeReturn(t *testing.T) {
	s, root := newStore(t)
	desc := layerOf(t, "boot/vmlinuz", []byte("kernel"))
	var got []string
	s.syncFile = func(f *os.File) error {
		got = append(got, fmt.Sprintf("file blob=%t", exists(blobFile(root, desc))))
		return f.Sync()
	}
	s.syncDirFn = func(r *os.Root, rel string) error {
		got = append(got, fmt.Sprintf("dir %s blob=%t", rel, exists(blobFile(root, desc))))
		return fsyncDir(r, rel)
	}

	err := s.PushBlob(t.Context(), desc, bytes.NewReader([]byte("kernel")))

	if err != nil {
		t.Fatalf("PushBlob() error = %v", err)
	}
	want := []string{"file blob=false", "dir blobs/sha256 blob=true"}
	if !slices.Equal(got, want) {
		t.Errorf("sync order = %v, want %v", got, want)
	}
}

func TestPushBlob_SyncErrorFails(t *testing.T) {
	s, root := newStore(t)
	desc := layerOf(t, "boot/vmlinuz", []byte("kernel"))
	s.syncFile = func(*os.File) error { return errInjected }

	err := s.PushBlob(t.Context(), desc, bytes.NewReader([]byte("kernel")))

	if !errors.Is(err, errInjected) {
		t.Fatalf("PushBlob() error = %v, want the injected sync error", err)
	}
	if exists(blobFile(root, desc)) {
		t.Error("blob is at its final path, want none after a failed sync")
	}
	emptyDir(t, filepath.Join(root, "ingest"))
}

func TestPushBlob_PresentWrongHashReplaced(t *testing.T) {
	s, root := newStore(t)
	data := []byte("kernel bytes")
	desc := pushLayer(t, s, "boot/vmlinuz", data)
	path := blobFile(root, desc)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove blob: %v", err)
	}
	writeFile(t, path, string(make([]byte, len(data))))
	// every Stat of the blob is recorded; the sync seams look at it from inside
	// the push, where a replacement that removed the file first would not find it
	var seen []bool
	s.statFn = func(r *os.Root, name string) (fs.FileInfo, error) {
		fi, err := r.Stat(name)
		seen = append(seen, err == nil)
		return fi, err
	}
	s.syncFile = func(f *os.File) error {
		_, _ = s.blobSize(desc.Digest)
		return f.Sync()
	}
	s.syncDirFn = func(r *os.Root, rel string) error {
		_, _ = s.blobSize(desc.Digest)
		return fsyncDir(r, rel)
	}

	err := s.PushBlob(t.Context(), desc, bytes.NewReader(data))

	if err != nil {
		t.Fatalf("PushBlob() error = %v", err)
	}
	if want := []bool{true, true, true}; !slices.Equal(seen, want) {
		t.Errorf("blob present at each look (size check, file sync, directory sync) = %v, want %v", seen, want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read replaced blob: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("replaced blob = %q, want %q", got, data)
	}
}

func TestPushBlob_PresentRightHashSynced(t *testing.T) {
	s, _ := newStore(t)
	desc := pushLayer(t, s, "boot/vmlinuz", []byte("kernel"))
	var got []string
	s.syncFile = func(f *os.File) error {
		got = append(got, "file")
		return f.Sync()
	}
	s.syncDirFn = func(r *os.Root, rel string) error {
		got = append(got, "dir "+rel)
		return fsyncDir(r, rel)
	}
	second := &unreadReader{}

	err := s.PushBlob(t.Context(), desc, second)

	if err != nil {
		t.Fatalf("PushBlob() error = %v", err)
	}
	if second.read {
		t.Error("PushBlob() read its reader, want the present blob kept")
	}
	if want := []string{"file", "dir blobs/sha256"}; !slices.Equal(got, want) {
		t.Errorf("syncs = %v, want %v", got, want)
	}
}

// ingested reads the content of an ingest file the store has open.
func ingested(t *testing.T, f *os.File) []byte {
	t.Helper()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read ingest file %s: %v", f.Name(), err)
	}
	return data
}

func TestTagArtifact_ManifestDurableBeforeTag(t *testing.T) {
	s, root := newStore(t)
	layer := pushLayer(t, s, "boot/vmlinuz", []byte("kernel"))
	var got []string
	s.syncFile = func(f *os.File) error {
		kind := "manifest"
		if bytes.Equal(ingested(t, f), []byte("{}")) {
			kind = "config"
		}
		index, err := os.ReadFile(filepath.Join(root, "index.json"))
		if err != nil {
			t.Errorf("read index.json: %v", err)
		}
		got = append(got, fmt.Sprintf("file %s tagged=%t", kind, bytes.Contains(index, []byte(utilityRef))))
		return f.Sync()
	}
	s.syncDirFn = func(r *os.Root, rel string) error {
		got = append(got, "dir "+rel)
		return fsyncDir(r, rel)
	}

	m := tagLayers(t, s, utilityRef, layer)

	want := []string{
		"file config tagged=false", "dir blobs/sha256",
		"file manifest tagged=false", "dir blobs/sha256",
	}
	if !slices.Equal(got, want) {
		t.Errorf("syncs = %v, want %v", got, want)
	}
	if !exists(blobFile(root, m)) {
		t.Errorf("manifest blob %s is not on disk", m.Digest)
	}
	if _, err := s.Resolve(t.Context(), utilityRef); err != nil {
		t.Errorf("Resolve() error = %v, want the tag", err)
	}
}

func TestTagArtifact_SyncErrorFails(t *testing.T) {
	s, root := newStore(t)
	layer := pushLayer(t, s, "boot/vmlinuz", []byte("kernel"))
	s.syncFile = func(f *os.File) error {
		if bytes.Contains(ingested(t, f), []byte("layers")) {
			return errInjected
		}
		return f.Sync()
	}

	_, err := s.TagArtifact(t.Context(), utilityRef, artifactType, []ocispec.Descriptor{layer})

	if !errors.Is(err, errInjected) {
		t.Fatalf("TagArtifact() error = %v, want the injected sync error", err)
	}
	if _, err := s.Resolve(t.Context(), utilityRef); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve() error = %v, want ErrNotFound: the tag must be absent", err)
	}
	emptyDir(t, filepath.Join(root, "ingest"))
}

func TestOpen_FreshStoreCreatesDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	var got []string
	record := func(s *Store) {
		s.syncDirFn = func(r *os.Root, rel string) error {
			got = append(got, rel)
			return fsyncDir(r, rel)
		}
	}

	if _, _, err := Open(t.Context(), root, testNow, record); err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	for _, dir := range []string{"ingest", "blobs/sha256"} {
		if fi, err := os.Stat(filepath.Join(root, dir)); err != nil || !fi.IsDir() {
			t.Errorf("stat %s = %v, %v, want a directory", dir, fi, err)
		}
	}
	if want := []string{"blobs/sha256", "blobs", "."}; !slices.Equal(got, want) {
		t.Errorf("directories fsynced = %v, want %v", got, want)
	}
}

func TestSyncDir_EINVALTolerated(t *testing.T) {
	tests := []struct {
		name        string
		errno       syscall.Errno
		wantErr     bool
		wantReports int
	}{
		{name: "EINVAL", errno: syscall.EINVAL, wantReports: 1},
		{name: "ENOTSUP", errno: syscall.ENOTSUP, wantReports: 1},
		{name: "EIO", errno: syscall.EIO, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var reports []error
			root := filepath.Join(t.TempDir(), "store")
			s, _, err := Open(t.Context(), root, testNow, WithErrorHandler(func(_ context.Context, err error) {
				reports = append(reports, err)
			}))
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			s.syncDirFn = func(_ *os.Root, rel string) error {
				return &fs.PathError{Op: "sync", Path: rel, Err: tt.errno}
			}

			first := s.syncDir(t.Context(), "blobs")
			second := s.syncDir(t.Context(), "blobs")

			if (first != nil) != tt.wantErr || (second != nil) != tt.wantErr {
				t.Fatalf("syncDir() errors = %v, %v, wantErr %v", first, second, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(first, tt.errno) {
				t.Errorf("syncDir() error = %v, want it to match %v", first, tt.errno)
			}
			if len(reports) != tt.wantReports {
				t.Fatalf("error handler told %d times (%v), want %d", len(reports), reports, tt.wantReports)
			}
			if tt.wantReports > 0 && !errors.Is(reports[0], ErrDirSyncUnsupported) {
				t.Errorf("reported %v, want ErrDirSyncUnsupported", reports[0])
			}
		})
	}
}
