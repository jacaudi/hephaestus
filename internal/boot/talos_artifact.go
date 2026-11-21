package boot

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/store"
)

// errTalosMissing is reported to the error handler when a ready Talos row's
// content is missing from the store, or is not the content the row records.
// The row is put back to wanted.
var errTalosMissing = errors.New("boot: ready Talos content is missing from the store; fetch re-queued")

// talosArtifact serves GET /t/{schematic}/{version}/{arch}/{asset}: the kernel
// or the initramfs of a ready talos_cache row, from the layer of the row's tag
// titled by the asset, with Range support and the blob's digest as its ETag.
// The tag is the stored row's, never one built from the request. Another asset,
// a row that is absent or not ready, and content the store lacks are a 404; the
// last also puts the row back to wanted. A layer whose digest or size is not the
// row's recorded one counts as content the store lacks.
//
// As for /u/, the copy never watches the request context: a long transfer ends
// when the client stops reading, under the write deadline idleWriter pushes out
// per write.
func (h *Handlers) talosArtifact(w http.ResponseWriter, r *http.Request) {
	part := fleet.TalosPart(r.PathValue("asset"))
	if part != fleet.TalosKernel && part != fleet.TalosInitramfs {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	key := fleet.TalosKey{Schematic: r.PathValue("schematic"), Version: r.PathValue("version"), Arch: fleet.Arch(r.PathValue("arch"))}
	row, found, err := h.repo.Talos(ctx, key)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	if !found || row.State != fleet.CacheReady {
		http.NotFound(w, r)
		return
	}
	layers, err := h.content.Layers(ctx, row.Key().Tag())
	if err != nil {
		h.talosArtifactError(w, r, row.Key(), err)
		return
	}
	l, err := talosLayer(layers, row, part)
	if err != nil {
		h.talosArtifactError(w, r, row.Key(), err)
		return
	}
	blob, err := h.content.OpenBlob(l)
	if err != nil {
		h.talosArtifactError(w, r, row.Key(), err)
		return
	}
	defer func() {
		if err := blob.Close(); err != nil {
			h.onError(ctx, fmt.Errorf("boot: close %s %s: %w", row.Key().Tag(), part, err))
		}
	}()
	w.Header().Set("Content-Type", contentTypeBinary)
	w.Header().Set("ETag", `"`+l.Digest.String()+`"`)
	http.ServeContent(idleWriter{w}, r, string(part), time.Time{}, blob)
}

// talosArtifactError answers a failure to open the content of the ready row at
// key: content the store lacks is a 404 and puts the row back to wanted; any
// other failure is reported and answered 500, with the row left alone.
func (h *Handlers) talosArtifactError(w http.ResponseWriter, r *http.Request, key fleet.TalosKey, err error) {
	h.talosUnusable(r.Context(), key, err, h.now())
	if store.ContentLost(err) {
		http.NotFound(w, r)
		return
	}
	h.internalError(w, r, err)
}

// talosUnusable reports that the content of the ready row at key failed with
// err. Content the store lacks puts the row back to wanted and is reported as
// errTalosMissing, with the failure of the requeue, if any; content the store
// cannot read is reported as it is and leaves the row alone, since a permission
// or I/O failure is not a reason to fetch it again.
func (h *Handlers) talosUnusable(ctx context.Context, key fleet.TalosKey, err error, at time.Time) {
	if !store.ContentLost(err) {
		h.onError(ctx, fmt.Errorf("boot: read %s content: %w", key.Tag(), err))
		return
	}
	_, rerr := h.repo.RequeueTalos(ctx, key, at)
	h.onError(ctx, errors.Join(fmt.Errorf("%w: %s: %w", errTalosMissing, key.Tag(), err), rerr))
}

// talosLayer returns the layer of layers titled by part when it is the asset
// row records, by digest and size; otherwise the error matches
// store.ErrLayerMissing, since the tag lacks the recorded content.
func talosLayer(layers []ocispec.Descriptor, row fleet.TalosRow, part fleet.TalosPart) (ocispec.Descriptor, error) {
	tag := row.Key().Tag()
	i := slices.IndexFunc(layers, func(l ocispec.Descriptor) bool { return l.Annotations[ocispec.AnnotationTitle] == string(part) })
	if i < 0 {
		return ocispec.Descriptor{}, fmt.Errorf("%s has no %s layer: %w", tag, part, store.ErrLayerMissing)
	}
	l, rec := layers[i], row.Assets[part]
	if l.Digest.String() != "sha256:"+rec.SHA256 || l.Size != rec.Size {
		return ocispec.Descriptor{}, fmt.Errorf("layer %s of %s is %s (%d bytes), not the recorded sha256:%s (%d bytes): %w",
			part, tag, l.Digest, l.Size, rec.SHA256, rec.Size, store.ErrLayerMissing)
	}
	return l, nil
}
