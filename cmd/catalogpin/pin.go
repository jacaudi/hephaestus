package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/toolcache"
)

// The absent-reason classes. A reason is "<class>: <detail>", and Task 15
// acts on the class: network and the retryable http classes pause the run,
// the rest rule the entry absent.
const (
	// classTLS13 is a hop that did not complete TLS 1.3, a redirect off https
	// included.
	classTLS13 = "tls13"
	// classNetwork is DNS, connect, reset, timeout, any other transfer or
	// handshake I/O failure, a body that ends short of its Content-Length, and
	// a local failure to store the download (the disk is not the upstream's
	// verdict, so it is retried, not ruled absent).
	classNetwork = "network"
	// classIntegrity is a size, hash or extraction failure.
	classIntegrity = "integrity"
	// classSignature is a verification, key-fingerprint or provenance failure.
	classSignature = "signature"
	// classChecksum is a checksum list that does not name the object's digest.
	classChecksum = "checksum"
	// classHTTPPrefix, followed by the status, is an unexpected status.
	classHTTPPrefix = "http-"
)

const (
	// maxAttempts is how many times one download is tried.
	maxAttempts = 3
	// defaultRetryWait is the pause between attempts in production.
	defaultRetryWait = 5 * time.Second
	// maxSmallFile bounds a signature or checksum list, which is read into
	// memory before it is verified.
	maxSmallFile = 8 << 20
	// efiHeaderLen is how much of a file efiStub looks at.
	efiHeaderLen = 512
	// alertProtocolVersion is TLS alert 70, sent by a peer that cannot speak the
	// offered version.
	alertProtocolVersion = 70
	// notPinnedYet is the reason of a spec with neither a pin nor a reason to keep.
	notPinnedYet = "not pinned yet: run task catalog:pin"

	listSuffix = ".list"
	sigSuffix  = ".sig"
)

// hop is one response on the way to an object: the URL asked for, with any
// query removed, its status and the TLS version its connection negotiated.
type hop struct {
	URL        string
	Status     int
	TLSVersion uint16
}

// verifyFunc checks the detached signature in sigFile over dataFile with the
// public key in keyFile and returns the primary fingerprint of the signer.
type verifyFunc func(ctx context.Context, keyFile, sigFile, dataFile string) (string, error)

// keyPrimaryFunc returns the primary fingerprint of the first key in keyFile.
type keyPrimaryFunc func(ctx context.Context, keyFile string) (string, error)

// pinner downloads, authenticates and extracts catalog objects. work holds the
// downloads and keys the signer keys. verify and keyPrimary are gpg in
// production and fakes in tests.
type pinner struct {
	client     *http.Client
	work, keys *os.Root
	provenance map[string]bool
	verify     verifyFunc
	keyPrimary keyPrimaryFunc
	// retryWait is the pause before a retried download; tests leave it zero.
	retryWait time.Duration
}

// pinRun is the input of one pinAll: the specs to pin, the compiled-in pins
// and absent reasons to keep for the specs not in Only, and Only itself, the
// entries to re-pin (all of them when empty).
type pinRun struct {
	Specs    []catalog.Spec
	Existing map[catalog.Key]catalog.Pin
	Absent   map[catalog.Key]string
	Only     []string
}

// pinError is an absent reason: a class and the detail behind it.
type pinError struct {
	class string
	err   error
}

// Error implements error: "<class>: <detail>".
func (e *pinError) Error() string { return e.class + ": " + e.err.Error() }

// Unwrap returns the detail.
func (e *pinError) Unwrap() error { return e.err }

// retryable reports whether the class is one worth another attempt: a network
// failure, a 5xx or a 429.
func (e *pinError) retryable() bool {
	status, ok := strings.CutPrefix(e.class, classHTTPPrefix)
	if !ok {
		return e.class == classNetwork
	}
	return status == "429" || (len(status) == 3 && status[0] == '5')
}

func failf(class, format string, args ...any) error {
	return &pinError{class: class, err: fmt.Errorf(format, args...)}
}

// asPinError returns the pinError in err's chain, or a new one for an error
// from the transfer or from local I/O: classified by classifyFetchError, with
// any URL in it redacted.
func asPinError(err error) *pinError {
	if pe, ok := errors.AsType[*pinError](err); ok {
		return pe
	}
	return &pinError{class: classifyFetchError(err), err: toolcache.RedactURLError(err)}
}

// classifyFetchError returns the class of a transfer error that is not yet a
// pinError. TLS 1.3 is refused when the peer sent alert 70 or selected a
// version below it; over TCP crypto/tls returns a received alert as a
// *net.OpError whose Err is unexported, so it is matched by its text.
func classifyFetchError(err error) string {
	// The only IntegrityError the client returns is a redirect off https: a hop
	// that will not complete TLS.
	if _, ok := errors.AsType[*toolcache.IntegrityError](err); ok { //nolint:errcheck // the match target is not a dropped error; modernize requires AsType over errors.As
		return classTLS13
	}
	if tlsVersionRefused(err) {
		return classTLS13
	}
	return classNetwork
}

func tlsVersionRefused(err error) bool {
	if a, ok := errors.AsType[tls.AlertError](err); ok && a == alertProtocolVersion {
		return true
	}
	if oe, ok := errors.AsType[*net.OpError](err); ok && oe.Op == "remote error" && oe.Err != nil &&
		oe.Err.Error() == "tls: protocol version not supported" {
		return true
	}
	// A server that answers a TLS 1.3-only hello with a lower version. Only the
	// cause is read: the URL in a url.Error is not part of the verdict.
	cause := err
	if ue, ok := errors.AsType[*url.Error](err); ok && ue.Err != nil {
		cause = ue.Err
	}
	return strings.Contains(cause.Error(), "tls: server selected unsupported protocol version")
}

// hopOf records the response r: its request's URL, redacted, its status and
// its TLS version (zero without TLS).
func hopOf(r *http.Response) hop {
	h := hop{URL: toolcache.RedactURL(r.Request.URL.String()), Status: r.StatusCode}
	if r.TLS != nil {
		h.TLSVersion = r.TLS.Version
	}
	return h
}

// checkHop refuses a hop that did not complete TLS 1.3.
func checkHop(h hop) error {
	switch {
	case h.TLSVersion == 0:
		return failf(classTLS13, "hop %s: no TLS, TLS 1.3 required", h.URL)
	case h.TLSVersion < tls.VersionTLS13:
		return failf(classTLS13, "hop %s: completed %s, TLS 1.3 required", h.URL, tls.VersionName(h.TLSVersion))
	}
	return nil
}

// hopReport returns one report line per hop.
func hopReport(hops []hop) []string {
	lines := make([]string, 0, len(hops))
	for i, h := range hops {
		lines = append(lines, fmt.Sprintf("hop %d %s status=%d tls=0x%04x", i+1, h.URL, h.Status, h.TLSVersion))
	}
	return lines
}

// fetched is a download in the work directory.
type fetched struct {
	name string
	size int64
	sums hashers
	hops []hop
}

// fetch downloads rawURL into the work file name, trying up to maxAttempts
// times when the failure is a network one, a 5xx or a 429. limit, when above
// zero, bounds the body. The returned error is always a *pinError.
func (p *pinner) fetch(ctx context.Context, rawURL, name string, limit int64) (fetched, error) {
	for attempt := 1; ; attempt++ {
		got, err := p.fetchOnce(ctx, rawURL, name, limit)
		if err == nil {
			return got, nil
		}
		pe := asPinError(err)
		if attempt == maxAttempts || !pe.retryable() || ctx.Err() != nil {
			return got, pe
		}
		select {
		case <-ctx.Done():
			return got, pe
		case <-time.After(p.retryWait):
		}
	}
}

// fetchOnce makes one attempt. Every hop is recorded and must have completed
// TLS 1.3, redirect targets included; the status must be 200; the bytes
// written must equal the Content-Length when one was sent.
func (p *pinner) fetchOnce(ctx context.Context, rawURL, name string, limit int64) (got fetched, err error) {
	got = fetched{name: name, sums: newHashers()}
	client := *p.client
	next := p.client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		h := hopOf(req.Response)
		got.hops = append(got.hops, h)
		if err := checkHop(h); err != nil {
			return err
		}
		if next != nil {
			return next(req, via)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return got, toolcache.RedactURLError(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return got, err
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	h := hopOf(resp)
	got.hops = append(got.hops, h)
	if err := checkHop(h); err != nil {
		return got, err
	}
	if resp.StatusCode != http.StatusOK {
		return got, failf(classHTTPPrefix+strconv.Itoa(resp.StatusCode), "GET %s: %s", h.URL, resp.Status)
	}

	f, err := p.work.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return got, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	body := io.Reader(resp.Body)
	if limit > 0 {
		body = io.LimitReader(body, limit+1)
	}
	got.size, err = io.Copy(io.MultiWriter(f, got.sums.writer()), body)
	if err != nil {
		return got, toolcache.RedactURLError(err)
	}
	switch {
	case limit > 0 && got.size > limit:
		return got, failf(classIntegrity, "GET %s: more than %d bytes", h.URL, limit)
	case resp.ContentLength >= 0 && got.size != resp.ContentLength:
		return got, failf(classIntegrity, "GET %s: wrote %d bytes, Content-Length says %d", h.URL, got.size, resp.ContentLength)
	}
	return got, nil
}

// discard removes the named work files; one that is not there is fine.
func (p *pinner) discard(names ...string) error {
	var errs []error
	for _, n := range names {
		if err := p.work.Remove(n); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// workPath is the file system path of the work file name, for gpg.
func (p *pinner) workPath(name string) string { return filepath.Join(p.work.Name(), name) }

// fetchSmall downloads a signature or checksum list.
func (p *pinner) fetchSmall(ctx context.Context, rawURL, name string) ([]string, error) {
	got, err := p.fetch(ctx, rawURL, name, maxSmallFile)
	return hopReport(got.hops), err
}

// pinSpec pins every object of s and returns the pin and a report of what it
// saw. The error is nil, or a *pinError whose text is the absent reason. The
// report is returned either way.
func (p *pinner) pinSpec(ctx context.Context, s catalog.Spec) (catalog.Pin, []string, error) {
	var lines []string
	objects := make([]catalog.ObjectPin, 0, len(s.Objects))
	for i := range s.Objects {
		o, objLines, err := p.pinObject(ctx, s, i)
		lines = append(lines, objLines...)
		if err != nil {
			return catalog.Pin{}, lines, asPinError(err)
		}
		objects = append(objects, o)
	}
	pin := catalog.Pin{ID: catalog.PinID(objects), Objects: objects}
	return pin, append(lines, findings(s, pin)...), nil
}

// findings are the report lines that belong to the whole entry: the pin id,
// whether its kernel is an EFI binary, and whether its recipe's operating
// system honours BOOTIF.
func findings(s catalog.Spec, pin catalog.Pin) []string {
	lines := []string{"pin " + pin.ID}
	if f, ok := (catalog.Entry{Spec: s, Pin: pin}).File(s.Boot.Kernel); ok {
		lines = append(lines, fmt.Sprintf("finding efi_stub %s %t", f.Path, f.EFIStub))
	}
	return append(lines, fmt.Sprintf("finding bootif recipe=%s honoured=%t", s.Recipe, s.Recipe.HonoursBootIF()))
}

// pinObject downloads object i of s, authenticates it and hashes the files
// taken from it. Its downloads are deleted before it returns.
func (p *pinner) pinObject(ctx context.Context, s catalog.Spec, i int) (op catalog.ObjectPin, lines []string, err error) {
	o := s.Objects[i]
	name := fmt.Sprintf("%s-%s-%d", s.Entry, s.Arch, i)
	defer func() {
		err = errors.Join(err, p.discard(name, name+listSuffix, name+listSuffix+sigSuffix, name+sigSuffix))
	}()

	obj, err := p.fetch(ctx, o.URL, name, 0)
	lines = append(lines, hopReport(obj.hops)...)
	if err != nil {
		return op, lines, err
	}
	sha := obj.sums.sum(algoSHA256)
	lines = append(lines, fmt.Sprintf("object %s size=%d sha256=%s", toolcache.RedactURL(o.URL), obj.size, sha))

	signer, authLines, err := p.authenticate(ctx, o, name, obj)
	lines = append(lines, authLines...)
	if err != nil {
		return op, lines, err
	}
	files, fileLines, err := p.extract(o, name, obj.size)
	lines = append(lines, fileLines...)
	if err != nil {
		return op, lines, err
	}
	return catalog.ObjectPin{URL: o.URL, Size: obj.size, SHA256: sha, Signer: signer, Files: files}, lines, nil
}

// authenticate checks the object against what its spec says authenticates
// it, before anything is parsed: nothing for TOFU with no list; a list the
// object's digest must appear in; a detached signature over the object, or
// over the list (and then the list check). It returns the signer's primary
// fingerprint, empty for TOFU.
func (p *pinner) authenticate(ctx context.Context, o catalog.ObjectSpec, name string, obj fetched) (signer string, lines []string, err error) {
	sig := o.Signature
	if sig.TOFU && sig.ChecksumsURL == "" {
		return "", nil, nil
	}
	covered := name // the work file the signature covers
	var hl []string
	if sig.ChecksumsURL != "" {
		covered = name + listSuffix
		hl, err = p.fetchSmall(ctx, sig.ChecksumsURL, covered)
		lines = append(lines, hl...)
		if err != nil {
			return "", lines, err
		}
	}
	if !sig.TOFU {
		signer, hl, err = p.verifySignature(ctx, sig, covered)
		lines = append(lines, hl...)
		if err != nil {
			return "", lines, err
		}
		lines = append(lines, "signer "+signer)
	}
	if sig.ChecksumsURL != "" {
		if err := p.checkList(o, covered, obj); err != nil {
			return "", lines, err
		}
	}
	return signer, lines, nil
}

// checkList requires the checksum list in the work file list to name the
// object's basename with the object's digest.
func (p *pinner) checkList(o catalog.ObjectSpec, list string, obj fetched) error {
	data, err := p.work.ReadFile(list)
	if err != nil {
		return err
	}
	u, err := url.Parse(o.URL)
	if err != nil {
		return failf(classChecksum, "object URL %s: %w", toolcache.RedactURL(o.URL), err)
	}
	base := path.Base(u.Path)
	got := obj.sums.sum(o.Signature.ChecksumAlgo)
	want, ok := parseChecksums(data, o.Signature.ChecksumAlgo, base)
	switch {
	case !ok:
		return failf(classChecksum, "the %s list names no unique %s digest for %s", o.Signature.ChecksumAlgo, o.Signature.ChecksumAlgo, base)
	case want != got:
		return failf(classChecksum, "the %s list gives %s for %s, the download is %s", o.Signature.ChecksumAlgo, want, base, got)
	}
	return nil
}

// verifySignature downloads the detached signature, has gpg check it over the
// work file covered, and requires the signer's primary fingerprint to be the
// one the key file holds and one PROVENANCE.md names.
func (p *pinner) verifySignature(ctx context.Context, sig catalog.Signature, covered string) (string, []string, error) {
	sigName := covered + sigSuffix
	lines, err := p.fetchSmall(ctx, sig.SigURL, sigName)
	if err != nil {
		return "", lines, err
	}
	keyFile, err := p.keyPath(sig.Key)
	if err != nil {
		return "", lines, err
	}
	signer, err := p.verify(ctx, keyFile, p.workPath(sigName), p.workPath(covered))
	if err != nil {
		return "", lines, failf(gpgClass(err), "verify with %s: %w", sig.Key, err)
	}
	primary, err := p.keyPrimary(ctx, keyFile)
	switch {
	case err != nil:
		return "", lines, failf(gpgClass(err), "read the fingerprint of %s: %w", sig.Key, err)
	case signer != primary:
		return "", lines, failf(classSignature, "signed by %s, but %s holds %s", signer, sig.Key, primary)
	case !p.provenance[signer]:
		return "", lines, failf(classSignature, "signer %s is not in PROVENANCE.md", signer)
	}
	return signer, lines, nil
}

// gpgClass is the class of a gpg failure: a local one says nothing about the
// signature, so it is network (Task 15 pauses on it) and not signature (which
// rules the entry absent).
func gpgClass(err error) string {
	if errors.Is(err, errGPGLocal) {
		return classNetwork
	}
	return classSignature
}

// keyPath returns the path of the signer key file key, after confirming it
// opens beneath the keys directory.
func (p *pinner) keyPath(key string) (keyFile string, err error) {
	f, err := p.keys.Open(key)
	if err != nil {
		return "", failf(classSignature, "open signer key %s: %w", key, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return filepath.Join(p.keys.Name(), key), nil
}

// extract hashes every file of o out of the verified download in the work
// file name.
func (p *pinner) extract(o catalog.ObjectSpec, name string, size int64) (files []catalog.FilePin, lines []string, err error) {
	f, err := p.work.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	for _, member := range o.Files {
		fp, err := hashMember(o.Container, f, size, member)
		if err != nil {
			return nil, lines, failf(classIntegrity, "extract %s: %w", member, err)
		}
		files = append(files, fp)
		lines = append(lines, fmt.Sprintf("file %s size=%d sha256=%s efi_stub=%t", fp.Path, fp.Size, fp.SHA256, fp.EFIStub))
	}
	return files, lines, nil
}

// hashMember reads one member of the object in ra through toolcache.OpenMember
// and returns its size, sha256 and EFI-stub finding. The bytes read must be as
// many as the container declares, and not none.
func hashMember(c catalog.Container, ra io.ReaderAt, size int64, member string) (catalog.FilePin, error) {
	r, declared, err := toolcache.OpenMember(c, ra, size, member)
	if err != nil {
		return catalog.FilePin{}, err
	}
	head := make([]byte, efiHeaderLen)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return catalog.FilePin{}, err
	}
	head = head[:n]
	h := sha256.New()
	total, err := io.Copy(h, io.MultiReader(bytes.NewReader(head), r))
	if err != nil {
		return catalog.FilePin{}, err
	}
	switch {
	case total != declared:
		return catalog.FilePin{}, fmt.Errorf("read %d bytes of the %d it declares", total, declared)
	case total == 0:
		return catalog.FilePin{}, errors.New("the member is empty")
	}
	return catalog.FilePin{Path: member, Size: total, SHA256: hex.EncodeToString(h.Sum(nil)), EFIStub: efiStub(head)}, nil
}

// pinAll pins the specs of run and returns the pins, the absent reasons and
// the report. With Only set, only those entries are downloaded; every other
// spec keeps its pin or absent reason from run, and a spec with neither is
// absent as not pinned yet. Pins and reasons for keys no spec names are
// dropped, never validated. It stops at the first sign that ctx is done, so
// the result is then incomplete and must not be written.
func (p *pinner) pinAll(ctx context.Context, run pinRun) (map[catalog.Key]catalog.Pin, map[catalog.Key]string, []string) {
	pins := make(map[catalog.Key]catalog.Pin)
	absent := make(map[catalog.Key]string)
	var lines []string
	for i := range run.Specs {
		if ctx.Err() != nil {
			break
		}
		s := &run.Specs[i]
		k := s.Key()
		lines = append(lines, "== "+k.Entry+"/"+k.Arch)
		if !run.selected(s.Entry) {
			lines = append(lines, "  "+keep(k, run, pins, absent))
			continue
		}
		pin, specLines, err := p.pinSpec(ctx, *s)
		for _, l := range specLines {
			lines = append(lines, "  "+l)
		}
		if err != nil {
			absent[k] = err.Error()
			lines = append(lines, "  ABSENT "+err.Error())
			continue
		}
		pins[k] = pin
		lines = append(lines, "  PINNED "+pin.ID)
	}
	return pins, absent, lines
}

// keep carries the compiled-in pin or absent reason of k into the result and
// returns its report line.
func keep(k catalog.Key, run pinRun, pins map[catalog.Key]catalog.Pin, absent map[catalog.Key]string) string {
	if pin, ok := run.Existing[k]; ok {
		pins[k] = pin
		return "KEPT pin " + pin.ID
	}
	reason, ok := run.Absent[k]
	if !ok {
		reason = notPinnedYet
	}
	absent[k] = reason
	return "KEPT ABSENT " + reason
}
