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
	"path/filepath"
	"strings"
	"time"

	"github.com/oras-project/oras-go/v3"
	"github.com/oras-project/oras-go/v3/content"
	"github.com/oras-project/oras-go/v3/content/oci"
	"github.com/oras-project/oras-go/v3/errdef"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// LayerMediaType is the media type of every layer the store holds: a raw file.
const LayerMediaType = "application/octet-stream"

const (
	// corruptPrefix names the siblings of the store root that hold the layouts
	// Open set aside.
	corruptPrefix = "store.corrupt-"
	// corruptStamp is the time layout of a set-aside directory's suffix.
	corruptStamp = "20060102T150405Z"
)

// ErrNotFound is returned for a reference the store does not hold. It wraps
// oras's errdef.ErrNotFound, so either matches with errors.Is.
var ErrNotFound = fmt.Errorf("store: %w", errdef.ErrNotFound)

// Store is an OCI image layout on disk. Blobs are pushed with PushBlob,
// tagged with TagArtifact and read back with OpenBlob. Its methods may be
// called concurrently, except PushBlob for one digest whose blob is on disk at
// the wrong size: two such calls race on the repair, so callers serialise them
// (serve runs one fetch at a time).
type Store struct {
	root string
	oci  *oci.Store
}

// Layer returns the descriptor of a layer holding a file: media type
// LayerMediaType, digest sha256:<sha256Hex>, and file (its path in the artifact) as its title annotation.
// It returns an error for a size below zero or a digest that is not a
// well-formed sha256.
func Layer(file string, size int64, sha256Hex string) (ocispec.Descriptor, error) {
	dgst := digest.NewDigestFromEncoded(digest.SHA256, sha256Hex)
	if err := dgst.Validate(); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("layer %q: %w", file, err)
	}
	if size < 0 {
		return ocispec.Descriptor{}, fmt.Errorf("layer %q: size %d is negative", file, size)
	}
	return ocispec.Descriptor{
		MediaType:   LayerMediaType,
		Digest:      dgst,
		Size:        size,
		Annotations: map[string]string{ocispec.AnnotationTitle: file},
	}, nil
}

// Open opens the OCI layout at root, creating it when it is absent. A layout
// that is provably corrupt is renamed to <parent>/store.corrupt-<now as
// 20060102T150405Z>, every older store.corrupt-* sibling is deleted, and a
// fresh layout is opened at root; the second result is the new name of the
// moved layout, empty when nothing was moved. When moving fails the moved name
// is returned with the error if the rename had already happened.
//
// Corruption is recognised by an explicit list of load errors only: an
// undecodable or mistyped index.json, an unsupported oci-layout version, a bad
// digest, a manifest whose bytes do not match its digest, and a truncated
// read. Every other error, a permission or I/O failure included, is returned
// untouched, so serve stops rather than discarding a store it merely cannot
// read. An empty index.json is not corruption: oras resets it and keeps the
// blobs.
//
// The load ignores cancellation of ctx, so a stop signal never leaves a half
// loaded index; a corrupt layout is never moved aside once ctx is done.
func Open(ctx context.Context, root string, now func() time.Time) (*Store, string, error) {
	o, err := newOCI(ctx, root)
	if err == nil {
		return &Store{root: root, oci: o}, "", nil
	}
	if ctx.Err() != nil || !isCorrupt(err) {
		return nil, "", fmt.Errorf("open store %s: %w", root, err)
	}
	moved, mvErr := moveAside(root, now())
	if mvErr != nil {
		return nil, moved, fmt.Errorf("store %s is corrupt (%w) and could not be set aside: %w", root, err, mvErr)
	}
	o, err = newOCI(ctx, root)
	if err != nil {
		return nil, moved, fmt.Errorf("open store %s after moving the corrupt one to %s: %w", root, moved, err)
	}
	return &Store{root: root, oci: o}, moved, nil
}

// newOCI opens the layout at root with index writes synced to disk.
func newOCI(ctx context.Context, root string) (*oci.Store, error) {
	o, err := oci.NewWithContext(context.WithoutCancel(ctx), root)
	if err != nil {
		return nil, err
	}
	o.SyncIndex = true
	return o, nil
}

// isCorrupt reports whether err is on the corruption allow-list.
func isCorrupt(err error) bool {
	_, syntax := errors.AsType[*json.SyntaxError](err)          //nolint:errcheck // the match target is not a dropped error; modernize requires AsType over errors.As
	_, mistyped := errors.AsType[*json.UnmarshalTypeError](err) //nolint:errcheck // the match target is not a dropped error; modernize requires AsType over errors.As
	return syntax || mistyped ||
		errors.Is(err, errdef.ErrUnsupportedVersion) ||
		errors.Is(err, errdef.ErrInvalidDigest) ||
		errors.Is(err, content.ErrMismatchedDigest) ||
		errors.Is(err, content.ErrTrailingData) ||
		errors.Is(err, io.ErrUnexpectedEOF)
}

// moveAside renames root to its store.corrupt-<stamp> sibling and deletes the
// older store.corrupt-* siblings. It returns the new name, set once the rename
// succeeded.
func moveAside(root string, now time.Time) (string, error) {
	parent := filepath.Dir(filepath.Clean(root))
	dst := filepath.Join(parent, corruptPrefix+now.UTC().Format(corruptStamp))
	if err := os.Rename(root, dst); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return dst, err
	}
	var errs []error
	for _, e := range entries {
		if name := e.Name(); strings.HasPrefix(name, corruptPrefix) && name != filepath.Base(dst) {
			errs = append(errs, os.RemoveAll(filepath.Join(parent, name)))
		}
	}
	return dst, errors.Join(errs...)
}

// inRoot runs fn with the store root opened as an os.Root, so no path a
// caller derives can leave the store directory, and closes the root after.
func (s *Store) inRoot(fn func(*os.Root) error) (err error) {
	r, err := os.OpenRoot(s.root)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	return fn(r)
}

// blobRel is the slash-relative path of a blob under the store root. It
// rejects a digest that is not well formed.
func blobRel(d digest.Digest) (string, error) {
	if err := d.Validate(); err != nil {
		return "", fmt.Errorf("blob digest %q: %w", d, err)
	}
	return path.Join(ocispec.ImageBlobsDir, d.Algorithm().String(), d.Encoded()), nil
}

// blobSize is the on-disk size of a blob; the error matches fs.ErrNotExist
// when the blob file is absent.
func (s *Store) blobSize(d digest.Digest) (int64, error) {
	rel, err := blobRel(d)
	if err != nil {
		return 0, err
	}
	var size int64
	err = s.inRoot(func(r *os.Root) error {
		fi, err := r.Stat(rel)
		if err != nil {
			return err
		}
		size = fi.Size()
		return nil
	})
	return size, err
}

// PushBlob stores the blob described by desc, read from r and verified by oras
// against desc. A blob already on disk at desc.Size is left alone and r is not
// read. A blob on disk at another size is a truncated leftover: oras keeps any
// existing file, so it is unlinked first (that needs only write permission on
// the directory, not on the 0444 file) and pushed again. A concurrent push of
// the same blob is not an error.
func (s *Store) PushBlob(ctx context.Context, desc ocispec.Descriptor, r io.Reader) error {
	size, err := s.blobSize(desc.Digest)
	switch {
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	case err == nil && size == desc.Size:
		return nil
	case err == nil:
		if err := s.removeBlob(desc.Digest); err != nil {
			return err
		}
	}
	if err := s.oci.Push(ctx, desc, r); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return fmt.Errorf("push blob %s: %w", desc.Digest, err)
	}
	return nil
}

// removeBlob unlinks a blob file; one that is already gone is not an error.
func (s *Store) removeBlob(d digest.Digest) error {
	rel, err := blobRel(d)
	if err != nil {
		return err
	}
	err = s.inRoot(func(r *os.Root) error { return r.Remove(rel) })
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove wrong-sized blob %s: %w", d, err)
	}
	return nil
}

// TagArtifact packs an OCI 1.1 artifact manifest of artifactType over layers,
// which must already be pushed, stores it and tags it ref. It returns the
// manifest descriptor.
func (s *Store) TagArtifact(ctx context.Context, ref, artifactType string, layers []ocispec.Descriptor) (ocispec.Descriptor, error) {
	m, err := oras.PackManifest(ctx, s.oci, oras.PackManifestVersion1_1, artifactType, oras.PackManifestOptions{Layers: layers})
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("pack manifest for %q: %w", ref, err)
	}
	if err := s.oci.Tag(ctx, m, ref); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("tag %q: %w", ref, err)
	}
	return m, nil
}

// Resolve returns the descriptor ref is tagged to, or an error matching
// ErrNotFound when no such tag exists. ref is a tag: a digest of a blob on disk
// resolves to a raw blob descriptor, not a manifest, and callers pass only
// tags.
func (s *Store) Resolve(ctx context.Context, ref string) (ocispec.Descriptor, error) {
	desc, err := s.oci.Resolve(ctx, ref)
	if err != nil {
		return ocispec.Descriptor{}, notFound("resolve", ref, err)
	}
	return desc, nil
}

// notFound wraps err as ErrNotFound when oras reports a missing item.
func notFound(op, ref string, err error) error {
	if errors.Is(err, errdef.ErrNotFound) {
		return fmt.Errorf("%s %q: %w", op, ref, ErrNotFound)
	}
	return fmt.Errorf("%s %q: %w", op, ref, err)
}

// Layers returns the layers of the manifest ref is tagged to, in manifest
// order. The manifest is read with its size and digest verified.
func (s *Store) Layers(ctx context.Context, ref string) ([]ocispec.Descriptor, error) {
	desc, err := s.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	raw, err := content.FetchAll(ctx, s.oci, desc)
	if err != nil {
		return nil, fmt.Errorf("read manifest of %q: %w", ref, err)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode manifest of %q: %w", ref, err)
	}
	return m.Layers, nil
}

// Tags lists, in ascending order, every tag that starts with prefix.
func (s *Store) Tags(ctx context.Context, prefix string) ([]string, error) {
	var tags []string
	err := s.oci.Tags(ctx, "", func(all []string) error {
		for _, tag := range all {
			if strings.HasPrefix(tag, prefix) {
				tags = append(tags, tag)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	return tags, nil
}

// Untag removes the tag ref; the blobs stay until GC. It returns an error
// matching ErrNotFound when ref is not a tag.
func (s *Store) Untag(ctx context.Context, ref string) error {
	if err := s.oci.Untag(ctx, ref); err != nil {
		return notFound("untag", ref, err)
	}
	return nil
}

// GC removes blobs no tag reaches. Unreachable blobs written in the last hour
// are kept, so a push in flight survives.
func (s *Store) GC(ctx context.Context) error {
	if err := s.oci.GC(ctx); err != nil {
		return fmt.Errorf("collect garbage: %w", err)
	}
	return nil
}
