package clusters

import (
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/idxfile"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"go.jacaudi.dev/hephaestus/internal/secret"
)

// moveAsideInterval is how long after one move-aside the next one is refused, so a store that is corrupt again at
// once is not re-cloned on every poll.
const moveAsideInterval = time.Hour

// moveAsideStamp is the time layout of the suffix of a moved-aside store; it sorts as the time does.
const moveAsideStamp = "20060102T150405Z"

// movedAsideInfix joins the store's directory name and the time in the name of a moved-aside store.
const movedAsideInfix = ".corrupt-"

// The bounds on a tree readTree puts in memory before any load rule has judged it: a definition is a few text files, so
// these leave room for a large one and stop a tree that expands far beyond its objects. Paths are counted once each, so
// a subtree that many paths reach counts for all of them.
const (
	maxTreeFiles = 20000
	maxTreeBytes = 128 << 20
)

// TreeTooLargeError reports a tree with more paths or more bytes than the store reads. Its text holds no content of the
// tree.
type TreeTooLargeError struct {
	// Files is the path bound that was exceeded, or 0 when the byte bound was.
	Files int
	// Bytes is the byte bound that was exceeded, or 0 when the path bound was.
	Bytes int64
}

// Error states the bound that was exceeded.
func (e *TreeTooLargeError) Error() string {
	if e.Files > 0 {
		return fmt.Sprintf("the tree has more than %d files", e.Files)
	}
	return fmt.Sprintf("the tree holds more than %d bytes", e.Bytes)
}

// gitSource is where a cluster definition comes from.
type gitSource struct {
	// URL is the repository's HTTPS address.
	URL string
	// Ref is the branch to follow.
	Ref string
	// Subpath is the directory of the definition in the repository; empty is the root.
	Subpath string
	// Username and Password are the HTTP basic credentials, when the repository needs them.
	Username string
	Password secret.Secret
}

// gitStore keeps one bare repository per cluster under dir, with the full history of the branch the cluster follows
// (an approved tree must stay readable after the head moves on), and reads trees out of them.
type gitStore struct {
	dir string
	mu  sync.Mutex
	// lastMovedAside is when the store was last moved aside; zero if it never was.
	lastMovedAside time.Time
	// expected holds the trees that must be readable: the ones fetched, read or named by expect since the store was
	// last moved aside. A tree that is missing from the store but not in expected is only a tree the repository no
	// longer has.
	expected map[string]struct{}
	now      func() time.Time
	// maxFiles and maxBytes bound what readTree reads.
	maxFiles int
	maxBytes int64
	// caBundle is an extra CA certificate for the fetches; tests use it to trust their server.
	caBundle []byte
}

// newGitStore returns a store that keeps its repositories under dir. now is the clock moveAside falls back to.
func newGitStore(dir string, now func() time.Time) *gitStore {
	return &gitStore{dir: dir, expected: map[string]struct{}{}, now: now, maxFiles: maxTreeFiles, maxBytes: maxTreeBytes}
}

// fetch brings refs/heads/<src.Ref> into the cluster's repository, creating it on first use, with its whole history,
// and returns the hash of the tree at src.Subpath of the branch's head. The tree is then expected.
func (g *gitStore) fetch(ctx context.Context, clusterID string, src gitSource) (tree string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	repo, err := g.openOrInit(clusterID)
	if err != nil {
		return "", fmt.Errorf("clusters: open the repository of cluster %s: %w", clusterID, err)
	}

	ref := plumbing.NewBranchReferenceName(src.Ref)
	opts := &git.FetchOptions{
		RefSpecs: []config.RefSpec{config.RefSpec("+" + ref.String() + ":" + ref.String())},
		Tags:     git.NoTags,
		Force:    true,
		CABundle: g.caBundle,
	}
	if src.Username != "" || src.Password != "" {
		opts.Auth = &githttp.BasicAuth{Username: src.Username, Password: src.Password.Expose()}
	}

	remote := git.NewRemote(repo.Storer, &config.RemoteConfig{Name: git.DefaultRemoteName, URLs: []string{src.URL}})
	if err := remote.FetchContext(ctx, opts); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return "", fmt.Errorf("clusters: fetch %s of cluster %s: %w", ref.Short(), clusterID, err)
	}

	hash, err := subtreeHash(repo, ref, src.Subpath)
	if err != nil {
		return "", err
	}
	g.expected[hash.String()] = struct{}{}
	return hash.String(), nil
}

// subtreeHash is the hash of the tree at subpath of the head of ref.
func subtreeHash(repo *git.Repository, ref plumbing.ReferenceName, subpath string) (plumbing.Hash, error) {
	head, err := repo.Reference(ref, true)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("clusters: branch %s: %w", ref.Short(), err)
	}
	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("clusters: head of %s: %w", ref.Short(), err)
	}
	root, err := commit.Tree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("clusters: tree of the head of %s: %w", ref.Short(), err)
	}

	subpath = strings.Trim(subpath, "/")
	if subpath == "" {
		return root.Hash, nil
	}
	entry, err := root.FindEntry(subpath)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("clusters: subpath %q: %w", subpath, err)
	}
	if entry.Mode != filemode.Dir {
		return plumbing.ZeroHash, fmt.Errorf("clusters: subpath %q is not a directory", subpath)
	}
	return entry.Hash, nil
}

// readTree returns every file of the tree in memory; the tree is then expected. A tree with more paths or bytes than
// the store's bounds is a *TreeTooLargeError, found before the contents are read.
func (g *gitStore) readTree(ctx context.Context, clusterID, tree string) (fs.FS, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	repo, err := git.PlainOpen(g.repoPath(clusterID))
	if err != nil {
		return nil, fmt.Errorf("clusters: open the repository of cluster %s: %w", clusterID, err)
	}
	root, err := object.GetTree(repo.Storer, plumbing.NewHash(tree))
	if err != nil {
		return nil, fmt.Errorf("clusters: read tree %s: %w", tree, err)
	}

	files := map[string]memFile{}
	var total int64
	err = root.Files().ForEach(func(f *object.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(files) >= g.maxFiles {
			return &TreeTooLargeError{Files: g.maxFiles}
		}
		if total += f.Size; total > g.maxBytes {
			return &TreeTooLargeError{Bytes: g.maxBytes}
		}
		content, err := f.Contents()
		if err != nil {
			return err
		}
		files[f.Name] = memFile{data: []byte(content), mode: fileMode(f.Mode)}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("clusters: read tree %s: %w", tree, err)
	}

	g.expected[tree] = struct{}{}
	return newMemFS(files), nil
}

// fileMode is the fs.FileMode of a file of a git tree.
func fileMode(m filemode.FileMode) fs.FileMode {
	switch m { //nolint:exhaustive // every other mode is a regular file; a tree has no directory or submodule here
	case filemode.Symlink:
		return fs.ModeSymlink | 0o777
	case filemode.Executable:
		return 0o555
	default:
		return 0o444
	}
}

// expect adds the trees to those that must be readable, for the cache fill at startup.
func (g *gitStore) expect(trees []string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for _, tree := range trees {
		g.expected[tree] = struct{}{}
	}
}

// corrupt reports whether err, from reading tree, means the store is damaged: a corrupt object always, a missing one
// only for a tree that was expected, since a tree is also missing when the repository no longer has it.
func (g *gitStore) corrupt(tree string, err error) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	switch classifyRead(err) {
	case readCorrupt:
		return true
	case readMissing:
		_, ok := g.expected[tree]
		return ok
	case readOther:
		return false
	default:
		return false
	}
}

// moveAside renames the store's directory to <dir>.corrupt-<UTC time>, keeps only the newest directory of that name
// and forgets the expected trees. ok is false, and nothing is done, when the store was moved aside less than an hour
// before at. When this process has not moved the store, the last move-aside is the time in the name of the newest
// moved-aside directory, so the hour holds across a restart. A zero at is the store's clock now.
func (g *gitStore) moveAside(at time.Time) (moved string, ok bool, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if at.IsZero() {
		at = g.now()
	}
	if g.lastMovedAside.IsZero() {
		if g.lastMovedAside, err = g.movedAsideAt(); err != nil {
			return "", false, err
		}
	}
	if !g.lastMovedAside.IsZero() && at.Sub(g.lastMovedAside) < moveAsideInterval {
		return "", false, nil
	}

	moved = g.dir + movedAsideInfix + at.UTC().Format(moveAsideStamp)
	if err := os.Rename(g.dir, moved); err != nil {
		return "", false, fmt.Errorf("clusters: move the git store aside: %w", err)
	}
	g.lastMovedAside = at
	clear(g.expected)

	if err := g.pruneMovedAside(); err != nil {
		return moved, true, err
	}
	return moved, true, nil
}

// movedAsideAt is the time in the name of the newest moved-aside directory; zero when there is none.
func (g *gitStore) movedAsideAt() (time.Time, error) {
	dirs, err := g.movedAside()
	if err != nil || len(dirs) == 0 {
		return time.Time{}, err
	}
	at, _ := g.movedAsideTime(filepath.Base(dirs[len(dirs)-1])) // movedAside lists only names that carry a time
	return at, nil
}

// movedAsideTime is the time in name, which is a moved-aside store's when ok: a name that only starts like one is
// somebody else's, and is neither listed nor pruned.
func (g *gitStore) movedAsideTime(name string) (at time.Time, ok bool) {
	suffix, found := strings.CutPrefix(name, filepath.Base(g.dir)+movedAsideInfix)
	if !found {
		return time.Time{}, false
	}
	at, err := time.Parse(moveAsideStamp, suffix)
	return at, err == nil
}

// pruneMovedAside removes every moved-aside directory but the newest.
func (g *gitStore) pruneMovedAside() error {
	stale, err := g.movedAside()
	if err != nil {
		return err
	}
	if len(stale) > 0 {
		stale = stale[:len(stale)-1]
	}
	for _, dir := range stale {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("clusters: remove a moved-aside git store: %w", err)
		}
	}
	return nil
}

// movedAside returns the store's moved-aside directories, oldest first: os.ReadDir sorts by name, and the suffix sorts
// as time does. A parent directory that does not exist holds none.
func (g *gitStore) movedAside() ([]string, error) {
	parent := filepath.Dir(g.dir)
	entries, err := os.ReadDir(parent)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("clusters: list the moved-aside git stores: %w", err)
	}
	var dirs []string
	for _, e := range entries {
		if _, ok := g.movedAsideTime(e.Name()); ok {
			dirs = append(dirs, filepath.Join(parent, e.Name()))
		}
	}
	return dirs, nil
}

func (g *gitStore) repoPath(clusterID string) string { return filepath.Join(g.dir, clusterID+".git") }

// openOrInit opens the cluster's bare repository, creating it when there is none.
func (g *gitStore) openOrInit(clusterID string) (*git.Repository, error) {
	repo, err := git.PlainOpen(g.repoPath(clusterID))
	if errors.Is(err, git.ErrRepositoryNotExists) {
		return git.PlainInit(g.repoPath(clusterID), true)
	}
	return repo, err
}

// readClass is what a failed tree read says about the store.
type readClass int

const (
	// readOther is any error that says nothing about the store's health, such as a canceled context.
	readOther readClass = iota
	// readMissing is an object the store does not have.
	readMissing
	// readCorrupt is an object the store has but cannot decode.
	readCorrupt
)

// classifyRead sorts an error from reading a tree by type: a missing object, an object that does not decode, or
// anything else.
func classifyRead(err error) readClass {
	if pe, ok := errors.AsType[*packfile.Error](err); ok && pe != nil {
		return readCorrupt
	}
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return readMissing
	}

	corrupt := []error{
		packfile.ErrMalformedPackFile, packfile.ErrReferenceDeltaNotFound, packfile.ErrInvalidDelta, packfile.ErrDeltaCmd,
		packfile.ErrLengthOverflow, packfile.ErrInflatedSizeMismatch,
		idxfile.ErrMalformedIdxFile, idxfile.ErrUnsupportedVersion,
		object.ErrMalformedTree, object.ErrEntriesNotSorted, plumbing.ErrInvalidType,
		zlib.ErrChecksum, zlib.ErrHeader,
		// A truncated object; safe to call corrupt because readTree reads only the local store, never the network.
		io.ErrUnexpectedEOF,
	}
	if slices.ContainsFunc(corrupt, func(target error) bool { return errors.Is(err, target) }) {
		return readCorrupt
	}
	return readOther
}
