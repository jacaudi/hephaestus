package boot

import (
	"fmt"
	"net/http"
	"path"
	"slices"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// artifact serves GET /u/{entry}/{arch}/{version}/{path...}: one file of a
// utility entry, from the layer of the entry's current pin whose title is the
// path, with Range support and the blob's digest as its ETag. An entry, arch,
// version or path the catalog does not know is a 404, and so is content the
// store lacks, which also puts the entry's ready cache row back to wanted.
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
	if _, ok := e.File(p); !ok {
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
	if !contentLost(err) {
		h.reportUnreadable(ctx, e, err)
		h.internalError(w, r, err)
		return
	}
	row, found, rerr := h.repo.CacheRow(ctx, fleet.CacheKey{Entry: e.Entry, Arch: fleet.Arch(e.Arch)})
	switch {
	case rerr != nil:
		h.onError(ctx, rerr)
	case found && row.State == fleet.CacheReady && row.Pin == e.Pin.ID:
		h.requeueMissing(ctx, e, err, h.now())
	}
	http.NotFound(w, r)
}
