package toolcache

import (
	"errors"
	"io"
	"io/fs"
	"os"

	"github.com/diskfs/go-diskfs/backend"
)

// maxParseReads is how many ReadAt calls one parse may make. A real Open makes
// a handful. go-diskfs follows SUSP continuation chains in an unbounded loop,
// so an entry that continues into itself would spin forever and recover cannot
// stop it; past the budget every read fails, which ends the loop.
const maxParseReads = 1 << 16

// errReadBudget is what a read past maxParseReads fails with.
var errReadBudget = errors.New("read budget exhausted")

// isoStorage makes an io.ReaderAt of known size the read-only backend.Storage
// go-diskfs's iso9660.Read wants. The embedded SectionReader supplies Read and
// Seek; ReadAt, which counts against maxParseReads, and the rest are here.
type isoStorage struct {
	*io.SectionReader

	reads int
}

func newISOStorage(ra io.ReaderAt, size int64) *isoStorage {
	return &isoStorage{SectionReader: io.NewSectionReader(ra, 0, size)}
}

// ReadAt reads from the object until the parse has used its read budget.
func (s *isoStorage) ReadAt(p []byte, off int64) (int, error) {
	s.reads++
	if s.reads > maxParseReads {
		return 0, errReadBudget
	}
	return s.SectionReader.ReadAt(p, off)
}

// Stat is not part of what iso9660.Read uses.
func (*isoStorage) Stat() (fs.FileInfo, error) { return nil, errors.ErrUnsupported }

// Close releases nothing: the caller owns the io.ReaderAt.
func (*isoStorage) Close() error { return nil }

// Sys has no OS file to give.
func (*isoStorage) Sys() (*os.File, error) { return nil, errors.ErrUnsupported }

// Writable refuses: the storage is read-only.
func (*isoStorage) Writable() (backend.WritableFile, error) { //nolint:ireturn // backend.Storage requires this return type
	return nil, backend.ErrIncorrectOpenMode
}

// Path is empty: there is no file behind the storage.
func (*isoStorage) Path() string { return "" }
