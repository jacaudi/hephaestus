// hephaestus: new

package config

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

// TestCollectPatchFiles_FolderReachedThroughALink pins when "no files" is the answer: only a folder that is
// missing as a plain path element. A link on the way that dangles or leaves the tree is an error.
func TestCollectPatchFiles_FolderReachedThroughALink(t *testing.T) {
	tests := []struct {
		name    string
		fsys    fs.FS
		folder  string
		want    []string
		wantErr error
	}{
		{
			name:    "patches directory is a dangling link",
			fsys:    fstest.MapFS{"patches": link("nowhere")},
			folder:  "patches/all",
			wantErr: fs.ErrNotExist,
		},
		{
			name:    "patches directory is a link leaving the tree",
			fsys:    fstest.MapFS{"patches": link("../outside")},
			folder:  "patches/all",
			wantErr: ErrEscapes,
		},
		{
			name:    "node directory is a dangling link",
			fsys:    fstest.MapFS{"patches/node": link("../nowhere")},
			folder:  "patches/node/cp-01",
			wantErr: fs.ErrNotExist,
		},
		{
			name:    "folder itself is a dangling link",
			fsys:    fstest.MapFS{"patches/all": link("../nowhere")},
			folder:  "patches/all",
			wantErr: fs.ErrNotExist,
		},
		{
			name:   "patches directory is a link to a tree without the folder",
			fsys:   fstest.MapFS{"patches": link("real"), "real/worker/a.yaml": file("a: 1\n")},
			folder: "patches/all",
		},
		{
			name:   "patches directory is a link to a tree with the folder",
			fsys:   fstest.MapFS{"patches": link("real"), "real/all/a.yaml": file("a: 1\n")},
			folder: "patches/all",
			want:   []string{"patches/all/a.yaml"},
		},
		{
			name:   "missing folder in a file system without links",
			fsys:   plainFS{fstest.MapFS{"all/a.yaml": file("a: 1\n")}},
			folder: "node/none",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := collectPatchFiles(tt.fsys, tt.folder)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("collectPatchFiles error = %v, want %v", err, tt.wantErr)
			}

			if len(got) != len(tt.want) || (len(got) > 0 && got[0] != tt.want[0]) {
				t.Errorf("collectPatchFiles = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPatchContext_Load_PatchesDirLink(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		wantErr error
	}{
		{name: "dangling", target: "nowhere", wantErr: fs.ErrNotExist},
		{name: "leaves the tree", target: "../outside", wantErr: ErrEscapes},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := patchContext(fstest.MapFS{"patches": link(tt.target)})

			if _, err := ctx.Load(); !errors.Is(err, tt.wantErr) {
				t.Errorf("Load error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
