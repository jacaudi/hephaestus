package tftp

import (
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
)

// fakeTransfer is the io.ReaderFrom a read handler receives from pin/tftp,
// with the peer address of an OutgoingTransfer.
type fakeTransfer struct {
	remote net.UDPAddr
	sent   []byte
}

func (f *fakeTransfer) ReadFrom(r io.Reader) (int64, error) {
	b, err := io.ReadAll(r)
	f.sent = b
	return int64(len(b)), err
}

func (f *fakeTransfer) SetSize(int64) {}

func (f *fakeTransfer) RemoteAddr() net.UDPAddr { return f.remote }

// anonymousTransfer is an io.ReaderFrom that does not say who the peer is.
type anonymousTransfer struct{ sent []byte }

func (a *anonymousTransfer) ReadFrom(r io.Reader) (int64, error) {
	b, err := io.ReadAll(r)
	a.sent = b
	return int64(len(b)), err
}

func TestServer_ServesOnlyTheSubnet(t *testing.T) {
	subnet := netip.MustParsePrefix("10.10.20.0/24")
	s := NewServer("10.10.20.5", subnet, map[string][]byte{"amd64/snp.efi": []byte("efi")}, slog.New(slog.DiscardHandler))

	tests := []struct {
		name      string
		peer      net.IP
		wantServe bool
	}{
		{"on subnet, 4-byte form", net.IPv4(10, 10, 20, 50).To4(), true},
		{"on subnet, 16-byte form", net.IPv4(10, 10, 20, 50).To16(), true},
		{"on subnet, IPv4-mapped IPv6", net.ParseIP("::ffff:10.10.20.50"), true},
		{"off subnet", net.IPv4(192, 0, 2, 7), false},
		{"next to the subnet", net.IPv4(10, 10, 21, 1), false},
		{"IPv6", net.ParseIP("fe80::1"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rf := &fakeTransfer{remote: net.UDPAddr{IP: tc.peer, Port: 40000}}

			err := s.handleRead("amd64/snp.efi", rf)

			switch {
			case tc.wantServe && (err != nil || string(rf.sent) != "efi"):
				t.Errorf("handleRead = %v, sent %q; want the file served", err, rf.sent)
			case !tc.wantServe && (err == nil || len(rf.sent) != 0):
				t.Errorf("handleRead = %v, sent %q; want the request refused and nothing sent", err, rf.sent)
			}
		})
	}
}

func TestServer_RefusesUnidentifiedPeer(t *testing.T) {
	s := NewServer("10.10.20.5", netip.MustParsePrefix("10.10.20.0/24"), map[string][]byte{"amd64/snp.efi": []byte("efi")}, slog.New(slog.DiscardHandler))
	rf := &anonymousTransfer{}

	err := s.handleRead("amd64/snp.efi", rf)

	if err == nil || len(rf.sent) != 0 {
		t.Errorf("handleRead = %v, sent %q; want the request refused and nothing sent", err, rf.sent)
	}
}
