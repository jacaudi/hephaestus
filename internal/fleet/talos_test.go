package fleet

import "testing"

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
