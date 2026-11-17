package main

import (
	"strings"
	"testing"
)

const (
	validSig = "[GNUPG:] VALIDSIG 62989046EB5C7E985ECDF5DD3B0FEA9BE13CA3C9 2026-11-01 1793404800 0 4 0 1 10 00 0FF11AF081E98345594812037091115F8320B897"
	goodSig  = "[GNUPG:] GOODSIG 3B0FEA9BE13CA3C9 Francois Dupoux"
	primaryA = "0FF11AF081E98345594812037091115F8320B897"
)

var validSigCases = []struct {
	name   string
	status string
	want   string
	wantOK bool
}{
	{"a subkey signature names the primary", goodSig + "\n" + validSig + "\n", primaryA, true},
	{"CRLF line ends", validSig + "\r\n", primaryA, true},
	{"an expired key", "[GNUPG:] EXPKEYSIG 3B0FEA9BE13CA3C9 x\n" + validSig + "\n", "", false},
	{"a revoked key", "[GNUPG:] REVKEYSIG 3B0FEA9BE13CA3C9 x\n" + validSig + "\n", "", false},
	{"a bad signature", validSig + "\n[GNUPG:] BADSIG 3B0FEA9BE13CA3C9 x\n", "", false},
	{"no VALIDSIG", goodSig + "\n[GNUPG:] NO_PUBKEY 3B0FEA9BE13CA3C9\n", "", false},
	{"empty status", "", "", false},
	{"VALIDSIG without the primary field", "[GNUPG:] VALIDSIG 62989046EB5C7E985ECDF5DD3B0FEA9BE13CA3C9\n", "", false},
	{"VALIDSIG whose last field is not a fingerprint", "[GNUPG:] VALIDSIG 62989046EB5C7E985ECDF5DD3B0FEA9BE13CA3C9 2026-11-01 1793404800 0 4 0 1 10 00\n", "", false},
	{"a VALIDSIG text that is not a status line", "VALIDSIG " + primaryA + "\n", "", false},
}

func TestParseValidSig(t *testing.T) {
	for _, tt := range validSigCases {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseValidSig([]byte(tt.status))
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("parseValidSig = %q, %t, want %q, %t", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func FuzzParseValidSig(f *testing.F) {
	for _, tt := range validSigCases {
		f.Add([]byte(tt.status))
	}
	f.Fuzz(func(t *testing.T, status []byte) {
		got, ok := parseValidSig(status)
		if !ok {
			if got != "" {
				t.Errorf("parseValidSig = %q with ok false", got)
			}
			return
		}
		if len(got) != 40 || strings.Trim(got, "0123456789ABCDEF") != "" {
			t.Errorf("parseValidSig = %q: not 40 upper-case hex digits", got)
		}
	})
}

func TestKeyFingerprint(t *testing.T) {
	const subkey = "62989046EB5C7E985ECDF5DD3B0FEA9BE13CA3C9"
	tests := []struct {
		name    string
		colons  string
		want    string
		wantErr bool
	}{
		{
			name: "the fpr after the first pub",
			colons: "pub:-:4096:1:7091115F8320B897:1625403790:2208949390::-:::scESCA:::::::23::0:\n" +
				"fpr:::::::::" + primaryA + ":\n" +
				"uid:-::::1625403790::AAAA::Francois Dupoux::::::::::0:\n" +
				"sub:-:4096:1:3B0FEA9BE13CA3C9:1625404504:2208950104:::::s:::::::23:\n" +
				"fpr:::::::::" + subkey + ":\n",
			want: primaryA,
		},
		{
			name:   "two keys: the first",
			colons: "pub:-:255:22:AAAA:1:::-:::scESC:::::::\nfpr:::::::::" + primaryA + ":\npub:-:255:22:BBBB:1:::-:::scESC:::::::\nfpr:::::::::" + subkey + ":\n",
			want:   primaryA,
		},
		{name: "no pub record", colons: "fpr:::::::::" + primaryA + ":\n", wantErr: true},
		{name: "pub with no fpr", colons: "pub:-:4096:1:AAAA:1:::-:::scESC:::::::\nuid:-::::1::A::x::::::::::0:\n", wantErr: true},
		{name: "a subkey record before the fpr", colons: "pub:-:4096:1:AAAA:1:::-:::scESC:::::::\nsub:-:4096:1:BBBB:1::::::s:::::::\nfpr:::::::::" + subkey + ":\n", wantErr: true},
		{name: "fpr of the wrong length", colons: "pub:-:4096:1:AAAA:1:::-:::scESC:::::::\nfpr:::::::::ABCD:\n", wantErr: true},
		{name: "empty", colons: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := keyFingerprint([]byte(tt.colons))
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("keyFingerprint = %q, %v, want %q, error %t", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
