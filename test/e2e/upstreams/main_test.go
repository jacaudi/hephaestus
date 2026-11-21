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
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/santhosh-tekuri/jsonschema/v6"
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

// The event record's schema and the harness's shell library, relative to this package.
const (
	schemaFile   = "../events.schema.json"
	eventsLib    = "../lib/events.sh"
	eventsSample = "../lib/events_test.sh"
)

// sampleCommit is the git_commit the sample runs record.
const sampleCommit = "0123456789abcdef0123456789abcdef01234567"

func TestEventsSchema_AcceptsAndRejects(t *testing.T) {
	sch, err := compileSchema(schemaFile)
	if err != nil {
		t.Fatalf("compile %s: %v", schemaFile, err)
	}
	const head = `"schema":"hephaestus.e2e.event/v1","ts":"2026-11-19T12:00:00Z","seq":1,`
	ev := func(rest string) string { return "{" + head + rest + "}" }
	runStart := `"type":"run_start","run_id":"20261119T120000Z","out_dir":"build/e2e/20261119T120000Z","git_commit":"` + sampleCommit + `","mode":"file+proxy"`
	check := `"type":"check","phase":"boot-ovmf","name":"held within 120s","result":"pass",`
	runEnd := `"type":"run_end","passed":141,"notes":3,"duration_s":842,`
	tests := []struct {
		name  string
		line  string
		valid bool
	}{
		{"RunStart", ev(runStart), true},
		{"PhaseStart", ev(`"type":"phase_start","phase":"boot-ovmf","title":"Boot ovmf","labels":{"arch":"amd64","firmware":"UEFI","mode":"file"}`), true},
		{"PhaseStartNoLabels", ev(`"type":"phase_start","phase":"toolbox","title":"Toolbox image","labels":{}`), true},
		{"CheckWithDetail", ev(check + `"detail":"after 18s","evidence":["ovmf.serial.log"],"elapsed_s":18`), true},
		{"CheckWithoutDetail", ev(`"type":"check","phase":"boot-seabios","name":"serial not asserted","result":"note","evidence":[],"elapsed_s":40`), true},
		{"PhaseEnd", ev(`"type":"phase_end","phase":"boot-ovmf","passed":7,"failed":0,"notes":1,"duration_s":21`), true},
		{"RunEndPass", ev(runEnd + `"result":"pass","failed":0,"failures":[],"exit_code":0`), true},
		{"RunEndFail", ev(runEnd + `"result":"fail","failed":1,"failures":[{"phase":"config-fetch","name":"the same ticket again gets 404"}],"exit_code":1`), true},
		{"RunEndStoppedWithNoFailedCheck", ev(runEnd + `"result":"fail","failed":0,"failures":[],"exit_code":1`), true},

		{"RunStartMissingMode", ev(strings.Replace(runStart, `,"mode":"file+proxy"`, "", 1)), false},
		{"RunStartExtraField", ev(runStart + `,"colour":true`), false},
		{"OtherSchemaVersion", strings.Replace(ev(runStart), "event/v1", "event/v2", 1), false},
		{"TimeWithFraction", strings.Replace(ev(runStart), "12:00:00Z", "12:00:00.5Z", 1), false},
		{"TimeWithOffset", strings.Replace(ev(runStart), "12:00:00Z", "12:00:00+00:00", 1), false},
		{"TimeOutOfRange", strings.Replace(ev(runStart), "12:00:00Z", "25:00:00Z", 1), false},
		{"SeqZero", strings.Replace(ev(runStart), `"seq":1`, `"seq":0`, 1), false},
		{"SeqString", strings.Replace(ev(runStart), `"seq":1`, `"seq":"1"`, 1), false},
		{"UnknownType", ev(`"type":"check_start","phase":"boot-ovmf"`), false},
		{"CheckUnknownResult", ev(strings.Replace(check, `"pass"`, `"skip"`, 1) + `"evidence":[],"elapsed_s":1`), false},
		{"CheckMissingEvidence", ev(check + `"elapsed_s":1`), false},
		{"CheckAbsoluteEvidence", ev(check + `"evidence":["/etc/hosts"],"elapsed_s":1`), false},
		{"CheckNumericDetail", ev(check + `"detail":200,"evidence":[],"elapsed_s":1`), false},
		{"CheckOutsideAPhase", ev(strings.Replace(check, `"boot-ovmf"`, `""`, 1) + `"evidence":[],"elapsed_s":1`), false},
		{"PhaseIDWithSpace", ev(`"type":"phase_end","phase":"boot ovmf","passed":7,"failed":0,"notes":1,"duration_s":21`), false},
		{"PhaseEndNegativeDuration", ev(`"type":"phase_end","phase":"boot-ovmf","passed":7,"failed":0,"notes":1,"duration_s":-1`), false},
		{"LabelNotAString", ev(`"type":"phase_start","phase":"boot-ovmf","title":"Boot ovmf","labels":{"arch":1}`), false},
		{"RunEndPassWithAFailure", ev(runEnd + `"result":"pass","failed":1,"failures":[{"phase":"p","name":"n"}],"exit_code":1`), false},
		{"RunEndPassNonZeroExit", ev(runEnd + `"result":"pass","failed":0,"failures":[],"exit_code":1`), false},
		{"RunEndFailZeroExit", ev(runEnd + `"result":"fail","failed":1,"failures":[{"phase":"p","name":"n"}],"exit_code":0`), false},
		{"FailureExtraField", ev(runEnd + `"result":"fail","failed":1,"failures":[{"phase":"p","name":"n","detail":"d"}],"exit_code":1`), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst, err := jsonschema.UnmarshalJSON(strings.NewReader(tt.line))
			if err != nil {
				t.Fatalf("decode %s: %v", tt.line, err)
			}
			err = sch.Validate(inst)
			if tt.valid && err != nil {
				t.Errorf("the schema rejects a valid event: %v\n%s", err, tt.line)
			}
			if !tt.valid && err == nil {
				t.Errorf("the schema accepts an invalid event:\n%s", tt.line)
			}
		})
	}
}

// phaseLine is one valid event with the given seq.
func phaseLine(seq int) string {
	return fmt.Sprintf(`{"schema":"hephaestus.e2e.event/v1","ts":"2026-11-19T12:00:00Z","seq":%d,"type":"phase_start","phase":"p","title":"P","labels":{}}`, seq) + "\n"
}

func TestValidateEvents_File(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantN   int
		wantErr string // a substring of the error; empty means no error
	}{
		{"InOrder", phaseLine(1) + phaseLine(2) + phaseLine(3), 3, ""},
		{"NoTrailingNewline", phaseLine(1) + strings.TrimSuffix(phaseLine(2), "\n"), 2, ""},
		{"SeqGap", phaseLine(1) + phaseLine(3), 0, "line 2: seq 3, want 2"},
		{"SeqRepeated", phaseLine(1) + phaseLine(1), 0, "line 2: seq 1, want 2"},
		{"BlankLine", phaseLine(1) + "\n" + phaseLine(2), 0, "line 2:"},
		{"NotJSON", phaseLine(1) + "PASS held\n", 0, "line 2:"},
		{"SchemaViolation", phaseLine(1) + `{"seq":2}` + "\n", 0, "line 2:"},
		{"Empty", "", 0, "no events"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			n, err := validateEvents(schemaFile, path)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("validateEvents = %v, want no error", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("validateEvents error = %v, want one containing %q", err, tt.wantErr)
			case n != tt.wantN:
				t.Errorf("validateEvents counted %d events, want %d", n, tt.wantN)
			}
		})
	}
}

func TestCLI_ValidateEvents(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.jsonl")
	bad := filepath.Join(dir, "bad.jsonl")
	for path, body := range map[string]string{good: phaseLine(1) + phaseLine(2), bad: phaseLine(2)} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	noEnv := func(string) string { return "" }
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"Valid", []string{"validate-events", schemaFile, good}, 0, good + " matches " + schemaFile + " (2 events)", ""},
		{"Invalid", []string{"validate-events", schemaFile, bad}, exitError, "", "line 1: seq 2, want 1"},
		{"MissingFile", []string{"validate-events", schemaFile, filepath.Join(dir, "absent.jsonl")}, exitError, "", "absent.jsonl"},
		{"MissingArgument", []string{"validate-events", schemaFile}, exitUsage, "", "usage: upstreams validate-events <schema> <events.jsonl>"},
		{"UnknownSubcommand", []string{"validate"}, exitUsage, "", `unknown subcommand "validate"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := cli(t.Context(), tt.args, noEnv, &stdout, &stderr)
			if code != tt.wantCode || !strings.Contains(stdout.String(), tt.wantStdout) || !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("cli(%q) = %d\nstdout: %s\nstderr: %s\nwant %d, stdout containing %q, stderr containing %q",
					tt.args, code, &stdout, &stderr, tt.wantCode, tt.wantStdout, tt.wantStderr)
			}
		})
	}
}

// event is one line of events.jsonl, every type's fields together. Detail is a pointer so that an absent detail is nil.
type event struct {
	Schema    string            `json:"schema"`
	TS        string            `json:"ts"`
	Seq       int               `json:"seq"`
	Type      string            `json:"type"`
	RunID     string            `json:"run_id"`
	OutDir    string            `json:"out_dir"`
	GitCommit string            `json:"git_commit"`
	Mode      string            `json:"mode"`
	Phase     string            `json:"phase"`
	Title     string            `json:"title"`
	Labels    map[string]string `json:"labels"`
	Name      string            `json:"name"`
	Result    string            `json:"result"`
	Detail    *string           `json:"detail"`
	Evidence  []string          `json:"evidence"`
	ElapsedS  int               `json:"elapsed_s"`
	Passed    int               `json:"passed"`
	Failed    int               `json:"failed"`
	Notes     int               `json:"notes"`
	DurationS int               `json:"duration_s"`
	Failures  []failure         `json:"failures"`
	ExitCode  int               `json:"exit_code"`
}

// failure is one entry of run_end's failures.
type failure struct {
	Phase string `json:"phase"`
	Name  string `json:"name"`
}

// runSample runs the emitter's test sample into a new directory with E2E_COLOR set to colour, and returns the
// directory and the console the sample printed.
func runSample(t *testing.T, colour string) (dir, console string) {
	t.Helper()
	dir = t.TempDir()
	cmd := exec.CommandContext(t.Context(), "sh", eventsSample, dir)
	cmd.Env = append(os.Environ(), "E2E_COLOR="+colour)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sh %s: %v\n%s", eventsSample, err, &stderr)
	}
	if stderr.Len() > 0 {
		t.Errorf("sh %s wrote to stderr:\n%s", eventsSample, &stderr)
	}
	return dir, string(out)
}

// readEvents validates the record at path against the schema and decodes it.
func readEvents(t *testing.T, path string) []event {
	t.Helper()
	if _, err := validateEvents(schemaFile, path); err != nil {
		t.Fatalf("validate %s: %v", path, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var evs []event
	for line := range strings.Lines(string(raw)) {
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		evs = append(evs, e)
	}
	return evs
}

// findEvent returns the event of the type in the phase, with the name when name is not empty.
func findEvent(t *testing.T, evs []event, typ, phase, name string) event {
	t.Helper()
	for i := range evs {
		if e := &evs[i]; e.Type == typ && e.Phase == phase && (name == "" || e.Name == name) {
			return *e
		}
	}
	t.Fatalf("no %s event in phase %q named %q", typ, phase, name)
	return event{}
}

// The sample's escaping phase: a backslash is built at run time there, so neither file holds a backslash-u sequence.
const (
	escapedName1   = `name with "quotes" and a \backslash`
	escapedDetail1 = "detail with a tab\tand \"quotes\""
	escapedDetail2 = "first line\nsecond line"
	escapedDetail3 = "control \x01 and carriage return \r characters"
	nonASCIIName   = "non-ASCII: café · ▸ ━ ─ ✓"
)

// escapedName2 holds a newline and a backslash followed by "u00e9", which must stay six characters, not become é.
var escapedName2 = "name with a newline\nand an escape-looking \\" + "u00e9 \\n"

func TestEvents_SampleRecord(t *testing.T) {
	dir, _ := runSample(t, "0")
	evs := readEvents(t, filepath.Join(dir, "fail", "events.jsonl"))

	first, last := evs[0], evs[len(evs)-1]
	if first.Type != "run_start" || first.RunID != "20261120T000000Z" || first.OutDir != filepath.Join(dir, "fail") ||
		first.GitCommit != sampleCommit || first.Mode != "file+proxy" || first.TS != "2026-11-20T00:00:00Z" {
		t.Errorf("first event = %+v, want the sample's run_start", first)
	}
	wantFailures := []failure{{"install-boot", "the same ticket again gets 404"}, {"escapes", escapedName2}}
	if last.Type != "run_end" || last.Result != "fail" || last.Passed != 8 || last.Failed != 2 || last.Notes != 3 ||
		last.DurationS != 4081 || last.ExitCode != 1 || !slices.Equal(last.Failures, wantFailures) || last.TS != "2026-11-20T01:08:01Z" {
		t.Errorf("last event = %+v, want run_end fail, 8 passed, 2 failed, 3 notes, 4081 s, exit 1, failures %q", last, wantFailures)
	}

	held := findEvent(t, evs, "check", "boot-ovmf", "held within 120s")
	if held.Result != "pass" || held.Detail == nil || *held.Detail != "after 18s" || !slices.Equal(held.Evidence, []string{"ovmf.serial.log"}) || held.ElapsedS != 32 {
		t.Errorf("held = %+v, want pass, detail after 18s, evidence ovmf.serial.log (relative to the run directory), 32 s in", held)
	}
	start := findEvent(t, evs, "check", "boot-ovmf", "/start 200 over plain HTTP (tls:false)")
	if start.Detail != nil || start.Evidence == nil || len(start.Evidence) != 0 {
		t.Errorf("/start = %+v, want no detail and an empty evidence list", start)
	}
	refused := findEvent(t, evs, "check", "install-boot", "the same ticket again gets 404")
	if refused.Result != "fail" || refused.Detail == nil || *refused.Detail != "200" ||
		!slices.Equal(refused.Evidence, []string{"config-fetch-2.txt", "hephaestus-file.log"}) || refused.ElapsedS != 349 {
		t.Errorf("refused = %+v, want fail, detail 200, two evidence files, 349 s in", refused)
	}
	for _, c := range []struct{ name, result, detail string }{
		{escapedName1, "pass", escapedDetail1},
		{escapedName2, "fail", escapedDetail2},
		{nonASCIIName, "note", escapedDetail3},
	} {
		got := findEvent(t, evs, "check", "escapes", c.name)
		if got.Result != c.result || got.Detail == nil || *got.Detail != c.detail {
			t.Errorf("escapes check %q = %+v, want %s with detail %q", c.name, got, c.result, c.detail)
		}
	}

	ovmf := findEvent(t, evs, "phase_start", "boot-ovmf", "")
	if ovmf.Title != "Boot ovmf" || !maps.Equal(ovmf.Labels, map[string]string{"arch": "amd64", "firmware": "UEFI", "mode": "file"}) {
		t.Errorf("boot-ovmf phase_start = %+v, want title Boot ovmf and its three labels", ovmf)
	}
	if toolbox := findEvent(t, evs, "phase_start", "toolbox", ""); toolbox.Labels == nil || len(toolbox.Labels) != 0 {
		t.Errorf("toolbox phase_start labels = %v, want an empty object", toolbox.Labels)
	}
	for phase, want := range map[string][4]int{"toolbox": {0, 0, 0, 14}, "install-boot": {2, 1, 1, 312}, "soak": {1, 0, 0, 3720}} {
		e := findEvent(t, evs, "phase_end", phase, "")
		if got := [4]int{e.Passed, e.Failed, e.Notes, e.DurationS}; got != want {
			t.Errorf("%s phase_end passed, failed, notes, duration = %v, want %v", phase, got, want)
		}
	}

	// Every phase_end and the run_end count exactly the checks emitted between them.
	counts := map[string]*[3]int{}
	total := [3]int{}
	open := ""
	for i := range evs {
		e := &evs[i]
		switch e.Type {
		case "phase_start":
			if open != "" {
				t.Errorf("phase %s starts while %s is open", e.Phase, open)
			}
			open, counts[e.Phase] = e.Phase, &[3]int{}
		case "check":
			if e.Phase != open {
				t.Errorf("check %q names phase %s, want the open phase %s", e.Name, e.Phase, open)
				continue
			}
			i := map[string]int{"pass": 0, "fail": 1, "note": 2}[e.Result]
			counts[open][i]++
			total[i]++
		case "phase_end":
			if got := [3]int{e.Passed, e.Failed, e.Notes}; e.Phase != open || got != *counts[open] {
				t.Errorf("phase_end %s counts %v, want the open phase %s and its checks %v", e.Phase, got, open, *counts[open])
			}
			open = ""
		}
	}
	if got := [3]int{last.Passed, last.Failed, last.Notes}; got != total || open != "" {
		t.Errorf("run_end counts %v with phase %q open, want %v and every phase closed", got, open, total)
	}

	for session, want := range map[string]event{
		"pass":    {Result: "pass", Passed: 1, Notes: 1, DurationS: 5},
		"stopped": {Result: "fail", ExitCode: 1, DurationS: 2},
	} {
		evs := readEvents(t, filepath.Join(dir, session, "events.jsonl"))
		e := evs[len(evs)-1]
		if e.Type != "run_end" || e.Result != want.Result || e.Passed != want.Passed || e.Failed != 0 || e.Notes != want.Notes ||
			e.DurationS != want.DurationS || e.ExitCode != want.ExitCode || e.Failures == nil || len(e.Failures) != 0 {
			t.Errorf("%s run_end = %+v, want %+v with no failures", session, e, want)
		}
	}
}

func TestEvents_Console(t *testing.T) {
	dir, console := runSample(t, "0")
	if strings.ContainsRune(console, '\x1b') {
		t.Errorf("the console with colour off holds an escape character:\n%s", console)
	}
	lines := strings.Split(console, "\n")
	for _, prefix := range []string{"━━ hephaestus e2e · run 20261120T000000Z · file+proxy mode ━━", "━━ Summary ━━"} {
		i := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, prefix) })
		if i < 0 || utf8.RuneCountInString(lines[i]) != 72 || !strings.HasSuffix(lines[i], "━━") {
			t.Errorf("no 72-column rule line starting %q", prefix)
		}
	}
	row := func(phase, pass, fail, took string) string {
		return fmt.Sprintf("  %-12s  %4s  %4s  %6s", phase, pass, fail, took)
	}
	fail := filepath.Join(dir, "fail")
	want := []string{
		"▸ Toolbox image",
		"  ── 0 passed · 0 failed · 14s",
		"▸ Boot ovmf  amd64 · UEFI · file mode",
		"  PASS  held within 120s (after 18s)",
		"  PASS  dnsmasq leased an address to 52:54:00:e2:e0:03 (10.99.0.131)",
		"  PASS  /start 200 over plain HTTP (tls:false)",
		"  ── 3 passed · 0 failed · 21s",
		"▸ Boot seabios  amd64 · BIOS · file mode",
		"  NOTE  serial not asserted (SeaBIOS writes to VGA only); the server log above is the evidence",
		"  ── 1 passed · 0 failed · 1 note · 14s",
		"▸ Install boot install-ovmf  52:54:00:e2:e0:03 · cluster lab · cp-01",
		"  FAIL  the same ticket again gets 404",
		"        got:      200",
		"        evidence: config-fetch-2.txt, hephaestus-file.log",
		"  ── 2 passed · 1 failed · 1 note · 5m12s",
		"  PASS  " + escapedName1 + " (" + escapedDetail1 + ")",
		"  FAIL  name with a newline",
		"  ── 1 passed · 1 failed · 1 note · 0s",
		"  ── 1 passed · 0 failed · 1h02m",
		row("phase", "pass", "fail", "time"),
		row("toolbox", "0", "0", "14s"),
		row("boot-ovmf", "3", "0", "21s"),
		row("boot-seabios", "1", "0", "14s"),
		row("install-boot", "2", "1", "5m12s"),
		row("escapes", "1", "1", "0s"),
		row("soak", "1", "0", "1h02m"),
		"  RESULT  FAIL   8 passed · 2 failed · 3 notes · 1h08m",
		"  FAIL  install-boot · the same ticket again gets 404",
		"  FAIL  escapes · name with a newline",
		"  record:       " + fail + "/events.jsonl",
		"  full output:  " + fail + "/run.log",
		"  manifest:     " + fail + "/manifest.json",
		"  RESULT  PASS   1 passed · 0 failed · 1 note · 5s",
		"  RESULT  FAIL   0 passed · 0 failed · 2s",
		"  stopped: exit status 1 with no failed check; run.log holds the cause",
	}
	for _, w := range want {
		if !slices.Contains(lines, w) {
			t.Errorf("the console has no line %q", w)
		}
	}
	// A phase header follows a blank line, so phases stand apart.
	for i, l := range lines {
		if strings.HasPrefix(l, "▸ ") && (i == 0 || lines[i-1] != "") {
			t.Errorf("phase header %q does not follow a blank line", l)
		}
	}
	if t.Failed() {
		t.Logf("console:\n%s", console)
	}
}

func TestEvents_ColourOnlyAddsSGR(t *testing.T) {
	plainDir, plain := runSample(t, "0")
	colourDir, coloured := runSample(t, "1")
	if !strings.Contains(coloured, "\x1b[1;32mPASS\x1b[0m") || !strings.Contains(coloured, "\x1b[1;31mFAIL\x1b[0m") {
		t.Errorf("the console with colour on has no coloured PASS and FAIL:\n%q", coloured)
	}
	sgr := regexp.MustCompile("\x1b\\[[0-9;]*m")
	got := strings.ReplaceAll(sgr.ReplaceAllString(coloured, ""), colourDir, "<dir>")
	if want := strings.ReplaceAll(plain, plainDir, "<dir>"); got != want {
		t.Errorf("the coloured console without its SGR codes differs from the plain one:\n%s\nwant:\n%s", got, want)
	}
}

func TestEvents_RealClockRecordValidates(t *testing.T) {
	dir := t.TempDir()
	script := `. "$0"; ev_init "$1" 20261120T000000Z ` + sampleCommit + ` file+proxy 0; step only 'The only phase'; pass 'a check'; ev_end 0`
	cmd := exec.CommandContext(t.Context(), "sh", "-c", script, eventsLib, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh: %v\n%s", err, out)
	}
	if n := len(readEvents(t, filepath.Join(dir, "events.jsonl"))); n != 5 {
		t.Errorf("the record holds %d events, want 5", n)
	}
}

// captureScript sources the library ($0), runs capture <log> <command...> under set -eu as run.sh does, then prints
// what capture returned and exits with it.
const captureScript = `. "$0"; set -eu; st=0; log=$1; shift; capture "$log" "$@" || st=$?; echo "capture returned $st"; exit "$st"`

func TestCapture_KeepsExitStatusAndOutput(t *testing.T) {
	log := filepath.Join(t.TempDir(), "run.log")
	inner := `echo out; echo err >&2; printf '\033[1;31mred\033[0m plain\n'; exit 3`
	cmd := exec.CommandContext(t.Context(), "sh", "-c", captureScript, eventsLib, log, "sh", "-c", inner)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 3 {
		t.Errorf("the wrapper exited with %v, want exit status 3", err)
	}
	if want := "out\nerr\n\x1b[1;31mred\x1b[0m plain\ncapture returned 3\n"; stdout.String() != want || stderr.Len() != 0 {
		t.Errorf("stdout = %q, stderr = %q; want stdout %q and nothing on stderr", &stdout, &stderr, want)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read %s: %v", log, err)
	}
	if want := "out\nerr\nred plain\n"; string(got) != want {
		t.Errorf("run.log = %q, want %q (stdout and stderr, without colour codes)", got, want)
	}
	if _, err := os.Stat(log + ".status"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat %s.status = %v, want it removed", log, err)
	}
}

func TestCapture_InterruptWaitsForTheRunsCleanup(t *testing.T) {
	log := filepath.Join(t.TempDir(), "run.log")
	// The run's own traps, as run.sh's: SIGINT exits 130, and the EXIT trap cleans up and prints.
	inner := `trap 'exit 130' INT; trap 'echo cleanup ran' EXIT; echo ready; while :; do sleep 1; done`
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", captureScript, eventsLib, log, "sh", "-c", inner)
	// Its own process group, so the SIGINT reaches every process in it, as a terminal's Ctrl-C does. On a timeout the
	// whole group is killed: a survivor holding the pipe would keep the read below waiting.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	var lines []string
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if sc.Text() == "ready" {
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
				t.Errorf("send SIGINT to the process group: %v", err)
			}
		}
	}
	err = cmd.Wait()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.ExitCode() != 130 {
		t.Errorf("the wrapper exited with %v, want exit status 130", err)
	}
	if want := []string{"ready", "cleanup ran", "capture returned 130"}; !slices.Equal(lines, want) {
		t.Errorf("stdout lines = %q, want %q", lines, want)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read %s: %v", log, err)
	}
	if want := "ready\ncleanup ran\n"; string(got) != want {
		t.Errorf("run.log = %q, want %q: the cleanup output reaches the log", got, want)
	}
}

func TestEvents_AllPassedUntilACheckFails(t *testing.T) {
	script := `. "$0"; ev_init "$1" 20261120T000000Z ` + sampleCommit + ` file+proxy 0 >/dev/null; step only 'The only phase' >/dev/null
ev_all_passed && echo "before any check: yes"
pass 'a pass' >/dev/null; note 'a note' >/dev/null
ev_all_passed && echo "after a pass and a note: yes"
fail 'a failure' >/dev/null
ev_all_passed || echo "after a failure: no"`
	out, err := exec.CommandContext(t.Context(), "sh", "-c", script, eventsLib, t.TempDir()).CombinedOutput()
	if want := "before any check: yes\nafter a pass and a note: yes\nafter a failure: no\n"; err != nil || string(out) != want {
		t.Errorf("sh = %v, output %q; want %q", err, out, want)
	}
}
