package clusters

import (
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"go.uber.org/goleak"

	"go.jacaudi.dev/hephaestus/internal/clusters/gittest"
	"go.jacaudi.dev/hephaestus/internal/secret"
)

const (
	gitCluster = "c1"
	gitBranch  = "main"
	gitLab     = "lab"
)

var gitT0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// noise returns n incompressible bytes, so a pack holding them is large and every byte of it matters.
func noise(n int) string {
	r := rand.New(rand.NewPCG(1, 2))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.UintN(256))
	}
	return string(b)
}

// labFiles is a small cluster definition under lab/ and an unrelated file under other/.
func labFiles(version string) map[string]string {
	return map[string]string{
		"lab/topf.yaml":  "clusterName: lab\nversion: " + version + "\n",
		"lab/sub/a.txt":  "a",
		"lab/big.bin":    noise(64 << 10),
		"other/note.txt": "n",
	}
}

func labSource(srv *gittest.Server, subpath string) gitSource {
	return gitSource{URL: srv.URL(gittest.DefaultRepo), Ref: gitBranch, Subpath: subpath}
}

// newTestStore is a store in a fresh directory that trusts srv's certificate.
func newTestStore(t *testing.T, srv *gittest.Server) *gitStore {
	t.Helper()
	g := newGitStore(filepath.Join(t.TempDir(), "git"), func() time.Time { return gitT0 })
	g.caBundle = srv.CAPEM()
	return g
}

// storeDir is the directory of the cluster's bare repository inside g.
func storeDir(g *gitStore) string { return filepath.Join(g.dir, gitCluster+".git") }

// chainOf names every error type in err's Unwrap chain, for a failure message.
func chainOf(err error) string {
	var parts []string
	for ; err != nil; err = errors.Unwrap(err) {
		parts = append(parts, fmt.Sprintf("%T", err))
	}
	return strings.Join(parts, " -> ")
}

func TestGit_FetchReturnsSubtreeHash(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer srv.Close()
	a := srv.Commit(t, labFiles("1"), nil)
	g := newTestStore(t, srv)

	got, err := g.fetch(t.Context(), gitCluster, labSource(srv, gitLab))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if want := srv.Tree(t, a, gitLab); got != want {
		t.Errorf("fetch tree = %s, want %s (git rev-parse %s:%s)", got, want, a, gitLab)
	}
	if _, ok := g.expected[got]; !ok {
		t.Errorf("the fetched tree %s is not expected", got)
	}
}

func TestGit_CommitOutsideSubpathSameTree(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	srv.Commit(t, labFiles("1"), nil)
	g := newTestStore(t, srv)
	first, err := g.fetch(t.Context(), gitCluster, labSource(srv, gitLab))
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	srv.Commit(t, map[string]string{"other/note.txt": "changed"}, nil)
	second, err := g.fetch(t.Context(), gitCluster, labSource(srv, gitLab))
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if second != first {
		t.Errorf("a commit touching other/ only changed the lab tree: %s, then %s", first, second)
	}
}

func TestGit_ReadTreeFiles(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	srv.Commit(t, labFiles("1"), map[string]string{"lab/link": "sub/a.txt"})
	g := newTestStore(t, srv)
	tree, err := g.fetch(t.Context(), gitCluster, labSource(srv, gitLab))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	fsys, err := g.readTree(t.Context(), gitCluster, tree)
	if err != nil {
		t.Fatalf("readTree: %v", err)
	}
	if got, err := fs.ReadFile(fsys, "topf.yaml"); err != nil || string(got) != labFiles("1")["lab/topf.yaml"] {
		t.Errorf("topf.yaml = %q, %v", got, err)
	}
	if got, err := fs.ReadFile(fsys, "big.bin"); err != nil || string(got) != noise(64<<10) {
		t.Errorf("big.bin differs (%d bytes), %v", len(got), err)
	}
	if got, err := fs.ReadLink(fsys, "link"); err != nil || got != "sub/a.txt" {
		t.Errorf("ReadLink(link) = %q, %v; want sub/a.txt", got, err)
	}
	if got, err := fs.ReadFile(fsys, "link"); err != nil || string(got) != "a" {
		t.Errorf("ReadFile(link) = %q, %v; want the target's bytes", got, err)
	}
	if _, err := fs.Stat(fsys, "note.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file outside the subpath is in the tree: %v", err)
	}
}

func TestGit_BasicAuth(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{BasicAuth: &[2]string{"deploy", "s3cret"}})
	srv.Commit(t, labFiles("1"), nil)

	for _, tc := range []struct {
		name     string
		user     string
		password secret.Secret
		wantErr  bool
	}{
		{name: "right credentials", user: "deploy", password: "s3cret"},
		{name: "no credentials", wantErr: true},
		{name: "wrong password", user: "deploy", password: "nope", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := labSource(srv, gitLab)
			src.Username, src.Password = tc.user, tc.password

			_, err := newTestStore(t, srv).fetch(t.Context(), gitCluster, src)
			if (err != nil) != tc.wantErr {
				t.Errorf("fetch error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), "s3cret") {
				t.Errorf("the fetch error quotes the password: %v", err)
			}
		})
	}
}

func TestGit_MissingSubpath(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	srv.Commit(t, map[string]string{"other/note.txt": "n"}, nil)

	_, err := newTestStore(t, srv).fetch(t.Context(), gitCluster, labSource(srv, gitLab))
	if err == nil || !strings.Contains(err.Error(), `"lab"`) {
		t.Errorf("fetch error = %v, want one naming the subpath \"lab\"", err)
	}
}

func TestGit_FullHistoryKept(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	srv.Commit(t, labFiles("1"), nil)
	g := newTestStore(t, srv)
	a, err := g.fetch(t.Context(), gitCluster, labSource(srv, gitLab))
	if err != nil {
		t.Fatalf("fetch A: %v", err)
	}

	srv.Commit(t, labFiles("2"), nil)
	srv.Commit(t, labFiles("3"), nil)
	c, err := g.fetch(t.Context(), gitCluster, labSource(srv, gitLab))
	if err != nil {
		t.Fatalf("fetch C: %v", err)
	}
	if c == a {
		t.Fatalf("the later commits did not change the lab tree: %s", a)
	}

	fsys, err := g.readTree(t.Context(), gitCluster, a)
	if err != nil {
		t.Fatalf("readTree(A's tree) after fetching C: %v", err)
	}
	if got, err := fs.ReadFile(fsys, "topf.yaml"); err != nil || !strings.Contains(string(got), "version: 1") {
		t.Errorf("A's topf.yaml = %q, %v", got, err)
	}
}

func TestGit_FetchDropIsNotCorruption(t *testing.T) {
	dropping := gittest.NewServer(t, gittest.Options{DropAfter: 2048})
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	defer dropping.Close()
	dropping.Commit(t, labFiles("1"), nil)
	healthy := gittest.NewServer(t, gittest.Options{})
	defer healthy.Close()
	healthy.Commit(t, labFiles("1"), nil)

	g := newTestStore(t, dropping)
	g.caBundle = slices.Concat(dropping.CAPEM(), healthy.CAPEM())
	if _, err := g.fetch(t.Context(), gitCluster, labSource(dropping, gitLab)); err == nil {
		t.Fatal("fetch over a connection cut after 2048 bytes succeeded")
	}
	if matches, err := filepath.Glob(g.dir + ".corrupt-*"); err != nil || len(matches) != 0 {
		t.Errorf("the store was moved aside after a dropped fetch: %v, %v", matches, err)
	}

	tree, err := g.fetch(t.Context(), gitCluster, labSource(healthy, gitLab))
	if err != nil {
		t.Fatalf("fetch after the drop: %v", err)
	}
	if _, err := g.readTree(t.Context(), gitCluster, tree); err != nil {
		t.Errorf("readTree after the drop: %v", err)
	}
}

func TestGit_CorruptPackMovesAside(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	srv.Commit(t, labFiles("1"), nil)
	g := newTestStore(t, srv)
	tree, err := g.fetch(t.Context(), gitCluster, labSource(srv, gitLab))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	packs, err := filepath.Glob(filepath.Join(storeDir(g), "objects", "pack", "*.pack"))
	if err != nil || len(packs) != 1 {
		t.Fatalf("pack files = %v, %v; want exactly one", packs, err)
	}
	info, err := os.Stat(packs[0])
	if err != nil {
		t.Fatalf("stat pack: %v", err)
	}
	if err := os.Chmod(packs[0], 0o600); err != nil { // go-git writes packs read-only
		t.Fatalf("make the pack writable: %v", err)
	}
	if err := os.Truncate(packs[0], info.Size()/2); err != nil {
		t.Fatalf("truncate pack: %v", err)
	}

	fresh := newGitStore(g.dir, func() time.Time { return gitT0 })
	_, err = fresh.readTree(t.Context(), gitCluster, tree)
	if err == nil {
		t.Fatal("readTree over a half pack succeeded")
	}
	if !fresh.corrupt(tree, err) {
		t.Fatalf("corrupt(tree, err) = false for %v; chain: %s", err, chainOf(err))
	}

	moved, ok, err := fresh.moveAside(gitT0)
	if err != nil || !ok {
		t.Fatalf("moveAside = %q, %v, %v; want it to move", moved, ok, err)
	}
	if want := g.dir + ".corrupt-20261119T120000Z"; moved != want {
		t.Errorf("moved to %q, want %q", moved, want)
	}
	if _, err := os.Stat(moved); err != nil {
		t.Errorf("the moved store: %v", err)
	}
	if _, err := os.Stat(g.dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the store directory is still there: %v", err)
	}
	if len(fresh.expected) != 0 {
		t.Errorf("expected = %v after the move, want none", fresh.expected)
	}

	if moved, ok, err := fresh.moveAside(gitT0.Add(30 * time.Minute)); ok || err != nil {
		t.Errorf("a second moveAside within the hour = %q, %v, %v; want ok false", moved, ok, err)
	}
}

func TestGit_MoveAsideKeepsNewest(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	srv.Commit(t, labFiles("1"), nil)
	g := newTestStore(t, srv)

	var movedTo []string
	for _, at := range []time.Time{gitT0, gitT0.Add(2 * time.Hour), {}} {
		if _, err := g.fetch(t.Context(), gitCluster, labSource(srv, gitLab)); err != nil {
			t.Fatalf("fetch before the move at %v: %v", at, err)
		}
		moved, ok, err := g.moveAside(at)
		if err != nil || !ok {
			t.Fatalf("moveAside(%v) = %q, %v, %v; want it to move", at, moved, ok, err)
		}
		movedTo = append(movedTo, moved)
		g.now = func() time.Time { return gitT0.Add(4 * time.Hour) }
	}

	// The last call passed a zero time and so used the store's clock, which stood at T0+4h then.
	if want := g.dir + ".corrupt-20261119T160000Z"; movedTo[2] != want {
		t.Errorf("a zero time moved to %q, want %q", movedTo[2], want)
	}
	siblings, err := filepath.Glob(g.dir + ".corrupt-*")
	if err != nil || len(siblings) != 1 {
		t.Fatalf("siblings after three moves = %v, %v; want only the newest", siblings, err)
	}
	if siblings[0] != movedTo[2] {
		t.Errorf("kept %q, want the newest %q", siblings[0], movedTo[2])
	}
}

func TestGit_MissingTreeAfterMoveAsideNotCorrupt(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	srv.Commit(t, labFiles("1"), nil)
	srv.CommitTo(t, "other", labFiles("2"), nil)
	g := newTestStore(t, srv)
	tree, err := g.fetch(t.Context(), gitCluster, labSource(srv, gitLab))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, ok, err := g.moveAside(gitT0); err != nil || !ok {
		t.Fatalf("moveAside = %v, %v", ok, err)
	}

	src := gitSource{URL: srv.URL("other"), Ref: gitBranch, Subpath: gitLab}
	if _, err := g.fetch(t.Context(), gitCluster, src); err != nil {
		t.Fatalf("re-fetch of a repository without the tree: %v", err)
	}

	_, err = g.readTree(t.Context(), gitCluster, tree)
	if err == nil {
		t.Fatal("readTree of a tree the repository no longer has succeeded")
	}
	if got := classifyRead(err); got != readMissing {
		t.Errorf("classifyRead = %v, want readMissing; chain: %s", got, chainOf(err))
	}
	if g.corrupt(tree, err) {
		t.Error("a tree missing after a move-aside is called corrupt")
	}
}

func TestGit_ExpectedTreeMissingIsCorrupt(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	srv.Commit(t, labFiles("1"), nil)
	g := newTestStore(t, srv)
	tree, err := g.fetch(t.Context(), gitCluster, labSource(srv, gitLab))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	objects, err := filepath.Glob(filepath.Join(storeDir(g), "objects", "pack", "pack-*"))
	if err != nil || len(objects) == 0 {
		t.Fatalf("pack files = %v, %v", objects, err)
	}
	for _, p := range objects {
		if err := os.Remove(p); err != nil {
			t.Fatalf("remove %s: %v", p, err)
		}
	}

	fresh := newGitStore(g.dir, func() time.Time { return gitT0 })
	_, err = fresh.readTree(t.Context(), gitCluster, tree)
	if err == nil {
		t.Fatal("readTree after the objects were deleted succeeded")
	}
	if fresh.corrupt(tree, err) {
		t.Errorf("a missing tree nobody expected is called corrupt; chain: %s", chainOf(err))
	}
	fresh.expect([]string{tree})
	if !fresh.corrupt(tree, err) {
		t.Errorf("an expected tree that is missing is not called corrupt; chain: %s", chainOf(err))
	}
}

func TestClassifyRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want readClass
	}{
		{"invalid object", packfile.ErrInvalidObject.AddDetails("x"), readCorrupt},
		{"wrapped malformed pack", fmt.Errorf("w: %w", packfile.ErrMalformedPackFile), readCorrupt},
		{"zlib checksum", zlib.ErrChecksum, readCorrupt},
		{"truncated stream", fmt.Errorf("w: %w", io.ErrUnexpectedEOF), readCorrupt},
		{"object not found", plumbing.ErrObjectNotFound, readMissing},
		{"wrapped object not found", fmt.Errorf("w: %w", plumbing.ErrObjectNotFound), readMissing},
		{"canceled", context.Canceled, readOther},
		{"url error", &url.Error{Op: "Get", URL: "https://x.test", Err: errors.New("refused")}, readOther},
		{"nil", nil, readOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRead(tc.err); got != tc.want {
				t.Errorf("classifyRead(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestGit_ClustersKeepSeparateRepositories(t *testing.T) {
	srv := gittest.NewServer(t, gittest.Options{})
	srv.Commit(t, labFiles("1"), nil)
	srv.CommitTo(t, "other", labFiles("2"), nil)
	g := newTestStore(t, srv)

	first, err := g.fetch(t.Context(), "c1", labSource(srv, gitLab))
	if err != nil {
		t.Fatalf("fetch c1: %v", err)
	}
	second, err := g.fetch(t.Context(), "c2", gitSource{URL: srv.URL("other"), Ref: gitBranch, Subpath: gitLab})
	if err != nil {
		t.Fatalf("fetch c2: %v", err)
	}

	for id, tree := range map[string]string{"c1": first, "c2": second} {
		if _, err := os.Stat(filepath.Join(g.dir, id+".git")); err != nil {
			t.Errorf("repository of %s: %v", id, err)
		}
		if _, err := g.readTree(t.Context(), id, tree); err != nil {
			t.Errorf("readTree(%s): %v", id, err)
		}
	}
	if _, err := g.readTree(t.Context(), "c1", second); !errors.Is(err, plumbing.ErrObjectNotFound) {
		t.Errorf("readTree(c1, c2's tree) error = %v, want plumbing.ErrObjectNotFound", err)
	}
}
