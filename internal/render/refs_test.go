package render

import (
	"testing"

	"go.jacaudi.dev/hephaestus/internal/third_party/topf/config"
)

func TestIsReference(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{in: "ref+op://v/i/f", want: true},
		{in: "ref+op://v/i/s/f", want: true},
		{in: "ref+op://v/i", want: false},
		{in: "ref+op://v/i/s/f/x", want: false},
		{in: "ref+op://v//f", want: false},
		{in: "ref+op://v/i/f/", want: false},
		{in: "ref+op:///i/f", want: false},
		{in: "ref+op://", want: false},
		{in: "ref+op://v/i/f?attr=otp", want: false},
		{in: "ref+op://v/i/f#x", want: false},
		{in: "ref+op://v/i/f x", want: false},
		{in: "ref+op://v/i/f\n", want: false},
		{in: "ref+op://v/i/f\t", want: false},
		{in: "ref+op://v/i/f+g", want: false},
		{in: "ref+vault://a", want: false},
		{in: "ref+vault://a/b/c", want: false},
		{in: "op://v/i/f", want: false},
		{in: " ref+op://v/i/f", want: false},
		{in: "x ref+op://v/i/f", want: false},
		{in: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := IsReference(tt.in); got != tt.want {
				t.Errorf("IsReference(%q) = %t, want %t", tt.in, got, tt.want)
			}
		})
	}
}

func TestHasEmbeddedReference(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{in: "x ref+op://v/i/f", want: true},
		{in: "ref+vault://x", want: true},
		{in: "ref+op://v/i/f", want: true},
		{in: "a\nb ref+sops://c", want: true},
		{in: "ref+ref+op://x", want: true},
		{in: "ref+op://", want: true},
		{in: "op://v/i/f", want: false},
		{in: "ref+://x", want: false},
		{in: "ref+op:/x", want: false},
		{in: "ref+a-b://x", want: false},
		{in: "ref+", want: false},
		{in: "ref", want: false},
		{in: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := HasEmbeddedReference(tt.in); got != tt.want {
				t.Errorf("HasEmbeddedReference(%q) = %t, want %t", tt.in, got, tt.want)
			}
		})
	}
}

func FuzzIsReference(f *testing.F) {
	for _, s := range []string{
		"ref+op://v/i/f", "ref+op://v/i/s/f", "ref+op://v/i", "ref+op://v/i/f?attr=otp", "ref+op://v/i/f#x",
		"ref+op://v/i/f x", "ref+op://v/i/f+g", "ref+vault://a", "op://v/i/f", "x ref+op://v/i/f", "", "ref+", "ref+op://",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		if !IsReference(s) {
			return
		}

		if !HasEmbeddedReference(s) {
			t.Errorf("IsReference(%q) holds but HasEmbeddedReference does not", s)
		}

		if !config.HasReferencePrefix(s) {
			t.Errorf("IsReference(%q) holds but config.HasReferencePrefix does not", s)
		}
	})
}
