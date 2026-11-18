package toolcache

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/store"
)

const (
	// PollInterval is how often Run looks for a row to fetch.
	PollInterval = 10 * time.Second
	// IdleTimeout is how long a download may receive no data before it is
	// abandoned. It restarts on every read that makes progress; it is not a
	// total timeout.
	IdleTimeout = 60 * time.Second
	// MinThroughput is the least the upstream must send, in bytes per second,
	// averaged over ThroughputWindow. An upstream that sends a byte just often
	// enough to keep the idle timeout from firing would otherwise hold a worker
	// for good. It is not configurable.
	MinThroughput = 32 << 10
	// ThroughputWindow is the span MinThroughput is averaged over.
	ThroughputWindow = 2 * time.Minute

	// DefaultFetchWorkers is how many downloads run at once when
	// FetcherConfig.Workers is 0.
	DefaultFetchWorkers = 4
	// MaxFetchWorkers is the most downloads FetcherConfig.Workers may ask for at once.
	MaxFetchWorkers = 16

	// progressEvery bounds how often a download records its progress.
	progressEvery = 5 * time.Second

	// The FetchEvent classes of a failure.
	classIntegrity = "integrity"
	classOther     = "other"

	// spaceReserve is what a download leaves free on the spool volume. The data
	// directory also holds the SQLite database, which needs room for its journal
	// and for growth, and a full disk would stop serve, not just the fetcher.
	spaceReserve = 1 << 30

	spoolFileMode = 0o600
	spoolDirMode  = 0o700
)

// locationParseFailure starts the message net/http gives a redirect whose
// Location header does not parse; the message holds the raw header.
const locationParseFailure = "failed to parse Location header"

// errUnparsableLocation replaces that message, so a redirect target's query
// never reaches an event, a handler or a stored error.
var errUnparsableLocation = errors.New("unparsable Location header")

// errIdle is why a download was cancelled: no data arrived within the idle
// timeout.
var errIdle = errors.New("idle timeout: no data from the upstream")

// errTooSlow is why a download was abandoned: the upstream sent fewer than
// MinThroughput bytes per second over ThroughputWindow. It is not an integrity
// failure; a retry may meet a faster server.
var errTooSlow = fmt.Errorf("the upstream sent fewer than %d KiB/s over %s", MinThroughput>>10, ThroughputWindow)

// FetcherConfig is what a Fetcher works on. Every field but Workers is required.
type FetcherConfig struct {
	// Repo holds the utility_cache rows the fetcher claims and settles.
	Repo *fleet.Repository
	// Store receives the extracted files.
	Store *store.Store
	// Catalog says what each row's pin fixes.
	Catalog *catalog.Catalog
	// Client is the upstream client, NewHTTPClient's.
	Client *http.Client
	// SpoolDir holds the partial downloads. It is created when missing.
	SpoolDir string
	// Workers is how many downloads run at once, from 1 to MaxFetchWorkers. 0
	// means DefaultFetchWorkers.
	Workers int
}

// FetchEvent is one state change of a cache row the fetcher made.
type FetchEvent struct {
	Key      fleet.CacheKey
	Pin      string
	From, To fleet.CacheState
	// Attempts is the row's failed attempts after the change.
	Attempts int
	// Class is "" for a change that is not a failure, "integrity" for a failure
	// no retry can fix and "other" for any other failure.
	Class string
	// Err is the failure's cause as printable text, with the userinfo, query and fragment of any URL removed.
	Err error
	// Bytes is how many bytes this run downloaded.
	Bytes    int64
	Duration time.Duration
}

// FetcherOption configures a Fetcher.
type FetcherOption func(*Fetcher)

// WithClock sets the clock behind the times written to the database.
func WithClock(now func() time.Time) FetcherOption { return func(f *Fetcher) { f.now = now } }

// WithMeter sets the meter the fetcher's counters are created on.
func WithMeter(m metric.Meter) FetcherOption { return func(f *Fetcher) { f.meter = m } }

// WithEvents sets the function called with every state change. Several workers
// call it at once, so fn must be safe for concurrent use.
func WithEvents(fn func(context.Context, FetchEvent)) FetcherOption {
	return func(f *Fetcher) { f.events = fn }
}

// WithErrorHandler sets the function called with every error the fetcher
// cannot settle on a row: an unrecorded repository error, a progress record
// that failed, or a spool file or the spool directory it could not remove or
// close. Several workers call it at once, so fn must be safe for concurrent use.
func WithErrorHandler(fn func(context.Context, error)) FetcherOption {
	return func(f *Fetcher) { f.onError = fn }
}

// Fetcher downloads the objects of wanted cache rows, verifies them against
// the catalog's pins, extracts their files into the store and settles each
// row. Run it once: its workers fetch rows concurrently, each row by one worker.
type Fetcher struct {
	cfg      FetcherConfig
	now      func() time.Time
	meter    metric.Meter
	events   func(context.Context, FetchEvent)
	onError  func(context.Context, error)
	bytes    metric.Int64Counter
	failures metric.Int64Counter

	// Test hooks.
	poll   time.Duration
	idle   time.Duration
	statfs func(path string) (free uint64, err error)

	// spaceMu guards reserved: the free space on the spool volume that the
	// downloads in flight have claimed.
	spaceMu  sync.Mutex
	reserved uint64
}

// NewFetcher returns a Fetcher over cfg and creates cfg.SpoolDir if it is
// missing. Every problem with cfg is reported together.
func NewFetcher(cfg FetcherConfig, opts ...FetcherOption) (*Fetcher, error) {
	var errs []error
	for _, missing := range []struct {
		name string
		is   bool
	}{
		{"Repo", cfg.Repo == nil}, {"Store", cfg.Store == nil}, {"Catalog", cfg.Catalog == nil},
		{"Client", cfg.Client == nil}, {"SpoolDir", cfg.SpoolDir == ""},
	} {
		if missing.is {
			errs = append(errs, fmt.Errorf("toolcache: fetcher config has no %s", missing.name))
		}
	}
	if cfg.Workers < 0 || cfg.Workers > MaxFetchWorkers {
		errs = append(errs, fmt.Errorf("toolcache: fetcher config Workers is %d, want 1 to %d (0 means %d)", cfg.Workers, MaxFetchWorkers, DefaultFetchWorkers))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	cfg.Workers = cmp.Or(cfg.Workers, DefaultFetchWorkers)

	f := &Fetcher{
		cfg:     cfg,
		now:     time.Now,
		meter:   noop.NewMeterProvider().Meter(""),
		events:  func(context.Context, FetchEvent) {},
		onError: func(context.Context, error) {},
		poll:    PollInterval,
		idle:    IdleTimeout,
		statfs:  diskFree,
	}
	for _, opt := range opts {
		opt(f)
	}

	var err error
	f.bytes, err = f.meter.Int64Counter("hephaestus.utility.fetch.bytes", metric.WithUnit("By"),
		metric.WithDescription("Bytes downloaded from upstreams."))
	if err != nil {
		return nil, fmt.Errorf("toolcache: create the fetch bytes counter: %w", err)
	}
	f.failures, err = f.meter.Int64Counter("hephaestus.utility.fetch.failures",
		metric.WithDescription("Failed fetches, by class: integrity or other."))
	if err != nil {
		return nil, fmt.Errorf("toolcache: create the fetch failures counter: %w", err)
	}
	if err := os.MkdirAll(cfg.SpoolDir, spoolDirMode); err != nil {
		return nil, fmt.Errorf("toolcache: create the spool dir: %w", err)
	}
	return f, nil
}

// Run starts Workers fetch loops and returns nil once ctx is done and every
// loop has returned. It never returns an error: each runOnce error other than
// ctx's own goes to the error handler and the loop goes on.
func (f *Fetcher) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for range f.cfg.Workers {
		wg.Go(func() { (&worker{f: f}).run(ctx) })
	}
	wg.Wait()
	return nil
}

// worker is one fetch loop. It owns the outcome the database refused it, so it
// only ever resets the row it claimed itself.
type worker struct {
	f *Fetcher
	// held is an outcome the database refused, retried on the next poll.
	held *heldOutcome
}

// run settles rows until ctx is done. After a row it claims again at once; when
// nothing was claimed, or an outcome could not be stored, it waits one poll
// interval, so a failing database is not asked again in a tight loop.
func (w *worker) run(ctx context.Context) {
	for ctx.Err() == nil {
		claimed, err := w.runOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.f.onError(ctx, err)
		}
		if claimed && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(w.f.poll):
		}
	}
}

// outcome is how one claimed row ended: ready, or a failure to record.
type outcome struct {
	ready bool
	fail  fleet.FetchFailure
	// ev is the event to emit once the outcome is stored, less To and Attempts.
	ev FetchEvent
}

// heldOutcome is an outcome the database refused. retried is set once the
// next poll has tried it again.
type heldOutcome struct {
	outcome

	retried bool
	// cause is the latest error that kept the outcome from being stored.
	cause error
}

// runOnce settles one row. It reports false when nothing was claimed, and true
// when a row was: with a nil error when its outcome (ready or a recorded
// failure) was stored, with ctx's error when ctx ended the fetch, which leaves
// the row fetching and its spool in place, and otherwise with the repository
// error that kept the outcome from being stored.
func (w *worker) runOnce(ctx context.Context) (bool, error) {
	if w.held != nil {
		return w.retryHeld(ctx)
	}
	f := w.f
	row, ok, err := f.cfg.Repo.ClaimFetch(ctx, f.now())
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	f.events(ctx, FetchEvent{Key: row.Key(), Pin: row.Pin, From: claimedFrom(row), To: fleet.CacheFetching, Attempts: row.Attempts})

	start := time.Now()
	job := &fetchJob{f: f, row: row}
	defer job.close(ctx)
	err = job.run(ctx)
	o := outcome{ev: FetchEvent{Key: row.Key(), Pin: row.Pin, From: fleet.CacheFetching, Bytes: job.downloaded}}
	if err == nil {
		o.ready = true
	} else {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		o.ev.Class = classOf(err)
		cause := fleet.PrintableText(err.Error())
		o.ev.Err = errors.New(cause)
		o.fail = fleet.FetchFailure{Key: row.Key(), Pin: row.Pin, Cause: cause, Integrity: o.ev.Class == classIntegrity, At: f.now()}
		f.failures.Add(ctx, 1, metric.WithAttributes(attribute.String("class", o.ev.Class)))
		if o.fail.Integrity {
			job.discard(ctx)
		}
	}
	o.ev.Duration = time.Since(start)

	recordErr := f.record(ctx, o)
	if o.ready {
		job.discard(ctx)
	}
	if recordErr == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	w.held = &heldOutcome{outcome: o, cause: recordErr}
	return true, recordErr
}

// claimedFrom is the state a claimed row was in. ClaimFetch returns it already
// fetching. A failure sets next_attempt_at and every way back to wanted clears
// it except ResetFetching and ResetFetchingRow, so a set value means failed; a
// row either gave back after a failed claim is reported as failed too.
func claimedFrom(row fleet.CacheRow) fleet.CacheState {
	if row.NextAttemptAt.IsZero() {
		return fleet.CacheWanted
	}
	return fleet.CacheFailed
}

// record stores o and emits its event. A failure that could not be stored
// carries its cause along, for the error handler.
func (f *Fetcher) record(ctx context.Context, o outcome) error {
	if o.ready {
		moved, err := f.cfg.Repo.MarkReady(ctx, o.ev.Key, o.ev.Pin, f.now())
		if err != nil {
			return err
		}
		if moved {
			o.ev.To = fleet.CacheReady
			f.events(ctx, o.ev)
		}
		return nil
	}
	row, err := f.cfg.Repo.RecordFetchFailure(ctx, o.fail)
	if err != nil {
		return fmt.Errorf("%w (the fetch failed: %s)", err, o.fail.Cause)
	}
	if row.Pin == o.ev.Pin && (row.State == fleet.CacheFailed || row.State == fleet.CacheBroken) {
		o.ev.To, o.ev.Attempts = row.State, row.Attempts
		f.events(ctx, o.ev)
	}
	return nil
}

// retryHeld tries the held outcome once more. When that fails too, or when an
// earlier reset did, it gives the row back to wanted and drops the outcome, so
// no row stays fetching because the database refused its settlement. Only the
// held row is reset: another worker's fetching row is its own to settle.
func (w *worker) retryHeld(ctx context.Context) (bool, error) {
	f, h := w.f, w.held
	firstRetry := !h.retried
	if firstRetry {
		err := f.record(ctx, h.outcome)
		switch {
		case err == nil:
			w.held = nil
			return true, nil
		case ctx.Err() != nil:
			return true, ctx.Err()
		}
		h.retried, h.cause = true, err
	}
	// The row may have been repinned or settled since it was claimed; the reset
	// then moves nothing, and no event says it did.
	moved, err := f.cfg.Repo.ResetFetchingRow(ctx, h.ev.Key, h.ev.Pin, f.now())
	if err != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		return true, errors.Join(h.cause, err)
	}
	w.held = nil
	if moved {
		// The cause is a database error, which may hold control runes: like every event's Err, it is printable text.
		f.events(ctx, FetchEvent{Key: h.ev.Key, Pin: h.ev.Pin, From: fleet.CacheFetching, To: fleet.CacheWanted, Err: errors.New(fleet.PrintableText(h.cause.Error()))})
	}
	if !firstRetry {
		return true, nil // the error was returned when the retry failed
	}
	return true, fmt.Errorf("the outcome of %s/%s could not be stored twice and the row went back to wanted: %w", h.ev.Key.Entry, h.ev.Key.Arch, h.cause)
}

// fetchJob is the work on one claimed row.
type fetchJob struct {
	f     *Fetcher
	row   fleet.CacheRow
	entry catalog.Entry
	root  *os.Root

	// downloaded is how many bytes this run received.
	downloaded int64
	// reserved is the free space on the spool volume this job holds, counted in
	// f.reserved until close.
	reserved uint64
}

// close releases what the job holds: its free space reservation, whichever way
// the job ended, and its handle on the spool directory.
func (j *fetchJob) close(ctx context.Context) {
	j.f.spaceMu.Lock()
	j.f.reserved -= j.reserved
	j.f.spaceMu.Unlock()
	if j.root != nil {
		if err := j.root.Close(); err != nil {
			j.f.onError(ctx, fmt.Errorf("close the spool dir: %w", err))
		}
	}
}

// run takes the row to the point where its files are in the store, tagged and
// whole. It returns ctx's error, wrapped, when ctx ends it.
func (j *fetchJob) run(ctx context.Context) error {
	root, err := os.OpenRoot(j.f.cfg.SpoolDir)
	if err != nil {
		return fmt.Errorf("open the spool dir: %w", err)
	}
	j.root = root

	e, ok := j.f.cfg.Catalog.Lookup(j.row.Entry, string(j.row.Arch))
	if !ok || e.Pin.ID != j.row.Pin {
		return fmt.Errorf("%s/%s is not in the catalog at this pin %q", j.row.Entry, j.row.Arch, j.row.Pin)
	}
	j.entry = e
	// Only lost content is fetched again: content the store cannot read is not
	// content it lacks, so that fails the attempt without a download.
	switch err := j.f.cfg.Store.CheckLayers(ctx, e.Tag()); {
	case err == nil:
		return nil
	case !store.ContentLost(err):
		return fmt.Errorf("check %s before fetching it: %w", e.Tag(), err)
	}

	var layers []ocispec.Descriptor
	for n := range e.Pin.Objects {
		if err := j.download(ctx, n); err != nil {
			return err
		}
		if err := j.verify(ctx, n); err != nil {
			return err
		}
		l, err := j.extract(ctx, n)
		if err != nil {
			return err
		}
		layers = append(layers, l...)
	}
	if _, err := j.f.cfg.Store.TagArtifact(ctx, e.Tag(), catalog.ArtifactType, layers); err != nil {
		return fmt.Errorf("tag %s: %w", e.Tag(), err)
	}
	if err := j.f.cfg.Store.CheckLayers(ctx, e.Tag()); err != nil {
		return fmt.Errorf("check %s after tagging it: %w", e.Tag(), err)
	}
	return nil
}

// discard removes the pin's spool files and sidecars. It cannot fail the row,
// so what it cannot remove goes to the error handler.
func (j *fetchJob) discard(ctx context.Context) {
	if j.root == nil {
		return
	}
	var errs []error
	for n := range j.entry.Pin.Objects {
		for _, name := range []string{SpoolName(j.row.Pin, n), ETagName(j.row.Pin, n), ETagName(j.row.Pin, n) + ".tmp"} {
			if err := j.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		j.f.onError(ctx, fmt.Errorf("remove the spool of %s/%s: %w", j.row.Entry, j.row.Arch, err))
	}
}

// progress records that done of total bytes of the current object are in
// place. It is advisory: a failure goes to the error handler and the fetch goes on.
func (j *fetchJob) progress(ctx context.Context, done, total int64) {
	err := j.f.cfg.Repo.RecordProgress(ctx, fleet.FetchProgress{Key: j.row.Key(), Pin: j.row.Pin, Done: done, Total: total})
	if err != nil && ctx.Err() == nil {
		j.f.onError(ctx, err)
	}
}

// download brings object n of the pin into its spool file in full.
func (j *fetchJob) download(ctx context.Context, n int) error {
	obj := j.entry.Pin.Objects[n]
	have, etag, err := j.resumePoint(n, obj.Size)
	if err != nil {
		return err
	}
	if have == obj.Size {
		// Whole already, so nothing is downloaded, but its extraction still writes
		// about its size into the store.
		if err := j.reserveSpace(obj.Size, 1); err != nil {
			return err
		}
		j.progress(ctx, obj.Size, obj.Size)
		return nil
	}
	if err := j.reserveSpace(obj.Size, 2); err != nil {
		return err
	}
	return j.transfer(ctx, n, have, etag)
}

// resumePoint says how much of object n is already in its spool and under
// which strong ETag. A spool longer than the pin, or one with no strong ETag
// to resume under, is emptied.
func (j *fetchJob) resumePoint(n int, size int64) (have int64, etag string, err error) {
	spool := SpoolName(j.row.Pin, n)
	fi, err := j.root.Stat(spool)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("stat the spool: %w", err)
	}
	have = fi.Size()
	if have == size || have == 0 {
		return have, "", nil
	}
	if have < size {
		raw, err := j.root.ReadFile(ETagName(j.row.Pin, n))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, "", fmt.Errorf("read the ETag sidecar: %w", err)
		}
		if err == nil && strongETag(string(raw)) {
			return have, string(raw), nil
		}
	}
	return 0, "", j.reset(n)
}

// reset empties object n's spool and removes its sidecar.
func (j *fetchJob) reset(n int) error {
	f, err := j.root.OpenFile(SpoolName(j.row.Pin, n), os.O_WRONLY|os.O_TRUNC, spoolFileMode)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return fmt.Errorf("empty the spool: %w", err)
	default:
		if err := f.Close(); err != nil {
			return fmt.Errorf("empty the spool: %w", err)
		}
	}
	return j.dropETag(n)
}

func (j *fetchJob) dropETag(n int) error {
	if err := j.root.Remove(ETagName(j.row.Pin, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the ETag sidecar: %w", err)
	}
	return nil
}

// keepETag stores etag as object n's sidecar when it is strong, atomically,
// and removes the sidecar otherwise.
func (j *fetchJob) keepETag(n int, etag string) error {
	if !strongETag(etag) {
		return j.dropETag(n)
	}
	name := ETagName(j.row.Pin, n)
	tmp := name + ".tmp"
	if err := j.root.WriteFile(tmp, []byte(etag), spoolFileMode); err != nil {
		return errors.Join(fmt.Errorf("write the ETag sidecar: %w", err), j.root.Remove(tmp))
	}
	if err := j.root.Rename(tmp, name); err != nil {
		return errors.Join(fmt.Errorf("replace the ETag sidecar: %w", err), j.root.Remove(tmp))
	}
	return nil
}

// reserveSpace refuses a download the spool volume has no room for, and
// otherwise holds the room for the job until close. The need is copies times
// the object (2: the spool and its extracted copy; 1 when the spool is already
// whole and only the extraction is left) and spaceReserve, and it must fit in
// what is free less what the other downloads in flight hold, because their
// bytes are not all on the disk yet. A job that reserved for an earlier object
// swaps that reservation for this one: those bytes are on the disk by now.
func (j *fetchJob) reserveSpace(size int64, copies uint64) error {
	f := j.f
	// Past this, the need below does not fit in a uint64; no volume holds it.
	if uint64(max(size, 0)) > (math.MaxUint64-spaceReserve)/copies {
		return fmt.Errorf("free space on the spool volume cannot hold a %d byte object", size)
	}
	need := copies*uint64(max(size, 0)) + spaceReserve

	// ponytail: statfs runs under the lock so check and reserve are one step;
	// a slow volume delays only other reservations, and a measured delay can move it out.
	f.spaceMu.Lock()
	defer f.spaceMu.Unlock()
	free, err := f.statfs(f.cfg.SpoolDir)
	if err != nil {
		return fmt.Errorf("free space on the spool volume: %w", err)
	}
	others := f.reserved - j.reserved
	if free-min(free, others) < need {
		return fmt.Errorf("free space on the spool volume is %d bytes with %d reserved by other downloads, a %d byte object needs %d", free, others, size, need)
	}
	f.reserved, j.reserved = others+need, need
	return nil
}

// transfer requests object n, from byte have under etag when etag is set, and
// writes the body into the spool.
func (j *fetchJob) transfer(ctx context.Context, n int, have int64, etag string) (err error) {
	obj := j.entry.Pin.Objects[n]
	reqCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, obj.URL, http.NoBody)
	if err != nil {
		return fmt.Errorf("build the request for object %d: %w", n, RedactURLError(err))
	}
	if etag != "" {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
		req.Header.Set("If-Range", etag)
	}
	resp, err := j.f.cfg.Client.Do(req)
	if err != nil {
		return fmt.Errorf("get object %d: %w", n, RedactURLError(err))
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()

	offset, err := j.accept(resp, n, have)
	if err != nil {
		return err
	}
	body := newIdleReader(resp.Body, j.f.idle, cancel)
	defer body.Stop()
	// The floor counts only what this response sends, never the offset's bytes
	// that an earlier attempt left in the spool.
	return j.write(ctx, newFloorReader(body, j.f.now), n, offset)
}

// accept checks the response to object n's request against the pin and returns
// the spool offset its body continues at. A 206 is checked in this order: the
// Content-Range start, then its total, then its end, then the Content-Length.
func (j *fetchJob) accept(resp *http.Response, n int, have int64) (int64, error) {
	size := j.entry.Pin.Objects[n].Size
	switch resp.StatusCode {
	case http.StatusOK:
		if resp.ContentLength >= 0 && resp.ContentLength != size {
			return 0, integrityf("the upstream sent %d bytes for an object pinned at %d", resp.ContentLength, size)
		}
		return 0, j.keepETag(n, resp.Header.Get("ETag"))
	case http.StatusPartialContent:
		header := resp.Header.Get("Content-Range")
		start, end, total, err := parseContentRange(header)
		if err != nil || start != have {
			return 0, errors.Join(fmt.Errorf("a 206 with Content-Range %q does not continue at byte %d", header, have), err, j.reset(n))
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

// write copies body into object n's spool from offset, and records the
// progress. Bytes past the pinned size are an integrity failure; a body that
// ends short is not, and its spool is kept.
func (j *fetchJob) write(ctx context.Context, body io.Reader, n int, offset int64) (err error) {
	obj := j.entry.Pin.Objects[n]
	flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
	if offset == 0 {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	file, err := j.root.OpenFile(SpoolName(j.row.Pin, n), flags, spoolFileMode)
	if err != nil {
		return fmt.Errorf("open the spool: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()

	want := obj.Size - offset
	w := &spoolWriter{file: file, last: time.Now()}
	w.tick = func() { j.progress(ctx, offset+w.n, obj.Size) }
	// One byte past the end is enough to see an oversize body.
	copied, err := io.Copy(w, io.LimitReader(body, want+1))
	j.downloaded += w.n
	j.f.bytes.Add(ctx, w.n)
	switch {
	case err != nil:
		return fmt.Errorf("copy object %d: %w", n, err)
	case copied > want:
		return integrityf("the upstream sent more than the pinned %d bytes", obj.Size)
	case copied < want:
		return fmt.Errorf("the body of object %d ended at byte %d of %d", n, offset+copied, obj.Size)
	}
	j.progress(ctx, obj.Size, obj.Size)
	return nil
}

// spoolWriter writes to a spool file and calls tick at most every
// progressEvery.
type spoolWriter struct {
	file *os.File
	n    int64
	last time.Time
	tick func()
}

func (s *spoolWriter) Write(p []byte) (int, error) {
	n, err := s.file.Write(p)
	s.n += int64(n)
	if time.Since(s.last) >= progressEvery {
		s.last = time.Now()
		s.tick()
	}
	return n, err
}

// verify checks that object n's whole spool is the pinned size and sha256.
func (j *fetchJob) verify(ctx context.Context, n int) (err error) {
	obj := j.entry.Pin.Objects[n]
	f, err := j.root.Open(SpoolName(j.row.Pin, n))
	if err != nil {
		return fmt.Errorf("open the spool: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	h := sha256.New()
	size, err := io.Copy(h, ctxReader{r: f, err: ctx.Err})
	if err != nil {
		return fmt.Errorf("hash the spool: %w", err)
	}
	if size != obj.Size {
		return integrityf("the spool holds %d bytes, the pin says %d", size, obj.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != obj.SHA256 {
		return integrityf("the object's sha256 is %s, the pin says %s", got, obj.SHA256)
	}
	return nil
}

// extract pushes each pinned file of object n from its spool into the store
// and returns their layers. A file that is not what the pin says, or a member
// the extractor refuses, is an integrity failure; a failing disk is not.
func (j *fetchJob) extract(ctx context.Context, n int) (layers []ocispec.Descriptor, err error) {
	pin := j.entry.Pin.Objects[n]
	container := j.entry.Objects[n].Container
	f, err := j.root.Open(SpoolName(j.row.Pin, n))
	if err != nil {
		return nil, fmt.Errorf("open the spool: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	layers = make([]ocispec.Descriptor, 0, len(pin.Files))
	for _, fp := range pin.Files {
		desc, err := store.Layer(fp.Path, fp.Size, fp.SHA256)
		if err != nil {
			return nil, err
		}
		member, declared, err := OpenMember(container, f, pin.Size, fp.Path)
		if err != nil {
			return nil, extractionError(err)
		}
		if declared != fp.Size {
			return nil, integrityf("%s is %d bytes in the object, the pin says %d", fp.Path, declared, fp.Size)
		}
		src := ctxReader{r: io.LimitReader(member, fp.Size), err: ctx.Err}
		if err := j.f.cfg.Store.PushBlob(ctx, desc, src); err != nil {
			return nil, extractionError(fmt.Errorf("store %s: %w", fp.Path, err))
		}
		layers = append(layers, desc)
	}
	return layers, nil
}

// ctxReader fails every read once its context is done. It holds the context's
// Err method, not the context.
type ctxReader struct {
	r   io.Reader
	err func() error
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// idleReader cancels a request when no read makes progress for idle. Every
// read that returns data restarts the wait; the request's own context is
// cancelled, so a Read blocked on the network returns.
type idleReader struct {
	r     io.Reader
	timer *time.Timer
	idle  time.Duration
	fired atomic.Bool
}

// newIdleReader wraps r and starts its idle timer. The caller stops it with
// Stop on every return path.
func newIdleReader(r io.Reader, idle time.Duration, cancel context.CancelCauseFunc) *idleReader {
	i := &idleReader{r: r, idle: idle}
	i.timer = time.AfterFunc(idle, func() {
		i.fired.Store(true)
		cancel(errIdle)
	})
	return i
}

// Read implements io.Reader.
func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.timer.Reset(i.idle)
	}
	if err != nil && i.fired.Load() {
		err = fmt.Errorf("%w for %s", errIdle, i.idle)
	}
	return n, err
}

// Stop ends the idle timer.
func (i *idleReader) Stop() { i.timer.Stop() }

// floorSampleEvery is how often a floorReader keeps a sample of its running
// total; it bounds the samples to about one window's worth.
const floorSampleEvery = time.Second

type floorSample struct {
	at    time.Time
	total int64
}

// floorReader fails a Read with errTooSlow once the transfer has run at least
// ThroughputWindow and the bytes received in the last ThroughputWindow are
// fewer than MinThroughput times its seconds. It counts only the bytes it
// returns, and reads time from now.
type floorReader struct {
	r     io.Reader
	now   func() time.Time
	start time.Time
	total int64
	// samples holds the running total at the times it was last kept, oldest
	// first. The first is the latest one at or before the window's start.
	samples []floorSample
}

// newFloorReader wraps r and starts the transfer's clock.
func newFloorReader(r io.Reader, now func() time.Time) *floorReader {
	start := now()
	return &floorReader{r: r, now: now, start: start, samples: []floorSample{{at: start}}}
}

// Read implements io.Reader. A Read that returns an error, the end of the body
// included, is not judged. The bytes of the Read that fails are still returned.
func (f *floorReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	f.total += int64(n)
	if err != nil {
		return n, err
	}
	now := f.now()
	if now.Sub(f.samples[len(f.samples)-1].at) >= floorSampleEvery {
		f.samples = append(f.samples, floorSample{at: now, total: f.total})
	}
	if now.Sub(f.start) < ThroughputWindow {
		return n, nil
	}
	cutoff := now.Add(-ThroughputWindow)
	for len(f.samples) > 1 && !f.samples[1].at.After(cutoff) {
		f.samples = f.samples[1:]
	}
	// ponytail: samples are a second apart, so the window may run up to a second
	// past ThroughputWindow, in the upstream's favour; exact bucketing if that matters.
	if f.total-f.samples[0].total < MinThroughput*int64(ThroughputWindow/time.Second) {
		return n, errTooSlow
	}
	return n, nil
}

// integrityf returns an *IntegrityError for a failure the retry cannot fix.
func integrityf(format string, args ...any) error {
	return &IntegrityError{Err: fmt.Errorf(format, args...)}
}

// extractionError classes a failure to open a member or store its file:
// integrity, unless the local disk failed. A *fs.PathError says so, and so does
// any OS errno in the chain: oras-go returns the raw os.Rename error, a
// *os.LinkError, whose ENOSPC or EIO is a full or failing disk (design 5.4), not
// bad bytes. *fs.PathError, *os.LinkError and *os.SyscallError all unwrap to one.
func extractionError(err error) error {
	if _, ok := errors.AsType[*fs.PathError](err); ok { //nolint:errcheck // the match target is not a dropped error; modernize requires AsType over errors.As
		return err
	}
	if _, ok := errors.AsType[syscall.Errno](err); ok { //nolint:errcheck // the match target is not a dropped error; modernize requires AsType over errors.As
		return err
	}
	return &IntegrityError{Err: err}
}

// classOf is the FetchEvent class of a failure.
func classOf(err error) string {
	if _, ok := errors.AsType[*IntegrityError](err); ok { //nolint:errcheck // the match target is not a dropped error; modernize requires AsType over errors.As
		return classIntegrity
	}
	return classOther
}

// RedactURL returns raw without its userinfo, query and fragment, since a
// mirror's redirect target often carries a signed token. An unparsable URL
// becomes "<unparsable>". It is for any text that reaches a log, a report or a
// stored error.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparsable>"
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	return u.String()
}

// RedactURLError returns err with the URL of any *url.Error in its chain
// redacted as RedactURL does. The cause is kept, so
// errors.AsType[*IntegrityError] still finds it. err is returned as it is
// when no *url.Error is in its chain. net/http also builds "failed to parse
// Location header %q" around the raw Location, which is a redirect target
// too, so that cause is replaced by a fixed message.
func RedactURLError(err error) error {
	ue, ok := errors.AsType[*url.Error](err)
	if !ok {
		return err
	}
	cause := ue.Err
	if cause != nil && strings.HasPrefix(cause.Error(), locationParseFailure) {
		cause = errUnparsableLocation
	}
	return &url.Error{Op: ue.Op, URL: RedactURL(ue.URL), Err: cause}
}
