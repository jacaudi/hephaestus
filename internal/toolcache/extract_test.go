package toolcache

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/catalog"
)

// maxFuzzRead bounds how much of a returned member a fuzz body reads.
const maxFuzzRead = 1 << 20

// openDeadline is how long a test waits for OpenMember before it calls a hang.
const openDeadline = 5 * time.Second

// zipEntry is one member a test zip is built from. A symlink entry carries
// its target as the body, as zip tools write it.
type zipEntry struct {
	name    string
	body    string
	symlink bool
}

// buildZip writes a zip holding entries in order. A name ending in "/" is a
// directory entry.
func buildZip(tb testing.TB, entries ...zipEntry) []byte {
	tb.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.symlink {
			h.SetMode(fs.ModeSymlink | 0o777)
		}
		fw, err := w.CreateHeader(h)
		if err != nil {
			tb.Fatalf("CreateHeader %q: %v", e.name, err)
		}
		if _, err := io.WriteString(fw, e.body); err != nil {
			tb.Fatalf("write %q: %v", e.name, err)
		}
	}
	if err := w.Close(); err != nil {
		tb.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func liveZip(tb testing.TB) []byte {
	tb.Helper()
	return buildZip(tb, zipEntry{name: "live/vmlinuz", body: "k"}, zipEntry{name: "live/initrd.img", body: "i"})
}

func symlinkZip(tb testing.TB) []byte {
	tb.Helper()
	return buildZip(tb, zipEntry{name: "live/vmlinuz", body: "../../etc/passwd", symlink: true})
}

func directoryZip(tb testing.TB) []byte {
	tb.Helper()
	return buildZip(tb, zipEntry{name: "live/"}, zipEntry{name: "live/vmlinuz", body: "k"})
}

func duplicateZip(tb testing.TB) []byte {
	tb.Helper()
	return buildZip(tb, zipEntry{name: "live/vmlinuz", body: "one"}, zipEntry{name: "live/vmlinuz", body: "two"})
}

// readFixture returns testdata/rr.iso, built by testdata/mkiso.sh.
func readFixture(tb testing.TB) []byte {
	tb.Helper()
	b, err := os.ReadFile("testdata/rr.iso")
	if err != nil {
		tb.Fatalf("read fixture: %v", err)
	}
	return b
}

// seqText is the output of `seq 1 n`.
func seqText(n int) []byte {
	var b []byte
	for i := 1; i <= n; i++ {
		b = strconv.AppendInt(b, int64(i), 10)
		b = append(b, '\n')
	}
	return b
}

// open calls OpenMember over data.
func open(c catalog.Container, data []byte, name string) (io.Reader, int64, error) {
	return OpenMember(c, bytes.NewReader(data), int64(len(data)), name)
}

// member opens name and reads its bytes, failing the test on any error.
func member(t *testing.T, c catalog.Container, data []byte, name string) ([]byte, int64) {
	t.Helper()
	r, n, err := open(c, data, name)
	if err != nil {
		t.Fatalf("OpenMember(%s, %q): %v", c, name, err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %q: %v", name, err)
	}
	return got, n
}

func TestOpenMember_Zip(t *testing.T) {
	data := liveZip(t)
	tests := []struct {
		name string
		want string
	}{
		{"live/vmlinuz", "k"},
		{"live/initrd.img", "i"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, n := member(t, catalog.ContainerZip, data, tt.name)
			if string(got) != tt.want || n != int64(len(tt.want)) {
				t.Errorf("got %q (declared %d), want %q (declared %d)", got, n, tt.want, len(tt.want))
			}
		})
	}
}

func TestOpenMember_Raw(t *testing.T) {
	data := []byte("MZ\x90\x00 an EFI binary")
	got, n := member(t, catalog.ContainerRaw, data, "tool.efi")
	if !bytes.Equal(got, data) || n != int64(len(data)) {
		t.Errorf("got %q (declared %d), want the whole object (%d bytes)", got, n, len(data))
	}
}

func TestOpenMember_ISO(t *testing.T) {
	data := readFixture(t)
	squashfs := seqText(40000)
	tests := []struct {
		name string
		want []byte
	}{
		{"boot/vmlinuz", []byte("kernel-fixture\n")},
		{"live/tool-amd64/tool-amd64.squashfs", squashfs},
		// Rock Ridge names across a multi-sector directory.
		{"many/long-rock-ridge-name-37.txt", []byte("37\n")},
		{"many/long-rock-ridge-name-01.txt", []byte("01\n")},
		{"many/long-rock-ridge-name-60.txt", []byte("60\n")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, n := member(t, catalog.ContainerISO, data, tt.name)
			if !bytes.Equal(got, tt.want) || n != int64(len(tt.want)) {
				t.Errorf("got %d bytes (declared %d), want %d bytes", len(got), n, len(tt.want))
			}
		})
	}
}

func TestOpenMember_UnsafeMembers(t *testing.T) {
	iso := readFixture(t)
	tests := []struct {
		name      string
		container catalog.Container
		data      []byte
		member    string
		want      error
	}{
		{"absolute name", catalog.ContainerZip, liveZip(t), "/abs", ErrUnsafeMember},
		{"dot dot name", catalog.ContainerZip, liveZip(t), "a/../b", ErrUnsafeMember},
		{"empty name", catalog.ContainerZip, liveZip(t), "", ErrUnsafeMember},
		{"absolute name raw", catalog.ContainerRaw, []byte("x"), "/abs", ErrUnsafeMember},
		{"absolute name iso", catalog.ContainerISO, iso, "/boot/vmlinuz", ErrUnsafeMember},
		{"zip symlink", catalog.ContainerZip, symlinkZip(t), "live/vmlinuz", ErrUnsafeMember},
		{"zip directory entry", catalog.ContainerZip, directoryZip(t), "live", ErrMemberNotFound},
		{"zip duplicate", catalog.ContainerZip, duplicateZip(t), "live/vmlinuz", ErrUnsafeMember},
		{"iso symlink", catalog.ContainerISO, iso, "boot/link", ErrUnsafeMember},
		{"iso directory", catalog.ContainerISO, iso, "boot", ErrUnsafeMember},
		{"zip missing", catalog.ContainerZip, liveZip(t), "live/missing", ErrMemberNotFound},
		{"iso missing", catalog.ContainerISO, iso, "boot/missing", ErrMemberNotFound},
		{"iso missing directory", catalog.ContainerISO, iso, "nosuch/vmlinuz", ErrMemberNotFound},
		{"iso missing nested directory", catalog.ContainerISO, iso, "boot/nosuch/deeper/vmlinuz", ErrMemberNotFound},
		{"iso path through a file", catalog.ContainerISO, iso, "boot/vmlinuz/x", ErrMemberNotFound},
		{"garbage zip", catalog.ContainerZip, garbageZip(), "live/vmlinuz", ErrMalformed},
		{"garbage iso", catalog.ContainerISO, garbageISO(), "boot/vmlinuz", ErrMalformed},
		{"empty iso", catalog.ContainerISO, nil, "boot/vmlinuz", ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _, err := open(tt.container, tt.data, tt.member)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error %v, want %v", err, tt.want)
			}
			if r != nil {
				t.Errorf("returned a reader alongside the error")
			}
		})
	}
}

func TestOpenMember_UnknownContainer(t *testing.T) {
	if _, _, err := open("tar", []byte("x"), "a"); err == nil {
		t.Fatal("an unknown container was opened")
	}
}

// Each image is rr.iso with one header field bent. go-diskfs v1.9.4 panics on
// the first two (found by fuzzing: a slice out of range in parsePathTable,
// pathtable.go:196, and in parseTimestamps, rockridge.go:905), follows SUSP
// continuation entries forever on the next two (an entry that points at
// itself, and an area that loops back to its start, growing the record each
// pass), allocates the size a path table claims on the next two, and wraps
// the failure to read
// an oversized directory in a *fs.PathError on the last. OpenMember must
// return ErrMalformed, promptly, and never a local-disk error.
func TestOpenMember_ISOCraftedMetadata(t *testing.T) {
	for _, img := range craftedISOs(t) {
		t.Run(img.name, func(t *testing.T) {
			r, _, err := openWithin(t, catalog.ContainerISO, img.data, "boot/vmlinuz")
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("error %v, want ErrMalformed", err)
			}
			if pe, ok := errors.AsType[*fs.PathError](err); ok {
				t.Errorf("a malformed image reports a local failure: %v", pe)
			}
			if r != nil {
				t.Error("returned a reader alongside the error")
			}
		})
	}
}

// A path table size from the header is allocated before anything is read, so
// 0xFFFFFF00 asks for 4 GiB on a 745 KB image: OpenMember must refuse it first.
func TestOpenMember_ISOPathTableSizeIsNotAllocated(t *testing.T) {
	const limit = 64 << 20
	for name, data := range map[string][]byte{
		"primary":       hugePathTableISO(t, isoPrimary),
		"supplementary": hugePathTableISO(t, isoSupplementary),
	} {
		t.Run(name, func(t *testing.T) {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, _, err := open(catalog.ContainerISO, data, "boot/vmlinuz")
			runtime.ReadMemStats(&after)
			if !errors.Is(err, ErrMalformed) {
				t.Errorf("error %v, want ErrMalformed", err)
			}
			if grew := after.TotalAlloc - before.TotalAlloc; grew > limit {
				t.Errorf("allocated %d bytes for a %d-byte image", grew, len(data))
			}
		})
	}
}

// A continuation area that loops back to its start makes go-diskfs append the
// area's entries to the record on every pass, so a budget of read calls alone
// lets the record grow to gigabytes before it runs out.
func TestOpenMember_ISOContinuationLoopIsBounded(t *testing.T) {
	const limit = 256 << 20
	data := loopingContinuationISO(t)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, _, err := openWithin(t, catalog.ContainerISO, data, "boot/vmlinuz")
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("error %v, want ErrMalformed", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > limit {
		t.Errorf("allocated %d bytes for a %d-byte image", grew, len(data))
	}
}

// go-diskfs's File.Read invents the tail of a file cut short, so OpenMember
// must refuse a member whose declared bytes are not all in the image.
func TestOpenMember_ISOTruncatedData(t *testing.T) {
	r, n, err := open(catalog.ContainerISO, truncatedSquashfsISO(t), "live/tool-amd64/tool-amd64.squashfs")
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("error %v, want ErrMalformed", err)
	}
	if r != nil || n != 0 {
		t.Errorf("returned a reader (%v) of %d bytes alongside the error", r, n)
	}
}

// errReaderAt fails every read with err.
type errReaderAt struct{ err error }

func (e errReaderAt) ReadAt([]byte, int64) (int, error) { return 0, e.err }

// A local read failure stays a *fs.PathError in the error chain so the
// caller can tell a failing disk from a malformed archive.
func TestOpenMember_ReadFailureKeepsPathError(t *testing.T) {
	pathErr := &fs.PathError{Op: "read", Path: "/var/cache/x.iso", Err: errors.New("input/output error")}
	for _, c := range []catalog.Container{catalog.ContainerZip, catalog.ContainerISO} {
		t.Run(string(c), func(t *testing.T) {
			_, _, err := OpenMember(c, errReaderAt{pathErr}, 1<<20, "boot/vmlinuz")
			if err == nil {
				t.Fatal("OpenMember succeeded over a failing reader")
			}
			if !errors.Is(err, pathErr) {
				t.Errorf("error %v lost the *fs.PathError", err)
			}
		})
	}
}

// failRangeReaderAt reads from ra, but fails with err any read that touches
// the bytes [from, to).
type failRangeReaderAt struct {
	ra       io.ReaderAt
	from, to int64
	err      error
}

func (f failRangeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < f.to && off+int64(len(p)) > f.from {
		return 0, f.err
	}
	return f.ra.ReadAt(p, off)
}

// The ancestor walk reads directories the parse itself does not, so a local
// read failure there must keep its *fs.PathError and never become
// ErrMemberNotFound.
func TestOpenMember_ISOWalkReadFailureKeepsPathError(t *testing.T) {
	data := readFixture(t)
	rec := isoBootRecord(t, data)
	from := int64(binary.LittleEndian.Uint32(data[rec+2:])) * isoBlockSize
	pathErr := &fs.PathError{Op: "read", Path: "/var/cache/x.iso", Err: errors.New("input/output error")}
	ra := failRangeReaderAt{ra: bytes.NewReader(data), from: from, to: from + int64(binary.LittleEndian.Uint32(data[rec+10:])), err: pathErr}
	size := int64(len(data))
	// Opening boot/ itself reads only the root directory, so a failure below
	// comes from walking into boot/.
	if _, _, err := OpenMember(catalog.ContainerISO, ra, size, "boot"); !errors.Is(err, ErrUnsafeMember) {
		t.Fatalf("open boot: error %v, want ErrUnsafeMember (the parse read the boot directory)", err)
	}
	for _, name := range []string{"boot/vmlinuz/x", "boot/nosuch/x"} {
		t.Run(name, func(t *testing.T) {
			_, _, err := OpenMember(catalog.ContainerISO, ra, size, name)
			if !errors.Is(err, pathErr) || errors.Is(err, ErrMemberNotFound) {
				t.Errorf("error %v, want the *fs.PathError and not ErrMemberNotFound", err)
			}
		})
	}
}

// namedImage is a test input with the name of its subtest.
type namedImage struct {
	name string
	data []byte
}

func garbageZip() []byte { return []byte("this is not a zip archive at all") }

func garbageISO() []byte { return bytes.Repeat([]byte("garbage "), 8192) }

// truncatedMetadataISO is rr.iso cut inside its directory metadata.
func truncatedMetadataISO(tb testing.TB) []byte {
	tb.Helper()
	return readFixture(tb)[:40<<10]
}

// truncatedSquashfsISO is rr.iso cut half way through the squashfs member.
func truncatedSquashfsISO(tb testing.TB) []byte {
	tb.Helper()
	full := readFixture(tb)
	squashfs := seqText(40000)
	off := bytes.Index(full, squashfs[:64])
	if off < 0 {
		tb.Fatal("the squashfs member is not in the fixture")
	}
	return full[:off+len(squashfs)/2]
}

// corruptISO returns rr.iso after fn has edited it.
func corruptISO(tb testing.TB, fn func(tb testing.TB, data []byte)) []byte {
	tb.Helper()
	data := readFixture(tb)
	fn(tb, data)
	return data
}

// putBoth32 writes v at b[0:4] little-endian and b[4:8] big-endian, ECMA-119's
// both-byte-order 32-bit field.
func putBoth32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
}

// isoDescriptor returns the offset of the first volume descriptor of type typ.
func isoDescriptor(tb testing.TB, data []byte, typ byte) int {
	tb.Helper()
	for off := isoDescriptorStart; off+isoBlockSize <= len(data) && string(data[off+1:off+6]) == "CD001"; off += isoBlockSize {
		if data[off] == typ {
			return off
		}
	}
	tb.Fatalf("no volume descriptor of type %d in the fixture", typ)
	return 0
}

// isoRootDir returns the offset and size of the root directory.
func isoRootDir(tb testing.TB, data []byte) (off, size int) {
	tb.Helper()
	record := isoDescriptor(tb, data, isoPrimary) + 156 // the root record, 34 bytes
	return int(binary.LittleEndian.Uint32(data[record+2:])) * isoBlockSize, int(binary.LittleEndian.Uint32(data[record+10:]))
}

// isoBootRecord returns the offset of the BOOT directory's record in the root
// directory: its extent's block is at +2 and its size at +10.
func isoBootRecord(tb testing.TB, data []byte) int {
	tb.Helper()
	root, size := isoRootDir(tb, data)
	i := bytes.Index(data[root:root+size], []byte{4, 'B', 'O', 'O', 'T'}) // the name length, then the name
	if i < 32 {
		tb.Fatal("no BOOT record in the root directory")
	}
	return root + i - 32
}

// hugePathTableISO claims a 0xFFFFFF00-byte path table in the descriptor of
// type typ.
func hugePathTableISO(tb testing.TB, typ byte) []byte {
	tb.Helper()
	return corruptISO(tb, func(tb testing.TB, data []byte) {
		tb.Helper()
		putBoth32(data[isoDescriptor(tb, data, typ)+isoPathTableSize:], 0xFFFFFF00)
	})
}

// ceBlock is the block of rr.iso's SUSP continuation area, which its root
// "." record points at; ceLen is the size of a CE entry.
const (
	ceBlock = 20
	ceLen   = 28
)

// ceEntry is a SUSP continuation entry for length bytes at block and offset.
func ceEntry(block, offset, length uint32) []byte {
	e := make([]byte, ceLen)
	copy(e, []byte{'C', 'E', ceLen, 1})
	putBoth32(e[4:], block)
	putBoth32(e[12:], offset)
	putBoth32(e[20:], length)
	return e
}

// continuationISO is rr.iso with area as the root "." record's continuation
// area, at the start of ceBlock.
func continuationISO(tb testing.TB, area []byte) []byte {
	tb.Helper()
	return corruptISO(tb, func(tb testing.TB, data []byte) {
		tb.Helper()
		root, _ := isoRootDir(tb, data)
		i := bytes.Index(data[root:root+int(data[root])], []byte{'C', 'E', ceLen, 1})
		if i < 0 {
			tb.Fatal("no CE entry in the root record")
		}
		copy(data[root+i:], ceEntry(ceBlock, 0, uint32(len(area))))
		copy(data[ceBlock*isoBlockSize:], area)
	})
}

// loopingContinuationISO has a block-sized continuation area of 4-byte SUSP
// padding entries that ends in a CE back to its own start. go-diskfs appends
// every pass's entries to the record, so each pass retains more memory.
func loopingContinuationISO(tb testing.TB) []byte {
	tb.Helper()
	pad := bytes.Repeat([]byte{'P', 'D', 4, 1}, (isoBlockSize-ceLen)/4)
	return continuationISO(tb, append(pad, ceEntry(ceBlock, 0, isoBlockSize)...))
}

// craftedISOs are the malformed images of TestOpenMember_ISOCraftedMetadata.
func craftedISOs(tb testing.TB) []namedImage {
	tb.Helper()
	return []namedImage{
		{"path table entry overruns the table", corruptISO(tb, func(tb testing.TB, data []byte) {
			tb.Helper()
			putBoth32(data[isoDescriptor(tb, data, isoPrimary)+isoPathTableSize:], 3) // shorter than its first entry
		})},
		{"timestamps flagged beyond the entry", corruptISO(tb, func(tb testing.TB, data []byte) {
			tb.Helper()
			// Every Rock Ridge TF entry (xorriso writes three short-form
			// timestamps, flags 0x0e) now claims all seven.
			tf := []byte{'T', 'F', 26, 1, 0x0e}
			hits := 0
			for off := 0; ; hits++ {
				i := bytes.Index(data[off:], tf)
				if i < 0 {
					break
				}
				data[off+i+4] = 0x7f
				off += i + len(tf)
			}
			if hits == 0 {
				tb.Fatal("no TF entry in the fixture")
			}
		})},
		{"continuation entry points at itself", continuationISO(tb, ceEntry(ceBlock, 0, ceLen))},
		{"continuation area loops back to its start", loopingContinuationISO(tb)},
		{"primary path table larger than the image", hugePathTableISO(tb, isoPrimary)},
		{"supplementary path table larger than the image", hugePathTableISO(tb, isoSupplementary)},
		{"directory larger than the image", corruptISO(tb, func(tb testing.TB, data []byte) {
			tb.Helper()
			putBoth32(data[isoBootRecord(tb, data)+10:], 1<<20) // the record's data length
		})},
		{"truncated metadata", truncatedMetadataISO(tb)},
		{"garbage", garbageISO()},
		{"empty", nil},
	}
}

// openResult is what OpenMember returned.
type openResult struct {
	r   io.Reader
	n   int64
	err error
}

// openWithin calls OpenMember in a goroutine and fails tb when it has not
// returned in openDeadline, so a hang is a failure and not a stuck run.
func openWithin(tb testing.TB, c catalog.Container, data []byte, name string) (io.Reader, int64, error) {
	tb.Helper()
	done := make(chan openResult, 1)
	go func() {
		r, n, err := open(c, data, name)
		done <- openResult{r, n, err}
	}()
	select {
	case res := <-done:
		return res.r, res.n, res.err
	case <-time.After(openDeadline):
		tb.Fatalf("OpenMember did not return within %s", openDeadline)
		return nil, 0, nil
	}
}

func FuzzOpenMemberZip(f *testing.F) {
	for _, seed := range [][]byte{liveZip(f), symlinkZip(f), directoryZip(f), duplicateZip(f), garbageZip()} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r, _, err := open(catalog.ContainerZip, data, "live/vmlinuz")
		if err != nil {
			return
		}
		if _, err := io.Copy(io.Discard, io.LimitReader(r, maxFuzzRead)); err != nil {
			return
		}
	})
}

func FuzzOpenMemberISO(f *testing.F) {
	f.Add(readFixture(f))
	f.Add(truncatedSquashfsISO(f))
	for _, img := range craftedISOs(f) {
		f.Add(img.data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r, _, err := openWithin(t, catalog.ContainerISO, data, "boot/vmlinuz")
		if err != nil {
			return
		}
		if _, err := io.Copy(io.Discard, io.LimitReader(r, maxFuzzRead)); err != nil {
			return
		}
	})
}
