// hephaestus: new

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// ErrEscapes reports a path, or a symlink target, that leaves the definition tree.
var ErrEscapes = errors.New("topf: path leaves the definition tree")

// ErrSymlinkLoop reports symlinks that form a loop.
var ErrSymlinkLoop = errors.New("topf: symlinks form a loop")

// errLinkTarget marks an EvalSymlinks failure whose missing component came from the target of a symlink, not from the
// name asked for: the link dangles. It always travels with an error matching fs.ErrNotExist.
var errLinkTarget = errors.New("topf: symlink target does not exist")

// maxSymlinks is how many links EvalSymlinks follows before it reports a loop (the limit of Linux's path walk).
const maxSymlinks = 40

// CleanRel cleans p and requires it to name a place inside the tree: an absolute path, or one that leaves the
// root after cleaning, is ErrEscapes. The result satisfies fs.ValidPath.
func CleanRel(p string) (string, error) {
	cleaned := path.Clean(p)
	if path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: %q", ErrEscapes, p)
	}

	return cleaned, nil
}

// EvalSymlinks resolves every symlink in name and returns the real path, relative to the root of fsys.
//
// A file system that does not implement fs.ReadLinkFS has no links, so the cleaned name is returned.
// A relative target resolves against the directory of its link; an absolute target, or one that leaves the root,
// is ErrEscapes. More than 40 links is ErrSymlinkLoop. A component that does not exist is an error matching fs.ErrNotExist.
func EvalSymlinks(fsys fs.FS, name string) (string, error) {
	cleaned, err := CleanRel(name)
	if err != nil {
		return "", err
	}

	if _, ok := fsys.(fs.ReadLinkFS); !ok {
		return cleaned, nil
	}

	var (
		resolved []string // the real path so far: no link, no "." and no ".."
		pending  = splitPath(cleaned)
		fromLink int // how many leading elements of pending came from a symlink target
		links    int
	)

	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		inTarget := fromLink > 0

		if inTarget {
			fromLink--
		}

		if part == ".." {
			if len(resolved) == 0 {
				return "", fmt.Errorf("%w: %q", ErrEscapes, name)
			}

			resolved = resolved[:len(resolved)-1]

			continue
		}

		next := path.Join(append(resolved[:len(resolved):len(resolved)], part)...)

		info, err := fs.Lstat(fsys, next)
		if err != nil {
			if inTarget && errors.Is(err, fs.ErrNotExist) {
				return "", fmt.Errorf("%w: %q: %w", errLinkTarget, name, err)
			}

			return "", err
		}

		if info.Mode()&fs.ModeSymlink == 0 {
			resolved = append(resolved, part)

			continue
		}

		links++
		if links > maxSymlinks {
			return "", fmt.Errorf("%w: %q", ErrSymlinkLoop, name)
		}

		target, err := fs.ReadLink(fsys, next)
		if err != nil {
			return "", err
		}

		if path.IsAbs(target) {
			return "", fmt.Errorf("%w: %q links to %q", ErrEscapes, next, target)
		}

		// A relative target resolves against the directory of the link, which is resolved already.
		targetParts := splitPath(path.Clean(target))
		pending = append(targetParts, pending...)
		fromLink += len(targetParts)
	}

	if len(resolved) == 0 {
		return ".", nil
	}

	return path.Join(resolved...), nil
}

// splitPath returns the elements of a cleaned relative path; "." has none.
func splitPath(cleaned string) []string {
	if cleaned == "." {
		return nil
	}

	return strings.Split(cleaned, "/")
}
