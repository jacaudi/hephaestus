package fleet

import (
	"strings"
	"testing"
)

// fixtureUUID is the shared UUID fixture.
const fixtureUUID = "4c4c4544-0042-3510-8052-b4c04f4e4d32"

type uuidCase struct {
	name string
	in   string
	want string
}

func normalizeUUIDCases() []uuidCase {
	return []uuidCase{
		{name: "uppercase", in: "4C4C4544-0042-3510-8052-B4C04F4E4D32", want: fixtureUUID},
		{name: "padded", in: " " + fixtureUUID + " ", want: fixtureUUID},
		{name: "already normal", in: fixtureUUID, want: fixtureUUID},
		{name: "not a uuid", in: "not-a-uuid", want: ""},
		{name: "empty", in: "", want: ""},
		{name: "no dashes", in: strings.ReplaceAll(fixtureUUID, "-", ""), want: ""},
		{name: "misplaced dash", in: "4c4c45440-042-3510-8052-b4c04f4e4d32", want: ""},
		{name: "non-hex digit", in: "4c4c4544-0042-3510-8052-b4c04f4e4d3g", want: ""},
		{name: "trailing newline script", in: fixtureUUID + "\n#!ipxe", want: ""},
	}
}

func TestNormalizeUUID(t *testing.T) {
	for _, tt := range normalizeUUIDCases() {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeUUID(tt.in); got != tt.want {
				t.Errorf("NormalizeUUID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestJunkPattern(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "empty", in: "", want: true},
		{name: "all zero", in: "00000000-0000-0000-0000-000000000000", want: true},
		{name: "all f", in: "ffffffff-ffff-ffff-ffff-ffffffffffff", want: true},
		{name: "fixture", in: fixtureUUID, want: false},
		{name: "one zero short of all zero", in: "00000000-0000-0000-0000-000000000001", want: false},
		{name: "mixed zero and f", in: "00000000-ffff-0000-ffff-000000000000", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := JunkPattern(tt.in); got != tt.want {
				t.Errorf("JunkPattern(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// ipoibMAC is a 20-byte InfiniBand hardware address, which net.ParseMAC accepts.
const ipoibMAC = "00:00:00:00:fe:80:00:00:00:00:00:00:02:00:5e:10:00:00:00:01"

type macCase struct {
	name    string
	in      string
	want    string
	wantErr string
}

func normalizeMACCases() []macCase {
	return []macCase{
		{name: "dashes upper", in: "AA-BB-CC-DD-EE-FF", want: "aa:bb:cc:dd:ee:ff"},
		{name: "colons lower", in: "aa:bb:cc:dd:ee:ff", want: "aa:bb:cc:dd:ee:ff"},
		{name: "dotted", in: "aabb.ccdd.eeff", want: "aa:bb:cc:dd:ee:ff"},
		{name: "20 bytes", in: ipoibMAC, wantErr: "must be a 48-bit MAC"},
		{name: "garbage", in: "zz", wantErr: "invalid MAC address"},
		{name: "empty", in: "", wantErr: "invalid MAC address"},
	}
}

func TestNormalizeMAC(t *testing.T) {
	for _, tt := range normalizeMACCases() {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeMAC(tt.in)
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("NormalizeMAC(%q) error = %v, want %q", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeMAC(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("NormalizeMAC(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func FuzzNormalizeMAC(f *testing.F) {
	for _, tt := range normalizeMACCases() {
		f.Add(tt.in)
	}

	f.Fuzz(func(t *testing.T, in string) {
		got, err := NormalizeMAC(in)
		if err != nil {
			return
		}
		if len(got) != 17 {
			t.Fatalf("NormalizeMAC(%q) = %q, length %d, want 17", in, got, len(got))
		}
		again, err := NormalizeMAC(got)
		if err != nil || again != got {
			t.Fatalf("NormalizeMAC(%q) = (%q, %v), want (%q, nil)", got, again, err, got)
		}
	})
}

func FuzzNormalizeUUID(f *testing.F) {
	for _, tt := range normalizeUUIDCases() {
		f.Add(tt.in)
	}

	f.Fuzz(func(t *testing.T, in string) {
		got := NormalizeUUID(in)
		if got == "" {
			return
		}
		if len(got) != 36 || got != strings.ToLower(got) {
			t.Fatalf("NormalizeUUID(%q) = %q, want 36 lowercase chars", in, got)
		}
		if again := NormalizeUUID(got); again != got {
			t.Fatalf("NormalizeUUID(%q) = %q, want %q", got, again, got)
		}
	})
}

func TestPrintableText(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{name: "plain text", in: "plain text", want: "plain text"},
		{name: "NUL", in: "a\x00b", want: "ab"},
		{name: "tab CR LF", in: "a\tb\r\n", want: "ab"},
		{name: "escape sequence", in: "a\x1b[2Jb", want: "a[2Jb"},
		{name: "C1 control", in: "a\u009bb", want: "ab"},
		{name: "bidi override", in: "a\u202eb", want: "ab"},
		{name: "zero width space", in: "a\u200bb", want: "ab"},
		{name: "line separator", in: "a\u2028b", want: "ab"},
		{name: "non-ASCII letters kept", in: "café 世", want: "café 世"},
		{name: "invalid UTF-8", in: "bad\xffutf8", want: "badutf8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PrintableText(tt.in); got != tt.want {
				t.Errorf("PrintableText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
