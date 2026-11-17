package toolcache

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"strconv"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/catalog"
)

// maxFuzzRead bounds how much of a returned member a fuzz body reads.
const maxFuzzRead = 1 << 20

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
		{"garbage zip", catalog.ContainerZip, []byte("this is not a zip archive at all"), "live/vmlinuz", ErrMalformed},
		{"garbage iso", catalog.ContainerISO, bytes.Repeat([]byte("garbage "), 8192), "boot/vmlinuz", ErrMalformed},
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

func TestOpenMember_ISOTruncatedMetadata(t *testing.T) {
	cut := readFixture(t)[:40<<10]
	r, _, err := open(catalog.ContainerISO, cut, "boot/vmlinuz")
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("error %v, want ErrMalformed", err)
	}
	if r != nil {
		t.Error("returned a reader alongside the error")
	}
}

// Each corruption makes go-diskfs v1.9.4 panic, both found by fuzzing: a slice
// out of range in parsePathTable (pathtable.go:196) and in parseTimestamps
// (rockridge.go:905). OpenMember must turn the panic into ErrMalformed.
func TestOpenMember_ISOParserPanicIsMalformed(t *testing.T) {
	const pvd = 16 * isoBlockSize // the primary volume descriptor
	tests := []struct {
		name    string
		corrupt func(t *testing.T, data []byte)
	}{
		{"path table entry overruns the table", func(_ *testing.T, data []byte) {
			// The path table size, in both byte orders, shorter than its first entry.
			binary.LittleEndian.PutUint32(data[pvd+132:], 3)
			binary.BigEndian.PutUint32(data[pvd+136:], 3)
		}},
		{"timestamps flagged beyond the entry", func(t *testing.T, data []byte) {
			t.Helper()
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
				t.Fatal("no TF entry in the fixture")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := readFixture(t)
			tt.corrupt(t, data)
			r, _, err := open(catalog.ContainerISO, data, "boot/vmlinuz")
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("error %v, want ErrMalformed", err)
			}
			if r != nil {
				t.Error("returned a reader alongside the error")
			}
		})
	}
}

// go-diskfs's File.Read invents the tail of a file cut short, so OpenMember
// must refuse a member whose declared bytes are not all in the image.
func TestOpenMember_ISOTruncatedData(t *testing.T) {
	full := readFixture(t)
	squashfs := seqText(40000)
	off := bytes.Index(full, squashfs[:64])
	if off < 0 {
		t.Fatal("the squashfs member is not in the fixture")
	}
	cut := full[:off+len(squashfs)/2]
	r, n, err := open(catalog.ContainerISO, cut, "live/tool-amd64/tool-amd64.squashfs")
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

func FuzzOpenMemberZip(f *testing.F) {
	for _, seed := range [][]byte{liveZip(f), symlinkZip(f), directoryZip(f), duplicateZip(f), []byte("this is not a zip archive at all")} {
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
	full := readFixture(f)
	f.Add(full)
	f.Add(full[:40<<10])
	f.Add(full[:bytes.Index(full, seqText(40000)[:64])+len(seqText(40000))/2])
	f.Fuzz(func(t *testing.T, data []byte) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			r, _, err := open(catalog.ContainerISO, data, "boot/vmlinuz")
			if err != nil {
				return
			}
			if _, err := io.Copy(io.Discard, io.LimitReader(r, maxFuzzRead)); err != nil {
				return
			}
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("OpenMember hung")
		}
	})
}
