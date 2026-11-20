// hephaestus: new

package config

import (
	"errors"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// link returns a symlink entry for a MapFS.
func link(target string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(target), Mode: fs.ModeSymlink}
}

// file returns a regular file entry for a MapFS.
func file(content string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(content)}
}

// plainFS hides every optional interface of the wrapped file system.
type plainFS struct{ fs.FS }

func TestCleanRel(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr error
	}{
		{in: "a/../b", want: "b"},
		{in: "./x", want: "x"},
		{in: ".", want: "."},
		{in: "", want: "."},
		{in: "/x", wantErr: ErrEscapes},
		{in: "..", wantErr: ErrEscapes},
		{in: "../x", wantErr: ErrEscapes},
		{in: "a/../../x", wantErr: ErrEscapes},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := CleanRel(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CleanRel(%q) error = %v, want %v", tt.in, err, tt.wantErr)
			}

			if got != tt.want {
				t.Errorf("CleanRel(%q) = %q, want %q", tt.in, got, tt.want)
			}

			if err == nil && !fs.ValidPath(got) {
				t.Errorf("CleanRel(%q) = %q, which is not an fs.ValidPath", tt.in, got)
			}
		})
	}
}

func TestEvalSymlinks(t *testing.T) {
	tests := []struct {
		name    string
		fsys    fs.FS
		in      string
		want    string
		wantErr error
	}{
		{
			name: "plain path",
			fsys: fstest.MapFS{"a/b.yaml": file("x")},
			in:   "a/b.yaml",
			want: "a/b.yaml",
		},
		{
			name: "link to a directory",
			fsys: fstest.MapFS{"a/b.yaml": file("x"), "l": link("a")},
			in:   "l",
			want: "a",
		},
		{
			name: "path through a linked directory",
			fsys: fstest.MapFS{"a/b.yaml": file("x"), "l": link("a")},
			in:   "l/b.yaml",
			want: "a/b.yaml",
		},
		{
			name: "target is relative to the link directory",
			fsys: fstest.MapFS{"a/b.yaml": file("x"), "d/l": link("../a/b.yaml")},
			in:   "d/l",
			want: "a/b.yaml",
		},
		{
			name: "chain of links",
			fsys: fstest.MapFS{"a/b.yaml": file("x"), "l1": link("l2"), "l2": link("a/b.yaml")},
			in:   "l1",
			want: "a/b.yaml",
		},
		{
			name:    "target leaves the root",
			fsys:    fstest.MapFS{"l": link("../../x")},
			in:      "l",
			wantErr: ErrEscapes,
		},
		{
			name:    "target is absolute",
			fsys:    fstest.MapFS{"l": link("/etc")},
			in:      "l",
			wantErr: ErrEscapes,
		},
		{
			name:    "input leaves the root",
			fsys:    fstest.MapFS{"a": file("x")},
			in:      "../a",
			wantErr: ErrEscapes,
		},
		{
			name:    "two links pointing at each other",
			fsys:    fstest.MapFS{"p": link("q"), "q": link("p")},
			in:      "p",
			wantErr: ErrSymlinkLoop,
		},
		{
			name:    "missing component",
			fsys:    fstest.MapFS{"a": file("x")},
			in:      "missing",
			wantErr: fs.ErrNotExist,
		},
		{
			name:    "dangling link",
			fsys:    fstest.MapFS{"l": link("nowhere")},
			in:      "l",
			wantErr: fs.ErrNotExist,
		},
		{
			name: "a file system without links returns the cleaned name",
			fsys: plainFS{fstest.MapFS{"b": file("x")}},
			in:   "a/../b",
			want: "b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EvalSymlinks(tt.fsys, tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("EvalSymlinks(%q) error = %v, want %v", tt.in, err, tt.wantErr)
			}

			if got != tt.want {
				t.Errorf("EvalSymlinks(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCollectPatchFiles_FollowsInTreeSymlinks(t *testing.T) {
	fsys := fstest.MapFS{
		"all/a.yaml":      file("a: 1\n"),
		"all/b.yaml.tpl":  file("b: 2\n"),
		"all/linked":      link("../shared"),
		"all/notes":       link("../other/n.yaml"),
		"all/c.yaml":      link("../other/c.txt"),
		"all/z.txt":       file("z"),
		"shared/e.yaml":   file("e: 5\n"),
		"other/n.yaml":    file("n: 6\n"),
		"other/c.txt":     file("c"),
		"node/cp-01/x.md": file("x"),
	}

	got, err := collectPatchFiles(fsys, "all")
	if err != nil {
		t.Fatalf("collectPatchFiles: %v", err)
	}

	want := []string{"all/a.yaml", "all/b.yaml.tpl", "all/linked/e.yaml", "all/notes"}
	if !slices.Equal(got, want) {
		t.Errorf("collectPatchFiles = %q, want %q", got, want)
	}
}

func TestCollectPatchFiles_SameTargetTwice(t *testing.T) {
	fsys := fstest.MapFS{
		"all/one":       link("../shared"),
		"all/two":       link("../shared"),
		"shared/e.yaml": file("e: 5\n"),
	}

	got, err := collectPatchFiles(fsys, "all")
	if err != nil {
		t.Fatalf("collectPatchFiles: %v", err)
	}

	want := []string{"all/one/e.yaml", "all/two/e.yaml"}
	if !slices.Equal(got, want) {
		t.Errorf("collectPatchFiles = %q, want %q", got, want)
	}
}

func TestCollectPatchFiles_SymlinkLoop(t *testing.T) {
	tests := []struct {
		name     string
		fsys     fs.FS
		wantName string
	}{
		{
			name:     "link to its own directory",
			fsys:     fstest.MapFS{"all/a.yaml": file("a: 1\n"), "all/loop": link(".")},
			wantName: `"all/loop"`,
		},
		{
			name:     "link to an ancestor",
			fsys:     fstest.MapFS{"all/sub/a.yaml": file("a: 1\n"), "all/sub/up": link("..")},
			wantName: `"all/sub/up"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := collectPatchFiles(tt.fsys, "all")
			if !errors.Is(err, ErrSymlinkLoop) {
				t.Errorf("collectPatchFiles error = %v, want %v", err, ErrSymlinkLoop)
			}

			if err != nil && !strings.Contains(err.Error(), tt.wantName) {
				t.Errorf("collectPatchFiles error = %v, want it to name the link %s", err, tt.wantName)
			}
		})
	}
}

func TestCollectPatchFiles_MissingFolderEmpty(t *testing.T) {
	fsys := fstest.MapFS{"all/a.yaml": file("a: 1\n")}

	got, err := collectPatchFiles(fsys, "node/none")
	if err != nil {
		t.Fatalf("collectPatchFiles: %v", err)
	}

	if got != nil {
		t.Errorf("collectPatchFiles = %q, want nil", got)
	}
}

func TestCollectPatchFiles_DanglingLinkFails(t *testing.T) {
	fsys := fstest.MapFS{
		"all/a.yaml": file("a: 1\n"),
		"all/dead":   link("../nowhere"),
	}

	_, err := collectPatchFiles(fsys, "all")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("collectPatchFiles error = %v, want %v", err, fs.ErrNotExist)
	}
}

func TestCollectPatchFiles_LinkLeavingTreeRefused(t *testing.T) {
	fsys := fstest.MapFS{"all/out": link("../../elsewhere")}

	_, err := collectPatchFiles(fsys, "all")
	if !errors.Is(err, ErrEscapes) {
		t.Errorf("collectPatchFiles error = %v, want %v", err, ErrEscapes)
	}
}
