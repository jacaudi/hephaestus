package catalog

import (
	"net/netip"
	"path"
	"strings"
)

// bootIFArg is the BOOTIF argument: the NIC the machine booted from, written
// with iPXE's own setting expansion. idx is the embedded script's interface
// counter. It is the only substitution a script leaves for iPXE to make.
const bootIFArg = "BOOTIF=01-${net${idx}/mac:hexhyp}"

// BaseURL returns the URL the entry's files are served under, ending in "/".
// addr is the provisioning address: live-boot's fetch= accepts an IP only.
func (s Spec) BaseURL(addr netip.Addr) string {
	return "http://" + addr.String() + "/u/" + s.Entry + "/" + s.Arch + "/" + s.Version + "/"
}

// Script returns the iPXE script that boots the entry. Its only substitutions
// are the provisioning address, the entry's own paths and BOOTIF; no request
// text reaches it.
func (s Spec) Script(addr netip.Addr) string {
	base := s.BaseURL(addr)
	if s.Recipe == RecipeChain {
		return "#!ipxe\nchain " + base + s.Boot.Kernel + "\n"
	}

	var b strings.Builder
	b.WriteString("#!ipxe\nkernel " + base + s.Boot.Kernel)
	for _, i := range s.Boot.Initrds {
		b.WriteString(" initrd=" + path.Base(i))
	}
	if s.Args != "" {
		b.WriteString(" " + s.Args)
	}
	switch s.Recipe {
	case RecipeArchiso:
		b.WriteString(" ip=dhcp " + bootIFArg + " archiso_http_srv=" + base + " checksum")
	case RecipeLiveBoot:
		b.WriteString(" " + bootIFArg + " fetch=" + base + s.Boot.Second)
	case RecipeKernel, RecipeChain:
	}
	b.WriteString("\n")
	for _, i := range s.Boot.Initrds {
		b.WriteString("initrd " + base + i + "\n")
	}
	b.WriteString("boot\n")
	return b.String()
}
