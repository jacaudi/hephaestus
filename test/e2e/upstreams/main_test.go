package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
	"go.uber.org/goleak"

	"go.jacaudi.dev/hephaestus/internal/factory"
	"go.jacaudi.dev/hephaestus/internal/fleet"
	"go.jacaudi.dev/hephaestus/internal/render"
	"go.jacaudi.dev/hephaestus/internal/tlscert/tlscerttest"
)

// fixtureDir is the Topf tree the harness mounts as UPSTREAMS_FIXTURE_DIR.
const fixtureDir = "../fixture-cluster"

// running is a started program and a client that reaches it under every host name it serves.
type running struct {
	client *http.Client
	log    *bytes.Buffer // the program's log; read it only after stop
	stop   func()        // safe to call twice
}

// start runs the program on a free loopback port with fresh self-signed leaves for its three host names, and returns
// a client that trusts them and sends every connection to that port, so each request carries its host name as SNI.
// stop ends the program and fails the test if run returned an error.
func start(t *testing.T) *running {
	t.Helper()
	pool := x509.NewCertPool()
	leaf := func(host string) (certFile, keyFile string) {
		certFile, keyFile = tlscerttest.WriteSelfSigned(t, t.TempDir(), host, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		pem, err := os.ReadFile(certFile)
		if err != nil {
			t.Fatalf("read %s: %v", certFile, err)
		}
		pool.AppendCertsFromPEM(pem)
		return certFile, keyFile
	}

	addr := freeAddr(t)
	var log bytes.Buffer
	cfg := config{Addr: addr, FixtureDir: fixtureDir, WorkDir: t.TempDir(), Log: &log}
	cfg.FactoryCertFile, cfg.FactoryKeyFile = leaf(factoryHost)
	cfg.AssetsCertFile, cfg.AssetsKeyFile = leaf(assetsHost)
	cfg.GitCertFile, cfg.GitKeyFile = leaf(gitHost)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg) }()
	stop := sync.OnceFunc(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	})
	waitListening(t, addr, done)

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &running{
		client: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13},
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, network, addr)
				},
				DisableKeepAlives: true,
				// hephaestus's clients clone http.DefaultTransport, which negotiates HTTP/2, so the tests do too.
				ForceAttemptHTTP2: true,
			},
			Timeout: 30 * time.Second,
		},
		log:  &log,
		stop: stop,
	}
}

// freeAddr returns a loopback address that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return addr
}

// waitListening waits until addr accepts a TCP connection, failing the test if run returns first or 10 s pass.
func waitListening(t *testing.T, addr string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("run returned before listening: %v", err)
		default:
		}
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", addr)
		if err == nil {
			if err := conn.Close(); err != nil {
				t.Fatalf("close probe connection: %v", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s after 10 s", addr)
}

// get sends a GET with header and returns the status, the header and the whole body.
func get(t *testing.T, c *http.Client, target string, header http.Header) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, http.NoBody)
	if err != nil {
		t.Fatalf("request %s: %v", target, err)
	}
	maps.Copy(req.Header, header)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}
	if resp.ProtoMajor != 2 {
		t.Errorf("GET %s was answered over %s, want HTTP/2", target, resp.Proto)
	}
	return resp.StatusCode, resp.Header, body
}

// logLine is the part of a request log line the harness reads.
type logLine struct {
	Msg    string `json:"msg"`
	Host   string `json:"host"`
	Proto  string `json:"proto"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Status int    `json:"status"`
}

// requestLines returns the request lines of the program's log.
func requestLines(t *testing.T, log *bytes.Buffer) []logLine {
	t.Helper()
	var lines []logLine
	sc := bufio.NewScanner(log)
	for sc.Scan() {
		var l logLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("log line %q is not JSON: %v", sc.Text(), err)
		}
		if l.Msg == "request" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestUpstreams_FactoryRedirectAndRange(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	u := start(t)
	defer u.stop()

	noFollow := *u.client
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	fc := factory.New("https://"+factoryHost, &noFollow)
	assets := factoryAssets()

	for _, part := range fleet.TalosParts() {
		name := factory.FileName(part, fleet.ArchAMD64)
		want, ok := assets[name]
		if !ok {
			t.Errorf("%s: the program serves no asset %q", part, name)
			continue
		}
		imageURL := fc.URL(plainSchematic, talosVersion, fleet.ArchAMD64, part)

		status, header, _ := get(t, &noFollow, imageURL, nil)
		if status != http.StatusFound {
			t.Errorf("%s: GET %s = %d, want %d", part, imageURL, status, http.StatusFound)
			continue
		}
		loc, err := url.Parse(header.Get("Location"))
		if err != nil {
			t.Fatalf("%s: parse Location: %v", part, err)
		}
		if loc.Scheme != "https" || loc.Host != assetsHost {
			t.Errorf("%s: Location = %s, want an https URL on %s", part, loc, assetsHost)
			continue
		}

		// The image factory ignores If-Range, so a validator that matches nothing still gets the range.
		status, _, body := get(t, &noFollow, loc.String(), http.Header{"Range": {"bytes=1-"}, "If-Range": {`"another"`}})
		if status != http.StatusPartialContent || !bytes.Equal(body, want[1:]) {
			t.Errorf("%s: ranged GET of the asset = %d %q, want %d %q", part, status, body, http.StatusPartialContent, want[1:])
		}

		// The asset lives on the assets host only, so a client that stayed on the factory host gets nothing.
		onFactory := *loc
		onFactory.Host = factoryHost
		if status, _, _ := get(t, &noFollow, onFactory.String(), nil); status != http.StatusNotFound {
			t.Errorf("%s: GET %s = %d, want %d", part, onFactory.String(), status, http.StatusNotFound)
		}
	}

	u.stop()
	var found302, found206 bool
	for _, l := range requestLines(t, u.log) {
		found302 = found302 || (l.Host == factoryHost && l.Method == http.MethodGet && l.Status == http.StatusFound && strings.HasPrefix(l.Path, "/image/"))
		found206 = found206 || (l.Host == assetsHost && l.Status == http.StatusPartialContent && strings.HasPrefix(l.Path, "/assets/"))
	}
	if !found302 || !found206 {
		t.Errorf("request log has a factory 302: %t, an assets 206: %t; want both\n%s", found302, found206, u.log)
	}
}

func TestUpstreams_RegisterSchematic(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	u := start(t)
	defer u.stop()

	body := "customization: {}\n"
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+factoryHost+"/schematics", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		t.Fatalf("POST /schematics: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()
	var reply struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		t.Fatalf("decode the reply: %v", err)
	}
	sum := sha256.Sum256([]byte(body))
	if resp.StatusCode != http.StatusCreated || reply.ID != hex.EncodeToString(sum[:]) {
		t.Errorf("POST /schematics = %d id %q, want %d id %x", resp.StatusCode, reply.ID, http.StatusCreated, sum)
	}
}

func TestUpstreams_GitSmartHTTP(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	u := start(t)
	defer u.stop()

	// go-git picks its transport by scheme from a process-wide table; this test's client reaches the program.
	client.InstallProtocol("https", githttp.NewClient(u.client))
	defer client.InstallProtocol("https", githttp.DefaultClient)

	repo, err := git.CloneContext(t.Context(), memory.NewStorage(), nil, &git.CloneOptions{URL: "https://" + gitHost + "/" + repoName + ".git"})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	if want := plumbing.NewBranchReferenceName("main"); head.Name() != want {
		t.Errorf("HEAD = %s, want %s", head.Name(), want)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("commit %s: %v", head.Hash(), err)
	}
	for _, path := range []string{"lab/topf.yaml", "lab/secrets.yaml"} {
		f, err := commit.File(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		got, err := f.Contents()
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		want, err := os.ReadFile(filepath.Join(fixtureDir, path))
		if err != nil {
			t.Fatalf("read the fixture %s: %v", path, err)
		}
		if got != string(want) {
			t.Errorf("%s in the clone = %q, want the fixture's %q", path, got, want)
		}
	}

	// The CGI handler streams git's pack over HTTP/2, which is what hephaestus negotiates.
	u.stop()
	var gitLines int
	for _, l := range requestLines(t, u.log) {
		if l.Host != gitHost {
			continue
		}
		gitLines++
		if l.Proto != "HTTP/2.0" || l.Status != http.StatusOK {
			t.Errorf("git request %s %s = %s %d, want HTTP/2.0 200", l.Method, l.Path, l.Proto, l.Status)
		}
	}
	if gitLines < 2 {
		t.Errorf("request log has %d git requests, want the ref advertisement and the upload-pack\n%s", gitLines, u.log)
	}
}

func TestUpstreams_MissingEnv(t *testing.T) {
	all := map[string]string{
		"UPSTREAMS_ADDR":              ":443",
		"UPSTREAMS_FACTORY_CERT_FILE": "/certs/factory-fullchain.pem",
		"UPSTREAMS_FACTORY_KEY_FILE":  "/certs/factory-privkey.pem",
		"UPSTREAMS_ASSETS_CERT_FILE":  "/certs/assets-fullchain.pem",
		"UPSTREAMS_ASSETS_KEY_FILE":   "/certs/assets-privkey.pem",
		"UPSTREAMS_GIT_CERT_FILE":     "/certs/git-fullchain.pem",
		"UPSTREAMS_GIT_KEY_FILE":      "/certs/git-privkey.pem",
		"UPSTREAMS_FIXTURE_DIR":       "/src/test/e2e/fixture-cluster",
		"UPSTREAMS_WORK_DIR":          "/work",
	}
	if _, err := loadConfig(func(k string) string { return all[k] }); err != nil {
		t.Fatalf("loadConfig with every variable set: %v", err)
	}
	for missing := range all {
		t.Run(missing, func(t *testing.T) {
			_, err := loadConfig(func(k string) string {
				if k == missing {
					return ""
				}
				return all[k]
			})
			if err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("loadConfig without %s: error = %v, want one naming it", missing, err)
			}
			for other := range all {
				if other != missing && strings.Contains(err.Error(), other+" ") {
					t.Errorf("loadConfig without %s: error %q also names %s", missing, err, other)
				}
			}
		})
	}
	_, err := loadConfig(func(string) string { return "" })
	if err == nil || strings.Count(err.Error(), "UPSTREAMS_") != len(all) {
		t.Errorf("loadConfig with nothing set: error = %v, want all %d variables named together", err, len(all))
	}
}

// The harness boots a machine into the fixture cluster, so a fixture that hephaestus refuses would only show in the
// owner's end-to-end run.
func TestFixtureCluster_Renders(t *testing.T) {
	c, err := render.Load(os.DirFS(filepath.Join(fixtureDir, "lab")))
	if err != nil {
		t.Fatalf("load the fixture: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(fixtureDir, "values.json"))
	if err != nil {
		t.Fatalf("read values.json: %v", err)
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		t.Fatalf("decode values.json: %v", err)
	}
	hosts, err := c.Render(t.Context(), values)
	if err != nil {
		t.Fatalf("render the fixture: %v", err)
	}
	if len(hosts) != 2 {
		t.Errorf("rendered %d hosts, want 2 (cp-01 and wrk-01)", len(hosts))
	}
}
