package clusters

import (
	"bytes"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
)

// maxLinks is how many symbolic links one path resolution follows, the bound config.EvalSymlinks uses.
const maxLinks = 40

// memFile is a regular file, or a symbolic link whose data is its target.
type memFile struct {
	data []byte
	mode fs.FileMode
}

// memFS is a file system held in memory: the files of one git tree by their slash-separated path, with the
// directories between them implied. It has the semantics testing/fstest.MapFS has, which the vendored loader relies
// on: Open follows links that stay inside the tree, ReadDir reports a link with fs.ModeSymlink, and ReadLink and Lstat
// do not follow the last link. Unlike MapFS, a path that needs more than maxLinks links (a cycle, for example) is
// fs.ErrNotExist instead of a stack overflow. The zero value is an empty tree.
type memFS struct {
	files map[string]memFile
}

var (
	_ fs.ReadDirFS  = memFS{}
	_ fs.ReadFileFS = memFS{}
	_ fs.StatFS     = memFS{}
	_ fs.ReadLinkFS = memFS{}
)

// Open opens the named file or directory after following any links in it.
func (m memFS) Open(name string) (fs.File, error) {
	resolved, ok := m.resolve(name)
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	if file, found := m.files[resolved]; found {
		return &memOpenFile{Reader: bytes.NewReader(file.data), info: memInfo{name: path.Base(name), file: file}}, nil
	}

	entries, found := m.list(resolved)
	if !found {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &memOpenDir{info: memInfo{name: path.Base(name), file: memFile{mode: fs.ModeDir | 0o555}}, entries: entries}, nil
}

// Stat describes the named file after following any links.
func (m memFS) Stat(name string) (fs.FileInfo, error) { return fs.Stat(openOnly{m}, name) }

// ReadFile returns the content of the named file after following any links.
func (m memFS) ReadFile(name string) ([]byte, error) { return fs.ReadFile(openOnly{m}, name) }

// ReadDir lists the named directory, sorted by name, after following any links.
func (m memFS) ReadDir(name string) ([]fs.DirEntry, error) { return fs.ReadDir(openOnly{m}, name) }

// ReadLink returns the target of the named link.
func (m memFS) ReadLink(name string) (string, error) {
	info, err := m.lstat(name)
	if err != nil {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: err}
	}
	if info.file.mode.Type() != fs.ModeSymlink {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrInvalid}
	}
	return string(info.file.data), nil
}

// Lstat describes the named file; for a link it describes the link.
func (m memFS) Lstat(name string) (fs.FileInfo, error) {
	info, err := m.lstat(name)
	if err != nil {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: err}
	}
	return info, nil
}

func (m memFS) lstat(name string) (memInfo, error) {
	parent, ok := m.resolve(path.Dir(name))
	if !ok || !fs.ValidPath(name) {
		return memInfo{}, fs.ErrNotExist
	}

	elem := path.Base(name)
	resolved := path.Join(parent, elem)
	if file, found := m.files[resolved]; found {
		return memInfo{name: elem, file: file}, nil
	}
	if _, found := m.list(resolved); found {
		return memInfo{name: elem, file: memFile{mode: fs.ModeDir | 0o555}}, nil
	}
	return memInfo{}, fs.ErrNotExist
}

// list returns the entries of the directory dir, sorted by name, and whether the directory exists: the root always
// does, any other directory only when a file lies below it.
func (m memFS) list(dir string) ([]fs.DirEntry, bool) {
	prefix := ""
	if dir != "." {
		prefix = dir + "/"
	}

	children := map[string]memFile{}
	subdirs := map[string]struct{}{}
	for name, file := range m.files {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		if elem, _, nested := strings.Cut(rest, "/"); nested {
			subdirs[elem] = struct{}{}
		} else {
			children[elem] = file
		}
	}
	if dir != "." && len(children) == 0 && len(subdirs) == 0 {
		return nil, false
	}

	entries := make([]fs.DirEntry, 0, len(children)+len(subdirs))
	for elem, file := range children {
		entries = append(entries, memInfo{name: elem, file: file})
	}
	for elem := range subdirs {
		if _, isFile := children[elem]; !isFile {
			entries = append(entries, memInfo{name: elem, file: memFile{mode: fs.ModeDir | 0o555}})
		}
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, true
}

// resolve follows the links of name, as MapFS does: the whole name first, then each parent directory from the root,
// until none is left. It reports false for an invalid name, an absolute link target, a target that leaves the tree and
// more than maxLinks links.
func (m memFS) resolve(name string) (string, bool) {
	for range maxLinks + 1 {
		next, followed, ok := m.followOne(name)
		if !ok {
			return "", false
		}
		if !followed {
			return name, fs.ValidPath(name)
		}
		name = next
	}
	return "", false
}

// followOne replaces the first link of name by its target. followed is false when name has no link.
func (m memFS) followOne(name string) (next string, followed, ok bool) {
	if file, isLink := m.link(name); isLink {
		target := string(file.data)
		if path.IsAbs(target) {
			return "", false, false
		}
		return path.Join(path.Dir(name), target), true, true
	}

	for i := range len(name) {
		if name[i] != '/' {
			continue
		}
		if file, isLink := m.link(name[:i]); isLink {
			target := string(file.data)
			if path.IsAbs(target) {
				return "", false, false
			}
			return path.Join(path.Dir(name[:i]), target) + name[i:], true, true
		}
	}
	return name, false, true
}

// link returns the file at name when it is a symbolic link.
func (m memFS) link(name string) (memFile, bool) {
	file, found := m.files[name]
	return file, found && file.mode.Type() == fs.ModeSymlink
}

// openOnly hides every method of an fs.FS but Open, so the fs helpers that implement Stat, ReadFile and ReadDir go
// through Open and cannot call back into the method they implement.
type openOnly struct{ fs.FS }

// memInfo is the fs.FileInfo and fs.DirEntry of one memFile.
type memInfo struct {
	name string
	file memFile
}

func (i memInfo) Name() string               { return i.name }
func (i memInfo) Size() int64                { return int64(len(i.file.data)) }
func (i memInfo) Mode() fs.FileMode          { return i.file.mode }
func (i memInfo) Type() fs.FileMode          { return i.file.mode.Type() }
func (i memInfo) ModTime() time.Time         { return time.Time{} }
func (i memInfo) IsDir() bool                { return i.file.mode.IsDir() }
func (i memInfo) Sys() any                   { return nil }
func (i memInfo) Info() (fs.FileInfo, error) { return i, nil }

// memOpenFile is an open regular file.
type memOpenFile struct {
	*bytes.Reader

	info memInfo
}

func (f *memOpenFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *memOpenFile) Close() error               { return nil }

// memOpenDir is an open directory.
type memOpenDir struct {
	info    memInfo
	entries []fs.DirEntry
	offset  int
}

func (d *memOpenDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *memOpenDir) Close() error               { return nil }

func (d *memOpenDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: fs.ErrInvalid}
}

// ReadDir returns up to count of the entries not yet returned, or all of them when count is not above zero.
func (d *memOpenDir) ReadDir(count int) ([]fs.DirEntry, error) {
	left := d.entries[d.offset:]
	if count > 0 && len(left) == 0 {
		return nil, io.EOF
	}
	if count > 0 {
		left = left[:min(count, len(left))]
	}
	d.offset += len(left)
	return slices.Clone(left), nil
}
