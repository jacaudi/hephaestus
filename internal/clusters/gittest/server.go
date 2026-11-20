package gittest

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/cgi" //nolint:gosec // G504: httpoxy is fixed in Go 1.6.3, and this module needs far newer
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.jacaudi.dev/hephaestus/internal/tlscert/tlscerttest"
)

// DefaultRepo is the repository that Commit writes to.
const DefaultRepo = "repo"

// branch is the one branch every repository has.
const branch = "main"

// host is the name the server certificate is valid for, and so the host of every URL.
const host = "localhost"

// Options shapes a Server.
type Options struct {
	// BasicAuth, when set, is the user name and password every request must carry.
	BasicAuth *[2]string
	// DropAfter, when above zero, cuts every response body off after that many bytes by aborting the connection.
	DropAfter int
}

// Server is a TLS 1.3 git smart-HTTP server over repositories made with the host git. Its methods are for one test
// goroutine at a time.
type Server struct {
	srv     *httptest.Server
	git     string
	root    string // holds <name>.git, the repositories the server serves
	work    string // holds <name>, the work tree each repository is committed from
	caPEM   []byte
	commits int
	once    sync.Once
}

// NewServer starts a server with no repositories and registers its Close with tb. A missing git fails the test.
func NewServer(tb testing.TB, opts Options) *Server {
	tb.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		tb.Fatalf("gittest: git is required: %v", err)
	}

	certFile, keyFile := tlscerttest.WriteSelfSigned(tb, tb.TempDir(), host, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		tb.Fatalf("gittest: load certificate: %v", err)
	}
	caPEM, err := os.ReadFile(certFile) //nolint:gosec // G304: the file is the certificate this function just wrote
	if err != nil {
		tb.Fatalf("gittest: read certificate: %v", err)
	}

	s := &Server{git: gitPath, root: tb.TempDir(), work: tb.TempDir(), caPEM: caPEM}
	handler := Handler(s.root)
	if opts.BasicAuth != nil {
		handler = requireBasicAuth(handler, *opts.BasicAuth)
	}
	if opts.DropAfter > 0 {
		handler = dropAfter(handler, opts.DropAfter)
	}

	s.srv = httptest.NewUnstartedServer(handler)
	s.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	// go-git clones its transport per session when it is given a CA bundle and never closes that transport's idle
	// connections, so the server must not keep them alive.
	s.srv.Config.SetKeepAlivesEnabled(false)
	s.srv.StartTLS()
	tb.Cleanup(s.Close)
	return s
}

// Handler serves every bare repository <root>/<name>.git over git's smart HTTP protocol, with the host git's
// http-backend. When git cannot be found it answers every request with 500.
func Handler(root string) http.Handler {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "git is not installed", http.StatusInternalServerError)
		})
	}
	return &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}
}

// URL is the address of the repository name.
func (s *Server) URL(name string) string {
	_, port, _ := strings.Cut(s.srv.Listener.Addr().String(), ":")
	return fmt.Sprintf("https://%s:%s/%s.git", host, port, name)
}

// CAPEM is the certificate a client must trust to reach the server.
func (s *Server) CAPEM() []byte { return s.caPEM }

// Close stops the server and its connections. It is safe to call twice.
func (s *Server) Close() { s.once.Do(s.srv.Close) }

// Commit is CommitTo for DefaultRepo.
func (s *Server) Commit(tb testing.TB, files, links map[string]string) string {
	tb.Helper()
	return s.CommitTo(tb, DefaultRepo, files, links)
}

// CommitTo writes files (path to content) and links (path to target) over the work tree of the repository name,
// creating the repository on first use, and commits the result on top of the repository's last commit. A commit that
// changes nothing is still made. It returns the commit's hash.
func (s *Server) CommitTo(tb testing.TB, name string, files, links map[string]string) string {
	tb.Helper()
	bare, work := filepath.Join(s.root, name+".git"), filepath.Join(s.work, name)
	if _, err := os.Stat(bare); err != nil {
		s.run(tb, "init", "--quiet", "--bare", "--initial-branch="+branch, bare)
	}

	for path, content := range files {
		write(tb, filepath.Join(work, path), func(p string) error { return os.WriteFile(p, []byte(content), 0o600) })
	}
	for path, target := range links {
		write(tb, filepath.Join(work, path), func(p string) error {
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return os.Symlink(target, p)
		})
	}

	s.commits++
	repo := []string{"--git-dir=" + bare, "--work-tree=" + work}
	s.run(tb, append(repo, "add", "--all")...)
	s.run(tb, append(repo, "-c", "user.name=gittest", "-c", "user.email=gittest@example.test", "-c", "commit.gpgsign=false",
		"commit", "--quiet", "--allow-empty", "-m", fmt.Sprintf("commit %d", s.commits))...)
	return s.run(tb, append(repo, "rev-parse", "HEAD")...)
}

// Tree is the hash of the tree at path in the revision rev of DefaultRepo, as the host git reports it.
func (s *Server) Tree(tb testing.TB, rev, path string) string {
	tb.Helper()
	return s.run(tb, "--git-dir="+filepath.Join(s.root, DefaultRepo+".git"), "rev-parse", rev+":"+path)
}

// run runs the host git with args and returns its trimmed output; a failure fails the test.
func (s *Server) run(tb testing.TB, args ...string) string {
	tb.Helper()
	cmd := exec.CommandContext(tb.Context(), s.git, args...) //nolint:gosec // G204: the path is git's, and the arguments are the test's own
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		tb.Fatalf("gittest: git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// write makes path's directory and then calls create on path.
func write(tb testing.TB, path string, create func(string) error) {
	tb.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		tb.Fatalf("gittest: make directory for %s: %v", path, err)
	}
	if err := create(path); err != nil {
		tb.Fatalf("gittest: write %s: %v", path, err)
	}
}

// requireBasicAuth answers 401 to a request that does not carry the user name and password in want.
func requireBasicAuth(next http.Handler, want [2]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != want[0] || password != want[1] {
			w.Header().Set("WWW-Authenticate", `Basic realm="gittest"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// dropAfter aborts the connection of a response once it has written limit bytes of body.
func dropAfter(next http.Handler, limit int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&dropWriter{ResponseWriter: w, left: limit}, r)
	})
}

type dropWriter struct {
	http.ResponseWriter

	left int
}

// Write passes on up to the bytes left, then aborts the connection, which the server does without a log line.
func (d *dropWriter) Write(p []byte) (int, error) {
	if len(p) <= d.left {
		d.left -= len(p)
		return d.ResponseWriter.Write(p)
	}
	if _, err := d.ResponseWriter.Write(p[:d.left]); err == nil {
		d.Flush() // the client has the prefix before the connection dies
	}
	panic(http.ErrAbortHandler)
}

// Flush passes a flush on; a flush that fails aborts the connection, which is the point of the writer.
func (d *dropWriter) Flush() {
	if err := http.NewResponseController(d.ResponseWriter).Flush(); err != nil {
		panic(http.ErrAbortHandler)
	}
}
