package factorytest

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"
)

// defaultURLTTL is how long an asset URL lives when Options.URLTTL is zero.
const defaultURLTTL = time.Hour

// Options shapes a Fake.
type Options struct {
	// Schematic and Version are the one image the fake serves: a request for any
	// other schematic or version is answered 404.
	Schematic, Version string
	// Assets maps a factory file name ("kernel-amd64") to the bytes it serves.
	Assets map[string][]byte
	// RegisterID is the ID a registration answers with; empty means the hex
	// sha256 of the posted body.
	RegisterID string
	// URLTTL is how long an asset URL is valid after the redirect that names it;
	// zero means an hour.
	URLTTL time.Duration
	// Now is the fake's clock; nil means time.Now.
	Now func() time.Time
}

// asset is one file the fake serves.
type asset struct {
	data []byte
	etag string
}

// Fake is a Talos image factory. It is safe for concurrent use.
type Fake struct {
	opts Options
	mux  *http.ServeMux

	mu       sync.Mutex
	assets   map[string]asset
	requests []string
}

// New returns a Fake for opts.
func New(opts Options) *Fake {
	opts.URLTTL = cmp.Or(opts.URLTTL, defaultURLTTL)
	if opts.Now == nil {
		opts.Now = time.Now
	}
	f := &Fake{opts: opts, mux: http.NewServeMux()}
	f.SetAssets(opts.Assets)
	f.mux.HandleFunc("GET /image/{schematic}/{version}/{file}", f.redirect)
	f.mux.HandleFunc("GET /assets/{token}", f.serveAsset)
	f.mux.HandleFunc("POST /schematics", f.register)
	return f
}

// SetAssets replaces the files the fake serves; a request after the call gets
// the new bytes and the ETags of those bytes. The fake keeps assets' slices, so
// the caller must not change them.
func (f *Fake) SetAssets(assets map[string][]byte) {
	next := make(map[string]asset, len(assets))
	for name, data := range assets {
		sum := sha256.Sum256(data)
		next[name] = asset{data: data, etag: `"` + hex.EncodeToString(sum[:]) + `"`}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assets = next
}

// Requests returns the method and path of every request the fake has served,
// in order, as "GET /image/...".
func (f *Fake) Requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// ServeHTTP implements http.Handler.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	f.mux.ServeHTTP(w, r)
}

// lookup returns the file name, or false when the fake does not have it.
func (f *Fake) lookup(name string) (asset, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.assets[name]
	return a, ok
}

// redirect answers an image request with a 302 to the asset URL of the file,
// valid for URLTTL, as the public factory sends clients to its CDN. The token
// is the file name in hex, so a URL names its file without the fake keeping any
// state about it.
func (f *Fake) redirect(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.PathValue("schematic") != f.opts.Schematic:
		http.Error(w, "unknown schematic "+r.PathValue("schematic"), http.StatusNotFound)
		return
	case r.PathValue("version") != f.opts.Version:
		http.Error(w, "unknown version "+r.PathValue("version"), http.StatusNotFound)
		return
	}
	file := r.PathValue("file")
	if _, ok := f.lookup(file); !ok {
		http.Error(w, "unknown file "+file, http.StatusNotFound)
		return
	}
	expiresSec := f.opts.Now().Add(f.opts.URLTTL).Unix()
	w.Header().Set("Location", "/assets/"+hex.EncodeToString([]byte(file))+"?expires="+strconv.FormatInt(expiresSec, 10))
	w.WriteHeader(http.StatusFound)
}

// serveAsset serves the file a token names, with Range and a strong ETag. Like
// the public factory it ignores If-Range, so a client cannot rely on it to
// refuse a range of changed bytes.
func (f *Fake) serveAsset(w http.ResponseWriter, r *http.Request) {
	expiresSec, err := strconv.ParseInt(r.URL.Query().Get("expires"), 10, 64)
	if err != nil || f.opts.Now().Unix() > expiresSec {
		http.Error(w, "the asset URL has expired", http.StatusForbidden)
		return
	}
	name, err := hex.DecodeString(r.PathValue("token"))
	if err != nil {
		http.Error(w, "unknown asset", http.StatusNotFound)
		return
	}
	a, ok := f.lookup(string(name))
	if !ok {
		http.Error(w, "unknown asset", http.StatusNotFound)
		return
	}

	// A handler must not change its request, so the header is dropped on a clone.
	r = r.Clone(r.Context())
	r.Header.Del("If-Range")
	w.Header().Set("ETag", a.etag)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.data))
}

// register answers a schematic registration with 201 and the ID.
func (f *Fake) register(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := f.opts.RegisterID
	if id == "" {
		sum := sha256.Sum256(body)
		id = hex.EncodeToString(sum[:])
	}
	reply, err := json.Marshal(struct {
		ID        string `json:"id"`
		Schematic string `json:"schematic"`
	}{ID: id, Schematic: string(body)})
	if err != nil {
		http.Error(w, fmt.Sprint(err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(reply) //nolint:errcheck // a connection that broke leaves a test fake nothing to do
}
