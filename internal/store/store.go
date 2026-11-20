package store

import (
	"bytes"
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
	"sync"
	"time"

	"github.com/oras-project/oras-go/v3"
	"github.com/oras-project/oras-go/v3/content"
	"github.com/oras-project/oras-go/v3/content/memory"
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
// called concurrently; PushBlob serialises the pushes of one digest itself.
type Store struct {
	root string
	oci  *oci.Store

	// onError is told of failures the store survives (ErrDirSyncUnsupported).
	onError func(context.Context, error)
	// dirSyncWarned makes the unsupported-directory-fsync report happen once.
	dirSyncWarned sync.Once

	// syncFile, syncDirFn and statFn are the file sync, directory fsync and
	// stat the store performs; tests replace them to record calls or inject
	// failures, so they stay the real ones everywhere else.
	syncFile  func(*os.File) error
	syncDirFn func(*os.Root, string) error
	statFn    func(*os.Root, string) (fs.FileInfo, error)

	// pushMu guards pushing: one mutex per digest ever pushed, held by PushBlob
	// from its size check to the end of its push.
	pushMu  sync.Mutex
	pushing map[digest.Digest]*sync.Mutex
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
// digest, a manifest whose bytes do not match its digest, trailing data after a
// manifest, and a truncated read. Every other error, a permission or I/O failure included, is returned
// untouched, so serve stops rather than discarding a store it merely cannot
// read. An empty index.json is not corruption: oras resets it and keeps the
// blobs.
//
// The load ignores cancellation of ctx, so a stop signal never leaves a half
// loaded index; a corrupt layout is never moved aside once ctx is done.
//
// Open creates ingest/ and blobs/sha256/ when they are missing, then fsyncs
// blobs/sha256/, blobs/ and root, so the layout is durable before anything is
// written to it.
func Open(ctx context.Context, root string, now func() time.Time, opts ...Option) (*Store, string, error) {
	o, err := newOCI(ctx, root)
	if err == nil {
		s := makeStore(root, o, opts)
		if err := s.prepare(ctx); err != nil {
			return nil, "", fmt.Errorf("open store %s: %w", root, err)
		}
		return s, "", nil
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
	s := makeStore(root, o, opts)
	if err := s.prepare(ctx); err != nil {
		return nil, moved, fmt.Errorf("open store %s after moving the corrupt one to %s: %w", root, moved, err)
	}
	return s, moved, nil
}

// Option configures a Store at Open.
type Option func(*Store)

// WithErrorHandler sets the function that is told of failures the store
// survives, as ErrDirSyncUnsupported; the default does nothing. The package
// never logs, so the caller decides what these are worth.
func WithErrorHandler(fn func(context.Context, error)) Option {
	return func(s *Store) { s.onError = fn }
}

// makeStore returns the store over o at root with opts applied.
func makeStore(root string, o *oci.Store, opts []Option) *Store {
	s := &Store{
		root:      root,
		oci:       o,
		onError:   func(context.Context, error) {},
		syncFile:  (*os.File).Sync,
		syncDirFn: fsyncDir,
		statFn:    (*os.Root).Stat,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
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
		fi, err := s.statFn(r, rel)
		if err != nil {
			return err
		}
		size = fi.Size()
		return nil
	})
	return size, err
}

// PushBlob stores the blob described by desc, read from r and verified against
// desc, durably: the blob file and blobs/sha256 are synced before it returns.
// A blob already on disk at desc.Size is hashed; when it is desc's, it is
// synced and r is not read. A blob of another hash or size is a leftover (a
// truncated file, or a zero-filled one a power cut left): the new blob is
// written to ingest/ and renamed over it, so the path is never absent. A
// concurrent push of the same blob is not an error: pushes of one digest take
// turns, because the size check, the hash check and the write are one repair.
// Pushes of different digests run together.
func (s *Store) PushBlob(ctx context.Context, desc ocispec.Descriptor, r io.Reader) error {
	rel, err := blobRel(desc.Digest)
	if err != nil {
		return err
	}
	unlock := s.lockPush(desc.Digest)
	defer unlock()
	size, err := s.blobSize(desc.Digest)
	switch {
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	case err == nil && size == desc.Size:
		ok, err := s.syncPresent(desc, rel)
		if err != nil {
			return fmt.Errorf("sync blob %s: %w", desc.Digest, err)
		}
		if ok {
			return s.syncDir(ctx, path.Dir(rel))
		}
	}
	return s.writeBlob(ctx, desc, r)
}

// lockPush takes the push lock of d, which the returned function releases.
// ponytail: a mutex stays in the map for every digest pushed, a few per catalog
// entry, so it is not freed; a reference count would if digests ever churn.
func (s *Store) lockPush(d digest.Digest) (unlock func()) {
	s.pushMu.Lock()
	if s.pushing == nil {
		s.pushing = map[digest.Digest]*sync.Mutex{}
	}
	m, ok := s.pushing[d]
	if !ok {
		m = &sync.Mutex{}
		s.pushing[d] = m
	}
	s.pushMu.Unlock()
	m.Lock()
	return m.Unlock
}

// TagArtifact packs an OCI 1.1 artifact manifest of artifactType over layers,
// which must already be pushed, stores it and tags it ref. The manifest and
// its config blob are written durably before the tag names them. It returns
// the manifest descriptor.
func (s *Store) TagArtifact(ctx context.Context, ref, artifactType string, layers []ocispec.Descriptor) (ocispec.Descriptor, error) {
	mem := memory.New()
	m, err := oras.PackManifest(ctx, mem, oras.PackManifestVersion1_1, artifactType, oras.PackManifestOptions{Layers: layers})
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("pack manifest for %q: %w", ref, err)
	}
	raw, err := content.FetchAll(ctx, mem, m)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("read packed manifest for %q: %w", ref, err)
	}
	var packed ocispec.Manifest
	if err := json.Unmarshal(raw, &packed); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("decode packed manifest for %q: %w", ref, err)
	}
	cfg, err := content.FetchAll(ctx, mem, packed.Config)
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("read packed config for %q: %w", ref, err)
	}
	// the config goes first, so a manifest on disk never names a missing config
	err = s.writeBlob(ctx, packed.Config, bytes.NewReader(cfg))
	if err == nil {
		err = s.writeBlob(ctx, m, bytes.NewReader(raw))
	}
	if err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("store manifest of %q: %w", ref, err)
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
// order. The manifest is read with its size and digest verified. It returns an
// error matching ErrNotFound for an unknown ref, and ErrLayerMissing when the
// tag is known but the manifest's own blob is gone.
func (s *Store) Layers(ctx context.Context, ref string) ([]ocispec.Descriptor, error) {
	desc, err := s.Resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	raw, err := content.FetchAll(ctx, s.oci, desc)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			// the tag resolved, so it is the manifest blob that is missing
			return nil, fmt.Errorf("read manifest of %q: %w: %w", ref, ErrLayerMissing, err)
		}
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
