package factorytest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testSchematic = "376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba"
	testVersion   = "v1.12.11"
	testKernel    = "kernel-amd64"
)

// reply is a response read to the end.
type reply struct {
	status int
	header http.Header
	body   string
}

// serve sends one request to f and returns the whole response.
func serve(tb testing.TB, f *Fake, method, target string, header http.Header, body string) reply {
	tb.Helper()
	req := httptest.NewRequestWithContext(tb.Context(), method, target, strings.NewReader(body))
	maps.Copy(req.Header, header)
	rec := httptest.NewRecorder()
	f.ServeHTTP(rec, req)
	resp := rec.Result()
	defer func() {
		if err := resp.Body.Close(); err != nil {
			tb.Errorf("close body: %v", err)
		}
	}()
	text, err := io.ReadAll(resp.Body)
	if err != nil {
		tb.Fatalf("read body: %v", err)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: string(text)}
}

func imagePath(file string) string {
	return "/image/" + testSchematic + "/" + testVersion + "/" + file
}

func TestFake_RedirectRangeAndExpiry(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	now := t0
	f := New(Options{
		Schematic: testSchematic,
		Version:   testVersion,
		Assets:    map[string][]byte{testKernel: []byte("0123456789")},
		URLTTL:    time.Minute,
		Now:       func() time.Time { return now },
	})

	redirect := serve(t, f, http.MethodGet, imagePath(testKernel), nil, "")
	if redirect.status != http.StatusFound {
		t.Fatalf("image status = %d, want %d", redirect.status, http.StatusFound)
	}
	loc, err := url.Parse(redirect.header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if !strings.HasPrefix(loc.Path, "/assets/") {
		t.Errorf("Location path = %q, want prefix /assets/", loc.Path)
	}
	if got, want := loc.Query().Get("expires"), strconv.FormatInt(t0.Add(time.Minute).Unix(), 10); got != want {
		t.Errorf("Location expires = %q, want %q", got, want)
	}

	full := serve(t, f, http.MethodGet, loc.RequestURI(), nil, "")
	if full.status != http.StatusOK || full.body != "0123456789" {
		t.Fatalf("asset = %d %q, want 200 0123456789", full.status, full.body)
	}
	etag := full.header.Get("ETag")
	if etag == "" || strings.HasPrefix(etag, "W/") {
		t.Fatalf("asset ETag = %q, want a strong ETag", etag)
	}

	// The real factory ignores If-Range: a mismatched validator still gets the range.
	part := serve(t, f, http.MethodGet, loc.RequestURI(), http.Header{"Range": {"bytes=5-"}, "If-Range": {`"another"`}}, "")
	if part.status != http.StatusPartialContent || part.body != "56789" {
		t.Errorf("ranged asset = %d %q, want 206 56789", part.status, part.body)
	}
	if got := part.header.Get("ETag"); got != etag {
		t.Errorf("ranged ETag = %q, want %q", got, etag)
	}
	if got, want := part.header.Get("Content-Range"), "bytes 5-9/10"; got != want {
		t.Errorf("Content-Range = %q, want %q", got, want)
	}

	now = t0.Add(2 * time.Minute)
	if expired := serve(t, f, http.MethodGet, loc.RequestURI(), nil, ""); expired.status != http.StatusForbidden {
		t.Errorf("expired asset status = %d, want %d", expired.status, http.StatusForbidden)
	}

	for _, target := range []string{
		"/image/" + testSchematic + "/v9.9.9/" + testKernel,
		"/image/" + strings.Repeat("0", 64) + "/" + testVersion + "/" + testKernel,
		imagePath("kernel-s390x"),
	} {
		missing := serve(t, f, http.MethodGet, target, nil, "")
		if missing.status != http.StatusNotFound || missing.body == "" || !strings.HasPrefix(missing.header.Get("Content-Type"), "text/plain") {
			t.Errorf("GET %s = %d %q (%s), want 404 with a text body", target, missing.status, missing.body, missing.header.Get("Content-Type"))
		}
	}
}

func TestFake_SetAssets(t *testing.T) {
	f := New(Options{Schematic: testSchematic, Version: testVersion, Assets: map[string][]byte{testKernel: []byte("old")}})
	fetch := func() reply {
		t.Helper()
		redirect := serve(t, f, http.MethodGet, imagePath(testKernel), nil, "")
		if redirect.status != http.StatusFound {
			t.Fatalf("image status = %d, want %d", redirect.status, http.StatusFound)
		}
		return serve(t, f, http.MethodGet, redirect.header.Get("Location"), nil, "")
	}

	before := fetch()
	f.SetAssets(map[string][]byte{testKernel: []byte("new bytes")})
	after := fetch()

	if before.body != "old" || after.body != "new bytes" {
		t.Errorf("bodies = %q then %q, want old then new bytes", before.body, after.body)
	}
	if before.header.Get("ETag") == after.header.Get("ETag") {
		t.Errorf("ETag stayed %q across different bytes", before.header.Get("ETag"))
	}
}

func TestFake_Register(t *testing.T) {
	body := "customization:\n  extraKernelArgs: [a]\n"
	sum := sha256.Sum256([]byte(body))
	tests := []struct {
		name       string
		registerID string
		wantID     string
	}{
		{name: "hash of the body", wantID: hex.EncodeToString(sum[:])},
		{name: "told id", registerID: "told", wantID: "told"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := New(Options{RegisterID: tt.registerID})
			got := serve(t, f, http.MethodPost, "/schematics", http.Header{"Content-Type": {"application/yaml"}}, body)
			if got.status != http.StatusCreated {
				t.Fatalf("status = %d, want %d", got.status, http.StatusCreated)
			}
			var reply struct {
				ID        string `json:"id"`
				Schematic string `json:"schematic"`
			}
			if err := json.Unmarshal([]byte(got.body), &reply); err != nil {
				t.Fatalf("decode %q: %v", got.body, err)
			}
			if reply.ID != tt.wantID || reply.Schematic != body {
				t.Errorf("reply = %+v, want id %q schematic %q", reply, tt.wantID, body)
			}
		})
	}
}

func TestFake_Requests(t *testing.T) {
	f := New(Options{Schematic: testSchematic, Version: testVersion, Assets: map[string][]byte{testKernel: []byte("k")}})
	serve(t, f, http.MethodPost, "/schematics", nil, "x")
	redirect := serve(t, f, http.MethodGet, imagePath(testKernel), nil, "")
	loc, err := url.Parse(redirect.header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	serve(t, f, http.MethodGet, loc.RequestURI(), nil, "")

	want := []string{"POST /schematics", "GET " + imagePath(testKernel), "GET " + loc.Path}
	got := f.Requests()
	if !slices.Equal(got, want) {
		t.Errorf("Requests() = %q, want %q", got, want)
	}
	got[0] = "changed"
	if f.Requests()[0] != want[0] {
		t.Error("Requests() shares its backing array with the fake")
	}
}
