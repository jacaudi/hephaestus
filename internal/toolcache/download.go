package toolcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/otel/metric"
)

// object is one file a download brings into the spool.
type object struct {
	URL string
	// SpoolName is the spool file; its ETag sidecar is SpoolName + ".etag".
	SpoolName string
	// Size is the pinned size.
	Size int64
	// SHA256 is the pinned hex digest.
	SHA256 string
	// Index is the object's place among its row's objects, named in error text.
	Index int
}

// etagName is the sidecar that holds the strong ETag the spool was started under.
func (o object) etagName() string { return o.SpoolName + ".etag" }

// downloader brings objects into the spool for one job. The state is the
// job's: root is its spool handle, and downloaded and reserved point at its
// received-byte count and its free space reservation, which is also counted
// in f.reserved.
type downloader struct {
	f          *Fetcher
	root       *os.Root
	downloaded *int64
	reserved   *uint64
	bytes      metric.Int64Counter
	progress   func(ctx context.Context, done, total int64)
}

// fetch brings o into its spool file in full.
func (d *downloader) fetch(ctx context.Context, o object) error {
	have, etag, err := d.resumePoint(o)
	if err != nil {
		return err
	}
	if have == o.Size {
		// Whole already, so nothing is downloaded, but its extraction still writes
		// about its size into the store.
		if err := d.reserveSpace(o.Size, 1); err != nil {
			return err
		}
		d.progress(ctx, o.Size, o.Size)
		return nil
	}
	if err := d.reserveSpace(o.Size, 2); err != nil {
		return err
	}
	return d.transfer(ctx, o, have, etag)
}

// resumePoint says how much of o is already in its spool and under which
// strong ETag. A spool longer than the pin, or one with no strong ETag to
// resume under, is emptied.
func (d *downloader) resumePoint(o object) (have int64, etag string, err error) {
	fi, err := d.root.Stat(o.SpoolName)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("stat the spool: %w", err)
	}
	have = fi.Size()
	if have == o.Size || have == 0 {
		return have, "", nil
	}
	if have < o.Size {
		raw, err := d.root.ReadFile(o.etagName())
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, "", fmt.Errorf("read the ETag sidecar: %w", err)
		}
		if err == nil && strongETag(string(raw)) {
			return have, string(raw), nil
		}
	}
	return 0, "", d.reset(o)
}

// reset empties o's spool and removes its sidecar.
func (d *downloader) reset(o object) error {
	f, err := d.root.OpenFile(o.SpoolName, os.O_WRONLY|os.O_TRUNC, spoolFileMode)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("empty the spool: %w", err)
	default:
		if err := f.Close(); err != nil {
			return fmt.Errorf("empty the spool: %w", err)
		}
	}
	return d.dropETag(o)
}

func (d *downloader) dropETag(o object) error {
	if err := d.root.Remove(o.etagName()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the ETag sidecar: %w", err)
	}
	return nil
}

// keepETag stores etag as o's sidecar when it is strong, atomically, and
// removes the sidecar otherwise.
func (d *downloader) keepETag(o object, etag string) error {
	if !strongETag(etag) {
		return d.dropETag(o)
	}
	name := o.etagName()
	tmp := name + ".tmp"
	if err := d.root.WriteFile(tmp, []byte(etag), spoolFileMode); err != nil {
		return errors.Join(fmt.Errorf("write the ETag sidecar: %w", err), d.root.Remove(tmp))
	}
	if err := d.root.Rename(tmp, name); err != nil {
		return errors.Join(fmt.Errorf("replace the ETag sidecar: %w", err), d.root.Remove(tmp))
	}
	return nil
}

// reserveSpace refuses a download the spool volume has no room for, and
// otherwise holds the room for the job until close. The job's need is copies
// times the object (2: the spool and its extracted copy; 1 when the spool is
// already whole and only the extraction is left), and that is all it holds.
// spaceReserve is the margin the volume keeps whatever runs on it, so it is
// counted once, not once per download. The need must fit in what is free less
// the margin and less what the other downloads in flight hold, because their
// bytes are not all on the disk yet.
//
// A need that does not fit even alone, that is, without the others' holds, is
// an ordinary error: no wait would change it. One that fits alone but not beside
// the others is errSpaceBusy, which the worker does not count as a failure.
// A job that reserved for an earlier object swaps that reservation for this
// one: those bytes are on the disk by now.
func (d *downloader) reserveSpace(size int64, copies uint64) error {
	f := d.f
	// Past this, the need below does not fit in a uint64; no volume holds it.
	if uint64(max(size, 0)) > (math.MaxUint64-spaceReserve)/copies {
		return fmt.Errorf("free space on the spool volume cannot hold a %d byte object", size)
	}
	need := copies * uint64(max(size, 0))

	// ponytail: statfs runs under the lock so check and reserve are one step;
	// a slow volume delays only other reservations, and a measured delay can move it out.
	f.spaceMu.Lock()
	defer f.spaceMu.Unlock()
	free, err := f.statfs(f.cfg.SpoolDir)
	if err != nil {
		return fmt.Errorf("free space on the spool volume: %w", err)
	}
	if free < need+spaceReserve {
		return fmt.Errorf("free space on the spool volume is %d bytes, a %d byte object needs %d and the %d byte margin", free, size, need, spaceReserve)
	}
	others := f.reserved - *d.reserved
	if free-min(free, others) < need+spaceReserve {
		return fmt.Errorf("%w: free space is %d bytes with %d reserved by other downloads, a %d byte object needs %d and the %d byte margin",
			errSpaceBusy, free, others, size, need, spaceReserve)
	}
	f.reserved, *d.reserved = others+need, need
	return nil
}

// transfer requests o, from byte have under etag when etag is set, and writes
// the body into the spool.
func (d *downloader) transfer(ctx context.Context, o object, have int64, etag string) (err error) {
	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, o.URL, http.NoBody)
	if err != nil {
		return fmt.Errorf("build the request for object %d: %w", o.Index, RedactURLError(err))
	}
	if etag != "" {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
		req.Header.Set("If-Range", etag)
	}
	resp, err := d.f.cfg.Client.Do(req)
	if err != nil {
		return fmt.Errorf("get object %d: %w", o.Index, RedactURLError(err))
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()

	offset, err := d.accept(resp, o, have)
	if err != nil {
		return err
	}
	body := newIdleReader(resp.Body, d.f.idle, cancel)
	defer body.Stop()
	// The floor counts only what this response sends, never the offset's bytes
	// that an earlier attempt left in the spool.
	return d.write(ctx, newFloorReader(body, d.f.now), o, offset)
}

// accept checks the response to o's request against the pin and returns the
// spool offset its body continues at. A 206 is checked in this order: the
// Content-Range start, then its total, then its end, then the Content-Length.
func (d *downloader) accept(resp *http.Response, o object, have int64) (int64, error) {
	size := o.Size
	switch resp.StatusCode {
	case http.StatusOK:
		if resp.ContentLength >= 0 && resp.ContentLength != size {
			return 0, integrityf("the upstream sent %d bytes for an object pinned at %d", resp.ContentLength, size)
		}
		return 0, d.keepETag(o, resp.Header.Get("ETag"))
	case http.StatusPartialContent:
		header := resp.Header.Get("Content-Range")
		start, end, total, err := parseContentRange(header)
		if err != nil || start != have {
			return 0, errors.Join(fmt.Errorf("a 206 with Content-Range %q does not continue at byte %d", header, have), err, d.reset(o))
		}
		if total != size {
			return 0, integrityf("Content-Range %q says the object is %d bytes, pinned at %d", header, total, size)
		}
		// Only the total is a fact about the object (minor-16): a range that
		// stops short is a misbehaving server, which a retry may not meet. It is
		// checked before the Content-Length, which a short range also disagrees with.
		if end != size-1 {
			return 0, fmt.Errorf("a 206 with Content-Range %q ends at byte %d, not at the last byte %d", header, end, size-1)
		}
		if resp.ContentLength >= 0 && resp.ContentLength != size-have {
			return 0, integrityf("the upstream sent %d bytes for the %d the range asked for", resp.ContentLength, size-have)
		}
		return have, nil
	default:
		return 0, fmt.Errorf("the upstream answered %q", resp.Status)
	}
}

// write copies body into o's spool from offset, and records the progress.
// Bytes past the pinned size are an integrity failure; a body that ends short
// is not, and its spool is kept.
func (d *downloader) write(ctx context.Context, body io.Reader, o object, offset int64) (err error) {
	flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
	if offset == 0 {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	file, err := d.root.OpenFile(o.SpoolName, flags, spoolFileMode)
	if err != nil {
		return fmt.Errorf("open the spool: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()

	want := o.Size - offset
	w := &spoolWriter{file: file, last: time.Now()}
	w.tick = func() { d.progress(ctx, offset+w.n, o.Size) }
	// One byte past the end is enough to see an oversize body.
	copied, err := io.Copy(w, io.LimitReader(body, want+1))
	*d.downloaded += w.n
	d.bytes.Add(ctx, w.n)
	// The floor judges a Read after its bytes were returned, so it can cut the Read
	// that delivered the object's last byte. The spool is then whole: no failure.
	if copied == want && errors.Is(err, errTooSlow) {
		err = nil
	}
	switch {
	case err != nil:
		return fmt.Errorf("copy object %d: %w", o.Index, err)
	case copied > want:
		return integrityf("the upstream sent more than the pinned %d bytes", o.Size)
	case copied < want:
		return fmt.Errorf("the body of object %d ended at byte %d of %d", o.Index, offset+copied, o.Size)
	}
	d.progress(ctx, o.Size, o.Size)
	return nil
}

// verify checks that o's whole spool is the pinned size and sha256.
func (d *downloader) verify(ctx context.Context, o object) (err error) {
	f, err := d.open(o)
	if err != nil {
		return fmt.Errorf("open the spool: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	h := sha256.New()
	size, err := io.Copy(h, ctxReader{r: f, err: ctx.Err})
	if err != nil {
		return fmt.Errorf("hash the spool: %w", err)
	}
	if size != o.Size {
		return integrityf("the spool holds %d bytes, the pin says %d", size, o.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != o.SHA256 {
		return integrityf("the object's sha256 is %s, the pin says %s", got, o.SHA256)
	}
	return nil
}

// open opens o's spool file for reading, for extraction or a push.
func (d *downloader) open(o object) (*os.File, error) {
	return d.root.Open(o.SpoolName)
}

// discard removes the spool file of each object and its sidecars. A file that
// is already gone is no error.
func (d *downloader) discard(_ context.Context, objects []object) error {
	var errs []error
	for _, o := range objects {
		for _, name := range []string{o.SpoolName, o.etagName(), o.etagName() + ".tmp"} {
			if err := d.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// close releases the job's free space reservation from f.reserved and closes
// its handle on the spool directory, which may be nil.
func (d *downloader) close(ctx context.Context) {
	d.f.spaceMu.Lock()
	d.f.reserved -= *d.reserved
	d.f.spaceMu.Unlock()
	if d.root != nil {
		if err := d.root.Close(); err != nil {
			d.f.onError(ctx, fmt.Errorf("close the spool dir: %w", err))
		}
	}
}
