package toolcache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/store"
)

const (
	// TalosArtifactType is the artifact type of the manifest a Talos row's tag names.
	TalosArtifactType = "application/vnd.hephaestus.talos.v1"
	// TalosMaxAsset is the most bytes a kernel or initramfs of unknown size may have.
	TalosMaxAsset = 1 << 30

	// talosMaxCmdline is the most bytes the factory's cmdline file may have.
	talosMaxCmdline = 4096
	// maxCmdline is the longest kernel command line iPXE passes on, on every
	// platform (bzimage.c:314).
	maxCmdline = 2046
	// talosRegisterTimeout bounds a schematic registration.
	talosRegisterTimeout = 2 * time.Minute
)

// TalosSpoolName is the spool file of part n of the row at key, n counting
// kernel 0, initramfs 1 and cmdline 2: "talos-<schematic>-<version>-<arch>-<n>".
// The whole schematic is in the name, so rows of different keys never share a
// file.
func TalosSpoolName(key fleet.TalosKey, n int) string {
	return "talos-" + key.Schematic + "-" + key.Version + "-" + string(key.Arch) + "-" + strconv.Itoa(n)
}

// talosSource claims talos_cache rows. Each row it claims is a talosJob, which
// is both the row's run and its settler.
type talosSource struct{ f *Fetcher }

// claim returns a row an assigned machine wants for claimAssigned, any other
// row for claimOther, and nothing for the utility classes.
func (s talosSource) claim(ctx context.Context, class claimClass, at time.Time) (claimed, bool, error) {
	var tc fleet.TalosClass
	switch class {
	case claimAssigned:
		tc = fleet.TalosAssigned
	case claimOther:
		tc = fleet.TalosOther
	case claimUrgent, claimRest:
		return claimed{}, false, nil
	}
	row, ok, err := s.f.cfg.Repo.ClaimTalos(ctx, tc, at)
	if err != nil || !ok {
		return claimed{}, false, err
	}
	job := &talosJob{f: s.f, row: row}
	return claimed{
		name:  row.Key().Tag(),
		event: FetchEvent{Source: SourceTalos, Talos: row.Key(), From: claimedFrom(row.NextAttemptAt), To: fleet.CacheFetching, Attempts: row.Attempts},
		run:   job,
		row:   job,
	}, true, nil
}

// talosJob is the work on one claimed talos_cache row: it registers the
// schematic, downloads the three parts, records or checks their sizes and
// hashes, and pushes and tags them.
type talosJob struct {
	f    *Fetcher
	row  fleet.TalosRow
	root *os.Root
	// release lets the row's key go; nil until the job holds it.
	release func()

	// assets are the sizes and hashes the run found, which ready records.
	assets fleet.TalosAssets
	// cmdline is the command line with its trailing newline trimmed.
	cmdline []byte

	// downloaded is how many bytes this run received.
	downloaded int64
	// reserved is the free space on the spool volume this job holds, counted in
	// f.reserved until close.
	reserved uint64
}

// dl is the job's downloader, over the job's own root, byte count and reservation.
func (j *talosJob) dl() *downloader {
	return &downloader{f: j.f, root: j.root, downloaded: &j.downloaded, reserved: &j.reserved, bytes: j.f.talosBytes, progress: j.progress}
}

// object is part n of the row, asked of the factory, which redirects each
// request afresh. A kernel or initramfs is pinned by its recorded asset when
// it has one. The cmdline never is: its recorded asset is of the trimmed line.
func (j *talosJob) object(n int, part fleet.TalosPart) object {
	key := j.row.Key()
	o := object{URL: j.f.cfg.Talos.URL(key.Schematic, key.Version, key.Arch, part), SpoolName: TalosSpoolName(key, n), Max: TalosMaxAsset, Index: n}
	if part == fleet.TalosCmdline {
		o.Max = talosMaxCmdline
		return o
	}
	rec := j.row.Assets[part]
	o.Size, o.SHA256 = rec.Size, rec.SHA256
	return o
}

// run takes the row to the point where its three parts are in the store,
// tagged, whole and equal to the recorded assets. It returns ctx's error,
// wrapped, when ctx ends it.
func (j *talosJob) run(ctx context.Context) error {
	key := j.row.Key()
	// The schematic becomes a URL path segment and a tag.
	if !fleet.IsSHA256Hex(key.Schematic) {
		return integrityf("the schematic %q is not 64 lower-case hex digits", key.Schematic)
	}
	// A hash reset lets another worker claim the row while an earlier job on it
	// runs on. The claim fences only the database writes; the spool files and the
	// tag are the key's, so the job waits until no other job holds the key.
	release, err := j.f.holdTalos(ctx, key)
	if err != nil {
		return fmt.Errorf("wait for the other job on %s: %w", key.Tag(), err)
	}
	j.release = release
	root, err := os.OpenRoot(j.f.cfg.SpoolDir)
	if err != nil {
		return fmt.Errorf("open the spool dir: %w", err)
	}
	j.root = root

	// A row without its hashes always downloads: the tag cannot be checked
	// against anything.
	if j.row.Assets.Complete() {
		stored, err := talosStored(ctx, j.f.cfg.Store, key, j.row.Assets)
		if err != nil {
			return fmt.Errorf("before fetching it: %w", err)
		}
		if stored {
			j.assets = j.row.Assets
			return nil
		}
	}
	if len(j.row.Body) > 0 {
		if err := j.register(ctx); err != nil {
			return err
		}
	}
	j.assets = fleet.TalosAssets{}
	for n, part := range fleet.TalosParts() {
		if err := j.fetch(ctx, n, part); err != nil {
			return fmt.Errorf("%s: %w", part, err)
		}
	}
	return j.push(ctx)
}

// register posts the row's body to the factory and refuses an ID other than
// the row's, before the ID reaches a URL.
func (j *talosJob) register(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, talosRegisterTimeout)
	defer cancel()
	id, err := j.f.cfg.Talos.Register(ctx, j.row.Body)
	if err != nil {
		return err
	}
	if id != j.row.Schematic {
		return integrityf("the image factory registered the schematic as %q, not %s", id, j.row.Schematic)
	}
	return nil
}

// fetch downloads part n and finds its asset: the spool's size and sha256, or
// for the cmdline the trimmed line's, which must also be a valid command line.
// A recorded asset it differs from is an integrity failure. The factory's 404
// is too: the schematic or the version is unknown to it.
func (j *talosJob) fetch(ctx context.Context, n int, part fleet.TalosPart) error {
	o := j.object(n, part)
	if err := j.dl().fetch(ctx, o); err != nil {
		if se, ok := errors.AsType[*StatusError](err); ok && se.Code == http.StatusNotFound {
			return integrityf("factory: %s", se.Body)
		}
		return err
	}
	var got fleet.TalosAsset
	if part == fleet.TalosCmdline {
		line, err := j.readCmdline(ctx, o)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(line)
		j.cmdline, got = line, fleet.TalosAsset{Size: int64(len(line)), SHA256: hex.EncodeToString(sum[:])}
	} else {
		sum, size, err := j.dl().digest(ctx, o)
		if err != nil {
			return err
		}
		got = fleet.TalosAsset{Size: size, SHA256: sum}
	}
	if rec, ok := j.row.Assets[part]; ok && rec != got {
		return integrityf("the asset is %d bytes with sha256 %s, recorded as %d bytes with sha256 %s", got.Size, got.SHA256, rec.Size, rec.SHA256)
	}
	if part == fleet.TalosCmdline {
		if err := validCmdline(string(j.cmdline)); err != nil {
			return &IntegrityError{Err: err}
		}
	}
	j.assets[part] = got
	return nil
}

// readCmdline reads the cmdline spool, which the download held to
// talosMaxCmdline bytes, and trims exactly one trailing newline.
func (j *talosJob) readCmdline(ctx context.Context, o object) (line []byte, err error) {
	f, err := j.dl().open(o)
	if err != nil {
		return nil, fmt.Errorf("open the spool: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	b, err := io.ReadAll(ctxReader{r: io.LimitReader(f, talosMaxCmdline), err: ctx.Err})
	if err != nil {
		return nil, fmt.Errorf("read the spool: %w", err)
	}
	return []byte(strings.TrimSuffix(string(b), "\n")), nil
}

// push stores each part durably, then tags the three and checks the tag.
func (j *talosJob) push(ctx context.Context) error {
	key := j.row.Key()
	layers, err := talosLayers(j.assets)
	if err != nil {
		return err
	}
	// Each download swapped out the reservation of the one before, so the job
	// holds room for the cmdline alone, while the store copies of all three are
	// still to be written: it holds room for them until close.
	var size int64
	for i := range layers {
		size += layers[i].Size
	}
	if err := j.dl().reserveSpace(size, 1); err != nil {
		return err
	}
	for n, part := range fleet.TalosParts() {
		if err := j.pushPart(ctx, n, part, layers[n]); err != nil {
			return fmt.Errorf("store %s: %w", part, err)
		}
	}
	if _, err := j.f.cfg.Store.TagArtifact(ctx, key.Tag(), TalosArtifactType, layers); err != nil {
		return fmt.Errorf("tag %s: %w", key.Tag(), err)
	}
	if err := j.f.cfg.Store.CheckLayers(ctx, key.Tag()); err != nil {
		return fmt.Errorf("check %s after tagging it: %w", key.Tag(), err)
	}
	return nil
}

// pushPart pushes part n as desc: the kernel and initramfs from their spools,
// the cmdline from the trimmed line.
func (j *talosJob) pushPart(ctx context.Context, n int, part fleet.TalosPart, desc ocispec.Descriptor) (err error) {
	if part == fleet.TalosCmdline {
		return j.f.cfg.Store.PushBlob(ctx, desc, bytes.NewReader(j.cmdline))
	}
	f, err := j.dl().open(j.object(n, part))
	if err != nil {
		return fmt.Errorf("open the spool: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return j.f.cfg.Store.PushBlob(ctx, desc, ctxReader{r: f, err: ctx.Err})
}

// talosLayers are the layers of a tag holding assets: one per part, in
// TalosParts order, titled by the part.
func talosLayers(assets fleet.TalosAssets) ([]ocispec.Descriptor, error) {
	parts := fleet.TalosParts()
	layers := make([]ocispec.Descriptor, 0, len(parts))
	for _, p := range parts {
		l, err := store.Layer(string(p), assets[p].Size, assets[p].SHA256)
		if err != nil {
			return nil, err
		}
		layers = append(layers, l)
	}
	return layers, nil
}

// talosStored reports whether the tag of key passes the layer check and its
// layers are the ones assets record. A tag the store lacks content for is not
// stored; one it cannot read is an error.
func talosStored(ctx context.Context, st *store.Store, key fleet.TalosKey, assets fleet.TalosAssets) (bool, error) {
	switch err := st.CheckLayers(ctx, key.Tag()); {
	case store.ContentLost(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check %s: %w", key.Tag(), err)
	}
	got, err := st.Layers(ctx, key.Tag())
	if err != nil {
		return false, fmt.Errorf("read the layers of %s: %w", key.Tag(), err)
	}
	want, err := talosLayers(assets)
	if err != nil {
		return false, err
	}
	return slices.EqualFunc(got, want, func(a, b ocispec.Descriptor) bool {
		return a.Digest == b.Digest && a.Size == b.Size && a.Annotations[ocispec.AnnotationTitle] == b.Annotations[ocispec.AnnotationTitle]
	}), nil
}

// validCmdline refuses a command line iPXE would not pass on whole as kernel
// arguments: one that is not printable ASCII (a newline included), longer than
// maxCmdline, or that holds a $ (a setting expansion), a ;, && or || (command
// separators) or a token starting with # (a comment) (src/core/exec.c:184-211).
func validCmdline(s string) error {
	if len(s) > maxCmdline {
		return fmt.Errorf("the command line is %d bytes, over the %d iPXE passes on", len(s), maxCmdline)
	}
	for i := range len(s) {
		if c := s[i]; c < 0x20 || c > 0x7e {
			return fmt.Errorf("the command line has the byte %#x at %d, which is not printable ASCII", c, i)
		}
	}
	for _, bad := range []string{"$", ";", "&&", "||"} {
		if strings.Contains(s, bad) {
			return fmt.Errorf("the command line holds %q", bad)
		}
	}
	for tok := range strings.FieldsSeq(s) {
		if strings.HasPrefix(tok, "#") {
			return fmt.Errorf("the command line holds the comment token %q", tok)
		}
	}
	return nil
}

// received is how many bytes this run received.
func (j *talosJob) received() int64 { return j.downloaded }

// discard removes the spool files and sidecars of the three parts. It cannot
// fail the row, so what it cannot remove goes to the error handler.
func (j *talosJob) discard(ctx context.Context) {
	if j.root == nil {
		return
	}
	parts := fleet.TalosParts()
	objects := make([]object, 0, len(parts))
	for n, part := range parts {
		objects = append(objects, j.object(n, part))
	}
	if err := j.dl().discard(ctx, objects); err != nil {
		j.f.onError(ctx, fmt.Errorf("remove the spool of %s: %w", j.row.Key().Tag(), err))
	}
}

// close releases the job's free space reservation, its handle on the spool
// directory and then its key. The worker calls it after any discard, so the
// next job on the key finds none of this job's spool files.
func (j *talosJob) close(ctx context.Context) {
	j.dl().close(ctx)
	if j.release != nil {
		j.release()
	}
}

// holdTalos waits until no other job of f holds key, then holds it until the
// returned release is called. It returns ctx's error when ctx ends the wait.
func (f *Fetcher) holdTalos(ctx context.Context, key fleet.TalosKey) (release func(), err error) {
	f.talosMu.Lock()
	for {
		ended, held := f.talosHeld[key]
		if !held {
			break
		}
		f.talosMu.Unlock()
		select {
		case <-ended:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		f.talosMu.Lock()
	}
	ended := make(chan struct{})
	f.talosHeld[key] = ended
	f.talosMu.Unlock()
	return func() {
		f.talosMu.Lock()
		delete(f.talosHeld, key)
		f.talosMu.Unlock()
		close(ended)
	}, nil
}

// progress records that done of total bytes of the current part are in place.
// It is advisory: a failure goes to the error handler and the fetch goes on.
func (j *talosJob) progress(ctx context.Context, done, total int64) {
	if err := j.f.cfg.Repo.RecordTalosProgress(ctx, j.row.Claim(), done, total); err != nil && ctx.Err() == nil {
		j.f.onError(ctx, err)
	}
}

// record stores o and emits its event: ready with the assets the run found, a
// deferred row given back, any other a failure. A failure that could not be
// stored carries its cause along, for the error handler.
func (j *talosJob) record(ctx context.Context, o outcome) error {
	f := j.f
	if o.deferred {
		moved, err := j.giveBack(ctx, f.now())
		if err != nil {
			return err
		}
		if moved {
			o.ev.To = fleet.CacheWanted
			f.events(ctx, o.ev)
		}
		return nil
	}
	if o.ready {
		moved, err := f.cfg.Repo.MarkTalosReady(ctx, j.row.Claim(), j.assets, f.now())
		if err != nil {
			return err
		}
		if moved {
			o.ev.To = fleet.CacheReady
			f.events(ctx, o.ev)
		}
		return nil
	}
	row, err := f.cfg.Repo.RecordTalosFailure(ctx, fleet.TalosFailure{Claim: j.row.Claim(), Cause: o.cause, Integrity: o.integrity, At: o.at})
	if err != nil {
		return fmt.Errorf("%w (the fetch failed: %s)", err, o.cause)
	}
	if row.State == fleet.CacheFailed || row.State == fleet.CacheBroken {
		o.ev.To, o.ev.Attempts = row.State, row.Attempts
		f.events(ctx, o.ev)
	}
	return nil
}

// giveBack moves the row from fetching back to wanted. The row may have been
// settled since it was claimed; the reset then moves nothing.
func (j *talosJob) giveBack(ctx context.Context, at time.Time) (bool, error) {
	return j.f.cfg.Repo.ResetTalosFetchingRow(ctx, j.row.Claim(), at)
}

// countFailure adds one to hephaestus.talos.fetch.failures under class.
func (j *talosJob) countFailure(ctx context.Context, class string) {
	j.f.talosFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("class", class)))
}
