package admin

import "testing"

func TestDiskCheck(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		minFreeRatio float64
		want         Status
	}{
		{name: "enough free space", path: t.TempDir(), minFreeRatio: 0, want: StatusPass},
		{name: "below the minimum", path: t.TempDir(), minFreeRatio: 1.01, want: StatusWarn},
		{name: "statfs error", path: "/nonexistent-hephaestus", minFreeRatio: 0, want: StatusFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := DiskCheck("disk:data_dir", tt.path, tt.minFreeRatio)
			if c.Name != "disk:data_dir" || c.Critical {
				t.Fatalf("DiskCheck = {Name: %q, Critical: %v}, want informational disk:data_dir", c.Name, c.Critical)
			}

			got := c.Run(t.Context())

			if got.Status != tt.want {
				t.Fatalf("Status = %q (output %q), want %q", got.Status, got.Output, tt.want)
			}
			if tt.want == StatusFail {
				if got.Output == "" {
					t.Error("fail result has no output")
				}
				return
			}
			if got.ObservedUnit != "%" {
				t.Errorf("ObservedUnit = %q, want %%", got.ObservedUnit)
			}
			if _, ok := got.ObservedValue.(float64); !ok {
				t.Errorf("ObservedValue = %#v, want a float64", got.ObservedValue)
			}
			if tt.want == StatusWarn && got.Output == "" {
				t.Error("warn result has no output")
			}
		})
	}
}
