package boot

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/store"
)

// artifact serves GET /u/{entry}/{arch}/{version}/{path...}: one file of a
// utility entry, from the layer of the entry's current pin whose title is the
// path, with Range support and the blob's digest as its ETag. An entry, arch,
// version or path the catalog does not know is a 404, and so is content the
// store lacks, which also puts the entry's ready cache row back to wanted. A
// layer whose digest or size is not the catalog's pin for the file counts as
// content the store lacks.
//
// The copy never watches the request context (plan gate 1, S6): a long transfer
// ends when the client stops reading, under the write deadline idleWriter
// pushes out per write, and for no other reason.
func (h *Handlers) artifact(w http.ResponseWriter, r *http.Request) {
	p := r.PathValue("path")
	e, ok := h.catalog.Lookup(r.PathValue("entry"), r.PathValue("arch"))
	if !ok || r.PathValue("version") != e.Version {
		http.NotFound(w, r)
		return
	}
	pin, ok := e.File(p)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	layers, err := h.content.Layers(ctx, e.Tag())
	if err != nil {
		h.artifactError(w, r, e, err)
		return
	}
	i := slices.IndexFunc(layers, func(l ocispec.Descriptor) bool { return l.Annotations[ocispec.AnnotationTitle] == p })
	if i < 0 {
		http.NotFound(w, r)
		return
	}
	// The tag must hold the file the catalog pins, not merely a whole blob: a layer
	// of another digest or size is content the store lacks.
	if l := layers[i]; l.Digest.String() != "sha256:"+pin.SHA256 || l.Size != pin.Size {
		h.artifactError(w, r, e, fmt.Errorf("layer %q of %q is %s (%d bytes), not the pinned sha256:%s (%d bytes): %w", p, e.Tag(), l.Digest, l.Size, pin.SHA256, pin.Size, store.ErrLayerMissing))
		return
	}
	blob, err := h.content.OpenBlob(layers[i])
	if err != nil {
		h.artifactError(w, r, e, err)
		return
	}
	defer func() {
		if err := blob.Close(); err != nil {
			h.onError(ctx, fmt.Errorf("boot: close %s/%s %s: %w", e.Entry, e.Arch, p, err))
		}
	}()
	w.Header().Set("Content-Type", contentTypeBinary)
	w.Header().Set("ETag", `"`+layers[i].Digest.String()+`"`)
	http.ServeContent(idleWriter{w}, r, path.Base(p), time.Time{}, blob)
}

// artifactError answers a failure to open e's content. Content the store lacks
// is a 404, and a row that claims to be ready at the pin goes back to wanted,
// since the claim is false; a row that is not ready lost nothing. Any other
// failure is reported to the error handler and answered 500, with the row left
// alone.
func (h *Handlers) artifactError(w http.ResponseWriter, r *http.Request, e catalog.Entry, err error) {
	ctx := r.Context()
	if !store.ContentLost(err) {
		h.reportUnreadable(ctx, e, err)
		h.internalError(w, r, err)
		return
	}
	row, found, rerr := h.repo.CacheRow(ctx, fleet.CacheKey{Entry: e.Entry, Arch: fleet.Arch(e.Arch)})
	switch {
	case rerr != nil:
		h.onError(ctx, errors.Join(fmt.Errorf("boot: %s/%s content lost: %w", e.Entry, e.Arch, err), rerr))
	case found && row.State == fleet.CacheReady && row.Pin == e.Pin.ID:
		h.requeueMissing(ctx, e, err, h.now())
	}
	http.NotFound(w, r)
}
