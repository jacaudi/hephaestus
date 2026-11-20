//go:build parity

package render

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

const (
	parityFixtures = "testdata/parity"
	// parityOut is where task render:parity has `topf render` write <case>/<host>.yaml, from the pinned Topf commit.
	parityOut = "../../build/parity/out"
)

// TestParity_Golden renders every fixture with Load and Cluster.Render and requires each host's config to equal, byte
// for byte, what `topf render` wrote for the same fixture. A host with no Topf output, or an output with no host, fails.
// A mismatch names the host and the line, never the text: a config holds secrets.
func TestParity_Golden(t *testing.T) {
	cases, err := os.ReadDir(parityFixtures)
	if err != nil {
		t.Fatalf("read %s: %v", parityFixtures, err)
	}

	if len(cases) == 0 {
		t.Fatalf("no fixture under %s", parityFixtures)
	}

	for _, c := range cases {
		t.Run(c.Name(), func(t *testing.T) {
			dir := filepath.Join(parityFixtures, c.Name())

			cluster, err := Load(os.DirFS(dir))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			var values map[string]string
			if err := json.Unmarshal(readParityFile(t, filepath.Join(dir, "values.json")), &values); err != nil {
				t.Fatalf("decode values.json: %v", err)
			}

			hosts, err := cluster.Render(t.Context(), values)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}

			outDir := filepath.Join(parityOut, c.Name())
			rendered := make([]string, 0, len(hosts))

			for _, host := range hosts {
				rendered = append(rendered, host.Host+".yaml")

				want, err := os.ReadFile(filepath.Join(outDir, host.Host+".yaml"))
				if err != nil {
					t.Errorf("host %s: no topf render output: %v", host.Host, err)

					continue
				}

				if !bytes.Equal(host.Config, want) {
					t.Errorf("host %s: config differs from topf render: %s", host.Host, describeDifference(host.Config, want))
				}
			}

			assertSameHosts(t, outDir, rendered)
		})
	}
}

// assertSameHosts fails when the Topf output directory holds a file for a host Render did not return.
func assertSameHosts(t *testing.T, outDir string, rendered []string) {
	t.Helper()

	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Errorf("read %s: %v", outDir, err)

		return
	}

	for _, entry := range entries {
		if !slices.Contains(rendered, entry.Name()) {
			t.Errorf("topf render wrote %s, hephaestus rendered no such host", entry.Name())
		}
	}
}

func readParityFile(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}

	return data
}

// describeDifference says where two configs first differ without quoting either: the line, and each side's line count
// and the length of its line there.
func describeDifference(got, want []byte) string {
	gotLines, wantLines := bytes.Split(got, []byte("\n")), bytes.Split(want, []byte("\n"))

	for i := range max(len(gotLines), len(wantLines)) {
		if i < len(gotLines) && i < len(wantLines) && bytes.Equal(gotLines[i], wantLines[i]) {
			continue
		}

		return "first differing line " + strconv.Itoa(i+1) +
			": got " + sideAt(gotLines, i) + ", want " + sideAt(wantLines, i)
	}

	return "no differing line"
}

// sideAt describes line i of one side: its length, or that the side ended before it, with the side's line count.
func sideAt(lines [][]byte, i int) string {
	if i >= len(lines) {
		return "no line (" + strconv.Itoa(len(lines)) + " lines)"
	}

	return strconv.Itoa(len(lines[i])) + " bytes (" + strconv.Itoa(len(lines)) + " lines)"
}
