package dhcp_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/insomniacslk/dhcp/dhcpv4"

	"go.jacaudi.dev/hephaestus/internal/boot/ipxe"
	"go.jacaudi.dev/hephaestus/internal/third_party/omniprovider/dhcp"
)

// TestAdvertisedBootFilesAreServed binds the two halves of the PXE boot contract: every boot file
// name and URL the vendored proxy hands out must be a file the embedded iPXE binaries are served
// under. The names appear on both sides as independent literals, so this is the test that fails
// when either side drifts. It replaces the upstream boot_contract_test.go, which needs the
// provider's own packages.
func TestAdvertisedBootFilesAreServed(t *testing.T) {
	t.Parallel()

	const (
		apiAddr = "10.10.20.5"
		apiPort = 80
	)

	files, err := ipxe.Files()
	if err != nil {
		t.Fatalf("ipxe.Files: %v", err)
	}

	offer := func(t *testing.T, fw dhcp.Firmware) *dhcpv4.DHCPv4 {
		t.Helper()

		req, err := dhcpv4.New(
			dhcpv4.WithMessageType(dhcpv4.MessageTypeRequest),
			dhcpv4.WithHwAddr([]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}),
		)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}

		resp, err := dhcp.OfferDHCP(req, apiAddr, apiPort, fw, dhcp.Port4011)
		if err != nil {
			t.Fatalf("OfferDHCP(firmware %d): %v", fw, err)
		}

		return resp
	}

	t.Run("TFTP boot file names are served", func(t *testing.T) {
		t.Parallel()

		for _, fw := range []dhcp.Firmware{dhcp.FirmwareX86PC, dhcp.FirmwareX86EFI, dhcp.FirmwareARMEFI} {
			name := offer(t, fw).BootFileName
			if _, ok := files[name]; !ok {
				t.Errorf("firmware %d advertises TFTP file %q, which is not among the served files", fw, name)
			}
		}
	})

	t.Run("iPXE chain URL file is served", func(t *testing.T) {
		t.Parallel()

		bootURL, err := url.Parse(offer(t, dhcp.FirmwareX86Ipxe).BootFileNameOption())
		if err != nil {
			t.Fatalf("parse boot URL: %v", err)
		}

		name := strings.TrimPrefix(bootURL.Path, "/")
		if _, ok := files[name]; !ok {
			t.Errorf("the advertised iPXE chain file %q is not among the served files", name)
		}
	})

	t.Run("HTTP boot URLs resolve to served files", func(t *testing.T) {
		t.Parallel()

		for _, fw := range []dhcp.Firmware{dhcp.FirmwareX86HTTP, dhcp.FirmwareARMHTTP} {
			bootURL, err := url.Parse(offer(t, fw).BootFileNameOption())
			if err != nil {
				t.Fatalf("parse boot URL: %v", err)
			}

			if bootURL.Host != "10.10.20.5:80" {
				t.Errorf("firmware %d boot URL host = %q, want %q", fw, bootURL.Host, "10.10.20.5:80")
			}

			name, ok := strings.CutPrefix(bootURL.Path, "/tftp/")
			if !ok {
				t.Errorf("firmware %d boot URL path %q is not under /tftp/", fw, bootURL.Path)

				continue
			}

			if _, ok := files[name]; !ok {
				t.Errorf("firmware %d advertises %q, which is not among the served files", fw, name)
			}
		}
	})
}
