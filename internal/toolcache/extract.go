package toolcache

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"

	"github.com/diskfs/go-diskfs/filesystem/iso9660"

	"go.jacaudi.dev/hephaestus/internal/catalog"
)

// isoBlockSize is the logical block size of an ISO9660 image, and so the unit
// of a member's Location.
const isoBlockSize = 2048

// Why OpenMember refused a member. Each error wraps the cause.
var (
	// ErrMemberNotFound means the object has no member of that name.
	ErrMemberNotFound = errors.New("toolcache: member not found")
	// ErrUnsafeMember means the name or the member is not one to extract: a
	// name that is not a clean relative path, a name two members share, or a
	// member that is not a regular file.
	ErrUnsafeMember = errors.New("toolcache: unsafe member")
	// ErrMalformed means the object is not the container it claims to be, or
	// is cut short.
	ErrMalformed = errors.New("toolcache: malformed object")
)

// OpenMember returns the bytes of the member name in the size-byte object ra
// holds, and the size the object declares for it. The container says how the
// object is packed; name must be a catalog.ValidPath. A raw object is its own
// only member, so name is checked but not looked up.
//
// A member that is not a regular file (a symlink or a directory), a name two
// zip members share, and an ISO member whose bytes are not all in the image
// are refused. The member's bytes are not verified: the caller hashes them.
// The reader needs no Close and reads through ra, so ra must stay valid until
// it is drained.
//
// A read error from ra is returned as it is, so a *fs.PathError stays in the
// chain; every other failure to parse the object wraps ErrMalformed.
func OpenMember(c catalog.Container, ra io.ReaderAt, size int64, name string) (io.Reader, int64, error) {
	if !catalog.ValidPath(name) {
		return nil, 0, fmt.Errorf("%w: %q is not a clean relative path", ErrUnsafeMember, name)
	}
	switch c {
	case catalog.ContainerRaw:
		return io.NewSectionReader(ra, 0, size), size, nil
	case catalog.ContainerZip:
		return openZip(ra, size, name)
	case catalog.ContainerISO:
		return openISO(ra, size, name)
	default:
		return nil, 0, fmt.Errorf("toolcache: unknown container %q", c)
	}
}

// readRecorder remembers the first read error from ra that is not a short
// read. The zip and go-diskfs parsers flatten the errors they get from ra, so
// without it a failing disk would look like a malformed object.
type readRecorder struct {
	ra  io.ReaderAt
	err error
}

func (r *readRecorder) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.ra.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && r.err == nil {
		r.err = err
	}
	return n, err
}

// parseError turns a parser's cause into OpenMember's error: the recorded
// read error if ra failed, else cause marked ErrMalformed.
func (r *readRecorder) parseError(cause error) error {
	if r.err != nil {
		return fmt.Errorf("toolcache: read object: %w", r.err)
	}
	return fmt.Errorf("%w: %w", ErrMalformed, cause)
}

func openZip(ra io.ReaderAt, size int64, name string) (io.Reader, int64, error) {
	rec := &readRecorder{ra: ra}
	zr, err := zip.NewReader(rec, size)
	if err != nil {
		return nil, 0, rec.parseError(err)
	}
	var found *zip.File
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		if found != nil {
			return nil, 0, fmt.Errorf("%w: the zip lists %q twice", ErrUnsafeMember, name)
		}
		found = f
	}
	if found == nil {
		return nil, 0, fmt.Errorf("%w: %q is not in the zip", ErrMemberNotFound, name)
	}
	if !found.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%w: %q is a %s, not a regular file", ErrUnsafeMember, name, found.Mode().Type())
	}
	if found.UncompressedSize64 > math.MaxInt64 {
		return nil, 0, fmt.Errorf("%w: %q declares %d bytes", ErrMalformed, name, found.UncompressedSize64)
	}
	rc, err := found.Open()
	if err != nil {
		return nil, 0, rec.parseError(err)
	}
	return rc, int64(found.UncompressedSize64), nil
}

// openISO reads one file from a Rock Ridge ISO9660 image. go-diskfs panics on
// some malformed images, so the parse runs under recover. Its File.Read
// invents the tail of a file cut short (it ignores the EOF from ReadAt), so
// the data is read through a SectionReader at the file's Location once the
// image is known to hold all of it.
func openISO(ra io.ReaderAt, size int64, name string) (r io.Reader, n int64, err error) {
	rec := &readRecorder{ra: ra}
	defer func() {
		if p := recover(); p != nil {
			r, n, err = nil, 0, fmt.Errorf("%w: iso9660 parser panic: %v", ErrMalformed, p)
		}
	}()
	// ponytail: the FileSystem is never Closed; read-only, it holds nothing.
	fsys, err := iso9660.Read(newISOStorage(rec, size), size, 0, isoBlockSize)
	if err != nil {
		return nil, 0, rec.parseError(err)
	}
	f, err := fsys.Open(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, 0, fmt.Errorf("%w: %q is not in the image", ErrMemberNotFound, name)
	case err != nil:
		return nil, 0, rec.parseError(err)
	}
	file, ok := f.(*iso9660.File)
	if !ok {
		return nil, 0, fmt.Errorf("%w: %q is a directory", ErrUnsafeMember, name)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, 0, rec.parseError(err)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%w: %q is a %s, not a regular file", ErrUnsafeMember, name, info.Mode().Type())
	}
	declared := info.Size()
	off := int64(file.Location()) * isoBlockSize
	if off+declared > size {
		return nil, 0, fmt.Errorf("%w: %q declares %d bytes at %d but the image is %d bytes", ErrMalformed, name, declared, off, size)
	}
	return io.NewSectionReader(ra, off, declared), declared, nil
}
