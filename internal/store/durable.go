package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"syscall"

	"github.com/oras-project/oras-go/v3/content"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// ingestDir is where a blob is written before it is renamed into place.
	ingestDir = "ingest"
	// blobsDir is the layout's blobs directory, and blobDir the one every sha256
	// blob lives in.
	blobsDir = ocispec.ImageBlobsDir
	blobDir  = blobsDir + "/sha256"
	// dirMode is the mode of the directories Open creates; blobMode is the mode
	// of a stored blob, read-only as oras-go stores it.
	dirMode  fs.FileMode = 0o750
	blobMode fs.FileMode = 0o444
)

// ErrDirSyncUnsupported is what the error handler is told, once, when the
// filesystem refuses to fsync a directory (EINVAL or ENOTSUP). The store goes
// on without that guarantee; the caller decides how loud to be.
var ErrDirSyncUnsupported = errors.New("directory fsync is not supported here")

// writeBlob stores the blob of desc, read from r, durably. It writes an ingest
// file under the store root, verifies its size and digest, syncs it, renames it
// over blobs/sha256/<hex> (so a blob already there is never absent in between)
// and fsyncs blobs/sha256.
func (s *Store) writeBlob(ctx context.Context, desc ocispec.Descriptor, r io.Reader) error {
	rel, err := blobRel(desc.Digest)
	if err != nil {
		return err
	}
	err = s.inRoot(func(root *os.Root) error { return s.ingest(root, desc, rel, r) })
	if err == nil {
		err = s.syncDir(ctx, path.Dir(rel))
	}
	if err != nil {
		return fmt.Errorf("write blob %s: %w", desc.Digest, err)
	}
	return nil
}

// ingest writes r to a new file under ingestDir, verified against desc and
// synced, then renames it to rel. A failure leaves no ingest file behind.
func (s *Store) ingest(root *os.Root, desc ocispec.Descriptor, rel string, r io.Reader) error {
	tmp := path.Join(ingestDir, rand.Text())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, blobMode)
	if err != nil {
		return fmt.Errorf("create ingest file: %w", err)
	}
	vr := content.NewVerifyReader(r, desc)
	_, err = io.Copy(f, vr)
	if err == nil {
		err = vr.Verify()
	}
	if err == nil {
		err = s.syncFile(f)
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		err = root.Rename(tmp, rel)
	}
	if err != nil {
		return errors.Join(err, root.Remove(tmp))
	}
	return nil
}

// syncPresent hashes the blob file at rel and, when it is desc's, syncs it; ok
// reports whether it was. A file of other content is left for the caller to
// replace.
func (s *Store) syncPresent(desc ocispec.Descriptor, rel string) (ok bool, err error) {
	err = s.inRoot(func(root *os.Root) (err error) {
		f, err := root.Open(rel)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, f.Close()) }()
		v := desc.Digest.Verifier()
		if _, err := io.Copy(v, f); err != nil {
			return err
		}
		if ok = v.Verified(); !ok {
			return nil
		}
		return s.syncFile(f)
	})
	return ok, err
}

// syncDir fsyncs the directory rel under the store root. A filesystem that
// cannot fsync a directory (EINVAL, ENOTSUP) is told to the error handler once,
// as ErrDirSyncUnsupported, and is not an error; any other failure is returned.
func (s *Store) syncDir(ctx context.Context, rel string) error {
	err := s.inRoot(func(root *os.Root) error { return s.syncDirFn(root, rel) })
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP):
		s.dirSyncWarned.Do(func() {
			s.onError(ctx, fmt.Errorf("%w: fsync %s: %w", ErrDirSyncUnsupported, rel, err))
		})
		return nil
	default:
		return fmt.Errorf("sync directory %s: %w", rel, err)
	}
}

// fsyncDir fsyncs the directory rel under root.
func fsyncDir(root *os.Root, rel string) (err error) {
	d, err := root.Open(rel)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, d.Close()) }()
	return d.Sync()
}

// prepare creates the directories the durable write path needs, which oras-go
// leaves to its own Push, then fsyncs them and the root, so a layout that Open
// returned survives a power cut as a layout.
func (s *Store) prepare(ctx context.Context) error {
	err := s.inRoot(func(root *os.Root) error {
		for _, dir := range []string{ingestDir, blobDir} {
			if err := root.MkdirAll(dir, dirMode); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, dir := range []string{blobDir, blobsDir, "."} {
		if err := s.syncDir(ctx, dir); err != nil {
			return err
		}
	}
	return nil
}
