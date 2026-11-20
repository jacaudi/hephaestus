package clusters

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"

	"go.jacaudi.dev/hephaestus/internal/render"
)

// link is a symbolic link to target.
func link(target string) memFile {
	return memFile{data: []byte(target), mode: fs.ModeSymlink | 0o777}
}

// regular is a regular file with the given content.
func regular(content string) memFile {
	return memFile{data: []byte(content), mode: 0o444}
}

// linkChain is a memFS where l0 → l1 → … → l<n-1> → f, so resolving l0 follows n links.
func linkChain(n int) memFS {
	files := map[string]memFile{"f": regular("hello")}
	for i := range n {
		target := "f"
		if i < n-1 {
			target = fmt.Sprintf("l%d", i+1)
		}
		files[fmt.Sprintf("l%d", i)] = link(target)
	}
	return memFS{files: files}
}

func TestMemFS_FSTest(t *testing.T) {
	m := memFS{files: map[string]memFile{
		"a.txt":      regular("a"),
		"dir/b.txt":  regular("b"),
		"dir/c/d.go": regular("d"),
		"link.txt":   link("a.txt"),
		"dirlink":    link("dir"),
	}}

	if err := fstest.TestFS(m, "a.txt", "dir/b.txt", "dir/c/d.go", "link.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestMemFS_Links(t *testing.T) {
	m := memFS{files: map[string]memFile{
		"dir/b.txt": regular("b"),
		"link.txt":  link("dir/b.txt"),
		"dirlink":   link("dir"),
		"up/in":     link("../dir/b.txt"),
		"out":       link("../outside"),
		"abs":       link("/etc/hostname"),
	}}

	if got, err := fs.ReadFile(m, "link.txt"); err != nil || string(got) != "b" {
		t.Errorf("ReadFile(link.txt) = %q, %v; want b", got, err)
	}
	if got, err := fs.ReadFile(m, "dirlink/b.txt"); err != nil || string(got) != "b" {
		t.Errorf("ReadFile(dirlink/b.txt) = %q, %v; want b", got, err)
	}
	if got, err := fs.ReadFile(m, "up/in"); err != nil || string(got) != "b" {
		t.Errorf("ReadFile(up/in) = %q, %v; want b", got, err)
	}
	if got, err := fs.ReadLink(m, "link.txt"); err != nil || got != "dir/b.txt" {
		t.Errorf("ReadLink(link.txt) = %q, %v; want dir/b.txt", got, err)
	}
	if info, err := fs.Lstat(m, "link.txt"); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("Lstat(link.txt) = %v, %v; want a symlink", info, err)
	}
	if info, err := fs.Stat(m, "link.txt"); err != nil || info.Mode()&fs.ModeSymlink != 0 {
		t.Errorf("Stat(link.txt) = %v, %v; want the target, not the link", info, err)
	}

	entries, err := fs.ReadDir(m, ".")
	if err != nil {
		t.Fatalf("ReadDir(.): %v", err)
	}
	var symlinks []string
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink != 0 {
			symlinks = append(symlinks, e.Name())
		}
	}
	if want := []string{"abs", "dirlink", "link.txt", "out"}; !slices.Equal(symlinks, want) {
		t.Errorf("symlink entries of . = %v, want %v", symlinks, want)
	}

	for _, name := range []string{"out", "abs", "missing", "/etc/hostname", "../outside"} {
		if _, err := m.Open(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Open(%q) error = %v, want fs.ErrNotExist", name, err)
		}
	}
}

func TestMemFS_SymlinkCycleBounded(t *testing.T) {
	cycle := memFS{files: map[string]memFile{"p": link("q"), "q": link("p"), "d": link("d")}}

	for _, name := range []string{"p", "q", "d", "d/x"} {
		checks := map[string]func() error{
			"Open":     func() error { _, err := cycle.Open(name); return err },
			"Stat":     func() error { _, err := cycle.Stat(name); return err },
			"ReadFile": func() error { _, err := cycle.ReadFile(name); return err },
			"ReadDir":  func() error { _, err := cycle.ReadDir(name); return err },
		}
		for op, run := range checks {
			var pe *fs.PathError
			err := run()
			if !errors.Is(err, fs.ErrNotExist) || !errors.As(err, &pe) {
				t.Errorf("%s(%q) error = %v, want a *fs.PathError matching fs.ErrNotExist", op, name, err)
			}
		}
	}

	if got, err := linkChain(40).ReadFile("l0"); err != nil || string(got) != "hello" {
		t.Errorf("ReadFile through a chain of 40 links = %q, %v; want hello", got, err)
	}
	if _, err := linkChain(41).ReadFile("l0"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadFile through a chain of 41 links error = %v, want fs.ErrNotExist", err)
	}
}

func TestMemFS_LoadTopfInCycle(t *testing.T) {
	m := memFS{files: map[string]memFile{"topf.yaml": link("a"), "a": link("topf.yaml")}}

	_, err := render.Load(m)
	refusal, ok := errors.AsType[*render.RefusalError](err)
	if !ok {
		t.Fatalf("Load error = %v, want a *render.RefusalError", err)
	}
	// Load checks the links of topf.yaml before it reads the file, so the cycle is refused as a path that does not resolve.
	if refusal.File != "topf.yaml" || refusal.Rule != "path escapes tree" {
		t.Errorf("refusal = {File: %q, Rule: %q}, want topf.yaml / path escapes tree", refusal.File, refusal.Rule)
	}
}

func FuzzMemFSPaths(f *testing.F) {
	m := memFS{files: map[string]memFile{
		"a.txt":     regular("a"),
		"dir/b.txt": regular("b"),
		"link.txt":  link("dir/b.txt"),
		"dirlink":   link("dir"),
		"out":       link("../outside"),
		"abs":       link("/etc/hostname"),
		"p":         link("q"),
		"q":         link("p"),
	}}
	known := map[string]bool{"a": true, "b": true}

	for _, seed := range []string{"", ".", "..", "/", "/etc/hostname", "a.txt", "dir", "dir/b.txt", "dir/../a.txt", "dirlink/b.txt",
		"dirlink/../a.txt", "link.txt", "out", "out/x", "abs", "p", "p/x", "a//b", "a.txt/", "dir/./b.txt"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		ops := map[string]func() error{
			"Open": func() error {
				file, err := m.Open(name)
				if err != nil {
					return err
				}
				return file.Close()
			},
			"Stat":     func() error { _, err := m.Stat(name); return err },
			"Lstat":    func() error { _, err := m.Lstat(name); return err },
			"ReadDir":  func() error { _, err := m.ReadDir(name); return err },
			"ReadLink": func() error { _, err := m.ReadLink(name); return err },
		}
		for op, run := range ops {
			if err := run(); err == nil && !fs.ValidPath(name) {
				t.Errorf("%s(%q) succeeded for an invalid path", op, name)
			}
		}

		if data, err := m.ReadFile(name); err == nil && (!fs.ValidPath(name) || !known[string(data)]) {
			t.Errorf("ReadFile(%q) = %q, which is not tree content", name, data)
		}
	})
}
