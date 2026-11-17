package toolcache

import "testing"

func TestSpoolNames(t *testing.T) {
	if got, want := SpoolName("43a4f134ae5e", 0), "43a4f134ae5e-0"; got != want {
		t.Errorf("SpoolName = %q, want %q", got, want)
	}
	if got, want := ETagName("43a4f134ae5e", 0), "43a4f134ae5e-0.etag"; got != want {
		t.Errorf("ETagName = %q, want %q", got, want)
	}
	if got, want := ETagName("43a4f134ae5e", 12), "43a4f134ae5e-12.etag"; got != want {
		t.Errorf("ETagName(12) = %q, want %q", got, want)
	}
}

func TestStrongETag(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"strong", `"v1"`, true},
		{"empty opaque tag", `""`, true},
		{"weak", `W/"v1"`, false},
		{"empty", "", false},
		{"unquoted", "v1", false},
		{"lone quote", `"`, false},
		{"open quote only", `"v1`, false},
		{"close quote only", `v1"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strongETag(tt.in); got != tt.want {
				t.Errorf("strongETag(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// contentRangeCases are the table inputs; the fuzz test seeds from the same.
var contentRangeCases = []struct {
	name              string
	in                string
	start, end, total int64
	wantErr           bool
}{
	{name: "whole", in: "bytes 0-9/10", start: 0, end: 9, total: 10},
	{name: "capitalised unit", in: "Bytes 0-9/10", start: 0, end: 9, total: 10},
	{name: "upper-case unit", in: "BYTES 5-9/10", start: 5, end: 9, total: 10},
	{name: "tail", in: "bytes 5-9/10", start: 5, end: 9, total: 10},
	{name: "one byte", in: "bytes 9-9/10", start: 9, end: 9, total: 10},
	{name: "unknown total", in: "bytes 5-9/*", wantErr: true},
	{name: "end before start", in: "bytes 9-5/10", wantErr: true},
	{name: "wrong unit", in: "items 0-1/2", wantErr: true},
	{name: "empty", in: "", wantErr: true},
	{name: "trailing junk", in: "bytes 0-9/10x", wantErr: true},
	{name: "end at total", in: "bytes 0-10/10", wantErr: true},
	{name: "unsatisfied form", in: "bytes */10", wantErr: true},
	{name: "no total", in: "bytes 0-9", wantErr: true},
	{name: "signed start", in: "bytes +0-9/10", wantErr: true},
	{name: "negative start", in: "bytes -1-5/10", wantErr: true},
	{name: "leading space", in: "bytes  0-9/10", wantErr: true},
	{name: "overflow", in: "bytes 0-9/99999999999999999999", wantErr: true},
}

func TestParseContentRange(t *testing.T) {
	for _, tt := range contentRangeCases {
		t.Run(tt.name, func(t *testing.T) {
			start, end, total, err := parseContentRange(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseContentRange(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if start != tt.start || end != tt.end || total != tt.total {
				t.Errorf("parseContentRange(%q) = %d, %d, %d; want %d, %d, %d",
					tt.in, start, end, total, tt.start, tt.end, tt.total)
			}
		})
	}
}

func FuzzParseContentRange(f *testing.F) {
	for _, tt := range contentRangeCases {
		f.Add(tt.in)
	}
	f.Fuzz(func(t *testing.T, h string) {
		start, end, total, err := parseContentRange(h)
		if err != nil {
			return
		}
		if start < 0 || start > end || end >= total {
			t.Errorf("parseContentRange(%q) = %d, %d, %d; want 0 <= start <= end < total", h, start, end, total)
		}
	})
}
