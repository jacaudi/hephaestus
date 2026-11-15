// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
// hephaestus: vendored from github.com/siderolabs/omni-infra-provider-bare-metal v0.15.0 (13d64d3ec8e1a4f90f50af160380b086030f2343), pkg/dhcp/export_test.go. Changes: none.

package dhcp

var IsBootDHCP = isBootDHCP

var OfferDHCP = offerDHCP

type Firmware = firmware

const (
	FirmwareUnsupported = firmwareUnsupported
	FirmwareX86PC       = firmwareX86PC
	FirmwareX86EFI      = firmwareX86EFI
	FirmwareARMEFI      = firmwareARMEFI
	FirmwareX86Ipxe     = firmwareX86Ipxe
	FirmwareX86HTTP     = firmwareX86HTTP
	FirmwareARMHTTP     = firmwareARMHTTP

	Port67   = port67
	Port4011 = port4011
)
