package factory

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"go.jacaudi.dev/hephaestus/internal/factory/factorytest"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

const (
	testSchematic = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"
	testVersion   = "v1.12.11"
)

// startTLS serves h over TLS 1.3 and returns the server with a client that trusts it.
func startTLS(tb testing.TB, h http.Handler) (*httptest.Server, *http.Client) {
	tb.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	tb.Cleanup(srv.Close)
	hc := srv.Client()
	hc.Transport.(*http.Transport).DisableKeepAlives = true //nolint:forcetypeassert // httptest's client always has an *http.Transport
	return srv, hc
}

func TestFileName(t *testing.T) {
	tests := []struct {
		part fleet.TalosPart
		arch fleet.Arch
		want string
	}{
		{fleet.TalosKernel, fleet.ArchAMD64, "kernel-amd64"},
		{fleet.TalosInitramfs, fleet.ArchAMD64, "initramfs-amd64.xz"},
		{fleet.TalosCmdline, fleet.ArchAMD64, "cmdline-metal-amd64"},
		{fleet.TalosKernel, fleet.ArchARM64, "kernel-arm64"},
		{fleet.TalosInitramfs, fleet.ArchARM64, "initramfs-arm64.xz"},
		{fleet.TalosCmdline, fleet.ArchARM64, "cmdline-metal-arm64"},
	}
	for _, tt := range tests {
		t.Run(string(tt.part)+"/"+string(tt.arch), func(t *testing.T) {
			if got := FileName(tt.part, tt.arch); got != tt.want {
				t.Errorf("FileName(%q, %q) = %q, want %q", tt.part, tt.arch, got, tt.want)
			}
		})
	}
}

func TestClient_URL(t *testing.T) {
	c := New("https://f.test", nil)
	got := c.URL(testSchematic, testVersion, fleet.ArchAMD64, fleet.TalosKernel)
	want := "https://f.test/image/" + testSchematic + "/" + testVersion + "/kernel-amd64"
	if got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}
}

func TestClient_URL_ServedByFake(t *testing.T) {
	fake := factorytest.New(factorytest.Options{
		Schematic: testSchematic,
		Version:   testVersion,
		Assets:    map[string][]byte{FileName(fleet.TalosInitramfs, fleet.ArchARM64): []byte("initramfs bytes")},
	})
	srv, hc := startTLS(t, fake)
	c := New(srv.URL, hc)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, c.URL(testSchematic, testVersion, fleet.ArchARM64, fleet.TalosInitramfs), http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(got) != "initramfs bytes" {
		t.Errorf("GET = %d %q, want 200 initramfs bytes", resp.StatusCode, got)
	}
}

// capture records the content type and body of every request it passes on to next.
type capture struct {
	next        http.Handler
	contentType string
	body        []byte
}

func (c *capture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.contentType = r.Header.Get("Content-Type")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	c.body = body
	r.Body = io.NopCloser(bytes.NewReader(body))
	c.next.ServeHTTP(w, r)
}

func TestClient_Register(t *testing.T) {
	body := []byte("customization:\n  systemExtensions:\n    officialExtensions:\n      - siderolabs/iscsi-tools\n")
	rec := &capture{next: factorytest.New(factorytest.Options{RegisterID: testSchematic})}
	srv, hc := startTLS(t, rec)

	got, err := New(srv.URL, hc).Register(t.Context(), body)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got != testSchematic {
		t.Errorf("id = %q, want %q", got, testSchematic)
	}
	if rec.contentType != "application/yaml" {
		t.Errorf("Content-Type = %q, want application/yaml", rec.contentType)
	}
	if !bytes.Equal(rec.body, body) {
		t.Errorf("posted body = %q, want %q", rec.body, body)
	}
}

func TestClient_RegisterStatuses(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv, hc := startTLS(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				fmt.Fprintf(w, `{"id": %q}`, testSchematic)
			}))
			got, err := New(srv.URL, hc).Register(t.Context(), []byte("{}"))
			if err != nil || got != testSchematic {
				t.Errorf("Register = %q, %v, want %q, nil", got, err, testSchematic)
			}
		})
	}
}

func TestClient_RegisterError(t *testing.T) {
	// 2 KiB of text that has to be cut and cleaned: control characters, then a
	// multi-byte rune the cut at 1024 bytes lands in the middle of.
	reply := strings.Repeat("bad schematic\x07\n", 59) + strings.Repeat("\xc3\xa9", 1100)
	srv, hc := startTLS(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, reply, http.StatusBadRequest)
	}))

	id, err := New(srv.URL, hc).Register(t.Context(), []byte("bad"))
	if id != "" {
		t.Errorf("id = %q, want empty", id)
	}
	se, ok := errors.AsType[*StatusError](err)
	if !ok {
		t.Fatalf("error = %v, want a *StatusError", err)
	}
	if se.Status != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", se.Status, http.StatusBadRequest)
	}
	if se.Body == "" || len(se.Body) > fleet.MaxCauseBytes {
		t.Errorf("len(Body) = %d, want 1..%d", len(se.Body), fleet.MaxCauseBytes)
	}
	if !utf8.ValidString(se.Body) || strings.ContainsFunc(se.Body, func(r rune) bool { return !unicode.IsGraphic(r) }) {
		t.Errorf("Body = %q, want printable text", se.Body)
	}
	if !strings.HasPrefix(se.Body, "bad schematic") {
		t.Errorf("Body = %.40q..., want the start of the reply", se.Body)
	}
	want := "the image factory answered 400 Bad Request: " + se.Body
	if got := se.Error(); got != want {
		t.Errorf("Error() = %.60q..., want %.60q...", got, want)
	}
}

func TestClient_RegisterBadReply(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not JSON", "created"},
		{"no id", `{"schematic": "x"}`},
		{"empty id", `{"id": ""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, hc := startTLS(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, tt.body)
			}))
			id, err := New(srv.URL, hc).Register(t.Context(), []byte("{}"))
			if err == nil || id != "" {
				t.Errorf("Register = %q, %v, want an error and no id", id, err)
			}
			if _, ok := errors.AsType[*StatusError](err); ok {
				t.Errorf("a 201 with a bad body is a *StatusError: %v", err)
			}
		})
	}
}
