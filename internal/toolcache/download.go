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
	"strconv"
	"time"

	"go.opentelemetry.io/otel/metric"

	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// errRestart is accept's answer to a response that cannot continue the spool:
// the spool is emptied, and fetch asks again from byte zero, once.
var errRestart = errors.New("the upstream does not continue the spool")

// StatusError is a download answered with a status other than 200 and 206.
type StatusError struct {
	// Code is the HTTP status code.
	Code int
	// Body is the start of the response body as printable text, at most
	// fleet.MaxCauseBytes long: an upstream often says there why it refused.
	Body string
}

// Error implements error.
func (e *StatusError) Error() string {
	s := "the upstream answered " + strconv.Itoa(e.Code)
	if text := http.StatusText(e.Code); text != "" {
		s += " " + text
	}
	if e.Body != "" {
		s += ": " + e.Body
	}
	return s
}

// object is one file a download brings into the spool.
type object struct {
	URL string
	// SpoolName is the spool file; its ETag sidecar is SpoolName + ".etag".
	SpoolName string
	// Size is the pinned size; 0 means it is not known, and the response's
	// headers give it.
	Size int64
	// Max is the largest size an object of unknown size may have.
	Max int64
	// SHA256 is the pinned hex digest; empty means the object is not pinned.
	SHA256 string
	// Index is the object's place among its row's objects, named in error text.
	Index int
}

// etagName is the sidecar that holds the strong ETag the spool was started under.
func (o object) etagName() string { return o.SpoolName + ".etag" }

// limit is the most bytes the object may have: its size, or Max when the size
// is not known.
func (o object) limit() int64 {
	if o.Size == 0 {
		return o.Max
	}
	return o.Size
}

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

// fetch brings o into its spool file in full. A pinned o has its space
// reserved before the request; one of unknown size has it reserved once the
// response says how big it is.
func (d *downloader) fetch(ctx context.Context, o object) error {
	have, etag, err := d.resumePoint(o)
	if err != nil {
		return err
	}
	if o.Size != 0 {
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
	}
	err = d.transfer(ctx, o, have, etag)
	if errors.Is(err, errRestart) {
		// accept emptied the spool; the second try asks for no range, so it cannot
		// restart again, and a second mismatch is an ordinary error.
		err = d.transfer(ctx, o, 0, "")
	}
	return err
}

// resumePoint says how much of o is already in its spool and under which
// strong ETag. A spool longer than the object may be, or one with no strong
// ETag to resume under, is emptied.
func (d *downloader) resumePoint(o object) (have int64, etag string, err error) {
	fi, err := d.root.Stat(o.SpoolName)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("stat the spool: %w", err)
	}
	have = fi.Size()
	if have == 0 || have == o.Size {
		return have, "", nil
	}
	if have < o.limit() {
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

	offset, size, err := d.accept(resp, o, have, etag)
	if err != nil {
		return err
	}
	if o.Size == 0 {
		o.Size = size
		if err := d.reserveSpace(size, 2); err != nil {
			return err
		}
	}
	body := newIdleReader(resp.Body, d.f.idle, cancel)
	defer body.Stop()
	// The floor counts only what this response sends, never the offset's bytes
	// that an earlier attempt left in the spool.
	return d.write(ctx, newFloorReader(body, d.f.now), o, offset)
}

// accept checks the response to o's request, made from byte have under etag
// when etag is set, and returns the spool offset its body continues at and the
// size of the object: the pin's, or for an object of unknown size the one the
// headers give, which Max bounds. A 416 to a ranged request means the range
// starts at or past the end of what the upstream has, so the spool is emptied
// and errRestart returned.
func (d *downloader) accept(resp *http.Response, o object, have int64, etag string) (offset, size int64, err error) {
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && etag != "" {
		return 0, 0, errors.Join(errRestart, d.reset(o))
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, 0, statusError(resp)
	}
	size = o.Size
	if size == 0 {
		if size, err = responseSize(resp); err != nil {
			return 0, 0, err
		}
		if size > o.Max {
			return 0, 0, integrityf("the upstream says the object is %d bytes, over the %d it may be", size, o.Max)
		}
	}
	if resp.StatusCode == http.StatusPartialContent {
		return have, size, d.acceptRange(resp, o, size, have, etag)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != size {
		return 0, 0, integrityf("the upstream sent %d bytes for an object pinned at %d", resp.ContentLength, size)
	}
	return 0, size, d.keepETag(o, resp.Header.Get("ETag"))
}

// acceptRange checks a 206 to o's request for the bytes from have on, against
// size. It is checked in this order: the Content-Range start, then its total,
// then its end, then the Content-Length, then that its strong ETag is the
// sidecar's etag. A 206 under another ETag comes from other bytes than the
// spool holds, so the spool is emptied and errRestart returned.
func (d *downloader) acceptRange(resp *http.Response, o object, size, have int64, etag string) error {
	header := resp.Header.Get("Content-Range")
	start, end, total, err := parseContentRange(header)
	if err != nil || start != have {
		return errors.Join(fmt.Errorf("a 206 with Content-Range %q does not continue at byte %d", header, have), err, d.reset(o))
	}
	if total != size {
		return integrityf("Content-Range %q says the object is %d bytes, pinned at %d", header, total, size)
	}
	// Only the total is a fact about the object (minor-16): a range that
	// stops short is a misbehaving server, which a retry may not meet. It is
	// checked before the Content-Length, which a short range also disagrees with.
	if end != size-1 {
		return fmt.Errorf("a 206 with Content-Range %q ends at byte %d, not at the last byte %d", header, end, size-1)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != size-have {
		return integrityf("the upstream sent %d bytes for the %d the range asked for", resp.ContentLength, size-have)
	}
	switch got := resp.Header.Get("ETag"); {
	case etag == "":
		return errors.New("a 206 answered a request that asked for no range")
	case got != etag:
		// Some upstreams answer 206 whatever If-Range says (RFC 9110 section 13.1.5
		// says they must not); the bytes they send may belong to another version.
		return errors.Join(errRestart, d.reset(o))
	}
	return nil
}

// statusError is the *StatusError for resp, its body read up to
// fleet.MaxCauseBytes. A body that breaks off is noted after the status, which
// stays the error.
func statusError(resp *http.Response) error {
	text, err := io.ReadAll(io.LimitReader(resp.Body, fleet.MaxCauseBytes))
	se := &StatusError{Code: resp.StatusCode, Body: fleet.PrintableText(string(text))}
	if err != nil {
		return fmt.Errorf("%w (its body broke off: %w)", se, err)
	}
	return se
}

// responseSize is the size of the object a 200 or a 206 says it has: the
// Content-Length of a 200, the total of a 206's Content-Range. Either must be
// stated, and an object has at least one byte.
func responseSize(resp *http.Response) (int64, error) {
	switch resp.StatusCode {
	case http.StatusOK:
		if resp.ContentLength < 1 {
			return 0, integrityf("the upstream does not give a size for the object (Content-Length %d)", resp.ContentLength)
		}
		return resp.ContentLength, nil
	case http.StatusPartialContent:
		_, _, total, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return 0, integrityf("the upstream does not give a size for the object: %w", err)
		}
		return total, nil
	default:
		return 0, fmt.Errorf("a %d response has no object size", resp.StatusCode)
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

// verify checks o's whole spool against the pin: its size unless that is
// unknown, and its sha256 unless o is unpinned.
func (d *downloader) verify(ctx context.Context, o object) error {
	sum, size, err := d.digest(ctx, o)
	if err != nil {
		return err
	}
	if o.Size != 0 && size != o.Size {
		return integrityf("the spool holds %d bytes, the pin says %d", size, o.Size)
	}
	if o.SHA256 != "" && sum != o.SHA256 {
		return integrityf("the object's sha256 is %s, the pin says %s", sum, o.SHA256)
	}
	return nil
}

// digest hashes o's whole spool and returns the hex sha256 and the size.
func (d *downloader) digest(ctx context.Context, o object) (sha256Hex string, size int64, err error) {
	f, err := d.open(o)
	if err != nil {
		return "", 0, fmt.Errorf("open the spool: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	h := sha256.New()
	size, err = io.Copy(h, ctxReader{r: f, err: ctx.Err})
	if err != nil {
		return "", 0, fmt.Errorf("hash the spool: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
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
