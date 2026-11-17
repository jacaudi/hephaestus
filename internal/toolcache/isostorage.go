package toolcache

import (
	"errors"
	"io"
	"io/fs"
	"os"

	"github.com/diskfs/go-diskfs/backend"
)

// isoStorage makes an io.ReaderAt of known size the read-only backend.Storage
// go-diskfs's iso9660.Read wants. The embedded SectionReader supplies the
// fs.File, io.ReaderAt, io.Seeker part of the contract.
type isoStorage struct {
	*io.SectionReader
}

func newISOStorage(ra io.ReaderAt, size int64) isoStorage {
	return isoStorage{io.NewSectionReader(ra, 0, size)}
}

// Stat is not part of what iso9660.Read uses.
func (isoStorage) Stat() (fs.FileInfo, error) { return nil, errors.ErrUnsupported }

// Close releases nothing: the caller owns the io.ReaderAt.
func (isoStorage) Close() error { return nil }

// Sys has no OS file to give.
func (isoStorage) Sys() (*os.File, error) { return nil, errors.ErrUnsupported }

// Writable refuses: the storage is read-only.
func (isoStorage) Writable() (backend.WritableFile, error) { //nolint:ireturn // backend.Storage requires this return type
	return nil, backend.ErrIncorrectOpenMode
}

// Path is empty: there is no file behind the storage.
func (isoStorage) Path() string { return "" }
