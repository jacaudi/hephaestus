package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"

	"github.com/opencontainers/go-digest"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"go.jacaudi.dev/hephaestus/internal/admin"
)

// ErrLayerMissing means a layer's blob file does not exist.
var ErrLayerMissing = errors.New("layer blob missing")

// ErrLayerSize means a layer's blob file is not the size its descriptor says.
var ErrLayerSize = errors.New("layer blob has the wrong size")

// CheckLayers verifies that every layer of the manifest ref is tagged to is on
// disk at its descriptor's size. It returns an error matching ErrNotFound for
// an unknown ref, ErrLayerMissing when the manifest's own blob is gone, then
// for the first bad layer ErrLayerMissing or ErrLayerSize, naming its digest.
func (s *Store) CheckLayers(ctx context.Context, ref string) error {
	layers, err := s.Layers(ctx, ref)
	if err != nil {
		return err
	}
	for _, l := range layers {
		size, err := s.blobSize(l.Digest)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("layer %s of %q: %w", l.Digest, ref, ErrLayerMissing)
		case err != nil:
			return fmt.Errorf("layer %s of %q: %w", l.Digest, ref, err)
		case size != l.Size:
			return fmt.Errorf("layer %s of %q is %d bytes, want %d: %w", l.Digest, ref, size, l.Size, ErrLayerSize)
		}
	}
	return nil
}

// OpenBlob opens the blob file of desc for reading and seeking. It returns
// ErrLayerMissing when the file does not exist and ErrLayerSize when its size
// is not desc.Size. The reader never watches a context.
func (s *Store) OpenBlob(desc ocispec.Descriptor) (io.ReadSeekCloser, error) {
	rel, err := blobRel(desc.Digest)
	if err != nil {
		return nil, err
	}
	var f *os.File
	err = s.inRoot(func(r *os.Root) error {
		var err error
		f, err = r.Open(rel)
		return err
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("layer %s: %w", desc.Digest, ErrLayerMissing)
	}
	if err != nil {
		return nil, fmt.Errorf("open layer %s: %w", desc.Digest, err)
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("stat layer %s: %w", desc.Digest, err), f.Close())
	}
	if fi.Size() != desc.Size {
		return nil, errors.Join(fmt.Errorf("layer %s is %d bytes, want %d: %w", desc.Digest, fi.Size(), desc.Size, ErrLayerSize), f.Close())
	}
	return f, nil
}

// Checks returns the store's health checks: store:readable (critical) and
// store:bytes (informational).
func (s *Store) Checks() []admin.Check {
	return []admin.Check{
		{Name: "store:readable", Critical: true, Run: s.checkReadable},
		{Name: "store:bytes", Run: s.checkBytes},
	}
}

// checkReadable passes when index.json decodes and the blob directory lists.
func (s *Store) checkReadable(context.Context) admin.Result {
	err := s.inRoot(func(r *os.Root) error {
		if err := decodeIndex(r); err != nil {
			return err
		}
		_, err := readBlobDir(r)
		return err
	})
	if err != nil {
		return admin.Result{Status: admin.StatusFail, Output: err.Error()}
	}
	return admin.Result{Status: admin.StatusPass}
}

// checkBytes reports the total size of the blob files.
func (s *Store) checkBytes(context.Context) admin.Result {
	var total int64
	err := s.inRoot(func(r *os.Root) error {
		entries, err := readBlobDir(r)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			fi, err := e.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue // removed between the listing and the stat
			}
			if err != nil {
				return err
			}
			total += fi.Size()
		}
		return nil
	})
	if err != nil {
		return admin.Result{Status: admin.StatusFail, Output: err.Error()}
	}
	return admin.Result{Status: admin.StatusPass, ObservedValue: total, ObservedUnit: "bytes"}
}

// decodeIndex reads index.json under r and decodes it.
func decodeIndex(r *os.Root) error {
	f, err := r.Open(ocispec.ImageIndexFile)
	if err != nil {
		return err
	}
	err = json.NewDecoder(f).Decode(new(ocispec.Index))
	return errors.Join(err, f.Close())
}

// readBlobDir lists the sha256 blob directory under r. oras creates that
// directory with the first blob, so a store nothing was pushed to lacks it and
// the list is empty, provided the blobs directory itself is there.
func readBlobDir(r *os.Root) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(r.FS(), path.Join(ocispec.ImageBlobsDir, digest.Canonical.String()))
	if !errors.Is(err, fs.ErrNotExist) {
		return entries, err
	}
	if _, err := r.Stat(ocispec.ImageBlobsDir); err != nil {
		return nil, err
	}
	return nil, nil
}

// ContentLost reports whether err, from CheckLayers or Layers, says the store
// lacks the content: an unknown tag, a missing layer or one of the wrong size.
// Any other error means the content could not be read, which says nothing
// about whether it is there.
func ContentLost(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, ErrLayerMissing) || errors.Is(err, ErrLayerSize)
}
