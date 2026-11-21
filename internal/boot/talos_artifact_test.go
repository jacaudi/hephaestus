package boot

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"go.jacaudi.dev/hephaestus/internal/fleet"
)

// talosURL is the Talos artifact route of the fixture key's asset.
func talosURL(asset string) string {
	return "/t/" + plainSchematic + "/" + talosVersion + "/amd64/" + asset
}

func TestTalosArtifact_ServesKernelWithRange(t *testing.T) {
	g := newRig(t)
	g.stockTalos(fixtureCmdline)
	kernel := talosContent(fixtureCmdline)[fleet.TalosKernel]

	for _, mux := range []struct {
		name   string
		h      http.Handler
		secure bool
	}{
		{"plain HTTP", g.h.PlainHTTP(), false},
		{"TLS", g.h.TLS(), true},
	} {
		t.Run(mux.name, func(t *testing.T) {
			req := newRequest(t, talosURL("kernel"), mux.secure)
			req.Header.Set("Range", "bytes=10-")
			rec := httptest.NewRecorder()

			mux.h.ServeHTTP(rec, req)

			wantStatus(t, rec, http.StatusPartialContent)
			if !bytes.Equal(rec.Body.Bytes(), kernel[10:]) {
				t.Errorf("served %d bytes, want the kernel's last %d", rec.Body.Len(), len(kernel)-10)
			}
			if got, want := rec.Header().Get("ETag"), `"sha256:`+g.talosRow().Assets[fleet.TalosKernel].SHA256+`"`; got != want {
				t.Errorf("ETag = %q, want %q", got, want)
			}
		})
	}
}

func TestTalosArtifact_ServesInitramfs(t *testing.T) {
	g := newRig(t)
	g.stockTalos(fixtureCmdline)

	rec := g.plain(talosURL("initramfs"))

	wantStatus(t, rec, http.StatusOK)
	if want := talosContent(fixtureCmdline)[fleet.TalosInitramfs]; !bytes.Equal(rec.Body.Bytes(), want) {
		t.Errorf("served %d bytes, want the %d bytes of the initramfs", rec.Body.Len(), len(want))
	}
}

func TestTalosArtifact_UnknownAsset404(t *testing.T) {
	g := newRig(t)
	g.stockTalos(fixtureCmdline)
	cases := []struct{ name, target string }{
		{"cmdline", talosURL("cmdline")},
		{"other asset", talosURL("x")},
		{"other version", "/t/" + plainSchematic + "/v1.12.10/amd64/kernel"},
		{"other arch", "/t/" + plainSchematic + "/" + talosVersion + "/arm64/kernel"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantStatus(t, g.plain(tc.target), http.StatusNotFound)
		})
	}
}

func TestTalosArtifact_RowNotReady404(t *testing.T) {
	g := newRig(t)
	g.pushTalos(fixtureCmdline)
	if _, err := fleet.WantTalosTx(t.Context(), g.d.Bun(), fleet.TalosWant{Key: fixtureTalosKey()}, testNow()); err != nil {
		t.Fatalf("WantTalosTx: %v", err)
	}

	wantStatus(t, g.plain(talosURL("kernel")), http.StatusNotFound)

	if len(g.errs) != 0 {
		t.Errorf("reported errors = %v, want none", g.errs)
	}
}

func TestTalosArtifact_LostLayerRequeues404(t *testing.T) {
	g := newRig(t)
	g.stockTalos(fixtureCmdline)
	if err := os.Remove(g.talosBlob(fleet.TalosInitramfs)); err != nil {
		t.Fatalf("remove initramfs blob: %v", err)
	}

	wantStatus(t, g.plain(talosURL("initramfs")), http.StatusNotFound)

	if got := g.talosRow().State; got != fleet.CacheWanted {
		t.Errorf("Talos row state = %q, want %q", got, fleet.CacheWanted)
	}
	if len(g.errs) != 1 || !errors.Is(g.errs[0], errTalosMissing) {
		t.Errorf("reported errors = %v, want one errTalosMissing", g.errs)
	}
}

func TestTalosArtifact_UnreadableIs500(t *testing.T) {
	g := newRig(t)
	g.stockTalos(fixtureCmdline)
	g.makeUnreadable(g.talosBlob(fleet.TalosKernel))

	wantStatus(t, g.plain(talosURL("kernel")), http.StatusInternalServerError)

	if got := g.talosRow().State; got != fleet.CacheReady {
		t.Errorf("Talos row state = %q, want it left %q", got, fleet.CacheReady)
	}
	if len(g.errs) != 1 || errors.Is(g.errs[0], errTalosMissing) {
		t.Errorf("reported errors = %v, want one that is not errTalosMissing", g.errs)
	}
}
