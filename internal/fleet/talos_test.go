package fleet

import (
	"strings"
	"testing"
)

func TestTalosKey_Tag(t *testing.T) {
	tests := []struct {
		name string
		key  TalosKey
		want string
	}{
		{
			"amd64",
			TalosKey{Schematic: plainSchematic, Version: talosVersion, Arch: ArchAMD64},
			"talos/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba-amd64:v1.12.11",
		},
		{
			"arm64",
			TalosKey{Schematic: plainSchematic, Version: "v1.12.10", Arch: ArchARM64},
			"talos/376567988ad370138ad8b2698212367b8edcb69b5fd68c80be1f2ec7d603b4ba-arm64:v1.12.10",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.key.Tag(); got != tt.want {
				t.Errorf("Tag() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTalosRow_Key(t *testing.T) {
	row := TalosRow{Schematic: plainSchematic, Version: talosVersion, Arch: ArchARM64}
	want := TalosKey{Schematic: plainSchematic, Version: talosVersion, Arch: ArchARM64}
	if got := row.Key(); got != want {
		t.Errorf("Key() = %+v, want %+v", got, want)
	}
}

func TestTalosParts(t *testing.T) {
	want := [3]TalosPart{TalosKernel, TalosInitramfs, TalosCmdline}
	if got := TalosParts(); got != want {
		t.Errorf("TalosParts() = %v, want %v", got, want)
	}
}

func TestIsSHA256Hex(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want bool
	}{
		{"plain schematic", plainSchematic, true},
		{"all f", strings.Repeat("f", 64), true},
		{"empty", "", false},
		{"63 digits", strings.Repeat("a", 63), false},
		{"65 digits", strings.Repeat("a", 65), false},
		{"upper case", strings.Repeat("A", 64), false},
		{"not hex", strings.Repeat("g", 64), false},
		{"prefixed", "sha256:" + strings.Repeat("a", 57), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSHA256Hex(tt.s); got != tt.want {
				t.Errorf("IsSHA256Hex(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

func TestTalosAssets_Complete(t *testing.T) {
	without := func(p TalosPart) TalosAssets {
		a := talosAssets()
		delete(a, p)
		return a
	}
	badHash := talosAssets()
	badHash[TalosInitramfs] = TalosAsset{Size: 1, SHA256: strings.Repeat("B", 64)}
	tests := []struct {
		name   string
		assets TalosAssets
		want   bool
	}{
		{"all three", talosAssets(), true},
		{"nil", nil, false},
		{"empty", TalosAssets{}, false},
		{"no kernel", without(TalosKernel), false},
		{"no initramfs", without(TalosInitramfs), false},
		{"no cmdline", without(TalosCmdline), false},
		{"a hash that is not lower-case hex", badHash, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.assets.Complete(); got != tt.want {
				t.Errorf("Complete(%v) = %v, want %v", tt.assets, got, tt.want)
			}
		})
	}
}
